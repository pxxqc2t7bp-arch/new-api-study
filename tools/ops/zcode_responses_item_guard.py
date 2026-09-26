#!/usr/bin/env python3
"""Safely configure the ZCode Responses input-item soft limit."""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
import re
import stat
import sys
import tempfile
import time
from contextlib import contextmanager
from copy import deepcopy
from pathlib import Path
from typing import Any, Callable, Iterator, Optional, Sequence
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


def _fsync_directory(path: Path) -> None:
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0)
    descriptor = os.open(path, flags)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _ensure_private_directory(path: Path, *, parents: bool = False) -> None:
    try:
        file_status = path.lstat()
    except FileNotFoundError:
        path.mkdir(mode=0o700, parents=parents)
        os.chmod(path, 0o700)
        _fsync_directory(path.parent)
        return
    if not stat.S_ISDIR(file_status.st_mode):
        raise ValueError("private directory path is not a directory: %s" % path)
    os.chmod(path, 0o700)


def atomic_write(
    path: Path,
    data: bytes,
    mode: int,
    *,
    on_replace: Optional[Callable[[tuple[int, int]], None]] = None,
) -> None:
    """Write bytes through a same-directory temporary file and replace."""
    file_descriptor, temporary_name = tempfile.mkstemp(
        prefix=".%s." % path.name,
        dir=str(path.parent),
    )
    temporary_path = Path(temporary_name)
    descriptor_open = True
    try:
        os.fchmod(file_descriptor, mode)
        temporary_status = os.fstat(file_descriptor)
        replacement_identity = (
            temporary_status.st_dev,
            temporary_status.st_ino,
        )
        with os.fdopen(file_descriptor, "wb") as handle:
            descriptor_open = False
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_path, path)
        if on_replace is not None:
            on_replace(replacement_identity)
        _fsync_directory(path.parent)
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
    _fsync_directory(path.parent)


def _config_labels(paths: Sequence[Path]) -> list[str]:
    if len(paths) != 2:
        raise ValueError("exactly two ZCode configuration paths are required")
    labels = [path.parent.name for path in paths]
    if set(labels) != {"v2", "cli"}:
        raise ValueError("configuration paths must identify v2 and cli")
    return labels


def _validate_output_path(
    paths: Sequence[Path],
    output_path: Optional[Path],
) -> None:
    if output_path is None:
        return
    resolved_output = output_path.resolve(strict=False)
    for path in paths:
        resolved_config = path.resolve(strict=False)
        aliases_config = resolved_output == resolved_config
        if output_path.exists() and path.exists():
            aliases_config = aliases_config or output_path.samefile(path)
        if aliases_config:
            raise ValueError(
                "output path must not alias a configuration path"
            )


@contextmanager
def _transaction_lock(root: Path) -> Iterator[None]:
    _ensure_private_directory(root, parents=True)
    lock_directory = root / ".zcode-responses-item-guard"
    _ensure_private_directory(lock_directory)
    lock_path = lock_directory / "transaction.lock"
    flags = (
        os.O_RDWR
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )
    lock_created = False
    try:
        descriptor = os.open(
            lock_path,
            flags | os.O_CREAT | os.O_EXCL,
            0o600,
        )
        lock_created = True
    except FileExistsError:
        descriptor = os.open(lock_path, flags)
    try:
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            raise RuntimeError("transaction lock is not a regular file")
        os.fchmod(descriptor, 0o600)
        if lock_created:
            _fsync_directory(lock_directory)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise RuntimeError(
                "configuration transaction is already active"
            ) from exc
        try:
            yield
        finally:
            fcntl.flock(descriptor, fcntl.LOCK_UN)
    finally:
        os.close(descriptor)


