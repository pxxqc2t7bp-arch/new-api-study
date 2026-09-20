package model

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"time"

	"gorm.io/gorm"
)

// AppServiceCredentialBinding adds immutable scopes/version fencing without
// changing the B1.4 credential schema or its frozen installation responses.
// Legacy credentials without a binding cannot authenticate.
type AppServiceCredentialBinding struct {
	CredentialRowID uint          `gorm:"primaryKey;autoIncrement:false" json:"-"`
	CredentialID    string        `gorm:"size:64;not null;uniqueIndex" json:"-"`
	Version         string        `gorm:"size:64;not null" json:"-"`
	Scopes          AppStringList `gorm:"type:text;not null" json:"-"`
}

type AppServiceCredentialIssued struct {
	Credential   string    `json:"credential"`
	CredentialID string    `json:"credential_id"`
	Version      string    `json:"version"`
	ExpiresAt    int64     `json:"expires_at"`
	OverlapUntil time.Time `json:"overlap_until,omitempty"`
}

type AppServiceIdentity struct {
	InstallationID string
	AppKey         string
	CredentialID   string
	Version        string
	Scopes         []string
}

type AppPluginLaunchCode struct {
	ID             uint   `gorm:"primaryKey" json:"-"`
	ScopeHash      string `gorm:"size:64;not null;uniqueIndex" json:"-"`
	RequestHash    string `gorm:"size:64;not null" json:"-"`
	InstallationID string `gorm:"size:64;not null;index" json:"-"`
	CodeHash       string `gorm:"size:64;not null;uniqueIndex" json:"-"`
	Salt           string `gorm:"size:64;not null" json:"-"`
	BindingJSON    string `gorm:"type:text;not null" json:"-"`
	ExpiresAt      int64  `gorm:"not null;index" json:"-"`
	ConsumedAt     int64  `gorm:"not null" json:"-"`
	AppSessionID   string `gorm:"size:64;not null" json:"-"`
}

type AppPluginLaunchBinding struct {
	Issuer             string
	UserID             int
	DashboardSessionID string
	AuthVersion        int64
	SessionVersion     int64
	InstallationID     string
	AppKey             string
	Generation         string
	Callback           string
	Surface            string
	TransactionID      string
	StateHash          string
	NonceHash          string
	CodeChallenge      string
	UpstreamExpiresAt  int64
	ExpiresAt          int64
}

type AppPluginSession struct {
	AppSessionID       string        `gorm:"primaryKey;size:64" json:"-"`
	InstallationID     string        `gorm:"size:64;not null;index" json:"-"`
	AppKey             string        `gorm:"size:128;not null" json:"-"`
	Generation         string        `gorm:"size:64;not null" json:"-"`
	Issuer             string        `gorm:"size:512;not null" json:"-"`
	Subject            string        `gorm:"size:64;not null" json:"-"`
	UserID             int           `gorm:"not null" json:"-"`
	DashboardSessionID string        `gorm:"size:64;not null" json:"-"`
	AuthVersion        int64         `gorm:"not null" json:"-"`
	SessionVersion     int64         `gorm:"not null" json:"-"`
	GrantedScopes      AppStringList `gorm:"type:text;not null" json:"-"`
	UpstreamExpiresAt  int64         `gorm:"not null" json:"-"`
	RevokedAt          int64         `gorm:"not null" json:"-"`
}

type AppPluginExchangeReplay struct {
	ScopeHash      string  `gorm:"primaryKey;size:64" json:"-"`
	RequestHash    string  `gorm:"size:64;not null" json:"-"`
	InstallationID string  `gorm:"size:64;not null;index;uniqueIndex:idx_app_plugin_exchange_replay_launch,priority:1" json:"-"`
	LaunchCodeHash *string `gorm:"size:64;uniqueIndex:idx_app_plugin_exchange_replay_launch,priority:2" json:"-"`
	AppSessionID   *string `gorm:"size:64" json:"-"`
	ResponseJSON   string  `gorm:"type:text;not null" json:"-"`
	ResponseMAC    string  `gorm:"size:64" json:"-"`
	ExpiresAt      int64   `gorm:"not null;index" json:"-"`
}

type AppPluginSessionRevokeReplay struct {
	ScopeHash      string `gorm:"primaryKey;size:64" json:"-"`
	RequestHash    string `gorm:"size:64;not null" json:"-"`
	InstallationID string `gorm:"size:64;not null;index" json:"-"`
	ResponseJSON   string `gorm:"type:text;not null" json:"-"`
}

func MigrateAppPluginLaunchTables(db *gorm.DB) error {
	return db.AutoMigrate(&AppServiceCredentialBinding{}, &AppPluginLaunchCode{},
		&AppPluginSession{}, &AppPluginExchangeReplay{}, &AppPluginSessionRevokeReplay{})
}

