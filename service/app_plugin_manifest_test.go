package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const appManifestFixturePath = "../testdata/app-plugin/seedance-repro-v1.json"

func TestValidateAppManifestAcceptsSeedanceV1(t *testing.T) {
	raw := readAppManifestFixture(t)

	manifest, err := ValidateAppManifest(raw)
	require.NoError(t, err)
	assert.Equal(t, 1, manifest.APIVersion)
	assert.Equal(t, "app", manifest.Kind)
	assert.Equal(t, "seedance-repro", manifest.Key)
	assert.Equal(t, "Seedance Repro", manifest.Name.EN)
	assert.Equal(t, "Seedance 复现台", manifest.Name.ZH)
	assert.Equal(t, "0.3.0", manifest.Version)
	assert.Equal(t, "/auth/callback", manifest.CallbackPath)
	assert.Equal(t, "/auth/start", manifest.Surfaces.Direct.StartPath)
	assert.Equal(t, "/auth/embed/start", manifest.Surfaces.Embedded.StartPath)
	assert.Equal(t, []string{
		"identity.read",
		"model.invoke",
		"task.import",
		"task.read",
		"resource.reserve",
		"resource.meter",
	}, manifest.RequestedScopes)
	require.Len(t, manifest.Requires.TaskPlugins, 1)
	assert.Equal(t, "doubao", manifest.Requires.TaskPlugins[0].Key)
	assert.Equal(t, "1.2.0", manifest.Requires.TaskPlugins[0].MinimumVersion)
}

func TestValidateAppManifestRejectsDuplicateUnknownAndForbiddenFields(t *testing.T) {
	t.Run("duplicate top-level key", func(t *testing.T) {
		assertAppManifestErrorCode(t, []byte(`{"key":"first","key":"second"}`), AppManifestInvalidErrorCode)
	})

	t.Run("duplicate nested key", func(t *testing.T) {
		assertAppManifestErrorCode(t, []byte(`{"name":{"en":"first","en":"second"}}`), AppManifestInvalidErrorCode)
	})

	t.Run("duplicate forbidden key is invalid", func(t *testing.T) {
		assertAppManifestErrorCode(t, []byte(`{"secret":"first","secret":"second"}`), AppManifestInvalidErrorCode)
	})

	t.Run("malformed forbidden field is invalid", func(t *testing.T) {
		assertAppManifestErrorCode(t, []byte(`{"secret":"incomplete"`), AppManifestInvalidErrorCode)
	})

	t.Run("unknown top-level field", func(t *testing.T) {
		raw := mutateAppManifest(t, func(manifest map[string]any) {
			manifest["unknown"] = true
		})
		assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
	})

	t.Run("unknown nested field", func(t *testing.T) {
		raw := mutateAppManifest(t, func(manifest map[string]any) {
			direct := manifest["surfaces"].(map[string]any)["direct"].(map[string]any)
			direct["title"] = "not in v1"
		})
		assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
	})

	t.Run("top-level field with mismatched case", func(t *testing.T) {
		raw := mutateAppManifest(t, func(manifest map[string]any) {
			delete(manifest, "apiVersion")
			manifest["APIVERSION"] = 1
		})
		assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
	})

	t.Run("nested field with mismatched case", func(t *testing.T) {
		raw := mutateAppManifest(t, func(manifest map[string]any) {
			direct := manifest["surfaces"].(map[string]any)["direct"].(map[string]any)
			delete(direct, "startPath")
			direct["STARTPATH"] = "/auth/start"
		})
		assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
	})

	forbiddenFields := []string{
		"base_url",
		"enabled",
		"enabled_surfaces",
		"allowed_parent_origins",
		"allowed_user_policy",
		"allowed_origins",
		"network_policy",
		"secret_ref",
		"credentials",
		"clientSecret",
		"private_key",
		"apiKey",
		"token",
		"tokenValue",
		"script",
		"executable",
		"code",
		"codePayload",
		"iframe",
		"iframeUrl",
		"proxy",
		"proxyRules",
	}
	for _, field := range forbiddenFields {
		t.Run("forbidden nested "+field, func(t *testing.T) {
			const secretValue = "do-not-disclose-this-value"
			raw := mutateAppManifest(t, func(manifest map[string]any) {
				requires := manifest["requires"].(map[string]any)
				requires[field] = secretValue
			})

			err := assertAppManifestErrorCode(t, raw, AppManifestForbiddenFieldErrorCode)
			assert.NotContains(t, err.Error(), field)
			assert.NotContains(t, err.Error(), secretValue)
		})
	}

	t.Run("unrelated field containing code is unknown", func(t *testing.T) {
		raw := mutateAppManifest(t, func(manifest map[string]any) {
			manifest["requires"].(map[string]any)["postcode"] = "12345"
		})
		assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
	})
}

