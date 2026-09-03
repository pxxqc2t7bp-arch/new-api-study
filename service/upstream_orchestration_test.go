package service

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
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

func TestManagedAdvancedCustomConfigPrefersNativeProtocols(t *testing.T) {
	for _, platform := range []string{"openai", "anthropic", "grok"} {
		t.Run(platform, func(t *testing.T) {
			payload := dto.UpstreamEnrollmentCommand{Platform: platform}

			openAI := managedAdvancedCustomConfig(payload, model.UpstreamProtocolOpenAI)
			require.Len(t, openAI.Routes, 2)
			assert.Equal(t, "/v1/chat/completions", openAI.Routes[0].UpstreamPath)
			assert.Equal(t, relayconvert.ConverterNone, openAI.Routes[0].Converter)
			assert.Equal(t, "/v1/responses", openAI.Routes[1].UpstreamPath)
			assert.Equal(t, relayconvert.ConverterNone, openAI.Routes[1].Converter)

			anthropic := managedAdvancedCustomConfig(payload, model.UpstreamProtocolAnthropic)
			require.Len(t, anthropic.Routes, 1)
			assert.Equal(t, "/v1/messages", anthropic.Routes[0].UpstreamPath)
			assert.Equal(t, relayconvert.ConverterNone, anthropic.Routes[0].Converter)
		})
	}

	t.Run("explicit fallback overrides", func(t *testing.T) {
		payload := dto.UpstreamEnrollmentCommand{
			Platform:           "openai",
			ResponsesPath:      "/v1/chat/completions",
			ResponsesConverter: relayconvert.ConverterOpenAIResponsesToOpenAIChat,
			MessagesPath:       "/v1/chat/completions",
			MessagesConverter:  relayconvert.ConverterClaudeMessagesToOpenAIChat,
		}

		openAI := managedAdvancedCustomConfig(payload, model.UpstreamProtocolOpenAI)
		assert.Equal(t, payload.ResponsesPath, openAI.Routes[1].UpstreamPath)
		assert.Equal(t, payload.ResponsesConverter, openAI.Routes[1].Converter)

		anthropic := managedAdvancedCustomConfig(payload, model.UpstreamProtocolAnthropic)
		assert.Equal(t, payload.MessagesPath, anthropic.Routes[0].UpstreamPath)
		assert.Equal(t, payload.MessagesConverter, anthropic.Routes[0].Converter)
	})
}

func TestManagedTextModelFilter(t *testing.T) {
	assert.True(t, isManagedTextModel("gpt-5.6-sol", "openai"))
	assert.True(t, isManagedTextModel("claude-opus-5", "anthropic"))
	assert.True(t, isManagedTextModel("grok-4.6", "grok"))
	assert.False(t, isManagedTextModel("gpt-image-2", "openai"))
	assert.False(t, isManagedTextModel("grok-imagine", "grok"))
	assert.False(t, isManagedTextModel("grok-imagine-video-1.5", "grok"))
}

func TestManagedModelExcludedIsScopedToSourceGroup(t *testing.T) {
	exclusions := map[string][]string{
		"hualong:21": {"claude-haiku-4-5-20251001"},
	}

	assert.True(t, managedModelExcluded(
		"Hualong",
		"21",
		"claude-haiku-4-5-20251001",
		"claude-haiku-4-5-20251001",
		exclusions,
	))
	assert.False(t, managedModelExcluded(
		"ebond",
		"21",
		"claude-haiku-4-5-20251001",
		"claude-haiku-4-5-20251001",
		exclusions,
	))
}

