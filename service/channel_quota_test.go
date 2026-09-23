package service

import (
	"crypto/sha256"
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
	require.NoError(t, db.AutoMigrate(
		&model.Channel{},
		&model.Ability{},
		&model.User{},
		&model.UpstreamManagedRoute{},
	))
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

	EnableChannel(channels[0].Id, "", channels[0].Name)

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

func TestEnablePlanQuotaDomainScopesRecoveryByMarker(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	sharedTag := "plan:support:shared"
	nativeTag := "plan:support:native"
	channels := []model.Channel{
		{Id: 11, Name: "messages", Key: "credential-a", Status: common.ChannelStatusEnabled, Tag: &sharedTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 12, Name: "responses", Key: "credential-a", Status: common.ChannelStatusEnabled, Tag: &sharedTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 13, Name: "native", Key: "credential-a", Status: common.ChannelStatusEnabled, Tag: &nativeTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 14, Name: "other-domain", Key: "credential-b", Status: common.ChannelStatusEnabled, Tag: &sharedTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 15, Name: "manual", Key: "credential-c", Status: common.ChannelStatusManuallyDisabled, Tag: &sharedTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
	}
	credentialADomainID := fmt.Sprintf("%x", sha256.Sum256([]byte("credential-a")))
	channels[4].SetOtherInfo(map[string]any{
		"quota_domain_id": credentialADomainID,
		"quota_reset_at":  int64(1234),
		"quota_type":      "plan",
	})
	require.NoError(t, db.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}

	resetAt := time.Now().Add(time.Hour).Unix()
	disablePlanQuotaDomain(&channels[3], "credential-b exhausted", resetAt)
	disablePlanQuotaDomain(&channels[0], "credential-a exhausted", resetAt)

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 5)
	domainID, domainOK := stored[0].GetOtherInfo()["quota_domain_id"].(string)
	otherDomainID, otherDomainOK := stored[3].GetOtherInfo()["quota_domain_id"].(string)
	assert.True(t, domainOK)
	assert.Equal(t, credentialADomainID, domainID)
	assert.Equal(t, domainID, stored[1].GetOtherInfo()["quota_domain_id"])
	assert.Equal(t, domainID, stored[2].GetOtherInfo()["quota_domain_id"])
	assert.True(t, otherDomainOK)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("credential-b"))), otherDomainID)
	assert.NotEqual(t, domainID, otherDomainID)

	EnableChannel(channels[0].Id, "", channels[0].Name)

	require.NoError(t, db.Order("id").Find(&stored).Error)
	assert.Equal(t, common.ChannelStatusEnabled, stored[0].Status)
	assert.Equal(t, common.ChannelStatusEnabled, stored[1].Status)
	assert.Equal(t, common.ChannelStatusEnabled, stored[2].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[3].Status)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored[4].Status)
	for _, channel := range stored[:3] {
		info := channel.GetOtherInfo()
		assert.NotContains(t, info, "disabled_until")
		assert.NotContains(t, info, "quota_reset_at")
		assert.NotContains(t, info, "quota_domain")
		assert.NotContains(t, info, "quota_domain_id")
		assert.NotContains(t, info, "quota_type")
	}
	assert.Equal(t, otherDomainID, stored[3].GetOtherInfo()["quota_domain_id"])
	assert.Equal(t, float64(resetAt), stored[3].GetOtherInfo()["quota_reset_at"])
	assert.Equal(t, credentialADomainID, stored[4].GetOtherInfo()["quota_domain_id"])
	assert.Equal(t, float64(1234), stored[4].GetOtherInfo()["quota_reset_at"])

	var abilities []model.Ability
	require.NoError(t, db.Order("channel_id").Find(&abilities).Error)
	require.Len(t, abilities, 5)
	assert.True(t, abilities[0].Enabled)
	assert.True(t, abilities[1].Enabled)
	assert.True(t, abilities[2].Enabled)
	assert.False(t, abilities[3].Enabled)
	assert.False(t, abilities[4].Enabled)
}

