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
- Treat an observed empty credential as source-only only while the source
  still has that exact empty credential and observed Plan tag.
- Prevent managed reconciliation from enabling a channel while valid Plan
  quota ownership remains.
- Prevent managed reconciliation from enabling an all-disabled multi-key
  channel, while allowing reconciliation when at least one key remains
  enabled.
- Preserve immediate per-key isolation for structured Plan quota errors on
  managed multi-key channels, bypassing managed failure thresholds.
- Count only health-check recoveries that actually commit, and fail passive
  selection closed when managed-route ownership cannot be queried.
- Honor the global automatic-disable switch at the structured Plan service
  entry so direct callers cannot mutate channel, ability, or route state while
  automatic disable is off.
- Make a fresh Plan quota generation serialize with recovery and membership
  changes through one durable authority lock.
- Fence managed multi-key Plan isolation against request-time tag, key
  membership, and multi-key mode rotation.
- Automatically probe an auto-disabled multi-key channel after its final
  enabled key is disabled without exposing that key to normal routing.
- Persist a known structured Plan reset deadline when that final-key disable
  makes the overall channel auto-disabled, without assigning shared-domain
  ownership to the multi-key row.
- Admit each due managed all-disabled multi-key channel to passive recovery as
  its own candidate even though per-key isolation has no quota-domain marker.
- Restore or remove memory-cache routing membership for every production
  status transition, including legacy and snapshot-CAS enable paths.
- Use route-before-channel row-lock ordering for managed reconciliation and
  unsupported-model isolation.
- Serialize shared single-key Plan domain membership across processes so a
  concurrent create or credential/tag/mode rotation cannot escape an active
  disable.
- Keep a durable, non-secret domain authority that disable, recovery, create,
  and identity mutation all consult before changing channel membership.

## Non-Goals

- Do not disable every generic HTTP 429.
- Do not change retry counts or fallback ordering.
- Do not alter channel keys, priorities, weights, models, or tags.
- Do not persist raw credentials in quota metadata or the domain authority
  table.
- Do not claim safety for out-of-band SQL writers or a rolling deployment that
  leaves old application writers active after the authority migration.
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
channel. The model transaction compares each current single credential against
the observed request credential case-sensitively while holding that
credential's durable domain authority lock. It never substitutes later
reloaded source identity. Therefore a request sent under Plan tag/key A cannot
disable a source whose tag and key rotated to an ordinary/B identity, or any B
peer, while current Plan peers still on A are isolated. Multi-key Plan errors
bypass the shared-domain path and retain existing per-key status handling.

The selected channels are auto-disabled and have their abilities disabled
through a model compare-and-swap operation. Existing reset metadata remains in
place, and `quota_domain_id` stores the lowercase hexadecimal SHA-256 digest of
the credential. The digest is deterministic across tags without exposing the
credential. If the request observed an empty credential, no broad lookup is
performed. The known source is disabled with
`quota_domain_id=channel:<id>` only when its freshly loaded row remains
non-multi-key, has the exact empty raw key, and retains the exact observed Plan
tag. A source whose key or tag rotated is left enabled and unmarked. A nil
channel remains a no-op. The sweep may write only an enabled channel or an
auto-disabled channel that already carries the same `quota_domain_id`. It
leaves manually disabled rows and auto-disabled rows owned by another marker
unchanged, including their metadata and ability state.

### Persistent Domain Authority

`plan_quota_domains` contains one permanent authority row for each non-empty,
single-key Plan credential hash:

```text
credential_hash char(64) primary key
generation      bigint not null
state           varchar(16) not null
disabled_until  bigint not null
```

`credential_hash` is the existing lowercase SHA-256 marker. The table never
stores the credential, channel tag, upstream error, or any other secret.
`state` is `active` or `disabled`. A disable allocates a decimal
Unix-nanosecond generation that is strictly newer than the stored generation;
the same decimal value is projected into each owned channel's
`quota_generation`. Recovery advances the authority generation again and
returns the row to `active`, preventing a stale disable or recovery snapshot
from inheriting the new state. Authority rows are retained permanently so lock
identity cannot be deleted and recreated.

Startup migration creates the table through GORM naming strategy without a
fixed `TableName`, then backfills one authority per current non-empty,
single-key `plan:` credential before the process serves writes. Existing
matching quota-owned rows make the authority fail closed as `disabled`; the
backfill uses the newest valid generation and latest known deadline, and
malformed or conflicting ownership cannot produce an active authority. Only
counts are logged. Existing Plan members without an authority after startup
are an invariant violation: mutation and recovery fail closed.

