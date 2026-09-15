package controller

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type appPluginTestEnvelope struct {
	Success bool                   `json:"success"`
	Data    model.AppInstallResult `json:"data"`
	Error   struct {
		Code string `json:"code"`
	} `json:"error"`
}

type appPluginNavigationEnvelope struct {
	Success bool `json:"success"`
	Data    []struct {
		Key             string            `json:"key"`
		Name            map[string]string `json:"name"`
		Version         string            `json:"version"`
		EnabledSurfaces []string          `json:"enabled_surfaces"`
		DashboardPath   string            `json:"dashboard_path"`
		DirectURL       string            `json:"direct_url"`
		GrantedScopes   []string          `json:"granted_scopes"`
	} `json:"data"`
}

func TestAppPluginDashboardCreatesAndUpdatesDisabledInstallations(t *testing.T) {
	setupAppPluginControllerTest(t)
	manifest := appPluginTestManifest("seedance-repro", "Seedance Repro", "1.0.0")
	body := appPluginInstallBody(t, manifest, "https://apps.example.com/seedance/")

	created := appPluginControllerRequest(
		t,
		CreateAppPluginInstallation,
		http.MethodPost,
		"/api/app_plugins/installations",
		body,
		common.RoleRootUser,
		"default",
		map[string]string{"Idempotency-Key": "install-seedance-v1"},
	)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var createResult appPluginTestEnvelope
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &createResult))
	require.True(t, createResult.Success)
	assert.Equal(t, model.AppInstallationStatusDisabled, createResult.Data.Status)
	assert.Equal(t, int64(1), createResult.Data.Revision)
	assert.Empty(t, createResult.Data.ServiceCredentialSet.CredentialID)
	assert.NotContains(t, created.Body.String(), "credential_hash")
	assert.NotContains(t, created.Body.String(), "secret_ref")

	replayed := appPluginControllerRequest(
		t,
		CreateAppPluginInstallation,
		http.MethodPost,
		"/api/app_plugins/installations",
		body,
		common.RoleRootUser,
		"default",
		map[string]string{"Idempotency-Key": "install-seedance-v1"},
	)
	require.Equal(t, http.StatusCreated, replayed.Code, replayed.Body.String())
	var replayResult appPluginTestEnvelope
	require.NoError(t, common.Unmarshal(replayed.Body.Bytes(), &replayResult))
	assert.Equal(t, createResult.Data.InstallationID, replayResult.Data.InstallationID)
	assert.Equal(t, createResult.Data.Revision, replayResult.Data.Revision)

	conflictingBody := appPluginInstallBody(t, manifest, "https://apps.example.com/changed/")
	conflict := appPluginControllerRequest(
		t,
		CreateAppPluginInstallation,
		http.MethodPost,
		"/api/app_plugins/installations",
		conflictingBody,
		common.RoleRootUser,
		"default",
		map[string]string{"Idempotency-Key": "install-seedance-v1"},
	)
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	assert.Equal(t, "idempotency_conflict", appPluginErrorCode(t, conflict))

	patchBody := appPluginPatchBody(t, createResult.Data.InstallationID, createResult.Data.Revision, map[string]any{
		"base_url":            "https://apps.example.com/seedance-v2/",
		"allowed_origins":     []string{"https://client.example.com"},
		"allowed_user_policy": map[string]any{"groups": []string{"default", "paid"}},
	})
	updated := appPluginControllerRequest(
		t,
		PatchAppPluginInstallation,
		http.MethodPatch,
		"/api/app_plugins/installations",
		patchBody,
		common.RolePluginAdminUser,
		"default",
		nil,
	)
	require.Equal(t, http.StatusOK, updated.Code, updated.Body.String())
	var updateResult appPluginTestEnvelope
	require.NoError(t, common.Unmarshal(updated.Body.Bytes(), &updateResult))
	require.True(t, updateResult.Success)
	assert.Equal(t, model.AppInstallationStatusDisabled, updateResult.Data.Status)
	assert.Equal(t, int64(2), updateResult.Data.Revision)
	assert.Equal(t, "https://apps.example.com/seedance-v2/", updateResult.Data.BaseURL)

	enable := appPluginControllerRequest(
		t,
		PatchAppPluginInstallation,
		http.MethodPatch,
		"/api/app_plugins/installations",
		appPluginPatchBody(t, createResult.Data.InstallationID, updateResult.Data.Revision, map[string]any{"status": "enabled"}),
		common.RolePluginAdminUser,
		"default",
		nil,
	)
	require.Equal(t, http.StatusConflict, enable.Code, enable.Body.String())
	assert.Equal(t, "app_enable_prerequisite_missing", appPluginErrorCode(t, enable))

	var stored model.AppInstallation
	require.NoError(t, model.DB.Where("installation_id = ?", createResult.Data.InstallationID).First(&stored).Error)
	assert.Equal(t, model.AppInstallationStatusDisabled, stored.Status)
	assert.Equal(t, int64(2), stored.Revision)

	t.Run("root entitlement draft is idempotent", func(t *testing.T) {
		policyBody := appPluginJSONBody(t, map[string]any{
			"manifest":               appPluginTestManifest("policy-app", "Policy App", "1.0.0"),
			"base_url":               "https://apps.example.com/policy/",
			"enabled_surfaces":       []string{"direct"},
			"allowed_parent_origins": []string{"https://console.example.com"},
			"allowed_origins":        []string{"https://client.example.com"},
			"allowed_user_policy":    map[string]any{"groups": []string{"default"}},
			"network_policy":         map[string]any{"allow_hosts": []string{"api.example.com"}, "deny_private_ip_ranges": true},
			"entitlement_policy": map[string]any{
				"key":   "policy-app-access",
				"rules": map[string][]string{"app_plugin": {"manage"}},
			},
		})
		first := appPluginControllerRequest(
			t,
			CreateAppPluginInstallation,
			http.MethodPost,
			"/api/app_plugins/installations",
			policyBody,
			common.RoleRootUser,
			"default",
			map[string]string{"Idempotency-Key": "install-policy-app"},
		)
		require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
		var firstResult appPluginTestEnvelope
		require.NoError(t, common.Unmarshal(first.Body.Bytes(), &firstResult))
		require.NotEmpty(t, firstResult.Data.EntitlementPolicyVersion)

		second := appPluginControllerRequest(
			t,
			CreateAppPluginInstallation,
			http.MethodPost,
			"/api/app_plugins/installations",
			policyBody,
			common.RoleRootUser,
			"default",
			map[string]string{"Idempotency-Key": "install-policy-app"},
		)
		require.Equal(t, http.StatusCreated, second.Code, second.Body.String())
		var secondResult appPluginTestEnvelope
		require.NoError(t, common.Unmarshal(second.Body.Bytes(), &secondResult))
		assert.Equal(t, firstResult.Data.InstallationID, secondResult.Data.InstallationID)
		assert.Equal(t, firstResult.Data.EntitlementPolicyVersion, secondResult.Data.EntitlementPolicyVersion)

		var policyCount int64
		require.NoError(t, model.DB.Model(&model.AppEntitlementPolicy{}).
			Where(&model.AppEntitlementPolicy{Key: "policy-app-access"}).
			Count(&policyCount).Error)
		assert.EqualValues(t, 1, policyCount)
	})

	t.Run("validation and storage failures use distinct safe errors", func(t *testing.T) {
		invalidURLBody := appPluginInstallBody(
			t,
			appPluginTestManifest("invalid-base-app", "Invalid Base App", "1.0.0"),
			"http://apps.example.com/invalid/",
		)
		invalidURL := appPluginControllerRequest(
			t,
			CreateAppPluginInstallation,
			http.MethodPost,
			"/api/app_plugins/installations",
			invalidURLBody,
			common.RoleRootUser,
			"default",
			map[string]string{"Idempotency-Key": "invalid-base-app"},
		)
		require.Equal(t, http.StatusUnprocessableEntity, invalidURL.Code, invalidURL.Body.String())
		assert.Equal(t, "validation_error", appPluginErrorCode(t, invalidURL))

		const callbackName = "test:app_plugin_dashboard_storage_failure"
		privateError := "storage failed with private detail"
		require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
			if _, ok := tx.Statement.Dest.(*model.AppInstallation); ok {
				tx.AddError(errors.New(privateError))
			}
		}))
		storageFailure := appPluginControllerRequest(
			t,
			CreateAppPluginInstallation,
			http.MethodPost,
			"/api/app_plugins/installations",
			appPluginInstallBody(t, appPluginTestManifest("storage-failure-app", "Storage Failure App", "1.0.0"), "https://apps.example.com/storage-failure/"),
			common.RoleRootUser,
			"default",
			map[string]string{"Idempotency-Key": "storage-failure-app"},
		)
		require.NoError(t, model.DB.Callback().Create().Remove(callbackName))
		require.Equal(t, http.StatusServiceUnavailable, storageFailure.Code, storageFailure.Body.String())
		assert.Equal(t, "service_unavailable", appPluginErrorCode(t, storageFailure))
		assert.NotContains(t, storageFailure.Body.String(), privateError)
	})

	t.Run("root may clear disabled surfaces", func(t *testing.T) {
		surfaceApp := createAppPluginForControllerTest(t, "surface-app", "Surface App", "surface-app")
		cleared := appPluginControllerRequest(
			t,
			PatchAppPluginInstallation,
			http.MethodPatch,
			"/api/app_plugins/installations",
			appPluginPatchBody(t, surfaceApp.InstallationID, surfaceApp.Revision, map[string]any{
				"enabled_surfaces": []string{},
			}),
			common.RoleRootUser,
			"default",
			nil,
		)
		require.Equal(t, http.StatusOK, cleared.Code, cleared.Body.String())
		var clearedResult appPluginTestEnvelope
		require.NoError(t, common.Unmarshal(cleared.Body.Bytes(), &clearedResult))
		assert.Empty(t, clearedResult.Data.EnabledSurfaces)
		assert.Equal(t, model.AppInstallationStatusDisabled, clearedResult.Data.Status)
		assert.Contains(t, cleared.Body.String(), `"enabled_surfaces":[]`)
	})

	t.Run("nonroot upgrades cannot clear root approved policies", func(t *testing.T) {
		const key = "approved-policy-app"
		var approvedBody map[string]any
		require.NoError(t, common.Unmarshal(appPluginInstallBody(t,
			appPluginTestManifest(key, key, "1.0.0"), "https://apps.example.com/"+key+"/"), &approvedBody))
		approvedBody["entitlement_policy"] = map[string]any{
			"key": key, "rules": map[string][]string{"app_plugin": {"manage"}},
		}
		created := appPluginControllerRequest(t, CreateAppPluginInstallation, http.MethodPost,
			"/api/app_plugins/installations", appPluginJSONBody(t, approvedBody),
			common.RoleRootUser, "default", map[string]string{"Idempotency-Key": key})
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
		var frozen appPluginTestEnvelope
		require.NoError(t, common.Unmarshal(created.Body.Bytes(), &frozen))

		var before model.AppInstallation
		require.NoError(t, model.DB.Where("installation_id = ?", frozen.Data.InstallationID).First(&before).Error)
		counts := appPluginControllerPersistenceCounts(t)
		upgrade := appPluginJSONBody(t, map[string]any{
			"manifest": appPluginTestManifest(key, key, "2.0.0"),
			"base_url": "https://apps.example.com/upgrade/",
		})
		rejected := appPluginControllerRequest(t, CreateAppPluginInstallation, http.MethodPost,
			"/api/app_plugins/installations", upgrade, common.RolePluginAdminUser, "default",
			map[string]string{"Idempotency-Key": key + "-v2"})
		assert.Equal(t, http.StatusForbidden, rejected.Code, rejected.Body.String())
		assert.Equal(t, "forbidden", appPluginErrorCode(t, rejected))
		var after model.AppInstallation
		require.NoError(t, model.DB.Where("installation_id = ?", before.InstallationID).First(&after).Error)
		assert.Equal(t, before, after, "rejected upgrade must not alter approved policy or revision")
		assert.Equal(t, counts, appPluginControllerPersistenceCounts(t), "rejected upgrade must roll back all rows")
	})

	t.Run("nonroot creation and immutable replay preserve request hash semantics", func(t *testing.T) {
		const key = "nonroot-replay-app"
		request := map[string]any{
			"manifest": appPluginTestManifest(key, key, "1.0.0"),
			"base_url": "https://apps.example.com/" + key + "/",
		}
		body := appPluginJSONBody(t, request)
		created := appPluginControllerRequest(t, CreateAppPluginInstallation, http.MethodPost,
			"/api/app_plugins/installations", body, common.RolePluginAdminUser, "default",
			map[string]string{"Idempotency-Key": key})
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
		var frozen appPluginTestEnvelope
		require.NoError(t, common.Unmarshal(created.Body.Bytes(), &frozen))
		assert.Equal(t, model.AppInstallationStatusDisabled, frozen.Data.Status)

		rootUpgrade := appPluginControllerRequest(t, CreateAppPluginInstallation, http.MethodPost,
			"/api/app_plugins/installations", appPluginInstallBody(t,
				appPluginTestManifest(key, key, "2.0.0"), "https://apps.example.com/"+key+"/"),
			common.RoleRootUser, "default", map[string]string{"Idempotency-Key": key + "-root-v2"})
		require.Equal(t, http.StatusCreated, rootUpgrade.Code, rootUpgrade.Body.String())
		var rootFrozen appPluginTestEnvelope
		require.NoError(t, common.Unmarshal(rootUpgrade.Body.Bytes(), &rootFrozen))
		require.Equal(t, int64(2), rootFrozen.Data.Revision)
		var before model.AppInstallation
		require.NoError(t, model.DB.Where("installation_id = ?", frozen.Data.InstallationID).First(&before).Error)

		for _, tc := range []struct {
			name, scope, version string
			want                 appPluginTestEnvelope
		}{
			{"same scope old generation", key, "1.0.0", frozen},
			{"different scope old generation", key + "-old-replay", "1.0.0", frozen},
			{"different scope root generation", key + "-root-replay", "2.0.0", rootFrozen},
		} {
			t.Run(tc.name, func(t *testing.T) {
				request["manifest"] = appPluginTestManifest(key, key, tc.version)
				replayed := appPluginControllerRequest(t, CreateAppPluginInstallation, http.MethodPost,
					"/api/app_plugins/installations", appPluginJSONBody(t, request),
					common.RolePluginAdminUser, "default", map[string]string{"Idempotency-Key": tc.scope})
				require.Equal(t, http.StatusCreated, replayed.Code, replayed.Body.String())
				var actual appPluginTestEnvelope
				require.NoError(t, common.Unmarshal(replayed.Body.Bytes(), &actual))
				assert.Equal(t, tc.want, actual)
			})
		}
		for _, tc := range []struct {
			name, scope, version, baseURL, code string
		}{
			{"same scope changed payload", key, "1.0.0", "https://apps.example.com/changed/", "idempotency_conflict"},
			{"same scope changed generation", key, "3.0.0", request["base_url"].(string), "idempotency_conflict"},
			{"different scope changed digest", key + "-digest", "1.0.0", request["base_url"].(string), "app_version_conflict"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				manifest := appPluginTestManifest(key, key, tc.version)
				if tc.code == "app_version_conflict" {
					manifest["name"] = map[string]string{"en": "Changed", "zh": "Changed"}
				}
				conflict := appPluginControllerRequest(t, CreateAppPluginInstallation, http.MethodPost,
					"/api/app_plugins/installations", appPluginJSONBody(t, map[string]any{
						"manifest": manifest, "base_url": tc.baseURL,
					}), common.RolePluginAdminUser, "default", map[string]string{"Idempotency-Key": tc.scope})
				assert.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
				assert.Equal(t, tc.code, appPluginErrorCode(t, conflict))
			})
		}
		var after model.AppInstallation
		require.NoError(t, model.DB.Where("installation_id = ?", before.InstallationID).First(&after).Error)
		assert.Equal(t, before, after, "replays and conflicts must not mutate current approved state")
	})
}

