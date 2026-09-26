import hashlib
import io
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from copy import deepcopy
from pathlib import Path
from unittest import mock

from tools.ops import zcode_responses_item_guard as guard


SECRET = "secret-fixture"
OTHER_SECRET = "unrelated-secret-fixture"
EXPECTED_BASE_URL = "https://study.chenxy.online:10443/v1"
DEFAULT_HEADERS = object()


def config_fixture(
    *,
    kind="openai",
    base_url=EXPECTED_BASE_URL,
    headers=DEFAULT_HEADERS,
    include_headers=True,
):
    options = {
        "apiKey": SECRET,
        "baseURL": base_url,
        "model": "glm-5.3",
        "models": {
            "glm-5.3": {
                "name": "GLM 5.3",
                "contextWindow": 1048576,
            }
        },
    }
    if include_headers:
        options["headers"] = (
            {"X-Unrelated": "keep-me"}
            if headers is DEFAULT_HEADERS
            else headers
        )
    return {
        "provider": {
            guard.PROVIDER_ID: {
                "kind": kind,
                "options": options,
            },
            "unrelated-provider": {
                "kind": "openai",
                "options": {
                    "apiKey": OTHER_SECRET,
                    "baseURL": "https://unrelated.example/v1",
                },
            },
        },
        "selectedModel": "glm-5.3",
        "unrelated": {"token": OTHER_SECRET},
    }


def json_bytes(value):
    return (
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    ).encode()


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def write_config(path, config, mode):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(json_bytes(config))
    path.chmod(mode)


class FailingStdout:
    def __init__(self, error):
        self.error = error
        self.write_count = 0

    def write(self, value):
        self.write_count += 1
        raise self.error


def assert_safe_report(test_case, report):
    serialized = json.dumps(report, ensure_ascii=False, sort_keys=True)
    lowered = serialized.lower()
    test_case.assertNotIn(SECRET, serialized)
    test_case.assertNotIn(OTHER_SECRET, serialized)
    for forbidden in (
        "secret",
        "key",
        "token",
        "cookie",
        "authorization",
        "jwt",
    ):
        test_case.assertNotIn(forbidden, lowered)
    test_case.assertEqual(
        set(report),
        {"hash", "provider", "header", "change"},
    )
    test_case.assertEqual(report["provider"], {"id": guard.PROVIDER_ID})
    test_case.assertEqual(
        report["header"],
        {"name": guard.HEADER_NAME, "value": guard.HEADER_VALUE},
    )
    test_case.assertEqual(set(report["change"]), {"v2", "cli"})
    for change in report["change"].values():
        test_case.assertEqual(set(change), {"status"})
        test_case.assertIn(change["status"], {"pending", "applied", "unchanged"})
    test_case.assertIn(
        set(report["hash"]),
        (
            {"v2", "cli"},
            {"v2", "cli", "backup_manifest_sha256"},
        ),
    )
    for label in ("v2", "cli"):
        test_case.assertEqual(
            set(report["hash"][label]),
            {"before_sha256", "after_sha256"},
        )

    def all_keys(value):
        if isinstance(value, dict):
            for key, child in value.items():
                yield key
                yield from all_keys(child)
        elif isinstance(value, list):
            for child in value:
                yield from all_keys(child)

    forbidden_keys = {
        "kind",
        "base_url",
        "before",
        "after",
        "backup_path",
    }
    test_case.assertTrue(forbidden_keys.isdisjoint(all_keys(report)))


