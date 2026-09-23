import gzip
import json
import unittest
from unittest import mock

import model_alias_normalization as migration


class CanonicalAliasTest(unittest.TestCase):
    def test_required_aliases(self) -> None:
        cases = {
            "gpt-5.6-luna": "gpt-5.6",
            "gpt-5.6-sol": "gpt-5.6",
            "gpt-5.6-terra": "gpt-5.6",
            "gpt-6-astra": "gpt-6",
            "deepseek-v4-flash": "deepseekv4.1flash",
            "deepseek-v4-flash-260425": "deepseekv4.1flash",
            "deepseek-v4-flash-ga-260731": "deepseekv4.1flash",
            "deepseek-v4-flash-原厂": "deepseekv4.1flash",
            "deepseek-v4-flash-vision-exp-原厂": "deepseekv4.1flash-vision",
        }
        for concrete, alias in cases.items():
            with self.subTest(concrete=concrete):
                self.assertEqual(migration.canonical_alias(concrete), alias)

    def test_release_suffixes_are_hidden_but_capabilities_remain(self) -> None:
        cases = {
            "claude-haiku-4-5-20251001": "claude-haiku-4-5",
            "doubao-seedance-2-0-fast-260128": "doubao-seedance-2-0-fast",
            "doubao-seedance-2-0-mini-260615": "doubao-seedance-2-0-mini",
            "doubao-seed-2-0-pro-preview-260115": "doubao-seed-2.0-pro",
            "doubao-seed-evolving-latest-version": "doubao-seed-evolving",
            "qwen3-32b-20250429": "qwen3-32b",
            "gpt-5.3-codex-spark": "gpt-5.3-codex-spark",
            "claude-opus-4-8": "claude-opus-4-8",
        }
        for concrete, alias in cases.items():
            with self.subTest(concrete=concrete):
                self.assertEqual(migration.canonical_alias(concrete), alias)

    def test_known_stable_spellings_are_reused(self) -> None:
        cases = {
            "doubao-seed-2-0-lite-260428": "doubao-seed-2.0-lite",
            "doubao-seed-2-0-mini-260428": "doubao-seed-2.0-mini",
            "doubao-seed-2-0-pro-260215": "doubao-seed-2.0-pro",
            "doubao-seed-2-1-pro-260628": "doubao-seed-2.1-pro",
            "doubao-seed-2-1-turbo-260628": "doubao-seed-2.1-turbo",
            "glm-5-2-260617": "glm-5.2",
            "glm-5-3-260814": "glm-5.3",
            "glm-5-3-flash-260826": "glm-5.3-flash",
        }
        for concrete, alias in cases.items():
            with self.subTest(concrete=concrete):
                self.assertEqual(migration.canonical_alias(concrete), alias)