func TestValidateAppManifestLimitsPathsScopesAndSemver(t *testing.T) {
	t.Run("maximum byte size accepted", func(t *testing.T) {
		raw := readAppManifestFixture(t)
		require.LessOrEqual(t, len(raw), AppManifestMaxBytes)
		raw = append(raw, bytes.Repeat([]byte(" "), AppManifestMaxBytes-len(raw))...)

		_, err := ValidateAppManifest(raw)
		require.NoError(t, err)
	})

	t.Run("one byte over maximum rejected", func(t *testing.T) {
		raw := readAppManifestFixture(t)
		raw = append(raw, bytes.Repeat([]byte(" "), AppManifestMaxBytes-len(raw)+1)...)
		assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
	})

	t.Run("invalid UTF-8 rejected", func(t *testing.T) {
		raw := append(readAppManifestFixture(t), byte(0xff))
		assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
	})

	t.Run("trailing JSON rejected", func(t *testing.T) {
		raw := append(readAppManifestFixture(t), []byte("\n{}")...)
		assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
	})

	for name, raw := range map[string][]byte{
		"array":  []byte(`[]`),
		"null":   []byte(`null`),
		"scalar": []byte(`"manifest"`),
	} {
		t.Run("non-object "+name, func(t *testing.T) {
			assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
		})
	}

	for name, scopes := range map[string][]any{
		"empty":     {},
		"duplicate": {"identity.read", "identity.read"},
		"unknown":   {"identity.read", "admin.write"},
	} {
		t.Run("scope "+name, func(t *testing.T) {
			raw := mutateAppManifest(t, func(manifest map[string]any) {
				manifest["requestedScopes"] = scopes
			})
			assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
		})
	}

	for name, mutate := range map[string]func(map[string]any){
		"manifest version missing patch": func(manifest map[string]any) {
			manifest["version"] = "0.3"
		},
		"manifest version with v prefix": func(manifest map[string]any) {
			manifest["version"] = "v0.3.0"
		},
		"manifest version leading zero major": func(manifest map[string]any) {
			manifest["version"] = "01.2.3"
		},
		"manifest version leading zero minor": func(manifest map[string]any) {
			manifest["version"] = "1.02.3"
		},
		"manifest version leading zero patch": func(manifest map[string]any) {
			manifest["version"] = "1.2.03"
		},
		"manifest version numeric prerelease leading zero": func(manifest map[string]any) {
			manifest["version"] = "1.0.0-01"
		},
		"task plugin version missing patch": func(manifest map[string]any) {
			taskPlugin := manifest["requires"].(map[string]any)["taskPlugins"].([]any)[0].(map[string]any)
			taskPlugin["minimumVersion"] = "1.2"
		},
		"task plugin version leading zero major": func(manifest map[string]any) {
			taskPlugin := manifest["requires"].(map[string]any)["taskPlugins"].([]any)[0].(map[string]any)
			taskPlugin["minimumVersion"] = "01.2.3"
		},
		"task plugin version leading zero minor": func(manifest map[string]any) {
			taskPlugin := manifest["requires"].(map[string]any)["taskPlugins"].([]any)[0].(map[string]any)
			taskPlugin["minimumVersion"] = "1.02.3"
		},
		"task plugin version leading zero patch": func(manifest map[string]any) {
			taskPlugin := manifest["requires"].(map[string]any)["taskPlugins"].([]any)[0].(map[string]any)
			taskPlugin["minimumVersion"] = "1.2.03"
		},
		"task plugin version numeric prerelease leading zero": func(manifest map[string]any) {
			taskPlugin := manifest["requires"].(map[string]any)["taskPlugins"].([]any)[0].(map[string]any)
			taskPlugin["minimumVersion"] = "1.0.0-01"
		},
	} {
		t.Run(name, func(t *testing.T) {
			assertAppManifestErrorCode(t, mutateAppManifest(t, mutate), AppManifestInvalidErrorCode)
		})
	}

	for name, mutate := range map[string]func(map[string]any){
		"manifest version prerelease and build metadata": func(manifest map[string]any) {
			manifest["version"] = "1.0.0-alpha.1+build.5"
		},
		"task plugin version prerelease and build metadata": func(manifest map[string]any) {
			taskPlugin := manifest["requires"].(map[string]any)["taskPlugins"].([]any)[0].(map[string]any)
			taskPlugin["minimumVersion"] = "1.0.0-alpha.1+build.5"
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateAppManifest(mutateAppManifest(t, mutate))
			require.NoError(t, err)
		})
	}

	for name, path := range map[string]string{
		"empty":                    "",
		"relative":                 "auth/callback",
		"scheme":                   "https://example.com/auth/callback",
		"authority":                "//example.com/auth/callback",
		"encoded authority":        "/%2fexample.com/auth/callback",
		"double encoded authority": "/%252fexample.com/auth/callback",
		"query":                    "/auth/callback?code=1",
		"encoded query":            "/auth/callback%3fcode=1",
		"fragment":                 "/auth/callback#done",
		"empty fragment":           "/auth/callback#",
		"encoded fragment":         "/auth/callback%23done",
		"encoded empty fragment":   "/auth/callback%23",
		"backslash":                `/auth\callback`,
		"control character":        "/auth/\ncallback",
		"dot traversal":            "/auth/../callback",
		"encoded traversal":        "/auth/%2e%2e/callback",
		"double encoded":           "/auth/%252e%252e/callback",
	} {
		t.Run("callback path "+name, func(t *testing.T) {
			raw := mutateAppManifest(t, func(manifest map[string]any) {
				manifest["callbackPath"] = path
			})
			assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
		})
	}

	for _, surface := range []string{"direct", "embedded"} {
		t.Run(surface+" start path invalid", func(t *testing.T) {
			raw := mutateAppManifest(t, func(manifest map[string]any) {
				target := manifest["surfaces"].(map[string]any)[surface].(map[string]any)
				target["startPath"] = "/auth/../../escape"
			})
			assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
		})
	}
}

