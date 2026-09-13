package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestAppInstallationNeverReturnsSecretRef(t *testing.T) {
	db := openAppPluginServiceDB(t)
	logAppPluginServiceDBVersion(t, db)
	t.Run("records database runtime metadata", func(t *testing.T) {
		assertAppPluginServiceRuntimeMetadata(t, db)
	})
	require.NoError(t, model.MigrateAppPluginTables(db))

	svc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{
		TaskPluginChecker: fixedTaskPluginChecker{"doubao": "1.2.0"},
	})
	resp, err := svc.Install(context.Background(), appPluginInstallCommand("redaction-view", "1.0.0"))
	require.NoError(t, err)

	body, err := common.Marshal(resp)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, common.Unmarshal(body, &fields))

	assert.ElementsMatch(t, []string{
		"installation_id",
		"app_key",
		"manifest_version",
		"manifest_sha256",
		"base_url",
		"enabled_surfaces",
		"allowed_parent_origins",
		"service_credential_set",
		"allowed_origins",
		"allowed_user_policy",
		"network_policy",
		"entitlement_policy_version",
		"status",
		"revision",
		"created_at",
		"updated_at",
	}, sortedMapKeys(fields))
	assert.NotContains(t, string(body), "secret")
	assert.NotContains(t, string(body), "credential_hash")
	assert.NotContains(t, string(body), "secret_ref")
	assert.NotContains(t, string(body), "raw_policy")
	assert.NotContains(t, string(body), "manifest_json")
	assert.NotContains(t, string(body), `"id"`)

	var credential model.AppServiceCredential
	require.NoError(t, db.Where("installation_id = ?", resp.InstallationID).First(&credential).Error)
	assert.NotEmpty(t, credential.CredentialHash)
	assert.NotEmpty(t, credential.CredentialID)
	assert.NotEmpty(t, credential.CredentialVersion)
	assert.NotEmpty(t, credential.Status)
	assert.NotZero(t, credential.ExpiresAt)
	assert.Equal(t, model.AppCredentialMeta{
		CredentialID: credential.CredentialID,
		Version:      credential.CredentialVersion,
		Status:       credential.Status,
		ExpiresAt:    credential.ExpiresAt,
	}, resp.ServiceCredentialSet)
	directCredentialJSON, err := common.Marshal(credential)
	require.NoError(t, err)
	assert.NotContains(t, string(directCredentialJSON), "credential_hash")
	assert.NotContains(t, string(directCredentialJSON), "secret")
	assert.NotContains(t, string(directCredentialJSON), "app_key")
	assert.NotContains(t, string(directCredentialJSON), "installation_id")

	t.Run("disabled installation may omit service credential", func(t *testing.T) {
		cmd := appPluginInstallCommand("credentialless-service", "1.0.0")
		cmd.ServiceCredential = AppServiceCredentialInput{}

		credentialless, err := svc.Install(context.Background(), cmd)
		require.NoError(t, err)
		assert.Equal(t, model.AppInstallationStatusDisabled, credentialless.Status)
		assert.Equal(t, model.AppCredentialMeta{}, credentialless.ServiceCredentialSet)

		var credentialCount int64
		require.NoError(t, db.Model(&model.AppServiceCredential{}).
			Where("installation_id = ?", credentialless.InstallationID).
			Count(&credentialCount).Error)
		assert.Zero(t, credentialCount)
	})
}

