package model

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	mysqlDriver "github.com/go-sql-driver/mysql"
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
	ErrAppInstallRequestInvalid        = errors.New("app_install_request_invalid")
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
		return "idempotency_conflict"
	case errors.Is(err, ErrAppRouteClaimConflict):
		return "app_route_collision"
	case errors.Is(err, ErrAppInstallationRevisionConflict):
		return "version_conflict"
	case errors.Is(err, ErrAppInstallationRevoked),
		errors.Is(err, ErrAppInstallationStatusInvalid):
		return "invalid_state_transition"
	case errors.Is(err, ErrAppInstallRequestInvalid):
		return "validation_error"
	default:
		return "service_unavailable"
	}
}

func MigrateAppPluginTables(db *gorm.DB) error {
	if db.Migrator().HasTable(&AppInstallation{}) {
		if !db.Migrator().HasColumn(&AppInstallation{}, "installation_id") {
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
	return db.Transaction(func(tx *gorm.DB) error {
		var installations []AppInstallation
		if err := tx.Where("status <> ?", AppInstallationStatusRevoked).
			Order("id").
			Find(&installations).Error; err != nil {
			return err
		}

		// Preserve an enabled legacy installation when possible, then use the
		// lowest primary key as the stable cross-database tie breaker.
		owners := make(map[string]AppInstallation)
		for _, installation := range installations {
			owner, found := owners[installation.AppKey]
			if !found || (owner.Status != AppInstallationStatusEnabled && installation.Status == AppInstallationStatusEnabled) {
				owners[installation.AppKey] = installation
			}
		}
		for _, installation := range installations {
			if installation.ID == owners[installation.AppKey].ID {
				continue
			}
			update := tx.Model(&AppInstallation{}).
				Where("id = ? AND status <> ?", installation.ID, AppInstallationStatusRevoked).
				Updates(map[string]any{
					"status":   AppInstallationStatusRevoked,
					"revision": gorm.Expr("revision + ?", 1),
				})
			if update.Error != nil {
				return update.Error
			}
			if err := tx.Model(&AppServiceCredential{}).
				Where("installation_id = ? AND status <> ?", installation.InstallationID, AppInstallationStatusRevoked).
				Update("status", AppInstallationStatusRevoked).Error; err != nil {
				return err
			}
			if err := tx.Where("installation_id = ?", installation.InstallationID).
				Delete(&AppRouteClaim{}).Error; err != nil {
				return err
			}
		}

		var routeClaims []AppRouteClaim
		if err := tx.Where("kind <> ?", "app_key").Find(&routeClaims).Error; err != nil {
			return err
		}
		for _, claim := range routeClaims {
			expected := appRouteClaim(claim.AppKey, claim.InstallationID, claim.Kind, claim.AbsoluteEndpoint)
			if claim.ClaimKey == expected.ClaimKey {
				continue
			}
			if err := tx.Model(&AppRouteClaim{}).
				Where("id = ?", claim.ID).
				Update("claim_key", expected.ClaimKey).Error; err != nil {
				return err
			}
		}

		for _, installation := range installations {
			if installation.ID != owners[installation.AppKey].ID {
				continue
			}
			claim := appKeyClaim(installation.AppKey, installation.InstallationID)
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "claim_key"}}, DoNothing: true}).
				Create(&claim).Error; err != nil {
				return err
			}
			var stored AppRouteClaim
			if err := tx.Where("claim_key = ?", claim.ClaimKey).First(&stored).Error; err != nil {
				return err
			}
			if stored.InstallationID != installation.InstallationID || stored.Kind != "app_key" {
				return ErrAppRouteClaimConflict
			}
		}
		return nil
	})
}

