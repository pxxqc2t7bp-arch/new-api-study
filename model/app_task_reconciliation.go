package model

import (
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// Worker lease recovery may authorize another observation, never a second
// provider create. Reconciliation and settlement are separate from submission.
type AppTaskReconcile struct {
	ExecutionID          int64  `gorm:"primaryKey;autoIncrement:false"`
	NextAttemptAt        int64  `gorm:"not null;index"`
	LeaseOwner           string `gorm:"size:64;not null"`
	LeaseEpoch           int64  `gorm:"not null"`
	LeaseExpiresAt       int64  `gorm:"not null"`
	LastObservationError string `gorm:"size:128;not null"`
}

type AppTaskSettlement struct {
	ExecutionID          int64  `gorm:"primaryKey;autoIncrement:false"`
	FundingSource        string `gorm:"size:32;not null"`
	FundingRef           string `gorm:"size:128;not null"`
	ReservedQuota        int64  `gorm:"not null"`
	FinalQuota           int64  `gorm:"not null"`
	TerminalEvidenceHash string `gorm:"size:64;not null"`
	CreatedAt            int64  `gorm:"not null"`
}

type AppTaskOutbox struct {
	EventID     string `gorm:"primaryKey;size:64"`
	ExecutionID int64  `gorm:"uniqueIndex;not null"`
	FactsJSON   string `gorm:"type:text;not null"`
	CreatedAt   int64  `gorm:"not null"`
	DeliveredAt int64  `gorm:"not null"`
}

// ScopeHash binds the service credential/version and caller idempotency key.
// Only the first secret-free factual response is retained.
type AppTaskCancelReplay struct {
	ScopeHash      string `gorm:"primaryKey;size:64" json:"-"`
	RequestHash    string `gorm:"size:64;not null" json:"-"`
	InstallationID string `gorm:"size:64;not null;index" json:"-"`
	ResponseJSON   string `gorm:"type:text;not null" json:"-"`
}

var ErrAppTaskLeaseLost = errors.New("app task lease lost")

// AppTaskTerminal contains validated provider facts and a host-computed price.
// It is internal to the Host worker, never a client-supplied settlement API.
type AppTaskTerminal struct {
	ProviderState string `json:"provider_state"`
	FinalQuota    int64  `json:"final_quota"`
	UsageJSON     string `json:"usage_json"`
	ArtifactsJSON string `json:"artifacts_json"`
	ResultJSON    string `json:"result_json"`
	// Signed delivery URLs may rotate without changing immutable billing facts.
	ArtifactURLs map[string]string `json:"-"`
}

func ClaimAppTaskReconcile(ctx context.Context, db *gorm.DB, owner string, now time.Time, duration time.Duration) (AppTaskExecution, AppTaskReconcile, bool, error) {
	if owner == "" || len(owner) > 64 || strings.TrimSpace(owner) != owner || now.Unix() <= 0 ||
		duration < time.Second || duration > 5*time.Minute {
		return AppTaskExecution{}, AppTaskReconcile{}, false, errors.New("invalid_reconcile_lease")
	}
	var execution AppTaskExecution
	var lease AppTaskReconcile
	won := false
	err := RunAppPluginTransaction(db.WithContext(ctx), func(tx *gorm.DB) error {
		execution, lease, won = AppTaskExecution{}, AppTaskReconcile{}, false
		query := lockForUpdate(tx).Where("next_attempt_at > 0 AND next_attempt_at <= ? AND lease_expires_at <= ?",
			now.Unix(), now.Unix()).Order("next_attempt_at, execution_id").Limit(1).Find(&lease)
		if query.Error != nil || query.RowsAffected == 0 {
			return query.Error
		}
		if lease.LeaseEpoch == math.MaxInt64 {
			return errors.New("reconcile_epoch_exhausted")
		}
		// No execution lock here: finalizers lock funding -> execution -> lease.
		// The lease CAS fences a later write regardless of this read's snapshot.
		if err := tx.Where("id = ?", lease.ExecutionID).First(&execution).Error; err != nil {
			return err
		}
		if execution.ExecutionKind != AppExecutionKindTask {
			return tx.Model(&AppTaskReconcile{}).Where("execution_id = ?", lease.ExecutionID).
				Update("next_attempt_at", 0).Error
		}
		if execution.BillingState == "settled" {
			return tx.Model(&AppTaskReconcile{}).Where("execution_id = ?", lease.ExecutionID).
				Update("next_attempt_at", 0).Error
		}
		lease.LeaseOwner, lease.LeaseEpoch, lease.LeaseExpiresAt = owner, lease.LeaseEpoch+1, now.Add(duration).Unix()
		if err := tx.Model(&AppTaskReconcile{}).Where("execution_id = ?", lease.ExecutionID).
			Updates(map[string]any{"lease_owner": owner, "lease_epoch": lease.LeaseEpoch, "lease_expires_at": lease.LeaseExpiresAt}).Error; err != nil {
			return err
		}
		won = true
		return nil
	})
	return execution, lease, won, err
}

func RecordAppTaskObservationTx(tx *gorm.DB, lease AppTaskReconcile, state, observationError string, now time.Time) error {
	if (state != "" && state != "queued" && state != "running") || len(observationError) > 128 {
		return errors.New("invalid_provider_observation")
	}
	var execution AppTaskExecution
	if err := lockForUpdate(tx).Where("id = ?", lease.ExecutionID).First(&execution).Error; err != nil {
		return err
	}
	if execution.ExecutionKind != AppExecutionKindTask {
		return errors.New("invalid_execution_kind")
	}
	var current AppTaskReconcile
	if err := lockForUpdate(tx).Where("execution_id = ?", lease.ExecutionID).First(&current).Error; err != nil {
		return err
	}
	if lease.LeaseOwner == "" || current.LeaseOwner != lease.LeaseOwner ||
		current.LeaseEpoch != lease.LeaseEpoch || current.LeaseExpiresAt <= now.Unix() {
		return ErrAppTaskLeaseLost
	}
	if state != "" {
		if !execution.ProviderAccepted || execution.ProviderEvidenceHash != "" {
			return errors.New("invalid_state_transition")
		}
		if err := tx.Model(&AppTaskExecution{}).Where(
			"id = ? AND execution_kind = ?", execution.ID, AppExecutionKindTask,
		).
			Updates(map[string]any{"provider_state": state, "updated_at": now.Unix()}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Task{}).Where("task_id = ? AND user_id = ? AND execution_mode = ?",
			execution.TaskID, execution.UserID, "app_managed").
			Updates(map[string]any{"status": appProviderTaskStatus(state), "updated_at": now.Unix()}).Error; err != nil {
			return err
		}
	}
	return tx.Model(&AppTaskReconcile{}).Where("execution_id = ?", lease.ExecutionID).Updates(map[string]any{
		"lease_owner": "", "lease_expires_at": 0, "next_attempt_at": now.Add(30 * time.Second).Unix(),
		"last_observation_error": observationError,
	}).Error
}

func HasPendingAppTaskReconciliation() bool {
	var row AppTaskReconcile
	query := DB.Where("next_attempt_at > 0 AND EXISTS (?)",
		DB.Model(&AppTaskExecution{}).Select("1").
			Where("app_task_executions.id = app_task_reconciles.execution_id AND app_task_executions.execution_kind = ?",
				AppExecutionKindTask)).
		Limit(1).Find(&row)
	if query.Error != nil {
		common.SysError("check pending app task reconciliation failed")
		return true
	}
	return query.RowsAffected > 0
}

// FinalizeAppTaskTx commits provider terminal facts, primary funding, counters,
// Task projection, settlement marker and redacted outbox as one transaction.
// Insufficient adjustment commits facts as pending without changing money.
func FinalizeAppTaskTx(tx *gorm.DB, lease AppTaskReconcile, terminal AppTaskTerminal, now time.Time) (bool, error) {
	if lease.ExecutionID <= 0 || !slices.Contains([]string{"succeeded", "failed", "cancelled"}, terminal.ProviderState) ||
		terminal.FinalQuota < 0 || terminal.FinalQuota > math.MaxInt32 {
		return false, errors.New("invalid_terminal_evidence")
	}
	for _, raw := range []*string{&terminal.UsageJSON, &terminal.ArtifactsJSON, &terminal.ResultJSON} {
		if *raw == "" {
			continue
		}
		var value any
		if len(*raw) > 64*1024 || common.UnmarshalJsonStr(*raw, &value) != nil {
			return false, errors.New("invalid_terminal_evidence")
		}
		canonical, err := common.Marshal(value)
		if err != nil {
			return false, err
		}
		*raw = string(canonical)
	}
	raw, err := common.Marshal(terminal)
	if err != nil {
		return false, err
	}
	digest := appPluginStableID("app-task-terminal/v1", strconv.FormatInt(lease.ExecutionID, 10), string(raw))
	var locator AppTaskExecution
	if err := tx.Where("id = ? AND execution_kind = ?", lease.ExecutionID, AppExecutionKindTask).
		First(&locator).Error; err != nil {
		return false, err
	}
	user, sub, err := lockAppTaskFundingTx(tx, locator)
	if err != nil {
		return false, err
	}
	var execution AppTaskExecution
	if err := lockForUpdate(tx).Where("id = ?", lease.ExecutionID).First(&execution).Error; err != nil {
		return false, err
	}
	if execution.ExecutionKind != AppExecutionKindTask {
		return false, errors.New("invalid_execution_kind")
	}
	if execution.UserID != locator.UserID || execution.FundingSource != locator.FundingSource ||
		execution.SubscriptionID != locator.SubscriptionID || execution.FundingRef != locator.FundingRef ||
		execution.ReservedQuota < 0 || execution.ReservedQuota > math.MaxInt32 {
		return false, errors.New("invalid_funding")
	}
	var marker AppTaskSettlement
	existing := lockForUpdate(tx).Where("execution_id = ?", execution.ID).Limit(1).Find(&marker)
	if existing.Error != nil {
		return false, existing.Error
	}
	if existing.RowsAffected != 0 {
		if marker.TerminalEvidenceHash != digest {
			return false, errors.New("terminal_evidence_conflict")
		}
		return true, nil
	}
	var current AppTaskReconcile
	if err := lockForUpdate(tx).Where("execution_id = ?", execution.ID).First(&current).Error; err != nil {
		return false, err
	}
	if lease.LeaseOwner == "" || current.LeaseOwner != lease.LeaseOwner ||
		current.LeaseEpoch != lease.LeaseEpoch || current.LeaseExpiresAt <= now.Unix() {
		return false, ErrAppTaskLeaseLost
	}
	if !execution.ProviderAccepted || execution.ProviderTaskID == "" {
		return false, errors.New("provider_acceptance_unresolved")
	}
	if execution.ProviderEvidenceHash != "" && execution.ProviderEvidenceHash != digest {
		return false, errors.New("terminal_evidence_conflict")
	}
	settled, err := settleAppTaskFundingTx(tx, execution, user, sub, terminal.FinalQuota)
	if err != nil {
		return false, err
	}
	billing := "settlement_pending"
	if settled {
		billing = "settled"
	}
	if err := tx.Model(&AppTaskExecution{}).Where(
		"id = ? AND execution_kind = ?", execution.ID, AppExecutionKindTask,
	).Updates(map[string]any{
		"provider_state": terminal.ProviderState, "billing_state": billing, "final_quota": terminal.FinalQuota,
		"usage_json": terminal.UsageJSON, "artifacts_json": terminal.ArtifactsJSON, "result_json": terminal.ResultJSON,
		"provider_evidence_hash": digest, "updated_at": now.Unix(),
	}).Error; err != nil {
		return false, err
	}
	var task Task
	if err := lockForUpdate(tx).Where("task_id = ? AND user_id = ? AND execution_mode = ?",
		execution.TaskID, execution.UserID, "app_managed").First(&task).Error; err != nil {
		return false, err
	}
	if task.TaskID != execution.TaskID || task.ChannelId != execution.ChannelID ||
		task.PrivateData.UpstreamTaskID != execution.ProviderTaskID {
		return false, errors.New("invalid_task_projection")
	}
	quota := execution.ReservedQuota
	if settled {
		quota = terminal.FinalQuota
	}
	projection := map[string]any{
		"status": appProviderTaskStatus(terminal.ProviderState), "quota": quota, "progress": "100%",
		"finish_time": now.Unix(), "updated_at": now.Unix(),
	}
	if terminal.ArtifactURLs != nil {
		task.PrivateData.AppArtifactURLs = maps.Clone(terminal.ArtifactURLs)
		projection["private_data"] = task.PrivateData
	}
	if err := tx.Model(&Task{}).Where("id = ?", task.ID).Updates(projection).Error; err != nil {
		return false, err
	}
	nextAttempt := now.Add(30 * time.Second).Unix()
	if settled {
		nextAttempt = 0
	}
	if err := tx.Model(&AppTaskReconcile{}).Where("execution_id = ?", execution.ID).Updates(map[string]any{
		"next_attempt_at": nextAttempt, "lease_owner": "", "lease_expires_at": 0,
	}).Error; err != nil {
		return false, err
	}
	if !settled {
		return false, nil
	}
	if err := tx.Model(&User{}).Where("id = ?", execution.UserID).Updates(map[string]any{
		"used_quota":    gorm.Expr("used_quota + ?", terminal.FinalQuota),
		"request_count": gorm.Expr("request_count + 1"),
	}).Error; err != nil {
		return false, err
	}
	channel := tx.Model(&Channel{}).Where("id = ?", execution.ChannelID).
		Update("used_quota", gorm.Expr("used_quota + ?", terminal.FinalQuota))
	if channel.Error != nil {
		return false, channel.Error
	}
	if channel.RowsAffected == 0 {
		// MySQL reports zero changed rows for a zero charge. A current locking
		// read distinguishes that from deletion; a snapshot COUNT can still see
		// a channel deleted after this transaction's earlier execution read.
		var currentChannel Channel
		if err := lockForUpdate(tx).Select("id").Where("id = ?", execution.ChannelID).First(&currentChannel).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, errors.New("settlement_channel_missing")
			}
			return false, err
		}
	}
	marker = AppTaskSettlement{ExecutionID: execution.ID, FundingSource: execution.FundingSource,
		FundingRef: execution.FundingRef, ReservedQuota: execution.ReservedQuota, FinalQuota: terminal.FinalQuota,
		TerminalEvidenceHash: digest, CreatedAt: now.Unix()}
	if err := tx.Create(&marker).Error; err != nil {
		return false, err
	}
	eventID := appPluginStableID("app-task-terminal-event/v1", strconv.FormatInt(execution.ID, 10))
	facts, err := common.Marshal(map[string]any{
		"event_id": eventID, "task_id": execution.TaskID, "grant_id": execution.GrantID,
		"status": terminal.ProviderState, "billing_state": "settled", "final_quota": terminal.FinalQuota,
		"actual_model": execution.ActualModel, "updated_at": now.Unix(),
	})
	if err != nil {
		return false, err
	}
	if err := tx.Create(&AppTaskOutbox{EventID: eventID, ExecutionID: execution.ID, FactsJSON: string(facts), CreatedAt: now.Unix()}).Error; err != nil {
		return false, err
	}
	return true, nil
}