class ChannelPlanTest(unittest.TestCase):
    def test_managed_route_target_is_persisted_as_upstream_alias(self) -> None:
        channels = [
            {
                "id": 50,
                "name": "managed",
                "group": "default,cxy",
                "priority": 999,
                "models": "gpt-6-astra",
                "model_mapping": '{"gpt-6":"gpt-6-astra"}',
            },
        ]
        managed_channels = channels + [
            {
                "id": 56,
                "name": "managed-inactive",
                "group": "default,cxy",
                "priority": 0,
                "status": 3,
                "models": "gpt-5.6-sol",
                "model_mapping": "{}",
            },
        ]
        options = {key: {} for key in migration.OPTION_KEYS}
        options[migration.UPSTREAM_MODEL_ALIASES_KEY] = {
            "existing-concrete": "existing-alias",
        }
        options["ModelRatio"]["gpt-6-astra"] = 0.1

        manifest = migration.build_manifest(
            channels,
            options,
            {50: {"gpt-6-astra": 100}},
            managed_channels=managed_channels,
        )

        self.assertEqual(
            manifest["desired_options"][
                migration.UPSTREAM_MODEL_ALIASES_KEY
            ],
            {
                "existing-concrete": "existing-alias",
                "gpt-5.6-sol": "gpt-5.6",
                "gpt-6-astra": "gpt-6",
            },
        )

    def test_recent_success_wins_gpt_codename_collision(self) -> None:
        selected = migration.select_concrete(
            "gpt-5.6",
            ["gpt-5.6-sol", "gpt-5.6-terra"],
            {},
            {"gpt-5.6-sol": 200, "gpt-5.6-terra": 100},
        )
        self.assertEqual(selected, "gpt-5.6-sol")

    def test_stable_release_beats_more_recent_preview_log(self) -> None:
        selected = migration.select_concrete(
            "doubao-seed-2.1-pro",
            [
                "doubao-seed-2-1-pro-260628",
                "doubao-seed-2-1-pro-preview",
            ],
            {},
            {
                "doubao-seed-2-1-pro-260628": 100,
                "doubao-seed-2-1-pro-preview": 200,
            },
        )
        self.assertEqual(selected, "doubao-seed-2-1-pro-260628")

    def test_exact_stable_alias_beats_latest_version_label(self) -> None:
        selected = migration.select_concrete(
            "doubao-seed-evolving",
            [
                "doubao-seed-evolving",
                "doubao-seed-evolving-latest-version",
            ],
            {},
            {
                "doubao-seed-evolving": 100,
                "doubao-seed-evolving-latest-version": 200,
            },
        )
        self.assertEqual(selected, "doubao-seed-evolving")

    def test_existing_explicit_mapping_wins(self) -> None:
        selected = migration.select_concrete(
            "deepseekv4.1flash",
            [
                "deepseek-v4-flash",
                "deepseek-v4-flash-260425",
                "deepseek-v4-flash-ga-260731",
            ],
            {"deepseek-v4-flash": "deepseek-v4-flash-260425"},
            {"deepseek-v4-flash-ga-260731": 300},
        )
        self.assertEqual(selected, "deepseek-v4-flash-260425")

    def test_public_channel_plan_contains_aliases_only(self) -> None:
        plan = migration.build_channel_plan(
            {
                "id": 92,
                "group": "default,cxy",
                "models": "gpt-5.6-sol,gpt-5.6-terra,gpt-6-astra",
                "model_mapping": "{}",
            },
            {
                "gpt-5.6-sol": 200,
                "gpt-5.6-terra": 100,
                "gpt-6-astra": 150,
            },
        )
        self.assertEqual(plan["models"], ["gpt-5.6", "gpt-6"])
        self.assertEqual(
            plan["model_mapping"],
            {"gpt-5.6": "gpt-5.6-sol", "gpt-6": "gpt-6-astra"},
        )

    def test_internal_only_channel_is_not_public(self) -> None:
        self.assertFalse(
            migration.is_public_channel({"group": "ark-canary,ark-quarantine"})
        )
        self.assertTrue(
            migration.is_public_channel({"group": "default,cxy"})
        )

    def test_manifest_includes_only_changed_public_channels(self) -> None:
        channels = [
            {
                "id": 4,
                "name": "support",
                "group": "default,cxy",
                "priority": 40,
                "models": "deepseek-v4-flash",
                "model_mapping": "{}",
            },
            {
                "id": 92,
                "name": "rvcompute",
                "group": "default,cxy",
                "priority": 1000,
                "models": "gpt-5.6-sol,gpt-5.6-terra,gpt-6-astra",
                "model_mapping": "{}",
            },
            {
                "id": 107,
                "name": "canary",
                "group": "ark-canary",
                "priority": 25,
                "models": "deepseek-v4-flash-ga-260731",
                "model_mapping": "{}",
            },
        ]
        options = {
            key: {} for key in migration.OPTION_KEYS
        }
        options["ModelRatio"] = {
            "deepseek-v4-flash": 0.2,
            "gpt-5.6-sol": 0.1,
            "gpt-5.6-terra": 0.1,
            "gpt-6-astra": 0.1,
        }
        manifest = migration.build_manifest(
            channels,
            options,
            {
                4: {"deepseek-v4-flash": 220},
                92: {
                    "gpt-5.6-sol": 200,
                    "gpt-5.6-terra": 100,
                    "gpt-6-astra": 150,
                }
            },
        )
        self.assertEqual(
            [item["channel_id"] for item in manifest["channel_changes"]],
            [4, 92],
        )
        self.assertEqual(
            manifest["required_aliases"],
            ["deepseekv4.1flash", "gpt-5.6", "gpt-6"],
        )
        self.assertEqual(manifest["blockers"], [])

    def test_mapped_target_success_satisfies_channel_gate(self) -> None:
        channels = [
            {
                "id": 124,
                "name": "fallback",
                "group": "default,cxy",
                "priority": 20,
                "models": (
                    "deepseek-v4-flash,"
                    "deepseek-v4-flash-260425,"
                    "deepseek-v4-flash-ga-260731"
                ),
                "model_mapping": (
                    '{"deepseek-v4-flash":"deepseek-v4-flash-260425"}'
                ),
            },
            {
                "id": 92,
                "name": "rvcompute",
                "group": "default,cxy",
                "priority": 1000,
                "models": "gpt-5.6-sol,gpt-6-astra",
                "model_mapping": "{}",
            },
        ]
        options = {key: {} for key in migration.OPTION_KEYS}
        options["ModelRatio"] = {
            "deepseek-v4-flash": 0.2,
            "gpt-5.6-sol": 0.1,
            "gpt-6-astra": 0.1,
        }
        manifest = migration.build_manifest(
            channels,
            options,
            {
                124: {"deepseek-v4-flash-260425": 300},
                92: {"gpt-5.6-sol": 200, "gpt-6-astra": 150},
            },
        )
        self.assertFalse(
            any(
                "channel 124 alias deepseekv4.1flash" in blocker
                for blocker in manifest["blockers"]
            )
        )

    def test_unverified_alias_is_pruned_when_healthy_route_exists(self) -> None:
        channels = [
            {
                "id": 50,
                "name": "healthy",
                "group": "default,cxy",
                "priority": 999,
                "models": "gpt-6-astra",
                "model_mapping": "{}",
            },
            {
                "id": 62,
                "name": "unverified",
                "group": "default,cxy",
                "priority": 997,
                "models": "gpt-5.5,gpt-6-astra",
                "model_mapping": "{}",
            },
            {
                "id": 92,
                "name": "rvcompute",
                "group": "default,cxy",
                "priority": 1000,
                "models": "gpt-5.6-sol",
                "model_mapping": "{}",
            },
            {
                "id": 97,
                "name": "deepseek",
                "group": "default,cxy",
                "priority": 25,
                "models": "deepseek-v4-flash",
                "model_mapping": "{}",
            },
        ]
        options = {key: {} for key in migration.OPTION_KEYS}
        options["ModelRatio"] = {
            "deepseek-v4-flash": 0.2,
            "gpt-5.5": 0.1,
            "gpt-5.6-sol": 0.1,
            "gpt-6-astra": 0.1,
        }
        manifest = migration.build_manifest(
            channels,
            options,
            {
                50: {"gpt-6-astra": 100},
                62: {"gpt-5.5": 90},
                92: {"gpt-5.6-sol": 200},
                97: {"deepseek-v4-flash": 150},
            },
        )
        plan = next(
            item
            for item in manifest["channel_changes"]
            if item["channel_id"] == 62
        )
        self.assertNotIn("gpt-6", plan["models"])
        self.assertEqual(plan["pruned_unverified_aliases"], ["gpt-6"])
        self.assertFalse(
            any("channel 62 alias gpt-6" in item for item in manifest["blockers"])
        )

    def test_unverified_mapped_alias_is_pruned_when_healthy_route_exists(
        self,
    ) -> None:
        channels = [
            {
                "id": 50,
                "name": "healthy",
                "group": "default,cxy",
                "priority": 999,
                "models": "gpt-6-astra",
                "model_mapping": "{}",
            },
            {
                "id": 62,
                "name": "unverified-mapped-alias",
                "group": "default,cxy",
                "priority": 997,
                "models": "gpt-6",
                "model_mapping": '{"gpt-6":"gpt-6-astra"}',
            },
            {
                "id": 92,
                "name": "rvcompute",
                "group": "default,cxy",
                "priority": 1000,
                "models": "gpt-5.6-sol",
                "model_mapping": "{}",
            },
            {
                "id": 97,
                "name": "deepseek",
                "group": "default,cxy",
                "priority": 25,
                "models": "deepseek-v4-flash",
                "model_mapping": "{}",
            },
        ]
        options = {key: {} for key in migration.OPTION_KEYS}
        options["ModelRatio"] = {
            "deepseek-v4-flash": 0.2,
            "gpt-5.6-sol": 0.1,
            "gpt-6": 0.1,
            "gpt-6-astra": 0.1,
        }

        manifest = migration.build_manifest(
            channels,
            options,
            {
                50: {"gpt-6-astra": 100},
                92: {"gpt-5.6-sol": 200},
                97: {"deepseek-v4-flash": 150},
            },
        )

        plan = next(
            item
            for item in manifest["channel_changes"]
            if item["channel_id"] == 62
        )
        self.assertNotIn("gpt-6", plan["models"])
        self.assertEqual(plan["pruned_unverified_aliases"], ["gpt-6"])

    def test_advanced_route_filters_use_public_aliases(self) -> None:
        settings = {
            "routing_account": "support",
            "advanced_custom": {
                "advanced_routes": [
                    {
                        "incoming_path": "/v1/responses",
                        "upstream_path": "/api/v3/chat/completions",
                        "converter": "openai_responses_to_openai_chat_completions",
                        "models": [
                            "deepseek-v4-flash",
                            "deepseek-v4-flash-260425",
                            "deepseek-v4-flash-ga-260731",
                        ],
                    }
                ]
            },
        }
        normalized = migration.normalize_route_settings(settings)
        route = normalized["advanced_custom"]["advanced_routes"][0]
        self.assertEqual(route["models"], ["deepseekv4.1flash"])
        self.assertEqual(normalized["routing_account"], "support")
        self.assertEqual(
            route["converter"],
            "openai_responses_to_openai_chat_completions",
        )