func TestDisabledAndRevokedAppsDisappearFromNavigation(t *testing.T) {
	setupAppPluginControllerTest(t)
	canvas := createAppPluginForControllerTest(t, "infinite-canvas", "Infinite Canvas", "canvas")
	seedance := createAppPluginForControllerTest(t, "seedance-repro", "Seedance Repro", "seedance")

	canvasInstallation, err := model.CompareAndSwapAppInstallationStatus(
		t.Context(),
		model.DB,
		canvas.InstallationID,
		canvas.Revision,
		model.AppInstallationStatusEnabled,
	)
	require.NoError(t, err)
	seedanceInstallation, err := model.CompareAndSwapAppInstallationStatus(
		t.Context(),
		model.DB,
		seedance.InstallationID,
		seedance.Revision,
		model.AppInstallationStatusEnabled,
	)
	require.NoError(t, err)

	visible := appPluginControllerRequest(
		t,
		ListAppPlugins,
		http.MethodGet,
		"/api/app_plugins",
		nil,
		common.RoleCommonUser,
		"default",
		nil,
	)
	require.Equal(t, http.StatusOK, visible.Code, visible.Body.String())
	var visibleResult appPluginNavigationEnvelope
	require.NoError(t, common.Unmarshal(visible.Body.Bytes(), &visibleResult))
	require.True(t, visibleResult.Success)
	require.Len(t, visibleResult.Data, 2)
	assert.Equal(t, []string{"infinite-canvas", "seedance-repro"}, []string{visibleResult.Data[0].Key, visibleResult.Data[1].Key})
	assert.Equal(t, "/apps/infinite-canvas", visibleResult.Data[0].DashboardPath)
	assert.Equal(t, "https://apps.example.com/infinite-canvas/auth/start", visibleResult.Data[0].DirectURL)
	assert.ElementsMatch(t, []string{"identity.read", "task.read"}, visibleResult.Data[0].GrantedScopes)

	blocked := appPluginControllerRequest(
		t,
		ListAppPlugins,
		http.MethodGet,
		"/api/app_plugins",
		nil,
		common.RoleCommonUser,
		"blocked",
		nil,
	)
	require.Equal(t, http.StatusOK, blocked.Code, blocked.Body.String())
	var blockedResult appPluginNavigationEnvelope
	require.NoError(t, common.Unmarshal(blocked.Body.Bytes(), &blockedResult))
	assert.Empty(t, blockedResult.Data)

	disabled := appPluginControllerRequest(
		t,
		PatchAppPluginInstallation,
		http.MethodPatch,
		"/api/app_plugins/installations",
		appPluginPatchBody(t, seedance.InstallationID, seedanceInstallation.Revision, map[string]any{"status": "disabled"}),
		common.RolePluginAdminUser,
		"default",
		nil,
	)
	require.Equal(t, http.StatusOK, disabled.Code, disabled.Body.String())

	visible = appPluginControllerRequest(
		t,
		ListAppPlugins,
		http.MethodGet,
		"/api/app_plugins",
		nil,
		common.RoleCommonUser,
		"default",
		nil,
	)
	require.NoError(t, common.Unmarshal(visible.Body.Bytes(), &visibleResult))
	require.Len(t, visibleResult.Data, 1)
	assert.Equal(t, "infinite-canvas", visibleResult.Data[0].Key)

	var disabledResult appPluginTestEnvelope
	require.NoError(t, common.Unmarshal(disabled.Body.Bytes(), &disabledResult))
	seedanceInstallation, err = model.CompareAndSwapAppInstallationStatus(
		t.Context(),
		model.DB,
		seedance.InstallationID,
		disabledResult.Data.Revision,
		model.AppInstallationStatusEnabled,
	)
	require.NoError(t, err)
	revoked := appPluginControllerRequest(
		t,
		PatchAppPluginInstallation,
		http.MethodPatch,
		"/api/app_plugins/installations",
		appPluginPatchBody(t, seedance.InstallationID, seedanceInstallation.Revision, map[string]any{"status": "revoked"}),
		common.RoleRootUser,
		"default",
		nil,
	)
	require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())

	visible = appPluginControllerRequest(
		t,
		ListAppPlugins,
		http.MethodGet,
		"/api/app_plugins",
		nil,
		common.RoleCommonUser,
		"default",
		nil,
	)
	require.NoError(t, common.Unmarshal(visible.Body.Bytes(), &visibleResult))
	require.Len(t, visibleResult.Data, 1)
	assert.Equal(t, "infinite-canvas", visibleResult.Data[0].Key)
	assert.Equal(t, model.AppInstallationStatusEnabled, canvasInstallation.Status)
}

