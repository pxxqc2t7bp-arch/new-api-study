package model

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func createSingleKeyChannelStatusCASFixture(t *testing.T, otherInfo map[string]any) Channel {
	t.Helper()
	setupChannelStatusTest(t)

	channel := Channel{
		Name:   "single-key-status-cas",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-3.5-turbo",
		Group:  "default",
	}
	channel.SetOtherInfo(otherInfo)
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	return channel
}

func loadChannelStatusCASFixture(t *testing.T, channelId int) (Channel, Ability) {
	t.Helper()
	var channel Channel
	require.NoError(t, DB.First(&channel, "id = ?", channelId).Error)
	var ability Ability
	require.NoError(t, DB.First(&ability, "channel_id = ?", channelId).Error)
	return channel, ability
}

func TestUpdateSingleKeyChannelStatusIfUnchangedUpdatesChannelAndAbility(t *testing.T) {
	channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
		"owner": "snapshot",
	})
	expectedOtherInfo := channel.OtherInfo
	channel.SetOtherInfo(map[string]any{
		"owner":           "snapshot",
		"quota_domain_id": "domain-a",
		"quota_type":      "plan",
	})
	desiredOtherInfo := channel.OtherInfo
	common.MemoryCacheEnabled = true
	InitChannelCache()

	changed, err := UpdateSingleKeyChannelStatusIfUnchanged(
		channel.Id,
		channel.Key,
		channel.GetTag(),
		common.ChannelStatusEnabled,
		expectedOtherInfo,
		common.ChannelStatusAutoDisabled,
		desiredOtherInfo,
	)
	require.NoError(t, err)
	require.True(t, changed)

	stored, ability := loadChannelStatusCASFixture(t, channel.Id)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	assert.Equal(t, desiredOtherInfo, stored.OtherInfo)
	assert.False(t, ability.Enabled)
	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.Status)
	assert.Equal(t, desiredOtherInfo, cached.OtherInfo)
}

func TestUpdateSingleKeyChannelStatusIfUnchangedPreservesNewerCacheFields(t *testing.T) {
	channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
		"owner": "snapshot",
	})
	expectedOtherInfo := channel.OtherInfo
	desired := channel
	desired.SetOtherInfo(map[string]any{
		"owner":           "snapshot",
		"quota_domain_id": "domain-a",
		"quota_type":      "plan",
	})
	common.MemoryCacheEnabled = true
	InitChannelCache()

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	newerCache := *cached
	newerCache.Models = "cache-owned-model"
	CacheUpdateChannel(&newerCache)

	changed, err := UpdateSingleKeyChannelStatusIfUnchanged(
		channel.Id,
		channel.Key,
		channel.GetTag(),
		common.ChannelStatusEnabled,
		expectedOtherInfo,
		common.ChannelStatusAutoDisabled,
		desired.OtherInfo,
	)
	require.NoError(t, err)
	require.True(t, changed)

	cached, err = CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, "cache-owned-model", cached.Models)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.Status)
	assert.Equal(t, desired.OtherInfo, cached.OtherInfo)
}

func TestUpdateSingleKeyChannelStatusIfUnchangedRejectsStaleSnapshot(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
			"owner": "snapshot",
		})
		expectedOtherInfo := channel.OtherInfo
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
			Update("status", common.ChannelStatusManuallyDisabled).Error)
		require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", channel.Id).
			Update("enabled", false).Error)

		changed, err := UpdateSingleKeyChannelStatusIfUnchanged(
			channel.Id,
			channel.Key,
			channel.GetTag(),
			common.ChannelStatusEnabled,
			expectedOtherInfo,
			common.ChannelStatusAutoDisabled,
			`{"owner":"quota"}`,
		)
		require.NoError(t, err)
		assert.False(t, changed)

		stored, ability := loadChannelStatusCASFixture(t, channel.Id)
		assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status)
		assert.Equal(t, expectedOtherInfo, stored.OtherInfo)
		assert.False(t, ability.Enabled)
	})

	t.Run("other_info", func(t *testing.T) {
		channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
			"owner": "snapshot",
		})
		expectedOtherInfo := channel.OtherInfo
		concurrent := channel
		concurrent.SetOtherInfo(map[string]any{
			"owner": "concurrent",
		})
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
			Update("other_info", concurrent.OtherInfo).Error)

		changed, err := UpdateSingleKeyChannelStatusIfUnchanged(
			channel.Id,
			channel.Key,
			channel.GetTag(),
			common.ChannelStatusEnabled,
			expectedOtherInfo,
			common.ChannelStatusAutoDisabled,
			`{"owner":"quota"}`,
		)
		require.NoError(t, err)
		assert.False(t, changed)

		stored, ability := loadChannelStatusCASFixture(t, channel.Id)
		assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
		assert.Equal(t, concurrent.OtherInfo, stored.OtherInfo)
		assert.True(t, ability.Enabled)
	})

	t.Run("key", func(t *testing.T) {
		channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
			"owner": "snapshot",
		})
		expectedOtherInfo := channel.OtherInfo
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
			Update("key", "rotated-credential").Error)

		changed, err := UpdateSingleKeyChannelStatusIfUnchanged(
			channel.Id,
			channel.Key,
			channel.GetTag(),
			common.ChannelStatusEnabled,
			expectedOtherInfo,
			common.ChannelStatusAutoDisabled,
			`{"owner":"quota"}`,
		)
		require.NoError(t, err)
		assert.False(t, changed)

		stored, ability := loadChannelStatusCASFixture(t, channel.Id)
		assert.Equal(t, "rotated-credential", stored.Key)
		assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
		assert.Equal(t, expectedOtherInfo, stored.OtherInfo)
		assert.True(t, ability.Enabled)
	})

	t.Run("tag", func(t *testing.T) {
		channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
			"owner": "snapshot",
		})
		channel.SetTag("plan:original")
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
			Update("tag", channel.Tag).Error)
		expectedOtherInfo := channel.OtherInfo
		rotatedTag := "plan:rotated"
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
			Update("tag", rotatedTag).Error)

		changed, err := UpdateSingleKeyChannelStatusIfUnchanged(
			channel.Id,
			channel.Key,
			channel.GetTag(),
			common.ChannelStatusEnabled,
			expectedOtherInfo,
			common.ChannelStatusAutoDisabled,
			`{"owner":"quota"}`,
		)
		require.NoError(t, err)
		assert.False(t, changed)

		stored, ability := loadChannelStatusCASFixture(t, channel.Id)
		assert.Equal(t, rotatedTag, stored.GetTag())
		assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
		assert.Equal(t, expectedOtherInfo, stored.OtherInfo)
		assert.True(t, ability.Enabled)
	})
}

