package model

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type BatchStatus string
type BatchItemStatus string

const (
	BatchStatusValidating BatchStatus = "validating"
	BatchStatusInProgress BatchStatus = "in_progress"
	BatchStatusFinalizing BatchStatus = "finalizing"
	BatchStatusCompleted  BatchStatus = "completed"
	BatchStatusFailed     BatchStatus = "failed"
	BatchStatusCancelling BatchStatus = "cancelling"
	BatchStatusCancelled  BatchStatus = "cancelled"
	BatchStatusExpired    BatchStatus = "expired"

	BatchItemStatusPending   BatchItemStatus = "pending"
	BatchItemStatusRunning   BatchItemStatus = "running"
	BatchItemStatusCompleted BatchItemStatus = "completed"
	BatchItemStatusFailed    BatchItemStatus = "failed"
	BatchItemStatusCancelled BatchItemStatus = "cancelled"
)

type BatchRequestCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

type Batch struct {
	ID               int64              `json:"-" gorm:"primaryKey"`
	BatchID          string             `json:"id" gorm:"type:varchar(64);uniqueIndex"`
	Object           string             `json:"object" gorm:"type:varchar(16);not null"`
	UserID           int                `json:"-" gorm:"index"`
	TokenID          int                `json:"-" gorm:"index"`
	ClientIP         string             `json:"-" gorm:"type:varchar(64)"`
	InputFileID      string             `json:"input_file_id" gorm:"type:varchar(64);index"`
	Endpoint         string             `json:"endpoint" gorm:"type:varchar(64);index"`
	CompletionWindow string             `json:"completion_window" gorm:"type:varchar(16)"`
	Status           BatchStatus        `json:"status" gorm:"type:varchar(32);index"`
	OutputFileID     *string            `json:"output_file_id"`
	ErrorFileID      *string            `json:"error_file_id"`
	Errors           json.RawMessage    `json:"errors,omitempty" gorm:"type:json"`
	Metadata         json.RawMessage    `json:"metadata,omitempty" gorm:"type:json"`
	RequestCounts    BatchRequestCounts `json:"request_counts" gorm:"-"`
	IdempotencyScope *string            `json:"-" gorm:"type:varchar(191);uniqueIndex"`
	RequestHash      string             `json:"-" gorm:"type:char(64)"`
	LockedBy         string             `json:"-" gorm:"type:varchar(128);index"`
	LockedUntil      int64              `json:"-" gorm:"bigint;index"`
	CreatedAt        int64              `json:"created_at" gorm:"bigint;index"`
	InProgressAt     int64              `json:"in_progress_at,omitempty" gorm:"bigint"`
	ExpiresAt        int64              `json:"expires_at,omitempty" gorm:"bigint;index"`
	FinalizingAt     int64              `json:"finalizing_at,omitempty" gorm:"bigint"`
	CompletedAt      int64              `json:"completed_at,omitempty" gorm:"bigint"`
	FailedAt         int64              `json:"failed_at,omitempty" gorm:"bigint"`
	CancellingAt     int64              `json:"cancelling_at,omitempty" gorm:"bigint"`
	CancelledAt      int64              `json:"cancelled_at,omitempty" gorm:"bigint"`
}

type BatchItem struct {
	ID            int64           `json:"-" gorm:"primaryKey"`
	BatchRecordID int64           `json:"-" gorm:"uniqueIndex:uk_batch_item_custom,priority:1;index"`
	LineNumber    int             `json:"-" gorm:"index"`
	CustomID      string          `json:"custom_id" gorm:"type:varchar(128);uniqueIndex:uk_batch_item_custom,priority:2"`
	Method        string          `json:"method" gorm:"type:varchar(8)"`
	URL           string          `json:"url" gorm:"type:varchar(128)"`
	Body          json.RawMessage `json:"body" gorm:"type:json"`
	Status        BatchItemStatus `json:"status" gorm:"type:varchar(32);index"`
	Response      json.RawMessage `json:"response,omitempty" gorm:"type:json"`
	Error         json.RawMessage `json:"error,omitempty" gorm:"type:json"`
	StartedAt     int64           `json:"-" gorm:"bigint"`
	CompletedAt   int64           `json:"-" gorm:"bigint"`
}

