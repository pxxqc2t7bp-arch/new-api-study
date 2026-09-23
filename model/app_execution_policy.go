package model

import (
	"errors"
	"math"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	AppModelInvokePolicyKey    = "AppModelInvokePolicy"
	AppArkImportDelegationsKey = "AppArkImportDelegations"
)

// The option contains only the current version ID. Published documents are
// append-only; grants retain their version even after the head changes.
type AppExecutionPolicyVersion struct {
	Version        int64  `gorm:"primaryKey;autoIncrement:false"`
	PolicyKey      string `gorm:"primaryKey;size:64"`
	DocumentSHA256 string `gorm:"size:64;not null"`
	CanonicalJSON  string `gorm:"type:text;not null"`
	PublishedBy    int    `gorm:"not null"`
	PublishedAt    int64  `gorm:"not null"`
}

type AppPassthroughRuleVersion struct {
	IdentityHash string `gorm:"primaryKey;size:64"`
	SchemaSHA256 string `gorm:"size:64;not null"`
}

func PublishAppPassthroughRuleTx(tx *gorm.DB, identity, schema string) error {
	row := AppPassthroughRuleVersion{IdentityHash: identity, SchemaSHA256: schema}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return err
	}
	var stored AppPassthroughRuleVersion
	if err := lockForUpdate(tx).Where("identity_hash = ?", identity).First(&stored).Error; err != nil {
		return err
	}
	if stored.SchemaSHA256 != schema {
		return errors.New("immutable_rule")
	}
	return nil
}

func IsAppExecutionPolicyOption(key string) bool {
	return key == AppModelInvokePolicyKey || key == AppArkImportDelegationsKey
}

// PublishAppExecutionPolicyTx requires current root/session authorization by
// the service in this same transaction. The owner must use
// RunAppPluginTransaction when it owns the outer transaction; this function
// never retries a caller-owned transaction. Generic option writes cannot call it.
func PublishAppExecutionPolicyTx(tx *gorm.DB, key, canonical string, actor int, now time.Time) (int64, error) {
	if !IsAppExecutionPolicyOption(key) || canonical == "" || actor <= 0 {
		return 0, errors.New("invalid_policy")
	}
	head, err := lockOptionForWriteTx(tx, key, "0", optionWriteAppPolicy)
	if err != nil {
		return 0, err
	}
	current, err := strconv.ParseInt(head.Value, 10, 64)
	if err != nil || current < 0 || current == math.MaxInt64 {
		return 0, errors.New("invalid_policy_version")
	}
	version := current + 1
	row := AppExecutionPolicyVersion{Version: version, PolicyKey: key, CanonicalJSON: canonical,
		DocumentSHA256: appPluginStableID("app-execution-policy/v1", key, canonical), PublishedBy: actor, PublishedAt: now.Unix()}
	if err := tx.Create(&row).Error; err != nil {
		return 0, err
	}
	if err := saveOptionTx(tx, key, strconv.FormatInt(version, 10), optionWriteAppPolicy); err != nil {
		return 0, err
	}
	return version, nil
}

func GetAppExecutionPolicyTx(tx *gorm.DB, key string) (AppExecutionPolicyVersion, error) {
	var head Option
	var version AppExecutionPolicyVersion
	if err := lockForUpdate(tx).Where(clause.Eq{Column: "key", Value: key}).First(&head).Error; err != nil {
		return version, err
	}
	number, err := strconv.ParseInt(head.Value, 10, 64)
	if err != nil || number <= 0 {
		return version, errors.New("invalid_policy_version")
	}
	err = lockForUpdate(tx).Where("version = ? AND policy_key = ?", number, key).First(&version).Error
	if err == nil && version.DocumentSHA256 != appPluginStableID("app-execution-policy/v1", key, version.CanonicalJSON) {
		return AppExecutionPolicyVersion{}, errors.New("invalid_policy")
	}
	return version, err
}