def create_backup(
    snapshots: list[dict[str, Any]],
    timestamp: str,
    backup_root: Optional[Path] = None,
) -> Path:
    """Create private content backups plus a secret-free hash manifest."""
    if not TIMESTAMP_PATTERN.fullmatch(timestamp):
        raise ValueError("backup timestamp is invalid")
    paths = [snapshot["path"] for snapshot in snapshots]
    labels = _config_labels(paths)
    root = backup_root or Path.home() / ".zcode" / "backups"
    _ensure_private_directory(root, parents=True)
    backup_dir = root / timestamp
    backup_dir.mkdir(mode=0o700, exist_ok=False)
    os.chmod(backup_dir, 0o700)
    _fsync_directory(root)

    hashes = {}
    backup_paths = {}
    for label, source, snapshot in zip(labels, paths, snapshots):
        _verify_transaction_state(snapshots, set())
        label_dir = backup_dir / label
        label_dir.mkdir(mode=0o700)
        os.chmod(label_dir, 0o700)
        _fsync_directory(backup_dir)
        destination = label_dir / source.name
        raw = snapshot["original"]
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
    file_status = path.lstat()
    if not stat.S_ISREG(file_status.st_mode):
        raise ValueError("configuration path is not a regular file: %s" % path)
    mode = stat.S_IMODE(file_status.st_mode)
    config = load_config(path)
    original = path.read_bytes()
    if _decode_config(original, path) != config:
        raise RuntimeError("configuration changed while being read: %s" % path)
    final_status = path.lstat()
    identity = (file_status.st_dev, file_status.st_ino)
    if (
        not stat.S_ISREG(final_status.st_mode)
        or (final_status.st_dev, final_status.st_ino) != identity
    ):
        raise RuntimeError("configuration changed while being read: %s" % path)
    updated, change = apply_guard(config)
    changed = updated != config
    updated_raw = _encode_config(updated) if changed else original
    return {
        "label": label,
        "path": path,
        "mode": mode,
        "identity": identity,
        "updated_identity": None,
        "original": original,
        "original_sha256": _sha256(original),
        "updated": updated,
        "updated_raw": updated_raw,
        "updated_sha256": _sha256(updated_raw),
        "change": change,
        "changed": changed,
    }


def _snapshot_output(path: Path) -> dict[str, Any]:
    try:
        file_status = path.lstat()
    except FileNotFoundError:
        return {
            "path": path,
            "existed": False,
            "mode": 0o600,
            "updated_identity": None,
            "updated_sha256": None,
        }
    if not stat.S_ISREG(file_status.st_mode):
        raise ValueError("output path is not a regular file: %s" % path)
    original = path.read_bytes()
    final_status = path.lstat()
    identity = (file_status.st_dev, file_status.st_ino)
    if (
        not stat.S_ISREG(final_status.st_mode)
        or (final_status.st_dev, final_status.st_ino) != identity
    ):
        raise RuntimeError("output changed while being read: %s" % path)
    return {
        "path": path,
        "existed": True,
        "identity": identity,
        "mode": stat.S_IMODE(file_status.st_mode),
        "updated_identity": None,
        "original": original,
        "original_sha256": _sha256(original),
        "updated_sha256": None,
    }


def _observed_file_state(
    path: Path,
    description: str = "configuration",
) -> tuple[tuple[int, int], bytes, int]:
    before = path.lstat()
    if not stat.S_ISREG(before.st_mode):
        raise RuntimeError("%s drift detected: %s" % (description, path))
    raw = path.read_bytes()
    after = path.lstat()
    before_identity = (before.st_dev, before.st_ino)
    after_identity = (after.st_dev, after.st_ino)
    if not stat.S_ISREG(after.st_mode) or before_identity != after_identity:
        raise RuntimeError("%s drift detected: %s" % (description, path))
    return before_identity, raw, stat.S_IMODE(after.st_mode)


def _matches_file_state(
    snapshot: dict[str, Any],
    state: tuple[tuple[int, int], bytes, int],
    *,
    updated: bool,
) -> bool:
    identity, raw, mode = state
    identity_key = "updated_identity" if updated else "identity"
    hash_key = "updated_sha256" if updated else "original_sha256"
    return (
        identity == snapshot[identity_key]
        and _sha256(raw) == snapshot[hash_key]
        and mode == snapshot["mode"]
    )


