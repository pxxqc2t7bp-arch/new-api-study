package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type StreamExecutionStatus string

const (
	StreamExecutionPending    StreamExecutionStatus = "pending"
	StreamExecutionRunning    StreamExecutionStatus = "running"
	StreamExecutionRecovering StreamExecutionStatus = "recovering"
	StreamExecutionCompleted  StreamExecutionStatus = "completed"
	StreamExecutionFailed     StreamExecutionStatus = "failed"
	StreamExecutionCancelled  StreamExecutionStatus = "cancelled"

	StreamBillingNone      = "none"
	StreamBillingReserving = "reserving"
	StreamBillingReserved  = "reserved"
	StreamBillingSettling  = "settling"
	StreamBillingSettled   = "settled"
	StreamBillingRefunding = "refunding"
	StreamBillingRefunded  = "refunded"
	StreamBillingUncertain = "uncertain"

	StreamConsumeLogNone      = "none"
	StreamConsumeLogRecording = "recording"
	StreamConsumeLogRecorded  = "recorded"
)

var ErrStreamExecutionLeaseLost = errors.New("stream execution lease lost")

type StreamExecution struct {
	ID                          int64                 `json:"id" gorm:"primaryKey"`
	StreamID                    string                `json:"stream_id" gorm:"type:varchar(64);uniqueIndex"`
	DedupeKey                   string                `json:"-" gorm:"type:varchar(64);uniqueIndex"`
	UserID                      int                   `json:"user_id" gorm:"index"`
	TokenID                     int                   `json:"token_id" gorm:"index"`
	ModelName                   string                `json:"model_name" gorm:"type:varchar(191);index"`
	RelayFormat                 string                `json:"relay_format" gorm:"type:varchar(32)"`
	RequestPath                 string                `json:"request_path" gorm:"type:varchar(255)"`
	RequestDigest               string                `json:"request_digest" gorm:"type:varchar(64)"`
	Status                      StreamExecutionStatus `json:"status" gorm:"type:varchar(32);index"`
	PublicAttempt               int                   `json:"public_attempt"`
	AttemptCount                int                   `json:"attempt_count"`
	ActiveChannelID             int                   `json:"active_channel_id"`
	CommittedSequence           int64                 `json:"committed_sequence"`
	TerminalSequence            int64                 `json:"terminal_sequence"`
	LockedBy                    string                `json:"locked_by" gorm:"type:varchar(128);index"`
	LockedUntil                 int64                 `json:"locked_until" gorm:"bigint;index"`
	BillingStatus               string                `json:"billing_status" gorm:"type:varchar(32);index"`
	BillingSource               string                `json:"billing_source" gorm:"type:varchar(32)"`
	ReservedQuota               int                   `json:"reserved_quota"`
	ActualQuota                 int                   `json:"actual_quota"`
	TokenConsumed               int                   `json:"token_consumed"`
	ExtraReserved               int                   `json:"extra_reserved"`
	Trusted                     bool                  `json:"trusted"`
	SubscriptionID              int                   `json:"subscription_id"`
	SubscriptionPreConsumed     int64                 `json:"subscription_pre_consumed" gorm:"bigint"`
	SubscriptionAmountTotal     int64                 `json:"subscription_amount_total" gorm:"bigint"`
	SubscriptionAmountUsedAfter int64                 `json:"subscription_amount_used_after" gorm:"bigint"`
	SubscriptionPlanID          int                   `json:"subscription_plan_id"`
	SubscriptionPlanTitle       string                `json:"subscription_plan_title" gorm:"type:varchar(255)"`
	ConsumeLogStatus            string                `json:"consume_log_status" gorm:"type:varchar(32);index"`
	RecoveryReason              string                `json:"recovery_reason" gorm:"type:text"`
	Error                       string                `json:"error" gorm:"type:text"`
	CreatedAt                   int64                 `json:"created_at" gorm:"bigint;index"`
	UpdatedAt                   int64                 `json:"updated_at" gorm:"bigint;index"`
	ExpiresAt                   int64                 `json:"expires_at" gorm:"bigint;index"`
}

func (execution *StreamExecution) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	if execution.CreatedAt == 0 {
		execution.CreatedAt = now
	}
	if execution.UpdatedAt == 0 {
		execution.UpdatedAt = now
	}
	if execution.Status == "" {
		execution.Status = StreamExecutionPending
	}
	if execution.BillingStatus == "" {
		execution.BillingStatus = StreamBillingNone
	}
	if execution.ConsumeLogStatus == "" {
		execution.ConsumeLogStatus = StreamConsumeLogNone
	}
	return nil
}

func GenerateStreamExecutionID() (string, error) {
	key, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return "", err
	}
	return "stream_" + key, nil
}

func CreateOrGetStreamExecution(execution *StreamExecution) (*StreamExecution, bool, error) {
	if execution.StreamID == "" {
		streamID, err := GenerateStreamExecutionID()
		if err != nil {
			return nil, false, err
		}
		execution.StreamID = streamID
	}
	result := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(execution)
	if result.Error != nil {
		return nil, false, result.Error
	}
	if result.RowsAffected == 1 {
		return execution, true, nil
	}
	var existing StreamExecution
	if err := DB.Where("dedupe_key = ?", execution.DedupeKey).First(&existing).Error; err != nil {
		return nil, false, err
	}
	return &existing, false, nil
}

func GetStreamExecution(streamID string) (*StreamExecution, error) {
	var execution StreamExecution
	if err := DB.Where("stream_id = ?", streamID).First(&execution).Error; err != nil {
		return nil, err
	}
	return &execution, nil
}

