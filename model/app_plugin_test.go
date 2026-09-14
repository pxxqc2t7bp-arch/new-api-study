package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
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

		var ownership AppRouteClaim
		require.NoError(t, oldDB.Where("app_key = ? AND kind = ?", "legacy", "app_key").First(&ownership).Error)
		assert.Equal(t, upgraded.InstallationID, ownership.InstallationID)
	})

	t.Run("partial DDL resume backfills null and empty public identifiers before auto migration", func(t *testing.T) {
		oldDB := openAppPluginModelDB(t)
		require.NoError(t, oldDB.Migrator().DropTable(&AppRouteClaim{}, &AppInstallation{}))
		require.NoError(t, oldDB.AutoMigrate(&legacyAppInstallation{}))
		legacyInstallations := []legacyAppInstallation{
			{
				ID:                   301,
				AppKey:               "partial-ddl-null",
				AppVersionID:         "appver-null",
				ManifestVersion:      "1.0.0",
				ManifestSHA256:       appPluginDigest("partial-ddl-null"),
				BaseURL:              "https://apps.example.com/partial-ddl-null/",
				EnabledSurfaces:      AppStringList{},
				AllowedParentOrigins: AppStringList{},
				AllowedOrigins:       AppStringList{},
				AllowedUserPolicy:    AppAllowedUserPolicy{},
				NetworkPolicy:        AppNetworkPolicy{},
				EntitlementPolicyID:  "policy",
				Status:               AppInstallationStatusDisabled,
				Revision:             1,
			},
			{
				ID:                   302,
				AppKey:               "partial-ddl-empty",
				AppVersionID:         "appver-empty",
				ManifestVersion:      "1.0.0",
				ManifestSHA256:       appPluginDigest("partial-ddl-empty"),
				BaseURL:              "https://apps.example.com/partial-ddl-empty/",
				EnabledSurfaces:      AppStringList{},
				AllowedParentOrigins: AppStringList{},
				AllowedOrigins:       AppStringList{},
				AllowedUserPolicy:    AppAllowedUserPolicy{},
				NetworkPolicy:        AppNetworkPolicy{},
				EntitlementPolicyID:  "policy",
				Status:               AppInstallationStatusDisabled,
				Revision:             1,
			},
		}
		require.NoError(t, oldDB.Create(&legacyInstallations).Error)

		columnType := "text"
		if oldDB.Dialector.Name() != "sqlite" {
			columnType = "varchar(64)"
		}
		require.NoError(t, oldDB.Exec("ALTER TABLE app_installations ADD COLUMN installation_id "+columnType).Error)
		require.NoError(t, oldDB.Exec("UPDATE app_installations SET installation_id = '' WHERE id = ?", 302).Error)

		require.NoError(t, MigrateAppPluginTables(oldDB))
		require.NoError(t, MigrateAppPluginTables(oldDB))

		var upgraded []AppInstallation
		require.NoError(t, oldDB.Where("id IN ?", []uint{301, 302}).Order("id").Find(&upgraded).Error)
		require.Len(t, upgraded, 2)
		assert.Equal(t, []string{"301", "302"}, []string{upgraded[0].InstallationID, upgraded[1].InstallationID})
		for _, installation := range upgraded {
			var ownership AppRouteClaim
			require.NoError(t, oldDB.Where("app_key = ? AND kind = ?", installation.AppKey, "app_key").First(&ownership).Error)
			assert.Equal(t, installation.InstallationID, ownership.InstallationID)
		}
	})

	t.Run("upgrade converges duplicate legacy ownership without deleting history", func(t *testing.T) {
		oldDB := openAppPluginModelDB(t)
		require.NoError(t, oldDB.Migrator().DropTable(
			&AppRouteClaim{},
			&AppInstallationIdempotency{},
			&AppServiceCredential{},
			&AppInstallation{},
		))
		require.NoError(t, oldDB.AutoMigrate(
			&legacyAppInstallation{},
			&AppInstallationIdempotency{},
			&AppServiceCredential{},
		))

		legacyInstallations := []legacyAppInstallation{
			{
				ID:                   201,
				AppKey:               "legacy-duplicate",
				AppVersionID:         "appver-disabled",
				ManifestVersion:      "1.0.0",
				ManifestSHA256:       appPluginDigest("legacy-disabled"),
				BaseURL:              "https://apps.example.com/legacy-disabled/",
				EnabledSurfaces:      AppStringList{},
				AllowedParentOrigins: AppStringList{},
				AllowedOrigins:       AppStringList{},
				AllowedUserPolicy:    AppAllowedUserPolicy{},
				NetworkPolicy:        AppNetworkPolicy{},
				EntitlementPolicyID:  "policy",
				Status:               AppInstallationStatusDisabled,
				Revision:             3,
			},
			{
				ID:                   202,
				AppKey:               "legacy-duplicate",
				AppVersionID:         "appver-enabled-owner",
				ManifestVersion:      "2.0.0",
				ManifestSHA256:       appPluginDigest("legacy-enabled-owner"),
				BaseURL:              "https://apps.example.com/legacy-enabled-owner/",
				EnabledSurfaces:      AppStringList{"direct"},
				AllowedParentOrigins: AppStringList{},
				AllowedOrigins:       AppStringList{},
				AllowedUserPolicy:    AppAllowedUserPolicy{},
				NetworkPolicy:        AppNetworkPolicy{},
				EntitlementPolicyID:  "policy",
				Status:               AppInstallationStatusEnabled,
				Revision:             7,
			},
			{
				ID:                   203,
				AppKey:               "legacy-duplicate",
				AppVersionID:         "appver-enabled-non-owner",
				ManifestVersion:      "3.0.0",
				ManifestSHA256:       appPluginDigest("legacy-enabled-non-owner"),
				BaseURL:              "https://apps.example.com/legacy-enabled-non-owner/",
				EnabledSurfaces:      AppStringList{"embedded"},
				AllowedParentOrigins: AppStringList{"https://console.example.com"},
				AllowedOrigins:       AppStringList{},
				AllowedUserPolicy:    AppAllowedUserPolicy{},
				NetworkPolicy:        AppNetworkPolicy{},
				EntitlementPolicyID:  "policy",
				Status:               AppInstallationStatusEnabled,
				Revision:             11,
			},
		}
		require.NoError(t, oldDB.Create(&legacyInstallations).Error)

		for _, installation := range legacyInstallations {
			installationID := fmt.Sprintf("%d", installation.ID)
			require.NoError(t, oldDB.Create(&AppServiceCredential{
				AppKey:            installation.AppKey,
				InstallationID:    installationID,
				CredentialID:      "cred-" + installationID,
				CredentialHash:    appPluginDigest("credential-" + installationID),
				CredentialVersion: "v" + installationID,
				Status:            "active",
				ExpiresAt:         4102444800,
			}).Error)
			require.NoError(t, oldDB.Create(&AppInstallationIdempotency{
				ScopeHash:      appPluginDigest("scope-" + installationID),
				ActorID:        int64(installation.ID),
				ScopeKey:       "scope-" + installationID,
				ClaimToken:     appPluginDigest("claim-" + installationID),
				RequestHash:    appPluginDigest("request-" + installationID),
				InstallationID: installationID,
				AppVersionID:   installation.AppVersionID,
				ResponseJSON:   `{"installation_id":"` + installationID + `"}`,
				ResponseDigest: appPluginDigest("response-" + installationID),
			}).Error)
		}

		require.NoError(t, MigrateAppPluginTables(oldDB))
		require.NoError(t, MigrateAppPluginTables(oldDB))

		var upgraded []AppInstallation
		require.NoError(t, oldDB.Where("app_key = ?", "legacy-duplicate").Order("id").Find(&upgraded).Error)
		require.Len(t, upgraded, 3)
		assert.Equal(t, []string{
			AppInstallationStatusRevoked,
			AppInstallationStatusEnabled,
			AppInstallationStatusRevoked,
		}, []string{upgraded[0].Status, upgraded[1].Status, upgraded[2].Status})
		assert.Equal(t, []int64{4, 7, 12}, []int64{upgraded[0].Revision, upgraded[1].Revision, upgraded[2].Revision})

		var ownership []AppRouteClaim
		require.NoError(t, oldDB.Where("app_key = ? AND kind = ?", "legacy-duplicate", "app_key").Find(&ownership).Error)
		require.Len(t, ownership, 1)
		assert.Equal(t, "202", ownership[0].InstallationID)

		var credentials []AppServiceCredential
		require.NoError(t, oldDB.Where("app_key = ?", "legacy-duplicate").Order("installation_id").Find(&credentials).Error)
		require.Len(t, credentials, 3)
		assert.Equal(t, []string{"cred-201", "cred-202", "cred-203"}, []string{
			credentials[0].CredentialID,
			credentials[1].CredentialID,
			credentials[2].CredentialID,
		})
		assert.Equal(t, []string{AppInstallationStatusRevoked, "active", AppInstallationStatusRevoked}, []string{
			credentials[0].Status,
			credentials[1].Status,
			credentials[2].Status,
		})

		var frozen []AppInstallationIdempotency
		require.NoError(t, oldDB.Where("installation_id IN ?", []string{"201", "202", "203"}).Order("installation_id").Find(&frozen).Error)
		require.Len(t, frozen, 3)
		assert.Equal(t, []string{
			`{"installation_id":"201"}`,
			`{"installation_id":"202"}`,
			`{"installation_id":"203"}`,
		}, []string{frozen[0].ResponseJSON, frozen[1].ResponseJSON, frozen[2].ResponseJSON})
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
	seedAppPluginModelPolicy(t, db, "policy-basic")

	t.Run("maps model errors to the stable error registry", func(t *testing.T) {
		tests := []struct {
			name string
			err  error
			code string
		}{
			{name: "version conflict", err: ErrAppVersionConflict, code: "app_version_conflict"},
			{name: "idempotency conflict", err: ErrAppIdempotencyConflict, code: "idempotency_conflict"},
			{name: "route collision", err: ErrAppRouteClaimConflict, code: "app_route_collision"},
			{name: "revision conflict", err: ErrAppInstallationRevisionConflict, code: "version_conflict"},
			{name: "revoked installation", err: ErrAppInstallationRevoked, code: "invalid_state_transition"},
			{name: "invalid installation status", err: ErrAppInstallationStatusInvalid, code: "invalid_state_transition"},
			{name: "invalid install request", err: ErrAppInstallRequestInvalid, code: "validation_error"},
			{name: "unknown internal error", err: errors.New("database unavailable"), code: "service_unavailable"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				assert.Equal(t, test.code, AppPluginErrorCode(fmt.Errorf("wrapped: %w", test.err)))
			})
		}
	})

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

	t.Run("same immutable version replays its original response across idempotency scopes", func(t *testing.T) {
		for _, differentHostInput := range []bool{false, true} {
			t.Run(fmt.Sprintf("different_host_input_%t", differentHostInput), func(t *testing.T) {
				key := fmt.Sprintf("version-replay-%t", differentHostInput)
				firstRequest := appPluginModelInstallRequest(key, "1.0.0", "https://apps.example.com/"+key+"/")
				first, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 111, Key: key + "-first"}, firstRequest)
				require.NoError(t, err)

				replayRequest := firstRequest
				if differentHostInput {
					replayRequest.BaseURL = "https://apps.example.com/" + key + "-changed/"
					replayRequest.CallbackURL = replayRequest.BaseURL + "callback"
					replayRequest.DirectURL = replayRequest.BaseURL + "direct"
					replayRequest.EmbeddedURL = replayRequest.BaseURL + "embedded"
					replayRequest.EnabledSurfaces = []string{"direct"}
					replayRequest.AllowedOrigins = []string{"https://changed.example.com"}
					replayRequest.ServiceCredentialID = "changed-" + replayRequest.ServiceCredentialID
				}
				replayScope := AppIdempotencyScope{ActorID: 111, Key: key + "-replay"}
				replayed, err := InstallAppVersion(context.Background(), db, replayScope, replayRequest)
				require.NoError(t, err)
				assert.Equal(t, first, replayed)

				replayedAgain, err := InstallAppVersion(context.Background(), db, replayScope, replayRequest)
				require.NoError(t, err)
				assert.Equal(t, first, replayedAgain)

				assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppVersion{}, key))
				assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppInstallation{}, key))
				assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppServiceCredential{}, key))
				assert.Equal(t, int64(4), appPluginCountForKey(t, db, &AppRouteClaim{}, key))

				var frozen AppInstallationIdempotency
				require.NoError(t, db.Where("scope_key = ?", replayScope.Key).First(&frozen).Error)
				assert.Equal(t, first.InstallationID, frozen.InstallationID)
				assert.Equal(t, first.ResponseDigest, frozen.ResponseDigest)
			})
		}
	})

	t.Run("concurrent same scope frozen replay returns the original response", func(t *testing.T) {
		if db.Dialector.Name() != "mysql" {
			t.Skip("requires MySQL REPEATABLE READ snapshot semantics")
		}

		req := appPluginModelInstallRequest("concurrent-frozen-replay", "1.0.0", "https://apps.example.com/concurrent-frozen-replay/")
		original, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 112, Key: "concurrent-frozen-replay-original"}, req)
		require.NoError(t, err)

		const callbackName = "test:app_plugin_same_scope_frozen_replay_snapshot"
		snapshotsReady := make(chan struct{}, 2)
		releaseSnapshots := make(chan struct{})
		var coordinated atomic.Int32
		require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
			if tx.Statement.Table != "app_installation_idempotencies" || coordinated.Add(1) > 2 {
				return
			}
			snapshotsReady <- struct{}{}
			<-releaseSnapshots
		}))
		t.Cleanup(func() {
			require.NoError(t, db.Callback().Query().Remove(callbackName))
		})

		scope := AppIdempotencyScope{ActorID: 112, Key: "concurrent-frozen-replay"}
		start := make(chan struct{})
		results := make(chan AppInstallResult, 2)
		foundResults := make(chan bool, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				<-start
				result, found, replayErr := ReplayAppInstall(context.Background(), db, scope, req)
				results <- result
				foundResults <- found
				errs <- replayErr
			})
		}
		close(start)
		<-snapshotsReady
		<-snapshotsReady
		close(releaseSnapshots)
		wg.Wait()
		close(results)
		close(foundResults)
		close(errs)

		for replayErr := range errs {
			require.NoError(t, replayErr)
		}
		for found := range foundResults {
			assert.True(t, found)
		}
		for result := range results {
			assert.Equal(t, original, result)
		}
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppVersion{}, req.AppKey))
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppInstallation{}, req.AppKey))
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppServiceCredential{}, req.AppKey))
		assert.Equal(t, int64(4), appPluginCountForKey(t, db, &AppRouteClaim{}, req.AppKey))
		assert.Equal(t, int64(1), appPluginCountForScope(t, db, scope.Key))
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
			{name: "callback dot traversal", mutate: func(req *AppInstallRequest) { req.CallbackURL = req.BaseURL + "../outside" }},
			{name: "direct encoded traversal", mutate: func(req *AppInstallRequest) { req.DirectURL = req.BaseURL + "%2e%2e/outside" }},
			{name: "embedded different origin", mutate: func(req *AppInstallRequest) { req.EmbeddedURL = "https://other.example.com/embedded" }},
			{name: "embedded different port", mutate: func(req *AppInstallRequest) { req.EmbeddedURL = "https://apps.example.com:8443/app/embedded" }},
			{name: "endpoint prefix sibling", mutate: func(req *AppInstallRequest) {
				req.CallbackURL = strings.TrimSuffix(req.BaseURL, "/") + "-outside/callback"
			}},
			{name: "unknown enabled surface", mutate: func(req *AppInstallRequest) { req.EnabledSurfaces = []string{"admin"} }},
			{name: "non-origin parent URL", mutate: func(req *AppInstallRequest) { req.AllowedParentOrigins = []string{"https://console.example.com/path"} }},
			{name: "insecure allowed origin", mutate: func(req *AppInstallRequest) { req.AllowedOrigins = []string{"http://apps.example.com"} }},
			{name: "invalid user group reference", mutate: func(req *AppInstallRequest) { req.AllowedUserPolicy.Groups = []string{""} }},
			{name: "invalid network host reference", mutate: func(req *AppInstallRequest) { req.NetworkPolicy.AllowHosts = []string{"https://api.example.com/path"} }},
			{name: "unknown entitlement policy reference", mutate: func(req *AppInstallRequest) { req.EntitlementPolicyID = "missing-policy" }},
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

	t.Run("disabled install may omit service credential but rejects partial identity", func(t *testing.T) {
		credentialless := appPluginModelInstallRequest("credentialless", "1.0.0", "https://apps.example.com/credentialless/")
		credentialless.ServiceCredentialHash = ""
		credentialless.ServiceCredentialID = ""
		credentialless.ServiceCredentialVersion = ""
		credentialless.ServiceCredentialExpiry = 0

		result, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 19, Key: "credentialless"}, credentialless)
		require.NoError(t, err)
		assert.Equal(t, AppCredentialMeta{}, result.ServiceCredentialSet)
		assert.Equal(t, int64(0), appPluginCountForInstallation(t, db, &AppServiceCredential{}, result.InstallationID))

		partialMutations := []func(*AppInstallRequest){
			func(req *AppInstallRequest) { req.ServiceCredentialID = "" },
			func(req *AppInstallRequest) { req.ServiceCredentialVersion = "" },
			func(req *AppInstallRequest) { req.ServiceCredentialHash = "" },
			func(req *AppInstallRequest) { req.ServiceCredentialExpiry = 0 },
		}
		for i, mutate := range partialMutations {
			partial := appPluginModelInstallRequest(fmt.Sprintf("partial-credential-%d", i), "1.0.0", fmt.Sprintf("https://apps.example.com/partial-credential-%d/", i))
			mutate(&partial)
			before := appPluginPersistenceCounts(t, db)

			_, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 19, Key: fmt.Sprintf("partial-credential-%d", i)}, partial)

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrAppInstallRequestInvalid)
			assert.Equal(t, before, appPluginPersistenceCounts(t, db))
		}
	})

	t.Run("disabled install may omit entitlement policy", func(t *testing.T) {
		req := appPluginModelInstallRequest("policyless", "1.0.0", "https://apps.example.com/policyless/")
		req.EntitlementPolicyID = ""

		result, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 20, Key: "policyless"}, req)

		require.NoError(t, err)
		assert.Empty(t, result.EntitlementPolicyVersion)
	})
}