// InstallAppVersion persists a trusted host request already validated and assembled by the host service.
// These checks are defense in depth for cheap cross-field and storage invariants, not a
// replacement for service.ValidateAppManifest at the external boundary.
func InstallAppVersion(ctx context.Context, db *gorm.DB, scope AppIdempotencyScope, req AppInstallRequest) (AppInstallResult, error) {
	if err := validateAppInstallRequest(req); err != nil {
		return AppInstallResult{}, err
	}
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
	transaction := func(tx *gorm.DB) error {
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
		if err := lockForUpdate(tx).Where("scope_hash = ?", scopeHash).First(&replay).Error; err != nil {
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

		if req.EntitlementPolicyID != "" {
			var policyCount int64
			if err := tx.Model(&AppEntitlementPolicy{}).
				Where("id = ?", req.EntitlementPolicyID).
				Count(&policyCount).Error; err != nil {
				return err
			}
			if policyCount != 1 {
				return fmt.Errorf("%w: entitlement policy not found", ErrAppInstallRequestInvalid)
			}
		}

		version := AppVersion{
			ID:                    appPluginStableID("appver", req.AppKey, req.ManifestVersion),
			IdentityHash:          appPluginSHA256([]byte(req.AppKey + "\x00" + req.ManifestVersion)),
			AppKey:                req.AppKey,
			ManifestVersion:       req.ManifestVersion,
			ManifestSHA256:        req.ManifestSHA256,
			CanonicalManifestJSON: string(req.CanonicalManifestJSON),
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&version).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("identity_hash = ?", version.IdentityHash).First(&version).Error; err != nil {
			return err
		}
		if version.ManifestSHA256 != req.ManifestSHA256 {
			return ErrAppVersionConflict
		}

		candidateInstallationID := appPluginStableID("inst", req.AppKey, req.ManifestVersion, req.ManifestSHA256, installationNonce)
		ownership := appKeyClaim(req.AppKey, candidateInstallationID)
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "claim_key"}}, DoNothing: true}).
			Create(&ownership).Error; err != nil {
			return err
		}
		ownershipQuery := lockForUpdate(tx).Where("claim_key = ?", ownership.ClaimKey)
		if err := ownershipQuery.First(&ownership).Error; err != nil {
			return err
		}
		if ownership.AppKey != req.AppKey || ownership.Kind != "app_key" {
			return ErrAppRouteClaimConflict
		}

		isNewInstallation := ownership.InstallationID == candidateInstallationID
		var installation AppInstallation
		if isNewInstallation {
			installation = AppInstallation{
				InstallationID:       candidateInstallationID,
				AppKey:               req.AppKey,
				AppVersionID:         version.ID,
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
		} else {
			installationQuery := lockForUpdate(tx).Where("installation_id = ?", ownership.InstallationID)
			if err := installationQuery.First(&installation).Error; err != nil {
				return err
			}
			if installation.Status == AppInstallationStatusRevoked {
				return ErrAppInstallationRevoked
			}

			frozen, found, err := findFrozenAppInstallResponse(tx, installation.InstallationID, version.ID)
			if err != nil {
				return err
			}
			if found {
				result = frozen
				return freezeAppInstallIdempotency(tx, scopeHash, claimToken, &result)
			}

			if err := tx.Where("installation_id = ? AND kind <> ?", installation.InstallationID, "app_key").
				Delete(&AppRouteClaim{}).Error; err != nil {
				return err
			}
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

		if !isNewInstallation {
			update := tx.Model(&AppInstallation{}).
				Where("installation_id = ? AND revision = ? AND status <> ?",
					installation.InstallationID, installation.Revision, AppInstallationStatusRevoked).
				Updates(map[string]any{
					"app_version_id":         version.ID,
					"manifest_version":       req.ManifestVersion,
					"manifest_sha256":        req.ManifestSHA256,
					"base_url":               req.BaseURL,
					"enabled_surfaces":       AppStringList(req.EnabledSurfaces),
					"allowed_parent_origins": AppStringList(req.AllowedParentOrigins),
					"allowed_origins":        AppStringList(req.AllowedOrigins),
					"allowed_user_policy":    req.AllowedUserPolicy,
					"network_policy":         req.NetworkPolicy,
					"entitlement_policy_id":  req.EntitlementPolicyID,
					"status":                 AppInstallationStatusDisabled,
					"revision":               gorm.Expr("revision + ?", 1),
				})
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return ErrAppInstallationRevisionConflict
			}
			if err := tx.Where("installation_id = ?", installation.InstallationID).First(&installation).Error; err != nil {
				return err
			}
		}

		credentialSet := AppCredentialMeta{}
		if isNewInstallation && appInstallHasServiceCredential(req) {
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
			credentialSet = credentialMeta(credential)
		} else if !isNewInstallation {
			var credential AppServiceCredential
			credentialQuery := tx.Where("installation_id = ?", installation.InstallationID).
				Order("id DESC").
				Limit(1).
				Find(&credential)
			if credentialQuery.Error != nil {
				return credentialQuery.Error
			}
			if credentialQuery.RowsAffected == 1 {
				credentialSet = credentialMeta(credential)
			}
		}
		result = resultFromInstallation(version.ID, installation, credentialSet)
		return freezeAppInstallIdempotency(tx, scopeHash, claimToken, &result)
	}
	const maxTransactionAttempts = 3
	for attempt := range maxTransactionAttempts {
		result = AppInstallResult{}
		err = db.WithContext(ctx).Transaction(transaction)
		if err == nil || db.Dialector.Name() != "mysql" {
			break
		}
		var mysqlErr *mysqlDriver.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1213 || attempt == maxTransactionAttempts-1 {
			break
		}
	}
	return result, err
}

