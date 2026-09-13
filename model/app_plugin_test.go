package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestAppPluginMigrationFreshUpgradeReplay(t *testing.T) {
	db := openAppPluginModelDB(t)
	logAppPluginDBVersion(t, db)
	t.Run("records database runtime metadata", func(t *testing.T) {
		assertAppPluginModelRuntimeMetadata(t, db)
	})

	t.Run("fresh migration creates every host-owned app plugin table", func(t *testing.T) {
		require.NoError(t, MigrateAppPluginTables(db))
		for _, table := range []string{
			"app_versions",
			"app_installations",
			"app_installation_idempotencies",
			"app_route_claims",
			"app_service_credentials",
			"app_entitlement_policies",
		} {
			assert.True(t, db.Migrator().HasTable(table), table)
		}
	})

	t.Run("upgrade preserves seeded legacy task plugin rows", func(t *testing.T) {
		require.NoError(t, db.AutoMigrate(&TaskPlugin{}))
		legacy := TaskPlugin{
			Key:        "doubao",
			APIVersion: 1,
			Version:    "1.2.0",
			Source:     "module.exports = {};",
			SourceHash: appPluginDigest("doubao-1.2.0"),
			Enabled:    true,
			Active:     true,
		}
		require.NoError(t, db.Create(&legacy).Error)
		require.NoError(t, MigrateAppPluginTables(db))
		require.NoError(t, MigrateAppPluginTables(db))

		var reloaded TaskPlugin
		require.NoError(t, db.Where(&TaskPlugin{Key: "doubao"}).First(&reloaded).Error)
		assert.Equal(t, "1.2.0", reloaded.Version)
		assert.True(t, reloaded.Enabled)
		assert.True(t, reloaded.Active)
	})

	t.Run("model main migration integration calls the same app plugin migrator", func(t *testing.T) {
		require.NoError(t, MigrateAppPluginTables(db))
		assert.True(t, db.Migrator().HasTable(&AppInstallation{}))
	})

	t.Run("upgrade preserves legacy app installation public identifier rows", func(t *testing.T) {
		oldDB := openAppPluginModelDB(t)
		require.NoError(t, oldDB.Migrator().DropTable("app_installations"))
		require.NoError(t, oldDB.AutoMigrate(&legacyAppInstallation{}))
		require.NoError(t, oldDB.Create(&legacyAppInstallation{
			ID:                   77,
			AppKey:               "legacy",
			AppVersionID:         "appver",
			ManifestVersion:      "1.0.0",
			ManifestSHA256:       appPluginDigest("legacy"),
			BaseURL:              "https://apps.example.com/legacy/",
			EnabledSurfaces:      AppStringList{},
			AllowedParentOrigins: AppStringList{},
			AllowedOrigins:       AppStringList{},
			AllowedUserPolicy:    AppAllowedUserPolicy{},
			NetworkPolicy:        AppNetworkPolicy{},
			EntitlementPolicyID:  "policy",
			Status:               AppInstallationStatusDisabled,
			Revision:             1,
		}).Error)
		require.NoError(t, MigrateAppPluginTables(oldDB))

		var upgraded AppInstallation
		require.NoError(t, oldDB.Where("id = ?", 77).First(&upgraded).Error)
		assert.Equal(t, "legacy", upgraded.AppKey)
		assert.Equal(t, "77", upgraded.InstallationID)
	})
}

type legacyAppInstallation struct {
	ID                   uint   `gorm:"primaryKey"`
	AppKey               string `gorm:"size:128;not null;index"`
	AppVersionID         string `gorm:"size:64;not null;index"`
	ManifestVersion      string `gorm:"size:64;not null"`
	ManifestSHA256       string `gorm:"size:64;not null"`
	BaseURL              string `gorm:"size:512;not null"`
	EnabledSurfaces      AppStringList
	AllowedParentOrigins AppStringList
	AllowedOrigins       AppStringList
	AllowedUserPolicy    AppAllowedUserPolicy
	NetworkPolicy        AppNetworkPolicy
	EntitlementPolicyID  string `gorm:"size:64;not null"`
	Status               string `gorm:"size:32;not null;index"`
	Revision             int64  `gorm:"not null"`
}

