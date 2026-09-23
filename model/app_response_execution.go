package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

func ClaimAppResponseExecutionTx(tx *gorm.DB, grantID string, request AppTaskExecution,
	now time.Time) (AppTaskExecution, *AppResponseResult, bool, error) {
	var grant AppExecutionGrant
	if err := lockForUpdate(tx).Where("grant_id = ?", grantID).First(&grant).Error; err != nil {
		return AppTaskExecution{}, nil, false, err
	}
	if request.GrantID != grantID || grant.AppKey != request.AppKey ||
		grant.InstallationID != request.InstallationID ||
		grant.AppSessionID != request.AppSessionID || grant.Subject != request.Subject ||
		grant.UserID != request.UserID || grant.RunID != request.RunID ||
		grant.ExecutionRequestID != request.ExecutionRequestID ||
		grant.Operation != request.Operation || request.SubmissionHash == "" {
		return AppTaskExecution{}, nil, false, errors.New("scope_denied")
	}
	existing, replay, found, err := lookupAppResponseExecutionTx(
		tx, grant, request.SubmissionHash,
	)
	if err != nil || found {
		return existing, replay, false, err
	}
	if grant.ExpiresAt <= now.Unix() {
		return AppTaskExecution{}, nil, false, errors.New("execution_grant_expired")
	}
	if request.ReservedQuota < 0 || request.ReservedQuota > common.MaxQuota ||
		request.FundingSource != grant.FundingSource ||
		request.RequestFactsJSON == "" || len(request.RequestFactsJSON) > 64*1024 {
		return AppTaskExecution{}, nil, false, errors.New("invalid_funding")
	}
	var requestFacts map[string]any
	if common.UnmarshalJsonStr(request.RequestFactsJSON, &requestFacts) != nil {
		return AppTaskExecution{}, nil, false, errors.New("invalid_request_facts")
	}
	var candidates []AppExecutionModel
	if common.UnmarshalJsonStr(grant.ModelsJSON, &candidates) != nil {
		return AppTaskExecution{}, nil, false, errors.New("invalid_grant")
	}
	selected := slices.IndexFunc(candidates, func(candidate AppExecutionModel) bool {
		return candidate.ExecutionKind == AppExecutionModelKindNativeResponse &&
			candidate.Protocol == "openai_responses" &&
			candidate.ChannelType == constant.ChannelTypeOpenAI &&
			candidate.APIType == constant.APITypeOpenAI &&
			candidate.RequestProfile == "responses.text.v1" &&
			candidate.PluginKey == "" && candidate.PluginVersion == "" && candidate.PluginSHA256 == "" &&
			candidate.PublicModel == request.PublicModel && candidate.ActualModel == request.ActualModel &&
			candidate.ChannelID == request.ChannelID && candidate.Group == request.ActualGroup &&
			candidate.ConnectionDigest == request.ConnectionDigest &&
			candidate.CredentialDigest == request.CredentialDigest &&
			candidate.CredentialIndex == request.CredentialIndex
	})
	if selected < 0 {
		return AppTaskExecution{}, nil, false, errors.New("scope_denied")
	}
	funding, err := ReserveAppExecutionFundingTx(tx, grant, request.ReservedQuota, now)
	if err != nil {
		return AppTaskExecution{}, nil, false, err
	}
	executionID, err := NewAppPluginOpaqueID()
	if err != nil {
		return AppTaskExecution{}, nil, false, err
	}
	price, err := common.Marshal(candidates[selected])
	if err != nil {
		return AppTaskExecution{}, nil, false, err
	}
	logical := appPluginStableID("app-task-logical/v1", grant.InstallationID, grant.Subject,
		grant.RunID, grant.ExecutionRequestID, grant.Operation)
	row := AppTaskExecution{
		ExecutionKind: AppExecutionKindResponse, LogicalHash: logical, TaskID: executionID,
		GrantID: grantID, AppKey: grant.AppKey, InstallationID: grant.InstallationID,
		AppSessionID: grant.AppSessionID, Subject: grant.Subject, UserID: grant.UserID,
		RunID: grant.RunID, ExecutionRequestID: grant.ExecutionRequestID, Operation: grant.Operation,
		SubmissionHash: request.SubmissionHash, PublicModel: request.PublicModel,
		ActualModel: request.ActualModel, ActualGroup: request.ActualGroup,
		ChannelID: request.ChannelID, ConnectionDigest: request.ConnectionDigest,
		CredentialDigest: request.CredentialDigest, CredentialIndex: request.CredentialIndex,
		Protocol: request.Protocol, Status: "dispatching", ProviderState: "unknown",
		BillingState: "reserved", FundingSource: grant.FundingSource, FundingRef: grant.FundingRef,
		SubscriptionID: funding.SubscriptionID, PeriodStart: funding.PeriodStart,
		PeriodEnd: funding.PeriodEnd, PeriodReset: funding.PeriodReset,
		ReservedQuota: request.ReservedQuota, PriceSnapshotJSON: string(price),
		RequestFactsJSON: request.RequestFactsJSON, PricedAt: now.Unix(),
		CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	}
	if err := tx.Create(&row).Error; err != nil {
		return AppTaskExecution{}, nil, false, err
	}
	return row, nil, true, nil
}

