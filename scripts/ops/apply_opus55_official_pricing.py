#!/usr/bin/env python3
"""Pure helpers and orchestration for an audited Opus 5.5 pricing change."""

from __future__ import annotations

import argparse
import ctypes
import gzip
import hashlib
from html.parser import HTMLParser
import http.client
import json
import os
from pathlib import Path
import platform
import re
import socket
import stat
import subprocess
import sys
import time
import urllib.request
import uuid


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
OFFICIAL_QUOTE_TOKEN_FIXTURE = {
    "input_tokens": 1_000_000,
    "output_tokens": 1_000_000,
    "cache_read_tokens": 1_000_000,
    "cache_write_5m_tokens": 1_000_000,
    "cache_write_1h_tokens": 1_000_000,
}
TARGET_EXPRESSION = (
    'tier("official", p*4 + cr*0.2 + cc*5 + cc1h*8 + c*20)'
)
TARGET_PRICING = {
    "billing_setting.billing_mode": "tiered_expr",
    "billing_setting.billing_expr": TARGET_EXPRESSION,
}

_OFFICIAL_PRICE_CELLS = [
    "$4 / MTok",
    "$20 / MTok",
    "$5 / MTok",
    "$8 / MTok",
    "$0.20 / MTok",
]
EXPECTED_HOSTNAME = "xingyugpu"
EXPECTED_MACHINE_ID_SHA256 = (
    "f131f2873ea46b3cea3d9388bd44bf30751e97c0a1b35ea1e87b8b94ac44ac8c"
)
EXPECTED_ARCHITECTURE = "x86_64"
HASH_CHUNK_SIZE = 4 * 1024 * 1024
PRICING_PATH = "/api/option/model_pricing"
PRICING_SNAPSHOT_PATH = PRICING_PATH + "?model=" + MODEL
PREVIEW_PATH = PRICING_PATH + "/preview"
CANARY_PATH = "/v1/chat/completions"
MUTATED_OPTION_KEYS = {
    "billing_setting.billing_expr": TARGET_EXPRESSION,
    "billing_setting.billing_mode": "tiered_expr",
}
PRODUCTION_ROOT = Path("/opt/newapi-study")
BACKUP_ROOT = Path("/data1/newapi-study/backups")
COMPOSE_PATH = PRODUCTION_ROOT / "docker-compose.yml"
EXTENSION_PATH = PRODUCTION_ROOT / (
    "reports/responses-native-history-normalization-20260918/"
    "newapi-sub2api-sync-extension-v0.1.5.zip"
)
PRICING_LOCK_PATH = PRODUCTION_ROOT / ".pricing-change.lock"
DEPLOYMENT_LOCK_PATH = PRODUCTION_ROOT / ".deployment.lock"
SEAL_MARKER_NAME = "seal.json"
SEAL_PROTOCOL = "opus55-pricing-seal-v1"
TEMPORARY_PROBE_TOKEN_PREFIXES = (
    "alias-e2e-",
    "alias-all-",
)
PRE_PATCH_REQUIRED_ARTIFACTS = frozenset(
    {
        "SHA256SUMS.pre",
        "docker-compose.yml",
        "new-api.dump",
        "newapi-sub2api-sync-extension-v0.1.5.zip",
        "official-evidence.json",
        "pg-restore-list.txt",
        "plan-before.txt",
        "pricing-before.json",
        "pricing-preview.json",
        "runtime-before.txt",
    }
)
REQUIRED_ARTIFACTS = PRE_PATCH_REQUIRED_ARTIFACTS | {
    "SHA256SUMS",
    "canary.json",
    "plan-after.txt",
    "pricing-after.json",
    "pricing-conflict.json",
    "pricing-write-error.json",
    "runtime-after.txt",
    "summary.json",
}
FINAL_OWNER_STATES = frozenset({"pending_release"})


class LockAcquireCleanupError(RuntimeError):
    def __init__(
        self,
        acquire_error: BaseException,
        cleanup_errors: list[tuple[str, BaseException]],
        *,
        active_lock_exists: bool,
        owner_exists: bool,
    ) -> None:
        self.acquire_error = acquire_error
        self.cleanup_errors = tuple(cleanup_errors)
        self.active_lock_exists = active_lock_exists
        self.owner_exists = owner_exists
        cleanup_text = "; ".join(
            "%s failed (%s: %s)" % (stage, type(error).__name__, error)
            for stage, error in cleanup_errors
        )
        super().__init__(
            "pricing lock acquisition failed (%s: %s); cleanup errors: %s"
            % (
                type(acquire_error).__name__,
                acquire_error,
                cleanup_text,
            )
        )


class LockCommitDurabilityError(RuntimeError):
    def __init__(
        self,
        durability_error: BaseException,
        tombstone_path: Path,
    ) -> None:
        self.durability_error = durability_error
        self.lock_tombstone_path = tombstone_path
        super().__init__(
            "pricing lock commit durability unknown (%s: %s)"
            % (type(durability_error).__name__, durability_error)
        )


def evaluate_official_quote(
    token_counts: dict[str, int],
) -> dict[str, float]:
    if set(token_counts) != set(OFFICIAL_QUOTE_TOKEN_FIXTURE):
        raise ValueError("official quote token categories mismatch")
    if any(
        not isinstance(count, int)
        or isinstance(count, bool)
        or count < 0
        for count in token_counts.values()
    ):
        raise ValueError("official quote token counts must be nonnegative")
    per_million = {
        "input": (
            token_counts["input_tokens"],
            OFFICIAL_PRICE["input_per_m"],
        ),
        "output": (
            token_counts["output_tokens"],
            OFFICIAL_PRICE["output_per_m"],
        ),
        "cache_read": (
            token_counts["cache_read_tokens"],
            OFFICIAL_PRICE["cache_read_per_m"],
        ),
        "cache_write_5m": (
            token_counts["cache_write_5m_tokens"],
            OFFICIAL_PRICE["cache_write_5m_per_m"],
        ),
        "cache_write_1h": (
            token_counts["cache_write_1h_tokens"],
            OFFICIAL_PRICE["cache_write_1h_per_m"],
        ),
    }
    return {
        category: count / 1_000_000 * price
        for category, (count, price) in per_million.items()
    }


def official_quote_fixture() -> dict[str, object]:
    token_counts = dict(OFFICIAL_QUOTE_TOKEN_FIXTURE)
    return {
        "token_counts": token_counts,
        "category_cost_usd": evaluate_official_quote(token_counts),
    }


class OfficialTableParser(HTMLParser):
    """Collect normalized cells for each HTML table row."""

    def __init__(self) -> None:
        super().__init__()
        self.rows: list[list[str]] = []
        self._table_depth = 0
        self._row: list[str] | None = None
        self._cell_parts: list[str] | None = None

    def handle_starttag(
        self,
        tag: str,
        attrs: list[tuple[str, str | None]],
    ) -> None:
        del attrs
        if tag == "table":
            self._table_depth += 1
        elif self._table_depth == 0:
            return
        elif tag == "tr":
            self._row = []
        elif tag in {"td", "th"} and self._row is not None:
            self._cell_parts = []

    def handle_data(self, data: str) -> None:
        if self._cell_parts is not None:
            self._cell_parts.append(data)

    def handle_endtag(self, tag: str) -> None:
        if tag == "table":
            if self._table_depth > 0:
                self._table_depth -= 1
            if self._table_depth == 0:
                self._row = None
                self._cell_parts = None
            return
        if self._table_depth == 0:
            return
        if tag in {"td", "th"} and self._cell_parts is not None:
            if self._row is not None:
                self._row.append(
                    " ".join(" ".join(self._cell_parts).split())
                )
            self._cell_parts = None
        elif tag == "tr" and self._row is not None:
            self.rows.append(self._row)
            self._row = None
            self._cell_parts = None


def parse_official_page(body: bytes) -> dict[str, object]:
    parser = OfficialTableParser()
    parser.feed(body.decode("utf-8"))
    parser.close()

    for row in parser.rows:
        if not row:
            continue
        model_cell = row[0]
        if model_cell not in {
            "Claude Opus 5.5",
            (
                "Claude Opus 5.5 For long-running agentic coding "
                "and knowledge work"
            ),
        }:
            continue
        if len(row) != 6 or row[1:] != _OFFICIAL_PRICE_CELLS:
            raise RuntimeError("official price mismatch for Claude Opus 5.5")

        document_sha256 = hashlib.sha256(body).hexdigest()
        evidence: dict[str, object] = {
            "document_sha256": document_sha256,
            "model": MODEL,
            "price": dict(OFFICIAL_PRICE),
            "quote_fixture": official_quote_fixture(),
            "source_url": SOURCE_URL,
        }
        normalized = json.dumps(
            evidence,
            sort_keys=True,
            separators=(",", ":"),
        ).encode()
        evidence["evidence_sha256"] = hashlib.sha256(normalized).hexdigest()
        return evidence

    raise RuntimeError("official price row missing for Claude Opus 5.5")


def patch_payload(expected_version: str) -> dict[str, object]:
    return {
        "changes": [
            {
                "model_name": MODEL,
                "expected_version": expected_version,
                "pricing": dict(TARGET_PRICING),
            }
        ]
    }


def classify_write_outcome(
    before: dict[str, object],
    after: dict[str, object],
) -> str:
    before_version = before.get("version")
    after_version = after.get("version")
    before_configured = before.get("configured")
    after_configured = after.get("configured")
    lowercase_hex = "0123456789abcdef"
    before_version_is_valid = (
        isinstance(before_version, str)
        and len(before_version) == 64
        and all(character in lowercase_hex for character in before_version)
    )
    after_version_is_valid = (
        isinstance(after_version, str)
        and len(after_version) == 64
        and all(character in lowercase_hex for character in after_version)
    )

    if (
        before_version_is_valid
        and after_version_is_valid
        and after_version != before_version
        and after_configured == TARGET_PRICING
        and isinstance(before_configured, dict)
    ):
        return "committed"
    if (
        before_version_is_valid
        and after_version_is_valid
        and after_version == before_version
        and isinstance(before_configured, dict)
        and isinstance(after_configured, dict)
        and after_configured == before_configured
    ):
        return "not_committed"
    return "unknown"


def _canonical_hash(value: object) -> str:
    encoded = json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode()
    return hashlib.sha256(encoded).hexdigest()


def _parse_option_maps(
    options: dict[str, object],
) -> dict[str, dict[str, object]]:
    if not isinstance(options, dict):
        raise RuntimeError("pricing options are not an object")
    parsed: dict[str, dict[str, object]] = {}
    for key, raw in options.items():
        try:
            value = json.loads(raw) if isinstance(raw, str) else raw
        except (TypeError, ValueError) as exc:
            raise RuntimeError("pricing option is not valid JSON: " + key) from exc
        if not isinstance(value, dict):
            raise RuntimeError("pricing option is not an object: " + key)
        parsed[key] = value
    return parsed


def validate_option_transition(
    before_options: dict[str, object],
    after_options: dict[str, object],
) -> tuple[dict[str, str], dict[str, str]]:
    before = _parse_option_maps(before_options)
    after = _parse_option_maps(after_options)
    if set(before) != set(after):
        raise RuntimeError("pricing option key set changed")

    for key in sorted(before):
        before_map = dict(before[key])
        after_map = dict(after[key])
        if key in MUTATED_OPTION_KEYS:
            if MODEL in before_map:
                raise RuntimeError("target pricing existed before write")
            expected = MUTATED_OPTION_KEYS[key]
            if after_map.pop(MODEL, None) != expected:
                raise RuntimeError("target pricing readback mismatch")
            before_map.pop(MODEL, None)
        if before_map != after_map:
            raise RuntimeError("unrelated pricing option changed: " + key)

    return (
        {key: _canonical_hash(value) for key, value in before.items()},
        {key: _canonical_hash(value) for key, value in after.items()},
    )


def _api_payload(
    response: tuple[int, dict[str, str], bytes],
    operation: str,
) -> tuple[int, dict[str, str], dict[str, object]]:
    status, headers, raw = response
    try:
        payload = json.loads(raw or b"{}")
    except (TypeError, ValueError) as exc:
        raise RuntimeError(operation + " returned invalid JSON") from exc
    if not isinstance(payload, dict):
        raise RuntimeError(operation + " response is not an object")
    return status, headers, payload


