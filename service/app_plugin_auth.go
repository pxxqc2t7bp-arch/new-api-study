package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"
	"gorm.io/gorm"
)

type AppPluginAuthError struct{ Code string }

func (e *AppPluginAuthError) Error() string { return e.Code }

func appAuthError(code string) error { return &AppPluginAuthError{Code: code} }

func classifyDBLookup(err error, notFoundCode string) error {
	if err == nil {
		return nil
	}
	var authErr *AppPluginAuthError
	if errors.As(err, &authErr) {
		return err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return appAuthError(notFoundCode)
	}
	return appAuthError("service_unavailable")
}

func appExecutionBoundaryError(err error) error {
	if err == nil {
		return nil
	}
	var authErr *AppPluginAuthError
	if errors.As(err, &authErr) {
		return err
	}
	return appAuthError("service_unavailable")
}

func appServiceIdentityRevalidationError(err error) error {
	if errors.Is(err, model.ErrAppServiceIdentityInvalid) {
		return appAuthError("service_identity_invalid")
	}
	return appAuthError("service_unavailable")
}

type AppPluginAuthOptions struct {
	Issuer             string
	DerivationKey      []byte
	Now                func() time.Time
	CurrentPermissions func(context.Context, *gorm.DB, model.User) (model.AppJSONMap, error)
}

type AppPluginAuthService struct {
	db      *gorm.DB
	options AppPluginAuthOptions
}

func NewAppPluginAuthService(db *gorm.DB, options AppPluginAuthOptions) *AppPluginAuthService {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.CurrentPermissions == nil {
		options.CurrentPermissions = appPluginCurrentPermissions
	}
	options.DerivationKey = slices.Clone(options.DerivationKey)
	return &AppPluginAuthService{db: db, options: options}
}

// Only explicitly configured secrets are accepted. Never fall back to the
// process-random common.CryptoSecret or SessionSecret defaults.
func ConfiguredAppPluginAuthOptions() AppPluginAuthOptions {
	key := os.Getenv("APP_PLUGIN_LAUNCH_SECRET")
	if key == "" {
		key = os.Getenv("CRYPTO_SECRET")
	}
	return AppPluginAuthOptions{Issuer: system_setting.ServerAddress, DerivationKey: []byte(key)}
}