func TestUpdateSingleKeyChannelStatusIfUnchangedRollsBackAbilityFailure(t *testing.T) {
	channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
		"owner": "snapshot",
	})
	expectedOtherInfo := channel.OtherInfo

	forcedErr := errors.New("forced ability failure")
	const callbackName = "test:fail_channel_status_cas_ability"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "abilities" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, DB.Callback().Update().Remove(callbackName))
	})
	common.MemoryCacheEnabled = true
	InitChannelCache()

	changed, err := UpdateSingleKeyChannelStatusIfUnchanged(
		channel.Id,
		channel.Key,
		channel.GetTag(),
		common.ChannelStatusEnabled,
		expectedOtherInfo,
		common.ChannelStatusAutoDisabled,
		`{"owner":"quota"}`,
	)
	require.ErrorIs(t, err, forcedErr)
	assert.False(t, changed)

	stored, ability := loadChannelStatusCASFixture(t, channel.Id)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, expectedOtherInfo, stored.OtherInfo)
	assert.True(t, ability.Enabled)
	cached, cacheErr := CacheGetChannel(channel.Id)
	require.NoError(t, cacheErr)
	assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
	assert.Equal(t, expectedOtherInfo, cached.OtherInfo)
}

func TestUpdateSingleKeyChannelStatusesIfUnchangedQuotesKeyWithoutInitializedColumns(t *testing.T) {
	channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
		"owner": "snapshot",
	})
	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	desired := *expected
	desired.SetOtherInfo(map[string]any{
		"owner":           "snapshot",
		"quota_domain_id": "domain-a",
		"quota_type":      "plan",
	})

	previousCommonKeyCol := commonKeyCol
	commonKeyCol = ""
	t.Cleanup(func() {
		commonKeyCol = previousCommonKeyCol
	})

	changed, err := UpdateSingleKeyChannelStatusesIfUnchanged([]SingleKeyChannelStatusUpdate{{
		Expected:  expected,
		Status:    common.ChannelStatusAutoDisabled,
		OtherInfo: desired.OtherInfo,
	}})
	require.NoError(t, err)
	require.True(t, changed)

	stored, ability := loadChannelStatusCASFixture(t, channel.Id)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	assert.Equal(t, desired.OtherInfo, stored.OtherInfo)
	assert.False(t, ability.Enabled)
}

func TestUpdateSingleKeyChannelStatusesIfUnchangedPreservesNewerCacheFields(t *testing.T) {
	setupChannelStatusTest(t)
	databasePriorities := []int64{10, 20}
	databaseBaseURLs := []string{
		"https://database-a.example.com",
		"https://database-b.example.com",
	}
	channels := []Channel{
		{
			Name:     "batch-cache-a",
			Key:      "credential-a",
			Status:   common.ChannelStatusEnabled,
			Models:   "database-model-a",
			Group:    "database-group",
			Priority: &databasePriorities[0],
			BaseURL:  &databaseBaseURLs[0],
		},
		{
			Name:     "batch-cache-b",
			Key:      "credential-b",
			Status:   common.ChannelStatusEnabled,
			Models:   "database-model-b",
			Group:    "database-group",
			Priority: &databasePriorities[1],
			BaseURL:  &databaseBaseURLs[1],
		},
	}
	for index := range channels {
		channels[index].SetOtherInfo(map[string]any{"owner": "snapshot"})
	}
	require.NoError(t, DB.Create(&channels).Error)
	for index := range channels {
		require.NoError(t, channels[index].AddAbilities(nil))
	}

	expected := make([]*Channel, len(channels))
	for index := range channels {
		var err error
		expected[index], err = GetChannelById(channels[index].Id, true)
		require.NoError(t, err)
	}
	common.MemoryCacheEnabled = true
	InitChannelCache()

	cachePriorities := []int64{100, 200}
	cacheBaseURLs := []string{
		"https://cache-a.example.com",
		"https://cache-b.example.com",
	}
	newerCache := make([]*Channel, len(channels))
	for index := range channels {
		cached, err := CacheGetChannel(channels[index].Id)
		require.NoError(t, err)
		updated := *cached
		updated.Key = fmt.Sprintf("cache-owned-key-%d", index)
		updated.Models = "cache-owned-model"
		updated.Group = "cache-owned-group"
		updated.Priority = &cachePriorities[index]
		updated.BaseURL = &cacheBaseURLs[index]
		updated.Status = common.ChannelStatusAutoDisabled
		newerCache[index] = &updated
	}
	CacheUpdateChannels(newerCache)

	updates := make([]SingleKeyChannelStatusUpdate, len(channels))
	for index := range channels {
		desired := *expected[index]
		desired.SetOtherInfo(map[string]any{
			"owner":           fmt.Sprintf("status-publisher-%d", index),
			"quota_domain_id": "domain-a",
			"quota_type":      "plan",
		})
		updates[index] = SingleKeyChannelStatusUpdate{
			Expected:  expected[index],
			Status:    common.ChannelStatusEnabled,
			OtherInfo: desired.OtherInfo,
		}
	}

	changed, err := UpdateSingleKeyChannelStatusesIfUnchanged(updates)
	require.NoError(t, err)
	require.True(t, changed)

	for index := range channels {
		cached, err := CacheGetChannel(channels[index].Id)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("cache-owned-key-%d", index), cached.Key)
		assert.Equal(t, "cache-owned-model", cached.Models)
		assert.Equal(t, "cache-owned-group", cached.Group)
		assert.Equal(t, cachePriorities[index], cached.GetPriority())
		assert.Equal(t, cacheBaseURLs[index], cached.GetBaseURL())
		assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
		assert.Equal(t, updates[index].OtherInfo, cached.OtherInfo)
	}

	channelSyncLock.RLock()
	routingIDs := append([]int(nil), group2model2channels["cache-owned-group"]["cache-owned-model"]...)
	oldRoutingIDs := make([]int, 0, len(channels))
	for index := range channels {
		oldRoutingIDs = append(
			oldRoutingIDs,
			group2model2channels["database-group"][channels[index].Models]...,
		)
	}
	channelSyncLock.RUnlock()
	assert.Equal(t, []int{channels[1].Id, channels[0].Id}, routingIDs)
	assert.Empty(t, oldRoutingIDs)

	priorities, err := ListSatisfiedChannelPriorities("cache-owned-group", "cache-owned-model", nil)
	require.NoError(t, err)
	assert.Equal(t, []int64{cachePriorities[1], cachePriorities[0]}, priorities)
}