def _successful_data(
    response: tuple[int, dict[str, str], bytes],
    operation: str,
) -> object:
    status, _, payload = _api_payload(response, operation)
    if status != 200 or payload.get("success") is not True:
        raise RuntimeError(operation + " failed")
    return payload.get("data")


def _snapshot_entry(snapshot: object) -> tuple[dict[str, object], dict[str, object]]:
    if not isinstance(snapshot, dict):
        raise RuntimeError("pricing snapshot is not an object")
    entries = snapshot.get("entries")
    options = snapshot.get("options")
    if not isinstance(entries, list) or len(entries) != 1:
        raise RuntimeError("pricing snapshot must contain exactly one entry")
    entry = entries[0]
    if (
        not isinstance(entry, dict)
        or entry.get("model_name") != MODEL
        or not isinstance(options, dict)
    ):
        raise RuntimeError("pricing snapshot target is invalid")
    return entry, options


def _validate_before_snapshot(
    snapshot: object,
) -> tuple[dict[str, object], dict[str, object], dict[str, str]]:
    entry, options = _snapshot_entry(snapshot)
    if (
        entry.get("configured") != {}
        or entry.get("version") != EMPTY_VERSION
        or snapshot.get("empty_version") != EMPTY_VERSION
    ):
        raise RuntimeError("target pricing pre-state mismatch")
    parsed = _parse_option_maps(options)
    for key in MUTATED_OPTION_KEYS:
        if MODEL in parsed.get(key, {}):
            raise RuntimeError("target pricing existed before write")
    hashes = {key: _canonical_hash(value) for key, value in parsed.items()}
    return entry, options, hashes


def _validate_preview(preview: object) -> dict[str, object]:
    if not isinstance(preview, dict):
        raise RuntimeError("pricing preview is not an object")
    if (
        preview.get("effective") != TARGET_PRICING
        or preview.get("cache_write_mode") != "claude_ttl"
        or preview.get("billing_details") != {}
    ):
        raise RuntimeError("pricing preview mismatch")
    return {
        "model": MODEL,
        "effective": dict(TARGET_PRICING),
        "cache_write_mode": "claude_ttl",
        "billing_details": {},
    }


def _validate_state(
    state: dict[str, object],
    expected_revision: str,
    *,
    pricing_lock: bool,
) -> None:
    runtime = state.get("runtime")
    locks = state.get("locks")
    plan = state.get("plan")
    route = state.get("route48")
    if (
        state.get("hostname") != EXPECTED_HOSTNAME
        or state.get("machine_id_sha256") != EXPECTED_MACHINE_ID_SHA256
        or state.get("architecture") != EXPECTED_ARCHITECTURE
        or not isinstance(runtime, dict)
        or runtime.get("container") != "new-api"
        or runtime.get("status") != "running"
        or runtime.get("health") != "healthy"
        or runtime.get("restart_count") != 0
        or runtime.get("revision") != expected_revision
        or not isinstance(locks, dict)
        or locks.get("deployment") is not False
        or locks.get("pricing") is not pricing_lock
        or not isinstance(plan, dict)
        or plan.get("target_channels") != {"4": 3, "66": 3, "96": 3}
        or plan.get("target_enabled_abilities") != 0
        or plan.get("authority_states") != {"active": 3, "disabled": 1}
        or plan.get("temporary_probe_token_count") != 0
        or not isinstance(route, dict)
        or route.get("channel_id") != CHANNEL_ID
        or route.get("channel_status") != 1
        or route.get("state") != "active"
        or not _is_nonnegative_int(route.get("consecutive_failures"))
        or not _is_nonnegative_int(route.get("failure_window_start"))
        or not _is_nonnegative_int(route.get("last_failure_at"))
    ):
        raise RuntimeError("production state assertion failed")


def _is_nonnegative_int(value: object) -> bool:
    return isinstance(value, int) and not isinstance(value, bool) and value >= 0


def _runtime_text(state: dict[str, object]) -> str:
    runtime = state["runtime"]
    assert isinstance(runtime, dict)
    return (
        "hostname=%s\n"
        "machine_id_sha256=%s\n"
        "architecture=%s\n"
        "container=%s\n"
        "revision=%s\n"
        "status=%s\n"
        "health=%s\n"
        "restart_count=%s\n"
        "deployment_lock=%s\n"
        "pricing_lock=%s\n"
        % (
            state["hostname"],
            state["machine_id_sha256"],
            state["architecture"],
            runtime["container"],
            runtime["revision"],
            runtime["status"],
            runtime["health"],
            runtime["restart_count"],
            str(state["locks"]["deployment"]).lower(),
            str(state["locks"]["pricing"]).lower(),
        )
    )


def _plan_text(state: dict[str, object]) -> str:
    plan = state["plan"]
    route = state["route48"]
    assert isinstance(plan, dict)
    assert isinstance(route, dict)
    channels = plan["target_channels"]
    authority = plan["authority_states"]
    assert isinstance(channels, dict)
    assert isinstance(authority, dict)
    return (
        "target_channels=4:%s,66:%s,96:%s\n"
        "target_enabled_abilities=%s\n"
        "authority_states=active:%s,disabled:%s\n"
        "temporary_probe_token_count=%s\n"
        "route48_channel_status=%s\n"
        "route48_state=%s\n"
        "route48_consecutive_failures=%s\n"
        "route48_failure_window_start=%s\n"
        "route48_last_failure_at=%s\n"
        % (
            channels["4"],
            channels["66"],
            channels["96"],
            plan["target_enabled_abilities"],
            authority["active"],
            authority["disabled"],
            plan["temporary_probe_token_count"],
            route.get("channel_status", ""),
            route.get("state", ""),
            route.get("consecutive_failures", ""),
            route.get("failure_window_start", ""),
            route.get("last_failure_at", ""),
        )
    )


def _target_evidence(
    entry: dict[str, object],
    hashes: dict[str, str],
) -> dict[str, object]:
    return {
        "model": MODEL,
        "target_entry": {
            "model_name": entry.get("model_name"),
            "version": entry.get("version"),
            "configured": entry.get("configured"),
        },
        "option_sha256": dict(sorted(hashes.items())),
    }


def parse_existing_token_row(
    row: str,
    *,
    expected_user_id: int,
    expected_token_id: int,
) -> str:
    if expected_user_id <= 1 or expected_token_id <= 1:
        raise RuntimeError("dedicated E2E user/token IDs must be greater than 1")
    parts = row.split("|")
    if len(parts) != 8:
        raise RuntimeError("dedicated E2E token readback is invalid")
    (
        token_id,
        user_id,
        token_status,
        unlimited_quota,
        token_name,
        key,
        user_role,
        user_status,
    ) = parts
    if (
        int(token_id or 0) != expected_token_id
        or int(user_id or 0) != expected_user_id
        or int(token_status or 0) != 1
        or unlimited_quota not in {"t", "true"}
        or "ark-probe" not in token_name.lower()
        or not key
        or int(user_role or 0) != 10
        or int(user_status or 0) != 1
    ):
        raise RuntimeError("dedicated E2E token policy mismatch")
    return key if key.startswith("sk-") else "sk-" + key


def _header_value(headers: dict[str, str], name: str) -> str:
    for key, value in headers.items():
        if key.lower() == name.lower():
            return value
    return ""


def _redacted_text(value: object) -> str:
    text = str(value)
    sensitive_name = (
        r"(?:authorization|set[-_]?cookie|cookie|password|passwd|"
        r"access[_-]?token|api[_-]?key|token|secret|key)"
    )
    header_name = r"(?:authorization|set[-_]?cookie|cookie)"
    text = re.sub(
        (
            r"(?is)(?P<prefix>(?<![A-Za-z0-9_-])[\"']?"
            + header_name
            + r"(?![A-Za-z0-9_-])[\"']?\s*[:=]\s*)"
            r"(?![\"']).*?"
            r"(?=(?:[,;]\s*[\"']?[A-Za-z][A-Za-z0-9_-]*[\"']?\s*:)"
            r"|(?:\s+and\s+[\"']?"
            + sensitive_name
            + r"[\"']?\s*[:=])|[\r\n}\]]|$)"
        ),
        r"\g<prefix>[REDACTED]",
        text,
    )
    text = re.sub(
        (
            r"(?i)\bBearer\s+"
            r"(?:\"[^\"]*\"|'[^']*'|[^,\s;}\]]+)"
        ),
        "Bearer [REDACTED]",
        text,
    )
    text = re.sub(r"\bsk-[A-Za-z0-9._-]+", "sk-[REDACTED]", text)
    text = re.sub(
        (
            r"(?i)(?P<prefix>(?<![A-Za-z0-9_-])"
            r"[\"']?"
            + sensitive_name
            + r"(?![A-Za-z0-9_-])[\"']?\s*[:=]\s*)"
            r"(?P<quote>[\"'])(?:\\.|(?!(?P=quote)).)*(?P=quote)"
        ),
        r"\g<prefix>\g<quote>[REDACTED]\g<quote>",
        text,
    )
    text = re.sub(
        (
            r"(?i)(?P<prefix>(?<![A-Za-z0-9_-])"
            r"[\"']?"
            + sensitive_name
            + r"(?![A-Za-z0-9_-])[\"']?\s*[:=]\s*)"
            r"[^,;\s}\]]+"
        ),
        r"\g<prefix>[REDACTED]",
        text,
    )
    return text[:500]


def _http_error(status: int, raw: bytes) -> dict[str, object]:
    result: dict[str, object] = {"http_status": status}
    try:
        payload = json.loads(raw or b"{}")
    except (TypeError, ValueError):
        result["message"] = "non-JSON response"
        return result
    if not isinstance(payload, dict):
        result["message"] = "non-object response"
        return result
    error = payload.get("error")
    if isinstance(error, dict):
        for key in ("type", "code", "message"):
            if key in error:
                result[key] = _redacted_text(error[key])
    else:
        message = payload.get("message") or payload.get("msg")
        if message:
            result["message"] = _redacted_text(message)
    return result


def _classify_canary_body(raw: bytes) -> str:
    try:
        payload = json.loads(raw)
    except (TypeError, ValueError):
        return "invalid_json"
    if not isinstance(payload, dict):
        return "non_object"
    if "error" in payload:
        return "top_level_error"
    if payload.get("success") is False:
        return "success_false"
    if "choices" not in payload:
        return "choices_missing"
    choices = payload["choices"]
    if not isinstance(choices, list):
        return "choices_not_list"
    if not choices:
        return "choices_empty"
    choice = choices[0]
    if not isinstance(choice, dict) or "message" not in choice:
        return "message_missing"
    message = choice["message"]
    if not isinstance(message, dict):
        return "message_not_object"
    if "content" not in message:
        return "content_missing"
    content = message["content"]
    if not isinstance(content, str):
        return "content_not_string"
    if content.strip() != "OK":
        return "content_not_ok"
    if "role" not in message:
        return "role_missing"
    role = message["role"]
    if not isinstance(role, str):
        return "role_not_string"
    if role != "assistant":
        return "role_not_assistant"
    return "assistant_ok"


def _emit_result(
    deps: object,
    write_outcome: str,
    canary: dict[str, object],
    lock_tombstone_path: str | None,
    lock_state: str | None = None,
) -> None:
    print("output_dir=%s" % deps.output_dir, file=deps.stdout)
    print("write_outcome=%s" % write_outcome, file=deps.stdout)
    channel = canary.get("channel_id")
    print(
        "canary_status=%s channel=%s"
        % (
            canary.get("status", "not_run"),
            channel if channel is not None else "none",
        ),
        file=deps.stdout,
    )
    if lock_state is not None:
        print("lock_state=%s" % lock_state, file=deps.stdout)
    print(
        "lock_tombstone_path=%s"
        % (lock_tombstone_path if lock_tombstone_path else "none"),
        file=deps.stdout,
    )