func setupAppPluginControllerTest(t *testing.T) {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis := common.RedisEnabled
	previousFlag := operation_setting.AppPluginV1Enabled

	db := openAppPluginControllerDB(t)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled = previousRedis
		operation_setting.AppPluginV1Enabled = previousFlag
	})
	dbType := map[string]common.DatabaseType{
		"sqlite": common.DatabaseTypeSQLite, "mysql": common.DatabaseTypeMySQL, "postgres": common.DatabaseTypePostgreSQL,
	}[db.Dialector.Name()]
	common.SetDatabaseTypes(dbType, dbType)
	require.NoError(t, db.AutoMigrate(&model.TaskPlugin{}, &model.AuditLog{}))
	require.NoError(t, model.MigrateAppPluginTables(db))
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled = false
	operation_setting.AppPluginV1Enabled = true
	require.NoError(t, db.Create(&model.TaskPlugin{
		Key:        "doubao",
		APIVersion: 1,
		Version:    "1.2.0",
		Source:     "module.exports = {};",
		SourceHash: "test-source-hash",
		Enabled:    true,
		Active:     true,
	}).Error)

	gin.SetMode(gin.TestMode)
}

func openAppPluginControllerDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialect, dsn := os.Getenv("APP_PLUGIN_TEST_DIALECT"), os.Getenv("APP_PLUGIN_TEST_DSN")
	if dialect == "" {
		dialect = "sqlite"
	}
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	name := fmt.Sprintf("app_plugin_controller_%d_%d", os.Getpid(), time.Now().UnixNano())
	var db *gorm.DB
	var err error
	switch dialect {
	case "sqlite":
		if dsn == "" {
			dsn = filepath.Join(t.TempDir(), "app-plugin.db")
		} else {
			// The runner supplies a temporary filename; each fixture owns a sibling.
			dsn += "." + name
			sqliteFile := dsn
			t.Cleanup(func() { assert.NoError(t, os.Remove(sqliteFile)) })
		}
		db, err = gorm.Open(sqlite.Open(dsn), config)
	case "mysql":
		require.NotEmpty(t, dsn, "APP_PLUGIN_TEST_DSN is required")
		parsed, parseErr := mysqlDriver.ParseDSN(dsn)
		require.True(t, parseErr == nil, "invalid mysql test DSN")
		admin, openErr := gorm.Open(mysql.Open(dsn), config)
		require.True(t, openErr == nil, "cannot open mysql test database")
		require.NoError(t, admin.Exec("CREATE DATABASE `"+name+"`").Error)
		t.Cleanup(func() {
			assert.NoError(t, admin.Exec("DROP DATABASE `"+name+"`").Error)
			sqlDB, err := admin.DB()
			require.NoError(t, err)
			assert.NoError(t, sqlDB.Close())
		})
		parsed.DBName = name
		db, err = gorm.Open(mysql.Open(parsed.FormatDSN()), config)
	case "postgres", "postgresql":
		require.NotEmpty(t, dsn, "APP_PLUGIN_TEST_DSN is required")
		parsed, parseErr := pgx.ParseConfig(dsn)
		require.True(t, parseErr == nil, "invalid postgres test DSN")
		admin, openErr := gorm.Open(postgres.Open(dsn), config)
		require.True(t, openErr == nil, "cannot open postgres test database")
		require.NoError(t, admin.Exec(`CREATE SCHEMA "`+name+`"`).Error)
		t.Cleanup(func() {
			assert.NoError(t, admin.Exec(`DROP SCHEMA "`+name+`" CASCADE`).Error)
			sqlDB, err := admin.DB()
			require.NoError(t, err)
			assert.NoError(t, sqlDB.Close())
		})
		parsed.RuntimeParams["search_path"] = name
		connection := stdlib.OpenDB(*parsed)
		db, err = gorm.Open(postgres.New(postgres.Config{Conn: connection}), config)
	default:
		t.Fatalf("unsupported APP_PLUGIN_TEST_DIALECT %q", dialect)
	}
	require.True(t, err == nil, "cannot open app plugin test database")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sqlDB.Close()) })
	if os.Getenv("APP_PLUGIN_TEST_DIALECT") != "" {
		require.Equal(t, strings.ReplaceAll(dialect, "postgresql", "postgres"), db.Dialector.Name())
		var version string
		query := "SELECT version()"
		if dialect == "sqlite" {
			query = "SELECT sqlite_version()"
		}
		require.NoError(t, db.Raw(query).Scan(&version).Error)
		require.Equal(t, os.Getenv("APP_PLUGIN_TEST_DATABASE_VERSION"), version)
		require.NotEmpty(t, os.Getenv("APP_PLUGIN_TEST_DRIVER"))
		t.Logf("verified database=%s version=%s driver=%s", db.Dialector.Name(), version, os.Getenv("APP_PLUGIN_TEST_DRIVER"))
	}
	return db
}

