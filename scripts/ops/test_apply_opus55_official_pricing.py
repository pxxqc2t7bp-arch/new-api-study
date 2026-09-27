from __future__ import annotations

import copy
import contextlib
import gzip
import hashlib
import importlib.util
import io
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock


SCRIPT = pathlib.Path(__file__).with_name("apply_opus55_official_pricing.py")
SPEC = importlib.util.spec_from_file_location("opus55_pricing", SCRIPT)
pricing = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(pricing)


def official_html() -> bytes:
    return b"""
    <html><body><table>
      <tr><th>Name</th><th>Input</th><th>Output</th>
          <th>5m writes</th><th>1h writes</th>
          <th>Hits and refreshes</th></tr>
      <tr><td><strong>Claude Opus 5.5</strong>
          For long-running agentic coding and knowledge work</td>
          <td>$4 / MTok</td><td>$20 / MTok</td>
          <td>$5 / MTok</td><td>$8 / MTok</td>
          <td>$0.20 / MTok</td></tr>
    </table></body></html>
    """


OPTION_KEYS = (
    "AudioCompletionRatio",
    "AudioRatio",
    "CacheRatio",
    "CompletionRatio",
    "CreateCacheRatio",
    "ImageRatio",
    "ModelPrice",
    "ModelRatio",
    "billing_setting.billing_expr",
    "billing_setting.billing_mode",
    "billing_setting.plugin_billing_expr",
)
EXPECTED_REVISION = "7" * 40
ROOT_SECRET = "root-secret-value"
TOKEN_SECRET = "dedicated-secret-value"


def option_maps(*, target: bool = False) -> dict[str, dict[str, object]]:
    maps = {key: {} for key in OPTION_KEYS}
    maps["ModelRatio"] = {"unrelated-model": 1.5}
    maps["CompletionRatio"] = {
        "another-model": 3,
        "unrelated-model": 2,
    }
    if target:
        maps["billing_setting.billing_mode"][pricing.MODEL] = "tiered_expr"
        maps["billing_setting.billing_expr"][pricing.MODEL] = (
            pricing.TARGET_EXPRESSION
        )
    return maps


def snapshot(
    *,
    configured: dict[str, object],
    version: str,
    target: bool,
) -> dict[str, object]:
    maps = option_maps(target=target)
    return {
        "empty_version": pricing.EMPTY_VERSION,
        "entries": [
            {
                "model_name": pricing.MODEL,
                "version": version,
                "configured": copy.deepcopy(configured),
            }
        ],
        "options": {
            key: json.dumps(value, separators=(",", ":"))
            for key, value in maps.items()
        },
    }


def api_response(
    data: object,
    *,
    status: int = 200,
    success: bool = True,
    message: str = "",
) -> tuple[int, dict[str, str], bytes]:
    return (
        status,
        {"Content-Type": "application/json"},
        json.dumps(
            {
                "success": success,
                "message": message,
                "data": data,
            }
        ).encode(),
    )


def valid_state(*, pricing_lock: bool) -> dict[str, object]:
    return {
        "hostname": "xingyugpu",
        "machine_id_sha256": (
            "f131f2873ea46b3cea3d9388bd44bf30751e97c0a1b35ea1e87b8b94ac44ac8c"
        ),
        "architecture": "x86_64",
        "runtime": {
            "container": "new-api",
            "status": "running",
            "health": "healthy",
            "restart_count": 0,
            "revision": EXPECTED_REVISION,
        },
        "locks": {
            "deployment": False,
            "pricing": pricing_lock,
        },
        "plan": {
            "target_channels": {"4": 3, "66": 3, "96": 3},
            "target_enabled_abilities": 0,
            "authority_states": {"active": 3, "disabled": 1},
            "temporary_probe_token_count": 0,
        },
        "route48": {
            "channel_id": 48,
            "channel_status": 1,
            "state": "active",
            "consecutive_failures": 0,
            "failure_window_start": 0,
            "last_failure_at": 0,
        },
    }


class FakeDependencies:
    def __init__(self) -> None:
        self.output_dir = pathlib.Path(
            "/data1/newapi-study/backups/opus55-test"
        )
        self.expected_revision = EXPECTED_REVISION
        self.existing_token_user_id = 20
        self.existing_token_id = 139
        self.stdout = io.StringIO()
        self.events: list[str] = []
        self.states = [
            valid_state(pricing_lock=False),
            valid_state(pricing_lock=True),
        ]
        self.snapshots = [
            snapshot(
                configured={},
                version=pricing.EMPTY_VERSION,
                target=False,
            ),
            snapshot(
                configured=pricing.TARGET_PRICING,
                version="b" * 64,
                target=True,
            ),
        ]
        self.patch_result = api_response(
            {"updated_models": [pricing.MODEL]}
        )
        self.patch_error: BaseException | None = None
        self.readback_error: BaseException | None = None
        self.snapshot_request_count = 0
        self.fetch_error: Exception | None = None
        self.preview_result = api_response(
            {
                "effective": dict(pricing.TARGET_PRICING),
                "cache_write_mode": "claude_ttl",
                "billing_details": {},
            }
        )
        self.canary_result = (
            200,
            {"X-Oneapi-Request-Id": "request-123"},
            json.dumps(
                {
                    "choices": [
                        {
                            "message": {
                                "role": "assistant",
                                "content": " \nOK\t ",
                            }
                        }
                    ]
                }
            ).encode(),
        )
        self.canary_error: BaseException | None = None
        self.log_channel = pricing.CHANNEL_ID
        self.log_error: Exception | None = None
        self.lock_held = False
        self.tombstone_exists = False
        self.seal_valid = False
        self.seal_marker_exists = False
        self.seal_publish_error: BaseException | None = None
        self.deployment_lock_results = [False, False]
        self.lock_owners: list[dict[str, object]] = []
        self.json_artifacts: dict[str, object] = {}
        self.json_artifact_history: dict[str, list[object]] = {}
        self.json_payload_attempts: dict[str, list[object]] = {}
        self.text_artifacts: dict[str, str] = {}
        self.expected_artifact_sets: list[set[str]] = []
        self.request_bodies: dict[str, list[dict[str, object]]] = {}
        self.canary_authorization = ""
        self.token_row_error: BaseException | None = None
        self.fail_artifact = ""
        self.fail_artifact_error: BaseException | None = None
        self.fail_artifact_on_attempt: int | None = None
        self.artifact_write_attempts: dict[str, int] = {}
        self.fail_checksum_validation = False
        self.fail_checksum_validation_on_attempt: int | None = None
        self.checksum_validation_attempts = 0
        self.release_error: Exception | None = None
        self.lock_topology_error: BaseException | None = None
        self.lock_tombstone_path = pathlib.Path(
            "/opt/newapi-study/.pricing-change.lock.released.test"
        )
        self.seal_id = "e" * 32
        self.monotonic_effects: list[float | BaseException] = [10.0, 10.125]

    def capture_state(self, stage: str) -> dict[str, object]:
        self.events.append("state-" + stage)
        return copy.deepcopy(self.states.pop(0))

    def acquire_lock(self, owner: dict[str, object]) -> None:
        self.events.append("acquire-lock")
        self.assert_secret_free(owner)
        self.lock_held = True
        self.lock_owners.append(copy.deepcopy(owner))

    def deployment_lock_exists(self) -> bool:
        self.events.append("deployment-lock-check")
        if not self.deployment_lock_results:
            raise AssertionError("unexpected deployment lock check")
        return self.deployment_lock_results.pop(0)

    def update_lock_owner(self, owner: dict[str, object]) -> None:
        self.events.append("update-lock-owner")
        self.assert_secret_free(owner)
        self.lock_owners.append(copy.deepcopy(owner))

    def create_output_dir(self) -> None:
        self.events.append("create-output")

    def copy_compose(self) -> None:
        self.events.append("copy-compose")

    def copy_extension(self) -> None:
        self.events.append("copy-extension")

    def dump_database(self) -> None:
        self.events.extend(("pg-dump", "pg-restore-list"))

    def fetch(self, url: str, timeout: int) -> bytes:
        self.events.append("fetch:%s:%d" % (url, timeout))
        if self.fetch_error is not None:
            raise self.fetch_error
        return official_html()

    def root_headers(self) -> dict[str, str]:
        self.events.append("root-headers")
        return {
            "Authorization": "Bearer " + ROOT_SECRET,
            "New-Api-User": "1",
            "Content-Type": "application/json",
        }

    def request(
        self,
        method: str,
        path: str,
        headers: dict[str, str],
        body: dict[str, object] | None = None,
        timeout: int = 120,
    ) -> tuple[int, dict[str, str], bytes]:
        del timeout
        if method == "GET" and path.startswith(
            "/api/option/model_pricing?"
        ):
            self.events.append("snapshot")
            self.snapshot_request_count += 1
            if (
                self.snapshot_request_count == 2
                and self.readback_error is not None
            ):
                raise self.readback_error
            return api_response(self.snapshots.pop(0))
        if method == "POST" and path == "/api/option/model_pricing/preview":
            self.events.append("preview")
            self.request_bodies.setdefault(path, []).append(
                copy.deepcopy(body or {})
            )
            return self.preview_result
        if method == "PATCH" and path == "/api/option/model_pricing":
            self.events.append("patch")
            self.request_bodies.setdefault(path, []).append(
                copy.deepcopy(body or {})
            )
            if self.patch_error is not None:
                raise self.patch_error
            return self.patch_result
        if method == "POST" and path == "/v1/chat/completions":
            self.events.append("canary")
            self.request_bodies.setdefault(path, []).append(
                copy.deepcopy(body or {})
            )
            self.canary_authorization = headers.get("Authorization", "")
            if self.canary_error is not None:
                raise self.canary_error
            return self.canary_result
        raise AssertionError("unexpected request: %s %s" % (method, path))

    def existing_token_row(self) -> str:
        self.events.append("token-row")
        if self.token_row_error is not None:
            raise self.token_row_error
        return (
            "139|20|1|true|ark-probe-dedicated|%s|10|1"
            % TOKEN_SECRET
        )

    def request_log_once(self, request_id: str) -> int:
        self.events.append("request-log:" + request_id)
        if self.log_error is not None:
            raise self.log_error
        return self.log_channel

    def write_json(self, name: str, payload: object) -> None:
        self.assert_secret_free(payload)
        self.events.append("write-json:" + name)
        self.json_payload_attempts.setdefault(name, []).append(
            copy.deepcopy(payload)
        )
        attempt = self.artifact_write_attempts.get(name, 0) + 1
        self.artifact_write_attempts[name] = attempt
        if (
            name == self.fail_artifact
            and (
                self.fail_artifact_on_attempt is None
                or attempt == self.fail_artifact_on_attempt
            )
        ):
            if self.fail_artifact_error is not None:
                raise self.fail_artifact_error
            raise OSError("artifact write failed: " + name)
        if name == "summary.json":
            self.seal_valid = False
        self.json_artifacts[name] = copy.deepcopy(payload)
        self.json_artifact_history.setdefault(name, []).append(
            copy.deepcopy(payload)
        )

    def write_text(self, name: str, payload: str) -> None:
        self.assert_secret_free(payload)
        self.events.append("write-text:" + name)
        attempt = self.artifact_write_attempts.get(name, 0) + 1
        self.artifact_write_attempts[name] = attempt
        if (
            name == self.fail_artifact
            and (
                self.fail_artifact_on_attempt is None
                or attempt == self.fail_artifact_on_attempt
            )
        ):
            if self.fail_artifact_error is not None:
                raise self.fail_artifact_error
            raise OSError("artifact write failed: " + name)
        self.text_artifacts[name] = payload

    def write_checksums(self, name: str) -> None:
        self.events.append("checksums:" + name)
        if name == "SHA256SUMS":
            self.seal_valid = True

    def invalidate_evidence(self) -> None:
        self.events.append("invalidate-evidence")
        self.seal_valid = False

    def validate_checksums(self, name: str) -> None:
        self.events.append("validate-checksums:" + name)
        if name == "SHA256SUMS":
            self.checksum_validation_attempts += 1
            if (
                self.fail_checksum_validation
                or self.fail_checksum_validation_on_attempt
                == self.checksum_validation_attempts
            ):
                raise RuntimeError("checksum validation failed")

    def validate_unsealed_checksums(self, name: str) -> None:
        self.validate_checksums(name)

    def validate_artifact_set(self, expected: set[str]) -> None:
        self.events.append("validate-artifact-set")
        self.expected_artifact_sets.append(set(expected))

    def finalize_evidence(
        self,
        summary: dict[str, object],
        required_artifacts: set[str],
    ) -> None:
        self.assert_secret_free(summary)
        self.events.append("stage-final-evidence")
        self.validate_artifact_set(required_artifacts)
        self.write_json("summary.json", summary)
        self.events.append("checksums:SHA256SUMS")
        self.validate_checksums("SHA256SUMS")
        self.seal_valid = False

    def publish_seal(self, tombstone_path: pathlib.Path) -> None:
        self.events.append("publish-seal")
        if tombstone_path != self.lock_tombstone_path:
            raise AssertionError("unexpected lock tombstone")
        if not self.lock_held or self.tombstone_exists:
            raise AssertionError(
                "seal publication requires only the active lock"
            )
        if self.seal_publish_error is not None:
            raise self.seal_publish_error
        summary = self.json_artifacts["summary.json"]
        if not isinstance(summary, dict):
            raise AssertionError("summary must be an object")
        if not isinstance(summary.get("run_id"), str):
            raise AssertionError("run_id is missing")
        if not isinstance(summary.get("seal_id"), str):
            raise AssertionError("seal_id is missing")
        if summary.get("seal_marker_path") != str(
            tombstone_path / "seal.json"
        ):
            raise AssertionError("seal marker path mismatch")
        self.seal_marker_exists = True
        self.seal_valid = True

    def seal_publication_state(
        self,
        tombstone_path: pathlib.Path,
    ) -> str:
        self.events.append("seal-publication-state")
        if tombstone_path != self.lock_tombstone_path:
            raise AssertionError("unexpected lock tombstone")
        if self.seal_marker_exists and self.seal_valid:
            return "committed"
        if self.seal_marker_exists:
            return "present_unverified"
        return "absent"

    def prepare_lock_commit(self) -> tuple[pathlib.Path, str]:
        self.events.append("prepare-lock-commit")
        if not self.lock_held or self.tombstone_exists:
            raise AssertionError("commit identity requires only the active lock")
        return self.lock_tombstone_path, self.seal_id

    def release_lock(
        self,
        expected_tombstone: pathlib.Path,
    ) -> pathlib.Path:
        self.events.append("release-lock")
        if expected_tombstone != self.lock_tombstone_path:
            raise AssertionError("unexpected lock tombstone")
        if (
            not self.lock_held
            or self.tombstone_exists
            or not self.seal_marker_exists
            or not self.seal_valid
        ):
            raise AssertionError("commit rename requires sealed active lock")
        if self.release_error is not None:
            raise self.release_error
        self.lock_held = False
        self.tombstone_exists = True
        return self.lock_tombstone_path

    def lock_topology(
        self,
        tombstone_path: pathlib.Path,
    ) -> tuple[bool, bool]:
        self.events.append("lock-topology")
        if tombstone_path != self.lock_tombstone_path:
            raise AssertionError("unexpected lock tombstone")
        if self.lock_topology_error is not None:
            error = self.lock_topology_error
            self.lock_topology_error = None
            raise error
        return self.lock_held, self.tombstone_exists

    def monotonic(self) -> float:
        self.events.append("monotonic")
        effect = self.monotonic_effects.pop(0)
        if isinstance(effect, BaseException):
            raise effect
        return effect

    @staticmethod
    def assert_secret_free(value: object) -> None:
        serialized = json.dumps(value, sort_keys=True)
        if ROOT_SECRET in serialized or TOKEN_SECRET in serialized:
            raise AssertionError("secret leaked into an artifact")