func TestAppInstallRouteCollisionIsAtomic(t *testing.T) {
	db := openAppPluginModelDB(t)
	logAppPluginDBVersion(t, db)
	t.Run("records database runtime metadata", func(t *testing.T) {
		assertAppPluginModelRuntimeMetadata(t, db)
	})
	require.NoError(t, MigrateAppPluginTables(db))
	seedAppPluginModelPolicy(t, db, "policy-basic")

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

	t.Run("new immutable version atomically upgrades the existing installation", func(t *testing.T) {
		v1 := appPluginModelInstallRequest("atomic-upgrade", "1.0.0", "https://apps.example.com/atomic-upgrade-v1/")
		created, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 23, Key: "atomic-upgrade-v1"}, v1)
		require.NoError(t, err)
		enabled, err := CompareAndSwapAppInstallationStatus(context.Background(), db, created.InstallationID, created.Revision, AppInstallationStatusEnabled)
		require.NoError(t, err)

		v2 := appPluginModelInstallRequest("atomic-upgrade", "2.0.0", "https://apps.example.com/atomic-upgrade-v2/")
		v2.EnabledSurfaces = []string{"direct"}
		v2.AllowedOrigins = []string{"https://v2.example.com"}
		upgraded, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 23, Key: "atomic-upgrade-v2"}, v2)
		require.NoError(t, err)

		assert.Equal(t, created.InstallationID, upgraded.InstallationID)
		assert.Equal(t, enabled.Revision+1, upgraded.Revision)
		assert.Equal(t, AppInstallationStatusDisabled, upgraded.Status)
		assert.Equal(t, v2.BaseURL, upgraded.BaseURL)
		assert.Equal(t, v2.EnabledSurfaces, upgraded.EnabledSurfaces)
		assert.Equal(t, v2.AllowedOrigins, upgraded.AllowedOrigins)
		assert.Equal(t, int64(2), appPluginCountForKey(t, db, &AppVersion{}, v2.AppKey))
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppInstallation{}, v2.AppKey))
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppServiceCredential{}, v2.AppKey))
		assert.Equal(t, int64(4), appPluginCountForKey(t, db, &AppRouteClaim{}, v2.AppKey))
		for _, endpoint := range []string{v1.CallbackURL, v1.DirectURL, v1.EmbeddedURL} {
			assert.Equal(t, int64(0), appPluginCountForEndpoint(t, db, endpoint))
		}
		for _, endpoint := range []string{v2.CallbackURL, v2.DirectURL, v2.EmbeddedURL} {
			assert.Equal(t, int64(1), appPluginCountForEndpoint(t, db, endpoint))
		}
	})

	t.Run("upgrade without credential input preserves existing credential", func(t *testing.T) {
		v1 := appPluginModelInstallRequest("credential-preserved", "1.0.0", "https://apps.example.com/credential-preserved-v1/")
		created, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 25, Key: "credential-preserved-v1"}, v1)
		require.NoError(t, err)
		var original AppServiceCredential
		require.NoError(t, db.Where("installation_id = ?", created.InstallationID).First(&original).Error)

		v2 := appPluginModelInstallRequest("credential-preserved", "2.0.0", "https://apps.example.com/credential-preserved-v2/")
		v2.ServiceCredentialHash = ""
		v2.ServiceCredentialID = ""
		v2.ServiceCredentialVersion = ""
		v2.ServiceCredentialExpiry = 0
		upgraded, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 25, Key: "credential-preserved-v2"}, v2)
		require.NoError(t, err)

		assert.Equal(t, credentialMeta(original), upgraded.ServiceCredentialSet)
		assert.Equal(t, int64(1), appPluginCountForInstallation(t, db, &AppServiceCredential{}, created.InstallationID))
		var preserved AppServiceCredential
		require.NoError(t, db.Where("installation_id = ?", created.InstallationID).First(&preserved).Error)
		assert.Equal(t, original, preserved)
	})

	t.Run("upgrade credential metadata cannot replace existing credential", func(t *testing.T) {
		v1 := appPluginModelInstallRequest("credential-metadata-ignored", "1.0.0", "https://apps.example.com/credential-metadata-ignored-v1/")
		created, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 26, Key: "credential-metadata-ignored-v1"}, v1)
		require.NoError(t, err)
		var original AppServiceCredential
		require.NoError(t, db.Where("installation_id = ?", created.InstallationID).First(&original).Error)

		v2 := appPluginModelInstallRequest("credential-metadata-ignored", "2.0.0", "https://apps.example.com/credential-metadata-ignored-v2/")
		v2.ServiceCredentialHash = appPluginDigest("replacement-credential")
		v2.ServiceCredentialID = "replacement-credential"
		v2.ServiceCredentialVersion = "replacement-version"
		v2.ServiceCredentialExpiry = 4102444900
		upgraded, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 26, Key: "credential-metadata-ignored-v2"}, v2)
		require.NoError(t, err)

		assert.Equal(t, credentialMeta(original), upgraded.ServiceCredentialSet)
		assert.Equal(t, int64(1), appPluginCountForInstallation(t, db, &AppServiceCredential{}, created.InstallationID))
		var preserved AppServiceCredential
		require.NoError(t, db.Where("installation_id = ?", created.InstallationID).First(&preserved).Error)
		assert.Equal(t, original, preserved)
	})

	t.Run("upgrade route collision rolls back version configuration claims and idempotency", func(t *testing.T) {
		blocker := appPluginModelInstallRequest("upgrade-blocker", "1.0.0", "https://apps.example.com/rollback-upgrade-v2/")
		_, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 24, Key: "upgrade-blocker"}, blocker)
		require.NoError(t, err)

		v1 := appPluginModelInstallRequest("rollback-upgrade", "1.0.0", "https://apps.example.com/rollback-upgrade-v1/")
		created, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 24, Key: "rollback-upgrade-v1"}, v1)
		require.NoError(t, err)
		var before AppInstallation
		require.NoError(t, db.Where("installation_id = ?", created.InstallationID).First(&before).Error)
		var credentialBefore AppServiceCredential
		require.NoError(t, db.Where("installation_id = ?", created.InstallationID).First(&credentialBefore).Error)

		v2 := appPluginModelInstallRequest("rollback-upgrade", "2.0.0", "https://apps.example.com/rollback-upgrade-v2/")
		_, err = InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 24, Key: "rollback-upgrade-v2"}, v2)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrAppRouteClaimConflict)

		var after AppInstallation
		require.NoError(t, db.Where("installation_id = ?", created.InstallationID).First(&after).Error)
		assert.Equal(t, before, after)
		var credentialAfter AppServiceCredential
		require.NoError(t, db.Where("installation_id = ?", created.InstallationID).First(&credentialAfter).Error)
		assert.Equal(t, credentialBefore, credentialAfter)
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppVersion{}, v1.AppKey))
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppInstallation{}, v1.AppKey))
		assert.Equal(t, int64(4), appPluginCountForKey(t, db, &AppRouteClaim{}, v1.AppKey))
		assert.Equal(t, int64(0), appPluginCountForScope(t, db, "rollback-upgrade-v2"))
		for _, endpoint := range []string{v1.CallbackURL, v1.DirectURL, v1.EmbeddedURL} {
			assert.Equal(t, int64(1), appPluginCountForEndpoint(t, db, endpoint))
		}
	})
}