func TestLegacyPlanQuotaDomainRecoveryByTag(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	legacyTag := "plan:support:legacy"
	otherTag := "plan:support:other"
	channels := []model.Channel{
		{Id: 31, Name: "legacy-a", Key: "credential-a", Status: common.ChannelStatusAutoDisabled, Tag: &legacyTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 32, Name: "legacy-b", Key: "credential-b", Status: common.ChannelStatusAutoDisabled, Tag: &legacyTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 33, Name: "legacy-manual", Key: "credential-c", Status: common.ChannelStatusManuallyDisabled, Tag: &legacyTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 34, Name: "legacy-other-tag", Key: "credential-d", Status: common.ChannelStatusAutoDisabled, Tag: &otherTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 35, Name: "generic-failure", Key: "credential-e", Status: common.ChannelStatusAutoDisabled, Tag: &legacyTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 36, Name: "wrong-quota-type", Key: "credential-f", Status: common.ChannelStatusAutoDisabled, Tag: &legacyTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 37, Name: "wrong-quota-domain", Key: "credential-g", Status: common.ChannelStatusAutoDisabled, Tag: &legacyTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
	}
	for i := range channels[:4] {
		channels[i].SetOtherInfo(map[string]any{
			"disabled_until":  int64(1234),
			"quota_domain":    channels[i].GetTag(),
			"quota_reset_at":  int64(1174),
			"quota_type":      "plan",
			"preserved_owner": channels[i].Name,
		})
	}
	channels[4].SetOtherInfo(map[string]any{
		"disabled_until":  int64(1234),
		"status_reason":   "generic upstream failure",
		"preserved_owner": channels[4].Name,
	})
	channels[5].SetOtherInfo(map[string]any{
		"disabled_until":  int64(1234),
		"quota_domain":    legacyTag,
		"quota_type":      "provider",
		"preserved_owner": channels[5].Name,
	})
	channels[6].SetOtherInfo(map[string]any{
		"disabled_until":  int64(1234),
		"quota_domain":    otherTag,
		"quota_type":      "plan",
		"preserved_owner": channels[6].Name,
	})
	require.NoError(t, db.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}

	EnableChannel(channels[0].Id, "", channels[0].Name)

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 7)
	assert.Equal(t, common.ChannelStatusEnabled, stored[0].Status)
	assert.Equal(t, common.ChannelStatusEnabled, stored[1].Status)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored[2].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[3].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[4].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[5].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[6].Status)
	for _, channel := range stored[:2] {
		info := channel.GetOtherInfo()
		assert.NotContains(t, info, "disabled_until")
		assert.NotContains(t, info, "quota_reset_at")
		assert.NotContains(t, info, "quota_domain")
		assert.NotContains(t, info, "quota_type")
		assert.Equal(t, channel.Name, info["preserved_owner"])
	}
	assert.Equal(t, float64(1234), stored[2].GetOtherInfo()["disabled_until"])
	assert.Equal(t, "plan", stored[2].GetOtherInfo()["quota_type"])
	assert.Equal(t, float64(1234), stored[3].GetOtherInfo()["disabled_until"])
	assert.Equal(t, "plan", stored[3].GetOtherInfo()["quota_type"])
	assert.Equal(t, map[string]any{
		"disabled_until":  float64(1234),
		"status_reason":   "generic upstream failure",
		"preserved_owner": "generic-failure",
	}, stored[4].GetOtherInfo())
	assert.Equal(t, "provider", stored[5].GetOtherInfo()["quota_type"])
	assert.Equal(t, legacyTag, stored[5].GetOtherInfo()["quota_domain"])
	assert.Equal(t, "plan", stored[6].GetOtherInfo()["quota_type"])
	assert.Equal(t, otherTag, stored[6].GetOtherInfo()["quota_domain"])

	var abilities []model.Ability
	require.NoError(t, db.Order("channel_id").Find(&abilities).Error)
	require.Len(t, abilities, 7)
	assert.True(t, abilities[0].Enabled)
	assert.True(t, abilities[1].Enabled)
	assert.False(t, abilities[2].Enabled)
	assert.False(t, abilities[3].Enabled)
	assert.False(t, abilities[4].Enabled)
	assert.False(t, abilities[5].Enabled)
	assert.False(t, abilities[6].Enabled)
}