func LookupAppResponseExecutionTx(tx *gorm.DB, grantID, submissionHash string,
) (AppTaskExecution, *AppResponseResult, bool, error) {
	var grant AppExecutionGrant
	if grantID == "" || submissionHash == "" {
		return AppTaskExecution{}, nil, false, errors.New("scope_denied")
	}
	if err := lockForUpdate(tx).Where("grant_id = ?", grantID).First(&grant).Error; err != nil {
		return AppTaskExecution{}, nil, false, err
	}
	return lookupAppResponseExecutionTx(tx, grant, submissionHash)
}

func lookupAppResponseExecutionTx(tx *gorm.DB, grant AppExecutionGrant, submissionHash string,
) (AppTaskExecution, *AppResponseResult, bool, error) {
	logical := appPluginStableID("app-task-logical/v1", grant.InstallationID, grant.Subject,
		grant.RunID, grant.ExecutionRequestID, grant.Operation)
	var existing AppTaskExecution
	query := lockForUpdate(tx).Where("logical_hash = ? OR grant_id = ?", logical, grant.GrantID).
		Limit(1).Find(&existing)
	if query.Error != nil {
		return AppTaskExecution{}, nil, false, query.Error
	}
	if query.RowsAffected == 0 {
		return AppTaskExecution{}, nil, false, nil
	}
	if existing.ExecutionKind != AppExecutionKindResponse ||
		existing.SubmissionHash != submissionHash || existing.GrantID != grant.GrantID {
		return AppTaskExecution{}, nil, true, errors.New("idempotency_conflict")
	}
	switch existing.Status {
	case "dispatching", "acceptance_unknown":
		return existing, nil, true, errors.New("execution_outcome_unknown")
	case "captured", "rejected":
		var result AppResponseResult
		if err := tx.Where("execution_id = ?", existing.ID).First(&result).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return existing, nil, true, errors.New("execution_outcome_unknown")
			}
			return existing, nil, true, err
		}
		return existing, &result, true, nil
	default:
		return existing, nil, true, errors.New("execution_outcome_unknown")
	}
}

type AppResponseTerminal struct {
	Rejected            bool
	HTTPStatus          int
	SafeHeadersJSON     string
	Body                []byte
	BodySHA256          string
	ResponseID          string
	ProviderModel       string
	ProviderStatus      string
	RawUsageJSON        string
	NormalizedUsageJSON string
	FinalQuota          int64
}

