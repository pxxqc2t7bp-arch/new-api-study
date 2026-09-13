package model

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	AppInstallationStatusDisabled = "disabled"
	AppInstallationStatusEnabled  = "enabled"
	AppInstallationStatusRevoked  = "revoked"
)

var (
	ErrAppVersionConflict              = errors.New("app_version_conflict")
	ErrAppIdempotencyConflict          = errors.New("app_idempotency_conflict")
	ErrAppRouteClaimConflict           = errors.New("app_route_claim_conflict")
	ErrAppInstallationRevisionConflict = errors.New("app_installation_revision_conflict")
	ErrAppInstallationRevoked          = errors.New("app_installation_revoked")
	ErrAppInstallationStatusInvalid    = errors.New("app_installation_status_invalid")
)

type AppJSONMap map[string][]string

func (m AppJSONMap) Value() (driver.Value, error) {
	if m == nil {
		return "{}", nil
	}
	data, err := common.Marshal(m)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func (m *AppJSONMap) Scan(value any) error {
	data := jsonScanBytes(value)
	if len(data) == 0 {
		*m = AppJSONMap{}
		return nil
	}
	var parsed map[string][]string
	if err := common.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*m = parsed
	return nil
}

type AppAllowedUserPolicy struct {
	Groups []string `json:"groups,omitempty"`
}

func (p AppAllowedUserPolicy) Value() (driver.Value, error) {
	data, err := common.Marshal(p)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func (p *AppAllowedUserPolicy) Scan(value any) error {
	data := jsonScanBytes(value)
	if len(data) == 0 {
		*p = AppAllowedUserPolicy{}
		return nil
	}
	return common.Unmarshal(data, p)
}

type AppNetworkPolicy struct {
	AllowHosts          []string `json:"allow_hosts,omitempty"`
	DenyPrivateIPRanges bool     `json:"deny_private_ip_ranges,omitempty"`
}

func (p AppNetworkPolicy) Value() (driver.Value, error) {
	data, err := common.Marshal(p)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func (p *AppNetworkPolicy) Scan(value any) error {
	data := jsonScanBytes(value)
	if len(data) == 0 {
		*p = AppNetworkPolicy{}
		return nil
	}
	return common.Unmarshal(data, p)
}

type AppStringList []string

func (l AppStringList) Value() (driver.Value, error) {
	if l == nil {
		return "[]", nil
	}
	data, err := common.Marshal(l)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func (l *AppStringList) Scan(value any) error {
	data := jsonScanBytes(value)
	if len(data) == 0 {
		*l = AppStringList{}
		return nil
	}
	var parsed []string
	if err := common.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*l = parsed
	return nil
}

type AppVersion struct {
	ID                    string    `gorm:"primaryKey;size:64" json:"-"`
	IdentityHash          string    `gorm:"size:64;not null;uniqueIndex" json:"-"`
	AppKey                string    `gorm:"size:128;not null;index" json:"-"`
	ManifestVersion       string    `gorm:"size:64;not null;index" json:"-"`
	ManifestSHA256        string    `gorm:"size:64;not null" json:"-"`
	CanonicalManifestJSON string    `gorm:"type:text;not null" json:"-"`
	CreatedAt             time.Time `json:"-"`
}

type AppInstallation struct {
	ID                   uint                 `gorm:"primaryKey" json:"-"`
	InstallationID       string               `gorm:"size:64;not null;uniqueIndex" json:"installation_id"`
	AppKey               string               `gorm:"size:128;not null;index" json:"app_key"`
	AppVersionID         string               `gorm:"size:64;not null;index" json:"-"`
	ManifestVersion      string               `gorm:"size:64;not null" json:"manifest_version"`
	ManifestSHA256       string               `gorm:"size:64;not null" json:"manifest_sha256"`
	BaseURL              string               `gorm:"size:512;not null" json:"base_url"`
	EnabledSurfaces      AppStringList        `gorm:"type:text;not null" json:"enabled_surfaces"`
	AllowedParentOrigins AppStringList        `gorm:"type:text;not null" json:"allowed_parent_origins"`
	AllowedOrigins       AppStringList        `gorm:"type:text;not null" json:"allowed_origins"`
	AllowedUserPolicy    AppAllowedUserPolicy `gorm:"type:text;not null" json:"allowed_user_policy"`
	NetworkPolicy        AppNetworkPolicy     `gorm:"type:text;not null" json:"network_policy"`
	EntitlementPolicyID  string               `gorm:"size:64;not null" json:"entitlement_policy_version"`
	Status               string               `gorm:"size:32;not null;index" json:"status"`
	Revision             int64                `gorm:"not null" json:"revision"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
}

type AppInstallationIdempotency struct {
	ID             uint      `gorm:"primaryKey" json:"-"`
	ScopeHash      string    `gorm:"size:64;not null;uniqueIndex" json:"-"`
	ActorID        int64     `gorm:"not null;index" json:"-"`
	ScopeKey       string    `gorm:"size:255;not null" json:"-"`
	ClaimToken     string    `gorm:"size:64;not null" json:"-"`
	RequestHash    string    `gorm:"size:64;not null" json:"-"`
	InstallationID string    `gorm:"size:64;not null" json:"-"`
	AppVersionID   string    `gorm:"size:64;not null" json:"-"`
	ResponseJSON   string    `gorm:"type:text;not null" json:"-"`
	ResponseDigest string    `gorm:"size:64;not null" json:"-"`
	CreatedAt      time.Time `json:"-"`
}

type AppRouteClaim struct {
	ID               uint      `gorm:"primaryKey" json:"-"`
	AppKey           string    `gorm:"size:128;not null;index" json:"-"`
	InstallationID   string    `gorm:"size:64;not null;index" json:"-"`
	ClaimKey         string    `gorm:"size:64;not null;uniqueIndex" json:"-"`
	Kind             string    `gorm:"size:32;not null" json:"-"`
	AbsoluteEndpoint string    `gorm:"size:512;not null" json:"-"`
	CreatedAt        time.Time `json:"-"`
}

type AppServiceCredential struct {
	ID                uint      `gorm:"primaryKey" json:"-"`
	AppKey            string    `gorm:"size:128;not null;index" json:"-"`
	InstallationID    string    `gorm:"size:64;not null;index" json:"-"`
	CredentialID      string    `gorm:"size:255;not null" json:"credential_id"`
	CredentialHash    string    `gorm:"size:64;not null" json:"-"`
	CredentialVersion string    `gorm:"size:128;not null" json:"version"`
	Status            string    `gorm:"size:32;not null" json:"status"`
	ExpiresAt         int64     `gorm:"not null" json:"expires_at"`
	CreatedAt         time.Time `json:"-"`
}

type AppEntitlementPolicy struct {
	ID             string     `gorm:"primaryKey;size:64" json:"-"`
	KeyHash        string     `gorm:"size:64;not null;index" json:"-"`
	VersionKey     string     `gorm:"size:64;not null;uniqueIndex" json:"-"`
	CreationToken  string     `gorm:"size:64;not null" json:"-"`
	Key            string     `gorm:"size:128;not null;index" json:"key"`
	Version        int64      `gorm:"not null;index" json:"version"`
	EffectiveRules AppJSONMap `gorm:"type:text;not null" json:"effective_rules"`
	CreatedAt      time.Time  `json:"created_at"`
}

type AppIdempotencyScope struct {
	ActorID int64
	Key     string
}

type AppInstallRequest struct {
	AppKey                   string
	ManifestVersion          string
	ManifestSHA256           string
	CanonicalManifestJSON    []byte
	BaseURL                  string
	CallbackURL              string
	DirectURL                string
	EmbeddedURL              string
	EnabledSurfaces          []string
	AllowedParentOrigins     []string
	AllowedOrigins           []string
	AllowedUserPolicy        AppAllowedUserPolicy
	NetworkPolicy            AppNetworkPolicy
	EntitlementPolicyID      string
	ServiceCredentialHash    string
	ServiceCredentialID      string
	ServiceCredentialVersion string
	ServiceCredentialExpiry  int64
}

type AppInstallResult struct {
	AppVersionID             string               `json:"-"`
	InstallationID           string               `json:"installation_id"`
	AppKey                   string               `json:"app_key"`
	ManifestVersion          string               `json:"manifest_version"`
	ManifestSHA256           string               `json:"manifest_sha256"`
	BaseURL                  string               `json:"base_url"`
	EnabledSurfaces          []string             `json:"enabled_surfaces"`
	AllowedParentOrigins     []string             `json:"allowed_parent_origins"`
	ServiceCredentialSet     AppCredentialMeta    `json:"service_credential_set"`
	AllowedOrigins           []string             `json:"allowed_origins"`
	AllowedUserPolicy        AppAllowedUserPolicy `json:"allowed_user_policy"`
	NetworkPolicy            AppNetworkPolicy     `json:"network_policy"`
	EntitlementPolicyVersion string               `json:"entitlement_policy_version"`
	Status                   string               `json:"status"`
	Revision                 int64                `json:"revision"`
	CreatedAt                time.Time            `json:"created_at"`
	UpdatedAt                time.Time            `json:"updated_at"`
	ResponseDigest           string               `json:"-"`
}

type AppCredentialMeta struct {
	CredentialID string `json:"credential_id"`
	Version      string `json:"version"`
	Status       string `json:"status"`
	ExpiresAt    int64  `json:"expires_at"`
}

func AppPluginErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrAppVersionConflict):
		return "app_version_conflict"
	case errors.Is(err, ErrAppIdempotencyConflict):
		return "app_idempotency_conflict"
	case errors.Is(err, ErrAppRouteClaimConflict):
		return "app_route_claim_conflict"
	default:
		return "app_plugin_error"
	}
}

func MigrateAppPluginTables(db *gorm.DB) error {
	if db.Migrator().HasTable(&AppInstallation{}) && !db.Migrator().HasColumn(&AppInstallation{}, "installation_id") {
		columnType := "text"
		switch db.Dialector.Name() {
		case "mysql":
			columnType = "varchar(64)"
		case "postgres":
			columnType = "varchar(64)"
		}
		if err := db.Exec("ALTER TABLE app_installations ADD COLUMN installation_id " + columnType).Error; err != nil {
			return err
		}
		if err := db.Exec("UPDATE app_installations SET installation_id = id WHERE installation_id IS NULL OR installation_id = ''").Error; err != nil {
			return err
		}
	}
	if err := db.AutoMigrate(
		&AppVersion{},
		&AppInstallation{},
		&AppInstallationIdempotency{},
		&AppRouteClaim{},
		&AppServiceCredential{},
		&AppEntitlementPolicy{},
	); err != nil {
		return err
	}
	return nil
}

func InstallAppVersion(ctx context.Context, db *gorm.DB, scope AppIdempotencyScope, req AppInstallRequest) (AppInstallResult, error) {
	requestHash, err := appInstallRequestHash(req)
	if err != nil {
		return AppInstallResult{}, err
	}
	scopeHash := appPluginSHA256([]byte(strconv.FormatInt(scope.ActorID, 10) + "\x00" + scope.Key))
	claimToken, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return AppInstallResult{}, err
	}
	installationNonce, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return AppInstallResult{}, err
	}
	var result AppInstallResult
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		claim := AppInstallationIdempotency{
			ScopeHash:   scopeHash,
			ActorID:     scope.ActorID,
			ScopeKey:    scope.Key,
			ClaimToken:  claimToken,
			RequestHash: requestHash,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim).Error; err != nil {
			return err
		}
		var replay AppInstallationIdempotency
		if err := tx.Where("scope_hash = ?", scopeHash).First(&replay).Error; err != nil {
			return err
		}
		if replay.RequestHash != requestHash {
			return ErrAppIdempotencyConflict
		}
		if replay.ClaimToken != claimToken {
			if replay.InstallationID == "" || replay.ResponseJSON == "" {
				return ErrAppIdempotencyConflict
			}
			if err := common.Unmarshal([]byte(replay.ResponseJSON), &result); err != nil {
				return err
			}
			result.AppVersionID = replay.AppVersionID
			result.ResponseDigest = replay.ResponseDigest
			return nil
		}

		existing := AppVersion{
			ID:                    appPluginStableID("appver", req.AppKey, req.ManifestVersion),
			IdentityHash:          appPluginSHA256([]byte(req.AppKey + "\x00" + req.ManifestVersion)),
			AppKey:                req.AppKey,
			ManifestVersion:       req.ManifestVersion,
			ManifestSHA256:        req.ManifestSHA256,
			CanonicalManifestJSON: string(req.CanonicalManifestJSON),
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&existing).Error; err != nil {
			return err
		}
		if err := tx.Where("identity_hash = ?", existing.IdentityHash).First(&existing).Error; err != nil {
			return err
		}
		if existing.ManifestSHA256 != req.ManifestSHA256 {
			return ErrAppVersionConflict
		}

		installation := AppInstallation{
			InstallationID:       appPluginStableID("inst", req.AppKey, req.ManifestVersion, req.ManifestSHA256, installationNonce),
			AppKey:               req.AppKey,
			AppVersionID:         existing.ID,
			ManifestVersion:      req.ManifestVersion,
			ManifestSHA256:       req.ManifestSHA256,
			BaseURL:              req.BaseURL,
			EnabledSurfaces:      append(AppStringList(nil), req.EnabledSurfaces...),
			AllowedParentOrigins: append(AppStringList(nil), req.AllowedParentOrigins...),
			AllowedOrigins:       append(AppStringList(nil), req.AllowedOrigins...),
			AllowedUserPolicy:    req.AllowedUserPolicy,
			NetworkPolicy:        req.NetworkPolicy,
			EntitlementPolicyID:  req.EntitlementPolicyID,
			Status:               AppInstallationStatusDisabled,
			Revision:             1,
		}
		if err := tx.Create(&installation).Error; err != nil {
			return err
		}
		claims := []AppRouteClaim{
			appRouteClaim(req.AppKey, installation.InstallationID, "callback", req.CallbackURL),
			appRouteClaim(req.AppKey, installation.InstallationID, "direct", req.DirectURL),
			appRouteClaim(req.AppKey, installation.InstallationID, "embedded", req.EmbeddedURL),
		}
		for i := range claims {
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "claim_key"}}, DoNothing: true}).Create(&claims[i]).Error; err != nil {
				return err
			}
			var stored AppRouteClaim
			if err := tx.Where("claim_key = ?", claims[i].ClaimKey).First(&stored).Error; err != nil {
				return err
			}
			if stored.AppKey != claims[i].AppKey || stored.InstallationID != claims[i].InstallationID || stored.Kind != claims[i].Kind {
				return ErrAppRouteClaimConflict
			}
		}
		credential := AppServiceCredential{
			AppKey:            req.AppKey,
			InstallationID:    installation.InstallationID,
			CredentialID:      req.ServiceCredentialID,
			CredentialHash:    req.ServiceCredentialHash,
			CredentialVersion: req.ServiceCredentialVersion,
			Status:            "active",
			ExpiresAt:         req.ServiceCredentialExpiry,
		}
		if err := tx.Create(&credential).Error; err != nil {
			return err
		}
		result = resultFromInstallation(existing.ID, installation, credentialMeta(credential))
		responseJSON, digest, err := frozenAppInstallResponse(result)
		if err != nil {
			return err
		}
		result.ResponseDigest = digest
		updates := map[string]any{
			"installation_id": installation.InstallationID,
			"app_version_id":  existing.ID,
			"response_json":   responseJSON,
			"response_digest": digest,
		}
		update := tx.Model(&AppInstallationIdempotency{}).
			Where("scope_hash = ? AND claim_token = ?", scopeHash, claimToken).
			Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrAppIdempotencyConflict
		}
		return nil
	})
	return result, err
}

func CompareAndSwapAppInstallationStatus(ctx context.Context, db *gorm.DB, installationID string, revision int64, status string) (AppInstallation, error) {
	if status != AppInstallationStatusDisabled && status != AppInstallationStatusEnabled && status != AppInstallationStatusRevoked {
		return AppInstallation{}, ErrAppInstallationStatusInvalid
	}
	var allowedCurrent []string
	switch status {
	case AppInstallationStatusEnabled:
		allowedCurrent = []string{AppInstallationStatusDisabled}
	case AppInstallationStatusDisabled:
		allowedCurrent = []string{AppInstallationStatusEnabled}
	case AppInstallationStatusRevoked:
		allowedCurrent = []string{AppInstallationStatusDisabled, AppInstallationStatusEnabled}
	}
	var updated AppInstallation
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&AppInstallation{}).
			Where("installation_id = ? AND revision = ? AND status IN ?", installationID, revision, allowedCurrent).
			Updates(map[string]any{"status": status, "revision": gorm.Expr("revision + ?", 1)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			var current AppInstallation
			if err := tx.Select("status", "revision").
				Where("installation_id = ?", installationID).First(&current).Error; err != nil {
				return err
			}
			if current.Status == AppInstallationStatusRevoked {
				return ErrAppInstallationRevoked
			}
			if current.Revision == revision {
				return ErrAppInstallationStatusInvalid
			}
			return ErrAppInstallationRevisionConflict
		}
		if status == AppInstallationStatusRevoked {
			if err := tx.Where("installation_id = ?", installationID).Delete(&AppRouteClaim{}).Error; err != nil {
				return err
			}
			if err := tx.Model(&AppServiceCredential{}).
				Where("installation_id = ?", installationID).
				Update("status", AppInstallationStatusRevoked).Error; err != nil {
				return err
			}
		}
		return tx.Where("installation_id = ?", installationID).First(&updated).Error
	})
	if err != nil {
		return AppInstallation{}, err
	}
	return updated, nil
}

func resultFromInstallation(appVersionID string, installation AppInstallation, credentialSet AppCredentialMeta) AppInstallResult {
	return AppInstallResult{
		AppVersionID:             appVersionID,
		InstallationID:           installation.InstallationID,
		AppKey:                   installation.AppKey,
		ManifestVersion:          installation.ManifestVersion,
		ManifestSHA256:           installation.ManifestSHA256,
		BaseURL:                  installation.BaseURL,
		EnabledSurfaces:          []string(installation.EnabledSurfaces),
		AllowedParentOrigins:     []string(installation.AllowedParentOrigins),
		ServiceCredentialSet:     credentialSet,
		AllowedOrigins:           []string(installation.AllowedOrigins),
		AllowedUserPolicy:        installation.AllowedUserPolicy,
		NetworkPolicy:            installation.NetworkPolicy,
		EntitlementPolicyVersion: installation.EntitlementPolicyID,
		Status:                   installation.Status,
		Revision:                 installation.Revision,
		CreatedAt:                installation.CreatedAt,
		UpdatedAt:                installation.UpdatedAt,
	}
}

func appRouteClaim(appKey, installationID, kind, endpoint string) AppRouteClaim {
	return AppRouteClaim{
		AppKey:           appKey,
		InstallationID:   installationID,
		Kind:             kind,
		AbsoluteEndpoint: endpoint,
		ClaimKey:         appPluginSHA256([]byte(endpoint)),
	}
}

func credentialMeta(credential AppServiceCredential) AppCredentialMeta {
	return AppCredentialMeta{
		CredentialID: credential.CredentialID,
		Version:      credential.CredentialVersion,
		Status:       credential.Status,
		ExpiresAt:    credential.ExpiresAt,
	}
}

func validInstallationTransition(current, next string) bool {
	switch current {
	case AppInstallationStatusDisabled:
		return next == AppInstallationStatusEnabled || next == AppInstallationStatusRevoked
	case AppInstallationStatusEnabled:
		return next == AppInstallationStatusDisabled || next == AppInstallationStatusRevoked
	default:
		return false
	}
}

func appInstallRequestHash(req AppInstallRequest) (string, error) {
	data, err := common.Marshal(req)
	if err != nil {
		return "", err
	}
	return appPluginSHA256(data), nil
}

func frozenAppInstallResponse(result AppInstallResult) (string, string, error) {
	data, err := common.Marshal(result)
	if err != nil {
		return "", "", err
	}
	return string(data), appPluginSHA256(data), nil
}

func appPluginStableID(parts ...string) string {
	var identity strings.Builder
	for _, part := range parts {
		identity.WriteString(strconv.Itoa(len(part)))
		identity.WriteByte(':')
		identity.WriteString(part)
		identity.WriteByte(0)
	}
	return parts[0] + "_" + appPluginSHA256([]byte(identity.String()))[:32]
}

func appPluginSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}
