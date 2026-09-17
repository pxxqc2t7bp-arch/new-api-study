# Production Model Alias Normalization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace release-suffixed production model abilities with stable, callable aliases and verify every enabled alias through three production regression rounds.

**Architecture:** Add an isolated Python migration under `tools/ops/`; do not modify NewAPI routing code. The migration reads enabled public channels, builds an audited alias manifest, copies effective pricing, updates channels through the management API, and verifies readback. A separate production verification script checks `/v1/models`, required aliases, routing logs, and delegates full capability coverage to the existing all-model E2E runner.

**Tech Stack:** Python 3 standard library, NewAPI management API, PostgreSQL via `docker exec psql`, existing `e2e_all_enabled_models_20260902.py`, SHA-256 audit artifacts.

---

## File Structure

- Create `tools/ops/model_alias_normalization.py`: pure alias classification, candidate ranking, channel-plan generation, pricing inheritance, backup, apply, rollback, and readback.
- Create `tools/ops/test_model_alias_normalization.py`: deterministic unit tests for canonicalization, collision selection, mapping preservation, and pricing gates.
- Create `tools/ops/verify_model_alias_normalization.py`: production list checks, required real requests, log-channel validation, and regression summary aggregation.
- Reuse `/Users/bytedance/newapi/e2e_all_enabled_models_20260902.py`: full enabled-model capability regression; no modification.
- Write runtime evidence under `reports/model-alias-normalization-20260917/`.

### Task 1: Canonical Alias Contract

**Files:**

- Create: `tools/ops/test_model_alias_normalization.py`
- Create: `tools/ops/model_alias_normalization.py`

- [ ] **Step 1: Write failing canonicalization tests**

```python
import unittest

import model_alias_normalization as migration


class CanonicalAliasTest(unittest.TestCase):
    def test_required_aliases(self) -> None:
        cases = {
            "gpt-5.6-luna": "gpt-5.6",
            "gpt-5.6-sol": "gpt-5.6",
            "gpt-5.6-terra": "gpt-5.6",
            "gpt-6-astra": "gpt-6",
            "deepseek-v4-flash": "deepseekv4.1flash",
            "deepseek-v4-flash-260425": "deepseekv4.1flash",
            "deepseek-v4-flash-ga-260731": "deepseekv4.1flash",
            "deepseek-v4-flash-原厂": "deepseekv4.1flash",
            "deepseek-v4-flash-vision-exp-原厂": "deepseekv4.1flash-vision",
        }
        for concrete, alias in cases.items():
            self.assertEqual(migration.canonical_alias(concrete), alias)

    def test_release_suffixes_are_hidden_but_capabilities_remain(self) -> None:
        cases = {
            "claude-haiku-4-5-20251001": "claude-haiku-4-5",
            "doubao-seedance-2-0-fast-260128": "doubao-seedance-2-0-fast",
            "doubao-seedance-2-0-mini-260615": "doubao-seedance-2-0-mini",
            "doubao-seed-2-0-pro-preview-260115": "doubao-seed-2-0-pro",
            "doubao-seed-evolving-latest-version": "doubao-seed-evolving",
            "qwen3-32b-20250429": "qwen3-32b",
            "gpt-5.3-codex-spark": "gpt-5.3-codex-spark",
            "claude-opus-4-8": "claude-opus-4-8",
        }
        for concrete, alias in cases.items():
            self.assertEqual(migration.canonical_alias(concrete), alias)
```

- [ ] **Step 2: Run the tests and verify failure**

Run:

```bash
cd /Users/bytedance/newapi/new-api-1/tools/ops
python3 -m unittest test_model_alias_normalization.py
```

Expected: `ERROR` because `model_alias_normalization` does not exist.

- [ ] **Step 3: Implement the canonicalization function**

```python
ISO_DATE = re.compile(r"-20\d{2}-\d{2}-\d{2}$")
COMPACT_RELEASE = re.compile(r"-(?:ga-)?\d{6,8}$")
TERMINAL_RELEASE_TAG = re.compile(r"-(?:draft-preview|preview|latest-version)$")

SPECIAL_ALIASES = {
    "gpt-5.6-luna": "gpt-5.6",
    "gpt-5.6-sol": "gpt-5.6",
    "gpt-5.6-terra": "gpt-5.6",
    "gpt-6-astra": "gpt-6",
    "glm-5.3-cxy": "glm-5.3",
}


def canonical_alias(model: str) -> str:
    if model in SPECIAL_ALIASES:
        return SPECIAL_ALIASES[model]
    if model == "deepseek-v4-flash-vision-exp-原厂":
        return "deepseekv4.1flash-vision"
    if re.fullmatch(r"deepseek-v4-flash(?:-(?:ga-)?\d{6,8}|-原厂)?", model):
        return "deepseekv4.1flash"
    alias = model.removesuffix("-原厂")
    alias = ISO_DATE.sub("", alias)
    alias = COMPACT_RELEASE.sub("", alias)
    alias = TERMINAL_RELEASE_TAG.sub("", alias)
    return alias
```

