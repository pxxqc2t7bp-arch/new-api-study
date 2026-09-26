# ZCode Responses Input-Item Compaction

Date: 2026-09-26

## Goal

Prevent long, tool-heavy ZCode sessions from becoming permanently unusable
when Volcengine Ark rejects an OpenAI Responses `input` array containing more
than 1000 items.

The fix must:

- trigger ZCode's existing reactive compaction before the Ark hard limit;
- normalize hard-limit overflow into the standard
  `context_length_exceeded` error contract;
- preserve every input item until ZCode explicitly compacts the conversation;
- avoid patching the signed ZCode application or its auto-updated minified
  runtime bundle.

## Confirmed Failure

Production evidence established a deterministic boundary:

- 1000 input items succeeded with 302,825 input tokens;
- the next request contained 1004 items and failed with HTTP 400;
- a later request contained 1006 items and failed identically;
- the 1004-item request contained 432 function calls, 432 matching function
  outputs, 70 assistant messages, 67 user messages, and 3 system messages;
- the 1004-item array was the exact 1000-item prefix plus four new items.

NewAPI's Ark history normalizer preserved the array length. ZCode 3.14.3 did
not compact because its configured 1,048,576-token context produces an
automatic compaction threshold near 1,014,576 tokens. The independent Ark
item-count limit was reached first.

## Implementation Boundary

No maintainable ZCode core source tree is present on this machine. The
installed runtime is a generated 14 MB `zcode.cjs` asset under ZCode's
auto-updated cache. Modifying that file would be overwritten by updates and
would not provide a reviewable source patch.

The maintained implementation therefore has two parts:

1. NewAPI exposes an input-item limit contract and returns a standard context
   overflow error without forwarding an oversized request.
2. The ZCode Responses provider declares a 900-item soft limit through a
   custom request header. ZCode then uses its existing, maintained reactive
   compaction path when NewAPI returns `context_length_exceeded`.

This implements item-aware ZCode behavior without changing ZCode's binary.
An upstream ZCode core change can later replace the header contract with a
native provider capability while retaining the same 900-item policy.

## Request Contract

The client opt-in header is:

`X-NewAPI-Responses-Input-Item-Soft-Limit: 900`

Rules:

- The header applies only to `POST /v1/responses`.
- It applies only when the resolved upstream URL is an official regional Ark
  host matching `ark.<region>.volces.com`.
- It applies only when `input` is an array. Scalar string input is unchanged.
- A valid soft limit is an integer from 1 through 1000.
- A malformed or out-of-range header fails locally with an
  `invalid_request_error`; it is never forwarded upstream.
- The effective limit is 1000 when the header is absent.
- The configured soft limit replaces the effective limit when present.
- A request with exactly the effective limit is accepted.
- A request above the effective limit is rejected locally.
- The opt-in header is removed before the upstream request is created.
- Pass-through body mode must not bypass this validation.

The 900-item value leaves a 100-item margin for the compaction summary request,
recent-message preservation, and small differences in provider message
encoding.

## Error Contract

