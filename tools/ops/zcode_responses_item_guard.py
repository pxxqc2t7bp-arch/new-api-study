#!/usr/bin/env python3
"""Safely configure the ZCode Responses input-item soft limit."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import stat
import sys
import tempfile
import time
from copy import deepcopy
from pathlib import Path
from typing import Any, Callable, Optional, Sequence
from urllib.parse import urlsplit


PROVIDER_ID = "4e71cad5-e14a-4b21-8feb-7727ba10511d"
HEADER_NAME = "X-NewAPI-Responses-Input-Item-Soft-Limit"
HEADER_VALUE = "900"

EXPECTED_BASE_URL = "https://study.chenxy.online:10443/v1"
EXPECTED_HOST = "study.chenxy.online"
EXPECTED_PORT = 10443
TIMESTAMP_PATTERN = re.compile(r"\A\d{8}T\d{6}Z\Z")


def _decode_config(raw: bytes, path: Path) -> dict[str, Any]:
    try:
        config = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("invalid JSON configuration: %s" % path) from exc
    if not isinstance(config, dict):
        raise ValueError("configuration root must be an object: %s" % path)
    return config


def load_config(path: Path) -> dict[str, Any]:
    """Load one JSON configuration without exposing its content."""
    return _decode_config(path.read_bytes(), path)


def _validate_base_url(raw_url: Any) -> None:
    if not isinstance(raw_url, str):
        raise ValueError("target provider baseURL must be a string")
    try:
        parsed = urlsplit(raw_url)
        port = parsed.port
    except ValueError as exc:
        raise ValueError("target provider baseURL is invalid") from exc

    if (
        parsed.scheme.lower() != "https"
        or parsed.hostname is None
        or parsed.hostname.lower() != EXPECTED_HOST
        or port != EXPECTED_PORT
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in ("/v1", "/v1/")
        or parsed.query
        or parsed.fragment
    ):
        raise ValueError(
            "target provider baseURL must be %s" % EXPECTED_BASE_URL
        )


def _target_provider(config: dict[str, Any]) -> dict[str, Any]:
    providers = config.get("provider")
    if not isinstance(providers, dict):
        raise ValueError("provider must be an object")
    provider = providers.get(PROVIDER_ID)
    if not isinstance(provider, dict):
        raise ValueError("target provider is missing or invalid")
    if provider.get("kind") != "openai":
        raise ValueError("target provider kind must be openai")
    options = provider.get("options")
    if not isinstance(options, dict):
        raise ValueError("target provider options must be an object")
    _validate_base_url(options.get("baseURL"))
    if "headers" in options and not isinstance(options["headers"], dict):
        raise ValueError("target provider options.headers must be an object")
    return provider


def apply_guard(
    config: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Return a deep-copied config with only the target header changed."""
    if not isinstance(config, dict):
        raise ValueError("configuration root must be an object")
    provider = _target_provider(config)
    headers = provider["options"].get("headers")
    before = headers.get(HEADER_NAME) if headers is not None else None

    updated = deepcopy(config)
    updated_options = updated["provider"][PROVIDER_ID]["options"]
    if "headers" not in updated_options:
        updated_options["headers"] = {}
    updated_options["headers"][HEADER_NAME] = HEADER_VALUE
    return updated, {"before": before, "after": HEADER_VALUE}


def redacted_report(config: dict[str, Any]) -> dict[str, Any]:
    """Project a validated config onto an explicit, secret-free allowlist."""
    provider = _target_provider(config)
    options = provider["options"]
    headers = options.get("headers") or {}
    value = headers.get(HEADER_NAME)
    return {
        "provider": {"id": PROVIDER_ID},
        "header": {
            "name": HEADER_NAME,
            "value": HEADER_VALUE if value == HEADER_VALUE else None,
        },
    }


