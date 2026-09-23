package model

import (
	"errors"
	"maps"

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

	StreamRecoveryIdentityVersionLegacy = 1
	StreamRecoveryIdentityVersionStable = 2
)

var (
	ErrStreamExecutionLeaseLost               = errors.New("stream execution lease lost")
	ErrStreamExecutionLegacyIdentityAmbiguous = errors.New(
		"legacy stream recovery identity is ambiguous",
	)
)

type StreamExecution struct {
	ID                          int64                 `json:"id" gorm:"primaryKey"`
	StreamID                    string                `json:"stream_id" gorm:"type:varchar(64);uniqueIndex"`
	DedupeKey                   string                `json:"-" gorm:"type:varchar(64);uniqueIndex"`
	StableDedupeKey             *string               `json:"-" gorm:"type:varchar(64);uniqueIndex"`
	IdentityVersion             int                   `json:"-" gorm:"not null;default:1;index"`
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
	if execution.IdentityVersion == 0 {
		execution.IdentityVersion = StreamRecoveryIdentityVersionLegacy
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

func CreateOrGetStreamExecution(
	execution *StreamExecution,
	legacyDedupeKeys ...string,
) (*StreamExecution, bool, error) {
	if execution.StreamID == "" {
		streamID, err := GenerateStreamExecutionID()
		if err != nil {
			return nil, false, err
		}
		execution.StreamID = streamID
	}
	lookupDedupeKeys := []string{execution.DedupeKey}
	seenDedupeKeys := map[string]struct{}{execution.DedupeKey: {}}
	legacyStorageKey := ""
	for _, legacyDedupeKey := range legacyDedupeKeys {
		if legacyDedupeKey == "" || legacyDedupeKey == execution.DedupeKey {
			continue
		}
		if legacyStorageKey == "" {
			legacyStorageKey = legacyDedupeKey
		}
		if _, ok := seenDedupeKeys[legacyDedupeKey]; ok {
			continue
		}
		seenDedupeKeys[legacyDedupeKey] = struct{}{}
		lookupDedupeKeys = append(lookupDedupeKeys, legacyDedupeKey)
	}
	stableDedupeKey := ""
	if execution.IdentityVersion >= StreamRecoveryIdentityVersionStable &&
		legacyStorageKey != "" {
		stableDedupeKey = execution.DedupeKey
	}
	findExisting := func() (*StreamExecution, error) {
		var existing []StreamExecution
		query := DB.Where("dedupe_key IN ?", lookupDedupeKeys)
		if stableDedupeKey != "" {
			query = query.Or("stable_dedupe_key = ?", stableDedupeKey)
		}
		if err := query.Limit(2).Find(&existing).Error; err != nil {
			return nil, err
		}
		if len(existing) > 1 {
			return nil, ErrStreamExecutionLegacyIdentityAmbiguous
		}
		if len(existing) == 1 {
			return &existing[0], nil
		}
		return nil, nil
	}
	existing, err := findExisting()
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}
	if stableDedupeKey != "" {
		var count int64
		result := DB.Model(&StreamExecution{}).
			Where("identity_version < ?", StreamRecoveryIdentityVersionStable).
			Where(
				"token_id = ? AND request_path = ? AND model_name = ?",
				execution.TokenID,
				execution.RequestPath,
				execution.ModelName,
			).
			Where("expires_at > ?", common.GetTimestamp()).
			Count(&count)
		if result.Error != nil {
			return nil, false, result.Error
		}
		if count > 0 {
			return nil, false, ErrStreamExecutionLegacyIdentityAmbiguous
		}
		// Preserve the legacy unique key so an identical request from an old
		// binary cannot create a second execution during the mandatory drain.
		execution.DedupeKey = legacyStorageKey
		execution.StableDedupeKey = &stableDedupeKey
	}
	result := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(execution)
	if result.Error != nil {
		return nil, false, result.Error
	}
	if result.RowsAffected == 1 {
		return execution, true, nil
	}
	existing, err = findExisting()
	if err != nil {
		return nil, false, err
	}
	if existing == nil {
		return nil, false, gorm.ErrRecordNotFound
	}
	return existing, false, nil
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
		Where("attempt_count IN ?", []int{attempt - 1, attempt}).
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
		var count int64
		err := DB.Model(&StreamExecution{}).
			Where("stream_id = ? AND locked_by = ? AND locked_until >= ?", streamID, runnerID, now).
			Where("status IN ?", activeStreamExecutionStatuses()).
			Where("public_attempt = ? AND attempt_count = ?", attempt, attempt).
			Count(&count).Error
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrStreamExecutionLeaseLost
		}
	}
	return nil
}