All participating transactions acquire database locks in this order:

```text
sorted credential hashes
  -> source -> group -> route, when a managed decision is involved
  -> sorted channel IDs
  -> abilities
```

Create may insert a previously unseen authority as `active`; the unique
primary key serializes concurrent first members. Updates derive old and new
hashes from complete snapshots and lock their union lexicographically before
locking the channel. Entering a disabled authority projects its generation and
deadline into the channel and cannot publish enabled abilities. Batch create,
copy, bulk tag changes, managed enrollment, and Codex key rotation use the same
protocol. Transaction or commit errors are returned without automatic retry;
a stale snapshot is a conflict and performs no write.

### Atomic Ownership Transition

The disable operation locks or safely creates the observed credential
authority, advances it to `disabled`, then discovers and locks all current
exact members inside that transaction. A writer cannot enter or leave the
domain until the operation commits. The transaction proceeds only while the
failing request identity and every selected channel's complete persisted
snapshot remain valid. It updates authority state, `channels.status`, full
`other_info`, and all corresponding `abilities.enabled` rows together. Any
identity, status, metadata, channel-info, channel, ability, or commit error
rolls back the whole domain. Cache publication and notification occur only
after commit.

Every Plan quota disable event writes one new decimal Unix-nanosecond string
as `quota_generation`, including when a channel is already auto-disabled and
owned by the same quota domain. This guarantees that a fresh disable changes
the exact raw metadata expected by recovery. A recovery snapshot taken before
that event therefore loses its CAS and cannot re-enable the channel. Successful
recovery removes `quota_generation` with the other quota ownership fields.

There is no per-member retry loop. The authority row serializes membership,
and one full-domain transaction either commits the new generation for every
eligible member or changes nothing. A manual disable, another quota owner, or
key/tag/mode rotation makes the full snapshot ineligible without mutation.

The non-multi-key branch of legacy `UpdateChannelStatus` uses a bounded
snapshot/CAS retry over the same internal lock and transaction primitive.
Status, `other_info`, and abilities commit together, and cache status changes
only after commit. The existing multi-key status-list behavior remains on its
established path.

Plan disable and recovery no longer call `UpdateChannelStatus` followed by
`MergeChannelStatusMetadata`. A concurrent manual disable, unrelated
auto-disable marker, or metadata edit invalidates the snapshot and makes the
attempt a no-op.

Automatic channel tests retain the original pre-probe `Channel` snapshot for
recovery. When that snapshot is an auto-disabled multi-key channel with no
enabled keys, the controller builds a health-check-only copy with a deep copy
of every `ChannelInfo` map. It chooses one auto-disabled key by the oldest
`MultiKeyDisabledTime`, breaking equal timestamps by lower key index, and marks
only that key enabled in the copy. The copy uses a non-persisting probe
selection mode, so key status and polling state in the source snapshot,
database, and cache remain untouched by setup and probing. A failed probe
performs no status mutation.

After a successful probe, the controller calls `EnableChannelForHealthCheck`
with the original pre-probe snapshot and the key actually selected for the
request. Multi-key recovery uses the same identity-fenced model CAS as
structured Plan disable handling. It enables exactly the selected key only
while key, tag, multi-key mode, status metadata, and complete `ChannelInfo`
still match the original snapshot; any stale snapshot or key, tag, or mode
rotation is a no-op.

For non-multi-key channels, quota ownership and recovery scope are derived
from that pre-probe snapshot. Before any status write, the service rejects a
source snapshot whose `disabled_until` is still in the future. It returns zero
without changing the source, peers, metadata, or abilities. Once due, the
service passes the exact key, tag, status, raw `other_info`, and generation to
the authority-backed recovery transaction. A stale source or authority makes
the whole operation a no-op; the call never falls through to generic ID-based
enablement. This fences a successful stale probe from a newer quota
generation, a manual disable, or an unrelated ownership change.