def _verify_transaction_state(
    snapshots: list[dict[str, Any]],
    written_labels: set[str],
) -> None:
    for snapshot in snapshots:
        state = _observed_file_state(snapshot["path"])
        if not _matches_file_state(
            snapshot,
            state,
            updated=snapshot["label"] in written_labels,
        ):
            raise RuntimeError(
                "configuration drift detected: %s" % snapshot["label"]
            )


def _verify_output_state(
    snapshot: dict[str, Any],
    *,
    updated: bool,
) -> None:
    path = snapshot["path"]
    if not snapshot["existed"] and not updated:
        try:
            path.lstat()
        except FileNotFoundError:
            return
        raise RuntimeError("output drift detected: %s" % path)

    state = _observed_file_state(path, "output")
    if not _matches_file_state(snapshot, state, updated=updated):
        raise RuntimeError("output drift detected: %s" % path)


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
    state = _observed_file_state(path)
    _, raw, _ = state
    config = _decode_config(raw, path)
    apply_guard(config)
    if config != snapshot["updated"]:
        raise RuntimeError("configuration readback mismatch")
    if not _matches_file_state(
        snapshot,
        state,
        updated=snapshot["changed"],
    ):
        raise RuntimeError("configuration readback hash mismatch")


def _restore_and_verify(
    backup_dir: Optional[Path],
    snapshots: list[dict[str, Any]],
    output_snapshot: Optional[dict[str, Any]] = None,
) -> list[str]:
    errors = []
    restored_identities = {}
    for snapshot in snapshots:
        try:
            state = _observed_file_state(snapshot["path"])
        except BaseException:
            errors.append("%s restore state" % snapshot["label"])
            continue
        if _matches_file_state(snapshot, state, updated=False):
            restored_identities[snapshot["label"]] = state[0]
            continue
        if not _matches_file_state(snapshot, state, updated=True):
            errors.append("%s third-party content" % snapshot["label"])
            continue
        try:
            if backup_dir is None:
                original = snapshot["original"]
            else:
                backup = _backup_file(backup_dir, snapshot)
                original = backup.read_bytes()
                if _sha256(original) != snapshot["original_sha256"]:
                    raise RuntimeError("backup hash mismatch")
            atomic_write(
                snapshot["path"],
                original,
                snapshot["mode"],
                on_replace=lambda identity, label=snapshot["label"]: (
                    restored_identities.__setitem__(label, identity)
                ),
            )
        except BaseException:
            errors.append("%s restore write" % snapshot["label"])
    for snapshot in snapshots:
        expected_identity = restored_identities.get(snapshot["label"])
        if expected_identity is None:
            continue
        try:
            identity, raw, mode = _observed_file_state(snapshot["path"])
            if (
                identity != expected_identity
                or _sha256(raw) != snapshot["original_sha256"]
            ):
                errors.append("%s restore hash" % snapshot["label"])
            if mode != snapshot["mode"]:
                errors.append("%s restore mode" % snapshot["label"])
        except BaseException:
            errors.append("%s restore validation" % snapshot["label"])
    if output_snapshot is not None:
        errors.extend(_restore_output(output_snapshot))
    return errors