func (batch *Batch) BeforeCreate(_ *gorm.DB) error {
	if batch.BatchID == "" {
		value, err := GenerateBatchID()
		if err != nil {
			return err
		}
		batch.BatchID = value
	}
	if batch.Object == "" {
		batch.Object = "batch"
	}
	if batch.Status == "" {
		batch.Status = BatchStatusValidating
	}
	if batch.CompletionWindow == "" {
		batch.CompletionWindow = "24h"
	}
	if batch.CreatedAt == 0 {
		batch.CreatedAt = time.Now().Unix()
	}
	return nil
}

func (item *BatchItem) BeforeCreate(_ *gorm.DB) error {
	if item.Status == "" {
		item.Status = BatchItemStatusPending
	}
	return nil
}

func GenerateBatchID() (string, error) {
	value, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return "", err
	}
	return "batch_" + value, nil
}

// CreateBatchWithItems persists the batch and all validated lines atomically.
// A matching idempotency scope returns the existing object.
func CreateBatchWithItems(batch *Batch, items []BatchItem) (*Batch, bool, error) {
	if batch == nil || len(items) == 0 {
		return nil, false, errors.New("batch and at least one item are required")
	}
	created := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(batch)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			if batch.IdempotencyScope == nil {
				return errors.New("batch identifier conflict")
			}
			var existing Batch
			if err := tx.Where("idempotency_scope = ?", *batch.IdempotencyScope).First(&existing).Error; err != nil {
				return err
			}
			*batch = existing
			return nil
		}
		created = true
		for index := range items {
			items[index].BatchRecordID = batch.ID
		}
		return tx.CreateInBatches(&items, 500).Error
	})
	return batch, created, err
}

func GetOwnedBatch(userID, tokenID int, batchID string) (*Batch, bool, error) {
	if userID <= 0 || tokenID <= 0 || batchID == "" {
		return nil, false, nil
	}
	var batch Batch
	err := DB.Where("user_id = ? AND token_id = ? AND batch_id = ?", userID, tokenID, batchID).
		First(&batch).Error
	exists, err := RecordExist(err)
	if err != nil || !exists {
		return nil, exists, err
	}
	if err = loadBatchRequestCounts(&batch); err != nil {
		return nil, false, err
	}
	return &batch, true, nil
}

func GetBatchByID(id int64) (*Batch, error) {
	var batch Batch
	if err := DB.First(&batch, id).Error; err != nil {
		return nil, err
	}
	if err := loadBatchRequestCounts(&batch); err != nil {
		return nil, err
	}
	return &batch, nil
}

func ListOwnedBatches(userID, tokenID int, after string, limit int) ([]Batch, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	query := DB.Where("user_id = ? AND token_id = ?", userID, tokenID)
	if after != "" {
		var cursor Batch
		if err := DB.Select("id").Where(
			"user_id = ? AND token_id = ? AND batch_id = ?",
			userID,
			tokenID,
			after,
		).First(&cursor).Error; err != nil {
			return nil, false, err
		}
		query = query.Where("id < ?", cursor.ID)
	}
	var batches []Batch
	if err := query.Order("id desc").Limit(limit + 1).Find(&batches).Error; err != nil {
		return nil, false, err
	}
	hasMore := len(batches) > limit
	if hasMore {
		batches = batches[:limit]
	}
	for index := range batches {
		if err := loadBatchRequestCounts(&batches[index]); err != nil {
			return nil, false, err
		}
	}
	return batches, hasMore, nil
}

