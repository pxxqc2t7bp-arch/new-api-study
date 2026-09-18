package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
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

	t.Run("rate limits use canonical envelopes", func(t *testing.T) {
		previousGlobal, previousGlobalNum, previousGlobalDuration := common.GlobalApiRateLimitEnable, common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration
		previousCritical, previousCriticalNum, previousCriticalDuration := common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration
		previousFlag := operation_setting.AppPluginV1Enabled
		t.Cleanup(func() {
			common.GlobalApiRateLimitEnable, common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration = previousGlobal, previousGlobalNum, previousGlobalDuration
			common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration = previousCritical, previousCriticalNum, previousCriticalDuration
			operation_setting.AppPluginV1Enabled = previousFlag
		})
		require.False(t, common.RedisEnabled, "rate-limit regression must use the in-memory limiter")
		operation_setting.AppPluginV1Enabled = false
		common.GlobalApiRateLimitEnable = false
		common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration = true, 1, 60

		for i, test := range []struct {
			name, path    string
			authenticated bool
			userLimit     bool
		}{
			{name: "IP authorize before authentication", path: "/api/app_plugins/test/authorize"},
			{name: "IP credential create after authentication", path: "/api/app_plugins/installations/test/service-credentials", authenticated: true},
			{name: "IP credential rotate after authentication", path: "/api/app_plugins/installations/test/service-credentials/rotate", authenticated: true},
			{name: "IP exchange", path: "/internal/apps/v1/launch-codes/exchange"},
			{name: "IP introspect", path: "/internal/apps/v1/sessions/introspect"},
			{name: "IP revoke", path: "/internal/apps/v1/sessions/revoke"},
			{name: "user authorize across IPs", path: "/api/app_plugins/test/authorize", authenticated: true, userLimit: true},
			{name: "user credential create across IPs", path: "/api/app_plugins/installations/test/service-credentials", authenticated: true, userLimit: true},
			{name: "user credential rotate across IPs", path: "/api/app_plugins/installations/test/service-credentials/rotate", authenticated: true, userLimit: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				token := ""
				if test.authenticated {
					token = fmt.Sprintf("b15-rate-limit-token-%d", i)
					name := fmt.Sprintf("b15-rate-limit-user-%d", i)
					require.NoError(t, model.DB.Create(&model.User{
						Id: 15000 + i, Username: name, Role: common.RoleRootUser,
						Status: common.UserStatusEnabled, Group: "default", AccessToken: &token,
						AuthVersion: 1, AffCode: name,
					}).Error)
				}
				// Isolate route-local limiters; the real SetApiRouter global chain is tested below.
				engine := gin.New()
				require.NoError(t, engine.SetTrustedProxies(nil))
				if !test.userLimit {
					engine.Use(middleware.RequestId())
				}
				registerAppPluginRoutes(engine.Group("/api"))
				firstIP := fmt.Sprintf("198.51.100.%d", 2*i+1)
				secondIP := firstIP
				if test.userLimit {
					secondIP = fmt.Sprintf("198.51.100.%d", 2*i+2)
				}
				first := routerAppPluginRateLimitRequest(engine, http.MethodPost, test.path, token, firstIP)
				if i == 0 {
					assertRouterAppPluginErrorEnvelope(t, first, http.StatusUnauthorized, "unauthenticated", "Authentication required", false)
				} else if strings.Contains(test.path, "/service-credentials") {
					assertRouterAppPluginErrorEnvelope(t, first, http.StatusForbidden, "forbidden", "App plugin request is forbidden", false)
				} else if test.path == "/internal/apps/v1/sessions/introspect" || test.path == "/internal/apps/v1/sessions/revoke" {
					assertRouterAppPluginErrorEnvelope(t, first, http.StatusUnauthorized, "service_identity_invalid", "App service request denied", false)
				} else {
					assertRouterAppPluginErrorEnvelope(t, first, http.StatusForbidden, "app_plugin_disabled", map[bool]string{
						true: "App plugin API is disabled", false: "App service request denied",
					}[test.authenticated], false)
				}
				limited := routerAppPluginRateLimitRequest(engine, http.MethodPost, test.path, token, secondIP)
				t.Logf("limiter=%s path=%s first_status=%d second_status=%d body_bytes=%d",
					test.name, test.path, first.Code, limited.Code, limited.Body.Len())
				assert.Equal(t, "60", limited.Header().Get("Retry-After"))
				assert.Equal(t, "no-store", limited.Header().Get("Cache-Control"))
				assert.Equal(t, "no-referrer", limited.Header().Get("Referrer-Policy"))
				assert.NotEqual(t, "client-request-id-must-not-be-trusted", limited.Header().Get(common.RequestIdKey))
				assert.NotEqual(t, first.Header().Get(common.RequestIdKey), limited.Header().Get(common.RequestIdKey))
				require.NotEmpty(t, limited.Body.String(), "B1.5 429 canonical envelope missing")
				assertRouterAppPluginErrorEnvelope(t, limited, http.StatusTooManyRequests, "rate_limited", "Too many app plugin requests", true)
			})
		}

		t.Run("SetApiRouter global limiter", func(t *testing.T) {
			common.CriticalRateLimitEnable = false
			common.GlobalApiRateLimitEnable, common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration = true, 1, 60
			engine := gin.New()
			require.NoError(t, engine.SetTrustedProxies(nil))
			engine.Use(middleware.RequestId())
			SetApiRouter(engine)
			for i, test := range []struct {
				method, path string
				app          bool
			}{
				{http.MethodGet, "/api/app_plugins", true},
				{http.MethodGet, "/api/app_plugins/installations", true},
				{http.MethodPost, "/api/app_plugins/installations", true},
				{http.MethodPatch, "/api/app_plugins/installations", true},
				{http.MethodPost, "/api/app_plugins/test/authorize", true},
				{http.MethodPost, "/api/app_plugins/installations/test/service-credentials", true},
				{http.MethodPost, "/api/app_plugins/installations/test/service-credentials/rotate", true},
				{http.MethodDelete, "/api/app_plugins/installations/test/service-credentials/test", true},
				{http.MethodPost, "/internal/apps/v1/launch-codes/exchange", true},
				{http.MethodPost, "/internal/apps/v1/sessions/introspect", true},
				{http.MethodPost, "/internal/apps/v1/sessions/revoke", true},
				{http.MethodGet, "/api/models", false},
				{http.MethodGet, "/api/user/self", false},
			} {
				t.Run(test.method+" "+test.path, func(t *testing.T) {
					ip := fmt.Sprintf("203.0.113.%d", i+1)
					first := routerAppPluginRateLimitRequest(engine, http.MethodGet, "/api/app_plugins", "", ip)
					assertRouterAppPluginErrorEnvelope(t, first, http.StatusUnauthorized, "unauthenticated", "Authentication required", false)
					limited := routerAppPluginRateLimitRequest(engine, test.method, test.path, "", ip)
					t.Logf("limiter=GlobalAPI method=%s path=%s first_status=%d second_status=%d body_bytes=%d",
						test.method, test.path, first.Code, limited.Code, limited.Body.Len())
					assert.Equal(t, "60", limited.Header().Get("Retry-After"))
					if !test.app {
						assert.Equal(t, http.StatusTooManyRequests, limited.Code)
						assert.Empty(t, limited.Body.String(), "non-App global limiter contract must remain unchanged")
						assert.Empty(t, limited.Header().Get("Cache-Control"))
						assert.Empty(t, limited.Header().Get("Referrer-Policy"))
						return
					}
					assert.Equal(t, "no-store", limited.Header().Get("Cache-Control"))
					assert.Equal(t, "no-referrer", limited.Header().Get("Referrer-Policy"))
					assert.NotEqual(t, "client-request-id-must-not-be-trusted", limited.Header().Get(common.RequestIdKey))
					assert.NotEqual(t, first.Header().Get(common.RequestIdKey), limited.Header().Get(common.RequestIdKey))
					require.NotEmpty(t, limited.Body.String(), "B1.5 429 canonical envelope missing")
					assertRouterAppPluginErrorEnvelope(t, limited, http.StatusTooManyRequests, "rate_limited", "Too many app plugin requests", true)
				})
			}
		})
	})

	t.Run("normalization preserves existing responses", func(t *testing.T) {
		for _, test := range []struct {
			name, body string
			status     int
		}{
			{"canonical 429", `{"error":{"code":"capacity_exhausted","message":"Capacity exhausted","field_errors":[],"retryable":true,"request_id":"server-existing"}}`, http.StatusTooManyRequests},
			{"canonical 401", `{"error":{"code":"unauthenticated","message":"Authentication required","field_errors":[],"retryable":false,"request_id":"server-existing"}}`, http.StatusUnauthorized},
			{"empty 500", "", http.StatusInternalServerError},
			{"legacy business error", `{"success":false,"code":"BUSINESS_ERROR","message":"Rejected"}`, http.StatusBadRequest},
		} {
			t.Run(test.name, func(t *testing.T) {
				engine := gin.New()
				engine.GET("/api/app_plugins", normalizeAppPluginAuthErrors(), func(c *gin.Context) {
					c.Header("Retry-After", "17")
					c.Header("Cache-Control", "no-store")
					c.Header("Referrer-Policy", "no-referrer")
					c.Data(test.status, "application/json", []byte(test.body))
					c.Abort()
				})
				response := routerAppPluginRequest(engine, http.MethodGet, "/api/app_plugins", "", nil)
				assert.Equal(t, test.status, response.Code)
				assert.Equal(t, test.body, response.Body.String())
				assert.Equal(t, "17", response.Header().Get("Retry-After"))
				assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
				assert.Equal(t, "no-referrer", response.Header().Get("Referrer-Policy"))
			})
		}
	})

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

	t.Run("configuration requires currently disabled installation", func(t *testing.T) {
		for _, actor := range []struct {
			name, token string
			root        bool
		}{
			{"root", fixture.rootToken, true},
			{"plugin-admin", fixture.pluginAdminToken, false},
		} {
			for _, currentStatus := range []string{"enabled", "disabled"} {
				for _, nextStatus := range []string{"disabled", "revoked"} {
					for _, bundled := range []bool{false, true} {
						name := fmt.Sprintf("%s-%s-%s-config-%t", actor.name, currentStatus, nextStatus, bundled)
						t.Run(name, func(t *testing.T) {
							created := routerAppPluginRequestWithHeaders(fixture.engine, http.MethodPost,
								"/api/app_plugins/installations", fixture.rootToken,
								routerAppPluginInstallBody(t, name, "1.0.0"), map[string]string{"Idempotency-Key": name})
							require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
							var installed routerAppPluginEnvelope
							require.NoError(t, common.Unmarshal(created.Body.Bytes(), &installed))
							id := installed.Data.InstallationID
							if currentStatus == "enabled" {
								_, err := model.CompareAndSwapAppInstallationStatus(t.Context(), model.DB,
									id, installed.Data.Revision, model.AppInstallationStatusEnabled)
								require.NoError(t, err)
							}
							require.NoError(t, model.DB.Create(&model.AppServiceCredential{
								AppKey: name, InstallationID: id, CredentialID: name,
								CredentialHash: "test-hash", CredentialVersion: "v1", Status: "active",
							}).Error)
							var before model.AppInstallation
							var claimsBefore []model.AppRouteClaim
							var credentialsBefore []model.AppServiceCredential
							var policiesBefore []model.AppEntitlementPolicy
							require.NoError(t, model.DB.Where("installation_id = ?", id).First(&before).Error)
							require.NoError(t, model.DB.Where("installation_id = ?", id).Order("id").Find(&claimsBefore).Error)
							require.NoError(t, model.DB.Where("installation_id = ?", id).Order("id").Find(&credentialsBefore).Error)
							require.NoError(t, model.DB.Order("id").Find(&policiesBefore).Error)

							changes := map[string]any{"status": nextStatus}
							if bundled {
								changes["allowed_origins"] = []string{"https://changed.example.com"}
								changes["base_url"] = "https://apps.example.com/changed-" + name + "/"
								if actor.root {
									changes["entitlement_policy"] = map[string]any{
										"key": name, "rules": map[string][]string{"app_plugin": {"manage"}},
									}
								}
							}
							wantStatus, wantCode := http.StatusOK, ""
							switch {
							case !actor.root && nextStatus == "revoked":
								wantStatus, wantCode = http.StatusForbidden, "forbidden"
							case currentStatus == "enabled" && bundled:
								wantStatus, wantCode = http.StatusConflict, "invalid_state_transition"
							case currentStatus == "disabled" && nextStatus == "disabled" && !bundled:
								wantStatus, wantCode = http.StatusConflict, "invalid_state_transition"
							}
							response := routerAppPluginRequest(fixture.engine, http.MethodPatch,
								"/api/app_plugins/installations", actor.token,
								routerAppPluginPatchBody(t, id, before.Revision, changes))
							assert.Equal(t, wantStatus, response.Code, response.Body.String())
							assert.Equal(t, wantCode, routerAppPluginErrorCode(t, response))

							var after model.AppInstallation
							var claimsAfter []model.AppRouteClaim
							var credentialsAfter []model.AppServiceCredential
							var policiesAfter []model.AppEntitlementPolicy
							require.NoError(t, model.DB.Where("installation_id = ?", id).First(&after).Error)
							require.NoError(t, model.DB.Where("installation_id = ?", id).Order("id").Find(&claimsAfter).Error)
							require.NoError(t, model.DB.Where("installation_id = ?", id).Order("id").Find(&credentialsAfter).Error)
							require.NoError(t, model.DB.Order("id").Find(&policiesAfter).Error)
							if wantStatus != http.StatusOK {
								assert.Equal(t, before, after, "rejection must preserve installation and revision")
								assert.Equal(t, claimsBefore, claimsAfter, "rejection must preserve route claims")
								assert.Equal(t, credentialsBefore, credentialsAfter, "rejection must preserve credentials")
								assert.Equal(t, policiesBefore, policiesAfter, "rejection must not create policy versions")
								return
							}
							assert.Equal(t, nextStatus, after.Status)
							assert.Equal(t, before.Revision+1, after.Revision)
							if bundled {
								assert.Equal(t, model.AppStringList{"https://changed.example.com"}, after.AllowedOrigins)
								assert.Equal(t, changes["base_url"], after.BaseURL)
							} else {
								assert.Equal(t, before.AllowedOrigins, after.AllowedOrigins)
								assert.Equal(t, policiesBefore, policiesAfter)
							}
							if nextStatus == "revoked" {
								assert.Empty(t, claimsAfter)
								require.Len(t, credentialsAfter, 1)
								assert.Equal(t, "revoked", credentialsAfter[0].Status)
							} else {
								assert.Len(t, claimsAfter, len(claimsBefore))
								assert.Equal(t, credentialsBefore, credentialsAfter)
							}
						})
					}
				}
			}
		}
	})
}

