# Claude Opus 5.5 Official Pricing Design

## Context

The production all-enabled-model regression found that
`claude-opus-5-5` is advertised and routed to managed channels 48 and 49,
but the selected upstream returns HTTP 503 with `channel pricing
restriction`.

The production database has no explicit price for this model. Self-use mode
currently supplies a generic fallback ratio, which is sufficient for route
eligibility but is not authoritative pricing.

Anthropic's official pricing page lists the standard, global Claude API price:

|Usage|USD per million tokens|
|---|---:|
|Input|4.00|
|Output|20.00|
|5-minute cache write|5.00|
|1-hour cache write|8.00|
|Cache read|0.20|

Source:
`https://platform.claude.com/docs/en/about-claude/pricing`

Fast mode, batch pricing, data-residency premiums, Bedrock pricing, Vertex AI
pricing, discounts, and negotiated reseller pricing are outside this change.

## Goals

- Add the exact standard official price for `claude-opus-5-5`.
- Change no other model's pricing.
- Use the existing model-level compare-and-swap pricing transaction.
- Preserve a pre-change snapshot and post-change audit evidence.
- Run one pinned production canary after the write.
- Distinguish a correct local price from upstream account support.

## Non-Goals

- Do not run the all-vendor official-pricing synchronization job.
- Do not change channel models, abilities, aliases, priorities, or status.
- Do not change Ark endpoint mappings.
- Do not infer or modify the upstream reseller's internal price policy.
- Do not retry a write whose outcome is unknown.
- Do not claim that adding local pricing guarantees upstream support.

## Pricing Representation

Use the existing tiered-expression billing mode because it preserves all five
official token categories exactly, including separate 5-minute and 1-hour
cache-write prices:

```text
tier("official", p*4 + cr*0.2 + cc*5 + cc1h*8 + c*20)
```

The model-level pricing draft contains only:

```json
{
  "billing_setting.billing_mode": "tiered_expr",
  "billing_setting.billing_expr": "tier(\"official\", p*4 + cr*0.2 + cc*5 + cc1h*8 + c*20)"
}
```

No `ModelRatio`, `CompletionRatio`, `CacheRatio`, or `CreateCacheRatio` entry
is added. This avoids maintaining duplicate legacy and expression pricing.
Managed route eligibility already treats a configured billing expression as
an explicit price.

## Execution Flow

1. Acquire `/opt/newapi-study/.pricing-change.lock` with an owner file that
   identifies the model and captured CAS version.
2. Assert the host identity, deployed immutable revision, container health,
   deployment-lock absence, and unchanged status of channels 4, 66, and 96.
3. Capture the current model-pricing snapshot, its CAS version, the relevant
   option-row hashes, and a PostgreSQL dump.
4. Load the allowlisted Anthropic pricing page and require the exact
   `Claude Opus 5.5` row with all five expected prices. The default path
   fetches `SOURCE_URL` directly. If the production host is redirected by a
   regional availability policy, an operator may download the unchanged HTML
   from `SOURCE_URL` on the trusted local workstation, upload it as a private
   regular file, and provide both its absolute path and SHA-256. The
   production script must verify the file owner, `0600` mode, single-link
   status, bounded size, and exact hash before parsing the HTML itself.
5. Submit the proposed draft to
   `POST /api/option/model_pricing/preview`.
6. Require the preview to compile to the exact expression above and report
   token billing with no task or request-based billing fields.
7. Submit exactly one
   `PATCH /api/option/model_pricing` request using the captured expected
   version.
8. Read the model snapshot again and verify that:
   - the configured mode and expression match exactly;
   - the version changed;
   - no legacy ratio field was added for this model;
   - hashes for unrelated pricing entries are unchanged.
9. Execute exactly one `/v1/chat/completions` canary pinned to channel 48.
10. Record the HTTP status, request ID, selected channel, official URL,
   normalized official price, page hash, snapshots, and artifact hashes.
11. Release the pricing lock only after the write outcome and all evidence
    paths are known.

The script uses an inline local API and PostgreSQL adapter fixed to the
production loopback proxy. Credentials remain inside the process and must not
be printed, stored in the report, or passed on the command line.

The pinned-source fallback accepts raw official HTML only. It must never accept
a precomputed price object or normalized evidence as a substitute for parsing
the official row. The source path and expected SHA-256 are recorded in the
audit evidence without copying credentials or unrelated local data.

## Failure Handling

### Preflight Or Preview Failure

Perform no write. Record the failure and leave production unchanged.

If a known pre-write failure retains the pricing lock, retry is permitted only
after all of the following are proven: the target snapshot is still the empty
pre-change version, no PATCH evidence exists, no canary was attempted, and the
lock owner matches the failed run and output directory. Preserve the failed
run by atomically renaming its active lock to a unique
`.pricing-change.failed.<run-id>` archive and fsyncing the parent directory.
Never delete or overwrite the retained lock. The replacement run must use a
new output directory and still permits at most one PATCH and one canary across
all attempts.

### CAS Conflict

Perform no retry. Reload and report the conflicting state for review.

### Unknown Write Outcome

Do not resend the PATCH. Read the model snapshot:

- Exact target expression present: classify the write as committed.
- Pre-change version and empty configuration present: classify it as not
  committed.
- Any other state: retain `/opt/newapi-study/.pricing-change.lock` and stop for
  manual review.

### Canary Failure

Do not retry the canary.

The official price remains stored if the local pricing snapshot is correct.
An upstream `channel pricing restriction` still proves that the managed
provider does not support the model under its own account policy; it does not
invalidate Anthropic's official price. The all-enabled-model release gate
remains blocked until upstream support is independently established.

### Rollback

Rollback is allowed only when the committed local pricing differs from the
approved expression or breaks local billing evaluation. Use a fresh CAS
snapshot and restore the pre-change empty configuration exactly once.

Do not roll back solely because the upstream canary returns the pre-existing
503.

## Verification

- Preview returns the exact configured expression.
- Post-write readback contains only the two approved billing fields.
- A local quote fixture evaluates:
  - 1M input tokens to USD 4;
  - 1M output tokens to USD 20;
  - 1M cache-read tokens to USD 0.20;
  - 1M 5-minute cache-write tokens to USD 5;
  - 1M 1-hour cache-write tokens to USD 8.
- Unrelated pricing-option hashes remain unchanged.
- Channels 4, 66, and 96 remain status 3 with zero enabled abilities.
- `plan_quota_domains` remains `active:3, disabled:1`.
- The application remains healthy with zero restarts.
- No temporary probe token remains.

## Acceptance Criteria

- Exactly one model is changed: `claude-opus-5-5`.
- The stored expression matches the official standard price exactly.
- The pricing write is committed at most once.
- The channel 48 canary is issued at most once.
- The canary result is reported without conflating local price correctness
  with upstream provider availability.
- Compose, database dump, pricing evidence, reports, and SHA-256 manifests are
  retained under the existing production audit directory.
- When the pinned-source fallback is used, the production parser validates the
  exact uploaded HTML bytes against the operator-supplied SHA-256 before any
  pricing snapshot or PATCH request.