func TestUpdateMultiKeyChannelStatusIfUnchangedFencesSnapshot(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, channel Channel)
	}{
		{
			name: "status metadata",
			mutate: func(t *testing.T, channel Channel) {
				t.Helper()
				concurrent := channel
				concurrent.SetOtherInfo(map[string]any{"owner": "concurrent"})
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
					Update("other_info", concurrent.OtherInfo).Error)
			},
		},
		{
			name: "channel info",
			mutate: func(t *testing.T, channel Channel) {
				t.Helper()
				concurrentInfo := channel.ChannelInfo
				concurrentInfo.MultiKeyPollingIndex = 1
				concurrentInfo.MultiKeyStatusList = map[int]int{
					1: common.ChannelStatusManuallyDisabled,
				}
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
					Update("channel_info", concurrentInfo).Error)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			setupChannelStatusTest(t)
			tag := "plan:managed:snapshot"
			channel := Channel{
				Name: "multi-key-snapshot", Key: "key-a\nkey-b",
				Status: common.ChannelStatusEnabled, Tag: &tag,
				Models: "gpt-3.5-turbo", Group: "default",
				ChannelInfo: ChannelInfo{
					IsMultiKey:   true,
					MultiKeySize: 2,
				},
			}
			channel.SetOtherInfo(map[string]any{"owner": "request"})
			require.NoError(t, DB.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			expected, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)
			testCase.mutate(t, channel)

			changed, err := UpdateMultiKeyChannelStatusIfUnchanged(
				expected,
				tag,
				"key-a",
				common.ChannelStatusAutoDisabled,
				"quota exhausted",
				MultiKeyChannelStatusUpdateOptions{},
			)
			require.NoError(t, err)
			assert.False(t, changed)

			stored, ability := loadChannelStatusCASFixture(t, channel.Id)
			assert.NotContains(t, stored.ChannelInfo.MultiKeyStatusList, 0)
			assert.True(t, ability.Enabled)
		})
	}
}

func TestUpdateMultiKeyChannelStatusIfUnchangedPreservesInMemoryPollingCursor(t *testing.T) {
	setupChannelStatusTest(t)
	tag := "plan:managed:polling-cache"
	channel := Channel{
		Name:   "multi-key-polling-cache",
		Key:    "key-a\nkey-b",
		Status: common.ChannelStatusEnabled,
		Tag:    &tag,
		Models: "gpt-3.5-turbo",
		Group:  "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
			MultiKeyMode: constant.MultiKeyModePolling,
		},
	}
	channel.SetOtherInfo(map[string]any{"owner": "preserved"})
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	common.MemoryCacheEnabled = true
	InitChannelCache()

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	usingKey, keyIndex, keyErr := cached.GetNextEnabledKey()
	require.Nil(t, keyErr)
	require.Equal(t, "key-a", usingKey)
	require.Equal(t, 0, keyIndex)
	require.Equal(t, 1, cached.ChannelInfo.MultiKeyPollingIndex)

	changed, err := UpdateMultiKeyChannelStatusIfUnchanged(
		expected,
		tag,
		usingKey,
		common.ChannelStatusAutoDisabled,
		"quota exhausted",
		MultiKeyChannelStatusUpdateOptions{},
	)
	require.NoError(t, err)
	require.True(t, changed)

	cached, err = CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, 1, cached.ChannelInfo.MultiKeyPollingIndex)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, "quota exhausted", cached.ChannelInfo.MultiKeyDisabledReason[0])
}

func TestAdvanceMultiKeyRecoveryCursorIfUnchangedFencesSnapshot(t *testing.T) {
	t.Run("advances only recovery cursor", func(t *testing.T) {
		setupChannelStatusTest(t)
		tag := "plan:recovery:cursor"
		channel := Channel{
			Name: "multi-key-recovery-cursor", Key: "key-a\nkey-b",
			Status: common.ChannelStatusAutoDisabled, Tag: &tag,
			Models: "gpt-3.5-turbo", Group: "default",
			ChannelInfo: ChannelInfo{
				IsMultiKey:             true,
				MultiKeySize:           2,
				MultiKeyPollingIndex:   1,
				MultiKeyRecoveryIndex:  0,
				MultiKeyStatusList:     map[int]int{0: common.ChannelStatusAutoDisabled, 1: common.ChannelStatusAutoDisabled},
				MultiKeyDisabledTime:   map[int]int64{0: 100, 1: 200},
				MultiKeyDisabledReason: map[int]string{0: "first", 1: "second"},
			},
		}
		channel.SetOtherInfo(map[string]any{"owner": "preserved"})
		require.NoError(t, DB.Create(&channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
		expected, err := GetChannelById(channel.Id, true)
		require.NoError(t, err)

		changed, err := AdvanceMultiKeyRecoveryCursorIfUnchanged(expected, "key-a")

		require.NoError(t, err)
		require.True(t, changed)
		stored, ability := loadChannelStatusCASFixture(t, channel.Id)
		assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
		assert.Equal(t, channel.OtherInfo, stored.OtherInfo)
		assert.Equal(t, 1, stored.ChannelInfo.MultiKeyPollingIndex)
		assert.Equal(t, 1, stored.ChannelInfo.MultiKeyRecoveryIndex)
		assert.Equal(t, channel.ChannelInfo.MultiKeyStatusList, stored.ChannelInfo.MultiKeyStatusList)
		assert.False(t, ability.Enabled)
	})

	t.Run("rejects stale channel info", func(t *testing.T) {
		setupChannelStatusTest(t)
		channel := Channel{
			Name: "multi-key-recovery-stale", Key: "key-a\nkey-b",
			Status: common.ChannelStatusAutoDisabled,
			Models: "gpt-3.5-turbo", Group: "default",
			ChannelInfo: ChannelInfo{
				IsMultiKey:           true,
				MultiKeySize:         2,
				MultiKeyStatusList:   map[int]int{0: common.ChannelStatusAutoDisabled, 1: common.ChannelStatusAutoDisabled},
				MultiKeyDisabledTime: map[int]int64{0: 100, 1: 200},
			},
		}
		require.NoError(t, DB.Create(&channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
		expected, err := GetChannelById(channel.Id, true)
		require.NoError(t, err)
		concurrentInfo := expected.ChannelInfo
		concurrentInfo.MultiKeyRecoveryIndex = 1
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
			Update("channel_info", concurrentInfo).Error)

		changed, err := AdvanceMultiKeyRecoveryCursorIfUnchanged(expected, "key-a")

		require.NoError(t, err)
		assert.False(t, changed)
		stored, ability := loadChannelStatusCASFixture(t, channel.Id)
		assert.Equal(t, 1, stored.ChannelInfo.MultiKeyRecoveryIndex)
		assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
		assert.False(t, ability.Enabled)
	})
}

func TestAdvanceMultiKeyRecoveryCursorIfUnchangedPreservesInMemoryPollingCursor(t *testing.T) {
	setupChannelStatusTest(t)
	channel := Channel{
		Name:   "multi-key-recovery-polling-cache",
		Key:    "key-a\nkey-b",
		Status: common.ChannelStatusAutoDisabled,
		Models: "gpt-3.5-turbo",
		Group:  "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:            true,
			MultiKeySize:          2,
			MultiKeyRecoveryIndex: 0,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusAutoDisabled,
			},
			MultiKeyDisabledTime: map[int]int64{0: 100, 1: 200},
		},
	}
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	common.MemoryCacheEnabled = true
	InitChannelCache()

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	pollingLock := GetChannelPollingLock(channel.Id)
	pollingLock.Lock()
	cached.ChannelInfo.MultiKeyPollingIndex = 1
	pollingLock.Unlock()

	changed, err := AdvanceMultiKeyRecoveryCursorIfUnchanged(expected, "key-a")
	require.NoError(t, err)
	require.True(t, changed)

	cached, err = CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, 1, cached.ChannelInfo.MultiKeyPollingIndex)
	assert.Equal(t, 1, cached.ChannelInfo.MultiKeyRecoveryIndex)
}

