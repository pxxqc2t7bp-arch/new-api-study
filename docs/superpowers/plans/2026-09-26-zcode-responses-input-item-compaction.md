# ZCode Responses Input-Item Compaction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Ark Responses item-limit overflow trigger ZCode compaction, with a 1000-item hard guard and a ZCode-specific 900-item soft guard.

**Architecture:** NewAPI validates top-level Responses input item count after channel routing but before pass-through or conversion. A standard `context_length_exceeded` error activates ZCode's existing reactive compaction; an opt-in request header lowers the limit to 900 for the ZCode Responses provider without changing the model's token window or patching the ZCode bundle.

**Tech Stack:** Go 1.25, Gin, testify, Python 3 standard library, ZCode CLI 0.16.9 / desktop 3.14.3, GitHub Actions, Docker Compose

---

## File Map

- Create `relay/responses_input_item_limit.go`: Ark target detection, soft-limit parsing, input counting, typed error construction, and bounded rejection logging.
- Create `relay/responses_input_item_limit_test.go`: pure boundary tests plus `PrepareResponsesRequest` pass-through integration coverage.
- Modify `relay/responses_request.go`: invoke the guard before the pass-through branch.
- Create `tools/ops/zcode_responses_item_guard.py`: dry-run/apply/rollback-safe updates for the two ZCode config files.
- Create `tools/ops/test_zcode_responses_item_guard.py`: config mutation, redaction, backup, and rollback tests.
- Create `tools/ops/probe_zcode_responses_item_guard.py`: isolated-HOME loopback proof that ZCode emits the soft-limit header.
- Create `tools/ops/test_probe_zcode_responses_item_guard.py`: loopback probe unit tests using a fake CLI process.
- Create `reports/zcode-responses-item-compaction-20260926/`: generated verification, deployment, config, and regression evidence.

### Task 1: RED Tests For The NewAPI Guard

**Files:**

- Create: `relay/responses_input_item_limit_test.go`
- Read: `relay/responses_request.go`
- Read: `relay/channel/advancedcustom/adaptor_test.go`

- [ ] **Step 1: Add pure boundary and target tests**

Add table-driven tests covering:

```go
func TestResolveResponsesInputItemLimit(t *testing.T) {
    tests := []struct {
        name, header string
        want         int
        wantSource   string
        wantErr      bool
    }{
        {name: "default", want: 1000, wantSource: "ark_default"},
        {name: "zcode soft limit", header: "900", want: 900, wantSource: "client_opt_in"},
        {name: "zero", header: "0", wantErr: true},
        {name: "negative", header: "-1", wantErr: true},
        {name: "above hard limit", header: "1001", wantErr: true},
        {name: "not an integer", header: "nine hundred", wantErr: true},
    }
}

func TestIsNativeArkResponsesURL(t *testing.T) {
    tests := []struct {
        rawURL string
        want   bool
    }{
        {rawURL: "https://ark.cn-beijing.volces.com/api/v3/responses", want: true},
        {rawURL: "https://ark.cn-beijing.volces.com/api/coding/v3/responses", want: true},
        {rawURL: "https://ark.cn-beijing.volces.com/api/v3/chat/completions", want: false},
        {rawURL: "https://ark.cn-beijing.volces.com.example/v1/responses", want: false},
        {rawURL: "https://fallback.example/v1/responses", want: false},
    }
}
```

- [ ] **Step 2: Add item-count error contract tests**

Generate arrays without meaningful content:

```go
func responsesInputItems(count int) json.RawMessage {
    items := make([]map[string]any, count)
    for index := range items {
        items[index] = map[string]any{
            "type": "message",
            "role": "user",
            "content": fmt.Sprintf("item-%d", index),
        }
    }
    raw, err := json.Marshal(items)
    if err != nil {
        panic(err)
    }
    return raw
}
```

Assert that 900 and 1000 are accepted at their respective limits, while 901
and 1001 return:

```go
types.OpenAIError{
    Message: "Responses input contains 901 items; the configured maximum is 900. Compact the conversation and retry.",
    Type:    "invalid_request_error",
    Param:   "input",
    Code:    "context_length_exceeded",
}
```

