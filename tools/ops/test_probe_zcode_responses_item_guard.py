import hashlib
import json
import os
import signal
import subprocess
import tempfile
import unittest
import urllib.error
import urllib.request
from pathlib import Path

from tools.ops import probe_zcode_responses_item_guard as probe


PROMPT_TEXT = "fixture prompt content must never be recorded"


def request_body(*contents):
    return json.dumps(
        {
            "model": probe.MODEL,
            "input": [
                {
                    "type": "message",
                    "role": "user",
                    "content": content,
                }
                for content in contents
            ],
            "stream": True,
        }
    ).encode()


BODY = request_body(
    PROMPT_TEXT,
    "fixture retained item one",
    "fixture retained item two",
)


def request_headers(soft_limit=probe.HEADER_VALUE):
    return {
        "Content-Type": "application/json",
        probe.HEADER_NAME: soft_limit,
        "Authorization": "Bearer " + probe.DUMMY_API_KEY,
    }


def compaction_body():
    return request_body(
        "Your task is to create a detailed summary of the conversation so far."
    )


def compacted_retry_body():
    return request_body(
        "fixture summary",
        "fixture request after compaction",
    )


class LoopbackURLTest(unittest.TestCase):
    def test_accepts_only_structural_ipv4_loopback_base_url(self):
        accepted = [
            "http://127.0.0.1:1/v1",
            "http://127.0.0.1:65535/v1",
            "http://127.0.0.1:43127/v1/",
        ]
        rejected = [
            "https://127.0.0.1:43127/v1",
            "http://localhost:43127/v1",
            "http://127.0.0.2:43127/v1",
            "http://127.0.0.1/v1",
            "http://127.0.0.1:0/v1",
            "http://127.0.0.1:65536/v1",
            "http://user@127.0.0.1:43127/v1",
            "http://127.0.0.1:43127",
            "http://127.0.0.1:43127/v1/responses",
            "http://127.0.0.1:43127/v1?redirect=https://example.com",
            "http://127.0.0.1:43127/v1#fragment",
            "http://127.0.0.1:43127//v1",
        ]

        for base_url in accepted:
            with self.subTest(base_url=base_url):
                self.assertEqual(
                    probe.validate_loopback_base_url(base_url),
                    base_url.rstrip("/"),
                )
        for base_url in rejected:
            with self.subTest(base_url=base_url):
                with self.assertRaisesRegex(ValueError, "loopback"):
                    probe.validate_loopback_base_url(base_url)