Automatic shared-domain recovery locks the authority first, requires its
disabled generation and deadline to match the pre-probe source snapshot, then
locks every current member and applicable managed route. It validates the
source's complete snapshot and advances the authority to `active` in the same
transaction that clears matching quota ownership. Routeable members are
enabled with their abilities; members still held by an independent managed
route state remain disabled but lose only the expired quota owner. A writer
that joins before the authority lock is included, while a writer that joins
after commit observes `active` and may remain enabled. A newer generation or
future authority deadline makes the entire recovery a no-op. Generic
single-key health-check recovery remains snapshot-bound; multi-key recovery
retains the existing per-key path. `EnableChannelForHealthCheck` returns the
number of members actually enabled, and controller accounting adds that value
rather than counting the probe attempt. Manual `EnableChannel` uses the same
domain transaction without the deadline gate.

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
Ordinary managed channels remain excluded from passive channel tests. A due
managed channel is included when the classifier recognizes a marked or
validated legacy Plan quota owner. A due auto-disabled managed multi-key
channel with no enabled key is also included even without a quota marker,
using its channel ID as the recovery key so same-credential channels are not
deduplicated. Managed multi-key rows with an enabled key remain excluded. This
allows both all-managed quota domains and per-key final-key isolation to
recover without depending on the managed route's `next_probe_at`. Selection
returns an error if the managed-route ID query fails; the system task
propagates that error and starts no probes rather than treating the managed set
as empty.

Before marker-scoped health-check recovery, the service computes a marker from
the recovering snapshot's single credential and compares it with the
snapshot's persisted `quota_domain_id`. If credential rotation changed the
marker, only the recovering source is enabled and has its quota metadata
cleared. Peers that still carry the old marker remain auto-disabled.

If authority, member, route, channel, ability, or commit work fails, recovery
rolls back in full and leaves the domain disabled. It never reports or
publishes partial recovery.

Managed-route error handling retains unsupported-model isolation as its first
special case. A recognized Plan quota error is then sent through
`DisableChannelForAPIError` synchronously, where Plan domain isolation runs
before retry selection and managed failure accounting. Generic disables remain
asynchronous. Ordinary managed failures retain their existing threshold
behavior. A structured Plan quota error for a managed multi-key channel calls
an identity-fenced model CAS immediately with the request's `UsingKey`, then
returns without incrementing the managed failure threshold. The CAS holds the
existing channel status locks, locks the current channel row, and requires the
request-observed tag, multi-key mode, and used-key membership to remain valid.
It also compares the snapshot's key, status, raw status metadata, and complete
`channel_info` before atomically persisting the per-key state and any required
ability transition. A tag rotation or multi-key-to-single-key rotation is a
no-op, so a credential introduced after request selection is never disabled.
After commit, the full updated channel replaces the memory-cache entry so the
new per-key status remains available. A single lock-scoped routing-index helper
removes every occurrence of the channel ID from all group/model entries, then
re-adds it from the cached channel's group, model, status, and priority fields
only when the channel is enabled. Both status-only cache updates and full
channel replacements use this symmetric helper when routing state changes.
Legacy `UpdateChannelStatus`, manual `EnableChannel`, and snapshot-CAS health
recovery therefore restore enabled routing membership, remove disabled
membership, preserve priority ordering, and avoid duplicate IDs. The general
multi-key status API remains unchanged.

The identity-fenced multi-key CAS accepts focused status-metadata options.
After applying the selected key transition, it merges `quota_reset_at` and
`disabled_until=quota_reset_at+60` only when a structured Plan update has a
known reset and leaves the overall channel auto-disabled. A multi-key channel
with another enabled key receives no deadline or shared Plan ownership fields.
An unknown reset retains the existing no-deadline behavior, so passive
selection remains immediately eligible. The reset fields commit in the same
transaction as overall status, complete per-key state, and abilities. They do
not contain raw credentials and do not add `quota_domain`,
`quota_domain_id`, `quota_generation`, or `quota_type`.

When an isolated health-check recovery enables the overall channel, the same
CAS removes only `quota_reset_at` and `disabled_until`. All unrelated
`other_info` survives, and a stale request identity or channel snapshot still
causes a no-op before any metadata, key status, ability, or cache change.

`DisableChannelForAPIError` rejects the structured path before classification
or mutation when `common.AutomaticDisableChannelEnabled` is false. This guard
lives at the public service entry rather than only in controller policy, so
relay, health-test, and future direct callers all obey the global switch.