Also assert `types.IsSkipRetryError(apiErr)` and HTTP 400.

- [ ] **Step 3: Add `PrepareResponsesRequest` pass-through tests**

Construct Gin contexts with:

```go
common.SetContextKey(c, constant.ContextKeyOriginalModel, "glm-5.3")
common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeVolcEngine)
common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://ark.cn-beijing.volces.com/api/v3")
common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{
    PassThroughBodyEnabled: true,
})
```

Verify:

- 1001 items fail before `common.GetBodyStorage`;
- a 901-item request with the soft-limit header fails before pass-through;
- the header is deleted from `c.Request.Header`;
- a non-Ark URL and an Ark `/chat/completions` route are not guarded;
- accepted input bytes remain unchanged.

- [ ] **Step 4: Run RED tests**

Run:

```bash
go test ./relay -run 'Test(ResolveResponsesInputItemLimit|IsNativeArkResponsesURL|ArkResponsesInputItemLimit|PrepareResponsesRequest.*ItemLimit)' -count=1
```

Expected: FAIL because the guard functions do not exist and
`PrepareResponsesRequest` does not enforce item limits.

- [ ] **Step 5: Commit RED tests**

```bash
git add relay/responses_input_item_limit_test.go
git commit -m "test(relay): reproduce Ark Responses item overflow"
```

### Task 2: GREEN NewAPI Guard

**Files:**

- Create: `relay/responses_input_item_limit.go`
- Modify: `relay/responses_request.go`
- Test: `relay/responses_input_item_limit_test.go`

- [ ] **Step 1: Implement the focused guard**

Create these constants and helpers:

```go
const (
    arkResponsesHardInputItemLimit = 1000
    responsesInputItemSoftLimitHeader = "X-NewAPI-Responses-Input-Item-Soft-Limit"
)

func resolveResponsesInputItemLimit(raw string) (limit int, source string, err error)
func isNativeArkResponsesURL(rawURL string) bool
func countResponsesInputItems(raw json.RawMessage) (count int, isArray bool, err error)
func enforceArkResponsesInputItemLimit(
    c *gin.Context,
    info *relaycommon.RelayInfo,
    requestURL string,
    request *dto.OpenAIResponsesRequest,
) *types.NewAPIError
```

Use `url.Parse`, exact hostname labels, and a path suffix check:

```go
path := strings.TrimSuffix(parsedURL.EscapedPath(), "/")
return len(labels) == 4 &&
    labels[0] == "ark" &&
    labels[1] != "" &&
    labels[2] == "volces" &&
    labels[3] == "com" &&
    strings.HasSuffix(path, "/responses")
```

Parse arrays with `common.Unmarshal` into `[]json.RawMessage`. Do not inspect,
rewrite, sort, or truncate individual items.

- [ ] **Step 2: Return the standard context error**

Construct the error with:

```go
return types.WithOpenAIError(types.OpenAIError{
    Message: fmt.Sprintf(
        "Responses input contains %d items; the configured maximum is %d. Compact the conversation and retry.",
        count,
        limit,
    ),
    Type:  "invalid_request_error",
    Param: "input",
    Code:  "context_length_exceeded",
}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
```

Log only request ID context, channel ID, model, item count, limit, and source.

- [ ] **Step 3: Invoke the guard before pass-through**

In `PrepareResponsesRequest`, after `adaptor.Init(info)`:

```go
requestURL, err := adaptor.GetRequestURL(info)
if err != nil {
    return nil, nil, nil, newConvertRequestFailedError(c, info, err)
}
if apiErr := enforceArkResponsesInputItemLimit(c, info, requestURL, request); apiErr != nil {
    return nil, nil, nil, apiErr
}
```

Capture then delete the soft-limit header inside the guard before any return.
The deleted header must remain available through a private Gin context key so
that a later channel attempt sees the same limit without forwarding the
control header upstream.

- [ ] **Step 4: Run GREEN and adjacent tests**

Run:

```bash
go test ./relay -run 'Test(ResolveResponsesInputItemLimit|IsNativeArkResponsesURL|ArkResponsesInputItemLimit|PrepareResponsesRequest.*ItemLimit|PrepareResponsesRequestRetainsConvertedAdaptor)' -count=1
go test ./relay/channel/volcengine ./relay/channel/advancedcustom -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the implementation**

```bash
git add relay/responses_input_item_limit.go relay/responses_request.go
git commit -m "fix(relay): guard Ark Responses input item limits"
```

### Task 3: RED/GREEN ZCode Configuration Tool

**Files:**

- Create: `tools/ops/zcode_responses_item_guard.py`
- Create: `tools/ops/test_zcode_responses_item_guard.py`

- [ ] **Step 1: Write failing config transformation tests**

Tests must create temporary JSON configs containing the target provider and
assert:

```python
updated, change = apply_guard(config)
assert updated["provider"][PROVIDER_ID]["options"]["headers"][HEADER_NAME] == "900"
assert change == {"before": None, "after": "900"}
assert config["provider"][PROVIDER_ID]["options"]["apiKey"] == "secret-fixture"
assert "secret-fixture" not in json.dumps(redacted_report(updated))
```

Also cover:

- preserving existing unrelated headers;
- idempotent second application;
- missing provider;
- wrong provider kind;
- unexpected base URL;
- atomic rollback when the second config write fails;
- mode preservation and `0600` backup files.

- [ ] **Step 2: Run RED tests**

Run:

```bash
python3 -m unittest -v tools.ops.test_zcode_responses_item_guard
```

Expected: FAIL because the module does not exist.

- [ ] **Step 3: Implement dry-run and apply modes**

The tool must expose:

```python
PROVIDER_ID = "4e71cad5-e14a-4b21-8feb-7727ba10511d"
HEADER_NAME = "X-NewAPI-Responses-Input-Item-Soft-Limit"
HEADER_VALUE = "900"

def load_config(path: Path) -> dict[str, Any]: ...
def apply_guard(config: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]: ...
def atomic_write(path: Path, data: bytes, mode: int) -> None: ...
def create_backup(paths: list[Path], timestamp: str) -> Path: ...
def redacted_report(config: dict[str, Any]) -> dict[str, Any]: ...
```

Default mode is dry-run. `--apply` must:

1. hash both original files;
2. create a `0700` timestamped backup directory under `~/.zcode/backups`;
3. copy both files as `0600`;
4. write both updates atomically;
5. re-read and validate both files;
6. restore both backups on any failure;
7. emit only hashes, provider ID, header name/value, and change status.

- [ ] **Step 4: Run GREEN tests and a real dry run**

Run:

```bash
python3 -m unittest -v tools.ops.test_zcode_responses_item_guard
python3 tools/ops/zcode_responses_item_guard.py \
  --output /tmp/zcode-responses-item-guard-dry-run.json
```

Expected: all tests PASS; dry run reports two pending changes and contains no
API key, token, Authorization value, Cookie, or JWT.

- [ ] **Step 5: Commit the config tool**

```bash
git add tools/ops/zcode_responses_item_guard.py tools/ops/test_zcode_responses_item_guard.py
git commit -m "feat(ops): configure ZCode Responses item guard"
```

### Task 4: Prove Header Emission And Reactive Compaction

**Files:**

- Create: `tools/ops/probe_zcode_responses_item_guard.py`
- Create: `tools/ops/test_probe_zcode_responses_item_guard.py`

- [ ] **Step 1: Write failing probe tests**

Test pure helpers for:

- isolated `HOME` config generation;
- dummy API key use;
- loopback-only URL enforcement;
- exact header/path capture;
- deterministic Responses SSE fixture generation;
- a fixture state machine that returns `context_length_exceeded` once and
  accepts the resulting compaction and retry requests;
- rejection of redirects and non-loopback addresses;
- bounded subprocess timeout;
- redacted report output.

- [ ] **Step 2: Run RED tests**

```bash
python3 -m unittest -v tools.ops.test_probe_zcode_responses_item_guard
```

Expected: FAIL because the probe module does not exist.

- [ ] **Step 3: Implement the isolated loopback probe**

The probe must:

1. locate the ZCode 3.14.3 `zcode.cjs` bundle explicitly;
2. create a temporary HOME with both `.zcode/v2/config.json` and
   `.zcode/cli/config.json`;
3. configure one `kind: "openai"` provider with a dummy key, loopback base URL,
   `glm-5.3`, and the 900-item header;
4. start a loopback `ThreadingHTTPServer`;
5. invoke:

   ```bash
   node /absolute/path/to/zcode.cjs \
     --prompt header-probe \
     --mode plan \
     --json \
     --cwd /tmp/zcode-header-probe-workspace
   ```

6. return one non-retryable fixture error after capturing the request;
7. run enough successful fixture turns to create compactable history;
8. return `context_length_exceeded` on the next ordinary model request;
9. return a valid text-only Responses SSE summary to the compaction request;
10. return a valid text-only Responses SSE answer to the automatic retry;
11. pass only when every request path is `/v1/responses`, every request carries
    the exact `900` header, a compaction request and automatic retry are
    observed in order, no credential is reported, and every subprocess
    terminates within 30 seconds.

- [ ] **Step 4: Run GREEN tests and the real bundle probe**

```bash
python3 -m unittest -v tools.ops.test_probe_zcode_responses_item_guard
python3 tools/ops/probe_zcode_responses_item_guard.py \
  --output /tmp/zcode-responses-header-probe.json
