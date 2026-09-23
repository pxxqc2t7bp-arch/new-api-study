package model

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupChannelStatusTest(t *testing.T) {
	t.Helper()
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
	require.NoError(t, DB.Exec("DELETE FROM channels").Error)

	memoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = memoryCacheEnabled
	})
}

func TestChannelHasEnabledKey(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		statusList map[int]int
		want       bool
	}{
		{
			name: "missing status defaults to enabled",
			key:  "key-a\nkey-b",
			statusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
			},
			want: true,
		},
		{
			name: "explicit enabled status",
			key:  "key-a\nkey-b",
			statusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusEnabled,
			},
			want: true,
		},
		{
			name: "all keys disabled",
			key:  "key-a\nkey-b",
			statusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusManuallyDisabled,
			},
		},
		{
			name: "status outside key range is ignored",
			key:  "key-a",
			statusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusEnabled,
			},
		},
		{
			name: "no keys",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &Channel{
				Key: test.key,
				ChannelInfo: ChannelInfo{
					IsMultiKey:         true,
					MultiKeyStatusList: test.statusList,
				},
			}

			assert.Equal(t, test.want, channel.HasEnabledKey())
		})
	}
}

func TestUpdateChannelStatusPersistsMultiKeyState(t *testing.T) {
	setupChannelStatusTest(t)

	channel := Channel{
		Name:   "multi-key-status",
		Key:    "key-a\nkey-b",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         2,
			MultiKeyMode:         constant.MultiKeyModePolling,
			MultiKeyPollingIndex: 1,
		},
	}
	require.NoError(t, DB.Create(&channel).Error)

	changed := UpdateChannelStatus(channel.Id, "key-a", common.ChannelStatusAutoDisabled, "provider rejected key")
	require.True(t, changed)

	var stored Channel
	require.NoError(t, DB.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, "provider rejected key", stored.ChannelInfo.MultiKeyDisabledReason[0])
	assert.NotZero(t, stored.ChannelInfo.MultiKeyDisabledTime[0])
	assert.Equal(t, 1, stored.ChannelInfo.MultiKeyPollingIndex)
}

func TestUpdateChannelStatusSingleKeyRollsBackAbilityFailure(t *testing.T) {
	channel := createSingleKeyChannelStatusCASFixture(t, map[string]any{
		"owner": "before",
	})
	expectedOtherInfo := channel.OtherInfo
	common.MemoryCacheEnabled = true
	InitChannelCache()

	forcedErr := errors.New("forced legacy ability failure")
	const callbackName = "test:fail_single_key_update_channel_status_ability"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "abilities" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, DB.Callback().Update().Remove(callbackName))
	})

	changed := UpdateChannelStatus(
		channel.Id,
		"",
		common.ChannelStatusAutoDisabled,
		"provider failure",
	)
	assert.False(t, changed)

	stored, ability := loadChannelStatusCASFixture(t, channel.Id)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, expectedOtherInfo, stored.OtherInfo)
	assert.True(t, ability.Enabled)
	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
}

func TestUpdateChannelStatusSingleKeyLeavesNoInterleavingGap(t *testing.T) {
	originalDB := DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	database, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:%s?mode=memory&cache=shared",
		strings.ReplaceAll(t.Name(), "/", "_"),
	)), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&Channel{}, &Ability{}))
	DB = database
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		DB = originalDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		sqlDB, dbErr := database.DB()
		if dbErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	channel := Channel{
		Name:   "single-key-interleaving",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-3.5-turbo",
		Group:  "default",
	}
	channel.SetOtherInfo(map[string]any{"owner": "before"})
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	const callbackName = "test:interleave_single_key_update_channel_status"
	attempted := false
	injected := false
	require.NoError(t, DB.Callback().Update().After("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "channels" || injected {
			return
		}
		attempted = true
		if _, inTransaction := tx.Statement.ConnPool.(*sql.Tx); inTransaction {
			return
		}
		injected = true
		concurrent := DB.Session(&gorm.Session{SkipHooks: true})
		err := concurrent.Transaction(func(recoveryTx *gorm.DB) error {
			if err := recoveryTx.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
				"status":     common.ChannelStatusEnabled,
				"other_info": `{"owner":"concurrent-recovery"}`,
			}).Error; err != nil {
				return err
			}
			return recoveryTx.Model(&Ability{}).Where("channel_id = ?", channel.Id).
				Select("enabled").Update("enabled", true).Error
		})
		require.NoError(t, err)
	}))
	t.Cleanup(func() {
		require.NoError(t, DB.Callback().Update().Remove(callbackName))
	})

	changed := UpdateChannelStatus(
		channel.Id,
		"",
		common.ChannelStatusAutoDisabled,
		"provider failure",
	)
	require.True(t, attempted)
	require.True(t, changed)

	stored, ability := loadChannelStatusCASFixture(t, channel.Id)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	assert.False(t, ability.Enabled)
}

func TestSaveStatusStateFromSingleKeySnapshotPreservesUnownedColumns(t *testing.T) {
	setupChannelStatusTest(t)

	channel := Channel{
		Name:        "single-key-status",
		Key:         "original-key",
		Status:      common.ChannelStatusEnabled,
		Models:      "original-model",
		Group:       "default",
		UsedQuota:   100,
		ChannelInfo: ChannelInfo{},
	}
	require.NoError(t, DB.Create(&channel).Error)

	stale, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)

	concurrentChannelInfo := ChannelInfo{
		IsMultiKey:           true,
		MultiKeySize:         2,
		MultiKeyMode:         constant.MultiKeyModePolling,
		MultiKeyPollingIndex: 1,
	}
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
		"key":          "rotated-key",
		"used_quota":   gorm.Expr("used_quota + ?", 250),
		"models":       "concurrent-model",
		"channel_info": concurrentChannelInfo,
	}).Error)

	stale.Status = common.ChannelStatusManuallyDisabled
	stale.SetOtherInfo(map[string]interface{}{
		"status_reason": "manual operation",
		"status_time":   int64(1234),
	})
	require.NoError(t, stale.saveStatusState())

	var stored Channel
	require.NoError(t, DB.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status)
	assert.Equal(t, "rotated-key", stored.Key)
	assert.Equal(t, int64(350), stored.UsedQuota)
	assert.Equal(t, "concurrent-model", stored.Models)
	assert.Equal(t, concurrentChannelInfo, stored.ChannelInfo)

	otherInfo := stored.GetOtherInfo()
	assert.Equal(t, "manual operation", otherInfo["status_reason"])
	assert.Equal(t, float64(1234), otherInfo["status_time"])
}
