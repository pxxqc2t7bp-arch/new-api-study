#!/usr/bin/env python3
"""Prove ZCode Responses header emission and reactive compaction locally."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
from collections import Counter
from contextlib import contextmanager
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Callable, Iterator, Mapping, Optional, Sequence
from urllib.parse import urlsplit


PROVIDER_ID = "zcode-loopback-probe"
MODEL = "glm-5.3"
CONTEXT_WINDOW = 1_048_576
HEADER_NAME = "X-NewAPI-Responses-Input-Item-Soft-Limit"
HEADER_VALUE = "900"
DUMMY_API_KEY = "zcode-loopback-dummy-key"
LOOPBACK_HOST = "127.0.0.1"
RESPONSES_PATH = "/v1/responses"
MAX_SUBPROCESS_TIMEOUT = 30
DEFAULT_SUBPROCESS_TIMEOUT = 25
DEFAULT_HISTORY_TURNS = 3
DEFAULT_MAX_REQUESTS = 12
EXPECTED_VERSION = "0.16.9"
EXPECTED_BUNDLE_SHA256 = (
    "b1df2ef3e5bd76c4af3ecb296bc003a10d3f13191a26610bd0ba940feadad529"
)
BUNDLE_PATH = (
    Path.home()
    / "Library"
    / "Application Support"
    / "ZCode"
    / "remote-assets-cache"
    / "releases"
    / "3.14.3"
    / "linux-x64"
    / "glm-content"
    / "464dc03681ff59fb43dbc9b36201b186b7f6ba1d1f18feaa89b8228ae771182a"
    / "glm"
    / "linux-x64"
    / "zcode.cjs"
)
BUILTIN_PROVIDER_CONFIG_PATH = (
    Path("/Applications")
    / "ZCode.app"
    / "Contents"
    / "Resources"
    / "config"
    / "provider"
    / "zcode-builtin.json"
)
COMPACTION_MARKER = (
    "your task is to create a detailed summary of the conversation so far"
)
SUMMARY_MARKER = "fixture summary"


class ProbeFailure(RuntimeError):
    def __init__(
        self,
        message: str,
        *,
        report: Optional[dict[str, Any]] = None,
    ) -> None:
        super().__init__(message)
        self.report = report


@dataclass(frozen=True)
class FixtureResponse:
    status: int
    content_type: str
    body: bytes
    headers: Mapping[str, str]


@dataclass(frozen=True)
class ProcessResult:
    returncode: int
    stdout: str
    stderr: str


def validate_loopback_base_url(raw_url: str) -> str:
    """Validate the exact disposable HTTP endpoint accepted by the probe."""
    if not isinstance(raw_url, str):
        raise ValueError("base URL must be an IPv4 loopback URL")
    try:
        parsed = urlsplit(raw_url)
        port = parsed.port
    except ValueError as exc:
        raise ValueError("base URL must be an IPv4 loopback URL") from exc
    if (
        parsed.scheme != "http"
        or parsed.hostname != LOOPBACK_HOST
        or port is None
        or not 1 <= port <= 65535
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in ("/v1", "/v1/")
        or parsed.query
        or parsed.fragment
    ):
        raise ValueError("base URL must be an IPv4 loopback URL")
    return "http://%s:%d/v1" % (LOOPBACK_HOST, port)


def _config(base_url: str) -> dict[str, Any]:
    return {
        "provider": {
            PROVIDER_ID: {
                "kind": "openai",
                "options": {
                    "apiKey": DUMMY_API_KEY,
                    "baseURL": base_url,
                    "headers": {HEADER_NAME: HEADER_VALUE},
                },
                "models": {
                    MODEL: {
                        "name": MODEL,
                        "contextWindow": CONTEXT_WINDOW,
                    }
                },
            }
        },
        "model": "%s/%s" % (PROVIDER_ID, MODEL),
    }


def _write_private_file(path: Path, data: bytes) -> None:
    descriptor = os.open(
        path,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL,
        0o600,
    )
    descriptor_open = True
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb") as handle:
            descriptor_open = False
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
    finally:
        if descriptor_open:
            os.close(descriptor)


def write_isolated_configs(
    home: Path,
    base_url: str,
) -> tuple[Path, Path]:
    """Create only the two disposable ZCode configs required by the probe."""
    normalized_url = validate_loopback_base_url(base_url)
    paths = (
        home / ".zcode" / "v2" / "config.json",
        home / ".zcode" / "cli" / "config.json",
    )
    encoded = (
        json.dumps(
            _config(normalized_url),
            ensure_ascii=True,
            indent=2,
            sort_keys=True,
        )
        + "\n"
    ).encode()
    for path in paths:
        path.parent.mkdir(mode=0o700, parents=True)
        os.chmod(path.parent, 0o700)
        _write_private_file(path, encoded)
    return paths


def _sse(event_type: str, payload: dict[str, Any]) -> bytes:
    data = json.dumps(payload, ensure_ascii=True, separators=(",", ":"))
    return ("event: %s\ndata: %s\n\n" % (event_type, data)).encode()


def responses_sse(text: str, response_id: str) -> bytes:
    """Build deterministic, text-only OpenAI Responses streaming events."""
    item_id = response_id + "_message"
    pending_item = {
        "id": item_id,
        "type": "message",
        "status": "in_progress",
        "role": "assistant",
        "content": [],
    }
    completed_item = {
        **pending_item,
        "status": "completed",
        "content": [
            {
                "type": "output_text",
                "text": text,
                "annotations": [],
            }
        ],
    }
    pending_response = {
        "id": response_id,
        "object": "response",
        "created_at": 0,
        "status": "in_progress",
        "model": MODEL,
        "output": [],
    }
    completed_response = {
        **pending_response,
        "status": "completed",
        "completed_at": 0,
        "output": [completed_item],
        "usage": {
            "input_tokens": 5,
            "output_tokens": 3,
            "total_tokens": 8,
        },
    }
    frames = [
        _sse(
            "response.created",
            {"type": "response.created", "response": pending_response},
        ),
        _sse(
            "response.output_item.added",
            {
                "type": "response.output_item.added",
                "output_index": 0,
                "item": pending_item,
            },
        ),
        _sse(
            "response.output_text.delta",
            {
                "type": "response.output_text.delta",
                "output_index": 0,
                "content_index": 0,
                "item_id": item_id,
                "delta": text,
            },
        ),
        _sse(
            "response.output_item.done",
            {
                "type": "response.output_item.done",
                "output_index": 0,
                "item": completed_item,
            },
        ),
        _sse(
            "response.completed",
            {
                "type": "response.completed",
                "response": completed_response,
            },
        ),
    ]
    return b"".join(frames)


def _strings(value: Any) -> Iterator[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, list):
        for child in value:
            yield from _strings(child)
    elif isinstance(value, dict):
        for child in value.values():
            yield from _strings(child)


def is_compaction_request(body: bytes) -> bool:
    try:
        value = json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return False
    return any(COMPACTION_MARKER in value.lower() for value in _strings(value))


def _request_object_with_array_input(body: bytes) -> Optional[dict[str, Any]]:
    try:
        value = json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return None
    if not isinstance(value, dict) or not isinstance(value.get("input"), list):
        return None
    return value


def _contains_marker(value: Any, marker: str) -> bool:
    return any(marker in text.lower() for text in _strings(value))


def _header(headers: Mapping[str, str], name: str) -> Optional[str]:
    lowered_name = name.lower()
    for candidate, value in headers.items():
        if candidate.lower() == lowered_name:
            return value
    return None


def _json_response(
    status: int,
    payload: dict[str, Any],
) -> FixtureResponse:
    return FixtureResponse(
        status=status,
        content_type="application/json",
        body=json.dumps(
            payload,
            ensure_ascii=True,
            separators=(",", ":"),
            sort_keys=True,
        ).encode(),
        headers={},
    )


def _text_response(text: str, response_id: str) -> FixtureResponse:
    return FixtureResponse(
        status=200,
        content_type="text/event-stream",
        body=responses_sse(text, response_id),
        headers={"Cache-Control": "no-cache"},
    )


class FixtureState:
    """Bounded state machine for one isolated ZCode session."""

    def __init__(
        self,
        *,
        history_turns: int = DEFAULT_HISTORY_TURNS,
        max_requests: int = DEFAULT_MAX_REQUESTS,
    ) -> None:
        if history_turns < 0:
            raise ValueError("history turns must not be negative")
        if max_requests < history_turns + 4:
            raise ValueError("request limit is too small for the fixture")
        self.history_turns = history_turns
        self.max_requests = max_requests
        self.requests: list[dict[str, Any]] = []
        self.order: list[str] = []
        self.failure_reason: Optional[str] = None
        self._phase = "header_probe"
        self._history_completed = 0
        self._ordinary_overflow_item_count: Optional[int] = None
        self._automatic_retry_item_count: Optional[int] = None
        self._compaction_marker_observed = False
        self._retry_summary_observed = False
        self._lock = threading.Lock()

    def _expected_phase(self) -> str:
        if self._phase == "history":
            return "history"
        return self._phase

    def _record(
        self,
        path: str,
        headers: Mapping[str, str],
        body: bytes,
        phase: str,
        item_count: Optional[int],
    ) -> None:
        self.requests.append(
            {
                "path": path,
                "soft_limit": _header(headers, HEADER_NAME),
                "content_type": _header(headers, "Content-Type"),
                "body_bytes": len(body),
                "body_sha256": hashlib.sha256(body).hexdigest(),
                "item_count": item_count,
                "phase": phase,
            }
        )
        self.order.append(phase)

    def _fail(
        self,
        reason: str,
        status: int,
        code: str,
    ) -> FixtureResponse:
        if self.failure_reason is None:
            self.failure_reason = reason
        return _json_response(
            status,
            {
                "error": {
                    "message": reason,
                    "type": "invalid_request_error",
                    "param": None,
                    "code": code,
                }
            },
        )

    def handle_post(
        self,
        path: str,
        headers: Mapping[str, str],
        body: bytes,
    ) -> FixtureResponse:
        with self._lock:
            if len(self.requests) >= self.max_requests:
                return self._fail(
                    "request limit exceeded",
                    429,
                    "probe_request_limit",
                )

            phase = self._expected_phase()
            request_value = _request_object_with_array_input(body)
            item_count = (
                len(request_value["input"])
                if request_value is not None
                else None
            )
            self._record(path, headers, body, phase, item_count)
            if path != RESPONSES_PATH:
                return self._fail(
                    "unexpected request path",
                    404,
                    "probe_unexpected_path",
                )
            if _header(headers, HEADER_NAME) != HEADER_VALUE:
                return self._fail(
                    "soft-limit header mismatch",
                    400,
                    "probe_header_mismatch",
                )

            if (
                self._phase
                in {
                    "ordinary_overflow",
                    "compaction_summary",
                    "automatic_retry",
                }
                and request_value is None
            ):
                return self._fail(
                    (
                        "%s request must be a JSON object with array input"
                        % self._phase.replace("_", " ")
                    ),
                    400,
                    "probe_invalid_request_body",
                )

            if self._phase == "header_probe":
                self._phase = "history" if self.history_turns else "ordinary_overflow"
                return _json_response(
                    400,
                    {
                        "error": {
                            "message": "fixture header probe complete",
                            "type": "invalid_request_error",
                            "param": None,
                            "code": "probe_header_captured",
                        }
                    },
                )

            if self._phase == "history":
                if is_compaction_request(body):
                    return self._fail(
                        "unexpected early compaction request",
                        409,
                        "probe_order_mismatch",
                    )
                self._history_completed += 1
                if self._history_completed >= self.history_turns:
                    self._phase = "ordinary_overflow"
                return _text_response(
                    "fixture history answer",
                    "response_history_%d" % self._history_completed,
                )

            if self._phase == "ordinary_overflow":
                assert request_value is not None
                if _contains_marker(request_value, COMPACTION_MARKER):
                    return self._fail(
                        "ordinary overflow request not observed",
                        409,
                        "probe_order_mismatch",
                    )
                self._ordinary_overflow_item_count = item_count
                self._phase = "compaction_summary"
                return _json_response(
                    400,
                    {
                        "error": {
                            "message": "fixture context limit exceeded",
                            "type": "invalid_request_error",
                            "param": "input",
                            "code": "context_length_exceeded",
                        }
                    },
                )

            if self._phase == "compaction_summary":
                assert request_value is not None
                if not _contains_marker(request_value, COMPACTION_MARKER):
                    return self._fail(
                        "compaction request not observed",
                        409,
                        "probe_order_mismatch",
                    )
                self._compaction_marker_observed = True
                self._phase = "automatic_retry"
                return _text_response(
                    (
                        "<analysis>fixture analysis</analysis>"
                        "<summary>fixture summary</summary>"
                    ),
                    "response_compaction_summary",
                )

            if self._phase == "automatic_retry":
                assert request_value is not None
                assert item_count is not None
                assert self._ordinary_overflow_item_count is not None
                if _contains_marker(request_value, COMPACTION_MARKER):
                    return self._fail(
                        "automatic retry request not observed",
                        409,
                        "probe_order_mismatch",
                    )
                if not _contains_marker(request_value, SUMMARY_MARKER):
                    return self._fail(
                        "automatic retry did not include fixture summary",
                        409,
                        "probe_retry_summary_missing",
                    )
                if item_count >= self._ordinary_overflow_item_count:
                    return self._fail(
                        "automatic retry did not reduce input item count",
                        409,
                        "probe_retry_not_compacted",
                    )
                self._automatic_retry_item_count = item_count
                self._retry_summary_observed = True
                self._phase = "done"
                return _text_response(
                    "fixture final answer",
                    "response_automatic_retry",
                )

            return self._fail(
                "unexpected request after completion",
                409,
                "probe_request_after_completion",
            )

    @property
    def reactive_compaction(self) -> bool:
        expected_tail = [
            "ordinary_overflow",
            "compaction_summary",
            "automatic_retry",
        ]
        return (
            self.failure_reason is None
            and self._phase == "done"
            and len(self.order) >= len(expected_tail)
            and self.order[-len(expected_tail) :] == expected_tail
            and self._compaction_marker_observed
            and self._retry_summary_observed
            and self._ordinary_overflow_item_count is not None
            and self._automatic_retry_item_count is not None
            and (
                self._automatic_retry_item_count
                < self._ordinary_overflow_item_count
            )
        )

    @property
    def passed(self) -> bool:
        expected_order = [
            "header_probe",
            *(["history"] * self.history_turns),
            "ordinary_overflow",
            "compaction_summary",
            "automatic_retry",
        ]
        return (
            self.failure_reason is None
            and self._phase == "done"
            and self.reactive_compaction
            and self.order == expected_order
            and all(
                request["path"] == RESPONSES_PATH
                and request["soft_limit"] == HEADER_VALUE
                for request in self.requests
            )
        )


class _FixtureHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "ZCodeLoopbackProbe/1"

    def do_GET(self) -> None:
        if self.path == "/v1/models":
            response = _json_response(
                200,
                {
                    "object": "list",
                    "data": [
                        {
                            "id": MODEL,
                            "object": "model",
                            "owned_by": "loopback-probe",
                        }
                    ],
                },
            )
        else:
            response = _json_response(
                404,
                {
                    "error": {
                        "message": "not found",
                        "type": "invalid_request_error",
                        "param": None,
                        "code": "not_found",
                    }
                },
            )
        self._send(response)

    def do_POST(self) -> None:
        try:
            content_length = int(self.headers.get("Content-Length", ""))
        except ValueError:
            content_length = -1
        if content_length < 0 or content_length > 16 * 1024 * 1024:
            self._send(
                _json_response(
                    400,
                    {
                        "error": {
                            "message": "invalid content length",
                            "type": "invalid_request_error",
                            "param": None,
                            "code": "probe_invalid_content_length",
                        }
                    },
                )
            )
            return
        body = self.rfile.read(content_length)
        state: FixtureState = self.server.fixture_state  # type: ignore[attr-defined]
        self._send(state.handle_post(self.path, self.headers, body))

    def _send(self, response: FixtureResponse) -> None:
        self.send_response(response.status)
        self.send_header("Content-Type", response.content_type)
        self.send_header("Content-Length", str(len(response.body)))
        self.send_header("Connection", "close")
        for name, value in response.headers.items():
            if name.lower() == "location":
                raise RuntimeError("fixture redirects are forbidden")
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(response.body)

    def log_message(self, format_string: str, *args: object) -> None:
        return


@contextmanager
def fixture_server(state: FixtureState) -> Iterator[str]:
    server = ThreadingHTTPServer((LOOPBACK_HOST, 0), _FixtureHandler)
    server.daemon_threads = True
    server.fixture_state = state  # type: ignore[attr-defined]
    host, port = server.server_address[:2]
    if host != LOOPBACK_HOST:
        server.server_close()
        raise ProbeFailure("fixture server did not bind IPv4 loopback")
    base_url = validate_loopback_base_url(
        "http://%s:%d/v1" % (host, port)
    )
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield base_url
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def run_process(
    argv: Sequence[str],
    *,
    cwd: Path,
    env: Mapping[str, str],
    timeout: float,
    popen_factory: Callable[..., Any] = subprocess.Popen,
    killpg: Callable[[int, int], None] = os.killpg,
) -> ProcessResult:
    if timeout <= 0 or timeout > MAX_SUBPROCESS_TIMEOUT:
        raise ValueError("subprocess timeout must be at most 30 seconds")
    process = popen_factory(
        list(argv),
        cwd=str(cwd),
        env=dict(env),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        start_new_session=True,
    )
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except BaseException as exc:
        try:
            killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        except BaseException:
            pass
        try:
            process.communicate(timeout=min(5, timeout))
        except BaseException:
            pass
        if isinstance(exc, subprocess.TimeoutExpired):
            raise ProbeFailure("ZCode subprocess timed out") from exc
        raise
    return ProcessResult(
        returncode=process.returncode,
        stdout=stdout,
        stderr=stderr,
    )


def _file_sha256(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def _probe_environment(
    home: Path,
    temporary_root: Path,
    *,
    builtin_config_path: Path = BUILTIN_PROVIDER_CONFIG_PATH,
) -> dict[str, str]:
    path = os.environ.get("PATH", os.defpath)
    return {
        "HOME": str(home),
        "PATH": path,
        "TMPDIR": str(temporary_root),
        "XDG_CACHE_HOME": str(temporary_root / "xdg-cache"),
        "XDG_CONFIG_HOME": str(temporary_root / "xdg-config"),
        "XDG_DATA_HOME": str(temporary_root / "xdg-data"),
        "LANG": "C.UTF-8",
        "NO_COLOR": "1",
        "ZCODE_BUILTIN_PROVIDER_CONFIG_FILE": str(builtin_config_path),
        "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE": str(
            home / ".zcode" / "v2" / "provider_config.json"
        ),
    }


def _read_cli_version(
    node_path: str,
    bundle_path: Path,
    *,
    cwd: Path,
    env: Mapping[str, str],
    timeout: float,
) -> str:
    result = run_process(
        [node_path, str(bundle_path), "--version"],
        cwd=cwd,
        env=env,
        timeout=timeout,
    )
    if result.returncode != 0:
        raise ProbeFailure("ZCode version command failed")
    match = re.search(r"(?:^|\s)(\d+\.\d+\.\d+)(?:\s|$)", result.stdout)
    if match is None:
        raise ProbeFailure("ZCode version output was not recognized")
    return match.group(1)


def _cli_command(
    node_path: str,
    bundle_path: Path,
    *,
    prompt: str,
    workspace: Path,
    continuing: bool,
) -> list[str]:
    command = [
        node_path,
        str(bundle_path),
        "--prompt",
        prompt,
        "--mode",
        "plan",
        "--json",
        "--cwd",
        str(workspace),
    ]
    if continuing:
        command.append("--continue")
    return command


def _run_turn(
    node_path: str,
    bundle_path: Path,
    *,
    prompt: str,
    workspace: Path,
    env: Mapping[str, str],
    timeout: float,
    continuing: bool,
) -> ProcessResult:
    return run_process(
        _cli_command(
            node_path,
            bundle_path,
            prompt=prompt,
            workspace=workspace,
            continuing=continuing,
        ),
        cwd=workspace,
        env=env,
        timeout=timeout,
    )


def build_report(
    state: FixtureState,
    *,
    bundle_sha256: str,
    version: str,
) -> dict[str, Any]:
    counts = Counter(state.order)
    item_counts = {
        phase: next(
            (
                request["item_count"]
                for request in reversed(state.requests)
                if request["phase"] == phase
            ),
            None,
        )
        for phase in (
            "ordinary_overflow",
            "compaction_summary",
            "automatic_retry",
        )
    }
    overflow_count = item_counts["ordinary_overflow"]
    retry_count = item_counts["automatic_retry"]
    compaction_delta = (
        overflow_count - retry_count
        if isinstance(overflow_count, int) and isinstance(retry_count, int)
        else None
    )
    path = (
        RESPONSES_PATH
        if state.requests
        and all(request["path"] == RESPONSES_PATH for request in state.requests)
        else None
    )
    soft_limit = (
        HEADER_VALUE
        if state.requests
        and all(
            request["soft_limit"] == HEADER_VALUE
            for request in state.requests
        )
        else None
    )
    return {
        "path": path,
        "soft_limit": soft_limit,
        "request_counts": {
            "total": len(state.requests),
            "header_probe": counts["header_probe"],
            "history": counts["history"],
            "ordinary_overflow": counts["ordinary_overflow"],
            "compaction_summary": counts["compaction_summary"],
            "automatic_retry": counts["automatic_retry"],
        },
        "item_counts": item_counts,
        "compaction_delta": compaction_delta,
        "order": list(state.order),
        "reactive_compaction": state.reactive_compaction,
        "passed": state.passed,
        "bundle_sha256": bundle_sha256,
        "version": version,
    }


def run_probe(
    *,
    bundle_path: Path = BUNDLE_PATH,
    timeout: float = DEFAULT_SUBPROCESS_TIMEOUT,
    history_turns: int = DEFAULT_HISTORY_TURNS,
) -> dict[str, Any]:
    state = FixtureState(history_turns=history_turns)
    if not bundle_path.is_file():
        raise ProbeFailure("fixed ZCode bundle is missing")
    bundle_sha256 = _file_sha256(bundle_path)
    if bundle_sha256 != EXPECTED_BUNDLE_SHA256:
        raise ProbeFailure("fixed ZCode bundle hash mismatch")
    if not BUILTIN_PROVIDER_CONFIG_PATH.is_file():
        raise ProbeFailure("ZCode builtin provider asset is missing")
    node_path = shutil.which("node")
    if node_path is None:
        raise ProbeFailure("node executable is unavailable")

    with tempfile.TemporaryDirectory(
        prefix="zcode-responses-item-probe-"
    ) as temporary_directory:
        temporary_root = Path(temporary_directory)
        home = temporary_root / "home"
        workspace = temporary_root / "workspace"
        home.mkdir(mode=0o700)
        workspace.mkdir(mode=0o700)
        env = _probe_environment(home, temporary_root)

        version = _read_cli_version(
            node_path,
            bundle_path,
            cwd=workspace,
            env=env,
            timeout=timeout,
        )
        if version != EXPECTED_VERSION:
            raise ProbeFailure("ZCode CLI version mismatch")

        with fixture_server(state) as base_url:
            write_isolated_configs(home, base_url)

            before = len(state.requests)
            _run_turn(
                node_path,
                bundle_path,
                prompt="header-probe",
                workspace=workspace,
                env=env,
                timeout=timeout,
                continuing=False,
            )
            if (
                len(state.requests) != before + 1
                or state.order != ["header_probe"]
                or state.failure_reason is not None
            ):
                report = build_report(
                    state,
                    bundle_sha256=bundle_sha256,
                    version=version,
                )
                raise ProbeFailure(
                    "header probe request contract failed",
                    report=report,
                )

            for turn in range(history_turns):
                before = len(state.requests)
                result = _run_turn(
                    node_path,
                    bundle_path,
                    prompt="history-turn-%d" % (turn + 1),
                    workspace=workspace,
                    env=env,
                    timeout=timeout,
                    continuing=True,
                )
                if (
                    result.returncode != 0
                    or len(state.requests) != before + 1
                    or state.failure_reason is not None
                ):
                    report = build_report(
                        state,
                        bundle_sha256=bundle_sha256,
                        version=version,
                    )
                    raise ProbeFailure(
                        "history accumulation turn failed",
                        report=report,
                    )

            before = len(state.requests)
            result = _run_turn(
                node_path,
                bundle_path,
                prompt="reactive-compaction-probe",
                workspace=workspace,
                env=env,
                timeout=timeout,
                continuing=True,
            )
            if (
                result.returncode != 0
                or len(state.requests) != before + 3
                or not state.passed
            ):
                report = build_report(
                    state,
                    bundle_sha256=bundle_sha256,
                    version=version,
                )
                reason = (
                    state.failure_reason
                    or "reactive compaction sequence incomplete"
                )
                raise ProbeFailure(reason, report=report)

    return build_report(
        state,
        bundle_sha256=bundle_sha256,
        version=version,
    )


def _write_report(path: Path, report: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = (
        json.dumps(report, ensure_ascii=True, indent=2, sort_keys=True) + "\n"
    ).encode()
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=".%s." % path.name,
        dir=str(path.parent),
    )
    temporary_path = Path(temporary_name)
    descriptor_open = True
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb") as handle:
            descriptor_open = False
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_path, path)
    finally:
        if descriptor_open:
            os.close(descriptor)
        if temporary_path.exists():
            temporary_path.unlink()


def _empty_report() -> dict[str, Any]:
    return {
        "path": None,
        "soft_limit": None,
        "request_counts": {
            "total": 0,
            "header_probe": 0,
            "history": 0,
            "ordinary_overflow": 0,
            "compaction_summary": 0,
            "automatic_retry": 0,
        },
        "item_counts": {
            "ordinary_overflow": None,
            "compaction_summary": None,
            "automatic_retry": None,
        },
        "compaction_delta": None,
        "order": [],
        "reactive_compaction": False,
        "passed": False,
        "bundle_sha256": "",
        "version": "",
    }


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Probe ZCode Responses guard behavior against an isolated "
            "loopback fixture."
        )
    )
    parser.add_argument("--output", type=Path)
    arguments = parser.parse_args(argv)

    try:
        report = run_probe()
    except ProbeFailure as exc:
        report = exc.report or _empty_report()
        if arguments.output is not None:
            _write_report(arguments.output, report)
        else:
            print(json.dumps(report, sort_keys=True))
        print("probe failed: %s" % exc, file=sys.stderr)
        return 1

    if arguments.output is not None:
        _write_report(arguments.output, report)
    else:
        print(json.dumps(report, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
