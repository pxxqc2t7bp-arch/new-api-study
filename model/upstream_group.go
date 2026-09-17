package model

import (
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	UpstreamHealthUnknown     = "unknown"
	UpstreamHealthOperational = "operational"
	UpstreamHealthDegraded    = "degraded"
	UpstreamHealthFailed      = "failed"
	UpstreamHealthError       = "error"
)

type UpstreamGroup struct {
	ID                  int64    `json:"id" gorm:"primaryKey"`
	SourceID            int64    `json:"source_id" gorm:"not null;uniqueIndex:idx_upstream_group_source_external,priority:1;index"`
	ExternalID          string   `json:"external_id" gorm:"type:varchar(128);not null;uniqueIndex:idx_upstream_group_source_external,priority:2"`
	Name                string   `json:"name" gorm:"type:varchar(255);not null"`
	Platform            string   `json:"platform" gorm:"type:varchar(32);not null;index"`
	SubscriptionType    string   `json:"subscription_type" gorm:"type:varchar(32)"`
	BaseMultiplier      float64  `json:"base_multiplier" gorm:"not null"`
	UserMultiplier      *float64 `json:"user_multiplier,omitempty"`
	EffectiveMultiplier float64  `json:"effective_multiplier" gorm:"not null;index"`
	PeakRateEnabled     bool     `json:"peak_rate_enabled" gorm:"not null"`
	PeakStart           string   `json:"peak_start,omitempty" gorm:"type:varchar(8)"`
	PeakEnd             string   `json:"peak_end,omitempty" gorm:"type:varchar(8)"`
	PeakMultiplier      *float64 `json:"peak_multiplier,omitempty"`
	IsExclusive         bool     `json:"is_exclusive" gorm:"not null"`
	HealthStatus        string   `json:"health_status" gorm:"type:varchar(32);not null;index"`
	Availability        *float64 `json:"availability,omitempty"`
	LatencyMS           *int64   `json:"latency_ms,omitempty"`
	EndpointPingMS      *int64   `json:"endpoint_ping_ms,omitempty"`
	Models              string   `json:"models" gorm:"type:text"`
	MonitorExternalID   string   `json:"monitor_external_id,omitempty" gorm:"type:varchar(128)"`
	RedSince            int64    `json:"red_since,omitempty" gorm:"bigint;index"`
	LastGreenAt         int64    `json:"last_green_at,omitempty" gorm:"bigint"`
	ObservedAt          int64    `json:"observed_at" gorm:"bigint;index"`
	CreatedAt           int64    `json:"created_at" gorm:"bigint;index"`
	UpdatedAt           int64    `json:"updated_at" gorm:"bigint;index"`
}

func (UpstreamGroup) TableName() string {
	return "upstream_groups"
}

func (group *UpstreamGroup) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	group.ExternalID = strings.TrimSpace(group.ExternalID)
	group.Platform = strings.ToLower(strings.TrimSpace(group.Platform))
	group.HealthStatus = NormalizeUpstreamHealth(group.HealthStatus)
	if group.CreatedAt == 0 {
		group.CreatedAt = now
	}
	if group.UpdatedAt == 0 {
		group.UpdatedAt = now
	}
	return nil
}

func NormalizeUpstreamHealth(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case UpstreamHealthOperational:
		return UpstreamHealthOperational
	case UpstreamHealthDegraded:
		return UpstreamHealthDegraded
	case UpstreamHealthFailed:
		return UpstreamHealthFailed
	case UpstreamHealthError:
		return UpstreamHealthError
	default:
		return UpstreamHealthUnknown
	}
}

func UpsertUpstreamGroup(group *UpstreamGroup) error {
	if group == nil {
		return nil
	}
	group.HealthStatus = NormalizeUpstreamHealth(group.HealthStatus)
	group.UpdatedAt = common.GetTimestamp()
	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "source_id"}, {Name: "external_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name",
			"platform",
			"subscription_type",
			"base_multiplier",
			"user_multiplier",
			"effective_multiplier",
			"peak_rate_enabled",
			"peak_start",
			"peak_end",
			"peak_multiplier",
			"is_exclusive",
			"health_status",
			"availability",
			"latency_ms",
			"endpoint_ping_ms",
			"models",
			"monitor_external_id",
			"red_since",
			"last_green_at",
			"observed_at",
			"updated_at",
		}),
	}).Create(group).Error
}

func ListUpstreamGroups() ([]UpstreamGroup, error) {
	var groups []UpstreamGroup
	err := DB.Order("effective_multiplier asc, source_id asc, id asc").Find(&groups).Error
	return groups, err
}