func TestManagedRouteUsesNativeProtocol(t *testing.T) {
	nativeOpenAI := model.Channel{OtherSettings: `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/chat/completions","upstream_path":"/v1/chat/completions","converter":"none"},{"incoming_path":"/v1/responses","upstream_path":"/v1/responses","converter":"none"}]}}`}
	convertedOpenAI := model.Channel{OtherSettings: `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/chat/completions","upstream_path":"/v1/messages","converter":"openai_chat_completions_to_anthropic_messages"}]}}`}
	nativeMessages := model.Channel{OtherSettings: `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/messages","upstream_path":"/v1/messages","converter":"none"}]}}`}
	mixedMessages := model.Channel{OtherSettings: `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/messages","upstream_path":"/v1/messages","converter":"none","models":["gpt-native"]},{"incoming_path":"/v1/messages","upstream_path":"/v1/chat/completions","converter":"anthropic_messages_to_openai_chat_completions","models":["gpt-fallback"]}]}}`}

	assert.True(t, managedRouteUsesNativeProtocol(
		nativeOpenAI,
		model.UpstreamProtocolOpenAI,
	))
	assert.False(t, managedRouteUsesNativeProtocol(
		convertedOpenAI,
		model.UpstreamProtocolOpenAI,
	))
	assert.True(t, managedRouteUsesNativeProtocol(
		nativeMessages,
		model.UpstreamProtocolAnthropic,
	))
	assert.False(t, managedRouteUsesNativeProtocol(
		mixedMessages,
		model.UpstreamProtocolAnthropic,
	))
}

func TestShouldRecordManagedRouteFailure(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		errorCode  relaytypes.ErrorCode
		options    []relaytypes.NewAPIErrorOptions
		expected   bool
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, errorCode: relaytypes.ErrorCodeBadResponseStatusCode, expected: true},
		{name: "forbidden", statusCode: http.StatusForbidden, errorCode: relaytypes.ErrorCodeBadResponseStatusCode, expected: true},
		{name: "rate limited", statusCode: http.StatusTooManyRequests, errorCode: relaytypes.ErrorCodeBadResponseStatusCode, expected: true},
		{name: "server error", statusCode: http.StatusInternalServerError, errorCode: relaytypes.ErrorCodeBadResponseStatusCode, expected: true},
		{name: "channel error", statusCode: http.StatusBadRequest, errorCode: relaytypes.ErrorCodeChannelNoAvailableKey, expected: true},
		{name: "ordinary bad request", statusCode: http.StatusBadRequest, errorCode: relaytypes.ErrorCodeBadResponseStatusCode, expected: false},
		{name: "ordinary not found", statusCode: http.StatusNotFound, errorCode: relaytypes.ErrorCodeBadResponseStatusCode, expected: false},
		{
			name:       "skip retry remains client attributable",
			statusCode: http.StatusInternalServerError,
			errorCode:  relaytypes.ErrorCodeBadResponseStatusCode,
			options:    []relaytypes.NewAPIErrorOptions{relaytypes.ErrOptionWithSkipRetry()},
			expected:   false,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := relaytypes.NewOpenAIError(
				errors.New(testCase.name),
				testCase.errorCode,
				testCase.statusCode,
				testCase.options...,
			)
			assert.Equal(t, testCase.expected, ShouldRecordManagedRouteFailure(err))
		})
	}
}

func TestSelectUpstreamCandidateGroupsCapsAtFiveWithSourceDiversity(t *testing.T) {
	candidates := []upstreamRouteCandidate{
		{source: model.UpstreamSource{ID: 1, Key: "a"}, group: model.UpstreamGroup{SourceID: 1, ExternalID: "cheap", Platform: "openai", EffectiveMultiplier: 0.01}, models: []string{"gpt-test"}},
		{source: model.UpstreamSource{ID: 1, Key: "a"}, group: model.UpstreamGroup{SourceID: 1, ExternalID: "duplicate", Platform: "openai", EffectiveMultiplier: 0.02}, models: []string{"gpt-test"}},
		{source: model.UpstreamSource{ID: 2, Key: "b"}, group: model.UpstreamGroup{SourceID: 2, ExternalID: "b", Platform: "openai", EffectiveMultiplier: 0.03}, models: []string{"gpt-test"}},
		{source: model.UpstreamSource{ID: 3, Key: "c"}, group: model.UpstreamGroup{SourceID: 3, ExternalID: "c", Platform: "openai", EffectiveMultiplier: 0.04}, models: []string{"gpt-test"}},
		{source: model.UpstreamSource{ID: 4, Key: "d"}, group: model.UpstreamGroup{SourceID: 4, ExternalID: "d", Platform: "openai", EffectiveMultiplier: 0.05}, models: []string{"gpt-test"}},
		{source: model.UpstreamSource{ID: 5, Key: "e"}, group: model.UpstreamGroup{SourceID: 5, ExternalID: "e", Platform: "openai", EffectiveMultiplier: 0.06}, models: []string{"gpt-test"}},
		{source: model.UpstreamSource{ID: 6, Key: "f"}, group: model.UpstreamGroup{SourceID: 6, ExternalID: "f", Platform: "openai", EffectiveMultiplier: 0.07}, models: []string{"gpt-test"}},
	}

	selected := selectUpstreamCandidateGroups(candidates, 5)

	require.Len(t, selected, 5)
	sourceIDs := make(map[int64]struct{}, len(selected))
	for _, candidate := range selected {
		sourceIDs[candidate.source.ID] = struct{}{}
	}
	assert.Len(t, sourceIDs, 5)
}

