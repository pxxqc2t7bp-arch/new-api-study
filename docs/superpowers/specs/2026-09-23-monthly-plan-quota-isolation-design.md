# Monthly Plan Quota Isolation Design

## Context

The upstream Plan API can return `AccountQuotaExceeded` with:

```text
You have exceeded the monthly usage quota. It will reset at
2026-09-30 23:59:59 +0800 CST.
```

The current runtime retries this 429 but does not auto-disable the affected
channels. `ParsePlanQuotaReset` recognizes only 5-hour and weekly quota
messages, while the configured automatic-disable keywords do not match the
monthly wording. Channels that share the exhausted credential but use
different tags can therefore continue receiving traffic.

## Goals

- Recognize monthly Plan quota exhaustion as a quota-limited channel error.
- Parse the provider reset timestamp and preserve the existing 60-second
  recovery grace period.
- Auto-disable every non-multi-key Plan channel that uses the same exact
  credential as the failing channel, even when those channels use different
  Plan tags or protocols.
- Preserve manually disabled channels and auto-disabled channels owned by
  another failure when sweeping a shared credential.
- Leave non-Plan channels and Plan channels with other credentials unchanged.
- Keep multi-key Plan errors on the existing per-key status path.
- Recover only auto-disabled channels from the same credential domain after
  `disabled_until`.
- Schedule one passive recovery probe per quota domain while leaving unrelated
  auto-disabled channels independently eligible.
- Fence quota ownership writes against credential and tag rotation.
- Prevent stale recovery snapshots from superseding a newer disable event.
- Keep legacy single-key status writes atomic with ability state.
- Apply Plan quota isolation to managed routes before managed failure
  thresholds, without changing unsupported-model handling.
- Let validated managed Plan quota domains participate in passive recovery
  after their quota reset while ordinary managed channels remain excluded.
- Require structured upstream evidence before entering the Plan quota domain
  path, while preserving configured generic disable behavior.
- Bind isolation to the credential and Plan tag observed by the failed
  request, even if either field rotates before error handling.
- Prevent managed reconciliation from enabling a channel while valid Plan
  quota ownership remains.
- Count only health-check recoveries that actually commit, and fail passive
  selection closed when managed-route ownership cannot be queried.

## Non-Goals

- Do not disable every generic HTTP 429.
- Do not change retry counts or fallback ordering.
- Do not alter channel keys, priorities, weights, models, or tags.
- Do not add a credential lookup query to the model layer.
- Do not persist raw credentials in quota metadata.
- Do not deploy the code change as part of the implementation commit.

## Design

`ParsePlanQuotaReset` parses monthly, weekly, and 5-hour usage quota messages
and their reset timestamps when present. `ClassifyPlanQuotaError` is the
service-level authority for the Plan domain path: it requires HTTP 429, an
error code from `NewAPIError.GetErrorCode()` or an available OpenAI error code
or type that normalizes to `AccountQuotaExceeded` after
case/underscore/hyphen normalization, and either a recognized quota window or
a `You have <n> weighted tokens left` message. This includes Claude errors,
whose upstream type is surfaced through `GetErrorCode`. `ShouldDisableChannel`
uses this classifier before configurable status-code and keyword checks.
Errors rejected by the classifier can still follow generic configured disable
behavior, but they cannot acquire Plan quota ownership.

`DisableChannelForAPIError` carries the structured classification and the
request's `ChannelError.UsingKey` plus the selected channel snapshot's tag into
disable handling. The observed tag, rather than a cache or database reload,
determines whether the failed request came from a non-multi-key `plan:`
channel. The service then loads current channels with
`model.GetAllChannels(..., selectAll=true)`, compares each current single
credential against the observed request credential case-sensitively, and
retains only exact non-multi-key Plan matches. It never substitutes later
reloaded source identity. Therefore a request sent under Plan tag/key A cannot
disable a source whose tag and key rotated to an ordinary/B identity, or any B
peer, while current Plan peers still on A are isolated. Multi-key Plan errors
bypass the domain path and retain existing per-key status handling.