// ReplayAppInstall returns and freezes an existing installation response without
// consulting mutable installation prerequisites.
func ReplayAppInstall(ctx context.Context, db *gorm.DB, scope AppIdempotencyScope, req AppInstallRequest) (AppInstallResult, bool, error) {
	if err := validateAppInstallRequest(req); err != nil {
		return AppInstallResult{}, false, err
	}
	requestHash, err := appInstallRequestHash(req)
	if err != nil {
		return AppInstallResult{}, false, err
	}
	scopeHash := appPluginSHA256([]byte(strconv.FormatInt(scope.ActorID, 10) + "\x00" + scope.Key))

	var result AppInstallResult
	found := false
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var replay AppInstallationIdempotency
		replayQuery := tx.Where("scope_hash = ?", scopeHash).Limit(1).Find(&replay)
		if replayQuery.Error != nil {
			return replayQuery.Error
		}
		if replayQuery.RowsAffected == 1 {
			if replay.RequestHash != requestHash ||
				replay.InstallationID == "" ||
				replay.ResponseJSON == "" {
				return ErrAppIdempotencyConflict
			}
			if err := common.Unmarshal([]byte(replay.ResponseJSON), &result); err != nil {
				return err
			}
			result.AppVersionID = replay.AppVersionID
			result.ResponseDigest = replay.ResponseDigest
			found = true
			return nil
		}

		var version AppVersion
		versionQuery := tx.Where("identity_hash = ?", appPluginSHA256([]byte(req.AppKey+"\x00"+req.ManifestVersion))).
			Limit(1).
			Find(&version)
		if versionQuery.Error != nil {
			return versionQuery.Error
		}
		if versionQuery.RowsAffected == 0 {
			return nil
		}
		if version.ManifestSHA256 != req.ManifestSHA256 {
			return ErrAppVersionConflict
		}

		var ownership AppRouteClaim
		ownershipQuery := tx.Where("claim_key = ?", appKeyClaim(req.AppKey, "").ClaimKey).
			Limit(1).
			Find(&ownership)
		if ownershipQuery.Error != nil {
			return ownershipQuery.Error
		}
		if ownershipQuery.RowsAffected == 0 {
			return nil
		}
		if ownership.AppKey != req.AppKey || ownership.Kind != "app_key" {
			return ErrAppRouteClaimConflict
		}

		var installation AppInstallation
		if err := tx.Where("installation_id = ? AND status <> ?", ownership.InstallationID, AppInstallationStatusRevoked).
			First(&installation).Error; err != nil {
			return err
		}
		frozen, frozenFound, err := findFrozenAppInstallResponse(tx, installation.InstallationID, version.ID)
		if err != nil {
			return err
		}
		if !frozenFound {
			return nil
		}

		claimToken, err := common.GenerateRandomCharsKey(32)
		if err != nil {
			return err
		}
		replay = AppInstallationIdempotency{
			ScopeHash:   scopeHash,
			ActorID:     scope.ActorID,
			ScopeKey:    scope.Key,
			ClaimToken:  claimToken,
			RequestHash: requestHash,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&replay).Error; err != nil {
			return err
		}
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
			found = true
			return nil
		}

		result = frozen
		if err := freezeAppInstallIdempotency(tx, scopeHash, claimToken, &result); err != nil {
			return err
		}
		found = true
		return nil
	})
	return result, found, err
}