func TestDisablePlanQuotaCredentialDomain(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	planTag := "plan:support:coding"
	nativeTag := "plan:support:native"
	ordinaryTag := "ark-ordinary:support"
	sharedDomainID := fmt.Sprintf("%x", sha256.Sum256([]byte("shared")))
	unrelatedDomainID := fmt.Sprintf("%x", sha256.Sum256([]byte("unrelated")))
	channels := []model.Channel{
		{Id: 11, Name: "messages", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &planTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 12, Name: "responses", Key: "shared\n", Status: common.ChannelStatusEnabled, Tag: &planTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 13, Name: "native", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &nativeTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 14, Name: "other-account", Key: "other", Status: common.ChannelStatusEnabled, Tag: &planTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 15, Name: "case-different", Key: "SHARED", Status: common.ChannelStatusEnabled, Tag: &planTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 16, Name: "ordinary", Key: "shared", Status: common.ChannelStatusEnabled, Tag: &ordinaryTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{
			Id:      17,
			Name:    "multi-key",
			Key:     "shared",
			Status:  common.ChannelStatusEnabled,
			Tag:     &planTag,
			AutoBan: &autoBan,
			Models:  "gpt-3.5-turbo",
			Group:   "default",
			ChannelInfo: model.ChannelInfo{
				IsMultiKey:   true,
				MultiKeySize: 1,
			},
		},
		{Id: 18, Name: "manual", Key: "shared", Status: common.ChannelStatusManuallyDisabled, Tag: &planTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 19, Name: "unrelated-auto-disabled", Key: "shared", Status: common.ChannelStatusAutoDisabled, Tag: &planTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 20, Name: "same-domain-auto-disabled", Key: "shared", Status: common.ChannelStatusAutoDisabled, Tag: &nativeTag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
	}
	channels[7].SetOtherInfo(map[string]any{
		"owner":         "manual",
		"status_reason": "operator disabled",
	})
	channels[8].SetOtherInfo(map[string]any{
		"owner":           "other failure",
		"quota_domain_id": unrelatedDomainID,
		"status_reason":   "upstream authentication failed",
	})
	channels[9].SetOtherInfo(map[string]any{
		"owner":           "same quota domain",
		"quota_domain_id": sharedDomainID,
		"quota_type":      "plan",
	})
	require.NoError(t, db.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}
	require.NoError(t, db.Model(&model.Ability{}).
		Where("channel_id IN ?", []int{18, 19}).
		Update("enabled", true).Error)

	resetAt := time.Now().Add(time.Hour).Unix()
	disablePlanQuotaDomain(&channels[0], "quota exhausted", resetAt)

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 10)
	for i, channel := range stored {
		if channel.Id >= 11 && channel.Id <= 13 || channel.Id == 20 {
			assert.Equal(t, common.ChannelStatusAutoDisabled, channel.Status)
			assert.Equal(t, resetAt+60, channel.GetDisabledUntil())
			assert.Equal(t, channels[i].GetTag(), channel.GetOtherInfo()["quota_domain"])
			assert.Equal(t, sharedDomainID, channel.GetOtherInfo()["quota_domain_id"])
			continue
		}
		if channel.Id == 18 {
			assert.Equal(t, common.ChannelStatusManuallyDisabled, channel.Status)
			assert.Equal(t, map[string]any{
				"owner":         "manual",
				"status_reason": "operator disabled",
			}, channel.GetOtherInfo())
			continue
		}
		if channel.Id == 19 {
			assert.Equal(t, common.ChannelStatusAutoDisabled, channel.Status)
			assert.Equal(t, map[string]any{
				"owner":           "other failure",
				"quota_domain_id": unrelatedDomainID,
				"status_reason":   "upstream authentication failed",
			}, channel.GetOtherInfo())
			continue
		}
		assert.Equal(t, common.ChannelStatusEnabled, channel.Status)
		assert.Zero(t, channel.GetDisabledUntil())
		assert.NotContains(t, channel.GetOtherInfo(), "quota_domain")
	}

	var abilities []model.Ability
	require.NoError(t, db.Order("channel_id").Find(&abilities).Error)
	require.Len(t, abilities, 10)
	for _, ability := range abilities {
		if ability.ChannelId >= 11 && ability.ChannelId <= 13 || ability.ChannelId == 20 {
			assert.False(t, ability.Enabled)
			continue
		}
		assert.True(t, ability.Enabled)
	}
}

func TestEnablePlanQuotaDomainAfterCredentialRotation(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	tag := "plan:support:rotation"
	channels := []model.Channel{
		{Id: 41, Name: "rotated", Key: "credential-a", Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
		{Id: 42, Name: "old-peer", Key: "credential-a", Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan, Models: "gpt-3.5-turbo", Group: "default"},
	}
	require.NoError(t, db.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}

	resetAt := time.Now().Add(time.Hour).Unix()
	disablePlanQuotaDomain(&channels[0], "credential-a exhausted", resetAt)
	require.NoError(t, db.Model(&model.Channel{}).
		Where("id = ?", channels[0].Id).
		Update("key", "credential-b").Error)

	EnableChannel(channels[0].Id, "", channels[0].Name)

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 2)
	assert.Equal(t, common.ChannelStatusEnabled, stored[0].Status)
	assert.Equal(t, "credential-b", stored[0].Key)
	assert.NotContains(t, stored[0].GetOtherInfo(), "quota_domain_id")
	assert.NotContains(t, stored[0].GetOtherInfo(), "quota_type")
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[1].Status)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("credential-a"))), stored[1].GetOtherInfo()["quota_domain_id"])
	assert.Equal(t, "plan", stored[1].GetOtherInfo()["quota_type"])

	var abilities []model.Ability
	require.NoError(t, db.Order("channel_id").Find(&abilities).Error)
	require.Len(t, abilities, 2)
	assert.True(t, abilities[0].Enabled)
	assert.False(t, abilities[1].Enabled)
}