func TestUpdateMultiKeyChannelStatusIfUnchangedRollsBackPlanDeadlineWithAbilityFailure(t *testing.T) {
	setupChannelStatusTest(t)
	tag := "plan:managed:deadline-rollback"
	channel := Channel{
		Name:   "multi-key-deadline-rollback",
		Key:    "key-a\nkey-b",
		Status: common.ChannelStatusEnabled,
		Tag:    &tag,
		Models: "gpt-3.5-turbo",
		Group:  "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
			},
		},
	}
	channel.SetOtherInfo(map[string]any{"owner": "preserved"})
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)

	forcedErr := errors.New("forced ability failure")
	const callbackName = "test:fail_multi_key_plan_deadline_ability"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "abilities" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, DB.Callback().Update().Remove(callbackName))
	})

	changed, err := UpdateMultiKeyChannelStatusIfUnchanged(
		expected,
		tag,
		"key-b",
		common.ChannelStatusAutoDisabled,
		"quota exhausted",
		MultiKeyChannelStatusUpdateOptions{PlanQuotaResetAt: 2_000_000_000},
	)
	require.ErrorIs(t, err, forcedErr)
	assert.False(t, changed)

	stored, ability := loadChannelStatusCASFixture(t, channel.Id)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.NotContains(t, stored.ChannelInfo.MultiKeyStatusList, 1)
	assert.Equal(t, "preserved", stored.GetOtherInfo()["owner"])
	assert.NotContains(t, stored.GetOtherInfo(), "quota_reset_at")
	assert.NotContains(t, stored.GetOtherInfo(), "disabled_until")
	assert.True(t, ability.Enabled)
}

func TestUpdateMultiKeyChannelStatusIfUnchangedSynchronizesMemoryRouting(t *testing.T) {
	setupChannelStatusTest(t)
	tag := "plan:managed:memory-routing"
	const resetAt int64 = 2_000_000_000
	channel := Channel{
		Name:   "multi-key-memory-routing",
		Key:    "key-a\nkey-b",
		Status: common.ChannelStatusEnabled,
		Tag:    &tag,
		Models: "gpt-3.5-turbo",
		Group:  "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         2,
			MultiKeyPollingIndex: 1,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
			},
		},
	}
	channel.SetOtherInfo(map[string]any{"owner": "preserved"})
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	common.MemoryCacheEnabled = true
	InitChannelCache()

	selected, err := GetRandomSatisfiedChannel("default", channel.Models, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, channel.Id, selected.Id)

	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	changed, err := UpdateMultiKeyChannelStatusIfUnchanged(
		expected,
		tag,
		"key-b",
		common.ChannelStatusAutoDisabled,
		"quota exhausted",
		MultiKeyChannelStatusUpdateOptions{PlanQuotaResetAt: resetAt},
	)
	require.NoError(t, err)
	require.True(t, changed)

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.ChannelInfo.MultiKeyStatusList[1])
	assert.Equal(t, 1, cached.ChannelInfo.MultiKeyPollingIndex)
	assert.Equal(t, float64(resetAt), cached.GetOtherInfo()["quota_reset_at"])
	assert.Equal(t, resetAt+60, cached.GetDisabledUntil())
	assert.Equal(t, "preserved", cached.GetOtherInfo()["owner"])
	assert.NotContains(t, cached.GetOtherInfo(), "quota_domain_id")
	selected, err = GetRandomSatisfiedChannel("default", channel.Models, 0, nil)
	require.NoError(t, err)
	assert.Nil(t, selected)

	expected, err = GetChannelById(channel.Id, true)
	require.NoError(t, err)
	changed, err = UpdateMultiKeyChannelStatusIfUnchanged(
		expected,
		tag,
		"key-b",
		common.ChannelStatusEnabled,
		"",
		MultiKeyChannelStatusUpdateOptions{ClearPlanQuotaDeadline: true},
	)
	require.NoError(t, err)
	require.True(t, changed)

	selected, err = GetRandomSatisfiedChannel("default", channel.Models, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, channel.Id, selected.Id)
	assert.Equal(t, common.ChannelStatusAutoDisabled, selected.ChannelInfo.MultiKeyStatusList[0])
	assert.NotContains(t, selected.ChannelInfo.MultiKeyStatusList, 1)
	assert.Equal(t, 1, selected.ChannelInfo.MultiKeyPollingIndex)
	assert.Equal(t, "preserved", selected.GetOtherInfo()["owner"])
	assert.NotContains(t, selected.GetOtherInfo(), "quota_reset_at")
	assert.NotContains(t, selected.GetOtherInfo(), "disabled_until")
}