func GetOwnedStreamExecution(streamID string, userID int, tokenID int) (*StreamExecution, error) {
	var execution StreamExecution
	if err := DB.Where(
		"stream_id = ? AND user_id = ? AND token_id = ?",
		streamID,
		userID,
		tokenID,
	).First(&execution).Error; err != nil {
		return nil, err
	}
	return &execution, nil
}

func ClaimStreamExecution(streamID string, runnerID string, now int64, lockUntil int64) (*StreamExecution, bool, error) {
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND status IN ?", streamID, activeStreamExecutionStatuses()).
		Where("(locked_by = ? OR locked_by = '' OR locked_until < ?)", runnerID, now).
		Updates(map[string]any{
			"status":       StreamExecutionRunning,
			"locked_by":    runnerID,
			"locked_until": lockUntil,
			"updated_at":   now,
		})
	if result.Error != nil {
		return nil, false, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, false, nil
	}
	execution, err := GetStreamExecution(streamID)
	if err != nil {
		return nil, false, err
	}
	return execution, true, nil
}

func RenewStreamExecutionLease(streamID string, runnerID string, now int64, lockUntil int64) error {
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND locked_by = ? AND locked_until >= ?", streamID, runnerID, now).
		Where("status IN ?", activeStreamExecutionStatuses()).
		Updates(map[string]any{
			"locked_until": lockUntil,
			"updated_at":   now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrStreamExecutionLeaseLost
	}
	return nil
}

func StartStreamExecutionAttempt(
	streamID string,
	runnerID string,
	attempt int,
	channelID int,
	recoveryReason string,
) error {
	now := common.GetTimestamp()
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND locked_by = ? AND locked_until >= ?", streamID, runnerID, now).
		Where("status IN ?", activeStreamExecutionStatuses()).
		Updates(map[string]any{
			"status":             StreamExecutionRunning,
			"public_attempt":     attempt,
			"attempt_count":      attempt,
			"active_channel_id":  channelID,
			"committed_sequence": 0,
			"terminal_sequence":  0,
			"recovery_reason":    recoveryReason,
			"error":              "",
			"updated_at":         now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrStreamExecutionLeaseLost
	}
	return nil
}

func UpdateStreamExecutionSequence(streamID string, runnerID string, sequence int64) error {
	now := common.GetTimestamp()
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND locked_by = ? AND locked_until >= ?", streamID, runnerID, now).
		Where("status IN ?", activeStreamExecutionStatuses()).
		Updates(map[string]any{
			"committed_sequence": sequence,
			"updated_at":         now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrStreamExecutionLeaseLost
	}
	return nil
}

func FinishStreamExecution(
	streamID string,
	runnerID string,
	status StreamExecutionStatus,
	terminalSequence int64,
	errorMessage string,
) error {
	if status != StreamExecutionCompleted &&
		status != StreamExecutionFailed &&
		status != StreamExecutionCancelled {
		return errors.New("invalid terminal stream execution status")
	}
	now := common.GetTimestamp()
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND locked_by = ? AND locked_until >= ?", streamID, runnerID, now).
		Where("status IN ?", activeStreamExecutionStatuses()).
		Updates(map[string]any{
			"status":            status,
			"terminal_sequence": terminalSequence,
			"error":             errorMessage,
			"locked_by":         "",
			"locked_until":      0,
			"updated_at":        now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrStreamExecutionLeaseLost
	}
	return nil
}

func CancelOwnedStreamExecution(streamID string, userID int, tokenID int) (bool, error) {
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND user_id = ? AND token_id = ?", streamID, userID, tokenID).
		Where("status IN ?", activeStreamExecutionStatuses()).
		Updates(map[string]any{
			"status":       StreamExecutionCancelled,
			"locked_by":    "",
			"locked_until": 0,
			"updated_at":   common.GetTimestamp(),
		})
	return result.RowsAffected > 0, result.Error
}

func FindRecoverableStreamExecutions(now int64, limit int) ([]*StreamExecution, error) {
	if limit <= 0 {
		limit = 20
	}
	var executions []*StreamExecution
	err := DB.Where("status IN ? AND locked_until < ? AND expires_at > ?", activeStreamExecutionStatuses(), now, now).
		Order("updated_at asc").
		Limit(limit).
		Find(&executions).Error
	return executions, err
}

func DeleteExpiredStreamExecutions(now int64) error {
	return DB.Where("expires_at > 0 AND expires_at < ?", now).Delete(&StreamExecution{}).Error
}

func UpdateStreamBillingState(
	streamID string,
	fromStatuses []string,
	updates map[string]any,
) (bool, error) {
	if len(fromStatuses) == 0 {
		return false, errors.New("billing source statuses are required")
	}
	updates["updated_at"] = common.GetTimestamp()
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND billing_status IN ?", streamID, fromStatuses).
		Updates(updates)
	return result.RowsAffected > 0, result.Error
}

func ClaimStreamConsumeLog(streamID string) (bool, error) {
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND consume_log_status = ?", streamID, StreamConsumeLogNone).
		Updates(map[string]any{
			"consume_log_status": StreamConsumeLogRecording,
			"updated_at":         common.GetTimestamp(),
		})
	return result.RowsAffected > 0, result.Error
}

func FinishStreamConsumeLog(streamID string, recorded bool) error {
	next := StreamConsumeLogNone
	if recorded {
		next = StreamConsumeLogRecorded
	}
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND consume_log_status = ?", streamID, StreamConsumeLogRecording).
		Updates(map[string]any{
			"consume_log_status": next,
			"updated_at":         common.GetTimestamp(),
		})
	return result.Error
}

func activeStreamExecutionStatuses() []string {
	return []string{
		string(StreamExecutionPending),
		string(StreamExecutionRunning),
		string(StreamExecutionRecovering),
	}
}
