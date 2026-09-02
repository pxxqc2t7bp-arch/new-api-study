package model

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

const (
	UpstreamMetricQualityReported    = "reported"
	UpstreamMetricQualityEstimated   = "estimated"
	UpstreamMetricQualityUnavailable = "unavailable"
)

// UpstreamMetricSnapshot stores bounded operational history. Nullable values
// distinguish unavailable metrics from a reported zero.
type UpstreamMetricSnapshot struct {
	ID              int64    `json:"id" gorm:"primaryKey"`
	SnapshotID      string   `json:"snapshot_id" gorm:"type:varchar(64);not null;index:idx_upstream_metric_snapshot"`
	SourceID        int64    `json:"source_id" gorm:"not null;index:idx_upstream_metric_snapshot;index"`
	ExternalGroupID string   `json:"external_group_id,omitempty" gorm:"type:varchar(128);index"`
	ChannelID       *int     `json:"channel_id,omitempty" gorm:"index"`
	ModelName       string   `json:"model_name,omitempty" gorm:"type:varchar(128);index"`
	Protocol        string   `json:"protocol,omitempty" gorm:"type:varchar(32);index"`
	HealthStatus    string   `json:"health_status,omitempty" gorm:"type:varchar(32)"`
	RateMultiplier  *float64 `json:"rate_multiplier,omitempty"`
	Balance         *float64 `json:"balance,omitempty"`
	InputTokens     *int64   `json:"input_tokens,omitempty" gorm:"bigint"`
	OutputTokens    *int64   `json:"output_tokens,omitempty" gorm:"bigint"`
	CachedTokens    *int64   `json:"cached_tokens,omitempty" gorm:"bigint"`
	TotalTokens     *int64   `json:"total_tokens,omitempty" gorm:"bigint"`
	Usage5H         *float64 `json:"usage_5h,omitempty"`
	Limit5H         *float64 `json:"limit_5h,omitempty"`
	Reset5HAt       *int64   `json:"reset_5h_at,omitempty" gorm:"bigint"`
	Usage1D         *float64 `json:"usage_1d,omitempty"`
	Limit1D         *float64 `json:"limit_1d,omitempty"`
	Reset1DAt       *int64   `json:"reset_1d_at,omitempty" gorm:"bigint"`
	Usage7D         *float64 `json:"usage_7d,omitempty"`
	Limit7D         *float64 `json:"limit_7d,omitempty"`
	Reset7DAt       *int64   `json:"reset_7d_at,omitempty" gorm:"bigint"`
	Usage30D        *float64 `json:"usage_30d,omitempty"`
	Limit30D        *float64 `json:"limit_30d,omitempty"`
	Reset30DAt      *int64   `json:"reset_30d_at,omitempty" gorm:"bigint"`
	Availability    *float64 `json:"availability,omitempty"`
	LatencyMS       *int64   `json:"latency_ms,omitempty" gorm:"bigint"`
	EndpointPingMS  *int64   `json:"endpoint_ping_ms,omitempty" gorm:"bigint"`
	DataQuality     string   `json:"data_quality" gorm:"type:varchar(16);not null"`
	IsDailyRollup   bool     `json:"is_daily_rollup" gorm:"not null;index"`
	ObservedAt      int64    `json:"observed_at" gorm:"bigint;not null;index"`
	CreatedAt       int64    `json:"created_at" gorm:"bigint;index"`
}

func (UpstreamMetricSnapshot) TableName() string {
	return "upstream_metric_snapshots"
}

func (snapshot *UpstreamMetricSnapshot) BeforeCreate(_ *gorm.DB) error {
	if snapshot.DataQuality == "" {
		snapshot.DataQuality = UpstreamMetricQualityUnavailable
	}
	if snapshot.ObservedAt == 0 {
		snapshot.ObservedAt = common.GetTimestamp()
	}
	if snapshot.CreatedAt == 0 {
		snapshot.CreatedAt = common.GetTimestamp()
	}
	return nil
}

func DeleteUpstreamMetricSnapshotsBefore(cutoff int64) error {
	if cutoff <= 0 {
		return nil
	}
	return DB.Where("is_daily_rollup = ? AND observed_at < ?", false, cutoff).
		Delete(&UpstreamMetricSnapshot{}).Error
}
