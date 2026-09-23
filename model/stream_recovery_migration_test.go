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

type legacyStreamRecoveryExecution struct {
	ID                          int64                 `gorm:"primaryKey"`
	StreamID                    string                `gorm:"type:varchar(64);uniqueIndex"`
	DedupeKey                   string                `gorm:"type:varchar(64);uniqueIndex"`
	UserID                      int                   `gorm:"index"`
	TokenID                     int                   `gorm:"index"`
	ModelName                   string                `gorm:"type:varchar(191);index"`
	RelayFormat                 string                `gorm:"type:varchar(32)"`
	RequestPath                 string                `gorm:"type:varchar(255)"`
	RequestDigest               string                `gorm:"type:varchar(64)"`
	Status                      StreamExecutionStatus `gorm:"type:varchar(32);index"`
	PublicAttempt               int
	AttemptCount                int
	ActiveChannelID             int
	CommittedSequence           int64
	TerminalSequence            int64
	LockedBy                    string `gorm:"type:varchar(128);index"`
	LockedUntil                 int64  `gorm:"bigint;index"`
	BillingStatus               string `gorm:"type:varchar(32);index"`
	BillingSource               string `gorm:"type:varchar(32)"`
	ReservedQuota               int
	ActualQuota                 int
	TokenConsumed               int
	ExtraReserved               int
	Trusted                     bool
	SubscriptionID              int
	SubscriptionPreConsumed     int64 `gorm:"bigint"`
	SubscriptionAmountTotal     int64 `gorm:"bigint"`
	SubscriptionAmountUsedAfter int64 `gorm:"bigint"`
	SubscriptionPlanID          int
	SubscriptionPlanTitle       string `gorm:"type:varchar(255)"`
	ConsumeLogStatus            string `gorm:"type:varchar(32);index"`
	RecoveryReason              string `gorm:"type:text"`
	Error                       string `gorm:"type:text"`
	CreatedAt                   int64  `gorm:"bigint;index"`
	UpdatedAt                   int64  `gorm:"bigint;index"`
	ExpiresAt                   int64  `gorm:"bigint;index"`
}

func (legacyStreamRecoveryExecution) TableName() string {
	return "stream_executions"
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
	require.NoError(t, database.AutoMigrate(
		&legacyStreamRecoveryToken{},
		&legacyStreamRecoveryExecution{},
	))
	legacy := legacyStreamRecoveryToken{
		UserId: 1,
		Key:    "migration-key",
		Status: 1,
		Name:   "legacy",
	}
	require.NoError(t, database.Create(&legacy).Error)
	legacyExecution := legacyStreamRecoveryExecution{
		StreamID:          "stream_legacy_upgrade",
		DedupeKey:         "dedupe_legacy_upgrade",
		UserID:            1,
		TokenID:           legacy.Id,
		ModelName:         "glm-5.3",
		RelayFormat:       "openai_responses",
		RequestPath:       "/v1/responses",
		RequestDigest:     fmt.Sprintf("%064d", 2),
		Status:            StreamExecutionRunning,
		PublicAttempt:     1,
		AttemptCount:      1,
		CommittedSequence: 3,
		BillingStatus:     StreamBillingReserved,
		ConsumeLogStatus:  StreamConsumeLogNone,
		ExpiresAt:         2_000_000_000,
	}
	require.NoError(t, database.Create(&legacyExecution).Error)
	assert.False(t, database.Migrator().HasColumn(
		&legacyStreamRecoveryExecution{},
		"identity_version",
	))
	assert.False(t, database.Migrator().HasColumn(
		&legacyStreamRecoveryExecution{},
		"stable_dedupe_key",
	))

	require.NoError(t, database.AutoMigrate(&Token{}, &StreamExecution{}))
	require.NoError(t, database.AutoMigrate(&Token{}, &StreamExecution{}))
	assert.True(t, database.Migrator().HasColumn(&Token{}, "stream_recovery_enabled"))
	assert.True(t, database.Migrator().HasTable(&StreamExecution{}))
	assert.True(t, database.Migrator().HasColumn(&StreamExecution{}, "identity_version"))
	assert.True(t, database.Migrator().HasColumn(&StreamExecution{}, "stable_dedupe_key"))

	var migrated Token
	require.NoError(t, database.First(&migrated, legacy.Id).Error)
	assert.False(t, migrated.StreamRecoveryEnabled)
	var migratedExecution StreamExecution
	require.NoError(t, database.Where(
		"stream_id = ?",
		legacyExecution.StreamID,
	).First(&migratedExecution).Error)
	assert.Equal(t, legacyExecution.DedupeKey, migratedExecution.DedupeKey)
	assert.Equal(t, legacyExecution.CommittedSequence, migratedExecution.CommittedSequence)
	assert.Equal(t, StreamRecoveryIdentityVersionLegacy, migratedExecution.IdentityVersion)
	assert.Nil(t, migratedExecution.StableDedupeKey)

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

	stable := &StreamExecution{
		StreamID:        "stream_migration_stable",
		DedupeKey:       "stable-migration-key",
		IdentityVersion: StreamRecoveryIdentityVersionStable,
		UserID:          1,
		TokenID:         migrated.Id,
		ModelName:       "glm-rolling-upgrade",
		RelayFormat:     "openai_responses",
		RequestPath:     "/v1/responses",
		RequestDigest:   fmt.Sprintf("%064d", 3),
		ExpiresAt:       2_000_000_000,
	}
	const legacyDedupeKey = "legacy-migration-key"
	stableInserted, created, err := CreateOrGetStreamExecution(
		stable,
		legacyDedupeKey,
	)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, legacyDedupeKey, stableInserted.DedupeKey)
	require.NotNil(t, stableInserted.StableDedupeKey)
	assert.Equal(t, "stable-migration-key", *stableInserted.StableDedupeKey)

	oldNode := *stable
	oldNode.ID = 0
	oldNode.StreamID = "stream_migration_old_node"
	oldNode.DedupeKey = legacyDedupeKey
	oldNode.StableDedupeKey = nil
	oldNode.IdentityVersion = StreamRecoveryIdentityVersionLegacy
	existing, created, err = CreateOrGetStreamExecution(&oldNode)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, stableInserted.StreamID, existing.StreamID)

	require.NoError(t, database.Migrator().DropTable(&StreamExecution{}, &Token{}))
	require.NoError(t, database.AutoMigrate(&Token{}, &StreamExecution{}))
	require.NoError(t, database.AutoMigrate(&Token{}, &StreamExecution{}))
	assert.True(t, database.Migrator().HasColumn(&Token{}, "stream_recovery_enabled"))
	assert.True(t, database.Migrator().HasTable(&StreamExecution{}))
	assert.True(t, database.Migrator().HasColumn(&StreamExecution{}, "identity_version"))
	assert.True(t, database.Migrator().HasColumn(&StreamExecution{}, "stable_dedupe_key"))
}
