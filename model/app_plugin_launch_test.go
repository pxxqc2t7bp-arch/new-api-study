package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
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

func openAppCredentialTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousType := common.MainDatabaseType()
	t.Cleanup(func() { common.SetMainDatabaseType(previousType) })
	dialect, dsn := os.Getenv("APP_PLUGIN_TEST_DIALECT"), os.Getenv("APP_PLUGIN_TEST_DSN")
	name := "app_credential_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	var db *gorm.DB
	var err error
	switch dialect {
	case "", "sqlite":
		db, err = gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "credential.sqlite")), config)
	case "mysql":
		require.NotEmpty(t, dsn)
		parsed, parseErr := mysqlDriver.ParseDSN(dsn)
		require.NoError(t, parseErr)
		admin, openErr := gorm.Open(mysql.Open(dsn), config)
		require.NoError(t, openErr)
		require.NoError(t, admin.Exec("CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error)
		t.Cleanup(func() {
			assert.NoError(t, admin.Exec("DROP DATABASE `"+name+"`").Error)
			conn, err := admin.DB()
			require.NoError(t, err)
			assert.NoError(t, conn.Close())
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
	common.SetMainDatabaseType(map[string]common.DatabaseType{
		"sqlite": common.DatabaseTypeSQLite, "mysql": common.DatabaseTypeMySQL, "postgres": common.DatabaseTypePostgreSQL,
	}[db.Dialector.Name()])
	connection, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, connection.Close()) })
	if db.Dialector.Name() == "sqlite" {
		connection.SetMaxOpenConns(1)
	}
	logAppPluginDBVersion(t, db)
	if dialect != "" {
		assertAppPluginModelRuntimeMetadata(t, db)
	}
	return db
}

func appCredentialFixture(t *testing.T) (*gorm.DB, AppInstallResult, time.Time) {
	t.Helper()
	db := openAppCredentialTestDB(t)
	require.NoError(t, MigrateAppPluginTables(db))
	require.NoError(t, MigrateAppPluginLaunchTables(db))
	key := "credential-" + uuid.NewString()
	req := appPluginModelInstallRequest(key, "1.0.0", "https://apps.example.com/"+key+"/")
	req.EntitlementPolicyID = ""
	req.ServiceCredentialHash, req.ServiceCredentialID, req.ServiceCredentialVersion = "", "", ""
	req.ServiceCredentialExpiry = 0
	installation, err := InstallAppVersion(t.Context(), db, AppIdempotencyScope{ActorID: 101, Key: key}, req)
	require.NoError(t, err)
	return db, installation, time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
}

