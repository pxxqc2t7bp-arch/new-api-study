package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupUpstreamOrchestrationTest(t *testing.T) {
	t.Helper()
	originalDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.UpstreamSource{},
		&model.UpstreamGroup{},
		&model.UpstreamManagedRoute{},
		&model.UpstreamMetricSnapshot{},
		&model.UpstreamSyncDevice{},
		&model.UpstreamSyncBatch{},
		&model.UpstreamSyncCommand{},
		&model.Vendor{},
		&model.Model{},
	))
	model.DB = db
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func TestNormalizeUpstreamEndpoints(t *testing.T) {
	t.Run("selects the fastest healthy allowlisted endpoint", func(t *testing.T) {
		slow := int64(50)
		fast := int64(10)
		healthy := true
		endpoints, selected, err := normalizeUpstreamEndpoints(
			model.UpstreamSourceKeyLeyi,
			"https://leyi12.xyz",
			[]dto.UpstreamEndpointSnapshot{
				{Name: "slow", URL: "https://api.leyiapi.com/", LatencyMS: &slow, Healthy: &healthy},
				{Name: "fast", URL: "https://leyiapi.com", LatencyMS: &fast, Healthy: &healthy},
			},
		)
		require.NoError(t, err)
		assert.Len(t, endpoints, 3)
		assert.Equal(t, "https://leyiapi.com", selected)
	})

	t.Run("rejects endpoints outside the source allowlist", func(t *testing.T) {
		_, _, err := normalizeUpstreamEndpoints(
			model.UpstreamSourceKeyLeyi,
			"",
			[]dto.UpstreamEndpointSnapshot{{URL: "https://example.com"}},
		)
		assert.Error(t, err)
	})
}