- [ ] **Step 4: Run the canonicalization tests**

Run:

```bash
cd /Users/bytedance/newapi/new-api-1/tools/ops
python3 -m unittest test_model_alias_normalization.py
```

Expected: all canonicalization tests pass.

### Task 2: Deterministic Channel Manifest

**Files:**

- Modify: `tools/ops/test_model_alias_normalization.py`
- Modify: `tools/ops/model_alias_normalization.py`

- [ ] **Step 1: Add collision and mapping tests**

```python
class ChannelPlanTest(unittest.TestCase):
    def test_recent_success_wins_gpt_codename_collision(self) -> None:
        selected = migration.select_concrete(
            "gpt-5.6",
            ["gpt-5.6-sol", "gpt-5.6-terra"],
            {},
            {"gpt-5.6-sol": 200, "gpt-5.6-terra": 100},
        )
        self.assertEqual(selected, "gpt-5.6-sol")

    def test_existing_explicit_mapping_wins(self) -> None:
        selected = migration.select_concrete(
            "deepseekv4.1flash",
            [
                "deepseek-v4-flash",
                "deepseek-v4-flash-260425",
                "deepseek-v4-flash-ga-260731",
            ],
            {"deepseek-v4-flash": "deepseek-v4-flash-260425"},
            {"deepseek-v4-flash-ga-260731": 300},
        )
        self.assertEqual(selected, "deepseek-v4-flash-260425")

    def test_public_channel_plan_contains_aliases_only(self) -> None:
        plan = migration.build_channel_plan(
            {
                "id": 92,
                "group": "default,cxy",
                "models": "gpt-5.6-sol,gpt-5.6-terra,gpt-6-astra",
                "model_mapping": "{}",
            },
            {
                "gpt-5.6-sol": 200,
                "gpt-5.6-terra": 100,
                "gpt-6-astra": 150,
            },
        )
        self.assertEqual(plan["models"], ["gpt-5.6", "gpt-6"])
        self.assertEqual(
            plan["model_mapping"],
            {"gpt-5.6": "gpt-5.6-sol", "gpt-6": "gpt-6-astra"},
        )
```

- [ ] **Step 2: Run the new tests and verify failure**

Run:

```bash
cd /Users/bytedance/newapi/new-api-1/tools/ops
python3 -m unittest test_model_alias_normalization.py
```

Expected: failures for missing `select_concrete` and `build_channel_plan`.

- [ ] **Step 3: Implement deterministic manifest generation**

Implement these interfaces:

```python
def select_concrete(
    alias: str,
    candidates: list[str],
    existing_mapping: dict[str, str],
    recent_success: dict[str, int],
) -> str:
    for current_alias, target in existing_mapping.items():
        if canonical_alias(current_alias) == alias and target in candidates:
            return target
    return max(
        candidates,
        key=lambda item: (
            recent_success.get(item, 0),
            release_number(item),
            item,
        ),
    )


def build_channel_plan(
    channel: dict[str, object],
    recent_success: dict[str, int],
) -> dict[str, object]:
    concrete_models = parse_models(str(channel["models"]))
    existing_mapping = parse_mapping(channel.get("model_mapping"))
    by_alias = group_by_alias(concrete_models)
    desired_mapping = preserve_unaffected_mappings(existing_mapping, by_alias)
    desired_models = []
    for alias, candidates in sorted(by_alias.items()):
        target = select_concrete(alias, candidates, existing_mapping, recent_success)
        desired_models.append(alias)
        if target != alias:
            desired_mapping[alias] = target
    return {
        "channel_id": int(channel["id"]),
        "models": desired_models,
        "model_mapping": desired_mapping,
        "changed": desired_models != concrete_models
        or desired_mapping != existing_mapping,
    }
```

Only channels whose group contains `default` or `cxy` are migrated. Internal
`ark-canary` and `ark-quarantine` channels remain untouched.