class ApplyGuardTest(unittest.TestCase):
    def test_adds_header_without_mutating_or_dropping_configuration(self):
        original = config_fixture()
        before = deepcopy(original)

        updated, change = guard.apply_guard(original)

        self.assertEqual(original, before)
        self.assertIsNot(updated, original)
        self.assertEqual(
            updated["provider"][guard.PROVIDER_ID]["options"]["headers"][
                guard.HEADER_NAME
            ],
            guard.HEADER_VALUE,
        )
        self.assertEqual(
            updated["provider"][guard.PROVIDER_ID]["options"]["headers"][
                "X-Unrelated"
            ],
            "keep-me",
        )
        self.assertEqual(
            updated["provider"][guard.PROVIDER_ID]["options"]["apiKey"],
            SECRET,
        )
        self.assertEqual(
            updated["provider"][guard.PROVIDER_ID]["options"]["models"],
            before["provider"][guard.PROVIDER_ID]["options"]["models"],
        )
        self.assertEqual(
            updated["provider"]["unrelated-provider"],
            before["provider"]["unrelated-provider"],
        )
        self.assertEqual(change, {"before": None, "after": "900"})

    def test_creates_missing_headers_mapping(self):
        original = config_fixture(include_headers=False)

        updated, change = guard.apply_guard(original)

        self.assertEqual(
            updated["provider"][guard.PROVIDER_ID]["options"]["headers"],
            {guard.HEADER_NAME: guard.HEADER_VALUE},
        )
        self.assertEqual(change, {"before": None, "after": "900"})

    def test_second_application_is_idempotent(self):
        once, _ = guard.apply_guard(config_fixture())

        twice, change = guard.apply_guard(once)

        self.assertEqual(twice, once)
        self.assertIsNot(twice, once)
        self.assertEqual(change, {"before": "900", "after": "900"})

    def test_rejects_missing_provider(self):
        config = config_fixture()
        del config["provider"][guard.PROVIDER_ID]

        with self.assertRaisesRegex(ValueError, "target provider"):
            guard.apply_guard(config)

    def test_rejects_wrong_provider_kind(self):
        with self.assertRaisesRegex(ValueError, "kind"):
            guard.apply_guard(config_fixture(kind="anthropic"))

    def test_accepts_only_exact_structural_base_url(self):
        accepted = [
            EXPECTED_BASE_URL,
            EXPECTED_BASE_URL + "/",
        ]
        rejected = [
            "http://study.chenxy.online:10443/v1",
            "https://study.chenxy.online/v1",
            "https://study.chenxy.online:10444/v1",
            "https://study.chenxy.online.evil:10443/v1",
            "https://evil.study.chenxy.online:10443/v1",
            "https://user@study.chenxy.online:10443/v1",
            "https://study.chenxy.online:10443/v10",
            "https://study.chenxy.online:10443/v1//",
            "https://study.chenxy.online:10443/v1/responses",
            "https://study.chenxy.online:10443/v1?next=/v1",
            "https://study.chenxy.online:10443/v1#fragment",
        ]

        for base_url in accepted:
            with self.subTest(base_url=base_url):
                updated, _ = guard.apply_guard(
                    config_fixture(base_url=base_url)
                )
                self.assertEqual(
                    updated["provider"][guard.PROVIDER_ID]["options"][
                        "baseURL"
                    ],
                    base_url,
                )
        for base_url in rejected:
            with self.subTest(base_url=base_url):
                with self.assertRaisesRegex(ValueError, "baseURL"):
                    guard.apply_guard(config_fixture(base_url=base_url))

    def test_rejects_non_mapping_headers(self):
        for headers in (None, [], "Authorization: secret"):
            with self.subTest(headers=headers):
                with self.assertRaisesRegex(ValueError, "headers"):
                    guard.apply_guard(config_fixture(headers=headers))

    def test_redacted_report_is_an_explicit_allowlist_projection(self):
        config = config_fixture(
            headers={
                guard.HEADER_NAME: guard.HEADER_VALUE,
                "Authorization": "Bearer " + OTHER_SECRET,
            }
        )
        config["provider"][guard.PROVIDER_ID]["options"]["cookie"] = SECRET

        report = guard.redacted_report(config)

        self.assertEqual(
            report,
            {
                "provider": {"id": guard.PROVIDER_ID},
                "header": {
                    "name": guard.HEADER_NAME,
                    "value": guard.HEADER_VALUE,
                },
            },
        )
        serialized = json.dumps(report, ensure_ascii=False, sort_keys=True)
        lowered = serialized.lower()
        for forbidden in (
            SECRET,
            OTHER_SECRET,
            "secret",
            "key",
            "token",
            "cookie",
            "authorization",
            "jwt",
        ):
            self.assertNotIn(forbidden.lower(), lowered)