func TestAppLifecycleCASAndRevokedTerminal(t *testing.T) {
	db := openAppPluginModelDB(t)
	logAppPluginDBVersion(t, db)
	t.Run("records database runtime metadata", func(t *testing.T) {
		assertAppPluginModelRuntimeMetadata(t, db)
	})
	require.NoError(t, MigrateAppPluginTables(db))
	seedAppPluginModelPolicy(t, db, "policy-basic")

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
	assert.Equal(t, int64(0), appPluginCountForKey(t, db, &AppRouteClaim{}, install.AppKey))

	_, err = CompareAndSwapAppInstallationStatus(context.Background(), db, created.InstallationID, revoked.Revision, AppInstallationStatusEnabled)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAppInstallationRevoked)

	reinstalled, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 31, Key: "lifecycle-reinstall"}, install)
	require.NoError(t, err)
	assert.NotEqual(t, created.InstallationID, reinstalled.InstallationID)
	assert.Equal(t, int64(4), appPluginCountForKey(t, db, &AppRouteClaim{}, install.AppKey))
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

	t.Run("concurrent same generation with different scopes replays one installation", func(t *testing.T) {
		if db.Dialector.Name() != "mysql" {
			t.Skip("requires MySQL REPEATABLE READ snapshot semantics")
		}

		const callbackName = "test:app_plugin_same_generation_snapshot"
		snapshotsReady := make(chan struct{}, 2)
		releaseSnapshots := make(chan struct{})
		var coordinated atomic.Int32
		require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
			if tx.Statement.Table != "app_installation_idempotencies" || coordinated.Add(1) > 2 {
				return
			}
			snapshotsReady <- struct{}{}
			<-releaseSnapshots
		}))
		t.Cleanup(func() {
			require.NoError(t, db.Callback().Query().Remove(callbackName))
		})

		req := appPluginModelInstallRequest("concurrent-generation", "1.0.0", "https://apps.example.com/concurrent-generation/")
		start := make(chan struct{})
		results := make(chan AppInstallResult, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, scopeKey := range []string{"concurrent-generation-a", "concurrent-generation-b"} {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				<-start
				result, installErr := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 35, Key: key}, req)
				results <- result
				errs <- installErr
			}(scopeKey)
		}
		close(start)
		<-snapshotsReady
		<-snapshotsReady
		close(releaseSnapshots)
		wg.Wait()
		close(results)
		close(errs)

		var installed []AppInstallResult
		for installErr := range errs {
			require.NoError(t, installErr)
		}
		for result := range results {
			installed = append(installed, result)
		}
		require.Len(t, installed, 2)
		assert.Equal(t, installed[0], installed[1])
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppVersion{}, req.AppKey))
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppInstallation{}, req.AppKey))
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppServiceCredential{}, req.AppKey))
		assert.Equal(t, int64(4), appPluginCountForKey(t, db, &AppRouteClaim{}, req.AppKey))
		assert.Equal(t, int64(2), appPluginCountForScope(t, db, "concurrent-generation-a")+appPluginCountForScope(t, db, "concurrent-generation-b"))
	})

	t.Run("concurrent different versions serialize onto one installation", func(t *testing.T) {
		v1 := appPluginModelInstallRequest("concurrent-upgrade", "1.0.0", "https://apps.example.com/concurrent-upgrade-v1/")
		created, err := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 34, Key: "concurrent-upgrade-v1"}, v1)
		require.NoError(t, err)

		start := make(chan struct{})
		results := make(chan AppInstallResult, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, version := range []string{"2.0.0", "3.0.0"} {
			version := version
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req := appPluginModelInstallRequest("concurrent-upgrade", version, "https://apps.example.com/concurrent-upgrade-v"+string(version[0])+"/")
				result, installErr := InstallAppVersion(context.Background(), db, AppIdempotencyScope{ActorID: 34, Key: "concurrent-upgrade-v" + string(version[0])}, req)
				results <- result
				errs <- installErr
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)

		for installErr := range errs {
			require.NoError(t, installErr)
		}
		for result := range results {
			assert.Equal(t, created.InstallationID, result.InstallationID)
		}
		assert.Equal(t, int64(3), appPluginCountForKey(t, db, &AppVersion{}, v1.AppKey))
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppInstallation{}, v1.AppKey))
		assert.Equal(t, int64(1), appPluginCountForKey(t, db, &AppServiceCredential{}, v1.AppKey))
		assert.Equal(t, int64(4), appPluginCountForKey(t, db, &AppRouteClaim{}, v1.AppKey))

		var final AppInstallation
		require.NoError(t, db.Where("installation_id = ?", created.InstallationID).First(&final).Error)
		assert.Equal(t, int64(3), final.Revision)
		assert.Contains(t, []string{"2.0.0", "3.0.0"}, final.ManifestVersion)
	})
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
		common.SetMainDatabaseType(common.DatabaseTypeSQLite)
		if dsn == "" {
			dsn = filepath.Join(t.TempDir(), "app_plugin.sqlite")
		}
		db, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	case "mysql":
		common.SetMainDatabaseType(common.DatabaseTypeMySQL)
		require.NotEmpty(t, dsn, "APP_PLUGIN_TEST_DSN is required for mysql")
		db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	case "postgres", "postgresql":
		common.SetMainDatabaseType(common.DatabaseTypePostgreSQL)
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

func appPluginCountForKey(t *testing.T, db *gorm.DB, table any, appKey string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(table).Where("app_key = ?", appKey).Count(&count).Error)
	return count
}

func appPluginCountForInstallation(t *testing.T, db *gorm.DB, table any, installationID string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(table).Where("installation_id = ?", installationID).Count(&count).Error)
	return count
}

func appPluginCountForEndpoint(t *testing.T, db *gorm.DB, endpoint string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&AppRouteClaim{}).Where("absolute_endpoint = ?", endpoint).Count(&count).Error)
	return count
}

func appPluginCountForScope(t *testing.T, db *gorm.DB, scope string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&AppInstallationIdempotency{}).Where("scope_key = ?", scope).Count(&count).Error)
	return count
}

func seedAppPluginModelPolicy(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	key := "fixture-" + id
	policy := AppEntitlementPolicy{
		ID:             id,
		KeyHash:        appPluginDigest(key),
		VersionKey:     appPluginDigest(key + "-v1"),
		CreationToken:  appPluginDigest(key + "-creation"),
		Key:            key,
		Version:        1,
		EffectiveRules: AppJSONMap{},
	}
	require.NoError(t, db.Where("id = ?", id).FirstOrCreate(&policy).Error)
}

func appPluginDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}