An item-limit rejection returns HTTP 400 with:

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "context_length_exceeded",
    "param": "input",
    "message": "Responses input contains N items; the configured maximum is L. Compact the conversation and retry."
  }
}
```

The error is non-retryable inside NewAPI. NewAPI must not select another Ark
channel, truncate the request, reorder items, or retry the same body.

The standard error code is intentional: ZCode 3.14.3 already recognizes
`context_length_exceeded` and starts reactive compaction. After compaction,
ZCode performs a new request with the compacted history.

## Components

### NewAPI Guard

Add a focused Responses input-item guard before the pass-through branch in
`PrepareResponsesRequest`.

The guard:

1. Resolves the final request URL through the selected adaptor.
2. Determines whether the final host is an official Ark regional host.
3. Parses the optional soft-limit header.
4. Counts top-level Responses input items without inspecting their content.
5. Returns a typed OpenAI context-limit error when the count exceeds the
   effective limit.
6. Removes the opt-in header before normal header forwarding.

The existing Ark history normalizer remains responsible only for adding
required `type` and `status` fields. It does not own size policy.

### ZCode Provider Configuration

Update only the custom Responses provider used by the failing request:

`4e71cad5-e14a-4b21-8feb-7727ba10511d`

Add the soft-limit header to that provider in:

- `~/.zcode/v2/config.json`
- `~/.zcode/cli/config.json`

The update must:

- create timestamped backups before mutation;
- modify the provider options atomically;
- preserve API-key values and all unrelated provider/model settings;
- never print or record credentials;
- verify readback using a redacted projection;
- avoid changing Chat or Messages providers.

Before either real config file is changed, a disposable loopback capture must
prove that ZCode 3.14.3 accepts `options.headers` for this provider kind and
emits the exact soft-limit header. Failure blocks the configuration change.

The model's advertised 1,048,576-token context remains unchanged. Compaction
is now triggered by item pressure independently of token pressure.

## Current Over-Limit Session

After the NewAPI hard guard is deployed, the current 1006-item session should
receive `context_length_exceeded` locally instead of Ark's
`InvalidParameter`. ZCode should then enter reactive compaction.

This recovery is a single Canary:

- perform one controlled continuation attempt;
- do not automatically retry an unknown outcome;
- confirm a compaction timeline event before accepting recovery;
- confirm the subsequent model request contains at most 1000 items;
- if compaction does not start, stop and preserve the session for diagnosis.

Starting a new session remains the rollback recovery path.

## Testing

### Unit Tests

Cover:

- scalar input bypass;
- arrays of 899, 900, 901, 999, 1000, and 1001 items;
- header absent, valid, malformed, zero, negative, and above 1000;
- exact Ark regional hosts and lookalike/non-Ark hosts;
- direct Volcengine and Advanced Custom Ark routes;
- pass-through body mode;
- header removal before upstream forwarding;
- exact OpenAI error type, code, param, status, and non-retry behavior;
- no changes to item order, values, or count for accepted requests.

Tests must demonstrate RED before implementation and GREEN afterward.

### Regression Tests

Run focused packages first, then the repository's required backend suites.
The immutable candidate image must pass three consecutive rounds covering:

- ordinary ZCode Responses requests;
- 900-item acceptance with the opt-in header;
- 901-item local context-limit rejection with no upstream request;
- 1000-item acceptance without the opt-in header;
- 1001-item hard-limit rejection with no upstream request;
- native Ark Responses history normalization;
- all enabled models and existing protocol routing.

## Observability

For a local item-limit rejection, record only:

- request ID;
- channel ID;
- model;
- observed item count;
- effective limit;
- whether the limit came from the default or client opt-in.

Do not log input content, provider credentials, the full request body, or
custom headers.

## Rollout

1. Complete RED/GREEN tests and code review in the isolated worktree.
2. Build an immutable candidate image.
3. Capture the required Compose, database, configuration, extension, report,
   and image evidence with SHA-256 values.
4. Deploy the NewAPI image without changing ZCode configuration.
5. Verify the 1001-item hard guard and three normal regression rounds.
6. Run the single current-session recovery Canary against the 1000-item hard
   guard, before enabling the lower soft limit.
7. Prove custom-header emission with a disposable loopback capture.
8. Back up and update the two ZCode configuration files.
9. Restart or reload ZCode only if configuration hot reload does not apply.
10. Verify that a disposable 901-item ZCode session triggers compaction before
    a normal model request reaches Ark.
11. Run three post-configuration ZCode and all-enabled-model regression
    rounds.

Any Important finding blocks the next rollout step.

## Rollback

Rollback is ordered:

1. Remove the ZCode soft-limit header by restoring the timestamped config
   backups.
2. Verify normal ZCode requests still use the prior provider configuration.
3. Roll back the NewAPI image only if the hard guard or unrelated request
   behavior regressed.

The database schema is unchanged. No destructive cleanup is required.

## Acceptance Criteria

- Ark never receives a Responses request above 1000 input items.
- The opted-in ZCode Responses provider never sends a normal model request
  above 900 items.
- Over-limit requests return `context_length_exceeded`, not
  `InvalidParameter`.
- ZCode starts reactive compaction and completes the current-session Canary
  once.
- Accepted requests preserve item order and content exactly, apart from the
  existing Ark-required history normalization.
- Chat, Messages, non-Ark Responses, and scalar Responses inputs remain
  unchanged.
- Three complete regression rounds pass before production completion.