func setupRouterAppPluginTest(t *testing.T) routerAppPluginFixture {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis := common.RedisEnabled
	previousMaster := common.IsMasterNode
	previousFlag := operation_setting.AppPluginV1Enabled

	db := openRouterAppPluginDB(t)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled = previousRedis
		common.IsMasterNode = previousMaster
		operation_setting.AppPluginV1Enabled = previousFlag
	})
	dbType := map[string]common.DatabaseType{
		"sqlite": common.DatabaseTypeSQLite, "mysql": common.DatabaseTypeMySQL, "postgres": common.DatabaseTypePostgreSQL,
	}[db.Dialector.Name()]
	common.SetDatabaseTypes(dbType, dbType)
	require.NoError(t, db.AutoMigrate(
		&model.User{},
		&model.TaskPlugin{},
		&model.AuditLog{},
		&model.CasbinRule{},
		&model.AuthzRole{},
	))
	require.NoError(t, model.MigrateAppPluginTables(db))
	model.DB, model.LOG_DB = db, db
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

	return routerAppPluginFixture{
		engine:            engine,
		rootToken:         rootToken,
		pluginAdminToken:  pluginAdminToken,
		userToken:         userToken,
		disabledUserToken: disabledUserToken,
	}
}

func openRouterAppPluginDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialect, dsn := os.Getenv("APP_PLUGIN_TEST_DIALECT"), os.Getenv("APP_PLUGIN_TEST_DSN")
	if dialect == "" {
		dialect = "sqlite"
	}
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	name := fmt.Sprintf("app_plugin_router_%d_%d", os.Getpid(), time.Now().UnixNano())
	var db *gorm.DB
	var err error
	switch dialect {
	case "sqlite":
		if dsn == "" {
			dsn = filepath.Join(t.TempDir(), "app-plugin-router.db")
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
		require.NoError(t, admin.Exec("CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error)
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

func routerAppPluginRequest(engine http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	return routerAppPluginRequestWithHeaders(engine, method, path, token, body, nil)
}

func routerAppPluginRateLimitRequest(engine http.Handler, method, path, token, clientIP string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = clientIP + ":12345"
	request.Header.Set(common.RequestIdKey, "client-request-id-must-not-be-trusted")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
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

const routerExecutionPolicy = `{"operations":{"model.generate":{
	"models":[{"public_model":"doubao-seedance-2-5-260628","actual_model":"doubao-seedance-2-5-260628",
	"plugin_key":"doubao","plugin_version":"1.2.0","protocol":"openai_video"}],
	"strategies":["stable"],"conversion_policies":["strict"],"passthrough_rules":[]}}}`

type routerExecutionFixture struct {
	engine     *gin.Engine
	app        model.AppInstallation
	session    model.AppPluginSession
	root       service.AuthIdentity
	rootJWT    string
	userJWT    string
	rootPAT    string
	credential model.AppServiceCredentialIssued
}

func setupRouterExecutionTest(t *testing.T) *routerExecutionFixture {
	t.Helper()
	return setupRouterExecutionAppTest(t, "http-app")
}

func setupRouterExecutionAppTest(t *testing.T, appKey string) *routerExecutionFixture {
	t.Helper()
	base := setupRouterAppPluginTest(t)
	oldAddress, oldBatch := system_setting.ServerAddress, common.BatchUpdateEnabled
	oldCritical, oldGlobal := common.CriticalRateLimitEnable, common.GlobalApiRateLimitEnable
	oldGrant, oldSeedance, oldEmbedded := operation_setting.AppExecutionGrantsEnabled,
		operation_setting.AppPluginSeedanceEnabled, operation_setting.AppPluginEmbeddedSurfaceEnabled
	oldRegistry := jsplugin.DefaultRegistry
	t.Cleanup(func() {
		system_setting.ServerAddress, common.BatchUpdateEnabled = oldAddress, oldBatch
		common.CriticalRateLimitEnable, common.GlobalApiRateLimitEnable = oldCritical, oldGlobal
		operation_setting.AppExecutionGrantsEnabled = oldGrant
		operation_setting.AppPluginSeedanceEnabled, operation_setting.AppPluginEmbeddedSurfaceEnabled = oldSeedance, oldEmbedded
		jsplugin.DefaultRegistry = oldRegistry
	})
	system_setting.ServerAddress = "https://console.example.com"
	common.BatchUpdateEnabled, common.CriticalRateLimitEnable, common.GlobalApiRateLimitEnable = false, false, false
	t.Setenv("APP_PLUGIN_LAUNCH_SECRET", strings.Repeat("offline-http-fixture-", 3))
	operation_setting.AppPluginV1Enabled = true
	operation_setting.AppExecutionGrantsEnabled = true
	operation_setting.AppPluginSeedanceEnabled, operation_setting.AppPluginEmbeddedSurfaceEnabled = true, true
	require.NoError(t, model.DB.AutoMigrate(&model.UserSession{}, &model.Option{}, &model.Channel{}, &model.Ability{},
		&model.UserSubscription{}, &model.SubscriptionPlan{}))
	require.NoError(t, model.MigrateAppPluginLaunchTables(model.DB))
	require.NoError(t, model.MigrateAppExecutionTables(model.DB))
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	source, err := os.ReadFile("../plugins/tasks/doubao/plugin.js")
	require.NoError(t, err)
	_, err = jsplugin.DefaultRegistry.RegisterFactory(string(source), jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	var installBody map[string]any
	require.NoError(t, common.Unmarshal(routerAppPluginInstallBody(t, appKey, "1.0.0"), &installBody))
	scopes := []string{"identity.read", "model.invoke", "task.read", "task.import"}
	manifest := installBody["manifest"].(map[string]any)
	manifest["requestedScopes"] = scopes
	manifest["surfaces"].(map[string]any)["embedded"] = map[string]string{"startPath": "/bootstrap/frame"}
	created := routerAppPluginRequestWithHeaders(base.engine, http.MethodPost, "/api/app_plugins/installations",
		base.rootToken, routerAppPluginJSON(t, installBody), map[string]string{"Idempotency-Key": "http-fixture"})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var envelope routerAppPluginEnvelope
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &envelope))
	app, err := model.CompareAndSwapAppInstallationStatus(t.Context(), model.DB, envelope.Data.InstallationID,
		envelope.Data.Revision, model.AppInstallationStatusEnabled)
	require.NoError(t, err)
	f := &routerExecutionFixture{app: app, rootPAT: base.rootToken}
	var user model.User
	require.NoError(t, model.DB.Where("username = ?", "app-plugin-user").First(&user).Error)
	require.NoError(t, model.DB.Model(&user).Update("quota", 1000000).Error)
	for _, root := range []bool{true, false} {
		actor := user
		if root {
			actor = model.User{}
			require.NoError(t, model.DB.Where("username = ?", "app-plugin-root").First(&actor).Error)
		}
		dashboard := model.UserSession{SID: uuid.NewString(), UserID: actor.Id, Version: 1, UserAuthVersion: 1,
			Status: model.UserSessionStatusActive, ExpiresAt: time.Now().Add(time.Hour).Unix(),
			RefreshHash: fmt.Sprintf("%x", sha256.Sum256([]byte("http-refresh-fixture")))}
		require.NoError(t, model.DB.Create(&dashboard).Error)
		identity := service.AuthIdentity{UserID: actor.Id, SessionID: dashboard.SID, UserAuthVersion: 1, SessionVersion: 1}
		token, _, err := service.IssueAccessToken(identity)
		require.NoError(t, err)
		if root {
			f.root, f.rootJWT = identity, token
		} else {
			f.userJWT = token
			f.session = model.AppPluginSession{AppSessionID: uuid.NewString(), InstallationID: app.InstallationID,
				AppKey: app.AppKey, Generation: app.AppVersionID, Issuer: system_setting.ServerAddress,
				Subject: fmt.Sprintf("user_%d", user.Id), UserID: user.Id, DashboardSessionID: dashboard.SID,
				AuthVersion: 1, SessionVersion: 1, GrantedScopes: scopes, UpstreamExpiresAt: dashboard.ExpiresAt}
			require.NoError(t, model.DB.Create(&f.session).Error)
		}
	}
	f.credential, err = model.IssueAppServiceCredential(t.Context(), model.DB, app.InstallationID,
		scopes, time.Now(), time.Now().Add(time.Hour), false)
	require.NoError(t, err)
	setting := `{"task_plugin_key":"doubao"}`
	channel := model.Channel{Type: constant.ChannelTypeDoubaoVideo, Name: "http-offline", Key: "offline-key",
		Setting: &setting, Status: common.ChannelStatusEnabled, Group: "default", Models: "doubao-seedance-2-5-260628"}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(model.DB))
	require.NoError(t, model.DB.Create(&[]model.Option{
		{Key: "ModelPrice", Value: `{"doubao-seedance-2-5-260628":0.01}`},
		{Key: "UserUsableGroups", Value: `{"default":"Default"}`},
		{Key: "GroupRatio", Value: `{"default":1}`}, {Key: "AutoGroups", Value: `["default"]`},
	}).Error)
	f.engine = gin.New()
	f.engine.Use(middleware.RequestId())
	SetApiRouter(f.engine)
	return f
}

func (f *routerExecutionFixture) call(t *testing.T, method, path, authorization, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw = routerAppPluginJSON(t, body)
	}
	return routerAppPluginRequestWithHeaders(f.engine, method, "https://console.example.com"+path, "", raw,
		map[string]string{"Authorization": authorization, "Idempotency-Key": key})
}

func (f *routerExecutionFixture) publish(t *testing.T) int64 {
	t.Helper()
	version, err := service.PublishAppModelInvokePolicy(t.Context(), model.DB, f.root, []byte(routerExecutionPolicy), time.Now())
	require.NoError(t, err)
	return version
}

func TestAppExecutionControlHTTPContracts(t *testing.T) {
	f := setupRouterExecutionTest(t)
	version := f.publish(t)
	request := service.AppExecutionGrantRequest{RequestID: uuid.NewString(), AppKey: f.app.AppKey,
		AppSessionID: f.session.AppSessionID, Subject: f.session.Subject, RunID: uuid.NewString(),
		ExecutionRequestID: uuid.NewString(), Operation: "model.generate", ModelPolicyVersion: version,
		RequestedModels: []string{"doubao-seedance-2-5-260628"}, Strategy: "stable", ConversionPolicy: "strict",
		PassthroughSelections: []service.AppPassthroughSelection{}}
	auth := "AppService " + f.credential.Credential
	grantResponse := f.call(t, http.MethodPost, "/internal/apps/v1/execution-grants", auth, "", request)
	require.Equal(t, http.StatusOK, grantResponse.Code, "real grant route must accept a current service")
	var grant struct {
		Data service.AppExecutionGrantResult `json:"data"`
	}
	require.NoError(t, common.Unmarshal(grantResponse.Body.Bytes(), &grant))
	require.True(t, grant.Data.GrantToken != "")
	assert.Equal(t, "no-store", grantResponse.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", grantResponse.Header().Get("Referrer-Policy"))
	stale := request
	stale.RequestID, stale.ModelPolicyVersion = uuid.NewString(), version+1
	response := f.call(t, http.MethodPost, "/internal/apps/v1/execution-grants", auth, "", stale)
	require.Equal(t, http.StatusConflict, response.Code)
	assert.Equal(t, "version_conflict", routerAppPluginErrorCode(t, response))
	denied := request
	denied.RequestID, denied.RequestedModels = uuid.NewString(), []string{"unapproved"}
	response = f.call(t, http.MethodPost, "/internal/apps/v1/execution-grants", auth, "", denied)
	require.Equal(t, http.StatusForbidden, response.Code)
	assert.Equal(t, "model_policy_denied", routerAppPluginErrorCode(t, response))
	task := model.AppTaskExecution{TaskID: "http-task", GrantID: grant.Data.GrantID, LogicalHash: "http-logical",
		AppKey: f.app.AppKey, InstallationID: f.app.InstallationID, AppSessionID: f.session.AppSessionID,
		Subject: f.session.Subject, UserID: f.session.UserID, RunID: request.RunID, ExecutionRequestID: request.ExecutionRequestID,
		Operation: request.Operation, ProviderAccepted: true, Status: "accepted", ProviderState: "running",
		ActualModel: request.RequestedModels[0], PluginKey: "doubao", PluginVersion: "1.2.0", UpdatedAt: time.Now().Unix()}
	require.NoError(t, model.DB.Create(&task).Error)
	lookup := map[string]any{"request_id": uuid.NewString(), "app_key": f.app.AppKey,
		"app_session_id": f.session.AppSessionID, "subject": f.session.Subject, "task_id": task.TaskID, "grant_id": nil}
	require.NoError(t, model.DB.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.credential.CredentialID).
		Update("scopes", `["task.read"]`).Error)
	response = f.call(t, http.MethodPost, "/internal/apps/v1/tasks/lookup", auth, "", lookup)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var facts struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &facts))
	assert.Len(t, facts.Data, 13)
	assert.Equal(t, "running", facts.Data["status"])
	delete(lookup, "grant_id")
	response = f.call(t, http.MethodPost, "/internal/apps/v1/tasks/lookup", auth, "", lookup)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	lookup["grant_id"] = nil
	cancel := service.AppTaskCancelRequest{RequestID: uuid.NewString(), AppKey: f.app.AppKey,
		AppSessionID: f.session.AppSessionID, Subject: f.session.Subject, TaskID: task.TaskID, Reason: "user_requested"}
	response = f.call(t, http.MethodPost, "/internal/apps/v1/tasks/cancel", auth, "cancel-http", cancel)
	assert.Equal(t, http.StatusForbidden, response.Code)
	require.NoError(t, model.DB.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.credential.CredentialID).
		Update("scopes", `["task.read","model.invoke"]`).Error)
	first := f.call(t, http.MethodPost, "/internal/apps/v1/tasks/cancel", auth, "cancel-http", cancel)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	again := f.call(t, http.MethodPost, "/internal/apps/v1/tasks/cancel", auth, "cancel-http", cancel)
	assert.JSONEq(t, first.Body.String(), again.Body.String())
	assert.Contains(t, first.Body.String(), `"state":"not_cancellable"`)
	assert.Contains(t, first.Body.String(), `"cancelled_at":null`)
	cancel.Reason = "changed"
	assert.Equal(t, http.StatusConflict, f.call(t, http.MethodPost, "/internal/apps/v1/tasks/cancel", auth, "cancel-http", cancel).Code)
	assert.Equal(t, http.StatusBadRequest, f.call(t, http.MethodPost, "/internal/apps/v1/tasks/cancel", auth, "", cancel).Code)
	require.NoError(t, model.DB.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.credential.CredentialID).
		Update("scopes", `["task.import"]`).Error)
	_, err := service.PublishAppArkImportDelegations(t.Context(), model.DB, f.root, routerAppPluginJSON(t,
		service.AppArkImportDelegations{Delegations: []service.AppArkImportDelegation{{InstallationID: f.app.InstallationID,
			UserID: f.session.UserID, AccountRef: "host-account", ProjectID: "project",
			StartAt: time.Now().Add(-time.Hour), EndAt: time.Now().Add(time.Hour)}}}), time.Now())
	require.NoError(t, err)
	ark := service.AppArkImportLookupRequest{RequestID: uuid.NewString(), AppKey: f.app.AppKey,
		AppSessionID: f.session.AppSessionID, Subject: f.session.Subject, TaskID: "cgt-old",
		QueryScope: service.AppArkQueryScope{ProjectID: "project", StartAt: time.Now().Add(-time.Minute), EndAt: time.Now()}}
	response = f.call(t, http.MethodPost, "/internal/apps/v1/imports/ark-task-lookup", auth, "", ark)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, "unconfigured live source fails closed")
	assert.Equal(t, "service_unavailable", routerAppPluginErrorCode(t, response))
	for _, path := range []string{"/internal/apps/v1/execution-grants", "/internal/apps/v1/tasks/lookup",
		"/internal/apps/v1/tasks/cancel", "/internal/apps/v1/imports/ark-task-lookup"} {
		assert.Equal(t, http.StatusUnauthorized, f.call(t, http.MethodPost, path, "Bearer "+f.rootJWT, "", lookup).Code)
	}
	var after model.AppTaskExecution
	require.NoError(t, model.DB.First(&after, task.ID).Error)
	assert.Equal(t, task, after)
	for _, table := range []any{&model.AppTaskSettlement{}, &model.AppTaskOutbox{}} {
		var count int64
		require.NoError(t, model.DB.Model(table).Count(&count).Error)
		assert.Zero(t, count)
	}

	t.Run("internal evidence error uses the public vocabulary", func(t *testing.T) {
		require.NoError(t, model.DB.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.credential.CredentialID).
			Update("scopes", `["task.read"]`).Error)
		require.NoError(t, model.DB.Model(&task).Update("provider_state", "unrecognized").Error)
		response := f.call(t, http.MethodPost, "/internal/apps/v1/tasks/lookup", auth, "", lookup)
		assertRouterAppPluginErrorEnvelope(t, response, http.StatusServiceUnavailable,
			"service_unavailable", "App plugin request failed", true)
	})

	t.Run("internal funding error uses the public vocabulary", func(t *testing.T) {
		require.NoError(t, model.DB.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.credential.CredentialID).
			Update("scopes", `["model.invoke"]`).Error)
		common.BatchUpdateEnabled = true
		defer func() { common.BatchUpdateEnabled = false }()
		request.RequestID = uuid.NewString()
		response := f.call(t, http.MethodPost, "/internal/apps/v1/execution-grants", auth, "", request)
		assertRouterAppPluginErrorEnvelope(t, response, http.StatusServiceUnavailable,
			"service_unavailable", "App plugin request failed", true)
	})
}