func TestIngestUpstreamSnapshot(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	now := common.GetTimestamp()
	device := &model.UpstreamSyncDevice{
		DeviceID: "device-one",
		Status:   model.UpstreamSyncDeviceActive,
	}

	t.Run("stores a valid source group and metric once", func(t *testing.T) {
		multiplier := 0.15
		availability := 99.5
		latency := int64(1500)
		snapshot := dto.UpstreamSyncSnapshot{
			SchemaVersion: upstreamSnapshotSchemaVersion,
			SnapshotID:    "snapshot-valid",
			DeviceID:      device.DeviceID,
			CapturedAt:    now,
			Sources: []dto.UpstreamSourceSnapshot{{
				Key:            model.UpstreamSourceKeyHualong,
				Name:           "Hualong",
				ConsoleURL:     "https://api.hualong.online",
				APIBaseURL:     "https://api-fast.hualong.online",
				Status:         model.UpstreamHealthOperational,
				Balance:        floatPointer(25),
				AdapterVersion: "sub2api-1",
				Groups: []dto.UpstreamGroupSnapshot{{
					ExternalID:         "group-1",
					Name:               "GPT Pro",
					Platform:           "openai",
					RateMultiplier:     0.2,
					UserRateMultiplier: &multiplier,
					HealthStatus:       model.UpstreamHealthOperational,
					Availability:       &availability,
					LatencyMS:          &latency,
					Models:             []dto.UpstreamModelSnapshot{{Name: "gpt-5.5"}},
				}},
			}},
		}

		result, err := IngestUpstreamSnapshot(device, snapshot)
		require.NoError(t, err)
		assert.Equal(t, 1, result.Sources)
		assert.Equal(t, 1, result.Groups)
		assert.Equal(t, 1, result.Metrics)

		result, err = IngestUpstreamSnapshot(device, snapshot)
		require.NoError(t, err)
		assert.Zero(t, result.Sources)
		var count int64
		require.NoError(t, model.DB.Model(&model.UpstreamMetricSnapshot{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})

	t.Run("failed partial snapshot preserves last valid operational values", func(t *testing.T) {
		source := model.UpstreamSource{
			Key:                 model.UpstreamSourceKeyEBond,
			Name:                "EBond",
			ConsoleURL:          "https://ebondai.com",
			EndpointCandidates:  `[{"name":"default","url":"https://api.ebondai.com"}]`,
			SelectedEndpoint:    "https://api.ebondai.com",
			Status:              model.UpstreamHealthOperational,
			Enabled:             false,
			Balance:             floatPointer(12),
			LowBalanceThreshold: 7,
			LastSnapshotID:      "old",
			LastSnapshotAt:      now - 60,
			LastSuccessAt:       now - 60,
		}
		require.NoError(t, model.DB.Create(&source).Error)

		_, err := IngestUpstreamSnapshot(device, dto.UpstreamSyncSnapshot{
			SchemaVersion: upstreamSnapshotSchemaVersion,
			SnapshotID:    "snapshot-error",
			DeviceID:      device.DeviceID,
			CapturedAt:    now,
			Sources: []dto.UpstreamSourceSnapshot{{
				Key:        model.UpstreamSourceKeyEBond,
				Name:       "EBond",
				ConsoleURL: "https://ebondai.com",
				Status:     model.UpstreamHealthError,
				Error:      "login required",
			}},
		})
		require.NoError(t, err)

		stored, err := model.GetUpstreamSourceByKey(model.UpstreamSourceKeyEBond)
		require.NoError(t, err)
		assert.False(t, stored.Enabled)
		assert.Equal(t, "https://api.ebondai.com", stored.SelectedEndpoint)
		require.NotNil(t, stored.Balance)
		assert.Equal(t, 12.0, *stored.Balance)
		assert.Equal(t, now-60, stored.LastSuccessAt)
		assert.Equal(t, "login required", stored.LastError)
		assert.Equal(t, model.UpstreamHealthError, stored.Status)
	})
}

func TestManagedAdvancedCustomConfigUsesUpstreamProtocol(t *testing.T) {
	t.Run("OpenAI-compatible upstream", func(t *testing.T) {
		payload := dto.UpstreamEnrollmentCommand{Platform: "openai"}

		openAI := managedAdvancedCustomConfig(payload, model.UpstreamProtocolOpenAI)
		require.Len(t, openAI.Routes, 2)
		assert.Equal(t, "/v1/chat/completions", openAI.Routes[0].UpstreamPath)
		assert.Equal(t, relayconvert.ConverterNone, openAI.Routes[0].Converter)
		assert.Equal(t, "/v1/responses", openAI.Routes[1].UpstreamPath)
		assert.Equal(t, relayconvert.ConverterNone, openAI.Routes[1].Converter)

		anthropic := managedAdvancedCustomConfig(payload, model.UpstreamProtocolAnthropic)
		require.Len(t, anthropic.Routes, 1)
		assert.Equal(t, "/v1/chat/completions", anthropic.Routes[0].UpstreamPath)
		assert.Equal(t, relayconvert.ConverterClaudeMessagesToOpenAIChat, anthropic.Routes[0].Converter)
	})

	t.Run("Anthropic upstream", func(t *testing.T) {
		payload := dto.UpstreamEnrollmentCommand{Platform: "anthropic"}

		openAI := managedAdvancedCustomConfig(payload, model.UpstreamProtocolOpenAI)
		require.Len(t, openAI.Routes, 2)
		assert.Equal(t, "/v1/messages", openAI.Routes[0].UpstreamPath)
		assert.Equal(t, relayconvert.ConverterOpenAIChatToClaudeMessages, openAI.Routes[0].Converter)
		assert.Equal(t, "/v1/chat/completions", openAI.Routes[1].UpstreamPath)
		assert.Equal(t, relayconvert.ConverterOpenAIResponsesToOpenAIChat, openAI.Routes[1].Converter)

		anthropic := managedAdvancedCustomConfig(payload, model.UpstreamProtocolAnthropic)
		require.Len(t, anthropic.Routes, 1)
		assert.Equal(t, "/v1/messages", anthropic.Routes[0].UpstreamPath)
		assert.Equal(t, relayconvert.ConverterNone, anthropic.Routes[0].Converter)
	})
}

func TestManagedTextModelFilter(t *testing.T) {
	assert.True(t, isManagedTextModel("gpt-5.6-sol", "openai"))
	assert.True(t, isManagedTextModel("claude-opus-5", "anthropic"))
	assert.True(t, isManagedTextModel("grok-4.6", "grok"))
	assert.False(t, isManagedTextModel("gpt-image-2", "openai"))
	assert.False(t, isManagedTextModel("grok-imagine-video-1.5", "grok"))
}

func TestDesiredManagedRouteStatePromotesValidatedShadow(t *testing.T) {
	now := time.Unix(1_788_320_000, 0)
	setting := &operation_setting.UpstreamOrchestrationSetting{
		SyncIntervalHours:       4,
		ShadowSuccessesRequired: 3,
	}
	source := model.UpstreamSource{
		Enabled:        true,
		Status:         model.UpstreamHealthOperational,
		LastSnapshotAt: now.Unix(),
	}
	group := model.UpstreamGroup{
		HealthStatus: model.UpstreamHealthOperational,
		ObservedAt:   now.Unix(),
	}

	state, reason := desiredManagedRouteState(
		model.UpstreamManagedRoute{
			State:                model.UpstreamRouteStateShadow,
			ConsecutiveSuccesses: 3,
		},
		source,
		group,
		now,
		setting,
	)

	assert.Equal(t, model.UpstreamRouteStateActive, state)
	assert.Empty(t, reason)
}

func TestShadowProbeDoesNotEnableChannelBeforeOrchestration(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	original := *setting
	setting.Enabled = false
	setting.ShadowSuccessesRequired = 3
	t.Cleanup(func() {
		*setting = original
	})

	route := model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "20",
		Platform:        "openai",
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       999,
		State:           model.UpstreamRouteStateShadow,
		NextProbeAt:     common.GetTimestamp(),
	}
	require.NoError(t, model.DB.Create(&route).Error)

	for attempt := 1; attempt <= 3; attempt++ {
		require.NoError(t, MarkManagedRouteProbeResult(route.ID, true, int64(attempt), ""))
	}

	var stored model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&stored, route.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateShadow, stored.State)
	assert.Equal(t, 3, stored.ConsecutiveSuccesses)
}

func TestPrepareManagedUpstreamShadowsIsIdempotent(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	original := *setting
	setting.AutoEnroll = true
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4.1":1}`))
	t.Cleanup(func() {
		*setting = original
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
	})

	source := model.UpstreamSource{
		Key:              model.UpstreamSourceKeyHualong,
		Name:             "Hualong",
		ConsoleURL:       "https://api.hualong.online",
		SelectedEndpoint: "https://api.hualong.online",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotAt:   now.Unix(),
		LastSuccessAt:    now.Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	require.NoError(t, model.DB.Create(&model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "20",
		Name:                "GPT Pro",
		Platform:            "openai",
		BaseMultiplier:      0.15,
		EffectiveMultiplier: 0.15,
		HealthStatus:        model.UpstreamHealthOperational,
		Models:              `["gpt-4.1"]`,
		ObservedAt:          now.Unix(),
	}).Error)

	first, err := PrepareManagedUpstreamShadows(now)
	require.NoError(t, err)
	assert.Equal(t, 1, first.EnrollmentQueued)

	second, err := PrepareManagedUpstreamShadows(now)
	require.NoError(t, err)
	assert.Zero(t, second.EnrollmentQueued)

	var commandCount int64
	require.NoError(t, model.DB.Model(&model.UpstreamSyncCommand{}).Count(&commandCount).Error)
	assert.EqualValues(t, 1, commandCount)
}

func floatPointer(value float64) *float64 {
	return &value
}
