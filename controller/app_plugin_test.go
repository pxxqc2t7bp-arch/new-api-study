package controller

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			Where("key = ?", "policy-app-access").
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

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "app-plugin.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.TaskPlugin{}, &model.AuditLog{}))
	require.NoError(t, model.MigrateAppPluginTables(db))
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
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

	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled = previousRedis
		operation_setting.AppPluginV1Enabled = previousFlag
	})
	gin.SetMode(gin.TestMode)
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
