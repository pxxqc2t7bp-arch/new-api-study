package model

import (
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	UpstreamSourceKeyLeyi    = "leyi"
	UpstreamSourceKeyHualong = "hualong"
	UpstreamSourceKeyEBond   = "ebond"
)

// UpstreamSource is the server-side, credential-free definition of a Sub2API
// console. Browser login credentials never cross the companion boundary.
type UpstreamSource struct {
	ID                  int64    `json:"id" gorm:"primaryKey"`
	Key                 string   `json:"key" gorm:"type:varchar(32);not null;uniqueIndex"`
	Name                string   `json:"name" gorm:"type:varchar(128);not null"`
	ConsoleURL          string   `json:"console_url" gorm:"type:varchar(512);not null"`
	AdapterVersion      string   `json:"adapter_version" gorm:"type:varchar(64)"`
	EndpointCandidates  string   `json:"endpoint_candidates" gorm:"type:text"`
	SelectedEndpoint    string   `json:"selected_endpoint" gorm:"type:varchar(512)"`
	Status              string   `json:"status" gorm:"type:varchar(32);not null;index"`
	Enabled             bool     `json:"enabled" gorm:"not null"`
	Balance             *float64 `json:"balance,omitempty"`
	LowBalanceThreshold float64  `json:"low_balance_threshold" gorm:"not null"`
	LowBalanceAlerted   bool     `json:"low_balance_alerted" gorm:"not null;default:false"`
	LastSnapshotID      string   `json:"last_snapshot_id" gorm:"type:varchar(64);index"`
	LastSnapshotAt      int64    `json:"last_snapshot_at" gorm:"bigint;index"`
	LastSuccessAt       int64    `json:"last_success_at" gorm:"bigint"`
	LastError           string   `json:"last_error,omitempty" gorm:"type:text"`
	CreatedAt           int64    `json:"created_at" gorm:"bigint;index"`
	UpdatedAt           int64    `json:"updated_at" gorm:"bigint;index"`
}

func (UpstreamSource) TableName() string {
	return "upstream_sources"
}

func (source *UpstreamSource) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	source.Key = strings.TrimSpace(source.Key)
	source.ConsoleURL = strings.TrimRight(strings.TrimSpace(source.ConsoleURL), "/")
	if source.Status == "" {
		source.Status = "unknown"
	}
	if source.LowBalanceThreshold <= 0 {
		source.LowBalanceThreshold = 5
	}
	if source.CreatedAt == 0 {
		source.CreatedAt = now
	}
	if source.UpdatedAt == 0 {
		source.UpdatedAt = now
	}
	return nil
}

func UpsertUpstreamSource(source *UpstreamSource) error {
	if source == nil {
		return nil
	}
	source.UpdatedAt = common.GetTimestamp()
	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name",
			"console_url",
			"adapter_version",
			"endpoint_candidates",
			"selected_endpoint",
			"status",
			"enabled",
			"balance",
			"low_balance_threshold",
			"last_snapshot_id",
			"last_snapshot_at",
			"last_success_at",
			"last_error",
			"updated_at",
		}),
	}).Create(source).Error
}

func GetUpstreamSourceByKey(key string) (*UpstreamSource, error) {
	var source UpstreamSource
	if err := DB.Where("key = ?", strings.TrimSpace(key)).First(&source).Error; err != nil {
		return nil, err
	}
	return &source, nil
}

func ListUpstreamSources() ([]UpstreamSource, error) {
	var sources []UpstreamSource
	err := DB.Order("id asc").Find(&sources).Error
	return sources, err
}
