package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagedSourceDiversityWithinSamePriority(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	require.NoError(t, db.AutoMigrate(&model.UpstreamManagedRoute{}))
	const modelName = "gpt-5.4"
	createChannelSelectPriorityChannel(t, db, 62, "default", modelName, 998)
	createChannelSelectPriorityChannel(t, db, 54, "default", modelName, 0)
	createChannelSelectPriorityChannel(t, db, 60, "default", modelName, 0)
	for _, route := range []model.UpstreamManagedRoute{
		{SourceID: 1, ExternalGroupID: "25", Platform: "openai", Protocol: "openai", ChannelID: 62, State: model.UpstreamRouteStateActive},
		{SourceID: 1, ExternalGroupID: "31", Platform: "openai", Protocol: "openai", ChannelID: 54, State: model.UpstreamRouteStateActive},
		{SourceID: 2, ExternalGroupID: "55", Platform: "openai", Protocol: "openai", ChannelID: 60, State: model.UpstreamRouteStateActive},
	} {
		require.NoError(t, db.Create(&route).Error)
	}
	model.InitChannelCache()

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	param := &RetryParam{
		Ctx:        ctx,
		TokenGroup: "default",
		ModelName:  modelName,
	}

	assert.Equal(t, 2, AdaptiveRetryTimes(param))
	assert.Equal(t, []int64{998, 0, 0}, param.PriorityPath)

	first, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, 62, first.Id)

	param.IncreaseRetry()
	second, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, 60, second.Id)

	param.IncreaseRetry()
	third, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, third)
	assert.Equal(t, 54, third.Id)
}

func TestManagedModelUnsupportedDetection(t *testing.T) {
	unsupported := relaytypes.NewOpenAIError(
		errors.New(`Model "gpt-5.4" is not supported by any configured account in this group`),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusNotFound,
	)
	ordinary := relaytypes.NewOpenAIError(
		errors.New("not found"),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusNotFound,
	)

	assert.True(t, IsManagedModelUnsupported(unsupported))
	assert.True(t, ShouldRecordManagedRouteFailure(unsupported))
	assert.False(t, IsManagedModelUnsupported(ordinary))
	assert.False(t, ShouldRecordManagedRouteFailure(ordinary))
}

func TestIsolateManagedRouteModelOnlyRemovesFailedModel(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	require.NoError(t, db.AutoMigrate(
		&model.UpstreamGroup{},
		&model.UpstreamManagedRoute{},
	))
	priority := int64(998)
	weight := uint(100)
	channel := model.Channel{
		Id:       62,
		Status:   common.ChannelStatusEnabled,
		Name:     "ebond",
		Models:   "gpt-5.4,gpt-5.5",
		Group:    "default",
		Priority: &priority,
		Weight:   &weight,
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	group := model.UpstreamGroup{
		SourceID:            1,
		ExternalID:          "25",
		Name:                "GPT Pro",
		Platform:            "openai",
		EffectiveMultiplier: 0.2,
		HealthStatus:        model.UpstreamHealthUnknown,
		Models:              `["gpt-5.4","gpt-5.5"]`,
	}
	require.NoError(t, db.Create(&group).Error)
	require.NoError(t, db.Create(&model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "25",
		Platform:        "openai",
		Protocol:        "openai",
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateActive,
	}).Error)
	model.InitChannelCache()

	isolated, err := IsolateManagedRouteModel(
		channel.Id,
		"gpt-5.4",
		"status_code=404, unsupported",
	)
	require.NoError(t, err)
	assert.True(t, isolated)

	var storedChannel model.Channel
	require.NoError(t, db.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, "gpt-5.5", storedChannel.Models)
	var failedAbilityCount int64
	require.NoError(t, db.Model(&model.Ability{}).
		Where("channel_id = ? AND model = ?", channel.Id, "gpt-5.4").
		Count(&failedAbilityCount).Error)
	assert.Zero(t, failedAbilityCount)
	var retainedAbilityCount int64
	require.NoError(t, db.Model(&model.Ability{}).
		Where("channel_id = ? AND model = ?", channel.Id, "gpt-5.5").
		Count(&retainedAbilityCount).Error)
	assert.Equal(t, int64(1), retainedAbilityCount)

	var storedGroup model.UpstreamGroup
	require.NoError(t, db.First(&storedGroup, group.ID).Error)
	assert.Equal(t, `["gpt-5.5"]`, storedGroup.Models)
}