func TestAppManifestAcceptsOnlyDirectAndEmbeddedSurfaces(t *testing.T) {
	for _, surface := range []string{"direct", "embedded"} {
		t.Run("missing "+surface, func(t *testing.T) {
			raw := mutateAppManifest(t, func(manifest map[string]any) {
				delete(manifest["surfaces"].(map[string]any), surface)
			})
			assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
		})
	}

	t.Run("additional surface", func(t *testing.T) {
		raw := mutateAppManifest(t, func(manifest map[string]any) {
			manifest["surfaces"].(map[string]any)["standalone"] = map[string]any{
				"startPath": "/auth/standalone",
			}
		})
		assertAppManifestErrorCode(t, raw, AppManifestInvalidErrorCode)
	})

	for name, mutate := range map[string]func(map[string]any){
		"api version": func(manifest map[string]any) {
			manifest["apiVersion"] = 2
		},
		"kind": func(manifest map[string]any) {
			manifest["kind"] = "task"
		},
		"empty name": func(manifest map[string]any) {
			manifest["name"].(map[string]any)["en"] = ""
		},
		"unsafe key": func(manifest map[string]any) {
			manifest["key"] = "../seedance"
		},
		"missing requires": func(manifest map[string]any) {
			delete(manifest, "requires")
		},
		"empty task plugin key": func(manifest map[string]any) {
			taskPlugin := manifest["requires"].(map[string]any)["taskPlugins"].([]any)[0].(map[string]any)
			taskPlugin["key"] = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			assertAppManifestErrorCode(t, mutateAppManifest(t, mutate), AppManifestInvalidErrorCode)
		})
	}
}

func readAppManifestFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(appManifestFixturePath)
	require.NoError(t, err)
	return raw
}

func mutateAppManifest(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(readAppManifestFixture(t), &manifest))
	mutate(manifest)
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	return raw
}

func assertAppManifestErrorCode(t *testing.T, raw []byte, code string) error {
	t.Helper()
	_, err := ValidateAppManifest(raw)
	require.Error(t, err)
	var validationErr *AppManifestError
	require.True(t, errors.As(err, &validationErr))
	assert.Equal(t, code, validationErr.Code)
	assert.True(t, strings.HasPrefix(err.Error(), code))
	return err
}