type AppPluginAuthorizeRequest struct {
	Surface             string `json:"surface"`
	TransactionID       string `json:"transaction_id"`
	State               string `json:"state"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
	Nonce               string `json:"nonce"`
}

type AppPluginAuthorizeResult struct {
	LaunchURL string `json:"launch_url"`
	Surface   string `json:"surface"`
	ExpiresIn int    `json:"expires_in"`
}

type AppPluginExchangeRequest struct {
	ExchangeRequestID string `json:"exchange_request_id"`
	AppKey            string `json:"app_key"`
	Surface           string `json:"surface"`
	TransactionID     string `json:"transaction_id"`
	Code              string `json:"code"`
	CodeVerifier      string `json:"code_verifier"`
	State             string `json:"state"`
	Nonce             string `json:"nonce"`
}

type AppPluginExchangeResult struct {
	Issuer             string           `json:"issuer"`
	Subject            string           `json:"subject"`
	UserID             int              `json:"user_id"`
	AppSessionID       string           `json:"app_session_id"`
	AuthVersion        int64            `json:"auth_version"`
	SessionVersion     int64            `json:"session_version"`
	GrantedScopes      []string         `json:"granted_scopes"`
	Entitlements       model.AppJSONMap `json:"entitlements"`
	EntitlementVersion string           `json:"entitlement_version"`
	IssuedAt           time.Time        `json:"issued_at"`
	UpstreamExpiresAt  time.Time        `json:"upstream_expires_at"`
}

type AppPluginIntrospectRequest struct {
	AppKey               string           `json:"app_key"`
	AppSessionID         string           `json:"app_session_id"`
	Subject              string           `json:"subject"`
	RequiredScopes       []string         `json:"required_scopes"`
	RequiredEntitlements model.AppJSONMap `json:"required_entitlements"`
}

type AppPluginIntrospectResult struct {
	Active             bool             `json:"active"`
	Reason             string           `json:"reason"`
	UserStatus         string           `json:"user_status"`
	AppStatus          string           `json:"app_status"`
	AuthVersion        int64            `json:"auth_version"`
	SessionVersion     int64            `json:"session_version"`
	GrantedScopes      []string         `json:"granted_scopes"`
	Entitlements       model.AppJSONMap `json:"entitlements"`
	EntitlementVersion string           `json:"entitlement_version"`
	CheckedAt          time.Time        `json:"checked_at"`
	ModelPolicyVersion int64            `json:"-"`
}

type AppPluginLaunchContext struct {
	AppKey   string `json:"app_key"`
	Surface  string `json:"surface"`
	StartURL string `json:"start_url"`
	Origin   string `json:"origin"`
}

func (s *AppPluginAuthService) LaunchContext(ctx context.Context, identity AuthIdentity, appKey, surface string) (AppPluginLaunchContext, error) {
	if !appControlOpaque(appKey, 128) || (surface != "direct" && surface != "embedded") {
		return AppPluginLaunchContext{}, appAuthError("invalid_request")
	}
	var result AppPluginLaunchContext
	err := model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		result = AppPluginLaunchContext{}
		installation, err := model.LockAppPluginInstallation(tx, appKey, "")
		if err != nil {
			return classifyDBLookup(err, "not_found")
		}
		if installation.AppKey != appKey {
			return appAuthError("not_found")
		}
		user, dashboard, err := AppPluginDashboardIdentity(tx, identity, s.options.Now())
		if err != nil {
			return err
		}
		if dashboard.SID != identity.SessionID {
			return appAuthError("unauthenticated")
		}
		if installation.Status != model.AppInstallationStatusEnabled {
			return appAuthError("app_plugin_disabled")
		}
		if len(installation.AllowedUserPolicy.Groups) != 0 && !slices.Contains(installation.AllowedUserPolicy.Groups, user.Group) {
			return appAuthError("not_found")
		}
		manifest, _, err := ValidateAppPluginRegistration(tx, installation)
		if err != nil {
			return classifyDBLookup(err, "not_found")
		}
		if !slices.Contains(installation.EnabledSurfaces, surface) {
			return appAuthError("forbidden")
		}
		startPath := manifest.Surfaces.Direct.StartPath
		if surface == "embedded" {
			parent, err := url.Parse(s.options.Issuer)
			if err != nil || !AppPluginTrustedOrigin(s.options.Issuer, "https://"+parent.Host) ||
				!slices.Contains(installation.AllowedParentOrigins, "https://"+parent.Host) ||
				!AppPluginSameSiteHTTPS(installation.BaseURL, "https://"+parent.Host) {
				return appAuthError("forbidden")
			}
			startPath = manifest.Surfaces.Embedded.StartPath
		}
		endpoint := absoluteManifestEndpoint(installation.BaseURL, startPath)
		var claim model.AppRouteClaim
		if err := model.AppPluginCurrentRead(tx).Where("installation_id = ? AND kind = ?", installation.InstallationID, surface).First(&claim).Error; err != nil {
			return classifyDBLookup(err, "not_found")
		}
		if claim.AppKey != appKey || claim.InstallationID != installation.InstallationID || claim.AbsoluteEndpoint != endpoint {
			return appAuthError("not_found")
		}
		target, err := url.Parse(endpoint)
		if err != nil {
			return appAuthError("service_unavailable")
		}
		result = AppPluginLaunchContext{AppKey: appKey, Surface: surface, StartURL: endpoint, Origin: target.Scheme + "://" + target.Host}
		return nil
	})
	return result, appExecutionBoundaryError(err)
}

type AppPluginSessionRevokeRequest struct {
	RequestID    string `json:"request_id"`
	AppKey       string `json:"app_key"`
	AppSessionID string `json:"app_session_id"`
	Subject      string `json:"subject"`
	Reason       string `json:"reason"`
}

type AppPluginSessionRevokeResult struct {
	AppSessionID   string    `json:"app_session_id"`
	State          string    `json:"state"`
	RevokedAt      time.Time `json:"revoked_at"`
	AlreadyRevoked bool      `json:"already_revoked"`
}

func appPluginHash(value any) (string, error) {
	data, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	return serviceDigestBytes(data), nil
}

func appPluginOpaque(value string, max int) bool {
	return value != "" && len(value) <= max && !strings.ContainsAny(value, "\x00\r\n\t ") && strings.TrimSpace(value) == value
}

func appPluginChallenge(value string) bool {
	data, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(data) == 32 && len(value) == 43
}

func AppPluginTrustedOrigin(issuer, origin string) bool {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	return origin == "https://"+parsed.Host
}

func AppPluginSameSiteHTTPS(base, parent string) bool {
	b, err := url.Parse(base)
	p, parentErr := url.Parse(parent)
	if err != nil || parentErr != nil || b.Scheme != "https" || p.Scheme != "https" ||
		b.Hostname() == "" || p.Hostname() == "" || b.User != nil || p.User != nil ||
		p.Path != "" || p.RawQuery != "" || p.Fragment != "" || p.ForceQuery {
		return false
	}
	if b.Hostname() == p.Hostname() {
		return true
	}
	bSite, err := publicsuffix.EffectiveTLDPlusOne(b.Hostname())
	pSite, parentErr := publicsuffix.EffectiveTLDPlusOne(p.Hostname())
	return err == nil && parentErr == nil && bSite == pSite
}

// ValidateAppPluginRegistration uses only the generation and route claims read
// after the installation lock, and checks the exact host-owned callback.
func ValidateAppPluginRegistration(tx *gorm.DB, installation model.AppInstallation) (AppManifest, string, error) {
	version, err := model.GetAppInstallationVersion(tx, installation)
	if err != nil {
		return AppManifest{}, "", err
	}
	manifest, err := ValidateAppManifest([]byte(version.CanonicalManifestJSON))
	if err != nil || version.ManifestSHA256 != serviceDigestBytes([]byte(version.CanonicalManifestJSON)) ||
		version.ManifestSHA256 != installation.ManifestSHA256 || manifest.Key != installation.AppKey {
		return AppManifest{}, "", appAuthError("forbidden")
	}
	base, err := normalizeHTTPSBaseURL(installation.BaseURL)
	if err != nil || base != installation.BaseURL {
		return AppManifest{}, "", appAuthError("forbidden")
	}
	callback := absoluteManifestEndpoint(base, manifest.CallbackPath)
	expectedClaimKey := serviceDigestBytes([]byte("route\x00" + callback))
	var claims []model.AppRouteClaim
	query := model.AppPluginCurrentRead(tx).Where(
		"claim_key = ? OR (installation_id = ? AND kind = ?)",
		expectedClaimKey, installation.InstallationID, "callback",
	).Limit(2).Find(&claims)
	if query.Error != nil {
		return AppManifest{}, "", query.Error
	}
	if len(claims) == 0 {
		return AppManifest{}, "", gorm.ErrRecordNotFound
	}
	if len(claims) != 1 ||
		claims[0].InstallationID != installation.InstallationID ||
		claims[0].AppKey != installation.AppKey ||
		claims[0].Kind != "callback" ||
		claims[0].AbsoluteEndpoint != callback ||
		claims[0].ClaimKey != expectedClaimKey {
		return AppManifest{}, "", appAuthError("forbidden")
	}
	return manifest, callback, nil
}

func AppPluginDashboardIdentity(tx *gorm.DB, identity AuthIdentity, now time.Time) (model.User, model.UserSession, error) {
	var user model.User
	var session model.UserSession
	if identity.UserID <= 0 || identity.SessionID == "" || identity.UserAuthVersion <= 0 || identity.SessionVersion <= 0 {
		return user, session, appAuthError("unauthenticated")
	}
	if err := model.AppPluginCurrentRead(tx).Where("id = ?", identity.UserID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return user, session, appAuthError("unauthenticated")
		}
		return user, session, err
	}
	if user.Status != common.UserStatusEnabled {
		return user, session, appAuthError("identity_inactive")
	}
	if err := model.AppPluginCurrentRead(tx).Where("sid = ? AND user_id = ?", identity.SessionID, identity.UserID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return user, session, appAuthError("unauthenticated")
		}
		return user, session, err
	}
	if user.AuthVersion != identity.UserAuthVersion || session.UserAuthVersion != identity.UserAuthVersion ||
		session.Version != identity.SessionVersion || session.Status != model.UserSessionStatusActive ||
		session.RevokedAt != 0 || session.ExpiresAt <= now.Unix() {
		return user, session, appAuthError("unauthenticated")
	}
	return user, session, nil
}

func (s *AppPluginAuthService) launchCode(row model.AppPluginLaunchCode) string {
	key := hmac.New(sha256.New, s.options.DerivationKey)
	key.Write([]byte("new-api/app-plugin/launch-key/v1"))
	mac := hmac.New(sha256.New, key.Sum(nil))
	// Fixed-size salt followed by the canonical JSON binding is unambiguous.
	mac.Write([]byte(row.Salt))
	mac.Write([]byte(row.BindingJSON))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *AppPluginAuthService) validLaunchCode(row model.AppPluginLaunchCode, code string) bool {
	codeMatches := hmac.Equal([]byte(s.launchCode(row)), []byte(code))
	hashMatches := hmac.Equal([]byte(row.CodeHash), []byte(serviceDigestBytes([]byte(code))))
	return codeMatches && hashMatches
}

func (s *AppPluginAuthService) exchangeReplayMAC(replay model.AppPluginExchangeReplay) string {
	key := hmac.New(sha256.New, s.options.DerivationKey)
	key.Write([]byte("new-api/app-plugin/exchange-replay-key/v1"))
	mac := hmac.New(sha256.New, key.Sum(nil))
	var encoded [8]byte
	launchCodeHash, appSessionID := "", ""
	if replay.LaunchCodeHash != nil {
		launchCodeHash = *replay.LaunchCodeHash
	}
	if replay.AppSessionID != nil {
		appSessionID = *replay.AppSessionID
	}
	for _, value := range []string{
		replay.ScopeHash,
		replay.RequestHash,
		replay.InstallationID,
		launchCodeHash,
		appSessionID,
	} {
		binary.BigEndian.PutUint64(encoded[:], uint64(len(value)))
		mac.Write(encoded[:])
		mac.Write([]byte(value))
	}
	binary.BigEndian.PutUint64(encoded[:], uint64(replay.ExpiresAt))
	mac.Write(encoded[:])
	binary.BigEndian.PutUint64(encoded[:], uint64(len(replay.ResponseJSON)))
	mac.Write(encoded[:])
	mac.Write([]byte(replay.ResponseJSON))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *AppPluginAuthService) authenticateExchangeReplay(
	tx *gorm.DB,
	replay model.AppPluginExchangeReplay,
	code model.AppPluginLaunchCode,
	installation model.AppInstallation,
	request AppPluginExchangeRequest,
) (AppPluginExchangeResult, model.AppPluginSession, error) {
	var result AppPluginExchangeResult
	var session model.AppPluginSession
	expectedMAC := s.exchangeReplayMAC(replay)
	requestCodeHash := serviceDigestBytes([]byte(request.Code))
	if replay.LaunchCodeHash == nil || replay.AppSessionID == nil ||
		*replay.LaunchCodeHash == "" || *replay.AppSessionID == "" ||
		!hmac.Equal([]byte(replay.ResponseMAC), []byte(expectedMAC)) ||
		len(replay.ScopeHash) != 64 || len(replay.RequestHash) != 64 ||
		replay.InstallationID != installation.InstallationID ||
		!hmac.Equal([]byte(*replay.LaunchCodeHash), []byte(requestCodeHash)) ||
		!hmac.Equal([]byte(*replay.LaunchCodeHash), []byte(code.CodeHash)) ||
		*replay.AppSessionID != code.AppSessionID ||
		!s.validLaunchCode(code, request.Code) {
		return result, session, appAuthError("not_found")
	}
	var binding model.AppPluginLaunchBinding
	if err := common.UnmarshalJsonStr(code.BindingJSON, &binding); err != nil {
		return result, session, appAuthError("not_found")
	}
	challenge := sha256.Sum256([]byte(request.CodeVerifier))
	upstreamExpiry := time.Unix(binding.UpstreamExpiresAt, 0).UTC()
	expectedReplayExpiry := code.ConsumedAt + int64(5*time.Minute)
	if code.ConsumedAt <= 0 ||
		code.ConsumedAt >= code.ExpiresAt ||
		code.ConsumedAt >= upstreamExpiry.UnixNano() ||
		expectedReplayExpiry < code.ConsumedAt ||
		replay.ExpiresAt != expectedReplayExpiry {
		return result, session, appAuthError("not_found")
	}
	_, callback, err := ValidateAppPluginRegistration(tx, installation)
	if err != nil {
		var authErr *AppPluginAuthError
		if errors.As(err, &authErr) && authErr.Code == "forbidden" {
			return result, session, appAuthError("not_found")
		}
		return result, session, classifyDBLookup(err, "not_found")
	}
	if code.InstallationID != installation.InstallationID ||
		code.ExpiresAt != binding.ExpiresAt ||
		binding.InstallationID != installation.InstallationID ||
		binding.AppKey != request.AppKey ||
		binding.AppKey != installation.AppKey ||
		binding.Generation != installation.AppVersionID ||
		binding.Callback != callback ||
		binding.Issuer != s.options.Issuer ||
		binding.Surface != request.Surface ||
		!slices.Contains(installation.EnabledSurfaces, binding.Surface) ||
		binding.TransactionID != request.TransactionID ||
		!hmac.Equal([]byte(binding.StateHash), []byte(serviceDigestBytes([]byte(request.State)))) ||
		!hmac.Equal([]byte(binding.NonceHash), []byte(serviceDigestBytes([]byte(request.Nonce)))) ||
		!hmac.Equal([]byte(binding.CodeChallenge), []byte(base64.RawURLEncoding.EncodeToString(challenge[:]))) {
		return result, session, appAuthError("not_found")
	}
	if err := common.UnmarshalJsonStr(replay.ResponseJSON, &result); err != nil {
		return AppPluginExchangeResult{}, session, appAuthError("not_found")
	}
	if err := model.AppPluginCurrentRead(tx).Where(
		"app_session_id = ? AND installation_id = ?",
		*replay.AppSessionID, installation.InstallationID,
	).First(&session).Error; err != nil {
		return AppPluginExchangeResult{}, session, classifyDBLookup(err, "not_found")
	}
	if session.AppSessionID != *replay.AppSessionID ||
		session.AppSessionID != result.AppSessionID ||
		session.InstallationID != binding.InstallationID ||
		session.InstallationID != installation.InstallationID ||
		session.AppKey != binding.AppKey ||
		session.AppKey != installation.AppKey ||
		session.Generation != binding.Generation ||
		session.Generation != installation.AppVersionID ||
		session.Issuer != binding.Issuer ||
		session.Issuer != result.Issuer ||
		session.UserID != binding.UserID ||
		session.UserID != result.UserID ||
		session.DashboardSessionID != binding.DashboardSessionID ||
		session.AuthVersion != binding.AuthVersion ||
		session.AuthVersion != result.AuthVersion ||
		session.SessionVersion != binding.SessionVersion ||
		session.SessionVersion != result.SessionVersion ||
		session.Subject != "user_"+strconv.Itoa(binding.UserID) ||
		session.Subject != result.Subject ||
		!slices.Equal(session.GrantedScopes, result.GrantedScopes) ||
		session.UpstreamExpiresAt != binding.UpstreamExpiresAt ||
		!result.UpstreamExpiresAt.Equal(upstreamExpiry) ||
		result.Entitlements == nil ||
		result.EntitlementVersion == "" ||
		!result.IssuedAt.Equal(time.Unix(0, code.ConsumedAt).UTC()) {
		return AppPluginExchangeResult{}, model.AppPluginSession{}, appAuthError("not_found")
	}
	return result, session, nil
}

func (s *AppPluginAuthService) Authorize(ctx context.Context, identity AuthIdentity, appKey, origin, key string, request AppPluginAuthorizeRequest) (AppPluginAuthorizeResult, error) {
	if identity.SessionID == "" {
		return AppPluginAuthorizeResult{}, appAuthError("unauthenticated")
	}
	if !appPluginOpaque(key, 255) || !appPluginOpaque(request.TransactionID, 255) ||
		(request.Surface != "direct" && request.Surface != "embedded") ||
		request.CodeChallengeMethod != "S256" || !appPluginChallenge(request.CodeChallenge) ||
		!appPluginChallenge(request.State) || !appPluginChallenge(request.Nonce) {
		return AppPluginAuthorizeResult{}, appAuthError("invalid_request")
	}
	if !AppPluginTrustedOrigin(s.options.Issuer, origin) {
		return AppPluginAuthorizeResult{}, appAuthError("forbidden")
	}
	if len(s.options.DerivationKey) < 32 {
		return AppPluginAuthorizeResult{}, appAuthError("identity_not_configured")
	}
	scope, err := appPluginHash([]any{"authorize/v1", identity.UserID, identity.SessionID, identity.UserAuthVersion, identity.SessionVersion, appKey, key})
	if err != nil {
		return AppPluginAuthorizeResult{}, err
	}
	hash, err := appPluginHash(request)
	if err != nil {
		return AppPluginAuthorizeResult{}, err
	}
	var result AppPluginAuthorizeResult
	err = model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		result = AppPluginAuthorizeResult{}
		now := s.options.Now().UTC()
		installation, err := model.LockAppPluginInstallation(tx, appKey, "")
		if err != nil {
			return classifyDBLookup(err, "not_found")
		}
		user, dashboard, err := AppPluginDashboardIdentity(tx, identity, now)
		if err != nil {
			return err
		}
		if installation.Status != model.AppInstallationStatusEnabled {
			return appAuthError("app_plugin_disabled")
		}
		if len(installation.AllowedUserPolicy.Groups) != 0 && !slices.Contains(installation.AllowedUserPolicy.Groups, user.Group) {
			return appAuthError("not_found")
		}
		_, callback, err := ValidateAppPluginRegistration(tx, installation)
		if err != nil {
			return classifyDBLookup(err, "not_found")
		}
		if !slices.Contains(installation.EnabledSurfaces, request.Surface) ||
			(request.Surface == "embedded" && (!slices.Contains(installation.AllowedParentOrigins, origin) || !AppPluginSameSiteHTTPS(installation.BaseURL, origin))) {
			return appAuthError("forbidden")
		}
		var row model.AppPluginLaunchCode
		q := model.AppPluginCurrentRead(tx).Where("scope_hash = ?", scope).Limit(1).Find(&row)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 1 {
			if row.RequestHash != hash {
				return appAuthError("idempotency_conflict")
			}
			if row.ConsumedAt != 0 {
				return appAuthError("launch_code_replayed")
			}
			if row.ExpiresAt <= now.UnixNano() {
				return appAuthError("launch_code_expired")
			}
			var binding model.AppPluginLaunchBinding
			if err := common.UnmarshalJsonStr(row.BindingJSON, &binding); err != nil {
				return err
			}
			if binding.Generation != installation.AppVersionID || binding.InstallationID != installation.InstallationID ||
				binding.Callback != callback || binding.Issuer != s.options.Issuer {
				return appAuthError("forbidden")
			}
		} else {
			salt, err := model.NewAppPluginOpaqueID()
			if err != nil {
				return err
			}
			binding := model.AppPluginLaunchBinding{
				Issuer: s.options.Issuer, UserID: identity.UserID, DashboardSessionID: identity.SessionID,
				AuthVersion: identity.UserAuthVersion, SessionVersion: identity.SessionVersion,
				InstallationID: installation.InstallationID, AppKey: appKey, Generation: installation.AppVersionID,
				Callback: callback, Surface: request.Surface, TransactionID: request.TransactionID,
				StateHash: serviceDigestBytes([]byte(request.State)), NonceHash: serviceDigestBytes([]byte(request.Nonce)),
				CodeChallenge: request.CodeChallenge, UpstreamExpiresAt: dashboard.ExpiresAt, ExpiresAt: now.Add(time.Minute).UnixNano(),
			}
			raw, err := common.Marshal(binding)
			if err != nil {
				return err
			}
			row = model.AppPluginLaunchCode{ScopeHash: scope, RequestHash: hash, InstallationID: installation.InstallationID,
				Salt: salt, BindingJSON: string(raw), ExpiresAt: binding.ExpiresAt}
			row.CodeHash = serviceDigestBytes([]byte(s.launchCode(row)))
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		code := s.launchCode(row)
		if !hmac.Equal([]byte(row.CodeHash), []byte(serviceDigestBytes([]byte(code)))) {
			return appAuthError("identity_not_configured")
		}
		target, err := url.Parse(callback)
		if err != nil {
			return err
		}
		query := url.Values{"code": {code}, "state": {request.State}}
		target.RawQuery = query.Encode()
		result = AppPluginAuthorizeResult{LaunchURL: target.String(), Surface: request.Surface, ExpiresIn: 60}
		return nil
	})
	return result, appExecutionBoundaryError(err)
}

func (s *AppPluginAuthService) Exchange(ctx context.Context, service model.AppServiceIdentity, request AppPluginExchangeRequest) (AppPluginExchangeResult, error) {
	if _, err := uuid.Parse(request.ExchangeRequestID); err != nil || !appPluginOpaque(request.Code, 128) ||
		!appPluginOpaque(request.TransactionID, 255) || !appPluginChallenge(request.State) || !appPluginChallenge(request.Nonce) ||
		len(request.CodeVerifier) < 43 || len(request.CodeVerifier) > 128 ||
		strings.ContainsFunc(request.CodeVerifier, func(r rune) bool {
			return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r))
		}) {
		return AppPluginExchangeResult{}, appAuthError("invalid_request")
	}
	if request.AppKey != service.AppKey {
		return AppPluginExchangeResult{}, appAuthError("not_found")
	}
	scope, err := appPluginHash([]string{"exchange/v1", service.InstallationID, service.CredentialID, service.Version, request.ExchangeRequestID})
	if err != nil {
		return AppPluginExchangeResult{}, err
	}
	hash, err := appPluginHash(request)
	if err != nil {
		return AppPluginExchangeResult{}, err
	}
	var result AppPluginExchangeResult
	var rejection error
	err = model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		result, rejection = AppPluginExchangeResult{}, nil
		now := s.options.Now().UTC()
		installation, err := model.LockAppPluginInstallation(tx, service.AppKey, service.InstallationID)
		if err != nil {
			return classifyDBLookup(err, "not_found")
		}
		currentService, err := model.ValidateAppServiceIdentity(tx, service, now)
		if err != nil {
			return appServiceIdentityRevalidationError(err)
		}
		if !slices.Contains(currentService.Scopes, "identity.read") {
			return appAuthError("scope_denied")
		}
		var replay model.AppPluginExchangeReplay
		q := model.AppPluginCurrentRead(tx).Where("scope_hash = ?", scope).Limit(1).Find(&replay)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 1 {
			if replay.ScopeHash != scope ||
				!hmac.Equal([]byte(replay.ResponseMAC), []byte(s.exchangeReplayMAC(replay))) {
				return appAuthError("not_found")
			}
			if replay.RequestHash != hash {
				return appAuthError("idempotency_conflict")
			}
			var code model.AppPluginLaunchCode
			if err := model.AppPluginCurrentRead(tx).Where(
				"code_hash = ? AND installation_id = ?",
				serviceDigestBytes([]byte(request.Code)), service.InstallationID,
			).First(&code).Error; err != nil {
				return classifyDBLookup(err, "not_found")
			}
			var replayErr error
			result, _, replayErr = s.authenticateExchangeReplay(
				tx, replay, code, installation, request,
			)
			if replayErr != nil {
				return replayErr
			}
			if replay.ExpiresAt <= now.UnixNano() {
				return appAuthError("launch_code_replayed")
			}
			return nil
		}
		var code model.AppPluginLaunchCode
		if err := model.AppPluginCurrentRead(tx).Where("code_hash = ? AND installation_id = ?",
			serviceDigestBytes([]byte(request.Code)), service.InstallationID).First(&code).Error; err != nil {
			return classifyDBLookup(err, "not_found")
		}
		if !s.validLaunchCode(code, request.Code) {
			return appAuthError("not_found")
		}
		if code.ConsumedAt != 0 {
			var originalReplays []model.AppPluginExchangeReplay
			q := model.AppPluginCurrentRead(tx).Where(
				"installation_id = ? AND launch_code_hash = ?",
				service.InstallationID, code.CodeHash,
			).Limit(2).Find(&originalReplays)
			if q.Error != nil {
				return q.Error
			}
			if len(originalReplays) != 1 {
				return appAuthError("not_found")
			}
			originalReplay := originalReplays[0]
			if originalReplay.ScopeHash == scope {
				return appAuthError("not_found")
			}
			authenticated, session, replayErr := s.authenticateExchangeReplay(
				tx, originalReplay, code, installation, request,
			)
			if replayErr != nil {
				return replayErr
			}
			if originalReplay.RequestHash == hash {
				if originalReplay.ExpiresAt <= now.UnixNano() {
					return appAuthError("launch_code_replayed")
				}
				result = authenticated
				return nil
			}
			if session.RevokedAt == 0 {
				update := tx.Model(&model.AppPluginSession{}).Where(
					"app_session_id = ? AND installation_id = ? AND revoked_at = ?",
					session.AppSessionID, installation.InstallationID, 0,
				).Update("revoked_at", now.UnixNano())
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return errors.New("exchange session revocation affected unexpected row count")
				}
			}
			rejection = appAuthError("launch_code_replayed")
			return nil
		}
		var binding model.AppPluginLaunchBinding
		if err := common.UnmarshalJsonStr(code.BindingJSON, &binding); err != nil {
			return appAuthError("not_found")
		}
		manifest, callback, err := ValidateAppPluginRegistration(tx, installation)
		if err != nil {
			return classifyDBLookup(err, "not_found")
		}
		if binding.InstallationID != service.InstallationID || binding.AppKey != request.AppKey ||
			binding.Generation != installation.AppVersionID || binding.Callback != callback ||
			binding.Surface != request.Surface || binding.TransactionID != request.TransactionID ||
			binding.Issuer != s.options.Issuer || binding.ExpiresAt != code.ExpiresAt {
			return appAuthError("not_found")
		}
		if !hmac.Equal([]byte(binding.StateHash), []byte(serviceDigestBytes([]byte(request.State)))) {
			return appAuthError("state_mismatch")
		}
		if !hmac.Equal([]byte(binding.NonceHash), []byte(serviceDigestBytes([]byte(request.Nonce)))) {
			return appAuthError("nonce_mismatch")
		}
		challenge := sha256.Sum256([]byte(request.CodeVerifier))
		if !hmac.Equal([]byte(binding.CodeChallenge), []byte(base64.RawURLEncoding.EncodeToString(challenge[:]))) {
			return appAuthError("pkce_verification_failed")
		}
		if code.ExpiresAt <= now.UnixNano() {
			return appAuthError("launch_code_expired")
		}
		if installation.Status != model.AppInstallationStatusEnabled || !slices.Contains(installation.EnabledSurfaces, binding.Surface) {
			return appAuthError("app_plugin_disabled")
		}
		user, dashboard, err := AppPluginDashboardIdentity(tx, AuthIdentity{UserID: binding.UserID,
			SessionID: binding.DashboardSessionID, UserAuthVersion: binding.AuthVersion, SessionVersion: binding.SessionVersion}, now)
		if err != nil {
			return err
		}
		if dashboard.ExpiresAt != binding.UpstreamExpiresAt {
			return appAuthError("unauthenticated")
		}
		if len(installation.AllowedUserPolicy.Groups) != 0 && !slices.Contains(installation.AllowedUserPolicy.Groups, user.Group) {
			return appAuthError("not_found")
		}
		scopes := appPluginScopeIntersection(manifest.RequestedScopes, currentService.Scopes)
		if !slices.Contains(scopes, "identity.read") {
			return appAuthError("scope_denied")
		}
		entitlements, version, err := s.entitlements(ctx, tx, installation, user)
		if err != nil {
			return err
		}
		id, err := model.NewAppPluginOpaqueID()
		if err != nil {
			return err
		}
		session := model.AppPluginSession{AppSessionID: id, InstallationID: installation.InstallationID, AppKey: installation.AppKey,
			Generation: installation.AppVersionID, Issuer: binding.Issuer, Subject: "user_" + strconv.Itoa(user.Id),
			UserID: user.Id, DashboardSessionID: dashboard.SID, AuthVersion: binding.AuthVersion, SessionVersion: binding.SessionVersion,
			GrantedScopes: scopes, UpstreamExpiresAt: binding.UpstreamExpiresAt}
		if session.UpstreamExpiresAt <= now.Unix() {
			return appAuthError("unauthenticated")
		}
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		update := tx.Model(&model.AppPluginLaunchCode{}).Where("id = ? AND consumed_at = ?", code.ID, 0).
			Updates(map[string]any{"consumed_at": now.UnixNano(), "app_session_id": id})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return appAuthError("launch_code_replayed")
		}
		result = AppPluginExchangeResult{Issuer: session.Issuer, Subject: session.Subject, UserID: session.UserID,
			AppSessionID: id, AuthVersion: session.AuthVersion, SessionVersion: session.SessionVersion,
			GrantedScopes: scopes, Entitlements: entitlements, EntitlementVersion: version, IssuedAt: now,
			UpstreamExpiresAt: time.Unix(session.UpstreamExpiresAt, 0).UTC()}
		raw, err := common.Marshal(result)
		if err != nil {
			return err
		}
		launchCodeHash, appSessionID := code.CodeHash, session.AppSessionID
		replay = model.AppPluginExchangeReplay{
			ScopeHash: scope, RequestHash: hash, InstallationID: service.InstallationID,
			LaunchCodeHash: &launchCodeHash, AppSessionID: &appSessionID,
			ResponseJSON: string(raw), ExpiresAt: now.Add(5 * time.Minute).UnixNano(),
		}
		replay.ResponseMAC = s.exchangeReplayMAC(replay)
		return tx.Create(&replay).Error
	})
	if err != nil {
		return AppPluginExchangeResult{}, appExecutionBoundaryError(err)
	}
	return result, rejection
}

func appPluginScopeIntersection(requested, allowed []string) []string {
	result := []string{}
	for _, scope := range requested {
		if slices.Contains(allowed, scope) {
			result = append(result, scope)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func (s *AppPluginAuthService) entitlements(ctx context.Context, tx *gorm.DB, installation model.AppInstallation, user model.User) (model.AppJSONMap, string, error) {
	if installation.EntitlementPolicyID == "" {
		return model.AppJSONMap{}, "none", nil
	}
	var policy model.AppEntitlementPolicy
	if err := model.AppPluginCurrentRead(tx).Where("id = ?", installation.EntitlementPolicyID).First(&policy).Error; err != nil {
		return nil, "", err
	}
	permissions, err := s.options.CurrentPermissions(ctx, tx, user)
	if err != nil {
		return nil, "", err
	}
	return intersectRules(policy.EffectiveRules, permissions), policy.ID, nil
}

func (s *AppPluginAuthService) Introspect(ctx context.Context, service model.AppServiceIdentity, request AppPluginIntrospectRequest) (AppPluginIntrospectResult, error) {
	if request.AppKey != service.AppKey || request.AppSessionID == "" || request.Subject == "" {
		return AppPluginIntrospectResult{}, appAuthError("not_found")
	}
	var result AppPluginIntrospectResult
	err := model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		now := s.options.Now().UTC()
		result = AppPluginIntrospectResult{Reason: "unauthenticated", CheckedAt: now,
			UserStatus: "inactive", GrantedScopes: []string{}, Entitlements: model.AppJSONMap{}, EntitlementVersion: "none"}
		installation, err := model.LockAppPluginInstallation(tx, service.AppKey, service.InstallationID)
		if err != nil {
			return classifyDBLookup(err, "not_found")
		}
		currentService, err := model.ValidateAppServiceIdentity(tx, service, now)
		if err != nil {
			return appServiceIdentityRevalidationError(err)
		}
		if !slices.Contains(currentService.Scopes, "identity.read") {
			return appAuthError("scope_denied")
		}
		var session model.AppPluginSession
		if err := model.AppPluginCurrentRead(tx).Where("app_session_id = ? AND installation_id = ? AND app_key = ? AND subject = ?",
			request.AppSessionID, service.InstallationID, request.AppKey, request.Subject).First(&session).Error; err != nil {
			return classifyDBLookup(err, "not_found")
		}
		if session.AppSessionID != request.AppSessionID || session.Subject != request.Subject ||
			session.AppKey != request.AppKey || session.InstallationID != service.InstallationID ||
			installation.AppKey != service.AppKey || installation.InstallationID != service.InstallationID {
			return appAuthError("not_found")
		}
		result.AppStatus = installation.Status
		result.AuthVersion, result.SessionVersion = session.AuthVersion, session.SessionVersion
		if installation.Status == model.AppInstallationStatusRevoked {
			result.Reason = "app_plugin_revoked"
			return nil
		}
		user, dashboard, identityErr := AppPluginDashboardIdentity(tx, AuthIdentity{UserID: session.UserID,
			SessionID: session.DashboardSessionID, UserAuthVersion: session.AuthVersion, SessionVersion: session.SessionVersion}, now)
		if user.Id != 0 {
			result.AuthVersion = user.AuthVersion
			if user.Status == common.UserStatusEnabled {
				result.UserStatus = "active"
			}
		}
		if dashboard.SID != "" {
			result.SessionVersion = dashboard.Version
		}
		if identityErr != nil {
			var authErr *AppPluginAuthError
			if !errors.As(identityErr, &authErr) {
				return identityErr
			}
			result.Reason = authErr.Code
			return nil
		}
		if dashboard.SID != session.DashboardSessionID || dashboard.UserID != session.UserID || user.Id != session.UserID {
			return nil
		}
		if session.RevokedAt != 0 || session.UpstreamExpiresAt <= now.Unix() ||
			session.Generation != installation.AppVersionID || session.Issuer != s.options.Issuer {
			return nil
		}
		if len(installation.AllowedUserPolicy.Groups) != 0 && !slices.Contains(installation.AllowedUserPolicy.Groups, user.Group) {
			result.Reason = "scope_denied"
			return nil
		}
		manifest, _, err := ValidateAppPluginRegistration(tx, installation)
		if err != nil {
			return classifyDBLookup(err, "not_found")
		}
		result.GrantedScopes = appPluginScopeIntersection(session.GrantedScopes, appPluginScopeIntersection(manifest.RequestedScopes, currentService.Scopes))
		result.Entitlements, result.EntitlementVersion, err = s.entitlements(ctx, tx, installation, user)
		if err != nil {
			return err
		}
		result.Reason = ""
		if installation.Status == model.AppInstallationStatusDisabled {
			result.Reason = "app_plugin_disabled"
			result.GrantedScopes = appPluginScopeIntersection(result.GrantedScopes, []string{"identity.read", "task.read"})
			readOnly := model.AppJSONMap{}
			for resource, actions := range result.Entitlements {
				if slices.Contains(actions, "read") {
					readOnly[resource] = []string{"read"}
				}
			}
			result.Entitlements = readOnly
		}
		if len(appPluginScopeIntersection(request.RequiredScopes, result.GrantedScopes)) != len(normalizeStringSet(request.RequiredScopes)) {
			result.Reason = "scope_denied"
			return nil
		}
		for resource, actions := range request.RequiredEntitlements {
			for _, action := range actions {
				if !slices.Contains(result.Entitlements[resource], action) {
					result.Reason = "scope_denied"
					return nil
				}
			}
		}
		if installation.Status == model.AppInstallationStatusEnabled && slices.Contains(result.GrantedScopes, "model.invoke") {
			// Distinguish an unconfigured head from a dangling/corrupt version.
			var head model.Option
			q := model.AppPluginCurrentRead(tx).Where(map[string]any{"key": model.AppModelInvokePolicyKey}).Limit(1).Find(&head)
			if q.Error != nil {
				return q.Error
			}
			if q.RowsAffected != 0 {
				policy, err := model.GetAppExecutionPolicyTx(tx, model.AppModelInvokePolicyKey)
				var document AppModelInvokePolicy
				if err != nil || decodeAppExecutionJSON([]byte(policy.CanonicalJSON), &document) != nil {
					return appAuthError("service_unavailable")
				}
				result.ModelPolicyVersion = policy.Version
			}
		}
		result.Active = true
		return nil
	})
	return result, appExecutionBoundaryError(err)
}

func (s *AppPluginAuthService) RevokeSession(ctx context.Context, service model.AppServiceIdentity, key string, request AppPluginSessionRevokeRequest) (AppPluginSessionRevokeResult, error) {
	if request.AppKey != service.AppKey || request.AppSessionID == "" || request.Subject == "" {
		return AppPluginSessionRevokeResult{}, appAuthError("not_found")
	}
	if _, err := uuid.Parse(request.RequestID); err != nil || !appPluginOpaque(key, 255) || !appPluginOpaque(request.Reason, 128) {
		return AppPluginSessionRevokeResult{}, appAuthError("invalid_request")
	}
	scope, err := appPluginHash([]string{"session-revoke/v1", service.InstallationID, service.CredentialID, service.Version, key})
	if err != nil {
		return AppPluginSessionRevokeResult{}, err
	}
	hash, err := appPluginHash(request)
	if err != nil {
		return AppPluginSessionRevokeResult{}, err
	}
	var result AppPluginSessionRevokeResult
	err = model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		result = AppPluginSessionRevokeResult{}
		now := s.options.Now().UTC()
		if _, err := model.LockAppPluginInstallation(tx, service.AppKey, service.InstallationID); err != nil {
			return classifyDBLookup(err, "not_found")
		}
		currentService, err := model.ValidateAppServiceIdentity(tx, service, now)
		if err != nil {
			return appServiceIdentityRevalidationError(err)
		}
		if !slices.Contains(currentService.Scopes, "identity.read") {
			return appAuthError("scope_denied")
		}
		var session model.AppPluginSession
		if err := model.AppPluginCurrentRead(tx).Where("app_session_id = ? AND installation_id = ? AND app_key = ? AND subject = ?",
			request.AppSessionID, service.InstallationID, request.AppKey, request.Subject).First(&session).Error; err != nil {
			return classifyDBLookup(err, "not_found")
		}
		var replay model.AppPluginSessionRevokeReplay
		q := model.AppPluginCurrentRead(tx).Where("scope_hash = ?", scope).Limit(1).Find(&replay)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected == 1 {
			if replay.RequestHash != hash {
				return appAuthError("idempotency_conflict")
			}
			return common.UnmarshalJsonStr(replay.ResponseJSON, &result)
		}
		result = AppPluginSessionRevokeResult{AppSessionID: session.AppSessionID, State: "revoked", AlreadyRevoked: session.RevokedAt != 0}
		if session.RevokedAt == 0 {
			session.RevokedAt = now.UnixNano()
			if err := tx.Model(&model.AppPluginSession{}).Where("app_session_id = ?", session.AppSessionID).Update("revoked_at", session.RevokedAt).Error; err != nil {
				return err
			}
		}
		result.RevokedAt = time.Unix(0, session.RevokedAt).UTC()
		raw, err := common.Marshal(result)
		if err != nil {
			return err
		}
		return tx.Create(&model.AppPluginSessionRevokeReplay{ScopeHash: scope, RequestHash: hash,
			InstallationID: service.InstallationID, ResponseJSON: string(raw)}).Error
	})
	return result, appExecutionBoundaryError(err)
}

// Match authz.Can's role/override semantics using current DB rows, not the
// process-local Casbin snapshot. Unknown resources/actions are never granted.
func appPluginCurrentPermissions(ctx context.Context, tx *gorm.DB, user model.User) (model.AppJSONMap, error) {
	result := model.AppJSONMap{}
	role := ""
	switch {
	case user.Role == common.RolePluginAdminUser:
		role = authz.BuiltInRolePluginAdmin
	case user.Role >= common.RoleRootUser:
		for _, permission := range authz.AllPermissions() {
			result[permission.Resource] = append(result[permission.Resource], permission.Action)
		}
		return result, nil
	case user.Role >= common.RoleAdminUser:
		role = authz.BuiltInRoleAdmin
	default:
		return result, nil
	}
	subject := authz.UserSubject(user.Id)
	roleSubject := authz.RoleSubject(role)
	var rules []model.CasbinRule
	if err := model.AppPluginCurrentRead(tx.WithContext(ctx)).Where("ptype = ? AND v0 IN ?", "p", []string{subject, roleSubject}).Order("id").Find(&rules).Error; err != nil {
		return nil, err
	}
	for _, permission := range authz.AllPermissions() {
		effects := map[string]string{}
		for _, rule := range rules {
			if rule.V1 != permission.Resource || rule.V2 != permission.Action || effects[rule.V0] == authz.EffectDeny {
				continue
			}
			if rule.V3 == authz.EffectDeny {
				effects[rule.V0] = authz.EffectDeny
			} else if rule.V3 == "" || rule.V3 == authz.EffectAllow {
				effects[rule.V0] = authz.EffectAllow
			}
		}
		effect, explicit := effects[subject]
		if !explicit {
			effect = effects[roleSubject]
		}
		if effect == authz.EffectAllow {
			result[permission.Resource] = append(result[permission.Resource], permission.Action)
		}
	}
	return result, nil
}

// ProbeAppPluginNetwork never inherits ambient proxy or insecure TLS settings.
// The existing protected dialer validates every resolved IP and dials that IP
// directly, closing the DNS rebinding window. Redirects are not followed.
func ProbeAppPluginNetwork(ctx context.Context, installation model.AppInstallation) error {
	target, err := url.Parse(installation.BaseURL)
	if err != nil || target.Scheme != "https" || target.User != nil || target.RawQuery != "" ||
		target.Fragment != "" || !installation.NetworkPolicy.DenyPrivateIPRanges ||
		!slices.Contains(installation.NetworkPolicy.AllowHosts, target.Host) {
		return appAuthError("app_enable_prerequisite_missing")
	}
	port := target.Port()
	if port == "" {
		port = "443"
	}
	protection, err := common.NewSSRFProtectionFromFetchSetting(false, false, false,
		nil, nil, []string{port}, true)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dialer := &protectedFetchDialer{resolver: net.DefaultResolver,
		dialContext:   (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		getProtection: func() (*common.SSRFProtection, bool, error) { return protection, true, nil }}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 2 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second, MaxResponseHeaderBytes: 16 * 1024, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return appAuthError("app_enable_prerequisite_missing")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return appAuthError("app_enable_prerequisite_missing")
	}
	return nil
}