func UpdateStreamExecutionSequence(
	streamID string,
	runnerID string,
	attempt int,
	sequence int64,
) error {
	if sequence <= 0 {
		return errors.New("stream execution sequence must be positive")
	}
	now := common.GetTimestamp()
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND locked_by = ? AND locked_until >= ?", streamID, runnerID, now).
		Where("status IN ?", activeStreamExecutionStatuses()).
		Where("public_attempt = ? AND attempt_count = ?", attempt, attempt).
		Where("committed_sequence = ?", sequence-1).
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
	attempt int,
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
		Where("public_attempt = ? AND attempt_count = ?", attempt, attempt).
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

func FailExpiredStreamExecution(
	streamID string,
	observedAttempt int,
	now int64,
	errorMessage string,
) (bool, error) {
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND attempt_count = ?", streamID, observedAttempt).
		Where("status IN ?", activeStreamExecutionStatuses()).
		Where("locked_until < ?", now).
		Updates(map[string]any{
			"status": StreamExecutionFailed,
			"billing_status": gorm.Expr(
				"CASE WHEN billing_status IN ? THEN ? ELSE billing_status END",
				[]string{
					StreamBillingReserving,
					StreamBillingReserved,
					StreamBillingSettling,
					StreamBillingRefunding,
				},
				StreamBillingUncertain,
			),
			"error":        errorMessage,
			"locked_by":    "",
			"locked_until": 0,
			"updated_at":   now,
		})
	return result.RowsAffected > 0, result.Error
}

func ClaimExpiredStreamExecutionForReconciliation(
	streamID string,
	observedAttempt int,
	runnerID string,
	now int64,
	lockUntil int64,
) (*StreamExecution, bool, error) {
	result := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND attempt_count = ?", streamID, observedAttempt).
		Where("status IN ?", activeStreamExecutionStatuses()).
		Where("locked_until < ?", now).
		Updates(map[string]any{
			"status": StreamExecutionRecovering,
			"billing_status": gorm.Expr(
				"CASE WHEN billing_status IN ? THEN ? ELSE billing_status END",
				[]string{
					StreamBillingReserving,
					StreamBillingReserved,
					StreamBillingSettling,
					StreamBillingRefunding,
				},
				StreamBillingUncertain,
			),
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

func OwnsStreamExecutionReconciliationLease(
	streamID string,
	runnerID string,
	attempt int,
	expectedSequence int64,
) (bool, error) {
	var count int64
	err := DB.Model(&StreamExecution{}).
		Where(
			"stream_id = ? AND status = ? AND locked_by = ? AND locked_until >= ?",
			streamID,
			StreamExecutionRecovering,
			runnerID,
			common.GetTimestamp(),
		).
		Where("public_attempt = ? AND attempt_count = ?", attempt, attempt).
		Where("committed_sequence = ?", expectedSequence).
		Count(&count).Error
	return count == 1, err
}

func FinishStreamExecutionReconciliation(
	streamID string,
	runnerID string,
	attempt int,
	status StreamExecutionStatus,
	expectedSequence int64,
	terminalSequence int64,
	errorMessage string,
) (bool, error) {
	if status != StreamExecutionCompleted && status != StreamExecutionFailed {
		return false, errors.New("invalid reconciled stream execution status")
	}
	if terminalSequence < expectedSequence {
		return false, errors.New("terminal sequence cannot move backwards")
	}
	now := common.GetTimestamp()
	result := DB.Model(&StreamExecution{}).
		Where(
			"stream_id = ? AND status = ? AND locked_by = ? AND locked_until >= ?",
			streamID,
			StreamExecutionRecovering,
			runnerID,
			now,
		).
		Where("public_attempt = ? AND attempt_count = ?", attempt, attempt).
		Where("committed_sequence = ?", expectedSequence).
		Updates(map[string]any{
			"status":             status,
			"committed_sequence": terminalSequence,
			"terminal_sequence":  terminalSequence,
			"billing_status": gorm.Expr(
				"CASE WHEN billing_status IN ? THEN ? ELSE billing_status END",
				[]string{
					StreamBillingReserving,
					StreamBillingReserved,
					StreamBillingSettling,
					StreamBillingRefunding,
				},
				StreamBillingUncertain,
			),
			"error":        errorMessage,
			"locked_by":    "",
			"locked_until": 0,
			"updated_at":   now,
		})
	return result.RowsAffected > 0, result.Error
}

func CommitFailedStreamExecutionTerminal(
	streamID string,
	attempt int,
	expectedSequence int64,
	terminalSequence int64,
) (bool, error) {
	if terminalSequence <= expectedSequence {
		return false, errors.New("terminal sequence must advance")
	}
	result := DB.Model(&StreamExecution{}).
		Where(
			"stream_id = ? AND status = ? AND public_attempt = ? AND attempt_count = ?",
			streamID,
			StreamExecutionFailed,
			attempt,
			attempt,
		).
		Where("committed_sequence = ?", expectedSequence).
		Updates(map[string]any{
			"committed_sequence": terminalSequence,
			"terminal_sequence":  terminalSequence,
			"updated_at":         common.GetTimestamp(),
		})
	return result.RowsAffected > 0, result.Error
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
	err := DB.Where(
		"status IN ? AND attempt_count > 0 AND locked_until < ? AND expires_at > ?",
		activeStreamExecutionStatuses(),
		now,
		now,
	).
		Order("updated_at asc").
		Limit(limit).
		Find(&executions).Error
	return executions, err
}

func DeleteExpiredStreamExecutions(now int64) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		unresolved := []string{
			StreamBillingReserving,
			StreamBillingReserved,
			StreamBillingSettling,
			StreamBillingRefunding,
		}
		if err := tx.Model(&StreamExecution{}).
			Where("expires_at > 0 AND expires_at < ?", now).
			Where("billing_status IN ?", unresolved).
			Updates(map[string]any{
				"billing_status": StreamBillingUncertain,
				"updated_at":     common.GetTimestamp(),
			}).Error; err != nil {
			return err
		}
		if err := tx.Model(&StreamExecution{}).
			Where("expires_at > 0 AND expires_at < ?", now).
			Where("status IN ?", activeStreamExecutionStatuses()).
			Updates(map[string]any{
				"status":       StreamExecutionFailed,
				"error":        "STATEFUL_REPLAY_UNSAFE: execution expired before terminal reconciliation",
				"locked_by":    "",
				"locked_until": 0,
				"updated_at":   common.GetTimestamp(),
			}).Error; err != nil {
			return err
		}
		return tx.Where("expires_at > 0 AND expires_at < ?", now).
			Where("status IN ?", []StreamExecutionStatus{
				StreamExecutionCompleted,
				StreamExecutionFailed,
				StreamExecutionCancelled,
			}).
			Where("error NOT LIKE ?", "STATEFUL_REPLAY_UNSAFE:%").
			Where("billing_status IN ?", []string{
				StreamBillingNone,
				StreamBillingSettled,
				StreamBillingRefunded,
			}).
			Delete(&StreamExecution{}).Error
	})
}

func UpdateStreamBillingState(
	streamID string,
	fromStatuses []string,
	updates map[string]any,
) (bool, error) {
	if len(fromStatuses) == 0 {
		return false, errors.New("billing source statuses are required")
	}
	now := common.GetTimestamp()
	updates["updated_at"] = now
	query := DB.Model(&StreamExecution{}).
		Where("stream_id = ? AND billing_status IN ?", streamID, fromStatuses)
	if targetStatus, ok := updates["billing_status"].(string); ok {
		if targetStatus != StreamBillingUncertain {
			query = query.Where("billing_status <> ?", StreamBillingUncertain)
		}
		switch targetStatus {
		case StreamBillingReserving,
			StreamBillingReserved,
			StreamBillingSettling,
			StreamBillingRefunding:
			query = query.Where(
				"(attempt_count = 0 OR (status = ? AND locked_until >= ?))",
				StreamExecutionRunning,
				now,
			)
		}
	}
	result := query.Updates(updates)
	if result.Error == nil &&
		result.RowsAffected == 0 &&
		updates["billing_status"] == StreamBillingReserved {
		fallbackUpdates := make(map[string]any, len(updates))
		maps.Copy(fallbackUpdates, updates)
		fallbackUpdates["billing_status"] = StreamBillingUncertain
		fallbackStatuses := append(
			append([]string(nil), fromStatuses...),
			StreamBillingUncertain,
		)
		fallback := DB.Model(&StreamExecution{}).
			Where(
				"stream_id = ? AND billing_status IN ?",
				streamID,
				fallbackStatuses,
			).
			Where("attempt_count > 0").
			Where(
				"(status <> ? OR locked_until < ?)",
				StreamExecutionRunning,
				now,
			).
			Updates(fallbackUpdates)
		if fallback.Error != nil {
			return false, fallback.Error
		}
	}
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
