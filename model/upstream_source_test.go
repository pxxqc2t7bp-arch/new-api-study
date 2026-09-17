package model

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type legacyUpstreamSource struct {
	ID                  int64  `gorm:"primaryKey"`
	Key                 string `gorm:"type:varchar(32);not null;uniqueIndex"`
	Name                string `gorm:"type:varchar(128);not null"`
	ConsoleURL          string `gorm:"type:varchar(512);not null"`
	AdapterVersion      string `gorm:"type:varchar(64)"`
	EndpointCandidates  string `gorm:"type:text"`
	SelectedEndpoint    string `gorm:"type:varchar(512)"`
	Status              string `gorm:"type:varchar(32);not null;index"`
	Enabled             bool   `gorm:"not null"`
	Balance             *float64
	LowBalanceThreshold float64 `gorm:"not null"`
	LastSnapshotID      string  `gorm:"type:varchar(64);index"`
	LastSnapshotAt      int64   `gorm:"bigint;index"`
	LastSuccessAt       int64   `gorm:"bigint"`
	LastError           string  `gorm:"type:text"`
	CreatedAt           int64   `gorm:"bigint;index"`
	UpdatedAt           int64   `gorm:"bigint;index"`
}

func (legacyUpstreamSource) TableName() string {
	return "upstream_sources"
}

func TestUpstreamSourceMigrationAddsLowBalanceAlertedToExistingSQLiteTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&legacyUpstreamSource{}))
	require.NoError(t, db.Create(&legacyUpstreamSource{
		ID:                  1,
		Key:                 "leyi",
		Name:                "LeyI",
		ConsoleURL:          "https://leyi12.xyz",
		Status:              "operational",
		Enabled:             true,
		LowBalanceThreshold: 5,
	}).Error)

	require.NoError(t, db.AutoMigrate(&UpstreamSource{}))

	var source UpstreamSource
	require.NoError(t, db.First(&source, 1).Error)
	assert.False(t, source.LowBalanceAlerted)
	assert.True(t, db.Migrator().HasColumn(&UpstreamSource{}, "low_balance_alerted"))
}
