package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAuthorizeReturnsRegisteredOneTimeLaunchURL(t *testing.T) {
	setupAppPluginControllerTest(t)
	app, identity := setupAppPluginLaunchController(t)
	require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", app.InstallationID).
		Update("status", "enabled").Error)
	verifier := strings.Repeat("v", 43)
	challenge := sha256.Sum256([]byte(verifier))
	request := map[string]any{
		"surface": "direct", "transaction_id": "controller-transaction",
		"state":          base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)),
		"nonce":          base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)),
		"code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]), "code_challenge_method": "S256",
	}
	call := func(body []byte, key, origin string, session bool) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "https://console.example.com/api/app_plugins/"+app.AppKey+"/authorize", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("Origin", origin)
		c.Request.Header.Set("Idempotency-Key", key)
		c.Params = gin.Params{{Key: "key", Value: app.AppKey}}
		c.Set("id", identity.UserID)
		c.Set("role", common.RoleRootUser)
		c.Set("group", "default")
		if session {
			c.Set("session_id", identity.SessionID)
			c.Set("auth_version", identity.UserAuthVersion)
			c.Set("session_version", identity.SessionVersion)
		}
		AuthorizeAppPlugin(c)
		return recorder
	}
	first := call(appPluginJSONBody(t, request), "launch-once", "https://console.example.com", true)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Equal(t, "no-store", first.Header().Get("Cache-Control"))
	var response struct {
		Success bool                             `json:"success"`
		Data    service.AppPluginAuthorizeResult `json:"data"`
	}
	require.NoError(t, common.Unmarshal(first.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, 60, response.Data.ExpiresIn)
	assert.Equal(t, "direct", response.Data.Surface)
	callback, err := url.Parse(response.Data.LaunchURL)
	require.NoError(t, err)
	assert.Equal(t, "https", callback.Scheme)
	assert.Equal(t, "apps.example.com", callback.Host)
	assert.Equal(t, "/launch-controller/auth/callback", callback.Path)
	assert.Empty(t, callback.Fragment)
	assert.Nil(t, callback.User)
	assert.Equal(t, request["state"], callback.Query().Get("state"))
	require.NotEmpty(t, callback.Query().Get("code"))
	assert.Empty(t, callback.Query().Get("code_verifier"))
	assert.Empty(t, callback.Query().Get("credential"))
	again := call(appPluginJSONBody(t, request), "launch-once", "https://console.example.com", true)
	require.Equal(t, http.StatusOK, again.Code)
	assert.JSONEq(t, first.Body.String(), again.Body.String())
	t.Run("missing session cannot authorize with a PAT principal", func(t *testing.T) {
		response := call(appPluginJSONBody(t, request), "pat", "https://console.example.com", false)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.Equal(t, "unauthenticated", appPluginErrorCode(t, response))
	})
	t.Run("exact request fields", func(t *testing.T) {
		for field := range request {
			changed := make(map[string]any, len(request))
			for key, value := range request {
				if key != field {
					changed[key] = value
				}
			}
			response := call(appPluginJSONBody(t, changed), "missing-"+field, "https://console.example.com", true)
			assert.Equal(t, http.StatusBadRequest, response.Code, field)
		}
		for _, field := range []string{"redirect_uri", "callback", "user_id", "entitlements", "extra"} {
			changed := make(map[string]any, len(request)+1)
			for key, value := range request {
				changed[key] = value
			}
			changed[field] = "https://evil.example/callback"
			response := call(appPluginJSONBody(t, changed), "unknown-"+field, "https://console.example.com", true)
			assert.Equal(t, http.StatusBadRequest, response.Code, field)
			assert.NotContains(t, response.Body.String(), "https://evil.example")
		}
		body := appPluginJSONBody(t, request)
		duplicate := append([]byte(`{"surface":"embedded",`), body[1:]...)
		assert.Equal(t, http.StatusBadRequest, call(duplicate, "duplicate", "https://console.example.com", true).Code)
	})
	t.Run("origin and idempotency are mandatory", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, call(appPluginJSONBody(t, request), "", "https://console.example.com", true).Code)
		for _, origin := range []string{"", "null", "https://evil.example.com", "http://console.example.com"} {
			assert.Equal(t, http.StatusForbidden, call(appPluginJSONBody(t, request), "origin", origin, true).Code)
		}
	})
	t.Run("database session wins over authenticated context", func(t *testing.T) {
		require.NoError(t, model.DB.Model(&model.UserSession{}).Where("sid = ?", identity.SessionID).Update("version", 2).Error)
		response := call(appPluginJSONBody(t, request), "stale-context", "https://console.example.com", true)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
	})
}

