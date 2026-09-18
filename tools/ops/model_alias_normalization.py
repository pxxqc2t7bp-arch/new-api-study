#!/usr/bin/env python3
"""Normalize production channel model IDs to stable public aliases."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import http.client
import json
import re
import shutil
import subprocess
import sys
import time
from collections import defaultdict
from copy import deepcopy
from pathlib import Path
from typing import Any


ISO_DATE = re.compile(r"-20\d{2}-\d{2}-\d{2}$")
COMPACT_RELEASE = re.compile(r"-(?:ga-)?\d{6,8}$")
TERMINAL_RELEASE_TAG = re.compile(r"-(?:draft-preview|preview|latest-version)$")
DEEPSEEK_FLASH = re.compile(
    r"deepseek-v4-flash(?:-(?:ga-)?\d{6,8}|-原厂)?"
)

SPECIAL_ALIASES = {
    "gpt-5.6-luna": "gpt-5.6",
    "gpt-5.6-sol": "gpt-5.6",
    "gpt-5.6-terra": "gpt-5.6",
    "gpt-6-astra": "gpt-6",
    "glm-5.3-cxy": "glm-5.3",
}

OPTION_KEYS = (
    "ModelRatio",
    "ModelPrice",
    "CompletionRatio",
    "CacheRatio",
    "billing_setting.billing_mode",
    "billing_setting.billing_expr",
)
UPSTREAM_MODEL_ALIASES_KEY = "upstream_orchestration.model_aliases"
MANAGED_OPTION_KEYS = OPTION_KEYS + (UPSTREAM_MODEL_ALIASES_KEY,)
PUBLIC_GROUPS = {"default", "cxy"}
REQUIRED_ALIASES = {"gpt-5.6", "gpt-6", "deepseekv4.1flash"}
API_HOST = "127.0.0.1"
API_PORT = 13000
BACKUP_ROOT = Path("/opt/newapi-study/backups")
COMPOSE_PATH = Path("/opt/newapi-study/docker-compose.yml")
REPORT_ROOT = Path(
    "/opt/newapi-study/reports/model-alias-normalization-20260917"
)


def canonical_alias(model: str) -> str:
    if model in SPECIAL_ALIASES:
        return SPECIAL_ALIASES[model]
    if model == "deepseek-v4-flash-vision-exp-原厂":
        return "deepseekv4.1flash-vision"
    if DEEPSEEK_FLASH.fullmatch(model):
        return "deepseekv4.1flash"

    alias = model.removesuffix("-原厂")
    alias = ISO_DATE.sub("", alias)
    alias = COMPACT_RELEASE.sub("", alias)
    alias = TERMINAL_RELEASE_TAG.sub("", alias)
    alias = re.sub(
        r"^doubao-seed-(\d+)-(\d+)(?=-|$)",
        r"doubao-seed-\1.\2",
        alias,
    )
    alias = re.sub(r"^glm-(\d+)-(\d+)(?=-|$)", r"glm-\1.\2", alias)
    return alias


def parse_models(value: str) -> list[str]:
    return [item.strip() for item in value.split(",") if item.strip()]


def parse_mapping(value: Any) -> dict[str, str]:
    if value in (None, ""):
        return {}
    parsed = json.loads(value) if isinstance(value, str) else value
    if not isinstance(parsed, dict):
        raise ValueError("model_mapping must be an object")
    return {
        str(alias): str(target)
        for alias, target in parsed.items()
        if str(alias) and str(target)
    }


def parse_object(value: Any, field_name: str) -> dict[str, Any]:
    if value in (None, ""):
        return {}
    parsed = json.loads(value) if isinstance(value, str) else value
    if not isinstance(parsed, dict):
        raise ValueError("%s must be an object" % field_name)
    return deepcopy(parsed)


def normalize_route_settings(value: Any) -> dict[str, Any]:
    settings = parse_object(value, "settings")
    advanced = settings.get("advanced_custom")
    if not isinstance(advanced, dict):
        return settings
    routes = advanced.get("advanced_routes")
    if not isinstance(routes, list):
        return settings
    for route in routes:
        if not isinstance(route, dict):
            continue
        models = route.get("models")
        if not isinstance(models, list):
            continue
        route["models"] = sorted(
            {
                model
                if str(model).startswith("re:")
                else canonical_alias(str(model))
                for model in models
                if str(model).strip()
            }
        )
    return settings


def release_number(model: str) -> int:
    iso_match = re.search(r"-(20\d{2})-(\d{2})-(\d{2})$", model)
    if iso_match:
        return int("".join(iso_match.groups()))
    compact_match = re.search(r"-(\d{6,8})$", model)
    if compact_match:
        return int(compact_match.group(1))
    return 0


def candidate_rank(
    alias: str,
    model: str,
    recent_success: dict[str, int],
) -> tuple[int, int, int, int, str]:
    if model == alias:
        stability = 4
    elif re.search(r"(?:^|-)(?:draft-)?preview(?:-|$)", model):
        stability = 0
    elif model.endswith("-latest-version"):
        stability = 1
    elif release_number(model) > 0:
        stability = 2
    else:
        stability = 3
    last_success = recent_success.get(model, 0)
    return (
        int(last_success > 0),
        stability,
        release_number(model),
        last_success,
        model,
    )


def select_concrete(
    alias: str,
    candidates: list[str],
    existing_mapping: dict[str, str],
    recent_success: dict[str, int],
) -> str:
    if not candidates:
        raise ValueError("candidates must not be empty")
    for current_alias, target in existing_mapping.items():
        if canonical_alias(current_alias) == alias and current_alias in candidates:
            return target
    candidate = max(
        candidates,
        key=lambda item: candidate_rank(alias, item, recent_success),
    )
    return existing_mapping.get(candidate, candidate)


def build_channel_plan(
    channel: dict[str, Any],
    recent_success: dict[str, int],
) -> dict[str, Any]:
    concrete_models = parse_models(str(channel.get("models") or ""))
    existing_mapping = parse_mapping(channel.get("model_mapping"))
    settings_field = (
        "settings"
        if "settings" in channel
        else "setting"
        if "setting" in channel
        else ""
    )
    current_settings = (
        parse_object(channel.get(settings_field), settings_field)
        if settings_field
        else {}
    )
    desired_settings = normalize_route_settings(current_settings)
    by_alias: dict[str, list[str]] = defaultdict(list)
    for concrete in concrete_models:
        by_alias[canonical_alias(concrete)].append(concrete)

    desired_mapping = {
        alias: target
        for alias, target in existing_mapping.items()
        if canonical_alias(alias) not in by_alias
    }
    desired_models = sorted(by_alias)
    selected: dict[str, str] = {}
    selected_candidates: dict[str, str] = {}
    for alias in desired_models:
        selected_candidate = max(
            by_alias[alias],
            key=lambda item: (
                int(item in existing_mapping),
                *candidate_rank(alias, item, recent_success),
            ),
        )
        target = select_concrete(
            alias,
            by_alias[alias],
            existing_mapping,
            recent_success,
        )
        selected[alias] = target
        selected_candidates[alias] = selected_candidate
        if target != alias:
            desired_mapping[alias] = target

    return {
        "channel_id": int(channel["id"]),
        "models": desired_models,
        "model_mapping": desired_mapping,
        "selected": selected,
        "selected_candidates": selected_candidates,
        "candidates": {
            alias: sorted(candidates)
            for alias, candidates in sorted(by_alias.items())
        },
        "hidden_concrete_models": sorted(
            model for model in concrete_models if model not in desired_models
        ),
        "settings_field": settings_field,
        "desired_settings": desired_settings,
        "settings_changed": desired_settings != current_settings,
        "changed": (
            desired_models != concrete_models
            or desired_mapping != existing_mapping
            or desired_settings != current_settings
        ),
    }


def is_public_channel(channel: dict[str, Any]) -> bool:
    groups = {
        item.strip()
        for item in str(channel.get("group") or "").split(",")
        if item.strip()
    }
    return bool(groups & PUBLIC_GROUPS)


def public_groups(channel: dict[str, Any]) -> set[str]:
    return {
        item.strip()
        for item in str(channel.get("group") or "").split(",")
        if item.strip() in PUBLIC_GROUPS
    }


def prune_unverified_alias(plan: dict[str, Any], alias: str) -> None:
    plan["models"] = [
        model for model in plan["models"] if model != alias
    ]
    plan["changed"] = True
    plan["model_mapping"].pop(alias, None)
    plan["selected"].pop(alias, None)
    plan["selected_candidates"].pop(alias, None)
    plan["candidates"].pop(alias, None)
    plan.setdefault("pruned_unverified_aliases", []).append(alias)
    settings = plan.get("desired_settings")
    if not isinstance(settings, dict):
        return
    advanced = settings.get("advanced_custom")
    if not isinstance(advanced, dict):
        return
    routes = advanced.get("advanced_routes")
    if not isinstance(routes, list):
        return
    for route in routes:
        if not isinstance(route, dict):
            continue
        models = route.get("models")
        if isinstance(models, list):
            route["models"] = [
                model for model in models if model != alias
            ]


def has_effective_pricing(options: dict[str, dict[str, Any]], model: str) -> bool:
    fixed_price = options["ModelPrice"].get(model)
    if isinstance(fixed_price, (int, float)) and fixed_price > 0:
        return True
    ratio = options["ModelRatio"].get(model)
    if isinstance(ratio, (int, float)) and ratio > 0:
        return True
    mode = options["billing_setting.billing_mode"].get(model)
    expression = str(
        options["billing_setting.billing_expr"].get(model) or ""
    ).strip()
    return mode == "tiered_expr" and bool(expression)


def inherit_alias_pricing(
    options: dict[str, dict[str, Any]],
    selected: dict[str, str],
) -> dict[str, dict[str, Any]]:
    updated = {
        key: deepcopy(options.get(key) or {})
        for key in MANAGED_OPTION_KEYS
    }
    for alias, concrete in sorted(selected.items()):
        if not has_effective_pricing(updated, concrete):
            raise RuntimeError(
                "unpriced alias %s: selected concrete %s has no effective price"
                % (alias, concrete)
            )
        for key in OPTION_KEYS:
            if concrete in updated[key]:
                updated[key][alias] = deepcopy(updated[key][concrete])
    return updated


def inherit_managed_upstream_aliases(
    aliases: dict[str, Any],
    channels: list[dict[str, Any]],
) -> dict[str, Any]:
    updated = deepcopy(aliases)
    for channel in channels:
        mapping = parse_mapping(channel.get("model_mapping"))
        for model in parse_models(str(channel.get("models") or "")):
            alias = canonical_alias(model)
            target = mapping.get(model, model)
            if target != alias:
                updated[target] = alias
    return updated


def validate_apply_hash(expected: str, actual: str) -> None:
    if not expected or expected != actual:
        raise RuntimeError("manifest SHA-256 does not match current dry-run")


def build_manifest(
    channels: list[dict[str, Any]],
    options: dict[str, dict[str, Any]],
    recent_success_by_channel: dict[int, dict[str, int]],
    managed_channels: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    managed_channels = managed_channels or []
    all_plans: list[dict[str, Any]] = []
    for channel in sorted(channels, key=lambda item: int(item["id"])):
        if not is_public_channel(channel):
            continue
        channel_id = int(channel["id"])
        plan = build_channel_plan(
            channel,
            recent_success_by_channel.get(channel_id, {}),
        )
        plan["name"] = str(channel.get("name") or "")
        plan["group"] = str(channel.get("group") or "")
        plan["priority"] = int(channel.get("priority") or 0)
        plan["pruned_unverified_aliases"] = []
        all_plans.append(plan)

    blockers: list[str] = []
    healthy_coverage: dict[str, set[str]] = defaultdict(set)
    for plan in all_plans:
        successes = recent_success_by_channel.get(plan["channel_id"], {})
        for alias, candidate in plan["selected_candidates"].items():
            target = plan["selected"][alias]
            if max(
                successes.get(candidate, 0),
                successes.get(target, 0),
            ) > 0:
                healthy_coverage[alias].update(public_groups(plan))

    for alias in sorted(REQUIRED_ALIASES):
        missing_groups = sorted(
            PUBLIC_GROUPS - healthy_coverage.get(alias, set())
        )
        if missing_groups:
            blockers.append(
                "%s missing public groups: %s"
                % (alias, ",".join(missing_groups))
            )

    for plan in all_plans:
        successes = recent_success_by_channel.get(plan["channel_id"], {})
        for alias, candidate in list(plan["selected_candidates"].items()):
            target = plan["selected"][alias]
            if candidate == alias and target == alias:
                continue
            if max(
                successes.get(candidate, 0),
                successes.get(target, 0),
            ) <= 0:
                groups = public_groups(plan)
                if groups <= healthy_coverage.get(alias, set()):
                    prune_unverified_alias(plan, alias)
                else:
                    blockers.append(
                        "channel %d alias %s selected target has no successful log: %s"
                        % (plan["channel_id"], alias, target)
                    )

    plans = [plan for plan in all_plans if plan["changed"]]

    price_sources: dict[str, str] = {}
    for plan in sorted(
        plans,
        key=lambda item: (-int(item["priority"]), int(item["channel_id"])),
    ):
        for alias, target in plan["selected"].items():
            if alias in price_sources or has_effective_pricing(options, alias):
                continue
            source_candidates = (
                [target, plan["selected_candidates"][alias]]
                + plan["candidates"][alias]
            )
            source = next(
                (
                    candidate
                    for candidate in source_candidates
                    if has_effective_pricing(options, candidate)
                ),
                "",
            )
            if source:
                price_sources[alias] = source
            else:
                blockers.append(
                    "unpriced alias %s on channel %d"
                    % (alias, plan["channel_id"])
                )

    desired_options = inherit_alias_pricing(options, price_sources)
    desired_options[UPSTREAM_MODEL_ALIASES_KEY] = (
        inherit_managed_upstream_aliases(
            options.get(UPSTREAM_MODEL_ALIASES_KEY) or {},
            managed_channels,
        )
    )
    return {
        "channel_changes": plans,
        "required_aliases": sorted(REQUIRED_ALIASES),
        "price_sources": price_sources,
        "desired_options": desired_options,
        "hidden_concrete_models": sorted(
            {
                model
                for plan in plans
                for model in plan["hidden_concrete_models"]
            }
        ),
        "blockers": sorted(set(blockers)),
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--expected-manifest-sha256", default="")
    parser.add_argument(
        "--output",
        default=str(REPORT_ROOT / "dry-run.json"),
    )
    return parser.parse_args()


def psql(sql: str) -> str:
    return subprocess.check_output(
        [
            "docker",
            "exec",
            "newapi-postgres",
            "psql",
            "-U",
            "newapi",
            "-d",
            "new-api",
            "-t",
            "-A",
            "-F",
            "\t",
            "-c",
            sql,
        ],
        timeout=120,
        text=True,
    ).strip()


def api_request(
    headers: dict[str, str],
    method: str,
    path: str,
    body: dict[str, Any] | None = None,
    timeout: int = 120,
) -> Any:
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
        content_encoding = response.getheader("Content-Encoding") or ""
    finally:
        connection.close()
    parsed = decode_response_body(raw, content_encoding)
    if response.status >= 400 or not parsed.get("success", False):
        raise RuntimeError(
            "%s %s failed: HTTP %d %s"
            % (method, path, response.status, parsed)
        )
    return parsed.get("data")


def decode_response_body(raw: bytes, content_encoding: str) -> dict[str, Any]:
    if content_encoding.lower() == "gzip" or raw[:2] == b"\x1f\x8b":
        raw = gzip.decompress(raw)
    parsed = json.loads(raw or b"{}")
    if not isinstance(parsed, dict):
        raise RuntimeError("management API response is not an object")
    return parsed


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


def load_channels() -> list[dict[str, Any]]:
    raw = psql(
        """
