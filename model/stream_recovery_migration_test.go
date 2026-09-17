package model

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type legacyStreamRecoveryToken struct {
	Id             int    `gorm:"primaryKey"`
	UserId         int    `gorm:"index"`
	Key            string `gorm:"column:key;type:varchar(128);uniqueIndex"`
	Status         int
	Name           string
	CreatedTime    int64
	AccessedTime   int64
	ExpiredTime    int64
	RemainQuota    int
	UnlimitedQuota bool
}

func (legacyStreamRecoveryToken) TableName() string {
	return "tokens"
}

func TestStreamRecoveryMigrationSQLiteFreshAndUpgrade(t *testing.T) {
	database, err := gorm.Open(
		sqlite.Open(filepath.Join(t.TempDir(), "stream-recovery-migration.db")),
		&gorm.Config{},
	)
	require.NoError(t, err)
	var version string
	require.NoError(t, database.Raw("SELECT sqlite_version()").Scan(&version).Error)
	t.Logf("SQLite version: %s", version)
	verifyStreamRecoveryMigration(t, database)
}

func TestStreamRecoveryMigrationExternalFreshAndUpgrade(t *testing.T) {
	driver := os.Getenv("STREAM_RECOVERY_MIGRATION_DRIVER")
	dsn := os.Getenv("STREAM_RECOVERY_MIGRATION_DSN")
	if driver == "" || dsn == "" {
		t.Skip("external migration database is not configured")
	}
	var dialector gorm.Dialector
	switch driver {
	case "mysql":
		dialector = mysql.Open(dsn)
	case "postgres":
		dialector = postgres.Open(dsn)
	default:
		t.Fatalf("unsupported migration driver %q", driver)
	}
	database, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err)
	var version string
	require.NoError(t, database.Raw("SELECT VERSION()").Scan(&version).Error)
	t.Logf("%s version: %s", driver, version)
	require.NoError(t, database.Migrator().DropTable(&StreamExecution{}, &Token{}))
	verifyStreamRecoveryMigration(t, database)
}

func verifyStreamRecoveryMigration(t *testing.T, database *gorm.DB) {
	t.Helper()
	previousDB := DB
	DB = database
	defer func() { DB = previousDB }()
	require.NoError(t, database.AutoMigrate(&legacyStreamRecoveryToken{}))
	legacy := legacyStreamRecoveryToken{
		UserId: 1,
		Key:    "migration-key",
		Status: 1,
		Name:   "legacy",
	}
	require.NoError(t, database.Create(&legacy).Error)

	require.NoError(t, database.AutoMigrate(&Token{}, &StreamExecution{}))
	require.NoError(t, database.AutoMigrate(&Token{}, &StreamExecution{}))
	assert.True(t, database.Migrator().HasColumn(&Token{}, "stream_recovery_enabled"))
	assert.True(t, database.Migrator().HasTable(&StreamExecution{}))

	var migrated Token
	require.NoError(t, database.First(&migrated, legacy.Id).Error)
	assert.False(t, migrated.StreamRecoveryEnabled)

	execution := &StreamExecution{
		StreamID:      "stream_migration",
		DedupeKey:     "dedupe_migration",
		UserID:        1,
		TokenID:       migrated.Id,
		ModelName:     "glm-5.3",
		RelayFormat:   "openai_responses",
		RequestPath:   "/v1/responses",
		RequestDigest: fmt.Sprintf("%064d", 1),
		ExpiresAt:     2_000_000_000,
	}
	inserted, created, err := CreateOrGetStreamExecution(execution)
	require.NoError(t, err)
	require.True(t, created)
	duplicate := *execution
	duplicate.ID = 0
	duplicate.StreamID = "stream_migration_duplicate"
	existing, created, err := CreateOrGetStreamExecution(&duplicate)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, inserted.StreamID, existing.StreamID)

	require.NoError(t, database.Migrator().DropTable(&StreamExecution{}, &Token{}))
	require.NoError(t, database.AutoMigrate(&Token{}, &StreamExecution{}))
	require.NoError(t, database.AutoMigrate(&Token{}, &StreamExecution{}))
	assert.True(t, database.Migrator().HasColumn(&Token{}, "stream_recovery_enabled"))
	assert.True(t, database.Migrator().HasTable(&StreamExecution{}))
}