func TestSelectUpstreamCandidateGroupsPrunesSharedModelsFromExtraGroups(t *testing.T) {
	candidates := []upstreamRouteCandidate{
		{source: model.UpstreamSource{ID: 1, Key: "a"}, group: model.UpstreamGroup{SourceID: 1, ExternalID: "a", Platform: "openai", EffectiveMultiplier: 0.01}, models: []string{"gpt-shared"}},
		{source: model.UpstreamSource{ID: 2, Key: "b"}, group: model.UpstreamGroup{SourceID: 2, ExternalID: "b", Platform: "openai", EffectiveMultiplier: 0.02}, models: []string{"gpt-shared"}},
		{source: model.UpstreamSource{ID: 3, Key: "c"}, group: model.UpstreamGroup{SourceID: 3, ExternalID: "c", Platform: "openai", EffectiveMultiplier: 0.03}, models: []string{"gpt-shared"}},
		{source: model.UpstreamSource{ID: 4, Key: "d"}, group: model.UpstreamGroup{SourceID: 4, ExternalID: "d", Platform: "openai", EffectiveMultiplier: 0.04}, models: []string{"gpt-shared"}},
		{source: model.UpstreamSource{ID: 5, Key: "e"}, group: model.UpstreamGroup{SourceID: 5, ExternalID: "e", Platform: "openai", EffectiveMultiplier: 0.05}, models: []string{"gpt-shared"}},
		{source: model.UpstreamSource{ID: 6, Key: "f"}, group: model.UpstreamGroup{SourceID: 6, ExternalID: "f", Platform: "openai", EffectiveMultiplier: 0.06}, models: []string{"gpt-shared", "gpt-unique"}},
	}

	selected := selectUpstreamCandidateGroups(candidates, 5)

	require.Len(t, selected, 6)
	sharedCount := 0
	for _, candidate := range selected {
		if slices.Contains(candidate.models, "gpt-shared") {
			sharedCount++
		}
		if candidate.group.ExternalID == "f" {
			assert.Equal(t, []string{"gpt-unique"}, candidate.models)
		}
	}
	assert.Equal(t, 5, sharedCount)
}