func TestAppEntitlementPolicyIsHostOwnedAndVersioned(t *testing.T) {
	db := openAppPluginServiceDB(t)
	logAppPluginServiceDBVersion(t, db)
	t.Run("records database runtime metadata", func(t *testing.T) {
		assertAppPluginServiceRuntimeMetadata(t, db)
	})
	require.NoError(t, model.MigrateAppPluginTables(db))

	svc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{
		TaskPluginChecker: fixedTaskPluginChecker{"doubao": "1.2.0"},
		CurrentAuthz: map[string][]string{
			"files":  {"read", "write"},
			"models": {"invoke"},
		},
	})

	policyV1, err := svc.CreateEntitlementPolicy(context.Background(), AppEntitlementPolicyDraft{
		Key: "basic",
		Rules: map[string][]string{
			"files":  {"write", "read"},
			"models": {"invoke"},
			"admin":  {"manage"},
		},
	})
	require.NoError(t, err)
	policyV2, err := svc.CreateEntitlementPolicy(context.Background(), AppEntitlementPolicyDraft{
		Key: "basic",
		Rules: map[string][]string{
			"files": {"read"},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, int64(1), policyV1.Version)
	assert.Equal(t, int64(2), policyV2.Version)
	assert.Equal(t, map[string][]string{"files": {"read", "write"}, "models": {"invoke"}}, policyV1.EffectiveRules)
	assert.Equal(t, map[string][]string{"files": {"read"}}, policyV2.EffectiveRules)
	assert.ErrorIs(t, svc.UpdateEntitlementPolicy(context.Background(), policyV1.ID, AppEntitlementPolicyDraft{}), ErrEntitlementPolicyImmutable)
	assert.ErrorIs(t, svc.DeleteEntitlementPolicy(context.Background(), policyV1.ID), ErrEntitlementPolicyImmutable)
	cmdWithPolicy := appPluginInstallCommand("host-policy-version", "1.0.0")
	cmdWithPolicy.EntitlementPolicyID = policyV1.ID
	resp, err := svc.Install(context.Background(), cmdWithPolicy)
	require.NoError(t, err)
	assert.Equal(t, policyV1.ID, resp.EntitlementPolicyVersion)

	t.Run("empty policy reference remains empty for a disabled installation", func(t *testing.T) {
		cmdWithoutPolicy := appPluginInstallCommand("without-policy", "1.0.0")
		resp, err := svc.Install(context.Background(), cmdWithoutPolicy)
		require.NoError(t, err)
		assert.Empty(t, resp.EntitlementPolicyVersion)

		var defaultPolicyCount int64
		require.NoError(t, db.Model(&model.AppEntitlementPolicy{}).
			Where("id = ?", "host-default").
			Count(&defaultPolicyCount).Error)
		assert.Zero(t, defaultPolicyCount)
	})

	t.Run("unknown explicit policy fails without persistence", func(t *testing.T) {
		cmdWithMissingPolicy := appPluginInstallCommand("missing-policy", "1.0.0")
		cmdWithMissingPolicy.EntitlementPolicyID = "policy_missing"
		before := appPluginServicePersistenceCounts(t, db)

		_, err := svc.Install(context.Background(), cmdWithMissingPolicy)

		require.Error(t, err)
		assert.ErrorIs(t, err, model.ErrAppInstallRequestInvalid)
		assert.Equal(t, before, appPluginServicePersistenceCounts(t, db))
	})

	cmd := appPluginInstallCommand("manifest-policy", "1.0.0")
	cmd.ManifestJSON = []byte(`{"apiVersion":1,"kind":"app","key":"manifest-policy","name":{"en":"Manifest Policy","zh":"Manifest Policy"},"version":"1.0.0","callbackPath":"/callback","surfaces":{"direct":{"startPath":"/direct"},"embedded":{"startPath":"/embedded"}},"requestedScopes":["identity.read"],"requires":{"taskPlugins":[{"key":"doubao","minimumVersion":"1.2.0"}]},"entitlementPolicy":{"files":["write"]}}`)
	_, err = svc.Install(context.Background(), cmd)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrManifestCannotOwnEntitlementPolicy)
}

