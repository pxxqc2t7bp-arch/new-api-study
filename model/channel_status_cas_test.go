package model

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
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
		RouteID:             route.ID,
		Rank:                1,
		EffectiveMultiplier: 0.1,
		UpdatedAt:           1_788_320_000,
		Priority:            999,
		BaseURL:             "https://api.example.com",
		Models:              "gpt-4.1",
		Status:              common.ChannelStatusEnabled,
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
				RouteID:             route.ID,
				Rank:                1,
				EffectiveMultiplier: 0.1,
				UpdatedAt:           1_788_320_000,
				Priority:            999,
				BaseURL:             "https://desired.example.com",
				Models:              "desired-model",
				Status:              common.ChannelStatusEnabled,
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
		RouteID:             route.ID,
		Rank:                1,
		EffectiveMultiplier: 0.1,
		UpdatedAt:           1_788_320_000,
		Priority:            999,
		BaseURL:             "https://desired.example.com",
		Models:              "desired-model",
		Status:              common.ChannelStatusEnabled,
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
				RouteID:             route.ID,
				Rank:                1,
				EffectiveMultiplier: 0.1,
				UpdatedAt:           1_788_320_000,
				Priority:            999,
				BaseURL:             "https://desired.example.com",
				Models:              "desired-model",
				Status:              common.ChannelStatusEnabled,
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
		RouteID:             route.ID,
		Rank:                1,
		EffectiveMultiplier: 0.25,
		UpdatedAt:           1_788_320_000,
		Priority:            999,
		BaseURL:             "https://api.example.com",
		Models:              "gpt-4.1",
		Status:              common.ChannelStatusAutoDisabled,
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
			require.NoError(t, database.AutoMigrate(&Channel{}, &Ability{}, &UpstreamManagedRoute{}))
			t.Cleanup(func() {
				require.NoError(t, database.Migrator().DropTable(&UpstreamManagedRoute{}, &Ability{}, &Channel{}))
			})

			previousDB := DB
			previousMainType := common.MainDatabaseType()
			previousLogType := common.LogDatabaseType()
			previousMemoryCacheEnabled := common.MemoryCacheEnabled
			DB = database
			common.SetDatabaseTypes(test.databaseType, previousLogType)
			common.MemoryCacheEnabled = false
			t.Cleanup(func() {
				DB = previousDB
				common.SetDatabaseTypes(previousMainType, previousLogType)
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

			route := UpstreamManagedRoute{
				SourceID:        1,
				ExternalGroupID: "configured-group",
				Platform:        "openai",
				Protocol:        UpstreamProtocolOpenAI,
				ChannelID:       channel.Id,
				State:           UpstreamRouteStateActive,
				Rank:            7,
			}
			require.NoError(t, DB.Create(&route).Error)
			managedSnapshot, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)
			changed, err = UpdateManagedChannelIfUnchanged(managedSnapshot, ManagedChannelUpdate{
				RouteID:             route.ID,
				Rank:                1,
				EffectiveMultiplier: 0.1,
				UpdatedAt:           time.Now().Unix(),
				Priority:            999,
				BaseURL:             "https://api.example.com",
				Models:              "gpt-4.1",
				Status:              common.ChannelStatusAutoDisabled,
			})
			require.NoError(t, err)
			require.True(t, changed)

			stored, ability = loadChannelStatusCASFixture(t, channel.Id)
			assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
			assert.Equal(t, channel.OtherInfo, stored.OtherInfo)
			assert.Equal(t, "gpt-4.1", stored.Models)
			assert.False(t, ability.Enabled)
			var storedRoute UpstreamManagedRoute
			require.NoError(t, DB.First(&storedRoute, route.ID).Error)
			assert.Equal(t, 1, storedRoute.Rank)
		})
	}
}