def _restore_output(snapshot: dict[str, Any]) -> list[str]:
    path = snapshot["path"]
    if not snapshot["existed"]:
        try:
            path.lstat()
        except FileNotFoundError:
            return []
        try:
            state = _observed_file_state(path, "output")
        except BaseException:
            return ["output restore state"]
        if not _matches_file_state(snapshot, state, updated=True):
            return ["output third-party content"]
        try:
            path.unlink()
            _fsync_directory(path.parent)
        except BaseException:
            return ["output restore delete"]
        try:
            path.lstat()
        except FileNotFoundError:
            return []
        else:
            return ["output restore validation"]

    try:
        state = _observed_file_state(path, "output")
    except BaseException:
        return ["output restore state"]
    if _matches_file_state(snapshot, state, updated=False):
        return []
    if not _matches_file_state(snapshot, state, updated=True):
        return ["output third-party content"]
    restored_identity = []
    try:
        atomic_write(
            path,
            snapshot["original"],
            snapshot["mode"],
            on_replace=restored_identity.append,
        )
        identity, restored_raw, restored_mode = _observed_file_state(
            path, "output"
        )
    except BaseException:
        return ["output restore write"]
    errors = []
    if (
        identity != restored_identity[0]
        or _sha256(restored_raw) != snapshot["original_sha256"]
    ):
        errors.append("output restore hash")
    if restored_mode != snapshot["mode"]:
        errors.append("output restore mode")
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
    _validate_output_path(paths, output_path)
    labels = _config_labels(paths)
    snapshots = [
        _snapshot(path, label) for path, label in zip(paths, labels)
    ]
    report = _build_report(snapshots, applied=apply)
    if not apply:
        if output_path is not None:
            _write_report(output_path, report)
        _emit_status(report, status_sink)
        return report

    output_snapshot = (
        _snapshot_output(output_path) if output_path is not None else None
    )
    has_changes = any(snapshot["changed"] for snapshot in snapshots)
    backup_timestamp = timestamp or time.strftime(
        "%Y%m%dT%H%M%SZ", time.gmtime()
    )
    root = backup_root or Path.home() / ".zcode" / "backups"
    with _transaction_lock(root):
        backup_dir: Optional[Path] = None
        try:
            _verify_transaction_state(snapshots, set())
            if output_snapshot is not None:
                _verify_output_state(output_snapshot, updated=False)
            if has_changes:
                backup_dir = create_backup(
                    snapshots,
                    backup_timestamp,
                    root,
                )
                _verify_backups(backup_dir, snapshots)
                report["hash"]["backup_manifest_sha256"] = _file_sha256(
                    backup_dir / "manifest.json"
                )

            written_labels = set()
            for snapshot in snapshots:
                if snapshot["changed"]:
                    _verify_transaction_state(snapshots, written_labels)
                    atomic_write(
                        snapshot["path"],
                        snapshot["updated_raw"],
                        snapshot["mode"],
                        on_replace=lambda identity, current=snapshot: (
                            current.__setitem__(
                                "updated_identity",
                                identity,
                            )
                        ),
                    )
                    written_labels.add(snapshot["label"])
            for snapshot in snapshots:
                _verify_readback(snapshot)
            if output_path is not None:
                output_snapshot["updated_sha256"] = _sha256(
                    _encode_report(report)
                )
                _verify_transaction_state(snapshots, written_labels)
                _verify_output_state(output_snapshot, updated=False)
                _write_report(
                    output_path,
                    report,
                    on_replace=lambda identity: output_snapshot.__setitem__(
                        "updated_identity",
                        identity,
                    ),
                )
                _verify_output_state(output_snapshot, updated=True)
            _emit_status(report, status_sink)
        except BaseException as exc:
            rollback_errors = _restore_and_verify(
                backup_dir,
                snapshots,
                output_snapshot,
            )
            if rollback_errors:
                raise RuntimeError(
                    "configuration apply failed; "
                    "rollback verification failed: %s"
                    % ", ".join(rollback_errors)
                ) from exc
            if not isinstance(exc, Exception):
                raise
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


def _write_report(
    path: Path,
    report: dict[str, Any],
    *,
    on_replace: Optional[Callable[[tuple[int, int]], None]] = None,
) -> str:
    raw = _encode_report(report)
    mode = stat.S_IMODE(path.stat().st_mode) if path.exists() else 0o600
    atomic_write(path, raw, mode, on_replace=on_replace)
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
    sys.stdout.flush()


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