func (legacyAppInstallation) TableName() string {
	return "app_installations"
}

func TestAppInstallIdempotencyAndVersionConflict(t *testing.T) {
	db := openAppPluginModelDB(t)
	logAppPluginDBVersion(t, db)
	t.Run("records database runtime metadata", func(t *testing.T) {
		assertAppPluginModelRuntimeMetadata(t, db)
	})
	require.NoError(t, MigrateAppPluginTables(db))

	t.Run("same scope request hash and content digest replays frozen response with stable IDs", func(t *testing.T) {
		req := appPluginModelInstallRequest("writer", "1.0.0", "https://apps.example.com/writer/")
		scope := AppIdempotencyScope{ActorID: 11, Key: "install-writer-1"}

		first, err := InstallAppVersion(context.Background(), db, scope, req)
		require.NoError(t, err)
		second, err := InstallAppVersion(context.Background(), db, scope, req)
		require.NoError(t, err)

		assert.Equal(t, first.AppVersionID, second.AppVersionID)
		assert.Equal(t, first.InstallationID, second.InstallationID)
		assert.Equal(t, first.ResponseDigest, second.ResponseDigest)
		assert.Equal(t, first, second)
	})

	t.Run("same app key and version with different canonical manifest digest is a stable conflict", func(t *testing.T) {
		first := appPluginModelInstallRequest("writer", "1.1.0", "https://apps.example.com/writer-1-1/")
		second := first
		second.CanonicalManifestJSON = []byte(`{"apiVersion":1,"kind":"app","key":"writer","version":"1.1.0","name":"changed"}`)
		second.ManifestSHA256 = appPluginDigest(string(second.CanonicalManifestJSON))

		_, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 12, Key: "first"}, first)
		require.NoError(t, err)
		_, err = InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 12, Key: "second"}, second)

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrAppVersionConflict)
		assert.Equal(t, "app_version_conflict", AppPluginErrorCode(err))
	})

	t.Run("same idempotency scope with a different request hash conflicts", func(t *testing.T) {
		scope := AppIdempotencyScope{ActorID: 13, Key: "install-writer-2"}
		first := appPluginModelInstallRequest("reader", "1.0.0", "https://apps.example.com/reader/")
		second := appPluginModelInstallRequest("reader", "1.0.0", "https://apps.example.com/reader-v2/")

		_, err := InstallAppVersion(context.Background(), db, scope, first)
		require.NoError(t, err)
		_, err = InstallAppVersion(context.Background(), db, scope, second)

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrAppIdempotencyConflict)
	})

	t.Run("failed install does not reserve an idempotency row", func(t *testing.T) {
		scope := AppIdempotencyScope{ActorID: 14, Key: "route-collision-retry"}
		first := appPluginModelInstallRequest("owner", "1.0.0", "https://apps.example.com/owner/")
		colliding := appPluginModelInstallRequest("intruder", "1.0.0", "https://apps.example.com/intruder/")
		colliding.CallbackURL = first.CallbackURL

		_, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 14, Key: "owner"}, first)
		require.NoError(t, err)
		_, err = InstallAppVersion(context.Background(), db, scope, colliding)
		require.Error(t, err)

		var rows int64
		require.NoError(t, db.Model(&AppInstallationIdempotency{}).Where("scope_key = ?", scope.Key).Count(&rows).Error)
		assert.Zero(t, rows)
	})

	t.Run("scope identity remains case-sensitive on every database", func(t *testing.T) {
		upper := appPluginModelInstallRequest("scope-upper", "1.0.0", "https://apps.example.com/scope-upper/")
		lower := appPluginModelInstallRequest("scope-lower", "1.0.0", "https://apps.example.com/scope-lower/")

		_, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 15, Key: "CaseSensitive"}, upper)
		require.NoError(t, err)
		_, err = InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 15, Key: "casesensitive"}, lower)
		require.NoError(t, err)
	})

	t.Run("rejects tampered or malformed canonical manifest without persistence", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(*AppInstallRequest)
		}{
			{
				name: "tampered digest",
				mutate: func(req *AppInstallRequest) {
					req.ManifestSHA256 = appPluginDigest("tampered")
				},
			},
			{
				name: "malformed JSON",
				mutate: func(req *AppInstallRequest) {
					req.CanonicalManifestJSON = []byte(`{"key":`)
					req.ManifestSHA256 = appPluginDigest(string(req.CanonicalManifestJSON))
				},
			},
		}
		for i, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				key := fmt.Sprintf("invalid-manifest-%d", i)
				req := appPluginModelInstallRequest(key, "1.0.0", fmt.Sprintf("https://apps.example.com/invalid/%d/", i))
				test.mutate(&req)
				before := appPluginPersistenceCounts(t, db)

				_, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 16, Key: fmt.Sprintf("%s-%d", test.name, i)}, req)

				require.Error(t, err)
				assert.ErrorIs(t, err, ErrAppInstallRequestInvalid)
				assert.Equal(t, before, appPluginPersistenceCounts(t, db))
			})
		}
	})

	t.Run("rejects manifest key or version mismatch without persistence", func(t *testing.T) {
		for i, field := range []string{"key", "version"} {
			t.Run(field, func(t *testing.T) {
				key := fmt.Sprintf("manifest-mismatch-%d", i)
				req := appPluginModelInstallRequest(key, "1.0.0", fmt.Sprintf("https://apps.example.com/mismatch/%d/", i))
				if field == "key" {
					req.AppKey = "different-key"
				} else {
					req.ManifestVersion = "2.0.0"
				}
				before := appPluginPersistenceCounts(t, db)

				_, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 17, Key: fmt.Sprintf("%s-%d", field, i)}, req)

				require.Error(t, err)
				assert.ErrorIs(t, err, ErrAppInstallRequestInvalid)
				assert.Equal(t, before, appPluginPersistenceCounts(t, db))
			})
		}
	})

	t.Run("rejects invalid host-owned input without persistence", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(*AppInstallRequest)
		}{
			{name: "insecure base URL", mutate: func(req *AppInstallRequest) { req.BaseURL = "http://apps.example.com/plugin/" }},
			{name: "endpoint outside base URL", mutate: func(req *AppInstallRequest) { req.CallbackURL = "https://evil.example/callback" }},
			{name: "unknown enabled surface", mutate: func(req *AppInstallRequest) { req.EnabledSurfaces = []string{"admin"} }},
			{name: "non-origin parent URL", mutate: func(req *AppInstallRequest) { req.AllowedParentOrigins = []string{"https://console.example.com/path"} }},
			{name: "insecure allowed origin", mutate: func(req *AppInstallRequest) { req.AllowedOrigins = []string{"http://apps.example.com"} }},
			{name: "invalid user group reference", mutate: func(req *AppInstallRequest) { req.AllowedUserPolicy.Groups = []string{""} }},
			{name: "invalid network host reference", mutate: func(req *AppInstallRequest) { req.NetworkPolicy.AllowHosts = []string{"https://api.example.com/path"} }},
			{name: "missing entitlement policy reference", mutate: func(req *AppInstallRequest) { req.EntitlementPolicyID = "" }},
			{name: "missing credential ID", mutate: func(req *AppInstallRequest) { req.ServiceCredentialID = "" }},
			{name: "missing credential version", mutate: func(req *AppInstallRequest) { req.ServiceCredentialVersion = "" }},
			{name: "invalid credential hash", mutate: func(req *AppInstallRequest) { req.ServiceCredentialHash = "not-a-sha256" }},
			{name: "invalid credential expiry", mutate: func(req *AppInstallRequest) { req.ServiceCredentialExpiry = 0 }},
		}
		for i, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				key := fmt.Sprintf("invalid-host-%d", i)
				req := appPluginModelInstallRequest(key, "1.0.0", fmt.Sprintf("https://apps.example.com/invalid-host/%d/", i))
				test.mutate(&req)
				before := appPluginPersistenceCounts(t, db)

				_, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 18, Key: fmt.Sprintf("%s-%d", test.name, i)}, req)

				require.Error(t, err)
				assert.ErrorIs(t, err, ErrAppInstallRequestInvalid)
				assert.Equal(t, before, appPluginPersistenceCounts(t, db))
			})
		}
	})
}