func TestFreshPlanQuotaDisableFencesStaleRecoverySnapshot(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	tag := "plan:support:generation"
	channel := model.Channel{
		Id:      43,
		Name:    "generation",
		Key:     "credential",
		Status:  common.ChannelStatusEnabled,
		Tag:     &tag,
		AutoBan: &autoBan,
		Models:  "gpt-3.5-turbo",
		Group:   "default",
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	const resetAt = int64(2_000_000_000)
	disablePlanQuotaDomain(&channel, "quota exhausted", resetAt)

	var staleRecoverySnapshot model.Channel
	require.NoError(t, db.First(&staleRecoverySnapshot, channel.Id).Error)
	firstGeneration, firstGenerationOK := staleRecoverySnapshot.GetOtherInfo()["quota_generation"].(string)

	disablePlanQuotaDomain(&staleRecoverySnapshot, "quota exhausted", resetAt)

	var freshlyDisabled model.Channel
	require.NoError(t, db.First(&freshlyDisabled, channel.Id).Error)
	secondGeneration, secondGenerationOK := freshlyDisabled.GetOtherInfo()["quota_generation"].(string)
	assert.True(t, firstGenerationOK)
	assert.True(t, secondGenerationOK)
	assert.NotEqual(t, firstGeneration, secondGeneration)

	expectedOtherInfo := staleRecoverySnapshot.OtherInfo
	recoveryMetadata := staleRecoverySnapshot.GetOtherInfo()
	recoveryMetadata["status_reason"] = ""
	recoveryMetadata["status_time"] = common.GetTimestamp()
	delete(recoveryMetadata, "disabled_until")
	delete(recoveryMetadata, "quota_reset_at")
	delete(recoveryMetadata, "quota_domain")
	delete(recoveryMetadata, "quota_domain_id")
	delete(recoveryMetadata, "quota_generation")
	delete(recoveryMetadata, "quota_type")
	staleRecoverySnapshot.SetOtherInfo(recoveryMetadata)

	changed, err := model.UpdateSingleKeyChannelStatusIfUnchanged(
		staleRecoverySnapshot.Id,
		staleRecoverySnapshot.Key,
		staleRecoverySnapshot.GetTag(),
		staleRecoverySnapshot.Status,
		expectedOtherInfo,
		common.ChannelStatusEnabled,
		staleRecoverySnapshot.OtherInfo,
	)
	require.NoError(t, err)
	assert.False(t, changed)

	var ability model.Ability
	require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
	require.NoError(t, db.First(&freshlyDisabled, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, freshlyDisabled.Status)
	assert.Equal(t, secondGeneration, freshlyDisabled.GetOtherInfo()["quota_generation"])
	assert.False(t, ability.Enabled)
}

func TestDisablePlanQuotaDomainPreservesConcurrentOwnership(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	tag := "plan:support:stale"
	channel := model.Channel{
		Id:      43,
		Name:    "stale-snapshot",
		Status:  common.ChannelStatusEnabled,
		Tag:     &tag,
		AutoBan: &autoBan,
		Models:  "gpt-3.5-turbo",
		Group:   "default",
	}
	channel.SetOtherInfo(map[string]any{"owner": "snapshot"})
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	var stale model.Channel
	require.NoError(t, db.First(&stale, "id = ?", channel.Id).Error)
	concurrent := stale
	concurrent.SetOtherInfo(map[string]any{
		"owner":         "operator",
		"status_reason": "manual operation",
	})
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
		"status":     common.ChannelStatusManuallyDisabled,
		"other_info": concurrent.OtherInfo,
	}).Error)
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).
		Update("enabled", false).Error)

	disablePlanQuotaDomain(&stale, "quota exhausted", 2_000_000_000)

	var stored model.Channel
	require.NoError(t, db.First(&stored, "id = ?", channel.Id).Error)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status)
	assert.Equal(t, concurrent.OtherInfo, stored.OtherInfo)

	var ability model.Ability
	require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
}

