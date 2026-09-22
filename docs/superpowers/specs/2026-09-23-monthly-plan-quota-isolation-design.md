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
- Auto-disable every Plan channel that uses the same credential as the failing
  channel, even when those channels use different Plan tags or protocols.
- Leave non-Plan channels and Plan channels with other credentials unchanged.
- Preserve the existing passive-recovery behavior after `disabled_until`.

## Non-Goals

- Do not disable every generic HTTP 429.
- Do not change retry counts or fallback ordering.
- Do not alter channel keys, priorities, weights, models, or tags.
- Do not deploy the code change as part of the implementation commit.

## Design

`ParsePlanQuotaReset` will treat monthly, weekly, and 5-hour usage quota
messages as Plan quota exhaustion. `ShouldDisableChannel` will call this parser
before the configurable status-code and keyword checks so a persisted keyword
override cannot omit a supported Plan quota window.

When `DisableChannel` receives a recognized quota error for a `plan:` channel,
it will load channels with the exact same stored credential value and retain
only channels whose tags start with `plan:`. It will auto-disable those
channels, disable their abilities through `model.UpdateChannelStatus`, and
store each channel's own tag as `quota_domain`. This keeps the existing
tag-based passive recovery behavior: after the reset grace period, one channel
per tag is tested, and a successful test re-enables the channels in that tag.

If loading the shared credential domain fails, the operation remains
fail-closed by logging the error and leaving the affected channels unchanged
rather than partially updating an unknown set.

## Data Flow

1. An upstream response becomes a `types.NewAPIError`.
2. `ShouldDisableChannel` recognizes the monthly quota message.
3. `DisableChannel` loads the failing channel and parses its reset timestamp.
4. Plan channels with the exact same credential are selected.
5. Each selected channel is auto-disabled and receives quota metadata.
6. Existing retry logic sends the current request to the next eligible tier.
7. Passive recovery skips the channels until `reset_at + 60 seconds`.

## Tests

- Monthly reset parsing returns the exact Unix timestamp.
- Monthly quota errors trigger `ShouldDisableChannel` even when the configured
  keyword list does not contain monthly wording.
- An ordinary 429 remains retryable without becoming auto-disabled.
- Shared-credential Plan channels across different tags are disabled.
- Same-tag Plan channels with another credential remain enabled.
- Non-Plan channels with the exhausted credential remain enabled.
- Existing disable/enable lifecycle and no-reset behavior continue to pass.

## Operational State

Production channels `#4`, `#66`, and `#96` were immediately isolated with
`status=3`, disabled abilities, `quota_reset_at=1790783999`, and
`disabled_until=1790784059`. The code hotfix is developed separately from that
operational mutation.