func TestServiceCredentialPlaintextIsReturnedOnceAndStoredHashed(t *testing.T) {
	db, installation, now := appCredentialFixture(t)
	issued, err := IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read", "task.read"}, now, now.Add(24*time.Hour), false)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(issued.Credential), 43)
	require.NotEmpty(t, issued.CredentialID)
	require.NotEmpty(t, issued.Version)
	identity, err := AuthenticateAppServiceCredential(t.Context(), db, issued.Credential, now)
	require.NoError(t, err)
	assert.Equal(t, installation.InstallationID, identity.InstallationID)
	assert.Equal(t, installation.AppKey, identity.AppKey)
	assert.Equal(t, issued.Version, identity.Version)
	assert.ElementsMatch(t, []string{"identity.read", "task.read"}, identity.Scopes)
	var stored AppServiceCredential
	require.NoError(t, db.Where("credential_id = ?", issued.CredentialID).First(&stored).Error)
	digest := sha256.Sum256([]byte(issued.Credential))
	assert.Equal(t, hex.EncodeToString(digest[:]), stored.CredentialHash)
	assert.NotEqual(t, issued.Credential, stored.CredentialHash)
	assert.Equal(t, "active", stored.Status)
	assert.Equal(t, now.Add(24*time.Hour).Unix(), stored.ExpiresAt)
	var rows []map[string]any
	require.NoError(t, db.Table("app_service_credentials").Where("installation_id = ?", installation.InstallationID).Find(&rows).Error)
	raw, err := common.Marshal(rows)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), issued.Credential)
	for _, row := range rows {
		for column := range row {
			assert.NotContains(t, column, "ciphertext")
			assert.NotContains(t, column, "plaintext")
			assert.NotContains(t, column, "secret")
		}
	}
	metadata, err := common.Marshal(stored)
	require.NoError(t, err)
	assert.NotContains(t, string(metadata), issued.Credential)
	assert.NotContains(t, string(metadata), stored.CredentialHash)
	_, err = IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read"}, now, now.Add(24*time.Hour), false)
	require.Error(t, err, "create is not a credential retrieval or implicit rotation endpoint")
	t.Run("invalid expiry and scope do not persist", func(t *testing.T) {
		for _, expiry := range []time.Time{now, now.Add(-time.Second)} {
			_, err := IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
				[]string{"identity.read"}, now, expiry, true)
			require.Error(t, err)
		}
		_, err := IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
			[]string{"dashboard.admin"}, now, now.Add(time.Hour), true)
		require.Error(t, err)
	})
	t.Run("migrations preserve credential across restart", func(t *testing.T) {
		require.NoError(t, MigrateAppPluginLaunchTables(db))
		require.NoError(t, MigrateAppPluginLaunchTables(db))
		reloaded, err := AuthenticateAppServiceCredential(t.Context(), db.Session(&gorm.Session{NewDB: true}), issued.Credential, now)
		require.NoError(t, err)
		assert.Equal(t, identity, reloaded)
		for _, table := range []any{&AppPluginLaunchCode{}, &AppPluginSession{}, &AppPluginExchangeReplay{}, &AppPluginSessionRevokeReplay{}} {
			assert.True(t, db.Migrator().HasTable(table))
		}
	})
	t.Run("upgrade from B1.4 and rollback preserve existing rows", func(t *testing.T) {
		upgradeDB := openAppCredentialTestDB(t)
		// Frozen credential table shape from B1.4 (46f3a3d), independent of
		// the current credential model and the new scoped binding table.
		type b14Credential struct {
			ID                uint   `gorm:"primaryKey"`
			AppKey            string `gorm:"size:128;not null;index"`
			InstallationID    string `gorm:"size:64;not null;index"`
			CredentialID      string `gorm:"size:255;not null"`
			CredentialHash    string `gorm:"size:64;not null"`
			CredentialVersion string `gorm:"size:128;not null"`
			Status            string `gorm:"size:32;not null"`
			ExpiresAt         int64  `gorm:"not null"`
			CreatedAt         time.Time
		}
		require.NoError(t, upgradeDB.Table("app_service_credentials").AutoMigrate(&b14Credential{}))
		require.NoError(t, MigrateAppPluginTables(upgradeDB))
		req := appPluginModelInstallRequest("legacy", "1.0.0", "https://apps.example.com/legacy/")
		req.EntitlementPolicyID = ""
		req.ServiceCredentialHash, req.ServiceCredentialID, req.ServiceCredentialVersion = "", "", ""
		req.ServiceCredentialExpiry = 0
		legacyApp, err := InstallAppVersion(t.Context(), upgradeDB, AppIdempotencyScope{ActorID: 101, Key: "legacy"}, req)
		require.NoError(t, err)
		legacySecret := strings.Repeat("z", 43)
		legacy := b14Credential{AppKey: "legacy", InstallationID: legacyApp.InstallationID,
			CredentialID: "legacy-id", CredentialHash: appPluginDigest(legacySecret),
			CredentialVersion: "v1", Status: "active", ExpiresAt: now.Add(time.Hour).Unix()}
		require.NoError(t, upgradeDB.Table("app_service_credentials").Create(&legacy).Error)
		assert.False(t, upgradeDB.Migrator().HasTable(&AppServiceCredentialBinding{}))
		var frozenBefore AppInstallationIdempotency
		var claimsBefore []AppRouteClaim
		require.NoError(t, upgradeDB.First(&frozenBefore).Error)
		require.NoError(t, upgradeDB.Order("id").Find(&claimsBefore).Error)
		beforeColumns, err := upgradeDB.Migrator().ColumnTypes(&AppServiceCredential{})
		require.NoError(t, err)
		require.NoError(t, MigrateAppPluginLaunchTables(upgradeDB))
		require.NoError(t, MigrateAppPluginTables(upgradeDB))
		require.NoError(t, MigrateAppPluginLaunchTables(upgradeDB))
		afterColumns, err := upgradeDB.Migrator().ColumnTypes(&AppServiceCredential{})
		require.NoError(t, err)
		require.Len(t, afterColumns, len(beforeColumns))
		for i := range beforeColumns {
			assert.Equal(t, beforeColumns[i].Name(), afterColumns[i].Name())
			assert.Equal(t, beforeColumns[i].DatabaseTypeName(), afterColumns[i].DatabaseTypeName())
		}
		var preserved AppServiceCredential
		require.NoError(t, upgradeDB.Where("credential_id = ?", legacy.CredentialID).First(&preserved).Error)
		assert.Equal(t, legacy.CredentialHash, preserved.CredentialHash)
		assert.Equal(t, legacy.ExpiresAt, preserved.ExpiresAt)
		var frozenAfter AppInstallationIdempotency
		var claimsAfter []AppRouteClaim
		require.NoError(t, upgradeDB.First(&frozenAfter).Error)
		require.NoError(t, upgradeDB.Order("id").Find(&claimsAfter).Error)
		assert.Equal(t, frozenBefore, frozenAfter, "migration cannot rewrite existing frozen installation responses")
		assert.Equal(t, claimsBefore, claimsAfter)
		_, err = AuthenticateAppServiceCredential(t.Context(), upgradeDB, legacySecret, now)
		require.ErrorContains(t, err, "service_identity_invalid", "unscoped legacy credentials must fail closed")
		upgraded, err := IssueAppServiceCredential(t.Context(), upgradeDB, legacyApp.InstallationID,
			[]string{"identity.read"}, now, now.Add(time.Hour), true)
		require.NoError(t, err)
		_, err = AuthenticateAppServiceCredential(t.Context(), upgradeDB, upgraded.Credential, now)
		require.NoError(t, err)
		var binding AppServiceCredentialBinding
		require.NoError(t, upgradeDB.Where("credential_id = ?", upgraded.CredentialID).First(&binding).Error)
		binding.CredentialRowID++
		require.Error(t, upgradeDB.Create(&binding).Error, "credential ID uniqueness must survive restart")
		rollback := errors.New("injected outer transaction failure")
		err = RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			_, err := IssueAppServiceCredential(t.Context(), tx, installation.InstallationID,
				[]string{"identity.read"}, now, now.Add(time.Hour), true)
			if err != nil {
				return err
			}
			return rollback
		})
		require.ErrorIs(t, err, rollback)
		var current AppServiceCredential
		require.NoError(t, db.Where("credential_id = ?", issued.CredentialID).First(&current).Error)
		assert.Equal(t, stored.ExpiresAt, current.ExpiresAt, "failed rotation must not shorten the old credential")
		assert.EqualValues(t, 1, appPluginCountForInstallation(t, db, &AppServiceCredential{}, installation.InstallationID))
	})
}