// AppPluginCurrentRead is for child reads after ownership/installation locking.
// In particular it must not use an earlier MySQL REPEATABLE READ snapshot.
func AppPluginCurrentRead(tx *gorm.DB) *gorm.DB {
	return lockForUpdate(tx)
}

// LockAppPluginInstallation requires an outer RunAppPluginTransaction. A
// revoked installation has no ownership claim, but may still be read by ID to
// report revocation or perform emergency credential revocation.
func LockAppPluginInstallation(tx *gorm.DB, appKey, installationID string) (AppInstallation, error) {
	var owner AppRouteClaim
	q := lockForUpdate(tx).Where("claim_key = ?", appKeyClaim(appKey, "").ClaimKey).Limit(1).Find(&owner)
	if q.Error != nil {
		return AppInstallation{}, q.Error
	}
	if installationID == "" {
		installationID = owner.InstallationID
	}
	var installation AppInstallation
	if installationID == "" {
		return installation, gorm.ErrRecordNotFound
	}
	if err := lockForUpdate(tx).Where("installation_id = ? AND app_key = ?", installationID, appKey).First(&installation).Error; err != nil {
		return installation, err
	}
	if installation.Status != AppInstallationStatusRevoked &&
		(q.RowsAffected != 1 || owner.InstallationID != installationID || owner.AppKey != appKey || owner.Kind != "app_key") {
		return AppInstallation{}, gorm.ErrRecordNotFound
	}
	return installation, nil
}

func NewAppPluginOpaqueID() (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

func IssueAppServiceCredential(ctx context.Context, db *gorm.DB, installationID string, scopes []string, now, expiry time.Time, rotate bool) (AppServiceCredentialIssued, error) {
	if !expiry.After(now) || expiry.Unix() <= now.Unix() || len(scopes) == 0 {
		return AppServiceCredentialIssued{}, errors.New("invalid_request")
	}
	scopes = slices.Clone(scopes)
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	for _, scope := range scopes {
		switch scope {
		case "identity.read", "model.invoke", "task.import", "task.read", "resource.reserve", "resource.meter":
		default:
			return AppServiceCredentialIssued{}, errors.New("scope_denied")
		}
	}
	secret, err := NewAppPluginOpaqueID()
	if err != nil {
		return AppServiceCredentialIssued{}, err
	}
	id, err := NewAppPluginOpaqueID()
	if err != nil {
		return AppServiceCredentialIssued{}, err
	}
	version, err := NewAppPluginOpaqueID()
	if err != nil {
		return AppServiceCredentialIssued{}, err
	}
	var result AppServiceCredentialIssued
	err = RunAppPluginTransaction(db.WithContext(ctx), func(tx *gorm.DB) error {
		result = AppServiceCredentialIssued{}
		var locator AppInstallation
		if err := tx.Select("app_key").Where("installation_id = ?", installationID).First(&locator).Error; err != nil {
			return err
		}
		installation, err := LockAppPluginInstallation(tx, locator.AppKey, installationID)
		if err != nil {
			return err
		}
		if installation.Status == AppInstallationStatusRevoked {
			return ErrAppInstallationRevoked
		}
		var existing []AppServiceCredential
		if err := lockForUpdate(tx).Where("installation_id = ?", installationID).Order("id").Find(&existing).Error; err != nil {
			return err
		}
		if !rotate && len(existing) != 0 {
			return errors.New("invalid_state_transition")
		}
		if rotate && len(existing) == 0 {
			return errors.New("invalid_state_transition")
		}
		overlap := time.Time{}
		if rotate {
			overlap = now.Add(10 * time.Minute)
			if err := tx.Model(&AppServiceCredential{}).
				Where("installation_id = ? AND status = ? AND expires_at > ?", installationID, "active", overlap.Unix()).
				Update("expires_at", overlap.Unix()).Error; err != nil {
				return err
			}
		}
		record := AppServiceCredential{
			AppKey: installation.AppKey, InstallationID: installationID, CredentialID: id,
			CredentialHash: appPluginSHA256([]byte(secret)), CredentialVersion: version,
			Status: "active", ExpiresAt: expiry.Unix(), CreatedAt: now,
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		binding := AppServiceCredentialBinding{CredentialRowID: record.ID, CredentialID: id, Version: version, Scopes: scopes}
		if err := tx.Create(&binding).Error; err != nil {
			return err
		}
		result = AppServiceCredentialIssued{Credential: secret, CredentialID: id, Version: version,
			ExpiresAt: expiry.Unix(), OverlapUntil: overlap}
		return nil
	})
	if err != nil {
		return AppServiceCredentialIssued{}, err
	}
	return result, nil
}

func AuthenticateAppServiceCredential(ctx context.Context, db *gorm.DB, raw string, now time.Time) (AppServiceIdentity, error) {
	if len(raw) != 43 {
		return AppServiceIdentity{}, ErrAppServiceIdentityInvalid
	}
	digest := appPluginSHA256([]byte(raw))
	var identity AppServiceIdentity
	err := RunAppPluginTransaction(db.WithContext(ctx), func(tx *gorm.DB) error {
		identity = AppServiceIdentity{}
		var locator AppServiceCredential
		if err := tx.Where("credential_hash = ?", digest).First(&locator).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAppServiceIdentityInvalid
			}
			return err
		}
		installation, err := LockAppPluginInstallation(tx, locator.AppKey, locator.InstallationID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAppServiceIdentityInvalid
			}
			return err
		}
		if installation.Status == AppInstallationStatusRevoked {
			return ErrAppServiceIdentityInvalid
		}
		var current AppServiceCredential
		if err := lockForUpdate(tx).Where("id = ?", locator.ID).First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAppServiceIdentityInvalid
			}
			return err
		}
		if !hmac.Equal([]byte(current.CredentialHash), []byte(digest)) {
			return ErrAppServiceIdentityInvalid
		}
		identity = AppServiceIdentity{InstallationID: current.InstallationID, AppKey: current.AppKey,
			CredentialID: current.CredentialID, Version: current.CredentialVersion}
		identity, err = ValidateAppServiceIdentity(tx, identity, now)
		return err
	})
	if err != nil {
		return AppServiceIdentity{}, err
	}
	return identity, nil
}