func validateAppInstallRequest(req AppInstallRequest) error {
	invalid := func(reason string) error {
		return fmt.Errorf("%w: %s", ErrAppInstallRequestInvalid, reason)
	}
	if req.ManifestSHA256 != appPluginSHA256(req.CanonicalManifestJSON) {
		return invalid("canonical manifest digest mismatch")
	}
	var manifestIdentity struct {
		Key     string `json:"key"`
		Version string `json:"version"`
	}
	if err := common.Unmarshal(req.CanonicalManifestJSON, &manifestIdentity); err != nil {
		return invalid("canonical manifest is not valid JSON")
	}
	if req.AppKey == "" || manifestIdentity.Key != req.AppKey {
		return invalid("manifest key mismatch")
	}
	if req.ManifestVersion == "" || manifestIdentity.Version != req.ManifestVersion {
		return invalid("manifest version mismatch")
	}

	baseURL, err := url.Parse(req.BaseURL)
	if err != nil || !validAppInstallHTTPSURL(baseURL) || !strings.HasSuffix(baseURL.Path, "/") {
		return invalid("invalid base URL")
	}
	for _, endpoint := range []string{req.CallbackURL, req.DirectURL, req.EmbeddedURL} {
		endpointURL, parseErr := url.Parse(endpoint)
		if parseErr != nil || !validAppInstallEndpoint(baseURL, endpointURL) {
			return invalid("invalid app endpoint")
		}
	}
	if !validAppInstallStringSet(req.EnabledSurfaces, func(surface string) bool {
		return surface == "direct" || surface == "embedded"
	}) {
		return invalid("invalid enabled surfaces")
	}
	for _, origins := range [][]string{req.AllowedParentOrigins, req.AllowedOrigins} {
		if !validAppInstallStringSet(origins, validAppInstallOrigin) {
			return invalid("invalid allowed origins")
		}
	}
	if !validAppInstallStringSet(req.AllowedUserPolicy.Groups, validAppInstallReference) {
		return invalid("invalid allowed user policy")
	}
	if !validAppInstallStringSet(req.NetworkPolicy.AllowHosts, validAppInstallHost) {
		return invalid("invalid network policy")
	}
	if req.EntitlementPolicyID != "" && !validAppInstallReference(req.EntitlementPolicyID) {
		return invalid("invalid entitlement policy reference")
	}
	hasCredential := appInstallHasServiceCredential(req)
	hasAnyCredentialField := req.ServiceCredentialID != "" ||
		req.ServiceCredentialVersion != "" ||
		req.ServiceCredentialExpiry != 0 ||
		req.ServiceCredentialHash != ""
	if hasAnyCredentialField && !hasCredential {
		return invalid("invalid service credential reference")
	}
	return nil
}

func validAppInstallHTTPSURL(value *url.URL) bool {
	return value != nil &&
		value.Scheme == "https" &&
		value.Host != "" &&
		value.User == nil &&
		value.RawQuery == "" &&
		!value.ForceQuery &&
		value.Fragment == ""
}

