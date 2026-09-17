# Production Model Alias Normalization

Date: 2026-09-17

## Goal

Expose only stable model aliases from NewAPI. Provider release dates, GA tags,
preview/build identifiers, and GPT generation codenames must remain internal
channel details and must not appear in `/v1/models`.

The public aliases must be real routing entries. Every alias maps to a concrete
upstream model on each eligible channel; no alias may be published solely as a
display-name rewrite.

## Scope

This change applies to every enabled production model and both production
groups, `default` and `cxy`.

Initial required aliases include:

- `gpt-5.6-luna`, `gpt-5.6-sol`, and `gpt-5.6-terra` -> `gpt-5.6`
- `gpt-6-astra` -> `gpt-6`
- the deployed DeepSeek v4 Flash base, dated, and GA revisions ->
  `deepseekv4.1flash`
- dated Claude, GPT, Gemini, DeepSeek, and other provider revisions -> their
  stable family or capability alias

Capability SKUs remain distinct. Names such as `mini`, `nano`, `pro`, `vision`,
`audio`, `realtime`, `reasoning`, and `codex` are capabilities, not release
suffixes, and are retained.

## Architecture

Use channel-level aliases rather than changing the NewAPI routing core.

For each enabled channel:

1. Read its current `models`, `model_mapping`, groups, priority, weight, and
   recent request results.
2. Classify each concrete model ID into a canonical public alias.
3. Replace the channel's routable model entries with canonical aliases.
4. Add `model_mapping` entries from each alias to the selected concrete
   upstream ID.
5. Preserve unrelated channel settings, credentials, protocol routes,
   priorities, weights, and existing mappings.
6. Rebuild or verify `abilities` through the supported channel update path.

Because `/v1/models` is built from enabled abilities, publishing only canonical
abilities removes concrete revisions from the public list and makes every
listed alias routable.

## Canonicalization Rules

Canonicalization is inventory-driven and conservative. It uses an audited
manifest rather than blindly stripping the last hyphenated token.

Remove release-only suffixes such as:

- ISO dates: `-2026-03-17`
- compact dates: `-20251001`, `-260425`
- release tags followed by dates: `-ga-260731`
- provider build or snapshot identifiers confirmed by the inventory
- GPT generation codenames: `-luna`, `-sol`, `-terra`, and `-astra`

Retain tokens that identify a materially different capability or price tier,
including `mini`, `nano`, `pro`, `vision`, `audio`, `realtime`, `reasoning`,
and `codex`.

Explicit overrides take precedence over generic rules. In particular, all
deployed DeepSeek v4 Flash revisions use the exact public alias
`deepseekv4.1flash`.

## Collision Selection

A channel can map one public alias to only one concrete upstream ID. When
multiple concrete IDs on the same channel collapse to one alias, select the
target in this order:

1. An existing explicit stable upstream ID with recent successful requests.
2. The concrete revision with the most recent successful production request.
3. The newest version according to its parsed release date or build number.

If no candidate has a successful request and a safe probe cannot verify one,
do not publish that alias on the channel. Keep the channel unchanged for that
alias and report the blocker.

`gpt-5.6` on rvcompute channel 92 must map to a verified available model such
as `gpt-5.6-sol` or `gpt-5.6-terra`; it must not map to the unavailable
`gpt-5.6-luna`. `deepseekv4.1flash` must use another channel that actually
provides a verified DeepSeek v4 Flash revision because rvcompute currently
advertises no DeepSeek model.

## Pricing

Each canonical alias must have an effective billing configuration before it is
enabled. Reuse the selected concrete model's existing ratio or billing
expression. Do not silently invent a price.

Keep concrete pricing entries when they are needed for historical logs or
rollback, but public routing and model listing use the canonical alias.

## Safety And Rollback

Before mutation, capture:

- `/opt/newapi-study/docker-compose.yml`
- a PostgreSQL database dump
- a redacted channel and ability snapshot
- the alias manifest and selected concrete target for every collision

Generate and verify SHA-256 values for the Compose file, database backup,
configuration artifacts, audit report, and extension ZIP when present.

Apply all channel updates transactionally or through idempotent management API
operations. Save a rollback manifest containing the original `models`,
`model_mapping`, abilities, pricing options, groups, priorities, and weights.

No channel credential is written to reports.

## Verification

The production gate requires:

1. `/v1/models` in `default` and `cxy` contains canonical aliases and contains
   no mapped concrete release identifiers.
2. `gpt-5.6`, `gpt-6`, and `deepseekv4.1flash` each complete real non-streaming
   requests.
3. Supported aliases complete streaming and Responses API checks where their
   selected channels support those protocols.
4. Request logs show the expected channel and concrete upstream mapping.
5. Automatic routing still prefers rvcompute channel 92 for its verified GPT
   aliases and falls back only on timeout or retryable error.
6. Three consecutive regression rounds cover every enabled canonical model.
7. Any failed alias blocks publication of that alias and leaves its original
   channel configuration recoverable.

## Delivery

The implementation is a new idempotent production migration and verification
script. Existing strict rvcompute scripts remain as historical evidence and
are not weakened. The migration first performs a dry run that emits the full
alias manifest and collision decisions, then requires an explicit apply step.
