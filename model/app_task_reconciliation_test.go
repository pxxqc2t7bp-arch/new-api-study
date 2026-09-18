package model

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestScopedTaskLookupCannotReadAnotherUser(t *testing.T) {
	db, installation, _ := appCredentialFixture(t)
	require.NoError(t, MigrateAppExecutionTables(db))
	owned := AppTaskExecution{
		TaskID: "task-owned", GrantID: "grant-owned", AppKey: installation.AppKey,
		InstallationID: installation.InstallationID, Subject: "owner", AppSessionID: "owner-session",
		UserID: 101, RunID: "run-owned", ExecutionRequestID: "execution-owned", Operation: "model.generate",
		SubmissionHash: appPluginDigest("owned"), ProviderAccepted: true, Status: "accepted",
		ActualModel: "doubao-seedance-2-5-260628", PluginKey: "doubao", PluginVersion: "1.2.0",
		LogicalHash: appPluginDigest("owned-logical-operation"),
	}
	require.NoError(t, db.Create(&owned).Error)
	scope := AppTaskScope{AppKey: owned.AppKey, InstallationID: owned.InstallationID,
		Subject: owned.Subject, AppSessionID: owned.AppSessionID, UserID: owned.UserID}
	for _, byGrant := range []bool{false, true} {
		taskID, grantID := owned.TaskID, ""
		if byGrant {
			taskID, grantID = "", owned.GrantID
		}
		found, err := GetScopedAppTask(t.Context(), db, scope, taskID, grantID)
		require.NoError(t, err)
		assert.Equal(t, owned.TaskID, found.TaskID)
		alias := scope
		alias.Subject = strings.ToUpper(scope.Subject)
		_, err = GetScopedAppTask(t.Context(), db, alias, taskID, grantID)
		require.ErrorContains(t, err, "not_found", "database collation cannot change subject authority")
		_, err = GetScopedAppTask(t.Context(), db, scope, strings.ToUpper(taskID), strings.ToUpper(grantID))
		require.ErrorContains(t, err, "not_found", "opaque selectors are exact")
		current := scope
		current.AppSessionID = "new-active-session"
		found, err = GetScopedAppTask(t.Context(), db, current, taskID, grantID)
		require.NoError(t, err, "current same-user session need not be the original session")
		assert.Equal(t, owned.TaskID, found.TaskID)
		for _, field := range []string{"user", "subject", "app", "installation"} {
			t.Run(field+map[bool]string{false: " by task", true: " by grant"}[byGrant], func(t *testing.T) {
				other := scope
				switch field {
				case "user":
					other.UserID++
				case "subject":
					other.Subject = "other"
				case "app":
					other.AppKey = "other"
				case "installation":
					other.InstallationID = "other"
				}
				result, err := GetScopedAppTask(t.Context(), db, other, taskID, grantID)
				require.ErrorContains(t, err, "not_found")
				assert.Empty(t, result.TaskID)
			})
		}
	}
	var after AppTaskExecution
	require.NoError(t, db.Where("task_id = ?", owned.TaskID).First(&after).Error)
	assert.Equal(t, owned, after, "lookup must not advance provider or accounting state")
	for _, table := range []any{&AppTaskSettlement{}, &AppTaskOutbox{}} {
		var count int64
		require.NoError(t, db.Model(table).Count(&count).Error)
		assert.Zero(t, count, "reads must not settle or publish terminal events")
	}
}