func TestRankManagedRoutesPersistsSelectedModelSubsets(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	now := time.Unix(1_788_320_000, 0)
	sources := make([]model.UpstreamSource, 0, 6)
	groups := make([]model.UpstreamGroup, 0, 6)
	candidates := make([]upstreamRouteCandidate, 0, 6)
	channelIDs := make(map[string]int)

	for index := 1; index <= 6; index++ {
		source := model.UpstreamSource{
			Key:              string(rune('a' + index - 1)),
			Name:             string(rune('A' + index - 1)),
			ConsoleURL:       "https://example.com",
			SelectedEndpoint: "https://api.example.com",
			Status:           model.UpstreamHealthOperational,
			Enabled:          true,
			LastSnapshotAt:   now.Unix(),
		}
		require.NoError(t, model.DB.Create(&source).Error)
		models := []string{"gpt-shared"}
		if index == 6 {
			models = append(models, "gpt-unique")
		}
		group := model.UpstreamGroup{
			SourceID:            source.ID,
			ExternalID:          source.Key,
			Name:                source.Name,
			Platform:            "openai",
			EffectiveMultiplier: float64(index) / 100,
			HealthStatus:        model.UpstreamHealthOperational,
			ObservedAt:          now.Unix(),
		}
		require.NoError(t, model.DB.Create(&group).Error)
		priority := int64(0)
		weight := uint(100)
		channel := model.Channel{
			Type:     58,
			Status:   common.ChannelStatusEnabled,
			Name:     source.Name,
			Weight:   &weight,
			BaseURL:  &source.SelectedEndpoint,
			Models:   strings.Join(models, ","),
			Group:    "default",
			Priority: &priority,
		}
		require.NoError(t, model.DB.Create(&channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
		require.NoError(t, model.DB.Create(&model.UpstreamManagedRoute{
			SourceID:        source.ID,
			ExternalGroupID: group.ExternalID,
			Platform:        group.Platform,
			Protocol:        model.UpstreamProtocolOpenAI,
			ChannelID:       channel.Id,
			State:           model.UpstreamRouteStateActive,
		}).Error)
		sources = append(sources, source)
		groups = append(groups, group)
		candidates = append(candidates, upstreamRouteCandidate{
			source: source,
			group:  group,
			models: models,
		})
		channelIDs[group.ExternalID] = channel.Id
	}

	selected := selectUpstreamCandidateGroups(candidates, 5)
	updated, err := rankManagedRoutes(
		now,
		sources,
		groups,
		selected,
		&operation_setting.UpstreamOrchestrationSetting{SyncIntervalHours: 4},
	)

	require.NoError(t, err)
	assert.Equal(t, 6, updated)
	var sharedCount int64
	require.NoError(t, model.DB.Model(&model.Ability{}).
		Where("model = ? AND enabled = ?", "gpt-shared", true).
		Count(&sharedCount).Error)
	assert.EqualValues(t, 5, sharedCount)
	var sixth model.Channel
	require.NoError(t, model.DB.First(&sixth, channelIDs["f"]).Error)
	assert.Equal(t, "gpt-unique", sixth.Models)
}

func TestRankManagedRoutesPreservesChannelWhenSnapshotIsStale(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	now := time.Unix(1_788_320_000, 0)
	setting := &operation_setting.UpstreamOrchestrationSetting{
		SyncIntervalHours: 4,
	}
	source := model.UpstreamSource{
		Key:              "source",
		Name:             "Source",
		ConsoleURL:       "https://example.com",
		SelectedEndpoint: "https://api.example.com",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotAt:   now.Add(-6 * time.Hour).Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group",
		Name:                "Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.5,
		HealthStatus:        model.UpstreamHealthOperational,
		ObservedAt:          now.Add(-6 * time.Hour).Unix(),
	}
	require.NoError(t, model.DB.Create(&group).Error)
	priority := int64(777)
	weight := uint(100)
	baseURL := source.SelectedEndpoint
	channel := model.Channel{
		Type:     58,
		Status:   common.ChannelStatusEnabled,
		Name:     "stale",
		Weight:   &weight,
		BaseURL:  &baseURL,
		Models:   "gpt-stable",
		Group:    "default",
		Priority: &priority,
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:        source.ID,
		ExternalGroupID: group.ExternalID,
		Platform:        group.Platform,
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateActive,
		Rank:            7,
	}
	require.NoError(t, model.DB.Create(&route).Error)

	updated, err := rankManagedRoutes(
		now,
		[]model.UpstreamSource{source},
		[]model.UpstreamGroup{group},
		nil,
		setting,
	)

	require.NoError(t, err)
	assert.Zero(t, updated)
	var reloadedChannel model.Channel
	require.NoError(t, model.DB.First(&reloadedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, reloadedChannel.Status)
	assert.EqualValues(t, 777, *reloadedChannel.Priority)
	assert.Equal(t, "gpt-stable", reloadedChannel.Models)
	var reloadedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&reloadedRoute, route.ID).Error)
	assert.EqualValues(t, 7, reloadedRoute.Rank)
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

func TestManagedCandidateSelectionEvaluableRequiresFreshSnapshot(t *testing.T) {
	now := time.Unix(1_788_320_000, 0)
	setting := &operation_setting.UpstreamOrchestrationSetting{
		SyncIntervalHours: 4,
	}
	source := model.UpstreamSource{
		Enabled:          true,
		SelectedEndpoint: "https://api.example.com",
		LastSnapshotAt:   now.Unix(),
	}
	group := model.UpstreamGroup{
		HealthStatus: model.UpstreamHealthOperational,
		ObservedAt:   now.Unix(),
	}

	assert.True(t, managedCandidateSelectionEvaluable(source, group, now, setting))

	source.LastSnapshotAt = now.Add(-6 * time.Hour).Unix()
	assert.False(t, managedCandidateSelectionEvaluable(source, group, now, setting))

	source.LastSnapshotAt = now.Unix()
	group.ObservedAt = now.Add(-6 * time.Hour).Unix()
	assert.False(t, managedCandidateSelectionEvaluable(source, group, now, setting))
}

func TestDesiredManagedRouteStateSafetyMatrix(t *testing.T) {
	now := time.Unix(1_788_320_000, 0)
	setting := &operation_setting.UpstreamOrchestrationSetting{
		SyncIntervalHours:       4,
		RedLongTermHours:        24,
		ShadowSuccessesRequired: 3,
	}
	positiveBalance := 10.0
	zeroBalance := 0.0
	baseSource := model.UpstreamSource{
		Enabled:        true,
		Balance:        &positiveBalance,
		LastSnapshotAt: now.Unix(),
	}
	tests := []struct {
		name     string
		route    model.UpstreamManagedRoute
		source   model.UpstreamSource
		group    model.UpstreamGroup
		expected string
	}{
		{
			name:     "degraded validated shadow activates",
			route:    model.UpstreamManagedRoute{State: model.UpstreamRouteStateShadow, ConsecutiveSuccesses: 3},
			source:   baseSource,
			group:    model.UpstreamGroup{HealthStatus: model.UpstreamHealthDegraded, ObservedAt: now.Unix()},
			expected: model.UpstreamRouteStateActive,
		},
		{
			name:     "unknown remains shadow",
			route:    model.UpstreamManagedRoute{State: model.UpstreamRouteStateShadow, ConsecutiveSuccesses: 3},
			source:   baseSource,
			group:    model.UpstreamGroup{HealthStatus: model.UpstreamHealthUnknown, ObservedAt: now.Unix()},
			expected: model.UpstreamRouteStateShadow,
		},
		{
			name:     "red quarantines",
			route:    model.UpstreamManagedRoute{State: model.UpstreamRouteStateActive},
			source:   baseSource,
			group:    model.UpstreamGroup{HealthStatus: model.UpstreamHealthFailed, ObservedAt: now.Unix(), RedSince: now.Add(-time.Hour).Unix()},
			expected: model.UpstreamRouteStateQuarantined,
		},
		{
			name:     "red for 24 hours becomes long red",
			route:    model.UpstreamManagedRoute{State: model.UpstreamRouteStateQuarantined},
			source:   baseSource,
			group:    model.UpstreamGroup{HealthStatus: model.UpstreamHealthFailed, ObservedAt: now.Unix(), RedSince: now.Add(-25 * time.Hour).Unix()},
			expected: model.UpstreamRouteStateLongRed,
		},
		{
			name:     "zero balance quarantines",
			route:    model.UpstreamManagedRoute{State: model.UpstreamRouteStateActive},
			source:   model.UpstreamSource{Enabled: true, Balance: &zeroBalance, LastSnapshotAt: now.Unix()},
			group:    model.UpstreamGroup{HealthStatus: model.UpstreamHealthOperational, ObservedAt: now.Unix()},
			expected: model.UpstreamRouteStateQuarantined,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			state, _ := desiredManagedRouteState(
				testCase.route,
				testCase.source,
				testCase.group,
				now,
				setting,
			)
			assert.Equal(t, testCase.expected, state)
		})
	}
}

func TestManagedRouteRecoveryBackoff(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	route := model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "recovery",
		Platform:        "openai",
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       1001,
		State:           model.UpstreamRouteStateShadow,
	}
	require.NoError(t, model.DB.Create(&route).Error)
	expectedDelays := []int64{300, 900, 3600, 14400, 0}
	for attempt, expectedDelay := range expectedDelays {
		before := common.GetTimestamp()
		require.NoError(t, MarkManagedRouteProbeResult(route.ID, false, 10, "failed"))
		var stored model.UpstreamManagedRoute
		require.NoError(t, model.DB.First(&stored, route.ID).Error)
		assert.Equal(t, attempt+1, stored.RecoveryAttempts)
		if expectedDelay == 0 {
			assert.Zero(t, stored.NextProbeAt)
			continue
		}
		assert.GreaterOrEqual(t, stored.NextProbeAt, before+expectedDelay)
		assert.LessOrEqual(t, stored.NextProbeAt, common.GetTimestamp()+expectedDelay)
	}
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