func TestAppExecutionHTTPRolloutGates(t *testing.T) {
	f := setupRouterExecutionAppTest(t, "seedance-repro")
	request := service.AppExecutionGrantRequest{RequestID: uuid.NewString(), AppKey: f.app.AppKey,
		AppSessionID: f.session.AppSessionID, Subject: f.session.Subject, RunID: uuid.NewString(),
		ExecutionRequestID: uuid.NewString(), Operation: "model.generate", ModelPolicyVersion: f.publish(t),
		RequestedModels: []string{"doubao-seedance-2-5-260628"}, Strategy: "stable", ConversionPolicy: "strict",
		PassthroughSelections: []service.AppPassthroughSelection{}}
	auth := "AppService " + f.credential.Credential
	response := f.call(t, http.MethodPost, "/internal/apps/v1/execution-grants", auth, "", request)
	require.Equal(t, http.StatusOK, response.Code)
	var grant struct {
		Data service.AppExecutionGrantResult `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &grant))
	task := model.AppTaskExecution{TaskID: "rollout-task", GrantID: grant.Data.GrantID, LogicalHash: "rollout-logical",
		AppKey: f.app.AppKey, InstallationID: f.app.InstallationID, AppSessionID: f.session.AppSessionID,
		Subject: f.session.Subject, UserID: f.session.UserID, RunID: request.RunID, ExecutionRequestID: request.ExecutionRequestID,
		Operation: request.Operation, ProviderAccepted: true, Status: "accepted", ProviderState: "running",
		ActualModel: request.RequestedModels[0], PluginKey: "doubao", PluginVersion: "1.2.0", UpdatedAt: time.Now().Unix()}
	require.NoError(t, model.DB.Create(&task).Error)
	lookup := map[string]any{"request_id": uuid.NewString(), "app_key": f.app.AppKey,
		"app_session_id": f.session.AppSessionID, "subject": f.session.Subject, "task_id": task.TaskID, "grant_id": nil}
	cancel := service.AppTaskCancelRequest{RequestID: uuid.NewString(), AppKey: f.app.AppKey,
		AppSessionID: f.session.AppSessionID, Subject: f.session.Subject, TaskID: task.TaskID, Reason: "user_requested"}
	introspect := service.AppPluginIntrospectRequest{AppKey: f.app.AppKey, AppSessionID: f.session.AppSessionID,
		Subject: f.session.Subject, RequiredScopes: []string{}, RequiredEntitlements: model.AppJSONMap{}}
	for _, test := range []struct {
		name string
		flag *bool
	}{
		{"execution", &operation_setting.AppExecutionGrantsEnabled},
		{"seedance", &operation_setting.AppPluginSeedanceEnabled},
		{"app_plugin", &operation_setting.AppPluginV1Enabled},
	} {
		t.Run(test.name, func(t *testing.T) {
			*test.flag = false
			defer func() { *test.flag = true }()
			request.RequestID = uuid.NewString()
			response := f.call(t, http.MethodPost, "/internal/apps/v1/execution-grants", auth, "", request)
			assert.Equal(t, http.StatusForbidden, response.Code, "closed rollout gate must reject new grants")
			if response.Code != http.StatusOK {
				assert.Equal(t, "app_plugin_disabled", routerAppPluginErrorCode(t, response))
			}
			assert.Equal(t, http.StatusForbidden,
				f.call(t, http.MethodPost, "/internal/apps/v1/tasks/cancel", auth, "rollout-cancel", cancel).Code)
			response = f.call(t, http.MethodPost, "/internal/apps/v1/tasks/lookup", auth, "", lookup)
			assert.Equal(t, http.StatusOK, response.Code, "closed rollout gate must preserve accepted Task facts")
			assert.Equal(t, http.StatusUnauthorized,
				f.call(t, http.MethodPost, "/internal/apps/v1/tasks/lookup", "Bearer "+f.userJWT, "", lookup).Code)
			response = f.call(t, http.MethodPost, "/internal/apps/v1/sessions/introspect", auth, "", introspect)
			assert.Equal(t, http.StatusOK, response.Code)
			assert.Empty(t, response.Header().Get("X-App-Model-Policy-Version"))
		})
	}
	for _, test := range []struct {
		name     string
		flag     *bool
		surfaces []string
	}{
		{"seedance", &operation_setting.AppPluginSeedanceEnabled, []string{"direct", "embedded"}},
		{"embedded", &operation_setting.AppPluginEmbeddedSurfaceEnabled, []string{"embedded"}},
	} {
		t.Run("navigation_"+test.name, func(t *testing.T) {
			*test.flag = false
			defer func() { *test.flag = true }()
			response := f.call(t, http.MethodGet, "/api/app_plugins", "Bearer "+f.userJWT, "", nil)
			require.Equal(t, http.StatusOK, response.Code)
			var navigation struct {
				Data []struct {
					Key      string   `json:"key"`
					Surfaces []string `json:"enabled_surfaces"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &navigation))
			if test.name == "seedance" {
				assert.Empty(t, navigation.Data, "disabled Seedance must not be advertised")
			} else {
				require.Len(t, navigation.Data, 1)
				assert.Equal(t, []string{"direct"}, navigation.Data[0].Surfaces)
				assert.Equal(t, http.StatusOK, f.call(t, http.MethodGet,
					"/api/app_plugins/seedance-repro/launch-context?surface=direct", "Bearer "+f.userJWT, "", nil).Code)
			}
			for _, surface := range test.surfaces {
				response = f.call(t, http.MethodGet, "/api/app_plugins/seedance-repro/launch-context?surface="+surface,
					"Bearer "+f.userJWT, "", nil)
				assert.Equal(t, http.StatusForbidden, response.Code, "closed surface gate must reject launch context")
				authorize := service.AppPluginAuthorizeRequest{Surface: surface, TransactionID: uuid.NewString(),
					State: strings.Repeat("s", 42) + "A", Nonce: strings.Repeat("n", 42) + "A",
					CodeChallenge: strings.Repeat("c", 42) + "A", CodeChallengeMethod: "S256"}
				response = routerAppPluginRequestWithHeaders(f.engine, http.MethodPost,
					"https://console.example.com/api/app_plugins/seedance-repro/authorize", "",
					routerAppPluginJSON(t, authorize), map[string]string{"Authorization": "Bearer " + f.userJWT,
						"Origin": "https://console.example.com", "Idempotency-Key": uuid.NewString()})
				assert.Equal(t, http.StatusForbidden, response.Code, "authorize must not bypass closed surface gate")
				if test.name == "seedance" && surface == "direct" {
					response = routerAppPluginRequestWithHeaders(f.engine, http.MethodPost,
						"https://console.example.com/api/app_plugins/SEEDANCE-REPRO/authorize", "",
						routerAppPluginJSON(t, authorize), map[string]string{"Authorization": "Bearer " + f.userJWT,
							"Origin": "https://console.example.com", "Idempotency-Key": uuid.NewString()})
					assert.Equal(t, http.StatusNotFound, response.Code, "authorize requires case-sensitive App identity")
				}
			}
		})
	}
	var after model.AppTaskExecution
	require.NoError(t, model.DB.First(&after, task.ID).Error)
	assert.Equal(t, task, after)
	for _, test := range []struct {
		table any
		count int64
	}{
		{&model.AppExecutionGrant{}, 1}, {&model.AppTaskExecution{}, 1},
		{&model.AppPluginLaunchCode{}, 0}, {&model.AppTaskCancelReplay{}, 0},
		{&model.AppTaskSettlement{}, 0}, {&model.AppTaskOutbox{}, 0},
	} {
		var count int64
		require.NoError(t, model.DB.Model(test.table).Count(&count).Error)
		assert.Equal(t, test.count, count, "closed rollout gates must not create side effects")
	}
	t.Run("session invalidation remains available", func(t *testing.T) {
		operation_setting.AppPluginV1Enabled = false
		defer func() { operation_setting.AppPluginV1Enabled = true }()
		revoke := service.AppPluginSessionRevokeRequest{RequestID: uuid.NewString(), AppKey: f.app.AppKey,
			AppSessionID: f.session.AppSessionID, Subject: f.session.Subject, Reason: "user_logout"}
		response := f.call(t, http.MethodPost, "/internal/apps/v1/sessions/revoke", auth, "stop-session", revoke)
		require.Equal(t, http.StatusOK, response.Code)
		assert.Contains(t, response.Body.String(), `"state":"revoked"`)
		response = f.call(t, http.MethodPost, "/internal/apps/v1/tasks/lookup", auth, "", lookup)
		assert.Equal(t, http.StatusUnauthorized, response.Code, "retained reads still require an unrevoked App session")
	})
}