func appPluginControllerPersistenceCounts(t *testing.T) [6]int64 {
	t.Helper()
	var counts [6]int64
	for i, table := range []any{
		&model.AppInstallation{}, &model.AppVersion{}, &model.AppInstallationIdempotency{},
		&model.AppRouteClaim{}, &model.AppEntitlementPolicy{}, &model.AppServiceCredential{},
	} {
		require.NoError(t, model.DB.Model(table).Count(&counts[i]).Error)
	}
	return counts
}

func createAppPluginForControllerTest(t *testing.T, key, name, idempotencyKey string) model.AppInstallResult {
	t.Helper()
	response := appPluginControllerRequest(
		t,
		CreateAppPluginInstallation,
		http.MethodPost,
		"/api/app_plugins/installations",
		appPluginInstallBody(t, appPluginTestManifest(key, name, "1.0.0"), "https://apps.example.com/"+key+"/"),
		common.RoleRootUser,
		"default",
		map[string]string{"Idempotency-Key": idempotencyKey},
	)
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	var result appPluginTestEnvelope
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success)
	return result.Data
}

func appPluginControllerRequest(
	t *testing.T,
	handler gin.HandlerFunc,
	method string,
	path string,
	body []byte,
	role int,
	group string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		context.Request.Header.Set(key, value)
	}
	context.Set("id", 1001)
	context.Set("role", role)
	context.Set("username", "app-plugin-test")
	context.Set("group", group)
	context.Set("user_group", group)
	context.Set(common.RequestIdKey, "app-plugin-controller-test")
	handler(context)
	return recorder
}