func TestAppInstallationOwnsParentOriginsAndEnabledSurfaces(t *testing.T) {
	db := openAppPluginServiceDB(t)
	logAppPluginServiceDBVersion(t, db)
	t.Run("records database runtime metadata", func(t *testing.T) {
		assertAppPluginServiceRuntimeMetadata(t, db)
	})
	require.NoError(t, model.MigrateAppPluginTables(db))

	svc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{
		TaskPluginChecker: fixedTaskPluginChecker{"doubao": "1.2.0"},
	})
	cmd := appPluginInstallCommand("host-owned", "1.0.0")
	cmd.BaseURL = "HTTPS://Apps.Example.COM/plugin/../plugin/"
	cmd.EnabledSurfaces = []string{"embedded", "direct", "direct"}
	cmd.AllowedParentOrigins = []string{"https://Console.Example.com/", "https://console.example.com"}
	cmd.AllowedOrigins = []string{"https://Apps.Example.com", "https://apps.example.com/"}
	cmd.AllowedUserPolicy = AppAllowedUserPolicy{Groups: []string{"default", "paid"}}
	cmd.NetworkPolicy = AppNetworkPolicy{AllowHosts: []string{"api.example.com"}, DenyPrivateIPRanges: true}

	resp, err := svc.Install(context.Background(), cmd)
	require.NoError(t, err)

	assert.Equal(t, "https://apps.example.com/plugin/", resp.BaseURL)
	assert.Equal(t, []string{"direct", "embedded"}, resp.EnabledSurfaces)
	assert.Equal(t, []string{"https://console.example.com"}, resp.AllowedParentOrigins)
	assert.Equal(t, []string{"https://apps.example.com"}, resp.AllowedOrigins)
	assert.Equal(t, cmd.AllowedUserPolicy, resp.AllowedUserPolicy)
	assert.Equal(t, cmd.NetworkPolicy, resp.NetworkPolicy)

	reordered := appPluginInstallCommand("canonical-json", "1.0.0")
	reordered.ManifestJSON = []byte(`{
		"requires":{"taskPlugins":[{"minimumVersion":"1.0.0","key":"other"},{"minimumVersion":"1.2.0","key":"doubao"}]},
		"requestedScopes":["model.invoke","identity.read"],
		"surfaces":{"embedded":{"startPath":"/embedded"},"direct":{"startPath":"/direct"}},
		"callbackPath":"/callback","version":"1.0.0","name":{"zh":"canonical-json","en":"canonical-json"},"key":"canonical-json","kind":"app","apiVersion":1
	}`)
	canonicalSvc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{
		TaskPluginChecker: fixedTaskPluginChecker{"doubao": "1.2.0", "other": "1.0.0"},
	})
	canonicalFirst, err := canonicalSvc.Install(context.Background(), reordered)
	require.NoError(t, err)
	reordered.ManifestJSON = []byte(`{"apiVersion":1,"kind":"app","key":"canonical-json","name":{"en":"canonical-json","zh":"canonical-json"},"version":"1.0.0","callbackPath":"/callback","surfaces":{"direct":{"startPath":"/direct"},"embedded":{"startPath":"/embedded"}},"requestedScopes":["identity.read","model.invoke"],"requires":{"taskPlugins":[{"key":"doubao","minimumVersion":"1.2.0"},{"key":"other","minimumVersion":"1.0.0"}]}}`)
	canonicalSecond, err := canonicalSvc.Install(context.Background(), reordered)
	require.NoError(t, err)
	assert.Equal(t, canonicalFirst.InstallationID, canonicalSecond.InstallationID)

	t.Run("same scope frozen replay precedes mutable dependency checks", func(t *testing.T) {
		checker := fixedTaskPluginChecker{"doubao": "1.2.0"}
		replaySvc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{TaskPluginChecker: checker})
		cmd := appPluginInstallCommand("same-scope-frozen-replay", "1.0.0")

		first, err := replaySvc.Install(context.Background(), cmd)
		require.NoError(t, err)
		delete(checker, "doubao")

		replayed, err := replaySvc.Install(context.Background(), cmd)
		require.NoError(t, err)
		assert.Equal(t, first, replayed)
	})

	t.Run("new scope generation replay precedes mutable dependency checks", func(t *testing.T) {
		checker := fixedTaskPluginChecker{"doubao": "1.2.0"}
		replaySvc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{TaskPluginChecker: checker})
		cmd := appPluginInstallCommand("new-scope-frozen-replay", "1.0.0")

		first, err := replaySvc.Install(context.Background(), cmd)
		require.NoError(t, err)
		delete(checker, "doubao")
		cmd.IdempotencyKey += "-replay"

		replayed, err := replaySvc.Install(context.Background(), cmd)
		require.NoError(t, err)
		assert.Equal(t, first, replayed)

		var frozen model.AppInstallationIdempotency
		require.NoError(t, db.Where("scope_key = ?", cmd.IdempotencyKey).First(&frozen).Error)
		assert.Equal(t, first.InstallationID, frozen.InstallationID)
		assert.Equal(t, first.ResponseDigest, frozen.ResponseDigest)
	})

	t.Run("new version still checks current dependencies", func(t *testing.T) {
		checker := fixedTaskPluginChecker{"doubao": "1.2.0"}
		replaySvc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{TaskPluginChecker: checker})
		first := appPluginInstallCommand("new-version-checks-dependencies", "1.0.0")
		_, err := replaySvc.Install(context.Background(), first)
		require.NoError(t, err)
		delete(checker, "doubao")
		before := appPluginServicePersistenceCounts(t, db)

		_, err = replaySvc.Install(context.Background(), appPluginInstallCommand("new-version-checks-dependencies", "2.0.0"))

		require.Error(t, err)
		assert.Contains(t, err.Error(), "task plugin doubao below 1.2.0")
		assert.Equal(t, before, appPluginServicePersistenceCounts(t, db))
	})

	t.Run("same scope different payload keeps idempotency conflict", func(t *testing.T) {
		checker := fixedTaskPluginChecker{"doubao": "1.2.0"}
		replaySvc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{TaskPluginChecker: checker})
		cmd := appPluginInstallCommand("same-scope-payload-conflict", "1.0.0")
		_, err := replaySvc.Install(context.Background(), cmd)
		require.NoError(t, err)
		delete(checker, "doubao")
		cmd.BaseURL = "https://apps.example.com/same-scope-payload-conflict-changed/"

		_, err = replaySvc.Install(context.Background(), cmd)

		require.Error(t, err)
		assert.ErrorIs(t, err, model.ErrAppIdempotencyConflict)
	})

	t.Run("frozen replay still rejects unsafe input before dependency checks", func(t *testing.T) {
		checker := fixedTaskPluginChecker{"doubao": "1.2.0"}
		replaySvc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{TaskPluginChecker: checker})
		cmd := appPluginInstallCommand("validated-frozen-replay", "1.0.0")
		_, err := replaySvc.Install(context.Background(), cmd)
		require.NoError(t, err)
		delete(checker, "doubao")

		tests := []struct {
			name   string
			mutate func(*AppInstallCommand)
		}{
			{
				name: "malformed raw manifest",
				mutate: func(replay *AppInstallCommand) {
					replay.ManifestJSON = []byte(`{"apiVersion":`)
				},
			},
			{
				name: "forbidden manifest field",
				mutate: func(replay *AppInstallCommand) {
					replay.ManifestJSON = []byte(`{"apiVersion":1,"kind":"app","key":"validated-frozen-replay","name":{"en":"Replay","zh":"Replay"},"version":"1.0.0","callbackPath":"/callback","surfaces":{"direct":{"startPath":"/direct"},"embedded":{"startPath":"/embedded"}},"requestedScopes":["identity.read"],"requires":{"taskPlugins":[{"key":"doubao","minimumVersion":"1.2.0"}]},"baseUrl":"https://evil.example"}`)
				},
			},
			{
				name: "invalid manifest schema",
				mutate: func(replay *AppInstallCommand) {
					replay.ManifestJSON = []byte(`{"apiVersion":1,"kind":"app","key":"validated-frozen-replay","name":{"en":"Replay","zh":"Replay"},"version":"1.0.0","callbackPath":"/callback/../outside","surfaces":{"direct":{"startPath":"/direct"},"embedded":{"startPath":"/embedded"}},"requestedScopes":["identity.read"],"requires":{"taskPlugins":[{"key":"doubao","minimumVersion":"1.2.0"}]}}`)
				},
			},
			{
				name: "insecure base URL",
				mutate: func(replay *AppInstallCommand) {
					replay.BaseURL = "http://apps.example.com/validated-frozen-replay/"
				},
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				replay := cmd
				test.mutate(&replay)

				_, err := replaySvc.Install(context.Background(), replay)

				require.Error(t, err)
				assert.NotContains(t, err.Error(), "task plugin")
			})
		}
	})

	withDefaultPort := appPluginInstallCommand("default-port-one", "1.0.0")
	withDefaultPort.BaseURL = "https://apps.example.com:443/shared/"
	_, err = svc.Install(context.Background(), withDefaultPort)
	require.NoError(t, err)
	withoutDefaultPort := appPluginInstallCommand("default-port-two", "1.0.0")
	withoutDefaultPort.BaseURL = "https://apps.example.com/shared/"
	_, err = svc.Install(context.Background(), withoutDefaultPort)
	require.ErrorIs(t, err, model.ErrAppRouteClaimConflict)

	override := appPluginInstallCommand("manifest-override", "1.0.0")
	override.ManifestJSON = []byte(`{"apiVersion":1,"kind":"app","key":"manifest-override","name":{"en":"Override","zh":"Override"},"version":"1.0.0","callbackPath":"/callback","surfaces":{"direct":{"startPath":"/direct"},"embedded":{"startPath":"/embedded"}},"requestedScopes":["identity.read"],"requires":{"taskPlugins":[{"key":"doubao","minimumVersion":"1.2.0"}]},"baseUrl":"https://evil.example","allowedOrigins":["https://evil.example"]}`)
	_, err = svc.Install(context.Background(), override)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrManifestCannotOwnHostPolicy)

	t.Run("install service validates the complete manifest before persistence", func(t *testing.T) {
		invalid := appPluginInstallCommand("service-manifest-boundary", "1.0.0")
		invalid.ManifestJSON = []byte(`{"apiVersion":1,"kind":"app","key":"service-manifest-boundary","name":{"en":"Invalid","zh":"Invalid"},"version":"1.0.0","callbackPath":"/callback/../outside","surfaces":{"direct":{"startPath":"/direct"},"embedded":{"startPath":"/embedded"}},"requestedScopes":["identity.read"],"requires":{"taskPlugins":[{"key":"doubao","minimumVersion":"1.2.0"}]}}`)
		before := appPluginServicePersistenceCounts(t, db)

		_, err := svc.Install(context.Background(), invalid)

		require.Error(t, err)
		assert.Equal(t, before, appPluginServicePersistenceCounts(t, db))
	})

	t.Run("default dependency checker uses semver prerelease precedence and injected doubao floor", func(t *testing.T) {
		require.NoError(t, db.AutoMigrate(&model.TaskPlugin{}))
		require.NoError(t, db.Where("1 = 1").Delete(&model.TaskPlugin{}).Error)
		require.NoError(t, db.Create(&model.TaskPlugin{
			Key:        "doubao-prerelease",
			APIVersion: 1,
			Version:    "1.2.0-beta.1",
			Source:     "module.exports = {};",
			SourceHash: serviceDigest("doubao-prerelease"),
			Enabled:    true,
			Active:     true,
		}).Error)
		checker := dbTaskPluginChecker{db: db}
		require.NoError(t, checker.CheckTaskPluginDependency(context.Background(), "doubao-prerelease", "1.2.0-alpha.1"))
		require.Error(t, checker.CheckTaskPluginDependency(context.Background(), "doubao-prerelease", "1.2.0"))

		require.NoError(t, db.Create(&model.TaskPlugin{
			Key:        "doubao",
			APIVersion: 1,
			Version:    "1.1.9",
			Source:     "module.exports = {};",
			SourceHash: serviceDigest("doubao-floor"),
			Enabled:    true,
			Active:     true,
		}).Error)
		defaultSvc := NewAppPluginInstallationService(db, AppPluginInstallationOptions{})
		_, err := defaultSvc.Install(context.Background(), appPluginInstallCommand("requires-injected-doubao", "1.0.0"))
		require.Error(t, err)
	})
}

func openAppPluginServiceDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialect := os.Getenv("APP_PLUGIN_TEST_DIALECT")
	dsn := os.Getenv("APP_PLUGIN_TEST_DSN")
	t.Logf("APP_PLUGIN_TEST_IMAGE=%s APP_PLUGIN_TEST_PLATFORM=%s APP_PLUGIN_TEST_DIALECT=%s", os.Getenv("APP_PLUGIN_TEST_IMAGE"), os.Getenv("APP_PLUGIN_TEST_PLATFORM"), dialect)
	if dialect == "" {
		dialect = "sqlite"
	}
	var (
		db  *gorm.DB
		err error
	)
	switch dialect {
	case "sqlite":
		if dsn == "" {
			dsn = filepath.Join(t.TempDir(), "app_plugin.sqlite")
		}
		db, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	case "mysql":
		require.NotEmpty(t, dsn, "APP_PLUGIN_TEST_DSN is required for mysql")
		db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	case "postgres", "postgresql":
		require.NotEmpty(t, dsn, "APP_PLUGIN_TEST_DSN is required for postgres")
		db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	default:
		t.Fatalf("unsupported APP_PLUGIN_TEST_DIALECT %q", dialect)
	}
	require.NoError(t, err)
	return db
}

func logAppPluginServiceDBVersion(t *testing.T, db *gorm.DB) {
	t.Helper()
	var version string
	switch db.Dialector.Name() {
	case "sqlite":
		require.NoError(t, db.Raw("select sqlite_version()").Scan(&version).Error)
	case "mysql":
		require.NoError(t, db.Raw("select version()").Scan(&version).Error)
	case "postgres":
		require.NoError(t, db.Raw("select version()").Scan(&version).Error)
	}
	t.Logf("database=%s version=%s", db.Dialector.Name(), version)
}