func TestTaskPathsExcludeResponseExecutions(t *testing.T) {
	db, installation, now := appCredentialFixture(t)
	require.NoError(t, db.AutoMigrate(&User{}, &Task{}, &SystemTask{}))
	require.NoError(t, MigrateAppExecutionTables(db))
	response := AppTaskExecution{
		ExecutionKind: AppExecutionKindResponse,
		TaskID:        "response-execution", GrantID: "response-grant", AppKey: installation.AppKey,
		InstallationID: installation.InstallationID, Subject: "owner", AppSessionID: "owner-session",
		UserID: 101, RunID: "response-run", ExecutionRequestID: "response-request", Operation: "model.generate",
		SubmissionHash: appPluginDigest("response"), Status: "dispatching", ProviderState: "unknown",
		BillingState: "reserved", LogicalHash: appPluginDigest("response-logical"),
	}
	require.NoError(t, db.Create(&response).Error)
	require.NoError(t, db.Create(&AppTaskReconcile{ExecutionID: response.ID, NextAttemptAt: now.Unix()}).Error)
	scope := AppTaskScope{AppKey: response.AppKey, InstallationID: response.InstallationID,
		Subject: response.Subject, AppSessionID: response.AppSessionID, UserID: response.UserID}

	_, err := GetScopedAppTask(t.Context(), db, scope, response.TaskID, "")
	require.ErrorContains(t, err, "not_found")
	err = RunAppPluginTransaction(db, func(tx *gorm.DB) error {
		return RecordAppTaskAcceptanceTx(tx, response.ID, "provider-task", "queued", now)
	})
	require.ErrorContains(t, err, "invalid_execution_kind")
	var taskCount int64
	require.NoError(t, db.Model(&Task{}).Count(&taskCount).Error)
	assert.Zero(t, taskCount)
	_, _, won, err := ClaimAppTaskReconcile(t.Context(), db, "task-worker", now, time.Minute)
	require.NoError(t, err)
	assert.False(t, won)
}

func TestAppTaskCancelReplayMigration(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "B1.5 upgrade"}[upgrade], func(t *testing.T) {
			db := openAppCredentialTestDB(t)
			if upgrade {
				require.NoError(t, MigrateAppPluginTables(db))
				require.NoError(t, MigrateAppPluginLaunchTables(db))
				require.NoError(t, db.Create(&AppPluginSessionRevokeReplay{
					ScopeHash: "old-scope", RequestHash: "old-request",
					InstallationID: "old-install", ResponseJSON: `{"state":"revoked"}`,
				}).Error)
			}
			require.NoError(t, MigrateAppExecutionTables(db))
			row := AppTaskCancelReplay{ScopeHash: "scope", RequestHash: "payload",
				InstallationID: "install", ResponseJSON: `{"state":"not_cancellable"}`}
			require.NoError(t, db.Create(&row).Error)
			require.NoError(t, MigrateAppExecutionTables(db))
			require.NoError(t, MigrateAppExecutionTables(db))
			var saved AppTaskCancelReplay
			require.NoError(t, db.Where("scope_hash = ?", row.ScopeHash).First(&saved).Error)
			assert.Equal(t, row, saved)
			require.Error(t, db.Create(&row).Error, "scope must be unique")
			require.ErrorContains(t, db.Transaction(func(tx *gorm.DB) error {
				row.ScopeHash = "rolled-back"
				if err := tx.Create(&row).Error; err != nil {
					return err
				}
				return errors.New("rollback probe")
			}), "rollback probe")
			var count int64
			require.NoError(t, db.Model(&AppTaskCancelReplay{}).Count(&count).Error)
			assert.EqualValues(t, 1, count)
			if upgrade {
				var old AppPluginSessionRevokeReplay
				require.NoError(t, db.Where("scope_hash = ?", "old-scope").First(&old).Error)
				assert.Equal(t, `{"state":"revoked"}`, old.ResponseJSON)
			}
		})
	}
}

func appTaskSettlementFixture(t *testing.T) (*gorm.DB, AppTaskExecution, time.Time) {
	t.Helper()
	return appTaskFundingFixture(t, "wallet")
}