```

Expected: tests PASS and the real probe reports
`{"path":"/v1/responses","soft_limit":"900","reactive_compaction":true,"passed":true}`.

- [ ] **Step 5: Commit the probe**

```bash
git add tools/ops/probe_zcode_responses_item_guard.py tools/ops/test_probe_zcode_responses_item_guard.py
git commit -m "test(ops): verify ZCode Responses guard header"
```

### Task 5: Full Verification And Review

**Files:**

- Modify only files already listed in Tasks 1-4 if review finds defects.

- [ ] **Step 1: Run formatting and focused suites**

```bash
gofmt -w relay/responses_input_item_limit.go relay/responses_input_item_limit_test.go relay/responses_request.go
go test ./relay ./relay/channel/volcengine ./relay/channel/advancedcustom -count=1
python3 -m unittest -v \
  tools.ops.test_zcode_responses_item_guard \
  tools.ops.test_probe_zcode_responses_item_guard
```

Expected: PASS with no warnings.

- [ ] **Step 2: Run full backend suites**

```bash
mkdir -p web/dist
touch web/dist/index.html
go test ./... -count=1
(cd relaykit && go test ./... -count=1)
```

Expected: PASS.

- [ ] **Step 3: Review the diff**

Use `superpowers:requesting-code-review` and verify:

- no non-Ark behavior change;
- no converter-to-Chat false positive;
- no pass-through bypass;
- no request content or credentials in logs;
- no retry of deterministic item-limit failures;
- config updater is idempotent and rollback-safe.

- [ ] **Step 4: Commit review corrections**

```bash
git add relay tools/ops
git commit -m "fix: address Responses item guard review"
```

Skip this commit only when review produces no changes.

### Task 6: Publish And Deploy The Hard Guard

**Files:**

- Generate: `/Users/bytedance/newapi/reports/zcode-responses-item-compaction-20260926/`
- Reuse: `/Users/bytedance/newapi/reports/ark-advancedcustom-responses-20260925/deploy.sh`

- [ ] **Step 1: Push and build an immutable image**

```bash
git push -u origin fix/zcode-responses-item-compaction-20260926
gh workflow run study-image.yml \
  --ref fix/zcode-responses-item-compaction-20260926