func FindDispatchableBatches(now int64, limit int) ([]*Batch, error) {
	if limit <= 0 {
		limit = 1
	}
	var batches []*Batch
	err := DB.Where("status IN ?", []BatchStatus{
		BatchStatusValidating,
		BatchStatusInProgress,
		BatchStatusFinalizing,
		BatchStatusCancelling,
	}).
		Where("locked_by = '' OR locked_until < ?", now).
		Order("id").
		Limit(limit).
		Find(&batches).Error
	return batches, err
}

func HasDispatchableBatches(now int64) bool {
	var id int64
	err := DB.Model(&Batch{}).
		Where("status IN ?", []BatchStatus{
			BatchStatusValidating,
			BatchStatusInProgress,
			BatchStatusFinalizing,
			BatchStatusCancelling,
		}).
		Where("locked_by = '' OR locked_until < ?", now).
		Limit(1).
		Pluck("id", &id).Error
	return err == nil && id != 0
}

func ClaimBatch(id int64, owner string, now, lockUntil int64) (*Batch, bool, error) {
	if id <= 0 || owner == "" || lockUntil <= now {
		return nil, false, nil
	}
	returned := &Batch{}
	err := DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&Batch{}).
			Where("id = ? AND status IN ?", id, []BatchStatus{
				BatchStatusValidating,
				BatchStatusInProgress,
				BatchStatusFinalizing,
				BatchStatusCancelling,
			}).
			Where("locked_by = '' OR locked_until < ?", now).
			Updates(map[string]any{
				"status":         gorm.Expr("CASE WHEN status = ? THEN ? ELSE status END", BatchStatusValidating, BatchStatusInProgress),
				"in_progress_at": gorm.Expr("CASE WHEN in_progress_at = 0 THEN ? ELSE in_progress_at END", now),
				"locked_by":      owner,
				"locked_until":   lockUntil,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		if err := tx.Model(&BatchItem{}).
			Where("batch_record_id = ? AND status = ?", id, BatchItemStatusRunning).
			Updates(map[string]any{"status": BatchItemStatusPending, "started_at": 0}).Error; err != nil {
			return err
		}
		return tx.First(returned, id).Error
	})
	if err != nil {
		return nil, false, err
	}
	if returned.ID == 0 {
		return nil, false, nil
	}
	return returned, true, nil
}