def execute_operation(deps: object) -> int:
    """Execute one fail-closed pricing write using injected side effects."""
    run_id = uuid.uuid4().hex
    write_outcome = "not_committed"
    canary: dict[str, object] = {
        "status": "not_run",
        "attempted": False,
        "outcome_known": False,
        "http_status": None,
        "request_id": "",
        "channel_id": None,
        "body_classification": None,
        "duration_ms": None,
    }
    summary: dict[str, object] = {
        "model": MODEL,
        "expected_revision": deps.expected_revision,
        "output_dir": str(deps.output_dir),
        "write_outcome": write_outcome,
        "canary_status": "not_run",
        "canary_attempted": False,
        "canary_outcome_known": False,
        "canary_channel": None,
        "canary_duration_ms": None,
        "pricing_readback_valid": False,
        "poststate_valid": False,
        "evidence_complete": False,
        "sealed": False,
        "required_artifacts": sorted(REQUIRED_ARTIFACTS),
        "evidence_errors": [],
        "lock_retained": False,
        "final_lock_absent": False,
        "lock_state": "not_acquired",
        "lock_tombstone_path": None,
        "run_id": run_id,
        "seal_id": None,
        "seal_marker_path": None,
        "temporary_probe_token_count_before": None,
        "temporary_probe_token_count_after": None,
        "route48_before": None,
        "route48_after": None,
        "official_quote_fixture": official_quote_fixture(),
        "failure": "",
    }
    lock_acquired = False
    output_created = False
    write_attempted = False
    write_outcome_known = False
    canary_attempted = False
    canary_outcome_known = False
    before_entry: dict[str, object] | None = None
    before_options: dict[str, object] | None = None
    relay_headers: dict[str, str] | None = None
    canary_body: dict[str, object] | None = None
    poststate_required = False
    lock_tombstone_path: str | None = None
    required_artifacts = set(REQUIRED_ARTIFACTS)
    evidence_errors: list[str] = []
    deferred_operational_exceptions: list[BaseException] = []
    deferred_artifact_exceptions: list[BaseException] = []
    acquire_failed = False
    finalization_failed = False
    exit_code = 1

    def record_evidence_error(name: str, exc: BaseException) -> None:
        evidence_errors.append(
            _redacted_text(
                "%s: %s: %s" % (name, type(exc).__name__, exc)
            )
        )
        if (
            not isinstance(exc, Exception)
            and not deferred_artifact_exceptions
        ):
            deferred_artifact_exceptions.append(exc)

    try:
        before_state = deps.capture_state("before")
        _validate_state(
            before_state,
            deps.expected_revision,
            pricing_lock=False,
        )
        summary["temporary_probe_token_count_before"] = before_state[
            "plan"
        ]["temporary_probe_token_count"]
        summary["route48_before"] = dict(before_state["route48"])
        deps.acquire_lock(
            {
                "model": MODEL,
                "expected_revision": deps.expected_revision,
                "output_dir": str(deps.output_dir),
                "run_id": run_id,
                "state": "pending_snapshot",
            }
        )
        lock_acquired = True
        if deps.deployment_lock_exists():
            raise RuntimeError(
                "deployment lock appeared after pricing lock acquisition"
            )
        deps.create_output_dir()
        output_created = True
        deps.write_text("runtime-before.txt", _runtime_text(before_state))
        deps.write_text("plan-before.txt", _plan_text(before_state))

        deps.copy_compose()
        deps.copy_extension()
        deps.dump_database()

        official = parse_official_page(deps.fetch(SOURCE_URL, 30))
        deps.write_json("official-evidence.json", official)

        admin_headers = deps.root_headers()
        before_snapshot = _successful_data(
            deps.request(
                "GET",
                PRICING_SNAPSHOT_PATH,
                admin_headers,
                timeout=120,
            ),
            "pricing snapshot",
        )
        before_entry, before_options, before_hashes = (
            _validate_before_snapshot(before_snapshot)
        )
        deps.update_lock_owner(
            {
                "model": MODEL,
                "expected_revision": deps.expected_revision,
                "output_dir": str(deps.output_dir),
                "run_id": run_id,
                "state": "snapshot_captured",
                "captured_pricing_version": before_entry["version"],
            }
        )
        deps.write_json(
            "pricing-before.json",
            _target_evidence(before_entry, before_hashes),
        )

        preview = _validate_preview(
            _successful_data(
                deps.request(
                    "POST",
                    PREVIEW_PATH,
                    admin_headers,
                    {
                        "model_name": MODEL,
                        "pricing": dict(TARGET_PRICING),
                    },
                    timeout=120,
                ),
                "pricing preview",
            )
        )
        deps.write_json("pricing-preview.json", preview)
        poststate_required = True
        token = parse_existing_token_row(
            deps.existing_token_row(),
            expected_user_id=deps.existing_token_user_id,
            expected_token_id=deps.existing_token_id,
        )
        relay_headers = {
            "Authorization": "Bearer " + token + "-48",
            "Content-Type": "application/json",
            "Accept-Encoding": "identity",
        }
        canary_body = {
            "model": MODEL,
            "messages": [
                {
                    "role": "user",
                    "content": "Reply with exactly: OK",
                }
            ],
            "max_tokens": 32,
            "stream": False,
        }
        deps.write_checksums("SHA256SUMS.pre")
        deps.validate_artifact_set(set(PRE_PATCH_REQUIRED_ARTIFACTS))
        deps.validate_checksums("SHA256SUMS.pre")
        deps.write_json(
            "pricing-write-error.json",
            {"model": MODEL, "status": "not_applicable"},
        )
        deps.write_json(
            "pricing-conflict.json",
            {"model": MODEL, "status": "not_applicable"},
        )

        patch_response: tuple[int, dict[str, str], bytes] | None = None
        if deps.deployment_lock_exists():
            raise RuntimeError(
                "deployment lock appeared immediately before pricing write"
            )
        try:
            write_attempted = True
            patch_response = deps.request(
                "PATCH",
                PRICING_PATH,
                admin_headers,
                patch_payload(str(before_entry["version"])),
                timeout=120,
            )
        except Exception as exc:
            summary["failure"] = _redacted_text(
                "pricing write transport error: %s: %s"
                % (type(exc).__name__, exc)
            )

        conflict_response = False
        patch_explicit_failure = False
        if patch_response is not None:
            status, _, raw = patch_response
            conflict_response = status == 409
            try:
                _, _, response_payload = _api_payload(
                    patch_response,
                    "pricing write",
                )
            except RuntimeError:
                response_payload = None
            response_succeeded = (
                status == 200
                and response_payload is not None
                and response_payload.get("success") is True
            )
            patch_explicit_failure = not response_succeeded
            if not response_succeeded:
                required_artifacts.add("pricing-write-error.json")
                summary["failure"] = (
                    "pricing write HTTP %d" % status
                    if status != 200
                    else "pricing write rejected"
                )
                try:
                    deps.write_json(
                        "pricing-write-error.json",
                        _http_error(status, raw),
                    )
                except BaseException as exc:
                    record_evidence_error(
                        "pricing-write-error.json",
                        exc,
                    )

        try:
            after_snapshot = _successful_data(
                deps.request(
                    "GET",
                    PRICING_SNAPSHOT_PATH,
                    admin_headers,
                    timeout=120,
                ),
                "pricing readback",
            )
            after_entry, after_options = _snapshot_entry(after_snapshot)
            after_hashes = {
                key: _canonical_hash(value)
                for key, value in _parse_option_maps(after_options).items()
            }
        except BaseException as exc:
            if not patch_explicit_failure:
                raise
            write_outcome = "unknown"
            required_artifacts.add("pricing-conflict.json")
            try:
                deps.write_json(
                    "pricing-conflict.json",
                    {
                        "model": MODEL,
                        "status": "conflict_state_unavailable",
                        "error": {
                            "class": type(exc).__name__,
                            "message": _redacted_text(exc),
                        },
                    },
                )
            except BaseException as artifact_exc:
                record_evidence_error(
                    "pricing-conflict.json",
                    artifact_exc,
                )
            summary["failure"] = _redacted_text(
                "pricing conflict reload error: %s: %s"
                % (type(exc).__name__, exc)
            )
            if not isinstance(exc, Exception):
                raise
        else:
            readback_outcome = classify_write_outcome(
                before_entry,
                after_entry,
            )
            exact_prestate = (
                readback_outcome == "not_committed"
                and after_hashes == before_hashes
            )
            if patch_explicit_failure:
                write_outcome = (
                    "not_committed" if exact_prestate else "unknown"
                )
                write_outcome_known = exact_prestate
            else:
                write_outcome = readback_outcome
                if write_outcome == "committed":
                    assert before_options is not None
                    _, after_hashes = validate_option_transition(
                        before_options,
                        after_options,
                    )
                    write_outcome_known = True
                    summary["pricing_readback_valid"] = True
                elif write_outcome == "not_committed":
                    if not exact_prestate:
                        raise RuntimeError(
                            "pricing pre-state readback mismatch"
                        )
                    write_outcome_known = True

            readback_evidence = _target_evidence(
                after_entry,
                after_hashes,
            )
            if patch_explicit_failure and (
                conflict_response or not exact_prestate
            ):
                required_artifacts.add("pricing-conflict.json")
                try:
                    deps.write_json(
                        "pricing-conflict.json",
                        readback_evidence,
                    )
                except BaseException as exc:
                    record_evidence_error(
                        "pricing-conflict.json",
                        exc,
                    )
            try:
                deps.write_json(
                    "pricing-after.json",
                    readback_evidence,
                )
            except BaseException as exc:
                record_evidence_error("pricing-after.json", exc)

        if write_outcome == "committed":
            assert relay_headers is not None
            assert canary_body is not None
            canary_attempted = True
            canary["attempted"] = True
            canary_started: float | None = None
            timing_error: dict[str, str] | None = None
            try:
                canary_started = deps.monotonic()
            except BaseException as exc:
                timing_error = {
                    "stage": "start",
                    "class": type(exc).__name__,
                    "message": _redacted_text(
                        "%s: %s" % (type(exc).__name__, exc)
                    ),
                }
                if not isinstance(exc, Exception):
                    deferred_operational_exceptions.append(exc)
            try:
                canary_response = deps.request(
                    "POST",
                    CANARY_PATH,
                    relay_headers,
                    canary_body,
                    timeout=300,
                )
            except BaseException as exc:
                interrupted = not isinstance(exc, Exception)
                canary_outcome_known = not interrupted
                canary = {
                    "status": (
                        "unknown_after_attempt"
                        if interrupted
                        else "failed"
                    ),
                    "attempted": True,
                    "outcome_known": canary_outcome_known,
                    "http_status": None,
                    "request_id": "",
                    "channel_id": None,
                    "body_classification": None,
                    "duration_ms": None,
                    "error": {
                        "kind": (
                            "interruption"
                            if interrupted
                            else "transport"
                        ),
                        "class": type(exc).__name__,
                        "message": _redacted_text(
                            "%s: %s" % (type(exc).__name__, exc)
                        ),
                    },
                }
                if interrupted:
                    deferred_operational_exceptions.append(exc)
            else:
                try:
                    status, response_headers, raw = canary_response
                    body_classification = _classify_canary_body(raw)
                    canary_outcome_known = True
                    canary = {
                        "status": "failed",
                        "attempted": True,
                        "outcome_known": True,
                        "http_status": status,
                        "request_id": "",
                        "channel_id": None,
                        "body_classification": body_classification,
                        "duration_ms": None,
                    }
                    if status != 200:
                        canary["response_error"] = _http_error(status, raw)
                    elif body_classification != "assistant_ok":
                        canary["response_error"] = {
                            "kind": "response_validation",
                            "classification": body_classification,
                            "message": (
                                "canary response body did not contain "
                                "exact assistant OK"
                            ),
                        }
                    request_id = _header_value(
                        response_headers,
                        "X-Oneapi-Request-Id",
                    )
                    canary["request_id"] = request_id
                    channel_id = None
                    log_lookup_error: dict[str, str] | None = None
                    log_lookup_interrupted = False
                    if request_id:
                        try:
                            channel_id = deps.request_log_once(request_id)
                        except BaseException as exc:
                            log_lookup_interrupted = not isinstance(
                                exc,
                                Exception,
                            )
                            log_lookup_error = {
                                "class": type(exc).__name__,
                                "message": _redacted_text(
                                    "%s: %s" % (type(exc).__name__, exc)
                                ),
                            }
                            if log_lookup_interrupted:
                                canary_outcome_known = False
                                deferred_operational_exceptions.append(exc)
                    canary["channel_id"] = channel_id
                    if log_lookup_interrupted:
                        canary["status"] = "unknown_after_response"
                        canary["outcome_known"] = False
                        canary["log_lookup_error"] = log_lookup_error
                        canary["error"] = {
                            "kind": "log_lookup_interruption",
                            "message": log_lookup_error["message"],
                        }
                    elif (
                        status == 200
                        and body_classification == "assistant_ok"
                        and bool(request_id)
                        and channel_id == CHANNEL_ID
                    ):
                        canary["status"] = "passed"
                    if canary["status"] != "passed":
                        response_error = canary.get("response_error")
                        if (
                            response_error is None
                            and not log_lookup_interrupted
                        ):
                            response_error = {
                                "kind": "route_verification",
                                "message": (
                                    "canary request ID or selected channel "
                                    "verification failed"
                                ),
                            }
                            canary["response_error"] = response_error
                        if log_lookup_error is not None:
                            canary["log_lookup_error"] = log_lookup_error
                        if not log_lookup_interrupted:
                            canary["error"] = (
                                {
                                    "kind": "log_lookup",
                                    "message": log_lookup_error["message"],
                                }
                                if log_lookup_error is not None
                                else response_error
                            )
                except BaseException as exc:
                    interrupted = not isinstance(exc, Exception)
                    canary_outcome_known = not interrupted
                    canary.update(
                        {
                            "status": (
                                "unknown_after_attempt"
                                if interrupted
                                else "failed"
                            ),
                            "attempted": True,
                            "outcome_known": canary_outcome_known,
                            "duration_ms": None,
                            "error": {
                                "kind": "result",
                                "class": type(exc).__name__,
                                "message": _redacted_text(
                                    "%s: %s" % (type(exc).__name__, exc)
                                ),
                            },
                        }
                    )
                    if interrupted:
                        deferred_operational_exceptions.append(exc)
            if canary_started is not None:
                try:
                    canary["duration_ms"] = max(
                        0,
                        round(
                            (deps.monotonic() - canary_started) * 1000
                        ),
                    )
                except BaseException as exc:
                    timing_error = {
                        "stage": "finish",
                        "class": type(exc).__name__,
                        "message": _redacted_text(
                            "%s: %s" % (type(exc).__name__, exc)
                        ),
                    }
                    if not isinstance(exc, Exception):
                        deferred_operational_exceptions.append(exc)
            if timing_error is not None:
                canary["timing_error"] = timing_error

        if write_outcome == "committed":
            exit_code = 0 if canary["status"] == "passed" else 1
        elif write_outcome == "unknown":
            exit_code = 1 if patch_explicit_failure else 2
        else:
            exit_code = 1
    except BaseException as exc:
        acquire_error = getattr(exc, "acquire_error", None)
        cleanup_errors = getattr(exc, "cleanup_errors", ())
        if isinstance(acquire_error, BaseException):
            acquire_failed = True
            lock_acquired = bool(
                getattr(exc, "active_lock_exists", False)
            )
            for related_error in (
                acquire_error,
                *(
                    error
                    for _, error in cleanup_errors
                    if isinstance(error, BaseException)
                ),
            ):
                if (
                    not isinstance(related_error, Exception)
                    and related_error not in deferred_operational_exceptions
                ):
                    deferred_operational_exceptions.append(related_error)
        if write_attempted and not write_outcome_known:
            write_outcome = "unknown"
            exit_code = 2
        else:
            exit_code = 1
        summary["failure"] = _redacted_text(
            "%s: %s" % (type(exc).__name__, exc)
        )
        if not isinstance(exc, Exception):
            raise
    finally:
        if poststate_required:
            try:
                after_state = deps.capture_state("after")
                try:
                    deps.write_text(
                        "runtime-after.txt",
                        _runtime_text(after_state),
                    )
                except BaseException as exc:
                    record_evidence_error("runtime-after.txt", exc)
                try:
                    deps.write_text(
                        "plan-after.txt",
                        _plan_text(after_state),
                    )
                except BaseException as exc:
                    record_evidence_error("plan-after.txt", exc)
                _validate_state(
                    after_state,
                    deps.expected_revision,
                    pricing_lock=True,
                )
                summary["temporary_probe_token_count_after"] = after_state[
                    "plan"
                ]["temporary_probe_token_count"]
                summary["route48_after"] = dict(after_state["route48"])
                summary["poststate_valid"] = True
            except BaseException as exc:
                if write_outcome != "unknown":
                    exit_code = 1
                summary["failure"] = _redacted_text(
                    "post-state validation error: %s: %s"
                    % (type(exc).__name__, exc)
                )
                if not isinstance(exc, Exception):
                    deferred_operational_exceptions.append(exc)
        evidence_complete = False
        release_expected = lock_acquired and write_outcome != "unknown"
        if lock_acquired:
            lock_state = (
                "held_pending_release"
                if release_expected
                else "retained_unknown_outcome"
            )
        else:
            lock_state = "absent"
        summary.update(
            {
                "write_outcome": write_outcome,
                "canary_status": canary["status"],
                "canary_attempted": canary_attempted,
                "canary_outcome_known": canary_outcome_known,
                "canary_channel": canary["channel_id"],
                "canary_duration_ms": canary["duration_ms"],
                "lock_retained": lock_acquired,
                "final_lock_absent": False,
                "lock_state": lock_state,
                "required_artifacts": sorted(required_artifacts),
                "evidence_errors": list(evidence_errors),
            }
        )
        if output_created:
            try:
                deps.write_json("canary.json", canary)
            except BaseException as exc:
                record_evidence_error("canary.json", exc)
            summary.update(
                {
                    "evidence_complete": not evidence_errors,
                    "sealed": False,
                    "evidence_errors": list(evidence_errors),
                }
            )
            try:
                deps.write_json("summary.json", summary)
            except BaseException as exc:
                record_evidence_error("summary.json", exc)
            try:
                deps.write_checksums("SHA256SUMS")
            except BaseException as exc:
                record_evidence_error("SHA256SUMS", exc)
            try:
                deps.validate_artifact_set(required_artifacts)
            except BaseException as exc:
                record_evidence_error("required artifact set", exc)
            try:
                deps.validate_unsealed_checksums("SHA256SUMS")
            except BaseException as exc:
                record_evidence_error("SHA256SUMS validation", exc)
            evidence_complete = not evidence_errors
        if not evidence_complete:
            if write_outcome != "unknown":
                exit_code = 1
            summary.update(
                {
                    "evidence_complete": False,
                    "sealed": False,
                    "evidence_errors": list(evidence_errors),
                    "lock_retained": lock_acquired,
                    "final_lock_absent": False,
                    "lock_state": (
                        "retained_acquire_failure"
                        if acquire_failed and lock_acquired
                        else (
                            "retained_evidence_failure"
                            if lock_acquired
                            else "absent"
                        )
                    ),
                }
            )
            if output_created:
                try:
                    deps.write_json("summary.json", summary)
                    deps.write_checksums("SHA256SUMS")
                    deps.validate_unsealed_checksums("SHA256SUMS")
                except BaseException as exc:
                    record_evidence_error("evidence recovery", exc)
        if (
            lock_acquired
            and write_outcome != "unknown"
            and evidence_complete
        ):
            commit_phase = "prepare"
            expected_tombstone: Path | None = None
            try:
                expected_tombstone, seal_id = deps.prepare_lock_commit()
                lock_tombstone_path = _redacted_text(expected_tombstone)
                committed_summary = {
                    **summary,
                    "lock_retained": False,
                    "final_lock_absent": True,
                    "lock_state": "absent",
                    "lock_tombstone_path": lock_tombstone_path,
                    "seal_id": seal_id,
                    "seal_marker_path": str(
                        expected_tombstone / SEAL_MARKER_NAME
                    ),
                    "evidence_complete": True,
                    "sealed": True,
                }
                if before_entry is None:
                    raise RuntimeError(
                        "captured pricing identity is unavailable"
                    )
                commit_phase = "finalize_owner"
                deps.update_lock_owner(
                    {
                        "model": MODEL,
                        "expected_revision": deps.expected_revision,
                        "output_dir": str(deps.output_dir),
                        "run_id": run_id,
                        "state": "pending_release",
                        "captured_pricing_version": before_entry["version"],
                        "seal_id": seal_id,
                    }
                )
                commit_phase = "finalize_evidence"
                deps.finalize_evidence(
                    committed_summary,
                    required_artifacts,
                )
                commit_phase = "publish_seal"
                deps.publish_seal(expected_tombstone)
                commit_phase = "rename"
                deps.release_lock(expected_tombstone)
                lock_acquired = False
                summary = committed_summary
            except BaseException as exc:
                exit_code = 1
                finalization_failed = True
                if commit_phase == "rename" and expected_tombstone is not None:
                    lock_tombstone_path = _redacted_text(expected_tombstone)
                    failed_lock_state = "commit_state_unknown"
                    lock_acquired = False
                    signal = (
                        exc.durability_error
                        if isinstance(exc, LockCommitDurabilityError)
                        else exc
                    )
                    if (
                        not isinstance(signal, Exception)
                        and signal not in deferred_operational_exceptions
                    ):
                        deferred_operational_exceptions.append(signal)
                    try:
                        (
                            active_lock_exists,
                            tombstone_exists,
                        ) = deps.lock_topology(expected_tombstone)
                    except BaseException as probe_error:
                        failure_probe_text = _redacted_text(
                            "; commit probe error: %s: %s"
                            % (type(probe_error).__name__, probe_error)
                        )
                        if (
                            not isinstance(probe_error, Exception)
                            and probe_error
                            not in deferred_operational_exceptions
                        ):
                            deferred_operational_exceptions.append(probe_error)
                    else:
                        failure_probe_text = ""
                        if active_lock_exists and not tombstone_exists:
                            failed_lock_state = "precommit_retained"
                            lock_acquired = True
                        elif not active_lock_exists and tombstone_exists:
                            try:
                                seal_state = deps.seal_publication_state(
                                    expected_tombstone
                                )
                            except BaseException as probe_error:
                                failure_probe_text = _redacted_text(
                                    "; seal probe error: %s: %s"
                                    % (
                                        type(probe_error).__name__,
                                        probe_error,
                                    )
                                )
                                if (
                                    not isinstance(probe_error, Exception)
                                    and probe_error
                                    not in deferred_operational_exceptions
                                ):
                                    deferred_operational_exceptions.append(
                                        probe_error
                                    )
                            else:
                                if seal_state == "committed":
                                    failed_lock_state = "committed"
                else:
                    failed_lock_state = "retained_commit_failure"
                    lock_tombstone_path = None
                    failure_probe_text = ""
                failure_text = _redacted_text(
                    "%s error: %s: %s"
                    % (commit_phase, type(exc).__name__, exc)
                ) + failure_probe_text
                if (
                    not isinstance(exc, Exception)
                    and exc not in deferred_operational_exceptions
                ):
                    deferred_operational_exceptions.append(exc)
        if finalization_failed:
            print("output_dir=%s" % deps.output_dir, file=deps.stdout)
            print("overall_status=failed", file=deps.stdout)
            print(
                "lock_state=%s" % failed_lock_state,
                file=deps.stdout,
            )
            print(
                "lock_tombstone_path=%s"
                % (
                    lock_tombstone_path
                    if lock_tombstone_path is not None
                    else "none"
                ),
                file=deps.stdout,
            )
            print("failure=%s" % failure_text, file=deps.stdout)
        else:
            _emit_result(
                deps,
                write_outcome,
                canary,
                lock_tombstone_path,
                (
                    str(summary["lock_state"])
                    if acquire_failed
                    else None
                ),
            )
    if deferred_operational_exceptions:
        raise deferred_operational_exceptions[0]
    if deferred_artifact_exceptions:
        raise deferred_artifact_exceptions[0]
    return exit_code


