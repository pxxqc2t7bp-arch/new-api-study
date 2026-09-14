package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type routerAppPluginFixture struct {
	engine            *gin.Engine
	rootToken         string
	pluginAdminToken  string
	userToken         string
	disabledUserToken string
}

type routerAppPluginEnvelope struct {
	Success bool                   `json:"success"`
	Data    model.AppInstallResult `json:"data"`
	Error   struct {
		Code string `json:"code"`
	} `json:"error"`
}

func TestAppPluginDashboardRoutesAndRedaction(t *testing.T) {
	fixture := setupRouterAppPluginTest(t)
	registered := map[string]bool{}
	for _, route := range fixture.engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		"GET /api/app_plugins",
		"GET /api/app_plugins/installations",
		"POST /api/app_plugins/installations",
		"PATCH /api/app_plugins/installations",
		"GET /api/infinite-canvas",
	} {
		assert.Truef(t, registered[route], "route %s must remain registered", route)
	}

	for _, request := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "navigation", method: http.MethodGet, path: "/api/app_plugins"},
		{name: "installation list", method: http.MethodGet, path: "/api/app_plugins/installations"},
		{name: "installation create", method: http.MethodPost, path: "/api/app_plugins/installations"},
		{name: "installation update", method: http.MethodPatch, path: "/api/app_plugins/installations"},
	} {
		t.Run(request.name+" requires authentication envelope", func(t *testing.T) {
			response := routerAppPluginRequest(fixture.engine, request.method, request.path, "", nil)
			assertRouterAppPluginErrorEnvelope(t, response, http.StatusUnauthorized, "unauthenticated", "Authentication required", false)
		})
	}

	t.Run("malformed authorization is unauthenticated", func(t *testing.T) {
		response := routerAppPluginRequestWithHeaders(
			fixture.engine,
			http.MethodGet,
			"/api/app_plugins",
			"",
			nil,
			map[string]string{"Authorization": "Bearer malformed token"},
		)
		assertRouterAppPluginErrorEnvelope(t, response, http.StatusUnauthorized, "unauthenticated", "Authentication required", false)
	})

	t.Run("invalid token is unauthenticated without disclosure", func(t *testing.T) {
		const invalidToken = "invalid-app-plugin-token"
		response := routerAppPluginRequest(fixture.engine, http.MethodGet, "/api/app_plugins", invalidToken, nil)
		assertRouterAppPluginErrorEnvelope(t, response, http.StatusUnauthorized, "unauthenticated", "Authentication required", false)
		assert.NotContains(t, response.Body.String(), invalidToken)
	})

	t.Run("disabled identity is inactive", func(t *testing.T) {
		response := routerAppPluginRequest(fixture.engine, http.MethodGet, "/api/app_plugins", fixture.disabledUserToken, nil)
		assertRouterAppPluginErrorEnvelope(t, response, http.StatusForbidden, "identity_inactive", "Identity is inactive", false)
	})

	disabled := routerAppPluginRequest(fixture.engine, http.MethodGet, "/api/app_plugins", fixture.userToken, nil)
	require.Equal(t, http.StatusForbidden, disabled.Code, disabled.Body.String())
	assert.Equal(t, "app_plugin_disabled", routerAppPluginErrorCode(t, disabled))
	disabledManagement := routerAppPluginRequest(fixture.engine, http.MethodGet, "/api/app_plugins/installations", fixture.rootToken, nil)
	require.Equal(t, http.StatusForbidden, disabledManagement.Code, disabledManagement.Body.String())
	assert.Equal(t, "app_plugin_disabled", routerAppPluginErrorCode(t, disabledManagement))

	operation_setting.AppPluginV1Enabled = true
	missingIdempotency := routerAppPluginRequest(
		fixture.engine,
		http.MethodPost,
		"/api/app_plugins/installations",
		fixture.rootToken,
		routerAppPluginInstallBody(t, "redacted-app", "1.0.0"),
	)
	require.Equal(t, http.StatusBadRequest, missingIdempotency.Code, missingIdempotency.Body.String())

	headers := map[string]string{"Idempotency-Key": "router-redaction"}
	created := routerAppPluginRequestWithHeaders(
		fixture.engine,
		http.MethodPost,
		"/api/app_plugins/installations",
		fixture.rootToken,
		routerAppPluginInstallBody(t, "redacted-app", "1.0.0"),
		headers,
	)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var createResult routerAppPluginEnvelope
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &createResult))
	require.True(t, createResult.Success)
	require.NoError(t, model.DB.Create(&model.AppServiceCredential{
		AppKey:            "redacted-app",
		InstallationID:    createResult.Data.InstallationID,
		CredentialID:      "cred-visible-metadata",
		CredentialHash:    "credential-hash-must-not-leak",
		CredentialVersion: "v1",
		Status:            "active",
		ExpiresAt:         4102444800,
	}).Error)

	for name, token := range map[string]string{
		"root":         fixture.rootToken,
		"plugin_admin": fixture.pluginAdminToken,
	} {
		t.Run(name+" can list installations", func(t *testing.T) {
			response := routerAppPluginRequest(
				fixture.engine,
				http.MethodGet,
				"/api/app_plugins/installations?status=disabled&key=redacted-app&limit=1",
				token,
				nil,
			)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"credential_id":"cred-visible-metadata"`)
			assert.NotContains(t, response.Body.String(), "credential-hash-must-not-leak")
			assert.NotContains(t, response.Body.String(), "credential_hash")
			assert.NotContains(t, response.Body.String(), "secret_ref")
			assert.NotContains(t, response.Body.String(), "canonical_manifest")
		})
	}

	forbidden := routerAppPluginRequest(
		fixture.engine,
		http.MethodGet,
		"/api/app_plugins/installations",
		fixture.userToken,
		nil,
	)
	require.Equal(t, http.StatusNotFound, forbidden.Code, forbidden.Body.String())
	assert.Equal(t, "not_found", routerAppPluginErrorCode(t, forbidden))

	canvas := routerAppPluginRequest(fixture.engine, http.MethodGet, "/api/infinite-canvas", "", nil)
	assert.Equal(t, http.StatusNoContent, canvas.Code)

	t.Run("internal authentication error is unavailable without disclosure", func(t *testing.T) {
		sqlDB, err := model.DB.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
		response := routerAppPluginRequest(fixture.engine, http.MethodGet, "/api/app_plugins", fixture.rootToken, nil)
		assertRouterAppPluginErrorEnvelope(t, response, http.StatusServiceUnavailable, "service_unavailable", "App plugin request failed", true)
		assert.NotContains(t, response.Body.String(), "database is closed")
		assert.NotContains(t, response.Body.String(), "AUTH_INTERNAL_ERROR")
	})
}

func TestAppPluginPatchRequiresRevisionAndRootForSensitiveChanges(t *testing.T) {
	fixture := setupRouterAppPluginTest(t)
	operation_setting.AppPluginV1Enabled = true
	created := routerAppPluginRequestWithHeaders(
		fixture.engine,
		http.MethodPost,
		"/api/app_plugins/installations",
		fixture.rootToken,
		routerAppPluginInstallBody(t, "managed-app", "1.0.0"),
		map[string]string{"Idempotency-Key": "router-managed-app"},
	)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var createResult routerAppPluginEnvelope
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &createResult))

	missingRevision := routerAppPluginRequest(
		fixture.engine,
		http.MethodPatch,
		"/api/app_plugins/installations",
		fixture.pluginAdminToken,
		routerAppPluginJSON(t, map[string]any{
			"installation_id": createResult.Data.InstallationID,
			"changes":         map[string]any{"base_url": "https://apps.example.com/managed-v2/"},
		}),
	)
	require.Equal(t, http.StatusBadRequest, missingRevision.Code, missingRevision.Body.String())
	assert.Equal(t, "invalid_request", routerAppPluginErrorCode(t, missingRevision))

	for name, changes := range map[string]map[string]any{
		"enabled surfaces": {"enabled_surfaces": []string{"direct"}},
		"parent origins":   {"allowed_parent_origins": []string{"https://console2.example.com"}},
		"network loosen": {
			"network_policy": map[string]any{
				"allow_hosts":            []string{"api.example.com", "new.example.com"},
				"deny_private_ip_ranges": false,
			},
		},
		"entitlement self report": {
			"entitlement_policy": map[string]any{
				"key":   "self-reported",
				"rules": map[string][]string{"app_plugin": {"manage"}},
			},
		},
		"revoke":     {"status": "revoked"},
		"secret ref": {"secret_ref": "must-never-enter-audit"},
	} {
		t.Run("plugin admin cannot change "+name, func(t *testing.T) {
			response := routerAppPluginRequest(
				fixture.engine,
				http.MethodPatch,
				"/api/app_plugins/installations",
				fixture.pluginAdminToken,
				routerAppPluginPatchBody(t, createResult.Data.InstallationID, createResult.Data.Revision, changes),
			)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Equal(t, "forbidden", routerAppPluginErrorCode(t, response))
		})
	}

	rootUpdate := routerAppPluginRequest(
		fixture.engine,
		http.MethodPatch,
		"/api/app_plugins/installations",
		fixture.rootToken,
		routerAppPluginPatchBody(t, createResult.Data.InstallationID, createResult.Data.Revision, map[string]any{
			"enabled_surfaces":       []string{"direct"},
			"allowed_parent_origins": []string{"https://console2.example.com"},
			"network_policy": map[string]any{
				"allow_hosts":            []string{"api.example.com", "cdn.example.com"},
				"deny_private_ip_ranges": false,
			},
			"entitlement_policy": map[string]any{
				"key":   "managed-app-policy",
				"rules": map[string][]string{"app_plugin": {"manage"}},
			},
		}),
	)
	require.Equal(t, http.StatusOK, rootUpdate.Code, rootUpdate.Body.String())
	var rootUpdateResult routerAppPluginEnvelope
	require.NoError(t, common.Unmarshal(rootUpdate.Body.Bytes(), &rootUpdateResult))
	assert.Equal(t, int64(2), rootUpdateResult.Data.Revision)
	assert.NotEmpty(t, rootUpdateResult.Data.EntitlementPolicyVersion)

	stale := routerAppPluginRequest(
		fixture.engine,
		http.MethodPatch,
		"/api/app_plugins/installations",
		fixture.rootToken,
		routerAppPluginPatchBody(t, createResult.Data.InstallationID, createResult.Data.Revision, map[string]any{"base_url": "https://apps.example.com/stale/"}),
	)
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	assert.Equal(t, "version_conflict", routerAppPluginErrorCode(t, stale))

	enable := routerAppPluginRequest(
		fixture.engine,
		http.MethodPatch,
		"/api/app_plugins/installations",
		fixture.pluginAdminToken,
		routerAppPluginPatchBody(t, createResult.Data.InstallationID, rootUpdateResult.Data.Revision, map[string]any{"status": "enabled"}),
	)
	require.Equal(t, http.StatusConflict, enable.Code, enable.Body.String())
	assert.Equal(t, "app_enable_prerequisite_missing", routerAppPluginErrorCode(t, enable))

	tightened := routerAppPluginRequest(
		fixture.engine,
		http.MethodPatch,
		"/api/app_plugins/installations",
		fixture.pluginAdminToken,
		routerAppPluginPatchBody(t, createResult.Data.InstallationID, rootUpdateResult.Data.Revision, map[string]any{
			"network_policy": map[string]any{
				"allow_hosts":            []string{"api.example.com"},
				"deny_private_ip_ranges": true,
			},
		}),
	)
	require.Equal(t, http.StatusOK, tightened.Code, tightened.Body.String())
	var tightenedResult routerAppPluginEnvelope
	require.NoError(t, common.Unmarshal(tightened.Body.Bytes(), &tightenedResult))
	assert.Equal(t, int64(3), tightenedResult.Data.Revision)

	enabled, err := model.CompareAndSwapAppInstallationStatus(
		t.Context(),
		model.DB,
		createResult.Data.InstallationID,
		tightenedResult.Data.Revision,
		model.AppInstallationStatusEnabled,
	)
	require.NoError(t, err)
	configWhileEnabled := routerAppPluginRequest(
		fixture.engine,
		http.MethodPatch,
		"/api/app_plugins/installations",
		fixture.pluginAdminToken,
		routerAppPluginPatchBody(t, createResult.Data.InstallationID, enabled.Revision, map[string]any{
			"allowed_origins": []string{"https://other.example.com"},
		}),
	)
	require.Equal(t, http.StatusConflict, configWhileEnabled.Code, configWhileEnabled.Body.String())
	assert.Equal(t, "invalid_state_transition", routerAppPluginErrorCode(t, configWhileEnabled))

	disabled := routerAppPluginRequest(
		fixture.engine,
		http.MethodPatch,
		"/api/app_plugins/installations",
		fixture.pluginAdminToken,
		routerAppPluginPatchBody(t, createResult.Data.InstallationID, enabled.Revision, map[string]any{"status": "disabled"}),
	)
	require.Equal(t, http.StatusOK, disabled.Code, disabled.Body.String())
	var disabledResult routerAppPluginEnvelope
	require.NoError(t, common.Unmarshal(disabled.Body.Bytes(), &disabledResult))

	revoke := routerAppPluginRequest(
		fixture.engine,
		http.MethodPatch,
		"/api/app_plugins/installations",
		fixture.rootToken,
		routerAppPluginPatchBody(t, createResult.Data.InstallationID, disabledResult.Data.Revision, map[string]any{"status": "revoked"}),
	)
	require.Equal(t, http.StatusOK, revoke.Code, revoke.Body.String())

	var audits []model.AuditLog
	require.NoError(t, model.LOG_DB.Where("category = ?", model.AuditCategoryOperation).Order("id").Find(&audits).Error)
	require.NotEmpty(t, audits)
	hasInstall, hasPatch := false, false
	for _, audit := range audits {
		switch audit.Action {
		case "app_plugin.install":
			hasInstall = true
		case "app_plugin.update":
			hasPatch = true
		}
		require.NotNil(t, audit.Other.Op)
		params := audit.Other.Op.Params
		assert.Contains(t, params, "actor")
		objectJSON, ok := params["object"].(json.RawMessage)
		require.True(t, ok)
		var object string
		require.NoError(t, common.Unmarshal(objectJSON, &object))
		assert.Equal(t, "app_installation", object)
		assert.Contains(t, params, "revision")
		assert.Contains(t, params, "result")
	}
	assert.True(t, hasInstall)
	assert.True(t, hasPatch)
	auditJSON, err := common.Marshal(audits)
	require.NoError(t, err)
	assert.NotContains(t, string(auditJSON), "must-never-enter-audit")
	assert.NotContains(t, string(auditJSON), "credential_hash")
	assert.NotContains(t, string(auditJSON), "secret_ref")
}

func setupRouterAppPluginTest(t *testing.T) routerAppPluginFixture {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis := common.RedisEnabled
	previousMaster := common.IsMasterNode
	previousFlag := operation_setting.AppPluginV1Enabled

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "app-plugin-router.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.User{},
		&model.TaskPlugin{},
		&model.AuditLog{},
		&model.CasbinRule{},
		&model.AuthzRole{},
	))
	require.NoError(t, model.MigrateAppPluginTables(db))
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.IsMasterNode = true
	operation_setting.AppPluginV1Enabled = false
	require.NoError(t, authz.Init(db))
	require.NoError(t, db.Create(&model.TaskPlugin{
		Key:        "doubao",
		APIVersion: 1,
		Version:    "1.2.0",
		Source:     "module.exports = {};",
		SourceHash: "router-test-source-hash",
		Enabled:    true,
		Active:     true,
	}).Error)

	rootToken := "app-plugin-root-token"
	pluginAdminToken := "app-plugin-admin-token"
	userToken := "app-plugin-user-token"
	disabledUserToken := "app-plugin-disabled-user-token"
	users := []model.User{
		{Username: "app-plugin-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default", AccessToken: &rootToken, AuthVersion: 1, AffCode: "app-plugin-root"},
		{Username: "app-plugin-admin", Role: common.RolePluginAdminUser, Status: common.UserStatusEnabled, Group: "default", AccessToken: &pluginAdminToken, AuthVersion: 1, AffCode: "app-plugin-admin"},
		{Username: "app-plugin-user", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AccessToken: &userToken, AuthVersion: 1, AffCode: "app-plugin-user"},
		{Username: "app-plugin-disabled-user", Role: common.RoleCommonUser, Status: common.UserStatusDisabled, Group: "default", AccessToken: &disabledUserToken, AuthVersion: 1, AffCode: "app-plugin-disabled-user"},
	}
	require.NoError(t, db.Create(&users).Error)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware.RequestId())
	api := engine.Group("/api")
	api.GET("/infinite-canvas", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	registerAppPluginRoutes(api)

	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled = previousRedis
		common.IsMasterNode = previousMaster
		operation_setting.AppPluginV1Enabled = previousFlag
	})
	return routerAppPluginFixture{
		engine:            engine,
		rootToken:         rootToken,
		pluginAdminToken:  pluginAdminToken,
		userToken:         userToken,
		disabledUserToken: disabledUserToken,
	}
}

func routerAppPluginRequest(engine http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	return routerAppPluginRequestWithHeaders(engine, method, path, token, body, nil)
}

func routerAppPluginRequestWithHeaders(
	engine http.Handler,
	method string,
	path string,
	token string,
	body []byte,
	headers map[string]string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func routerAppPluginInstallBody(t *testing.T, key, version string) []byte {
	t.Helper()
	return routerAppPluginJSON(t, map[string]any{
		"manifest": map[string]any{
			"apiVersion":   1,
			"kind":         "app",
			"key":          key,
			"name":         map[string]string{"en": key, "zh": key},
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
		},
		"base_url":               "https://apps.example.com/" + key + "/",
		"enabled_surfaces":       []string{"direct", "embedded"},
		"allowed_parent_origins": []string{"https://console.example.com"},
		"allowed_origins":        []string{"https://client.example.com"},
		"allowed_user_policy":    map[string]any{"groups": []string{"default"}},
		"network_policy": map[string]any{
			"allow_hosts":            []string{"api.example.com"},
			"deny_private_ip_ranges": true,
		},
	})
}

func routerAppPluginPatchBody(t *testing.T, installationID string, revision int64, changes map[string]any) []byte {
	t.Helper()
	return routerAppPluginJSON(t, map[string]any{
		"installation_id": installationID,
		"revision":        revision,
		"changes":         changes,
	})
}

func routerAppPluginJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := common.Marshal(value)
	require.NoError(t, err)
	return body
}

func routerAppPluginErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope routerAppPluginEnvelope
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &envelope), response.Body.String())
	return envelope.Error.Code
}

func assertRouterAppPluginErrorEnvelope(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	code string,
	message string,
	retryable bool,
) {
	t.Helper()
	require.Equal(t, status, response.Code, response.Body.String())
	requestID := response.Header().Get(common.RequestIdKey)
	require.NotEmpty(t, requestID)
	var actual map[string]any
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &actual), response.Body.String())
	assert.Equal(t, map[string]any{
		"error": map[string]any{
			"code":         code,
			"message":      message,
			"field_errors": []any{},
			"retryable":    retryable,
			"request_id":   requestID,
		},
	}, actual)
}