func TestDisablePlanQuotaDomainStaleCASDoesNotMutateCachedMetadata(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	tag := "plan:support:cached-stale"
	channel := model.Channel{
		Id:      44,
		Name:    "cached-stale-snapshot",
		Status:  common.ChannelStatusEnabled,
		Tag:     &tag,
		AutoBan: &autoBan,
		Models:  "gpt-3.5-turbo",
		Group:   "default",
	}
	channel.SetOtherInfo(map[string]any{"owner": "cached-snapshot"})
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	common.MemoryCacheEnabled = true
	model.InitChannelCache()
	cached, err := model.CacheGetChannel(channel.Id)
	require.NoError(t, err)
	cachedOtherInfo := cached.OtherInfo

	concurrent := channel
	concurrent.SetOtherInfo(map[string]any{"owner": "concurrent-update"})
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).
		Update("other_info", concurrent.OtherInfo).Error)

	disablePlanQuotaDomain(cached, "quota exhausted", 2_000_000_000)

	cachedAfter, err := model.CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, cachedOtherInfo, cachedAfter.OtherInfo)
	assert.Equal(t, common.ChannelStatusEnabled, cachedAfter.Status)

	var stored model.Channel
	require.NoError(t, db.First(&stored, "id = ?", channel.Id).Error)
	assert.Equal(t, concurrent.OtherInfo, stored.OtherInfo)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)

	var ability model.Ability
	require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.True(t, ability.Enabled)
}

func TestDisablePlanQuotaCredentialDomainHandlesMissingCredential(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		useNilChannel    bool
		channelID        int
		channelKey       string
		channelTagName   string
		expectedStatus   int
		expectedDomainID string
	}{
		{
			name:           "nil channel",
			useNilChannel:  true,
			channelID:      21,
			channelTagName: "plan:support:nil",
			expectedStatus: common.ChannelStatusEnabled,
		},
		{
			name:             "empty credential",
			channelID:        22,
			channelTagName:   "plan:support:empty",
			expectedStatus:   common.ChannelStatusAutoDisabled,
			expectedDomainID: "channel:22",
		},
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
			resetAt := time.Now().Add(time.Hour).Unix()
			disablePlanQuotaDomain(failingChannel, "quota exhausted", resetAt)

			var stored model.Channel
			require.NoError(t, db.First(&stored, "id = ?", channel.Id).Error)
			assert.Equal(t, testCase.expectedStatus, stored.Status)

			var ability model.Ability
			require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
			if testCase.useNilChannel {
				assert.Zero(t, stored.GetDisabledUntil())
				assert.NotContains(t, stored.GetOtherInfo(), "quota_domain")
				assert.NotContains(t, stored.GetOtherInfo(), "quota_domain_id")
				assert.True(t, ability.Enabled)
				return
			}
			assert.Equal(t, resetAt+60, stored.GetDisabledUntil())
			assert.Equal(t, testCase.channelTagName, stored.GetOtherInfo()["quota_domain"])
			assert.Equal(t, testCase.expectedDomainID, stored.GetOtherInfo()["quota_domain_id"])
			assert.Equal(t, "plan", stored.GetOtherInfo()["quota_type"])
			assert.Equal(t, float64(resetAt), stored.GetOtherInfo()["quota_reset_at"])
			assert.False(t, ability.Enabled)
		})
	}
}