func TestAppInstallRouteCollisionIsAtomic(t *testing.T) {
	db := openAppPluginModelDB(t)
	logAppPluginDBVersion(t, db)
	t.Run("records database runtime metadata", func(t *testing.T) {
		assertAppPluginModelRuntimeMetadata(t, db)
	})
	require.NoError(t, MigrateAppPluginTables(db))

	first := appPluginModelInstallRequest("routes-one", "1.0.0", "https://apps.example.com/one/")
	result, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 21, Key: "routes-one"}, first)
	require.NoError(t, err)

	for _, endpoint := range []string{first.CallbackURL, first.DirectURL, first.EmbeddedURL} {
		var claim AppRouteClaim
		require.NoError(t, db.Where("absolute_endpoint = ?", endpoint).First(&claim).Error)
		assert.Equal(t, result.InstallationID, claim.InstallationID)
	}

	colliding := appPluginModelInstallRequest("routes-two", "1.0.0", "https://apps.example.com/one/")
	_, err = InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 22, Key: "routes-two"}, colliding)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAppRouteClaimConflict)

	for _, table := range []any{&AppVersion{}, &AppInstallation{}, &AppRouteClaim{}, &AppServiceCredential{}} {
		var rows int64
		require.NoError(t, db.Model(table).Where("app_key = ?", "routes-two").Count(&rows).Error)
		assert.Zero(t, rows)
	}
}