class PricingPlanTest(unittest.TestCase):
    def test_alias_inherits_selected_concrete_pricing(self) -> None:
        options = {
            "ModelRatio": {"gpt-5.6-sol": 0.1},
            "CompletionRatio": {"gpt-5.6-sol": 4.0},
            "ModelPrice": {},
            "CacheRatio": {"gpt-5.6-sol": 0.25},
            "billing_setting.billing_mode": {},
            "billing_setting.billing_expr": {},
        }
        updated = migration.inherit_alias_pricing(
            options, {"gpt-5.6": "gpt-5.6-sol"}
        )
        self.assertEqual(updated["ModelRatio"]["gpt-5.6"], 0.1)
        self.assertEqual(updated["CompletionRatio"]["gpt-5.6"], 4.0)
        self.assertEqual(updated["CacheRatio"]["gpt-5.6"], 0.25)

    def test_unpriced_alias_is_rejected(self) -> None:
        options = {key: {} for key in migration.OPTION_KEYS}
        with self.assertRaisesRegex(RuntimeError, "unpriced alias"):
            migration.inherit_alias_pricing(
                options, {"gpt-5.6": "gpt-5.6-sol"}
            )

    def test_apply_requires_exact_manifest_hash(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "manifest SHA-256"):
            migration.validate_apply_hash("expected", "actual")

    def test_gzip_management_response_is_decoded(self) -> None:
        payload = {"success": True, "data": {"id": 92}}
        compressed = gzip.compress(json.dumps(payload).encode())
        self.assertEqual(
            migration.decode_response_body(compressed, "gzip"),
            payload,
        )

    def test_channel_aliases_are_applied_before_alias_pricing(self) -> None:
        original_options = {key: {} for key in migration.OPTION_KEYS}
        desired_options = {
            key: dict(value)
            for key, value in original_options.items()
        }
        desired_options["ModelRatio"] = {"gpt-6": 0.1}
        plan = {
            "channel_id": 92,
            "group": "default,cxy",
            "models": ["gpt-6"],
            "model_mapping": {"gpt-6": "gpt-6-astra"},
            "settings_field": "",
            "desired_settings": {},
        }
        manifest = {
            "channel_changes": [plan],
            "desired_options": desired_options,
        }
        original_channel = {
            "id": 92,
            "models": "gpt-6-astra",
            "model_mapping": "{}",
        }
        final_channel = {
            "id": 92,
            "models": "gpt-6",
            "model_mapping": '{"gpt-6":"gpt-6-astra"}',
        }
        option_rows = [
            {"key": key, "value": json.dumps(value)}
            for key, value in desired_options.items()
        ]
        events = []
        channel_reads = iter([original_channel, final_channel])

        def fake_api_request(headers, method, path, body=None, timeout=120):
            if method == "GET" and path == "/api/channel/92":
                return next(channel_reads)
            if method == "PUT" and path == "/api/channel/":
                events.append("channel")
                return None
            if method == "GET" and path == "/api/option/":
                return option_rows
            raise AssertionError((method, path, body, timeout))

        def fake_update_option(headers, key, value):
            events.append("option:" + key)

        with mock.patch.object(
            migration,
            "api_request",
            side_effect=fake_api_request,
        ), mock.patch.object(
            migration,
            "update_option",
            side_effect=fake_update_option,
        ), mock.patch.object(
            migration,
            "channel_key_hashes",
            side_effect=[
                {92: "same"},
                {92: "same"},
            ],
        ), mock.patch.object(
            migration,
            "verify_abilities",
            return_value=[],
        ):
            migration.apply_manifest({}, manifest, original_options)

        self.assertEqual(events[:2], ["channel", "option:ModelRatio"])


if __name__ == "__main__":
    unittest.main()
