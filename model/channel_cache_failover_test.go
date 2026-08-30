package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetRandomSatisfiedChannelVisitsEachPriorityOnce(t *testing.T) {
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalGroups := group2model2channels
	originalChannels := channelsIDM
	originalConfigs := channel2advancedCustomConfig
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		group2model2channels = originalGroups
		channelsIDM = originalChannels
		channel2advancedCustomConfig = originalConfigs
	})

	priority30 := int64(30)
	priority20 := int64(20)
	priority10 := int64(10)
	weight := uint(1)
	channelsIDM = map[int]*Channel{
		1: {Id: 1, Priority: &priority30, Weight: &weight},
		2: {Id: 2, Priority: &priority30, Weight: &weight},
		3: {Id: 3, Priority: &priority20, Weight: &weight},
		4: {Id: 4, Priority: &priority10, Weight: &weight},
	}
	group2model2channels = map[string]map[string][]int{
		"default": {"model-a": {1, 2, 3, 4}},
	}
	channel2advancedCustomConfig = map[int]*dto.AdvancedCustomConfig{}

	for retry, wantPriority := range []int64{30, 20, 10} {
		channel, err := GetRandomSatisfiedChannel("default", "model-a", retry, "")
		require.NoError(t, err)
		require.NotNil(t, channel)
		assert.Equal(t, wantPriority, channel.GetPriority())
	}

	channel, err := GetRandomSatisfiedChannel("default", "model-a", 3, "")
	require.NoError(t, err)
	assert.Nil(t, channel)

	count, err := CountSatisfiedChannelPriorities("default", "model-a", "")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

func TestGetRandomSatisfiedChannelDoesNotRepeatSinglePriority(t *testing.T) {
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalGroups := group2model2channels
	originalChannels := channelsIDM
	originalConfigs := channel2advancedCustomConfig
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		group2model2channels = originalGroups
		channelsIDM = originalChannels
		channel2advancedCustomConfig = originalConfigs
	})

	priority := int64(10)
	weight := uint(1)
	channelsIDM = map[int]*Channel{1: {Id: 1, Priority: &priority, Weight: &weight}}
	group2model2channels = map[string]map[string][]int{"default": {"model-a": {1}}}
	channel2advancedCustomConfig = map[int]*dto.AdvancedCustomConfig{}

	channel, err := GetRandomSatisfiedChannel("default", "model-a", 1, "")
	require.NoError(t, err)
	assert.Nil(t, channel)
}

func TestGetChannelFiltersPathBeforeChoosingPriority(t *testing.T) {
	setupChannelStatusTest(t)
	modelName := "path-aware-model"
	highPriority := int64(30)
	lowPriority := int64(20)
	weight := uint(100)
	high := Channel{
		Id:       101,
		Type:     constant.ChannelTypeAdvancedCustom,
		Key:      "high",
		Status:   common.ChannelStatusEnabled,
		Name:     "responses-only",
		Weight:   &weight,
		Models:   modelName,
		Group:    "default",
		Priority: &highPriority,
		OtherSettings: `{"advanced_custom":{"advanced_routes":[` +
			`{"incoming_path":"/v1/responses","upstream_path":"/v1/responses","converter":"none"}` +
			`]}}`,
	}
	low := Channel{
		Id:       102,
		Type:     constant.ChannelTypeAdvancedCustom,
		Key:      "low",
		Status:   common.ChannelStatusEnabled,
		Name:     "messages",
		Weight:   &weight,
		Models:   modelName,
		Group:    "default",
		Priority: &lowPriority,
		OtherSettings: `{"advanced_custom":{"advanced_routes":[` +
			`{"incoming_path":"/v1/messages","upstream_path":"/v1/messages","converter":"none"}` +
			`]}}`,
	}
	require.NoError(t, DB.Create(&high).Error)
	require.NoError(t, DB.Create(&low).Error)
	require.NoError(t, high.AddAbilities(nil))
	require.NoError(t, low.AddAbilities(nil))

	channel, err := GetChannel("default", modelName, 0, "/v1/messages")

	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, low.Id, channel.Id)
}