func TestServiceCredentialRotationOverlapsForTenMinutes(t *testing.T) {
	db, installation, now := appCredentialFixture(t)
	first, err := IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read"}, now, now.Add(time.Hour), false)
	require.NoError(t, err)
	rotated, err := IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read"}, now, now.Add(2*time.Hour), true)
	require.NoError(t, err)
	assert.NotEqual(t, first.Credential, rotated.Credential)
	assert.NotEqual(t, first.Version, rotated.Version)
	assert.Equal(t, now.Add(10*time.Minute), rotated.OverlapUntil)
	_, err = AuthenticateAppServiceCredential(t.Context(), db, first.Credential, now.Add(10*time.Minute-time.Second))
	require.NoError(t, err)
	_, err = AuthenticateAppServiceCredential(t.Context(), db, first.Credential, now.Add(10*time.Minute))
	require.ErrorContains(t, err, "service_identity_invalid")
	_, err = AuthenticateAppServiceCredential(t.Context(), db, rotated.Credential, now.Add(10*time.Minute))
	require.NoError(t, err)
	third, err := IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read"}, now.Add(time.Minute), now.Add(3*time.Hour), true)
	require.NoError(t, err)
	assert.NotEqual(t, rotated.Version, third.Version)
	var old AppServiceCredential
	require.NoError(t, db.Where("credential_id = ?", first.CredentialID).First(&old).Error)
	assert.Equal(t, now.Add(10*time.Minute).Unix(), old.ExpiresAt, "a later rotation cannot extend earlier overlap")
	require.NoError(t, db.Model(&AppServiceCredential{}).Where("credential_id = ?", third.CredentialID).
		Update("expires_at", now.Add(2*time.Minute).Unix()).Error)
	_, err = IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read"}, now.Add(time.Minute), now.Add(4*time.Hour), true)
	require.NoError(t, err)
	old = AppServiceCredential{}
	require.NoError(t, db.Where("credential_id = ?", third.CredentialID).First(&old).Error)
	assert.Equal(t, now.Add(2*time.Minute).Unix(), old.ExpiresAt)
}

func TestServiceCredentialEmergencyRevokeIsImmediate(t *testing.T) {
	db, installation, now := appCredentialFixture(t)
	first, err := IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read"}, now, now.Add(time.Hour), false)
	require.NoError(t, err)
	second, err := IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read"}, now, now.Add(time.Hour), true)
	require.NoError(t, err)
	require.NoError(t, RevokeAppServiceCredential(t.Context(), db, installation.InstallationID, first.CredentialID, now))
	_, err = AuthenticateAppServiceCredential(t.Context(), db, first.Credential, now)
	require.ErrorContains(t, err, "service_identity_invalid")
	_, err = AuthenticateAppServiceCredential(t.Context(), db, second.Credential, now)
	require.NoError(t, err)
	require.NoError(t, RevokeAppServiceCredential(t.Context(), db, installation.InstallationID, first.CredentialID, now))
	err = RevokeAppServiceCredential(t.Context(), db, "wrong-installation", second.CredentialID, now)
	require.ErrorContains(t, err, "not_found")
	for _, raw := range []string{"", strings.Repeat("x", 43), second.Credential + "x"} {
		_, err := AuthenticateAppServiceCredential(t.Context(), db, raw, now)
		require.ErrorContains(t, err, "service_identity_invalid")
	}
	_, err = CompareAndSwapAppInstallationStatus(t.Context(), db, installation.InstallationID, installation.Revision, "revoked")
	require.NoError(t, err)
	_, err = AuthenticateAppServiceCredential(t.Context(), db, second.Credential, now)
	require.ErrorContains(t, err, "service_identity_invalid")
	_, err = IssueAppServiceCredential(t.Context(), db, installation.InstallationID,
		[]string{"identity.read"}, now, now.Add(time.Hour), true)
	require.Error(t, err)
}