func RenewBatchLease(batchID int64, owner string, now, lockUntil int64) error {
	result := DB.Model(&Batch{}).
		Where("id = ? AND locked_by = ? AND locked_until >= ?", batchID, owner, now).
		Where("status IN ?", []BatchStatus{BatchStatusInProgress, BatchStatusCancelling}).
		Updates(map[string]any{"locked_until": lockUntil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("batch lease lost")
	}
	return nil
}

func ReleaseBatchLease(batchID int64, owner string) error {
	result := DB.Model(&Batch{}).
		Where("id = ? AND locked_by = ?", batchID, owner).
		Updates(map[string]any{"locked_by": "", "locked_until": 0})
	return result.Error
}

func ListPendingBatchItems(batchRecordID int64, limit int) ([]*BatchItem, error) {
	if limit <= 0 {
		limit = 100
	}
	var items []*BatchItem
	err := DB.Where("batch_record_id = ? AND status = ?", batchRecordID, BatchItemStatusPending).
		Order("line_number").
		Limit(limit).
		Find(&items).Error
	return items, err
}

func ClaimBatchItem(id int64, now int64) (bool, error) {
	result := DB.Model(&BatchItem{}).
		Where("id = ? AND status = ?", id, BatchItemStatusPending).
		Updates(map[string]any{"status": BatchItemStatusRunning, "started_at": now})
	return result.RowsAffected > 0, result.Error
}

func FinishBatchItem(id int64, status BatchItemStatus, response, itemError json.RawMessage, now int64) (bool, error) {
	if status != BatchItemStatusCompleted && status != BatchItemStatusFailed {
		return false, errors.New("invalid terminal batch item status")
	}
	result := DB.Model(&BatchItem{}).
		Where("id = ? AND status = ?", id, BatchItemStatusRunning).
		Updates(map[string]any{
			"status":       status,
			"response":     response,
			"error":        itemError,
			"completed_at": now,
		})
	return result.RowsAffected > 0, result.Error
}

func ListBatchItems(batchRecordID int64) ([]BatchItem, error) {
	var items []BatchItem
	err := DB.Where("batch_record_id = ?", batchRecordID).Order("line_number").Find(&items).Error
	return items, err
}

func CancelPendingBatchItems(batchRecordID int64, now int64) (int64, error) {
	result := DB.Model(&BatchItem{}).
		Where("batch_record_id = ? AND status = ?", batchRecordID, BatchItemStatusPending).
		Updates(map[string]any{
			"status":       BatchItemStatusCancelled,
			"completed_at": now,
		})
	return result.RowsAffected, result.Error
}

func RequestBatchCancellation(userID, tokenID int, batchID string, now int64) (*Batch, bool, error) {
	batch, exists, err := GetOwnedBatch(userID, tokenID, batchID)
	if err != nil || !exists {
		return batch, exists, err
	}
	if batch.Status == BatchStatusCompleted || batch.Status == BatchStatusFailed ||
		batch.Status == BatchStatusCancelled || batch.Status == BatchStatusExpired {
		return batch, true, nil
	}
	result := DB.Model(&Batch{}).
		Where("id = ? AND status IN ?", batch.ID, []BatchStatus{BatchStatusValidating, BatchStatusInProgress}).
		Updates(map[string]any{
			"status":        BatchStatusCancelling,
			"cancelling_at": now,
		})
	if result.Error != nil {
		return nil, true, result.Error
	}
	return GetOwnedBatch(userID, tokenID, batchID)
}

func FinishBatch(batch *Batch, owner string, status BatchStatus, outputFileID, errorFileID *string, batchError json.RawMessage, now int64) (bool, error) {
	if batch == nil {
		return false, nil
	}
	updates := map[string]any{
		"status":         status,
		"output_file_id": outputFileID,
		"error_file_id":  errorFileID,
		"errors":         batchError,
		"locked_by":      "",
		"locked_until":   0,
	}
	switch status {
	case BatchStatusCompleted:
		updates["completed_at"] = now
	case BatchStatusFailed:
		updates["failed_at"] = now
	case BatchStatusCancelled:
		updates["cancelled_at"] = now
	}
	result := DB.Model(&Batch{}).
		Where("id = ? AND locked_by = ?", batch.ID, owner).
		Updates(updates)
	return result.RowsAffected > 0, result.Error
}

func MarkBatchFinalizing(batch *Batch, owner string, now int64) (bool, error) {
	if batch == nil {
		return false, nil
	}
	result := DB.Model(&Batch{}).
		Where("id = ? AND locked_by = ? AND status = ?", batch.ID, owner, BatchStatusInProgress).
		Updates(map[string]any{"status": BatchStatusFinalizing, "finalizing_at": now})
	return result.RowsAffected > 0, result.Error
}

func loadBatchRequestCounts(batch *Batch) error {
	if batch == nil || batch.ID == 0 {
		return nil
	}
	type statusCount struct {
		Status BatchItemStatus
		Count  int
	}
	var counts []statusCount
	if err := DB.Model(&BatchItem{}).
		Select("status, count(*) as count").
		Where("batch_record_id = ?", batch.ID).
		Group("status").
		Scan(&counts).Error; err != nil {
		return err
	}
	batch.RequestCounts = BatchRequestCounts{}
	for _, count := range counts {
		batch.RequestCounts.Total += count.Count
		switch count.Status {
		case BatchItemStatusCompleted:
			batch.RequestCounts.Completed += count.Count
		case BatchItemStatusFailed:
			batch.RequestCounts.Failed += count.Count
		case BatchItemStatusCancelled:
			batch.RequestCounts.Failed += count.Count
		}
	}
	return nil
}

func activeBatchStatuses() []BatchStatus {
	return []BatchStatus{
		BatchStatusValidating,
		BatchStatusInProgress,
		BatchStatusFinalizing,
		BatchStatusCancelling,
	}
}