func appTaskFundingFixture(t *testing.T, source string) (*gorm.DB, AppTaskExecution, time.Time) {
	t.Helper()
	redis, batch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, common.BatchUpdateEnabled = redis, batch })
	db, installation, now := appCredentialFixture(t)
	require.NoError(t, db.AutoMigrate(&User{}, &Channel{}, &Task{}, &SystemTask{}, &UserSubscription{}, &SubscriptionPlan{}))
	require.NoError(t, MigrateAppExecutionTables(db))
	payer := User{Username: "settlement-owner", AffCode: "settlement-owner",
		Group: "default", Status: common.UserStatusEnabled, Quota: 1000}
	require.NoError(t, db.Create(&payer).Error)
	channel := Channel{Name: "settlement-channel", Type: constant.ChannelTypeDoubaoVideo,
		Key: "offline-key", Status: common.ChannelStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(&channel).Error)
	admission := AppExecutionModel{PublicModel: "model", ActualModel: "model",
		ExecutionKind: AppExecutionModelKindTaskBacked, PluginKey: "doubao",
		PluginVersion: "1.2.0", PluginSHA256: appPluginDigest("plugin"),
		Protocol: "openai_video", ChannelID: channel.Id, Group: "default", GroupRatio: 1}
	raw, err := common.Marshal([]AppExecutionModel{admission})
	require.NoError(t, err)
	grant := AppExecutionGrant{GrantID: "settlement-grant", ScopeHash: appPluginDigest("settlement-scope"),
		TokenHash: appPluginDigest("settlement-token"), AppKey: installation.AppKey, InstallationID: installation.InstallationID,
		AppSessionID: "session", Subject: "owner", UserID: payer.Id, RunID: "run", ExecutionRequestID: "operation",
		Operation: "model.generate", FundingSource: "wallet", FundingRef: "wallet",
		FundingSnapshotJSON: `{"source":"wallet","reference":"wallet"}`, ModelsJSON: string(raw),
		ExpiresAt: now.Add(time.Minute).Unix()}
	if source == "subscription" {
		plan := SubscriptionPlan{Title: "fixed", QuotaResetPeriod: SubscriptionResetNever}
		require.NoError(t, db.Create(&plan).Error)
		sub := UserSubscription{UserId: payer.Id, PlanId: plan.Id, Status: "active", AmountTotal: 1000,
			StartTime: now.Add(-time.Hour).Unix(), EndTime: now.Add(time.Hour).Unix()}
		require.NoError(t, db.Create(&sub).Error)
		grant.FundingSource, grant.FundingRef = source, "subscription:"+strconv.Itoa(sub.Id)
		funding, err := common.Marshal(AppExecutionFunding{Source: source, Reference: grant.FundingRef,
			SubscriptionID: sub.Id, PeriodStart: sub.StartTime, PeriodEnd: sub.EndTime, PeriodReset: sub.LastResetTime})
		require.NoError(t, err)
		grant.FundingSnapshotJSON = string(funding)
	}
	require.NoError(t, db.Create(&grant).Error)
	request := AppTaskExecution{GrantID: grant.GrantID, AppKey: grant.AppKey, InstallationID: grant.InstallationID,
		AppSessionID: grant.AppSessionID, Subject: grant.Subject, UserID: grant.UserID,
		RunID: grant.RunID, ExecutionRequestID: grant.ExecutionRequestID, Operation: grant.Operation,
		PublicModel: admission.PublicModel, ActualModel: admission.ActualModel, ChannelID: channel.Id,
		ActualGroup: admission.Group, PluginKey: admission.PluginKey, PluginVersion: admission.PluginVersion,
		PluginSHA256: admission.PluginSHA256, Protocol: admission.Protocol, FundingSource: grant.FundingSource,
		ReservedQuota: 100, SubmissionHash: appPluginDigest("submission"),
		RequestFactsJSON: `{"seconds":5}`}
	var execution AppTaskExecution
	require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
		var won bool
		execution, won, err = ClaimAppTaskExecutionTx(tx, grant.GrantID, request, now)
		if err == nil {
			require.True(t, won)
		}
		return err
	}))
	return db, execution, now
}