func assertAppPluginServiceRuntimeMetadata(t *testing.T, db *gorm.DB) {
	t.Helper()
	dialect := db.Dialector.Name()
	require.Equal(t, dialect, os.Getenv("APP_PLUGIN_TEST_DIALECT"))
	require.NotEmpty(t, os.Getenv("APP_PLUGIN_TEST_DRIVER"))

	var version string
	switch dialect {
	case "sqlite":
		require.NoError(t, db.Raw("select sqlite_version()").Scan(&version).Error)
		require.Equal(t, "github.com/glebarez/sqlite@v1.11.0", os.Getenv("APP_PLUGIN_TEST_DRIVER"))
		require.Empty(t, os.Getenv("APP_PLUGIN_TEST_IMAGE"))
		require.Empty(t, os.Getenv("APP_PLUGIN_TEST_PLATFORM"))
	case "mysql":
		require.NoError(t, db.Raw("select version()").Scan(&version).Error)
		require.Equal(t, "gorm.io/driver/mysql@v1.5.7", os.Getenv("APP_PLUGIN_TEST_DRIVER"))
		require.Equal(t, "mysql:5.7.44@sha256:4bc6bc963e6d8443453676cae56536f4b8156d78bae03c0145cbe47c2aad73bb", os.Getenv("APP_PLUGIN_TEST_IMAGE"))
		require.Equal(t, "linux/amd64", os.Getenv("APP_PLUGIN_TEST_PLATFORM"))
	case "postgres":
		require.NoError(t, db.Raw("select version()").Scan(&version).Error)
		require.Equal(t, "gorm.io/driver/postgres@v1.5.9", os.Getenv("APP_PLUGIN_TEST_DRIVER"))
		require.Equal(t, "postgres:15.19@sha256:9b1d34adbce1dd07ee6e94b4a2cf698884b89bd44a6c9c12f5da8f3acbfe4957", os.Getenv("APP_PLUGIN_TEST_IMAGE"))
		require.Equal(t, "linux/arm64", os.Getenv("APP_PLUGIN_TEST_PLATFORM"))
	default:
		t.Fatalf("unsupported database dialect %q", dialect)
	}
	require.Equal(t, version, os.Getenv("APP_PLUGIN_TEST_DATABASE_VERSION"))
}

