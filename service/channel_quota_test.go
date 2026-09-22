package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestParsePlanQuotaReset(t *testing.T) {
	resetAt, matched := ParsePlanQuotaReset(
		"status_code=429, You have exceeded the weekly usage quota. " +
			"It will reset at 2026-08-31 00:00:00 +0800 CST.",
	)

	assert.True(t, matched)
	assert.Equal(t, time.Date(2026, 8, 31, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60)).Unix(), resetAt)
}

func TestParsePlanQuotaResetMonthly(t *testing.T) {
	resetAt, matched := ParsePlanQuotaReset(
		"status_code=429, You have exceeded the monthly usage quota. " +
			"It will reset at 2026-09-30 23:59:59 +0800 CST.",
	)

	assert.True(t, matched)
	assert.Equal(t, time.Date(2026, 9, 30, 23, 59, 59, 0, time.FixedZone("CST", 8*60*60)).Unix(), resetAt)
}

func TestParsePlanQuotaResetRejectsOrdinaryRateLimit(t *testing.T) {
	resetAt, matched := ParsePlanQuotaReset("status_code=429, too many requests")

	assert.False(t, matched)
	assert.Zero(t, resetAt)
}

func TestParsePlanQuotaResetKeepsQuotaMatchWhenResetTimeIsInvalid(t *testing.T) {
	resetAt, matched := ParsePlanQuotaReset(
		"status_code=429, You have exceeded the weekly usage quota. It will reset at unknown.",
	)

	assert.True(t, matched)
	assert.Zero(t, resetAt)
}

func TestShouldDisableChannelRecognizesMonthlyPlanQuota(t *testing.T) {
	originalEnabled := common.AutomaticDisableChannelEnabled
	originalKeywords := operation_setting.AutomaticDisableKeywords
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableKeywords = []string{"unrelated"}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = originalEnabled
		operation_setting.AutomaticDisableKeywords = originalKeywords
	})

	err := types.NewOpenAIError(
		errors.New("You have exceeded the monthly usage quota. It will reset at 2026-09-30 23:59:59 +0800 CST."),
		types.ErrorCode("AccountQuotaExceeded"),
		http.StatusTooManyRequests,
	)

	assert.True(t, ShouldDisableChannel(err))
}

func setupPlanQuotaDomainTest(t *testing.T) *gorm.DB {
	t.Helper()
	originalDB := model.DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:%s?mode=memory&cache=shared",
		strings.ReplaceAll(t.Name(), "/", "_"),
	)), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}))
	model.DB = db
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		model.DB = originalDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})
	return db
}

func TestDisableAndEnablePlanQuotaDomainLifecycle(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	tag := "plan:support:coding"
	otherPlanTag := "plan:support:analysis"
	ordinaryTag := "ark-ordinary:support"
	channels := []model.Channel{
		{Id: 11, Name: "messages", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan},
		{Id: 12, Name: "responses", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan},
		{Id: 13, Name: "other-plan", Key: "other", Status: common.ChannelStatusEnabled, Tag: &otherPlanTag, AutoBan: &autoBan},
		{Id: 14, Name: "ordinary", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &ordinaryTag, AutoBan: &autoBan},
	}
	channels[0].Models = "gpt-3.5-turbo"
	channels[0].Group = "default"
	channels[0].SetOtherInfo(map[string]any{"owner": "preserved"})
	channels[1].Models = "gpt-3.5-turbo"
	channels[1].Group = "default"
	require.NoError(t, db.Create(&channels).Error)
	require.NoError(t, channels[0].AddAbilities(nil))
	require.NoError(t, channels[1].AddAbilities(nil))

	resetAt := time.Now().Add(time.Hour).Unix()
	disablePlanQuotaDomain(&channels[0], "quota exhausted", resetAt)

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 4)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[0].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[1].Status)
	assert.Equal(t, common.ChannelStatusEnabled, stored[2].Status)
	assert.Equal(t, common.ChannelStatusEnabled, stored[3].Status)
	assert.Equal(t, resetAt+60, stored[0].GetDisabledUntil())
	assert.Equal(t, resetAt+60, stored[1].GetDisabledUntil())
	assert.Zero(t, stored[2].GetDisabledUntil())
	assert.Zero(t, stored[3].GetDisabledUntil())
	firstInfo := stored[0].GetOtherInfo()
	assert.Equal(t, "preserved", firstInfo["owner"])
	assert.Equal(t, tag, firstInfo["quota_domain"])
	assert.Equal(t, "plan", firstInfo["quota_type"])
	assert.Equal(t, float64(resetAt), firstInfo["quota_reset_at"])

	var disabledAbilities []model.Ability
	require.NoError(t, db.Where("channel_id IN ?", []int{11, 12}).Order("channel_id").Find(&disabledAbilities).Error)
	require.Len(t, disabledAbilities, 2)
	assert.False(t, disabledAbilities[0].Enabled)
	assert.False(t, disabledAbilities[1].Enabled)

	enablePlanQuotaDomain(tag)

	require.NoError(t, db.Order("id").Find(&stored).Error)
	assert.Equal(t, common.ChannelStatusEnabled, stored[0].Status)
	assert.Equal(t, common.ChannelStatusEnabled, stored[1].Status)
	for _, channel := range stored[:2] {
		info := channel.GetOtherInfo()
		assert.NotContains(t, info, "disabled_until")
		assert.NotContains(t, info, "quota_reset_at")
		assert.NotContains(t, info, "quota_domain")
		assert.NotContains(t, info, "quota_type")
	}
	assert.Equal(t, "preserved", stored[0].GetOtherInfo()["owner"])
	require.NoError(t, db.Where("channel_id IN ?", []int{11, 12}).Order("channel_id").Find(&disabledAbilities).Error)
	require.Len(t, disabledAbilities, 2)
	assert.True(t, disabledAbilities[0].Enabled)
	assert.True(t, disabledAbilities[1].Enabled)
}