func TestAppTaskDispatchFactsAndLeaseFencing(t *testing.T) {
	db, execution, now := appTaskSettlementFixture(t)
	assert.Equal(t, `{"seconds":5}`, execution.RequestFactsJSON)
	var wakes int64
	require.NoError(t, db.Model(&SystemTask{}).Where("type = ? AND active_key = ?", "app_task_reconcile", "app_task_reconcile").
		Count(&wakes).Error)
	assert.EqualValues(t, 1, wakes)
	require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
		return RecordAppTaskAcceptanceTx(tx, execution.ID, "", "unknown", now)
	}))
	var unknown AppTaskExecution
	require.NoError(t, db.First(&unknown, execution.ID).Error)
	assert.Equal(t, "acceptance_unknown", unknown.Status)
	assert.False(t, unknown.ProviderAccepted)
	_, first, won, err := ClaimAppTaskReconcile(t.Context(), db, "worker-one", now, time.Minute)
	require.NoError(t, err)
	require.True(t, won)
	err = RunAppPluginTransaction(db, func(tx *gorm.DB) error {
		_, err := FinalizeAppTaskTx(tx, first, AppTaskTerminal{ProviderState: "failed", FinalQuota: 0}, now)
		return err
	})
	require.Error(t, err, "unknown acceptance is not refund authority")
	require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
		return RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-accepted", "queued", now)
	}))
	require.Error(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
		return RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-another", "queued", now)
	}))
	_, _, won, err = ClaimAppTaskReconcile(t.Context(), db, "worker-two", now.Add(time.Second), time.Minute)
	require.NoError(t, err)
	assert.False(t, won)
	accepted, second, won, err := ClaimAppTaskReconcile(t.Context(), db, "worker-two", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, won)
	assert.Greater(t, second.LeaseEpoch, first.LeaseEpoch)
	assert.Equal(t, "cgt-accepted", accepted.ProviderTaskID)
	err = RunAppPluginTransaction(db, func(tx *gorm.DB) error {
		_, err := FinalizeAppTaskTx(tx, first, AppTaskTerminal{ProviderState: "succeeded", FinalQuota: 100}, now.Add(2*time.Minute))
		return err
	})
	require.ErrorIs(t, err, ErrAppTaskLeaseLost)
	var payer User
	require.NoError(t, db.First(&payer, execution.UserID).Error)
	assert.Equal(t, 900, payer.Quota)
	var markers int64
	require.NoError(t, db.Model(&AppTaskSettlement{}).Count(&markers).Error)
	assert.Zero(t, markers)
	t.Run("failed wakeup rolls back the claim and reserve", func(t *testing.T) {
		var grant AppExecutionGrant
		require.NoError(t, db.Where("grant_id = ?", execution.GrantID).First(&grant).Error)
		grant.GrantID, grant.RunID = "second-grant", "second-run"
		grant.ScopeHash, grant.TokenHash = appPluginDigest("second-scope"), appPluginDigest("second-token")
		require.NoError(t, db.Create(&grant).Error)
		request := execution
		request.GrantID, request.RunID = grant.GrantID, grant.RunID
		injected := errors.New("wakeup persistence failure")
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("app-wakeup-failure", func(tx *gorm.DB) {
			if tx.Statement.Table == "system_tasks" {
				tx.AddError(injected)
			}
		}))
		t.Cleanup(func() { _ = db.Callback().Create().Remove("app-wakeup-failure") })
		err := RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			_, _, err := ClaimAppTaskExecutionTx(tx, grant.GrantID, request, now)
			return err
		})
		require.ErrorIs(t, err, injected)
		require.NoError(t, db.First(&payer, execution.UserID).Error)
		assert.Equal(t, 900, payer.Quota)
		for _, table := range []any{&AppTaskExecution{}, &AppTaskReconcile{}, &SystemTask{}} {
			var count int64
			require.NoError(t, db.Model(table).Count(&count).Error)
			assert.EqualValues(t, 1, count)
		}
	})
}