func appPluginInstallCommand(key, version string) AppInstallCommand {
	baseURL := fmt.Sprintf("https://apps.example.com/%s/", key)
	manifest := []byte(fmt.Sprintf(`{"apiVersion":1,"kind":"app","key":%q,"name":{"en":%q,"zh":%q},"version":%q,"callbackPath":"/callback","surfaces":{"direct":{"startPath":"/direct"},"embedded":{"startPath":"/embedded"}},"requestedScopes":["identity.read"],"requires":{"taskPlugins":[{"key":"doubao","minimumVersion":"1.2.0"}]}}`, key, key, key, version))
	return AppInstallCommand{
		ActorID:              101,
		IdempotencyKey:       "install-" + key + "-" + version,
		ManifestJSON:         manifest,
		BaseURL:              baseURL,
		EnabledSurfaces:      []string{"direct", "embedded"},
		AllowedParentOrigins: []string{"https://console.example.com"},
		AllowedOrigins:       []string{"https://apps.example.com"},
		AllowedUserPolicy:    AppAllowedUserPolicy{Groups: []string{"default"}},
		NetworkPolicy:        AppNetworkPolicy{AllowHosts: []string{"api.example.com"}},
		ServiceCredential: AppServiceCredentialInput{
			ID:        "cred_" + key,
			Secret:    "secret-value-" + key,
			Version:   "v1",
			ExpiresAt: 4102444800,
		},
	}
}

type fixedTaskPluginChecker map[string]string

func (f fixedTaskPluginChecker) CheckTaskPluginDependency(_ context.Context, key, minimumVersion string) error {
	if f[key] >= minimumVersion {
		return nil
	}
	return fmt.Errorf("task plugin %s below %s", key, minimumVersion)
}

func sortedMapKeys(fields map[string]any) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func appPluginServicePersistenceCounts(t *testing.T, db *gorm.DB) [5]int64 {
	t.Helper()
	var counts [5]int64
	for i, table := range []any{
		&model.AppVersion{},
		&model.AppInstallation{},
		&model.AppInstallationIdempotency{},
		&model.AppRouteClaim{},
		&model.AppServiceCredential{},
	} {
		require.NoError(t, db.Model(table).Count(&counts[i]).Error)
	}
	return counts
}

func serviceDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}