func TestAppGrantNativeResponseHTTPPersistsAndReplays(t *testing.T) {
	var sends atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		sends.Add(1)
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/v1/responses", request.URL.Path)
		assert.Equal(t, 1, request.ProtoMajor)
		assert.Equal(t, "Bearer native-router-secret", request.Header.Get("Authorization"))
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		assert.JSONEq(t, `{"model":"gpt-native-router","input":"hello","max_output_tokens":32,"stream":false,"store":false}`, string(body))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "provider-native-router")
		_, _ = io.WriteString(w, `{"id":"resp_router","status":"completed","model":"gpt-native-router","error":null,"incomplete_details":null,"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	}))
	t.Cleanup(upstream.Close)
	f := setupRouterExecutionTest(t)
	SetTaskPluginProtocolRouter(f.engine)
	require.NoError(t, model.DB.AutoMigrate(&model.Task{}, &model.SystemTask{}))
	previousQuotaUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = previousQuotaUnit })
	const publicModel = "registered-native-router"
	mapping := `{"` + publicModel + `":"gpt-native-router"}`
	channel := model.Channel{
		Type: constant.ChannelTypeOpenAI, Name: "native-router", Key: "native-router-secret",
		BaseURL: common.GetPointer(upstream.URL + "/v1"), ModelMapping: &mapping,
		Status: common.ChannelStatusEnabled, Group: "default", Models: publicModel,
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(model.DB))
	require.NoError(t, model.DB.Model(&model.Option{}).Where(map[string]any{"key": "ModelPrice"}).
		Update("value", `{"doubao-seedance-2-5-260628":0.01,"`+publicModel+`":0.02}`).Error)
	policy := service.AppModelInvokePolicy{Operations: map[string]service.AppModelOperationPolicy{
		"model.generate": {
			Models: []service.AppModelAdmission{{
				PublicModel: publicModel, ActualModel: "gpt-native-router",
				Protocol: "openai_responses", ExecutionKind: model.AppExecutionModelKindNativeResponse,
				ChannelTypes: []int{constant.ChannelTypeOpenAI}, RequestProfile: "responses.text.v1",
			}},
			Strategies: []string{"stable"}, ConversionPolicies: []string{"strict"},
			PassthroughRules: []service.AppPassthroughRule{},
		},
	}}
	policyRaw, err := common.Marshal(policy)
	require.NoError(t, err)
	policyVersion, err := service.PublishAppModelInvokePolicy(
		t.Context(), model.DB, f.root, policyRaw, time.Now(),
	)
	require.NoError(t, err)
	request := service.AppExecutionGrantRequest{
		RequestID: uuid.NewString(), AppKey: f.app.AppKey,
		AppSessionID: f.session.AppSessionID, Subject: f.session.Subject,
		RunID: uuid.NewString(), ExecutionRequestID: uuid.NewString(),
		Operation: "model.generate", ModelPolicyVersion: policyVersion,
		RequestedModels: []string{publicModel}, Strategy: "stable",
		ConversionPolicy: "strict", PassthroughSelections: []service.AppPassthroughSelection{},
	}
	issuedResponse := f.call(t, http.MethodPost, "/internal/apps/v1/execution-grants",
		"AppService "+f.credential.Credential, "", request)
	require.Equal(t, http.StatusOK, issuedResponse.Code, issuedResponse.Body.String())
	var issued struct {
		Data service.AppExecutionGrantResult `json:"data"`
	}
	require.NoError(t, common.Unmarshal(issuedResponse.Body.Bytes(), &issued))
	body := []byte(`{"model":"registered-native-router","input":"hello","max_output_tokens":32}`)
	call := func() *httptest.ResponseRecorder {
		return routerAppPluginRequestWithHeaders(
			f.engine, http.MethodPost, "https://console.example.com/v1/responses", "", body,
			map[string]string{"Authorization": "AppGrant " + issued.Data.GrantToken},
		)
	}

	first := call()
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Equal(t, "provider-native-router", first.Header().Get("X-Request-ID"))
	assert.JSONEq(t, `{"id":"resp_router","status":"completed","model":"gpt-native-router","error":null,"incomplete_details":null,"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`, first.Body.String())
	second := call()
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, int32(1), sends.Load())

	var execution model.AppTaskExecution
	require.NoError(t, model.DB.Where("grant_id = ?", issued.Data.GrantID).First(&execution).Error)
	assert.Equal(t, model.AppExecutionKindResponse, execution.ExecutionKind)
	assert.Equal(t, "captured", execution.Status)
	assert.Equal(t, "settled", execution.BillingState)
	for _, table := range []any{&model.AppResponseResult{}, &model.AppTaskSettlement{}, &model.AppTaskOutbox{}} {
		var count int64
		require.NoError(t, model.DB.Model(table).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	}
	for _, table := range []any{&model.Task{}, &model.AppTaskReconcile{}} {
		var count int64
		require.NoError(t, model.DB.Model(table).Count(&count).Error)
		assert.Zero(t, count)
	}
}

func TestAppExecutionPolicyHTTPPublication(t *testing.T) {
	f := setupRouterExecutionTest(t)
	for _, key := range []string{model.AppModelInvokePolicyKey, model.AppArkImportDelegationsKey} {
		raw := routerExecutionPolicy
		if key == model.AppArkImportDelegationsKey {
			raw = `{"delegations":[]}`
		}
		body := map[string]any{"key": key, "value": raw}
		response := f.call(t, http.MethodPut, "/api/option/", "Bearer "+f.rootJWT, "", body)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var published struct {
			Success bool `json:"success"`
			Data    struct {
				Version int64 `json:"version"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &published))
		require.True(t, published.Success, response.Body.String())
		assert.EqualValues(t, 1, published.Data.Version)
		response = f.call(t, http.MethodPut, "/api/option/", "Bearer "+f.rootJWT, "", body)
		require.Equal(t, http.StatusOK, response.Code)
		head, err := model.GetAppExecutionPolicyTx(model.DB, key)
		require.NoError(t, err)
		assert.EqualValues(t, 2, head.Version)
		body["value"] = `{"unexpected":true}`
		assert.Equal(t, http.StatusBadRequest, f.call(t, http.MethodPut, "/api/option/", "Bearer "+f.rootJWT, "", body).Code)
		body["value"] = raw
		assert.Equal(t, http.StatusUnauthorized, f.call(t, http.MethodPut, "/api/option/", "Bearer "+f.rootPAT, "", body).Code)
		assert.GreaterOrEqual(t, f.call(t, http.MethodPut, "/api/option/", "Bearer "+f.userJWT, "", body).Code, 400)
		for _, alias := range []string{strings.ToLower(key), key + " "} {
			body["key"] = alias
			response = f.call(t, http.MethodPut, "/api/option/", "Bearer "+f.rootJWT, "", body)
			var result map[string]any
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.NotEqual(t, true, result["success"])
		}
		var count int64
		require.NoError(t, model.DB.Model(&model.AppExecutionPolicyVersion{}).Where("policy_key = ?", key).Count(&count).Error)
		assert.EqualValues(t, 2, count)
	}
}

func TestAppIntrospectionPolicyHeaderHTTP(t *testing.T) {
	f := setupRouterExecutionTest(t)
	body := service.AppPluginIntrospectRequest{AppKey: f.app.AppKey, AppSessionID: f.session.AppSessionID,
		Subject: f.session.Subject, RequiredScopes: []string{}, RequiredEntitlements: model.AppJSONMap{}}
	read := func() *httptest.ResponseRecorder {
		return f.call(t, http.MethodPost, "/internal/apps/v1/sessions/introspect", "AppService "+f.credential.Credential, "", body)
	}
	response := read()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Empty(t, response.Header().Get("X-App-Model-Policy-Version"))
	version := f.publish(t)
	response = read()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, fmt.Sprint(version), response.Header().Get("X-App-Model-Policy-Version"))
	var facts struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &facts))
	assert.Len(t, facts.Data, 10)
	assert.Equal(t, true, facts.Data["active"])
	for _, field := range []string{"subject", "session"} {
		alias := body
		if field == "subject" {
			alias.Subject = strings.ToUpper(alias.Subject)
		} else {
			alias.AppSessionID = strings.ToUpper(alias.AppSessionID)
		}
		response = f.call(t, http.MethodPost, "/internal/apps/v1/sessions/introspect", "AppService "+f.credential.Credential, "", alias)
		assert.Equal(t, http.StatusNotFound, response.Code, "policy header requires exact current identity")
		assert.Empty(t, response.Header().Get("X-App-Model-Policy-Version"))
	}
	require.NoError(t, model.DB.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.credential.CredentialID).
		Update("scopes", `["identity.read"]`).Error)
	assert.Empty(t, read().Header().Get("X-App-Model-Policy-Version"))
	require.NoError(t, model.DB.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.credential.CredentialID).
		Update("scopes", `["identity.read","model.invoke"]`).Error)
	require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", f.app.InstallationID).
		Update("status", "disabled").Error)
	response = read()
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Empty(t, response.Header().Get("X-App-Model-Policy-Version"))
	require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", f.app.InstallationID).
		Update("status", "enabled").Error)
	require.NoError(t, model.DB.Model(&model.Option{}).Where(map[string]any{"key": model.AppModelInvokePolicyKey}).Update("value", "invalid").Error)
	response = read()
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Empty(t, response.Header().Get("X-App-Model-Policy-Version"))
}

func TestAppExecutionHTTPTransportAndLimits(t *testing.T) {
	f := setupRouterExecutionTest(t)
	for _, path := range []string{"/internal/apps/v1/execution-grants", "/internal/apps/v1/tasks/lookup",
		"/internal/apps/v1/tasks/cancel", "/internal/apps/v1/imports/ark-task-lookup"} {
		for _, kind := range []string{"no_tls", "duplicate_auth", "content_type", "unknown_fields", "oversized"} {
			t.Run(path+" "+kind, func(t *testing.T) {
				raw, contentType, status := `{}`, "application/json", http.StatusBadRequest
				if kind == "oversized" {
					raw, status = strings.Repeat(" ", 65537), http.StatusRequestEntityTooLarge
				}
				if kind == "unknown_fields" {
					raw = `{"unknown":true,"unknown":false}`
				}
				if kind == "content_type" {
					contentType = "text/plain"
				}
				request := httptest.NewRequest(http.MethodPost, "https://console.example.com"+path, strings.NewReader(raw))
				request.Header.Set("Authorization", "AppService "+f.credential.Credential)
				request.Header.Set("Content-Type", contentType)
				request.Header.Set("Idempotency-Key", "boundary")
				switch kind {
				case "no_tls":
					request.TLS = nil
					request.Header.Set("X-Forwarded-Proto", "https")
					status = http.StatusUnauthorized
				case "duplicate_auth":
					request.Header.Add("Authorization", "AppService "+f.credential.Credential)
					status = http.StatusUnauthorized
				}
				response := httptest.NewRecorder()
				f.engine.ServeHTTP(response, request)
				assert.Equal(t, status, response.Code)
				assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
				assert.Equal(t, "no-referrer", response.Header().Get("Referrer-Policy"))
				assert.NotContains(t, response.Body.String(), f.credential.Credential)
				assert.NotEmpty(t, routerAppPluginErrorCode(t, response))
			})
		}
	}
	// Verify the global limiter reaches the new exact route error boundary.
	previousNum, previousDuration := common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration
	t.Cleanup(func() {
		common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration = previousNum, previousDuration
	})
	common.GlobalApiRateLimitEnable, common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration = true, 1, 60
	limitedEngine := gin.New()
	limitedEngine.Use(middleware.RequestId())
	SetApiRouter(limitedEngine)
	for i, path := range []string{"/internal/apps/v1/execution-grants", "/internal/apps/v1/tasks/lookup",
		"/internal/apps/v1/tasks/cancel", "/internal/apps/v1/imports/ark-task-lookup",
		"/api/app_plugins/http-app/launch-context"} {
		method := http.MethodPost
		if i == 4 {
			method = http.MethodGet
		}
		ip := fmt.Sprintf("192.0.2.%d", i+150)
		routerAppPluginRateLimitRequest(limitedEngine, http.MethodGet, "/api/app_plugins", "", ip)
		response := routerAppPluginRateLimitRequest(limitedEngine, method, path, "", ip)
		assertRouterAppPluginErrorEnvelope(t, response, http.StatusTooManyRequests, "rate_limited", "Too many app plugin requests", true)
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	}
}

func TestAppLaunchContextHTTPAuthority(t *testing.T) {
	f := setupRouterExecutionTest(t)
	for surface, endpoint := range map[string]string{"direct": "/auth/start", "embedded": "/bootstrap/frame"} {
		response := f.call(t, http.MethodGet, "/api/app_plugins/http-app/launch-context?surface="+surface,
			"Bearer "+f.userJWT, "", nil)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var facts struct {
			Data map[string]any `json:"data"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &facts))
		assert.Equal(t, map[string]any{"app_key": "http-app", "surface": surface,
			"start_url": "https://apps.example.com/http-app" + endpoint, "origin": "https://apps.example.com"}, facts.Data)
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		assert.Equal(t, "no-referrer", response.Header().Get("Referrer-Policy"))
	}
	for _, query := range []string{"", "surface=direct&surface=embedded", "surface=direct&origin=https://evil.example", "surface=other"} {
		assert.Equal(t, http.StatusBadRequest, f.call(t, http.MethodGet, "/api/app_plugins/http-app/launch-context?"+query, "Bearer "+f.userJWT, "", nil).Code)
	}
	assert.Equal(t, http.StatusUnauthorized, f.call(t, http.MethodGet, "/api/app_plugins/http-app/launch-context?surface=direct", "Bearer "+f.rootPAT, "", nil).Code)
	require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", f.app.InstallationID).
		Update("allowed_parent_origins", `["https://other.example.com"]`).Error)
	assert.Equal(t, http.StatusForbidden, f.call(t, http.MethodGet, "/api/app_plugins/http-app/launch-context?surface=embedded", "Bearer "+f.userJWT, "", nil).Code)
	require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", f.app.InstallationID).
		Update("status", "disabled").Error)
	assert.Equal(t, http.StatusForbidden, f.call(t, http.MethodGet, "/api/app_plugins/http-app/launch-context?surface=direct", "Bearer "+f.userJWT, "", nil).Code)
	require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", f.app.InstallationID).
		Update("status", "enabled").Error)
	require.NoError(t, model.DB.Model(&model.AppRouteClaim{}).Where("installation_id = ? AND kind = ?", f.app.InstallationID, "direct").
		Update("absolute_endpoint", "https://apps.example.com/unregistered").Error)
	assert.Equal(t, http.StatusNotFound, f.call(t, http.MethodGet, "/api/app_plugins/http-app/launch-context?surface=direct", "Bearer "+f.userJWT, "", nil).Code)
	require.NoError(t, model.DB.Model(&model.UserSession{}).Where("sid = ?", f.session.DashboardSessionID).
		Update("version", 2).Error)
	assert.Equal(t, http.StatusUnauthorized, f.call(t, http.MethodGet, "/api/app_plugins/http-app/launch-context?surface=direct", "Bearer "+f.userJWT, "", nil).Code)
	var count int64
	require.NoError(t, model.DB.Model(&model.AppPluginLaunchCode{}).Count(&count).Error)
	assert.Zero(t, count)
}

