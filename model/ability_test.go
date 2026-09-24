package model

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestUpdateAbilitiesReloadsCurrentChannelState(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	channel := Channel{
		Name: "stale-ability-snapshot", Key: "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(db))
	InitChannelCache()

	stale := channel
	require.NoError(t, db.Model(&Channel{}).
		Where("id = ?", channel.Id).
		Update("status", common.ChannelStatusAutoDisabled).Error)
	require.NoError(t, db.Model(&Ability{}).
		Where("channel_id = ?", channel.Id).
		Update("enabled", false).Error)

	require.NoError(t, stale.UpdateAbilities(nil))

	var ability Ability
	require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stale.Status)
}

func TestUpdateChannelUpstreamModelStatePreservesCachedPollingCursor(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	channel := Channel{
		Name:   "upstream-model-cache-cursor",
		Key:    "key-a\nkey-b\nkey-c",
		Status: common.ChannelStatusEnabled,
		Models: "old-model",
		Group:  "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         3,
			MultiKeyStatusList:   map[int]int{},
			MultiKeyPollingIndex: 0,
			MultiKeyMode:         constant.MultiKeyModePolling,
		},
		OtherSettings: `{"owner":"old-settings"}`,
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(db))
	InitChannelCache()

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	newerCache := *cached
	newerCache.ChannelInfo = cloneChannelInfo(cached.ChannelInfo)
	newerCache.ChannelInfo.MultiKeyPollingIndex = 2
	CacheUpdateChannel(&newerCache)

	const updatedSettings = `{"owner":"upstream-refresh"}`
	updatedModels := "new-model"
	updated, err := UpdateChannelUpstreamModelState(
		channel.Id,
		updatedSettings,
		&updatedModels,
	)

	require.NoError(t, err)
	assert.Equal(t, 2, updated.ChannelInfo.MultiKeyPollingIndex)
	assert.Equal(t, updatedModels, updated.Models)
	assert.Equal(t, updatedSettings, updated.OtherSettings)

	cached, err = CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, 2, cached.ChannelInfo.MultiKeyPollingIndex)
	assert.Equal(t, updatedModels, cached.Models)
	assert.Equal(t, updatedSettings, cached.OtherSettings)

	var stored Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Zero(t, stored.ChannelInfo.MultiKeyPollingIndex)
	assert.Equal(t, updatedModels, stored.Models)
	assert.Equal(t, updatedSettings, stored.OtherSettings)
	var abilities []Ability
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	assert.Equal(t, updatedModels, abilities[0].Model)
	assert.True(t, abilities[0].Enabled)
}

func TestFixAbilitySerializesConcurrentPlanQuotaDisable(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	tag := "plan:test:fix-ability-race"
	const credential = "authority-secret-fix-ability-race"
	channel := createPlanQuotaDomainFixture(
		t,
		db,
		credential,
		tag,
		PlanQuotaDomainStateActive,
		0,
		0,
	)
	InitChannelCache()

	channelsRead := make(chan struct{})
	releaseRead := make(chan struct{})
	var intercepted atomic.Bool
	var releaseOnce sync.Once
	callbackName := "test:fix_ability_channel_snapshot:" + strings.ReplaceAll(t.Name(), "/", "_")
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil ||
			tx.Statement.Schema == nil ||
			tx.Statement.Schema.Name != "Channel" ||
			!intercepted.CompareAndSwap(false, true) {
			return
		}
		close(channelsRead)
		<-releaseRead
	}))
	release := func() {
		releaseOnce.Do(func() {
			close(releaseRead)
		})
	}
	t.Cleanup(func() {
		release()
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	var queryReached atomic.Bool
	disableAttempted := make(chan struct{})
	disableCommitted := make(chan struct{})
	var attemptOnce sync.Once
	var commitOnce sync.Once
	previousObserver := channelStatusPublicationObserver
	channelStatusPublicationObserver = func(phase channelStatusPublicationPhase) {
		if !queryReached.Load() {
			return
		}
		switch phase {
		case channelStatusPublicationBeforeWrite:
			attemptOnce.Do(func() {
				close(disableAttempted)
			})
		case channelStatusPublicationAfterCommit:
			commitOnce.Do(func() {
				close(disableCommitted)
			})
		}
	}
	t.Cleanup(func() {
		channelStatusPublicationObserver = previousObserver
	})

	type fixResult struct {
		success int
		failed  int
		err     error
	}
	fixed := make(chan fixResult, 1)
	go func() {
		success, failed, err := FixAbility()
		fixed <- fixResult{success: success, failed: failed, err: err}
	}()
	select {
	case <-channelsRead:
		queryReached.Store(true)
	case <-time.After(5 * time.Second):
		t.Fatal("FixAbility did not read channel snapshots")
	}

	fixHoldsStatusLock := !channelStatusLock.TryLock()
	if !fixHoldsStatusLock {
		channelStatusLock.Unlock()
	}

	disabled := make(chan error, 1)
	go func() {
		_, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
			FailingChannelID:   channel.Id,
			ObservedCredential: credential,
			ObservedTag:        tag,
			Reason:             "quota exhausted",
			ResetAt:            2_000_000_000,
		})
		disabled <- err
	}()
	select {
	case <-disableAttempted:
	case <-time.After(5 * time.Second):
		t.Fatal("quota disable did not attempt the status transition")
	}
	if !fixHoldsStatusLock {
		select {
		case <-disableCommitted:
		case <-time.After(5 * time.Second):
			t.Fatal("quota disable did not commit before stale rebuild")
		}
	}
	release()

	result := <-fixed
	require.NoError(t, result.err)
	assert.Equal(t, 1, result.success)
	assert.Zero(t, result.failed)
	require.NoError(t, <-disabled)

	var stored Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	var enabledAbilities int64
	require.NoError(t, db.Model(&Ability{}).
		Where("channel_id = ? AND enabled = ?", channel.Id, true).
		Count(&enabledAbilities).Error)
	assert.Zero(t, enabledAbilities)
	routingIDs, err := ListSatisfiedChannelIDsAtPriority(
		channel.Group,
		channel.Models,
		channel.GetPriority(),
		nil,
	)
	require.NoError(t, err)
	assert.NotContains(t, routingIDs, channel.Id)
}

func TestFixAbilityRollsBackOnRebuildFailure(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	channel := Channel{
		Name: "fix-ability-rollback", Key: "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(db))

	forcedErr := errors.New("forced FixAbility create failure")
	callbackName := "test:fix_ability_rollback"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil &&
			tx.Statement.Schema != nil &&
			tx.Statement.Schema.Name == "Ability" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Create().Remove(callbackName))
	})

	success, failed, err := FixAbility()
	require.ErrorIs(t, err, forcedErr)
	assert.Zero(t, success)
	assert.Equal(t, 1, failed)

	var abilities []Ability
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	assert.Equal(t, "gpt-4.1", abilities[0].Model)
	assert.True(t, abilities[0].Enabled)
}

func TestFixAbilityHonorsTablePrefix(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "tenant_")
	channel := Channel{
		Name: "fix-ability-prefix", Key: "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(db))

	success, failed, err := FixAbility()
	require.NoError(t, err)
	assert.Equal(t, 1, success)
	assert.Zero(t, failed)

	var abilities []Ability
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	assert.Equal(t, "gpt-4.1", abilities[0].Model)
	assert.True(t, abilities[0].Enabled)
}