- [ ] **Step 4: Run all unit tests**

Run:

```bash
cd /Users/bytedance/newapi/new-api-1/tools/ops
python3 -m unittest -v test_model_alias_normalization.py
```

Expected: all tests pass.

### Task 3: Pricing, Backup, Apply, And Rollback

**Files:**

- Modify: `tools/ops/test_model_alias_normalization.py`
- Modify: `tools/ops/model_alias_normalization.py`

- [ ] **Step 1: Add pricing and gate tests**

```python
class PricingPlanTest(unittest.TestCase):
    def test_alias_inherits_selected_concrete_pricing(self) -> None:
        options = {
            "ModelRatio": {"gpt-5.6-sol": 0.1},
            "CompletionRatio": {"gpt-5.6-sol": 4.0},
            "ModelPrice": {},
            "CacheRatio": {"gpt-5.6-sol": 0.25},
            "billing_setting.billing_mode": {},
            "billing_setting.billing_expr": {},
        }
        updated = migration.inherit_alias_pricing(
            options, {"gpt-5.6": "gpt-5.6-sol"}
        )
        self.assertEqual(updated["ModelRatio"]["gpt-5.6"], 0.1)
        self.assertEqual(updated["CompletionRatio"]["gpt-5.6"], 4.0)
        self.assertEqual(updated["CacheRatio"]["gpt-5.6"], 0.25)

    def test_unpriced_alias_is_rejected(self) -> None:
        options = {key: {} for key in migration.OPTION_KEYS}
        with self.assertRaisesRegex(RuntimeError, "unpriced alias"):
            migration.inherit_alias_pricing(
                options, {"gpt-5.6": "gpt-5.6-sol"}
            )

    def test_apply_requires_exact_manifest_hash(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "manifest SHA-256"):
            migration.validate_apply_hash("expected", "actual")
```

- [ ] **Step 2: Implement production client and pricing inheritance**

Use the existing management contracts:

```python
channel = client.api("GET", f"/api/channel/{channel_id}")
payload = copy.deepcopy(channel)
payload["models"] = ",".join(plan["models"])
payload["model_mapping"] = json.dumps(
    plan["model_mapping"], ensure_ascii=False, separators=(",", ":")
)
payload["key"] = ""
payload.pop("status", None)
client.api("PUT", "/api/channel/", payload)

client.api(
    "PUT",
    "/api/option/",
    {"key": option_key, "value": json.dumps(updated, ensure_ascii=False)},
)
```

Copy canonical pricing from the selected target across `ModelRatio`,
`ModelPrice`, `CompletionRatio`, `CacheRatio`,
`billing_setting.billing_mode`, and `billing_setting.billing_expr`. Apply is
blocked if none of those sources provides an effective price.

- [ ] **Step 3: Implement pre-apply backup and rollback**

The apply path creates:

```text
/opt/newapi-study/backups/pre-model-alias-normalization-$timestamp/
  docker-compose.yml
  new-api.sql
  channels-before.json
  options-before.json
  alias-manifest.json
  newapi-sub2api-sync-extension-v0.1.5.zip
  SHA256SUMS
```

It copies the newest existing extension ZIP, performs `pg_dump`, hashes every
artifact, verifies `sha256sum -c SHA256SUMS`, then mutates configuration.
Rollback restores option JSON and each original channel payload in reverse
order if any apply or readback step fails.

- [ ] **Step 4: Run syntax and unit verification**

Run:

```bash
python3 -m py_compile tools/ops/model_alias_normalization.py
cd tools/ops
python3 -m unittest -v test_model_alias_normalization.py
```

Expected: compilation succeeds and all tests pass.

- [ ] **Step 5: Commit the migration and tests**

```bash
git add tools/ops/model_alias_normalization.py tools/ops/test_model_alias_normalization.py
git commit -m "feat(ops): normalize production model aliases"
```

### Task 4: Production Verification Harness

**Files:**

- Create: `tools/ops/verify_model_alias_normalization.py`

- [ ] **Step 1: Implement public-list and required-model checks**

The verifier must:

- fetch `/v1/models` with a temporary default-group token;
- assert `gpt-5.6`, `gpt-6`, and `deepseekv4.1flash` are present;
- assert every concrete ID listed in the apply manifest is absent;
- send real requests for the three required aliases;
- wait for matching request logs and record channel IDs.

Use these assertions for the inventory gate:

```python
REQUIRED_ALIASES = {"gpt-5.6", "gpt-6", "deepseekv4.1flash"}


def verify_public_models(
    public_models: set[str],
    manifest: dict[str, object],
) -> None:
    missing = REQUIRED_ALIASES - public_models
    if missing:
        raise RuntimeError("required aliases missing: " + ",".join(sorted(missing)))
    hidden = set(manifest["hidden_concrete_models"]) & public_models
    if hidden:
        raise RuntimeError(
            "concrete release identifiers still exposed: "
            + ",".join(sorted(hidden))
        )
```

- [ ] **Step 2: Implement three-round regression orchestration**

For rounds 1 through 3, run:

```bash
python3 /opt/newapi-study/e2e_all_enabled_models_20260902.py \
  "reports/model-alias-normalization-20260917/all-models-round-${round}.json"
```

Each round must have zero failed results and an inventory hash matching the
post-apply alias manifest. The verifier writes `verification-summary.json` and
`audit.md` with hashes and pass/fail counts.

- [ ] **Step 3: Compile and commit the verifier**

Run:

```bash
python3 -m py_compile tools/ops/verify_model_alias_normalization.py
git add tools/ops/verify_model_alias_normalization.py
git commit -m "test(ops): verify production model aliases"
```

Expected: compilation succeeds and only the verifier is committed.

### Task 5: Production Dry Run And Gate

**Files:**

- Runtime output: `reports/model-alias-normalization-20260917/dry-run.json`

- [ ] **Step 1: Copy scripts to the production host**

```bash
scp tools/ops/model_alias_normalization.py study:/opt/newapi-study/
scp tools/ops/verify_model_alias_normalization.py study:/opt/newapi-study/
scp /Users/bytedance/newapi/e2e_all_enabled_models_20260902.py study:/opt/newapi-study/
scp /Users/bytedance/newapi/ark_capability_profiles_20260906.py study:/opt/newapi-study/
```

- [ ] **Step 2: Run production dry-run**

```bash
ssh study "cd /opt/newapi-study && python3 model_alias_normalization.py \
  --output reports/model-alias-normalization-20260917/dry-run.json"
```

Expected: `status=dry_run_ok`, no channel or option mutation, all required
aliases priced, and a printed manifest SHA-256.

- [ ] **Step 3: Inspect the complete diff**

Reject the apply if the dry-run:

- changes a channel outside `default`/`cxy`;
- removes a capability SKU;
- selects a concrete target with no recent success;
- publishes an unpriced alias;
- omits `gpt-5.6`, `gpt-6`, or `deepseekv4.1flash`;
- changes channel priority, weight, group, base URL, route settings, or key
  fingerprint.

### Task 6: Apply And Production Regression

**Files:**

- Runtime output: `reports/model-alias-normalization-20260917/applied.json`
- Runtime output: `reports/model-alias-normalization-20260917/verification-summary.json`
- Runtime output: `reports/model-alias-normalization-20260917/audit.md`

- [ ] **Step 1: Apply the exact approved manifest**

```bash
manifest_sha="$(ssh study "python3 -c 'import json; print(json.load(open(\"/opt/newapi-study/reports/model-alias-normalization-20260917/dry-run.json\"))[\"manifest_sha256\"])'")"
ssh study "cd /opt/newapi-study && python3 model_alias_normalization.py \
  --apply \
  --expected-manifest-sha256 ${manifest_sha} \
  --output reports/model-alias-normalization-20260917/applied.json"
```

Expected: backup hash verification passes, channel and pricing readback match,
and `status=applied`.

- [ ] **Step 2: Run targeted and list verification**

```bash
ssh study "cd /opt/newapi-study && python3 verify_model_alias_normalization.py \
  --manifest reports/model-alias-normalization-20260917/applied.json \
  --rounds 0"
```

Expected: all three required aliases complete real requests and no concrete
revision appears in `/v1/models`.

- [ ] **Step 3: Run all enabled models three times**

```bash
ssh study "cd /opt/newapi-study && python3 verify_model_alias_normalization.py \
  --manifest reports/model-alias-normalization-20260917/applied.json \
  --rounds 3"
```

Expected: three complete rounds, zero failures, stable inventory hash.

- [ ] **Step 4: Verify hashes and report**

Run:

```bash
ssh study "cd /opt/newapi-study/reports/model-alias-normalization-20260917 && sha256sum -c SHA256SUMS"
```

Expected: every artifact reports `OK`. Report Compose, database backup,
extension ZIP, applied manifest, verification summary, and audit SHA-256.
