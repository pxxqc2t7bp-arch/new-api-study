package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
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

type appLaunchFixture struct {
	db           *gorm.DB
	service      *AppPluginAuthService
	options      AppPluginAuthOptions
	installation model.AppInstallResult
	user         model.User
	session      model.UserSession
	identity     AuthIdentity
	credential   model.AppServiceCredentialIssued
	serviceID    model.AppServiceIdentity
	now          atomic.Int64
	request      AppPluginAuthorizeRequest
	verifier     string
}

func newAppLaunchFixture(t *testing.T) *appLaunchFixture {
	t.Helper()
	db := openAppLaunchTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}))
	require.NoError(t, model.MigrateAppPluginTables(db))
	require.NoError(t, model.MigrateAppPluginLaunchTables(db))
	f := &appLaunchFixture{db: db}
	f.now.Store(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC).UnixNano())
	f.options = AppPluginAuthOptions{
		Issuer:        "https://console.example.com",
		DerivationKey: []byte("test-only-shared-app-launch-key-32-bytes"),
		Now:           func() time.Time { return time.Unix(0, f.now.Load()).UTC() },
		CurrentPermissions: func(_ context.Context, _ *gorm.DB, user model.User) (model.AppJSONMap, error) {
			if user.Role == common.RoleRootUser {
				return model.AppJSONMap{"app_plugin": {"manage"}, "task": {"read"}}, nil
			}
			return model.AppJSONMap{"task": {"read"}}, nil
		},
	}
	f.service = NewAppPluginAuthService(db, f.options)
	f.user = model.User{Username: "launch-user", Password: "not-a-login-password", AffCode: "launch-user",
		Status: common.UserStatusEnabled, Role: common.RoleRootUser, Group: "default", AuthVersion: 4}
	require.NoError(t, db.Create(&f.user).Error)
	f.session = model.UserSession{SID: uuid.NewString(), UserID: f.user.Id, UserAuthVersion: 4, Version: 7,
		Status: model.UserSessionStatusActive, RefreshHash: "unusable-fixture-hash",
		CreatedAt: f.options.Now().Unix(), LastActiveAt: f.options.Now().Unix(),
		ExpiresAt: f.options.Now().Add(time.Hour).Unix()}
	require.NoError(t, db.Create(&f.session).Error)
	f.identity = AuthIdentity{UserID: f.user.Id, SessionID: f.session.SID, UserAuthVersion: 4, SessionVersion: 7}
	cmd := appPluginInstallCommand("launch-app", "1.0.0")
	cmd.ServiceCredential = AppServiceCredentialInput{}
	cmd.NetworkPolicy = model.AppNetworkPolicy{AllowHosts: []string{"apps.example.com"}, DenyPrivateIPRanges: true}
	cmd.EntitlementPolicyDraft = &AppEntitlementPolicyDraft{
		Key: "launch-policy", Rules: map[string][]string{"app_plugin": {"manage"}, "task": {"read"}},
	}
	var err error
	f.installation, err = NewAppPluginInstallationService(db, AppPluginInstallationOptions{
		TaskPluginChecker: fixedTaskPluginChecker{"doubao": "1.2.0"},
		CurrentAuthz:      map[string][]string{"app_plugin": {"manage"}, "task": {"read"}},
	}).Install(t.Context(), cmd)
	require.NoError(t, err)
	f.credential, err = model.IssueAppServiceCredential(t.Context(), db, f.installation.InstallationID,
		[]string{"identity.read"}, f.options.Now(), f.options.Now().Add(24*time.Hour), false)
	require.NoError(t, err)
	f.serviceID, err = model.AuthenticateAppServiceCredential(t.Context(), db, f.credential.Credential, f.options.Now())
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.AppInstallation{}).Where("installation_id = ?", f.installation.InstallationID).
		Update("status", model.AppInstallationStatusEnabled).Error)
	f.verifier = strings.Repeat("v", 43)
	challenge := sha256.Sum256([]byte(f.verifier))
	f.request = AppPluginAuthorizeRequest{
		Surface: "direct", TransactionID: uuid.NewString(),
		State:         base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(1)),
		Nonce:         base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(2)),
		CodeChallenge: base64.RawURLEncoding.EncodeToString(challenge[:]), CodeChallengeMethod: "S256",
	}
	return f
}

func bytesForAppLaunch(value byte) []byte {
	result := make([]byte, 32)
	for i := range result {
		result[i] = value
	}
	return result
}

func (f *appLaunchFixture) authorize(t *testing.T, key string) AppPluginAuthorizeResult {
	t.Helper()
	result, err := f.service.Authorize(t.Context(), f.identity, f.installation.AppKey,
		"https://console.example.com", key, f.request)
	require.NoError(t, err)
	return result
}

