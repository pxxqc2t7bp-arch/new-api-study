import hashlib
import io
import json
import os
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
from contextlib import contextmanager
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


def dir_fd_entry_matches_path(directory_fd, name, path):
    directory_status = os.fstat(directory_fd)
    parent_status = path.parent.stat()
    return (
        name == path.name
        and (directory_status.st_dev, directory_status.st_ino)
        == (parent_status.st_dev, parent_status.st_ino)
    )


def read_bytes_at(directory_fd, name):
    descriptor = os.open(name, os.O_RDONLY, dir_fd=directory_fd)
    try:
        chunks = []
        while True:
            chunk = os.read(descriptor, 1 << 20)
            if not chunk:
                return b"".join(chunks)
            chunks.append(chunk)
    finally:
        os.close(descriptor)


def assert_no_secret_temporary_files(test_case, root):
    secret_markers = (
        SECRET.encode("utf-8"),
        OTHER_SECRET.encode("utf-8"),
    )
    leaks = []
    for candidate in root.rglob("*"):
        if not candidate.name.startswith(".") or not candidate.is_file():
            continue
        content = candidate.read_bytes()
        if any(marker in content for marker in secret_markers):
            leaks.append(candidate)
    test_case.assertEqual(
        leaks,
        [],
        "secret_temp_leaks=%d" % len(leaks),
    )


def rename_noreplace_for_test(
    source_dir_fd,
    source_name,
    destination_dir_fd,
    destination_name,
    **_kwargs,
):
    try:
        os.stat(
            destination_name,
            dir_fd=destination_dir_fd,
            follow_symlinks=False,
        )
    except FileNotFoundError:
        pass
    else:
        raise FileExistsError(
            "destination exists: %s" % destination_name
        )
    os.rename(
        source_name,
        destination_name,
        src_dir_fd=source_dir_fd,
        dst_dir_fd=destination_dir_fd,
    )


class FailingStdout:
    def __init__(self, error):
        self.error = error
        self.write_count = 0

    def write(self, value):
        self.write_count += 1
        raise self.error


