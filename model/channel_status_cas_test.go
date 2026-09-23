package model

import (
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
		})
	}
}