func TestCacheUpdateChannelStatusRebuildsOrderedRoutingMembership(t *testing.T) {
	setupChannelStatusTest(t)
	highPriority := int64(200)
	lowPriority := int64(100)
	channels := []Channel{
		{
			Name: "high-priority", Key: "key-a",
			Status: common.ChannelStatusEnabled, Models: "gpt-cache-status",
			Group: "default", Priority: &highPriority,
		},
		{
			Name: "low-priority", Key: "key-b",
			Status: common.ChannelStatusEnabled, Models: "gpt-cache-status",
			Group: "default", Priority: &lowPriority,
		},
	}
	require.NoError(t, DB.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}
	common.MemoryCacheEnabled = true
	InitChannelCache()

	CacheUpdateChannelStatus(channels[0].Id, common.ChannelStatusAutoDisabled)
	CacheUpdateChannelStatus(channels[0].Id, common.ChannelStatusEnabled)
	CacheUpdateChannelStatus(channels[0].Id, common.ChannelStatusEnabled)

	channelSyncLock.RLock()
	routingIDs := append([]int(nil), group2model2channels["default"]["gpt-cache-status"]...)
	channelSyncLock.RUnlock()
	assert.Equal(t, []int{channels[0].Id, channels[1].Id}, routingIDs)
}

func TestCacheUpdateChannelStatusSnapshotsPublishesRoutingDomainAtomically(t *testing.T) {
	setupChannelStatusTest(t)
	const (
		channelCount = 64
		readerCount  = 16
		modelName    = "gpt-cache-batch-atomic"
	)

	channels := make([]Channel, channelCount)
	for index := range channels {
		channels[index] = Channel{
			Name:   fmt.Sprintf("batch-atomic-%d", index),
			Key:    "shared-credential",
			Status: common.ChannelStatusEnabled,
			Models: modelName,
			Group:  "default",
		}
		channels[index].SetOtherInfo(map[string]any{"generation": "old"})
	}
	require.NoError(t, DB.Create(&channels).Error)
	for index := range channels {
		require.NoError(t, channels[index].AddAbilities(nil))
	}
	common.MemoryCacheEnabled = true
	InitChannelCache()

	updates := make([]ChannelStatusCacheUpdate, channelCount)
	channelSyncLock.RLock()
	for index := range channels {
		updated := *channelsIDM[channels[index].Id]
		updated.Status = common.ChannelStatusAutoDisabled
		updated.SetOtherInfo(map[string]any{"generation": "new"})
		updates[index].Snapshot = &updated
	}
	channelSyncLock.RUnlock()

	start := make(chan struct{})
	ready := make(chan struct{}, readerCount)
	observed := make(chan struct{}, readerCount)
	stop := make(chan struct{})
	partial := make(chan string, 1)
	var readers sync.WaitGroup
	for range readerCount {
		readers.Add(1)
		go func() {
			defer readers.Done()
			ready <- struct{}{}
			<-start
			firstObservation := true
			for {
				select {
				case <-stop:
					return
				default:
				}

				channelSyncLock.RLock()
				oldCount := 0
				newCount := 0
				for _, channel := range channels {
					cached := channelsIDM[channel.Id]
					switch {
					case cached.Status == common.ChannelStatusEnabled &&
						cached.GetOtherInfo()["generation"] == "old":
						oldCount++
					case cached.Status == common.ChannelStatusAutoDisabled &&
						cached.GetOtherInfo()["generation"] == "new":
						newCount++
					}
				}
				routingCount := len(group2model2channels["default"][modelName])
				channelSyncLock.RUnlock()

				if firstObservation {
					observed <- struct{}{}
					firstObservation = false
				}
				oldState := oldCount == channelCount && newCount == 0 && routingCount == channelCount
				newState := oldCount == 0 && newCount == channelCount && routingCount == 0
				if !oldState && !newState {
					select {
					case partial <- fmt.Sprintf(
						"old=%d new=%d routed=%d",
						oldCount, newCount, routingCount,
					):
					default:
					}
					return
				}
			}
		}()
	}
	for range readerCount {
		<-ready
	}
	close(start)
	for range readerCount {
		<-observed
	}

	CacheUpdateChannelStatusSnapshots(updates)
	close(stop)
	readers.Wait()

	select {
	case snapshot := <-partial:
		t.Fatalf("observed partially published cache state: %s", snapshot)
	default:
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	for _, channel := range channels {
		cached := channelsIDM[channel.Id]
		assert.Equal(t, common.ChannelStatusAutoDisabled, cached.Status)
		assert.Equal(t, "new", cached.GetOtherInfo()["generation"])
	}
	assert.Empty(t, group2model2channels["default"][modelName])
}

func TestCacheUpdateChannelStatusSnapshotsInitializesMissingMultiKeyEntry(t *testing.T) {
	setupChannelStatusTest(t)
	priority := int64(123)
	baseURL := "https://cache-miss.example.com"
	channel := Channel{
		Name:     "multi-key-cache-miss",
		Key:      "key-a\nkey-b",
		Status:   common.ChannelStatusEnabled,
		Models:   "gpt-cache-miss",
		Group:    "cache-miss-group",
		Priority: &priority,
		BaseURL:  &baseURL,
		ChannelInfo: ChannelInfo{
			IsMultiKey:             true,
			MultiKeySize:           2,
			MultiKeyStatusList:     map[int]int{0: common.ChannelStatusAutoDisabled},
			MultiKeyDisabledReason: map[int]string{0: "quota exhausted"},
			MultiKeyDisabledTime:   map[int]int64{0: 1_788_320_000},
			MultiKeyRecoveryIndex:  1,
		},
	}
	channel.SetOtherInfo(map[string]any{
		"owner":           "committed-snapshot",
		"quota_domain_id": "domain-a",
	})
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	snapshot, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.Empty(t, snapshot.Keys)

	common.MemoryCacheEnabled = true
	InitChannelCache()
	channelSyncLock.Lock()
	delete(channelsIDM, channel.Id)
	missing := *snapshot
	missing.Status = common.ChannelStatusAutoDisabled
	syncChannelRoutingIndexLocked(&missing)
	channelSyncLock.Unlock()

	CacheUpdateChannelStatusSnapshots([]ChannelStatusCacheUpdate{{
		Snapshot:          snapshot,
		UpdateChannelInfo: true,
	}})

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, snapshot.Key, cached.Key)
	assert.Equal(t, []string{"key-a", "key-b"}, cached.Keys)
	assert.Equal(t, snapshot.Status, cached.Status)
	assert.Equal(t, snapshot.OtherInfo, cached.OtherInfo)
	assert.Equal(t, snapshot.ChannelInfo, cached.ChannelInfo)
	assert.Equal(t, snapshot.Models, cached.Models)
	assert.Equal(t, snapshot.Group, cached.Group)
	assert.Equal(t, snapshot.GetPriority(), cached.GetPriority())
	assert.Equal(t, snapshot.GetBaseURL(), cached.GetBaseURL())

	routingIDs, err := ListSatisfiedChannelIDsAtPriority(
		snapshot.Group,
		snapshot.Models,
		snapshot.GetPriority(),
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, []int{snapshot.Id}, routingIDs)
}