class ProductionDependencies:
    """Concrete side effects for execution on the production host."""

    def __init__(
        self,
        args: argparse.Namespace,
        *,
        stdout: object | None = None,
        root: Path = PRODUCTION_ROOT,
        backup_root: Path = BACKUP_ROOT,
        machine_id_path: Path = Path("/etc/machine-id"),
        hostname: object = socket.gethostname,
        machine: object = platform.machine,
        run: object = subprocess.run,
        check_output: object = subprocess.check_output,
        urlopen: object = urllib.request.urlopen,
        http_connection: object = http.client.HTTPConnection,
        monotonic: object = time.monotonic,
    ) -> None:
        self.root = root.resolve()
        self.backup_root = backup_root.resolve()
        self.output_dir = Path(args.output_dir).resolve()
        if self.output_dir == self.backup_root:
            raise ValueError("output directory must be under backup root")
        try:
            self.output_dir.relative_to(self.backup_root)
        except ValueError as exc:
            raise ValueError(
                "output directory must be under backup root"
            ) from exc
        if not re.fullmatch(r"[0-9a-f]{40}", args.expected_revision):
            raise ValueError("expected revision must be a full lowercase SHA")
        if (
            args.existing_token_user_id <= 1
            or args.existing_token_id <= 1
        ):
            raise ValueError("dedicated token IDs must be greater than 1")

        self.expected_revision = args.expected_revision
        self.existing_token_user_id = args.existing_token_user_id
        self.existing_token_id = args.existing_token_id
        self.stdout = stdout if stdout is not None else sys.stdout
        self.machine_id_path = machine_id_path
        self._hostname = hostname
        self._machine = machine
        self._run = run
        self._check_output = check_output
        self._urlopen = urlopen
        self._http_connection = http_connection
        self.monotonic = monotonic
        self.compose_path = self.root / "docker-compose.yml"
        self.extension_path = self.root / (
            "reports/responses-native-history-normalization-20260918/"
            "newapi-sub2api-sync-extension-v0.1.5.zip"
        )
        self.pricing_lock_path = self.root / ".pricing-change.lock"
        self.deployment_lock_path = self.root / ".deployment.lock"
        self._prepared_commit_paths: set[Path] = set()

    def _psql(self, sql: str) -> str:
        try:
            return self._check_output(
                [
                    "docker",
                    "exec",
                    "newapi-postgres",
                    "psql",
                    "-U",
                    "newapi",
                    "-d",
                    "new-api",
                    "-At",
                    "-v",
                    "ON_ERROR_STOP=1",
                    "-c",
                    sql,
                ],
                text=True,
                timeout=120,
            ).strip()
        except (KeyboardInterrupt, SystemExit):
            raise
        except Exception:
            raise RuntimeError("production database query failed") from None

    def capture_state(self, stage: str) -> dict[str, object]:
        del stage
        raw = self._check_output(
            ["docker", "inspect", "new-api"],
            text=True,
            timeout=60,
        )
        containers = json.loads(raw)
        if not isinstance(containers, list) or len(containers) != 1:
            raise RuntimeError("new-api inspect returned invalid state")
        container = containers[0]
        if not isinstance(container, dict):
            raise RuntimeError("new-api inspect returned invalid container")
        state = container.get("State")
        config = container.get("Config")
        if not isinstance(state, dict) or not isinstance(config, dict):
            raise RuntimeError("new-api inspect omitted state or config")
        health = state.get("Health")
        labels = config.get("Labels")
        if not isinstance(health, dict) or not isinstance(labels, dict):
            raise RuntimeError("new-api inspect omitted health or labels")

        temporary_probe_predicate = " OR ".join(
            "name LIKE '%s%%'" % prefix
            for prefix in TEMPORARY_PROBE_TOKEN_PREFIXES
        )
        database_raw = self._psql(
            """
SELECT json_build_object(
  'target_channels', json_build_object(
    '4', COALESCE(max(status) FILTER (WHERE id=4), -1),
    '66', COALESCE(max(status) FILTER (WHERE id=66), -1),
    '96', COALESCE(max(status) FILTER (WHERE id=96), -1)
  ),
  'target_enabled_abilities', (
    SELECT count(*) FROM abilities
    WHERE channel_id IN (4,66,96) AND enabled=true
  ),
  'authority_states', COALESCE((
    SELECT json_object_agg(state, state_count)
    FROM (
      SELECT state, count(*) AS state_count
      FROM plan_quota_domains
      GROUP BY state
      ORDER BY state
    ) authority
  ), '{}'::json),
  'temporary_probe_token_count', (
    SELECT count(*) FROM tokens
    WHERE deleted_at IS NULL AND (%s)
  ),
  'route48', COALESCE((
    SELECT json_build_object(
      'channel_id', c.id,
      'channel_status', c.status,
      'state', r.state,
      'consecutive_failures', r.consecutive_failures,
      'failure_window_start', r.failure_window_start,
      'last_failure_at', r.last_failure_at
    )
    FROM channels c
    LEFT JOIN upstream_managed_routes r ON r.channel_id=c.id
    WHERE c.id=48
  ), '{}'::json)
)::text
FROM channels
WHERE id IN (4,66,96);
"""
            % temporary_probe_predicate
        )
        database_state = json.loads(database_raw or "{}")
        if not isinstance(database_state, dict):
            raise RuntimeError("production database state is invalid")
        machine_id = self.machine_id_path.read_bytes().strip()
        return {
            "hostname": self._hostname(),
            "machine_id_sha256": hashlib.sha256(machine_id).hexdigest(),
            "architecture": self._machine(),
            "runtime": {
                "container": "new-api",
                "status": state.get("Status"),
                "health": health.get("Status"),
                "restart_count": container.get("RestartCount"),
                "revision": labels.get(
                    "org.opencontainers.image.revision"
                ),
            },
            "locks": {
                "deployment": self.deployment_lock_exists(),
                "pricing": self.pricing_lock_path.exists(),
            },
            "plan": {
                "target_channels": database_state.get("target_channels"),
                "target_enabled_abilities": database_state.get(
                    "target_enabled_abilities"
                ),
                "authority_states": database_state.get(
                    "authority_states"
                ),
                "temporary_probe_token_count": database_state.get(
                    "temporary_probe_token_count"
                ),
            },
            "route48": database_state.get("route48"),
        }

    def deployment_lock_exists(self) -> bool:
        try:
            os.lstat(self.deployment_lock_path)
        except FileNotFoundError:
            return False
        return True

    def acquire_lock(self, owner: dict[str, object]) -> None:
        self._validate_released_tombstones()
        os.mkdir(self.pricing_lock_path, 0o700)
        owner_path = self.pricing_lock_path / "owner"
        descriptor: int | None = None
        try:
            os.chmod(self.pricing_lock_path, 0o700)
            self._fsync_directory(self.root)
            self._validate_released_tombstones(allow_active_lock=True)
            descriptor = os.open(
                owner_path,
                os.O_CREAT
                | os.O_EXCL
                | os.O_WRONLY
                | getattr(os, "O_NOFOLLOW", 0),
                0o600,
            )
            remaining = memoryview(self._owner_bytes(owner))
            while remaining:
                written = os.write(descriptor, remaining)
                if written <= 0:
                    raise OSError("owner write made no progress")
                remaining = remaining[written:]
            os.chmod(owner_path, 0o600)
            os.fsync(descriptor)
            os.close(descriptor)
            descriptor = None
            self._fsync_directory(self.pricing_lock_path)
        except BaseException as acquire_error:
            cleanup_errors: list[tuple[str, BaseException]] = []
            if descriptor is not None:
                try:
                    os.close(descriptor)
                except BaseException as cleanup_error:
                    cleanup_errors.append(
                        ("close_owner", cleanup_error)
                    )
            try:
                owner_path.unlink(missing_ok=True)
            except BaseException as cleanup_error:
                cleanup_errors.append(("unlink_owner", cleanup_error))
            try:
                self.pricing_lock_path.rmdir()
            except BaseException as cleanup_error:
                cleanup_errors.append(("rmdir_lock", cleanup_error))
            try:
                self._fsync_directory(self.root)
            except BaseException as cleanup_error:
                cleanup_errors.append(("fsync_root", cleanup_error))
            if cleanup_errors:
                raise LockAcquireCleanupError(
                    acquire_error,
                    cleanup_errors,
                    active_lock_exists=os.path.lexists(
                        self.pricing_lock_path
                    ),
                    owner_exists=os.path.lexists(owner_path),
                ) from acquire_error
            raise

    @staticmethod
    def _owner_bytes(owner: dict[str, object]) -> bytes:
        safe_owner = {
            "model": owner.get("model"),
            "expected_revision": owner.get("expected_revision"),
            "output_dir": owner.get("output_dir"),
            "run_id": owner.get("run_id"),
            "state": owner.get("state"),
            "pid": os.getpid(),
        }
        if "captured_pricing_version" in owner:
            safe_owner["captured_pricing_version"] = owner.get(
                "captured_pricing_version"
            )
        if "seal_id" in owner:
            safe_owner["seal_id"] = owner.get("seal_id")
        return (
            json.dumps(safe_owner, sort_keys=True, separators=(",", ":"))
            + "\n"
        ).encode()

    @staticmethod
    def _fsync_directory(path: Path) -> None:
        descriptor = os.open(
            path,
            os.O_RDONLY | getattr(os, "O_DIRECTORY", 0),
        )
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def update_lock_owner(self, owner: dict[str, object]) -> None:
        owner_path = self.pricing_lock_path / "owner"
        self._write_bytes_atomically(owner_path, self._owner_bytes(owner))

    def create_output_dir(self) -> None:
        os.mkdir(self.output_dir, 0o700)
        os.chmod(self.output_dir, 0o700)
        self._fsync_directory(self.output_dir.parent)

    def copy_compose(self) -> None:
        try:
            self._copy_verified_file(
                self.compose_path,
                self.output_dir / "docker-compose.yml",
            )
        except FileNotFoundError as exc:
            raise RuntimeError(
                "production Compose file is missing"
            ) from exc

    def copy_extension(self) -> None:
        destination = (
            self.output_dir
            / "newapi-sub2api-sync-extension-v0.1.5.zip"
        )
        try:
            self._copy_verified_file(self.extension_path, destination)
        except FileNotFoundError as exc:
            raise RuntimeError(
                "production extension ZIP is missing"
            ) from exc

    def dump_database(self) -> None:
        dump_path = self.output_dir / "new-api.dump"
        dump_descriptor = self._open_new_private_file(dump_path)
        with os.fdopen(dump_descriptor, "wb") as dump:
            self._run(
                [
                    "docker",
                    "exec",
                    "newapi-postgres",
                    "pg_dump",
                    "-U",
                    "newapi",
                    "-d",
                    "new-api",
                    "-Fc",
                ],
                check=True,
                stdout=dump,
                timeout=600,
            )
        self._make_file_durable(dump_path)
        listing_path = self.output_dir / "pg-restore-list.txt"
        dump_descriptor = self._open_private_regular_file(dump_path)
        listing_descriptor = self._open_new_private_file(listing_path)
        with (
            os.fdopen(dump_descriptor, "rb") as dump,
            os.fdopen(listing_descriptor, "wb") as listing,
        ):
            self._run(
                [
                    "docker",
                    "exec",
                    "-i",
                    "newapi-postgres",
                    "pg_restore",
                    "--list",
                ],
                check=True,
                stdin=dump,
                stdout=listing,
                timeout=120,
            )
        self._make_file_durable(listing_path)

    def fetch(self, url: str, timeout: int) -> bytes:
        if url != SOURCE_URL or timeout != 30:
            raise RuntimeError("official pricing fetch contract mismatch")
        request = urllib.request.Request(
            url,
            headers={"User-Agent": "newapi-pricing-audit/1"},
        )
        with self._urlopen(request, timeout=timeout) as response:
            status = getattr(response, "status", 200)
            final_url = response.geturl()
            if status != 200 or final_url != SOURCE_URL:
                raise RuntimeError("official pricing fetch failed")
            return response.read()

    def root_headers(self) -> dict[str, str]:
        token = self._psql(
            "SELECT access_token FROM users WHERE id=1 AND status=1"
        )
        if not token:
            raise RuntimeError("enabled root access token is unavailable")
        return {
            "Authorization": "Bearer " + token,
            "New-Api-User": "1",
            "Content-Type": "application/json",
            "Accept-Encoding": "identity",
        }

    def request(
        self,
        method: str,
        path: str,
        headers: dict[str, str],
        body: dict[str, object] | None = None,
        timeout: int = 120,
    ) -> tuple[int, dict[str, str], bytes]:
        connection = self._http_connection(
            "127.0.0.1",
            3000,
            timeout=timeout,
        )
        operation_error: BaseException | None = None
        close_error: BaseException | None = None
        result: tuple[int, dict[str, str], bytes] | None = None
        try:
            payload = (
                json.dumps(body, ensure_ascii=False).encode()
                if body is not None
                else None
            )
            connection.request(method, path, body=payload, headers=headers)
            response = connection.getresponse()
            status = response.status
            response_headers = dict(response.getheaders())
            content_encoding = response.getheader("Content-Encoding") or ""
            raw = response.read()
            result = (status, response_headers, raw)
        except BaseException as exc:
            operation_error = exc
        try:
            connection.close()
        except BaseException as exc:
            close_error = exc
        if (
            operation_error is not None
            and not isinstance(operation_error, Exception)
        ):
            raise operation_error
        if close_error is not None and not isinstance(close_error, Exception):
            raise close_error
        if operation_error is not None:
            if close_error is not None:
                raise operation_error from close_error
            raise RuntimeError("local API request failed") from None
        if close_error is not None:
            raise close_error
        assert result is not None
        status, response_headers, raw = result
        encodings = {
            value.strip().lower()
            for value in content_encoding.split(",")
            if value.strip()
        }
        try:
            if "gzip" in encodings or raw[:2] == b"\x1f\x8b":
                raw = gzip.decompress(raw)
        except Exception:
            raise RuntimeError("local API response decompression failed") from None
        return status, response_headers, raw

    def existing_token_row(self) -> str:
        return self._psql(
            "SELECT t.id::text || '|' || t.user_id::text || '|' || "
            "t.status::text || '|' || t.unlimited_quota::text || '|' || "
            "t.name || '|' || t.key || '|' || u.role::text || '|' || "
            "u.status::text "
            "FROM tokens t JOIN users u ON u.id=t.user_id "
            "WHERE t.id=%d AND t.user_id=%d AND t.deleted_at IS NULL "
            "AND u.deleted_at IS NULL"
            % (self.existing_token_id, self.existing_token_user_id)
        )

    def request_log_once(self, request_id: str) -> int:
        if not re.fullmatch(r"[A-Za-z0-9_-]+", request_id):
            return 0
        row = self._psql(
            "SELECT channel_id FROM logs "
            "WHERE request_id='%s' AND type IN (2,5) "
            "ORDER BY id DESC LIMIT 1" % request_id
        )
        if not row:
            return 0
        return int(row.split("\t", 1)[0])

    def write_json(self, name: str, payload: object) -> None:
        path = self.output_dir / name
        self._write_bytes_atomically(path, self._json_bytes(payload))

    @staticmethod
    def _json_bytes(payload: object) -> bytes:
        return (
            json.dumps(
                payload,
                ensure_ascii=False,
                indent=2,
                sort_keys=True,
            )
            + "\n"
        ).encode()

    def write_text(self, name: str, payload: str) -> None:
        self._write_bytes_atomically(
            self.output_dir / name,
            payload.encode("utf-8"),
        )

    def write_checksums(self, name: str) -> None:
        entries = []
        for path in sorted(self.output_dir.iterdir()):
            if (
                not path.is_file()
                or path.name == name
                or path.name == "SHA256SUMS"
            ):
                continue
            digest = self._file_sha256(path)
            entries.append("%s  %s\n" % (digest, path.name))
        self._write_bytes_atomically(
            self.output_dir / name,
            "".join(entries).encode("ascii"),
        )

    def invalidate_evidence(self) -> None:
        manifest_path = self.output_dir / "SHA256SUMS"
        try:
            self._lstat_exclusive_regular_file(manifest_path)
        except FileNotFoundError:
            pass
        else:
            manifest_path.unlink()
        self._fsync_directory(self.output_dir)

    def seal_publication_state(self, tombstone_path: Path) -> str:
        tombstone_path = self._validated_tombstone_path(tombstone_path)
        marker_path = tombstone_path / SEAL_MARKER_NAME
        try:
            os.lstat(marker_path)
        except FileNotFoundError:
            return "absent"
        try:
            self._validate_checksums(
                "SHA256SUMS",
                require_seal=True,
                expected_tombstone=tombstone_path,
                allow_active_lock=True,
            )
        except Exception:
            return "present_unverified"
        return "committed"

    def finalize_evidence(
        self,
        summary: dict[str, object],
        required_artifacts: set[str],
    ) -> None:
        manifest_name = "SHA256SUMS"
        summary_name = "summary.json"
        if required_artifacts != set(REQUIRED_ARTIFACTS):
            raise RuntimeError("final evidence artifact set is incomplete")
        summary["required_artifacts"] = sorted(required_artifacts)
        self.invalidate_evidence()
        self.validate_artifact_set(required_artifacts - {manifest_name})
        summary_raw = self._json_bytes(summary)
        entries = []
        for filename in sorted(required_artifacts - {manifest_name}):
            if "/" in filename:
                raise RuntimeError("invalid final evidence artifact name")
            digest = (
                hashlib.sha256(summary_raw).hexdigest()
                if filename == summary_name
                else self._file_sha256(self.output_dir / filename)
            )
            entries.append("%s  %s\n" % (digest, filename))
        manifest_raw = "".join(entries).encode("ascii")
        suffix = "%d.%s.final" % (os.getpid(), uuid.uuid4().hex)
        staged_summary = self.output_dir / (
            ".%s.%s" % (summary_name, suffix)
        )
        staged_manifest = self.output_dir / (
            ".%s.%s" % (manifest_name, suffix)
        )
        try:
            self._write_bytes_atomically(staged_summary, summary_raw)
            self._write_bytes_atomically(staged_manifest, manifest_raw)
            if self._file_sha256(staged_summary) != hashlib.sha256(
                summary_raw
            ).hexdigest():
                raise RuntimeError("staged summary checksum mismatch")
            if staged_manifest.read_bytes() != manifest_raw:
                raise RuntimeError("staged checksum manifest mismatch")
            self._lstat_exclusive_regular_file(
                self.output_dir / summary_name
            )
            os.replace(staged_summary, self.output_dir / summary_name)
            self._fsync_directory(self.output_dir)
            if self._file_sha256(
                self.output_dir / summary_name
            ) != hashlib.sha256(summary_raw).hexdigest():
                raise RuntimeError("published summary checksum mismatch")
            if os.path.lexists(self.output_dir / manifest_name):
                raise RuntimeError(
                    "checksum manifest appeared during finalization"
                )
            os.replace(staged_manifest, self.output_dir / manifest_name)
            self._fsync_directory(self.output_dir)
            self._validate_checksums(
                manifest_name,
                require_seal=False,
                expected_tombstone=Path(
                    str(summary["lock_tombstone_path"])
                ),
                publication_pending=True,
            )
        except BaseException:
            staged_summary.unlink(missing_ok=True)
            staged_manifest.unlink(missing_ok=True)
            raise

    @classmethod
    def _write_bytes_atomically(cls, path: Path, payload: bytes) -> None:
        path = Path(path)
        try:
            original_stat = cls._lstat_exclusive_regular_file(path)
        except FileNotFoundError:
            original_stat = None
        temporary_path = path.with_name(
            ".%s.%d.tmp" % (path.name, os.getpid())
        )
        descriptor = os.open(
            temporary_path,
            os.O_CREAT
            | os.O_EXCL
            | os.O_WRONLY
            | getattr(os, "O_NOFOLLOW", 0),
            0o600,
        )
        try:
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(payload)
                handle.flush()
                os.fchmod(handle.fileno(), 0o600)
                cls._validate_private_file_stat(
                    temporary_path,
                    os.fstat(handle.fileno()),
                )
                os.fsync(handle.fileno())
            try:
                current_stat = cls._lstat_exclusive_regular_file(path)
            except FileNotFoundError:
                if original_stat is not None:
                    raise RuntimeError(
                        "%s disappeared during atomic write" % path.name
                    )
            else:
                if (
                    original_stat is None
                    or current_stat.st_dev != original_stat.st_dev
                    or current_stat.st_ino != original_stat.st_ino
                ):
                    raise RuntimeError(
                        "%s changed during atomic write" % path.name
                    )
            os.replace(temporary_path, path)
            cls._fsync_directory(path.parent)
        except BaseException:
            temporary_path.unlink(missing_ok=True)
            raise

    @classmethod
    def _fsync_file(cls, path: Path) -> None:
        descriptor = cls._open_verified_regular_file(path)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    @classmethod
    def _make_file_durable(cls, path: Path) -> None:
        cls._fsync_file(path)
        cls._fsync_directory(path.parent)

    @staticmethod
    def _lstat_exclusive_regular_file(path: Path) -> os.stat_result:
        path = Path(path)
        path_stat = os.lstat(path)
        if not stat.S_ISREG(path_stat.st_mode):
            raise RuntimeError("%s is not a regular file" % path.name)
        if path_stat.st_nlink != 1:
            raise RuntimeError("%s has unexpected hard links" % path.name)
        return path_stat

    @staticmethod
    def _validate_private_file_stat(
        path: Path,
        path_stat: os.stat_result,
    ) -> None:
        if (
            not stat.S_ISREG(path_stat.st_mode)
            or stat.S_IMODE(path_stat.st_mode) != 0o600
            or path_stat.st_uid != os.getuid()
            or path_stat.st_nlink != 1
        ):
            raise RuntimeError(
                "%s is not a private exclusive regular file"
                % Path(path).name
            )

    @classmethod
    def _lstat_private_regular_file(cls, path: Path) -> os.stat_result:
        path_stat = cls._lstat_exclusive_regular_file(path)
        cls._validate_private_file_stat(path, path_stat)
        return path_stat

    @staticmethod
    def _open_verified_regular_file(path: Path) -> int:
        path = Path(path)
        path_stat = ProductionDependencies._lstat_exclusive_regular_file(
            path
        )
        descriptor = os.open(
            path,
            os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0),
        )
        try:
            descriptor_stat = os.fstat(descriptor)
            if (
                not stat.S_ISREG(descriptor_stat.st_mode)
                or descriptor_stat.st_nlink != 1
                or descriptor_stat.st_dev != path_stat.st_dev
                or descriptor_stat.st_ino != path_stat.st_ino
            ):
                raise RuntimeError(
                    "%s changed during regular file validation" % path.name
                )
        except BaseException:
            os.close(descriptor)
            raise
        return descriptor

    @classmethod
    def _open_private_regular_file(cls, path: Path) -> int:
        path_stat = cls._lstat_private_regular_file(path)
        descriptor = os.open(
            path,
            os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0),
        )
        try:
            descriptor_stat = os.fstat(descriptor)
            cls._validate_private_file_stat(path, descriptor_stat)
            if (
                descriptor_stat.st_dev != path_stat.st_dev
                or descriptor_stat.st_ino != path_stat.st_ino
            ):
                raise RuntimeError(
                    "%s changed during private file validation"
                    % Path(path).name
                )
        except BaseException:
            os.close(descriptor)
            raise
        return descriptor

    @classmethod
    def _open_new_private_file(cls, path: Path) -> int:
        path = Path(path)
        descriptor = os.open(
            path,
            os.O_CREAT
            | os.O_EXCL
            | os.O_WRONLY
            | getattr(os, "O_NOFOLLOW", 0),
            0o600,
        )
        try:
            os.fchmod(descriptor, 0o600)
            cls._validate_private_file_stat(path, os.fstat(descriptor))
        except BaseException:
            os.close(descriptor)
            path.unlink(missing_ok=True)
            raise
        return descriptor

    @classmethod
    def _copy_open_file_to_new_path(
        cls,
        source_descriptor: int,
        destination: Path,
    ) -> None:
        destination = Path(destination)
        destination_descriptor = os.open(
            destination,
            os.O_CREAT
            | os.O_EXCL
            | os.O_WRONLY
            | getattr(os, "O_NOFOLLOW", 0),
            0o600,
        )
        try:
            while True:
                chunk = os.read(source_descriptor, HASH_CHUNK_SIZE)
                if not chunk:
                    break
                remaining = memoryview(chunk)
                while remaining:
                    written = os.write(
                        destination_descriptor,
                        remaining,
                    )
                    if written <= 0:
                        raise OSError("secure copy made no progress")
                    remaining = remaining[written:]
            os.fchmod(destination_descriptor, 0o600)
            destination_stat = os.fstat(destination_descriptor)
            cls._validate_private_file_stat(
                destination,
                destination_stat,
            )
            os.fsync(destination_descriptor)
        finally:
            os.close(destination_descriptor)
        cls._fsync_directory(destination.parent)

    @classmethod
    def _copy_verified_file(
        cls,
        source: Path,
        destination: Path,
    ) -> None:
        source_descriptor = cls._open_verified_regular_file(source)
        try:
            cls._copy_open_file_to_new_path(
                source_descriptor,
                destination,
            )
        finally:
            os.close(source_descriptor)

    @classmethod
    def _read_verified_text(cls, path: Path, encoding: str) -> str:
        descriptor = cls._open_verified_regular_file(path)
        with os.fdopen(descriptor, "r", encoding=encoding) as handle:
            return handle.read()

    @classmethod
    def _file_sha256(cls, path: Path) -> str:
        digest = hashlib.sha256()
        descriptor = cls._open_verified_regular_file(path)
        with os.fdopen(descriptor, "rb") as handle:
            while True:
                chunk = handle.read(HASH_CHUNK_SIZE)
                if not chunk:
                    return digest.hexdigest()
                digest.update(chunk)

    def validate_checksums(self, name: str) -> None:
        self._validate_checksums(
            name,
            require_seal=name == "SHA256SUMS",
        )

    def validate_unsealed_checksums(self, name: str) -> None:
        self._validate_checksums(name, require_seal=False)

    def _validate_checksums(
        self,
        name: str,
        *,
        require_seal: bool,
        output_dir: Path | None = None,
        expected_tombstone: Path | None = None,
        allow_active_lock: bool = False,
        publication_pending: bool = False,
    ) -> None:
        checked_output_dir = (
            self.output_dir if output_dir is None else Path(output_dir)
        )
        manifest_path = checked_output_dir / name
        self._lstat_private_regular_file(manifest_path)
        checksums: dict[str, str] = {}
        manifest_text = self._read_verified_text(
            manifest_path,
            encoding="ascii",
        )
        for line in manifest_text.splitlines():
            match = re.fullmatch(r"([0-9a-f]{64})  ([^/]+)", line)
            if match is None:
                raise RuntimeError("invalid checksum manifest")
            expected, filename = match.groups()
            if (
                filename in checksums
                or filename == name
                or filename == "SHA256SUMS"
            ):
                raise RuntimeError("invalid checksum manifest entry")
            checksums[filename] = expected
        if not checksums:
            raise RuntimeError("checksum manifest is empty")
        actual = {
            entry.name
            for entry in os.scandir(checked_output_dir)
            if entry.name not in {name, "SHA256SUMS"}
        }
        manifest_entries = set(checksums)
        missing = sorted(actual - manifest_entries)
        if missing:
            raise RuntimeError(
                "checksum manifest has missing entries: "
                + ", ".join(missing)
            )
        unexpected = sorted(manifest_entries - actual)
        if unexpected:
            raise RuntimeError(
                "checksum manifest has unexpected entries: "
                + ", ".join(unexpected)
            )
        for filename, expected in checksums.items():
            artifact = checked_output_dir / filename
            self._lstat_private_regular_file(artifact)
            if self._file_sha256(artifact) != expected:
                raise RuntimeError("artifact checksum mismatch")
        if require_seal and name != "SHA256SUMS":
            raise RuntimeError("sealed evidence requires final manifest")
        if name != "SHA256SUMS":
            return
        if "summary.json" not in checksums:
            if require_seal:
                raise RuntimeError("sealed evidence summary is missing")
            return
        try:
            summary = json.loads(
                self._read_verified_text(
                    checked_output_dir / "summary.json",
                    encoding="utf-8",
                )
            )
        except (OSError, UnicodeError, json.JSONDecodeError) as exc:
            raise RuntimeError("final summary is invalid") from exc
        if not isinstance(summary, dict):
            raise RuntimeError("final summary is invalid")
        if summary.get("sealed") is not True:
            if require_seal:
                raise RuntimeError("final summary is not sealed")
            return
        self._validate_final_summary(summary, checked_output_dir)
        required_artifacts = set(REQUIRED_ARTIFACTS)
        if manifest_entries != required_artifacts - {"SHA256SUMS"}:
            raise RuntimeError(
                "sealed evidence manifest artifact set is invalid"
            )
        if {
            entry.name for entry in os.scandir(checked_output_dir)
        } != required_artifacts:
            raise RuntimeError(
                "sealed evidence required artifact set is invalid"
            )
        self._validate_artifact_set_at(
            checked_output_dir,
            required_artifacts,
        )
        if not require_seal:
            self._validate_sealed_summary(
                summary,
                expected_tombstone=expected_tombstone,
                allow_active_lock=allow_active_lock,
                publication_pending=publication_pending,
            )
            return
        self._validate_seal_marker(
            summary,
            checksums,
            manifest_path,
            output_dir=checked_output_dir,
            expected_tombstone=expected_tombstone,
            allow_active_lock=allow_active_lock,
            publication_pending=publication_pending,
        )

    @staticmethod
    def _validate_final_summary(
        summary: dict[str, object],
        output_dir: Path,
    ) -> None:
        required_fields = {
            "model",
            "expected_revision",
            "output_dir",
            "write_outcome",
            "evidence_complete",
            "sealed",
            "required_artifacts",
            "final_lock_absent",
            "lock_state",
            "lock_tombstone_path",
            "run_id",
            "seal_id",
            "seal_marker_path",
        }
        if not required_fields.issubset(summary):
            raise RuntimeError("sealed evidence summary fields are incomplete")
        expected_revision = summary.get("expected_revision")
        output_text = summary.get("output_dir")
        required = summary.get("required_artifacts")
        if (
            summary.get("model") != MODEL
            or not isinstance(expected_revision, str)
            or re.fullmatch(r"[0-9a-f]{40}", expected_revision) is None
            or output_text != str(output_dir)
            or summary.get("write_outcome")
            not in {"committed", "not_committed"}
            or summary.get("evidence_complete") is not True
            or summary.get("sealed") is not True
            or not isinstance(required, list)
            or any(not isinstance(name, str) for name in required)
            or required != sorted(REQUIRED_ARTIFACTS)
        ):
            raise RuntimeError("sealed evidence summary fields are invalid")

    def _validate_sealed_summary(
        self,
        summary: dict[str, object],
        *,
        expected_tombstone: Path | None = None,
        allow_active_lock: bool = False,
        publication_pending: bool = False,
    ) -> Path:
        tombstone_text = summary.get("lock_tombstone_path")
        if (
            summary.get("final_lock_absent") is not True
            or summary.get("lock_state") != "absent"
            or not isinstance(tombstone_text, str)
            or not tombstone_text
        ):
            raise RuntimeError("sealed evidence lock topology is invalid")
        run_id = summary.get("run_id")
        seal_id = summary.get("seal_id")
        if (
            not isinstance(run_id, str)
            or re.fullmatch(r"[0-9a-f]{32}", run_id) is None
            or not isinstance(seal_id, str)
            or re.fullmatch(r"[0-9a-f]{32}", seal_id) is None
        ):
            raise RuntimeError("sealed evidence seal identity is invalid")
        tombstone_path = Path(tombstone_text)
        if (
            expected_tombstone is not None
            and tombstone_path != Path(expected_tombstone)
        ):
            raise RuntimeError("sealed evidence tombstone binding is invalid")
        marker_path = tombstone_path / SEAL_MARKER_NAME
        if summary.get("seal_marker_path") != str(marker_path):
            raise RuntimeError("sealed evidence seal marker path is invalid")
        active_lock_exists, tombstone_exists = self.lock_topology(
            tombstone_path
        )
        if publication_pending:
            valid_topology = active_lock_exists
            topology_path = self.pricing_lock_path
        else:
            valid_topology = (
                (allow_active_lock or not active_lock_exists)
                and tombstone_exists
            )
            topology_path = tombstone_path
        if not valid_topology:
            raise RuntimeError("sealed evidence lock topology is invalid")
        tombstone_mode = stat.S_IMODE(os.lstat(topology_path).st_mode)
        if tombstone_mode != 0o700:
            raise RuntimeError("sealed evidence tombstone mode is invalid")
        return tombstone_path

    def _validate_final_owner(
        self,
        owner_path: Path,
        summary: dict[str, object],
    ) -> str:
        self._lstat_private_regular_file(owner_path)
        try:
            owner = json.loads(
                self._read_verified_text(owner_path, encoding="utf-8")
            )
        except (OSError, UnicodeError, json.JSONDecodeError) as exc:
            raise RuntimeError("sealed evidence owner is invalid") from exc
        expected_keys = {
            "model",
            "expected_revision",
            "output_dir",
            "run_id",
            "state",
            "pid",
            "captured_pricing_version",
            "seal_id",
        }
        if not isinstance(owner, dict) or set(owner) != expected_keys:
            raise RuntimeError("sealed evidence owner is invalid")
        pid = owner.get("pid")
        captured_version = owner.get("captured_pricing_version")
        if (
            owner.get("model") != MODEL
            or owner.get("model") != summary.get("model")
            or owner.get("expected_revision")
            != summary.get("expected_revision")
            or owner.get("output_dir") != summary.get("output_dir")
            or owner.get("run_id") != summary.get("run_id")
            or owner.get("seal_id") != summary.get("seal_id")
            or owner.get("state") not in FINAL_OWNER_STATES
            or not isinstance(pid, int)
            or isinstance(pid, bool)
            or pid <= 0
            or not isinstance(captured_version, str)
            or re.fullmatch(r"[0-9a-f]{64}", captured_version) is None
        ):
            raise RuntimeError("sealed evidence owner is invalid")
        return self._file_sha256(owner_path)

    def _validate_seal_marker(
        self,
        summary: dict[str, object],
        checksums: dict[str, str],
        manifest_path: Path,
        *,
        output_dir: Path | None = None,
        expected_tombstone: Path | None = None,
        allow_active_lock: bool = False,
        publication_pending: bool = False,
    ) -> None:
        checked_output_dir = (
            self.output_dir if output_dir is None else Path(output_dir)
        )
        tombstone_path = self._validate_sealed_summary(
            summary,
            expected_tombstone=expected_tombstone,
            allow_active_lock=allow_active_lock,
            publication_pending=publication_pending,
        )
        marker_path = (
            self.pricing_lock_path
            if publication_pending
            else tombstone_path
        ) / SEAL_MARKER_NAME
        owner_path = (
            self.pricing_lock_path
            if publication_pending
            else tombstone_path
        ) / "owner"
        owner_sha256 = self._validate_final_owner(owner_path, summary)
        try:
            marker_stat = os.lstat(marker_path)
        except FileNotFoundError as exc:
            raise RuntimeError("durable seal marker is missing") from exc
        if (
            not stat.S_ISREG(marker_stat.st_mode)
            or stat.S_IMODE(marker_stat.st_mode) != 0o600
            or marker_stat.st_uid != os.getuid()
            or marker_stat.st_nlink != 1
        ):
            raise RuntimeError("durable seal marker is invalid")
        try:
            marker = json.loads(
                self._read_verified_text(
                    marker_path,
                    encoding="utf-8",
                )
            )
        except (OSError, UnicodeError, json.JSONDecodeError) as exc:
            raise RuntimeError("durable seal marker is invalid") from exc
        expected = {
            "protocol": SEAL_PROTOCOL,
            "model": summary["model"],
            "expected_revision": summary["expected_revision"],
            "run_id": summary["run_id"],
            "seal_id": summary["seal_id"],
            "state": "committed",
            "output_dir": str(checked_output_dir),
            "tombstone_path": str(tombstone_path),
            "summary_sha256": checksums["summary.json"],
            "manifest_sha256": self._file_sha256(manifest_path),
            "owner_sha256": owner_sha256,
        }
        if marker != expected:
            raise RuntimeError("durable seal marker binding is invalid")

    def _validate_released_tombstones(
        self,
        *,
        allow_active_lock: bool = False,
    ) -> None:
        tombstone_pattern = re.compile(
            re.escape(self.pricing_lock_path.name)
            + r"\.released\.[0-9]+\.[0-9a-f]{32}"
        )
        candidates = sorted(
            self.root / entry.name
            for entry in os.scandir(self.root)
            if tombstone_pattern.fullmatch(entry.name)
        )
        for tombstone_path in candidates:
            try:
                tombstone_stat = os.lstat(tombstone_path)
                if (
                    not stat.S_ISDIR(tombstone_stat.st_mode)
                    or stat.S_IMODE(tombstone_stat.st_mode) != 0o700
                ):
                    raise RuntimeError("not a private directory")
                entries = {
                    entry.name: entry
                    for entry in os.scandir(tombstone_path)
                }
                if SEAL_MARKER_NAME not in entries:
                    raise RuntimeError("unsealed tombstone")
                if set(entries) != {"owner", SEAL_MARKER_NAME}:
                    raise RuntimeError("unexpected tombstone entries")
                owner_stat = os.lstat(tombstone_path / "owner")
                if (
                    not stat.S_ISREG(owner_stat.st_mode)
                    or stat.S_IMODE(owner_stat.st_mode) != 0o600
                    or owner_stat.st_uid != os.getuid()
                    or owner_stat.st_nlink != 1
                ):
                    raise RuntimeError("invalid tombstone owner")
                marker_path = tombstone_path / SEAL_MARKER_NAME
                marker_stat = os.lstat(marker_path)
                if (
                    not stat.S_ISREG(marker_stat.st_mode)
                    or stat.S_IMODE(marker_stat.st_mode) != 0o600
                    or marker_stat.st_uid != os.getuid()
                    or marker_stat.st_nlink != 1
                ):
                    raise RuntimeError("invalid seal marker")
                marker = json.loads(
                    self._read_verified_text(
                        marker_path,
                        encoding="utf-8",
                    )
                )
                if not isinstance(marker, dict):
                    raise RuntimeError("invalid seal marker")
                output_text = marker.get("output_dir")
                if not isinstance(output_text, str) or not output_text:
                    raise RuntimeError("invalid seal output directory")
                history_output = Path(output_text)
                if (
                    not history_output.is_absolute()
                    or history_output.resolve(strict=True) != history_output
                    or history_output == self.backup_root
                ):
                    raise RuntimeError("invalid seal output directory")
                history_output.relative_to(self.backup_root)
                self._validate_checksums(
                    "SHA256SUMS",
                    require_seal=True,
                    output_dir=history_output,
                    expected_tombstone=tombstone_path,
                    allow_active_lock=allow_active_lock,
                )
            except (
                OSError,
                UnicodeError,
                ValueError,
                json.JSONDecodeError,
            ) as exc:
                raise RuntimeError(
                    "pricing lock tombstone is malformed: %s"
                    % tombstone_path
                ) from exc
            except RuntimeError as exc:
                raise RuntimeError(
                    "pricing lock tombstone is unsealed or malformed: %s: %s"
                    % (tombstone_path, exc)
                ) from exc

    def publish_seal(self, tombstone_path: Path) -> None:
        tombstone_path = self._validated_tombstone_path(tombstone_path)
        manifest_path = self.output_dir / "SHA256SUMS"
        self._validate_checksums(
            "SHA256SUMS",
            require_seal=False,
            expected_tombstone=tombstone_path,
            publication_pending=True,
        )
        summary = json.loads(
            self._read_verified_text(
                self.output_dir / "summary.json",
                encoding="utf-8",
            )
        )
        if not isinstance(summary, dict):
            raise RuntimeError("final summary is invalid")
        checked_tombstone = self._validate_sealed_summary(
            summary,
            expected_tombstone=tombstone_path,
            publication_pending=True,
        )
        if Path(tombstone_path) != checked_tombstone:
            raise RuntimeError("durable seal tombstone binding is invalid")
        owner_sha256 = self._validate_final_owner(
            self.pricing_lock_path / "owner",
            summary,
        )
        marker_path = self.pricing_lock_path / SEAL_MARKER_NAME
        marker_binding = {
            "protocol": SEAL_PROTOCOL,
            "model": summary["model"],
            "expected_revision": summary["expected_revision"],
            "run_id": summary["run_id"],
            "seal_id": summary["seal_id"],
            "output_dir": str(self.output_dir),
            "tombstone_path": str(checked_tombstone),
            "summary_sha256": self._file_sha256(
                self.output_dir / "summary.json"
            ),
            "manifest_sha256": self._file_sha256(manifest_path),
            "owner_sha256": owner_sha256,
            "state": "committed",
        }
        descriptor = os.open(
            marker_path,
            os.O_CREAT
            | os.O_EXCL
            | os.O_WRONLY
            | getattr(os, "O_NOFOLLOW", 0),
            0o600,
        )
        try:
            self._rewrite_open_file(
                descriptor,
                self._json_bytes(marker_binding),
            )
            os.fchmod(descriptor, 0o600)
            os.fsync(descriptor)
            self._verify_open_file_path(marker_path, descriptor)
            self._fsync_directory(self.pricing_lock_path)
        finally:
            os.close(descriptor)

    @staticmethod
    def _rewrite_open_file(descriptor: int, payload: bytes) -> None:
        os.lseek(descriptor, 0, os.SEEK_SET)
        remaining = memoryview(payload)
        while remaining:
            written = os.write(descriptor, remaining)
            if written <= 0:
                raise OSError("seal marker write made no progress")
            remaining = remaining[written:]
        os.ftruncate(descriptor, len(payload))

    @classmethod
    def _verify_open_file_path(cls, path: Path, descriptor: int) -> None:
        path_stat = cls._lstat_exclusive_regular_file(path)
        descriptor_stat = os.fstat(descriptor)
        if (
            not stat.S_ISREG(descriptor_stat.st_mode)
            or descriptor_stat.st_nlink != 1
            or descriptor_stat.st_dev != path_stat.st_dev
            or descriptor_stat.st_ino != path_stat.st_ino
        ):
            raise RuntimeError(
                "%s changed during seal publication" % Path(path).name
            )

    def validate_artifact_set(self, expected: set[str]) -> None:
        self._validate_artifact_set_at(self.output_dir, expected)

    @classmethod
    def _validate_artifact_set_at(
        cls,
        output_dir: Path,
        expected: set[str],
    ) -> None:
        actual = {entry.name for entry in os.scandir(output_dir)}
        missing = sorted(expected - actual)
        if missing:
            raise RuntimeError(
                "required artifact set has missing files: "
                + ", ".join(missing)
            )
        unexpected = sorted(actual - expected)
        if unexpected:
            raise RuntimeError(
                "required artifact set has unexpected files: "
                + ", ".join(unexpected)
            )
        file_stats = {
            name: os.lstat(output_dir / name)
            for name in actual
        }
        invalid = sorted(
            name
            for name, file_stat in file_stats.items()
            if not stat.S_ISREG(file_stat.st_mode)
        )
        if invalid:
            raise RuntimeError(
                "required artifact set has non-regular files: "
                + ", ".join(invalid)
            )
        hardlinked = sorted(
            name
            for name, file_stat in file_stats.items()
            if file_stat.st_nlink != 1
        )
        if hardlinked:
            raise RuntimeError(
                "required artifact set has unexpected hard links: "
                + ", ".join(hardlinked)
            )
        non_private = sorted(
            name
            for name, file_stat in file_stats.items()
            if (
                stat.S_IMODE(file_stat.st_mode) != 0o600
                or file_stat.st_uid != os.getuid()
            )
        )
        if non_private:
            raise RuntimeError(
                "required artifact set has non-private mode or owner: "
                + ", ".join(non_private)
            )

    def prepare_lock_commit(self) -> tuple[Path, str]:
        while True:
            seal_id = uuid.uuid4().hex
            tombstone_path = self.root / (
                "%s.released.%d.%s"
                % (
                    self.pricing_lock_path.name,
                    os.getpid(),
                    seal_id,
                )
            )
            if (
                tombstone_path not in self._prepared_commit_paths
                and not os.path.lexists(tombstone_path)
            ):
                self._prepared_commit_paths.add(tombstone_path)
                return tombstone_path, seal_id

    def release_lock(self, tombstone_path: Path) -> Path:
        tombstone_path = self._validated_tombstone_path(tombstone_path)
        self._validate_checksums(
            "SHA256SUMS",
            require_seal=True,
            expected_tombstone=tombstone_path,
            publication_pending=True,
        )
        active_stat = os.lstat(self.pricing_lock_path)
        if (
            not stat.S_ISDIR(active_stat.st_mode)
            or stat.S_IMODE(active_stat.st_mode) != 0o700
        ):
            raise RuntimeError("active pricing lock is not a private directory")
        active_entries = {
            entry.name for entry in os.scandir(self.pricing_lock_path)
        }
        if active_entries != {"owner", SEAL_MARKER_NAME}:
            raise RuntimeError("active pricing lock entries are invalid")
        self._lstat_private_regular_file(
            self.pricing_lock_path / "owner"
        )
        self._rename_noreplace(self.pricing_lock_path, tombstone_path)
        try:
            self._fsync_directory(self.root)
        except BaseException as durability_error:
            raise LockCommitDurabilityError(
                durability_error,
                tombstone_path,
            ) from durability_error
        return tombstone_path

    @staticmethod
    def _rename_noreplace(source: Path, destination: Path) -> None:
        source_bytes = os.fsencode(os.fspath(source))
        destination_bytes = os.fsencode(os.fspath(destination))
        libc = ctypes.CDLL(None, use_errno=True)
        if sys.platform.startswith("linux"):
            try:
                rename_noreplace = libc.renameat2
            except AttributeError as exc:
                raise RuntimeError(
                    "renameat2(RENAME_NOREPLACE) is unavailable"
                ) from exc
            rename_noreplace.argtypes = [
                ctypes.c_int,
                ctypes.c_char_p,
                ctypes.c_int,
                ctypes.c_char_p,
                ctypes.c_uint,
            ]
            rename_noreplace.restype = ctypes.c_int
            result = rename_noreplace(
                -100,
                source_bytes,
                -100,
                destination_bytes,
                1,
            )
        elif sys.platform == "darwin":
            try:
                rename_noreplace = libc.renamex_np
            except AttributeError as exc:
                raise RuntimeError(
                    "renamex_np(RENAME_EXCL) is unavailable"
                ) from exc
            rename_noreplace.argtypes = [
                ctypes.c_char_p,
                ctypes.c_char_p,
                ctypes.c_uint,
            ]
            rename_noreplace.restype = ctypes.c_int
            result = rename_noreplace(
                source_bytes,
                destination_bytes,
                0x00000004,
            )
        else:
            raise RuntimeError(
                "atomic no-clobber rename is unsupported on %s"
                % sys.platform
            )
        if result != 0:
            error_number = ctypes.get_errno()
            raise OSError(
                error_number,
                os.strerror(error_number),
                os.fspath(destination),
            )

    def lock_topology(self, tombstone_path: Path) -> tuple[bool, bool]:
        tombstone_path = self._validated_tombstone_path(tombstone_path)
        try:
            tombstone_stat = os.lstat(tombstone_path)
        except FileNotFoundError:
            tombstone_exists = False
        else:
            if not stat.S_ISDIR(tombstone_stat.st_mode):
                raise RuntimeError(
                    "pricing lock tombstone is not a directory"
                )
            tombstone_exists = True
        return (
            os.path.lexists(self.pricing_lock_path),
            tombstone_exists,
        )

    def _validated_tombstone_path(self, tombstone_path: Path) -> Path:
        tombstone_path = Path(tombstone_path)
        tombstone_prefix = self.pricing_lock_path.name + ".released."
        if (
            tombstone_path.parent != self.root
            or not tombstone_path.name.startswith(tombstone_prefix)
            or not re.fullmatch(
                r"[0-9]+\.[0-9a-f]{32}",
                tombstone_path.name[len(tombstone_prefix) :],
            )
        ):
            raise RuntimeError("pricing lock tombstone path is invalid")
        return tombstone_path

def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Apply audited Claude Opus 5.5 official pricing.",
    )
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--expected-revision", required=True)
    parser.add_argument(
        "--existing-token-user-id",
        required=True,
        type=int,
    )
    parser.add_argument(
        "--existing-token-id",
        required=True,
        type=int,
    )
    return parser.parse_args(argv)


def main(
    argv: list[str] | None = None,
    *,
    deps_factory: object = ProductionDependencies,
    stdout: object | None = None,
) -> int:
    args = parse_args(argv)
    output = stdout if stdout is not None else sys.stdout
    try:
        deps = deps_factory(args, stdout=output)
    except Exception:
        print("output_dir=%s" % args.output_dir, file=output)
        print("write_outcome=not_committed", file=output)
        print("canary_status=not_run channel=none", file=output)
        print("lock_tombstone_path=none", file=output)
        return 1
    return execute_operation(deps)


if __name__ == "__main__":
    raise SystemExit(main())