The selected channels are auto-disabled and have their abilities disabled
through a model compare-and-swap operation. Existing reset metadata remains in
place, and `quota_domain_id` stores the lowercase hexadecimal SHA-256 digest of
the credential. The digest is deterministic across tags without exposing the
credential. If the failing channel has an empty credential, no broad lookup is
performed: only that known channel is disabled and marked with
`quota_domain_id=channel:<id>`. A nil channel remains a no-op. The sweep may
write only an enabled channel or an auto-disabled channel that already carries
the same `quota_domain_id`. It leaves manually disabled rows and auto-disabled
rows owned by another marker unchanged, including their metadata and ability
state.

### Atomic Ownership Transition

The service builds the complete desired `other_info` from each selected
channel snapshot. It passes the snapshot's exact key, tag, status, and raw
`other_info` string to a focused model API for single-key channels. The model
API holds the same process-local status and per-channel polling locks used by
`UpdateChannelStatus`, starts `DB.Transaction`, and reads the current channel
through `lockForUpdate(tx)`. MySQL and PostgreSQL therefore hold a row lock;
SQLite skips unsupported `FOR UPDATE` syntax and relies on its single-writer
transaction behavior.

The transaction proceeds only when the locked row is still single-key and its
key, tag, status, and raw `other_info` exactly match the expected snapshot. It
updates `channels.status`, `channels.other_info`, and all corresponding
`abilities.enabled` rows in that transaction. A stale identity, status, or
metadata snapshot returns `changed=false` without writing either table. Any
channel or ability update error rolls the transaction back, so their enabled
states cannot diverge. The in-memory channel status cache is updated only
after a successful commit.

Every Plan quota disable event writes one new decimal Unix-nanosecond string
as `quota_generation`, including when a channel is already auto-disabled and
owned by the same quota domain. This guarantees that a fresh disable changes
the exact raw metadata expected by recovery. A recovery snapshot taken before
that event therefore loses its CAS and cannot re-enable the channel. Successful
recovery removes `quota_generation` with the other quota ownership fields.

The non-multi-key branch of legacy `UpdateChannelStatus` uses a bounded
snapshot/CAS retry over the same internal lock and transaction primitive.
Status, `other_info`, and abilities commit together, and cache status changes
only after commit. The existing multi-key status-list behavior remains on its
established path.

Plan disable and recovery no longer call `UpdateChannelStatus` followed by
`MergeChannelStatusMetadata`. A concurrent manual disable, unrelated
auto-disable marker, or metadata edit invalidates the snapshot and makes the
attempt a no-op.

Automatic channel tests call `EnableChannelForHealthCheck` with the exact
`Channel` snapshot used by the probe and its selected key. Non-multi-key Plan
quota ownership and recovery scope are derived from that pre-probe snapshot.
The service first attempts to enable the source with a CAS over the snapshot's
exact key, tag, status, and raw `other_info`. If that CAS is stale or fails,
recovery stops: peers are not loaded or enabled, and the call never falls
through to generic ID-based enablement. This fences a successful stale probe
from a newer quota generation, a manual disable, or an unrelated ownership
change.

Only after the source CAS commits does automatic recovery load current channel
snapshots. If the source snapshot has `quota_domain_id`, current auto-disabled
rows with the same marker are eligible, including rows with different Plan
tags. A marked peer must also have the exact source snapshot
`quota_generation` and have `disabled_until <= now`. A newer, different, or
not-yet-due peer remains disabled with its abilities disabled. Marked legacy
rows remain compatible only when both source and peer omit
`quota_generation`; validated legacy tag recovery remains supported. Each peer
is recovered through its own exact CAS, so changes made after the peer query
remain protected. Rows from another marked domain and manually disabled rows
are left unchanged. Metadata is cleared only in the successful CAS that
enables a selected row. Generic single-key health-check recovery is also
snapshot-bound; multi-key recovery retains the existing per-key path.
`EnableChannelForHealthCheck` returns the number of source and peer enable
commits, and controller accounting adds that value rather than counting the
probe attempt. `EnableChannel` remains available with its current-state
behavior for manual and internal non-health-check callers.