func routerAppWorkerExecution(t *testing.T, upstream, expression string, querySource ...string) (model.AppTaskExecution, time.Time) {
	t.Helper()
	f := setupRouterExecutionTest(t)
	if len(querySource) != 0 {
		_, err := jsplugin.DefaultRegistry.RegisterFactory(querySource[0], jsplugin.Options{Key: "doubao"})
		require.NoError(t, err)
	}
	require.NoError(t, model.DB.AutoMigrate(&model.Task{}, &model.SystemTask{}))
	previousQuotaUnit, previousFactory := common.QuotaPerUnit, service.AppTaskAdaptorFactory
	common.QuotaPerUnit = 500000
	service.AppTaskAdaptorFactory = func(key string) service.AppTaskPollingAdaptor {
		adaptor := relay.GetTaskAdaptor(constant.TaskPlatform(key))
		host, _ := adaptor.(service.AppTaskPollingAdaptor)
		return host
	}
	t.Cleanup(func() {
		common.QuotaPerUnit, service.AppTaskAdaptorFactory = previousQuotaUnit, previousFactory
	})
	service.InitHttpClient()
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("name = ?", "http-offline").Update("base_url", upstream).Error)
	if expression != "" {
		body := routerAppPluginJSON(t, map[string]string{"doubao-seedance-2-5-260628": expression})
		require.NoError(t, model.DB.Create(&[]model.Option{
			{Key: "billing_setting.billing_mode", Value: `{"doubao-seedance-2-5-260628":"tiered_expr"}`},
			{Key: "billing_setting.billing_expr", Value: string(body)},
		}).Error)
	}
	request := service.AppExecutionGrantRequest{RequestID: uuid.NewString(), AppKey: f.app.AppKey,
		AppSessionID: f.session.AppSessionID, Subject: f.session.Subject, RunID: uuid.NewString(),
		ExecutionRequestID: uuid.NewString(), Operation: "model.generate", ModelPolicyVersion: f.publish(t),
		RequestedModels: []string{"doubao-seedance-2-5-260628"}, Strategy: "stable", ConversionPolicy: "strict",
		PassthroughSelections: []service.AppPassthroughSelection{}}
	response := f.call(t, http.MethodPost, "/internal/apps/v1/execution-grants", "AppService "+f.credential.Credential, "", request)
	require.Equal(t, http.StatusOK, response.Code, "issue grant; body intentionally omitted")
	var issued struct {
		Data service.AppExecutionGrantResult `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &issued))
	var grant model.AppExecutionGrant
	require.NoError(t, model.DB.Where("grant_id = ?", issued.Data.GrantID).First(&grant).Error)
	var models []model.AppExecutionModel
	require.NoError(t, common.UnmarshalJsonStr(grant.ModelsJSON, &models))
	require.Len(t, models, 1)
	selected := models[0]
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	claim := model.AppTaskExecution{GrantID: grant.GrantID, AppKey: grant.AppKey, InstallationID: grant.InstallationID,
		AppSessionID: grant.AppSessionID, Subject: grant.Subject, UserID: grant.UserID,
		RunID: grant.RunID, ExecutionRequestID: grant.ExecutionRequestID, Operation: grant.Operation,
		PublicModel: selected.PublicModel, ActualModel: selected.ActualModel, ActualGroup: selected.Group, ChannelID: selected.ChannelID,
		PluginKey: selected.PluginKey, PluginVersion: selected.PluginVersion, PluginSHA256: selected.PluginSHA256,
		Protocol: selected.Protocol, FundingSource: grant.FundingSource, ReservedQuota: 5000,
		SubmissionHash:   "worker-fixture-submission",
		RequestFactsJSON: `{"usage":{"tokens":5000},"headers":{},"body":{}}`}
	var execution model.AppTaskExecution
	require.NoError(t, model.RunAppPluginTransaction(model.DB, func(tx *gorm.DB) error {
		var won bool
		var err error
		execution, won, err = model.ClaimAppTaskExecutionTx(tx, grant.GrantID, claim, at)
		if err != nil {
			return err
		}
		require.True(t, won)
		return model.RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-host-worker", "queued", at)
	}))
	require.NoError(t, model.RevokeAppServiceCredential(t.Context(), model.DB, f.app.InstallationID, f.credential.CredentialID, time.Now()))
	require.NoError(t, model.DB.Model(&model.AppPluginSession{}).Where("app_session_id = ?", f.session.AppSessionID).
		Update("revoked_at", time.Now().Unix()).Error)
	require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", f.app.InstallationID).
		Update("status", "revoked").Error)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", grant.UserID).Update("status", common.UserStatusDisabled).Error)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", selected.ChannelID).Update("status", common.ChannelStatusManuallyDisabled).Error)
	require.NoError(t, model.DB.Model(&model.Option{}).Where(map[string]any{"key": "ModelPrice"}).
		Update("value", `{"doubao-seedance-2-5-260628":999}`).Error)
	operation_setting.AppPluginV1Enabled, operation_setting.AppExecutionGrantsEnabled = false, false
	operation_setting.AppPluginSeedanceEnabled, operation_setting.AppPluginEmbeddedSurfaceEnabled = false, false
	require.NoError(t, model.DB.First(&execution, execution.ID).Error)
	return execution, at.Add(2 * time.Hour)
}

const appWorkerSuccessResponse = `{"id":"cgt-host-worker","status":"succeeded","model":"doubao-seedance-2-5-260628","content":{"video_url":"https://cdn.example.com/video.mp4"},"usage":{"completion_tokens":108000,"total_tokens":108000}}`

type appWorkerDefaultBaseObserver struct {
	service.AppTaskPollingAdaptor
	t        *testing.T
	localURL string
}

func (a *appWorkerDefaultBaseObserver) FetchTaskWithContext(ctx context.Context, base, key string, task *model.Task, proxy string) (*http.Response, error) {
	assert.Equal(a.t, constant.GetChannelBaseURL(constant.ChannelTypeDoubaoVideo), base, "worker uses the channel default URL")
	return a.AppTaskPollingAdaptor.FetchTaskWithContext(ctx, a.localURL, key, task, proxy)
}

func TestHostAppTaskWorkerUsesFrozenFacts(t *testing.T) {
	for _, test := range []struct {
		name       string
		expression string
		finalQuota int
		overdrawn  bool
		failed     bool
		defaultURL bool
	}{
		{name: "per call", finalQuota: 5000},
		{name: "task usage and frozen time", expression: `tier("base",u("tokens")*0.000002)*(hour("UTC")==10?1:2)`, finalQuota: 108000},
		{name: "negative wallet zero delta", finalQuota: 5000, overdrawn: true},
		{name: "negative wallet refund", finalQuota: 0, overdrawn: true, failed: true},
		{name: "channel default URL", finalQuota: 5000, defaultURL: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var queries, writes atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
					http.Error(w, "unexpected provider write", http.StatusBadRequest)
					return
				}
				queries.Add(1)
				assert.Equal(t, "/api/v3/contents/generations/tasks/cgt-host-worker", r.URL.Path)
				assert.True(t, r.Header.Get("Authorization") == "Bearer offline-key", "Host credential must be used")
				w.Header().Set("Content-Type", "application/json")
				if test.failed {
					_, _ = io.WriteString(w, `{"id":"cgt-host-worker","status":"failed","model":"doubao-seedance-2-5-260628","error":{"message":"upstream failed"}}`)
					return
				}
				_, _ = io.WriteString(w, appWorkerSuccessResponse)
			}))
			t.Cleanup(upstream.Close)
			execution, now := routerAppWorkerExecution(t, upstream.URL, test.expression)
			expectedBalance := 1000000 - test.finalQuota
			if test.overdrawn {
				require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", execution.UserID).Update("quota", -10).Error)
				expectedBalance = -10 + 5000 - test.finalQuota
			}
			if test.defaultURL {
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", execution.ChannelID).Update("base_url", "").Error)
				factory := service.AppTaskAdaptorFactory
				service.AppTaskAdaptorFactory = func(key string) service.AppTaskPollingAdaptor {
					return &appWorkerDefaultBaseObserver{AppTaskPollingAdaptor: factory(key), t: t, localURL: upstream.URL}
				}
			}
			processed, err := service.RunAppTaskReconcileOnce(t.Context(), model.DB, now)
			require.NoError(t, err)
			require.True(t, processed)
			assert.Equal(t, int32(1), queries.Load())
			assert.Zero(t, writes.Load())
			var payer model.User
			require.NoError(t, model.DB.First(&payer, execution.UserID).Error)
			assert.Equal(t, expectedBalance, payer.Quota)
			assert.Equal(t, test.finalQuota, payer.UsedQuota)
			assert.Equal(t, 1, payer.RequestCount)
			var saved model.AppTaskExecution
			require.NoError(t, model.DB.First(&saved, execution.ID).Error)
			expectedState := "succeeded"
			if test.failed {
				expectedState = "failed"
			}
			assert.Equal(t, expectedState, saved.ProviderState)
			assert.Equal(t, "settled", saved.BillingState)
			assert.NotContains(t, saved.ArtifactsJSON, "cdn.example.com")
			var projection model.Task
			require.NoError(t, model.DB.Where("task_id = ?", execution.TaskID).First(&projection).Error)
			if test.failed {
				assert.Equal(t, "[]", saved.ArtifactsJSON)
				assert.Empty(t, projection.PrivateData.AppArtifactURLs)
			} else {
				assert.Contains(t, saved.UsageJSON, `"tokens":108000`)
				assert.Contains(t, saved.ArtifactsJSON, `"key":"video"`)
				assert.Equal(t, "https://cdn.example.com/video.mp4", projection.PrivateData.AppArtifactURLs["video"])
			}
			processed, err = service.RunAppTaskReconcileOnce(t.Context(), model.DB, now.Add(time.Minute))
			require.NoError(t, err)
			assert.False(t, processed)
			assert.Equal(t, int32(1), queries.Load())
			for _, table := range []any{&model.AppTaskSettlement{}, &model.AppTaskOutbox{}} {
				var count int64
				require.NoError(t, model.DB.Model(table).Count(&count).Error)
				assert.EqualValues(t, 1, count)
			}
		})
	}
}

func TestHostAppTaskWorkerPreservesUncertainQueries(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
	}{
		{"not found", http.StatusNotFound, `{"error":"gone"}`},
		{"unavailable", http.StatusServiceUnavailable, `{"error":"temporary"}`},
		{"invalid JSON", http.StatusOK, `{"status":`},
		{"other task", http.StatusOK, strings.Replace(appWorkerSuccessResponse, "cgt-host-worker", "cgt-other", 1)},
		{"other model", http.StatusOK, strings.Replace(appWorkerSuccessResponse, "doubao-seedance-2-5-260628", "other", 1)},
		{"source changed", http.StatusOK, appWorkerSuccessResponse},
		{"redirect", http.StatusFound, ""},
		{"write descriptor", http.StatusOK, appWorkerSuccessResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			var queries atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queries.Add(1)
				assert.Equal(t, http.MethodGet, r.Method)
				w.Header().Set("Content-Type", "application/json")
				if test.name == "redirect" {
					w.Header().Set("Location", "/another-task")
				}
				w.WriteHeader(test.code)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(upstream.Close)
			var sourceOverride []string
			if test.name == "write descriptor" {
				source, err := os.ReadFile("../plugins/tasks/doubao/plugin.js")
				require.NoError(t, err)
				const query = "url: ctx.baseUrl + \"/api/v3/contents/generations/tasks/\" + ctx.taskId,\n    method: \"GET\","
				require.Equal(t, 1, strings.Count(string(source), query))
				sourceOverride = []string{strings.Replace(string(source), query, strings.Replace(query, `"GET"`, `"POST"`, 1), 1)}
			}
			execution, now := routerAppWorkerExecution(t, upstream.URL, "", sourceOverride...)
			if test.name == "source changed" {
				source, err := os.ReadFile("../plugins/tasks/doubao/plugin.js")
				require.NoError(t, err)
				_, err = jsplugin.DefaultRegistry.RegisterFactory(string(source)+"\n// different source\n", jsplugin.Options{Key: "doubao"})
				require.NoError(t, err)
			}
			_, err := service.RunAppTaskReconcileOnce(t.Context(), model.DB, now)
			require.Error(t, err)
			var saved model.AppTaskExecution
			require.NoError(t, model.DB.First(&saved, execution.ID).Error)
			assert.Equal(t, "queued", saved.ProviderState)
			assert.Equal(t, "reserved", saved.BillingState)
			var payer model.User
			require.NoError(t, model.DB.First(&payer, execution.UserID).Error)
			assert.Equal(t, 995000, payer.Quota)
			for _, table := range []any{&model.AppTaskSettlement{}, &model.AppTaskOutbox{}} {
				var count int64
				require.NoError(t, model.DB.Model(table).Count(&count).Error)
				assert.Zero(t, count)
			}
			var lease model.AppTaskReconcile
			require.NoError(t, model.DB.First(&lease, "execution_id = ?", execution.ID).Error)
			assert.NotEmpty(t, lease.LastObservationError)
			assert.Greater(t, lease.NextAttemptAt, now.Unix())
			if test.name == "source changed" || test.name == "write descriptor" {
				assert.Zero(t, queries.Load())
			} else {
				assert.Equal(t, int32(1), queries.Load())
			}
		})
	}
	t.Run("cancellation reaches the provider query", func(t *testing.T) {
		arrived, release := make(chan struct{}), make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(arrived)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(upstream.Close)
		t.Cleanup(func() { close(release) })
		_, now := routerAppWorkerExecution(t, upstream.URL, "")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := service.RunAppTaskReconcileOnce(ctx, model.DB, now)
			done <- err
		}()
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("provider query did not start")
		}
		cancel()
		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("provider query ignored cancellation")
		}
	})
	t.Run("nonterminal observations reschedule without settling", func(t *testing.T) {
		var queries atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if queries.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"id":"cgt-host-worker","status":"running"}`)
				return
			}
			_, _ = io.WriteString(w, appWorkerSuccessResponse)
		}))
		t.Cleanup(upstream.Close)
		execution, now := routerAppWorkerExecution(t, upstream.URL, "")
		worked, err := service.RunAppTaskReconcileOnce(t.Context(), model.DB, now)
		require.NoError(t, err)
		require.True(t, worked)
		var saved model.AppTaskExecution
		require.NoError(t, model.DB.First(&saved, execution.ID).Error)
		assert.Equal(t, "running", saved.ProviderState)
		assert.Equal(t, "reserved", saved.BillingState)
		assert.True(t, model.HasPendingAppTaskReconciliation())
		worked, err = service.RunAppTaskReconcileOnce(t.Context(), model.DB, now.Add(time.Second))
		require.NoError(t, err)
		assert.False(t, worked)
		assert.Equal(t, int32(1), queries.Load())
		worked, err = service.RunAppTaskReconcileOnce(t.Context(), model.DB, now.Add(time.Minute))
		require.NoError(t, err)
		assert.True(t, worked)
		assert.Equal(t, int32(2), queries.Load())
		assert.False(t, model.HasPendingAppTaskReconciliation())
		require.NoError(t, model.DB.First(&saved, execution.ID).Error)
		assert.Equal(t, "settled", saved.BillingState)
	})
}