Managed reconciliation treats valid Plan quota metadata as separate ownership
of channel availability. It also treats a multi-key channel with no enabled
key as the owner of its disabled state, even though per-key isolation does not
write single-key Plan ownership metadata. A multi-key channel with any enabled
key is not preserved by this rule. A transition to active no longer performs a
pre-ranking enable. Final rank reconciliation reads a channel snapshot,
calculates the desired status while preserving either owner, and calls a
model-owned compare-and-swap. The model holds the existing channel status
locks, starts one transaction, reads through `lockForUpdate`, and compares key,
tag, status, and raw metadata. Its conditional channel update supplies the
SQLite optimistic fence; MySQL and PostgreSQL also hold the row lock. A stale
snapshot returns no change, and the service retries from fresh state.

On a matching snapshot, route rank and multiplier, channel priority, endpoint,
models and status, plus regenerated abilities commit in the same transaction.
Thus a concurrent quota disable either wins before the locked comparison and is
preserved, or commits after reconciliation; reconciliation cannot overwrite it.
Reconciliation still updates route state separately and retains ordinary
managed behavior, but valid quota ownership keeps channel status and abilities
disabled until health-check or manual recovery clears ownership.

The managed reconciliation transaction locks its
`upstream_managed_routes` row before its `channels` row, matching
unsupported-model isolation. It validates that the locked route still belongs
to the expected channel, then compares the channel snapshot and atomically
updates route rank, channel routing/status fields, and abilities. The order and
atomicity are identical on MySQL and PostgreSQL; SQLite omits unsupported
`FOR UPDATE` syntax and retains the same transactional operation order.

## Data Flow

1. An upstream response becomes a `types.NewAPIError`.
2. `ClassifyPlanQuotaError` validates status, upstream code/type, and message
   evidence; `ShouldDisableChannel` retains generic configured fallbacks.
3. `DisableChannelForAPIError` carries the observed request tag, credential,
   and parsed reset timestamp into the Plan disable path, but returns without
   mutation when global automatic disable is off.
4. Multi-key Plan channels immediately use per-key status handling, including
   managed channels below their failure threshold, only if current tag, key
   membership, and multi-key mode still match the failed request. A committed
   final-key transition also persists a known reset and its 60-second recovery
   grace in the same transaction as disabled abilities. A partial key disable
   and an unknown reset write no overall deadline. The committed overall
   status transition synchronizes both the cached channel state and its
   group/model routing membership.
5. For other Plan channels, the model locks the non-secret credential
   authority, marks it disabled with a fresh generation, and discovers exact
   single-key members inside the same transaction.
6. Create and identity-changing writers lock the same authority before
   inserting or moving membership. A writer that enters a disabled domain is
   committed only as quota-owned and non-routeable.
7. The full-domain transaction updates authority, channel status and metadata,
   plus ability state only while complete locked snapshots remain valid.
8. Existing retry logic sends the current request to the next eligible tier.
9. Passive recovery fails closed if managed ownership cannot be loaded, skips
   channels until `reset_at + 60 seconds`, then selects
   the oldest-tested row per marked or validated legacy domain and each
   unrelated unmanaged row independently. Validated managed Plan quota rows
   participate. Due managed all-disabled multi-key rows participate per channel
   without shared-domain deduplication; ordinary managed rows and multi-key
   rows with an enabled key do not.
10. A health-check recovery first rejects a not-yet-due source snapshot. Once
    due, an all-disabled multi-key source is probed through a deep-copied
    snapshot containing one deterministically selected temporary enabled key.
    A successful probe CAS-enables that exact key against the original
    snapshot. Single-key recovery CAS-enables its exact pre-probe source
    snapshot and disabled authority. One successful transaction advances the
    authority and recovers all matching due members. Credential rotation
    restricts recovery to the probed row, and accounting uses only committed
    enables.
11. Managed reconciliation omits the pre-rank activation enable and uses one
    model-owned locked CAS transaction for rank, channel routing/status fields,
    and abilities. Valid Plan ownership survives either ordering of a
    concurrent disable, and a multi-key channel remains disabled while every
    configured key is disabled.

## Tests

- Monthly reset parsing returns the exact Unix timestamp.
- Monthly quota errors trigger `ShouldDisableChannel` even when the configured
  keyword list does not contain monthly wording.
- A qualifying structured Plan quota error causes no channel, ability, or
  managed-route mutation when global automatic disable is off.
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
  channel-specific marker when key and tag are unchanged; key-only, tag-only,
  or combined rotation leaves the source enabled and unmarked. Nil channels
  remain unchanged.
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
- A fresh disable and recovery of the same domain serialize on the authority
  row; exactly one generation transition commits and no member is partially
  published.