func TestDisablePlanQuotaCredentialDomain(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	planTag := "plan:support:coding"
	nativeTag := "plan:support:native"
	ordinaryTag := "ark-ordinary:support"
	channels := []model.Channel{
		{Id: 11, Name: "messages", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &planTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 12, Name: "responses", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &planTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 13, Name: "native", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &nativeTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 14, Name: "other-account", Key: "other", Status: common.ChannelStatusEnabled, Tag: &planTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 15, Name: "ordinary", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &ordinaryTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
	}
	require.NoError(t, db.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}

	resetAt := time.Now().Add(time.Hour).Unix()
	disablePlanQuotaDomain(&channels[0], "quota exhausted", resetAt)

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 5)
	for i, channel := range stored {
		if channel.Id >= 11 && channel.Id <= 13 {
			assert.Equal(t, common.ChannelStatusAutoDisabled, channel.Status)
			assert.Equal(t, resetAt+60, channel.GetDisabledUntil())
			assert.Equal(t, channels[i].GetTag(), channel.GetOtherInfo()["quota_domain"])
			continue
		}
		assert.Equal(t, common.ChannelStatusEnabled, channel.Status)
		assert.Zero(t, channel.GetDisabledUntil())
		assert.NotContains(t, channel.GetOtherInfo(), "quota_domain")
	}

	var abilities []model.Ability
	require.NoError(t, db.Order("channel_id").Find(&abilities).Error)
	require.Len(t, abilities, 5)
	for _, ability := range abilities {
		if ability.ChannelId >= 11 && ability.ChannelId <= 13 {
			assert.False(t, ability.Enabled)
			continue
		}
		assert.True(t, ability.Enabled)
	}
}

func TestDisablePlanQuotaCredentialDomainRejectsMissingCredential(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		useNilChannel  bool
		channelID      int
		channelKey     string
		channelTagName string
	}{
		{name: "nil channel", useNilChannel: true, channelID: 21, channelTagName: "plan:support:nil"},
		{name: "empty credential", channelID: 22, channelTagName: "plan:support:empty"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db := setupPlanQuotaDomainTest(t)
			autoBan := 1
			channel := model.Channel{
				Id:      testCase.channelID,
				Name:    testCase.name,
				Key:     testCase.channelKey,
				Status:  common.ChannelStatusEnabled,
				Tag:     &testCase.channelTagName,
				AutoBan: &autoBan,
				Models:  "gpt-3.5-turbo",
				Group:   "default",
			}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))

			var failingChannel *model.Channel
			if !testCase.useNilChannel {
				failingChannel = &channel
			}
			disablePlanQuotaDomain(failingChannel, "quota exhausted", time.Now().Add(time.Hour).Unix())

			var stored model.Channel
			require.NoError(t, db.First(&stored, "id = ?", channel.Id).Error)
			assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
			assert.Zero(t, stored.GetDisabledUntil())
			assert.NotContains(t, stored.GetOtherInfo(), "quota_domain")

			var ability model.Ability
			require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
			assert.True(t, ability.Enabled)
		})
	}
}

func TestDisablePlanQuotaDomainWithoutResetStillDisables(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)
	autoBan := 1
	tag := "plan:support:no-reset"
	channels := []model.Channel{
		{Id: 21, Name: "messages", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan},
		{Id: 22, Name: "responses", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan},
	}
	require.NoError(t, db.Create(&channels).Error)

	resetAt, matched := ParsePlanQuotaReset(
		"status_code=429, You have exceeded the 5-hour usage quota. It will reset at unknown.",
	)
	require.True(t, matched)
	require.Zero(t, resetAt)
	disablePlanQuotaDomain(&channels[0], "quota reset unknown", resetAt)

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 2)
	for _, channel := range stored {
		assert.Equal(t, common.ChannelStatusAutoDisabled, channel.Status)
		info := channel.GetOtherInfo()
		assert.Equal(t, tag, info["quota_domain"])
		assert.Equal(t, "plan", info["quota_type"])
		assert.NotContains(t, info, "quota_reset_at")
		assert.NotContains(t, info, "disabled_until")
	}
}