For rows written before `quota_domain_id` existed, recovery falls back to the
recovering channel's tag. A markerless row qualifies for this fallback only
when it is auto-disabled and its metadata explicitly contains
`quota_type="plan"` plus `quota_domain` equal to its current tag. Generic
markerless failures, malformed legacy metadata, manually disabled rows, and
rows carrying another marker are not part of that recovery domain.

The same recovery-domain classifier is reused by passive test selection.
Marked rows deduplicate by `quota_domain_id`, validated legacy rows deduplicate
by their `quota_domain`/tag, and every other auto-disabled row receives an
independent probe opportunity even when several such rows share a Plan tag.
Within each shared recovery domain, the eligible row with the oldest
`TestTime` is selected; equal timestamps choose the lower channel ID.
Ordinary managed channels remain excluded from passive channel tests. A
managed channel that the classifier recognizes as a marked or validated
legacy Plan quota owner is included once `disabled_until` has elapsed. This
allows an all-managed quota domain to recover without depending on the managed
route's `next_probe_at`. Selection returns an error if the managed-route ID
query fails; the system task propagates that error and starts no probes rather
than treating the managed set as empty.

Before marker-scoped health-check recovery, the service computes a marker from
the recovering snapshot's single credential and compares it with the
snapshot's persisted `quota_domain_id`. If credential rotation changed the
marker, only the recovering source is enabled and has its quota metadata
cleared. Peers that still carry the old marker remain auto-disabled.

If loading current peers fails after a successful source CAS, the operation
logs the error and leaves every peer unchanged; it does not revert or conceal
the already committed source recovery. Manual/internal `EnableChannel`
continues to load the domain before applying its existing current-state
recovery behavior.

Managed-route error handling retains unsupported-model isolation as its first
special case. A recognized Plan quota error is then sent through
`DisableChannelForAPIError` synchronously, where Plan domain isolation runs
before retry selection and managed failure accounting. Generic disables remain
asynchronous. Ordinary managed failures retain their existing threshold
behavior, while multi-key Plan failures retain per-key isolation through the
same structured entry.

Managed reconciliation treats valid Plan quota metadata as separate ownership
of channel availability. Both the route transition to active and steady-state
active ranking consult the same guard before writing an enabled status.
Reconciliation continues updating route state, rank, priority, endpoint, and
model fields, but preserves the channel's auto-disabled status, quota metadata,
and disabled abilities until health-check or manual recovery clears ownership.

## Data Flow

1. An upstream response becomes a `types.NewAPIError`.
2. `ClassifyPlanQuotaError` validates status, upstream code/type, and message
   evidence; `ShouldDisableChannel` retains generic configured fallbacks.
3. `DisableChannelForAPIError` carries the observed request tag, credential,
   and parsed reset timestamp into the Plan disable path.
4. Multi-key Plan channels fall through to per-key status handling.
5. For other Plan channels, exact single-key matches are selected in Go.
6. Eligible enabled or same-marker auto-disabled snapshots build their complete
   desired status metadata with a fresh `quota_generation` and enter the
   transactional row-lock CAS.
7. The CAS updates channel status, metadata, and ability enabled state only
   while key, tag, status, and raw metadata still match the snapshot.
8. Existing retry logic sends the current request to the next eligible tier.
9. Passive recovery fails closed if managed ownership cannot be loaded, skips
   channels until `reset_at + 60 seconds`, then selects
   the oldest-tested row per marked or validated legacy domain and each
   unrelated unmanaged row independently. Validated managed Plan quota rows
   participate; ordinary managed rows do not.
10. A successful recovery probe CAS-enables its exact pre-probe source
    snapshot first. Only a successful source CAS permits current same-domain,
    same-generation, due peers to recover. Credential rotation restricts that
    recovery to the probed row, and accounting uses only committed enables.