func TestAppEnableRequiresAllPrerequisites(t *testing.T) {
	t.Run("root credential ceremony and redaction", appPluginCredentialControllerRegression)
	for _, missing := range []string{"credential", "callback", "surface", "parent", "same-site", "network", "scope", "none"} {
		t.Run(missing, func(t *testing.T) {
			setupAppPluginControllerTest(t)
			app, _ := setupAppPluginLaunchController(t)
			previousProbe := appPluginEnableNetworkProbe
			t.Cleanup(func() { appPluginEnableNetworkProbe = previousProbe })
			appPluginEnableNetworkProbe = func(_ context.Context, installation model.AppInstallation) error {
				assert.Equal(t, app.InstallationID, installation.InstallationID)
				assert.Equal(t, app.Revision, installation.Revision)
				if missing == "network" {
					return errors.New("network probe denied")
				}
				return nil
			}
			switch missing {
			case "credential":
				require.NoError(t, model.DB.Where("installation_id = ?", app.InstallationID).Delete(&model.AppServiceCredential{}).Error)
			case "callback":
				require.NoError(t, model.DB.Where("installation_id = ? AND kind = ?", app.InstallationID, "callback").Delete(&model.AppRouteClaim{}).Error)
			case "surface":
				require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", app.InstallationID).
					Update("enabled_surfaces", model.AppStringList{}).Error)
			case "parent":
				require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", app.InstallationID).
					Update("allowed_parent_origins", model.AppStringList{}).Error)
			case "same-site":
				require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", app.InstallationID).
					Update("allowed_parent_origins", model.AppStringList{"https://console.other.test"}).Error)
			case "scope":
				var generation model.AppVersion
				require.NoError(t, model.DB.Where("app_key = ? AND manifest_version = ?", app.AppKey, app.ManifestVersion).First(&generation).Error)
				var manifest service.AppManifest
				require.NoError(t, common.Unmarshal([]byte(generation.CanonicalManifestJSON), &manifest))
				manifest.RequestedScopes = append(manifest.RequestedScopes, "model.invoke")
				canonical := appPluginJSONBody(t, manifest)
				digest := sha256.Sum256(canonical)
				require.NoError(t, model.DB.Model(&generation).Updates(map[string]any{
					"canonical_manifest_json": string(canonical), "manifest_sha256": fmt.Sprintf("%x", digest),
				}).Error)
				require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", app.InstallationID).
					Update("manifest_sha256", fmt.Sprintf("%x", digest)).Error)
			}
			response := appPluginControllerRequest(t, PatchAppPluginInstallation, http.MethodPatch,
				"/api/app_plugins/installations", appPluginPatchBody(t, app.InstallationID, app.Revision, map[string]any{"status": "enabled"}),
				common.RolePluginAdminUser, "default", nil)
			var current model.AppInstallation
			require.NoError(t, model.DB.Where("installation_id = ?", app.InstallationID).First(&current).Error)
			if missing != "none" {
				assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
				assert.Equal(t, "app_enable_prerequisite_missing", appPluginErrorCode(t, response))
				assert.Equal(t, "disabled", current.Status)
				assert.Equal(t, app.Revision, current.Revision)
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, "enabled", current.Status)
			assert.Equal(t, app.Revision+1, current.Revision)
			stale := appPluginControllerRequest(t, PatchAppPluginInstallation, http.MethodPatch,
				"/api/app_plugins/installations", appPluginPatchBody(t, app.InstallationID, app.Revision, map[string]any{"status": "enabled"}),
				common.RolePluginAdminUser, "default", nil)
			assert.Equal(t, http.StatusConflict, stale.Code)
			assert.Equal(t, "version_conflict", appPluginErrorCode(t, stale))
			appPluginEnableNetworkProbe = func(context.Context, model.AppInstallation) error {
				t.Error("disable must not probe the network")
				return errors.New("unavailable")
			}
			disabled := appPluginControllerRequest(t, PatchAppPluginInstallation, http.MethodPatch,
				"/api/app_plugins/installations", appPluginPatchBody(t, app.InstallationID, current.Revision, map[string]any{"status": "disabled"}),
				common.RolePluginAdminUser, "default", nil)
			assert.Equal(t, http.StatusOK, disabled.Code, disabled.Body.String())
		})
	}
	t.Run("credentials are root-only and plaintext is response-only", func(t *testing.T) {
		setupAppPluginControllerTest(t)
		app, identity := setupAppPluginLaunchController(t)
		require.NoError(t, model.DB.Where("installation_id = ?", app.InstallationID).Delete(&model.AppServiceCredential{}).Error)
		for _, handler := range []gin.HandlerFunc{CreateAppPluginServiceCredential, RotateAppPluginServiceCredential, RevokeAppPluginServiceCredential} {
			for _, role := range []int{common.RoleCommonUser, common.RolePluginAdminUser, common.RoleAdminUser} {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "https://console.example.com/api/app_plugins/installations/"+app.InstallationID+"/service-credentials", bytes.NewBufferString(`{}`))
				c.Request.Header.Set("Origin", "https://console.example.com")
				c.Request.Header.Set("Content-Type", "application/json")
				c.Params = gin.Params{{Key: "id", Value: app.InstallationID}, {Key: "credential_id", Value: "unknown"}}
				c.Set("id", identity.UserID)
				c.Set("role", role)
				c.Set("session_id", identity.SessionID)
				c.Set("auth_version", identity.UserAuthVersion)
				c.Set("session_version", identity.SessionVersion)
				handler(c)
				assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
			}
		}
	})
}