func FinalizeAppResponseTx(tx *gorm.DB, executionID int64, terminal AppResponseTerminal,
	now time.Time) (AppResponseResult, error) {
	var result AppResponseResult
	if executionID <= 0 || now.Unix() <= 0 || terminal.FinalQuota < 0 ||
		terminal.FinalQuota > math.MaxInt32 || (!terminal.Rejected && len(terminal.Body) == 0) ||
		len(terminal.Body) > 64*1024 {
		return result, errors.New("invalid_terminal_evidence")
	}
	bodyDigest := sha256.Sum256(terminal.Body)
	if terminal.BodySHA256 != hex.EncodeToString(bodyDigest[:]) {
		return result, errors.New("invalid_terminal_evidence")
	}
	safeHeaders, err := canonicalAppResponseJSON(terminal.SafeHeadersJSON)
	if err != nil {
		return result, err
	}
	rawUsage, err := canonicalOptionalAppResponseJSON(terminal.RawUsageJSON)
	if err != nil {
		return result, err
	}
	normalizedUsage, err := canonicalOptionalAppResponseJSON(terminal.NormalizedUsageJSON)
	if err != nil {
		return result, err
	}
	if terminal.Rejected {
		if terminal.HTTPStatus < 300 || terminal.HTTPStatus > 599 || terminal.FinalQuota != 0 ||
			terminal.ResponseID != "" || terminal.ProviderModel != "" ||
			terminal.ProviderStatus != "rejected" || rawUsage != "" || normalizedUsage != "" {
			return result, errors.New("invalid_terminal_evidence")
		}
	} else if terminal.HTTPStatus != 200 || terminal.ResponseID == "" ||
		len(terminal.ResponseID) > 255 || terminal.ProviderModel == "" ||
		terminal.ProviderStatus != "completed" || rawUsage == "" || normalizedUsage == "" {
		return result, errors.New("invalid_terminal_evidence")
	}
	terminal.SafeHeadersJSON, terminal.RawUsageJSON = safeHeaders, rawUsage
	terminal.NormalizedUsageJSON = normalizedUsage
	evidence, err := common.Marshal(terminal)
	if err != nil {
		return result, err
	}
	digest := appPluginStableID("app-response-terminal/v1",
		strconv.FormatInt(executionID, 10), string(evidence))

	var locator AppTaskExecution
	if err := tx.Where("id = ? AND execution_kind = ?", executionID, AppExecutionKindResponse).
		First(&locator).Error; err != nil {
		return result, err
	}
	user, sub, err := lockAppTaskFundingTx(tx, locator)
	if err != nil {
		return result, err
	}
	var execution AppTaskExecution
	if err := lockForUpdate(tx).Where("id = ?", executionID).First(&execution).Error; err != nil {
		return result, err
	}
	if execution.ExecutionKind != AppExecutionKindResponse ||
		execution.UserID != locator.UserID || execution.FundingSource != locator.FundingSource ||
		execution.SubscriptionID != locator.SubscriptionID || execution.FundingRef != locator.FundingRef ||
		execution.ReservedQuota < 0 || execution.ReservedQuota > math.MaxInt32 ||
		terminal.FinalQuota > execution.ReservedQuota ||
		(!terminal.Rejected && terminal.ProviderModel != execution.ActualModel) {
		return result, errors.New("invalid_terminal_evidence")
	}
	var marker AppTaskSettlement
	existing := lockForUpdate(tx).Where("execution_id = ?", execution.ID).Limit(1).Find(&marker)
	if existing.Error != nil {
		return result, existing.Error
	}
	if existing.RowsAffected != 0 {
		if marker.TerminalEvidenceHash != digest {
			return result, errors.New("terminal_evidence_conflict")
		}
		if err := tx.Where("execution_id = ?", execution.ID).First(&result).Error; err != nil {
			return result, err
		}
		return result, nil
	}
	if execution.Status != "dispatching" || execution.BillingState != "reserved" {
		return result, errors.New("invalid_state_transition")
	}
	settled, err := settleAppTaskFundingTx(tx, execution, user, sub, terminal.FinalQuota)
	if err != nil {
		return result, err
	}
	if !settled {
		return result, errors.New("invalid_funding")
	}
	result = AppResponseResult{
		ExecutionID: execution.ID, HTTPStatus: terminal.HTTPStatus,
		SafeHeadersJSON: safeHeaders, Body: append([]byte{}, terminal.Body...),
		BodySHA256: terminal.BodySHA256, ResponseID: terminal.ResponseID,
		ProviderModel: terminal.ProviderModel, ProviderStatus: terminal.ProviderStatus,
		RawUsageJSON: rawUsage, NormalizedUsageJSON: normalizedUsage,
		CapturedAt: now.Unix(),
	}
	if err := tx.Create(&result).Error; err != nil {
		return AppResponseResult{}, err
	}
	status := "captured"
	if terminal.Rejected {
		status = "rejected"
	}
	if err := tx.Model(&AppTaskExecution{}).Where(
		"id = ? AND execution_kind = ?", execution.ID, AppExecutionKindResponse,
	).Updates(map[string]any{
		"status": status, "provider_state": terminal.ProviderStatus,
		"billing_state": "settled", "final_quota": terminal.FinalQuota,
		"usage_json": normalizedUsage, "provider_evidence_hash": digest,
		"updated_at": now.Unix(),
	}).Error; err != nil {
		return AppResponseResult{}, err
	}
	if err := tx.Model(&User{}).Where("id = ?", execution.UserID).Updates(map[string]any{
		"used_quota":    gorm.Expr("used_quota + ?", terminal.FinalQuota),
		"request_count": gorm.Expr("request_count + 1"),
	}).Error; err != nil {
		return AppResponseResult{}, err
	}
	channel := tx.Model(&Channel{}).Where("id = ?", execution.ChannelID).
		Update("used_quota", gorm.Expr("used_quota + ?", terminal.FinalQuota))
	if channel.Error != nil {
		return AppResponseResult{}, channel.Error
	}
	if channel.RowsAffected == 0 {
		var current Channel
		if err := lockForUpdate(tx).Select("id").Where("id = ?", execution.ChannelID).
			First(&current).Error; err != nil {
			return AppResponseResult{}, err
		}
	}
	marker = AppTaskSettlement{
		ExecutionID: execution.ID, FundingSource: execution.FundingSource,
		FundingRef: execution.FundingRef, ReservedQuota: execution.ReservedQuota,
		FinalQuota: terminal.FinalQuota, TerminalEvidenceHash: digest,
		CreatedAt: now.Unix(),
	}
	if err := tx.Create(&marker).Error; err != nil {
		return AppResponseResult{}, err
	}
	eventID := appPluginStableID("app-response-terminal-event/v1",
		strconv.FormatInt(execution.ID, 10))
	facts, err := common.Marshal(map[string]any{
		"event_id": eventID, "execution_id": execution.TaskID,
		"grant_id": execution.GrantID, "status": status,
		"billing_state": "settled", "final_quota": terminal.FinalQuota,
		"actual_model": execution.ActualModel, "updated_at": now.Unix(),
	})
	if err != nil {
		return AppResponseResult{}, err
	}
	if err := tx.Create(&AppTaskOutbox{
		EventID: eventID, ExecutionID: execution.ID,
		FactsJSON: string(facts), CreatedAt: now.Unix(),
	}).Error; err != nil {
		return AppResponseResult{}, err
	}
	return result, nil
}

