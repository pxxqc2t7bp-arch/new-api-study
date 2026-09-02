package model

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

const (
	UpstreamPriceStatusApplied   = "applied"
	UpstreamPriceStatusRejected  = "rejected"
	UpstreamPriceStatusUnchanged = "unchanged"
)

type UpstreamPriceEvidence struct {
	ID              int64  `json:"id" gorm:"primaryKey"`
	Vendor          string `json:"vendor" gorm:"type:varchar(32);not null;index"`
	ModelName       string `json:"model_name" gorm:"type:varchar(128);not null;index"`
	Currency        string `json:"currency" gorm:"type:varchar(8);not null"`
	Unit            string `json:"unit" gorm:"type:varchar(32);not null"`
	NormalizedPrice string `json:"normalized_price" gorm:"type:text;not null"`
	PreviousPrice   string `json:"previous_price,omitempty" gorm:"type:text"`
	SourceURL       string `json:"source_url" gorm:"type:varchar(1024);not null"`
	EvidenceHash    string `json:"evidence_hash" gorm:"type:char(64);not null;index"`
	Status          string `json:"status" gorm:"type:varchar(32);not null;index"`
	Error           string `json:"error,omitempty" gorm:"type:text"`
	CapturedAt      int64  `json:"captured_at" gorm:"bigint;not null;index"`
	AppliedAt       int64  `json:"applied_at,omitempty" gorm:"bigint"`
	CreatedAt       int64  `json:"created_at" gorm:"bigint;index"`
}

func (UpstreamPriceEvidence) TableName() string {
	return "upstream_price_evidence"
}

func (evidence *UpstreamPriceEvidence) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	if evidence.CapturedAt == 0 {
		evidence.CapturedAt = now
	}
	if evidence.CreatedAt == 0 {
		evidence.CreatedAt = now
	}
	return nil
}
