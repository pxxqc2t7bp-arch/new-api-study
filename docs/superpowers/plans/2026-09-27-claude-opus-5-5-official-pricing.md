# Claude Opus 5.5 Official Pricing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add only the standard Anthropic price for `claude-opus-5-5` through NewAPI's model-level CAS pricing API, then execute one pinned channel 48 canary and seal the result.

**Architecture:** A focused Python operations script validates the exact official Anthropic pricing row, snapshots and backs up production state, previews the exact tiered billing expression, performs one CAS-protected API write, classifies unknown outcomes through readback, and executes one pinned canary. Product code and every other model's pricing remain unchanged.

**Tech Stack:** Python 3 standard library, unittest, NewAPI root HTTP API, PostgreSQL `pg_dump`, Docker Compose, SHA-256

---

## Tasks

### Task 1: Add RED Tests for the Targeted Pricing Operation

**Files:**

- Create: `scripts/ops/test_apply_opus55_official_pricing.py`
- Create: `scripts/ops/apply_opus55_official_pricing.py`

- [ ] **Step 1: Create the test module**

Create `scripts/ops/test_apply_opus55_official_pricing.py` with tests for the
approved constants, official-page parsing, exact request payload, and unknown
write classification:

```python
import importlib.util
import pathlib
import unittest


SCRIPT = pathlib.Path(__file__).with_name(
    "apply_opus55_official_pricing.py"
)
SPEC = importlib.util.spec_from_file_location("opus55_pricing", SCRIPT)
pricing = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(pricing)


class Opus55PricingTest(unittest.TestCase):
    def test_exact_official_price_and_expression(self):
        self.assertEqual(
            {
                "input_per_m": 4.0,
                "output_per_m": 20.0,
                "cache_write_5m_per_m": 5.0,
                "cache_write_1h_per_m": 8.0,
                "cache_read_per_m": 0.2,
            },
            pricing.OFFICIAL_PRICE,
        )
        self.assertEqual(
            'tier("official", p*4 + cr*0.2 + cc*5 + cc1h*8 + c*20)',
            pricing.TARGET_EXPRESSION,
        )

    def test_official_page_requires_exact_model_row(self):
        html = """
        <table>
          <tr><th>Name</th><th>Input</th><th>Output</th>
              <th>5m writes</th><th>1h writes</th>
              <th>Hits and refreshes</th></tr>
          <tr><td>Claude Opus 5.5</td><td>$4 / MTok</td>
              <td>$20 / MTok</td><td>$5 / MTok</td>
              <td>$8 / MTok</td><td>$0.20 / MTok</td></tr>
        </table>
        """
        evidence = pricing.parse_official_page(html.encode())
        self.assertEqual(pricing.OFFICIAL_PRICE, evidence["price"])
        self.assertEqual(64, len(evidence["document_sha256"]))
        self.assertEqual(64, len(evidence["evidence_sha256"]))

    def test_official_page_rejects_near_match(self):
        html = """
        <table>
          <tr><td>Claude Opus 5.5</td><td>$5 / MTok</td>
              <td>$20 / MTok</td><td>$5 / MTok</td>
              <td>$8 / MTok</td><td>$0.20 / MTok</td></tr>
          <tr><td>Distractor</td><td>$4 / MTok</td>
              <td>$20 / MTok</td><td>$5 / MTok</td>
              <td>$8 / MTok</td><td>$0.20 / MTok</td></tr>
        </table>
        """
        with self.assertRaisesRegex(RuntimeError, "official price mismatch"):
            pricing.parse_official_page(html.encode())

    def test_patch_changes_only_target_model(self):
        payload = pricing.patch_payload(pricing.EMPTY_VERSION)
        self.assertEqual(1, len(payload["changes"]))
        change = payload["changes"][0]
        self.assertEqual(pricing.MODEL, change["model_name"])
        self.assertEqual(pricing.EMPTY_VERSION, change["expected_version"])
        self.assertEqual(
            {
                "billing_setting.billing_mode": "tiered_expr",
                "billing_setting.billing_expr": pricing.TARGET_EXPRESSION,
            },
            change["pricing"],
        )

    def test_unknown_write_classification(self):
        before = {"version": pricing.EMPTY_VERSION, "configured": {}}
        committed = {
            "version": "new-version",
            "configured": pricing.TARGET_PRICING,
        }
        divergent = {
            "version": "other-version",
            "configured": {"ModelRatio": 9.0},
        }
        self.assertEqual(
            "committed",
            pricing.classify_write_outcome(before, committed),
        )
        self.assertEqual(
            "not_committed",
            pricing.classify_write_outcome(before, before),
        )
        self.assertEqual(
            "unknown",
            pricing.classify_write_outcome(before, divergent),
        )


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Create the empty implementation module**

Create `scripts/ops/apply_opus55_official_pricing.py` with imports and no
target functions:

```python
#!/usr/bin/env python3
"""Apply one audited official-pricing change to production."""
```

- [ ] **Step 3: Run the tests and verify RED**

Run:

```bash
python3 -m unittest scripts/ops/test_apply_opus55_official_pricing.py -v
```

Expected: FAIL because `OFFICIAL_PRICE`, `TARGET_EXPRESSION`,
`parse_official_page`, `patch_payload`, and `classify_write_outcome` do not
exist.

### Task 2: Implement and Verify the Operations Script

**Files:**

- Modify: `scripts/ops/apply_opus55_official_pricing.py`
- Test: `scripts/ops/test_apply_opus55_official_pricing.py`

- [ ] **Step 1: Implement pure pricing and evidence functions**

Add these public constants and functions:

```python
MODEL = "claude-opus-5-5"
CHANNEL_ID = 48
SOURCE_URL = "https://platform.claude.com/docs/en/about-claude/pricing"
EMPTY_VERSION = (
    "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
)
OFFICIAL_PRICE = {
    "input_per_m": 4.0,
    "output_per_m": 20.0,
    "cache_write_5m_per_m": 5.0,
    "cache_write_1h_per_m": 8.0,
    "cache_read_per_m": 0.2,
}
TARGET_EXPRESSION = (
    'tier("official", p*4 + cr*0.2 + cc*5 + cc1h*8 + c*20)'
)
TARGET_PRICING = {
    "billing_setting.billing_mode": "tiered_expr",
    "billing_setting.billing_expr": TARGET_EXPRESSION,
}


class OfficialTableParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.in_table = False
        self.in_cell = False
        self.row: list[str] | None = None
        self.cell: list[str] = []
        self.rows: list[list[str]] = []

    def handle_starttag(
        self,
        tag: str,
        attrs: list[tuple[str, str | None]],
    ) -> None:
        del attrs
        if tag == "table":
            self.in_table = True
        elif self.in_table and tag == "tr":
            self.row = []
        elif self.row is not None and tag in {"td", "th"}:
            self.in_cell = True
            self.cell = []

    def handle_data(self, data: str) -> None:
        if self.in_cell:
            self.cell.append(data)

    def handle_endtag(self, tag: str) -> None:
        if self.in_cell and tag in {"td", "th"}:
            assert self.row is not None
            self.row.append(" ".join("".join(self.cell).split()))
            self.in_cell = False
        elif self.in_table and tag == "tr" and self.row is not None:
            if self.row:
                self.rows.append(self.row)
            self.row = None
        elif tag == "table":
            self.in_table = False


def parse_official_page(body: bytes) -> dict[str, object]:
    parser = OfficialTableParser()
    parser.feed(body.decode("utf-8"))
    target = next(
        (
            row
            for row in parser.rows
            if row and row[0].startswith("Claude Opus 5.5")
        ),
        None,
    )
    expected_cells = (
        "$4 / MTok",
        "$20 / MTok",
        "$5 / MTok",
        "$8 / MTok",
        "$0.20 / MTok",
    )
    if target is None or tuple(target[1:6]) != expected_cells:
        raise RuntimeError("official price mismatch")
    document_sha = hashlib.sha256(body).hexdigest()
    normalized = json.dumps(
        {
            "model": MODEL,
            "price": OFFICIAL_PRICE,
            "source_url": SOURCE_URL,
            "document_sha256": document_sha,
        },
        sort_keys=True,
        separators=(",", ":"),
    ).encode()
    return {
        "model": MODEL,
        "price": OFFICIAL_PRICE,
        "source_url": SOURCE_URL,
        "document_sha256": document_sha,
        "evidence_sha256": hashlib.sha256(normalized).hexdigest(),
    }