func (f *appLaunchFixture) exchangeRequest(t *testing.T, launch AppPluginAuthorizeResult) AppPluginExchangeRequest {
	t.Helper()
	callback, err := url.Parse(launch.LaunchURL)
	require.NoError(t, err)
	return AppPluginExchangeRequest{
		ExchangeRequestID: uuid.NewString(), AppKey: f.installation.AppKey,
		Surface: f.request.Surface, TransactionID: f.request.TransactionID,
		Code: callback.Query().Get("code"), CodeVerifier: f.verifier, State: f.request.State, Nonce: f.request.Nonce,
	}
}

func (f *appLaunchFixture) exchange(t *testing.T) AppPluginExchangeResult {
	t.Helper()
	request := f.exchangeRequest(t, f.authorize(t, uuid.NewString()))
	result, err := f.service.Exchange(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	return result
}

func TestAuthorizeBindsOriginCallbackPKCEAndSessionVersions(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*appLaunchFixture)
		code string
	}{
		{"PAT has no session", func(f *appLaunchFixture) { f.identity.SessionID = "" }, "unauthenticated"},
		{"stale auth version", func(f *appLaunchFixture) { f.identity.UserAuthVersion-- }, "unauthenticated"},
		{"stale session version", func(f *appLaunchFixture) { f.identity.SessionVersion-- }, "unauthenticated"},
		{"plain PKCE", func(f *appLaunchFixture) { f.request.CodeChallengeMethod = "plain" }, "invalid_request"},
		{"short challenge", func(f *appLaunchFixture) { f.request.CodeChallenge = "short" }, "invalid_request"},
		{"short state", func(f *appLaunchFixture) { f.request.State = "short" }, "invalid_request"},
		{"short nonce", func(f *appLaunchFixture) { f.request.Nonce = "short" }, "invalid_request"},
		{"missing transaction", func(f *appLaunchFixture) { f.request.TransactionID = "" }, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppLaunchFixture(t)
			tc.edit(f)
			_, err := f.service.Authorize(t.Context(), f.identity, f.installation.AppKey,
				"https://console.example.com", "authorize", f.request)
			require.ErrorContains(t, err, tc.code)
			var count int64
			require.NoError(t, f.db.Model(&model.AppPluginLaunchCode{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
	for _, tc := range []struct {
		name   string
		table  any
		values map[string]any
		code   string
	}{
		{"authoritative user disabled", &model.User{}, map[string]any{"status": common.UserStatusDisabled}, "identity_inactive"},
		{"authoritative auth version", &model.User{}, map[string]any{"auth_version": 5}, "unauthenticated"},
		{"authoritative session version", &model.UserSession{}, map[string]any{"version": 8}, "unauthenticated"},
		{"logged out", &model.UserSession{}, map[string]any{"status": model.UserSessionStatusRevoked}, "unauthenticated"},
		{"expired upstream", &model.UserSession{}, map[string]any{"expires_at": 1}, "unauthenticated"},
		{"disabled installation", &model.AppInstallation{}, map[string]any{"status": "disabled"}, "app_plugin_disabled"},
		{"user policy shrank", &model.User{}, map[string]any{"group": "not-allowed"}, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppLaunchFixture(t)
			require.NoError(t, f.db.Session(&gorm.Session{AllowGlobalUpdate: true}).Model(tc.table).Updates(tc.values).Error)
			_, err := f.service.Authorize(t.Context(), f.identity, f.installation.AppKey,
				"https://console.example.com", "authorize", f.request)
			require.ErrorContains(t, err, tc.code)
		})
	}
	for _, field := range []string{"state", "nonce", "verifier", "auth_version", "session_version", "logout"} {
		t.Run("exchange rejects changed "+field, func(t *testing.T) {
			f := newAppLaunchFixture(t)
			request := f.exchangeRequest(t, f.authorize(t, "authorize"))
			code := "unauthenticated"
			switch field {
			case "state":
				request.State = base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(3))
				code = "state_mismatch"
			case "nonce":
				request.Nonce = base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(3))
				code = "nonce_mismatch"
			case "verifier":
				request.CodeVerifier = strings.Repeat("x", 43)
				code = "pkce_verification_failed"
			case "auth_version":
				require.NoError(t, f.db.Model(&f.user).Update("auth_version", 5).Error)
			case "session_version":
				require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", f.session.SID).Update("version", 8).Error)
			case "logout":
				require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", f.session.SID).Update("revoked_at", f.options.Now().Unix()).Error)
			}
			_, err := f.service.Exchange(t.Context(), f.serviceID, request)
			require.ErrorContains(t, err, code)
			var count int64
			require.NoError(t, f.db.Model(&model.AppPluginSession{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestAuthorizeBindsSurfaceTransactionAndRegisteredCallback(t *testing.T) {
	for _, origin := range []string{"", "null", "http://console.example.com", "https://evil.example.com", "https://console.example.com:444"} {
		t.Run("embedded origin "+origin, func(t *testing.T) {
			f := newAppLaunchFixture(t)
			f.request.Surface = "embedded"
			_, err := f.service.Authorize(t.Context(), f.identity, f.installation.AppKey, origin, "authorize", f.request)
			require.ErrorContains(t, err, "forbidden")
		})
	}
	for _, tc := range []struct {
		name   string
		values map[string]any
	}{
		{"surface disabled", map[string]any{"enabled_surfaces": model.AppStringList{"direct"}}},
		{"cross site parent", map[string]any{"allowed_parent_origins": model.AppStringList{"https://console.other.test"}}},
		{"non HTTPS base", map[string]any{"base_url": "http://apps.example.com/launch-app/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppLaunchFixture(t)
			f.request.Surface = "embedded"
			require.NoError(t, f.db.Model(&model.AppInstallation{}).Where("installation_id = ?", f.installation.InstallationID).Updates(tc.values).Error)
			_, err := f.service.Authorize(t.Context(), f.identity, f.installation.AppKey,
				"https://console.example.com", "authorize", f.request)
			require.Error(t, err)
		})
	}
	for _, field := range []string{"surface", "transaction", "callback", "generation", "installation"} {
		t.Run("exchange binds "+field, func(t *testing.T) {
			f := newAppLaunchFixture(t)
			request := f.exchangeRequest(t, f.authorize(t, "authorize"))
			switch field {
			case "surface":
				request.Surface = "embedded"
			case "transaction":
				request.TransactionID = uuid.NewString()
			case "callback":
				require.NoError(t, f.db.Model(&model.AppRouteClaim{}).
					Where("installation_id = ? AND kind = ?", f.installation.InstallationID, "callback").
					Update("absolute_endpoint", "https://apps.example.com/unregistered").Error)
			case "generation":
				require.NoError(t, f.db.Model(&model.AppInstallation{}).Where("installation_id = ?", f.installation.InstallationID).
					Update("app_version_id", "another-generation").Error)
			case "installation":
				f.serviceID.InstallationID = "another-installation"
			}
			_, err := f.service.Exchange(t.Context(), f.serviceID, request)
			require.Error(t, err)
			var count int64
			require.NoError(t, f.db.Model(&model.AppPluginSession{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
	t.Run("registered callback is checked before minting", func(t *testing.T) {
		f := newAppLaunchFixture(t)
		require.NoError(t, f.db.Where("kind = ?", "callback").Delete(&model.AppRouteClaim{}).Error)
		_, err := f.service.Authorize(t.Context(), f.identity, f.installation.AppKey,
			"https://console.example.com", "authorize", f.request)
		require.Error(t, err)
	})
}

func TestAuthorizeIdempotencyDoesNotMintSecondCode(t *testing.T) {
	t.Run("contending replicas freeze one launch", func(t *testing.T) {
		f := newAppLaunchFixture(t)
		var launches [2]AppPluginAuthorizeResult
		outcomes := contendAppLaunchOperations(t, f, func(ctx context.Context) (err error) {
			launches[0], err = f.service.Authorize(ctx, f.identity, f.installation.AppKey, f.options.Issuer, "contended", f.request)
			return err
		}, func(ctx context.Context) (err error) {
			launches[1], err = NewAppPluginAuthService(f.db, f.options).Authorize(ctx, f.identity, f.installation.AppKey, f.options.Issuer, "contended", f.request)
			return err
		})
		require.NoError(t, outcomes[0])
		require.NoError(t, outcomes[1])
		assert.Equal(t, launches[0], launches[1])
		var count int64
		require.NoError(t, f.db.Model(&model.AppPluginLaunchCode{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
	f := newAppLaunchFixture(t)
	first := f.authorize(t, "one-key")
	assert.Equal(t, 60, first.ExpiresIn)
	secondReplica := NewAppPluginAuthService(f.db, f.options)
	replayed, err := secondReplica.Authorize(t.Context(), f.identity, f.installation.AppKey,
		"https://console.example.com", "one-key", f.request)
	require.NoError(t, err)
	assert.Equal(t, first, replayed)
	var rows []map[string]any
	require.NoError(t, f.db.Table("app_plugin_launch_codes").Find(&rows).Error)
	require.Len(t, rows, 1)
	raw, err := common.Marshal(rows)
	require.NoError(t, err)
	callback, err := url.Parse(first.LaunchURL)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), callback.Query().Get("code"), "no plaintext code, including frozen JSON")
	assert.NotContains(t, string(raw), first.LaunchURL)
	for _, field := range []string{"nonce", "state", "challenge", "surface", "transaction"} {
		t.Run("same key different "+field, func(t *testing.T) {
			changed := f.request
			switch field {
			case "nonce":
				changed.Nonce = base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(4))
			case "state":
				changed.State = base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(4))
			case "challenge":
				changed.CodeChallenge = base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(4))
			case "surface":
				changed.Surface = "embedded"
			case "transaction":
				changed.TransactionID = uuid.NewString()
			}
			_, err := secondReplica.Authorize(t.Context(), f.identity, f.installation.AppKey,
				"https://console.example.com", "one-key", changed)
			require.ErrorContains(t, err, "idempotency_conflict")
		})
	}
	t.Run("key unavailable fails closed", func(t *testing.T) {
		options := f.options
		options.DerivationKey = nil
		_, err := NewAppPluginAuthService(f.db, options).Authorize(t.Context(), f.identity, f.installation.AppKey,
			"https://console.example.com", "one-key", f.request)
		require.ErrorContains(t, err, "identity_not_configured")
		options.DerivationKey = []byte("different-shared-test-only-key-32-bytes")
		_, err = NewAppPluginAuthService(f.db, options).Authorize(t.Context(), f.identity, f.installation.AppKey,
			"https://console.example.com", "one-key", f.request)
		require.Error(t, err, "changing the derivation key must not re-sign a pending code")
	})
	_, err = f.service.Exchange(t.Context(), f.serviceID, f.exchangeRequest(t, first))
	require.NoError(t, err)
	_, err = f.service.Authorize(t.Context(), f.identity, f.installation.AppKey,
		"https://console.example.com", "one-key", f.request)
	require.ErrorContains(t, err, "launch_code_replayed")
	f.authorize(t, "expires-key")
	f.now.Add(int64(60 * time.Second))
	_, err = f.service.Authorize(t.Context(), f.identity, f.installation.AppKey,
		"https://console.example.com", "expires-key", f.request)
	require.ErrorContains(t, err, "launch_code_expired")
	f.authorize(t, "new-key")
	var count int64
	require.NoError(t, f.db.Model(&model.AppPluginLaunchCode{}).Count(&count).Error)
	assert.EqualValues(t, 3, count)
}

func TestExchangeHasOneLogicalWinnerAndFiveMinuteReplay(t *testing.T) {
	f := newAppLaunchFixture(t)
	request := f.exchangeRequest(t, f.authorize(t, "authorize"))
	var original, concurrent AppPluginExchangeResult
	outcomes := contendAppLaunchOperations(t, f, func(ctx context.Context) (err error) {
		original, err = f.service.Exchange(ctx, f.serviceID, request)
		return err
	}, func(ctx context.Context) (err error) {
		concurrent, err = NewAppPluginAuthService(f.db, f.options).Exchange(ctx, f.serviceID, request)
		return err
	})
	require.NoError(t, outcomes[0])
	require.NoError(t, outcomes[1])
	assert.Equal(t, original, concurrent)
	var count int64
	require.NoError(t, f.db.Model(&model.AppPluginSession{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	assert.Equal(t, f.options.Issuer, original.Issuer)
	assert.NotEmpty(t, original.Subject)
	assert.NotEqual(t, f.user.Username, original.Subject)
	assert.NotEqual(t, f.user.Email, original.Subject)
	assert.Equal(t, f.user.Id, original.UserID)
	assert.EqualValues(t, 4, original.AuthVersion)
	assert.EqualValues(t, 7, original.SessionVersion)
	assert.Equal(t, []string{"identity.read"}, original.GrantedScopes)
	assert.Equal(t, f.options.Now(), original.IssuedAt)
	assert.Equal(t, time.Unix(f.session.ExpiresAt, 0).UTC(), original.UpstreamExpiresAt)
	introspection := AppPluginIntrospectRequest{AppKey: request.AppKey, AppSessionID: original.AppSessionID,
		Subject: original.Subject, RequiredScopes: []string{}, RequiredEntitlements: model.AppJSONMap{}}
	for _, field := range []string{"state", "nonce", "verifier", "surface", "transaction", "service"} {
		t.Run("invalid second use cannot revoke "+field, func(t *testing.T) {
			bad, identity := request, f.serviceID
			bad.ExchangeRequestID = uuid.NewString()
			switch field {
			case "state":
				bad.State = base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(6))
			case "nonce":
				bad.Nonce = base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(6))
			case "verifier":
				bad.CodeVerifier = strings.Repeat("x", 43)
			case "surface":
				bad.Surface = "embedded"
			case "transaction":
				bad.TransactionID = "another-transaction"
			case "service":
				identity.Version = "another-version"
			}
			_, err := f.service.Exchange(t.Context(), identity, bad)
			require.Error(t, err)
			current, err := f.service.Introspect(t.Context(), f.serviceID, introspection)
			require.NoError(t, err)
			assert.True(t, current.Active)
		})
	}
	changed := request
	changed.Nonce = base64.RawURLEncoding.EncodeToString(bytesForAppLaunch(5))
	_, err := f.service.Exchange(t.Context(), f.serviceID, changed)
	require.ErrorContains(t, err, "idempotency_conflict")
	changed = request
	changed.ExchangeRequestID = uuid.NewString()
	_, err = f.service.Exchange(t.Context(), f.serviceID, changed)
	require.ErrorContains(t, err, "launch_code_replayed")
	current, err := f.service.Introspect(t.Context(), f.serviceID, introspection)
	require.NoError(t, err)
	assert.False(t, current.Active, "valid second logical consumption commits session revocation despite returning an error")
	f.now.Add(int64(299 * time.Second))
	replayed, err := NewAppPluginAuthService(f.db, f.options).Exchange(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.Equal(t, original, replayed)
	current, err = f.service.Introspect(t.Context(), f.serviceID, introspection)
	require.NoError(t, err)
	assert.False(t, current.Active, "frozen response replay cannot reactivate the session")
	f.now.Add(int64(time.Second))
	_, err = f.service.Exchange(t.Context(), f.serviceID, request)
	require.ErrorContains(t, err, "launch_code_replayed")
	t.Run("expired unconsumed code", func(t *testing.T) {
		next := f.exchangeRequest(t, f.authorize(t, "expires"))
		f.now.Add(int64(60 * time.Second))
		_, err := f.service.Exchange(t.Context(), f.serviceID, next)
		require.ErrorContains(t, err, "launch_code_expired")
	})
	t.Run("distinct request IDs have one winner", func(t *testing.T) {
		next := f.exchangeRequest(t, f.authorize(t, "distinct"))
		requests := []AppPluginExchangeRequest{next, next}
		requests[1].ExchangeRequestID = uuid.NewString()
		outcomes := contendAppLaunchOperations(t, f, func(ctx context.Context) error {
			_, err := f.service.Exchange(ctx, f.serviceID, requests[0])
			return err
		}, func(ctx context.Context) error {
			_, err := f.service.Exchange(ctx, f.serviceID, requests[1])
			return err
		})
		require.NoError(t, outcomes[0])
		require.ErrorContains(t, outcomes[1], "launch_code_replayed")
	})
}

// Hold the first operation at the ownership read until the second reaches the
// same lock (or waits for SQLite's single connection). No sleeps select a winner.
func contendAppLaunchOperations(t *testing.T, f *appLaunchFixture, first, second func(context.Context) error) [2]error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	type workerKey struct{}
	held, attempted, release := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{})
	var heldOnce, attemptedOnce atomic.Bool
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	callback := "test:b15_contention"
	require.NoError(t, f.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Context.Value(workerKey{}) != "first" || tx.Statement.Table != "app_route_claims" ||
			!heldOnce.CompareAndSwap(false, true) {
			return
		}
		held <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			tx.AddError(ctx.Err())
		}
	}))
	require.NoError(t, f.db.Callback().Query().Before("gorm:query").Register(callback+":waiter", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(workerKey{}) == "second" && tx.Statement.Table == "app_route_claims" &&
			attemptedOnce.CompareAndSwap(false, true) {
			attempted <- struct{}{}
		}
	}))
	var wg sync.WaitGroup
	defer func() {
		cancel()
		unblock()
		wg.Wait()
		require.NoError(t, f.db.Callback().Query().Remove(callback))
		require.NoError(t, f.db.Callback().Query().Remove(callback+":waiter"))
	}()
	var results [2]error
	done := make(chan int, 2)
	wg.Go(func() {
		results[0] = first(context.WithValue(ctx, workerKey{}, "first"))
		done <- 0
	})
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal("first operation did not hold ownership", ctx.Err())
	}
	conn, err := f.db.DB()
	require.NoError(t, err)
	waitCount := conn.Stats().WaitCount
	wg.Go(func() {
		results[1] = second(context.WithValue(ctx, workerKey{}, "second"))
		done <- 1
	})
	if f.db.Dialector.Name() == "sqlite" {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for conn.Stats().WaitCount == waitCount {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal("second operation did not wait for SQLite connection", ctx.Err())
			}
		}
	} else {
		select {
		case <-attempted:
		case <-ctx.Done():
			t.Fatal("second operation did not attempt ownership lock", ctx.Err())
		}
	}
	unblock()
	for range 2 {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("contending operation did not finish", ctx.Err())
		}
	}
	wg.Wait()
	return results
}

func TestIntrospectionReflectsLogoutDisableAndRevoke(t *testing.T) {
	for _, tc := range []struct {
		name   string
		table  any
		values map[string]any
		active bool
		reason string
	}{
		{"logout", &model.UserSession{}, map[string]any{"status": model.UserSessionStatusRevoked}, false, "unauthenticated"},
		{"session version", &model.UserSession{}, map[string]any{"version": 8}, false, "unauthenticated"},
		{"auth version", &model.User{}, map[string]any{"auth_version": 5}, false, "unauthenticated"},
		{"user disabled", &model.User{}, map[string]any{"status": common.UserStatusDisabled}, false, "identity_inactive"},
		{"upstream expired", &model.UserSession{}, map[string]any{"expires_at": 1}, false, "unauthenticated"},
		{"app disabled", &model.AppInstallation{}, map[string]any{"status": "disabled"}, true, "app_plugin_disabled"},
		{"app revoked", &model.AppInstallation{}, map[string]any{"status": "revoked"}, false, "app_plugin_revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppLaunchFixture(t)
			exchanged := f.exchange(t)
			request := AppPluginIntrospectRequest{AppKey: f.installation.AppKey, AppSessionID: exchanged.AppSessionID,
				Subject: exchanged.Subject, RequiredScopes: []string{}, RequiredEntitlements: model.AppJSONMap{}}
			before, err := f.service.Introspect(t.Context(), f.serviceID, request)
			require.NoError(t, err)
			require.True(t, before.Active)
			require.NoError(t, f.db.Session(&gorm.Session{AllowGlobalUpdate: true}).Model(tc.table).Updates(tc.values).Error)
			current, err := NewAppPluginAuthService(f.db, f.options).Introspect(t.Context(), f.serviceID, request)
			require.NoError(t, err)
			assert.Equal(t, tc.active, current.Active)
			assert.Equal(t, tc.reason, current.Reason)
			assert.Equal(t, f.options.Now(), current.CheckedAt)
			if tc.name == "app disabled" {
				assert.Equal(t, "disabled", current.AppStatus)
				request.RequiredEntitlements = model.AppJSONMap{"app_plugin": {"manage"}}
				write, err := f.service.Introspect(t.Context(), f.serviceID, request)
				require.NoError(t, err)
				assert.False(t, write.Active, "disabled sessions cannot authorize management")
			}
		})
	}
}

func TestIntrospectionReturnsVersionedHostEntitlements(t *testing.T) {
	t.Run("authoritative role and user overrides", func(t *testing.T) {
		f := newAppLaunchFixture(t)
		require.NoError(t, f.db.AutoMigrate(&model.CasbinRule{}))
		require.NoError(t, f.db.Model(&f.user).Update("role", common.RolePluginAdminUser).Error)
		rule := model.CasbinRule{Ptype: "p", V0: authz.RoleSubject(authz.BuiltInRolePluginAdmin),
			V1: "app_plugin", V2: "manage", V3: authz.EffectAllow}
		require.NoError(t, f.db.Create(&rule).Error)
		f.options.CurrentPermissions = nil
		f.service = NewAppPluginAuthService(f.db, f.options)
		exchanged := f.exchange(t)
		request := AppPluginIntrospectRequest{AppKey: f.installation.AppKey, AppSessionID: exchanged.AppSessionID,
			Subject: exchanged.Subject, RequiredScopes: []string{}, RequiredEntitlements: model.AppJSONMap{"app_plugin": {"manage"}}}
		allowed, err := f.service.Introspect(t.Context(), f.serviceID, request)
		require.NoError(t, err)
		require.True(t, allowed.Active)
		deny := model.CasbinRule{Ptype: "p", V0: authz.UserSubject(f.user.Id), V1: "app_plugin", V2: "manage", V3: authz.EffectDeny}
		require.NoError(t, f.db.Create(&deny).Error)
		denied, err := f.service.Introspect(t.Context(), f.serviceID, request)
		require.NoError(t, err)
		assert.False(t, denied.Active, "a current user override must supersede the role without cache refresh")
		require.NoError(t, f.db.Delete(&deny).Error)
		require.NoError(t, f.db.Delete(&rule).Error)
		removed, err := f.service.Introspect(t.Context(), f.serviceID, request)
		require.NoError(t, err)
		assert.False(t, removed.Active, "removed DB policy must not survive in process-local authorization")
	})
	f := newAppLaunchFixture(t)
	exchanged := f.exchange(t)
	assert.Equal(t, model.AppJSONMap{"app_plugin": {"manage"}, "task": {"read"}}, exchanged.Entitlements)
	request := AppPluginIntrospectRequest{AppKey: f.installation.AppKey, AppSessionID: exchanged.AppSessionID,
		Subject: exchanged.Subject, RequiredScopes: []string{"identity.read"},
		RequiredEntitlements: model.AppJSONMap{"app_plugin": {"manage"}}}
	initial, err := f.service.Introspect(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	require.True(t, initial.Active)
	require.NotEmpty(t, initial.EntitlementVersion)
	require.NoError(t, f.db.Model(&f.user).Update("role", common.RoleCommonUser).Error)
	shrunk, err := f.service.Introspect(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.False(t, shrunk.Active)
	assert.Equal(t, "scope_denied", shrunk.Reason)
	assert.NotContains(t, shrunk.Entitlements, "app_plugin")
	request.RequiredEntitlements = model.AppJSONMap{"task": {"read"}}
	read, err := f.service.Introspect(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.True(t, read.Active)
	policy, err := model.CreateAppEntitlementPolicy(t.Context(), f.db, "launch-policy", model.AppJSONMap{})
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&model.AppInstallation{}).Where("installation_id = ?", f.installation.InstallationID).
		Update("entitlement_policy_id", policy.ID).Error)
	current, err := f.service.Introspect(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.False(t, current.Active)
	assert.Empty(t, current.Entitlements)
	assert.NotEqual(t, initial.EntitlementVersion, current.EntitlementVersion)
	request.RequiredEntitlements = model.AppJSONMap{}
	request.RequiredScopes = []string{"model.invoke"}
	denied, err := f.service.Introspect(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.False(t, denied.Active)
	assert.Equal(t, "scope_denied", denied.Reason)
	require.NoError(t, f.db.Model(&f.user).Updates(map[string]any{"username": "renamed-user", "email": "renamed@example.com"}).Error)
	later := f.exchange(t)
	assert.Equal(t, exchanged.Issuer, later.Issuer)
	assert.Equal(t, exchanged.Subject, later.Subject)
}

func TestSessionRevokeContractAndOwnership(t *testing.T) {
	t.Run("contending revoke freezes the original result", func(t *testing.T) {
		f := newAppLaunchFixture(t)
		session := f.exchange(t)
		request := AppPluginSessionRevokeRequest{RequestID: uuid.NewString(), AppKey: f.installation.AppKey,
			AppSessionID: session.AppSessionID, Subject: session.Subject, Reason: "user_logout"}
		var responses [2]AppPluginSessionRevokeResult
		outcomes := contendAppLaunchOperations(t, f, func(ctx context.Context) (err error) {
			responses[0], err = f.service.RevokeSession(ctx, f.serviceID, "contended", request)
			return err
		}, func(ctx context.Context) (err error) {
			responses[1], err = NewAppPluginAuthService(f.db, f.options).RevokeSession(ctx, f.serviceID, "contended", request)
			return err
		})
		require.NoError(t, outcomes[0])
		require.NoError(t, outcomes[1])
		assert.Equal(t, responses[0], responses[1])
		assert.False(t, responses[1].AlreadyRevoked)
	})
	f := newAppLaunchFixture(t)
	exchanged := f.exchange(t)
	other := f.exchange(t)
	request := AppPluginSessionRevokeRequest{RequestID: uuid.NewString(), AppKey: f.installation.AppKey,
		AppSessionID: exchanged.AppSessionID, Subject: exchanged.Subject, Reason: "user_logout"}
	for _, field := range []string{"app", "installation", "subject", "session"} {
		t.Run("wrong "+field+" is not found", func(t *testing.T) {
			bad, identity := request, f.serviceID
			switch field {
			case "app":
				bad.AppKey = "another-app"
			case "installation":
				identity.InstallationID = "another-installation"
			case "subject":
				bad.Subject = "another-subject"
			case "session":
				bad.AppSessionID = "another-session"
			}
			_, err := f.service.RevokeSession(t.Context(), identity, "revoke-wrong-"+field, bad)
			require.ErrorContains(t, err, "not_found")
		})
	}
	first, err := f.service.RevokeSession(t.Context(), f.serviceID, "revoke", request)
	require.NoError(t, err)
	assert.Equal(t, exchanged.AppSessionID, first.AppSessionID)
	assert.Equal(t, "revoked", first.State)
	assert.False(t, first.AlreadyRevoked)
	assert.Equal(t, f.options.Now(), first.RevokedAt)
	f.now.Add(int64(time.Second))
	replayed, err := NewAppPluginAuthService(f.db, f.options).RevokeSession(t.Context(), f.serviceID, "revoke", request)
	require.NoError(t, err)
	assert.Equal(t, first, replayed)
	request.Reason = "changed"
	_, err = f.service.RevokeSession(t.Context(), f.serviceID, "revoke", request)
	require.ErrorContains(t, err, "idempotency_conflict")
	request.Reason = "user_logout"
	again, err := f.service.RevokeSession(t.Context(), f.serviceID, "new-revoke-key", request)
	require.NoError(t, err)
	assert.True(t, again.AlreadyRevoked)
	assert.Equal(t, first.RevokedAt, again.RevokedAt)
	for _, session := range []struct {
		id     string
		active bool
	}{{exchanged.AppSessionID, false}, {other.AppSessionID, true}} {
		result, err := f.service.Introspect(t.Context(), f.serviceID, AppPluginIntrospectRequest{
			AppKey: f.installation.AppKey, AppSessionID: session.id, Subject: exchanged.Subject,
			RequiredScopes: []string{}, RequiredEntitlements: model.AppJSONMap{},
		})
		require.NoError(t, err)
		assert.Equal(t, session.active, result.Active)
	}
	var dashboard model.UserSession
	require.NoError(t, f.db.Where("sid = ?", f.session.SID).First(&dashboard).Error)
	assert.Equal(t, model.UserSessionStatusActive, dashboard.Status)
	assert.EqualValues(t, 7, dashboard.Version)
}

func openAppLaunchTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialect, dsn := os.Getenv("APP_PLUGIN_TEST_DIALECT"), os.Getenv("APP_PLUGIN_TEST_DSN")
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	name := "app_launch_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var db *gorm.DB
	var err error
	switch dialect {
	case "", "sqlite":
		db, err = gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "launch.sqlite")), config)
	case "mysql":
		require.NotEmpty(t, dsn)
		parsed, parseErr := mysqlDriver.ParseDSN(dsn)
		require.NoError(t, parseErr)
		admin, openErr := gorm.Open(mysql.Open(dsn), config)
		require.NoError(t, openErr)
		require.NoError(t, admin.Exec("CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error)
		t.Cleanup(func() {
			assert.NoError(t, admin.Exec("DROP DATABASE `"+name+"`").Error)
			connection, err := admin.DB()
			require.NoError(t, err)
			assert.NoError(t, connection.Close())
		})
		parsed.DBName = name
		db, err = gorm.Open(mysql.Open(parsed.FormatDSN()), config)
	case "postgres", "postgresql":
		require.NotEmpty(t, dsn)
		parsed, parseErr := pgx.ParseConfig(dsn)
		require.NoError(t, parseErr)
		admin, openErr := gorm.Open(postgres.Open(dsn), config)
		require.NoError(t, openErr)
		require.NoError(t, admin.Exec(`CREATE SCHEMA "`+name+`"`).Error)
		t.Cleanup(func() {
			assert.NoError(t, admin.Exec(`DROP SCHEMA "`+name+`" CASCADE`).Error)
			connection, err := admin.DB()
			require.NoError(t, err)
			assert.NoError(t, connection.Close())
		})
		parsed.RuntimeParams["search_path"] = name
		db, err = gorm.Open(postgres.New(postgres.Config{Conn: stdlib.OpenDB(*parsed)}), config)
	default:
		t.Fatalf("unsupported database dialect %q", dialect)
	}
	require.NoError(t, err)
	previousType := common.MainDatabaseType()
	common.SetMainDatabaseType(map[string]common.DatabaseType{
		"sqlite": common.DatabaseTypeSQLite, "mysql": common.DatabaseTypeMySQL, "postgres": common.DatabaseTypePostgreSQL,
	}[db.Dialector.Name()])
	t.Cleanup(func() { common.SetMainDatabaseType(previousType) })
	connection, err := db.DB()
	require.NoError(t, err)
	if db.Dialector.Name() == "sqlite" {
		connection.SetMaxOpenConns(1)
	}
	t.Cleanup(func() { assert.NoError(t, connection.Close()) })
	var version string
	query := "SELECT version()"
	if db.Dialector.Name() == "sqlite" {
		query = "SELECT sqlite_version()"
	}
	require.NoError(t, db.Raw(query).Scan(&version).Error)
	if dialect != "" {
		require.Equal(t, strings.ReplaceAll(dialect, "postgresql", "postgres"), db.Dialector.Name())
		require.Equal(t, os.Getenv("APP_PLUGIN_TEST_DATABASE_VERSION"), version)
		require.NotEmpty(t, os.Getenv("APP_PLUGIN_TEST_DRIVER"))
	}
	t.Log(fmt.Sprintf("database=%s version=%s", db.Dialector.Name(), version))
	return db
}
