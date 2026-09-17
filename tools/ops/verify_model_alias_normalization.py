#!/usr/bin/env python3
"""Verify stable production model aliases and full enabled-model coverage."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import http.client
import importlib
import json
import sys
import time
from pathlib import Path
from typing import Any

from model_alias_normalization import canonical_alias, file_sha256, psql, write_json


REQUIRED_ALIASES = {"gpt-5.6", "gpt-6", "deepseekv4.1flash"}
API_HOST = "127.0.0.1"
API_PORT = 13000
OUTPUT_ROOT = Path(
    "/opt/newapi-study/reports/model-alias-normalization-20260917"
)


def verify_public_models(
    public_models: set[str],
    manifest: dict[str, Any],
) -> None:
    missing = REQUIRED_ALIASES - public_models
    if missing:
        raise RuntimeError(
            "required aliases missing: " + ",".join(sorted(missing))
        )
    hidden = set(manifest["hidden_concrete_models"]) & public_models
    if hidden:
        raise RuntimeError(
            "concrete release identifiers still exposed: "
            + ",".join(sorted(hidden))
        )


def extend_capability_profiles(profiles: Any) -> None:
    concrete_by_alias: dict[str, str] = {}
    for name in (
        "TRANSLATION_MODELS",
        "VISUAL_EMBEDDING_MODELS",
        "VIDEO_MODELS",
        "THREE_D_MODELS",
    ):
        values = getattr(profiles, name)
        for model in list(values):
            alias = canonical_alias(model)
            concrete_by_alias.setdefault(alias, model)
            values.add(alias)
    for model, size in list(profiles.IMAGE_MODEL_SIZES.items()):
        alias = canonical_alias(model)
        concrete_by_alias.setdefault(alias, model)
        profiles.IMAGE_MODEL_SIZES.setdefault(alias, size)
    original_request_body = getattr(profiles, "request_body", None)
    if callable(original_request_body):
        def request_body(
            model: str,
            capability: str,
            path: str,
        ) -> dict[str, Any]:
            concrete = concrete_by_alias.get(model, model)
            payload = original_request_body(concrete, capability, path)
            payload["model"] = model
            return payload

        profiles.request_body = request_body


def validate_round_reports(reports: list[dict[str, Any]]) -> str:
    inventory_hashes = set()
    for index, report in enumerate(reports, start=1):
        if not report.get("passed") or int(report.get("failed_count") or 0):
            raise RuntimeError("round %d failed" % index)
        inventory_hashes.add(str(report.get("inventory_sha256") or ""))
    if len(inventory_hashes) > 1:
        raise RuntimeError("inventory hash changed between regression rounds")
    return next(iter(inventory_hashes), "")


def prepare_e2e_module(e2e: Any) -> None:
    e2e.TOKEN_NAME_PREFIX = "alias-all"
    extend_capability_profiles(e2e.ark_profiles)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--rounds", type=int, default=3)
    parser.add_argument("--output-dir", default=str(OUTPUT_ROOT))
    return parser.parse_args()


def request(
    method: str,
    path: str,
    headers: dict[str, str],
    body: dict[str, Any] | None = None,
    timeout: int = 300,
) -> tuple[int, dict[str, str], bytes]:
    connection = http.client.HTTPConnection(API_HOST, API_PORT, timeout=timeout)
    try:
        payload = (
            json.dumps(body, ensure_ascii=False).encode()
            if body is not None
            else None
        )
        connection.request(method, path, body=payload, headers=headers)
        response = connection.getresponse()
        raw = response.read()
        if (
            response.getheader("Content-Encoding") == "gzip"
            or raw[:2] == b"\x1f\x8b"
        ):
            raw = gzip.decompress(raw)
        return response.status, dict(response.getheaders()), raw
    finally:
        connection.close()


def api_request(
    headers: dict[str, str],
    method: str,
    path: str,
    body: dict[str, Any] | None = None,
) -> Any:
    status, _, raw = request(method, path, headers, body)
    payload = json.loads(raw or b"{}")
    if status >= 400 or not payload.get("success", False):
        raise RuntimeError(
            "%s %s failed: HTTP %d %s"
            % (method, path, status, payload)
        )
    return payload.get("data")


def root_headers() -> dict[str, str]:
    token = psql("select access_token from users where id=1 and status=1")
    if not token:
        raise RuntimeError("enabled root access token is unavailable")
    return {
        "Authorization": "Bearer " + token,
        "New-Api-User": "1",
        "Content-Type": "application/json",
        "Accept-Encoding": "identity",
    }


def verification_token_name(group: str, nonce: int) -> str:
    return "alias-e2e-%s-%010d" % (group, nonce % 10_000_000_000)


def create_token(
    admin_headers: dict[str, str],
    group: str,
) -> tuple[int, str]:
    name = verification_token_name(group, time.time_ns())
    api_request(
        admin_headers,
        "POST",
        "/api/token/",
        {
            "name": name,
            "remain_quota": 100000000,
            "expired_time": -1,
            "unlimited_quota": False,
            "model_limits_enabled": False,
            "allow_ips": "",
            "group": group,
        },
    )
    escaped = name.replace("'", "''")
    row = psql(
        "select id,key from tokens where name='%s' and deleted_at is null "
        "order by id desc limit 1" % escaped
    )
    if not row:
        raise RuntimeError("verification token was not created")
    token_id, key = row.split("\t", 1)
    return int(token_id), key if key.startswith("sk-") else "sk-" + key


def delete_token(
    admin_headers: dict[str, str],
    token_id: int | None,
) -> None:
    if token_id is not None:
        api_request(
            admin_headers,
            "DELETE",
            "/api/token/%d" % token_id,
        )


def relay_headers(key: str) -> dict[str, str]:
    return {
        "Authorization": "Bearer " + key,
        "Content-Type": "application/json",
        "Accept-Encoding": "identity",
    }


def load_public_models(headers: dict[str, str]) -> set[str]:
    status, _, raw = request("GET", "/v1/models", headers)
    payload = json.loads(raw or b"{}")
    if status != 200 or not isinstance(payload.get("data"), list):
        raise RuntimeError(
            "GET /v1/models failed: HTTP %d %s" % (status, payload)
        )
    return {
        str(item.get("id") or "")
        for item in payload["data"]
        if isinstance(item, dict) and str(item.get("id") or "")
    }


def header_value(headers: dict[str, str], name: str) -> str:
    for key, value in headers.items():
        if key.lower() == name.lower():
            return value
    return ""


def response_has_text(payload: dict[str, Any]) -> bool:
    choices = payload.get("choices")
    if isinstance(choices, list):
        for choice in choices:
            if not isinstance(choice, dict):
                continue
            message = choice.get("message")
            if (
                isinstance(message, dict)
                and str(message.get("content") or "").strip()
            ):
                return True
    return bool(str(payload.get("output_text") or "").strip())


def wait_for_log(request_id: str) -> tuple[int, str]:
    escaped = request_id.replace("'", "''")
    for _ in range(30):
        row = psql(
            "select channel_id,model_name from logs "
            "where request_id='%s' and type=2 order by id desc limit 1"
            % escaped
        )
        if row:
            channel_id, model_name = row.split("\t", 1)
            return int(channel_id), model_name
        time.sleep(1)
    raise RuntimeError("successful request log not found: " + request_id)


def run_required_requests(headers: dict[str, str]) -> list[dict[str, Any]]:
    results = []
    for model in sorted(REQUIRED_ALIASES):
        status, response_headers, raw = request(
            "POST",
            "/v1/chat/completions",
            headers,
            {
                "model": model,
                "messages": [
                    {"role": "user", "content": "Reply with exactly: OK"}
                ],
                "max_tokens": 128,
                "stream": False,
            },
        )
        payload = json.loads(raw or b"{}")
        request_id = header_value(
            response_headers,
            "X-Oneapi-Request-Id",
        )
        if status != 200 or not response_has_text(payload) or not request_id:
            raise RuntimeError(
                "required alias request failed: model=%s status=%d body=%s"
                % (model, status, payload)
            )
        channel_id, logged_model = wait_for_log(request_id)
        if logged_model != model:
            raise RuntimeError(
                "request log model mismatch: expected=%s actual=%s"
                % (model, logged_model)
            )
        if model in {"gpt-5.6", "gpt-6"} and channel_id != 92:
            raise RuntimeError(
                "%s did not use highest-priority channel 92: %d"
                % (model, channel_id)
            )
        results.append(
            {
                "model": model,
                "status": status,
                "request_id": request_id,
                "channel_id": channel_id,
                "passed": True,
            }
        )
    return results


def run_regression_rounds(
    rounds: int,
    output_dir: Path,
) -> list[dict[str, Any]]:
    if rounds <= 0:
        return []
    e2e = importlib.import_module("e2e_all_enabled_models_20260902")
    prepare_e2e_module(e2e)
    reports = []
    original_argv = list(sys.argv)
    try:
        for round_number in range(1, rounds + 1):
            output = output_dir / (
                "all-models-round-%d.json" % round_number
            )
            sys.argv = [e2e.__file__, str(output)]
            result = e2e.main()
            report = json.loads(output.read_text(encoding="utf-8"))
            reports.append(report)
            if result != 0:
                raise RuntimeError(
                    "round %d failed; report=%s"
                    % (round_number, output)
                )
    finally:
        sys.argv = original_argv
    validate_round_reports(reports)
    return reports


def write_audit(
    path: Path,
    manifest: dict[str, Any],
    group_models: dict[str, list[str]],
    required_results: list[dict[str, Any]],
    round_reports: list[dict[str, Any]],
) -> None:
    backup_hashes = manifest.get("backup_sha256") or {}
    lines = [
        "# Production Model Alias Normalization Audit",
        "",
        "Status: PASS",
        "",
        "Manifest SHA-256: `%s`"
        % str(manifest.get("manifest_sha256") or ""),
        "Changed channels: %d"
        % len(manifest.get("channel_changes") or []),
        "Hidden concrete model IDs: %d"
        % len(manifest.get("hidden_concrete_models") or []),
        "Regression rounds: %d" % len(round_reports),
        "",
        "## Required Aliases",
        "",
    ]
    for result in required_results:
        lines.append(
            "- `%s`: HTTP %d, channel %d, request `%s`"
            % (
                result["model"],
                result["status"],
                result["channel_id"],
                result["request_id"],
            )
        )
    lines.extend(["", "## Public Model Lists", ""])
    for group, models in sorted(group_models.items()):
        lines.append("- `%s`: %d models" % (group, len(models)))
    lines.extend(["", "## Backup SHA-256", ""])
    for name, checksum in sorted(backup_hashes.items()):
        lines.append("- `%s`: `%s`" % (name, checksum))
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def write_checksums(output_dir: Path, files: list[Path]) -> dict[str, str]:
    hashes = {
        str(path.relative_to(output_dir)): file_sha256(path)
        for path in sorted(set(files))
        if path.is_file() and output_dir in path.parents
    }
    (output_dir / "SHA256SUMS").write_text(
        "".join(
            "%s  %s\n" % (checksum, name)
            for name, checksum in sorted(hashes.items())
        ),
        encoding="ascii",
    )
    return hashes


def main() -> int:
    args = parse_args()
    if args.rounds < 0:
        raise RuntimeError("--rounds must not be negative")
    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    manifest_path = Path(args.manifest)
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if not manifest.get("applied") or manifest.get("status") != "applied":
        raise RuntimeError("manifest is not an applied production report")

    admin_headers = root_headers()
    token_ids: list[int] = []
    group_models: dict[str, list[str]] = {}
    required_results: list[dict[str, Any]] = []
    try:
        keys = {}
        for group in ("default", "cxy"):
            token_id, key = create_token(admin_headers, group)
            token_ids.append(token_id)
            keys[group] = key
            models = load_public_models(relay_headers(key))
            verify_public_models(models, manifest)
            group_models[group] = sorted(models)
        required_results = run_required_requests(
            relay_headers(keys["default"])
        )
    finally:
        for token_id in reversed(token_ids):
            delete_token(admin_headers, token_id)

    round_reports = run_regression_rounds(args.rounds, output_dir)
    summary = {
        "passed": True,
        "manifest_sha256": manifest.get("manifest_sha256"),
        "group_models": group_models,
        "group_model_sha256": {
            group: hashlib.sha256(
                json.dumps(models, ensure_ascii=False).encode()
            ).hexdigest()
            for group, models in group_models.items()
        },
        "required_results": required_results,
        "rounds": [
            {
                "round": index,
                "inventory_count": report["inventory_count"],
                "inventory_sha256": report["inventory_sha256"],
                "passed_count": report["passed_count"],
                "failed_count": report["failed_count"],
            }
            for index, report in enumerate(round_reports, start=1)
        ],
        "stable_inventory_sha256": validate_round_reports(round_reports),
    }
    summary_path = output_dir / "verification-summary.json"
    audit_path = output_dir / "audit.md"
    write_json(summary_path, summary)
    write_audit(
        audit_path,
        manifest,
        group_models,
        required_results,
        round_reports,
    )
    report_files = [manifest_path, summary_path, audit_path]
    report_files.extend(
        output_dir / ("all-models-round-%d.json" % round_number)
        for round_number in range(1, args.rounds + 1)
    )
    hashes = write_checksums(output_dir, report_files)
    print(
        "status=pass groups=%d required=%d rounds=%d artifacts=%d"
        % (
            len(group_models),
            len(required_results),
            len(round_reports),
            len(hashes),
        )
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print("ERROR: %s" % exc, file=sys.stderr)
        raise SystemExit(1)