class IsolatedConfigTest(unittest.TestCase):
    def test_writes_minimal_v2_and_cli_configs_with_dummy_credentials(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            home = Path(temporary_directory) / "home"
            base_url = "http://127.0.0.1:43127/v1"

            paths = probe.write_isolated_configs(home, base_url)

            self.assertEqual(
                paths,
                (
                    home / ".zcode" / "v2" / "config.json",
                    home / ".zcode" / "cli" / "config.json",
                ),
            )
            self.assertEqual(paths[0].read_bytes(), paths[1].read_bytes())
            config = json.loads(paths[0].read_text())
            self.assertEqual(set(config), {"provider", "model"})
            self.assertEqual(set(config["provider"]), {probe.PROVIDER_ID})
            provider = config["provider"][probe.PROVIDER_ID]
            self.assertEqual(set(provider), {"kind", "models", "options"})
            self.assertEqual(provider["kind"], "openai")
            self.assertEqual(
                set(provider["options"]),
                {"apiKey", "baseURL", "headers"},
            )
            self.assertEqual(
                provider["options"],
                {
                    "apiKey": probe.DUMMY_API_KEY,
                    "baseURL": base_url,
                    "headers": {probe.HEADER_NAME: probe.HEADER_VALUE},
                },
            )
            self.assertEqual(
                provider["models"],
                {
                    probe.MODEL: {
                        "name": probe.MODEL,
                        "contextWindow": probe.CONTEXT_WINDOW,
                    }
                },
            )
            self.assertEqual(
                config["model"],
                "%s/%s" % (probe.PROVIDER_ID, probe.MODEL),
            )
            self.assertNotIn("secret-fixture", paths[0].read_text())
            self.assertEqual(paths[0].stat().st_mode & 0o777, 0o600)
            self.assertEqual(paths[1].stat().st_mode & 0o777, 0o600)

    def test_rejects_non_loopback_url_before_creating_home(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            home = Path(temporary_directory) / "home"

            with self.assertRaisesRegex(ValueError, "loopback"):
                probe.write_isolated_configs(
                    home,
                    "http://example.com:43127/v1",
                )

            self.assertFalse(home.exists())


class ResponsesFixtureTest(unittest.TestCase):
    def test_text_sse_is_deterministic_and_complete(self):
        first = probe.responses_sse("fixture answer", "response_fixture")
        second = probe.responses_sse("fixture answer", "response_fixture")

        self.assertEqual(first, second)
        events = []
        for frame in first.decode().strip().split("\n\n"):
            event_line, data_line = frame.splitlines()
            event_name = event_line.removeprefix("event: ")
            payload = json.loads(data_line.removeprefix("data: "))
            self.assertEqual(payload["type"], event_name)
            events.append((event_name, payload))
        self.assertEqual(
            [event_name for event_name, _ in events],
            [
                "response.created",
                "response.output_item.added",
                "response.output_text.delta",
                "response.output_item.done",
                "response.completed",
            ],
        )
        completed = events[-1][1]["response"]
        self.assertEqual(completed["status"], "completed")
        self.assertEqual(
            completed["output"][0]["content"][0]["text"],
            "fixture answer",
        )

    def test_compaction_detection_parses_json_without_retaining_content(self):
        self.assertTrue(probe.is_compaction_request(compaction_body()))
        self.assertFalse(probe.is_compaction_request(BODY))
        self.assertFalse(probe.is_compaction_request(b"not-json"))


class FixtureStateTest(unittest.TestCase):
    def setUp(self):
        self.state = probe.FixtureState(history_turns=3)
        self.headers = request_headers()

    def request(self, body=BODY, path=probe.RESPONSES_PATH, headers=None):
        return self.state.handle_post(
            path,
            self.headers if headers is None else headers,
            body,
        )

    def test_runs_header_history_overflow_compaction_retry_in_order(self):
        response = self.request()
        self.assertEqual(response.status, 400)
        self.assertIn(b"probe_header_captured", response.body)

        for _ in range(3):
            response = self.request()
            self.assertEqual(response.status, 200)
            self.assertEqual(response.content_type, "text/event-stream")

        overflow = self.request()
        self.assertEqual(overflow.status, 400)
        error = json.loads(overflow.body)
        self.assertEqual(
            error["error"],
            {
                "message": "fixture context limit exceeded",
                "type": "invalid_request_error",
                "param": "input",
                "code": "context_length_exceeded",
            },
        )

        summary = self.request(compaction_body())
        self.assertEqual(summary.status, 200)
        self.assertIn(b"<summary>fixture summary</summary>", summary.body)

        answer = self.request(compacted_retry_body())
        self.assertEqual(answer.status, 200)
        self.assertIn(b"fixture final answer", answer.body)

        self.assertEqual(
            self.state.order,
            [
                "header_probe",
                "history",
                "history",
                "history",
                "ordinary_overflow",
                "compaction_summary",
                "automatic_retry",
            ],
        )
        self.assertTrue(self.state.reactive_compaction)
        self.assertTrue(self.state.passed)
        self.assertEqual(len(self.state.requests), 7)
        serialized = json.dumps(self.state.requests, sort_keys=True)
        self.assertNotIn(PROMPT_TEXT, serialized)
        self.assertNotIn(probe.DUMMY_API_KEY, serialized)
        for request in self.state.requests:
            self.assertEqual(
                set(request),
                {
                    "path",
                    "soft_limit",
                    "content_type",
                    "body_bytes",
                    "body_sha256",
                    "item_count",
                    "phase",
                },
            )
            self.assertEqual(
                request["body_sha256"],
                hashlib.sha256(
                    compaction_body()
                    if request["phase"] == "compaction_summary"
                    else (
                        compacted_retry_body()
                        if request["phase"] == "automatic_retry"
                        else BODY
                    )
                ).hexdigest(),
            )
        self.assertEqual(
            [
                request["item_count"]
                for request in self.state.requests
                if request["phase"]
                in {
                    "ordinary_overflow",
                    "compaction_summary",
                    "automatic_retry",
                }
            ],
            [3, 1, 2],
        )

    def test_requires_compaction_marker_after_context_error(self):
        self.request()
        for _ in range(3):
            self.request()
        self.request()

        response = self.request()

        self.assertEqual(response.status, 409)
        self.assertFalse(self.state.passed)
        self.assertFalse(self.state.reactive_compaction)
        self.assertEqual(self.state.failure_reason, "compaction request not observed")

    def test_rejects_uncompacted_original_body_as_automatic_retry(self):
        self.request()
        for _ in range(3):
            self.request()
        self.request()
        self.request(compaction_body())

        response = self.request(BODY)

        self.assertEqual(response.status, 409)
        self.assertFalse(self.state.passed)
        self.assertFalse(self.state.reactive_compaction)
        self.assertEqual(
            self.state.failure_reason,
            "automatic retry did not include fixture summary",
        )

    def test_requires_automatic_retry_to_reduce_input_item_count(self):
        self.request()
        for _ in range(3):
            self.request()
        self.request()
        self.request(compaction_body())
        uncompressed_retry = request_body(
            "fixture summary",
            "fixture retained item one",
            "fixture retained item two",
        )

        response = self.request(uncompressed_retry)

        self.assertEqual(response.status, 409)
        self.assertFalse(self.state.passed)
        self.assertFalse(self.state.reactive_compaction)
        self.assertEqual(
            self.state.failure_reason,
            "automatic retry did not reduce input item count",
        )

    def test_requires_json_object_with_array_input_for_compaction_phases(self):
        invalid_bodies = (
            b"not-json",
            json.dumps(["not", "an", "object"]).encode(),
            json.dumps({"input": "not-an-array"}).encode(),
        )

        for phase in (
            "ordinary_overflow",
            "compaction_summary",
            "automatic_retry",
        ):
            for invalid_body in invalid_bodies:
                with self.subTest(phase=phase, invalid_body=invalid_body):
                    state = probe.FixtureState(history_turns=0)
                    state.handle_post(
                        probe.RESPONSES_PATH,
                        self.headers,
                        BODY,
                    )
                    if phase in {"compaction_summary", "automatic_retry"}:
                        state.handle_post(
                            probe.RESPONSES_PATH,
                            self.headers,
                            BODY,
                        )
                    if phase == "automatic_retry":
                        state.handle_post(
                            probe.RESPONSES_PATH,
                            self.headers,
                            compaction_body(),
                        )

                    response = state.handle_post(
                        probe.RESPONSES_PATH,
                        self.headers,
                        invalid_body,
                    )

                    self.assertEqual(response.status, 400)
                    self.assertFalse(state.passed)
                    self.assertFalse(state.reactive_compaction)
                    self.assertEqual(
                        state.failure_reason,
                        "%s request must be a JSON object with array input"
                        % phase.replace("_", " "),
                    )
                    self.assertIsNone(state.requests[-1]["item_count"])

    def test_rejects_wrong_path_or_header_without_redirect(self):
        wrong_path = self.request(path="/redirect")
        self.assertEqual(wrong_path.status, 404)
        self.assertNotIn("Location", wrong_path.headers)
        self.assertFalse(300 <= wrong_path.status < 400)

        state = probe.FixtureState(history_turns=3)
        wrong_header = state.handle_post(
            probe.RESPONSES_PATH,
            request_headers("901"),
            BODY,
        )
        self.assertEqual(wrong_header.status, 400)
        self.assertNotIn("Location", wrong_header.headers)
        self.assertFalse(state.passed)

    def test_bounds_total_request_count(self):
        state = probe.FixtureState(history_turns=0, max_requests=4)
        state.handle_post(probe.RESPONSES_PATH, self.headers, BODY)
        state.handle_post(probe.RESPONSES_PATH, self.headers, BODY)
        state.handle_post(
            probe.RESPONSES_PATH,
            self.headers,
            compaction_body(),
        )
        state.handle_post(
            probe.RESPONSES_PATH,
            self.headers,
            compacted_retry_body(),
        )

        response = state.handle_post(
            probe.RESPONSES_PATH,
            self.headers,
            BODY,
        )
        self.assertEqual(response.status, 429)
        self.assertEqual(len(state.requests), 4)
        self.assertEqual(state.failure_reason, "request limit exceeded")

    def test_live_fixture_server_never_redirects(self):
        state = probe.FixtureState(history_turns=0)
        with probe.fixture_server(state) as base_url:
            with self.assertRaises(urllib.error.HTTPError) as raised:
                urllib.request.urlopen(
                    base_url + "/redirect",
                    timeout=2,
                )

        self.assertEqual(raised.exception.code, 404)
        self.assertIsNone(raised.exception.headers.get("Location"))


class FakeProcess:
    def __init__(self, *, communicate_outcomes=(), returncode=0):
        self.pid = 4242
        self.returncode = returncode
        self.communicate_outcomes = list(communicate_outcomes)
        self.communicate_timeouts = []
        self.communicate_calls = 0

    def communicate(self, timeout):
        self.communicate_calls += 1
        self.communicate_timeouts.append(timeout)
        if self.communicate_outcomes:
            outcome = self.communicate_outcomes.pop(0)
            if isinstance(outcome, BaseException):
                raise outcome
            return outcome
        return "0.16.9\n", ""


class SubprocessTest(unittest.TestCase):
    def test_environment_routes_all_provider_state_to_temporary_home(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            root = Path(temporary_directory)
            home = root / "home"
            builtin = root / "zcode-builtin.json"

            env = probe._probe_environment(
                home,
                root,
                builtin_config_path=builtin,
            )

            self.assertEqual(
                env["ZCODE_BUILTIN_PROVIDER_CONFIG_FILE"],
                str(builtin),
            )
            self.assertEqual(
                env["ZCODE_PERSONAL_PROVIDER_CONFIG_FILE"],
                str(home / ".zcode" / "v2" / "provider_config.json"),
            )
            self.assertNotIn("ZCODE_DATA_BASE_DIR", env)
            for name in env:
                self.assertNotIn("TOKEN", name.upper())
                self.assertNotIn("KEY", name.upper())

    def test_uses_new_process_group_and_timeout_at_most_thirty_seconds(self):
        process = FakeProcess()
        calls = []

        def fake_popen(argv, **kwargs):
            calls.append((argv, kwargs))
            return process

        result = probe.run_process(
            ["node", "zcode.cjs", "--version"],
            cwd=Path("/tmp/workspace"),
            env={"HOME": "/tmp/home"},
            timeout=30,
            popen_factory=fake_popen,
        )

        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "0.16.9\n")
        self.assertEqual(process.communicate_timeouts, [30])
        self.assertTrue(calls[0][1]["start_new_session"])
        self.assertEqual(calls[0][1]["stdout"], subprocess.PIPE)
        self.assertEqual(calls[0][1]["stderr"], subprocess.PIPE)
        self.assertTrue(calls[0][1]["text"])

    def test_timeout_kills_process_group_and_raises_redacted_error(self):
        process = FakeProcess(
            communicate_outcomes=(
                subprocess.TimeoutExpired(["node", "zcode.cjs"], 7),
                subprocess.TimeoutExpired(["node", "zcode.cjs"], 5),
            )
        )
        killed = []

        with self.assertRaisesRegex(
            probe.ProbeFailure,
            "^ZCode subprocess timed out$",
        ) as raised:
            probe.run_process(
                ["node", "zcode.cjs", "--prompt", PROMPT_TEXT],
                cwd=Path("/private/tmp/private-workspace"),
                env={"HOME": "/private/tmp/private-home"},
                timeout=7,
                popen_factory=lambda argv, **kwargs: process,
                killpg=lambda pid, sig: killed.append((pid, sig)),
            )

        self.assertEqual(killed, [(process.pid, signal.SIGKILL)])
        self.assertEqual(process.communicate_timeouts, [7, 5])
        message = str(raised.exception)
        self.assertNotIn(PROMPT_TEXT, message)
        self.assertNotIn("/private/tmp", message)

    def test_keyboard_interrupt_kills_and_reaps_before_reraising(self):
        interrupted = KeyboardInterrupt("fixture interrupt")
        process = FakeProcess(communicate_outcomes=(interrupted,))
        killed = []

        with self.assertRaises(KeyboardInterrupt) as raised:
            probe.run_process(
                ["node", "zcode.cjs"],
                cwd=Path("/tmp/workspace"),
                env={"HOME": "/tmp/home"},
                timeout=7,
                popen_factory=lambda argv, **kwargs: process,
                killpg=lambda pid, sig: killed.append((pid, sig)),
            )

        self.assertIs(raised.exception, interrupted)
        self.assertEqual(killed, [(process.pid, signal.SIGKILL)])
        self.assertEqual(process.communicate_timeouts, [7, 5])

    def test_generic_communicate_error_kills_and_attempts_one_bounded_reap(self):
        communicate_error = RuntimeError("fixture communicate error")
        process = FakeProcess(
            communicate_outcomes=(
                communicate_error,
                subprocess.TimeoutExpired(["node", "zcode.cjs"], 5),
            )
        )
        killed = []

        with self.assertRaises(RuntimeError) as raised:
            probe.run_process(
                ["node", "zcode.cjs"],
                cwd=Path("/tmp/workspace"),
                env={"HOME": "/tmp/home"},
                timeout=7,
                popen_factory=lambda argv, **kwargs: process,
                killpg=lambda pid, sig: killed.append((pid, sig)),
            )

        self.assertIs(raised.exception, communicate_error)
        self.assertEqual(killed, [(process.pid, signal.SIGKILL)])
        self.assertEqual(process.communicate_timeouts, [7, 5])

    def test_rejects_timeout_above_bound_before_spawning(self):
        spawned = []

        with self.assertRaisesRegex(ValueError, "30 seconds"):
            probe.run_process(
                ["node", "zcode.cjs"],
                cwd=Path("/tmp/workspace"),
                env={"HOME": "/tmp/home"},
                timeout=31,
                popen_factory=lambda argv, **kwargs: spawned.append(argv),
            )

        self.assertEqual(spawned, [])


class ReportTest(unittest.TestCase):
    def test_report_is_allowlisted_and_redacted(self):
        state = probe.FixtureState(history_turns=0)
        state.handle_post(probe.RESPONSES_PATH, request_headers(), BODY)
        state.handle_post(probe.RESPONSES_PATH, request_headers(), BODY)
        state.handle_post(
            probe.RESPONSES_PATH,
            request_headers(),
            compaction_body(),
        )
        state.handle_post(
            probe.RESPONSES_PATH,
            request_headers(),
            compacted_retry_body(),
        )

        report = probe.build_report(
            state,
            bundle_sha256="b1df2ef3" + "0" * 56,
            version="0.16.9",
        )

        self.assertEqual(
            set(report),
            {
                "path",
                "soft_limit",
                "request_counts",
                "item_counts",
                "compaction_delta",
                "order",
                "reactive_compaction",
                "passed",
                "bundle_sha256",
                "version",
            },
        )
        self.assertEqual(report["path"], probe.RESPONSES_PATH)
        self.assertEqual(report["soft_limit"], probe.HEADER_VALUE)
        self.assertEqual(
            report["request_counts"],
            {
                "total": 4,
                "header_probe": 1,
                "history": 0,
                "ordinary_overflow": 1,
                "compaction_summary": 1,
                "automatic_retry": 1,
            },
        )
        self.assertEqual(
            report["item_counts"],
            {
                "ordinary_overflow": 3,
                "compaction_summary": 1,
                "automatic_retry": 2,
            },
        )
        self.assertEqual(report["compaction_delta"], 1)
        self.assertTrue(report["reactive_compaction"])
        self.assertTrue(report["passed"])
        serialized = json.dumps(report, sort_keys=True).lower()
        for forbidden in (
            PROMPT_TEXT.lower(),
            probe.DUMMY_API_KEY.lower(),
            "/users/",
            "/private/tmp",
            "authorization",
            "apikey",
            "cookie",
            "body",
            "prompt",
        ):
            self.assertNotIn(forbidden, serialized)


if __name__ == "__main__":
    unittest.main()
