import unittest

import verify_model_alias_normalization as verify


class PublicModelVerificationTest(unittest.TestCase):
    def test_verification_token_name_fits_api_limit(self) -> None:
        name = verify.verification_token_name("default", 1234567890123456789)
        self.assertLessEqual(len(name), 32)
        self.assertTrue(name.startswith("alias-e2e-default-"))

    def test_required_aliases_present_and_concrete_ids_hidden(self) -> None:
        verify.verify_public_models(
            {"gpt-5.6", "gpt-6", "deepseekv4.1flash", "claude-opus-5"},
            {
                "hidden_concrete_models": [
                    "gpt-5.6-sol",
                    "gpt-5.6-terra",
                    "gpt-6-astra",
                    "deepseek-v4-flash-260425",
                ]
            },
        )

    def test_missing_required_alias_is_rejected(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "required aliases missing"):
            verify.verify_public_models(
                {"gpt-5.6", "deepseekv4.1flash"},
                {"hidden_concrete_models": []},
            )

    def test_exposed_concrete_id_is_rejected(self) -> None:
        with self.assertRaisesRegex(
            RuntimeError,
            "concrete release identifiers still exposed",
        ):
            verify.verify_public_models(
                {
                    "gpt-5.6",
                    "gpt-6",
                    "deepseekv4.1flash",
                    "gpt-5.6-sol",
                },
                {"hidden_concrete_models": ["gpt-5.6-sol"]},
            )


class RegressionCompatibilityTest(unittest.TestCase):
    def test_full_regression_uses_short_token_prefix(self) -> None:
        class Profiles:
            TRANSLATION_MODELS = set()
            VISUAL_EMBEDDING_MODELS = set()
            IMAGE_MODEL_SIZES = {}
            VIDEO_MODELS = set()
            THREE_D_MODELS = set()

        class E2E:
            TOKEN_NAME_PREFIX = "e2e-all-enabled-models-20260902"
            ark_profiles = Profiles

        verify.prepare_e2e_module(E2E)
        self.assertEqual(E2E.TOKEN_NAME_PREFIX, "alias-all")

    def test_capability_profiles_gain_canonical_aliases(self) -> None:
        class Profiles:
            TRANSLATION_MODELS = {"doubao-seed-translation-250915"}
            VISUAL_EMBEDDING_MODELS = {
                "doubao-embedding-vision-251215"
            }
            IMAGE_MODEL_SIZES = {
                "doubao-seedream-5-0-260128": "2048x2048"
            }
            VIDEO_MODELS = {"doubao-seedance-2-0-fast-260128"}
            THREE_D_MODELS = {"hitem3d-2-0-251223"}

        verify.extend_capability_profiles(Profiles)
        self.assertIn(
            "doubao-seed-translation",
            Profiles.TRANSLATION_MODELS,
        )
        self.assertIn(
            "doubao-embedding-vision",
            Profiles.VISUAL_EMBEDDING_MODELS,
        )
        self.assertEqual(
            Profiles.IMAGE_MODEL_SIZES["doubao-seedream-5-0"],
            "2048x2048",
        )
        self.assertIn(
            "doubao-seedance-2-0-fast",
            Profiles.VIDEO_MODELS,
        )
        self.assertIn("hitem3d-2-0", Profiles.THREE_D_MODELS)

    def test_round_reports_require_stable_inventory_and_zero_failures(
        self,
    ) -> None:
        reports = [
            {
                "passed": True,
                "failed_count": 0,
                "inventory_sha256": "same",
            },
            {
                "passed": True,
                "failed_count": 0,
                "inventory_sha256": "same",
            },
        ]
        self.assertEqual(
            verify.validate_round_reports(reports),
            "same",
        )
        reports[1]["failed_count"] = 1
        with self.assertRaisesRegex(RuntimeError, "round 2 failed"):
            verify.validate_round_reports(reports)


if __name__ == "__main__":
    unittest.main()