func TestUpdateManagedChannelIfUnchangedPreservesConcurrentStatusOwner(t *testing.T) {
	channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
		"owner": "before",
	})
	require.NoError(t, DB.AutoMigrate(&UpstreamManagedRoute{}))
	require.NoError(t, DB.Exec("DELETE FROM upstream_managed_routes").Error)
	route := UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "group",
		Platform:        "openai",
		Protocol:        UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           UpstreamRouteStateActive,
		Rank:            7,
	}
	require.NoError(t, DB.Create(&route).Error)

	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	concurrent := *expected
	concurrent.Status = common.ChannelStatusAutoDisabled
	concurrent.SetOtherInfo(map[string]any{
		"owner":            "plan-quota",
		"quota_domain_id":  "domain-a",
		"quota_generation": "generation-a",
		"quota_type":       "plan",
	})
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
		"status":     concurrent.Status,
		"other_info": concurrent.OtherInfo,
	}).Error)
	require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", channel.Id).
		Update("enabled", false).Error)

	update := ManagedChannelUpdate{
		RouteID:               route.ID,
		ExpectedRouteState:    route.State,
		ExpectedRouteDetached: route.Detached,
		Rank:                  1,
		EffectiveMultiplier:   0.1,
		UpdatedAt:             1_788_320_000,
		Priority:              999,
		BaseURL:               "https://api.example.com",
		Models:                "gpt-4.1",
		Status:                common.ChannelStatusEnabled,
	}
	changed, err := UpdateManagedChannelIfUnchanged(expected, update)
	require.NoError(t, err)
	assert.False(t, changed)

	stored, ability := loadChannelStatusCASFixture(t, channel.Id)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	assert.Equal(t, concurrent.OtherInfo, stored.OtherInfo)
	assert.Equal(t, "gpt-3.5-turbo", stored.Models)
	assert.False(t, ability.Enabled)
	var storedRoute UpstreamManagedRoute
	require.NoError(t, DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, 7, storedRoute.Rank)

	fresh, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	update.Status = fresh.Status
	changed, err = UpdateManagedChannelIfUnchanged(fresh, update)
	require.NoError(t, err)
	require.True(t, changed)

	stored, ability = loadChannelStatusCASFixture(t, channel.Id)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	assert.Equal(t, concurrent.OtherInfo, stored.OtherInfo)
	assert.Equal(t, "gpt-4.1", stored.Models)
	require.NotNil(t, stored.Priority)
	assert.EqualValues(t, 999, *stored.Priority)
	assert.Equal(t, "https://api.example.com", stored.GetBaseURL())
	assert.False(t, ability.Enabled)
	assert.Equal(t, "gpt-4.1", ability.Model)
	require.NotNil(t, ability.Priority)
	assert.EqualValues(t, 999, *ability.Priority)
	require.NoError(t, DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, 1, storedRoute.Rank)
	assert.Equal(t, 0.1, storedRoute.EffectiveMultiplier)
}

func TestUpdateManagedChannelIfUnchangedRejectsConcurrentManagedFieldChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, channel Channel)
		verify func(t *testing.T, channel Channel)
	}{
		{
			name: "models",
			mutate: func(t *testing.T, channel Channel) {
				t.Helper()
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
					Update("models", "concurrent-model").Error)
			},
			verify: func(t *testing.T, channel Channel) {
				t.Helper()
				assert.Equal(t, "concurrent-model", channel.Models)
			},
		},
		{
			name: "priority",
			mutate: func(t *testing.T, channel Channel) {
				t.Helper()
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
					Update("priority", 777).Error)
			},
			verify: func(t *testing.T, channel Channel) {
				t.Helper()
				require.NotNil(t, channel.Priority)
				assert.EqualValues(t, 777, *channel.Priority)
			},
		},
		{
			name: "base_url",
			mutate: func(t *testing.T, channel Channel) {
				t.Helper()
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
					Update("base_url", "https://concurrent.example.com").Error)
			},
			verify: func(t *testing.T, channel Channel) {
				t.Helper()
				assert.Equal(t, "https://concurrent.example.com", channel.GetBaseURL())
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
				"owner": "before",
			})
			require.NoError(t, DB.AutoMigrate(&UpstreamManagedRoute{}))
			require.NoError(t, DB.Exec("DELETE FROM upstream_managed_routes").Error)
			route := UpstreamManagedRoute{
				SourceID:        1,
				ExternalGroupID: "managed-field-" + testCase.name,
				Platform:        "openai",
				Protocol:        UpstreamProtocolOpenAI,
				ChannelID:       channel.Id,
				State:           UpstreamRouteStateActive,
				Rank:            7,
			}
			require.NoError(t, DB.Create(&route).Error)

			expected, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)
			testCase.mutate(t, channel)

			changed, err := UpdateManagedChannelIfUnchanged(expected, ManagedChannelUpdate{
				RouteID:               route.ID,
				ExpectedRouteState:    route.State,
				ExpectedRouteDetached: route.Detached,
				Rank:                  1,
				EffectiveMultiplier:   0.1,
				UpdatedAt:             1_788_320_000,
				Priority:              999,
				BaseURL:               "https://desired.example.com",
				Models:                "desired-model",
				Status:                common.ChannelStatusEnabled,
			})
			require.NoError(t, err)
			assert.False(t, changed)

			stored, _ := loadChannelStatusCASFixture(t, channel.Id)
			testCase.verify(t, stored)
			var storedRoute UpstreamManagedRoute
			require.NoError(t, DB.First(&storedRoute, route.ID).Error)
			assert.Equal(t, 7, storedRoute.Rank)
		})
	}
}