func TestHostAppTaskLegacyBoundaries(t *testing.T) {
	for _, entry := range []string{"poll lists", "refund", "adjustment", "poll request"} {
		t.Run(entry, func(t *testing.T) {
			var queries atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queries.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, appWorkerSuccessResponse)
			}))
			t.Cleanup(upstream.Close)
			execution, now := routerAppWorkerExecution(t, upstream.URL, "")
			var projection model.Task
			require.NoError(t, model.DB.Where("task_id = ?", execution.TaskID).First(&projection).Error)
			before := projection
			switch entry {
			case "poll lists":
				assert.Empty(t, model.GetAllUnFinishSyncTasks(10), "legacy poller must exclude App tasks")
				assert.Empty(t, model.GetTimedOutUnfinishedTasks(now.Unix(), 10), "legacy timeout sweep must exclude App tasks")
				assert.False(t, model.HasUnfinishedSyncTasks())
			case "refund":
				assert.False(t, service.RefundTaskQuota(t.Context(), &projection, "legacy"),
					"legacy refund must refuse App tasks")
			case "adjustment":
				service.RecalculateTaskQuota(t.Context(), &projection, 6000, "legacy")
			case "poll request":
				oldFactory := service.GetTaskAdaptorFunc
				service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
					return relay.GetTaskAdaptor(platform)
				}
				t.Cleanup(func() { service.GetTaskAdaptorFunc = oldFactory })
				require.NoError(t, service.UpdateVideoTasks(t.Context(), constant.TaskPlatform("doubao"),
					map[int][]string{execution.ChannelID: {execution.ProviderTaskID}},
					map[string]*model.Task{execution.ProviderTaskID: &projection}))
			}
			assert.Zero(t, queries.Load(), "legacy polling must not query App tasks")
			var payer model.User
			require.NoError(t, model.DB.First(&payer, execution.UserID).Error)
			assert.Equal(t, 995000, payer.Quota, "legacy entry must leave the App reservation unchanged")
			assert.Zero(t, payer.UsedQuota)
			var after model.Task
			require.NoError(t, model.DB.First(&after, projection.ID).Error)
			assert.Equal(t, before, after, "only the Host worker may project App task observations")
		})
	}
}