def patch_payload(expected_version: str) -> dict[str, object]:
    return {
        "changes": [{
            "model_name": MODEL,
            "expected_version": expected_version,
            "pricing": TARGET_PRICING,
        }]
    }


def classify_write_outcome(
    before: dict[str, object],
    after: dict[str, object],
) -> str:
    if after.get("configured") == TARGET_PRICING:
        return "committed"
    if (
        after.get("version") == before.get("version")
        and after.get("configured") == before.get("configured")
    ):
        return "not_committed"
    return "unknown"
```

`OfficialTableParser` must use `html.parser.HTMLParser` and return structured
table rows; do not use regular expressions to parse the HTML structure.

- [ ] **Step 2: Implement production orchestration**

The script must:

1. Accept `--output-dir`, `--expected-revision`, and
   `--existing-token-user-id`/`--existing-token-id`.
2. Assert `xingyugpu`, the machine-id SHA-256, `x86_64`, current immutable
   revision, health, restart count zero, and absence of both deployment and
   pricing locks.
3. Create `/opt/newapi-study/.pricing-change.lock/owner`.
4. Create a mode-0700 output directory below
   `/data1/newapi-study/backups`.
5. Copy current Compose and the extension ZIP, dump PostgreSQL, and validate
   the dump with `pg_restore --list`.
6. Fetch `SOURCE_URL` with a 30-second timeout and call
   `parse_official_page`.
7. Import `/opt/newapi-study/verify_model_alias_normalization.py`, obtain
   root headers without printing them, and GET the one-model snapshot.
8. Require one entry, empty `configured`, and `version == EMPTY_VERSION`.
9. POST the preview and require exact mode/expression with
   `cache_write_mode == "claude_ttl"`.
10. Record hashes of every pricing option row and the Plan authority state.
11. Send one PATCH request using `patch_payload`; never retry it.
12. Read back the snapshot and classify the outcome.
13. If committed, load the existing dedicated E2E token after validating user,
    token, role, status, name, and unlimited-quota policy. Append `-48` only in
    memory and issue one non-streaming chat-completions request.
14. Do not retry the canary. Record only status, request ID, selected channel
    when available, response error text, and duration.
15. Verify pricing readback, unrelated option hashes, Plan authority state,
    target channel state, service health, restart count, and lock cleanup.
16. Write `summary.json`, `SHA256SUMS`, and print only the output directory and
    final state.

The cleanup handler must retain the pricing lock only when
`classify_write_outcome` returns `unknown`.

- [ ] **Step 3: Run unit tests and syntax verification**

Run:

```bash
python3 -m unittest scripts/ops/test_apply_opus55_official_pricing.py -v
python3 -m py_compile scripts/ops/apply_opus55_official_pricing.py
```

Expected: all tests PASS and compilation exits 0.

- [ ] **Step 4: Review the script for secret-safe output and one-write semantics**

Run:

```bash
rg -n 'print|Authorization|access_token|\\.key|PATCH|for .*range|while ' \
  scripts/ops/apply_opus55_official_pricing.py
```

Expected:

- no token or key is printed;
- one syntactic PATCH call exists;
- no loop can invoke PATCH or the canary;
- retry loops are absent.

- [ ] **Step 5: Commit the operation tooling**

```bash
git add \
  scripts/ops/apply_opus55_official_pricing.py \
  scripts/ops/test_apply_opus55_official_pricing.py
