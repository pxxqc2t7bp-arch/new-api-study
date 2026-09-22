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
- Leave non-Plan channels and Plan channels with other credentials unchanged.
- Keep multi-key Plan errors on the existing per-key status path.
- Recover only auto-disabled channels from the same credential domain after
  `disabled_until`.

## Non-Goals

- Do not disable every generic HTTP 429.
- Do not change retry counts or fallback ordering.
- Do not alter channel keys, priorities, weights, models, or tags.
- Do not add a credential lookup query to the model layer.
- Do not persist raw credentials in quota metadata.
- Do not deploy the code change as part of the implementation commit.

## Design

`ParsePlanQuotaReset` will treat monthly, weekly, and 5-hour usage quota
messages as Plan quota exhaustion. `ShouldDisableChannel` will call this parser
before the configurable status-code and keyword checks so a persisted keyword
override cannot omit a supported Plan quota window.

When `DisableChannel` receives a recognized quota error for a non-multi-key
`plan:` channel, it loads all channels with credentials selected by
`model.GetAllChannels(..., selectAll=true)`. The service compares the single
value returned by `Channel.GetKeys()` case-sensitively and retains only
non-multi-key Plan channels with an exact match. Multi-key Plan errors bypass
this path and use the existing `model.UpdateChannelStatus` call with
`ChannelError.UsingKey`.

The selected channels are auto-disabled and have their abilities disabled
through `model.UpdateChannelStatus`. Existing reset metadata remains in place,
and `quota_domain_id` stores the lowercase hexadecimal SHA-256 digest of the
credential. The digest is deterministic across tags without exposing the
credential. If the failing channel has an empty credential, no broad lookup is
performed: only that known channel is disabled and marked with
`quota_domain_id=channel:<id>`. A nil channel remains a no-op.

Recovery accepts the recovering channel and reloads all channel rows so it
does not depend on potentially stale cached metadata. If the recovering row
has `quota_domain_id`, only rows with the same marker and
`ChannelStatusAutoDisabled` are enabled, including rows with different Plan
tags. Rows from another marked domain and manually disabled rows are left
unchanged. Metadata is cleared only after a selected row is actually enabled.

For rows written before `quota_domain_id` existed, recovery falls back to the
recovering channel's tag. That fallback selects only auto-disabled rows with
the same tag whose marker key is absent; it never selects a manually disabled
row or a row carrying any marker.

If loading the shared credential domain fails, the operation remains
fail-closed by logging the error and leaving the affected channels unchanged
rather than partially updating an unknown set.

## Data Flow

1. An upstream response becomes a `types.NewAPIError`.
2. `ShouldDisableChannel` recognizes the monthly quota message.
3. `DisableChannel` loads the failing channel and parses its reset timestamp.
4. Multi-key Plan channels fall through to per-key status handling.
5. For other Plan channels, exact single-key matches are selected in Go.
6. Each selected channel is auto-disabled and receives non-secret domain and
   reset metadata.
7. Existing retry logic sends the current request to the next eligible tier.
8. Passive recovery skips the channels until `reset_at + 60 seconds`.
9. A successful recovery probe enables only auto-disabled rows in the same
   marked domain, or eligible markerless rows in the legacy tag fallback.

## Tests

- Monthly reset parsing returns the exact Unix timestamp.
- Monthly quota errors trigger `ShouldDisableChannel` even when the configured
  keyword list does not contain monthly wording.
- An ordinary 429 remains retryable without becoming auto-disabled.
- Shared-credential Plan channels across different tags are disabled.
- Credential matching follows `Channel.GetKeys()`, is case-sensitive, and
  excludes multi-key channels.
- Same-tag Plan channels with another credential remain enabled.
- Non-Plan channels with the exhausted credential remain enabled.
- Empty credentials disable only the known failing channel with a
  channel-specific marker; nil channels remain unchanged.
- Marker-scoped recovery leaves another credential domain and manually
  disabled rows unchanged.
- Legacy markerless recovery enables only same-tag auto-disabled rows.
- Public `DisableChannel` preserves per-key isolation for multi-key Plan
  channels.
- Existing disable/enable lifecycle and no-reset behavior continue to pass.

## Operational State

Production channels `#4`, `#66`, and `#96` were immediately isolated with
`status=3`, disabled abilities, `quota_reset_at=1790783999`, and
`disabled_until=1790784059`. The code hotfix is developed separately from that
operational mutation.
