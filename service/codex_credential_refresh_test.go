package service

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRefreshCodexChannelCredentialRoutesThroughPlanQuotaAuthority(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Channel{},
		&model.PlanQuotaDomain{},
		&model.Ability{},
	))

	previousDB := model.DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	previousRefresh := refreshCodexOAuthTokenForCredential
	var channelID int
	const (
		concurrentUsedQuota          = int64(73)
		concurrentBalance            = 12.5
		concurrentBalanceUpdatedTime = int64(1_999_999_990)
		concurrentResponseTime       = 321
		concurrentTestTime           = int64(1_999_999_995)
	)
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, previousLogType)
	common.MemoryCacheEnabled = true
	refreshCodexOAuthTokenForCredential = func(
		context.Context,
		string,
		string,
	) (*CodexOAuthTokenResult, error) {
		if updateErr := db.Model(&model.Channel{}).
			Where("id = ?", channelID).
			Updates(map[string]any{
				"used_quota":           concurrentUsedQuota,
				"balance":              concurrentBalance,
				"balance_updated_time": concurrentBalanceUpdatedTime,
				"response_time":        concurrentResponseTime,
				"test_time":            concurrentTestTime,
			}).Error; updateErr != nil {
			return nil, updateErr
		}
		return &CodexOAuthTokenResult{
			AccessToken:  "new-access-token",
			RefreshToken: "new-refresh-token",
			ExpiresAt:    time.Unix(2_000_000_000, 0).UTC(),
		}, nil
	}
	t.Cleanup(func() {
		refreshCodexOAuthTokenForCredential = previousRefresh
		model.DB = previousDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	tag := "plan:test:codex-refresh"
	oldCredential := `{"access_token":"old-access-token","refresh_token":"old-refresh-token","account_id":"account"}`
	oldHash, ok := model.PlanQuotaDomainHash(oldCredential)
	require.True(t, ok)
	require.NoError(t, db.Create(&model.PlanQuotaDomain{
		CredentialHash: oldHash,
		Generation:     7,
		State:          model.PlanQuotaDomainStateDisabled,
		DisabledUntil:  2_000_000_000,
	}).Error)
	channel := model.Channel{
		Type: constant.ChannelTypeCodex, Name: "codex",
		Key: oldCredential, Tag: &tag, Status: common.ChannelStatusAutoDisabled,
		Models: "gpt-5", Group: "default",
	}
	channel.SetOtherInfo(map[string]any{
		"disabled_until":   int64(2_000_000_000),
		"quota_domain":     tag,
		"quota_domain_id":  oldHash,
		"quota_generation": "7",
		"quota_type":       "plan",
		"status_reason":    "old Codex quota exhausted",
		"status_time":      int64(12345),
	})
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(db))
	channelID = channel.Id
	model.InitChannelCache()

	refreshed, updated, err := RefreshCodexChannelCredential(
		context.Background(),
		channel.Id,
		CodexCredentialRefreshOptions{},
	)
	require.NoError(t, err)
	require.NotNil(t, refreshed)
	require.NotNil(t, updated)
	assert.Equal(t, "new-refresh-token", refreshed.RefreshToken)
	assert.NotEqual(t, oldCredential, updated.Key)
	assert.Equal(t, concurrentUsedQuota, updated.UsedQuota)
	assert.Equal(t, concurrentBalance, updated.Balance)
	assert.Equal(t, concurrentBalanceUpdatedTime, updated.BalanceUpdatedTime)
	assert.Equal(t, concurrentResponseTime, updated.ResponseTime)
	assert.Equal(t, concurrentTestTime, updated.TestTime)

	newHash, ok := model.PlanQuotaDomainHash(updated.Key)
	require.True(t, ok)
	var authority model.PlanQuotaDomain
	require.NoError(t, db.First(&authority, "credential_hash = ?", newHash).Error)
	assert.Equal(t, model.PlanQuotaDomainStateActive, authority.State)
	assert.Equal(t, common.ChannelStatusAutoDisabled, updated.Status)
	info := updated.GetOtherInfo()
	assert.Equal(t, "old Codex quota exhausted", info["status_reason"])
	assert.EqualValues(t, 12345, info["status_time"])
	assert.NotContains(t, info, "disabled_until")
	assert.NotContains(t, info, "quota_domain")
	assert.NotContains(t, info, "quota_domain_id")
	assert.NotContains(t, info, "quota_generation")
	assert.NotContains(t, info, "quota_type")
	var ability model.Ability
	require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
	var stored model.Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Equal(t, updated.Key, stored.Key)
	assert.Equal(t, concurrentUsedQuota, stored.UsedQuota)
	assert.Equal(t, concurrentBalance, stored.Balance)
	assert.Equal(t, concurrentBalanceUpdatedTime, stored.BalanceUpdatedTime)
	assert.Equal(t, concurrentResponseTime, stored.ResponseTime)
	assert.Equal(t, concurrentTestTime, stored.TestTime)
	cached, cacheErr := model.CacheGetChannel(channel.Id)
	require.NoError(t, cacheErr)
	assert.Equal(t, stored, *cached)
}