git commit -m "ops: apply targeted opus 5.5 official pricing"
```

### Task 3: Run Production Preflight and Apply Exactly Once

**Files:**

- Execute: `scripts/ops/apply_opus55_official_pricing.py`
- Generate remotely:
  `/data1/newapi-study/backups/pre-opus55-pricing-${run_stamp}/`

- [ ] **Step 1: Verify the current production pre-state**

Run read-only checks on `v100s` for:

```text
hostname=xingyugpu
machine_id_sha256=f131f2873ea46b3cea3d9388bd44bf30751e97c0a1b35ea1e87b8b94ac44ac8c
revision=70127f55e020c9bf9a0af9d3acd2e7e15af167ef
runtime=running:healthy:restarts=0
pricing_lock=absent
deployment_lock=absent
target_channels=4:3,66:3,96:3
target_enabled_abilities=0
authority_states=active:3,disabled:1
opus55_configured={}
opus55_version=44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a
```

Stop without writing if any assertion differs.

- [ ] **Step 2: Stream the verified script to `v100s` exactly once**

Run:

```bash
run_stamp=$(date -u +%Y%m%dT%H%M%SZ)
output_dir="/data1/newapi-study/backups/pre-opus55-pricing-${run_stamp}"
ssh -o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 \
  v100s \
  'cd /opt/newapi-study && python3 - \
    --output-dir '"$output_dir"' \
    --expected-revision 70127f55e020c9bf9a0af9d3acd2e7e15af167ef \
    --existing-token-user-id 20 \
    --existing-token-id 139' \
  < scripts/ops/apply_opus55_official_pricing.py
```

Do not rerun this command after a connection failure. Inspect the lock and
one-model snapshot to determine the outcome.

- [ ] **Step 3: Evaluate the single canary**

Accepted local-pricing result:

```text
write_outcome=committed
stored_mode=tiered_expr
stored_expression=tier("official", p*4 + cr*0.2 + cc*5 + cc1h*8 + c*20)
```

The canary may pass or may retain the known upstream 503. A 503 is recorded as
upstream support still blocked; it does not cause a pricing retry or rollback.

### Task 4: Verify and Seal Production Evidence

**Files:**

- Modify:
  `/Users/bytedance/newapi/reports/monthly-plan-quota-prod-20260926/audit-report.md`
- Generate:
  `/Users/bytedance/newapi/reports/monthly-plan-quota-prod-20260926/opus55-pricing/`

- [ ] **Step 1: Copy only non-secret evidence locally**

Copy `summary.json`, option hashes, official evidence, canary summary,
Compose snapshot, extension ZIP hash, database dump hash, and `SHA256SUMS`.
Do not copy or print tokens, channel keys, cookies, or raw option maps.

- [ ] **Step 2: Run fresh production verification**

Verify:

```text
stored pricing is exact
unrelated pricing hashes are unchanged
runtime is running and healthy
restart count is zero
channels 4/66/96 remain status 3
their enabled ability count remains zero
authority states remain active:3,disabled:1
temporary probe-token count is zero
deployment and pricing locks are absent
remote SHA256SUMS validates
```

- [ ] **Step 3: Update the audit report**

Append:

- official source URL and captured document/evidence hashes;
- pre/post model pricing versions;
- exact stored expression;
- canary result and request ID;
- backup path and artifact SHA-256 values;
- explicit statement that no other model changed.

- [ ] **Step 4: Seal and verify local evidence**

Run:

```bash
cd /Users/bytedance/newapi/reports/monthly-plan-quota-prod-20260926
find . -type f ! -name SHA256SUMS -print0 |
  LC_ALL=C sort -z |
  xargs -0 shasum -a 256 > SHA256SUMS
shasum -a 256 -c SHA256SUMS
```

Expected: every listed artifact is `OK`.

- [ ] **Step 5: Commit and push documentation**

Commit only repository-tracked plan or audit changes. Keep `docs/mvp/`
untracked:

```bash
git status --short
git push origin deploy/monthly-plan-quota-20260926
```

Expected: the remote branch contains the design, plan, and operations-tool
commits; `docs/mvp/` remains excluded.