select coalesce(json_agg(row_to_json(q)),'[]'::json)::text
from (
  select id,name,type,status,base_url,"group",priority,weight,auto_ban,
         models,coalesce(model_mapping,'') as model_mapping,
         coalesce(settings,'') as settings
  from channels
  where status=1
  order by id
) q
"""
    )
    rows = json.loads(raw or "[]")
    if not isinstance(rows, list):
        raise RuntimeError("channel inventory is not a list")
    return rows


def load_options() -> dict[str, dict[str, Any]]:
    quoted = ",".join(
        "'" + key.replace("'", "''") + "'"
        for key in MANAGED_OPTION_KEYS
    )
    rows = psql(
        "select key,value from options where key in (%s) order by key" % quoted
    )
    options = {key: {} for key in MANAGED_OPTION_KEYS}
    for row in rows.splitlines():
        if not row:
            continue
        key, value = row.split("\t", 1)
        parsed = json.loads(value or "{}")
        if not isinstance(parsed, dict):
            raise RuntimeError("option %s is not an object" % key)
        options[key] = parsed
    return options


def load_managed_channels() -> list[dict[str, Any]]:
    raw = psql(
        """
select coalesce(json_agg(row_to_json(q)),'[]'::json)::text
from (
  select c.id,c.models,coalesce(c.model_mapping,'') as model_mapping
  from channels c
  join upstream_managed_routes r on r.channel_id=c.id
  where r.detached=false
  order by c.id
) q
"""
    )
    rows = json.loads(raw or "[]")
    if not isinstance(rows, list):
        raise RuntimeError("managed channel inventory is not a list")
    return rows


def load_recent_success() -> dict[int, dict[str, int]]:
    rows = psql(
        """