func TestUpdateManagedChannelIfUnchangedRefreshesManagedFieldsInCache(t *testing.T) {
	channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
		"owner": "before",
	})
	require.NoError(t, DB.AutoMigrate(&UpstreamManagedRoute{}))
	require.NoError(t, DB.Exec("DELETE FROM upstream_managed_routes").Error)
	route := UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "managed-cache",
		Platform:        "openai",
		Protocol:        UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           UpstreamRouteStateActive,
		Rank:            7,
	}
	require.NoError(t, DB.Create(&route).Error)
	common.MemoryCacheEnabled = true
	InitChannelCache()

	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	changed, err := UpdateManagedChannelIfUnchanged(expected, ManagedChannelUpdate{
		RouteID:               route.ID,
		ExpectedRouteState:    route.State,
		ExpectedRouteDetached: route.Detached,
		Rank:                  1,
		EffectiveMultiplier:   0.1,
		UpdatedAt:             1_788_320_000,
		Priority:              999,
		BaseURL:               "https://desired.example.com",
		Models:                "desired-model",
		Status:                common.ChannelStatusEnabled,
	})
	require.NoError(t, err)
	require.True(t, changed)

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, "desired-model", cached.Models)
	assert.EqualValues(t, 999, cached.GetPriority())
	assert.Equal(t, "https://desired.example.com", cached.GetBaseURL())

	channelSyncLock.RLock()
	oldModelIDs := append([]int(nil), group2model2channels["default"]["gpt-3.5-turbo"]...)
	newModelIDs := append([]int(nil), group2model2channels["default"]["desired-model"]...)
	channelSyncLock.RUnlock()
	assert.NotContains(t, oldModelIDs, channel.Id)
	assert.Contains(t, newModelIDs, channel.Id)
}

func TestUpdateManagedChannelIfUnchangedRejectsEnableForInactiveRoute(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		detached bool
	}{
		{name: "paused", state: UpstreamRouteStatePaused},
		{name: "detached", state: UpstreamRouteStateActive, detached: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
				"owner": "managed",
			})
			require.NoError(t, DB.AutoMigrate(&UpstreamManagedRoute{}))
			require.NoError(t, DB.Exec("DELETE FROM upstream_managed_routes").Error)
			require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
				Update("status", common.ChannelStatusAutoDisabled).Error)
			require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", channel.Id).
				Update("enabled", false).Error)
			route := UpstreamManagedRoute{
				SourceID:        1,
				ExternalGroupID: "inactive-" + testCase.name,
				Platform:        "openai",
				Protocol:        UpstreamProtocolOpenAI,
				ChannelID:       channel.Id,
				State:           testCase.state,
				Detached:        testCase.detached,
				Rank:            7,
			}
			require.NoError(t, DB.Create(&route).Error)
			expected, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)

			changed, err := UpdateManagedChannelIfUnchanged(expected, ManagedChannelUpdate{
				RouteID:               route.ID,
				ExpectedRouteState:    route.State,
				ExpectedRouteDetached: route.Detached,
				Rank:                  1,
				EffectiveMultiplier:   0.1,
				UpdatedAt:             1_788_320_000,
				Priority:              999,
				BaseURL:               "https://desired.example.com",
				Models:                "desired-model",
				Status:                common.ChannelStatusEnabled,
			})
			require.NoError(t, err)
			assert.False(t, changed)

			stored, ability := loadChannelStatusCASFixture(t, channel.Id)
			assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
			assert.False(t, ability.Enabled)
			var storedRoute UpstreamManagedRoute
			require.NoError(t, DB.First(&storedRoute, route.ID).Error)
			assert.Equal(t, 7, storedRoute.Rank)
			assert.Equal(t, testCase.state, storedRoute.State)
			assert.Equal(t, testCase.detached, storedRoute.Detached)
		})
	}
}

func TestUpdateManagedChannelIfUnchangedRejectsChangedRouteSnapshot(t *testing.T) {
	tests := []struct {
		name                 string
		initialChannelStatus int
		updateStatus         int
		routeUpdates         map[string]any
	}{
		{
			name:                 "enable after route paused",
			initialChannelStatus: common.ChannelStatusAutoDisabled,
			updateStatus:         common.ChannelStatusEnabled,
			routeUpdates:         map[string]any{"state": UpstreamRouteStatePaused},
		},
		{
			name:                 "disable after route detached",
			initialChannelStatus: common.ChannelStatusEnabled,
			updateStatus:         common.ChannelStatusAutoDisabled,
			routeUpdates:         map[string]any{"detached": true},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
				"owner": "managed-route-snapshot",
			})
			require.NoError(t, DB.AutoMigrate(&UpstreamManagedRoute{}))
			require.NoError(t, DB.Exec("DELETE FROM upstream_managed_routes").Error)
			if testCase.initialChannelStatus != common.ChannelStatusEnabled {
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).
					Update("status", testCase.initialChannelStatus).Error)
				require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", channel.Id).
					Update("enabled", false).Error)
			}
			route := UpstreamManagedRoute{
				SourceID:        1,
				ExternalGroupID: "changed-route-" + testCase.name,
				Platform:        "openai",
				Protocol:        UpstreamProtocolOpenAI,
				ChannelID:       channel.Id,
				State:           UpstreamRouteStateActive,
				Rank:            7,
			}
			require.NoError(t, DB.Create(&route).Error)
			expected, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)
			require.NoError(t, DB.Model(&UpstreamManagedRoute{}).
				Where("id = ?", route.ID).
				Updates(testCase.routeUpdates).Error)

			changed, err := UpdateManagedChannelIfUnchanged(expected, ManagedChannelUpdate{
				RouteID:               route.ID,
				ExpectedRouteState:    route.State,
				ExpectedRouteDetached: route.Detached,
				Rank:                  1,
				EffectiveMultiplier:   0.1,
				UpdatedAt:             1_788_320_000,
				Priority:              999,
				BaseURL:               "https://desired.example.com",
				Models:                "desired-model",
				Status:                testCase.updateStatus,
			})
			require.NoError(t, err)
			assert.False(t, changed)

			stored, ability := loadChannelStatusCASFixture(t, channel.Id)
			assert.Equal(t, testCase.initialChannelStatus, stored.Status)
			assert.Equal(t, "gpt-3.5-turbo", stored.Models)
			assert.Equal(t, testCase.initialChannelStatus == common.ChannelStatusEnabled, ability.Enabled)
			var storedRoute UpstreamManagedRoute
			require.NoError(t, DB.First(&storedRoute, route.ID).Error)
			assert.Equal(t, 7, storedRoute.Rank)
		})
	}
}