11. Managed reconciliation may refresh routing fields but cannot enable a
    channel while valid Plan quota ownership remains.

## Tests

- Monthly reset parsing returns the exact Unix timestamp.
- Monthly quota errors trigger `ShouldDisableChannel` even when the configured
  keyword list does not contain monthly wording.
- The Plan classifier rejects an ordinary 429, a wrong status, wrong upstream
  code/type, and matching semantics without quota evidence.
- Remaining weighted-token evidence is recognized with structured 429
  `AccountQuotaExceeded` semantics.
- `AccountQuotaExceeded` from `GetErrorCode`, including a Claude error type,
  is accepted without requiring an OpenAI relay error value.
- Generic configured disable behavior remains available after classifier
  rejection.
- A Plan-tag/credential-A request followed by source tag and key rotation
  isolates only current exact-case Plan A peers and never the rotated source
  or B channels.
- Shared-credential Plan channels across different tags are disabled.
- Credential matching follows `Channel.GetKeys()`, is case-sensitive, and
  excludes multi-key channels.
- Same-tag Plan channels with another credential remain enabled.
- Non-Plan channels with the exhausted credential remain enabled.
- Empty credentials disable only the known failing channel with a
  channel-specific marker; nil channels remain unchanged.
- Matching manually disabled rows and auto-disabled rows carrying another
  marker preserve status, metadata, and abilities during a disable sweep.
- Marker-scoped recovery leaves another credential domain and manually
  disabled rows unchanged.
- Legacy markerless recovery enables only rows with explicit matching Plan
  quota metadata; generic markerless failures remain disabled.
- Passive selection deduplicates marked rows by marker and validated legacy
  rows by domain/tag while selecting unrelated failures per channel.
- Credential rotation recovers only the rotated channel and leaves peers under
  the old marker disabled.
- Stale key and stale tag snapshots make the model CAS a no-op.
- A fresh disable of an already-owned auto-disabled channel changes
  `quota_generation` and rejects a stale recovery snapshot.
- A health-check snapshot taken before a fresh disable generation cannot
  enable either its source or its peers.
- A health-check snapshot superseded by a manual source disable cannot enable
  the source or any peer.
- A source recovery cannot enable a peer with another generation or a future
  `disabled_until`; legacy marked rows with both generations absent remain
  compatible.
- Health-check summaries count committed source and peer enables, while stale
  or no-op recovery contributes zero.
- A successful model CAS updates status, raw metadata, and ability enabled
  state together.
- A stale expected status or stale expected `other_info` returns no change and
  preserves both channel and ability rows.
- A forced ability update failure rolls the transaction back without changing
  channel status or metadata.
- Legacy single-key `UpdateChannelStatus` rolls back channel, metadata, ability,
  and cache changes together and leaves no interleaving gap.
- A stale service snapshot cannot overwrite a concurrent owner or status
  change.
- Optional `TEST_MYSQL_DSN` and `TEST_POSTGRES_DSN` coverage exercises the same
  CAS against temporary migrated real-dialect channel and ability tables.
- Public `DisableChannel` preserves per-key isolation for multi-key Plan
  channels.
- Passive recovery chooses the oldest `TestTime` in a shared domain and uses
  channel ID as its deterministic tie-break.
- Passive recovery includes managed marked and validated legacy Plan quota
  rows after reset while excluding ordinary managed rows.
- Managed single-key Plan quota failures isolate shared credential peers before
  managed route failure thresholds are evaluated.
- Active managed-route reconciliation preserves valid Plan quota ownership in
  both activation-transition and steady-state ranking paths while updating
  non-status route fields.
- A managed-route query failure aborts passive selection and performs zero
  probes.
- Existing disable/enable lifecycle and no-reset behavior continue to pass.

## Operational State

Production channels `#4`, `#66`, and `#96` were immediately isolated with
`status=3`, disabled abilities, `quota_reset_at=1790783999`, and
`disabled_until=1790784059`. The code hotfix is developed separately from that
operational mutation.
