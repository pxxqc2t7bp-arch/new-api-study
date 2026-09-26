#!/usr/bin/env python3
"""Safely configure the ZCode Responses input-item soft limit."""

from __future__ import annotations

import argparse
import ctypes
import errno
import fcntl
import hashlib
import json
import os
import re
import secrets
import stat
import sys
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
SHA256_PATTERN = re.compile(r"\A[0-9a-f]{64}\Z")
_UNCONDITIONAL_WRITE = object()
_MISSING_FILE_STATE = object()
_UNKNOWN_FILE_STATE = object()


class _RestoreErrors(list[str]):
    def __init__(self) -> None:
        super().__init__()
        self.fatal_error: Optional[BaseException] = None

    def record(self, message: str, error: BaseException) -> None:
        self.append(message)
        if self.fatal_error is None and not isinstance(error, Exception):
            self.fatal_error = error

    def include(self, errors: list[str]) -> None:
        self.extend(errors)
        if self.fatal_error is None:
            self.fatal_error = getattr(errors, "fatal_error", None)


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


@contextmanager
def _open_parent_directory(
    path: Path,
    *,
    create_parents: bool = False,
) -> Iterator[tuple[int, str]]:
    flags = (
        os.O_RDONLY
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_DIRECTORY", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )
    parent = path.parent
    if path.is_absolute():
        descriptor = os.open(path.anchor, flags)
        components = parent.parts[1:]
    else:
        descriptor = os.open(".", flags)
        components = parent.parts
    try:
        for component in components:
            if component in ("", "."):
                continue
            if component == "..":
                raise ValueError(
                    "path must not contain parent directory traversal"
                )
            component_created = False
            try:
                next_descriptor = os.open(
                    component,
                    flags,
                    dir_fd=descriptor,
                )
            except FileNotFoundError:
                if not create_parents:
                    raise
                try:
                    os.mkdir(component, mode=0o700, dir_fd=descriptor)
                    component_created = True
                except FileExistsError:
                    pass
                next_descriptor = os.open(
                    component,
                    flags,
                    dir_fd=descriptor,
                )
            if component_created:
                os.fchmod(next_descriptor, 0o700)
                os.fsync(descriptor)
            os.close(descriptor)
            descriptor = next_descriptor
        yield descriptor, path.name
    finally:
        os.close(descriptor)


def _rename_noreplace(
    source_dir_fd: int,
    source_name: str,
    destination_dir_fd: int,
    destination_name: str,
) -> None:
    """Atomically rename without replacing an existing destination."""
    libc = ctypes.CDLL(None, use_errno=True)
    source_raw = os.fsencode(source_name)
    destination_raw = os.fsencode(destination_name)
    if sys.platform == "darwin":
        rename_call = libc.renameatx_np
        rename_flag = 0x00000004
    elif sys.platform.startswith("linux"):
        try:
            rename_call = libc.renameat2
        except AttributeError as exc:
            raise RuntimeError(
                "atomic no-replace rename is unavailable"
            ) from exc
        rename_flag = 0x00000001
    else:
        raise RuntimeError("atomic no-replace rename is unavailable")
    rename_call.argtypes = [
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_uint,
    ]
    rename_call.restype = ctypes.c_int
    result = rename_call(
        source_dir_fd,
        source_raw,
        destination_dir_fd,
        destination_raw,
        rename_flag,
    )
    if result == 0:
        return
    error_number = ctypes.get_errno()
    if error_number == errno.EEXIST:
        raise FileExistsError(
            error_number,
            os.strerror(error_number),
            destination_name,
        )
    raise OSError(
        error_number,
        os.strerror(error_number),
        source_name,
        destination_name,
    )


def _rename_exchange(
    source_dir_fd: int,
    source_name: str,
    destination_dir_fd: int,
    destination_name: str,
) -> None:
    """Atomically exchange two directory entries."""
    libc = ctypes.CDLL(None, use_errno=True)
    source_raw = os.fsencode(source_name)
    destination_raw = os.fsencode(destination_name)
    if sys.platform == "darwin":
        rename_call = libc.renameatx_np
    elif sys.platform.startswith("linux"):
        try:
            rename_call = libc.renameat2
        except AttributeError as exc:
            raise RuntimeError(
                "atomic exchange rename is unavailable"
            ) from exc
    else:
        raise RuntimeError("atomic exchange rename is unavailable")
    rename_call.argtypes = [
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_uint,
    ]
    rename_call.restype = ctypes.c_int
    result = rename_call(
        source_dir_fd,
        source_raw,
        destination_dir_fd,
        destination_raw,
        0x00000002,
    )
    if result == 0:
        return
    error_number = ctypes.get_errno()
    raise OSError(
        error_number,
        os.strerror(error_number),
        source_name,
        destination_name,
    )


def _new_temporary_name(prefix: str) -> str:
    return "%s%s" % (prefix, secrets.token_hex(8))


def _create_temporary_file(
    parent_dir_fd: int,
    prefix: str,
) -> tuple[int, str]:
    flags = (
        os.O_WRONLY
        | os.O_CREAT
        | os.O_EXCL
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )
    for _attempt in range(128):
        name = _new_temporary_name(prefix)
        try:
            return os.open(name, flags, 0o600, dir_fd=parent_dir_fd), name
        except FileExistsError:
            continue
    raise FileExistsError("unable to allocate a temporary file")


def _observed_file_state_at(
    parent_dir_fd: int,
    name: str,
    path: Path,
    description: str = "configuration",
) -> tuple[tuple[int, int], bytes, int]:
    before = os.stat(name, dir_fd=parent_dir_fd, follow_symlinks=False)
    if not stat.S_ISREG(before.st_mode):
        raise RuntimeError("%s drift detected: %s" % (description, path))
    flags = (
        os.O_RDONLY
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )
    descriptor = os.open(name, flags, dir_fd=parent_dir_fd)
    try:
        opened = os.fstat(descriptor)
        opened_identity = (opened.st_dev, opened.st_ino)
        before_identity = (before.st_dev, before.st_ino)
        if (
            not stat.S_ISREG(opened.st_mode)
            or opened_identity != before_identity
        ):
            raise RuntimeError(
                "%s drift detected: %s" % (description, path)
            )
        chunks = []
        while True:
            chunk = os.read(descriptor, 1 << 20)
            if not chunk:
                break
            chunks.append(chunk)
        after_read = os.fstat(descriptor)
    finally:
        os.close(descriptor)
    after = os.stat(name, dir_fd=parent_dir_fd, follow_symlinks=False)
    after_identity = (after.st_dev, after.st_ino)
    if (
        not stat.S_ISREG(after_read.st_mode)
        or (after_read.st_dev, after_read.st_ino) != opened_identity
        or not stat.S_ISREG(after.st_mode)
        or after_identity != opened_identity
    ):
        raise RuntimeError("%s drift detected: %s" % (description, path))
    return opened_identity, b"".join(chunks), stat.S_IMODE(after.st_mode)


def _sanitize_temporary_file(
    parent_dir_fd: int,
    name: str,
    expected_identity: tuple[int, int],
) -> None:
    flags = (
        os.O_WRONLY
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )
    descriptor = os.open(name, flags, dir_fd=parent_dir_fd)
    try:
        opened = os.fstat(descriptor)
        opened_identity = (opened.st_dev, opened.st_ino)
        if (
            not stat.S_ISREG(opened.st_mode)
            or opened_identity != expected_identity
        ):
            raise RuntimeError(
                "temporary file identity verification failed"
            )
        permission_errors = []
        try:
            os.fchmod(descriptor, 0o600)
        except BaseException as fchmod_error:
            permission_errors.append(fchmod_error)
            try:
                named_before_chmod = os.stat(
                    name,
                    dir_fd=parent_dir_fd,
                    follow_symlinks=False,
                )
                if (
                    not stat.S_ISREG(named_before_chmod.st_mode)
                    or (
                        named_before_chmod.st_dev,
                        named_before_chmod.st_ino,
                    )
                    != opened_identity
                ):
                    raise RuntimeError(
                        "temporary file identity verification failed"
                    )
                os.chmod(
                    name,
                    0o600,
                    dir_fd=parent_dir_fd,
                    follow_symlinks=False,
                )
                named_after_chmod = os.stat(
                    name,
                    dir_fd=parent_dir_fd,
                    follow_symlinks=False,
                )
                if (
                    not stat.S_ISREG(named_after_chmod.st_mode)
                    or (
                        named_after_chmod.st_dev,
                        named_after_chmod.st_ino,
                    )
                    != opened_identity
                    or stat.S_IMODE(named_after_chmod.st_mode) != 0o600
                ):
                    raise RuntimeError(
                        "temporary file identity verification failed"
                    )
            except BaseException as chmod_error:
                permission_errors.append(chmod_error)
        os.fsync(descriptor)
        remaining = opened.st_size
        zeroes = b"\0" * min(1 << 20, max(remaining, 1))
        os.lseek(descriptor, 0, os.SEEK_SET)
        while remaining:
            written = os.write(
                descriptor,
                zeroes[: min(len(zeroes), remaining)],
            )
            if written <= 0:
                raise OSError("temporary file overwrite made no progress")
            remaining -= written
        os.fsync(descriptor)
        os.ftruncate(descriptor, 0)
        os.fsync(descriptor)
        named = os.stat(
            name,
            dir_fd=parent_dir_fd,
            follow_symlinks=False,
        )
        if (
            not stat.S_ISREG(named.st_mode)
            or (named.st_dev, named.st_ino) != opened_identity
        ):
            raise RuntimeError(
                "temporary file identity verification failed"
            )
        fatal_error = next(
            (
                error
                for error in permission_errors
                if not isinstance(error, Exception)
            ),
            None,
        )
        if fatal_error is not None:
            raise fatal_error.with_traceback(fatal_error.__traceback__)
        if stat.S_IMODE(named.st_mode) != 0o600:
            permission_error = (
                permission_errors[-1] if permission_errors else None
            )
            raise RuntimeError(
                "temporary file sanitization incomplete: mode is not 0600"
            ) from permission_error
    finally:
        os.close(descriptor)


def _cleanup_file_state_at(
    parent_dir_fd: int,
    name: str,
    path: Path,
    description: str,
    errors: list[BaseException],
) -> object:
    try:
        return _observed_file_state_at(
            parent_dir_fd,
            name,
            path,
            description,
        )
    except FileNotFoundError:
        return _MISSING_FILE_STATE
    except BaseException as error:
        errors.append(error)
        return _UNKNOWN_FILE_STATE


def _cleanup_state_pair(
    parent_dir_fd: int,
    temporary_name: str,
    target_name: str,
    path: Path,
    errors: list[BaseException],
) -> tuple[object, object]:
    target_state = _cleanup_file_state_at(
        parent_dir_fd,
        target_name,
        path,
        "cleanup target",
        errors,
    )
    temporary_state = _cleanup_file_state_at(
        parent_dir_fd,
        temporary_name,
        path,
        "cleanup temporary file",
        errors,
    )
    return target_state, temporary_state


def _recover_known_exchange(
    parent_dir_fd: int,
    temporary_name: str,
    target_name: str,
    path: Path,
    staged_pair: tuple[object, object],
    restored_pair: tuple[object, object],
    errors: list[BaseException],
) -> tuple[object, object]:
    for _attempt in range(2):
        current_pair = _cleanup_state_pair(
            parent_dir_fd,
            temporary_name,
            target_name,
            path,
            errors,
        )
        if current_pair == restored_pair:
            return current_pair
        if current_pair != staged_pair:
            errors.append(
                RuntimeError("cleanup exchange state verification failed")
            )
            return current_pair
        try:
            _rename_exchange(
                parent_dir_fd,
                temporary_name,
                parent_dir_fd,
                target_name,
            )
        except BaseException as error:
            errors.append(error)

    current_pair = _cleanup_state_pair(
        parent_dir_fd,
        temporary_name,
        target_name,
        path,
        errors,
    )
    if current_pair != restored_pair:
        errors.append(RuntimeError("cleanup exchange recovery failed"))
    return current_pair


def _unlink_known_temporary_once(
    parent_dir_fd: int,
    temporary_name: str,
    path: Path,
    known_states: tuple[tuple[tuple[int, int], bytes, int], ...],
    errors: list[BaseException],
    *,
    content_may_differ: bool = False,
) -> bool:
    temporary_state = _cleanup_file_state_at(
        parent_dir_fd,
        temporary_name,
        path,
        "cleanup temporary file",
        errors,
    )
    if temporary_state is _MISSING_FILE_STATE:
        return True
    state_is_known = temporary_state in known_states
    if (
        content_may_differ
        and temporary_state is not _UNKNOWN_FILE_STATE
    ):
        state_is_known = any(
            temporary_state[0] == identity
            and temporary_state[2] in (mode, 0o600)
            for identity, _content, mode in known_states
        )
    if (
        temporary_state is _UNKNOWN_FILE_STATE
        or not state_is_known
    ):
        if temporary_state is not _UNKNOWN_FILE_STATE:
            errors.append(RuntimeError("temporary file drift detected"))
        return False
    # The private random name, anchored dirfd, and inode precheck reduce
    # accidental drift. POSIX has no conditional unlink-by-inode syscall, so
    # a malicious same-UID process can still replace this entry between the
    # precheck and unlink; another precheck cannot eliminate that race.
    try:
        os.unlink(temporary_name, dir_fd=parent_dir_fd)
    except FileNotFoundError:
        return True
    except BaseException as error:
        errors.append(error)
        return False
    try:
        os.fsync(parent_dir_fd)
    except BaseException as error:
        errors.append(error)
    return True


def _sanitize_or_unlink_known_temporary(
    parent_dir_fd: int,
    temporary_name: str,
    path: Path,
    known_states: tuple[tuple[tuple[int, int], bytes, int], ...],
    errors: list[BaseException],
) -> bool:
    if _unlink_known_temporary_once(
        parent_dir_fd,
        temporary_name,
        path,
        known_states,
        errors,
    ):
        return True

    temporary_state = _cleanup_file_state_at(
        parent_dir_fd,
        temporary_name,
        path,
        "cleanup temporary file",
        errors,
    )
    if temporary_state is _MISSING_FILE_STATE:
        return True
    if (
        temporary_state is _UNKNOWN_FILE_STATE
        or temporary_state not in known_states
    ):
        if temporary_state is not _UNKNOWN_FILE_STATE:
            errors.append(RuntimeError("temporary file drift detected"))
        return False

    owned_state = (temporary_state,)
    try:
        _sanitize_temporary_file(
            parent_dir_fd,
            temporary_name,
            temporary_state[0],
        )
    except BaseException as error:
        errors.append(error)

    if _unlink_known_temporary_once(
        parent_dir_fd,
        temporary_name,
        path,
        owned_state,
        errors,
        content_may_differ=True,
    ):
        return True

    temporary_state = _cleanup_file_state_at(
        parent_dir_fd,
        temporary_name,
        path,
        "cleanup temporary file",
        errors,
    )
    return (
        temporary_state is not _MISSING_FILE_STATE
        and temporary_state is not _UNKNOWN_FILE_STATE
        and temporary_state[0] == owned_state[0][0]
        and temporary_state[2] == 0o600
        and temporary_state[1] == b""
    )


def _raise_cleanup_errors(
    message: str,
    errors: list[BaseException],
    cause: BaseException,
) -> None:
    fatal_error = next(
        (error for error in errors if not isinstance(error, Exception)),
        None,
    )
    if fatal_error is not None:
        raise fatal_error.with_traceback(fatal_error.__traceback__)
    error_types = ", ".join(type(error).__name__ for error in errors)
    raise RuntimeError("%s: %s" % (message, error_types)) from cause


def _exchange_target_if_matches(
    parent_dir_fd: int,
    replacement_name: str,
    target_name: str,
    path: Path,
    expected_state: tuple[tuple[int, int], bytes, int],
    replacement_state: tuple[tuple[int, int], bytes, int],
) -> None:
    observed_target_state = _observed_file_state_at(
        parent_dir_fd,
        target_name,
        path,
        "target",
    )
    if observed_target_state != expected_state:
        raise RuntimeError("target drift detected: %s" % path)

    exchange_error = None
    exchange_traceback = None
    try:
        _rename_exchange(
            parent_dir_fd,
            replacement_name,
            parent_dir_fd,
            target_name,
        )
    except BaseException as exc:
        exchange_error = exc
        exchange_traceback = exc.__traceback__

    try:
        displaced_state = _observed_file_state_at(
            parent_dir_fd,
            replacement_name,
            path,
            "target",
        )
    except BaseException as state_error:
        displaced_state = None
        verification_error = state_error
    else:
        verification_error = None

    if displaced_state == expected_state and exchange_error is None:
        return

    try:
        committed_state = _observed_file_state_at(
            parent_dir_fd,
            target_name,
            path,
            "replacement",
        )
    except BaseException as state_error:
        committed_state = None
        committed_error = state_error
    else:
        committed_error = None
    if committed_state == replacement_state:
        try:
            _rename_exchange(
                parent_dir_fd,
                replacement_name,
                parent_dir_fd,
                target_name,
            )
            restored_replacement = _observed_file_state_at(
                parent_dir_fd,
                replacement_name,
                path,
                "replacement",
            )
            if restored_replacement != replacement_state:
                raise RuntimeError(
                    "target exchange recovery verification failed: %s"
                    % path
                )
        except BaseException as recovery_error:
            fatal_error = next(
                (
                    error
                    for error in (
                        exchange_error,
                        verification_error,
                        committed_error,
                        recovery_error,
                    )
                    if error is not None
                    and not isinstance(error, Exception)
                ),
                None,
            )
            if fatal_error is not None:
                raise fatal_error.with_traceback(
                    fatal_error.__traceback__
                )
            raise RuntimeError(
                "target exchange recovery failed: %s" % path
            ) from recovery_error

    if exchange_error is not None:
        raise exchange_error.with_traceback(exchange_traceback)
    if verification_error is not None:
        raise verification_error
    if (
        committed_error is not None
        and not isinstance(committed_error, Exception)
    ):
        raise committed_error.with_traceback(committed_error.__traceback__)
    raise RuntimeError("target drift detected: %s" % path)


def _remove_if_matches(
    path: Path,
    expected_state: tuple[tuple[int, int], bytes, int],
) -> None:
    with _open_parent_directory(path) as (parent_dir_fd, target_name):
        placeholder_descriptor, temporary_name = _create_temporary_file(
            parent_dir_fd,
            ".%s." % target_name,
        )
        placeholder_open = True
        placeholder_status = os.fstat(placeholder_descriptor)
        placeholder_identity = (
            placeholder_status.st_dev,
            placeholder_status.st_ino,
        )
        placeholder_state = (
            placeholder_identity,
            b"",
            0o600,
        )
        temporary_cleanup_complete = False
        primary_error = None
        try:
            os.fchmod(placeholder_descriptor, 0o600)
            os.fsync(placeholder_descriptor)
            os.close(placeholder_descriptor)
            placeholder_open = False
            _exchange_target_if_matches(
                parent_dir_fd,
                temporary_name,
                target_name,
                path,
                expected_state,
                placeholder_state,
            )
            try:
                os.unlink(target_name, dir_fd=parent_dir_fd)
            except BaseException as removal_error:
                cleanup_errors = []
                _recover_known_exchange(
                    parent_dir_fd,
                    temporary_name,
                    target_name,
                    path,
                    (placeholder_state, expected_state),
                    (expected_state, placeholder_state),
                    cleanup_errors,
                )
                temporary_cleanup_complete = (
                    _sanitize_or_unlink_known_temporary(
                        parent_dir_fd,
                        temporary_name,
                        path,
                        (placeholder_state, expected_state),
                        cleanup_errors,
                    )
                )
                try:
                    os.fsync(parent_dir_fd)
                except BaseException as fsync_error:
                    cleanup_errors.append(fsync_error)
                if not temporary_cleanup_complete:
                    cleanup_errors.append(
                        RuntimeError(
                            "target removal temporary cleanup incomplete"
                        )
                    )
                if cleanup_errors:
                    _raise_cleanup_errors(
                        "target removal recovery failed",
                        [removal_error] + cleanup_errors,
                        removal_error,
                    )
                raise removal_error.with_traceback(
                    removal_error.__traceback__
                )
            os.fsync(parent_dir_fd)
            try:
                os.unlink(temporary_name, dir_fd=parent_dir_fd)
            except BaseException:
                try:
                    _rename_noreplace(
                        parent_dir_fd,
                        temporary_name,
                        parent_dir_fd,
                        target_name,
                    )
                except FileExistsError:
                    pass
                else:
                    temporary_cleanup_complete = True
                    os.fsync(parent_dir_fd)
                raise
            else:
                temporary_cleanup_complete = True
                os.fsync(parent_dir_fd)
            os.fsync(parent_dir_fd)
        except BaseException as error:
            primary_error = error
            raise
        finally:
            if placeholder_open:
                os.close(placeholder_descriptor)
            if not temporary_cleanup_complete:
                final_cleanup_errors = []
                temporary_cleanup_complete = (
                    _sanitize_or_unlink_known_temporary(
                        parent_dir_fd,
                        temporary_name,
                        path,
                        (placeholder_state, expected_state),
                        final_cleanup_errors,
                    )
                )
                if not temporary_cleanup_complete:
                    final_cleanup_errors.append(
                        RuntimeError(
                            "target removal temporary cleanup incomplete"
                        )
                    )
                if final_cleanup_errors:
                    _raise_cleanup_errors(
                        (
                            "target removal temporary cleanup incomplete"
                            if not temporary_cleanup_complete
                            else "target removal cleanup failed"
                        ),
                        (
                            [primary_error]
                            if primary_error is not None
                            else []
                        )
                        + final_cleanup_errors,
                        (
                            primary_error
                            if primary_error is not None
                            else final_cleanup_errors[0]
                        ),
                    )


def atomic_write(
    path: Path,
    data: bytes,
    mode: Optional[int],
    *,
    on_replace: Optional[Callable[[tuple[int, int]], None]] = None,
    expected_state: object = _UNCONDITIONAL_WRITE,
) -> None:
    """Write bytes through an anchored same-directory temporary file."""
    with _open_parent_directory(path) as (parent_dir_fd, target_name):
        if mode is None:
            try:
                target_status = os.stat(
                    target_name,
                    dir_fd=parent_dir_fd,
                    follow_symlinks=False,
                )
            except FileNotFoundError:
                mode = 0o600
            else:
                mode = stat.S_IMODE(target_status.st_mode)
        file_descriptor, temporary_name = _create_temporary_file(
            parent_dir_fd,
            ".%s." % target_name,
        )
        descriptor_open = True
        temporary_cleanup_complete = False
        temporary_status = os.fstat(file_descriptor)
        replacement_identity = (
            temporary_status.st_dev,
            temporary_status.st_ino,
        )
        replacement_state = (
            replacement_identity,
            data,
            mode,
        )
        primary_error = None
        try:
            os.fchmod(file_descriptor, mode)
            with os.fdopen(file_descriptor, "wb") as handle:
                descriptor_open = False
                handle.write(data)
                handle.flush()
                os.fsync(handle.fileno())
            if on_replace is not None:
                on_replace(replacement_identity)
            if expected_state is _UNCONDITIONAL_WRITE:
                os.rename(
                    temporary_name,
                    target_name,
                    src_dir_fd=parent_dir_fd,
                    dst_dir_fd=parent_dir_fd,
                )
                temporary_cleanup_complete = True
            elif expected_state is None:
                _rename_noreplace(
                    parent_dir_fd,
                    temporary_name,
                    parent_dir_fd,
                    target_name,
                )
                temporary_cleanup_complete = True
            else:
                _exchange_target_if_matches(
                    parent_dir_fd,
                    temporary_name,
                    target_name,
                    path,
                    expected_state,
                    replacement_state,
                )
                try:
                    os.unlink(
                        temporary_name,
                        dir_fd=parent_dir_fd,
                    )
                except FileNotFoundError:
                    temporary_cleanup_complete = True
                except BaseException as cleanup_error:
                    cleanup_errors = [cleanup_error]
                    current_pair = _cleanup_state_pair(
                        parent_dir_fd,
                        temporary_name,
                        target_name,
                        path,
                        cleanup_errors,
                    )
                    if current_pair == (
                        replacement_state,
                        expected_state,
                    ):
                        try:
                            os.unlink(
                                temporary_name,
                                dir_fd=parent_dir_fd,
                            )
                        except FileNotFoundError:
                            temporary_cleanup_complete = True
                        except BaseException as retry_error:
                            cleanup_errors.append(retry_error)
                        else:
                            temporary_cleanup_complete = True
                            try:
                                os.fsync(parent_dir_fd)
                            except BaseException as fsync_error:
                                cleanup_errors.append(fsync_error)
                    elif current_pair[1] is _MISSING_FILE_STATE:
                        temporary_cleanup_complete = True
                    else:
                        cleanup_errors.append(
                            RuntimeError(
                                "atomic replacement cleanup state "
                                "verification failed"
                            )
                        )

                    if not temporary_cleanup_complete:
                        _recover_known_exchange(
                            parent_dir_fd,
                            temporary_name,
                            target_name,
                            path,
                            (replacement_state, expected_state),
                            (expected_state, replacement_state),
                            cleanup_errors,
                        )
                        temporary_cleanup_complete = (
                            _sanitize_or_unlink_known_temporary(
                                parent_dir_fd,
                                temporary_name,
                                path,
                                (replacement_state, expected_state),
                                cleanup_errors,
                            )
                        )
                    try:
                        os.fsync(parent_dir_fd)
                    except BaseException as fsync_error:
                        cleanup_errors.append(fsync_error)
                    if not temporary_cleanup_complete:
                        cleanup_errors.append(
                            RuntimeError(
                                "atomic replacement temporary cleanup "
                                "incomplete"
                            )
                        )
                    _raise_cleanup_errors(
                        "atomic replacement cleanup failed",
                        cleanup_errors,
                        cleanup_error,
                    )
                else:
                    os.fsync(parent_dir_fd)
            os.fsync(parent_dir_fd)
        except BaseException as error:
            primary_error = error
            raise
        finally:
            if descriptor_open:
                os.close(file_descriptor)
            if not temporary_cleanup_complete:
                final_cleanup_errors = []
                temporary_cleanup_complete = (
                    _sanitize_or_unlink_known_temporary(
                        parent_dir_fd,
                        temporary_name,
                        path,
                        (replacement_state,)
                        if (
                            expected_state is _UNCONDITIONAL_WRITE
                            or expected_state is None
                        )
                        else (
                            replacement_state,
                            expected_state,
                        ),
                        final_cleanup_errors,
                    )
                )
                if not temporary_cleanup_complete:
                    final_cleanup_errors.append(
                        RuntimeError(
                            "atomic replacement temporary cleanup "
                            "incomplete"
                        )
                    )
                if final_cleanup_errors:
                    _raise_cleanup_errors(
                        (
                            "atomic replacement temporary cleanup incomplete"
                            if not temporary_cleanup_complete
                            else "atomic replacement cleanup failed"
                        ),
                        (
                            [primary_error]
                            if primary_error is not None
                            else []
                        )
                        + final_cleanup_errors,
                        (
                            primary_error
                            if primary_error is not None
                            else final_cleanup_errors[0]
                        ),
                    )


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


def _transaction_lock_directory(root: Path) -> Path:
    return root / ".zcode-responses-item-guard"


def _validate_transaction_output_path(
    root: Path,
    output_path: Optional[Path],
) -> None:
    if output_path is None:
        return
    lock_directory = _transaction_lock_directory(root)
    resolved_output = output_path.resolve(strict=False)
    resolved_lock_directory = lock_directory.resolve(strict=False)
    try:
        resolved_output.relative_to(resolved_lock_directory)
    except ValueError:
        pass
    else:
        raise ValueError(
            "output path must not alias the transaction lock directory"
        )
    if output_path.exists() and lock_directory.exists():
        for current_root, directory_names, file_names in os.walk(
            lock_directory,
            followlinks=False,
        ):
            for name in directory_names + file_names:
                protected_path = Path(current_root) / name
                try:
                    aliases_lock = output_path.samefile(protected_path)
                except FileNotFoundError:
                    continue
                if aliases_lock:
                    raise ValueError(
                        "output path must not alias the transaction lock "
                        "directory"
                    )


@contextmanager
def _stable_transaction_anchor_lock(root: Path) -> Iterator[None]:
    directory_flags = (
        os.O_RDONLY
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_DIRECTORY", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )
    anchor = root.anchor if root.is_absolute() else "."
    descriptor = os.open(anchor, directory_flags)
    try:
        try:
            fcntl.flock(
                descriptor,
                fcntl.LOCK_EX | fcntl.LOCK_NB,
            )
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


@contextmanager
def _transaction_lock(
    root: Path,
    *,
    create_root: bool = True,
) -> Iterator[None]:
    directory_flags = (
        os.O_RDONLY
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_DIRECTORY", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )
    with _stable_transaction_anchor_lock(root), _open_parent_directory(
        root, create_parents=create_root
    ) as (
        root_parent_descriptor,
        root_name,
    ):
        root_created = False
        if create_root:
            try:
                os.mkdir(
                    root_name,
                    mode=0o700,
                    dir_fd=root_parent_descriptor,
                )
                root_created = True
            except FileExistsError:
                pass
        try:
            root_descriptor = os.open(
                root_name,
                directory_flags,
                dir_fd=root_parent_descriptor,
            )
        except FileNotFoundError as exc:
            raise RuntimeError("backup root is missing") from exc
        try:
            opened_root_status = os.fstat(root_descriptor)
            if not stat.S_ISDIR(opened_root_status.st_mode):
                raise RuntimeError("backup root is not a directory")
            if create_root:
                os.fchmod(root_descriptor, 0o700)
            elif stat.S_IMODE(opened_root_status.st_mode) != 0o700:
                raise RuntimeError("backup root mode verification failed")
            if root_created:
                os.fsync(root_parent_descriptor)
            try:
                fcntl.flock(
                    root_descriptor,
                    fcntl.LOCK_EX | fcntl.LOCK_NB,
                )
            except BlockingIOError as exc:
                raise RuntimeError(
                    "configuration transaction is already active"
                ) from exc
            try:
                lock_directory_name = (
                    _transaction_lock_directory(root).name
                )
                lock_directory_created = False
                try:
                    os.mkdir(
                        lock_directory_name,
                        mode=0o700,
                        dir_fd=root_descriptor,
                    )
                    lock_directory_created = True
                except FileExistsError:
                    pass
                lock_directory_descriptor = os.open(
                    lock_directory_name,
                    directory_flags,
                    dir_fd=root_descriptor,
                )
                try:
                    lock_directory_status = os.fstat(
                        lock_directory_descriptor
                    )
                    if not stat.S_ISDIR(lock_directory_status.st_mode):
                        raise RuntimeError(
                            "transaction lock directory is not a directory"
                        )
                    os.fchmod(lock_directory_descriptor, 0o700)
                    if lock_directory_created:
                        os.fsync(root_descriptor)
                    lock_flags = (
                        os.O_RDWR
                        | getattr(os, "O_CLOEXEC", 0)
                        | getattr(os, "O_NOFOLLOW", 0)
                    )
                    lock_created = False
                    try:
                        descriptor = os.open(
                            "transaction.lock",
                            lock_flags | os.O_CREAT | os.O_EXCL,
                            0o600,
                            dir_fd=lock_directory_descriptor,
                        )
                        lock_created = True
                    except FileExistsError:
                        descriptor = os.open(
                            "transaction.lock",
                            lock_flags,
                            dir_fd=lock_directory_descriptor,
                        )
                    try:
                        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
                            raise RuntimeError(
                                "transaction lock is not a regular file"
                            )
                        os.fchmod(descriptor, 0o600)
                        if lock_created:
                            os.fsync(lock_directory_descriptor)
                        try:
                            fcntl.flock(
                                descriptor,
                                fcntl.LOCK_EX | fcntl.LOCK_NB,
                            )
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
                finally:
                    os.close(lock_directory_descriptor)
            finally:
                fcntl.flock(root_descriptor, fcntl.LOCK_UN)
        finally:
            os.close(root_descriptor)


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


def _read_private_backup_file(path: Path, description: str) -> bytes:
    try:
        before = path.lstat()
    except FileNotFoundError as exc:
        raise RuntimeError("%s is missing" % description) from exc
    if not stat.S_ISREG(before.st_mode):
        raise RuntimeError("%s is not a regular file" % description)
    if stat.S_IMODE(before.st_mode) != 0o600:
        raise RuntimeError("%s mode verification failed" % description)
    raw = path.read_bytes()
    after = path.lstat()
    if (
        not stat.S_ISREG(after.st_mode)
        or (before.st_dev, before.st_ino)
        != (after.st_dev, after.st_ino)
        or stat.S_IMODE(after.st_mode) != 0o600
    ):
        raise RuntimeError("%s drift detected" % description)
    return raw


def _load_rollback_backup(
    paths: Sequence[Path],
    labels: Sequence[str],
    backup_dir: Path,
) -> tuple[list[dict[str, Any]], str]:
    try:
        backup_status = backup_dir.lstat()
    except FileNotFoundError as exc:
        raise RuntimeError("backup directory is missing") from exc
    if not stat.S_ISDIR(backup_status.st_mode):
        raise RuntimeError("backup directory is not a directory")
    if stat.S_IMODE(backup_status.st_mode) != 0o700:
        raise RuntimeError("backup directory mode verification failed")

    manifest_path = backup_dir / "manifest.json"
    manifest_raw = _read_private_backup_file(
        manifest_path,
        "backup manifest",
    )
    try:
        manifest = json.loads(manifest_raw.decode("ascii"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("backup manifest is invalid") from exc
    if not isinstance(manifest, dict) or set(manifest) != {
        "hash",
        "backup_path",
    }:
        raise ValueError("backup manifest structure is invalid")
    hashes = manifest["hash"]
    backup_paths = manifest["backup_path"]
    expected_labels = set(labels)
    if (
        not isinstance(hashes, dict)
        or set(hashes) != expected_labels
        or not isinstance(backup_paths, dict)
        or set(backup_paths) != expected_labels
    ):
        raise ValueError("backup manifest structure is invalid")

    snapshots = []
    for path, label in zip(paths, labels):
        expected_relative_path = str(Path(label) / path.name)
        digest = hashes[label]
        if (
            not isinstance(digest, str)
            or SHA256_PATTERN.fullmatch(digest) is None
            or backup_paths[label] != expected_relative_path
        ):
            raise ValueError("backup manifest entry is invalid: %s" % label)
        label_dir = backup_dir / label
        try:
            label_status = label_dir.lstat()
        except FileNotFoundError as exc:
            raise RuntimeError(
                "backup label directory is missing: %s" % label
            ) from exc
        if not stat.S_ISDIR(label_status.st_mode):
            raise RuntimeError(
                "backup label path is not a directory: %s" % label
            )
        if stat.S_IMODE(label_status.st_mode) != 0o700:
            raise RuntimeError(
                "backup label directory mode verification failed: %s"
                % label
            )

        backup_path = backup_dir / expected_relative_path
        backup_raw = _read_private_backup_file(
            backup_path,
            "backup configuration %s" % label,
        )
        if _sha256(backup_raw) != digest:
            raise RuntimeError(
                "backup hash verification failed: %s" % label
            )
        backup_config = _decode_config(backup_raw, backup_path)
        applied_config, change = apply_guard(backup_config)
        applied_raw = (
            _encode_config(applied_config)
            if applied_config != backup_config
            else backup_raw
        )

        identity, current_raw, current_mode = _observed_file_state(path)
        if _sha256(current_raw) != _sha256(applied_raw):
            raise RuntimeError(
                "rollback current state does not match applied backup: %s"
                % label
            )
        snapshots.append(
            {
                "label": label,
                "path": path,
                "mode": current_mode,
                "identity": identity,
                "updated_identity": None,
                "original": current_raw,
                "original_sha256": _sha256(current_raw),
                "updated": backup_config,
                "updated_raw": backup_raw,
                "updated_sha256": digest,
                "change": change,
                "changed": True,
                "backup_path": backup_path,
            }
        )

    final_backup_status = backup_dir.lstat()
    if (
        not stat.S_ISDIR(final_backup_status.st_mode)
        or (backup_status.st_dev, backup_status.st_ino)
        != (final_backup_status.st_dev, final_backup_status.st_ino)
        or stat.S_IMODE(final_backup_status.st_mode) != 0o700
    ):
        raise RuntimeError("backup directory drift detected")
    return snapshots, _sha256(manifest_raw)


def _validate_rollback_output_path(
    backup_dir: Path,
    snapshots: Sequence[dict[str, Any]],
    output_path: Optional[Path],
) -> None:
    if output_path is None:
        return
    _validate_transaction_output_path(
        backup_dir.parent,
        output_path,
    )
    resolved_output = output_path.resolve(strict=False)
    resolved_backup = backup_dir.resolve(strict=False)
    try:
        resolved_output.relative_to(resolved_backup)
    except ValueError:
        pass
    else:
        raise ValueError("output path must not be inside the backup directory")

    protected_paths = [backup_dir / "manifest.json"]
    protected_paths.extend(
        snapshot["backup_path"] for snapshot in snapshots
    )
    for protected_path in protected_paths:
        aliases_backup = resolved_output == protected_path.resolve(
            strict=False
        )
        if output_path.exists() and protected_path.exists():
            aliases_backup = aliases_backup or output_path.samefile(
                protected_path
            )
        if aliases_backup:
            raise ValueError("output path must not alias a backup file")


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
) -> _RestoreErrors:
    errors = _RestoreErrors()
    restored_identities = {}
    for snapshot in snapshots:
        try:
            state = _observed_file_state(snapshot["path"])
        except BaseException as error:
            errors.record("%s restore state" % snapshot["label"], error)
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
                expected_state=state,
            )
        except BaseException as error:
            errors.record("%s restore write" % snapshot["label"], error)
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
        except BaseException as error:
            errors.record(
                "%s restore validation" % snapshot["label"],
                error,
            )
    if output_snapshot is not None:
        errors.include(_restore_output(output_snapshot))
    return errors


def _restore_output(snapshot: dict[str, Any]) -> _RestoreErrors:
    errors = _RestoreErrors()
    path = snapshot["path"]
    if not snapshot["existed"]:
        try:
            path.lstat()
        except FileNotFoundError:
            return errors
        except BaseException as error:
            errors.record("output restore state", error)
            return errors
        try:
            state = _observed_file_state(path, "output")
        except BaseException as error:
            errors.record("output restore state", error)
            return errors
        if not _matches_file_state(snapshot, state, updated=True):
            errors.append("output third-party content")
            return errors
        try:
            _remove_if_matches(path, state)
        except BaseException as error:
            errors.record("output restore delete", error)
        try:
            path.lstat()
        except FileNotFoundError:
            return errors
        except BaseException as error:
            errors.record("output restore validation", error)
        else:
            errors.append("output restore validation")
        return errors

    try:
        state = _observed_file_state(path, "output")
    except BaseException as error:
        errors.record("output restore state", error)
        return errors
    if _matches_file_state(snapshot, state, updated=False):
        return errors
    if not _matches_file_state(snapshot, state, updated=True):
        errors.append("output third-party content")
        return errors
    restored_identity = []
    try:
        atomic_write(
            path,
            snapshot["original"],
            snapshot["mode"],
            on_replace=restored_identity.append,
            expected_state=state,
        )
    except BaseException as error:
        errors.record("output restore write", error)
    if not restored_identity:
        return errors
    try:
        identity, restored_raw, restored_mode = _observed_file_state(
            path, "output"
        )
    except BaseException as error:
        errors.record("output restore validation", error)
        return errors
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
    root = backup_root or Path.home() / ".zcode" / "backups"
    _validate_transaction_output_path(root, output_path)
    if not apply:
        if output_path is not None:
            _write_report(output_path, report)
        _emit_status(report, status_sink)
        return report

    has_changes = any(snapshot["changed"] for snapshot in snapshots)
    backup_timestamp = timestamp or time.strftime(
        "%Y%m%dT%H%M%SZ", time.gmtime()
    )
    with _transaction_lock(root):
        _validate_transaction_output_path(root, output_path)
        output_snapshot = (
            _snapshot_output(output_path)
            if output_path is not None
            else None
        )
        _validate_transaction_output_path(root, output_path)
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
                        expected_state=(
                            snapshot["identity"],
                            snapshot["original"],
                            snapshot["mode"],
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
                    expected_state=(
                        (
                            output_snapshot["identity"],
                            output_snapshot["original"],
                            output_snapshot["mode"],
                        )
                        if output_snapshot["existed"]
                        else None
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
                failure_message = (
                    "configuration apply failed; "
                    "rollback verification failed: %s"
                    % ", ".join(rollback_errors)
                )
            else:
                failure_message = (
                    "configuration apply failed; original files restored"
                )
            if not isinstance(exc, Exception):
                add_note = getattr(exc, "add_note", None)
                if callable(add_note):
                    add_note(failure_message)
                raise
            restore_fatal = getattr(
                rollback_errors,
                "fatal_error",
                None,
            )
            if restore_fatal is not None:
                add_note = getattr(restore_fatal, "add_note", None)
                if callable(add_note):
                    add_note(failure_message)
                raise restore_fatal.with_traceback(
                    restore_fatal.__traceback__
                ) from exc
            raise RuntimeError(failure_message) from exc

    return report


def rollback_backup(
    paths: list[Path],
    backup_dir: Path,
    *,
    output_path: Optional[Path] = None,
    status_sink: Optional[Callable[[str], None]] = None,
) -> dict[str, Any]:
    """Restore both configs from one validated apply backup transaction."""
    if not backup_dir.is_absolute():
        raise ValueError("rollback backup directory must be absolute")
    if backup_dir != Path(os.path.abspath(str(backup_dir))):
        raise ValueError(
            "rollback backup directory must be a normalized absolute path"
        )
    _validate_output_path(paths, output_path)
    labels = _config_labels(paths)
    try:
        backup_status = backup_dir.lstat()
    except FileNotFoundError as exc:
        raise RuntimeError("backup directory is missing") from exc
    if not stat.S_ISDIR(backup_status.st_mode):
        raise RuntimeError("backup directory is not a directory")

    with _transaction_lock(backup_dir.parent, create_root=False):
        snapshots, manifest_sha256 = _load_rollback_backup(
            paths,
            labels,
            backup_dir,
        )
        _validate_rollback_output_path(
            backup_dir,
            snapshots,
            output_path,
        )
        output_snapshot = (
            _snapshot_output(output_path) if output_path is not None else None
        )
        projection = redacted_report(snapshots[0]["updated"])
        report = {
            "operation": "rollback",
            "hash": {
                snapshot["label"]: {
                    "before_sha256": snapshot["original_sha256"],
                    "after_sha256": snapshot["updated_sha256"],
                }
                for snapshot in snapshots
            },
            "provider": projection["provider"],
            "header": projection["header"],
            "change": {
                snapshot["label"]: {"status": "rolled_back"}
                for snapshot in snapshots
            },
        }
        report["hash"]["backup_manifest_sha256"] = manifest_sha256

        try:
            written_labels = set()
            _verify_transaction_state(snapshots, written_labels)
            if output_snapshot is not None:
                _verify_output_state(output_snapshot, updated=False)
            for snapshot in snapshots:
                _verify_transaction_state(snapshots, written_labels)
                atomic_write(
                    snapshot["path"],
                    snapshot["updated_raw"],
                    snapshot["mode"],
                    on_replace=lambda identity, current=snapshot: (
                        current.__setitem__("updated_identity", identity)
                    ),
                    expected_state=(
                        snapshot["identity"],
                        snapshot["original"],
                        snapshot["mode"],
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
                    expected_state=(
                        (
                            output_snapshot["identity"],
                            output_snapshot["original"],
                            output_snapshot["mode"],
                        )
                        if output_snapshot["existed"]
                        else None
                    ),
                )
                _verify_output_state(output_snapshot, updated=True)
            _emit_status(report, status_sink)
        except BaseException as exc:
            rollback_errors = _restore_and_verify(
                None,
                snapshots,
                output_snapshot,
            )
            if rollback_errors:
                failure_message = (
                    "configuration rollback failed; "
                    "roll-forward verification failed: %s"
                    % ", ".join(rollback_errors)
                )
            else:
                failure_message = (
                    "configuration rollback failed; applied files restored"
                )
            if not isinstance(exc, Exception):
                add_note = getattr(exc, "add_note", None)
                if callable(add_note):
                    add_note(failure_message)
                raise
            restore_fatal = getattr(
                rollback_errors,
                "fatal_error",
                None,
            )
            if restore_fatal is not None:
                add_note = getattr(restore_fatal, "add_note", None)
                if callable(add_note):
                    add_note(failure_message)
                raise restore_fatal.with_traceback(
                    restore_fatal.__traceback__
                ) from exc
            raise RuntimeError(failure_message) from exc

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
    operation = parser.add_mutually_exclusive_group()
    operation.add_argument("--apply", action="store_true")
    operation.add_argument(
        "--rollback-backup",
        metavar="ABSOLUTE_BACKUP_DIR",
    )
    parser.add_argument("--output", required=True, metavar="PATH")
    return parser.parse_args(argv)


def _write_report(
    path: Path,
    report: dict[str, Any],
    *,
    on_replace: Optional[Callable[[tuple[int, int]], None]] = None,
    expected_state: object = _UNCONDITIONAL_WRITE,
) -> str:
    raw = _encode_report(report)
    if expected_state is _UNCONDITIONAL_WRITE:
        mode = None
    elif expected_state is None:
        mode = 0o600
    else:
        mode = expected_state[2]
    atomic_write(
        path,
        raw,
        mode,
        on_replace=on_replace,
        expected_state=expected_state,
    )
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
    fields = []
    if "operation" in report:
        fields.append("operation=%s" % report["operation"])
    header_value = report["header"]["value"]
    fields.extend(
        [
            "hash=%s" % _sha256(_encode_report(report)),
            "provider=%s" % PROVIDER_ID,
            "header=%s:%s"
            % (
                HEADER_NAME,
                header_value if header_value is not None else "absent",
            ),
            "change.v2=%s" % report["change"]["v2"]["status"],
            "change.cli=%s" % report["change"]["cli"]["status"],
        ]
    )
    status_sink(" ".join(fields))


def _write_stdout_line(line: str) -> None:
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def main(argv: Optional[Sequence[str]] = None) -> int:
    args = parse_args(argv)
    try:
        paths = default_config_paths()
        if args.rollback_backup is not None:
            rollback_backup(
                paths,
                Path(args.rollback_backup),
                output_path=Path(args.output),
                status_sink=_write_stdout_line,
            )
        else:
            execute(
                paths,
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