func TestUpdateManagedChannelIfUnchangedLocksRouteBeforeChannel(t *testing.T) {
	channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
		"owner": "before",
	})
	require.NoError(t, DB.AutoMigrate(&UpstreamManagedRoute{}))
	require.NoError(t, DB.Exec("DELETE FROM upstream_managed_routes").Error)
	route := UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "lock-order",
		Platform:        "openai",
		Protocol:        UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           UpstreamRouteStateActive,
		Rank:            7,
	}
	require.NoError(t, DB.Create(&route).Error)
	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)

	var transactionalReads []string
	const callbackName = "test:capture_managed_channel_lock_order"
	require.NoError(t, DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil {
			return
		}
		if _, inTransaction := tx.Statement.ConnPool.(*sql.Tx); !inTransaction {
			return
		}
		switch tx.Statement.Table {
		case "upstream_managed_routes", "channels":
			transactionalReads = append(transactionalReads, tx.Statement.Table)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, DB.Callback().Query().Remove(callbackName))
	})

	changed, err := UpdateManagedChannelIfUnchanged(expected, ManagedChannelUpdate{
		RouteID:               route.ID,
		ExpectedRouteState:    route.State,
		ExpectedRouteDetached: route.Detached,
		Rank:                  1,
		EffectiveMultiplier:   0.25,
		UpdatedAt:             1_788_320_000,
		Priority:              999,
		BaseURL:               "https://api.example.com",
		Models:                "gpt-4.1",
		Status:                common.ChannelStatusAutoDisabled,
	})
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, []string{"upstream_managed_routes", "channels"}, transactionalReads)

	stored, ability := loadChannelStatusCASFixture(t, channel.Id)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	assert.Equal(t, "gpt-4.1", stored.Models)
	assert.False(t, ability.Enabled)
	assert.Equal(t, "gpt-4.1", ability.Model)
	var storedRoute UpstreamManagedRoute
	require.NoError(t, DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, 1, storedRoute.Rank)
	assert.Equal(t, 0.25, storedRoute.EffectiveMultiplier)
}

func TestUpdateSingleKeyChannelStatusIfUnchangedConfiguredDatabases(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		databaseType common.DatabaseType
		dialector    func(string) gorm.Dialector
	}{
		{
			name:         "mysql",
			env:          "TEST_MYSQL_DSN",
			databaseType: common.DatabaseTypeMySQL,
			dialector: func(dsn string) gorm.Dialector {
				return mysql.Open(dsn)
			},
		},
		{
			name:         "postgres",
			env:          "TEST_POSTGRES_DSN",
			databaseType: common.DatabaseTypePostgreSQL,
			dialector: func(dsn string) gorm.Dialector {
				return postgres.New(postgres.Config{
					DSN:                  dsn,
					PreferSimpleProtocol: true,
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skip(test.env + " is not configured")
			}

			tablePrefix := fmt.Sprintf("cas_%d_%d_", os.Getpid(), time.Now().UnixNano())
			database, err := gorm.Open(test.dialector(dsn), &gorm.Config{
				NamingStrategy: schema.NamingStrategy{TablePrefix: tablePrefix},
			})
			require.NoError(t, err)
			sqlDB, err := database.DB()
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, sqlDB.Close())
			})

			var version string
			require.NoError(t, database.Raw("SELECT VERSION()").Scan(&version).Error)
			t.Logf("%s version: %s", test.name, version)
			require.NoError(t, database.AutoMigrate(&Channel{}, &Ability{}))
			t.Cleanup(func() {
				require.NoError(t, database.Migrator().DropTable(&Ability{}, &Channel{}))
			})

			previousDB := DB
			previousMainType := common.MainDatabaseType()
			previousLogType := common.LogDatabaseType()
			previousMemoryCacheEnabled := common.MemoryCacheEnabled
			DB = database
			common.SetDatabaseTypes(test.databaseType, previousLogType)
			initCol()
			common.MemoryCacheEnabled = false
			t.Cleanup(func() {
				DB = previousDB
				common.SetDatabaseTypes(previousMainType, previousLogType)
				initCol()
				common.MemoryCacheEnabled = previousMemoryCacheEnabled
			})

			channel := Channel{
				Name:   "configured-database-status-cas",
				Key:    "credential",
				Status: common.ChannelStatusEnabled,
				Models: "gpt-3.5-turbo",
				Group:  "default",
			}
			channel.SetOtherInfo(map[string]any{"owner": "snapshot"})
			require.NoError(t, DB.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			expectedOtherInfo := channel.OtherInfo
			channel.SetOtherInfo(map[string]any{
				"owner":           "snapshot",
				"quota_domain_id": "configured-domain",
				"quota_type":      "plan",
			})

			changed, err := UpdateSingleKeyChannelStatusIfUnchanged(
				channel.Id,
				channel.Key,
				channel.GetTag(),
				common.ChannelStatusEnabled,
				expectedOtherInfo,
				common.ChannelStatusAutoDisabled,
				channel.OtherInfo,
			)
			require.NoError(t, err)
			require.True(t, changed)

			stored, ability := loadChannelStatusCASFixture(t, channel.Id)
			assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
			assert.Equal(t, channel.OtherInfo, stored.OtherInfo)
			assert.False(t, ability.Enabled)

			sibling := Channel{
				Name:   "configured-database-status-cas-sibling",
				Key:    channel.Key,
				Status: common.ChannelStatusEnabled,
				Models: channel.Models,
				Group:  channel.Group,
			}
			require.NoError(t, DB.Create(&sibling).Error)
			require.NoError(t, sibling.AddAbilities(nil))
			firstExpected, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)
			secondExpected, err := GetChannelById(sibling.Id, true)
			require.NoError(t, err)
			firstDesired := *firstExpected
			firstDesired.SetOtherInfo(map[string]any{
				"owner":            "snapshot",
				"quota_domain_id":  "configured-domain",
				"quota_generation": "configured-generation",
				"quota_type":       "plan",
			})
			secondDesired := *secondExpected
			secondDesired.SetOtherInfo(map[string]any{
				"quota_domain_id":  "configured-domain",
				"quota_generation": "configured-generation",
				"quota_type":       "plan",
			})
			changed, err = UpdateSingleKeyChannelStatusesIfUnchanged([]SingleKeyChannelStatusUpdate{
				{
					Expected:  firstExpected,
					Status:    common.ChannelStatusAutoDisabled,
					OtherInfo: firstDesired.OtherInfo,
				},
				{
					Expected:  secondExpected,
					Status:    common.ChannelStatusAutoDisabled,
					OtherInfo: secondDesired.OtherInfo,
				},
			})
			require.NoError(t, err)
			require.True(t, changed)

			stored, ability = loadChannelStatusCASFixture(t, channel.Id)
			assert.Equal(t, firstDesired.OtherInfo, stored.OtherInfo)
			assert.False(t, ability.Enabled)
			storedSibling, siblingAbility := loadChannelStatusCASFixture(t, sibling.Id)
			assert.Equal(t, common.ChannelStatusAutoDisabled, storedSibling.Status)
			assert.Equal(t, secondDesired.OtherInfo, storedSibling.OtherInfo)
			assert.False(t, siblingAbility.Enabled)
		})
	}
}