class Opus55PricingTest(unittest.TestCase):
    def test_exact_constants_and_expression(self) -> None:
        self.assertEqual("claude-opus-5-5", pricing.MODEL)
        self.assertEqual(48, pricing.CHANNEL_ID)
        self.assertEqual(
            "https://platform.claude.com/docs/en/about-claude/pricing",
            pricing.SOURCE_URL,
        )
        self.assertEqual(
            "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
            pricing.EMPTY_VERSION,
        )
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
        self.assertEqual(
            {
                "billing_setting.billing_mode": "tiered_expr",
                "billing_setting.billing_expr": pricing.TARGET_EXPRESSION,
            },
            pricing.TARGET_PRICING,
        )

    def test_official_page_returns_exact_structured_evidence(self) -> None:
        body = official_html()
        parser = pricing.OfficialTableParser()
        parser.feed(body.decode("utf-8"))
        self.assertIn(
            [
                (
                    "Claude Opus 5.5 For long-running agentic coding "
                    "and knowledge work"
                ),
                "$4 / MTok",
                "$20 / MTok",
                "$5 / MTok",
                "$8 / MTok",
                "$0.20 / MTok",
            ],
            parser.rows,
        )

        evidence = pricing.parse_official_page(body)

        self.assertEqual(
            {
                "document_sha256",
                "evidence_sha256",
                "model",
                "price",
                "quote_fixture",
                "source_url",
            },
            set(evidence),
        )
        self.assertEqual(pricing.MODEL, evidence["model"])
        self.assertEqual(pricing.OFFICIAL_PRICE, evidence["price"])
        self.assertEqual(pricing.SOURCE_URL, evidence["source_url"])
        self.assertEqual(
            hashlib.sha256(body).hexdigest(),
            evidence["document_sha256"],
        )
        normalized = json.dumps(
            {
                "document_sha256": evidence["document_sha256"],
                "model": pricing.MODEL,
                "price": pricing.OFFICIAL_PRICE,
                "quote_fixture": pricing.official_quote_fixture(),
                "source_url": pricing.SOURCE_URL,
            },
            sort_keys=True,
            separators=(",", ":"),
        ).encode()
        self.assertEqual(
            hashlib.sha256(normalized).hexdigest(),
            evidence["evidence_sha256"],
        )

    def test_official_quote_evaluator_prices_one_million_each_category(
        self,
    ) -> None:
        expected = {
            "input": 4.0,
            "output": 20.0,
            "cache_read": 0.2,
            "cache_write_5m": 5.0,
            "cache_write_1h": 8.0,
        }
        token_key_by_category = {
            "input": "input_tokens",
            "output": "output_tokens",
            "cache_read": "cache_read_tokens",
            "cache_write_5m": "cache_write_5m_tokens",
            "cache_write_1h": "cache_write_1h_tokens",
        }
        empty_usage = {
            token_key: 0
            for token_key in token_key_by_category.values()
        }

        for category, token_key in token_key_by_category.items():
            usage = dict(empty_usage)
            usage[token_key] = 1_000_000
            with self.subTest(category=category):
                quote = pricing.evaluate_official_quote(usage)
                self.assertEqual(expected[category], quote[category])
                self.assertEqual(
                    expected[category],
                    sum(quote.values()),
                )

        self.assertEqual(
            {
                "token_counts": {
                    token_key: 1_000_000
                    for token_key in token_key_by_category.values()
                },
                "category_cost_usd": expected,
            },
            pricing.official_quote_fixture(),
        )

    def test_official_page_accepts_adjacent_nested_model_description(
        self,
    ) -> None:
        body = (
            b"<table><tr>"
            b"<td><a>Claude Opus 5.5</a><span>For long-running agentic "
            b"coding and knowledge work</span></td>"
            b"<td>$4 / MTok</td><td>$20 / MTok</td>"
            b"<td>$5 / MTok</td><td>$8 / MTok</td>"
            b"<td>$0.20 / MTok</td>"
            b"</tr></table>"
        )

        evidence = pricing.parse_official_page(body)

        self.assertEqual(pricing.MODEL, evidence["model"])
        self.assertEqual(pricing.OFFICIAL_PRICE, evidence["price"])

    def test_wrong_opus_row_is_not_rescued_by_distractor(self) -> None:
        body = b"""
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
            pricing.parse_official_page(body)

    def test_near_prefix_model_row_is_rejected(self) -> None:
        body = b"""
        <table>
          <tr><td>Claude Opus 5.50</td><td>$4 / MTok</td>
              <td>$20 / MTok</td><td>$5 / MTok</td>
              <td>$8 / MTok</td><td>$0.20 / MTok</td></tr>
        </table>
        """

        with self.assertRaisesRegex(RuntimeError, "official price row missing"):
            pricing.parse_official_page(body)

    def test_arbitrary_model_suffix_is_rejected(self) -> None:
        body = b"""
        <table>
          <tr><td>Claude Opus 5.5 Distractor</td><td>$4 / MTok</td>
              <td>$20 / MTok</td><td>$5 / MTok</td>
              <td>$8 / MTok</td><td>$0.20 / MTok</td></tr>
        </table>
        """

        with self.assertRaisesRegex(RuntimeError, "official price row missing"):
            pricing.parse_official_page(body)

    def test_matching_row_outside_table_is_rejected(self) -> None:
        body = b"""
        <html><body>
          <tr><td>Claude Opus 5.5</td><td>$4 / MTok</td>
              <td>$20 / MTok</td><td>$5 / MTok</td>
              <td>$8 / MTok</td><td>$0.20 / MTok</td></tr>
        </body></html>
        """
        parser = pricing.OfficialTableParser()
        parser.feed(body.decode("utf-8"))

        self.assertEqual([], parser.rows)
        with self.assertRaisesRegex(RuntimeError, "official price row missing"):
            pricing.parse_official_page(body)

    def test_patch_changes_only_target_model_and_two_fields(self) -> None:
        self.assertEqual(
            {
                "changes": [
                    {
                        "model_name": pricing.MODEL,
                        "expected_version": pricing.EMPTY_VERSION,
                        "pricing": pricing.TARGET_PRICING,
                    }
                ]
            },
            pricing.patch_payload(pricing.EMPTY_VERSION),
        )

    def test_write_outcome_committed_for_exact_target(self) -> None:
        before = {"version": "a" * 64, "configured": {}}
        after = {
            "version": "b" * 64,
            "configured": dict(pricing.TARGET_PRICING),
        }

        self.assertEqual(
            "committed",
            pricing.classify_write_outcome(before, after),
        )

    def test_write_outcome_unknown_for_invalid_target_versions(self) -> None:
        valid_before = "a" * 64
        valid_after = "b" * 64
        cases = [
            (
                "missing before",
                {"configured": {}},
                {
                    "version": valid_after,
                    "configured": dict(pricing.TARGET_PRICING),
                },
            ),
            (
                "missing after",
                {"version": valid_before, "configured": {}},
                {"configured": dict(pricing.TARGET_PRICING)},
            ),
            (
                "unchanged valid",
                {"version": valid_before, "configured": {}},
                {
                    "version": valid_before,
                    "configured": dict(pricing.TARGET_PRICING),
                },
            ),
        ]
        malformed_versions = (
            ("whitespace", " \t\n"),
            ("uppercase", "A" * 64),
            ("wrong length", "a" * 63),
            ("non-hex", "g" * 64),
        )
        for description, malformed_version in malformed_versions:
            cases.extend(
                (
                    (
                        f"{description} before",
                        {"version": malformed_version, "configured": {}},
                        {
                            "version": valid_after,
                            "configured": dict(pricing.TARGET_PRICING),
                        },
                    ),
                    (
                        f"{description} after",
                        {"version": valid_before, "configured": {}},
                        {
                            "version": malformed_version,
                            "configured": dict(pricing.TARGET_PRICING),
                        },
                    ),
                )
            )

        for description, before, after in cases:
            with self.subTest(description=description):
                self.assertEqual(
                    "unknown",
                    pricing.classify_write_outcome(before, after),
                )

    def test_write_outcome_unknown_for_non_dict_target_pre_state(
        self,
    ) -> None:
        after = {
            "version": "b" * 64,
            "configured": dict(pricing.TARGET_PRICING),
        }
        cases = (
            {"version": "a" * 64},
            {"version": "a" * 64, "configured": None},
            {"version": "a" * 64, "configured": []},
        )

        for before in cases:
            with self.subTest(before=before):
                self.assertEqual(
                    "unknown",
                    pricing.classify_write_outcome(before, after),
                )

    def test_write_outcome_not_committed_for_exact_pre_state(self) -> None:
        before = {"version": pricing.EMPTY_VERSION, "configured": {}}

        self.assertEqual(
            "not_committed",
            pricing.classify_write_outcome(before, dict(before)),
        )

    def test_write_outcome_unknown_for_malformed_equal_versions(self) -> None:
        malformed_versions = (
            "same",
            " \t\n",
            pricing.EMPTY_VERSION.upper(),
            "a" * 63,
            "a" * 65,
            "g" * 64,
        )

        for version in malformed_versions:
            state = {"version": version, "configured": {}}
            with self.subTest(version=version):
                self.assertEqual(
                    "unknown",
                    pricing.classify_write_outcome(state, dict(state)),
                )

    def test_write_outcome_unknown_for_valid_unequal_versions(self) -> None:
        configured = {"existing": "value"}
        before = {"version": "a" * 64, "configured": configured}
        after = {"version": "b" * 64, "configured": dict(configured)}

        self.assertEqual(
            "unknown",
            pricing.classify_write_outcome(before, after),
        )

    def test_write_outcome_unknown_for_differing_config_at_same_version(
        self,
    ) -> None:
        version = "a" * 64
        before = {
            "version": version,
            "configured": {"existing": "before"},
        }
        after = {
            "version": version,
            "configured": {"existing": "after"},
        }

        self.assertEqual(
            "unknown",
            pricing.classify_write_outcome(before, after),
        )

    def test_write_outcome_unknown_for_malformed_state(self) -> None:
        cases = (
            ({}, {}),
            (
                {"version": "", "configured": {}},
                {"version": "", "configured": {}},
            ),
            (
                {"version": 1, "configured": {}},
                {"version": 1, "configured": {}},
            ),
            ({"version": "same"}, {"version": "same"}),
            (
                {"version": "same", "configured": None},
                {"version": "same", "configured": None},
            ),
            (
                {"version": "same", "configured": []},
                {"version": "same", "configured": []},
            ),
        )

        for before, after in cases:
            with self.subTest(before=before, after=after):
                self.assertEqual(
                    "unknown",
                    pricing.classify_write_outcome(before, after),
                )

    def test_write_outcome_unknown_for_every_other_state(self) -> None:
        before = {"version": pricing.EMPTY_VERSION, "configured": {}}
        cases = (
            {
                "version": "new-version",
                "configured": {
                    **pricing.TARGET_PRICING,
                    "ModelRatio": 9,
                },
            },
            {"version": "other-version", "configured": {}},
        )

        for after in cases:
            with self.subTest(after=after):
                self.assertEqual(
                    "unknown",
                    pricing.classify_write_outcome(before, after),
                )


class ExecuteOperationTest(unittest.TestCase):
    def test_preflight_mismatches_block_patch(self) -> None:
        cases = {
            "hostname": lambda state: state.__setitem__(
                "hostname", "wrong-host"
            ),
            "machine": lambda state: state.__setitem__(
                "machine_id_sha256", "0" * 64
            ),
            "architecture": lambda state: state.__setitem__(
                "architecture", "arm64"
            ),
            "runtime": lambda state: state["runtime"].__setitem__(
                "status", "exited"
            ),
            "health": lambda state: state["runtime"].__setitem__(
                "health", "unhealthy"
            ),
            "restart": lambda state: state["runtime"].__setitem__(
                "restart_count", 1
            ),
            "revision": lambda state: state["runtime"].__setitem__(
                "revision", "8" * 40
            ),
            "deployment lock": lambda state: state["locks"].__setitem__(
                "deployment", True
            ),
            "pricing lock": lambda state: state["locks"].__setitem__(
                "pricing", True
            ),
            "channel state": lambda state: state["plan"][
                "target_channels"
            ].__setitem__("4", 1),
            "ability state": lambda state: state["plan"].__setitem__(
                "target_enabled_abilities", 1
            ),
            "authority state": lambda state: state["plan"].__setitem__(
                "authority_states", {"active": 2, "disabled": 2}
            ),
            "temporary probe token": lambda state: state["plan"].__setitem__(
                "temporary_probe_token_count", 1
            ),
            "channel 48 status": lambda state: state["route48"].__setitem__(
                "channel_status", 3
            ),
            "channel 48 route state": lambda state: state[
                "route48"
            ].__setitem__("state", "paused"),
            "channel 48 failure count": lambda state: state[
                "route48"
            ].__setitem__("consecutive_failures", -1),
            "channel 48 failure window": lambda state: state[
                "route48"
            ].__setitem__("failure_window_start", -1),
        }
        for description, mutate in cases.items():
            deps = FakeDependencies()
            mutate(deps.states[0])

            with self.subTest(description=description):
                self.assertEqual(1, pricing.execute_operation(deps))
                self.assertNotIn("patch", deps.events)
                self.assertNotIn("canary", deps.events)
                self.assertFalse(deps.lock_held)

    def test_backups_fetch_preview_and_pre_manifest_precede_one_patch(
        self,
    ) -> None:
        deps = FakeDependencies()

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(0, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        patch_index = deps.events.index("patch")
        required_before_patch = (
            "copy-compose",
            "copy-extension",
            "pg-dump",
            "pg-restore-list",
            "fetch:%s:30" % pricing.SOURCE_URL,
            "snapshot",
            "preview",
            "checksums:SHA256SUMS.pre",
        )
        for event in required_before_patch:
            self.assertLess(deps.events.index(event), patch_index)
        self.assertEqual(
            [pricing.patch_payload(pricing.EMPTY_VERSION)],
            deps.request_bodies["/api/option/model_pricing"],
        )

    def test_deployment_marker_after_pricing_lock_retains_lock(self) -> None:
        deps = FakeDependencies()
        deps.deployment_lock_results = [True]

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(
            ["state-before", "acquire-lock", "deployment-lock-check"],
            deps.events[:3],
        )
        self.assertNotIn("patch", deps.events)
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)

    def test_deployment_marker_before_patch_retains_lock_without_evidence(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.deployment_lock_results = [False, True]
        deps.fail_checksum_validation = True

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(2, deps.events.count("deployment-lock-check"))
        self.assertNotIn("patch", deps.events)
        self.assertNotIn("canary", deps.events)
        self.assertEqual(
            [
                "write-json:pricing-write-error.json",
                "write-json:pricing-conflict.json",
                "deployment-lock-check",
            ],
            deps.events[
                deps.events.index("validate-checksums:SHA256SUMS.pre") + 1 :
                deps.events.index("validate-checksums:SHA256SUMS.pre") + 4
            ],
        )
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)

    def test_second_deployment_marker_check_is_adjacent_to_patch(
        self,
    ) -> None:
        deps = FakeDependencies()

        self.assertEqual(0, pricing.execute_operation(deps))

        self.assertEqual(2, deps.events.count("deployment-lock-check"))
        patch_index = deps.events.index("patch")
        self.assertEqual(
            "deployment-lock-check",
            deps.events[patch_index - 1],
        )

    def test_official_fetch_failure_retains_lock_without_evidence(self) -> None:
        deps = FakeDependencies()
        deps.fetch_error = RuntimeError("official fetch failed")
        deps.fail_checksum_validation = True

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(0, deps.events.count("patch"))
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)

    def test_preview_failure_retains_lock_without_evidence(self) -> None:
        deps = FakeDependencies()
        deps.preview_result = api_response(
            None,
            status=503,
            success=False,
            message="preview unavailable",
        )
        deps.fail_checksum_validation = True

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(0, deps.events.count("patch"))
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)

    def test_lock_owner_is_pending_then_records_captured_version(
        self,
    ) -> None:
        deps = FakeDependencies()

        self.assertEqual(0, pricing.execute_operation(deps))

        run_id = deps.lock_owners[0]["run_id"]
        self.assertRegex(run_id, r"^[0-9a-f]{32}$")
        self.assertEqual(
            [
                {
                    "model": pricing.MODEL,
                    "expected_revision": EXPECTED_REVISION,
                    "output_dir": str(deps.output_dir),
                    "run_id": run_id,
                    "state": "pending_snapshot",
                },
                {
                    "model": pricing.MODEL,
                    "expected_revision": EXPECTED_REVISION,
                    "output_dir": str(deps.output_dir),
                    "run_id": run_id,
                    "state": "snapshot_captured",
                    "captured_pricing_version": pricing.EMPTY_VERSION,
                },
                {
                    "model": pricing.MODEL,
                    "expected_revision": EXPECTED_REVISION,
                    "output_dir": str(deps.output_dir),
                    "run_id": run_id,
                    "state": "pending_release",
                    "captured_pricing_version": pricing.EMPTY_VERSION,
                    "seal_id": deps.seal_id,
                },
            ],
            deps.lock_owners,
        )
        self.assertLess(
            deps.events.index("acquire-lock"),
            deps.events.index("snapshot"),
        )
        self.assertLess(
            deps.events.index("snapshot"),
            deps.events.index("update-lock-owner"),
        )
        self.assertLess(
            deps.events.index("update-lock-owner"),
            deps.events.index("patch"),
        )
        final_owner_index = max(
            index
            for index, event in enumerate(deps.events)
            if event == "update-lock-owner"
        )
        self.assertLess(
            final_owner_index,
            deps.events.index("stage-final-evidence"),
        )

    def test_acquire_cleanup_failure_marks_retained_active_lock(self) -> None:
        deps = FakeDependencies()
        acquire_error = OSError("owner write failed")
        cleanup_error = OSError("owner unlink failed")
        combined_error = RuntimeError("lock acquire cleanup failed")
        combined_error.acquire_error = acquire_error
        combined_error.cleanup_errors = (("unlink_owner", cleanup_error),)
        combined_error.active_lock_exists = True
        combined_error.owner_exists = True

        def acquire_and_retain(owner: dict[str, object]) -> None:
            deps.events.append("acquire-lock")
            deps.assert_secret_free(owner)
            deps.lock_held = True
            raise combined_error

        deps.acquire_lock = acquire_and_retain

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertTrue(deps.lock_held)
        self.assertNotIn("release-lock", deps.events)
        self.assertIn(
            "lock_state=retained_acquire_failure",
            deps.stdout.getvalue().splitlines(),
        )
        self.assertNotIn(
            "lock_state=not_acquired",
            deps.stdout.getvalue().splitlines(),
        )

    def test_acquire_cleanup_failure_rethrows_original_signal(self) -> None:
        deps = FakeDependencies()
        signal = KeyboardInterrupt("owner write interrupted")
        combined_error = RuntimeError("lock acquire cleanup failed")
        combined_error.acquire_error = signal
        combined_error.cleanup_errors = (
            ("fsync_root", OSError("cleanup parent fsync failed")),
        )
        combined_error.active_lock_exists = True
        combined_error.owner_exists = False

        def acquire_and_retain(owner: dict[str, object]) -> None:
            deps.events.append("acquire-lock")
            deps.assert_secret_free(owner)
            deps.lock_held = True
            raise combined_error

        deps.acquire_lock = acquire_and_retain

        with self.assertRaises(KeyboardInterrupt) as raised:
            pricing.execute_operation(deps)

        self.assertIs(signal, raised.exception)
        self.assertTrue(deps.lock_held)
        self.assertIn(
            "lock_state=retained_acquire_failure",
            deps.stdout.getvalue().splitlines(),
        )

    def test_transport_exception_reads_once_and_never_resends_patch(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_error = ConnectionError("connection reset")

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(0, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertEqual(1, deps.events.count("canary"))

    def test_keyboard_interrupt_during_patch_retains_unknown_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_error = KeyboardInterrupt()

        with self.assertRaises(KeyboardInterrupt):
            pricing.execute_operation(deps)

        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(1, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        run_id = deps.lock_owners[-1]["run_id"]
        self.assertRegex(run_id, r"^[0-9a-f]{32}$")
        self.assertEqual(
            {
                "model": pricing.MODEL,
                "expected_revision": EXPECTED_REVISION,
                "output_dir": str(deps.output_dir),
                "run_id": run_id,
                "state": "snapshot_captured",
                "captured_pricing_version": pricing.EMPTY_VERSION,
            },
            deps.lock_owners[-1],
        )
        summary = deps.json_artifacts["summary.json"]
        self.assertEqual("unknown", summary["write_outcome"])
        self.assertEqual(
            "retained_unknown_outcome",
            summary["lock_state"],
        )
        self.assertIs(summary["lock_retained"], True)
        self.assertIs(summary["final_lock_absent"], False)
        self.assertNotIn(
            "write_outcome=not_committed",
            deps.stdout.getvalue(),
        )

    def test_keyboard_interrupt_during_ambiguous_readback_retains_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = (503, {}, b"upstream failure")
        deps.readback_error = KeyboardInterrupt()

        with self.assertRaises(KeyboardInterrupt):
            pricing.execute_operation(deps)

        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        summary = deps.json_artifacts["summary.json"]
        self.assertEqual("unknown", summary["write_outcome"])
        self.assertEqual(
            "retained_unknown_outcome",
            summary["lock_state"],
        )
        self.assertIs(summary["lock_retained"], True)
        self.assertIs(summary["final_lock_absent"], False)
        self.assertNotIn(
            "write_outcome=not_committed",
            deps.stdout.getvalue(),
        )

    def test_unknown_transport_outcome_retains_lock(self) -> None:
        deps = FakeDependencies()
        deps.patch_error = TimeoutError("timed out")
        deps.snapshots[1] = snapshot(
            configured={"ModelRatio": 9},
            version="c" * 64,
            target=False,
        )

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(2, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        self.assertEqual(
            "unknown",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )

    def test_known_not_committed_transport_outcome_releases_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_error = TimeoutError("timed out")
        deps.snapshots[1] = copy.deepcopy(deps.snapshots[0])

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertNotIn("canary", deps.events)
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertFalse(deps.lock_held)

    def test_explicit_patch_failure_target_readback_is_unknown_and_retained(
        self,
    ) -> None:
        cases = (
            (200, False),
            (400, True),
            (401, True),
            (409, True),
            (422, True),
        )
        for status, success in cases:
            with self.subTest(status=status, success=success):
                deps = FakeDependencies()
                deps.patch_result = api_response(
                    None,
                    status=status,
                    success=success,
                    message=(
                        "Authorization: Bearer %s; key=%s"
                        % (ROOT_SECRET, TOKEN_SECRET)
                    ),
                )

                exit_code = pricing.execute_operation(deps)

                self.assertEqual(1, exit_code)
                self.assertEqual(1, deps.events.count("patch"))
                self.assertEqual(2, deps.events.count("snapshot"))
                self.assertNotIn("canary", deps.events)
                self.assertNotIn("release-lock", deps.events)
                self.assertTrue(deps.lock_held)
                summary = deps.json_artifacts["summary.json"]
                self.assertEqual("unknown", summary["write_outcome"])
                self.assertEqual(
                    "retained_unknown_outcome",
                    summary["lock_state"],
                )
                error = deps.json_artifacts["pricing-write-error.json"]
                self.assertEqual(status, error["http_status"])
                serialized_error = json.dumps(error, sort_keys=True)
                self.assertNotIn(ROOT_SECRET, serialized_error)
                self.assertNotIn(TOKEN_SECRET, serialized_error)
                conflict = deps.json_artifacts["pricing-conflict.json"]
                self.assertEqual(
                    pricing.TARGET_PRICING,
                    conflict["target_entry"]["configured"],
                )

    def test_409_conflict_divergent_readback_is_unknown_and_retains_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = api_response(
            None,
            status=409,
            success=False,
            message="pricing conflict",
        )
        deps.snapshots[1] = snapshot(
            configured={"billing_setting.billing_mode": "ratio"},
            version="c" * 64,
            target=False,
        )

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        self.assertEqual(
            {
                "http_status": 409,
                "message": "pricing conflict",
            },
            deps.json_artifacts["pricing-write-error.json"],
        )
        conflict = deps.json_artifacts["pricing-conflict.json"]
        self.assertEqual(
            {
                "model",
                "option_sha256",
                "target_entry",
            },
            set(conflict),
        )
        self.assertEqual("c" * 64, conflict["target_entry"]["version"])
        self.assertNotIn("options", conflict)
        self.assertEqual(
            "unknown",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )

    def test_409_conflict_reload_failure_retains_lock_without_full_evidence(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = api_response(
            None,
            status=409,
            success=False,
            message="pricing conflict",
        )
        deps.snapshots.pop()
        deps.fail_checksum_validation = True

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        conflict = deps.json_artifacts["pricing-conflict.json"]
        self.assertEqual(pricing.MODEL, conflict["model"])
        self.assertEqual(
            "conflict_state_unavailable",
            conflict["status"],
        )
        self.assertEqual("IndexError", conflict["error"]["class"])
        self.assertNotEqual("", conflict["error"]["message"])
        self.assertEqual(
            409,
            deps.json_artifacts["pricing-write-error.json"]["http_status"],
        )
        self.assertEqual(
            "unknown",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )

    def test_409_conflict_parent_fsync_failure_retains_active_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = api_response(
            None,
            status=409,
            success=False,
            message="pricing conflict",
        )
        deps.snapshots[1] = copy.deepcopy(deps.snapshots[0])
        write_json = deps.write_json

        def fail_after_conflict_replace(
            name: str,
            payload: object,
        ) -> None:
            write_json(name, payload)
            if (
                name == "pricing-conflict.json"
                and isinstance(payload, dict)
                and payload.get("status") != "not_applicable"
            ):
                raise OSError("parent fsync failed")

        deps.write_json = fail_after_conflict_replace

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        self.assertIn("official-evidence.json", deps.json_artifacts)
        self.assertIn("pricing-before.json", deps.json_artifacts)
        self.assertIn("pricing-preview.json", deps.json_artifacts)
        self.assertIn("pricing-conflict.json", deps.json_artifacts)
        self.assertEqual(
            "not_committed",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )
        self.assertEqual(
            "retained_evidence_failure",
            deps.json_artifacts["summary.json"]["lock_state"],
        )

    def test_503_target_readback_is_unknown_and_retains_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = (
            503,
            {},
            json.dumps(
                {
                    "error": {
                        "type": "proxy_error",
                        "message": (
                            "Authorization: Bearer %s; key=%s"
                            % (ROOT_SECRET, TOKEN_SECRET)
                        ),
                    }
                }
            ).encode(),
        )

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        self.assertEqual(
            "unknown",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )
        error = deps.json_artifacts["pricing-write-error.json"]
        self.assertEqual(503, error["http_status"])
        self.assertEqual("proxy_error", error["type"])
        self.assertNotIn(ROOT_SECRET, json.dumps(error, sort_keys=True))
        self.assertNotIn(TOKEN_SECRET, json.dumps(error, sort_keys=True))

    def test_503_prestate_readback_is_not_committed_after_one_readback(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = (503, {}, b"upstream failure")
        deps.snapshots[1] = copy.deepcopy(deps.snapshots[0])

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertFalse(deps.lock_held)
        self.assertEqual(
            "not_committed",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )
        self.assertEqual(
            {
                "http_status": 503,
                "message": "non-JSON response",
            },
            deps.json_artifacts["pricing-write-error.json"],
        )

    def test_503_divergent_readback_is_unknown_after_one_readback(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = api_response(
            None,
            status=503,
            success=False,
            message="proxy unavailable",
        )
        deps.snapshots[1] = snapshot(
            configured={"billing_setting.billing_mode": "ratio"},
            version="c" * 64,
            target=False,
        )

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        self.assertEqual(
            "unknown",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )

    def test_503_malformed_readback_is_unknown_after_one_readback(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = api_response(
            None,
            status=503,
            success=False,
            message="proxy unavailable",
        )
        deps.snapshots[1] = {
            "entries": [],
            "options": {},
        }

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        self.assertEqual(
            "unknown",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )

    def test_503_readback_failure_is_unknown_after_one_readback_attempt(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = api_response(
            None,
            status=503,
            success=False,
            message="proxy unavailable",
        )
        deps.snapshots.pop()

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(2, deps.events.count("snapshot"))
        self.assertNotIn("canary", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        self.assertEqual(
            503,
            deps.json_artifacts["pricing-write-error.json"]["http_status"],
        )
        self.assertEqual(
            "unknown",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )

    def test_not_run_canary_records_null_duration(self) -> None:
        deps = FakeDependencies()
        deps.patch_result = (503, {}, b"upstream failure")
        deps.snapshots[1] = copy.deepcopy(deps.snapshots[0])

        self.assertEqual(1, pricing.execute_operation(deps))

        self.assertIsNone(
            deps.json_artifacts["canary.json"]["duration_ms"],
        )
        self.assertNotIn("monotonic", deps.events)

    def test_committed_write_runs_exactly_one_pinned_canary_then_releases(
        self,
    ) -> None:
        deps = FakeDependencies()

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(0, exit_code)
        self.assertEqual(1, deps.events.count("canary"))
        self.assertGreater(
            deps.events.index("canary"),
            deps.events.index("patch"),
        )
        self.assertEqual(
            [
                {
                    "model": pricing.MODEL,
                    "messages": [
                        {
                            "role": "user",
                            "content": "Reply with exactly: OK",
                        }
                    ],
                    "max_tokens": 32,
                    "stream": False,
                }
            ],
            deps.request_bodies["/v1/chat/completions"],
        )
        self.assertEqual(
            "Bearer sk-%s-48" % TOKEN_SECRET,
            deps.canary_authorization,
        )
        self.assertEqual(
            1,
            deps.events.count("request-log:request-123"),
        )
        self.assertEqual(
            125,
            deps.json_artifacts["canary.json"]["duration_ms"],
        )
        self.assertEqual(
            125,
            deps.json_artifacts["summary.json"]["canary_duration_ms"],
        )
        self.assertEqual(2, deps.events.count("monotonic"))
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertFalse(deps.lock_held)

    def test_canary_requires_exact_assistant_ok_response(self) -> None:
        cases = {
            "invalid_json": (b"not-json", "invalid_json"),
            "non_object": (b"[]", "non_object"),
            "top_level_error": (
                json.dumps(
                    {
                        "error": {
                            "message": "token=" + TOKEN_SECRET,
                        },
                        "choices": [
                            {"message": {"content": "OK"}},
                        ],
                    }
                ).encode(),
                "top_level_error",
            ),
            "success_false": (
                json.dumps(
                    {
                        "success": False,
                        "choices": [
                            {"message": {"content": "OK"}},
                        ],
                    }
                ).encode(),
                "success_false",
            ),
            "choices_missing": (b"{}", "choices_missing"),
            "choices_not_list": (
                b'{"choices":{}}',
                "choices_not_list",
            ),
            "choices_empty": (b'{"choices":[]}', "choices_empty"),
            "message_missing": (
                b'{"choices":[{}]}',
                "message_missing",
            ),
            "message_not_object": (
                b'{"choices":[{"message":"OK"}]}',
                "message_not_object",
            ),
            "content_missing": (
                b'{"choices":[{"message":{}}]}',
                "content_missing",
            ),
            "content_not_string": (
                b'{"choices":[{"message":{"content":42}}]}',
                "content_not_string",
            ),
            "content_not_ok": (
                json.dumps(
                    {
                        "choices": [
                            {
                                "message": {
                                    "content": "token=" + TOKEN_SECRET,
                                }
                            }
                        ]
                    }
                ).encode(),
                "content_not_ok",
            ),
        }
        for name, (raw, expected_classification) in cases.items():
            with self.subTest(name=name):
                deps = FakeDependencies()
                deps.canary_result = (
                    200,
                    {"X-Oneapi-Request-Id": "request-" + name},
                    raw,
                )

                self.assertEqual(1, pricing.execute_operation(deps))

                canary = deps.json_artifacts["canary.json"]
                self.assertEqual("failed", canary["status"])
                self.assertEqual(200, canary["http_status"])
                self.assertEqual(
                    expected_classification,
                    canary["body_classification"],
                )
                self.assertEqual(
                    "response_validation",
                    canary["response_error"]["kind"],
                )
                self.assertNotIn(
                    TOKEN_SECRET,
                    json.dumps(deps.json_artifacts, sort_keys=True),
                )

    def test_canary_requires_assistant_role_and_trimmed_exact_ok(
        self,
    ) -> None:
        missing = object()
        cases = (
            ("assistant", " \nOK\t ", "assistant_ok", 0),
            ("user", "OK", "role_not_assistant", 1),
            ("system", "OK", "role_not_assistant", 1),
            (missing, "OK", "role_missing", 1),
            (42, "OK", "role_not_string", 1),
            ("assistant", "OK.", "content_not_ok", 1),
        )

        for role, content, expected_classification, expected_exit in cases:
            with self.subTest(
                role=role,
                content=content,
                expected_classification=expected_classification,
            ):
                message = {"content": content}
                if role is not missing:
                    message["role"] = role
                deps = FakeDependencies()
                deps.canary_result = (
                    200,
                    {"X-Oneapi-Request-Id": "request-role-matrix"},
                    json.dumps(
                        {"choices": [{"message": message}]}
                    ).encode(),
                )

                self.assertEqual(
                    expected_exit,
                    pricing.execute_operation(deps),
                )

                canary = deps.json_artifacts["canary.json"]
                self.assertEqual(
                    expected_classification,
                    canary["body_classification"],
                )
                self.assertEqual(
                    "passed" if expected_exit == 0 else "failed",
                    canary["status"],
                )

    def test_canary_token_setup_failure_blocks_patch_and_validates_poststate(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.token_row_error = RuntimeError(
            "token=" + TOKEN_SECRET + " unavailable"
        )
        deps.fail_checksum_validation = True

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(0, deps.events.count("patch"))
        self.assertEqual(0, deps.events.count("canary"))
        self.assertEqual(1, deps.events.count("state-after"))
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        self.assertLess(
            deps.events.index("token-row"),
            deps.events.index("state-after"),
        )
        self.assertNotIn(
            TOKEN_SECRET,
            json.dumps(deps.json_artifacts, sort_keys=True),
        )

    def test_canary_timing_failures_do_not_block_request_or_poststate(
        self,
    ) -> None:
        cases = {
            "start": [RuntimeError("timer start failed")],
            "finish": [10.0, RuntimeError("timer finish failed")],
        }
        for stage, effects in cases.items():
            with self.subTest(stage=stage):
                deps = FakeDependencies()
                deps.monotonic_effects = effects

                exit_code = pricing.execute_operation(deps)

                self.assertEqual(0, exit_code)
                self.assertEqual(1, deps.events.count("patch"))
                self.assertEqual(1, deps.events.count("canary"))
                self.assertEqual(1, deps.events.count("state-after"))
                self.assertIsNone(
                    deps.json_artifacts["canary.json"]["duration_ms"],
                )
                self.assertIn(
                    "timing_error",
                    deps.json_artifacts["canary.json"],
                )

    def test_canary_result_error_is_recorded_before_poststate(self) -> None:
        deps = FakeDependencies()
        deps.canary_result = (200, None, b"{}")

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(1, deps.events.count("canary"))
        self.assertEqual(1, deps.events.count("state-after"))
        self.assertEqual(
            "result",
            deps.json_artifacts["canary.json"]["error"]["kind"],
        )

    def test_success_finalizes_evidence_before_commit_rename(
        self,
    ) -> None:
        deps = FakeDependencies()
        finalize_evidence = deps.finalize_evidence

        def finalize_before_stdout(
            summary: dict[str, object],
            required_artifacts: set[str],
        ) -> None:
            self.assertEqual("", deps.stdout.getvalue())
            finalize_evidence(summary, required_artifacts)

        deps.finalize_evidence = finalize_before_stdout

        self.assertEqual(0, pricing.execute_operation(deps))

        release_index = deps.events.index("release-lock")
        summary_indexes = [
            index
            for index, event in enumerate(deps.events)
            if event == "write-json:summary.json"
        ]
        checksum_indexes = [
            index
            for index, event in enumerate(deps.events)
            if event == "checksums:SHA256SUMS"
        ]
        validation_indexes = [
            index
            for index, event in enumerate(deps.events)
            if event == "validate-checksums:SHA256SUMS"
        ]
        self.assertEqual(2, len(summary_indexes))
        self.assertEqual(2, len(checksum_indexes))
        self.assertEqual(2, len(validation_indexes))
        self.assertLess(summary_indexes[0], checksum_indexes[0])
        self.assertLess(checksum_indexes[0], validation_indexes[0])
        self.assertLess(validation_indexes[0], summary_indexes[1])
        self.assertLess(summary_indexes[1], checksum_indexes[1])
        self.assertLess(checksum_indexes[1], validation_indexes[1])
        publish_index = deps.events.index("publish-seal")
        self.assertLess(validation_indexes[1], publish_index)
        self.assertLess(publish_index, release_index)
        self.assertEqual("release-lock", deps.events[-1])
        self.assertNotIn("lock-exists", deps.events)
        self.assertNotIn("lock-topology", deps.events)
        self.assertTrue(deps.seal_marker_exists)
        self.assertTrue(deps.seal_valid)
        held, absent = deps.json_artifact_history["summary.json"]
        self.assertEqual("held_pending_release", held["lock_state"])
        self.assertIs(held["sealed"], False)
        self.assertIs(held["final_lock_absent"], False)
        self.assertIsNone(held["lock_tombstone_path"])
        self.assertEqual("absent", absent["lock_state"])
        self.assertIs(absent["final_lock_absent"], True)
        self.assertEqual(
            str(deps.lock_tombstone_path),
            absent["lock_tombstone_path"],
        )
        self.assertRegex(absent["run_id"], r"^[0-9a-f]{32}$")
        self.assertRegex(absent["seal_id"], r"^[0-9a-f]{32}$")
        self.assertEqual(
            str(deps.lock_tombstone_path / "seal.json"),
            absent["seal_marker_path"],
        )

    def test_success_seals_active_lock_before_atomic_commit_rename(
        self,
    ) -> None:
        deps = FakeDependencies()
        publish_seal = deps.publish_seal
        release_lock = deps.release_lock

        def publish_while_active(tombstone_path: pathlib.Path) -> None:
            self.assertTrue(deps.lock_held)
            self.assertFalse(deps.tombstone_exists)
            publish_seal(tombstone_path)

        def release_expected(
            tombstone_path: pathlib.Path | None = None,
        ) -> pathlib.Path:
            self.assertEqual(deps.lock_tombstone_path, tombstone_path)
            self.assertTrue(deps.lock_held)
            self.assertFalse(deps.tombstone_exists)
            self.assertTrue(deps.seal_marker_exists)
            self.assertTrue(deps.seal_valid)
            return release_lock(tombstone_path)

        deps.publish_seal = publish_while_active
        deps.release_lock = release_expected

        self.assertEqual(0, pricing.execute_operation(deps))

        prepare_index = deps.events.index("prepare-lock-commit")
        finalize_index = deps.events.index("stage-final-evidence")
        publish_index = deps.events.index("publish-seal")
        release_index = deps.events.index("release-lock")
        self.assertLess(prepare_index, finalize_index)
        self.assertLess(finalize_index, publish_index)
        self.assertLess(publish_index, release_index)
        self.assertEqual("release-lock", deps.events[-1])
        self.assertFalse(deps.lock_held)
        self.assertTrue(deps.tombstone_exists)
        self.assertTrue(deps.seal_marker_exists)
        self.assertTrue(deps.seal_valid)

    def test_rename_failure_keeps_sealed_active_lock(self) -> None:
        deps = FakeDependencies()
        failure = OSError("commit rename failed")

        def fail_before_rename(
            tombstone_path: pathlib.Path,
        ) -> pathlib.Path:
            deps.events.append("release-lock")
            self.assertEqual(deps.lock_tombstone_path, tombstone_path)
            raise failure

        deps.release_lock = fail_before_rename

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertIn("stage-final-evidence", deps.events)
        self.assertIn("publish-seal", deps.events)
        self.assertTrue(deps.seal_marker_exists)
        self.assertTrue(deps.seal_valid)
        self.assertTrue(deps.lock_held)
        self.assertFalse(deps.tombstone_exists)
        self.assertEqual(1, deps.events.count("lock-topology"))
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("invalidate-seal", deps.events)
        self.assertNotIn("invalidate-evidence", deps.events)
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "overall_status=failed",
                "lock_state=precommit_retained",
                "lock_tombstone_path=%s" % deps.lock_tombstone_path,
                "failure=rename error: OSError: commit rename failed",
            ],
            deps.stdout.getvalue().splitlines(),
        )

    def test_committed_seal_survives_old_run_interrupt_after_next_acquire(
        self,
    ) -> None:
        for signal in (
            KeyboardInterrupt("old run interrupted after seal"),
            SystemExit(23),
        ):
            with self.subTest(signal=type(signal).__name__):
                deps = FakeDependencies()
                release_lock = deps.release_lock

                def release_then_next_run_acquires(
                    tombstone_path: pathlib.Path,
                ) -> pathlib.Path:
                    released = release_lock(tombstone_path)
                    deps.events.append("next-run-validate-seal")
                    deps.lock_held = True
                    deps.events.append("next-run-acquire")
                    raise signal

                deps.release_lock = release_then_next_run_acquires

                with self.assertRaises(type(signal)) as raised:
                    pricing.execute_operation(deps)

                self.assertIs(signal, raised.exception)
                self.assertTrue(deps.seal_marker_exists)
                self.assertTrue(deps.seal_valid)
                self.assertTrue(deps.tombstone_exists)
                self.assertTrue(deps.lock_held)
                next_run_index = deps.events.index("next-run-acquire")
                self.assertNotIn(
                    "invalidate-seal",
                    deps.events[next_run_index:],
                )
                self.assertNotIn(
                    "invalidate-evidence",
                    deps.events[next_run_index:],
                )
                self.assertNotIn(
                    "restore-lock",
                    deps.events[next_run_index:],
                )
                self.assertNotIn("seal-publication-state", deps.events)

    def test_publish_error_with_committed_marker_keeps_active_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        publish_seal = deps.publish_seal
        failure = OSError("publish interrupted before return")

        def publish_then_fail(tombstone_path: pathlib.Path) -> None:
            publish_seal(tombstone_path)
            raise failure

        deps.publish_seal = publish_then_fail

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertTrue(deps.seal_marker_exists)
        self.assertFalse(deps.tombstone_exists)
        self.assertTrue(deps.lock_held)
        self.assertNotIn("release-lock", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("invalidate-seal", deps.events)
        self.assertNotIn("invalidate-evidence", deps.events)
        self.assertIn(
            "lock_state=retained_commit_failure",
            deps.stdout.getvalue().splitlines(),
        )

    def test_post_publish_first_statement_failure_never_rolls_back(
        self,
    ) -> None:
        for failure in (
            KeyboardInterrupt("interrupted after seal return"),
            SystemExit(23),
            OSError("failed after seal return"),
        ):
            with self.subTest(failure=type(failure).__name__):
                deps = FakeDependencies()
                publish_seal = deps.publish_seal
                armed = False

                def publish_then_next_run_acquires(
                    tombstone_path: pathlib.Path,
                ) -> None:
                    nonlocal armed
                    publish_seal(tombstone_path)
                    armed = True

                def fail_next_execute_line(frame, event, arg):
                    del arg
                    nonlocal armed
                    if (
                        armed
                        and event == "line"
                        and frame.f_code
                        is pricing.execute_operation.__code__
                    ):
                        armed = False
                        raise failure
                    return fail_next_execute_line

                deps.publish_seal = publish_then_next_run_acquires
                previous_trace = sys.gettrace()
                sys.settrace(fail_next_execute_line)
                try:
                    if isinstance(failure, Exception):
                        self.assertEqual(1, pricing.execute_operation(deps))
                    else:
                        with self.assertRaises(type(failure)) as raised:
                            pricing.execute_operation(deps)
                        self.assertIs(failure, raised.exception)
                finally:
                    sys.settrace(previous_trace)

                self.assertTrue(deps.seal_marker_exists)
                self.assertTrue(deps.seal_valid)
                self.assertFalse(deps.tombstone_exists)
                self.assertTrue(deps.lock_held)
                publish_index = deps.events.index("publish-seal")
                self.assertNotIn(
                    "seal-publication-state",
                    deps.events[publish_index:],
                )
                self.assertNotIn(
                    "invalidate-seal",
                    deps.events[publish_index:],
                )
                self.assertNotIn(
                    "invalidate-evidence",
                    deps.events[publish_index:],
                )
                self.assertNotIn(
                    "restore-lock",
                    deps.events[publish_index:],
                )

    def test_unverified_competing_marker_is_never_clobbered_or_restored(
        self,
    ) -> None:
        deps = FakeDependencies()
        competing_marker = {"owner": "competing-run"}

        def lose_marker_publication_race(
            tombstone_path: pathlib.Path,
        ) -> None:
            deps.events.append("publish-seal")
            self.assertEqual(deps.lock_tombstone_path, tombstone_path)
            deps.seal_marker_exists = True
            deps.seal_valid = False
            deps.events.append("competing-marker")
            raise FileExistsError("seal.json already exists")

        deps.publish_seal = lose_marker_publication_race
        deps.competing_marker = competing_marker

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertIs(competing_marker, deps.competing_marker)
        self.assertTrue(deps.seal_marker_exists)
        self.assertFalse(deps.tombstone_exists)
        self.assertTrue(deps.lock_held)
        competing_index = deps.events.index("competing-marker")
        self.assertNotIn("invalidate-seal", deps.events[competing_index:])
        self.assertNotIn(
            "invalidate-evidence",
            deps.events[competing_index:],
        )
        self.assertNotIn("restore-lock", deps.events[competing_index:])
        self.assertIn(
            "lock_state=retained_commit_failure",
            deps.stdout.getvalue().splitlines(),
        )

    def test_publish_interrupt_preserves_active_marker_and_original_signal(
        self,
    ) -> None:
        deps = FakeDependencies()
        publish_seal = deps.publish_seal
        interrupt = KeyboardInterrupt("old run interrupted after seal")

        def publish_then_interrupt(tombstone_path: pathlib.Path) -> None:
            publish_seal(tombstone_path)
            raise interrupt

        deps.publish_seal = publish_then_interrupt

        with self.assertRaises(KeyboardInterrupt) as raised:
            pricing.execute_operation(deps)

        self.assertIs(interrupt, raised.exception)
        self.assertTrue(deps.seal_marker_exists)
        self.assertFalse(deps.tombstone_exists)
        self.assertTrue(deps.lock_held)
        publish_index = deps.events.index("publish-seal")
        self.assertNotIn("release-lock", deps.events[publish_index:])
        self.assertNotIn("invalidate-seal", deps.events[publish_index:])
        self.assertNotIn("invalidate-evidence", deps.events[publish_index:])
        self.assertNotIn("restore-lock", deps.events[publish_index:])

    def test_finalization_failure_before_publish_keeps_active_lock(self) -> None:
        deps = FakeDependencies()
        finalize_evidence = deps.finalize_evidence
        failure = OSError("final evidence durability unknown")

        def finalize_then_fail(
            summary: dict[str, object],
            required_artifacts: set[str],
        ) -> None:
            finalize_evidence(summary, required_artifacts)
            raise failure

        deps.finalize_evidence = finalize_then_fail

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertNotIn("publish-seal", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("invalidate-evidence", deps.events)
        self.assertFalse(deps.seal_marker_exists)
        self.assertTrue(deps.lock_held)
        self.assertFalse(deps.tombstone_exists)
        summary = deps.json_artifacts["summary.json"]
        self.assertIs(summary["evidence_complete"], True)
        self.assertIs(summary["sealed"], True)
        self.assertIs(summary["lock_retained"], False)
        self.assertIs(summary["final_lock_absent"], True)
        self.assertEqual("absent", summary["lock_state"])
        self.assertEqual(
            str(deps.lock_tombstone_path),
            summary["lock_tombstone_path"],
        )
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "overall_status=failed",
                "lock_state=retained_commit_failure",
                "lock_tombstone_path=none",
                (
                    "failure=finalize_evidence error: OSError: "
                    "final evidence durability unknown"
                ),
            ],
            deps.stdout.getvalue().splitlines(),
        )

    def test_precommit_tombstone_collision_keeps_active_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        finalize_evidence = deps.finalize_evidence

        def finalize_then_create_tombstone(
            summary: dict[str, object],
            required_artifacts: set[str],
        ) -> None:
            finalize_evidence(summary, required_artifacts)
            deps.tombstone_exists = True

        deps.finalize_evidence = finalize_then_create_tombstone

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertIn("publish-seal", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertFalse(deps.seal_marker_exists)
        self.assertTrue(deps.lock_held)
        self.assertTrue(deps.tombstone_exists)
        summary = deps.json_artifacts["summary.json"]
        self.assertIs(summary["evidence_complete"], True)
        self.assertIs(summary["sealed"], True)
        self.assertIs(summary["lock_retained"], False)
        self.assertIs(summary["final_lock_absent"], True)
        self.assertEqual("absent", summary["lock_state"])
        self.assertEqual(
            str(deps.lock_tombstone_path),
            summary["lock_tombstone_path"],
        )
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "overall_status=failed",
                "lock_state=retained_commit_failure",
                "lock_tombstone_path=none",
                (
                    "failure=publish_seal error: AssertionError: "
                    "seal publication requires only the active lock"
                ),
            ],
            deps.stdout.getvalue().splitlines(),
        )

    def test_prepare_commit_failure_keeps_active_evidence_unmodified(
        self,
    ) -> None:
        deps = FakeDependencies()

        def fail_prepare() -> tuple[pathlib.Path, str]:
            deps.events.append("prepare-lock-commit")
            raise OSError("commit identity allocation failed")

        deps.prepare_lock_commit = fail_prepare

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("prepare-lock-commit"))
        self.assertNotIn("stage-final-evidence", deps.events)
        self.assertNotIn("publish-seal", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertFalse(deps.seal_marker_exists)
        self.assertTrue(deps.lock_held)
        self.assertFalse(deps.tombstone_exists)
        summary = deps.json_artifacts["summary.json"]
        self.assertIs(summary["evidence_complete"], True)
        self.assertIs(summary["sealed"], False)
        self.assertIs(summary["lock_retained"], True)
        self.assertIs(summary["final_lock_absent"], False)
        self.assertEqual("held_pending_release", summary["lock_state"])
        self.assertIsNone(summary["lock_tombstone_path"])
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "overall_status=failed",
                "lock_state=retained_commit_failure",
                "lock_tombstone_path=none",
                (
                    "failure=prepare error: OSError: "
                    "commit identity allocation failed"
                ),
            ],
            deps.stdout.getvalue().splitlines(),
        )

    def test_final_summary_failure_keeps_active_lock_before_commit(
        self,
    ) -> None:
        deps = FakeDependencies()
        finalize_evidence = deps.finalize_evidence

        def fail_after_final_summary_replace(
            summary: dict[str, object],
            required_artifacts: set[str],
        ) -> None:
            finalize_evidence(summary, required_artifacts)
            raise OSError("final summary parent fsync failed")

        deps.finalize_evidence = fail_after_final_summary_replace

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertNotIn("publish-seal", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("invalidate-evidence", deps.events)
        self.assertTrue(deps.lock_held)
        summary = deps.json_artifacts["summary.json"]
        self.assertEqual("committed", summary["write_outcome"])
        self.assertIs(summary["evidence_complete"], True)
        self.assertIs(summary["sealed"], True)
        self.assertIs(summary["lock_retained"], False)
        self.assertIs(summary["final_lock_absent"], True)
        self.assertEqual("absent", summary["lock_state"])
        self.assertEqual(
            str(deps.lock_tombstone_path),
            summary["lock_tombstone_path"],
        )
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "overall_status=failed",
                "lock_state=retained_commit_failure",
                "lock_tombstone_path=none",
                (
                    "failure=finalize_evidence error: OSError: "
                    "final summary parent fsync failed"
                ),
            ],
            deps.stdout.getvalue().splitlines(),
        )

    def test_finalization_failure_does_not_rewrite_or_invalidate_evidence(
        self,
    ) -> None:
        deps = FakeDependencies()
        finalize_evidence = deps.finalize_evidence

        def fail_after_final_summary_replace(
            summary: dict[str, object],
            required_artifacts: set[str],
        ) -> None:
            finalize_evidence(summary, required_artifacts)
            raise OSError("final summary parent fsync failed")

        deps.finalize_evidence = fail_after_final_summary_replace
        deps.fail_artifact = "summary.json"
        deps.fail_artifact_on_attempt = 3

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertTrue(deps.lock_held)
        self.assertFalse(deps.tombstone_exists)
        self.assertFalse(deps.seal_valid)
        self.assertNotIn("invalidate-evidence", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("release-lock", deps.events)
        self.assertEqual(2, deps.artifact_write_attempts["summary.json"])
        persisted = deps.json_artifacts["summary.json"]
        self.assertIs(persisted["sealed"], True)
        self.assertIs(persisted["final_lock_absent"], True)

    def test_publish_failure_does_not_rewrite_final_evidence(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.seal_publish_error = OSError("seal publication failed")
        deps.fail_artifact = "summary.json"
        deps.fail_artifact_on_attempt = 3

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertTrue(deps.lock_held)
        self.assertFalse(deps.tombstone_exists)
        self.assertFalse(deps.seal_marker_exists)
        self.assertNotIn("release-lock", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("invalidate-evidence", deps.events)
        self.assertEqual(2, deps.artifact_write_attempts["summary.json"])
        persisted = deps.json_artifacts["summary.json"]
        self.assertIs(persisted["sealed"], True)
        self.assertIs(persisted["final_lock_absent"], True)

    def test_release_failure_keeps_finalized_active_evidence(self) -> None:
        deps = FakeDependencies()
        deps.release_error = OSError("release failed")

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertTrue(deps.lock_held)
        self.assertFalse(deps.tombstone_exists)
        self.assertTrue(deps.seal_marker_exists)
        self.assertTrue(deps.seal_valid)
        self.assertEqual(2, len(deps.json_artifact_history["summary.json"]))
        summary = deps.json_artifacts["summary.json"]
        self.assertEqual("absent", summary["lock_state"])
        self.assertIs(summary["lock_retained"], False)
        self.assertIs(summary["final_lock_absent"], True)
        self.assertEqual(
            str(deps.lock_tombstone_path),
            summary["lock_tombstone_path"],
        )
        self.assertEqual(
            2,
            deps.events.count("checksums:SHA256SUMS"),
        )
        self.assertEqual(
            2,
            deps.events.count("validate-checksums:SHA256SUMS"),
        )
        release_index = deps.events.index("release-lock")
        self.assertNotIn("restore-lock", deps.events[release_index:])
        self.assertNotIn("invalidate-seal", deps.events[release_index:])
        self.assertNotIn("invalidate-evidence", deps.events[release_index:])

    def test_parent_fsync_failure_is_classified_as_committed_after_probe(
        self,
    ) -> None:
        deps = FakeDependencies()
        release_error = OSError("release parent fsync failed")
        release_lock = deps.release_lock

        def release_then_fail_durability(
            tombstone_path: pathlib.Path,
        ) -> pathlib.Path:
            release_lock(tombstone_path)
            raise pricing.LockCommitDurabilityError(
                release_error,
                tombstone_path,
            )

        deps.release_lock = release_then_fail_durability

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(1, deps.events.count("canary"))
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("invalidate-seal", deps.events)
        self.assertNotIn("invalidate-evidence", deps.events)
        self.assertEqual(1, deps.events.count("lock-topology"))
        self.assertEqual(1, deps.events.count("seal-publication-state"))
        self.assertFalse(deps.lock_held)
        self.assertTrue(deps.tombstone_exists)
        self.assertTrue(deps.seal_valid)
        summary = deps.json_artifacts["summary.json"]
        self.assertIs(summary["evidence_complete"], True)
        self.assertIs(summary["sealed"], True)
        self.assertIs(summary["lock_retained"], False)
        self.assertIs(summary["final_lock_absent"], True)
        self.assertEqual("absent", summary["lock_state"])
        self.assertEqual(
            str(deps.lock_tombstone_path),
            summary["lock_tombstone_path"],
        )
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "overall_status=failed",
                "lock_state=committed",
                "lock_tombstone_path=%s" % deps.lock_tombstone_path,
                (
                    "failure=rename error: LockCommitDurabilityError: "
                    "pricing lock commit durability unknown "
                    "(OSError: release parent fsync failed)"
                ),
            ],
            deps.stdout.getvalue().splitlines(),
        )

    def test_rename_failure_is_classified_as_precommit_retained(
        self,
    ) -> None:
        deps = FakeDependencies()

        def fail_rename(
            tombstone_path: pathlib.Path,
        ) -> pathlib.Path:
            deps.events.append("release-lock")
            self.assertEqual(deps.lock_tombstone_path, tombstone_path)
            raise OSError("rename failed")

        deps.release_lock = fail_rename

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertTrue(deps.lock_held)
        self.assertFalse(deps.tombstone_exists)
        self.assertTrue(deps.seal_valid)
        self.assertEqual(1, deps.events.count("lock-topology"))
        self.assertNotIn("seal-publication-state", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("invalidate-evidence", deps.events)
        summary = deps.json_artifacts["summary.json"]
        self.assertIs(summary["sealed"], True)
        self.assertIs(summary["lock_retained"], False)
        self.assertIs(summary["final_lock_absent"], True)
        self.assertEqual("absent", summary["lock_state"])
        self.assertEqual(
            str(deps.lock_tombstone_path),
            summary["lock_tombstone_path"],
        )
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "overall_status=failed",
                "lock_state=precommit_retained",
                "lock_tombstone_path=%s" % deps.lock_tombstone_path,
                "failure=rename error: OSError: rename failed",
            ],
            deps.stdout.getvalue().splitlines(),
        )

    def test_release_signal_precommit_is_probed_then_rethrown(self) -> None:
        for signal in (
            KeyboardInterrupt("rename interrupted"),
            SystemExit(31),
        ):
            with self.subTest(signal=type(signal).__name__):
                deps = FakeDependencies()

                def fail_before_rename(
                    tombstone_path: pathlib.Path,
                ) -> pathlib.Path:
                    deps.events.append("release-lock")
                    self.assertEqual(
                        deps.lock_tombstone_path,
                        tombstone_path,
                    )
                    raise signal

                deps.release_lock = fail_before_rename

                with self.assertRaises(type(signal)) as raised:
                    pricing.execute_operation(deps)

                self.assertIs(signal, raised.exception)
                self.assertTrue(deps.lock_held)
                self.assertFalse(deps.tombstone_exists)
                self.assertEqual(1, deps.events.count("lock-topology"))
                self.assertNotIn("seal-publication-state", deps.events)
                output = deps.stdout.getvalue().splitlines()
                self.assertIn("lock_state=precommit_retained", output)
                self.assertIn(
                    "lock_tombstone_path=%s"
                    % deps.lock_tombstone_path,
                    output,
                )

    def test_release_original_signal_wins_when_commit_probe_is_interrupted(
        self,
    ) -> None:
        deps = FakeDependencies()
        original_signal = KeyboardInterrupt("rename interrupted")
        deps.lock_topology_error = SystemExit(41)

        def fail_release(tombstone_path: pathlib.Path) -> pathlib.Path:
            deps.events.append("release-lock")
            self.assertEqual(deps.lock_tombstone_path, tombstone_path)
            raise original_signal

        deps.release_lock = fail_release

        with self.assertRaises(KeyboardInterrupt) as raised:
            pricing.execute_operation(deps)

        self.assertIs(original_signal, raised.exception)
        self.assertEqual(1, deps.events.count("lock-topology"))
        self.assertEqual(1, deps.events.count("state-after"))
        output = deps.stdout.getvalue().splitlines()
        self.assertIn("lock_state=commit_state_unknown", output)
        self.assertIn(
            "lock_tombstone_path=%s" % deps.lock_tombstone_path,
            output,
        )
        self.assertFalse(any("retained" in line for line in output))

    def test_release_ambiguous_state_preserves_candidate_without_retained_claim(
        self,
    ) -> None:
        cases = {
            "both_paths": (True, True, None, True),
            "neither_path": (False, False, None, True),
            "topology_probe_failure": (
                True,
                False,
                OSError("topology probe failed"),
                True,
            ),
            "unverified_tombstone": (False, True, None, False),
        }
        for name, (
            active_exists,
            tombstone_exists,
            topology_error,
            seal_valid,
        ) in cases.items():
            with self.subTest(name=name):
                deps = FakeDependencies()
                deps.lock_topology_error = topology_error

                def fail_with_ambiguous_topology(
                    tombstone_path: pathlib.Path,
                ) -> pathlib.Path:
                    deps.events.append("release-lock")
                    self.assertEqual(
                        deps.lock_tombstone_path,
                        tombstone_path,
                    )
                    deps.lock_held = active_exists
                    deps.tombstone_exists = tombstone_exists
                    deps.seal_valid = seal_valid
                    raise OSError("release failed")

                deps.release_lock = fail_with_ambiguous_topology

                self.assertEqual(1, pricing.execute_operation(deps))

                output = deps.stdout.getvalue().splitlines()
                self.assertIn("lock_state=commit_state_unknown", output)
                self.assertIn(
                    "lock_tombstone_path=%s"
                    % deps.lock_tombstone_path,
                    output,
                )
                self.assertFalse(
                    any("retained" in line for line in output)
                )
                self.assertEqual(1, deps.events.count("lock-topology"))
                if (
                    topology_error is None
                    and not active_exists
                    and tombstone_exists
                ):
                    self.assertEqual(
                        1,
                        deps.events.count("seal-publication-state"),
                    )
                else:
                    self.assertNotIn(
                        "seal-publication-state",
                        deps.events,
                    )

    def test_parent_fsync_failure_does_not_correct_committed_evidence(
        self,
    ) -> None:
        deps = FakeDependencies()
        release_error = OSError("release parent fsync failed")
        release_lock = deps.release_lock
        deps.fail_artifact = "summary.json"
        deps.fail_artifact_on_attempt = 3

        def release_then_fail_durability(
            tombstone_path: pathlib.Path,
        ) -> pathlib.Path:
            release_lock(tombstone_path)
            raise pricing.LockCommitDurabilityError(
                release_error,
                tombstone_path,
            )

        deps.release_lock = release_then_fail_durability

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertFalse(deps.lock_held)
        self.assertTrue(deps.tombstone_exists)
        self.assertTrue(deps.seal_valid)
        self.assertEqual(2, deps.artifact_write_attempts["summary.json"])
        self.assertNotIn("invalidate-evidence", deps.events)
        self.assertNotIn("invalidate-seal", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertIs(
            deps.json_artifacts["summary.json"]["sealed"],
            True,
        )

    def test_success_does_not_probe_after_commit(self) -> None:
        deps = FakeDependencies()

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(0, exit_code)
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertNotIn("lock-topology", deps.events)
        self.assertFalse(deps.lock_held)
        self.assertTrue(deps.tombstone_exists)
        summary = deps.json_artifacts["summary.json"]
        self.assertIs(summary["sealed"], True)
        self.assertIs(summary["final_lock_absent"], True)
        self.assertIs(summary["lock_retained"], False)

    def test_success_has_no_post_commit_validation_or_recovery_write(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.fail_checksum_validation_on_attempt = 3
        deps.fail_artifact = "summary.json"
        deps.fail_artifact_on_attempt = 3

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(0, exit_code)
        self.assertFalse(deps.lock_held)
        self.assertTrue(deps.tombstone_exists)
        self.assertEqual(2, deps.checksum_validation_attempts)
        self.assertEqual(2, deps.artifact_write_attempts["summary.json"])
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("invalidate-evidence", deps.events)
        persisted = deps.json_artifacts["summary.json"]
        self.assertIs(persisted["sealed"], True)
        self.assertIs(persisted["final_lock_absent"], True)
        self.assertIs(persisted["lock_retained"], False)
        self.assertEqual("absent", persisted["lock_state"])

    def test_durability_failure_stdout_uses_committed_tombstone(
        self,
    ) -> None:
        deps = FakeDependencies()
        release_lock = deps.release_lock
        failure = OSError("parent fsync failed")

        def release_then_fail_durability(
            tombstone_path: pathlib.Path,
        ) -> pathlib.Path:
            release_lock(tombstone_path)
            raise pricing.LockCommitDurabilityError(
                failure,
                tombstone_path,
            )

        deps.release_lock = release_then_fail_durability

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertFalse(deps.lock_held)
        self.assertTrue(deps.tombstone_exists)
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "overall_status=failed",
                "lock_state=committed",
                "lock_tombstone_path=%s" % deps.lock_tombstone_path,
                (
                    "failure=rename error: LockCommitDurabilityError: "
                    "pricing lock commit durability unknown "
                    "(OSError: parent fsync failed)"
                ),
            ],
            deps.stdout.getvalue().splitlines(),
        )
        self.assertEqual(1, deps.events.count("lock-topology"))
        self.assertEqual(1, deps.events.count("seal-publication-state"))
        self.assertNotIn("restore-lock", deps.events)

    def test_failed_canary_stdout_reports_committed_tombstone(self) -> None:
        deps = FakeDependencies()
        deps.canary_result = (
            503,
            {"X-Oneapi-Request-Id": "request-123"},
            json.dumps(
                {
                    "error": {
                        "type": "service_unavailable",
                        "message": "upstream unavailable",
                    }
                }
            ).encode(),
        )
        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "write_outcome=committed",
                "canary_status=failed channel=48",
                "lock_tombstone_path=%s" % deps.lock_tombstone_path,
            ],
            deps.stdout.getvalue().splitlines(),
        )
        self.assertFalse(deps.lock_held)
        self.assertTrue(deps.tombstone_exists)
        self.assertTrue(deps.seal_valid)
        self.assertNotIn("lock-topology", deps.events)

    def test_publish_failure_never_calls_restore_or_invalidation(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.seal_publish_error = OSError("marker write failed")

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertTrue(deps.lock_held)
        self.assertFalse(deps.tombstone_exists)
        self.assertFalse(deps.seal_valid)
        self.assertNotIn("release-lock", deps.events)
        self.assertNotIn("restore-lock", deps.events)
        self.assertNotIn("invalidate-seal", deps.events)
        self.assertNotIn("invalidate-evidence", deps.events)

    def test_commit_durability_failure_rethrows_wrapped_signals(
        self,
    ) -> None:
        for signal in (
            KeyboardInterrupt("commit durability interrupted"),
            SystemExit(23),
        ):
            with self.subTest(signal=type(signal).__name__):
                deps = FakeDependencies()

                def release_then_fail_durability(
                    tombstone_path: pathlib.Path,
                ) -> pathlib.Path:
                    deps.events.append("release-lock")
                    self.assertEqual(
                        deps.lock_tombstone_path,
                        tombstone_path,
                    )
                    deps.lock_held = False
                    deps.tombstone_exists = True
                    raise pricing.LockCommitDurabilityError(
                        signal,
                        tombstone_path,
                    )

                deps.release_lock = release_then_fail_durability

                with self.assertRaises(type(signal)) as raised:
                    pricing.execute_operation(deps)

                self.assertIs(signal, raised.exception)
                self.assertFalse(deps.lock_held)
                self.assertTrue(deps.tombstone_exists)
                self.assertTrue(deps.seal_valid)
                self.assertEqual(1, deps.events.count("lock-topology"))
                self.assertEqual(
                    1,
                    deps.events.count("seal-publication-state"),
                )
                self.assertIn(
                    "lock_state=committed",
                    deps.stdout.getvalue().splitlines(),
                )
                self.assertIn(
                    "lock_tombstone_path=%s"
                    % deps.lock_tombstone_path,
                    deps.stdout.getvalue().splitlines(),
                )
                release_index = deps.events.index("release-lock")
                self.assertNotIn(
                    "restore-lock",
                    deps.events[release_index:],
                )
                self.assertNotIn(
                    "invalidate-seal",
                    deps.events[release_index:],
                )
                self.assertNotIn(
                    "invalidate-evidence",
                    deps.events[release_index:],
                )

    def test_poststate_records_probe_count_route_state_and_lock_absence(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.states[0]["route48"]["consecutive_failures"] = 1
        deps.states[0]["route48"]["failure_window_start"] = 100
        deps.states[0]["route48"]["last_failure_at"] = 120
        deps.states[1]["route48"]["consecutive_failures"] = 2
        deps.states[1]["route48"]["failure_window_start"] = 100
        deps.states[1]["route48"]["last_failure_at"] = 140

        self.assertEqual(0, pricing.execute_operation(deps))

        summary = deps.json_artifacts["summary.json"]
        self.assertEqual(
            pricing.official_quote_fixture(),
            summary["official_quote_fixture"],
        )
        self.assertEqual(
            pricing.official_quote_fixture(),
            deps.json_artifacts["official-evidence.json"]["quote_fixture"],
        )
        self.assertEqual(0, summary["temporary_probe_token_count_before"])
        self.assertEqual(0, summary["temporary_probe_token_count_after"])
        self.assertEqual(deps.states, [])
        self.assertEqual(
            {
                "channel_id": 48,
                "channel_status": 1,
                "state": "active",
                "consecutive_failures": 1,
                "failure_window_start": 100,
                "last_failure_at": 120,
            },
            summary["route48_before"],
        )
        self.assertEqual(
            {
                "channel_id": 48,
                "channel_status": 1,
                "state": "active",
                "consecutive_failures": 2,
                "failure_window_start": 100,
                "last_failure_at": 140,
            },
            summary["route48_after"],
        )
        self.assertIs(summary["final_lock_absent"], True)
        release_index = deps.events.index("release-lock")
        self.assertEqual(release_index, len(deps.events) - 1)
        self.assertNotIn("lock-exists", deps.events)
        self.assertNotIn("lock-topology", deps.events)

    def test_known_not_committed_outcome_also_validates_poststate(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.patch_result = (503, {}, b"upstream failure")
        deps.snapshots[1] = copy.deepcopy(deps.snapshots[0])
        deps.states[1]["plan"]["temporary_probe_token_count"] = 1

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertIn("state-after", deps.events)
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertFalse(deps.lock_held)

    def test_required_artifact_failure_retains_active_lock(self) -> None:
        deps = FakeDependencies()
        deps.fail_artifact = "official-evidence.json"
        deps.fail_checksum_validation = True

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertTrue(deps.lock_held)
        self.assertNotIn("release-lock", deps.events)
        self.assertNotIn("patch", deps.events)
        self.assertEqual(
            "retained_evidence_failure",
            deps.json_artifacts["summary.json"]["lock_state"],
        )

    def test_committed_pricing_after_failure_runs_canary_and_poststate_but_retains_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.fail_artifact = "pricing-after.json"

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(1, deps.events.count("canary"))
        self.assertEqual(1, deps.events.count("state-after"))
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        summary = deps.json_artifacts["summary.json"]
        self.assertEqual("committed", summary["write_outcome"])
        self.assertIs(summary["pricing_readback_valid"], True)
        self.assertIs(summary["poststate_valid"], True)
        self.assertIs(summary["evidence_complete"], False)
        self.assertIs(summary["sealed"], False)
        self.assertIs(summary["lock_retained"], True)
        self.assertIs(summary["final_lock_absent"], False)

    def test_committed_artifact_baseexception_defers_until_after_mandatory_checks(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.fail_artifact = "pricing-after.json"
        deps.fail_artifact_error = KeyboardInterrupt("artifact interrupted")

        with self.assertRaisesRegex(
            KeyboardInterrupt,
            "artifact interrupted",
        ):
            pricing.execute_operation(deps)

        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(1, deps.events.count("canary"))
        self.assertEqual(1, deps.events.count("state-after"))
        self.assertNotIn("release-lock", deps.events)
        self.assertTrue(deps.lock_held)
        summary = deps.json_artifacts["summary.json"]
        self.assertEqual("committed", summary["write_outcome"])
        self.assertIs(summary["pricing_readback_valid"], True)
        self.assertIs(summary["poststate_valid"], True)
        self.assertIs(summary["evidence_complete"], False)
        self.assertIs(summary["sealed"], False)
        self.assertIs(summary["lock_retained"], True)
        self.assertIs(summary["final_lock_absent"], False)

    def test_committed_poststate_artifact_failure_does_not_skip_validation(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.fail_artifact = "runtime-after.txt"

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("canary"))
        self.assertEqual(1, deps.events.count("state-after"))
        self.assertIn("write-text:plan-after.txt", deps.events)
        self.assertNotIn("release-lock", deps.events)
        summary = deps.json_artifacts["summary.json"]
        self.assertIs(summary["poststate_valid"], True)
        self.assertIs(summary["evidence_complete"], False)
        self.assertIs(summary["sealed"], False)

    def test_success_validates_exact_required_artifact_set(self) -> None:
        deps = FakeDependencies()

        self.assertEqual(0, pricing.execute_operation(deps))

        expected = {
            "SHA256SUMS",
            "SHA256SUMS.pre",
            "canary.json",
            "docker-compose.yml",
            "new-api.dump",
            "newapi-sub2api-sync-extension-v0.1.5.zip",
            "official-evidence.json",
            "pg-restore-list.txt",
            "plan-after.txt",
            "plan-before.txt",
            "pricing-after.json",
            "pricing-before.json",
            "pricing-conflict.json",
            "pricing-preview.json",
            "pricing-write-error.json",
            "runtime-after.txt",
            "runtime-before.txt",
            "summary.json",
        }
        self.assertGreaterEqual(len(deps.expected_artifact_sets), 1)
        self.assertEqual(expected, deps.expected_artifact_sets[-1])
        self.assertFalse(
            any(
                ".pricing-change.lock.released" in name
                for name in deps.expected_artifact_sets[-1]
            )
        )

    def test_checksum_validation_failure_retains_committed_lock(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.fail_checksum_validation = True

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertTrue(deps.lock_held)
        self.assertEqual(1, deps.events.count("canary"))
        self.assertIn("validate-checksums:SHA256SUMS", deps.events)
        self.assertNotIn("release-lock", deps.events)
        summary = deps.json_artifacts["summary.json"]
        self.assertIs(summary["evidence_complete"], False)
        self.assertIs(summary["sealed"], False)
        self.assertIs(summary["final_lock_absent"], False)

    def test_canary_failure_is_not_retried_or_rolled_back_and_is_redacted(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.canary_result = (
            503,
            {"X-Oneapi-Request-Id": "request-failed"},
            json.dumps(
                {
                    "error": {
                        "type": "upstream_error",
                        "message": (
                            "Authorization: Bearer %s; key=%s"
                            % (ROOT_SECRET, TOKEN_SECRET)
                        ),
                    }
                }
            ).encode(),
        )

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(1, deps.events.count("canary"))
        self.assertEqual(
            1,
            deps.events.count("request-log:request-failed"),
        )
        self.assertEqual(
            pricing.CHANNEL_ID,
            deps.json_artifacts["canary.json"]["channel_id"],
        )
        self.assertEqual(
            pricing.CHANNEL_ID,
            deps.json_artifacts["summary.json"]["canary_channel"],
        )
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertEqual(
            "committed",
            deps.json_artifacts["summary.json"]["write_outcome"],
        )
        self.assertEqual(
            503,
            deps.json_artifacts["canary.json"]["http_status"],
        )
        serialized = json.dumps(
            deps.json_artifacts,
            sort_keys=True,
        ) + deps.stdout.getvalue()
        self.assertNotIn(ROOT_SECRET, serialized)
        self.assertNotIn(TOKEN_SECRET, serialized)

    def test_opaque_credentials_are_redacted_from_every_output(self) -> None:
        deps = FakeDependencies()
        opaque_secrets = (
            "opaque-secret",
            "opaque-cookie",
            "opaque-password",
            "opaque-set-cookie",
            "opaque-passwd",
            "opaque-token",
            "opaque-access-token",
            "opaque-dash-key",
            "opaque-underscore-key",
            "opaque-secret-field",
            "opaque-bearer",
        )
        deps.canary_error = RuntimeError(
            "{'Authorization': 'opaque-secret'} and "
            "Cookie: session=opaque-cookie and "
            "password='opaque-password' and "
            "'Set-Cookie'=\"session=opaque-set-cookie\" and "
            "PASSWD=opaque-passwd and "
            "ToKeN:'opaque-token' and "
            "access_token = \"opaque-access-token\" and "
            "api-key=opaque-dash-key and "
            "API_KEY:'opaque-underscore-key' and "
            "secret=opaque-secret-field and "
            "bEaReR opaque-bearer"
        )

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        all_output = "\n".join(
            (
                json.dumps(deps.json_artifacts, sort_keys=True),
                json.dumps(deps.text_artifacts, sort_keys=True),
                deps.stdout.getvalue(),
            )
        )
        for secret in opaque_secrets:
            with self.subTest(secret=secret):
                self.assertNotIn(secret, all_output)

    def test_basic_authorization_and_complete_cookie_are_redacted(self) -> None:
        deps = FakeDependencies()
        deps.canary_error = RuntimeError(
            "Authorization: Basic opaque-basic-secret; "
            "Cookie: sid=one; prefs=opaque-two"
        )

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        all_output = "\n".join(
            (
                json.dumps(deps.json_artifacts, sort_keys=True),
                json.dumps(deps.text_artifacts, sort_keys=True),
                deps.stdout.getvalue(),
            )
        )
        for secret in (
            "opaque-basic-secret",
            "sid=one",
            "prefs=opaque-two",
        ):
            with self.subTest(secret=secret):
                self.assertNotIn(secret, all_output)

    def test_canary_transport_failure_is_recorded_once_before_poststate(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.canary_error = ConnectionError(
            "Authorization: Bearer " + TOKEN_SECRET
        )

        exit_code = pricing.execute_operation(deps)

        self.assertEqual(1, exit_code)
        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(1, deps.events.count("canary"))
        self.assertEqual(1, deps.events.count("state-after"))
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertEqual(
            "failed",
            deps.json_artifacts["canary.json"]["status"],
        )
        self.assertNotIn(
            TOKEN_SECRET,
            json.dumps(deps.json_artifacts, sort_keys=True),
        )

    def test_canary_preserves_response_error_when_log_lookup_fails(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.canary_result = (
            403,
            {"X-Oneapi-Request-Id": "request-restricted"},
            json.dumps(
                {
                    "error": {
                        "type": "channel_error",
                        "code": "channel_price_restricted",
                        "message": "channel pricing restriction",
                    }
                }
            ).encode(),
        )
        deps.log_error = RuntimeError(
            "lookup failed with token=" + TOKEN_SECRET
        )

        self.assertEqual(1, pricing.execute_operation(deps))

        self.assertEqual(
            1,
            deps.events.count("request-log:request-restricted"),
        )
        canary = deps.json_artifacts["canary.json"]
        self.assertIn("response_error", canary)
        self.assertIn("log_lookup_error", canary)
        self.assertEqual(
            {
                "http_status": 403,
                "type": "channel_error",
                "code": "channel_price_restricted",
                "message": "channel pricing restriction",
            },
            canary["response_error"],
        )
        self.assertEqual(
            "RuntimeError",
            canary["log_lookup_error"]["class"],
        )
        self.assertNotIn(
            TOKEN_SECRET,
            canary["log_lookup_error"]["message"],
        )

    def test_canary_log_lookup_interrupt_preserves_response_evidence(
        self,
    ) -> None:
        for signal in (
            KeyboardInterrupt("request log interrupted"),
            SystemExit(29),
        ):
            with self.subTest(signal=type(signal).__name__):
                deps = FakeDependencies()
                deps.log_error = signal

                with self.assertRaises(type(signal)) as raised:
                    pricing.execute_operation(deps)

                self.assertIs(signal, raised.exception)
                self.assertEqual(1, deps.events.count("canary"))
                self.assertEqual(
                    1,
                    deps.events.count("request-log:request-123"),
                )
                self.assertEqual(1, deps.events.count("state-after"))
                self.assertEqual(1, deps.events.count("release-lock"))
                canary = deps.json_artifacts["canary.json"]
                self.assertEqual("unknown_after_response", canary["status"])
                self.assertIs(canary["outcome_known"], False)
                self.assertEqual(200, canary["http_status"])
                self.assertEqual("request-123", canary["request_id"])
                self.assertEqual(
                    "assistant_ok",
                    canary["body_classification"],
                )
                self.assertIsNone(canary["channel_id"])
                self.assertEqual(
                    "log_lookup_interruption",
                    canary["error"]["kind"],
                )
                summary = deps.json_artifacts["summary.json"]
                self.assertEqual(
                    "unknown_after_response",
                    summary["canary_status"],
                )
                self.assertIs(summary["canary_outcome_known"], False)
                self.assertIs(summary["poststate_valid"], True)
                self.assertIs(summary["evidence_complete"], True)
                self.assertIs(summary["sealed"], True)

    def test_keyboard_interrupt_during_canary_records_unknown_and_releases(
        self,
    ) -> None:
        deps = FakeDependencies()
        deps.canary_error = KeyboardInterrupt(
            "Authorization: Bearer " + TOKEN_SECRET
        )

        with self.assertRaises(KeyboardInterrupt):
            pricing.execute_operation(deps)

        self.assertEqual(1, deps.events.count("patch"))
        self.assertEqual(1, deps.events.count("canary"))
        self.assertEqual(1, deps.events.count("state-after"))
        self.assertNotIn("request-log:request-123", deps.events)
        self.assertEqual(1, deps.events.count("release-lock"))
        self.assertFalse(deps.lock_held)
        canary = deps.json_artifacts["canary.json"]
        self.assertEqual("unknown_after_attempt", canary["status"])
        self.assertIs(canary["attempted"], True)
        self.assertIs(canary["outcome_known"], False)
        self.assertEqual(125, canary["duration_ms"])
        self.assertEqual("KeyboardInterrupt", canary["error"]["class"])
        self.assertNotIn(TOKEN_SECRET, canary["error"]["message"])
        summary = deps.json_artifacts["summary.json"]
        self.assertEqual("committed", summary["write_outcome"])
        self.assertEqual(
            "unknown_after_attempt",
            summary["canary_status"],
        )
        self.assertIs(summary["canary_attempted"], True)
        self.assertIs(summary["canary_outcome_known"], False)
        self.assertEqual(125, summary["canary_duration_ms"])
        self.assertIs(summary["final_lock_absent"], True)
        self.assertNotIn("canary_status=not_run", deps.stdout.getvalue())

    def test_unrelated_option_maps_are_compared_semantically(self) -> None:
        before = {
            key: json.dumps(value, indent=2, sort_keys=False)
            for key, value in option_maps(target=False).items()
        }
        reordered = option_maps(target=True)
        reordered["CompletionRatio"] = {
            "unrelated-model": 2,
            "another-model": 3,
        }
        after = {
            key: json.dumps(value, separators=(",", ":"), sort_keys=True)
            for key, value in reordered.items()
        }

        before_hashes, after_hashes = pricing.validate_option_transition(
            before,
            after,
        )

        self.assertEqual(set(OPTION_KEYS), set(before_hashes))
        self.assertEqual(set(OPTION_KEYS), set(after_hashes))
        changed = copy.deepcopy(after)
        changed["ModelRatio"] = json.dumps({"unrelated-model": 2.0})
        with self.assertRaisesRegex(
            RuntimeError,
            "unrelated pricing option changed",
        ):
            pricing.validate_option_transition(before, changed)

    def test_artifacts_contain_only_safe_pricing_evidence(self) -> None:
        deps = FakeDependencies()

        self.assertEqual(0, pricing.execute_operation(deps))

        self.assertEqual(
            {
                "official-evidence.json",
                "pricing-before.json",
                "pricing-preview.json",
                "pricing-write-error.json",
                "pricing-conflict.json",
                "pricing-after.json",
                "canary.json",
                "summary.json",
            },
            set(deps.json_artifacts),
        )
        self.assertEqual(
            {},
            deps.json_artifacts["pricing-before.json"]["target_entry"][
                "configured"
            ],
        )
        self.assertNotIn(
            "options",
            deps.json_artifacts["pricing-before.json"],
        )
        self.assertNotIn(
            "options",
            deps.json_artifacts["pricing-after.json"],
        )
        self.assertEqual(
            {
                "runtime-before.txt",
                "plan-before.txt",
                "runtime-after.txt",
                "plan-after.txt",
            },
            set(deps.text_artifacts),
        )
        self.assertEqual(
            [
                "output_dir=/data1/newapi-study/backups/opus55-test",
                "write_outcome=committed",
                "canary_status=passed channel=48",
                (
                    "lock_tombstone_path="
                    "/opt/newapi-study/.pricing-change.lock.released.test"
                ),
            ],
            deps.stdout.getvalue().splitlines(),
        )


class ProductionDependenciesTest(unittest.TestCase):
    def args(self, output_dir: pathlib.Path) -> types.SimpleNamespace:
        return types.SimpleNamespace(
            output_dir=str(output_dir),
            expected_revision=EXPECTED_REVISION,
            existing_token_user_id=20,
            existing_token_id=139,
        )

    def write_protocol_artifacts(self, deps: object) -> set[str]:
        required = set(pricing.REQUIRED_ARTIFACTS)
        for name in sorted(required - {"SHA256SUMS", "summary.json"}):
            if not pricing.os.path.lexists(deps.output_dir / name):
                deps.write_text(name, "durable:%s\n" % name)
        return required

    def final_summary(
        self,
        deps: object,
        tombstone: pathlib.Path,
        *,
        run_id: str = "b" * 32,
        seal_id: str = "c" * 32,
    ) -> dict[str, object]:
        return {
            "model": pricing.MODEL,
            "expected_revision": EXPECTED_REVISION,
            "output_dir": str(deps.output_dir),
            "write_outcome": "committed",
            "evidence_complete": True,
            "sealed": True,
            "final_lock_absent": True,
            "lock_state": "absent",
            "lock_tombstone_path": str(tombstone),
            "run_id": run_id,
            "seal_id": seal_id,
            "seal_marker_path": str(tombstone / "seal.json"),
            "required_artifacts": sorted(pricing.REQUIRED_ARTIFACTS),
        }

    def update_final_owner(
        self,
        deps: object,
        *,
        run_id: str = "b" * 32,
        seal_id: str = "c" * 32,
    ) -> dict[str, object]:
        owner = {
            "model": pricing.MODEL,
            "expected_revision": EXPECTED_REVISION,
            "output_dir": str(deps.output_dir),
            "run_id": run_id,
            "state": "pending_release",
            "captured_pricing_version": pricing.EMPTY_VERSION,
            "seal_id": seal_id,
        }
        deps.update_lock_owner(owner)
        return owner

    def write_sealed_pair(
        self,
        deps: object,
        tombstone: pathlib.Path,
        *,
        run_id: str = "b" * 32,
        seal_id: str = "c" * 32,
    ) -> tuple[dict[str, object], set[str]]:
        summary, required = self.write_active_seal_pair(
            deps,
            tombstone,
            run_id=run_id,
            seal_id=seal_id,
        )
        deps.publish_seal(tombstone)
        deps.release_lock(tombstone)
        return summary, required

    def write_active_seal_pair(
        self,
        deps: object,
        tombstone: pathlib.Path,
        *,
        run_id: str = "b" * 32,
        seal_id: str = "c" * 32,
    ) -> tuple[dict[str, object], set[str]]:
        deps.create_output_dir()
        deps.acquire_lock(
            {
                "model": pricing.MODEL,
                "expected_revision": EXPECTED_REVISION,
                "output_dir": str(deps.output_dir),
                "run_id": run_id,
                "state": "snapshot_captured",
                "captured_pricing_version": pricing.EMPTY_VERSION,
            }
        )
        required = self.write_protocol_artifacts(deps)
        deps.write_json(
            "summary.json",
            {"sealed": False, "final_lock_absent": False},
        )
        deps.write_checksums("SHA256SUMS")
        summary = self.final_summary(
            deps,
            tombstone,
            run_id=run_id,
            seal_id=seal_id,
        )
        self.update_final_owner(deps, run_id=run_id, seal_id=seal_id)
        deps.finalize_evidence(summary, required)
        return summary, required

    def rewrite_sealed_evidence(
        self,
        deps: object,
        tombstone: pathlib.Path,
    ) -> None:
        summary_path = deps.output_dir / "summary.json"
        summary_raw = summary_path.read_bytes()
        summary = json.loads(summary_raw)
        required = set(summary["required_artifacts"])
        manifest_path = deps.output_dir / "SHA256SUMS"
        manifest_path.write_text(
            "".join(
                "%s  %s\n"
                % (
                    hashlib.sha256(
                        (deps.output_dir / name).read_bytes()
                    ).hexdigest(),
                    name,
                )
                for name in sorted(required - {"SHA256SUMS"})
            ),
            encoding="ascii",
        )
        summary_path.chmod(0o600)
        manifest_path.chmod(0o600)
        marker_path = tombstone / "seal.json"
        marker = json.loads(marker_path.read_text(encoding="utf-8"))
        marker["summary_sha256"] = hashlib.sha256(summary_raw).hexdigest()
        marker["manifest_sha256"] = hashlib.sha256(
            manifest_path.read_bytes()
        ).hexdigest()
        marker_path.write_text(
            json.dumps(marker, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        marker_path.chmod(0o600)

    def assert_failed_owner_write_cleans_lock(
        self,
        failure_mode: str,
        error: BaseException,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            owner = deps.pricing_lock_path / "owner"
            real_open = pricing.os.open
            real_fsync = pricing.os.fsync
            real_chmod = pricing.os.chmod
            owner_descriptors: list[int] = []
            parent_fsyncs = 0
            owner_fsync_failed = False

            def tracked_open(path, *args, **kwargs):
                descriptor = real_open(path, *args, **kwargs)
                if pathlib.Path(path) == owner:
                    owner_descriptors.append(descriptor)
                return descriptor

            def injected_fsync(descriptor):
                nonlocal parent_fsyncs, owner_fsync_failed
                descriptor_stat = pricing.os.fstat(descriptor)
                root_stat = root.stat()
                if (
                    descriptor_stat.st_dev == root_stat.st_dev
                    and descriptor_stat.st_ino == root_stat.st_ino
                ):
                    parent_fsyncs += 1
                if (
                    failure_mode == "fsync"
                    and stat.S_ISREG(descriptor_stat.st_mode)
                    and not owner_fsync_failed
                ):
                    owner_fsync_failed = True
                    raise error
                return real_fsync(descriptor)

            def injected_chmod(path, mode):
                if failure_mode == "chmod" and pathlib.Path(path) == owner:
                    raise error
                return real_chmod(path, mode)

            with contextlib.ExitStack() as stack:
                stack.enter_context(
                    mock.patch.object(
                        pricing.os,
                        "open",
                        side_effect=tracked_open,
                    )
                )
                stack.enter_context(
                    mock.patch.object(
                        pricing.os,
                        "fsync",
                        side_effect=injected_fsync,
                    )
                )
                stack.enter_context(
                    mock.patch.object(
                        pricing.os,
                        "chmod",
                        side_effect=injected_chmod,
                    )
                )
                if failure_mode == "write":
                    stack.enter_context(
                        mock.patch.object(
                            pricing.os,
                            "write",
                            side_effect=error,
                        )
                    )
                with self.assertRaises(type(error)) as raised:
                    deps.acquire_lock({"model": pricing.MODEL})

            self.assertIs(error, raised.exception)
            self.assertEqual(1, len(owner_descriptors))
            for descriptor in owner_descriptors:
                with self.assertRaises(OSError):
                    pricing.os.fstat(descriptor)
            self.assertFalse(pricing.os.path.lexists(owner))
            self.assertFalse(
                pricing.os.path.lexists(deps.pricing_lock_path)
            )
            self.assertGreaterEqual(parent_fsyncs, 1)

    def test_acquire_lock_cleans_owner_after_write_failure(self) -> None:
        self.assert_failed_owner_write_cleans_lock(
            "write",
            OSError("owner write failed"),
        )

    def test_acquire_lock_cleans_owner_after_fsync_failure(self) -> None:
        self.assert_failed_owner_write_cleans_lock(
            "fsync",
            OSError("owner fsync failed"),
        )

    def test_acquire_lock_cleans_owner_after_chmod_failure(self) -> None:
        self.assert_failed_owner_write_cleans_lock(
            "chmod",
            OSError("owner chmod failed"),
        )

    def test_acquire_lock_cleans_owner_after_baseexception(self) -> None:
        self.assert_failed_owner_write_cleans_lock(
            "write",
            KeyboardInterrupt("owner write interrupted"),
        )

    def test_acquire_lock_preserves_cleanup_failures_and_topology(
        self,
    ) -> None:
        cases = (
            ("unlink_owner", OSError("owner write failed")),
            ("rmdir_lock", OSError("owner write failed")),
            ("fsync_root", KeyboardInterrupt("owner write interrupted")),
        )
        for cleanup_stage, acquire_error in cases:
            with self.subTest(cleanup_stage=cleanup_stage):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "run"),
                        root=root,
                        backup_root=backups,
                    )
                    owner = deps.pricing_lock_path / "owner"
                    cleanup_error = OSError(
                        "cleanup failed at " + cleanup_stage
                    )
                    real_unlink = pathlib.Path.unlink
                    real_rmdir = pathlib.Path.rmdir
                    real_fsync_directory = deps._fsync_directory
                    root_fsync_count = 0

                    def injected_unlink(path, *args, **kwargs):
                        if (
                            cleanup_stage == "unlink_owner"
                            and pathlib.Path(path) == owner
                        ):
                            raise cleanup_error
                        return real_unlink(path, *args, **kwargs)

                    def injected_rmdir(path, *args, **kwargs):
                        if (
                            cleanup_stage == "rmdir_lock"
                            and pathlib.Path(path)
                            == deps.pricing_lock_path
                        ):
                            raise cleanup_error
                        return real_rmdir(path, *args, **kwargs)

                    def injected_fsync_directory(path: pathlib.Path) -> None:
                        nonlocal root_fsync_count
                        if pathlib.Path(path) == deps.root:
                            root_fsync_count += 1
                            if (
                                cleanup_stage == "fsync_root"
                                and root_fsync_count == 2
                            ):
                                raise cleanup_error
                        real_fsync_directory(path)

                    with (
                        mock.patch.object(
                            pricing.os,
                            "write",
                            side_effect=acquire_error,
                        ),
                        mock.patch.object(
                            pathlib.Path,
                            "unlink",
                            autospec=True,
                            side_effect=injected_unlink,
                        ),
                        mock.patch.object(
                            pathlib.Path,
                            "rmdir",
                            autospec=True,
                            side_effect=injected_rmdir,
                        ),
                        mock.patch.object(
                            deps,
                            "_fsync_directory",
                            side_effect=injected_fsync_directory,
                        ),
                    ):
                        caught: BaseException | None = None
                        try:
                            deps.acquire_lock({"model": pricing.MODEL})
                        except BaseException as failure:
                            caught = failure
                        else:
                            self.fail("acquire_lock unexpectedly succeeded")

                    self.assertIsNotNone(caught)
                    assert caught is not None
                    self.assertIs(
                        acquire_error,
                        getattr(caught, "acquire_error", None),
                    )
                    cleanup_errors = dict(
                        getattr(caught, "cleanup_errors", ())
                    )
                    self.assertIs(
                        cleanup_error,
                        cleanup_errors.get(cleanup_stage),
                    )
                    active_lock_exists = pricing.os.path.lexists(
                        deps.pricing_lock_path
                    )
                    owner_exists = pricing.os.path.lexists(owner)
                    self.assertIs(
                        active_lock_exists,
                        getattr(caught, "active_lock_exists", None),
                    )
                    self.assertIs(
                        owner_exists,
                        getattr(caught, "owner_exists", None),
                    )

    def test_prepare_lock_commit_allocates_unique_expected_identity(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.acquire_lock({"model": pricing.MODEL})

            first_path, first_seal = deps.prepare_lock_commit()
            second_path, second_seal = deps.prepare_lock_commit()

            self.assertNotEqual(first_path, second_path)
            self.assertNotEqual(first_seal, second_seal)
            for tombstone, seal_id in (
                (first_path, first_seal),
                (second_path, second_seal),
            ):
                self.assertEqual(deps.root, tombstone.parent)
                self.assertRegex(
                    tombstone.name,
                    r"^\.pricing-change\.lock\.released\.\d+\.[0-9a-f]{32}$",
                )
                self.assertRegex(seal_id, r"^[0-9a-f]{32}$")
                self.assertFalse(pricing.os.path.lexists(tombstone))

    def test_active_lock_blocks_competing_acquire_until_commit_rename(
        self,
    ) -> None:
        for pause in (
            "marker_write",
            "marker_file_fsync",
            "active_dir_fsync",
            "rename",
        ):
            with self.subTest(pause=pause):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "run"),
                        root=root,
                        backup_root=backups,
                    )
                    tombstone = deps.root / (
                        ".pricing-change.lock.released.424242.%s"
                        % ("a" * 32)
                    )
                    self.write_active_seal_pair(deps, tombstone)
                    contender = pricing.ProductionDependencies(
                        self.args(backups / ("next-" + pause)),
                        root=root,
                        backup_root=backups,
                    )
                    paused = False

                    def assert_competing_acquire_blocked() -> None:
                        with self.assertRaises(FileExistsError):
                            contender.acquire_lock({"model": pricing.MODEL})
                        self.assertTrue(deps.pricing_lock_path.is_dir())
                        self.assertFalse(
                            pricing.os.path.lexists(tombstone)
                        )

                    real_write = pricing.os.write
                    real_fsync = pricing.os.fsync

                    def pause_write(descriptor: int, payload: bytes) -> int:
                        nonlocal paused
                        descriptor_stat = pricing.os.fstat(descriptor)
                        marker = deps.pricing_lock_path / "seal.json"
                        marker_stat = marker.stat()
                        is_marker = (
                            descriptor_stat.st_dev == marker_stat.st_dev
                            and descriptor_stat.st_ino == marker_stat.st_ino
                        )
                        if not paused and pause == "marker_write" and is_marker:
                            paused = True
                            assert_competing_acquire_blocked()
                        return real_write(descriptor, payload)

                    def pause_fsync(descriptor: int) -> None:
                        nonlocal paused
                        descriptor_stat = pricing.os.fstat(descriptor)
                        marker = deps.pricing_lock_path / "seal.json"
                        is_marker = (
                            marker.exists()
                            and descriptor_stat.st_dev == marker.stat().st_dev
                            and descriptor_stat.st_ino == marker.stat().st_ino
                        )
                        active_stat = deps.pricing_lock_path.stat()
                        is_active_dir = (
                            descriptor_stat.st_dev == active_stat.st_dev
                            and descriptor_stat.st_ino == active_stat.st_ino
                        )
                        if (
                            not paused
                            and (
                                (
                                    pause == "marker_file_fsync"
                                    and is_marker
                                )
                                or (
                                    pause == "active_dir_fsync"
                                    and is_active_dir
                                )
                            )
                        ):
                            paused = True
                            assert_competing_acquire_blocked()
                        real_fsync(descriptor)

                    with (
                        mock.patch.object(
                            pricing.os,
                            "write",
                            side_effect=pause_write,
                        ),
                        mock.patch.object(
                            pricing.os,
                            "fsync",
                            side_effect=pause_fsync,
                        ),
                    ):
                        deps.publish_seal(tombstone)

                    with self.assertRaisesRegex(
                        RuntimeError,
                        "lock topology",
                    ):
                        deps.validate_checksums("SHA256SUMS")

                    real_rename = (
                        pricing.ProductionDependencies._rename_noreplace
                    )

                    def pause_rename(source, destination) -> None:
                        nonlocal paused
                        if pause == "rename":
                            paused = True
                            assert_competing_acquire_blocked()
                        real_rename(source, destination)

                    with mock.patch.object(
                        pricing.ProductionDependencies,
                        "_rename_noreplace",
                        side_effect=pause_rename,
                    ):
                        deps.release_lock(tombstone)

                    self.assertTrue(paused)
                    contender.acquire_lock({"model": pricing.MODEL})
                    self.assertTrue(contender.pricing_lock_path.is_dir())
                    self.assertTrue((tombstone / "seal.json").is_file())

    def test_parent_fsync_failure_never_rolls_back_committed_tombstone(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            deps.publish_seal(tombstone)
            real_rename = pricing.ProductionDependencies._rename_noreplace
            rename_calls: list[tuple[pathlib.Path, pathlib.Path]] = []

            def tracked_rename(source, destination) -> None:
                rename_calls.append(
                    (pathlib.Path(source), pathlib.Path(destination))
                )
                real_rename(source, destination)

            with (
                mock.patch.object(
                    pricing.ProductionDependencies,
                    "_rename_noreplace",
                    side_effect=tracked_rename,
                ),
                mock.patch.object(
                    deps,
                    "_fsync_directory",
                    side_effect=OSError("commit durability unknown"),
                ),
                self.assertRaisesRegex(
                    pricing.LockCommitDurabilityError,
                    "durability unknown",
                ),
            ):
                deps.release_lock(tombstone)

            self.assertEqual(
                [(deps.pricing_lock_path, tombstone)],
                rename_calls,
            )
            self.assertFalse(
                pricing.os.path.lexists(deps.pricing_lock_path)
            )
            self.assertTrue(tombstone.is_dir())
            self.assertTrue((tombstone / "seal.json").is_file())
            deps.validate_checksums("SHA256SUMS")
            next_run = pricing.ProductionDependencies(
                self.args(backups / "next-run"),
                root=root,
                backup_root=backups,
            )
            next_run.acquire_lock({"model": pricing.MODEL})

    def test_crash_around_commit_rename_has_only_fail_safe_outcomes(
        self,
    ) -> None:
        for rename_persisted in (False, True):
            with self.subTest(rename_persisted=rename_persisted):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "run"),
                        root=root,
                        backup_root=backups,
                    )
                    tombstone = deps.root / (
                        ".pricing-change.lock.released.424242.%s"
                        % ("a" * 32)
                    )
                    self.write_active_seal_pair(deps, tombstone)
                    deps.publish_seal(tombstone)
                    if rename_persisted:
                        pricing.os.rename(
                            deps.pricing_lock_path,
                            tombstone,
                        )

                    next_run = pricing.ProductionDependencies(
                        self.args(backups / "next-run"),
                        root=root,
                        backup_root=backups,
                    )
                    if rename_persisted:
                        next_run.acquire_lock({"model": pricing.MODEL})
                        self.assertTrue(tombstone.is_dir())
                        self.assertTrue(
                            (tombstone / "seal.json").is_file()
                        )
                    else:
                        with self.assertRaises(FileExistsError):
                            next_run.acquire_lock({"model": pricing.MODEL})
                        self.assertTrue(
                            deps.pricing_lock_path.is_dir()
                        )
                        self.assertTrue(
                            (
                                deps.pricing_lock_path / "seal.json"
                            ).is_file()
                        )

    def test_active_seal_marker_is_created_no_clobber(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            marker = deps.pricing_lock_path / "seal.json"
            competing_marker = b'{"owner":"competing-run"}\n'
            marker.write_bytes(competing_marker)
            marker.chmod(0o600)

            with self.assertRaises(FileExistsError):
                deps.publish_seal(tombstone)

            self.assertEqual(competing_marker, marker.read_bytes())
            self.assertTrue(deps.pricing_lock_path.is_dir())
            self.assertFalse(pricing.os.path.lexists(tombstone))

    def test_unsealed_orphan_tombstone_blocks_lock_acquisition(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "next-run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            tombstone.mkdir(mode=0o700)
            owner = tombstone / "owner"
            owner.write_text('{"model":"claude-opus-5-5"}\n', encoding="utf-8")
            owner.chmod(0o600)

            with self.assertRaisesRegex(RuntimeError, "unsealed tombstone"):
                deps.acquire_lock({"model": pricing.MODEL})

            self.assertFalse(pricing.os.path.lexists(deps.pricing_lock_path))

    def test_tombstone_created_during_acquire_blocks_and_cleans_active(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "next-run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            real_mkdir = pricing.os.mkdir
            injected = False

            def racing_mkdir(path, mode=0o777, *, dir_fd=None):
                nonlocal injected
                if pathlib.Path(path) == deps.pricing_lock_path and not injected:
                    injected = True
                    real_mkdir(tombstone, 0o700)
                if dir_fd is None:
                    return real_mkdir(path, mode)
                return real_mkdir(path, mode, dir_fd=dir_fd)

            with (
                mock.patch.object(
                    pricing.os,
                    "mkdir",
                    side_effect=racing_mkdir,
                ),
                self.assertRaisesRegex(RuntimeError, "unsealed tombstone"),
            ):
                deps.acquire_lock({"model": pricing.MODEL})

            self.assertTrue(tombstone.is_dir())
            self.assertFalse(pricing.os.path.lexists(deps.pricing_lock_path))

    def test_malformed_and_non_directory_tombstones_block_acquisition(
        self,
    ) -> None:
        cases = ("malformed_marker", "regular_file", "symlink")
        for case in cases:
            with self.subTest(case=case):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "next-run"),
                        root=root,
                        backup_root=backups,
                    )
                    tombstone = deps.root / (
                        ".pricing-change.lock.released.424242.%s"
                        % ("a" * 32)
                    )
                    if case == "malformed_marker":
                        tombstone.mkdir(mode=0o700)
                        owner = tombstone / "owner"
                        owner.write_text("{}\n", encoding="utf-8")
                        owner.chmod(0o600)
                        marker = tombstone / "seal.json"
                        marker.write_text("not-json\n", encoding="utf-8")
                        marker.chmod(0o600)
                    elif case == "regular_file":
                        tombstone.write_text("not-a-directory\n", encoding="utf-8")
                    else:
                        target = deps.root / "unrelated-directory"
                        target.mkdir()
                        tombstone.symlink_to(target, target_is_directory=True)

                    with self.assertRaisesRegex(RuntimeError, "tombstone"):
                        deps.acquire_lock({"model": pricing.MODEL})

                    self.assertFalse(
                        pricing.os.path.lexists(deps.pricing_lock_path)
                    )

    def test_verified_sealed_history_allows_lock_acquisition(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            history = pricing.ProductionDependencies(
                self.args(backups / "history-run"),
                root=root,
                backup_root=backups,
            )
            tombstone = history.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_sealed_pair(history, tombstone)
            next_run = pricing.ProductionDependencies(
                self.args(backups / "next-run"),
                root=root,
                backup_root=backups,
            )

            next_run.acquire_lock({"model": pricing.MODEL})

            self.assertTrue(next_run.pricing_lock_path.is_dir())
            self.assertTrue((tombstone / "seal.json").is_file())

    def test_manifest_symlink_history_blocks_lock_acquisition(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            history = pricing.ProductionDependencies(
                self.args(backups / "history-run"),
                root=root,
                backup_root=backups,
            )
            tombstone = history.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_sealed_pair(history, tombstone)
            manifest = history.output_dir / "SHA256SUMS"
            manifest_copy = backups / "history-manifest-copy"
            manifest_copy.write_bytes(manifest.read_bytes())
            manifest.unlink()
            manifest.symlink_to(manifest_copy)
            next_run = pricing.ProductionDependencies(
                self.args(backups / "next-run"),
                root=root,
                backup_root=backups,
            )

            with self.assertRaisesRegex(
                RuntimeError,
                "tombstone is unsealed or malformed",
            ):
                next_run.acquire_lock({"model": pricing.MODEL})

            self.assertFalse(
                pricing.os.path.lexists(next_run.pricing_lock_path)
            )

    def test_malformed_manifest_history_blocks_lock_acquisition(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            history = pricing.ProductionDependencies(
                self.args(backups / "history-run"),
                root=root,
                backup_root=backups,
            )
            tombstone = history.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_sealed_pair(history, tombstone)
            (history.output_dir / "SHA256SUMS").write_text(
                "malformed manifest\n",
                encoding="ascii",
            )
            next_run = pricing.ProductionDependencies(
                self.args(backups / "next-run"),
                root=root,
                backup_root=backups,
            )

            with self.assertRaisesRegex(
                RuntimeError,
                "tombstone is unsealed or malformed",
            ):
                next_run.acquire_lock({"model": pricing.MODEL})

            self.assertFalse(
                pricing.os.path.lexists(next_run.pricing_lock_path)
            )

    def test_sealed_history_requires_complete_protocol_artifact_set(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            history = pricing.ProductionDependencies(
                self.args(backups / "history-run"),
                root=root,
                backup_root=backups,
            )
            tombstone = history.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_sealed_pair(history, tombstone)
            omitted = "new-api.dump"
            (history.output_dir / omitted).unlink()
            summary_path = history.output_dir / "summary.json"
            summary = json.loads(summary_path.read_text(encoding="utf-8"))
            summary["required_artifacts"].remove(omitted)
            summary_path.write_text(
                json.dumps(summary, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            self.rewrite_sealed_evidence(history, tombstone)
            next_run = pricing.ProductionDependencies(
                self.args(backups / "next-run"),
                root=root,
                backup_root=backups,
            )

            with self.assertRaisesRegex(
                RuntimeError,
                "artifact|tombstone",
            ):
                next_run.acquire_lock({"model": pricing.MODEL})

            self.assertFalse(
                pricing.os.path.lexists(next_run.pricing_lock_path)
            )

    def test_sealed_history_rejects_invalid_or_unbound_owner(self) -> None:
        for case in ("not_json", "wrong_model", "hash_mismatch"):
            with self.subTest(case=case):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    history = pricing.ProductionDependencies(
                        self.args(backups / "history-run"),
                        root=root,
                        backup_root=backups,
                    )
                    tombstone = history.root / (
                        ".pricing-change.lock.released.424242.%s"
                        % ("a" * 32)
                    )
                    self.write_sealed_pair(history, tombstone)
                    owner_path = tombstone / "owner"
                    if case == "not_json":
                        owner_path.write_text(
                            "not-json\n",
                            encoding="utf-8",
                        )
                    else:
                        owner = json.loads(
                            owner_path.read_text(encoding="utf-8")
                        )
                        if case == "wrong_model":
                            owner["model"] = "other-model"
                        else:
                            owner["pid"] += 1
                        owner_path.write_text(
                            json.dumps(owner, sort_keys=True) + "\n",
                            encoding="utf-8",
                        )
                    owner_path.chmod(0o600)
                    next_run = pricing.ProductionDependencies(
                        self.args(backups / ("next-" + case)),
                        root=root,
                        backup_root=backups,
                    )

                    with self.assertRaisesRegex(
                        RuntimeError,
                        "owner|tombstone",
                    ):
                        next_run.acquire_lock({"model": pricing.MODEL})

                    self.assertFalse(
                        pricing.os.path.lexists(next_run.pricing_lock_path)
                    )

    def test_non_protocol_tombstone_names_do_not_block_acquisition(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "next-run"),
                root=root,
                backup_root=backups,
            )
            (deps.root / ".pricing-change.lock.released.notes").write_text(
                "unrelated\n",
                encoding="utf-8",
            )
            (deps.root / ".pricing-change.lock.released.1.not-hex").mkdir()

            deps.acquire_lock({"model": pricing.MODEL})

            self.assertTrue(deps.pricing_lock_path.is_dir())

    def test_cli_requires_all_four_arguments_and_injects_them(self) -> None:
        with (
            contextlib.redirect_stderr(io.StringIO()),
            self.assertRaises(SystemExit),
        ):
            pricing.parse_args(
                [
                    "--output-dir",
                    "/data1/newapi-study/backups/test",
                    "--expected-revision",
                    EXPECTED_REVISION,
                    "--existing-token-user-id",
                    "20",
                ]
            )

        captured: dict[str, object] = {}
        fake = FakeDependencies()

        def factory(args, *, stdout):
            captured["args"] = args
            fake.output_dir = pathlib.Path(args.output_dir)
            fake.expected_revision = args.expected_revision
            fake.existing_token_user_id = args.existing_token_user_id
            fake.existing_token_id = args.existing_token_id
            fake.stdout = stdout
            return fake

        output = io.StringIO()
        exit_code = pricing.main(
            [
                "--output-dir",
                "/data1/newapi-study/backups/cli-test",
                "--expected-revision",
                EXPECTED_REVISION,
                "--existing-token-user-id",
                "20",
                "--existing-token-id",
                "139",
            ],
            deps_factory=factory,
            stdout=output,
        )

        self.assertEqual(0, exit_code)
        args = captured["args"]
        self.assertEqual(20, args.existing_token_user_id)
        self.assertEqual(139, args.existing_token_id)
        self.assertEqual(
            "/data1/newapi-study/backups/cli-test",
            args.output_dir,
        )

    def test_constructs_without_external_verification_helpers(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )

        self.assertEqual(root.resolve(), deps.root)
        source = SCRIPT.read_text(encoding="utf-8")
        self.assertNotIn("verify_model_alias_normalization", source)
        self.assertNotIn("model_alias_normalization", source)
        self.assertNotIn("importlib", source)
        self.assertFalse(hasattr(pricing, "LockReleaseRollbackError"))
        self.assertFalse(hasattr(pricing, "LockRestoreRollbackError"))
        for method in (
            "_restore_lock_from_tombstone",
            "restore_lock",
            "invalidate_seal",
            "lock_exists",
        ):
            with self.subTest(method=method):
                self.assertFalse(
                    hasattr(pricing.ProductionDependencies, method)
                )

    def test_psql_uses_fixed_argv_and_redacts_failures(self) -> None:
        calls: list[tuple[list[str], dict[str, object]]] = []

        def check_output(command, **kwargs):
            calls.append((command, kwargs))
            return "  result row  \n"

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                check_output=check_output,
            )

            self.assertEqual("result row", deps._psql("SELECT 1"))

        self.assertEqual(
            [
                (
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
                        "SELECT 1",
                    ],
                    {"text": True, "timeout": 120},
                )
            ],
            calls,
        )

        def fail_check_output(command, **kwargs):
            del kwargs
            raise subprocess.CalledProcessError(
                1,
                command,
                output="token=" + ROOT_SECRET,
            )

        deps._check_output = fail_check_output
        with self.assertRaisesRegex(
            RuntimeError,
            "^production database query failed$",
        ) as raised:
            deps._psql("SELECT access_token FROM users")
        self.assertNotIn(ROOT_SECRET, str(raised.exception))

    def test_root_headers_queries_token_only_in_memory(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            with mock.patch.object(
                deps,
                "_psql",
                return_value=ROOT_SECRET,
            ) as psql:
                headers = deps.root_headers()

        psql.assert_called_once_with(
            "SELECT access_token FROM users WHERE id=1 AND status=1"
        )
        self.assertEqual(
            {
                "Authorization": "Bearer " + ROOT_SECRET,
                "New-Api-User": "1",
                "Content-Type": "application/json",
                "Accept-Encoding": "identity",
            },
            headers,
        )

    def test_http_request_connects_to_local_log_proxy(self) -> None:
        connection_args: list[tuple[object, ...]] = []

        class Response:
            status = 200

            def read(self):
                return b"{}"

            def getheader(self, name):
                del name
                return None

            def getheaders(self):
                return []

        class Connection:
            def __init__(self, host, port, timeout):
                connection_args.append((host, port, timeout))

            def request(self, method, path, body=None, headers=None):
                del method, path, body, headers

            def getresponse(self):
                return Response()

            def close(self):
                pass

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                http_connection=Connection,
            )

            deps.request(
                "GET",
                pricing.PRICING_SNAPSHOT_PATH,
                {},
                timeout=17,
            )

        self.assertEqual(
            [("127.0.0.1", 13000, 17)],
            connection_args,
        )

    def test_http_request_json_gzip_and_close(self) -> None:
        calls: list[tuple[object, ...]] = []
        response_body = json.dumps({"success": True}).encode()

        class Response:
            status = 202

            def read(self):
                calls.append(("read",))
                return gzip.compress(response_body)

            def getheader(self, name):
                test_case.assertEqual("Content-Encoding", name)
                return "GZip"

            def getheaders(self):
                return [
                    ("Content-Encoding", "GZip"),
                    ("X-Oneapi-Request-Id", "request-gzip"),
                ]

        test_case = self

        class Connection:
            def __init__(self, host, port, timeout):
                calls.append(("connect", host, port, timeout))

            def request(self, method, path, body=None, headers=None):
                calls.append(("request", method, path, body, headers))

            def getresponse(self):
                calls.append(("getresponse",))
                return Response()

            def close(self):
                calls.append(("close",))

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                http_connection=Connection,
            )

            result = deps.request(
                "POST",
                pricing.PREVIEW_PATH,
                {"Authorization": "Bearer " + ROOT_SECRET},
                {"model": pricing.MODEL},
                timeout=17,
            )

        self.assertEqual(
            (
                202,
                {
                    "Content-Encoding": "GZip",
                    "X-Oneapi-Request-Id": "request-gzip",
                },
                response_body,
            ),
            result,
        )
        self.assertEqual(
            ("connect", "127.0.0.1", 13000, 17),
            calls[0],
        )
        self.assertEqual(
            (
                "request",
                "POST",
                pricing.PREVIEW_PATH,
                json.dumps(
                    {"model": pricing.MODEL},
                    ensure_ascii=False,
                ).encode(),
                {"Authorization": "Bearer " + ROOT_SECRET},
            ),
            calls[1],
        )
        self.assertEqual(("read",), calls[3])
        self.assertEqual(("close",), calls[-1])

    def test_http_request_error_is_redacted_and_connection_is_closed(
        self,
    ) -> None:
        events: list[str] = []

        class Connection:
            def __init__(self, host, port, timeout):
                del host, port, timeout

            def request(self, method, path, body=None, headers=None):
                del method, path, body, headers
                raise OSError("Authorization: Bearer " + ROOT_SECRET)

            def close(self):
                events.append("close")

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                http_connection=Connection,
            )

            with self.assertRaisesRegex(
                RuntimeError,
                "^local API request failed$",
            ) as raised:
                deps.request(
                    "GET",
                    pricing.PRICING_SNAPSHOT_PATH,
                    {"Authorization": "Bearer " + ROOT_SECRET},
                )

        self.assertEqual(["close"], events)
        self.assertNotIn(ROOT_SECRET, str(raised.exception))

    def test_http_request_exception_priority_matrix(self) -> None:
        class FatalAdapterError(BaseException):
            pass

        cases = (
            (
                "request_error_close_keyboard_interrupt",
                "request",
                OSError("request failed"),
                KeyboardInterrupt("close interrupted"),
                "close",
            ),
            (
                "request_error_close_system_exit",
                "request",
                OSError("request failed"),
                SystemExit(21),
                "close",
            ),
            (
                "read_error_close_keyboard_interrupt",
                "read",
                OSError("read failed"),
                KeyboardInterrupt("close interrupted"),
                "close",
            ),
            (
                "read_error_close_system_exit",
                "read",
                OSError("read failed"),
                SystemExit(22),
                "close",
            ),
            (
                "request_keyboard_interrupt_close_error",
                "request",
                KeyboardInterrupt("request interrupted"),
                OSError("close failed"),
                "primary",
            ),
            (
                "read_system_exit_close_error",
                "read",
                SystemExit(23),
                OSError("close failed"),
                "primary",
            ),
            (
                "request_generator_exit_close_error",
                "request",
                GeneratorExit("request interrupted"),
                OSError("close failed"),
                "primary",
            ),
            (
                "read_custom_baseexception_close_generator_exit",
                "read",
                FatalAdapterError("read interrupted"),
                GeneratorExit("close interrupted"),
                "primary",
            ),
            (
                "request_error_close_generator_exit",
                "request",
                OSError("request failed"),
                GeneratorExit("close interrupted"),
                "close",
            ),
            (
                "read_error_close_custom_baseexception",
                "read",
                OSError("read failed"),
                FatalAdapterError("close interrupted"),
                "close",
            ),
            (
                "request_error_close_error",
                "request",
                OSError("request failed"),
                OSError("close failed"),
                "primary_with_close_cause",
            ),
            (
                "read_error_close_error",
                "read",
                OSError("read failed"),
                OSError("close failed"),
                "primary_with_close_cause",
            ),
            (
                "close_error_only",
                None,
                None,
                OSError("close failed"),
                "close",
            ),
            (
                "close_keyboard_interrupt_only",
                None,
                None,
                KeyboardInterrupt("close interrupted"),
                "close",
            ),
            (
                "close_system_exit_only",
                None,
                None,
                SystemExit(24),
                "close",
            ),
        )

        for name, stage, primary, close_error, expected in cases:
            with self.subTest(name=name):
                events: list[str] = []

                class Response:
                    status = 200

                    def getheaders(self):
                        return []

                    def getheader(self, header_name):
                        del header_name
                        return ""

                    def read(self):
                        if stage == "read":
                            raise primary
                        return b"{}"

                class Connection:
                    def __init__(self, host, port, timeout):
                        del host, port, timeout

                    def request(
                        self,
                        method,
                        path,
                        body=None,
                        headers=None,
                    ):
                        del method, path, body, headers
                        if stage == "request":
                            raise primary

                    def getresponse(self):
                        return Response()

                    def close(self):
                        events.append("close")
                        raise close_error

                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "run"),
                        root=root,
                        backup_root=backups,
                        http_connection=Connection,
                    )

                    if expected == "primary_with_close_cause":
                        raised_error: BaseException | None = None
                        traceback_names: list[str] = []
                        try:
                            deps.request(
                                "GET",
                                pricing.PRICING_SNAPSHOT_PATH,
                                {},
                            )
                        except BaseException as exc:
                            raised_error = exc
                            traceback = exc.__traceback__
                            while traceback is not None:
                                traceback_names.append(
                                    traceback.tb_frame.f_code.co_name
                                )
                                traceback = traceback.tb_next
                        self.assertIs(primary, raised_error)
                        self.assertIs(
                            close_error,
                            raised_error.__cause__,
                        )
                        self.assertIn(stage, traceback_names)
                    else:
                        expected_error = (
                            primary if expected == "primary" else close_error
                        )
                        with self.assertRaises(
                            type(expected_error)
                        ) as raised:
                            deps.request(
                                "GET",
                                pricing.PRICING_SNAPSHOT_PATH,
                                {},
                            )
                        self.assertIs(expected_error, raised.exception)
                        if expected == "primary":
                            self.assertIsNone(raised.exception.__cause__)

                self.assertEqual(["close"], events)

    def test_http_request_invalid_gzip_is_safe_after_connection_close(
        self,
    ) -> None:
        events: list[str] = []

        class Response:
            status = 200

            def read(self):
                events.append("read")
                return b"not-gzip"

            def getheader(self, name):
                del name
                return "gzip"

            def getheaders(self):
                return [("Content-Encoding", "gzip")]

        class Connection:
            def __init__(self, host, port, timeout):
                del host, port, timeout

            def request(self, method, path, body=None, headers=None):
                del method, path, body, headers

            def getresponse(self):
                return Response()

            def close(self):
                events.append("close")

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                http_connection=Connection,
            )

            with self.assertRaisesRegex(
                RuntimeError,
                "^local API response decompression failed$",
            ):
                deps.request(
                    "GET",
                    pricing.PRICING_SNAPSHOT_PATH,
                    {},
                )

        self.assertEqual(["read", "close"], events)

    def test_seal_probe_does_not_swallow_process_signals(self) -> None:
        for signal in (
            KeyboardInterrupt("seal validation interrupted"),
            SystemExit(37),
        ):
            with self.subTest(signal=type(signal).__name__):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "run"),
                        root=root,
                        backup_root=backups,
                    )
                    tombstone = deps.root / (
                        ".pricing-change.lock.released.424242.%s"
                        % ("a" * 32)
                    )
                    tombstone.mkdir()
                    (tombstone / pricing.SEAL_MARKER_NAME).write_text(
                        "{}\n",
                        encoding="utf-8",
                    )

                    with (
                        mock.patch.object(
                            deps,
                            "_validate_checksums",
                            side_effect=signal,
                        ),
                        self.assertRaises(type(signal)) as raised,
                    ):
                        deps.seal_publication_state(tombstone)

                self.assertIs(signal, raised.exception)

    def test_output_directory_and_lock_are_created_atomically_with_modes(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )

            deps.acquire_lock(
                {
                    "model": pricing.MODEL,
                    "expected_revision": EXPECTED_REVISION,
                    "output_dir": str(backups / "run"),
                    "state": "pending_snapshot",
                }
            )
            deps.create_output_dir()

            lock = root / ".pricing-change.lock"
            owner = lock / "owner"
            self.assertTrue(lock.is_dir())
            self.assertEqual(0o700, stat.S_IMODE(lock.stat().st_mode))
            self.assertEqual(0o600, stat.S_IMODE(owner.stat().st_mode))
            self.assertEqual(
                0o700,
                stat.S_IMODE((backups / "run").stat().st_mode),
            )
            self.assertFalse((lock / "owner.json").exists())
            self.assertNotIn(
                ROOT_SECRET,
                owner.read_text(encoding="utf-8"),
            )
            pending_owner = json.loads(owner.read_text(encoding="utf-8"))
            self.assertEqual("pending_snapshot", pending_owner["state"])
            self.assertNotIn("captured_pricing_version", pending_owner)
            deps.update_lock_owner(
                {
                    "model": pricing.MODEL,
                    "expected_revision": EXPECTED_REVISION,
                    "output_dir": str(backups / "run"),
                    "state": "snapshot_captured",
                    "captured_pricing_version": pricing.EMPTY_VERSION,
                }
            )
            captured_owner = json.loads(owner.read_text(encoding="utf-8"))
            self.assertEqual(
                pricing.EMPTY_VERSION,
                captured_owner["captured_pricing_version"],
            )
            self.assertEqual("snapshot_captured", captured_owner["state"])
            self.assertEqual({owner}, set(lock.iterdir()))
            self.assertEqual(0o600, stat.S_IMODE(owner.stat().st_mode))
            with self.assertRaises(FileExistsError):
                deps.acquire_lock({"model": pricing.MODEL})
            first_tombstone, seal_id = deps.prepare_lock_commit()
            required = self.write_protocol_artifacts(deps)
            deps.write_json(
                "summary.json",
                {"sealed": False, "final_lock_absent": False},
            )
            deps.write_checksums("SHA256SUMS")
            final_summary = self.final_summary(
                deps,
                first_tombstone,
                seal_id=seal_id,
            )
            final_owner = self.update_final_owner(
                deps,
                seal_id=seal_id,
            )
            deps.finalize_evidence(final_summary, required)
            deps.publish_seal(first_tombstone)
            deps.release_lock(first_tombstone)
            self.assertFalse(lock.exists())
            self.assertEqual(deps.root, first_tombstone.parent)
            self.assertRegex(
                first_tombstone.name,
                r"^\.pricing-change\.lock\.released\.[0-9]+\.[0-9a-f]{32}$",
            )
            self.assertTrue(first_tombstone.is_dir())
            released_owner = first_tombstone / "owner"
            self.assertEqual(
                {**final_owner, "pid": pricing.os.getpid()},
                json.loads(released_owner.read_text(encoding="utf-8")),
            )
            self.assertEqual(
                0o600,
                stat.S_IMODE(released_owner.stat().st_mode),
            )

            deps.acquire_lock(
                {
                    "model": pricing.MODEL,
                    "expected_revision": EXPECTED_REVISION,
                    "output_dir": str(backups / "run"),
                    "state": "snapshot_captured",
                    "captured_pricing_version": pricing.EMPTY_VERSION,
                }
            )
            second_tombstone, _ = deps.prepare_lock_commit()
            self.assertNotEqual(first_tombstone, second_tombstone)
            self.assertTrue(first_tombstone.is_dir())
            self.assertFalse(pricing.os.path.lexists(second_tombstone))
            self.assertTrue(deps.pricing_lock_path.is_dir())

            existing = backups / "existing"
            existing.mkdir()
            existing_deps = pricing.ProductionDependencies(
                self.args(existing),
                root=root,
                backup_root=backups,
            )
            with self.assertRaises(FileExistsError):
                existing_deps.create_output_dir()
            with self.assertRaisesRegex(ValueError, "under"):
                pricing.ProductionDependencies(
                    self.args(pathlib.Path(directory) / "outside"),
                    root=root,
                    backup_root=backups,
                )

    def test_release_rename_failure_preserves_active_lock_and_owner(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            deps.publish_seal(tombstone)
            lock = root / ".pricing-change.lock"
            owner = lock / "owner"
            owner_before = owner.read_bytes()

            def fail_rename(source, destination):
                self.assertEqual(deps.pricing_lock_path, source)
                self.assertEqual(deps.root, destination.parent)
                self.assertTrue(owner.is_file())
                self.assertEqual(owner_before, owner.read_bytes())
                raise OSError("rename failed")

            with (
                mock.patch.object(
                    pricing.ProductionDependencies,
                    "_rename_noreplace",
                    side_effect=fail_rename,
                ),
                self.assertRaisesRegex(OSError, "rename failed"),
            ):
                deps.release_lock(tombstone)

            self.assertTrue(lock.is_dir())
            self.assertEqual(owner_before, owner.read_bytes())
            self.assertEqual({lock}, set(root.iterdir()))

    def test_commit_does_not_replace_preexisting_empty_tombstone(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            deps.publish_seal(tombstone)
            active_owner = (
                deps.pricing_lock_path / "owner"
            ).read_bytes()
            tombstone.mkdir(mode=0o700)

            with self.assertRaises(FileExistsError):
                deps.release_lock(tombstone)

            self.assertEqual(set(), set(tombstone.iterdir()))
            self.assertEqual(
                active_owner,
                (deps.pricing_lock_path / "owner").read_bytes(),
            )

    def test_commit_preserves_competing_tombstone_owner(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            deps.publish_seal(tombstone)
            active_owner = (
                deps.pricing_lock_path / "owner"
            ).read_bytes()
            tombstone.mkdir(mode=0o700)
            concurrent_owner = tombstone / "owner"
            concurrent_owner.write_bytes(b"concurrent-owner\n")

            with self.assertRaises(FileExistsError):
                deps.release_lock(tombstone)

            self.assertEqual(
                b"concurrent-owner\n",
                concurrent_owner.read_bytes(),
            )
            self.assertEqual(
                active_owner,
                (deps.pricing_lock_path / "owner").read_bytes(),
            )

    def test_atomic_noreplace_rename_preserves_both_directory_inodes(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            active = root / "active"
            tombstone = root / "tombstone"
            active.mkdir()
            tombstone.mkdir()
            (active / "owner").write_bytes(b"active-owner\n")
            active_inode = active.stat().st_ino
            tombstone_inode = tombstone.stat().st_ino

            with self.assertRaises(FileExistsError):
                pricing.ProductionDependencies._rename_noreplace(
                    active,
                    tombstone,
                )

            self.assertEqual(active_inode, active.stat().st_ino)
            self.assertEqual(tombstone_inode, tombstone.stat().st_ino)
            self.assertEqual(
                b"active-owner\n",
                (active / "owner").read_bytes(),
            )
            self.assertEqual(set(), set(tombstone.iterdir()))

    def test_lock_topology_requires_legal_tombstone_directory(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone_name = (
                "%s.released.%d.%s"
                % (
                    deps.pricing_lock_path.name,
                    pricing.os.getpid(),
                    "a" * 32,
                )
            )
            outside_tombstone = backups / tombstone_name
            outside_tombstone.mkdir()
            with self.assertRaisesRegex(RuntimeError, "path"):
                deps.lock_topology(outside_tombstone)

            tombstone = deps.root / tombstone_name
            tombstone.write_text("not a directory", encoding="utf-8")
            with self.assertRaisesRegex(RuntimeError, "directory"):
                deps.lock_topology(tombstone)

            tombstone.unlink()
            tombstone.mkdir()
            self.assertEqual((False, True), deps.lock_topology(tombstone))

    def test_release_parent_fsync_failure_exposes_committed_tombstone(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            deps.publish_seal(tombstone)
            failure = OSError("release parent fsync failed")

            with (
                mock.patch.object(
                    deps,
                    "_fsync_directory",
                    side_effect=failure,
                ),
                self.assertRaisesRegex(
                    pricing.LockCommitDurabilityError,
                    "release parent fsync failed",
                ) as raised,
            ):
                deps.release_lock(tombstone)

            self.assertIs(failure, raised.exception.durability_error)
            self.assertEqual(tombstone, raised.exception.lock_tombstone_path)
            self.assertFalse(
                pricing.os.path.lexists(deps.pricing_lock_path)
            )
            self.assertTrue(tombstone.is_dir())
            deps.validate_checksums("SHA256SUMS")

    def test_release_parent_fsync_baseexception_never_rolls_back(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            deps.publish_seal(tombstone)
            signal = KeyboardInterrupt("release parent fsync interrupted")

            with (
                mock.patch.object(
                    deps,
                    "_fsync_directory",
                    side_effect=signal,
                ),
                self.assertRaisesRegex(
                    pricing.LockCommitDurabilityError,
                    "release parent fsync interrupted",
                ) as raised,
            ):
                deps.release_lock(tombstone)

            self.assertIs(signal, raised.exception.durability_error)
            self.assertFalse(
                pricing.os.path.lexists(deps.pricing_lock_path)
            )
            self.assertTrue(tombstone.is_dir())
            deps.validate_checksums("SHA256SUMS")

    def test_atomic_json_rewrite_preserves_previous_summary_on_failure(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.create_output_dir()
            deps.write_json(
                "summary.json",
                {"lock_state": "held_pending_release"},
            )

            with (
                mock.patch.object(
                    pricing.os,
                    "replace",
                    side_effect=OSError("replace failed"),
                ),
                self.assertRaisesRegex(OSError, "replace failed"),
            ):
                deps.write_json(
                    "summary.json",
                    {"lock_state": "absent"},
                )

            summary = json.loads(
                (backups / "run/summary.json").read_text(encoding="utf-8")
            )
            self.assertEqual("held_pending_release", summary["lock_state"])
            self.assertEqual(
                {"summary.json"},
                {path.name for path in (backups / "run").iterdir()},
            )

    def test_staged_finalization_publishes_valid_success_summary_last(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.acquire_lock(
                {
                    "model": pricing.MODEL,
                    "expected_revision": EXPECTED_REVISION,
                    "output_dir": str(deps.output_dir),
                    "run_id": "b" * 32,
                    "state": "snapshot_captured",
                    "captured_pricing_version": pricing.EMPTY_VERSION,
                }
            )
            deps.create_output_dir()
            required = self.write_protocol_artifacts(deps)
            deps.write_json(
                "summary.json",
                {"sealed": False, "final_lock_absent": False},
            )
            deps.write_checksums("SHA256SUMS")
            deps.invalidate_evidence()
            self.assertFalse((deps.output_dir / "SHA256SUMS").exists())
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            final_summary = self.final_summary(deps, tombstone)
            self.update_final_owner(deps)
            events: list[str] = []
            real_fsync = pricing.os.fsync
            real_replace = pricing.os.replace

            def tracked_fsync(descriptor):
                descriptor_stat = pricing.os.fstat(descriptor)
                output_stat = deps.output_dir.stat()
                if (
                    descriptor_stat.st_dev == output_stat.st_dev
                    and descriptor_stat.st_ino == output_stat.st_ino
                ):
                    events.append("fsync-dir")
                return real_fsync(descriptor)

            def tracked_replace(source, destination):
                destination_name = pathlib.Path(destination).name
                if destination_name in {"SHA256SUMS", "summary.json"}:
                    events.append("replace:" + destination_name)
                return real_replace(source, destination)

            with (
                mock.patch.object(
                    pricing.os,
                    "fsync",
                    side_effect=tracked_fsync,
                ),
                mock.patch.object(
                    pricing.os,
                    "replace",
                    side_effect=tracked_replace,
                ),
            ):
                deps.finalize_evidence(final_summary, required)

            self.assertEqual(
                [
                    "replace:summary.json",
                    "fsync-dir",
                    "replace:SHA256SUMS",
                    "fsync-dir",
                ],
                events[-4:],
            )
            self.assertEqual(
                final_summary,
                json.loads(
                    (deps.output_dir / "summary.json").read_text(
                        encoding="utf-8"
                    )
                ),
            )
            deps.validate_artifact_set(required)
            with self.assertRaisesRegex(RuntimeError, "seal marker"):
                deps._validate_checksums(
                    "SHA256SUMS",
                    require_seal=True,
                    expected_tombstone=tombstone,
                    publication_pending=True,
                )
            deps.publish_seal(tombstone)
            deps.release_lock(tombstone)
            deps.validate_checksums("SHA256SUMS")

    def test_sealed_pair_without_durable_marker_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)

            with self.assertRaisesRegex(RuntimeError, "seal marker"):
                deps._validate_checksums(
                    "SHA256SUMS",
                    require_seal=True,
                    expected_tombstone=tombstone,
                    publication_pending=True,
                )

    def test_require_seal_rejects_missing_unsealed_or_incomplete_summary(
        self,
    ) -> None:
        for case in ("missing", "unsealed", "missing_expected_revision"):
            with self.subTest(case=case):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "run"),
                        root=root,
                        backup_root=backups,
                    )
                    tombstone = deps.root / (
                        ".pricing-change.lock.released.424242.%s"
                        % ("a" * 32)
                    )
                    if case == "missing":
                        deps.create_output_dir()
                        deps.write_text("artifact.txt", "durable\n")
                        deps.write_checksums("SHA256SUMS")
                        with self.assertRaisesRegex(
                            RuntimeError,
                            "summary",
                        ):
                            deps._validate_checksums(
                                "SHA256SUMS",
                                require_seal=True,
                                expected_tombstone=tombstone,
                            )
                        continue
                    if case == "unsealed":
                        deps.create_output_dir()
                        deps.write_json(
                            "summary.json",
                            {"sealed": False},
                        )
                        deps.write_checksums("SHA256SUMS")
                        with self.assertRaisesRegex(
                            RuntimeError,
                            "sealed|summary",
                        ):
                            deps._validate_checksums(
                                "SHA256SUMS",
                                require_seal=True,
                                expected_tombstone=tombstone,
                            )
                        continue

                    self.write_sealed_pair(deps, tombstone)
                    summary_path = deps.output_dir / "summary.json"
                    summary = json.loads(
                        summary_path.read_text(encoding="utf-8")
                    )
                    del summary["expected_revision"]
                    summary_path.write_text(
                        json.dumps(summary, sort_keys=True) + "\n",
                        encoding="utf-8",
                    )
                    self.rewrite_sealed_evidence(deps, tombstone)
                    with self.assertRaisesRegex(
                        RuntimeError,
                        "summary",
                    ):
                        deps.validate_checksums("SHA256SUMS")

    def test_durable_marker_with_mismatched_seal_id_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_sealed_pair(deps, tombstone)
            marker = tombstone / "seal.json"
            payload = json.loads(marker.read_text(encoding="utf-8"))
            payload["seal_id"] = "d" * 32
            marker.write_text(json.dumps(payload) + "\n", encoding="utf-8")

            with self.assertRaisesRegex(RuntimeError, "seal marker binding"):
                deps.validate_checksums("SHA256SUMS")

    def test_durable_marker_with_mismatched_hash_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_sealed_pair(deps, tombstone)
            marker = tombstone / "seal.json"
            payload = json.loads(marker.read_text(encoding="utf-8"))
            payload["manifest_sha256"] = "d" * 64
            marker.write_text(json.dumps(payload) + "\n", encoding="utf-8")

            with self.assertRaisesRegex(RuntimeError, "seal marker binding"):
                deps.validate_checksums("SHA256SUMS")

    def test_successful_durable_marker_is_secret_safe_and_valid(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            summary, required = self.write_active_seal_pair(deps, tombstone)
            marker = deps.pricing_lock_path / "seal.json"
            fsync_events: list[str] = []
            real_fsync = pricing.os.fsync

            def tracked_fsync(descriptor: int) -> None:
                descriptor_stat = pricing.os.fstat(descriptor)
                active_stat = deps.pricing_lock_path.stat()
                if (
                    descriptor_stat.st_dev == active_stat.st_dev
                    and descriptor_stat.st_ino == active_stat.st_ino
                ):
                    fsync_events.append("active-dir")
                elif stat.S_ISREG(descriptor_stat.st_mode):
                    marker_payload = json.loads(
                        marker.read_text(encoding="utf-8")
                    )
                    fsync_events.append(
                        "marker-file:%s" % marker_payload.get("state")
                    )
                real_fsync(descriptor)

            with mock.patch.object(
                pricing.os,
                "fsync",
                side_effect=tracked_fsync,
            ):
                deps.publish_seal(tombstone)

            deps.release_lock(tombstone)
            marker = tombstone / "seal.json"
            payload = json.loads(marker.read_text(encoding="utf-8"))
            self.assertEqual(
                {
                    "manifest_sha256",
                    "model",
                    "expected_revision",
                    "owner_sha256",
                    "output_dir",
                    "protocol",
                    "run_id",
                    "seal_id",
                    "state",
                    "summary_sha256",
                    "tombstone_path",
                },
                set(payload),
            )
            self.assertEqual("committed", payload["state"])
            self.assertEqual(summary["run_id"], payload["run_id"])
            self.assertEqual(summary["seal_id"], payload["seal_id"])
            self.assertEqual(pricing.MODEL, payload["model"])
            self.assertEqual(
                EXPECTED_REVISION,
                payload["expected_revision"],
            )
            self.assertEqual(str(deps.output_dir), payload["output_dir"])
            self.assertEqual(str(tombstone), payload["tombstone_path"])
            self.assertEqual(
                hashlib.sha256(
                    (tombstone / "owner").read_bytes()
                ).hexdigest(),
                payload["owner_sha256"],
            )
            self.assertEqual(0o600, stat.S_IMODE(marker.stat().st_mode))
            self.assertEqual(0o700, stat.S_IMODE(tombstone.stat().st_mode))
            self.assertNotIn(ROOT_SECRET, marker.read_text(encoding="utf-8"))
            self.assertNotIn(TOKEN_SECRET, marker.read_text(encoding="utf-8"))
            self.assertNotIn(marker, set(deps.output_dir.iterdir()))
            self.assertEqual(
                [
                    "marker-file:committed",
                    "active-dir",
                ],
                fsync_events,
            )
            deps.validate_artifact_set(required)
            deps.validate_checksums("SHA256SUMS")

    def test_publish_seal_creates_final_marker_directly_and_exclusively(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            marker = deps.pricing_lock_path / "seal.json"
            marker_open_flags: list[int] = []
            real_open = pricing.os.open

            def track_marker_open(path, flags, mode=0o777, **kwargs):
                if pathlib.Path(path) == marker:
                    marker_open_flags.append(flags)
                return real_open(path, flags, mode, **kwargs)

            with (
                mock.patch.object(
                    pricing.os,
                    "open",
                    side_effect=track_marker_open,
                ),
                mock.patch.object(
                    pricing.os,
                    "link",
                    side_effect=AssertionError(
                        "seal publication must not use hard links"
                    ),
                ),
            ):
                deps.publish_seal(tombstone)

            self.assertEqual(1, len(marker_open_flags))
            flags = marker_open_flags[0]
            self.assertTrue(flags & pricing.os.O_CREAT)
            self.assertTrue(flags & pricing.os.O_EXCL)
            self.assertTrue(flags & pricing.os.O_WRONLY)
            self.assertTrue(
                flags & getattr(pricing.os, "O_NOFOLLOW", 0)
            )
            self.assertEqual(
                {"owner", "seal.json"},
                {
                    entry.name
                    for entry in deps.pricing_lock_path.iterdir()
                },
            )
            self.assertEqual(1, marker.stat().st_nlink)

    def test_seal_validation_rejects_nonexclusive_evidence_files(
        self,
    ) -> None:
        cases = (
            ("manifest_symlink", "SHA256SUMS"),
            ("manifest_hardlink", "SHA256SUMS"),
            ("summary_hardlink", "summary.json"),
            ("marker_hardlink", "seal.json"),
        )
        for case, filename in cases:
            with self.subTest(case=case):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "run"),
                        root=root,
                        backup_root=backups,
                    )
                    tombstone = deps.root / (
                        ".pricing-change.lock.released.424242.%s"
                        % ("a" * 32)
                    )
                    self.write_sealed_pair(deps, tombstone)
                    evidence = (
                        tombstone / filename
                        if filename == "seal.json"
                        else deps.output_dir / filename
                    )
                    extra_link = backups / ("%s-copy" % filename)
                    if case == "manifest_symlink":
                        extra_link.write_bytes(evidence.read_bytes())
                        evidence.unlink()
                        evidence.symlink_to(extra_link)
                    else:
                        pricing.os.link(evidence, extra_link)

                    with self.assertRaisesRegex(
                        RuntimeError,
                        "regular file|hard links|invalid",
                    ):
                        deps.validate_checksums("SHA256SUMS")

    def test_seal_validation_rejects_unmanifested_symlink(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_sealed_pair(deps, tombstone)
            (deps.output_dir / "unmanifested").symlink_to(
                deps.output_dir / "artifact.txt"
            )

            with self.assertRaisesRegex(RuntimeError, "missing entries"):
                deps.validate_checksums("SHA256SUMS")

    def test_publish_seal_race_preserves_competing_marker(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            marker = deps.pricing_lock_path / "seal.json"
            competing_marker = b'{"owner":"competing-run"}\n'
            real_open = pricing.os.open

            def publish_competitor_then_open(
                path,
                flags,
                mode=0o777,
                **kwargs,
            ):
                if pathlib.Path(path) == marker:
                    marker.write_bytes(competing_marker)
                    marker.chmod(0o600)
                return real_open(path, flags, mode, **kwargs)

            with (
                mock.patch.object(
                    pricing.os,
                    "open",
                    side_effect=publish_competitor_then_open,
                ),
                self.assertRaises(FileExistsError),
            ):
                deps.publish_seal(tombstone)

            self.assertEqual(competing_marker, marker.read_bytes())
            self.assertEqual(
                [],
                [
                    entry.name
                    for entry in deps.pricing_lock_path.iterdir()
                    if entry.name.startswith(".seal.json.")
                ],
            )

    def assert_failed_seal_publication_blocks_next_run(
        self,
        failure_stage: str,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            self.write_active_seal_pair(deps, tombstone)
            marker = deps.pricing_lock_path / "seal.json"
            real_write = pricing.os.write
            real_fsync = pricing.os.fsync
            failure = OSError("%s failed" % failure_stage)

            def fail_selected_write(
                descriptor: int,
                payload: bytes,
            ) -> int:
                descriptor_stat = pricing.os.fstat(descriptor)
                marker_stat = marker.stat()
                is_marker = (
                    descriptor_stat.st_dev == marker_stat.st_dev
                    and descriptor_stat.st_ino == marker_stat.st_ino
                )
                if failure_stage == "marker_write" and is_marker:
                    raise failure
                return real_write(descriptor, payload)

            def fail_selected_fsync(descriptor: int) -> None:
                descriptor_stat = pricing.os.fstat(descriptor)
                active_stat = deps.pricing_lock_path.stat()
                is_active = (
                    descriptor_stat.st_dev == active_stat.st_dev
                    and descriptor_stat.st_ino == active_stat.st_ino
                )
                if failure_stage == "active_directory_fsync" and is_active:
                    raise failure
                if stat.S_ISREG(descriptor_stat.st_mode):
                    marker_stat = marker.stat()
                    is_marker = (
                        descriptor_stat.st_dev == marker_stat.st_dev
                        and descriptor_stat.st_ino == marker_stat.st_ino
                    )
                    if failure_stage == "marker_file_fsync" and is_marker:
                        raise failure
                real_fsync(descriptor)

            with (
                mock.patch.object(
                    pricing.os,
                    "write",
                    side_effect=fail_selected_write,
                ),
                mock.patch.object(
                    pricing.os,
                    "fsync",
                    side_effect=fail_selected_fsync,
                ),
                self.assertRaisesRegex(OSError, failure_stage) as raised,
            ):
                deps.publish_seal(tombstone)

            self.assertIs(failure, raised.exception)
            self.assertTrue(deps.pricing_lock_path.is_dir())
            self.assertFalse(pricing.os.path.lexists(tombstone))
            self.assertTrue(pricing.os.path.lexists(marker))
            self.assertEqual(1, marker.stat().st_nlink)
            self.assertEqual(
                {"owner", "seal.json"},
                {
                    entry.name
                    for entry in deps.pricing_lock_path.iterdir()
                },
            )
            next_run = pricing.ProductionDependencies(
                self.args(backups / "next-run"),
                root=root,
                backup_root=backups,
            )
            with self.assertRaises(FileExistsError):
                next_run.acquire_lock({"model": pricing.MODEL})
            self.assertTrue(
                pricing.os.path.lexists(next_run.pricing_lock_path)
            )
            if failure_stage != "marker_write":
                marker_payload = json.loads(
                    marker.read_text(encoding="utf-8")
                )
                self.assertEqual("committed", marker_payload.get("state"))

    def test_marker_write_failure_blocks_next_run(self) -> None:
        self.assert_failed_seal_publication_blocks_next_run("marker_write")

    def test_marker_file_fsync_failure_blocks_next_run(self) -> None:
        self.assert_failed_seal_publication_blocks_next_run(
            "marker_file_fsync"
        )

    def test_active_directory_fsync_failure_blocks_next_run(self) -> None:
        self.assert_failed_seal_publication_blocks_next_run(
            "active_directory_fsync"
        )

    def test_staged_finalization_propagates_final_parent_fsync_failure(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.create_output_dir()
            required = self.write_protocol_artifacts(deps)
            deps.write_json(
                "summary.json",
                {"sealed": False, "final_lock_absent": False},
            )
            deps.write_checksums("SHA256SUMS")
            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            final_summary = self.final_summary(deps, tombstone)
            real_fsync = pricing.os.fsync
            real_replace = pricing.os.replace
            summary_replaced = False

            def tracked_replace(source, destination):
                nonlocal summary_replaced
                result = real_replace(source, destination)
                if pathlib.Path(destination).name == "summary.json":
                    summary_replaced = True
                return result

            def fail_final_parent_fsync(descriptor):
                descriptor_stat = pricing.os.fstat(descriptor)
                output_stat = deps.output_dir.stat()
                is_output_dir = (
                    descriptor_stat.st_dev == output_stat.st_dev
                    and descriptor_stat.st_ino == output_stat.st_ino
                )
                if summary_replaced and is_output_dir:
                    raise OSError("final summary parent fsync failed")
                return real_fsync(descriptor)

            with (
                mock.patch.object(
                    pricing.os,
                    "replace",
                    side_effect=tracked_replace,
                ),
                mock.patch.object(
                    pricing.os,
                    "fsync",
                    side_effect=fail_final_parent_fsync,
                ),
                self.assertRaisesRegex(
                    OSError,
                    "final summary parent fsync failed",
                ),
            ):
                deps.finalize_evidence(final_summary, required)

    def test_staged_finalization_failure_keeps_unsealed_summary(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.create_output_dir()
            required = self.write_protocol_artifacts(deps)
            held_summary = {
                "sealed": False,
                "final_lock_absent": False,
            }
            deps.write_json("summary.json", held_summary)
            deps.write_checksums("SHA256SUMS")
            real_replace = pricing.os.replace

            def fail_final_summary(source, destination):
                if pathlib.Path(destination).name == "summary.json":
                    raise OSError("final summary publish failed")
                return real_replace(source, destination)

            with (
                mock.patch.object(
                    pricing.os,
                    "replace",
                    side_effect=fail_final_summary,
                ),
                self.assertRaisesRegex(OSError, "publish failed"),
            ):
                deps.finalize_evidence(
                    self.final_summary(
                        deps,
                        deps.root
                        / (
                            ".pricing-change.lock.released.424242.%s"
                            % ("a" * 32)
                        ),
                    ),
                    required,
                )

            self.assertEqual(
                held_summary,
                json.loads(
                    (deps.output_dir / "summary.json").read_text(
                        encoding="utf-8"
                    )
                ),
            )

    def test_json_and_text_replaces_fsync_file_then_parent_directory(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.create_output_dir()
            events: list[str] = []
            real_fsync = pricing.os.fsync
            real_replace = pricing.os.replace

            def tracked_fsync(descriptor):
                descriptor_stat = pricing.os.fstat(descriptor)
                output_stat = deps.output_dir.stat()
                if (
                    descriptor_stat.st_dev == output_stat.st_dev
                    and descriptor_stat.st_ino == output_stat.st_ino
                ):
                    events.append("fsync-dir")
                elif stat.S_ISREG(descriptor_stat.st_mode):
                    events.append("fsync-file")
                return real_fsync(descriptor)

            def tracked_replace(source, destination):
                events.append("replace:" + pathlib.Path(destination).name)
                return real_replace(source, destination)

            with (
                mock.patch.object(
                    pricing.os,
                    "fsync",
                    side_effect=tracked_fsync,
                ),
                mock.patch.object(
                    pricing.os,
                    "replace",
                    side_effect=tracked_replace,
                ),
            ):
                deps.write_json("summary.json", {"sealed": False})
                deps.write_text("runtime.txt", "status=running\n")

            self.assertEqual(
                [
                    "fsync-file",
                    "replace:summary.json",
                    "fsync-dir",
                    "fsync-file",
                    "replace:runtime.txt",
                    "fsync-dir",
                ],
                events,
            )

    def test_lock_mkdir_and_release_rename_fsync_parent_in_order(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            events: list[str] = []
            real_mkdir = pricing.os.mkdir
            real_fsync = pricing.os.fsync
            real_rename = pricing.ProductionDependencies._rename_noreplace

            def tracked_mkdir(path, mode=0o777):
                if pathlib.Path(path) == deps.pricing_lock_path:
                    events.append("mkdir-lock")
                return real_mkdir(path, mode)

            def tracked_fsync(descriptor):
                descriptor_stat = pricing.os.fstat(descriptor)
                root_stat = root.stat()
                if (
                    descriptor_stat.st_dev == root_stat.st_dev
                    and descriptor_stat.st_ino == root_stat.st_ino
                ):
                    events.append("fsync-root")
                return real_fsync(descriptor)

            def tracked_rename(source, destination):
                events.append("rename-lock")
                return real_rename(source, destination)

            tombstone = deps.root / (
                ".pricing-change.lock.released.424242.%s" % ("a" * 32)
            )
            with (
                mock.patch.object(
                    pricing.os,
                    "mkdir",
                    side_effect=tracked_mkdir,
                ),
                mock.patch.object(
                    pricing.os,
                    "fsync",
                    side_effect=tracked_fsync,
                ),
                mock.patch.object(
                    pricing.ProductionDependencies,
                    "_rename_noreplace",
                    side_effect=tracked_rename,
                ),
            ):
                self.write_active_seal_pair(deps, tombstone)
                deps.publish_seal(tombstone)
                deps.release_lock(tombstone)

            mkdir_index = events.index("mkdir-lock")
            rename_index = events.index("rename-lock")
            root_fsync_indexes = [
                index
                for index, event in enumerate(events)
                if event == "fsync-root"
            ]
            self.assertTrue(
                any(mkdir_index < index < rename_index for index in root_fsync_indexes)
            )
            self.assertTrue(
                any(rename_index < index for index in root_fsync_indexes)
            )

    def test_secure_backup_copy_rejects_unsafe_sources_and_destinations(
        self,
    ) -> None:
        copy_cases = (
            (
                "compose",
                "copy_compose",
                "docker-compose.yml",
            ),
            (
                "extension",
                "copy_extension",
                "newapi-sub2api-sync-extension-v0.1.5.zip",
            ),
        )
        for label, method_name, destination_name in copy_cases:
            for unsafe_side in ("source", "destination"):
                for unsafe_kind in ("symlink", "hardlink"):
                    with self.subTest(
                        copy=label,
                        side=unsafe_side,
                        kind=unsafe_kind,
                    ):
                        with tempfile.TemporaryDirectory() as directory:
                            root = pathlib.Path(directory) / "root"
                            backups = pathlib.Path(directory) / "backups"
                            root.mkdir()
                            backups.mkdir()
                            deps = pricing.ProductionDependencies(
                                self.args(backups / "run"),
                                root=root,
                                backup_root=backups,
                            )
                            source = (
                                deps.compose_path
                                if label == "compose"
                                else deps.extension_path
                            )
                            source.parent.mkdir(
                                parents=True,
                                exist_ok=True,
                            )
                            source.write_bytes(b"trusted-source\n")
                            deps.create_output_dir()
                            destination = (
                                deps.output_dir / destination_name
                            )
                            external = backups / (
                                "%s-%s-%s"
                                % (label, unsafe_side, unsafe_kind)
                            )
                            if unsafe_side == "source":
                                if unsafe_kind == "symlink":
                                    source.unlink()
                                    external.write_bytes(
                                        b"external-source\n"
                                    )
                                    source.symlink_to(external)
                                else:
                                    pricing.os.link(source, external)
                            else:
                                external.write_bytes(
                                    b"external-destination\n"
                                )
                                if unsafe_kind == "symlink":
                                    destination.symlink_to(external)
                                else:
                                    pricing.os.link(
                                        external,
                                        destination,
                                    )
                            external_before = external.read_bytes()

                            with self.assertRaises(
                                (RuntimeError, FileExistsError)
                            ):
                                getattr(deps, method_name)()

                            self.assertEqual(
                                external_before,
                                external.read_bytes(),
                            )

    def test_owner_update_rejects_unsafe_existing_owner(self) -> None:
        for unsafe_kind in ("symlink", "directory", "hardlink"):
            with self.subTest(kind=unsafe_kind):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "run"),
                        root=root,
                        backup_root=backups,
                    )
                    deps.acquire_lock({"model": pricing.MODEL})
                    owner = deps.pricing_lock_path / "owner"
                    external = backups / "external-owner"
                    if unsafe_kind == "symlink":
                        owner.unlink()
                        external.write_bytes(b"external-owner\n")
                        owner.symlink_to(external)
                    elif unsafe_kind == "directory":
                        owner.unlink()
                        owner.mkdir()
                    else:
                        pricing.os.link(owner, external)
                    external_before = (
                        external.read_bytes()
                        if external.exists()
                        else None
                    )

                    with self.assertRaises((RuntimeError, OSError)):
                        deps.update_lock_owner(
                            {
                                "model": pricing.MODEL,
                                "state": "snapshot_captured",
                            }
                        )

                    if external_before is not None:
                        self.assertEqual(
                            external_before,
                            external.read_bytes(),
                        )

    def test_commit_rejects_unsafe_active_owner(self) -> None:
        for unsafe_kind in ("symlink", "directory", "hardlink"):
            with self.subTest(kind=unsafe_kind):
                with tempfile.TemporaryDirectory() as directory:
                    root = pathlib.Path(directory) / "root"
                    backups = pathlib.Path(directory) / "backups"
                    root.mkdir()
                    backups.mkdir()
                    deps = pricing.ProductionDependencies(
                        self.args(backups / "run"),
                        root=root,
                        backup_root=backups,
                    )
                    tombstone = deps.root / (
                        ".pricing-change.lock.released.424242.%s"
                        % ("a" * 32)
                    )
                    self.write_active_seal_pair(deps, tombstone)
                    deps.publish_seal(tombstone)
                    owner = deps.pricing_lock_path / "owner"
                    owner_before = owner.read_bytes()
                    external = backups / "external-owner"
                    if unsafe_kind == "symlink":
                        owner.unlink()
                        external.write_bytes(owner_before)
                        owner.symlink_to(external)
                    elif unsafe_kind == "directory":
                        owner.unlink()
                        owner.mkdir()
                    else:
                        pricing.os.link(owner, external)

                    with self.assertRaises(RuntimeError):
                        deps.release_lock(tombstone)

                    self.assertTrue(deps.pricing_lock_path.is_dir())
                    self.assertFalse(pricing.os.path.lexists(tombstone))
                    if external.exists():
                        self.assertEqual(
                            owner_before,
                            external.read_bytes(),
                        )

    def test_summary_and_manifest_writers_reject_unsafe_targets(
        self,
    ) -> None:
        for name in ("summary.json", "SHA256SUMS"):
            for unsafe_kind in ("symlink", "hardlink"):
                with self.subTest(name=name, kind=unsafe_kind):
                    with tempfile.TemporaryDirectory() as directory:
                        root = pathlib.Path(directory) / "root"
                        backups = pathlib.Path(directory) / "backups"
                        root.mkdir()
                        backups.mkdir()
                        deps = pricing.ProductionDependencies(
                            self.args(backups / "run"),
                            root=root,
                            backup_root=backups,
                        )
                        deps.create_output_dir()
                        deps.write_text("artifact.txt", "durable\n")
                        target = deps.output_dir / name
                        external = backups / (
                            "%s-%s" % (name, unsafe_kind)
                        )
                        external.write_bytes(b"external-evidence\n")
                        if unsafe_kind == "symlink":
                            target.symlink_to(external)
                        else:
                            pricing.os.link(external, target)
                        external_before = external.read_bytes()

                        with self.assertRaises(RuntimeError):
                            if name == "summary.json":
                                deps.write_json(name, {"sealed": False})
                            else:
                                deps.write_checksums(name)

                        self.assertEqual(
                            external_before,
                            external.read_bytes(),
                        )

    def test_backup_files_and_directory_are_fsynced_before_hashing(
        self,
    ) -> None:
        commands: list[list[str]] = []

        def run(command, **kwargs):
            commands.append(list(command))
            if "pg_dump" in command:
                kwargs["stdout"].write(b"custom-dump")
            if "pg_restore" in command:
                kwargs["stdout"].write(b"restore-list")
            return types.SimpleNamespace(returncode=0)

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            source = root / (
                "reports/responses-native-history-normalization-20260918/"
                "newapi-sub2api-sync-extension-v0.1.5.zip"
            )
            source.parent.mkdir(parents=True)
            source.write_bytes(b"zip")
            (root / "docker-compose.yml").write_text(
                "services: {}\n",
                encoding="utf-8",
            )
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                run=run,
            )
            deps.create_output_dir()
            artifact_names = {
                "docker-compose.yml",
                "newapi-sub2api-sync-extension-v0.1.5.zip",
                "new-api.dump",
                "pg-restore-list.txt",
            }
            events: list[str] = []
            real_fsync = pricing.os.fsync
            real_file_sha256 = pricing.ProductionDependencies._file_sha256

            def tracked_fsync(descriptor):
                descriptor_stat = pricing.os.fstat(descriptor)
                output_stat = deps.output_dir.stat()
                if (
                    descriptor_stat.st_dev == output_stat.st_dev
                    and descriptor_stat.st_ino == output_stat.st_ino
                ):
                    events.append("fsync-dir")
                else:
                    for name in artifact_names:
                        path = deps.output_dir / name
                        if not path.exists():
                            continue
                        path_stat = path.stat()
                        if (
                            descriptor_stat.st_dev == path_stat.st_dev
                            and descriptor_stat.st_ino == path_stat.st_ino
                        ):
                            events.append("fsync-file:" + name)
                            break
                return real_fsync(descriptor)

            def tracked_hash(path):
                events.append("hash:" + path.name)
                return real_file_sha256(path)

            with (
                mock.patch.object(
                    pricing.os,
                    "fsync",
                    side_effect=tracked_fsync,
                ),
                mock.patch.object(
                    pricing.ProductionDependencies,
                    "_file_sha256",
                    side_effect=tracked_hash,
                ),
            ):
                deps.copy_compose()
                deps.copy_extension()
                deps.dump_database()
                deps.write_checksums("SHA256SUMS.pre")

            first_hash = min(
                events.index("hash:" + name)
                for name in artifact_names
            )
            file_fsync_indexes = []
            for name in artifact_names:
                fsync_index = events.index("fsync-file:" + name)
                file_fsync_indexes.append(fsync_index)
                self.assertLess(fsync_index, events.index("hash:" + name))
            self.assertTrue(
                any(
                    max(file_fsync_indexes) < index < first_hash
                    for index, event in enumerate(events)
                    if event == "fsync-dir"
                )
            )
            self.assertEqual(2, len(commands))

    def test_all_evidence_files_are_private_under_umask_022(self) -> None:
        def run(command, **kwargs):
            if "pg_dump" in command:
                kwargs["stdout"].write(b"custom-dump")
            if "pg_restore" in command:
                kwargs["stdout"].write(b"restore-list")
            return types.SimpleNamespace(returncode=0)

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            source = root / (
                "reports/responses-native-history-normalization-20260918/"
                "newapi-sub2api-sync-extension-v0.1.5.zip"
            )
            source.parent.mkdir(parents=True)
            source.write_bytes(b"zip")
            (root / "docker-compose.yml").write_text(
                "services: {}\n",
                encoding="utf-8",
            )
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                run=run,
            )
            previous_umask = pricing.os.umask(0o022)
            try:
                deps.create_output_dir()
                deps.copy_compose()
                deps.copy_extension()
                deps.dump_database()
                deps.write_text("runtime.txt", "safe\n")
                deps.write_json("safe.json", {"safe": True})
                deps.write_checksums("SHA256SUMS.pre")
            finally:
                pricing.os.umask(previous_umask)

            names = {entry.name for entry in deps.output_dir.iterdir()}
            deps.validate_artifact_set(names)
            for name in names:
                file_stat = pricing.os.lstat(deps.output_dir / name)
                self.assertTrue(stat.S_ISREG(file_stat.st_mode), name)
                self.assertEqual(0o600, stat.S_IMODE(file_stat.st_mode), name)
                self.assertEqual(pricing.os.getuid(), file_stat.st_uid, name)
                self.assertEqual(1, file_stat.st_nlink, name)

    def test_required_artifact_set_rejects_non_private_mode(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.create_output_dir()
            artifact = deps.output_dir / "artifact.txt"
            artifact.write_text("durable\n", encoding="utf-8")
            artifact.chmod(0o644)

            with self.assertRaisesRegex(RuntimeError, "mode|private"):
                deps.validate_artifact_set({"artifact.txt"})

    def test_required_artifact_set_rejects_missing_and_unexpected_files(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.create_output_dir()
            expected = {"one.json", "two.txt"}
            for name in expected:
                deps.write_text(name, name)

            deps.validate_artifact_set(expected)

            (deps.output_dir / "two.txt").unlink()
            with self.assertRaisesRegex(RuntimeError, "missing.*two.txt"):
                deps.validate_artifact_set(expected)
            deps.write_text("two.txt", "two")
            deps.write_text("unexpected.txt", "unexpected")
            with self.assertRaisesRegex(
                RuntimeError,
                "unexpected.*unexpected.txt",
            ):
                deps.validate_artifact_set(expected)

    def test_required_artifact_set_rejects_extra_hardlink(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.create_output_dir()
            artifact = deps.output_dir / "artifact.txt"
            artifact.write_text("durable\n", encoding="utf-8")
            pricing.os.link(artifact, backups / "artifact-copy")

            with self.assertRaisesRegex(RuntimeError, "hard links"):
                deps.validate_artifact_set({"artifact.txt"})

    def test_concrete_backup_uses_fixed_sources_and_validates_dump(
        self,
    ) -> None:
        commands: list[list[str]] = []

        def run(command, **kwargs):
            commands.append(list(command))
            if "pg_dump" in command:
                kwargs["stdout"].write(b"custom-dump")
            if "pg_restore" in command:
                self.assertEqual(b"custom-dump", kwargs["stdin"].read())
                kwargs["stdout"].write(b"restore-list")
            return types.SimpleNamespace(returncode=0)

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            source = root / (
                "reports/responses-native-history-normalization-20260918/"
                "newapi-sub2api-sync-extension-v0.1.5.zip"
            )
            source.parent.mkdir(parents=True)
            source.write_bytes(b"zip")
            (root / "docker-compose.yml").write_text(
                "services: {}\n",
                encoding="utf-8",
            )
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                run=run,
            )
            deps.create_output_dir()

            deps.copy_compose()
            deps.copy_extension()
            deps.dump_database()
            deps.write_json("safe.json", {"model": pricing.MODEL})
            deps.write_checksums("SHA256SUMS.pre")

            self.assertEqual(
                b"services: {}\n",
                (backups / "run/docker-compose.yml").read_bytes(),
            )
            self.assertEqual(
                b"zip",
                (
                    backups
                    / "run/newapi-sub2api-sync-extension-v0.1.5.zip"
                ).read_bytes(),
            )
            self.assertIn("pg_dump", commands[0])
            self.assertIn("-Fc", commands[0])
            self.assertIn("pg_restore", commands[1])
            self.assertIn("--list", commands[1])
            manifest = (
                backups / "run/SHA256SUMS.pre"
            ).read_text(encoding="ascii")
            self.assertIn("new-api.dump", manifest)
            self.assertNotIn("SHA256SUMS.pre", manifest)

    def test_checksum_write_and_validation_stream_large_dump(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.create_output_dir()
            dump = deps.output_dir / "new-api.dump"
            dump.write_bytes(b"x" * (8 * 1024 * 1024 + 1))
            dump.chmod(0o600)
            original_read_bytes = pathlib.Path.read_bytes

            def reject_dump_read_bytes(path):
                if path == dump:
                    raise AssertionError("dump must not use Path.read_bytes")
                return original_read_bytes(path)

            with mock.patch.object(
                pathlib.Path,
                "read_bytes",
                reject_dump_read_bytes,
            ):
                deps.write_checksums("SHA256SUMS")
                deps._validate_checksums(
                    "SHA256SUMS",
                    require_seal=False,
                )

            self.assertGreaterEqual(pricing.HASH_CHUNK_SIZE, 1024 * 1024)
            self.assertLessEqual(pricing.HASH_CHUNK_SIZE, 8 * 1024 * 1024)

    def test_checksum_validation_rejects_handwritten_manifest_missing_artifact(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            deps.create_output_dir()
            dump = deps.output_dir / "new-api.dump"
            evidence = deps.output_dir / "safe.json"
            dump.write_bytes(b"database backup")
            evidence.write_text('{"safe": true}\n', encoding="utf-8")
            dump.chmod(0o600)
            evidence.chmod(0o600)
            evidence_digest = hashlib.sha256(evidence.read_bytes()).hexdigest()
            manifest_path = deps.output_dir / "SHA256SUMS"
            manifest_path.write_text(
                "%s  safe.json\n" % evidence_digest,
                encoding="ascii",
            )
            manifest_path.chmod(0o600)

            with self.assertRaisesRegex(
                RuntimeError,
                "missing.*new-api.dump",
            ):
                deps.validate_checksums("SHA256SUMS")

    def test_capture_state_is_safe_and_includes_route_failure_window(
        self,
    ) -> None:
        machine_id = b"machine-identity\n"
        docker_state = {
            "State": {
                "Status": "running",
                "Health": {"Status": "healthy"},
            },
            "RestartCount": 0,
            "Config": {
                "Labels": {
                    "org.opencontainers.image.revision": EXPECTED_REVISION,
                }
            },
        }
        database_state = {
            "target_channels": {"4": 3, "66": 3, "96": 3},
            "target_enabled_abilities": 0,
            "authority_states": {"active": 3, "disabled": 1},
            "temporary_probe_token_count": 0,
            "route48": {
                "channel_id": 48,
                "channel_status": 1,
                "state": "active",
                "consecutive_failures": 2,
                "failure_window_start": 100,
                "last_failure_at": 120,
            },
        }
        sql_calls: list[str] = []

        def check_output(command, **kwargs):
            if command == ["docker", "inspect", "new-api"]:
                self.assertEqual({"text": True, "timeout": 60}, kwargs)
                return json.dumps([docker_state])
            self.assertEqual("docker", command[0])
            self.assertEqual("psql", command[3])
            self.assertEqual({"text": True, "timeout": 120}, kwargs)
            sql_calls.append(command[-1])
            return json.dumps(database_state)

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            machine_path = pathlib.Path(directory) / "machine-id"
            machine_path.write_bytes(machine_id)
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                machine_id_path=machine_path,
                hostname=lambda: "xingyugpu",
                machine=lambda: "x86_64",
                check_output=check_output,
            )

            state = deps.capture_state("before")

        self.assertEqual(
            hashlib.sha256(machine_id.strip()).hexdigest(),
            state["machine_id_sha256"],
        )
        self.assertEqual(database_state["route48"], state["route48"])
        self.assertEqual(
            0,
            state["plan"]["temporary_probe_token_count"],
        )
        self.assertEqual(1, len(sql_calls))
        self.assertIn("GROUP BY state", sql_calls[0])
        self.assertIn("name LIKE 'alias-e2e-%'", sql_calls[0])
        self.assertIn("name LIKE 'alias-all-%'", sql_calls[0])
        self.assertNotIn("ark-probe", sql_calls[0])
        self.assertNotIn("139", sql_calls[0])
        serialized = json.dumps(state, sort_keys=True)
        self.assertNotIn("key", serialized.lower())
        self.assertNotIn("credential", serialized.lower())

    def test_dangling_deployment_symlink_is_treated_as_locked(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
            )
            marker = root / ".deployment.lock"

            self.assertFalse(deps.deployment_lock_exists())
            marker.symlink_to(root / "missing-deployment-owner")
            self.assertFalse(marker.exists())
            self.assertTrue(pricing.os.path.lexists(marker))
            self.assertTrue(deps.deployment_lock_exists())

    def test_token_and_request_log_queries_are_single_and_scoped(self) -> None:
        calls: list[str] = []

        def check_output(command, **kwargs):
            self.assertEqual({"text": True, "timeout": 120}, kwargs)
            self.assertEqual("docker", command[0])
            self.assertEqual("psql", command[3])
            sql = command[-1]
            calls.append(sql)
            if "FROM tokens" in sql:
                return (
                    "139|20|1|true|ark-probe-dedicated|"
                    + TOKEN_SECRET
                    + "|10|1"
                )
            return "48"

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            backups = pathlib.Path(directory) / "backups"
            root.mkdir()
            backups.mkdir()
            deps = pricing.ProductionDependencies(
                self.args(backups / "run"),
                root=root,
                backup_root=backups,
                check_output=check_output,
            )

            row = deps.existing_token_row()
            channel = deps.request_log_once("request-123")
            invalid_channel = deps.request_log_once("request-'unsafe")

        self.assertIn(TOKEN_SECRET, row)
        self.assertEqual(48, channel)
        self.assertEqual(0, invalid_channel)
        self.assertEqual(2, len(calls))
        self.assertIn("t.id=139", calls[0])
        self.assertIn("t.user_id=20", calls[0])
        self.assertIn("request_id='request-123'", calls[1])
        self.assertIn("type IN (2,5)", calls[1])
        self.assertIn("ORDER BY id DESC LIMIT 1", calls[1])


if __name__ == "__main__":
    unittest.main()
