package model

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

type AppExecutionFunding struct {
	Source         string `json:"source"`
	Reference      string `json:"reference"`
	SubscriptionID int    `json:"subscription_id"`
	PeriodStart    int64  `json:"period_start"`
	PeriodEnd      int64  `json:"period_end"`
	PeriodReset    int64  `json:"period_reset"`
}

func ReserveAppExecutionFundingTx(tx *gorm.DB, grant AppExecutionGrant, amount int64, now time.Time) (AppExecutionFunding, error) {
	var funding AppExecutionFunding
	if amount < 0 || amount > 2147483647 || common.UnmarshalJsonStr(grant.FundingSnapshotJSON, &funding) != nil ||
		funding.Source != grant.FundingSource || funding.Reference != grant.FundingRef {
		return funding, errors.New("invalid_funding")
	}
	var user User
	if err := lockForUpdate(tx).Where("id = ?", grant.UserID).First(&user).Error; err != nil {
		return funding, err
	}
	if user.Status != common.UserStatusEnabled {
		return funding, errors.New("identity_inactive")
	}
	switch funding.Source {
	case "wallet":
		if common.RedisEnabled || common.BatchUpdateEnabled {
			return funding, errors.New("wallet_accounting_unavailable")
		}
		if user.Quota < 0 || int64(user.Quota) > common.MaxWalletQuota || int64(user.Quota) < amount {
			return funding, errors.New("insufficient_quota")
		}
		return funding, tx.Model(&User{}).Where("id = ?", user.Id).
			Update("quota", gorm.Expr("quota - ?", amount)).Error
	case "subscription":
		var sub UserSubscription
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", funding.SubscriptionID, user.Id).First(&sub).Error; err != nil {
			return funding, err
		}
		var plan SubscriptionPlan
		if err := lockForUpdate(tx).Where("id = ?", sub.PlanId).First(&plan).Error; err != nil {
			return funding, err
		}
		if sub.Status != "active" || sub.StartTime > now.Unix() || sub.EndTime <= now.Unix() ||
			sub.StartTime != funding.PeriodStart || sub.EndTime != funding.PeriodEnd || sub.LastResetTime != funding.PeriodReset ||
			NormalizeResetPeriod(plan.QuotaResetPeriod) != SubscriptionResetNever || sub.NextResetTime != 0 {
			return funding, errors.New("funding_period_changed")
		}
		if sub.AmountUsed < 0 || sub.AmountTotal < 0 || sub.AmountUsed > common.MaxWalletQuota-amount ||
			(sub.AmountTotal > 0 && (sub.AmountUsed > sub.AmountTotal || amount > sub.AmountTotal-sub.AmountUsed)) {
			return funding, errors.New("insufficient_quota")
		}
		return funding, tx.Model(&UserSubscription{}).Where("id = ?", sub.Id).
			Update("amount_used", gorm.Expr("amount_used + ?", amount)).Error
	default:
		return funding, errors.New("invalid_funding")
	}
}

// Caller locks the subscription first, matching reservation lock order.
func GuardAppSubscriptionMutationTx(tx *gorm.DB, subscriptionID int) error {
	// Older installations/tests may invoke subscription operations before B1.6
	// migration. Once installed, the authoritative execution records are used.
	if !tx.Migrator().HasTable(&AppTaskExecution{}) {
		return nil
	}
	var row AppTaskExecution
	q := lockForUpdate(tx).Where("subscription_id = ? AND billing_state <> ?", subscriptionID, "settled").Limit(1).Find(&row)
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 0 {
		return errors.New("app_execution_unsettled")
	}
	return nil
}

// Funding locks precede execution locks, matching reservation and subscription
// mutation order. Accepted work does not depend on current login/status flags.
func lockAppTaskFundingTx(tx *gorm.DB, execution AppTaskExecution) (User, *UserSubscription, error) {
	var user User
	if err := lockForUpdate(tx).Where("id = ?", execution.UserID).First(&user).Error; err != nil {
		return user, nil, err
	}
	if execution.FundingSource == "wallet" {
		return user, nil, nil
	}
	if execution.FundingSource != "subscription" || execution.SubscriptionID <= 0 {
		return user, nil, errors.New("invalid_funding")
	}
	var sub UserSubscription
	if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", execution.SubscriptionID, execution.UserID).
		First(&sub).Error; err != nil {
		return user, nil, err
	}
	return user, &sub, nil
}

func settleAppTaskFundingTx(tx *gorm.DB, execution AppTaskExecution, user User, sub *UserSubscription, final int64) (bool, error) {
	delta := final - execution.ReservedQuota
	if execution.FundingSource == "wallet" {
		balance := int64(user.Quota)
		// Existing incurred debt may make a wallet negative. A zero delta or
		// refund must still close accepted work; new debits require funds.
		if balance > common.MaxWalletQuota ||
			(delta > 0 && balance < delta) || (delta < 0 && balance > common.MaxWalletQuota+delta) {
			return false, nil
		}
		if err := tx.Model(&User{}).Where("id = ?", user.Id).
			Update("quota", gorm.Expr("quota - ?", delta)).Error; err != nil {
			return false, err
		}
		return true, nil
	}
	if sub == nil || sub.StartTime != execution.PeriodStart || sub.EndTime != execution.PeriodEnd ||
		sub.LastResetTime != execution.PeriodReset || sub.AmountUsed < 0 || sub.AmountTotal < 0 ||
		sub.AmountUsed > common.MaxWalletQuota || sub.AmountUsed < execution.ReservedQuota {
		return false, nil
	}
	if (delta > 0 && (sub.AmountUsed > common.MaxWalletQuota-delta ||
		(sub.AmountTotal > 0 && (sub.AmountUsed > sub.AmountTotal || delta > sub.AmountTotal-sub.AmountUsed)))) ||
		(delta < 0 && sub.AmountUsed < -delta) {
		return false, nil
	}
	if err := tx.Model(&UserSubscription{}).Where("id = ?", sub.Id).
		Update("amount_used", gorm.Expr("amount_used + ?", delta)).Error; err != nil {
		return false, err
	}
	return true, nil
}