func appPluginTestManifest(key, name, version string) map[string]any {
	return map[string]any{
		"apiVersion":   1,
		"kind":         "app",
		"key":          key,
		"name":         map[string]string{"en": name, "zh": name},
		"version":      version,
		"callbackPath": "/auth/callback",
		"surfaces": map[string]any{
			"direct":   map[string]string{"startPath": "/auth/start"},
			"embedded": map[string]string{"startPath": "/auth/embed/start"},
		},
		"requestedScopes": []string{"identity.read", "task.read"},
		"requires": map[string]any{
			"taskPlugins": []map[string]string{{"key": "doubao", "minimumVersion": "1.2.0"}},
		},
	}
}

func appPluginInstallBody(t *testing.T, manifest map[string]any, baseURL string) []byte {
	t.Helper()
	body, err := common.Marshal(map[string]any{
		"manifest":               manifest,
		"base_url":               baseURL,
		"enabled_surfaces":       []string{"direct", "embedded"},
		"allowed_parent_origins": []string{"https://console.example.com"},
		"allowed_origins":        []string{"https://client.example.com"},
		"allowed_user_policy":    map[string]any{"groups": []string{"default"}},
		"network_policy":         map[string]any{"allow_hosts": []string{"api.example.com"}, "deny_private_ip_ranges": true},
	})
	require.NoError(t, err)
	return body
}

func appPluginPatchBody(t *testing.T, installationID string, revision int64, changes map[string]any) []byte {
	t.Helper()
	return appPluginJSONBody(t, map[string]any{
		"installation_id": installationID,
		"revision":        revision,
		"changes":         changes,
	})
}

func appPluginJSONBody(t *testing.T, value any) []byte {
	t.Helper()
	body, err := common.Marshal(value)
	require.NoError(t, err)
	return body
}

func appPluginErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope appPluginTestEnvelope
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &envelope), fmt.Sprintf("body=%s", response.Body.String()))
	return envelope.Error.Code
}