func MarkAppResponseOutcomeUnknownTx(tx *gorm.DB, executionID int64, now time.Time) error {
	if executionID <= 0 || now.Unix() <= 0 {
		return errors.New("invalid_execution")
	}
	var execution AppTaskExecution
	if err := lockForUpdate(tx).Where("id = ?", executionID).First(&execution).Error; err != nil {
		return err
	}
	if execution.ExecutionKind != AppExecutionKindResponse {
		return errors.New("invalid_execution_kind")
	}
	switch execution.Status {
	case "captured", "rejected", "acceptance_unknown":
		return nil
	case "dispatching":
		return tx.Model(&AppTaskExecution{}).Where(
			"id = ? AND execution_kind = ?", executionID, AppExecutionKindResponse,
		).Updates(map[string]any{
			"status": "acceptance_unknown", "updated_at": now.Unix(),
		}).Error
	default:
		return errors.New("invalid_state_transition")
	}
}

func canonicalAppResponseJSON(raw string) (string, error) {
	if raw == "" || len(raw) > 64*1024 {
		return "", errors.New("invalid_terminal_evidence")
	}
	var value map[string]any
	if common.UnmarshalJsonStr(raw, &value) != nil {
		return "", errors.New("invalid_terminal_evidence")
	}
	canonical, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

func canonicalOptionalAppResponseJSON(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > 64*1024 {
		return "", errors.New("invalid_terminal_evidence")
	}
	var value any
	if common.UnmarshalJsonStr(raw, &value) != nil || value == nil {
		return "", errors.New("invalid_terminal_evidence")
	}
	canonical, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}