class ConfigTransactionTest(unittest.TestCase):
    def setUp(self):
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary_directory.name)
        self.home = self.root / "home"
        self.paths = [
            self.home / ".zcode" / "v2" / "config.json",
            self.home / ".zcode" / "cli" / "config.json",
        ]
        self.originals = [
            config_fixture(),
            config_fixture(base_url=EXPECTED_BASE_URL + "/"),
        ]
        self.original_modes = [0o640, 0o600]
        for path, config, mode in zip(
            self.paths, self.originals, self.original_modes
        ):
            write_config(path, config, mode)
        self.original_bytes = [path.read_bytes() for path in self.paths]
        self.original_hashes = [sha256(data) for data in self.original_bytes]
        self.updated_bytes = []
        for config in self.originals:
            updated = deepcopy(config)
            updated["provider"][guard.PROVIDER_ID]["options"]["headers"][
                guard.HEADER_NAME
            ] = guard.HEADER_VALUE
            self.updated_bytes.append(json_bytes(updated))
        self.backup_root = self.home / ".zcode" / "backups"

    def tearDown(self):
        self.temporary_directory.cleanup()

    def execute(self, apply, timestamp="20260926T120000Z"):
        return guard.execute(
            self.paths,
            apply=apply,
            backup_root=self.backup_root,
            timestamp=timestamp,
        )

    def expected_report(self, status, manifest_sha256=None):
        hashes = {
            label: {
                "before_sha256": original_hash,
                "after_sha256": sha256(updated),
            }
            for label, original_hash, updated in zip(
                ("v2", "cli"),
                self.original_hashes,
                self.updated_bytes,
            )
        }
        if manifest_sha256 is not None:
            hashes["backup_manifest_sha256"] = manifest_sha256
        return {
            "hash": hashes,
            "provider": {"id": guard.PROVIDER_ID},
            "header": {
                "name": guard.HEADER_NAME,
                "value": guard.HEADER_VALUE,
            },
            "change": {
                "v2": {"status": status},
                "cli": {"status": status},
            },
        }

    def test_dry_run_does_not_write_or_create_backups(self):
        report = self.execute(apply=False)

        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        self.assertFalse(self.backup_root.exists())
        self.assertEqual(report, self.expected_report("pending"))
        assert_safe_report(self, report)

    def test_apply_preserves_modes_and_creates_protected_backups(self):
        report = self.execute(apply=True)
        backup_dir = self.backup_root / "20260926T120000Z"

        self.assertEqual(stat.S_IMODE(backup_dir.stat().st_mode), 0o700)
        for path, mode in zip(self.paths, self.original_modes):
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), mode)
            updated = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(
                updated["provider"][guard.PROVIDER_ID]["options"]["headers"][
                    guard.HEADER_NAME
                ],
                guard.HEADER_VALUE,
            )

        for label, original in zip(("v2", "cli"), self.original_bytes):
            backup = backup_dir / label / "config.json"
            self.assertEqual(backup.read_bytes(), original)
            self.assertEqual(stat.S_IMODE(backup.stat().st_mode), 0o600)

        manifest = backup_dir / "manifest.json"
        self.assertEqual(stat.S_IMODE(manifest.stat().st_mode), 0o600)
        manifest_text = manifest.read_text(encoding="utf-8")
        self.assertNotIn(SECRET, manifest_text)
        self.assertNotIn(OTHER_SECRET, manifest_text)
        self.assertEqual(
            report,
            self.expected_report(
                "applied",
                manifest_sha256=sha256(manifest.read_bytes()),
            ),
        )
        assert_safe_report(self, report)

    def test_invalid_second_config_is_rejected_before_backup_or_write(self):
        invalid = config_fixture(kind="anthropic")
        write_config(self.paths[1], invalid, self.original_modes[1])
        before = [path.read_bytes() for path in self.paths]

        with self.assertRaisesRegex(ValueError, "kind"):
            self.execute(apply=True)

        self.assertEqual([path.read_bytes() for path in self.paths], before)
        self.assertFalse(self.backup_root.exists())

    def test_second_write_failure_rolls_back_both_files_and_hashes(self):
        real_atomic_write = guard.atomic_write
        target_write_count = 0

        def fail_second_target_write(path, data, mode):
            nonlocal target_write_count
            if path in self.paths:
                target_write_count += 1
                if target_write_count == 2:
                    raise OSError("injected second write failure")
            return real_atomic_write(path, data, mode)

        with mock.patch.object(
            guard,
            "atomic_write",
            side_effect=fail_second_target_write,
        ):
            with self.assertRaisesRegex(
                RuntimeError, "configuration apply failed"
            ):
                self.execute(apply=True)

        self.assertEqual(
            [sha256(path.read_bytes()) for path in self.paths],
            self.original_hashes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )
        backup_dir = self.backup_root / "20260926T120000Z"
        for label in ("v2", "cli"):
            self.assertEqual(
                stat.S_IMODE(
                    (backup_dir / label / "config.json").stat().st_mode
                ),
                0o600,
            )

    def test_readback_validation_failure_rolls_back_both_files(self):
        real_load_config = guard.load_config
        load_count = 0

        def fail_first_readback(path):
            nonlocal load_count
            load_count += 1
            if load_count == 3:
                raise ValueError("injected readback validation failure")
            return real_load_config(path)

        with mock.patch.object(
            guard,
            "load_config",
            side_effect=fail_first_readback,
        ):
            with self.assertRaisesRegex(
                RuntimeError, "configuration apply failed"
            ):
                self.execute(apply=True)

        self.assertEqual(
            [sha256(path.read_bytes()) for path in self.paths],
            self.original_hashes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )

    def test_cli_apply_report_failure_rolls_back_both_configs_once(self):
        output = self.root / "apply.json"
        timestamp = "20260926T120001Z"
        report_write_count = 0

        def fail_report_write(path, report):
            nonlocal report_write_count
            report_write_count += 1
            self.assertEqual(path, output)
            self.assertEqual(
                [sha256(path.read_bytes()) for path in self.paths],
                [sha256(data) for data in self.updated_bytes],
            )
            raise OSError("injected report write failure")

        stderr = io.StringIO()
        with (
            mock.patch.object(
                guard,
                "default_config_paths",
                return_value=self.paths,
            ),
            mock.patch.object(guard.Path, "home", return_value=self.home),
            mock.patch.object(
                guard.time,
                "strftime",
                return_value=timestamp,
            ),
            mock.patch.object(
                guard,
                "_write_report",
                side_effect=fail_report_write,
            ),
            mock.patch.object(guard.sys, "stderr", stderr),
        ):
            result = guard.main(
                ["--apply", "--output", str(output)]
            )

        self.assertEqual(result, 1)
        self.assertEqual(stderr.getvalue(), "ERROR: change=failed\n")
        self.assertEqual(report_write_count, 1)
        self.assertEqual(
            [sha256(path.read_bytes()) for path in self.paths],
            self.original_hashes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )
        backup_dir = self.backup_root / timestamp
        self.assertTrue((backup_dir / "manifest.json").is_file())
        for label, original in zip(("v2", "cli"), self.original_bytes):
            self.assertEqual(
                (backup_dir / label / "config.json").read_bytes(),
                original,
            )

    def test_cli_apply_stdout_failure_rolls_back_both_configs_once(self):
        failures = (
            ("broken-pipe", BrokenPipeError("injected broken pipe")),
            ("os-error", OSError("injected stdout failure")),
        )
        for index, (name, error) in enumerate(failures, start=2):
            with self.subTest(error=name):
                output = self.root / ("%s.json" % name)
                timestamp = "20260926T12000%dZ" % index
                stdout = FailingStdout(error)
                stderr = io.StringIO()
                with (
                    mock.patch.object(
                        guard,
                        "default_config_paths",
                        return_value=self.paths,
                    ),
                    mock.patch.object(
                        guard.Path,
                        "home",
                        return_value=self.home,
                    ),
                    mock.patch.object(
                        guard.time,
                        "strftime",
                        return_value=timestamp,
                    ),
                    mock.patch.object(guard.sys, "stdout", stdout),
                    mock.patch.object(guard.sys, "stderr", stderr),
                ):
                    try:
                        result = guard.main(
                            ["--apply", "--output", str(output)]
                        )
                    except OSError as exc:
                        self.fail(
                            "main leaked stdout failure: %s"
                            % type(exc).__name__
                        )

                self.assertEqual(result, 1)
                self.assertEqual(stdout.write_count, 1)
                self.assertEqual(
                    stderr.getvalue(),
                    "ERROR: change=failed\n",
                )
                self.assertTrue(output.is_file())
                self.assertEqual(
                    [sha256(path.read_bytes()) for path in self.paths],
                    self.original_hashes,
                )
                self.assertEqual(
                    [
                        stat.S_IMODE(path.stat().st_mode)
                        for path in self.paths
                    ],
                    self.original_modes,
                )
                backup_dir = self.backup_root / timestamp
                self.assertTrue((backup_dir / "manifest.json").is_file())
                for label, original in zip(
                    ("v2", "cli"),
                    self.original_bytes,
                ):
                    self.assertEqual(
                        (backup_dir / label / "config.json").read_bytes(),
                        original,
                    )

    def test_cli_dry_run_stdout_failure_is_caught_without_rollback(self):
        output = self.root / "dry-run-stdout-failure.json"
        stdout = FailingStdout(BrokenPipeError("injected broken pipe"))
        stderr = io.StringIO()
        with (
            mock.patch.object(
                guard,
                "default_config_paths",
                return_value=self.paths,
            ),
            mock.patch.object(guard, "_restore_and_verify") as restore,
            mock.patch.object(guard.sys, "stdout", stdout),
            mock.patch.object(guard.sys, "stderr", stderr),
        ):
            try:
                result = guard.main(["--output", str(output)])
            except OSError as exc:
                self.fail(
                    "main leaked stdout failure: %s"
                    % type(exc).__name__
                )

        self.assertEqual(result, 1)
        self.assertEqual(stdout.write_count, 1)
        self.assertEqual(stderr.getvalue(), "ERROR: change=failed\n")
        restore.assert_not_called()
        self.assertTrue(output.is_file())
        self.assertEqual(
            [sha256(path.read_bytes()) for path in self.paths],
            self.original_hashes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )
        self.assertFalse(self.backup_root.exists())

    def test_cli_dry_run_report_failure_does_not_mutate_or_rollback(self):
        output = self.root / "dry-run-failure.json"
        stderr = io.StringIO()
        with (
            mock.patch.object(
                guard,
                "default_config_paths",
                return_value=self.paths,
            ),
            mock.patch.object(
                guard,
                "_write_report",
                side_effect=OSError("injected dry-run report failure"),
            ) as report_write,
            mock.patch.object(guard, "_restore_and_verify") as restore,
            mock.patch.object(guard.sys, "stderr", stderr),
        ):
            result = guard.main(["--output", str(output)])

        self.assertEqual(result, 1)
        self.assertEqual(stderr.getvalue(), "ERROR: change=failed\n")
        report_write.assert_called_once()
        restore.assert_not_called()
        self.assertEqual(
            [sha256(path.read_bytes()) for path in self.paths],
            self.original_hashes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )
        self.assertFalse(self.backup_root.exists())

    def test_cli_defaults_to_real_paths_under_home_and_stays_dry_run(self):
        output = self.root / "dry-run.json"
        script = Path(guard.__file__)
        environment = os.environ.copy()
        environment["HOME"] = str(self.home)

        result = subprocess.run(
            [
                sys.executable,
                str(script),
                "--output",
                str(output),
            ],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        report = json.loads(output.read_text(encoding="utf-8"))
        self.assertEqual(report, self.expected_report("pending"))
        self.assertEqual(
            result.stdout,
            "hash=%s provider=%s header=%s:%s "
            "change.v2=pending change.cli=pending\n"
            % (
                sha256(output.read_bytes()),
                guard.PROVIDER_ID,
                guard.HEADER_NAME,
                guard.HEADER_VALUE,
            ),
        )
        self.assertEqual(result.stderr, "")
        combined_output = result.stdout + output.read_text(encoding="utf-8")
        lowered = combined_output.lower()
        self.assertNotIn(SECRET, combined_output)
        self.assertNotIn(OTHER_SECRET, combined_output)
        for forbidden in (
            "secret",
            "key",
            "token",
            "cookie",
            "authorization",
            "jwt",
        ):
            self.assertNotIn(forbidden, lowered)
        assert_safe_report(self, report)


if __name__ == "__main__":
    unittest.main()