func TestFinalizeAppTaskTx(t *testing.T) {
	t.Run("invalid terminal inputs cannot change money", func(t *testing.T) {
		db, execution, now := appTaskSettlementFixture(t)
		require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			return RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-invalid", "running", now)
		}))
		_, lease, won, err := ClaimAppTaskReconcile(t.Context(), db, "settler", now, time.Minute)
		require.NoError(t, err)
		require.True(t, won)
		for _, input := range []AppTaskTerminal{
			{ProviderState: "succeeded", FinalQuota: -1},
			{ProviderState: "succeeded", FinalQuota: 2147483648},
			{ProviderState: "running", FinalQuota: 100},
			{ProviderState: "succeeded", FinalQuota: 100, UsageJSON: `{"tokens":NaN}`},
		} {
			require.Error(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
				_, err := FinalizeAppTaskTx(tx, lease, input, now)
				return err
			}))
		}
		var payer User
		require.NoError(t, db.First(&payer, execution.UserID).Error)
		assert.Equal(t, 900, payer.Quota)
		assert.Zero(t, payer.UsedQuota)
	})
	for _, test := range []struct {
		name  string
		state string
		quota int64
	}{
		{"zero delta", "succeeded", 100}, {"additional charge", "succeeded", 150},
		{"partial refund", "succeeded", 50}, {"provider failure refund", "failed", 0},
		{"provider cancellation refund", "cancelled", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, execution, now := appTaskSettlementFixture(t)
			require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
				return RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-settlement", "running", now)
			}))
			_, lease, won, err := ClaimAppTaskReconcile(t.Context(), db, "settler", now, time.Minute)
			require.NoError(t, err)
			require.True(t, won)
			evidence := AppTaskTerminal{ProviderState: test.state, FinalQuota: test.quota,
				UsageJSON: `{"tokens":100}`, ArtifactsJSON: `[]`, ResultJSON: `{"error":null}`}
			// Existing accepted work survives App/service/user disable and changes
			// to the flags for admitting future wallet work.
			require.NoError(t, db.Model(&User{}).Where("id = ?", execution.UserID).Update("status", common.UserStatusDisabled).Error)
			common.RedisEnabled, common.BatchUpdateEnabled = true, true
			for range 2 {
				require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
					settled, err := FinalizeAppTaskTx(tx, lease, evidence, now)
					if err == nil {
						assert.True(t, settled)
					}
					return err
				}))
			}
			var payer User
			require.NoError(t, db.First(&payer, execution.UserID).Error)
			assert.EqualValues(t, 1000-test.quota, payer.Quota)
			assert.EqualValues(t, test.quota, payer.UsedQuota)
			assert.Equal(t, 1, payer.RequestCount)
			var channel Channel
			require.NoError(t, db.First(&channel, execution.ChannelID).Error)
			assert.Equal(t, test.quota, channel.UsedQuota)
			var saved AppTaskExecution
			require.NoError(t, db.First(&saved, execution.ID).Error)
			assert.Equal(t, "settled", saved.BillingState)
			assert.Equal(t, test.state, saved.ProviderState)
			assert.Equal(t, evidence.UsageJSON, saved.UsageJSON)
			for _, table := range []any{&AppTaskSettlement{}, &AppTaskOutbox{}} {
				var count int64
				require.NoError(t, db.Model(table).Count(&count).Error)
				assert.EqualValues(t, 1, count)
			}
			var projection Task
			require.NoError(t, db.Where("task_id = ?", execution.TaskID).First(&projection).Error)
			assert.Equal(t, "app_managed", projection.ExecutionMode)
			assert.EqualValues(t, test.quota, projection.Quota)
			assert.Equal(t, execution.UserID, projection.UserId)
			evidence.FinalQuota++
			err = RunAppPluginTransaction(db, func(tx *gorm.DB) error {
				_, err := FinalizeAppTaskTx(tx, lease, evidence, now)
				return err
			})
			require.ErrorContains(t, err, "terminal_evidence_conflict")
		})
	}
	t.Run("outbox failure rolls back every effect", func(t *testing.T) {
		db, execution, now := appTaskSettlementFixture(t)
		require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			return RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-rollback", "running", now)
		}))
		_, lease, won, err := ClaimAppTaskReconcile(t.Context(), db, "settler", now, time.Minute)
		require.NoError(t, err)
		require.True(t, won)
		injected := errors.New("outbox write rejected")
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("app-terminal-failure", func(tx *gorm.DB) {
			if tx.Statement.Table == "app_task_outboxes" {
				tx.AddError(injected)
			}
		}))
		t.Cleanup(func() { _ = db.Callback().Create().Remove("app-terminal-failure") })
		err = RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			_, err := FinalizeAppTaskTx(tx, lease, AppTaskTerminal{ProviderState: "succeeded", FinalQuota: 150}, now)
			return err
		})
		require.ErrorIs(t, err, injected)
		var payer User
		require.NoError(t, db.First(&payer, execution.UserID).Error)
		assert.Equal(t, 900, payer.Quota)
		assert.Zero(t, payer.UsedQuota)
		assert.Zero(t, payer.RequestCount)
		for _, table := range []any{&AppTaskSettlement{}, &AppTaskOutbox{}} {
			var count int64
			require.NoError(t, db.Model(table).Count(&count).Error)
			assert.Zero(t, count)
		}
		var saved AppTaskExecution
		require.NoError(t, db.First(&saved, execution.ID).Error)
		assert.Equal(t, "reserved", saved.BillingState)
		assert.Equal(t, "running", saved.ProviderState)
	})
	t.Run("insufficient adjustment retains evidence and reserve", func(t *testing.T) {
		db, execution, now := appTaskSettlementFixture(t)
		require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			return RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-pending", "running", now)
		}))
		_, lease, won, err := ClaimAppTaskReconcile(t.Context(), db, "settler", now, time.Minute)
		require.NoError(t, err)
		require.True(t, won)
		require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			settled, err := FinalizeAppTaskTx(tx, lease, AppTaskTerminal{
				ProviderState: "succeeded", FinalQuota: 1100, UsageJSON: `{"tokens":1100}`}, now)
			assert.False(t, settled)
			return err
		}))
		var saved AppTaskExecution
		require.NoError(t, db.First(&saved, execution.ID).Error)
		assert.Equal(t, "settlement_pending", saved.BillingState)
		assert.Equal(t, "succeeded", saved.ProviderState)
		assert.Equal(t, `{"tokens":1100}`, saved.UsageJSON)
		assert.EqualValues(t, 100, saved.ReservedQuota)
		var payer User
		require.NoError(t, db.First(&payer, execution.UserID).Error)
		assert.Equal(t, 900, payer.Quota)
		var count int64
		require.NoError(t, db.Model(&AppTaskSettlement{}).Count(&count).Error)
		assert.Zero(t, count)
		require.NoError(t, db.Model(&User{}).Where("id = ?", execution.UserID).
			Update("quota", gorm.Expr("quota + 200")).Error)
		_, retry, won, err := ClaimAppTaskReconcile(t.Context(), db, "retry", now.Add(time.Minute), time.Minute)
		require.NoError(t, err)
		require.True(t, won)
		require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			settled, err := FinalizeAppTaskTx(tx, retry, AppTaskTerminal{
				ProviderState: "succeeded", FinalQuota: 1100, UsageJSON: `{"tokens":1100}`}, now.Add(time.Minute))
			assert.True(t, settled)
			return err
		}))
		require.NoError(t, db.First(&payer, execution.UserID).Error)
		assert.Equal(t, 100, payer.Quota)
		assert.Equal(t, 1100, payer.UsedQuota)
		assert.Equal(t, 1, payer.RequestCount)
	})
	t.Run("channel removal cannot commit a partial settlement", func(t *testing.T) {
		db, execution, now := appTaskSettlementFixture(t)
		require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			return RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-channel-removal", "running", now)
		}))
		_, lease, won, err := ClaimAppTaskReconcile(t.Context(), db, "settler", now, time.Minute)
		require.NoError(t, err)
		require.True(t, won)
		require.NoError(t, db.Callback().Update().Before("gorm:update").Register("app-channel-removal", func(tx *gorm.DB) {
			if tx.Statement.Table != "channels" {
				return
			}
			source := db
			if db.Dialector.Name() == "sqlite" {
				// SQLite serializes writers. Exercise the missing-row branch
				// in this transaction; MySQL/PostgreSQL use another connection.
				source = tx
			}
			tx.AddError(source.Session(&gorm.Session{NewDB: true}).
				Where("id = ?", execution.ChannelID).Delete(&Channel{}).Error)
		}))
		t.Cleanup(func() { _ = db.Callback().Update().Remove("app-channel-removal") })
		err = RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			_, err := FinalizeAppTaskTx(tx, lease, AppTaskTerminal{ProviderState: "succeeded", FinalQuota: 150}, now)
			return err
		})
		require.Error(t, err, "channel removal cannot commit a partial settlement")
		var payer User
		require.NoError(t, db.First(&payer, execution.UserID).Error)
		assert.Equal(t, 900, payer.Quota)
		assert.Zero(t, payer.UsedQuota)
		assert.Zero(t, payer.RequestCount)
		var markers int64
		require.NoError(t, db.Model(&AppTaskSettlement{}).Count(&markers).Error)
		assert.Zero(t, markers)
	})
	t.Run("subscription uses its frozen funding period", func(t *testing.T) {
		for _, drift := range []bool{false, true} {
			t.Run(strconv.FormatBool(drift), func(t *testing.T) {
				db, execution, now := appTaskFundingFixture(t, "subscription")
				require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
					return RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-subscription", "running", now)
				}))
				// Expiry does not redirect an accepted charge to the wallet.
				later := now.Add(2 * time.Hour)
				require.NoError(t, db.Model(&UserSubscription{}).Where("id = ?", execution.SubscriptionID).
					Update("status", "expired").Error)
				if drift {
					require.NoError(t, db.Model(&UserSubscription{}).Where("id = ?", execution.SubscriptionID).
						Updates(map[string]any{"last_reset_time": later.Unix(), "amount_used": 0}).Error)
				}
				_, lease, won, err := ClaimAppTaskReconcile(t.Context(), db, "settler", later, time.Minute)
				require.NoError(t, err)
				require.True(t, won)
				require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
					settled, err := FinalizeAppTaskTx(tx, lease, AppTaskTerminal{ProviderState: "succeeded", FinalQuota: 150}, later)
					assert.Equal(t, !drift, settled)
					return err
				}))
				var payer User
				require.NoError(t, db.First(&payer, execution.UserID).Error)
				assert.Equal(t, 1000, payer.Quota)
				var sub UserSubscription
				require.NoError(t, db.First(&sub, execution.SubscriptionID).Error)
				if drift {
					assert.Zero(t, sub.AmountUsed, "a changed funding period cannot receive the old charge")
					assert.Zero(t, payer.UsedQuota)
				} else {
					assert.EqualValues(t, 150, sub.AmountUsed)
					assert.EqualValues(t, 150, payer.UsedQuota)
				}
			})
		}
	})
	t.Run("concurrent replay commits one financial fact", func(t *testing.T) {
		db, execution, now := appTaskSettlementFixture(t)
		require.NoError(t, RunAppPluginTransaction(db, func(tx *gorm.DB) error {
			return RecordAppTaskAcceptanceTx(tx, execution.ID, "cgt-concurrent", "running", now)
		}))
		_, lease, won, err := ClaimAppTaskReconcile(t.Context(), db, "settler", now, time.Minute)
		require.NoError(t, err)
		require.True(t, won)
		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for range 2 {
			workers.Go(func() {
				<-start
				results <- RunAppPluginTransaction(db, func(tx *gorm.DB) error {
					_, err := FinalizeAppTaskTx(tx, lease, AppTaskTerminal{ProviderState: "succeeded", FinalQuota: 150}, now)
					return err
				})
			})
		}
		close(start)
		workers.Wait()
		close(results)
		for err := range results {
			require.NoError(t, err)
		}
		var payer User
		require.NoError(t, db.First(&payer, execution.UserID).Error)
		assert.Equal(t, 850, payer.Quota)
		assert.Equal(t, 150, payer.UsedQuota)
		assert.Equal(t, 1, payer.RequestCount)
	})
}