- Manual disable, unrelated quota ownership, or key/tag/multi-key-mode
  rotation invalidates the full-domain snapshot without mutation.
- A health-check snapshot taken before a fresh disable generation cannot
  enable either its source or its peers.
- A health-check snapshot superseded by a manual source disable cannot enable
  the source or any peer.
- A source recovery cannot enable a peer with another generation or a future
  `disabled_until`; a source with a future `disabled_until` returns zero and
  preserves the whole domain. Legacy marked rows with both generations absent
  remain compatible, and manual enable remains an override.
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
- Optional `TEST_MYSQL_DSN` and `TEST_POSTGRES_DSN` coverage exercises both
  status CAS paths against temporary migrated real-dialect route, channel, and
  ability tables.
- Public `DisableChannel` preserves per-key isolation for multi-key Plan
  channels.
- Managed structured Plan quota handling immediately disables only the used key
  and does not increment the managed failure counter while another key remains.
- Managed structured Plan quota handling is a no-op after tag rotation or
  multi-key-to-single-key rotation, and never disables a replacement
  credential.
- With memory caching enabled, disabling the final enabled key removes the
  channel from cached selection while preserving the committed per-key state;
  re-enabling a key through `EnableChannelForHealthCheck` or `EnableChannel`
  restores routing membership in priority order without duplication.
- A structured final-key error with a known reset atomically persists
  `quota_reset_at` and `disabled_until=reset+60` with per-key, overall status,
  and ability disable state; passive recovery does not probe before that
  deadline. A partial multi-key disable receives no deadline or shared-domain
  marker, and an unknown reset retains no deadline.
- An all-disabled multi-key health check probes the oldest auto-disabled key,
  breaks timestamp ties by lower index, and does not mutate the source
  snapshot, database key status, or cache before recovery.
- A failed final-key probe preserves every disabled key. A successful probe
  enables only the selected key and clears only the overall reset deadline
  fields, while stale channel info and key, tag, or multi-key mode rotation
  make recovery a no-op.
- Passive recovery chooses the oldest `TestTime` in a shared domain and uses
  channel ID as its deterministic tie-break.
- Passive recovery includes managed marked and validated legacy Plan quota
  rows after reset while excluding ordinary managed rows.
- Passive recovery selects each due managed all-disabled multi-key row through
  the system task exactly once, without a quota marker or credential-domain
  deduplication, while excluding not-yet-due rows and rows with an enabled key.
- Managed single-key Plan quota failures isolate shared credential peers before
  managed route failure thresholds are evaluated.
- Active managed-route reconciliation preserves valid Plan quota ownership in
  deterministic activation-transition and steady-state interleavings while
  atomically updating route rank, priority, endpoint, models, and abilities.
- Structured final-key disable followed by healthy managed reconciliation
  leaves the channel auto-disabled, abilities disabled, and cache routing
  excluded; the isolated probe then recovers exactly its selected key and
  restores the route. A multi-key channel with an enabled key is not preserved.
- The model managed-channel CAS rejects stale status ownership without changing
  route rank, channel routing fields, or abilities, and commits all three on a
  fresh snapshot.
- Managed reconciliation observably locks the managed route before the channel
  and preserves atomic route, channel, and ability updates.
- A managed-route query failure aborts passive selection and performs zero
  probes.
- A create that races an active domain disable either commits before the
  disable and is included in the sweep, or commits afterward already
  quota-disabled with disabled abilities.
- A key, tag, or single/multi-key mode rotation into a disabled domain cannot
  publish an enabled channel; rotations between two domains lock hashes in
  lexical order.
- Recovery serializes with concurrent create and identity rotation so every
  committed member observes either the disabled generation or the recovered
  active authority.
- Startup backfill creates prefixed authority tables, reconstructs disabled
  state without persisting credentials, and fails closed for malformed or
  conflicting ownership.
- Domain, channel, and ability writes roll back together on injected failures,
  and transaction/commit errors are never automatically retried.
- Optional MySQL and PostgreSQL tests exercise the same authority row protocol
  against isolated prefixed tables.
- Existing disable/enable lifecycle and no-reset behavior continue to pass.

## Operational State

Production channels `#4`, `#66`, and `#96` were immediately isolated with
`status=3`, disabled abilities, `quota_reset_at=1790783999`, and
`disabled_until=1790784059`. The code hotfix is developed separately from that
operational mutation.