func appPluginCredentialControllerRegression(t *testing.T) {
	setupAppPluginControllerTest(t)
	app, identity := setupAppPluginLaunchController(t)
	require.NoError(t, model.DB.AutoMigrate(&model.AuthFlow{}, &model.TwoFA{}, &model.PasskeyCredential{}))
	require.NoError(t, model.DB.Where("installation_id = ?", app.InstallationID).Delete(&model.AppServiceCredential{}).Error)
	previousEncryption := common.PasswordLoginEncryptionEnabled
	common.PasswordLoginEncryptionEnabled = false
	t.Cleanup(func() { common.PasswordLoginEncryptionEnabled = previousEncryption })
	password, err := common.Password2Hash("b15-test-only-password")
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", identity.UserID).Update("password", password).Error)
	require.NoError(t, model.PublishUserAuthCache(identity.UserID))
	conn, err := model.DB.DB()
	require.NoError(t, err)
	if model.DB.Dialector.Name() == "sqlite" {
		conn.SetMaxOpenConns(1)
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("id", identity.UserID)
		c.Set("role", common.RoleRootUser)
		c.Set("session_id", identity.SessionID)
		c.Set("auth_version", identity.UserAuthVersion)
		c.Set("session_version", identity.SessionVersion)
		c.Set("group", "default")
		c.Next()
	})
	const base = "/api/app_plugins/installations"
	router.POST(base+"/:id/service-credentials", middleware.AppPluginOperationAudit(), CreateAppPluginServiceCredential)
	router.POST(base+"/:id/service-credentials/rotate", middleware.AppPluginOperationAudit(), RotateAppPluginServiceCredential)
	router.DELETE(base+"/:id/service-credentials/:credential_id", middleware.AppPluginOperationAudit(), RevokeAppPluginServiceCredential)
	router.GET(base, ListAppPluginInstallations)
	payload := appPluginJSONBody(t, map[string]any{"scopes": []string{"identity.read", "task.read"}, "expires_at": time.Now().Add(time.Hour).Unix()})
	call := func(method, path, proof string) *httptest.ResponseRecorder {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequest(method, "https://console.example.com"+path, bytes.NewReader(payload)).WithContext(ctx)
		req.Header.Set("Origin", "https://console.example.com")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Security-Proof", proof)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	operation, err := service.AppPluginCredentialOperation(app.InstallationID, "create")
	require.NoError(t, err)
	proof, err := service.VerifySecurityInput(identity, service.VerificationInput{
		Method: "password", Scope: operation.Scope, Context: operation.Context, Password: "b15-test-only-password",
	})
	require.NoError(t, err, "reuse the existing reauthentication ceremony")
	path := base + "/" + app.InstallationID + "/service-credentials"
	for _, target := range []string{path + "/rotate", base + "/another-installation/service-credentials"} {
		response := call(http.MethodPost, target, proof.ProofToken)
		assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		assert.Equal(t, "approval_required", appPluginErrorCode(t, response))
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	}
	for _, unrelated := range []service.VerificationOperation{
		{Scope: service.VerificationScopeAccessTokenGenerate},
		{Scope: service.VerificationScopeChannelKeyRead, Context: []byte(`{"channel_id":1}`)},
	} {
		unrelatedProof := issueSecurityEnrollmentProof(t, identity, unrelated, "password")
		response := call(http.MethodPost, path, unrelatedProof)
		assert.Equal(t, http.StatusForbidden, response.Code)
	}
	first := call(http.MethodPost, path, proof.ProofToken)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	assert.Equal(t, "no-store", first.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", first.Header().Get("Referrer-Policy"))
	var created struct {
		Success bool                             `json:"success"`
		Data    model.AppServiceCredentialIssued `json:"data"`
	}
	require.NoError(t, common.Unmarshal(first.Body.Bytes(), &created))
	require.True(t, created.Success)
	require.Len(t, created.Data.Credential, 43)
	assert.Equal(t, http.StatusForbidden, call(http.MethodPost, path, proof.ProofToken).Code, "consumed proof is not a retrieval API")
	rotate, err := service.AppPluginCredentialOperation(app.InstallationID, "rotate")
	require.NoError(t, err)
	rotationProof := issueSecurityEnrollmentProof(t, identity, rotate, "password")
	rotatedResponse := call(http.MethodPost, path+"/rotate", rotationProof)
	require.Equal(t, http.StatusCreated, rotatedResponse.Code, rotatedResponse.Body.String())
	var rotated struct {
		Data model.AppServiceCredentialIssued `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rotatedResponse.Body.Bytes(), &rotated))
	assert.NotEqual(t, created.Data.Credential, rotated.Data.Credential)
	assert.NotEqual(t, created.Data.Version, rotated.Data.Version)
	assert.WithinDuration(t, time.Now().Add(10*time.Minute), rotated.Data.OverlapUntil, 5*time.Second)
	listed := call(http.MethodGet, base, "")
	require.Equal(t, http.StatusOK, listed.Code)
	var credentials []model.AppServiceCredential
	require.NoError(t, model.DB.Find(&credentials).Error)
	require.Len(t, credentials, 2)
	var audits []model.AuditLog
	require.NoError(t, model.DB.Find(&audits).Error)
	require.NotEmpty(t, audits)
	auditJSON, err := common.Marshal(audits)
	require.NoError(t, err)
	for _, secret := range []string{created.Data.Credential, rotated.Data.Credential, proof.ProofToken, rotationProof,
		credentials[0].CredentialHash, credentials[1].CredentialHash} {
		assert.NotContains(t, listed.Body.String(), secret)
		assert.NotContains(t, string(auditJSON), secret)
	}
	staleProof := issueSecurityEnrollmentProof(t, identity, rotate, "password")
	require.NoError(t, model.DB.Model(&model.UserSession{}).Where("sid = ?", identity.SessionID).Update("version", 2).Error)
	stale := call(http.MethodPost, path+"/rotate", staleProof)
	assert.Equal(t, http.StatusForbidden, stale.Code, stale.Body.String())
	require.NoError(t, model.DB.Model(&model.UserSession{}).Where("sid = ?", identity.SessionID).Update("version", 1).Error)
	operation_setting.AppPluginV1Enabled = false
	revoked := call(http.MethodDelete, path+"/"+rotated.Data.CredentialID, "")
	require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())
	_, err = model.AuthenticateAppServiceCredential(t.Context(), model.DB, rotated.Data.Credential, time.Now())
	require.Error(t, err, "emergency revoke remains available when the feature is disabled")
}

func setupAppPluginLaunchController(t *testing.T) (model.AppInstallResult, service.AuthIdentity) {
	t.Helper()
	t.Setenv("CRYPTO_SECRET", "test-only-persistent-shared-launch-secret")
	previousSecret, previousAddress := common.CryptoSecret, system_setting.ServerAddress
	common.CryptoSecret, system_setting.ServerAddress = os.Getenv("CRYPTO_SECRET"), "https://console.example.com"
	t.Cleanup(func() {
		common.CryptoSecret, system_setting.ServerAddress = previousSecret, previousAddress
	})
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.UserSession{}))
	require.NoError(t, model.MigrateAppPluginLaunchTables(model.DB))
	user := model.User{Id: 1001, Username: "launch-controller", Password: "unusable-password",
		Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "launch-controller"}
	require.NoError(t, model.DB.Create(&user).Error)
	session := model.UserSession{SID: "controller-dashboard-session", UserID: user.Id, Version: 1, UserAuthVersion: 1,
		Status: model.UserSessionStatusActive, ExpiresAt: time.Now().Add(time.Hour).Unix(),
		CreatedAt: time.Now().Unix(), LastActiveAt: time.Now().Unix(), RefreshHash: "unusable-refresh-hash"}
	require.NoError(t, model.DB.Create(&session).Error)
	app := createAppPluginForControllerTest(t, "launch-controller", "Launch Controller", "launch-controller-install")
	require.NoError(t, model.DB.Model(&model.AppInstallation{}).Where("installation_id = ?", app.InstallationID).
		Update("network_policy", model.AppNetworkPolicy{AllowHosts: []string{"apps.example.com"}, DenyPrivateIPRanges: true}).Error)
	_, err := model.IssueAppServiceCredential(t.Context(), model.DB, app.InstallationID,
		[]string{"identity.read", "task.read"}, time.Now(), time.Now().Add(time.Hour), false)
	require.NoError(t, err)
	return app, service.AuthIdentity{UserID: user.Id, SessionID: session.SID, UserAuthVersion: 1, SessionVersion: 1}
}

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
	t.Run("atomic mutations", func(t *testing.T) {
		t.Run("unchanged normalized base URL", appPluginUnchangedRouteRegression)
		t.Run("claim uniqueness race rolls back", appPluginRouteCollisionRegression)
		t.Run("unrelated uniqueness errors stay internal", func(t *testing.T) {
			setupAppPluginControllerTest(t)
			if model.DB.Dialector.Name() == "sqlite" {
				t.Skip("driver-specific uniqueness error classification")
			}
			app := createAppPluginForControllerTest(t, "other-unique", "Other Unique", "other-unique")
			const callback = "test:b14_unrelated_unique"
			require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table != "app_route_claims" {
					return
				}
				if tx.Dialector.Name() == "mysql" {
					tx.AddError(&mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry for key 'PRIMARY'"})
				} else {
					tx.AddError(&pgconn.PgError{Code: "23505", ConstraintName: "app_route_claims_pkey"})
				}
			}))
			t.Cleanup(func() { require.NoError(t, model.DB.Callback().Update().Remove(callback)) })
			response := appPluginControllerRequest(t, PatchAppPluginInstallation, http.MethodPatch,
				"/api/app_plugins/installations", appPluginPatchBody(t, app.InstallationID, app.Revision,
					map[string]any{"base_url": "https://apps.example.com/other-unique-changed/"}),
				common.RoleRootUser, "default", nil)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.Equal(t, "service_unavailable", appPluginErrorCode(t, response))
			var current model.AppInstallation
			require.NoError(t, model.DB.Where("installation_id = ?", app.InstallationID).First(&current).Error)
			assert.Equal(t, app.Revision, current.Revision)
			assert.Equal(t, app.BaseURL, current.BaseURL)
		})
		t.Run("concurrent draft frozen replay", appPluginConcurrentDraftRegression)
		t.Run("real deadlock retries outer transaction", func(t *testing.T) {
			appPluginDeadlockRegression(t, false, false)
		})
		t.Run("real entitlement deadlock then failure rolls back", func(t *testing.T) {
			appPluginDeadlockRegression(t, true, false)
		})
		t.Run("existing transaction propagates real deadlock", func(t *testing.T) {
			appPluginDeadlockRegression(t, true, true)
		})
		t.Run("policy selection and reference contracts", appPluginPolicySelectionRegression)
		t.Run("PATCH serializes with upgrade", func(t *testing.T) {
			appPluginPatchUpgradeRegression(t, false)
		})
		t.Run("PATCH uses current generation after upgrade", func(t *testing.T) {
			appPluginPatchUpgradeRegression(t, true)
		})
	})
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
	requestCtx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequestWithContext(requestCtx, method, path, bytes.NewReader(body))
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

func appPluginUnchangedRouteRegression(t *testing.T) {
	setupAppPluginControllerTest(t)
	app := createAppPluginForControllerTest(t, "same-base", "Same Base", "same-base")
	var before []model.AppRouteClaim
	require.NoError(t, model.DB.Order("id").Find(&before).Error)
	response := appPluginControllerRequest(t, PatchAppPluginInstallation, http.MethodPatch,
		"/api/app_plugins/installations", appPluginPatchBody(t, app.InstallationID, app.Revision, map[string]any{
			"base_url":        "HTTPS://APPS.EXAMPLE.COM:443/same-base",
			"allowed_origins": []string{"https://changed.example.com"},
		}), common.RoleRootUser, "default", nil)
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var stored model.AppInstallation
	var after []model.AppRouteClaim
	require.NoError(t, model.DB.Where("installation_id = ?", app.InstallationID).First(&stored).Error)
	require.NoError(t, model.DB.Order("id").Find(&after).Error)
	assert.Equal(t, app.Revision+1, stored.Revision)
	assert.Equal(t, model.AppStringList{"https://changed.example.com"}, stored.AllowedOrigins)
	assert.Equal(t, before, after, "unchanged claims must not be replaced")
	require.NoError(t, model.DB.Where("installation_id = ? AND kind = ?", app.InstallationID, "embedded").
		Delete(&model.AppRouteClaim{}).Error)
	repaired := appPluginControllerRequest(t, PatchAppPluginInstallation, http.MethodPatch,
		"/api/app_plugins/installations", appPluginPatchBody(t, app.InstallationID, stored.Revision,
			map[string]any{"base_url": app.BaseURL}), common.RoleRootUser, "default", nil)
	assert.Equal(t, http.StatusOK, repaired.Code, repaired.Body.String())
	var repairedResult appPluginTestEnvelope
	require.NoError(t, common.Unmarshal(repaired.Body.Bytes(), &repairedResult))
	assert.Equal(t, app.Revision+2, repairedResult.Data.Revision)
	assert.Equal(t, [6]int64{1, 1, 1, 4, 0, 0}, appPluginControllerPersistenceCounts(t))
}

func appPluginRouteCollisionRegression(t *testing.T) {
	setupAppPluginControllerTest(t)
	if model.DB.Dialector.Name() == "sqlite" {
		t.Skip("SQLite serializes writers")
	}
	left := createAppPluginForControllerTest(t, "route-left", "Route Left", "route-left")
	right := createAppPluginForControllerTest(t, "route-right", "Route Right", "route-right")
	var rightRow model.AppInstallation
	require.NoError(t, model.DB.Where("installation_id = ?", right.InstallationID).First(&rightRow).Error)
	var before []model.AppRouteClaim
	require.NoError(t, model.DB.Where("installation_id = ?", left.InstallationID).Order("id").Find(&before).Error)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	const target = "https://apps.example.com/route-target/"
	var injected atomic.Bool
	var competitorErr, updateErr error
	const callback = "test:b14_claim_uniqueness_race"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "app_route_claims" || !injected.CompareAndSwap(false, true) {
			return
		}
		competitorErr = model.DB.WithContext(ctx).Transaction(func(other *gorm.DB) error {
			if err := updateAppPluginRouteClaims(other, rightRow, target); err != nil {
				return err
			}
			return other.Model(&model.AppInstallation{}).Where("installation_id = ?", right.InstallationID).
				Updates(map[string]any{"base_url": target, "revision": gorm.Expr("revision + 1")}).Error
		})
	}))
	require.NoError(t, model.DB.Callback().Update().After("gorm:update").Register(callback+":result", func(tx *gorm.DB) {
		if tx.Statement.Table == "app_route_claims" && tx.Error != nil {
			updateErr = tx.Error
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Update().Remove(callback))
		require.NoError(t, model.DB.Callback().Update().Remove(callback+":result"))
	})
	response := appPluginControllerRequest(t, PatchAppPluginInstallation, http.MethodPatch,
		"/api/app_plugins/installations", appPluginPatchBody(t, left.InstallationID, left.Revision,
			map[string]any{"base_url": target, "entitlement_policy": map[string]any{
				"key": "losing-policy", "rules": map[string][]string{"app_plugin": {"manage"}},
			}}), common.RoleRootUser, "default", nil)
	require.True(t, injected.Load())
	require.NoError(t, competitorErr)
	require.Error(t, updateErr, "exercise the actual database unique constraint")
	marker := "1062"
	if model.DB.Dialector.Name() == "postgres" {
		marker = "23505"
	}
	require.Contains(t, updateErr.Error(), marker)
	t.Logf("actual uniqueness error: %v", updateErr)
	var stored model.AppInstallation
	var after []model.AppRouteClaim
	require.NoError(t, model.DB.Where("installation_id = ?", left.InstallationID).First(&stored).Error)
	require.NoError(t, model.DB.Where("installation_id = ?", left.InstallationID).Order("id").Find(&after).Error)
	assert.Equal(t, before, after)
	assert.Equal(t, left.Revision, stored.Revision)
	assert.Equal(t, left.BaseURL, stored.BaseURL)
	assert.Equal(t, [6]int64{2, 2, 2, 8, 0, 0}, appPluginControllerPersistenceCounts(t))
	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	assert.Equal(t, "app_route_collision", appPluginErrorCode(t, response))
}

func appPluginConcurrentDraftRegression(t *testing.T) {
	setupAppPluginControllerTest(t)
	if model.DB.Dialector.Name() == "sqlite" {
		t.Skip("SQLite serializes writers")
	}
	const key = "concurrent-draft"
	body := appPluginJSONBody(t, map[string]any{
		"manifest": appPluginTestManifest(key, key, "1.0.0"),
		"base_url": "https://apps.example.com/" + key + "/",
		"entitlement_policy": map[string]any{
			"key": key, "rules": map[string][]string{"app_plugin": {"manage"}},
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	type workerKey struct{}
	var seen sync.Map
	var arrived atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	const callback = "test:b14_concurrent_draft"
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		worker := tx.Statement.Context.Value(workerKey{})
		if worker == nil || tx.Statement.Table != "app_installation_idempotencies" || tx.RowsAffected != 0 {
			return
		}
		if _, loaded := seen.LoadOrStore(worker, true); loaded {
			return
		}
		if arrived.Add(1) == 2 {
			unblock()
		}
		select {
		case <-release:
		case <-ctx.Done():
			tx.AddError(ctx.Err())
		}
	}))
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		unblock()
		wg.Wait()
		require.NoError(t, model.DB.Callback().Query().Remove(callback))
	})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for worker := range 2 {
		wg.Go(func() {
			responses <- appPluginControllerRequest(t, func(c *gin.Context) {
				c.Request = c.Request.WithContext(context.WithValue(ctx, workerKey{}, worker))
				CreateAppPluginInstallation(c)
			}, http.MethodPost, "/api/app_plugins/installations", body,
				common.RoleRootUser, "default", map[string]string{"Idempotency-Key": key})
		})
	}
	var first string
	for range 2 {
		select {
		case response := <-responses:
			assert.Equal(t, http.StatusCreated, response.Code, response.Body.String())
			if first == "" {
				first = response.Body.String()
			} else {
				assert.JSONEq(t, first, response.Body.String(), "both requests must return the identical frozen response")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.Equal(t, int32(2), arrived.Load())
	assert.Equal(t, [6]int64{1, 1, 1, 4, 1, 0}, appPluginControllerPersistenceCounts(t))
	var frozen model.AppInstallationIdempotency
	require.NoError(t, model.DB.First(&frozen).Error)
	assert.NotEmpty(t, frozen.ResponseJSON)
	assert.NotEmpty(t, frozen.ResponseDigest)
}

func appPluginDeadlockRegression(t *testing.T, entitlement, existingTransaction bool) {
	setupAppPluginControllerTest(t)
	dialect := model.DB.Dialector.Name()
	if dialect == "sqlite" {
		t.Skip("row deadlock detection is specific to MySQL/PostgreSQL")
	}
	require.NoError(t, model.DB.Exec("CREATE TABLE b14_regression_locks (id integer PRIMARY KEY, value integer NOT NULL)").Error)
	for id := 0; id <= 80; id++ {
		require.NoError(t, model.DB.Exec("INSERT INTO b14_regression_locks (id, value) VALUES (?, 0)", id).Error)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	rival := model.DB.WithContext(ctx).Begin()
	require.NoError(t, rival.Error)
	defer rival.Rollback()
	if dialect == "postgres" {
		require.NoError(t, rival.Exec("SET LOCAL deadlock_timeout = '10s'").Error)
	}
	require.NoError(t, rival.Exec("UPDATE b14_regression_locks SET value = value + 1 WHERE id >= 2").Error)
	var rivalID int
	idQuery := "SELECT CONNECTION_ID()"
	if dialect == "postgres" {
		idQuery = "SELECT pg_backend_pid()"
	}
	require.NoError(t, rival.Raw(idQuery).Scan(&rivalID).Error)
	var injected atomic.Bool
	var deadlockErr error
	rivalDone := make(chan error, 1)
	var wg sync.WaitGroup
	const callback = "test:b14_real_deadlock"
	deadlockTable := "app_installations"
	if entitlement {
		deadlockTable = "app_entitlement_policies"
	}
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if entitlement {
			if claim, ok := tx.Statement.Dest.(*model.AppRouteClaim); ok && claim.Kind == "direct" {
				tx.AddError(errors.New("later route creation failed"))
				return
			}
		}
		if tx.Statement.Table != deadlockTable || !injected.CompareAndSwap(false, true) {
			return
		}
		db := tx.Session(&gorm.Session{NewDB: true}).WithContext(ctx)
		if dialect == "postgres" {
			if err := db.Exec("SET LOCAL deadlock_timeout = '50ms'").Error; err != nil {
				tx.AddError(err)
				return
			}
		}
		if err := db.Exec("UPDATE b14_regression_locks SET value = value + 1 WHERE id = 1").Error; err != nil {
			tx.AddError(err)
			return
		}
		wg.Go(func() {
			err := rival.Exec("UPDATE b14_regression_locks SET value = value + 1 WHERE id = 1").Error
			if err == nil {
				err = rival.Commit().Error
			} else {
				rival.Rollback()
			}
			rivalDone <- err
		})
		if err := appPluginWaitForDBLock(ctx, model.DB, rivalID); err != nil {
			tx.AddError(err)
			return
		}
		deadlockErr = db.Exec("UPDATE b14_regression_locks SET value = value + 1 WHERE id = 2").Error
		tx.AddError(deadlockErr)
	}))
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		require.NoError(t, model.DB.Callback().Create().Remove(callback))
	})
	body := appPluginInstallBody(t, appPluginTestManifest("deadlock-app", "Deadlock App", "1.0.0"), "https://apps.example.com/deadlock/")
	if entitlement {
		var request map[string]any
		require.NoError(t, common.Unmarshal(body, &request))
		request["entitlement_policy"] = map[string]any{
			"key": "deadlock-policy", "rules": map[string][]string{"app_plugin": {"manage"}},
		}
		body = appPluginJSONBody(t, request)
	}
	handler := CreateAppPluginInstallation
	var nestedResult service.AppEntitlementPolicyResult
	var nestedErr error
	if existingTransaction {
		handler = func(c *gin.Context) {
			err := model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := tx.Exec("UPDATE b14_regression_locks SET value = 1 WHERE id = 0").Error; err != nil {
					return err
				}
				nestedResult, nestedErr = service.NewAppPluginInstallationService(tx,
					service.AppPluginInstallationOptions{CurrentAuthz: appPluginCurrentAuthz()}).
					CreateEntitlementPolicy(ctx, service.AppEntitlementPolicyDraft{
						Key: "nested-deadlock", Rules: map[string][]string{"app_plugin": {"manage"}},
					})
				if nestedErr != nil {
					return nestedErr
				}
				return errors.New("later outer failure")
			})
			writeAppPluginServiceError(c, err)
		}
	}
	response := appPluginControllerRequest(t, handler, http.MethodPost,
		"/api/app_plugins/installations", body, common.RoleRootUser, "default",
		map[string]string{"Idempotency-Key": "deadlock-app"})
	select {
	case err := <-rivalDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Error(t, deadlockErr)
	marker := "1213"
	if dialect == "postgres" {
		marker = "40P01"
	}
	require.Contains(t, deadlockErr.Error(), marker)
	if existingTransaction {
		require.Error(t, nestedErr, "caller-owned transaction must receive the original deadlock, not a savepoint retry")
		assert.Contains(t, nestedErr.Error(), marker)
		assert.Equal(t, service.AppEntitlementPolicyResult{}, nestedResult)
		var value int
		require.NoError(t, model.DB.Raw("SELECT value FROM b14_regression_locks WHERE id = 0").Scan(&value).Error)
		assert.Zero(t, value, "outer writes must roll back too")
	}
	counts := appPluginControllerPersistenceCounts(t)
	t.Logf("actual deadlock=%v status=%d persisted=%v", deadlockErr, response.Code, counts)
	if entitlement {
		assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		assert.Equal(t, [6]int64{}, counts, "later failure must roll back every row after a fresh retry")
	} else {
		assert.Equal(t, http.StatusCreated, response.Code, response.Body.String())
		assert.Equal(t, [6]int64{1, 1, 1, 4, 0, 0}, counts)
	}
}

func appPluginPolicySelectionRegression(t *testing.T) {
	setupAppPluginControllerTest(t)
	const key = "policy-selection"
	request := map[string]any{
		"manifest": appPluginTestManifest(key, key, "1.0.0"),
		"base_url": "https://apps.example.com/" + key + "/",
		"entitlement_policy": map[string]any{
			"key": key, "rules": map[string][]string{"app_plugin": {"manage"}, "not_grantable": {"read"}},
		},
	}
	post := func(scope string) *httptest.ResponseRecorder {
		return appPluginControllerRequest(t, CreateAppPluginInstallation, http.MethodPost,
			"/api/app_plugins/installations", appPluginJSONBody(t, request), common.RoleRootUser, "default",
			map[string]string{"Idempotency-Key": scope})
	}
	first := post(key)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	var frozen appPluginTestEnvelope
	require.NoError(t, common.Unmarshal(first.Body.Bytes(), &frozen))
	request["entitlement_policy"] = map[string]any{
		"key": key, "rules": map[string][]string{"not_grantable": {"read", "read"}, "app_plugin": {"manage", "manage"}},
	}
	canonical := post(key)
	assert.Equal(t, http.StatusCreated, canonical.Code, canonical.Body.String())
	assert.JSONEq(t, first.Body.String(), canonical.Body.String())
	request["entitlement_policy"] = map[string]any{
		"key": key, "rules": map[string][]string{"app_plugin": {"manage"}, "not_grantable": {"write"}},
	}
	conflict := post(key)
	assert.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	assert.Equal(t, "idempotency_conflict", appPluginErrorCode(t, conflict), "hash the original rules, not just the effective subset")
	replay := post(key + "-generation-replay")
	assert.Equal(t, http.StatusCreated, replay.Code, replay.Body.String())
	assert.JSONEq(t, first.Body.String(), replay.Body.String(), "generation replay must not allocate another policy")
	assert.Equal(t, [6]int64{1, 1, 2, 4, 1, 0}, appPluginControllerPersistenceCounts(t))

	request["manifest"] = appPluginTestManifest(key, key, "2.0.0")
	request["entitlement_policy"] = map[string]string{"id": frozen.Data.EntitlementPolicyVersion}
	existing := post(key + "-reference")
	require.Equal(t, http.StatusCreated, existing.Code, existing.Body.String())
	var referenced appPluginTestEnvelope
	require.NoError(t, common.Unmarshal(existing.Body.Bytes(), &referenced))
	assert.Equal(t, frozen.Data.EntitlementPolicyVersion, referenced.Data.EntitlementPolicyVersion)
	before := appPluginControllerPersistenceCounts(t)
	request["manifest"] = appPluginTestManifest(key, key, "3.0.0")
	request["entitlement_policy"] = "policy_missing"
	missing := post(key + "-missing")
	assert.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
	assert.Equal(t, "not_found", appPluginErrorCode(t, missing))
	assert.Equal(t, before, appPluginControllerPersistenceCounts(t))
}

func appPluginWaitForDBLock(ctx context.Context, db *gorm.DB, connectionID int) error {
	query := "SELECT COUNT(*) FROM information_schema.innodb_lock_waits w JOIN information_schema.innodb_trx t ON t.trx_id = w.requesting_trx_id WHERE t.trx_mysql_thread_id = ?"
	if db.Dialector.Name() == "postgres" {
		query = "SELECT COUNT(*) FROM pg_stat_activity WHERE pid = ? AND wait_event_type = 'Lock'"
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int64
		if err := db.WithContext(ctx).Raw(query, connectionID).Scan(&waiting).Error; err != nil {
			return err
		}
		if waiting > 0 {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func appPluginPatchUpgradeRegression(t *testing.T, nextRevision bool) {
	setupAppPluginControllerTest(t)
	if model.DB.Dialector.Name() == "sqlite" {
		t.Skip("SQLite serializes writers")
	}
	app := createAppPluginForControllerTest(t, "patch-upgrade", "Patch Upgrade", "patch-upgrade")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	type workerKey struct{}
	ownershipHeld := make(chan struct{}, 1)
	patchStarted := make(chan bool, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var held, started atomic.Bool
	const callback = "test:b14_patch_upgrade_order"
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Context.Value(workerKey{}) != "upgrade" || tx.Statement.Table != "app_route_claims" ||
			!strings.Contains(tx.Statement.SQL.String(), "FOR UPDATE") || !held.CompareAndSwap(false, true) {
			return
		}
		ownershipHeld <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			tx.AddError(ctx.Err())
		}
	}))
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register(callback+":patch", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(workerKey{}) != "patch" || tx.Statement.Table != "app_route_claims" {
			return
		}
		if _, locks := tx.Statement.Clauses["FOR"]; locks && started.CompareAndSwap(false, true) {
			patchStarted <- true
		}
	}))
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback+":write", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(workerKey{}) == "patch" && started.CompareAndSwap(false, true) {
			patchStarted <- false
			select {
			case <-release:
			case <-ctx.Done():
				tx.AddError(ctx.Err())
			}
		}
	}))
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		unblock()
		wg.Wait()
		require.NoError(t, model.DB.Callback().Query().Remove(callback))
		require.NoError(t, model.DB.Callback().Query().Remove(callback+":patch"))
		require.NoError(t, model.DB.Callback().Update().Remove(callback+":write"))
	})
	upgradeBody := appPluginInstallBody(t, appPluginTestManifest(app.AppKey, "Patch Upgrade", "2.0.0"),
		"https://apps.example.com/upgraded/")
	patchRevision := app.Revision
	if nextRevision {
		patchRevision++
	}
	patchBody := appPluginPatchBody(t, app.InstallationID, patchRevision,
		map[string]any{"base_url": "https://apps.example.com/patched/"})
	upgradeDone := make(chan *httptest.ResponseRecorder, 1)
	patchDone := make(chan *httptest.ResponseRecorder, 1)
	wg.Go(func() {
		upgradeDone <- appPluginControllerRequest(t, func(c *gin.Context) {
			c.Request = c.Request.WithContext(context.WithValue(ctx, workerKey{}, "upgrade"))
			CreateAppPluginInstallation(c)
		}, http.MethodPost, "/api/app_plugins/installations", upgradeBody,
			common.RoleRootUser, "default", map[string]string{"Idempotency-Key": "upgrade-v2"})
	})
	select {
	case <-ownershipHeld:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wg.Go(func() {
		patchDone <- appPluginControllerRequest(t, func(c *gin.Context) {
			c.Request = c.Request.WithContext(context.WithValue(ctx, workerKey{}, "patch"))
			PatchAppPluginInstallation(c)
		}, http.MethodPatch, "/api/app_plugins/installations", patchBody, common.RoleRootUser, "default", nil)
	})
	select {
	case ordered := <-patchStarted:
		assert.True(t, ordered, "PATCH must lock ownership before any mutation")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	unblock()
	select {
	case response := <-upgradeDone:
		assert.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case response := <-patchDone:
		if nextRevision {
			assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		} else {
			assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
			assert.Equal(t, "version_conflict", appPluginErrorCode(t, response))
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var current model.AppInstallation
	require.NoError(t, model.DB.Where("installation_id = ?", app.InstallationID).First(&current).Error)
	expectedRevision, expectedBase := int64(2), "https://apps.example.com/upgraded/"
	if nextRevision {
		expectedRevision, expectedBase = 3, "https://apps.example.com/patched/"
	}
	assert.Equal(t, expectedRevision, current.Revision)
	assert.Equal(t, "2.0.0", current.ManifestVersion)
	assert.Equal(t, expectedBase, current.BaseURL)
	var routes []model.AppRouteClaim
	require.NoError(t, model.DB.Where("installation_id = ? AND kind <> ?", app.InstallationID, "app_key").Find(&routes).Error)
	require.Len(t, routes, 3)
	for _, route := range routes {
		assert.True(t, strings.HasPrefix(route.AbsoluteEndpoint, current.BaseURL), route.AbsoluteEndpoint)
	}
}