def _encode_config(config: dict[str, Any]) -> bytes:
    return (
        json.dumps(config, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    ).encode("utf-8")


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _file_sha256(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def atomic_write(path: Path, data: bytes, mode: int) -> None:
    """Write bytes through a same-directory temporary file and replace."""
    file_descriptor, temporary_name = tempfile.mkstemp(
        prefix=".%s." % path.name,
        dir=str(path.parent),
    )
    temporary_path = Path(temporary_name)
    descriptor_open = True
    try:
        os.fchmod(file_descriptor, mode)
        with os.fdopen(file_descriptor, "wb") as handle:
            descriptor_open = False
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_path, path)
    finally:
        if descriptor_open:
            os.close(file_descriptor)
        if temporary_path.exists():
            temporary_path.unlink()


def _write_private_file(path: Path, data: bytes) -> None:
    file_descriptor = os.open(
        path,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL,
        0o600,
    )
    descriptor_open = True
    try:
        os.fchmod(file_descriptor, 0o600)
        with os.fdopen(file_descriptor, "wb") as handle:
            descriptor_open = False
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
    finally:
        if descriptor_open:
            os.close(file_descriptor)


def _config_labels(paths: Sequence[Path]) -> list[str]:
    if len(paths) != 2:
        raise ValueError("exactly two ZCode configuration paths are required")
    labels = [path.parent.name for path in paths]
    if set(labels) != {"v2", "cli"}:
        raise ValueError("configuration paths must identify v2 and cli")
    return labels


def create_backup(
    paths: list[Path],
    timestamp: str,
    backup_root: Optional[Path] = None,
) -> Path:
    """Create private content backups plus a secret-free hash manifest."""
    if not TIMESTAMP_PATTERN.fullmatch(timestamp):
        raise ValueError("backup timestamp is invalid")
    labels = _config_labels(paths)
    root = backup_root or Path.home() / ".zcode" / "backups"
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(root, 0o700)
    backup_dir = root / timestamp
    backup_dir.mkdir(mode=0o700, exist_ok=False)
    os.chmod(backup_dir, 0o700)

    hashes = {}
    backup_paths = {}
    for label, source in zip(labels, paths):
        label_dir = backup_dir / label
        label_dir.mkdir(mode=0o700)
        os.chmod(label_dir, 0o700)
        destination = label_dir / source.name
        raw = source.read_bytes()
        _write_private_file(destination, raw)
        hashes[label] = _sha256(raw)
        backup_paths[label] = str(Path(label) / source.name)

    manifest = {
        "hash": hashes,
        "backup_path": backup_paths,
    }
    _write_private_file(
        backup_dir / "manifest.json",
        (
            json.dumps(
                manifest,
                ensure_ascii=True,
                indent=2,
                sort_keys=True,
            )
            + "\n"
        ).encode("ascii"),
    )
    return backup_dir


def _snapshot(path: Path, label: str) -> dict[str, Any]:
    file_status = path.stat()
    if not stat.S_ISREG(file_status.st_mode):
        raise ValueError("configuration path is not a regular file: %s" % path)
    mode = stat.S_IMODE(file_status.st_mode)
    config = load_config(path)
    original = path.read_bytes()
    if _decode_config(original, path) != config:
        raise RuntimeError("configuration changed while being read: %s" % path)
    updated, change = apply_guard(config)
    changed = updated != config
    updated_raw = _encode_config(updated) if changed else original
    return {
        "label": label,
        "path": path,
        "mode": mode,
        "original": original,
        "original_sha256": _sha256(original),
        "updated": updated,
        "updated_raw": updated_raw,
        "updated_sha256": _sha256(updated_raw),
        "change": change,
        "changed": changed,
    }


def _build_report(
    snapshots: list[dict[str, Any]],
    applied: bool,
) -> dict[str, Any]:
    projection = redacted_report(snapshots[0]["updated"])
    report = {
        "hash": {
            snapshot["label"]: {
                "before_sha256": snapshot["original_sha256"],
                "after_sha256": snapshot["updated_sha256"],
            }
            for snapshot in snapshots
        },
        "provider": projection["provider"],
        "header": projection["header"],
        "change": {},
    }
    for snapshot in snapshots:
        if not snapshot["changed"]:
            status = "unchanged"
        elif applied:
            status = "applied"
        else:
            status = "pending"
        report["change"][snapshot["label"]] = {"status": status}
    return report


def _backup_file(backup_dir: Path, snapshot: dict[str, Any]) -> Path:
    return backup_dir / snapshot["label"] / snapshot["path"].name


def _verify_backups(
    backup_dir: Path,
    snapshots: list[dict[str, Any]],
) -> None:
    if stat.S_IMODE(backup_dir.stat().st_mode) != 0o700:
        raise RuntimeError("backup directory mode verification failed")
    for snapshot in snapshots:
        backup = _backup_file(backup_dir, snapshot)
        if _file_sha256(backup) != snapshot["original_sha256"]:
            raise RuntimeError("backup hash verification failed")
        if stat.S_IMODE(backup.stat().st_mode) != 0o600:
            raise RuntimeError("backup file mode verification failed")
    manifest = backup_dir / "manifest.json"
    if stat.S_IMODE(manifest.stat().st_mode) != 0o600:
        raise RuntimeError("backup manifest mode verification failed")


def _verify_readback(snapshot: dict[str, Any]) -> None:
    path = snapshot["path"]
    config = load_config(path)
    apply_guard(config)
    if config != snapshot["updated"]:
        raise RuntimeError("configuration readback mismatch")
    if _file_sha256(path) != snapshot["updated_sha256"]:
        raise RuntimeError("configuration readback hash mismatch")
    if stat.S_IMODE(path.stat().st_mode) != snapshot["mode"]:
        raise RuntimeError("configuration mode changed")


def _restore_and_verify(
    backup_dir: Path,
    snapshots: list[dict[str, Any]],
) -> list[str]:
    errors = []
    for snapshot in snapshots:
        try:
            backup = _backup_file(backup_dir, snapshot)
            original = backup.read_bytes()
            if _sha256(original) != snapshot["original_sha256"]:
                raise RuntimeError("backup hash mismatch")
            atomic_write(
                snapshot["path"],
                original,
                snapshot["mode"],
            )
        except Exception:
            errors.append("%s restore write" % snapshot["label"])
    for snapshot in snapshots:
        try:
            if _file_sha256(snapshot["path"]) != snapshot["original_sha256"]:
                errors.append("%s restore hash" % snapshot["label"])
            if (
                stat.S_IMODE(snapshot["path"].stat().st_mode)
                != snapshot["mode"]
            ):
                errors.append("%s restore mode" % snapshot["label"])
        except Exception:
            errors.append("%s restore validation" % snapshot["label"])
    return errors


def execute(
    paths: list[Path],
    *,
    apply: bool,
    backup_root: Optional[Path] = None,
    timestamp: Optional[str] = None,
    output_path: Optional[Path] = None,
    status_sink: Optional[Callable[[str], None]] = None,
) -> dict[str, Any]:
    """Validate both configs, then dry-run or apply one rollback-safe update."""
    labels = _config_labels(paths)
    snapshots = [
        _snapshot(path, label) for path, label in zip(paths, labels)
    ]
    report = _build_report(snapshots, applied=apply)
    if not apply or not any(snapshot["changed"] for snapshot in snapshots):
        if output_path is not None:
            _write_report(output_path, report)
        _emit_status(report, status_sink)
        return report

    backup_timestamp = timestamp or time.strftime(
        "%Y%m%dT%H%M%SZ", time.gmtime()
    )
    backup_dir = create_backup(paths, backup_timestamp, backup_root)
    _verify_backups(backup_dir, snapshots)
    report["hash"]["backup_manifest_sha256"] = _file_sha256(
        backup_dir / "manifest.json"
    )

    try:
        for snapshot in snapshots:
            if snapshot["changed"]:
                atomic_write(
                    snapshot["path"],
                    snapshot["updated_raw"],
                    snapshot["mode"],
                )
        for snapshot in snapshots:
            _verify_readback(snapshot)
        if output_path is not None:
            _write_report(output_path, report)
        _emit_status(report, status_sink)
    except Exception as exc:
        rollback_errors = _restore_and_verify(backup_dir, snapshots)
        if rollback_errors:
            raise RuntimeError(
                "configuration apply failed; rollback verification failed: %s"
                % ", ".join(rollback_errors)
            ) from exc
        raise RuntimeError(
            "configuration apply failed; original files restored"
        ) from exc

    return report


def default_config_paths() -> list[Path]:
    root = Path.home() / ".zcode"
    return [
        root / "v2" / "config.json",
        root / "cli" / "config.json",
    ]


def parse_args(argv: Optional[Sequence[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Configure the ZCode Responses input-item guard.",
    )
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--output", required=True)
    return parser.parse_args(argv)


def _write_report(path: Path, report: dict[str, Any]) -> str:
    raw = _encode_report(report)
    mode = stat.S_IMODE(path.stat().st_mode) if path.exists() else 0o600
    atomic_write(path, raw, mode)
    return _sha256(raw)


def _encode_report(report: dict[str, Any]) -> bytes:
    return (
        json.dumps(report, ensure_ascii=True, indent=2, sort_keys=True) + "\n"
    ).encode("ascii")


def _emit_status(
    report: dict[str, Any],
    status_sink: Optional[Callable[[str], None]],
) -> None:
    if status_sink is None:
        return
    fields = [
        "hash=%s" % _sha256(_encode_report(report)),
        "provider=%s" % PROVIDER_ID,
        "header=%s:%s" % (HEADER_NAME, HEADER_VALUE),
        "change.v2=%s" % report["change"]["v2"]["status"],
        "change.cli=%s" % report["change"]["cli"]["status"],
    ]
    status_sink(" ".join(fields))


def _write_stdout_line(line: str) -> None:
    sys.stdout.write(line + "\n")


def main(argv: Optional[Sequence[str]] = None) -> int:
    args = parse_args(argv)
    try:
        execute(
            default_config_paths(),
            apply=args.apply,
            output_path=Path(args.output),
            status_sink=_write_stdout_line,
        )
    except Exception:
        print("ERROR: change=failed", file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