// ValidateAppServiceIdentity rechecks a middleware identity inside the operation
// transaction. Callers already hold ownership and installation locks.
func ValidateAppServiceIdentity(tx *gorm.DB, identity AppServiceIdentity, now time.Time) (AppServiceIdentity, error) {
	var stored AppServiceCredential
	if err := lockForUpdate(tx).Where("credential_id = ? AND installation_id = ? AND app_key = ?",
		identity.CredentialID, identity.InstallationID, identity.AppKey).First(&stored).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AppServiceIdentity{}, ErrAppServiceIdentityInvalid
		}
		return AppServiceIdentity{}, err
	}
	var binding AppServiceCredentialBinding
	if err := lockForUpdate(tx).Where("credential_row_id = ?", stored.ID).First(&binding).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AppServiceIdentity{}, ErrAppServiceIdentityInvalid
		}
		return AppServiceIdentity{}, err
	}
	if stored.Status != "active" || stored.ExpiresAt <= now.Unix() ||
		stored.CredentialVersion != identity.Version || binding.Version != identity.Version ||
		binding.CredentialID != identity.CredentialID || len(binding.Scopes) == 0 {
		return AppServiceIdentity{}, ErrAppServiceIdentityInvalid
	}
	identity.Scopes = slices.Clone(binding.Scopes)
	return identity, nil
}

func AppPluginApprovedScopes(tx *gorm.DB, installation AppInstallation, now time.Time) ([]string, error) {
	var credentials []AppServiceCredential
	if err := lockForUpdate(tx).Where("installation_id = ? AND app_key = ? AND status = ? AND expires_at > ?",
		installation.InstallationID, installation.AppKey, "active", now.Unix()).Order("id").Find(&credentials).Error; err != nil {
		return nil, err
	}
	scopes := []string{}
	for _, credential := range credentials {
		var binding AppServiceCredentialBinding
		q := lockForUpdate(tx).Where("credential_row_id = ?", credential.ID).Limit(1).Find(&binding)
		if q.Error != nil {
			return nil, q.Error
		}
		if q.RowsAffected == 1 && binding.CredentialID == credential.CredentialID && binding.Version == credential.CredentialVersion {
			scopes = append(scopes, binding.Scopes...)
		}
	}
	slices.Sort(scopes)
	return slices.Compact(scopes), nil
}

func RevokeAppServiceCredential(ctx context.Context, db *gorm.DB, installationID, credentialID string, now time.Time) error {
	return RunAppPluginTransaction(db.WithContext(ctx), func(tx *gorm.DB) error {
		var locator AppInstallation
		if err := tx.Select("app_key").Where("installation_id = ?", installationID).First(&locator).Error; err != nil {
			return errors.New("not_found")
		}
		if _, err := LockAppPluginInstallation(tx, locator.AppKey, installationID); err != nil {
			return err
		}
		var credential AppServiceCredential
		if err := lockForUpdate(tx).Where("installation_id = ? AND credential_id = ?", installationID, credentialID).First(&credential).Error; err != nil {
			return errors.New("not_found")
		}
		return tx.Model(&credential).Updates(map[string]any{
			"status": "revoked", "expires_at": min(credential.ExpiresAt, now.Unix()),
		}).Error
	})
}