select channel_id,model_name,max(created_at)
from logs
where type=2 and channel_id is not null and model_name<>''
group by channel_id,model_name
order by channel_id,model_name
"""
    )
    by_channel: dict[int, dict[str, int]] = defaultdict(dict)
    for row in rows.splitlines():
        if not row:
            continue
        channel_id, model, last_success = row.split("\t", 2)
        by_channel[int(channel_id)][model] = int(last_success or 0)
    return dict(by_channel)


def digest(value: Any) -> str:
    encoded = json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode()
    return hashlib.sha256(encoded).hexdigest()


def file_sha256(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def write_json(path: Path, payload: Any) -> str:
    path.parent.mkdir(parents=True, exist_ok=True)
    raw = (
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    ).encode()
    path.write_bytes(raw)
    return hashlib.sha256(raw).hexdigest()


def sanitized_channel_inventory(
    channels: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    fields = (
        "id",
        "name",
        "type",
        "status",
        "base_url",
        "group",
        "priority",
        "weight",
        "auto_ban",
        "models",
        "model_mapping",
    )
    summaries = []
    for channel in channels:
        summary = {field: channel.get(field) for field in fields}
        summary["settings_sha256"] = digest(
            parse_object(channel.get("settings"), "settings")
        )
        summaries.append(summary)
    return summaries


def report_manifest(
    manifest: dict[str, Any],
    channels: list[dict[str, Any]],
    options: dict[str, dict[str, Any]],
) -> tuple[dict[str, Any], str]:
    safe = deepcopy(manifest)
    desired_options = safe.pop("desired_options")
    for plan in safe["channel_changes"]:
        settings = plan.pop("desired_settings")
        plan["desired_settings_sha256"] = digest(settings)
    safe["channel_inventory_sha256"] = digest(
        sanitized_channel_inventory(channels)
    )
    safe["options_before_sha256"] = digest(options)
    safe["options_after_sha256"] = digest(desired_options)
    safe["option_alias_changes"] = {
        key: sorted(
            alias
            for alias, value in desired_options[key].items()
            if options[key].get(alias) != value
        )
        for key in MANAGED_OPTION_KEYS
    }
    manifest_sha256 = digest(safe)
    safe["manifest_sha256"] = manifest_sha256
    return safe, manifest_sha256


def channel_key_hashes(channel_ids: list[int]) -> dict[int, str]:
    if not channel_ids:
        return {}
    rows = psql(
        "select id,key from channels where id in (%s) order by id"
        % ",".join(str(channel_id) for channel_id in channel_ids)
    )
    result = {}
    for row in rows.splitlines():
        channel_id, key = row.split("\t", 1)
        result[int(channel_id)] = hashlib.sha256(key.encode()).hexdigest()
    return result


def latest_extension_zip() -> Path:
    candidates = [
        path
        for path in BACKUP_ROOT.rglob(
            "newapi-sub2api-sync-extension-v*.zip"
        )
        if path.is_file()
    ]
    if not candidates:
        raise RuntimeError("production extension ZIP was not found")
    return max(candidates, key=lambda path: path.stat().st_mtime)


def create_backup(
    safe_manifest: dict[str, Any],
    channels: list[dict[str, Any]],
    options: dict[str, dict[str, Any]],
) -> tuple[Path, dict[str, str]]:
    timestamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    backup_dir = BACKUP_ROOT / (
        "pre-model-alias-normalization-" + timestamp
    )
    backup_dir.mkdir(parents=True, exist_ok=False)
    if not COMPOSE_PATH.is_file():
        raise RuntimeError("production Compose file is missing")
    shutil.copy2(COMPOSE_PATH, backup_dir / "docker-compose.yml")
    shutil.copy2(
        latest_extension_zip(),
        backup_dir / "newapi-sub2api-sync-extension-v0.1.5.zip",
    )
    write_json(
        backup_dir / "channels-before.json",
        sanitized_channel_inventory(channels),
    )
    write_json(backup_dir / "options-before.json", options)
    write_json(backup_dir / "alias-manifest.json", safe_manifest)

    with (backup_dir / "new-api.sql").open("wb") as handle:
        subprocess.run(
            [
                "docker",
                "exec",
                "newapi-postgres",
                "pg_dump",
                "-U",
                "newapi",
                "-d",
                "new-api",
            ],
            check=True,
            stdout=handle,
            timeout=600,
        )

    image_id = subprocess.check_output(
        ["docker", "inspect", "--format", "{{.Image}}", "new-api"],
        text=True,
        timeout=60,
    ).strip()
    if not image_id:
        raise RuntimeError("new-api image ID is empty")
    subprocess.run(
        [
            "docker",
            "image",
            "save",
            "--output",
            str(backup_dir / "new-api-image.tar"),
            image_id,
        ],
        check=True,
        timeout=1200,
    )

    artifacts = sorted(
        path
        for path in backup_dir.iterdir()
        if path.is_file() and path.name != "SHA256SUMS"
    )
    hashes = {path.name: file_sha256(path) for path in artifacts}
    checksum_path = backup_dir / "SHA256SUMS"
    checksum_path.write_text(
        "".join(
            "%s  %s\n" % (checksum, name)
            for name, checksum in hashes.items()
        ),
        encoding="ascii",
    )
    for name, expected in hashes.items():
        actual = file_sha256(backup_dir / name)
        if actual != expected:
            raise RuntimeError("backup checksum mismatch: " + name)
    return backup_dir, hashes


def option_rows_from_api(data: Any) -> dict[str, dict[str, Any]]:
    items = data.get("items", data) if isinstance(data, dict) else data
    if not isinstance(items, list):
        raise RuntimeError("option API payload is not a list")
    values = {key: {} for key in MANAGED_OPTION_KEYS}
    for item in items:
        key = str(item.get("key") or "")
        if key not in values:
            continue
        parsed = json.loads(str(item.get("value") or "{}"))
        if not isinstance(parsed, dict):
            raise RuntimeError("option %s is not an object" % key)
        values[key] = parsed
    return values


def update_option(
    headers: dict[str, str],
    key: str,
    value: dict[str, Any],
) -> None:
    api_request(
        headers,
        "PUT",
        "/api/option/",
        {
            "key": key,
            "value": json.dumps(
                value,
                ensure_ascii=False,
                separators=(",", ":"),
            ),
        },
    )


def channel_payload(
    original: dict[str, Any],
    plan: dict[str, Any],
) -> dict[str, Any]:
    payload = deepcopy(original)
    payload["models"] = ",".join(plan["models"])
    payload["model_mapping"] = json.dumps(
        plan["model_mapping"],
        ensure_ascii=False,
        separators=(",", ":"),
    )
    settings_field = str(plan.get("settings_field") or "")
    if settings_field:
        payload[settings_field] = json.dumps(
            plan["desired_settings"],
            ensure_ascii=False,
            separators=(",", ":"),
        )
    payload["key"] = ""
    payload.pop("status", None)
    return payload


def restore_channel_payload(original: dict[str, Any]) -> dict[str, Any]:
    payload = deepcopy(original)
    payload["key"] = ""
    payload.pop("status", None)
    return payload


def verify_channel_readback(
    channel: dict[str, Any],
    plan: dict[str, Any],
) -> list[str]:
    errors = []
    if sorted(parse_models(str(channel.get("models") or ""))) != sorted(
        plan["models"]
    ):
        errors.append("model readback mismatch")
    if parse_mapping(channel.get("model_mapping")) != plan["model_mapping"]:
        errors.append("model_mapping readback mismatch")
    settings_field = str(plan.get("settings_field") or "")
    if settings_field and parse_object(
        channel.get(settings_field),
        settings_field,
    ) != plan["desired_settings"]:
        errors.append("settings readback mismatch")
    return errors


def verify_abilities(plans: list[dict[str, Any]]) -> list[str]:
    errors = []
    for plan in plans:
        rows = psql(
            "select \"group\",model from abilities "
            "where enabled=true and channel_id=%d order by \"group\",model"
            % int(plan["channel_id"])
        )
        actual = {
            tuple(row.split("\t", 1))
            for row in rows.splitlines()
            if row
        }
        groups = {
            group.strip()
            for group in str(plan["group"]).split(",")
            if group.strip()
        }
        expected = {
            (group, model)
            for group in groups
            for model in plan["models"]
        }
        if actual != expected:
            errors.append(
                "channel %d ability readback mismatch"
                % int(plan["channel_id"])
            )
    return errors


def apply_manifest(
    headers: dict[str, str],
    manifest: dict[str, Any],
    original_options: dict[str, dict[str, Any]],
) -> dict[str, Any]:
    plans = manifest["channel_changes"]
    originals: dict[int, dict[str, Any]] = {}
    applied_options: list[str] = []
    applied_channels: list[int] = []
    before_key_hashes = channel_key_hashes(
        [int(plan["channel_id"]) for plan in plans]
    )
    try:
        for key in (UPSTREAM_MODEL_ALIASES_KEY,):
            desired = manifest["desired_options"].get(key) or {}
            original = original_options.get(key) or {}
            if desired == original:
                continue
            update_option(headers, key, desired)
            applied_options.append(key)

        for plan in plans:
            channel_id = int(plan["channel_id"])
            original = api_request(
                headers,
                "GET",
                "/api/channel/%d" % channel_id,
            )
            if not isinstance(original, dict):
                raise RuntimeError(
                    "channel %d payload is not an object" % channel_id
                )
            originals[channel_id] = original
            api_request(
                headers,
                "PUT",
                "/api/channel/",
                channel_payload(original, plan),
            )
            applied_channels.append(channel_id)

        for key in OPTION_KEYS:
            desired = manifest["desired_options"].get(key) or {}
            if desired == (original_options.get(key) or {}):
                continue
            update_option(headers, key, desired)
            applied_options.append(key)

        readback_errors = []
        for plan in plans:
            channel_id = int(plan["channel_id"])
            channel = api_request(
                headers,
                "GET",
                "/api/channel/%d" % channel_id,
            )
            readback_errors.extend(
                "channel %d: %s" % (channel_id, error)
                for error in verify_channel_readback(channel, plan)
            )
        api_options = option_rows_from_api(
            api_request(headers, "GET", "/api/option/")
        )
        desired_options = {
            key: manifest["desired_options"].get(key) or {}
            for key in MANAGED_OPTION_KEYS
        }
        if api_options != desired_options:
            readback_errors.append("managed option readback mismatch")
        readback_errors.extend(verify_abilities(plans))
        if channel_key_hashes(list(before_key_hashes)) != before_key_hashes:
            readback_errors.append("channel key fingerprint changed")
        if readback_errors:
            raise RuntimeError("; ".join(readback_errors))
        return {
            "applied_channels": applied_channels,
            "applied_options": applied_options,
            "key_hashes_preserved": True,
        }
    except Exception:
        for channel_id in reversed(applied_channels):
            api_request(
                headers,
                "PUT",
                "/api/channel/",
                restore_channel_payload(originals[channel_id]),
            )
        for key in reversed(applied_options):
            update_option(headers, key, original_options.get(key) or {})
        raise


def main() -> int:
    args = parse_args()
    output = Path(args.output)
    channels = load_channels()
    options = load_options()
    successes = load_recent_success()
    manifest = build_manifest(
        channels,
        options,
        successes,
        managed_channels=load_managed_channels(),
    )
    safe_manifest, manifest_sha256 = report_manifest(
        manifest,
        channels,
        options,
    )
    report = {
        "schema_version": 1,
        "mode": "apply" if args.apply else "dry-run",
        **safe_manifest,
    }
    if manifest["blockers"]:
        report["status"] = "blocked"
        report["applied"] = False
        report_sha256 = write_json(output, report)
        print(
            "status=blocked blockers=%d manifest_sha256=%s report_sha256=%s"
            % (
                len(manifest["blockers"]),
                manifest_sha256,
                report_sha256,
            )
        )
        return 2

    if not args.apply:
        report["status"] = "dry_run_ok"
        report["applied"] = False
        report_sha256 = write_json(output, report)
        print(
            "status=dry_run_ok channels=%d aliases=%d "
            "manifest_sha256=%s report_sha256=%s"
            % (
                len(manifest["channel_changes"]),
                len(manifest["hidden_concrete_models"]),
                manifest_sha256,
                report_sha256,
            )
        )
        return 0

    validate_apply_hash(args.expected_manifest_sha256, manifest_sha256)
    backup_dir, backup_hashes = create_backup(
        safe_manifest,
        channels,
        options,
    )
    report["backup_dir"] = str(backup_dir)
    report["backup_sha256"] = backup_hashes
    headers = root_headers()
    apply_result = apply_manifest(headers, manifest, options)
    report.update(apply_result)
    report["status"] = "applied"
    report["applied"] = True
    report["post_apply_channels_sha256"] = digest(
        sanitized_channel_inventory(load_channels())
    )
    report["post_apply_options_sha256"] = digest(load_options())
    report_sha256 = write_json(output, report)
    print(
        "status=applied channels=%d manifest_sha256=%s report_sha256=%s"
        % (
            len(manifest["channel_changes"]),
            manifest_sha256,
            report_sha256,
        )
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print("ERROR: %s" % exc, file=sys.stderr)
        raise SystemExit(1)