func TestAppLifecycleCASAndRevokedTerminal(t *testing.T) {
	db := openAppPluginModelDB(t)
	logAppPluginDBVersion(t, db)
	t.Run("records database runtime metadata", func(t *testing.T) {
		assertAppPluginModelRuntimeMetadata(t, db)
	})
	require.NoError(t, MigrateAppPluginTables(db))

	install := appPluginModelInstallRequest("lifecycle", "1.0.0", "https://apps.example.com/lifecycle/")
	created, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 31, Key: "lifecycle"}, install)
	require.NoError(t, err)
	assert.Equal(t, AppInstallationStatusDisabled, created.Status)
	assert.Equal(t, int64(1), created.Revision)

	enabled, err := CompareAndSwapAppInstallationStatus(context.Background(), db, created.InstallationID, created.Revision, AppInstallationStatusEnabled)
	require.NoError(t, err)
	assert.Equal(t, AppInstallationStatusEnabled, enabled.Status)
	disabled, err := CompareAndSwapAppInstallationStatus(context.Background(), db, created.InstallationID, enabled.Revision, AppInstallationStatusDisabled)
	require.NoError(t, err)
	assert.Equal(t, AppInstallationStatusDisabled, disabled.Status)
	revoked, err := CompareAndSwapAppInstallationStatus(context.Background(), db, created.InstallationID, disabled.Revision, AppInstallationStatusRevoked)
	require.NoError(t, err)
	assert.Equal(t, AppInstallationStatusRevoked, revoked.Status)

	_, err = CompareAndSwapAppInstallationStatus(context.Background(), db, created.InstallationID, revoked.Revision, AppInstallationStatusEnabled)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAppInstallationRevoked)

	reinstalled, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 31, Key: "lifecycle-reinstall"}, install)
	require.NoError(t, err)
	assert.NotEqual(t, created.InstallationID, reinstalled.InstallationID)
	var generationCount int64
	require.NoError(t, db.Model(&AppVersion{}).Where("app_key = ? AND manifest_version = ?", "lifecycle", "1.0.0").Count(&generationCount).Error)
	assert.Equal(t, int64(1), generationCount)

	race := appPluginModelInstallRequest("race", "1.0.0", "https://apps.example.com/race/")
	raceInstall, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 32, Key: "race"}, race)
	require.NoError(t, err)
	firstWinner, firstErr := CompareAndSwapAppInstallationStatus(context.Background(), db, raceInstall.InstallationID, raceInstall.Revision, AppInstallationStatusEnabled)
	secondWinner, secondErr := CompareAndSwapAppInstallationStatus(context.Background(), db, raceInstall.InstallationID, raceInstall.Revision, AppInstallationStatusRevoked)
	assert.NoError(t, firstErr)
	assert.ErrorIs(t, secondErr, ErrAppInstallationRevisionConflict)
	assert.Equal(t, AppInstallationStatusEnabled, firstWinner.Status)
	assert.Empty(t, secondWinner.InstallationID)

	concurrent := appPluginModelInstallRequest("concurrent-cas", "1.0.0", "https://apps.example.com/concurrent-cas/")
	concurrentInstall, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 33, Key: "concurrent-cas"}, concurrent)
	require.NoError(t, err)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, target := range []string{AppInstallationStatusEnabled, AppInstallationStatusRevoked} {
		wg.Add(1)
		go func(next string) {
			defer wg.Done()
			<-start
			_, updateErr := CompareAndSwapAppInstallationStatus(context.Background(), db, concurrentInstall.InstallationID, concurrentInstall.Revision, next)
			errs <- updateErr
		}(target)
	}
	close(start)
	wg.Wait()
	close(errs)

	successes := 0
	conflicts := 0
	for updateErr := range errs {
		switch {
		case updateErr == nil:
			successes++
		case errors.Is(updateErr, ErrAppInstallationRevisionConflict), errors.Is(updateErr, ErrAppInstallationRevoked):
			conflicts++
		default:
			require.NoError(t, updateErr)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
}

func openAppPluginModelDB(t *testing.T) *gorm.DB {
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

func logAppPluginDBVersion(t *testing.T, db *gorm.DB) {
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

func assertAppPluginModelRuntimeMetadata(t *testing.T, db *gorm.DB) {
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

func appPluginModelInstallRequest(key, version, baseURL string) AppInstallRequest {
	manifest := []byte(`{"apiVersion":1,"kind":"app","key":"` + key + `","version":"` + version + `"}`)
	return AppInstallRequest{
		AppKey:                   key,
		ManifestVersion:          version,
		ManifestSHA256:           appPluginDigest(string(manifest)),
		CanonicalManifestJSON:    manifest,
		BaseURL:                  baseURL,
		CallbackURL:              baseURL + "callback",
		DirectURL:                baseURL + "direct",
		EmbeddedURL:              baseURL + "embedded",
		EnabledSurfaces:          []string{"direct", "embedded"},
		AllowedParentOrigins:     []string{"https://console.example.com"},
		AllowedOrigins:           []string{"https://apps.example.com"},
		AllowedUserPolicy:        AppAllowedUserPolicy{Groups: []string{"default"}},
		NetworkPolicy:            AppNetworkPolicy{AllowHosts: []string{"api.example.com"}},
		EntitlementPolicyID:      "policy-basic",
		ServiceCredentialHash:    appPluginDigest("credential:" + key),
		ServiceCredentialID:      "cred_" + key,
		ServiceCredentialVersion: "v1",
		ServiceCredentialExpiry:  4102444800,
	}
}

func appPluginPersistenceCounts(t *testing.T, db *gorm.DB) [4]int64 {
	t.Helper()
	var counts [4]int64
	for i, table := range []any{
		&AppVersion{},
		&AppInstallation{},
		&AppInstallationIdempotency{},
		&AppRouteClaim{},
	} {
		require.NoError(t, db.Model(table).Count(&counts[i]).Error)
	}
	return counts
}

func appPluginDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}