gh run list --workflow study-image.yml --branch fix/zcode-responses-item-compaction-20260926 --limit 1
```

Watch the returned run to completion and record its source revision and image
digest. Do not deploy a mutable tag.

- [ ] **Step 2: Capture pre-deploy identities**

On `v100s`, record:

- current image reference and revision;
- Compose SHA-256;
- root, `/data`, and `/data1` free space;
- service health and restart count.

Stop if root has less than 4 GiB free or either data volume lacks space for
the required backup.

- [ ] **Step 3: Deploy with the existing guarded script**

Run the existing deployment script with the immutable digest, candidate
revision, current image reference, current revision, and current Compose
SHA-256. The script must create and verify:

- Compose backup;
- resolved Compose;
- PostgreSQL dump and restore list;
- old image archive;
- extension ZIP;
- image inspection;
- SHA-256 manifest.

- [ ] **Step 4: Verify the hard guard**

Using a temporary scoped token:

- send 1000 minimal items without the soft-limit header and expect the request
  to pass local validation;
- send 1001 minimal items and expect HTTP 400,
  `code=context_length_exceeded`, `param=input`;
- verify no Ark provider request ID was recorded for the 1001-item request;
- delete the temporary token in a `finally` path.

- [ ] **Step 5: Run three normal regression rounds**

Run the existing ZCode native probe and all-enabled-model suite three times.
Any failed round blocks the ZCode config change.

### Task 7: Recover The Existing Session And Enable The 900-Item Guard

**Files:**

- Modify through tool only: `~/.zcode/v2/config.json`
- Modify through tool only: `~/.zcode/cli/config.json`
- Generate: `/Users/bytedance/newapi/reports/zcode-responses-item-compaction-20260926/`

- [ ] **Step 1: Run the one-shot current-session recovery Canary**

Before applying the 900-item header, resume the affected ZCode session once.
Expected sequence:

1. NewAPI returns `context_length_exceeded` locally for the 1006-item request.
2. ZCode records a reactive compaction timeline event.
3. The compacted retry contains at most 1000 items.
4. The model turn completes successfully.

Do not automatically retry if the outcome is unknown or if compaction does
not start.

- [ ] **Step 2: Re-run the isolated header probe**

```bash
python3 tools/ops/probe_zcode_responses_item_guard.py \
  --output /Users/bytedance/newapi/reports/zcode-responses-item-compaction-20260926/header-probe.json
```

Expected: PASS.

- [ ] **Step 3: Dry-run and apply the config update**

```bash
python3 tools/ops/zcode_responses_item_guard.py \
  --output /Users/bytedance/newapi/reports/zcode-responses-item-compaction-20260926/config-dry-run.json
python3 tools/ops/zcode_responses_item_guard.py \
  --apply \
  --output /Users/bytedance/newapi/reports/zcode-responses-item-compaction-20260926/config-apply.json
```

Expected: both configs updated, backup manifest verified, selected model and
all unrelated providers unchanged, no secrets in reports.

- [ ] **Step 4: Verify the 900-item behavior**

Use the configured ZCode token for direct boundary probes and one ordinary
ZCode task:

- a direct 900-item request with the header must pass local validation;
- a direct 901-item request with the header must produce
  `context_length_exceeded` without an Ark provider request ID;
- the isolated loopback proof from Task 4 must still show ZCode reactive
  compaction and automatic retry;
- an ordinary real ZCode task must complete through the configured provider.

### Task 8: Final Regression And Audit

**Files:**

- Generate: `/Users/bytedance/newapi/reports/zcode-responses-item-compaction-20260926/summary.md`
- Generate: `/Users/bytedance/newapi/reports/zcode-responses-item-compaction-20260926/SHA256SUMS`

- [ ] **Step 1: Run three post-configuration rounds**

Run three complete rounds of:

- ordinary ZCode Responses;
- ZCode tool call plus tool output;
- all enabled models;
- Chat and Messages protocol smoke tests;
- 900/901 soft boundary;
- 1000/1001 hard boundary.

Expected: every round passes.

- [ ] **Step 2: Scan logs and runtime health**

Verify:

- no unexpected 4xx/5xx increase;
- no channel isolation;
- no restart;
- no secret-bearing evidence;
- item-limit logs contain only count, limit, source, channel, model, and
  request ID.

- [ ] **Step 3: Write and hash the audit**

The audit must include:

- root cause and exact 1000/1004 evidence;
- implementation revision and immutable image digest;
- RED and GREEN outputs;
- code-review findings and resolutions;
- deployment backup path;
- ZCode config backup path;
- one-shot recovery result;
- three-round results;
- rollback instructions.

Generate SHA-256 values for the report, Compose snapshot, database backup,
extension ZIP, config backups, and immutable image evidence.

- [ ] **Step 4: Final verification**

Use `superpowers:verification-before-completion`, run `git status`, and confirm
all required evidence exists and verifies before reporting completion.