func validAppInstallEndpoint(baseURL, endpointURL *url.URL) bool {
	if !validAppInstallHTTPSURL(endpointURL) || endpointURL.Host != baseURL.Host {
		return false
	}
	basePath, err := canonicalAppInstallPath(baseURL)
	if err != nil {
		return false
	}
	endpointPath, err := canonicalAppInstallPath(endpointURL)
	if err != nil {
		return false
	}
	return endpointPath == basePath || strings.HasPrefix(endpointPath, strings.TrimSuffix(basePath, "/")+"/")
}

func canonicalAppInstallPath(value *url.URL) (string, error) {
	decoded := value.EscapedPath()
	for {
		next, err := url.PathUnescape(decoded)
		if err != nil {
			return "", err
		}
		if next == decoded {
			break
		}
		decoded = next
	}
	if strings.ContainsRune(decoded, '\x00') {
		return "", fmt.Errorf("invalid NUL in path")
	}
	return path.Clean("/" + decoded), nil
}

func validAppInstallOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	return err == nil &&
		validAppInstallHTTPSURL(parsed) &&
		parsed.Path == "" &&
		parsed.RawPath == ""
}

func validAppInstallHost(host string) bool {
	if !validAppInstallReference(host) || strings.ContainsAny(host, "/?#@") {
		return false
	}
	parsed, err := url.Parse("https://" + host)
	return err == nil && parsed.Host == host && parsed.Hostname() != ""
}

func validAppInstallReference(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func validAppInstallStringSet(values []string, valid func(string) bool) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !valid(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validAppInstallSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func appInstallHasServiceCredential(req AppInstallRequest) bool {
	return validAppInstallReference(req.ServiceCredentialID) &&
		validAppInstallReference(req.ServiceCredentialVersion) &&
		req.ServiceCredentialExpiry > 0 &&
		validAppInstallSHA256(req.ServiceCredentialHash)
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
		ClaimKey:         appPluginSHA256([]byte("route\x00" + endpoint)),
	}
}

func appKeyClaim(appKey, installationID string) AppRouteClaim {
	return AppRouteClaim{
		AppKey:         appKey,
		InstallationID: installationID,
		Kind:           "app_key",
		ClaimKey:       appPluginSHA256([]byte("app_key\x00" + appKey)),
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

func findFrozenAppInstallResponse(tx *gorm.DB, installationID, appVersionID string) (AppInstallResult, bool, error) {
	var frozen AppInstallationIdempotency
	query := lockForUpdate(tx).Where("installation_id = ? AND app_version_id = ? AND response_json <> ''", installationID, appVersionID).
		Order("id").
		Limit(1).
		Find(&frozen)
	if query.Error != nil {
		return AppInstallResult{}, false, query.Error
	}
	if query.RowsAffected == 0 {
		return AppInstallResult{}, false, nil
	}
	var result AppInstallResult
	if err := common.Unmarshal([]byte(frozen.ResponseJSON), &result); err != nil {
		return AppInstallResult{}, false, err
	}
	result.AppVersionID = frozen.AppVersionID
	result.ResponseDigest = frozen.ResponseDigest
	return result, true, nil
}

func freezeAppInstallIdempotency(tx *gorm.DB, scopeHash, claimToken string, result *AppInstallResult) error {
	responseJSON, digest, err := frozenAppInstallResponse(*result)
	if err != nil {
		return err
	}
	if result.ResponseDigest == "" {
		result.ResponseDigest = digest
	} else {
		digest = result.ResponseDigest
	}
	update := tx.Model(&AppInstallationIdempotency{}).
		Where("scope_hash = ? AND claim_token = ?", scopeHash, claimToken).
		Updates(map[string]any{
			"installation_id": result.InstallationID,
			"app_version_id":  result.AppVersionID,
			"response_json":   responseJSON,
			"response_digest": digest,
		})
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return ErrAppIdempotencyConflict
	}
	return nil
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