class FlushFailingStdout:
    def __init__(self, error):
        self.error = error
        self.write_count = 0
        self.flush_count = 0

    def write(self, value):
        self.write_count += 1
        return len(value)

    def flush(self):
        self.flush_count += 1
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
    expected_keys = {"hash", "provider", "header", "change"}
    if "operation" in report:
        expected_keys.add("operation")
        test_case.assertEqual(report["operation"], "rollback")
    test_case.assertEqual(set(report), expected_keys)
    test_case.assertEqual(report["provider"], {"id": guard.PROVIDER_ID})
    test_case.assertIn(
        report["header"],
        (
            {"name": guard.HEADER_NAME, "value": guard.HEADER_VALUE},
            {"name": guard.HEADER_NAME, "value": None},
        ),
    )
    test_case.assertEqual(set(report["change"]), {"v2", "cli"})
    for change in report["change"].values():
        test_case.assertEqual(set(change), {"status"})
        test_case.assertIn(
            change["status"],
            {"pending", "applied", "unchanged", "rolled_back"},
        )
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
        self.root = Path(self.temporary_directory.name).resolve()
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

    def test_dry_run_output_obeys_stable_lock_directory_policy(self):
        output = self.root / "ordinary-dry-run-report.json"

        report = guard.execute(
            self.paths,
            apply=False,
            backup_root=self.backup_root,
            output_path=output,
        )

        self.assertEqual(
            json.loads(output.read_text(encoding="ascii")),
            report,
        )
        self.assertFalse(self.backup_root.exists())

        lock_directory = (
            self.backup_root / ".zcode-responses-item-guard"
        )
        for protected_output in (
            lock_directory / "transaction.lock",
            lock_directory / "reports" / "dry-run.json",
        ):
            with self.subTest(output=protected_output):
                with self.assertRaisesRegex(ValueError, "transaction lock"):
                    guard.execute(
                        self.paths,
                        apply=False,
                        backup_root=self.backup_root,
                        output_path=protected_output,
                    )
                self.assertFalse(self.backup_root.exists())

    def test_output_aliases_are_rejected_before_dry_run_or_apply(self):
        for apply in (False, True):
            for alias_kind in ("same-path", "symlink", "hardlink"):
                with self.subTest(apply=apply, alias=alias_kind):
                    with tempfile.TemporaryDirectory(
                        dir=self.root
                    ) as temporary_directory:
                        root = Path(temporary_directory)
                        paths = [
                            root / ".zcode" / "v2" / "config.json",
                            root / ".zcode" / "cli" / "config.json",
                        ]
                        for path, config, mode in zip(
                            paths,
                            self.originals,
                            self.original_modes,
                        ):
                            write_config(path, config, mode)
                        if alias_kind == "same-path":
                            output = paths[0]
                        else:
                            output = root / ("%s.json" % alias_kind)
                            if alias_kind == "symlink":
                                output.symlink_to(paths[0])
                            else:
                                os.link(paths[0], output)
                        before = [path.read_bytes() for path in paths]
                        modes = [
                            stat.S_IMODE(path.stat().st_mode)
                            for path in paths
                        ]
                        backup_root = root / ".zcode" / "backups"

                        with self.assertRaisesRegex(
                            ValueError,
                            "output path must not alias a configuration path",
                        ):
                            guard.execute(
                                paths,
                                apply=apply,
                                backup_root=backup_root,
                                timestamp="20260926T120010Z",
                                output_path=output,
                            )

                        self.assertEqual(
                            [path.read_bytes() for path in paths],
                            before,
                        )
                        self.assertEqual(
                            [
                                stat.S_IMODE(path.stat().st_mode)
                                for path in paths
                            ],
                            modes,
                        )
                        self.assertFalse(backup_root.exists())

    def test_apply_output_rejects_transaction_lock_aliases(self):
        for alias_kind in ("exact", "child", "symlink", "hardlink"):
            with self.subTest(alias=alias_kind):
                with tempfile.TemporaryDirectory(
                    dir=self.root
                ) as temporary_directory:
                    root = Path(temporary_directory)
                    paths = [
                        root / ".zcode" / "v2" / "config.json",
                        root / ".zcode" / "cli" / "config.json",
                    ]
                    for path, config, mode in zip(
                        paths,
                        self.originals,
                        self.original_modes,
                    ):
                        write_config(path, config, mode)
                    before = [path.read_bytes() for path in paths]
                    modes = [
                        stat.S_IMODE(path.stat().st_mode) for path in paths
                    ]
                    backup_root = root / ".zcode" / "backups"
                    lock_directory = (
                        backup_root / ".zcode-responses-item-guard"
                    )
                    lock_path = lock_directory / "transaction.lock"
                    if alias_kind == "exact":
                        output = lock_path
                    elif alias_kind == "child":
                        output = lock_directory / "report.json"
                    elif alias_kind == "symlink":
                        output = root / ("%s-lock-output.json" % alias_kind)
                        output.symlink_to(lock_path)
                    else:
                        with guard._transaction_lock(backup_root):
                            pass
                        output = root / "hardlink-lock-output.json"
                        os.link(lock_path, output)
                    lock_existed = lock_path.exists()
                    lock_bytes = (
                        lock_path.read_bytes() if lock_existed else None
                    )
                    lock_identity = (
                        (
                            lock_path.stat().st_dev,
                            lock_path.stat().st_ino,
                        )
                        if lock_existed
                        else None
                    )

                    with self.assertRaisesRegex(
                        ValueError,
                        "transaction lock",
                    ):
                        guard.execute(
                            paths,
                            apply=True,
                            backup_root=backup_root,
                            timestamp="20260926T120019Z",
                            output_path=output,
                        )

                    self.assertEqual(
                        [path.read_bytes() for path in paths],
                        before,
                    )
                    self.assertEqual(
                        [
                            stat.S_IMODE(path.stat().st_mode)
                            for path in paths
                        ],
                        modes,
                    )
                    self.assertFalse(
                        (backup_root / "20260926T120019Z").exists()
                    )
                    if lock_existed:
                        self.assertEqual(lock_path.read_bytes(), lock_bytes)
                        self.assertEqual(
                            (
                                lock_path.stat().st_dev,
                                lock_path.stat().st_ino,
                            ),
                            lock_identity,
                        )
                    else:
                        self.assertFalse(backup_root.exists())
                    if alias_kind == "child":
                        self.assertFalse(output.exists())
                    elif alias_kind == "symlink":
                        self.assertTrue(output.is_symlink())
                    elif alias_kind == "exact":
                        self.assertFalse(output.exists())
                    else:
                        self.assertTrue(output.samefile(lock_path))

    def test_config_symlink_is_rejected_before_dry_run_or_apply(self):
        for apply in (False, True):
            with self.subTest(apply=apply):
                with tempfile.TemporaryDirectory(
                    dir=self.root
                ) as temporary_directory:
                    root = Path(temporary_directory)
                    paths = [
                        root / ".zcode" / "v2" / "config.json",
                        root / ".zcode" / "cli" / "config.json",
                    ]
                    symlink_target = root / "actual-v2-config.json"
                    write_config(
                        symlink_target,
                        self.originals[0],
                        self.original_modes[0],
                    )
                    paths[0].parent.mkdir(parents=True)
                    paths[0].symlink_to(symlink_target)
                    write_config(
                        paths[1],
                        self.originals[1],
                        self.original_modes[1],
                    )
                    before = [
                        symlink_target.read_bytes(),
                        paths[1].read_bytes(),
                    ]
                    backup_root = root / ".zcode" / "backups"

                    with self.assertRaisesRegex(
                        ValueError,
                        "configuration path is not a regular file",
                    ):
                        guard.execute(
                            paths,
                            apply=apply,
                            backup_root=backup_root,
                            timestamp="20260926T120011Z",
                            output_path=root / "report.json",
                        )

                    self.assertTrue(paths[0].is_symlink())
                    self.assertEqual(
                        [
                            symlink_target.read_bytes(),
                            paths[1].read_bytes(),
                        ],
                        before,
                    )
                    self.assertFalse(backup_root.exists())

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

    def test_idempotent_apply_reports_without_replacing_configs_or_backup(self):
        for path, updated, mode in zip(
            self.paths,
            self.updated_bytes,
            self.original_modes,
        ):
            path.write_bytes(updated)
            path.chmod(mode)
        identities = [
            (path.stat().st_dev, path.stat().st_ino) for path in self.paths
        ]
        output = self.root / "unchanged-report.json"
        status = []

        report = guard.execute(
            self.paths,
            apply=True,
            backup_root=self.backup_root,
            timestamp="20260926T120012Z",
            output_path=output,
            status_sink=status.append,
        )

        self.assertEqual(
            [(path.stat().st_dev, path.stat().st_ino) for path in self.paths],
            identities,
        )
        self.assertEqual(
            [report["change"][label]["status"] for label in ("v2", "cli")],
            ["unchanged", "unchanged"],
        )
        self.assertTrue(output.is_file())
        self.assertEqual(len(status), 1)
        self.assertFalse(
            (self.backup_root / "20260926T120012Z").exists()
        )

    def test_apply_fsyncs_each_created_backup_directory_entry(self):
        fsynced_directories = set()
        real_fsync = os.fsync

        def record_fsync(file_descriptor):
            file_status = os.fstat(file_descriptor)
            if stat.S_ISDIR(file_status.st_mode):
                fsynced_directories.add(
                    (file_status.st_dev, file_status.st_ino)
                )
            return real_fsync(file_descriptor)

        with mock.patch.object(
            guard.os,
            "fsync",
            side_effect=record_fsync,
        ):
            self.execute(apply=True)

        backup_dir = self.backup_root / "20260926T120000Z"
        expected_directories = {
            path: (path.stat().st_dev, path.stat().st_ino)
            for path in (
                self.backup_root.parent,
                self.backup_root,
                self.backup_root / ".zcode-responses-item-guard",
                backup_dir,
                backup_dir / "v2",
                backup_dir / "cli",
                self.paths[0].parent,
                self.paths[1].parent,
            )
        }
        for path, identity in expected_directories.items():
            with self.subTest(path=path):
                self.assertIn(identity, fsynced_directories)

    def test_invalid_second_config_is_rejected_before_backup_or_write(self):
        invalid = config_fixture(kind="anthropic")
        write_config(self.paths[1], invalid, self.original_modes[1])
        before = [path.read_bytes() for path in self.paths]

        with self.assertRaisesRegex(ValueError, "kind"):
            self.execute(apply=True)

        self.assertEqual([path.read_bytes() for path in self.paths], before)
        self.assertFalse(self.backup_root.exists())

    def test_second_replace_failure_rolls_back_both_files_and_hashes(self):
        real_rename_exchange = guard._rename_exchange
        target_write_count = 0

        def fail_second_target_replace(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal target_write_count
            source_raw = read_bytes_at(source_dir_fd, source_name)
            is_target = any(
                dir_fd_entry_matches_path(
                    destination_dir_fd,
                    destination_name,
                    path,
                )
                for path in self.paths
            )
            if is_target and source_raw in self.updated_bytes:
                target_write_count += 1
                if target_write_count == 2:
                    raise OSError("injected second replace failure")
            return real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=fail_second_target_replace,
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

    def test_concurrent_apply_is_rejected_by_stable_private_lock(self):
        real_atomic_write = guard.atomic_write
        first_write_started = threading.Event()
        release_first_write = threading.Event()
        outer_errors = []

        def block_outer_first_write(path, data, mode, **kwargs):
            if (
                threading.current_thread().name == "outer-apply"
                and path == self.paths[0]
                and data == self.updated_bytes[0]
            ):
                first_write_started.set()
                if not release_first_write.wait(timeout=5):
                    raise RuntimeError("test timed out releasing first write")
            return real_atomic_write(path, data, mode, **kwargs)

        def run_outer_apply():
            try:
                self.execute(
                    apply=True,
                    timestamp="20260926T120020Z",
                )
            except BaseException as exc:
                outer_errors.append(exc)

        with mock.patch.object(
            guard,
            "atomic_write",
            side_effect=block_outer_first_write,
        ):
            worker = threading.Thread(
                target=run_outer_apply,
                name="outer-apply",
            )
            worker.start()
            self.assertTrue(first_write_started.wait(timeout=5))
            try:
                with self.assertRaisesRegex(
                    RuntimeError,
                    "configuration transaction is already active",
                ):
                    self.execute(
                        apply=True,
                        timestamp="20260926T120021Z",
                    )
                self.assertEqual(
                    [path.read_bytes() for path in self.paths],
                    self.original_bytes,
                )
            finally:
                release_first_write.set()
                worker.join(timeout=5)

        self.assertFalse(worker.is_alive())
        self.assertEqual(outer_errors, [])
        lock_files = list(self.backup_root.rglob("*.lock"))
        self.assertEqual(len(lock_files), 1)
        self.assertEqual(stat.S_IMODE(lock_files[0].stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE(lock_files[0].parent.stat().st_mode),
            0o700,
        )

    def test_transaction_lock_rejects_ancestor_symlink_redirect(self):
        lock_ancestor = self.root / "lock-anchor"
        lock_ancestor.mkdir()
        backup_root = lock_ancestor / "backups"
        alternate_ancestor = self.root / "alternate-lock-anchor"
        alternate_root = alternate_ancestor / "backups"
        alternate_root.mkdir(parents=True, mode=0o700)
        alternate_root.chmod(0o700)
        relocated_ancestor = self.root / "relocated-lock-anchor"
        second_entered = False
        second_error = None

        with guard._transaction_lock(backup_root):
            locked_root_status = backup_root.stat()
            lock_ancestor.rename(relocated_ancestor)
            lock_ancestor.symlink_to(
                alternate_ancestor,
                target_is_directory=True,
            )
            try:
                with guard._transaction_lock(
                    backup_root,
                    create_root=False,
                ):
                    second_entered = True
            except (OSError, RuntimeError) as exc:
                second_error = exc

        self.assertFalse(second_entered)
        self.assertIsNotNone(second_error)
        relocated_root_status = (
            relocated_ancestor / "backups"
        ).stat()
        self.assertEqual(
            (
                locked_root_status.st_dev,
                locked_root_status.st_ino,
            ),
            (
                relocated_root_status.st_dev,
                relocated_root_status.st_ino,
            ),
        )
        self.assertEqual(
            list(
                alternate_root.glob(
                    ".zcode-responses-item-guard/transaction.lock"
                )
            ),
            [],
        )

    def test_transaction_lock_creates_missing_root_ancestors(self):
        backup_root = (
            self.root
            / "new-backup-parent"
            / "nested"
            / "backups"
        )

        with guard._transaction_lock(backup_root):
            lock_path = (
                backup_root
                / ".zcode-responses-item-guard"
                / "transaction.lock"
            )
            self.assertTrue(lock_path.is_file())

        for directory in (
            self.root / "new-backup-parent",
            self.root / "new-backup-parent" / "nested",
            backup_root,
            backup_root / ".zcode-responses-item-guard",
        ):
            self.assertEqual(
                stat.S_IMODE(directory.stat().st_mode),
                0o700,
            )

    def test_lock_output_cannot_replace_lock_for_concurrent_transaction(self):
        with guard._transaction_lock(self.backup_root):
            pass
        lock_directory = (
            self.backup_root / ".zcode-responses-item-guard"
        )
        lock_path = lock_directory / "transaction.lock"
        lock_bytes = lock_path.read_bytes()
        lock_identity = (
            lock_path.stat().st_dev,
            lock_path.stat().st_ino,
        )
        first_output_parent = self.root / "first-output"
        first_output_parent.mkdir()
        first_output = first_output_parent / "transaction.lock"
        first_output.write_bytes(b"ordinary output before validation\n")
        first_body_finished = threading.Event()
        first_lock_acquired = threading.Event()
        release_first_lock = threading.Event()
        second_entered = threading.Event()
        first_errors = []
        second_errors = []
        first_validation_before_lock = []
        real_transaction_lock = guard._transaction_lock
        real_validate_output = guard._validate_transaction_output_path

        @contextmanager
        def track_transaction_lock(root, **kwargs):
            with real_transaction_lock(root, **kwargs):
                thread_name = threading.current_thread().name
                if thread_name == "first-apply":
                    first_lock_acquired.set()
                else:
                    second_entered.set()
                try:
                    yield
                finally:
                    if thread_name == "first-apply":
                        first_body_finished.set()
                        if not release_first_lock.wait(timeout=5):
                            raise RuntimeError(
                                "test timed out releasing first lock"
                            )

        def redirect_output_after_prelock_validation(root, output_path):
            result = real_validate_output(root, output_path)
            if (
                threading.current_thread().name == "first-apply"
                and output_path == first_output
                and not first_validation_before_lock
            ):
                first_validation_before_lock.append(
                    not first_lock_acquired.is_set()
                )
                first_output.unlink()
                first_output_parent.rmdir()
                first_output_parent.symlink_to(
                    lock_directory,
                    target_is_directory=True,
                )
            return result

        def run_first_apply():
            try:
                guard.execute(
                    self.paths,
                    apply=True,
                    backup_root=self.backup_root,
                    timestamp="20260926T120022Z",
                    output_path=first_output,
                )
            except BaseException as exc:
                first_errors.append(exc)

        def run_second_apply():
            try:
                guard.execute(
                    self.paths,
                    apply=True,
                    backup_root=self.backup_root,
                    timestamp="20260926T120023Z",
                    output_path=self.root / "second-report.json",
                )
            except BaseException as exc:
                second_errors.append(exc)

        with mock.patch.object(
            guard,
            "_transaction_lock",
            side_effect=track_transaction_lock,
        ), mock.patch.object(
            guard,
            "_validate_transaction_output_path",
            side_effect=redirect_output_after_prelock_validation,
        ):
            first_worker = threading.Thread(
                target=run_first_apply,
                name="first-apply",
            )
            second_worker = threading.Thread(
                target=run_second_apply,
                name="second-apply",
            )
            first_worker.start()
            self.assertTrue(first_body_finished.wait(timeout=5))
            try:
                second_worker.start()
                second_worker.join(timeout=5)
                self.assertFalse(second_worker.is_alive())
            finally:
                release_first_lock.set()
                first_worker.join(timeout=5)
                if second_worker.is_alive():
                    second_worker.join(timeout=5)

        self.assertFalse(first_worker.is_alive())
        self.assertFalse(second_worker.is_alive())
        self.assertEqual(first_validation_before_lock, [True])
        self.assertFalse(second_entered.is_set())
        self.assertEqual(len(first_errors), 1)
        self.assertIsInstance(first_errors[0], ValueError)
        self.assertRegex(str(first_errors[0]), "transaction lock")
        self.assertEqual(len(second_errors), 1)
        self.assertIsInstance(second_errors[0], RuntimeError)
        self.assertRegex(
            str(second_errors[0]),
            "configuration transaction is already active",
        )
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        self.assertEqual(lock_path.read_bytes(), lock_bytes)
        self.assertEqual(
            (lock_path.stat().st_dev, lock_path.stat().st_ino),
            lock_identity,
        )

    def test_root_lock_blocks_transaction_during_lock_path_cas(self):
        with guard._transaction_lock(self.backup_root):
            pass
        lock_directory = (
            self.backup_root / ".zcode-responses-item-guard"
        )
        lock_path = lock_directory / "transaction.lock"
        lock_bytes = lock_path.read_bytes()
        lock_identity = (
            lock_path.stat().st_dev,
            lock_path.stat().st_ino,
        )
        first_output_parent = self.root / "final-output"
        first_output_parent.mkdir()
        first_output = first_output_parent / "transaction.lock"
        first_output.write_bytes(b"ordinary output before validation\n")
        third_party = b'{"third_party":"must-not-win"}\n'
        output_redirected = threading.Event()
        cas_window_open = threading.Event()
        release_cas_window = threading.Event()
        second_entered = threading.Event()
        first_errors = []
        second_errors = []
        cas_after_final_validation = []
        output_checks = 0
        real_transaction_lock = guard._transaction_lock
        real_verify_output_state = guard._verify_output_state
        real_open_parent_directory = guard._open_parent_directory

        @contextmanager
        def track_transaction_lock(root, **kwargs):
            with real_transaction_lock(root, **kwargs):
                if threading.current_thread().name == "second-apply":
                    second_entered.set()
                    self.paths[0].write_bytes(third_party)
                    self.paths[0].chmod(self.original_modes[0])
                    raise AssertionError(
                        "second transaction entered critical section"
                    )
                yield

        def redirect_after_final_output_validation(snapshot, *, updated):
            nonlocal output_checks
            result = real_verify_output_state(snapshot, updated=updated)
            if (
                threading.current_thread().name == "first-apply"
                and snapshot["path"] == first_output
                and not updated
            ):
                output_checks += 1
                if output_checks == 2:
                    first_output.unlink()
                    first_output_parent.rmdir()
                    first_output_parent.symlink_to(
                        lock_directory,
                        target_is_directory=True,
                    )
                    output_redirected.set()
            return result

        @contextmanager
        def block_first_lock_parent_open(path, **kwargs):
            if (
                threading.current_thread().name == "first-apply"
                and path == first_output
            ):
                cas_after_final_validation.append(
                    output_redirected.is_set()
                )
                cas_window_open.set()
                if not release_cas_window.wait(timeout=5):
                    raise RuntimeError("test timed out releasing CAS window")
            with real_open_parent_directory(path, **kwargs) as opened:
                yield opened

        def run_first_apply():
            try:
                guard.execute(
                    self.paths,
                    apply=True,
                    backup_root=self.backup_root,
                    timestamp="20260926T120024Z",
                    output_path=first_output,
                )
            except BaseException as exc:
                first_errors.append(exc)

        def run_second_apply():
            try:
                guard.execute(
                    self.paths,
                    apply=True,
                    backup_root=self.backup_root,
                    timestamp="20260926T120025Z",
                    output_path=self.root / "second-report.json",
                )
            except BaseException as exc:
                second_errors.append(exc)

        with mock.patch.object(
            guard,
            "_transaction_lock",
            side_effect=track_transaction_lock,
        ), mock.patch.object(
            guard,
            "_verify_output_state",
            side_effect=redirect_after_final_output_validation,
        ), mock.patch.object(
            guard,
            "_open_parent_directory",
            side_effect=block_first_lock_parent_open,
        ):
            first_worker = threading.Thread(
                target=run_first_apply,
                name="first-apply",
            )
            second_worker = threading.Thread(
                target=run_second_apply,
                name="second-apply",
            )
            first_worker.start()
            self.assertTrue(cas_window_open.wait(timeout=5))
            self.assertTrue(lock_path.exists())
            try:
                second_worker.start()
                second_worker.join(timeout=5)
                self.assertFalse(second_worker.is_alive())
            finally:
                release_cas_window.set()
                first_worker.join(timeout=5)
                if second_worker.is_alive():
                    second_worker.join(timeout=5)

        self.assertFalse(first_worker.is_alive())
        self.assertFalse(second_worker.is_alive())
        self.assertEqual(output_checks, 2)
        self.assertEqual(cas_after_final_validation, [True])
        self.assertFalse(second_entered.is_set())
        self.assertEqual(len(second_errors), 1)
        self.assertIsInstance(second_errors[0], RuntimeError)
        self.assertRegex(
            str(second_errors[0]),
            "configuration transaction is already active",
        )
        self.assertFalse((self.root / "second-report.json").exists())
        self.assertEqual(len(first_errors), 1)
        self.assertIsInstance(first_errors[0], RuntimeError)
        self.assertRegex(str(first_errors[0]), "configuration apply failed")
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        self.assertNotEqual(self.paths[0].read_bytes(), third_party)
        self.assertEqual(lock_path.read_bytes(), lock_bytes)
        self.assertEqual(
            (lock_path.stat().st_dev, lock_path.stat().st_ino),
            lock_identity,
        )

    def test_output_ancestor_redirect_never_moves_transaction_lock(self):
        with guard._transaction_lock(self.backup_root):
            pass
        lock_directory = (
            self.backup_root / ".zcode-responses-item-guard"
        )
        lock_path = lock_directory / "transaction.lock"
        lock_bytes = lock_path.read_bytes()
        lock_identity = (
            lock_path.stat().st_dev,
            lock_path.stat().st_ino,
        )
        output_ancestor = self.root / "output-ancestor"
        output_parent = (
            output_ancestor / ".zcode-responses-item-guard"
        )
        output_parent.mkdir(parents=True)
        output = output_parent / "transaction.lock"
        original_output = b"ordinary output before redirect\n"
        output.write_bytes(original_output)
        output.chmod(0o640)
        output_snapshot = guard._snapshot_output(output)
        expected_state = (
            output_snapshot["identity"],
            output_snapshot["original"],
            output_snapshot["mode"],
        )
        relocated_ancestor = self.root / "relocated-output-ancestor"
        output_ancestor.rename(relocated_ancestor)
        output_ancestor.symlink_to(
            self.backup_root,
            target_is_directory=True,
        )
        redirected_exchanges = []
        real_rename_exchange = guard._rename_exchange

        def track_redirected_exchange(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            if dir_fd_entry_matches_path(
                destination_dir_fd,
                destination_name,
                lock_path,
            ):
                redirected_exchanges.append(destination_name)
            return real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=track_redirected_exchange,
        ):
            with self.assertRaises((OSError, RuntimeError)):
                guard._write_report(
                    output,
                    {"change": "must-fail-closed"},
                    expected_state=expected_state,
                )

        self.assertEqual(redirected_exchanges, [])
        relocated_output = (
            relocated_ancestor
            / ".zcode-responses-item-guard"
            / "transaction.lock"
        )
        self.assertEqual(relocated_output.read_bytes(), original_output)
        self.assertEqual(stat.S_IMODE(relocated_output.stat().st_mode), 0o640)
        self.assertEqual(lock_path.read_bytes(), lock_bytes)
        self.assertEqual(
            (lock_path.stat().st_dev, lock_path.stat().st_ino),
            lock_identity,
        )
        self.assertEqual(
            list(lock_directory.glob(".transaction.lock.displaced.*")),
            [],
        )

    def test_rename_exchange_atomically_swaps_directory_entries(self):
        first = self.root / "exchange-first"
        second = self.root / "exchange-second"
        first.write_bytes(b"first")
        second.write_bytes(b"second")
        directory_flags = (
            os.O_RDONLY
            | getattr(os, "O_DIRECTORY", 0)
            | getattr(os, "O_NOFOLLOW", 0)
        )
        directory_fd = os.open(self.root, directory_flags)
        try:
            guard._rename_exchange(
                directory_fd,
                first.name,
                directory_fd,
                second.name,
            )
        finally:
            os.close(directory_fd)

        self.assertEqual(first.read_bytes(), b"second")
        self.assertEqual(second.read_bytes(), b"first")

    def test_remove_if_matches_exchanges_before_deleting(self):
        output = self.root / "remove-output.json"
        output.write_bytes(b'{"report":"safe"}\n')
        output.chmod(0o640)
        expected_state = guard._observed_file_state(output, "output")
        exchange_observations = []
        real_rename_exchange = guard._rename_exchange

        def observe_exchange(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            result = real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )
            exchange_observations.append(
                (
                    os.stat(
                        source_name,
                        dir_fd=source_dir_fd,
                        follow_symlinks=False,
                    ).st_ino,
                    os.stat(
                        destination_name,
                        dir_fd=destination_dir_fd,
                        follow_symlinks=False,
                    ).st_ino,
                )
            )
            return result

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=observe_exchange,
        ):
            guard._remove_if_matches(output, expected_state)

        self.assertEqual(len(exchange_observations), 1)
        self.assertFalse(output.exists())
        self.assertEqual(
            list(self.root.glob(".remove-output.json.displaced.*")),
            [],
        )

    def test_remove_if_matches_recovers_when_restore_exchange_first_fails(
        self,
    ):
        output = self.root / "remove-secret.json"
        original = ("%s:%s" % (SECRET, OTHER_SECRET)).encode("utf-8")
        output.write_bytes(original)
        output.chmod(0o640)
        expected_state = guard._observed_file_state(output, "output")
        real_unlink = guard.os.unlink
        real_rename_exchange = guard._rename_exchange
        target_unlinks = 0
        target_exchanges = 0

        def fail_first_target_unlink(name, *args, **kwargs):
            nonlocal target_unlinks
            directory_fd = kwargs.get("dir_fd")
            if (
                directory_fd is not None
                and dir_fd_entry_matches_path(
                    directory_fd,
                    os.fspath(name),
                    output,
                )
                and target_unlinks == 0
            ):
                target_unlinks += 1
                raise OSError("injected target unlink failure")
            return real_unlink(name, *args, **kwargs)

        def fail_first_restore_exchange(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal target_exchanges
            if dir_fd_entry_matches_path(
                destination_dir_fd,
                destination_name,
                output,
            ):
                target_exchanges += 1
                if target_exchanges == 2:
                    raise OSError("injected restore exchange failure")
            return real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )

        with mock.patch.object(
            guard.os,
            "unlink",
            side_effect=fail_first_target_unlink,
        ), mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=fail_first_restore_exchange,
        ):
            with self.assertRaises((OSError, RuntimeError)):
                guard._remove_if_matches(output, expected_state)

        assert_no_secret_temporary_files(self, self.root)
        self.assertEqual(output.read_bytes(), original)
        self.assertEqual(target_unlinks, 1)
        self.assertGreaterEqual(target_exchanges, 3)

    def test_directory_replacement_is_not_moved_or_left_displaced(self):
        output = self.root / "report.json"
        original_output = b'{"existing":"report"}\n'
        output.write_bytes(original_output)
        output.chmod(0o640)
        output_snapshot = guard._snapshot_output(output)
        expected_state = (
            output_snapshot["identity"],
            output_snapshot["original"],
            output_snapshot["mode"],
        )
        preserved_original = self.root / "attacker-preserved-report.json"
        replacement_identities = []
        target_was_empty = []
        injected = False
        real_observed_file_state_at = guard._observed_file_state_at
        real_rename_exchange = guard._rename_exchange

        def replace_source_after_final_validation(
            parent_dir_fd,
            name,
            path,
            description="configuration",
        ):
            nonlocal injected
            result = real_observed_file_state_at(
                parent_dir_fd,
                name,
                path,
                description,
            )
            if (
                not injected
                and description == "target"
                and dir_fd_entry_matches_path(
                    parent_dir_fd,
                    name,
                    output,
                )
            ):
                injected = True
                os.rename(
                    name,
                    preserved_original.name,
                    src_dir_fd=parent_dir_fd,
                    dst_dir_fd=parent_dir_fd,
                )
                os.mkdir(name, dir_fd=parent_dir_fd)
                replacement = os.stat(
                    name,
                    dir_fd=parent_dir_fd,
                    follow_symlinks=False,
                )
                replacement_identities.append(
                    (replacement.st_dev, replacement.st_ino)
                )
            return result

        def observe_target_after_exchange(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            is_target_exchange = (
                not target_was_empty
                and
                dir_fd_entry_matches_path(
                    destination_dir_fd,
                    destination_name,
                    output,
                )
            )
            result = real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )
            if is_target_exchange:
                try:
                    os.stat(
                        destination_name,
                        dir_fd=destination_dir_fd,
                        follow_symlinks=False,
                    )
                except FileNotFoundError:
                    target_was_empty.append(True)
                else:
                    target_was_empty.append(False)
            return result

        with mock.patch.object(
            guard,
            "_observed_file_state_at",
            side_effect=replace_source_after_final_validation,
        ), mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=observe_target_after_exchange,
        ):
            with self.assertRaises((OSError, RuntimeError)):
                guard._write_report(
                    output,
                    {"change": "must-fail-closed"},
                    expected_state=expected_state,
                )

        self.assertTrue(injected)
        self.assertEqual(target_was_empty, [False])
        output_status = output.lstat()
        self.assertTrue(stat.S_ISDIR(output_status.st_mode))
        self.assertEqual(
            (output_status.st_dev, output_status.st_ino),
            replacement_identities[0],
        )
        self.assertEqual(preserved_original.read_bytes(), original_output)
        self.assertEqual(
            list(self.root.glob(".report.json.displaced.*")),
            [],
        )

    def test_output_parent_redirect_cannot_move_locked_backup_root(self):
        with guard._transaction_lock(self.backup_root):
            pass
        backup_root_identity = (
            self.backup_root.stat().st_dev,
            self.backup_root.stat().st_ino,
        )
        output_parent = self.root / "redirected-output"
        output_parent.mkdir()
        output = output_parent / self.backup_root.name
        output.write_bytes(b"ordinary output before validation\n")
        output.chmod(0o640)
        first_body_finished = threading.Event()
        release_first_lock = threading.Event()
        second_entered = threading.Event()
        first_errors = []
        second_errors = []
        output_checks = 0
        real_transaction_lock = guard._transaction_lock
        real_verify_output_state = guard._verify_output_state

        @contextmanager
        def track_transaction_lock(root, **kwargs):
            with real_transaction_lock(root, **kwargs):
                if threading.current_thread().name == "second-apply":
                    second_entered.set()
                try:
                    yield
                finally:
                    if threading.current_thread().name == "first-apply":
                        first_body_finished.set()
                        if not release_first_lock.wait(timeout=5):
                            raise RuntimeError(
                                "test timed out releasing first lock"
                            )

        def redirect_after_final_output_validation(snapshot, *, updated):
            nonlocal output_checks
            result = real_verify_output_state(snapshot, updated=updated)
            if (
                threading.current_thread().name == "first-apply"
                and snapshot["path"] == output
                and not updated
            ):
                output_checks += 1
                if output_checks == 2:
                    output.unlink()
                    output_parent.rmdir()
                    output_parent.symlink_to(
                        self.backup_root.parent,
                        target_is_directory=True,
                    )
            return result

        def run_first_apply():
            try:
                guard.execute(
                    self.paths,
                    apply=True,
                    backup_root=self.backup_root,
                    timestamp="20260926T120026Z",
                    output_path=output,
                )
            except BaseException as exc:
                first_errors.append(exc)

        def run_second_apply():
            try:
                guard.execute(
                    self.paths,
                    apply=True,
                    backup_root=self.backup_root,
                    timestamp="20260926T120027Z",
                    output_path=self.root / "second-root-report.json",
                )
            except BaseException as exc:
                second_errors.append(exc)

        with mock.patch.object(
            guard,
            "_transaction_lock",
            side_effect=track_transaction_lock,
        ), mock.patch.object(
            guard,
            "_verify_output_state",
            side_effect=redirect_after_final_output_validation,
        ):
            first_worker = threading.Thread(
                target=run_first_apply,
                name="first-apply",
            )
            second_worker = threading.Thread(
                target=run_second_apply,
                name="second-apply",
            )
            first_worker.start()
            self.assertTrue(first_body_finished.wait(timeout=5))
            try:
                try:
                    backup_root_status = self.backup_root.stat()
                except FileNotFoundError:
                    observed_backup_root_identity = None
                else:
                    observed_backup_root_identity = (
                        backup_root_status.st_dev,
                        backup_root_status.st_ino,
                    )
                second_worker.start()
                second_worker.join(timeout=5)
                self.assertFalse(second_worker.is_alive())
            finally:
                release_first_lock.set()
                first_worker.join(timeout=5)
                if second_worker.is_alive():
                    second_worker.join(timeout=5)

        self.assertFalse(first_worker.is_alive())
        self.assertFalse(second_worker.is_alive())
        self.assertEqual(output_checks, 2)
        self.assertEqual(
            {
                "backup_root_identity": observed_backup_root_identity,
                "second_transaction_entered": second_entered.is_set(),
            },
            {
                "backup_root_identity": backup_root_identity,
                "second_transaction_entered": False,
            },
        )
        self.assertEqual(len(first_errors), 1)
        self.assertIsInstance(first_errors[0], RuntimeError)
        self.assertRegex(str(first_errors[0]), "configuration apply failed")
        self.assertEqual(len(second_errors), 1)
        self.assertIsInstance(second_errors[0], RuntimeError)
        self.assertRegex(
            str(second_errors[0]),
            "configuration transaction is already active",
        )
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        self.assertEqual(
            list(self.backup_root.parent.glob(".backups.displaced.*")),
            [],
        )

    def test_transaction_root_lock_release_after_error_allows_later_transaction(
        self,
    ):
        with self.assertRaisesRegex(
            RuntimeError,
            "injected transaction failure",
        ):
            with guard._transaction_lock(self.backup_root):
                raise RuntimeError("injected transaction failure")

        entered = threading.Event()
        errors = []

        def run_later_transaction():
            try:
                with guard._transaction_lock(
                    self.backup_root,
                    create_root=False,
                ):
                    entered.set()
            except BaseException as exc:
                errors.append(exc)

        worker = threading.Thread(target=run_later_transaction)
        worker.start()
        worker.join(timeout=5)

        self.assertFalse(worker.is_alive())
        self.assertTrue(entered.is_set())
        self.assertEqual(errors, [])

    def test_backup_stops_before_copying_a_drifted_config(self):
        real_write_private_file = guard._write_private_file
        third_party = b'{"third_party":"during-backup"}\n'

        def drift_after_first_backup(path, data):
            result = real_write_private_file(path, data)
            if path.parent.name == "v2" and path.name == "config.json":
                self.paths[1].write_bytes(third_party)
                self.paths[1].chmod(self.original_modes[1])
            return result

        with mock.patch.object(
            guard,
            "_write_private_file",
            side_effect=drift_after_first_backup,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "rollback verification failed",
            ):
                self.execute(apply=True)

        backup_dir = self.backup_root / "20260926T120000Z"
        self.assertFalse((backup_dir / "cli" / "config.json").exists())
        self.assertEqual(self.paths[0].read_bytes(), self.original_bytes[0])
        self.assertEqual(self.paths[1].read_bytes(), third_party)

    def test_write_drift_does_not_overwrite_third_party_content(self):
        real_atomic_write = guard.atomic_write
        third_party = b'{"third_party":"between-writes"}\n'

        def drift_after_first_target_write(path, data, mode, **kwargs):
            result = real_atomic_write(path, data, mode, **kwargs)
            if path == self.paths[0] and data == self.updated_bytes[0]:
                self.paths[1].write_bytes(third_party)
                self.paths[1].chmod(self.original_modes[1])
            return result

        with mock.patch.object(
            guard,
            "atomic_write",
            side_effect=drift_after_first_target_write,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "rollback verification failed",
            ):
                self.execute(apply=True)

        self.assertEqual(self.paths[0].read_bytes(), self.original_bytes[0])
        self.assertEqual(self.paths[1].read_bytes(), third_party)

    def test_same_hash_new_inode_is_detected_before_target_write(self):
        real_atomic_write = guard.atomic_write
        updated_write_count = 0

        def replace_second_after_first_target_write(
            path, data, mode, **kwargs
        ):
            nonlocal updated_write_count
            result = real_atomic_write(path, data, mode, **kwargs)
            if path in self.paths and data in self.updated_bytes:
                updated_write_count += 1
            if path == self.paths[0] and data == self.updated_bytes[0]:
                replacement = self.paths[1].with_suffix(".replacement")
                replacement.write_bytes(self.original_bytes[1])
                replacement.chmod(self.original_modes[1])
                os.replace(replacement, self.paths[1])
            return result

        with mock.patch.object(
            guard,
            "atomic_write",
            side_effect=replace_second_after_first_target_write,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "configuration apply failed",
            ):
                self.execute(apply=True)

        self.assertEqual(updated_write_count, 1)
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )

    def test_directory_fsync_baseexception_rolls_back_and_reraises(self):
        real_fsync = os.fsync
        target_parent = self.paths[0].parent.stat()
        interrupted = False

        def interrupt_first_target_directory_fsync(file_descriptor):
            nonlocal interrupted
            file_status = os.fstat(file_descriptor)
            is_target_parent = (
                file_status.st_dev == target_parent.st_dev
                and file_status.st_ino == target_parent.st_ino
            )
            if is_target_parent and not interrupted:
                interrupted = True
                raise KeyboardInterrupt("injected directory fsync interrupt")
            return real_fsync(file_descriptor)

        with mock.patch.object(
            guard.os,
            "fsync",
            side_effect=interrupt_first_target_directory_fsync,
        ):
            with self.assertRaises(KeyboardInterrupt):
                self.execute(apply=True)

        self.assertTrue(interrupted)
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )

    def test_config_replace_baseexception_rolls_back_and_reraises(self):
        real_rename_exchange = guard._rename_exchange
        interrupted = False
        status = []

        def replace_first_config_then_interrupt(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal interrupted
            is_first_config_commit = (
                dir_fd_entry_matches_path(
                    destination_dir_fd,
                    destination_name,
                    self.paths[0],
                )
                and read_bytes_at(source_dir_fd, source_name)
                == self.updated_bytes[0]
            )
            result = real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )
            if is_first_config_commit and not interrupted:
                interrupted = True
                raise KeyboardInterrupt(
                    "injected config replace interrupt"
                )
            return result

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=replace_first_config_then_interrupt,
        ):
            with self.assertRaises(KeyboardInterrupt):
                guard.execute(
                    self.paths,
                    apply=True,
                    backup_root=self.backup_root,
                    timestamp="20260926T120034Z",
                    status_sink=status.append,
                )

        self.assertTrue(interrupted)
        self.assertEqual(status, [])
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )

    def test_sanitize_temporary_file_overwrites_and_truncates_content(self):
        temporary = self.root / ".secret-temporary"
        temporary.write_bytes(
            ("%s:%s" % (SECRET, OTHER_SECRET)).encode("utf-8")
        )
        temporary.chmod(0o600)
        temporary_status = temporary.stat()
        directory_flags = (
            os.O_RDONLY
            | getattr(os, "O_DIRECTORY", 0)
            | getattr(os, "O_NOFOLLOW", 0)
        )
        directory_fd = os.open(self.root, directory_flags)
        try:
            guard._sanitize_temporary_file(
                directory_fd,
                temporary.name,
                (
                    temporary_status.st_dev,
                    temporary_status.st_ino,
                ),
            )
        finally:
            os.close(directory_fd)

        self.assertEqual(temporary.read_bytes(), b"")

    def test_cleanup_retry_sanitizes_replacement_after_restore(self):
        real_unlink = guard.os.unlink
        cleanup_attempts = []

        def fail_first_four_temporary_cleanup_attempts(
            name,
            *args,
            **kwargs,
        ):
            directory_fd = kwargs.get("dir_fd")
            targets_first_config_parent = False
            if directory_fd is not None:
                directory_status = os.fstat(directory_fd)
                parent_status = self.paths[0].parent.stat()
                targets_first_config_parent = (
                    directory_status.st_dev,
                    directory_status.st_ino,
                ) == (
                    parent_status.st_dev,
                    parent_status.st_ino,
                )
            if (
                targets_first_config_parent
                and os.fspath(name).startswith(
                    ".config.json."
                )
                and len(cleanup_attempts) < 4
            ):
                cleanup_attempts.append(read_bytes_at(directory_fd, name))
                raise OSError("injected temporary cleanup failure")
            return real_unlink(name, *args, **kwargs)

        with mock.patch.object(
            guard.os,
            "unlink",
            side_effect=fail_first_four_temporary_cleanup_attempts,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "configuration apply failed",
            ):
                self.execute(apply=True)

        self.assertEqual(len(cleanup_attempts), 4)
        self.assertIn(SECRET.encode("utf-8"), cleanup_attempts[0])
        self.assertIn(SECRET.encode("utf-8"), cleanup_attempts[1])
        self.assertIn(SECRET.encode("utf-8"), cleanup_attempts[2])
        self.assertNotIn(SECRET.encode("utf-8"), cleanup_attempts[3])
        self.assertNotIn(
            OTHER_SECRET.encode("utf-8"),
            cleanup_attempts[3],
        )
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )
        temporary_files = [
            path
            for path in self.home.rglob(".config.json.*")
            if path.is_file()
        ]
        self.assertNotEqual(temporary_files, [])
        for temporary_file in temporary_files:
            temporary_content = temporary_file.read_bytes()
            self.assertNotIn(
                SECRET.encode("utf-8"),
                temporary_content,
            )
            self.assertNotIn(
                OTHER_SECRET.encode("utf-8"),
                temporary_content,
            )

    def test_atomic_cleanup_recovers_when_restore_exchange_first_fails(self):
        path = self.paths[0]
        expected_state = guard._observed_file_state(path)
        real_unlink = guard.os.unlink
        real_rename_exchange = guard._rename_exchange
        cleanup_attempts = 0
        target_exchanges = 0

        def fail_first_two_temporary_unlinks(name, *args, **kwargs):
            nonlocal cleanup_attempts
            directory_fd = kwargs.get("dir_fd")
            if (
                directory_fd is not None
                and dir_fd_entry_matches_path(
                    directory_fd,
                    path.name,
                    path,
                )
                and os.fspath(name).startswith(".config.json.")
                and cleanup_attempts < 2
            ):
                cleanup_attempts += 1
                raise OSError("injected temporary cleanup failure")
            return real_unlink(name, *args, **kwargs)

        def fail_first_restore_exchange(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal target_exchanges
            if dir_fd_entry_matches_path(
                destination_dir_fd,
                destination_name,
                path,
            ):
                target_exchanges += 1
                if target_exchanges == 2:
                    raise OSError("injected restore exchange failure")
            return real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )

        with mock.patch.object(
            guard.os,
            "unlink",
            side_effect=fail_first_two_temporary_unlinks,
        ), mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=fail_first_restore_exchange,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "atomic replacement cleanup failed",
            ):
                guard.atomic_write(
                    path,
                    self.updated_bytes[0],
                    self.original_modes[0],
                    expected_state=expected_state,
                )

        assert_no_secret_temporary_files(self, self.home)
        self.assertEqual(path.read_bytes(), self.original_bytes[0])
        self.assertGreaterEqual(cleanup_attempts, 2)
        self.assertGreaterEqual(target_exchanges, 3)

    def test_atomic_cleanup_retries_after_first_sanitize_failure(self):
        path = self.paths[0]
        expected_state = guard._observed_file_state(path)
        real_unlink = guard.os.unlink
        real_sanitize = guard._sanitize_temporary_file
        cleanup_attempts = 0
        sanitize_attempts = 0

        def fail_first_three_temporary_unlinks(name, *args, **kwargs):
            nonlocal cleanup_attempts
            directory_fd = kwargs.get("dir_fd")
            if (
                directory_fd is not None
                and dir_fd_entry_matches_path(
                    directory_fd,
                    path.name,
                    path,
                )
                and os.fspath(name).startswith(".config.json.")
                and cleanup_attempts < 3
            ):
                cleanup_attempts += 1
                raise OSError("injected temporary cleanup failure")
            return real_unlink(name, *args, **kwargs)

        def fail_first_sanitize(
            parent_dir_fd,
            name,
            expected_identity,
        ):
            nonlocal sanitize_attempts
            sanitize_attempts += 1
            if sanitize_attempts == 1:
                raise OSError("injected sanitize failure")
            return real_sanitize(
                parent_dir_fd,
                name,
                expected_identity,
            )

        with mock.patch.object(
            guard.os,
            "unlink",
            side_effect=fail_first_three_temporary_unlinks,
        ), mock.patch.object(
            guard,
            "_sanitize_temporary_file",
            side_effect=fail_first_sanitize,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "atomic replacement cleanup failed",
            ):
                guard.atomic_write(
                    path,
                    self.updated_bytes[0],
                    self.original_modes[0],
                    expected_state=expected_state,
                )

        assert_no_secret_temporary_files(self, self.home)
        self.assertEqual(path.read_bytes(), self.original_bytes[0])
        self.assertGreaterEqual(cleanup_attempts, 3)
        self.assertGreaterEqual(sanitize_attempts, 1)

    def test_persistent_cleanup_failure_leaves_only_sanitized_temp(self):
        real_unlink = guard.os.unlink
        cleanup_attempts = 0

        def fail_all_first_config_temporary_unlinks(
            name,
            *args,
            **kwargs,
        ):
            nonlocal cleanup_attempts
            directory_fd = kwargs.get("dir_fd")
            targets_first_config_parent = False
            if directory_fd is not None:
                directory_status = os.fstat(directory_fd)
                parent_status = self.paths[0].parent.stat()
                targets_first_config_parent = (
                    directory_status.st_dev,
                    directory_status.st_ino,
                ) == (
                    parent_status.st_dev,
                    parent_status.st_ino,
                )
            if (
                targets_first_config_parent
                and os.fspath(name).startswith(".config.json.")
            ):
                cleanup_attempts += 1
                raise OSError("injected persistent cleanup failure")
            return real_unlink(name, *args, **kwargs)

        with mock.patch.object(
            guard.os,
            "unlink",
            side_effect=fail_all_first_config_temporary_unlinks,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "configuration apply failed",
            ):
                self.execute(apply=True)

        self.assertGreaterEqual(cleanup_attempts, 2)
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        temporary_files = [
            path
            for path in self.home.rglob(".config.json.*")
            if path.is_file()
        ]
        self.assertNotEqual(temporary_files, [])
        for temporary_file in temporary_files:
            temporary_content = temporary_file.read_bytes()
            self.assertNotIn(
                SECRET.encode("utf-8"),
                temporary_content,
            )
            self.assertNotIn(
                OTHER_SECRET.encode("utf-8"),
                temporary_content,
            )

    def test_readback_validation_failure_rolls_back_both_files(self):
        real_verify_readback = guard._verify_readback
        readback_count = 0

        def fail_first_readback(snapshot):
            nonlocal readback_count
            readback_count += 1
            if readback_count == 1:
                raise ValueError("injected readback validation failure")
            return real_verify_readback(snapshot)

        with mock.patch.object(
            guard,
            "_verify_readback",
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

        def fail_report_write(path, report, **_kwargs):
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
        output = self.root / "write-failure.json"
        timestamp = "20260926T120002Z"
        stdout = FailingStdout(BrokenPipeError("injected broken pipe"))
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
            mock.patch.object(guard.sys, "stdout", stdout),
            mock.patch.object(guard.sys, "stderr", stderr),
        ):
            result = guard.main(["--apply", "--output", str(output)])

        self.assertEqual(result, 1)
        self.assertEqual(stdout.write_count, 1)
        self.assertEqual(stderr.getvalue(), "ERROR: change=failed\n")
        self.assertFalse(output.exists())
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

    def test_cli_apply_flush_failure_deletes_new_output_and_rolls_back(self):
        output = self.root / "flush-new-output.json"
        stdout = FlushFailingStdout(OSError("injected flush failure"))
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
                return_value="20260926T120030Z",
            ),
            mock.patch.object(guard.sys, "stdout", stdout),
            mock.patch.object(guard.sys, "stderr", stderr),
        ):
            result = guard.main(["--apply", "--output", str(output)])

        self.assertEqual(result, 1)
        self.assertEqual(stdout.write_count, 1)
        self.assertEqual(stdout.flush_count, 1)
        self.assertEqual(stderr.getvalue(), "ERROR: change=failed\n")
        self.assertFalse(output.exists())
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )

    def test_cli_apply_flush_failure_restores_existing_output(self):
        output = self.root / "flush-existing-output.json"
        original_output = b'{"existing":"report"}\n'
        output.write_bytes(original_output)
        output.chmod(0o640)
        stdout = FlushFailingStdout(
            BrokenPipeError("injected flush broken pipe")
        )
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
                return_value="20260926T120031Z",
            ),
            mock.patch.object(guard.sys, "stdout", stdout),
            mock.patch.object(guard.sys, "stderr", stderr),
        ):
            result = guard.main(["--apply", "--output", str(output)])

        self.assertEqual(result, 1)
        self.assertEqual(stdout.write_count, 1)
        self.assertEqual(stdout.flush_count, 1)
        self.assertEqual(stderr.getvalue(), "ERROR: change=failed\n")
        self.assertEqual(output.read_bytes(), original_output)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o640)
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )

    def test_output_replace_baseexception_restores_output_and_reraises(self):
        output = self.root / "interrupted-output.json"
        original_output = b'{"existing":"report"}\n'
        output.write_bytes(original_output)
        output.chmod(0o640)
        real_rename_exchange = guard._rename_exchange
        interrupted = False
        status = []

        def replace_output_then_interrupt(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal interrupted
            is_output_commit = dir_fd_entry_matches_path(
                destination_dir_fd,
                destination_name,
                output,
            )
            result = real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )
            if is_output_commit and not interrupted:
                interrupted = True
                raise KeyboardInterrupt(
                    "injected output replace interrupt"
                )
            return result

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=replace_output_then_interrupt,
        ):
            with self.assertRaises(KeyboardInterrupt):
                guard.execute(
                    self.paths,
                    apply=True,
                    backup_root=self.backup_root,
                    timestamp="20260926T120035Z",
                    output_path=output,
                    status_sink=status.append,
                )

        self.assertTrue(interrupted)
        self.assertEqual(status, [])
        self.assertEqual(output.read_bytes(), original_output)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o640)
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )

    def test_output_third_party_drift_is_not_overwritten_on_rollback(self):
        output = self.root / "third-party-output.json"
        original_output = b'{"existing":"report"}\n'
        third_party = b'{"third_party":"after-report"}\n'
        output.write_bytes(original_output)
        output.chmod(0o640)
        status_calls = 0

        def drift_output_and_fail(_line):
            nonlocal status_calls
            status_calls += 1
            output.write_bytes(third_party)
            output.chmod(0o600)
            raise OSError("injected status failure")

        with self.assertRaisesRegex(
            RuntimeError,
            "rollback verification failed",
        ):
            guard.execute(
                self.paths,
                apply=True,
                backup_root=self.backup_root,
                timestamp="20260926T120032Z",
                output_path=output,
                status_sink=drift_output_and_fail,
            )

        self.assertEqual(status_calls, 1)
        self.assertEqual(output.read_bytes(), third_party)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )

    def test_new_output_final_delete_drift_is_not_removed_on_rollback(self):
        output = self.root / "new-third-party-output.json"
        third_party = self.root / "new-third-party-replacement.json"
        third_party_bytes = b'{"third_party":"before-output-delete"}\n'
        third_party.write_bytes(third_party_bytes)
        third_party.chmod(0o604)
        third_party_identity = (
            third_party.stat().st_dev,
            third_party.stat().st_ino,
        )
        drift_injected = False
        real_rename_exchange = guard._rename_exchange

        def drift_at_output_exchange(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal drift_injected
            if (
                dir_fd_entry_matches_path(
                    destination_dir_fd,
                    destination_name,
                    output,
                )
                and not drift_injected
            ):
                os.replace(third_party, output)
                drift_injected = True
            return real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=drift_at_output_exchange,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "rollback verification failed",
            ):
                guard.execute(
                    self.paths,
                    apply=True,
                    backup_root=self.backup_root,
                    timestamp="20260926T120036Z",
                    output_path=output,
                    status_sink=lambda _line: (_ for _ in ()).throw(
                        OSError("injected status failure")
                    ),
                )

        self.assertTrue(drift_injected)
        self.assertEqual(output.read_bytes(), third_party_bytes)
        self.assertEqual(
            (output.stat().st_dev, output.stat().st_ino),
            third_party_identity,
        )
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )

    def test_same_hash_new_inode_drift_is_not_overwritten_on_rollback(self):
        output = self.root / "same-hash-third-party-output.json"
        replacement_identities = {}

        def replace_with_same_content_and_fail(_line):
            replacements = {
                "v2": self.paths[0],
                "output": output,
            }
            for label, path in replacements.items():
                replacement = path.with_suffix(".replacement")
                replacement.write_bytes(path.read_bytes())
                replacement.chmod(stat.S_IMODE(path.stat().st_mode))
                os.replace(replacement, path)
                replacement_identities[label] = (
                    path.stat().st_dev,
                    path.stat().st_ino,
                )
            raise OSError("injected status failure")

        with self.assertRaisesRegex(
            RuntimeError,
            "rollback verification failed",
        ):
            guard.execute(
                self.paths,
                apply=True,
                backup_root=self.backup_root,
                timestamp="20260926T120033Z",
                output_path=output,
                status_sink=replace_with_same_content_and_fail,
            )

        self.assertEqual(
            (self.paths[0].stat().st_dev, self.paths[0].stat().st_ino),
            replacement_identities["v2"],
        )
        self.assertEqual(self.paths[0].read_bytes(), self.updated_bytes[0])
        self.assertEqual(self.paths[1].read_bytes(), self.original_bytes[1])
        self.assertEqual(
            (output.stat().st_dev, output.stat().st_ino),
            replacement_identities["output"],
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

    def test_rollback_success_restores_backup_bytes_and_current_modes(self):
        timestamp = "20260926T121000Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        rollback_modes = [0o604, 0o640]
        for path, mode in zip(self.paths, rollback_modes):
            path.chmod(mode)
        output = self.root / "rollback.json"
        status = []

        report = guard.rollback_backup(
            self.paths,
            backup_dir,
            output_path=output,
            status_sink=status.append,
        )

        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.original_bytes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            rollback_modes,
        )
        self.assertEqual(report["operation"], "rollback")
        self.assertEqual(
            report["hash"],
            {
                "v2": {
                    "before_sha256": sha256(self.updated_bytes[0]),
                    "after_sha256": self.original_hashes[0],
                },
                "cli": {
                    "before_sha256": sha256(self.updated_bytes[1]),
                    "after_sha256": self.original_hashes[1],
                },
                "backup_manifest_sha256": sha256(
                    (backup_dir / "manifest.json").read_bytes()
                ),
            },
        )
        self.assertEqual(
            report["change"],
            {
                "v2": {"status": "rolled_back"},
                "cli": {"status": "rolled_back"},
            },
        )
        self.assertEqual(
            json.loads(output.read_text(encoding="ascii")),
            report,
        )
        self.assertEqual(len(status), 1)
        combined = json.dumps(report, sort_keys=True) + status[0]
        self.assertNotIn(str(backup_dir), combined)
        self.assertNotIn(SECRET, combined)
        self.assertNotIn(OTHER_SECRET, combined)
        assert_safe_report(self, report)

    def test_rollback_rejects_relative_backup_and_invalid_manifest_path(self):
        timestamp = "20260926T121001Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp

        with self.assertRaisesRegex(ValueError, "absolute"):
            guard.rollback_backup(
                self.paths,
                Path(timestamp),
                output_path=self.root / "relative.json",
            )

        manifest_path = backup_dir / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="ascii"))
        manifest["backup_path"]["v2"] = "../v2/config.json"
        manifest_path.write_bytes(json_bytes(manifest))
        manifest_path.chmod(0o600)

        with self.assertRaisesRegex(ValueError, "manifest"):
            guard.rollback_backup(
                self.paths,
                backup_dir,
                output_path=self.root / "bad-path.json",
            )

        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.updated_bytes,
        )

    def test_rollback_rejects_invalid_manifest_structure(self):
        timestamp = "20260926T121002Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        manifest_path = backup_dir / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="ascii"))
        manifest["unexpected"] = {}
        manifest_path.write_bytes(json_bytes(manifest))
        manifest_path.chmod(0o600)

        with self.assertRaisesRegex(ValueError, "manifest"):
            guard.rollback_backup(
                self.paths,
                backup_dir,
                output_path=self.root / "bad-manifest.json",
            )

        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.updated_bytes,
        )

    def test_rollback_rejects_backup_hash_mismatch(self):
        timestamp = "20260926T121003Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        backup = backup_dir / "v2" / "config.json"
        backup.write_bytes(b'{"tampered":true}\n')
        backup.chmod(0o600)

        with self.assertRaisesRegex(RuntimeError, "backup hash"):
            guard.rollback_backup(
                self.paths,
                backup_dir,
                output_path=self.root / "bad-hash.json",
            )

        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.updated_bytes,
        )

    def test_rollback_rejects_non_regular_and_wrong_mode_backup_files(self):
        for invalid_kind in ("symlink", "mode"):
            with self.subTest(invalid_kind=invalid_kind):
                with tempfile.TemporaryDirectory(
                    dir=self.root
                ) as temporary_directory:
                    root = Path(temporary_directory)
                    paths = [
                        root / ".zcode" / "v2" / "config.json",
                        root / ".zcode" / "cli" / "config.json",
                    ]
                    for path, config, mode in zip(
                        paths,
                        self.originals,
                        self.original_modes,
                    ):
                        write_config(path, config, mode)
                    backup_root = root / ".zcode" / "backups"
                    timestamp = "20260926T121004Z"
                    guard.execute(
                        paths,
                        apply=True,
                        backup_root=backup_root,
                        timestamp=timestamp,
                    )
                    backup_dir = backup_root / timestamp
                    manifest_path = backup_dir / "manifest.json"
                    if invalid_kind == "symlink":
                        manifest_copy = root / "manifest-copy.json"
                        manifest_copy.write_bytes(manifest_path.read_bytes())
                        manifest_copy.chmod(0o600)
                        manifest_path.unlink()
                        manifest_path.symlink_to(manifest_copy)
                        expected_error = "regular file"
                    else:
                        (
                            backup_dir / "cli" / "config.json"
                        ).chmod(0o640)
                        expected_error = "mode"

                    with self.assertRaisesRegex(
                        RuntimeError,
                        expected_error,
                    ):
                        guard.rollback_backup(
                            paths,
                            backup_dir,
                            output_path=root / "invalid-backup.json",
                        )

    def test_rollback_rejects_insecure_backup_root_without_chmod(self):
        timestamp = "20260926T121012Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        self.backup_root.chmod(0o755)

        with self.assertRaisesRegex(RuntimeError, "backup root mode"):
            guard.rollback_backup(
                self.paths,
                backup_dir,
                output_path=self.root / "insecure-root.json",
            )

        self.assertEqual(
            stat.S_IMODE(self.backup_root.stat().st_mode),
            0o755,
        )
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.updated_bytes,
        )

    def test_rollback_rejects_unapplied_current_content(self):
        timestamp = "20260926T121005Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        third_party = b'{"third_party":"before-rollback"}\n'
        self.paths[0].write_bytes(third_party)
        self.paths[0].chmod(self.original_modes[0])

        with self.assertRaisesRegex(RuntimeError, "current state"):
            guard.rollback_backup(
                self.paths,
                backup_dir,
                output_path=self.root / "drift.json",
            )

        self.assertEqual(self.paths[0].read_bytes(), third_party)
        self.assertEqual(self.paths[1].read_bytes(), self.updated_bytes[1])

    def test_rollback_cas_detects_same_hash_new_inode_without_overwrite(self):
        timestamp = "20260926T121006Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        real_atomic_write = guard.atomic_write
        drifted_identity = []

        def drift_after_first_rollback_write(path, data, mode, **kwargs):
            result = real_atomic_write(path, data, mode, **kwargs)
            if path == self.paths[0] and data == self.original_bytes[0]:
                replacement = self.paths[1].with_suffix(".replacement")
                replacement.write_bytes(self.paths[1].read_bytes())
                replacement.chmod(self.original_modes[1])
                os.replace(replacement, self.paths[1])
                drifted_identity.append(
                    (self.paths[1].stat().st_dev, self.paths[1].stat().st_ino)
                )
            return result

        with mock.patch.object(
            guard,
            "atomic_write",
            side_effect=drift_after_first_rollback_write,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "roll-forward verification failed",
            ):
                guard.rollback_backup(
                    self.paths,
                    backup_dir,
                    output_path=self.root / "inode-drift.json",
                )

        self.assertEqual(self.paths[0].read_bytes(), self.updated_bytes[0])
        self.assertEqual(self.paths[1].read_bytes(), self.updated_bytes[1])
        self.assertEqual(
            (self.paths[1].stat().st_dev, self.paths[1].stat().st_ino),
            drifted_identity[0],
        )

    def test_rollback_final_commit_drift_is_not_overwritten(self):
        timestamp = "20260926T121013Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        third_party = self.root / "third-party-v2.json"
        third_party_bytes = b'{"third_party":"rollback-final-commit"}\n'
        third_party.write_bytes(third_party_bytes)
        third_party.chmod(0o604)
        third_party_identity = (
            third_party.stat().st_dev,
            third_party.stat().st_ino,
        )
        drift_injected = False
        real_rename_exchange = guard._rename_exchange

        def drift_at_target_exchange(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal drift_injected
            if (
                dir_fd_entry_matches_path(
                    destination_dir_fd,
                    destination_name,
                    self.paths[0],
                )
                and not drift_injected
            ):
                os.replace(third_party, self.paths[0])
                drift_injected = True
            return real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=drift_at_target_exchange,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "roll-forward verification failed",
            ):
                guard.rollback_backup(
                    self.paths,
                    backup_dir,
                    output_path=self.root / "final-commit-drift.json",
                )

        self.assertTrue(drift_injected)
        self.assertEqual(self.paths[0].read_bytes(), third_party_bytes)
        self.assertEqual(
            (self.paths[0].stat().st_dev, self.paths[0].stat().st_ino),
            third_party_identity,
        )
        self.assertEqual(self.paths[1].read_bytes(), self.updated_bytes[1])

    def test_rollback_roll_forward_final_commit_drift_is_not_overwritten(self):
        timestamp = "20260926T121014Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        third_party = self.root / "third-party-roll-forward-v2.json"
        third_party_bytes = b'{"third_party":"roll-forward-final-commit"}\n'
        third_party.write_bytes(third_party_bytes)
        third_party.chmod(0o604)
        third_party_identity = (
            third_party.stat().st_dev,
            third_party.stat().st_ino,
        )
        v2_exchanges = 0
        second_rollback_commit_failed = False
        real_rename_exchange = guard._rename_exchange

        def fail_rollback_then_drift_roll_forward(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal v2_exchanges, second_rollback_commit_failed
            source_raw = read_bytes_at(source_dir_fd, source_name)
            if (
                dir_fd_entry_matches_path(
                    destination_dir_fd,
                    destination_name,
                    self.paths[0],
                )
                and source_raw
                in (self.original_bytes[0], self.updated_bytes[0])
            ):
                v2_exchanges += 1
                if v2_exchanges == 2:
                    os.replace(third_party, self.paths[0])
            if (
                dir_fd_entry_matches_path(
                    destination_dir_fd,
                    destination_name,
                    self.paths[1],
                )
                and source_raw == self.original_bytes[1]
                and not second_rollback_commit_failed
            ):
                second_rollback_commit_failed = True
                raise OSError("injected second rollback commit failure")
            return real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=fail_rollback_then_drift_roll_forward,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "roll-forward verification failed",
            ):
                guard.rollback_backup(
                    self.paths,
                    backup_dir,
                    output_path=self.root / "roll-forward-drift.json",
                )

        self.assertTrue(second_rollback_commit_failed)
        self.assertEqual(v2_exchanges, 2)
        self.assertEqual(self.paths[0].read_bytes(), third_party_bytes)
        self.assertEqual(
            (self.paths[0].stat().st_dev, self.paths[0].stat().st_ino),
            third_party_identity,
        )
        self.assertEqual(self.paths[1].read_bytes(), self.updated_bytes[1])

    def test_rollback_cas_detects_mode_drift_without_overwrite(self):
        timestamp = "20260926T121007Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        real_atomic_write = guard.atomic_write

        def drift_after_first_rollback_write(path, data, mode, **kwargs):
            result = real_atomic_write(path, data, mode, **kwargs)
            if path == self.paths[0] and data == self.original_bytes[0]:
                self.paths[1].chmod(0o604)
            return result

        with mock.patch.object(
            guard,
            "atomic_write",
            side_effect=drift_after_first_rollback_write,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "roll-forward verification failed",
            ):
                guard.rollback_backup(
                    self.paths,
                    backup_dir,
                    output_path=self.root / "mode-drift.json",
                )

        self.assertEqual(self.paths[0].read_bytes(), self.updated_bytes[0])
        self.assertEqual(self.paths[1].read_bytes(), self.updated_bytes[1])
        self.assertEqual(stat.S_IMODE(self.paths[1].stat().st_mode), 0o604)

    def test_rollback_second_replace_failure_restores_applied_state(self):
        timestamp = "20260926T121008Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        real_rename_exchange = guard._rename_exchange
        rollback_write_count = 0

        def fail_second_rollback_replace(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal rollback_write_count
            is_target = any(
                dir_fd_entry_matches_path(
                    destination_dir_fd,
                    destination_name,
                    path,
                )
                for path in self.paths
            )
            if is_target:
                source_raw = read_bytes_at(source_dir_fd, source_name)
                if source_raw in self.original_bytes:
                    rollback_write_count += 1
                    if rollback_write_count == 2:
                        raise OSError("injected second rollback failure")
            return real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=fail_second_rollback_replace,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "applied files restored",
            ):
                guard.rollback_backup(
                    self.paths,
                    backup_dir,
                    output_path=self.root / "second-write.json",
                )

        self.assertEqual(rollback_write_count, 2)
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.updated_bytes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )

    def test_rollback_report_failure_restores_applied_state(self):
        timestamp = "20260926T121009Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        output = self.root / "rollback-report-failure.json"

        with mock.patch.object(
            guard,
            "_write_report",
            side_effect=OSError("injected rollback report failure"),
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "applied files restored",
            ):
                guard.rollback_backup(
                    self.paths,
                    backup_dir,
                    output_path=output,
                )

        self.assertFalse(output.exists())
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.updated_bytes,
        )

    def test_cli_rollback_stdout_failure_restores_configs_and_output(self):
        timestamp = "20260926T121010Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        output = self.root / "rollback-stdout-failure.json"
        stdout = FailingStdout(BrokenPipeError("injected broken pipe"))
        stderr = io.StringIO()

        with (
            mock.patch.object(
                guard,
                "default_config_paths",
                return_value=self.paths,
            ),
            mock.patch.object(guard.sys, "stdout", stdout),
            mock.patch.object(guard.sys, "stderr", stderr),
        ):
            result = guard.main(
                [
                    "--rollback-backup",
                    str(backup_dir),
                    "--output",
                    str(output),
                ]
            )

        self.assertEqual(result, 1)
        self.assertEqual(stdout.write_count, 1)
        self.assertEqual(stderr.getvalue(), "ERROR: change=failed\n")
        self.assertFalse(output.exists())
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.updated_bytes,
        )

    def test_rollback_replace_baseexception_restores_and_reraises(self):
        timestamp = "20260926T121011Z"
        self.execute(apply=True, timestamp=timestamp)
        backup_dir = self.backup_root / timestamp
        real_rename_exchange = guard._rename_exchange
        interrupted = False

        def replace_first_config_then_interrupt(
            source_dir_fd,
            source_name,
            destination_dir_fd,
            destination_name,
        ):
            nonlocal interrupted
            is_first_config_commit = (
                dir_fd_entry_matches_path(
                    destination_dir_fd,
                    destination_name,
                    self.paths[0],
                )
                and read_bytes_at(source_dir_fd, source_name)
                == self.original_bytes[0]
            )
            result = real_rename_exchange(
                source_dir_fd,
                source_name,
                destination_dir_fd,
                destination_name,
            )
            if is_first_config_commit and not interrupted:
                interrupted = True
                raise KeyboardInterrupt("injected rollback interrupt")
            return result

        with mock.patch.object(
            guard,
            "_rename_exchange",
            side_effect=replace_first_config_then_interrupt,
        ):
            with self.assertRaises(KeyboardInterrupt):
                guard.rollback_backup(
                    self.paths,
                    backup_dir,
                    output_path=self.root / "interrupt.json",
                )

        self.assertTrue(interrupted)
        self.assertEqual(
            [path.read_bytes() for path in self.paths],
            self.updated_bytes,
        )
        self.assertEqual(
            [stat.S_IMODE(path.stat().st_mode) for path in self.paths],
            self.original_modes,
        )

    def test_rollback_output_rejects_transaction_lock_aliases(self):
        for alias_kind in ("exact", "child", "symlink", "hardlink"):
            with self.subTest(alias=alias_kind):
                with tempfile.TemporaryDirectory(
                    dir=self.root
                ) as temporary_directory:
                    root = Path(temporary_directory)
                    paths = [
                        root / ".zcode" / "v2" / "config.json",
                        root / ".zcode" / "cli" / "config.json",
                    ]
                    for path, config, mode in zip(
                        paths,
                        self.originals,
                        self.original_modes,
                    ):
                        write_config(path, config, mode)
                    backup_root = root / ".zcode" / "backups"
                    timestamp = "20260926T121015Z"
                    guard.execute(
                        paths,
                        apply=True,
                        backup_root=backup_root,
                        timestamp=timestamp,
                    )
                    backup_dir = backup_root / timestamp
                    lock_directory = (
                        backup_root / ".zcode-responses-item-guard"
                    )
                    lock_path = lock_directory / "transaction.lock"
                    lock_bytes = lock_path.read_bytes()
                    lock_identity = (
                        lock_path.stat().st_dev,
                        lock_path.stat().st_ino,
                    )
                    if alias_kind == "exact":
                        output = lock_path
                    elif alias_kind == "child":
                        output = lock_directory / "report.json"
                    else:
                        output = root / ("%s-lock-output.json" % alias_kind)
                        if alias_kind == "symlink":
                            output.symlink_to(lock_path)
                        else:
                            os.link(lock_path, output)

                    with self.assertRaisesRegex(
                        ValueError,
                        "transaction lock",
                    ):
                        guard.rollback_backup(
                            paths,
                            backup_dir,
                            output_path=output,
                        )

                    self.assertEqual(
                        [path.read_bytes() for path in paths],
                        self.updated_bytes,
                    )
                    self.assertEqual(lock_path.read_bytes(), lock_bytes)
                    self.assertEqual(
                        (lock_path.stat().st_dev, lock_path.stat().st_ino),
                        lock_identity,
                    )
                    if alias_kind == "child":
                        self.assertFalse(output.exists())
                    elif alias_kind == "symlink":
                        self.assertTrue(output.is_symlink())
                    else:
                        self.assertTrue(output.samefile(lock_path))

    def test_apply_and_rollback_cli_options_are_mutually_exclusive(self):
        stderr = io.StringIO()
        with mock.patch.object(guard.sys, "stderr", stderr):
            with self.assertRaises(SystemExit):
                guard.parse_args(
                    [
                        "--apply",
                        "--rollback-backup",
                        str(self.backup_root / "backup"),
                        "--output",
                        str(self.root / "report.json"),
                    ]
                )
        self.assertIn("not allowed with argument --apply", stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
