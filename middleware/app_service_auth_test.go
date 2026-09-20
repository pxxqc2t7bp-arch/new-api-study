package middleware

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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

func TestWriteAppServiceAuthErrorMarksServiceUnavailableRetryable(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	writeAppServiceAuthError(c, http.StatusServiceUnavailable, "app_execution_denied")

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	var response struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "app_execution_denied", response.Error.Code)
	assert.True(t, response.Error.Retryable)
}

func TestAppGrantAuthUnknownErrorDefaultsToServiceUnavailable(t *testing.T) {
	assert.Equal(t, "service_unavailable", appGrantAuthErrorCode(errors.New("private database detail")))
	assert.Equal(t, "service_unavailable",
		appGrantAuthErrorCode(&service.AppPluginAuthError{Code: "service_unavailable"}))
	assert.Equal(t, "scope_denied",
		appGrantAuthErrorCode(&service.AppPluginAuthError{Code: "scope_denied"}))
}

func TestAppServiceAuthReturnsRetryableServiceUnavailableOnCredentialQueryFailure(t *testing.T) {
	db, _, credential := setupAppServiceMiddlewareTest(t)
	injected := errors.New("injected credential query failure")
	var failed atomic.Bool
	const callbackName = "test:app-service-credential-query-failure"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "app_service_credentials" && failed.CompareAndSwap(false, true) {
			tx.AddError(injected)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	router := gin.New()
	router.POST("/internal/apps/v1/sessions/introspect", AppServiceAuth(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"https://host.example.com/internal/apps/v1/sessions/introspect",
		bytes.NewBufferString(`{"app_key":"middleware-app"}`),
	)
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Authorization", "AppService "+credential.Credential)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	assert.Equal(t, "service_unavailable", envelope.Error.Code)
	assert.Equal(t, "App service request denied", envelope.Error.Message)
	assert.True(t, envelope.Error.Retryable)
	assert.NotContains(t, response.Body.String(), credential.Credential)
	assert.NotContains(t, response.Body.String(), credential.CredentialID)
	assert.NotContains(t, response.Body.String(), fmt.Sprintf("%x", sha256.Sum256([]byte(credential.Credential))))
}

func TestServiceCredentialVersionIsEnforced(t *testing.T) {
	db, installation, credential := setupAppServiceMiddlewareTest(t)
	router := gin.New()
	router.POST("/internal/apps/v1/sessions/introspect", AppServiceAuth(), func(c *gin.Context) {
		identity, ok := GetAppServiceIdentity(c)
		require.True(t, ok)
		assert.Equal(t, credential.Version, identity.Version)
		assert.Equal(t, installation.InstallationID, identity.InstallationID)
		_, dashboard := GetSessionAuthIdentity(c)
		assert.False(t, dashboard)
		c.Status(http.StatusNoContent)
	})
	request := func(header string, secure bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "https://host.example.com/internal/apps/v1/sessions/introspect",
			bytes.NewBufferString(`{"app_key":"middleware-app"}`))
		req.Header.Set("Authorization", header)
		if !secure {
			req.TLS = nil
			req.Header.Set("X-Forwarded-Proto", "https")
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		return recorder
	}
	assertInvalid := func(response *httptest.ResponseRecorder) {
		t.Helper()
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		var envelope struct {
			Error struct {
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
		assert.Equal(t, "service_identity_invalid", envelope.Error.Code)
		assert.False(t, envelope.Error.Retryable)
		assert.NotContains(t, response.Body.String(), credential.Credential)
		assert.NotContains(t, response.Body.String(), credential.CredentialID)
	}
	require.Equal(t, http.StatusNoContent, request("AppService "+credential.Credential, true).Code)
	for _, header := range []string{"", "Bearer " + credential.Credential, credential.Credential, "AppService invalid"} {
		response := request(header, true)
		assertInvalid(response)
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	}
	insecure := request("AppService "+credential.Credential, false)
	assertInvalid(insecure)
	assert.Equal(t, http.StatusUnauthorized, insecure.Code, "an untrusted forwarded header must not replace TLS")
	require.NoError(t, db.Model(&model.AppServiceCredential{}).Where("credential_id = ?", credential.CredentialID).
		Update("credential_version", "forged-version").Error)
	assertInvalid(request("AppService "+credential.Credential, true))
	require.NoError(t, db.Model(&model.AppServiceCredential{}).Where("credential_id = ?", credential.CredentialID).
		Update("credential_version", credential.Version).Error)
	require.Equal(t, http.StatusNoContent, request("AppService "+credential.Credential, true).Code)
	require.NoError(t, model.RevokeAppServiceCredential(t.Context(), db, installation.InstallationID, credential.CredentialID, time.Now()))
	revoked := request("AppService "+credential.Credential, true)
	assertInvalid(revoked)
	assert.Equal(t, http.StatusUnauthorized, revoked.Code, "every request must reread revocation, with no positive credential cache")
}

func TestServiceIdentityCannotReachDashboardOrAdmin(t *testing.T) {
	db, _, credential := setupAppServiceMiddlewareTest(t)
	router := gin.New()
	reached := false
	handler := func(c *gin.Context) {
		reached = true
		c.Status(http.StatusNoContent)
	}
	router.GET("/api/user/self", UserAuth(), handler)
	router.GET("/api/admin", AdminAuth(), handler)
	router.GET("/api/root", RootAuth(), handler)
	router.POST("/v1/chat/completions", TokenAuth(), handler)
	router.POST("/internal/apps/v1/launch-codes/exchange", AppServiceAuth(), handler)
	router.POST("/internal/apps/v1/sessions/introspect", AppServiceAuth(), handler)
	router.POST("/internal/apps/v1/sessions/revoke", AppServiceAuth(), handler)
	router.POST("/internal/apps/v1/unregistered", AppServiceAuth(), handler)
	pat := "middleware-dashboard-pat"
	user := model.User{Username: "dashboard", Password: "unusable", AffCode: "dashboard",
		AccessToken: &pat, Status: common.UserStatusEnabled, Role: common.RoleRootUser, AuthVersion: 1}
	require.NoError(t, db.Create(&user).Error)
	for _, tc := range []struct {
		method string
		path   string
		header string
		allow  bool
	}{
		{http.MethodGet, "/api/user/self", "AppService " + credential.Credential, false},
		{http.MethodGet, "/api/admin", "AppService " + credential.Credential, false},
		{http.MethodGet, "/api/root", "AppService " + credential.Credential, false},
		{http.MethodPost, "/v1/chat/completions", "AppService " + credential.Credential, false},
		{http.MethodPost, "/internal/apps/v1/launch-codes/exchange", "AppService " + credential.Credential, true},
		{http.MethodPost, "/internal/apps/v1/sessions/introspect", "AppService " + credential.Credential, true},
		{http.MethodPost, "/internal/apps/v1/sessions/revoke", "AppService " + credential.Credential, true},
		{http.MethodPost, "/internal/apps/v1/unregistered", "AppService " + credential.Credential, false},
		{http.MethodPost, "/internal/apps/v1/launch-codes/exchange", "Bearer " + pat, false},
	} {
		t.Run(tc.method+" "+tc.path+" "+strings.Fields(tc.header)[0], func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(tc.method, "https://host.example.com"+tc.path, bytes.NewBufferString(`{}`))
			req.TLS = &tls.ConnectionState{}
			req.Header.Set("Authorization", tc.header)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			assert.Equal(t, tc.allow, reached, response.Body.String())
			if tc.allow {
				assert.Equal(t, http.StatusNoContent, response.Code)
			} else {
				assert.GreaterOrEqual(t, response.Code, 400)
			}
		})
	}
}

func setupAppServiceMiddlewareTest(t *testing.T) (*gorm.DB, model.AppInstallResult, model.AppServiceCredentialIssued) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dialect, dsn := os.Getenv("APP_PLUGIN_TEST_DIALECT"), os.Getenv("APP_PLUGIN_TEST_DSN")
	name := "app_service_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	var db *gorm.DB
	var err error
	switch dialect {
	case "", "sqlite":
		db, err = gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "app-service.sqlite")), config)
	case "mysql":
		parsed, parseErr := mysqlDriver.ParseDSN(dsn)
		require.NoError(t, parseErr)
		admin, openErr := gorm.Open(mysql.Open(dsn), config)
		require.NoError(t, openErr)
		require.NoError(t, admin.Exec("CREATE DATABASE `"+name+"`").Error)
		t.Cleanup(func() {
			assert.NoError(t, admin.Exec("DROP DATABASE `"+name+"`").Error)
			conn, err := admin.DB()
			require.NoError(t, err)
			assert.NoError(t, conn.Close())
		})
		parsed.DBName = name
		db, err = gorm.Open(mysql.Open(parsed.FormatDSN()), config)
	case "postgres", "postgresql":
		parsed, parseErr := pgx.ParseConfig(dsn)
		require.NoError(t, parseErr)
		admin, openErr := gorm.Open(postgres.Open(dsn), config)
		require.NoError(t, openErr)
		require.NoError(t, admin.Exec(`CREATE SCHEMA "`+name+`"`).Error)
		t.Cleanup(func() {
			assert.NoError(t, admin.Exec(`DROP SCHEMA "`+name+`" CASCADE`).Error)
			conn, err := admin.DB()
			require.NoError(t, err)
			assert.NoError(t, conn.Close())
		})
		parsed.RuntimeParams["search_path"] = name
		db, err = gorm.Open(postgres.New(postgres.Config{Conn: stdlib.OpenDB(*parsed)}), config)
	default:
		t.Fatalf("unsupported dialect %q", dialect)
	}
	require.NoError(t, err)
	conn, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	previousDB, previousLog, previousRedis := model.DB, model.LOG_DB, common.RedisEnabled
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	previousFlag, previousType := operation_setting.AppPluginV1Enabled, common.MainDatabaseType()
	common.OptionMap = maps.Clone(common.OptionMap)
	if common.OptionMap == nil {
		common.OptionMap = map[string]string{}
	}
	operation_setting.AppPluginV1Enabled = true
	common.OptionMap[operation_setting.AppPluginV1EnabledOptionKey] = "true"
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.RedisEnabled = previousDB, previousLog, previousRedis
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		operation_setting.AppPluginV1Enabled = previousFlag
		common.OptionMapRWMutex.Unlock()
		common.SetMainDatabaseType(previousType)
	})
	model.DB, model.LOG_DB, common.RedisEnabled = db, db, false
	common.SetMainDatabaseType(map[string]common.DatabaseType{
		"sqlite": common.DatabaseTypeSQLite, "mysql": common.DatabaseTypeMySQL, "postgres": common.DatabaseTypePostgreSQL,
	}[db.Dialector.Name()])
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.Token{}, &model.AuditLog{}))
	require.NoError(t, model.MigrateAppPluginTables(db))
	require.NoError(t, model.MigrateAppPluginLaunchTables(db))
	manifest, err := common.Marshal(service.AppManifest{Key: "middleware-app", Version: "1.0.0"})
	require.NoError(t, err)
	hash := sha256.Sum256(manifest)
	installation, err := model.InstallAppVersion(t.Context(), db, model.AppIdempotencyScope{ActorID: 1, Key: "middleware"},
		model.AppInstallRequest{AppKey: "middleware-app", ManifestVersion: "1.0.0",
			CanonicalManifestJSON: manifest, ManifestSHA256: fmt.Sprintf("%x", hash),
			BaseURL: "https://apps.example.com/", CallbackURL: "https://apps.example.com/callback",
			DirectURL: "https://apps.example.com/direct", EmbeddedURL: "https://apps.example.com/embedded",
			EnabledSurfaces: []string{"direct"}})
	require.NoError(t, err)
	credential, err := model.IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read"}, time.Now(), time.Now().Add(time.Hour), false)
	require.NoError(t, err)
	var version string
	query := "SELECT version()"
	if db.Dialector.Name() == "sqlite" {
		query = "SELECT sqlite_version()"
	}
	require.NoError(t, db.Raw(query).Scan(&version).Error)
	if dialect != "" {
		require.Equal(t, os.Getenv("APP_PLUGIN_TEST_DATABASE_VERSION"), version)
		require.Equal(t, strings.ReplaceAll(dialect, "postgresql", "postgres"), db.Dialector.Name())
		require.NotEmpty(t, os.Getenv("APP_PLUGIN_TEST_DRIVER"))
	}
	t.Logf("database=%s version=%s", db.Dialector.Name(), version)
	return db, installation, credential
}