func TestDisableChannelPreservesPlanMultiKeyIsolation(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	autoBan := 1
	tag := "plan:support:multi-key"
	channel := model.Channel{
		Id:      41,
		Name:    "multi-key",
		Key:     "key-a\nkey-b",
		Status:  common.ChannelStatusEnabled,
		Tag:     &tag,
		AutoBan: &autoBan,
		Models:  "gpt-3.5-turbo",
		Group:   "default",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
		},
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	DisableChannel(types.ChannelError{
		ChannelId:   channel.Id,
		ChannelName: channel.Name,
		IsMultiKey:  true,
		AutoBan:     true,
		UsingKey:    "key-a",
	}, "You have exceeded the monthly usage quota. It will reset at 2026-09-30 23:59:59 +0800 CST.")

	var stored model.Channel
	require.NoError(t, db.First(&stored, "id = ?", channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.NotContains(t, stored.ChannelInfo.MultiKeyStatusList, 1)
	assert.NotContains(t, stored.GetOtherInfo(), "quota_domain_id")

	var ability model.Ability
	require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.True(t, ability.Enabled)
}

func TestDisableChannelManagedPlanQuotaIsolatesCredentialDomain(t *testing.T) {
	db := setupPlanQuotaDomainTest(t)

	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.FailureThreshold = 3
	setting.FailureWindowMinutes = 5
	t.Cleanup(func() {
		*setting = originalSetting
	})

	autoBan := 1
	messagesTag := "plan:managed:messages"
	responsesTag := "plan:managed:responses"
	channels := []model.Channel{
		{
			Id: 61, Name: "managed-messages", Key: "shared-secret",
			Status: common.ChannelStatusEnabled, Tag: &messagesTag, AutoBan: &autoBan,
			Models: "gpt-3.5-turbo", Group: "default",
		},
		{
			Id: 62, Name: "managed-responses", Key: "shared-secret",
			Status: common.ChannelStatusEnabled, Tag: &responsesTag, AutoBan: &autoBan,
			Models: "gpt-3.5-turbo", Group: "default",
		},
	}
	require.NoError(t, db.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}
	route := model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "managed-plan",
		Platform:        "plan",
		Protocol:        "responses",
		ChannelID:       channels[0].Id,
		State:           model.UpstreamRouteStateActive,
	}
	require.NoError(t, db.Create(&route).Error)

	DisableChannel(types.ChannelError{
		ChannelId:   channels[0].Id,
		ChannelName: channels[0].Name,
		AutoBan:     true,
	}, "You have exceeded the monthly usage quota. It will reset at 2026-09-30 23:59:59 +0800 CST.")

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 2)
	firstInfo := stored[0].GetOtherInfo()
	secondInfo := stored[1].GetOtherInfo()
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[0].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[1].Status)
	assert.Equal(t, firstInfo["quota_domain_id"], secondInfo["quota_domain_id"])
	assert.NotEmpty(t, firstInfo["quota_generation"])
	assert.Equal(t, firstInfo["quota_generation"], secondInfo["quota_generation"])
	assert.NotContains(t, stored[0].OtherInfo, "shared-secret")
	assert.NotContains(t, stored[1].OtherInfo, "shared-secret")

	var abilities []model.Ability
	require.NoError(t, db.Order("channel_id").Find(&abilities).Error)
	require.Len(t, abilities, 2)
	assert.False(t, abilities[0].Enabled)
	assert.False(t, abilities[1].Enabled)

	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, db.First(&storedRoute, route.ID).Error)
	assert.Zero(t, storedRoute.ConsecutiveFailures)
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
