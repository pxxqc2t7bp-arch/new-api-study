package service

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/wsmanager"
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
		&model.Channel{},
		&model.PlanQuotaDomain{},
		&model.Ability{},
	))
	model.DB = db
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func TestDisableChannelManagedWebSocketLifecycle(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		createChannel       bool
		consecutiveFailures int
		wantStatus          int
		wantCloseReasons    []string
	}{
		{
			name:                "quarantine closes active websocket",
			createChannel:       true,
			consecutiveFailures: 1,
			wantStatus:          common.ChannelStatusAutoDisabled,
			wantCloseReasons:    []string{ChannelDisabledCloseReason},
		},
		{
			name:          "handled failure below threshold keeps websocket open",
			createChannel: true,
			wantStatus:    common.ChannelStatusEnabled,
		},
		{
			name:                "failed status update keeps websocket open",
			consecutiveFailures: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			setupUpstreamOrchestrationTest(t)
			require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

			setting := operation_setting.GetUpstreamOrchestrationSetting()
			original := *setting
			setting.Enabled = true
			setting.FailureThreshold = 2
			setting.FailureWindowMinutes = 5
			t.Cleanup(func() {
				*setting = original
			})

			channelID := 901
			if testCase.createChannel {
				channel := model.Channel{
					Id:     channelID,
					Name:   "managed-websocket",
					Status: common.ChannelStatusEnabled,
					Group:  "default",
					Models: "gpt-managed",
				}
				require.NoError(t, model.DB.Create(&channel).Error)
				require.NoError(t, channel.AddAbilities(nil))
			}
			require.NoError(t, model.DB.Create(&model.UpstreamManagedRoute{
				SourceID:            1,
				ExternalGroupID:     "managed-websocket",
				Platform:            "openai",
				Protocol:            model.UpstreamProtocolOpenAI,
				ChannelID:           channelID,
				State:               model.UpstreamRouteStateActive,
				ConsecutiveFailures: testCase.consecutiveFailures,
				FailureWindowStart:  common.GetTimestamp(),
			}).Error)

			var closeReasons []string
			unregister := wsmanager.Register(channelID, wsmanager.KindResponses, func(reason string) {
				closeReasons = append(closeReasons, reason)
			})
			t.Cleanup(unregister)

			DisableChannel(relaytypes.ChannelError{
				ChannelId:   channelID,
				ChannelName: "managed-websocket",
				AutoBan:     true,
			}, "managed failure")

			assert.Equal(t, testCase.wantCloseReasons, closeReasons)
			if testCase.createChannel {
				channel, err := model.GetChannelById(channelID, true)
				require.NoError(t, err)
				assert.Equal(t, testCase.wantStatus, channel.Status)
			}
		})
	}
}

func TestManagedRouteStatusChangesCloseWebSockets(t *testing.T) {
	newFixture := func(t *testing.T, channelStatus int, routeState, healthStatus string, createChannel bool) (time.Time, model.UpstreamManagedRoute, *[]string) {
		t.Helper()
		setupUpstreamOrchestrationTest(t)
		require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

		now := time.Unix(1_788_320_000, 0)
		source := model.UpstreamSource{
			Key:              model.UpstreamSourceKeyHualong,
			Name:             "Managed Status",
			ConsoleURL:       "https://api.hualong.online",
			SelectedEndpoint: "https://api.hualong.online",
			Status:           model.UpstreamHealthOperational,
			Enabled:          true,
			LastSnapshotAt:   now.Unix(),
		}
		require.NoError(t, model.DB.Create(&source).Error)
		group := model.UpstreamGroup{
			SourceID:            source.ID,
			ExternalID:          "managed-status",
			Name:                "Managed Status",
			Platform:            "openai",
			EffectiveMultiplier: 1,
			HealthStatus:        healthStatus,
			ObservedAt:          now.Unix(),
			RedSince:            now.Add(-time.Hour).Unix(),
			Models:              `["gpt-managed"]`,
		}
		require.NoError(t, model.DB.Create(&group).Error)

		channelID := 902
		if createChannel {
			channel := model.Channel{
				Id:     channelID,
				Name:   "managed-status",
				Status: channelStatus,
				Group:  "default",
				Models: "gpt-managed",
			}
			require.NoError(t, model.DB.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
		}
		route := model.UpstreamManagedRoute{
			SourceID:        source.ID,
			ExternalGroupID: group.ExternalID,
			Platform:        group.Platform,
			Protocol:        model.UpstreamProtocolOpenAI,
			ChannelID:       channelID,
			State:           routeState,
		}
		require.NoError(t, model.DB.Create(&route).Error)

		closeReasons := make([]string, 0, 1)
		unregister := wsmanager.Register(channelID, wsmanager.KindResponses, func(reason string) {
			closeReasons = append(closeReasons, reason)
		})
		t.Cleanup(unregister)
		return now, route, &closeReasons
	}

	t.Run("reconciliation disable closes once", func(t *testing.T) {
		now, _, closeReasons := newFixture(
			t,
			common.ChannelStatusEnabled,
			model.UpstreamRouteStateActive,
			model.UpstreamHealthFailed,
			true,
		)
		setting := operation_setting.GetUpstreamOrchestrationSetting()
		original := *setting
		setting.Enabled = true
		setting.AutoEnroll = false
		t.Cleanup(func() { *setting = original })

		_, err := ReconcileManagedUpstreams(now)

		require.NoError(t, err)
		assert.Equal(t, []string{ChannelDisabledCloseReason}, *closeReasons)
		assert.Zero(t, wsmanager.CloseChannel(902, "duplicate close"))
	})

	t.Run("reconciliation enable does not close", func(t *testing.T) {
		now, _, closeReasons := newFixture(
			t,
			common.ChannelStatusAutoDisabled,
			model.UpstreamRouteStateShadow,
			model.UpstreamHealthOperational,
			true,
		)
		setting := operation_setting.GetUpstreamOrchestrationSetting()
		original := *setting
		setting.Enabled = true
		setting.AutoEnroll = false
		setting.ShadowSuccessesRequired = 0
		t.Cleanup(func() { *setting = original })

		_, err := ReconcileManagedUpstreams(now)

		require.NoError(t, err)
		assert.Empty(t, *closeReasons)
		assert.Equal(t, 1, wsmanager.CloseChannel(902, "test cleanup"))
	})

	t.Run("reconciliation no change does not close", func(t *testing.T) {
		now, _, closeReasons := newFixture(
			t,
			common.ChannelStatusAutoDisabled,
			model.UpstreamRouteStateQuarantined,
			model.UpstreamHealthFailed,
			true,
		)
		setting := operation_setting.GetUpstreamOrchestrationSetting()
		original := *setting
		setting.Enabled = true
		setting.AutoEnroll = false
		t.Cleanup(func() { *setting = original })

		_, err := ReconcileManagedUpstreams(now)

		require.NoError(t, err)
		assert.Empty(t, *closeReasons)
		assert.Equal(t, 1, wsmanager.CloseChannel(902, "test cleanup"))
	})

	t.Run("manual pause closes once", func(t *testing.T) {
		_, route, closeReasons := newFixture(
			t,
			common.ChannelStatusEnabled,
			model.UpstreamRouteStateActive,
			model.UpstreamHealthOperational,
			true,
		)

		require.NoError(t, PauseManagedRoute(route.ID, "maintenance"))

		assert.Equal(t, []string{ChannelDisabledCloseReason}, *closeReasons)
		assert.Zero(t, wsmanager.CloseChannel(902, "duplicate close"))
	})

	t.Run("status update failure does not close", func(t *testing.T) {
		now, route, closeReasons := newFixture(
			t,
			common.ChannelStatusEnabled,
			model.UpstreamRouteStateActive,
			model.UpstreamHealthFailed,
			false,
		)
		setting := operation_setting.GetUpstreamOrchestrationSetting()
		original := *setting
		setting.Enabled = true
		setting.AutoEnroll = false
		t.Cleanup(func() { *setting = original })

		_, err := ReconcileManagedUpstreams(now)
		require.Error(t, err)
		assert.Empty(t, *closeReasons)
		require.Error(t, PauseManagedRoute(route.ID, "maintenance"))
		assert.Empty(t, *closeReasons)
		assert.Equal(t, 1, wsmanager.CloseChannel(902, "test cleanup"))
	})
}

func createPartialManagedRouteFixture(
	t *testing.T,
	now time.Time,
	suffix string,
	multiplier float64,
	health string,
	rank int,
) (model.UpstreamSource, model.UpstreamGroup, model.Channel, model.UpstreamManagedRoute) {
	t.Helper()
	source := model.UpstreamSource{
		Key:              "source-" + suffix,
		Name:             "Source " + suffix,
		ConsoleURL:       "https://example.com",
		SelectedEndpoint: "https://api.example.com/" + suffix,
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotAt:   now.Unix(),
		LastSuccessAt:    now.Unix(),
		UpdatedAt:        now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group-" + suffix,
		Name:                "Group " + suffix,
		Platform:            "openai",
		EffectiveMultiplier: multiplier,
		HealthStatus:        health,
		Models:              `["gpt-4.1"]`,
		RedSince:            now.Add(-time.Hour).Unix(),
		ObservedAt:          now.Unix(),
		UpdatedAt:           now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&group).Error)
	priority := int64(0)
	channel := model.Channel{
		Name:     "Channel " + suffix,
		Key:      "credential-" + suffix,
		Status:   common.ChannelStatusEnabled,
		Models:   "gpt-4.1",
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
		Rank:            rank,
		UpdatedAt:       now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&route).Error)
	return source, group, channel, route
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

func TestApplyUpstreamEnrollmentResultUsesAuthorityAwareChannelInsert(t *testing.T) {
	setupUpstreamOrchestrationTest(t)

	source := model.UpstreamSource{
		Key:              model.UpstreamSourceKeyHualong,
		Name:             "Enrollment Source",
		ConsoleURL:       "https://api.hualong.online",
		SelectedEndpoint: "https://api-fast.hualong.online",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
	}
	require.NoError(t, model.DB.Create(&source).Error)
	payload := dto.UpstreamEnrollmentCommand{
		SourceKey:       source.Key,
		ExternalGroupID: "group-1",
		GroupName:       "Group 1",
		Platform:        "openai",
		APIBaseURL:      source.SelectedEndpoint,
		Models:          []string{"gpt-4.1"},
	}
	payloadJSON, err := common.Marshal(payload)
	require.NoError(t, err)
	command := model.UpstreamSyncCommand{
		CommandID: "enrollment-command",
		DeviceID:  "device-1",
		Type:      upstreamSyncCommandEnroll,
		SourceKey: source.Key,
		Payload:   string(payloadJSON),
		Status:    model.UpstreamSyncCommandRunning,
	}
	require.NoError(t, model.DB.Create(&command).Error)

	require.NoError(t, ApplyUpstreamEnrollmentResult("device-1", dto.UpstreamEnrollmentResult{
		CommandID:       command.CommandID,
		Success:         true,
		SourceKey:       source.Key,
		ExternalGroupID: payload.ExternalGroupID,
		ExternalKeyID:   "external-key",
		APIKey:          "managed-secret",
	}))

	var channels []model.Channel
	require.NoError(t, model.DB.Order("id").Find(&channels).Error)
	require.Len(t, channels, 2)
	var routes int64
	require.NoError(t, model.DB.Model(&model.UpstreamManagedRoute{}).Count(&routes).Error)
	assert.Equal(t, int64(2), routes)
	var abilities int64
	require.NoError(t, model.DB.Model(&model.Ability{}).Count(&abilities).Error)
	assert.Equal(t, int64(4), abilities)
	var authorities int64
	require.NoError(t, model.DB.Model(&model.PlanQuotaDomain{}).Count(&authorities).Error)
	assert.Zero(t, authorities)
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

func TestManagedProtocolModelExcludedSupportsGlobalAndScopedRules(t *testing.T) {
	exclusions := map[string][]string{
		"anthropic":          {"gpt-6-astra"},
		"leyi:openai":        {"gpt-source-only"},
		"ebond:group:openai": {"gpt-group-only"},
	}

	assert.True(t, managedProtocolModelExcluded("hualong", "group", "anthropic", "gpt-6-astra", exclusions))
	assert.False(t, managedProtocolModelExcluded("hualong", "group", "openai", "gpt-6-astra", exclusions))
	assert.True(t, managedProtocolModelExcluded("leyi", "group", "openai", "gpt-source-only", exclusions))
	assert.False(t, managedProtocolModelExcluded("ebond", "other", "openai", "gpt-group-only", exclusions))
	assert.True(t, managedProtocolModelExcluded("ebond", "group", "openai", "gpt-group-only", exclusions))
}

func TestManagedRouteUsesNativeProtocol(t *testing.T) {
	nativeOpenAI := model.Channel{OtherSettings: `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/chat/completions","upstream_path":"/chat/completions","converter":"none"},{"incoming_path":"/v1/responses","upstream_path":"/responses","converter":"none"}]}}`}
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

func TestRankManagedRoutesReturnsCommittedCountBeforeStaleError(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	now := time.Unix(1_788_320_000, 0)
	firstSource, firstGroup, _, firstRoute := createPartialManagedRouteFixture(
		t,
		now,
		"first",
		0.1,
		model.UpstreamHealthOperational,
		1,
	)
	secondSource, secondGroup, _, _ := createPartialManagedRouteFixture(
		t,
		now,
		"second",
		0.2,
		model.UpstreamHealthOperational,
		2,
	)

	injected := false
	const callbackName = "test:stale_second_rank_after_first_commit"
	require.NoError(t, model.DB.Callback().Update().After("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if injected || tx.Statement == nil || tx.Statement.Table != "upstream_managed_routes" {
			return
		}
		injected = true
		writer := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true})
		require.NoError(t, writer.Model(&model.UpstreamGroup{}).
			Where("id = ?", secondGroup.ID).
			Update("updated_at", secondGroup.UpdatedAt+1).Error)
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Update().Remove(callbackName))
	})

	updated, err := rankManagedRoutes(
		now,
		[]model.UpstreamSource{firstSource, secondSource},
		[]model.UpstreamGroup{firstGroup, secondGroup},
		[]upstreamRouteCandidate{
			{source: firstSource, group: firstGroup, models: []string{"gpt-4.1"}},
			{source: secondSource, group: secondGroup, models: []string{"gpt-4.1"}},
		},
		&operation_setting.UpstreamOrchestrationSetting{SyncIntervalHours: 4},
	)

	require.Error(t, err)
	require.True(t, injected)
	assert.Equal(t, 1, updated)
	var storedFirstRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedFirstRoute, firstRoute.ID).Error)
	assert.Equal(t, 1, storedFirstRoute.Rank)
}

func TestReconcileManagedUpstreamsPreservesPartialRankCountOnError(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4.1":1}`))
	t.Cleanup(func() {
		*setting = originalSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
	})

	_, _, _, firstRoute := createPartialManagedRouteFixture(
		t,
		now,
		"first",
		0.1,
		model.UpstreamHealthOperational,
		1,
	)
	_, secondGroup, _, _ := createPartialManagedRouteFixture(
		t,
		now,
		"second",
		0.2,
		model.UpstreamHealthOperational,
		2,
	)

	injected := false
	const callbackName = "test:stale_second_rank_during_reconcile"
	require.NoError(t, model.DB.Callback().Update().After("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if injected || tx.Statement == nil || tx.Statement.Table != "upstream_managed_routes" {
			return
		}
		injected = true
		writer := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true})
		require.NoError(t, writer.Model(&model.UpstreamGroup{}).
			Where("id = ?", secondGroup.ID).
			Update("updated_at", secondGroup.UpdatedAt+1).Error)
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Update().Remove(callbackName))
	})

	summary, err := reconcileManagedUpstreams(now, nil)

	require.Error(t, err)
	require.True(t, injected)
	assert.Equal(t, 1, summary.PrioritiesUpdated)
	var storedFirstRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedFirstRoute, firstRoute.ID).Error)
	assert.Equal(t, 1, storedFirstRoute.Rank)
}

func TestRankManagedRoutesAppliesProtocolModelExclusions(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	now := time.Unix(1_788_320_000, 0)
	endpoint := "https://api.example.com"
	source := model.UpstreamSource{
		Key:              "source",
		Name:             "Source",
		SelectedEndpoint: endpoint,
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotAt:   now.Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group",
		Name:                "Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		HealthStatus:        model.UpstreamHealthOperational,
		ObservedAt:          now.Unix(),
	}
	require.NoError(t, model.DB.Create(&group).Error)
	models := []string{"gpt-5.6-sol", "gpt-6-astra"}
	channels := make(map[string]model.Channel)
	for _, protocol := range []string{model.UpstreamProtocolOpenAI, model.UpstreamProtocolAnthropic} {
		priority := int64(0)
		weight := uint(100)
		channel := model.Channel{
			Type:     constant.ChannelTypeAdvancedCustom,
			Status:   common.ChannelStatusEnabled,
			Name:     protocol,
			Weight:   &weight,
			BaseURL:  &endpoint,
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
			Protocol:        protocol,
			ChannelID:       channel.Id,
			State:           model.UpstreamRouteStateActive,
		}).Error)
		channels[protocol] = channel
	}

	updated, err := rankManagedRoutes(
		now,
		[]model.UpstreamSource{source},
		[]model.UpstreamGroup{group},
		[]upstreamRouteCandidate{{source: source, group: group, models: models}},
		&operation_setting.UpstreamOrchestrationSetting{
			SyncIntervalHours: 4,
			ProtocolModelExclusions: map[string][]string{
				model.UpstreamProtocolAnthropic: {"gpt-6-astra"},
			},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 2, updated)

	var openAI model.Channel
	require.NoError(t, model.DB.First(&openAI, channels[model.UpstreamProtocolOpenAI].Id).Error)
	assert.Equal(t, "gpt-5.6-sol,gpt-6-astra", openAI.Models)
	var anthropic model.Channel
	require.NoError(t, model.DB.First(&anthropic, channels[model.UpstreamProtocolAnthropic].Id).Error)
	assert.Equal(t, "gpt-5.6-sol", anthropic.Models)
}

func TestReconcileManagedUpstreamsPreservesPlanQuotaOwnership(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	setting.ShadowSuccessesRequired = 3
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4.1":1}`))
	t.Cleanup(func() {
		*setting = originalSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
	})

	source := model.UpstreamSource{
		Key:              "source",
		Name:             "Source",
		ConsoleURL:       "https://example.com",
		SelectedEndpoint: "https://api.example.com",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotAt:   now.Unix(),
		LastSuccessAt:    now.Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group",
		Name:                "Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		HealthStatus:        model.UpstreamHealthOperational,
		Models:              `["gpt-4.1"]`,
		ObservedAt:          now.Unix(),
	}
	require.NoError(t, model.DB.Create(&group).Error)

	autoBan := 1
	tag := "plan:managed:reconcile"
	channels := []model.Channel{
		{
			Id: 71, Name: "activating", Key: "credential-a",
			Status: common.ChannelStatusAutoDisabled, Tag: &tag, AutoBan: &autoBan,
			Models: "gpt-4.1", Group: "default",
		},
		{
			Id: 72, Name: "steady-active", Key: "credential-b",
			Status: common.ChannelStatusAutoDisabled, Tag: &tag, AutoBan: &autoBan,
			Models: "gpt-4.1", Group: "default",
		},
	}
	for i := range channels {
		channels[i].SetOtherInfo(map[string]any{
			"disabled_until":   now.Add(time.Hour).Unix(),
			"quota_domain_id":  planQuotaDomainID(channels[i].Id, channels[i].Key),
			"quota_generation": "1",
			"quota_type":       "plan",
			"preserved_owner":  channels[i].Name,
		})
	}
	require.NoError(t, model.DB.Create(&channels).Error)
	require.NoError(t, model.InitializePlanQuotaDomains())
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}
	routes := []model.UpstreamManagedRoute{
		{
			SourceID: source.ID, ExternalGroupID: group.ExternalID,
			Platform: group.Platform, Protocol: model.UpstreamProtocolOpenAI,
			ChannelID: channels[0].Id, State: model.UpstreamRouteStateShadow,
			ConsecutiveSuccesses: setting.ShadowSuccessesRequired,
		},
		{
			SourceID: source.ID, ExternalGroupID: group.ExternalID,
			Platform: group.Platform, Protocol: model.UpstreamProtocolAnthropic,
			ChannelID: channels[1].Id, State: model.UpstreamRouteStateActive,
		},
	}
	require.NoError(t, model.DB.Create(&routes).Error)

	summary, err := ReconcileManagedUpstreams(now)

	require.NoError(t, err)
	assert.Equal(t, 1, summary.RoutesActivated)
	assert.Equal(t, 2, summary.PrioritiesUpdated)

	var storedRoutes []model.UpstreamManagedRoute
	require.NoError(t, model.DB.Order("id").Find(&storedRoutes).Error)
	require.Len(t, storedRoutes, 2)
	assert.Equal(t, model.UpstreamRouteStateActive, storedRoutes[0].State)
	assert.Equal(t, model.UpstreamRouteStateActive, storedRoutes[1].State)
	assert.Equal(t, 1, storedRoutes[0].Rank)
	assert.Equal(t, 1, storedRoutes[1].Rank)

	var storedChannels []model.Channel
	require.NoError(t, model.DB.Order("id").Find(&storedChannels).Error)
	require.Len(t, storedChannels, 2)
	for i := range storedChannels {
		assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannels[i].Status)
		assert.Equal(t, channels[i].OtherInfo, storedChannels[i].OtherInfo)
		require.NotNil(t, storedChannels[i].Priority)
		assert.EqualValues(t, 999, *storedChannels[i].Priority)
		assert.Equal(t, source.SelectedEndpoint, storedChannels[i].GetBaseURL())
	}

	var abilities []model.Ability
	require.NoError(t, model.DB.Order("channel_id").Find(&abilities).Error)
	require.Len(t, abilities, 2)
	assert.False(t, abilities[0].Enabled)
	assert.False(t, abilities[1].Enabled)
}

func TestReconcileManagedUpstreamsReturnsAndNotifiesCommittedChangesBeforeStaleError(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	setting.RedLongTermHours = 24
	t.Cleanup(func() {
		*setting = originalSetting
	})

	_, _, firstChannel, firstRoute := createPartialManagedRouteFixture(
		t,
		now,
		"first",
		0.1,
		model.UpstreamHealthFailed,
		1,
	)
	_, _, secondChannel, secondRoute := createPartialManagedRouteFixture(
		t,
		now,
		"second",
		0.2,
		model.UpstreamHealthFailed,
		2,
	)

	injected := false
	const callbackName = "test:stale_second_reconcile_route_after_first_commit"
	require.NoError(t, model.DB.Callback().Update().After("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if injected || tx.Statement == nil || tx.Statement.Table != "upstream_managed_routes" {
			return
		}
		injected = true
		writer := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true})
		require.NoError(t, writer.Model(&model.UpstreamManagedRoute{}).
			Where("id = ?", secondRoute.ID).
			Update("updated_at", secondRoute.UpdatedAt+1).Error)
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Update().Remove(callbackName))
	})

	notificationCalls := 0
	notificationContent := ""
	notificationErr := errors.New("forced notification failure")
	summary, err := reconcileManagedUpstreams(
		now,
		func(_ string, _ string, content string) error {
			notificationCalls++
			notificationContent = content
			return notificationErr
		},
	)

	require.Error(t, err)
	assert.NotErrorIs(t, err, notificationErr)
	require.True(t, injected)
	assert.Equal(t, 1, summary.RoutesQuarantined)
	assert.Equal(t, 1, notificationCalls)
	assert.Contains(t, notificationContent, fmt.Sprintf("#%d", firstChannel.Id))
	assert.NotContains(t, notificationContent, fmt.Sprintf("#%d", secondChannel.Id))
	var storedFirstRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedFirstRoute, firstRoute.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateQuarantined, storedFirstRoute.State)
	assert.Zero(t, storedFirstRoute.Rank)
	var storedSecondRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedSecondRoute, secondRoute.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateActive, storedSecondRoute.State)
}

func TestReconcileManagedUpstreamsRepairsRouteableQuarantinedChannel(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	setting.RedLongTermHours = 24
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4.1":1}`))
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	t.Cleanup(func() {
		*setting = originalSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
	})

	source := model.UpstreamSource{
		Key:              "source",
		Name:             "Source",
		ConsoleURL:       "https://example.com",
		SelectedEndpoint: "https://api.example.com",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotID:   "snapshot-a",
		LastSnapshotAt:   now.Unix(),
		LastSuccessAt:    now.Unix(),
		UpdatedAt:        now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group",
		Name:                "Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		HealthStatus:        model.UpstreamHealthFailed,
		Models:              `["gpt-4.1"]`,
		RedSince:            now.Add(-time.Hour).Unix(),
		ObservedAt:          now.Unix(),
		UpdatedAt:           now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&group).Error)
	priority := int64(999)
	channel := model.Channel{
		Name:     "routeable-quarantine",
		Key:      "credential",
		Status:   common.ChannelStatusEnabled,
		Models:   "gpt-4.1",
		Group:    "default",
		Priority: &priority,
	}
	channel.SetOtherInfo(map[string]any{"preserved_owner": "status-domain"})
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:        source.ID,
		ExternalGroupID: group.ExternalID,
		Platform:        group.Platform,
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateQuarantined,
		RedSince:        group.RedSince,
		LastReason:      "upstream monitor red",
		UpdatedAt:       now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&route).Error)
	common.MemoryCacheEnabled = true
	model.InitChannelCache()

	summary, err := ReconcileManagedUpstreams(now)

	require.NoError(t, err)
	assert.Zero(t, summary.RoutesQuarantined)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateQuarantined, storedRoute.State)
	var storedChannel model.Channel
	require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannel.Status)
	info := storedChannel.GetOtherInfo()
	assert.Equal(t, "status-domain", info["preserved_owner"])
	assert.Equal(t, "upstream monitor red", info["status_reason"])
	assert.EqualValues(t, now.Unix(), info["status_time"])
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
	cached, err := model.CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.Status)
	assert.Equal(t, storedChannel.OtherInfo, cached.OtherInfo)
	routingIDs, err := model.ListSatisfiedChannelIDsAtPriority(
		channel.Group,
		channel.Models,
		priority,
		nil,
	)
	require.NoError(t, err)
	assert.NotContains(t, routingIDs, channel.Id)
}

func TestReconcileManagedUpstreamsRepairsRouteableDetachedChannelWithoutSourceOrGroup(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	t.Cleanup(func() {
		*setting = originalSetting
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
	})

	priority := int64(999)
	channel := model.Channel{
		Name:     "routeable-detached",
		Key:      "credential",
		Status:   common.ChannelStatusEnabled,
		Models:   "gpt-4.1",
		Group:    "default",
		Priority: &priority,
	}
	channel.SetOtherInfo(map[string]any{"preserved_owner": "status-domain"})
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:        91,
		ExternalGroupID: "deleted-group",
		Platform:        "openai",
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateDetached,
		Rank:            7,
		LastReason:      "operator detached",
		Detached:        true,
		UpdatedAt:       now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&route).Error)
	var expectedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&expectedRoute, route.ID).Error)
	common.MemoryCacheEnabled = true
	model.InitChannelCache()
	cachedBefore, err := model.CacheGetChannel(channel.Id)
	require.NoError(t, err)
	newerCache := *cachedBefore
	newerCache.SetOtherInfo(map[string]any{
		"preserved_owner": "newer-cache-owner",
		"cache_only":      true,
	})
	model.CacheUpdateChannel(&newerCache)
	routingIDs, err := model.ListSatisfiedChannelIDsAtPriority(
		channel.Group,
		channel.Models,
		priority,
		nil,
	)
	require.NoError(t, err)
	require.Contains(t, routingIDs, channel.Id)

	summary, err := ReconcileManagedUpstreams(now)

	require.NoError(t, err)
	assert.Zero(t, summary.PrioritiesUpdated)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, expectedRoute, storedRoute)
	var storedChannel model.Channel
	require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannel.Status)
	storedInfo := storedChannel.GetOtherInfo()
	assert.Equal(t, "status-domain", storedInfo["preserved_owner"])
	assert.Equal(t, "detached", storedInfo["status_reason"])
	assert.EqualValues(t, now.Unix(), storedInfo["status_time"])
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
	cached, err := model.CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.Status)
	cachedInfo := cached.GetOtherInfo()
	assert.Equal(t, "newer-cache-owner", cachedInfo["preserved_owner"])
	assert.Equal(t, true, cachedInfo["cache_only"])
	assert.Equal(t, "detached", cachedInfo["status_reason"])
	assert.EqualValues(t, now.Unix(), cachedInfo["status_time"])
	routingIDs, err = model.ListSatisfiedChannelIDsAtPriority(
		channel.Group,
		channel.Models,
		priority,
		nil,
	)
	require.NoError(t, err)
	assert.NotContains(t, routingIDs, channel.Id)
}

func TestReconcileManagedUpstreamsRollsBackRouteWhenAbilityDisableFails(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	setting.RedLongTermHours = 24
	t.Cleanup(func() {
		*setting = originalSetting
	})

	source := model.UpstreamSource{
		Key:              "source",
		Name:             "Source",
		ConsoleURL:       "https://example.com",
		SelectedEndpoint: "https://api.example.com",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotID:   "snapshot-a",
		LastSnapshotAt:   now.Unix(),
		LastSuccessAt:    now.Unix(),
		UpdatedAt:        now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group",
		Name:                "Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		HealthStatus:        model.UpstreamHealthFailed,
		Models:              `["gpt-4.1"]`,
		RedSince:            now.Add(-time.Hour).Unix(),
		ObservedAt:          now.Unix(),
		UpdatedAt:           now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&group).Error)
	channel := model.Channel{
		Name:   "ability-rollback",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-4.1",
		Group:  "default",
	}
	channel.SetOtherInfo(map[string]any{"owner": "before"})
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
		UpdatedAt:       now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&route).Error)

	forcedErr := errors.New("forced managed ability failure")
	const callbackName = "test:fail_managed_reconcile_ability"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "abilities" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Update().Remove(callbackName))
	})

	summary, err := ReconcileManagedUpstreams(now)

	require.ErrorIs(t, err, forcedErr)
	assert.Zero(t, summary.RoutesQuarantined)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateActive, storedRoute.State)
	assert.Equal(t, 7, storedRoute.Rank)
	assert.Equal(t, route.UpdatedAt, storedRoute.UpdatedAt)
	var storedChannel model.Channel
	require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, storedChannel.Status)
	assert.Equal(t, channel.OtherInfo, storedChannel.OtherInfo)
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.True(t, ability.Enabled)
}

func TestReconcileManagedUpstreamsRejectsStaleManualPause(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	setting.ShadowSuccessesRequired = 3
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4.1":1}`))
	t.Cleanup(func() {
		*setting = originalSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
	})

	source := model.UpstreamSource{
		Key:              "source",
		Name:             "Source",
		ConsoleURL:       "https://example.com",
		SelectedEndpoint: "https://api.example.com",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotID:   "snapshot-a",
		LastSnapshotAt:   now.Unix(),
		LastSuccessAt:    now.Unix(),
		UpdatedAt:        now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group",
		Name:                "Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		HealthStatus:        model.UpstreamHealthOperational,
		Models:              `["gpt-4.1"]`,
		ObservedAt:          now.Unix(),
		UpdatedAt:           now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&group).Error)
	channel := model.Channel{
		Name:   "manual-pause-race",
		Key:    "credential",
		Status: common.ChannelStatusAutoDisabled,
		Models: "gpt-4.1",
		Group:  "default",
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:             source.ID,
		ExternalGroupID:      group.ExternalID,
		Platform:             group.Platform,
		Protocol:             model.UpstreamProtocolOpenAI,
		ChannelID:            channel.Id,
		State:                model.UpstreamRouteStateShadow,
		ConsecutiveSuccesses: setting.ShadowSuccessesRequired,
		UpdatedAt:            now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&route).Error)

	pauseUntil := now.Add(6 * time.Hour).Unix()
	injected := false
	const callbackName = "test:inject_managed_manual_pause"
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if injected || tx.Statement == nil || tx.Statement.Table != "upstream_managed_routes" {
			return
		}
		if _, inTransaction := tx.Statement.ConnPool.(*sql.Tx); inTransaction {
			return
		}
		injected = true
		writer := model.DB.Session(&gorm.Session{NewDB: true, SkipHooks: true})
		require.NoError(t, writer.Model(&model.UpstreamManagedRoute{}).
			Where("id = ?", route.ID).
			Updates(map[string]any{
				"state":              model.UpstreamRouteStatePaused,
				"manual_pause_until": pauseUntil,
				"last_reason":        "operator pause",
				"updated_at":         now.Unix(),
			}).Error)
		require.NoError(t, writer.Model(&model.Channel{}).
			Where("id = ?", channel.Id).
			Update("status", common.ChannelStatusManuallyDisabled).Error)
		require.NoError(t, writer.Model(&model.Ability{}).
			Where("channel_id = ?", channel.Id).
			Update("enabled", false).Error)
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Query().Remove(callbackName))
	})

	_, err := ReconcileManagedUpstreams(now)

	require.Error(t, err)
	require.True(t, injected)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, model.UpstreamRouteStatePaused, storedRoute.State)
	assert.Equal(t, pauseUntil, storedRoute.ManualPauseUntil)
	assert.Equal(t, "operator pause", storedRoute.LastReason)
	var storedChannel model.Channel
	require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, storedChannel.Status)
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
}

func TestRankManagedRoutesRejectsChangedDecisionSnapshots(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, source model.UpstreamSource, group model.UpstreamGroup)
	}{
		{
			name: "source disabled",
			mutate: func(t *testing.T, source model.UpstreamSource, _ model.UpstreamGroup) {
				t.Helper()
				require.NoError(t, model.DB.Model(&model.UpstreamSource{}).
					Where("id = ?", source.ID).
					Updates(map[string]any{"enabled": false, "updated_at": source.UpdatedAt + 1}).Error)
			},
		},
		{
			name: "source balance exhausted",
			mutate: func(t *testing.T, source model.UpstreamSource, _ model.UpstreamGroup) {
				t.Helper()
				require.NoError(t, model.DB.Model(&model.UpstreamSource{}).
					Where("id = ?", source.ID).
					Updates(map[string]any{"balance": 0, "updated_at": source.UpdatedAt + 1}).Error)
			},
		},
		{
			name: "group failed",
			mutate: func(t *testing.T, _ model.UpstreamSource, group model.UpstreamGroup) {
				t.Helper()
				require.NoError(t, model.DB.Model(&model.UpstreamGroup{}).
					Where("id = ?", group.ID).
					Updates(map[string]any{
						"health_status": model.UpstreamHealthFailed,
						"updated_at":    group.UpdatedAt + 1,
					}).Error)
			},
		},
		{
			name: "group error",
			mutate: func(t *testing.T, _ model.UpstreamSource, group model.UpstreamGroup) {
				t.Helper()
				require.NoError(t, model.DB.Model(&model.UpstreamGroup{}).
					Where("id = ?", group.ID).
					Updates(map[string]any{
						"health_status": model.UpstreamHealthError,
						"updated_at":    group.UpdatedAt + 1,
					}).Error)
			},
		},
		{
			name: "group observed snapshot changed",
			mutate: func(t *testing.T, _ model.UpstreamSource, group model.UpstreamGroup) {
				t.Helper()
				require.NoError(t, model.DB.Model(&model.UpstreamGroup{}).
					Where("id = ?", group.ID).
					Updates(map[string]any{
						"observed_at": group.ObservedAt + 1,
						"updated_at":  group.UpdatedAt + 1,
					}).Error)
			},
		},
		{
			name: "group config changed",
			mutate: func(t *testing.T, _ model.UpstreamSource, group model.UpstreamGroup) {
				t.Helper()
				require.NoError(t, model.DB.Model(&model.UpstreamGroup{}).
					Where("id = ?", group.ID).
					Updates(map[string]any{
						"models":               `["gpt-new"]`,
						"effective_multiplier": 0.2,
						"updated_at":           group.UpdatedAt + 1,
					}).Error)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			setupUpstreamOrchestrationTest(t)
			require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
			now := time.Unix(1_788_320_000, 0)
			balance := 10.0
			source := model.UpstreamSource{
				Key:              "source",
				Name:             "Source",
				ConsoleURL:       "https://example.com",
				SelectedEndpoint: "https://api.example.com",
				Status:           model.UpstreamHealthOperational,
				Enabled:          true,
				Balance:          &balance,
				LastSnapshotID:   "snapshot-a",
				LastSnapshotAt:   now.Unix(),
				LastSuccessAt:    now.Unix(),
				UpdatedAt:        now.Add(-time.Minute).Unix(),
			}
			require.NoError(t, model.DB.Create(&source).Error)
			group := model.UpstreamGroup{
				SourceID:            source.ID,
				ExternalID:          "group",
				Name:                "Group",
				Platform:            "openai",
				EffectiveMultiplier: 0.1,
				HealthStatus:        model.UpstreamHealthOperational,
				Models:              `["gpt-4.1"]`,
				ObservedAt:          now.Unix(),
				UpdatedAt:           now.Add(-time.Minute).Unix(),
			}
			require.NoError(t, model.DB.Create(&group).Error)
			priority := int64(17)
			baseURL := "https://old.example.com"
			channel := model.Channel{
				Name:     "stale-decision",
				Key:      "credential",
				Status:   common.ChannelStatusAutoDisabled,
				Priority: &priority,
				BaseURL:  &baseURL,
				Models:   "gpt-old",
				Group:    "default",
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
				UpdatedAt:       now.Add(-time.Minute).Unix(),
			}
			require.NoError(t, model.DB.Create(&route).Error)
			testCase.mutate(t, source, group)

			updated, err := rankManagedRoutes(
				now,
				[]model.UpstreamSource{source},
				[]model.UpstreamGroup{group},
				[]upstreamRouteCandidate{{source: source, group: group, models: []string{"gpt-4.1"}}},
				&operation_setting.UpstreamOrchestrationSetting{SyncIntervalHours: 4},
			)

			require.Error(t, err)
			assert.Zero(t, updated)
			var storedChannel model.Channel
			require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
			assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannel.Status)
			assert.EqualValues(t, priority, storedChannel.GetPriority())
			assert.Equal(t, baseURL, storedChannel.GetBaseURL())
			assert.Equal(t, "gpt-old", storedChannel.Models)
			var ability model.Ability
			require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
			assert.False(t, ability.Enabled)
			var storedRoute model.UpstreamManagedRoute
			require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
			assert.Equal(t, 7, storedRoute.Rank)
		})
	}
}

func TestPreserveManagedPlanQuotaOwnershipRequiresAllMultiKeysDisabled(t *testing.T) {
	allDisabled := &model.Channel{
		Key:    "key-a\nkey-b",
		Status: common.ChannelStatusAutoDisabled,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusManuallyDisabled,
			},
		},
	}
	oneEnabled := &model.Channel{
		Key:    "key-a\nkey-b",
		Status: common.ChannelStatusAutoDisabled,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
			},
		},
	}

	assert.True(t, preserveManagedPlanQuotaOwnership(
		allDisabled,
		common.ChannelStatusEnabled,
	))
	assert.False(t, preserveManagedPlanQuotaOwnership(
		oneEnabled,
		common.ChannelStatusEnabled,
	))
	assert.False(t, preserveManagedPlanQuotaOwnership(
		allDisabled,
		common.ChannelStatusAutoDisabled,
	))
}

func TestReconcileManagedUpstreamsPreservesConcurrentPlanQuotaDisable(t *testing.T) {
	tests := []struct {
		name                 string
		initialState         string
		consecutiveSuccesses int
	}{
		{
			name:                 "activation",
			initialState:         model.UpstreamRouteStateShadow,
			consecutiveSuccesses: 3,
		},
		{
			name:         "steady-state rank",
			initialState: model.UpstreamRouteStateActive,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			setupUpstreamOrchestrationTest(t)
			require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

			now := time.Unix(1_788_320_000, 0)
			setting := operation_setting.GetUpstreamOrchestrationSetting()
			originalSetting := *setting
			setting.Enabled = true
			setting.AutoEnroll = false
			setting.CandidateLimit = 5
			setting.MaxUpstreamMultiplier = 1
			setting.SyncIntervalHours = 4
			setting.ShadowSuccessesRequired = 3
			originalModelRatios := ratio_setting.ModelRatio2JSONString()
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4.1":1}`))
			t.Cleanup(func() {
				*setting = originalSetting
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
			})

			source := model.UpstreamSource{
				Key:              "source",
				Name:             "Source",
				ConsoleURL:       "https://example.com",
				SelectedEndpoint: "https://api.example.com",
				Status:           model.UpstreamHealthOperational,
				Enabled:          true,
				LastSnapshotAt:   now.Unix(),
				LastSuccessAt:    now.Unix(),
			}
			require.NoError(t, model.DB.Create(&source).Error)
			group := model.UpstreamGroup{
				SourceID:            source.ID,
				ExternalID:          "group",
				Name:                "Group",
				Platform:            "openai",
				EffectiveMultiplier: 0.1,
				HealthStatus:        model.UpstreamHealthOperational,
				Models:              `["gpt-4.1"]`,
				ObservedAt:          now.Unix(),
			}
			require.NoError(t, model.DB.Create(&group).Error)

			autoBan := 1
			tag := "plan:managed:concurrent"
			priority := int64(17)
			baseURL := "https://old.example.com"
			channel := model.Channel{
				Id: 73, Name: "concurrently-disabled", Key: "credential",
				Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan,
				Priority: &priority, BaseURL: &baseURL,
				Models: "gpt-old", Group: "default",
			}
			require.NoError(t, model.DB.Create(&channel).Error)
			require.NoError(t, model.InitializePlanQuotaDomains())
			require.NoError(t, channel.AddAbilities(nil))
			route := model.UpstreamManagedRoute{
				SourceID: source.ID, ExternalGroupID: group.ExternalID,
				Platform: group.Platform, Protocol: model.UpstreamProtocolOpenAI,
				ChannelID: channel.Id, State: testCase.initialState,
				ConsecutiveSuccesses: testCase.consecutiveSuccesses,
			}
			require.NoError(t, model.DB.Create(&route).Error)

			disabled := channel
			disabled.Status = common.ChannelStatusAutoDisabled
			disabled.SetOtherInfo(map[string]any{
				"disabled_until":   now.Add(time.Hour).Unix(),
				"quota_domain_id":  planQuotaDomainID(channel.Id, channel.Key),
				"quota_generation": "1",
				"quota_type":       "plan",
				"preserved_owner":  testCase.name,
			})
			injected := false
			const callbackName = "test:inject_concurrent_plan_quota_disable"
			require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if injected || tx.Statement == nil || tx.Statement.Table != "channels" {
					return
				}
				if _, singleChannel := tx.Statement.Dest.(*model.Channel); !singleChannel {
					return
				}
				injected = true
				writer := model.DB.Session(&gorm.Session{NewDB: true, SkipHooks: true})
				if _, inTransaction := tx.Statement.ConnPool.(*sql.Tx); inTransaction {
					writer = tx.Session(&gorm.Session{NewDB: true, SkipHooks: true})
				}
				domainHash, member := model.PlanQuotaDomainMembership(&channel)
				require.True(t, member)
				require.NoError(t, writer.Model(&model.PlanQuotaDomain{}).
					Where("credential_hash = ?", domainHash).
					Updates(map[string]any{
						"generation":     int64(1),
						"state":          model.PlanQuotaDomainStateDisabled,
						"disabled_until": now.Add(time.Hour).Unix(),
					}).Error)
				require.NoError(t, writer.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
					"status":     disabled.Status,
					"other_info": disabled.OtherInfo,
				}).Error)
				require.NoError(t, writer.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).
					Update("enabled", false).Error)
			}))
			t.Cleanup(func() {
				require.NoError(t, model.DB.Callback().Query().Remove(callbackName))
			})

			summary, err := ReconcileManagedUpstreams(now)

			require.NoError(t, err)
			require.True(t, injected)
			if testCase.initialState == model.UpstreamRouteStateShadow {
				assert.Equal(t, 1, summary.RoutesActivated)
			}
			assert.Equal(t, 1, summary.PrioritiesUpdated)

			var storedRoute model.UpstreamManagedRoute
			require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
			assert.Equal(t, model.UpstreamRouteStateActive, storedRoute.State)
			assert.Equal(t, 1, storedRoute.Rank)

			var storedChannel model.Channel
			require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
			assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannel.Status)
			assert.Equal(t, disabled.OtherInfo, storedChannel.OtherInfo)
			require.NotNil(t, storedChannel.Priority)
			assert.EqualValues(t, 999, *storedChannel.Priority)
			assert.Equal(t, source.SelectedEndpoint, storedChannel.GetBaseURL())
			assert.Equal(t, "gpt-4.1", storedChannel.Models)

			var abilities []model.Ability
			require.NoError(t, model.DB.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
			require.Len(t, abilities, 1)
			assert.False(t, abilities[0].Enabled)
			assert.Equal(t, "gpt-4.1", abilities[0].Model)
			require.NotNil(t, abilities[0].Priority)
			assert.EqualValues(t, 999, *abilities[0].Priority)
		})
	}
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

func TestDesiredManagedRouteStatePreservesManualPauseReason(t *testing.T) {
	now := time.Unix(1_788_320_000, 0)
	route := model.UpstreamManagedRoute{
		State:            model.UpstreamRouteStatePaused,
		ManualPauseUntil: now.Add(time.Hour).Unix(),
		LastReason:       "operator maintenance",
	}

	state, reason := desiredManagedRouteState(
		route,
		model.UpstreamSource{},
		model.UpstreamGroup{},
		now,
		&operation_setting.UpstreamOrchestrationSetting{},
	)

	assert.Equal(t, model.UpstreamRouteStatePaused, state)
	assert.Equal(t, route.LastReason, reason)
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
			name:     "operational quarantined waits for its own successful probe",
			route:    model.UpstreamManagedRoute{State: model.UpstreamRouteStateQuarantined, ConsecutiveSuccesses: 9},
			source:   baseSource,
			group:    model.UpstreamGroup{HealthStatus: model.UpstreamHealthOperational, ObservedAt: now.Unix()},
			expected: model.UpstreamRouteStateQuarantined,
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
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	channel := model.Channel{
		Name:   "managed-recovery-backoff",
		Key:    "credential",
		Status: common.ChannelStatusAutoDisabled,
		Models: "gpt-4.1",
		Group:  "default",
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "recovery",
		Platform:        "openai",
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateShadow,
	}
	require.NoError(t, model.DB.Create(&route).Error)
	expectedDelays := []int64{300, 900, 3600, 14400, 0}
	for attempt, expectedDelay := range expectedDelays {
		before := common.GetTimestamp()
		transition, err := MarkManagedRouteProbeResult(&route, false, 10, "failed")
		require.NoError(t, err)
		require.True(t, transition.Applied)
		var stored model.UpstreamManagedRoute
		require.NoError(t, model.DB.First(&stored, route.ID).Error)
		assert.Equal(t, attempt+1, stored.RecoveryAttempts)
		route = stored
		if expectedDelay == 0 {
			assert.Zero(t, stored.NextProbeAt)
			continue
		}
		assert.GreaterOrEqual(t, stored.NextProbeAt, before+expectedDelay)
		assert.LessOrEqual(t, stored.NextProbeAt, common.GetTimestamp()+expectedDelay)
	}
}

func TestManagedRouteStateActionsClearRank(t *testing.T) {
	tests := []struct {
		name         string
		initialState string
		action       func(int64) error
		wantState    string
	}{
		{
			name:         "pause",
			initialState: model.UpstreamRouteStateActive,
			action: func(routeID int64) error {
				return PauseManagedRoute(routeID, "operator pause")
			},
			wantState: model.UpstreamRouteStatePaused,
		},
		{
			name:         "resume",
			initialState: model.UpstreamRouteStatePaused,
			action:       ResumeManagedRoute,
			wantState:    model.UpstreamRouteStateShadow,
		},
		{
			name:         "detach",
			initialState: model.UpstreamRouteStateActive,
			action:       DetachManagedRoute,
			wantState:    model.UpstreamRouteStateDetached,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			setupUpstreamOrchestrationTest(t)
			require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
			setting := operation_setting.GetUpstreamOrchestrationSetting()
			originalSetting := *setting
			setting.ManualPauseHours = 1
			t.Cleanup(func() {
				*setting = originalSetting
			})

			channel := model.Channel{
				Name:   "rank-clear-" + testCase.name,
				Key:    "credential",
				Status: common.ChannelStatusEnabled,
				Models: "gpt-4.1",
				Group:  "default",
			}
			require.NoError(t, model.DB.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			route := model.UpstreamManagedRoute{
				SourceID:        1,
				ExternalGroupID: "rank-clear-" + testCase.name,
				Platform:        "openai",
				Protocol:        model.UpstreamProtocolOpenAI,
				ChannelID:       channel.Id,
				State:           testCase.initialState,
				Rank:            7,
			}
			require.NoError(t, model.DB.Create(&route).Error)

			require.NoError(t, testCase.action(route.ID))

			var stored model.UpstreamManagedRoute
			require.NoError(t, model.DB.First(&stored, route.ID).Error)
			assert.Equal(t, testCase.wantState, stored.State)
			assert.Zero(t, stored.Rank)
		})
	}
}

func TestManagedRouteStateActionsRollBackOnAbilityFailure(t *testing.T) {
	tests := []struct {
		name          string
		initialState  string
		initialStatus int
		action        func(int64) error
	}{
		{
			name:          "pause",
			initialState:  model.UpstreamRouteStateActive,
			initialStatus: common.ChannelStatusEnabled,
			action: func(routeID int64) error {
				return PauseManagedRoute(routeID, "operator pause")
			},
		},
		{
			name:          "resume",
			initialState:  model.UpstreamRouteStatePaused,
			initialStatus: common.ChannelStatusAutoDisabled,
			action:        ResumeManagedRoute,
		},
		{
			name:          "detach",
			initialState:  model.UpstreamRouteStateActive,
			initialStatus: common.ChannelStatusEnabled,
			action:        DetachManagedRoute,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			setupUpstreamOrchestrationTest(t)
			require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
			setting := operation_setting.GetUpstreamOrchestrationSetting()
			originalSetting := *setting
			setting.ManualPauseHours = 1
			t.Cleanup(func() {
				*setting = originalSetting
			})

			channel := model.Channel{
				Name:   "action-rollback-" + testCase.name,
				Key:    "credential",
				Status: testCase.initialStatus,
				Models: "gpt-4.1",
				Group:  "default",
			}
			channel.SetOtherInfo(map[string]any{"owner": "before"})
			require.NoError(t, model.DB.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			require.NoError(t, model.DB.Model(&model.Ability{}).
				Where("channel_id = ?", channel.Id).
				Update("enabled", testCase.initialStatus == common.ChannelStatusEnabled).Error)
			route := model.UpstreamManagedRoute{
				SourceID:        1,
				ExternalGroupID: "action-rollback-" + testCase.name,
				Platform:        "openai",
				Protocol:        model.UpstreamProtocolOpenAI,
				ChannelID:       channel.Id,
				State:           testCase.initialState,
				Rank:            7,
				UpdatedAt:       1_788_319_000,
			}
			if testCase.initialState == model.UpstreamRouteStatePaused {
				route.ManualPauseUntil = 1_788_330_000
				route.LastReason = "operator pause"
			}
			require.NoError(t, model.DB.Create(&route).Error)
			var expectedRoute model.UpstreamManagedRoute
			require.NoError(t, model.DB.First(&expectedRoute, route.ID).Error)
			var expectedChannel model.Channel
			require.NoError(t, model.DB.First(&expectedChannel, channel.Id).Error)
			var expectedAbility model.Ability
			require.NoError(t, model.DB.First(&expectedAbility, "channel_id = ?", channel.Id).Error)

			forcedErr := errors.New("forced " + testCase.name + " ability failure")
			callbackName := "test:fail_managed_action_ability_" + testCase.name
			require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Table == "abilities" {
					tx.AddError(forcedErr)
				}
			}))
			t.Cleanup(func() {
				require.NoError(t, model.DB.Callback().Update().Remove(callbackName))
			})

			err := testCase.action(route.ID)

			require.ErrorIs(t, err, forcedErr)
			var storedRoute model.UpstreamManagedRoute
			require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
			assert.Equal(t, expectedRoute, storedRoute)
			var storedChannel model.Channel
			require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
			assert.Equal(t, expectedChannel, storedChannel)
			var storedAbility model.Ability
			require.NoError(t, model.DB.First(&storedAbility, "channel_id = ?", channel.Id).Error)
			assert.Equal(t, expectedAbility, storedAbility)
		})
	}
}

func TestManagedRoutePauseResumePreservesPlanQuotaOwner(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	setting.ShadowSuccessesRequired = 1
	setting.ManualPauseHours = 1
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4.1":1}`))
	t.Cleanup(func() {
		*setting = originalSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
	})

	now := time.Now()
	source := model.UpstreamSource{
		Key:              "plan-owned-managed-route",
		Name:             "Plan owned managed route",
		ConsoleURL:       "https://example.com",
		SelectedEndpoint: "https://api.example.com",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotAt:   now.Unix(),
		LastSuccessAt:    now.Unix(),
		UpdatedAt:        now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "plan-owned-managed-route",
		Name:                "Plan owned managed route",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		HealthStatus:        model.UpstreamHealthOperational,
		Models:              `["gpt-4.1"]`,
		ObservedAt:          now.Unix(),
		UpdatedAt:           now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&group).Error)
	channel := model.Channel{
		Name:   "plan-owned-managed-route",
		Key:    "credential",
		Status: common.ChannelStatusAutoDisabled,
		Models: "gpt-4.1",
		Group:  "default",
	}
	disabledUntil := now.Add(time.Hour).Unix()
	channel.SetOtherInfo(map[string]any{
		"disabled_until":   disabledUntil,
		"quota_domain":     "plan:managed",
		"quota_domain_id":  "plan-domain",
		"quota_generation": "plan-generation",
		"quota_type":       "plan",
		"status_reason":    "monthly quota exhausted",
		"status_time":      int64(1_788_319_000),
	})
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	require.NoError(t, model.DB.Model(&model.Ability{}).
		Where("channel_id = ?", channel.Id).
		Update("enabled", true).Error)
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

	require.NoError(t, PauseManagedRoute(route.ID, "operator pause"))

	var pausedChannel model.Channel
	require.NoError(t, model.DB.First(&pausedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, pausedChannel.Status)
	pausedInfo := pausedChannel.GetOtherInfo()
	assert.EqualValues(t, disabledUntil, pausedInfo["disabled_until"])
	assert.Equal(t, "plan:managed", pausedInfo["quota_domain"])
	assert.Equal(t, "plan-domain", pausedInfo["quota_domain_id"])
	assert.Equal(t, "plan-generation", pausedInfo["quota_generation"])
	assert.Equal(t, "plan", pausedInfo["quota_type"])
	assert.Equal(t, "upstream orchestration manual pause", pausedInfo["status_reason"])
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)

	require.NoError(t, ResumeManagedRoute(route.ID))

	var resumedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&resumedRoute, route.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateShadow, resumedRoute.State)
	assert.Zero(t, resumedRoute.Rank)
	var resumedChannel model.Channel
	require.NoError(t, model.DB.First(&resumedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, resumedChannel.Status)
	assert.Equal(t, pausedChannel.OtherInfo, resumedChannel.OtherInfo)
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)

	transition, err := MarkManagedRouteProbeResult(&resumedRoute, true, 123, "")
	require.NoError(t, err)
	require.True(t, transition.Applied)
	assert.Equal(t, model.UpstreamRouteStateActive, transition.State)
	summary, err := ReconcileManagedUpstreams(time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, summary.PrioritiesUpdated)

	var reconciledRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&reconciledRoute, route.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateActive, reconciledRoute.State)
	assert.Positive(t, reconciledRoute.Rank)
	var reconciledChannel model.Channel
	require.NoError(t, model.DB.First(&reconciledChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reconciledChannel.Status)
	assert.Equal(t, pausedChannel.OtherInfo, reconciledChannel.OtherInfo)
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
}

func TestManagedRouteProbeQuarantineClearsRank(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.FailureThreshold = 1
	setting.FailureWindowMinutes = 5
	t.Cleanup(func() {
		*setting = originalSetting
	})

	channel := model.Channel{
		Name:   "probe-quarantine-rank",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-4.1",
		Group:  "default",
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "probe-quarantine-rank",
		Platform:        "openai",
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateActive,
		Rank:            7,
	}
	require.NoError(t, model.DB.Create(&route).Error)
	require.NoError(t, model.DB.First(&route, route.ID).Error)

	transition, err := MarkManagedRouteProbeResult(&route, false, 123, "probe failed")

	require.NoError(t, err)
	require.True(t, transition.Applied)
	assert.Equal(t, model.UpstreamRouteStateQuarantined, transition.State)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
	assert.Zero(t, storedRoute.Rank)
	var storedChannel model.Channel
	require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannel.Status)
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
}

func TestManagedRouteActiveProbeSuccessPreservesRank(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	route := model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "active-probe-rank",
		Platform:        "openai",
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       1002,
		State:           model.UpstreamRouteStateActive,
		Rank:            7,
	}
	require.NoError(t, model.DB.Create(&route).Error)
	require.NoError(t, model.DB.First(&route, route.ID).Error)

	transition, err := MarkManagedRouteProbeResult(&route, true, 123, "")

	require.NoError(t, err)
	require.True(t, transition.Applied)
	assert.Equal(t, model.UpstreamRouteStateActive, transition.State)
	var stored model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&stored, route.ID).Error)
	assert.Equal(t, 7, stored.Rank)
}

func TestManagedRouteProbeResultRejectsStalePauseAndDetach(t *testing.T) {
	tests := []struct {
		name      string
		succeeded bool
		state     string
		detached  bool
	}{
		{name: "successful probe after pause", succeeded: true, state: model.UpstreamRouteStatePaused},
		{name: "failed probe after pause", succeeded: false, state: model.UpstreamRouteStatePaused},
		{name: "successful probe after detach", succeeded: true, state: model.UpstreamRouteStateDetached, detached: true},
		{name: "failed probe after detach", succeeded: false, state: model.UpstreamRouteStateDetached, detached: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			setupUpstreamOrchestrationTest(t)
			require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

			setting := operation_setting.GetUpstreamOrchestrationSetting()
			originalSetting := *setting
			setting.Enabled = true
			setting.ShadowSuccessesRequired = 1
			t.Cleanup(func() {
				*setting = originalSetting
			})
			originalMemoryCacheEnabled := common.MemoryCacheEnabled
			common.MemoryCacheEnabled = true
			t.Cleanup(func() {
				common.MemoryCacheEnabled = originalMemoryCacheEnabled
			})

			channel := model.Channel{
				Name:   "stale-probe-" + testCase.name,
				Key:    "credential",
				Status: common.ChannelStatusAutoDisabled,
				Models: "gpt-4.1",
				Group:  "default",
			}
			require.NoError(t, model.DB.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			route := model.UpstreamManagedRoute{
				SourceID:             1,
				ExternalGroupID:      "stale-probe-" + testCase.name,
				Platform:             "openai",
				Protocol:             model.UpstreamProtocolOpenAI,
				ChannelID:            channel.Id,
				State:                model.UpstreamRouteStateShadow,
				ConsecutiveSuccesses: 0,
				NextProbeAt:          1_788_320_000,
				UpdatedAt:            1_788_319_000,
			}
			require.NoError(t, model.DB.Create(&route).Error)
			model.InitChannelCache()

			const pauseUntil = int64(1_788_330_000)
			require.NoError(t, model.DB.Model(&model.UpstreamManagedRoute{}).
				Where("id = ?", route.ID).
				Updates(map[string]any{
					"state":              testCase.state,
					"detached":           testCase.detached,
					"manual_pause_until": pauseUntil,
					"last_reason":        "operator transition",
					"updated_at":         int64(1_788_320_001),
				}).Error)

			transition, err := MarkManagedRouteProbeResult(
				&route,
				testCase.succeeded,
				123,
				"stale probe failure",
			)
			require.NoError(t, err)
			assert.False(t, transition.Applied)

			var storedRoute model.UpstreamManagedRoute
			require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
			assert.Equal(t, testCase.state, storedRoute.State)
			assert.Equal(t, testCase.detached, storedRoute.Detached)
			assert.Equal(t, pauseUntil, storedRoute.ManualPauseUntil)
			assert.Equal(t, "operator transition", storedRoute.LastReason)
			assert.Equal(t, int64(1_788_320_001), storedRoute.UpdatedAt)
			assert.Zero(t, storedRoute.LastProbeAt)
			assert.Zero(t, storedRoute.LastSuccessAt)
			assert.Zero(t, storedRoute.LastFailureAt)
			assert.Zero(t, storedRoute.RecoveryAttempts)

			var storedChannel model.Channel
			require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
			assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannel.Status)
			var ability model.Ability
			require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
			assert.False(t, ability.Enabled)
		})
	}
}

func TestManagedRouteProbeQuarantineRollsBackOnAbilityFailure(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.FailureThreshold = 2
	setting.FailureWindowMinutes = 5
	t.Cleanup(func() {
		*setting = originalSetting
	})
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
	})

	channel := model.Channel{
		Name:   "probe-quarantine-rollback",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-4.1",
		Group:  "default",
	}
	channel.SetOtherInfo(map[string]any{"owner": "before"})
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:            1,
		ExternalGroupID:     "probe-quarantine-rollback",
		Platform:            "openai",
		Protocol:            model.UpstreamProtocolOpenAI,
		ChannelID:           channel.Id,
		State:               model.UpstreamRouteStateActive,
		Rank:                7,
		ConsecutiveFailures: setting.FailureThreshold - 1,
		FailureWindowStart:  common.GetTimestamp(),
		UpdatedAt:           common.GetTimestamp() - 60,
	}
	require.NoError(t, model.DB.Create(&route).Error)
	var expectedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&expectedRoute, route.ID).Error)
	model.InitChannelCache()

	forcedErr := errors.New("forced probe quarantine ability failure")
	const callbackName = "test:fail_probe_quarantine_ability"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "abilities" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Update().Remove(callbackName))
	})

	_, err := MarkManagedRouteProbeResult(&expectedRoute, false, 123, "probe failed")

	require.ErrorIs(t, err, forcedErr)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, expectedRoute, storedRoute)
	var storedChannel model.Channel
	require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, storedChannel.Status)
	assert.Equal(t, channel.OtherInfo, storedChannel.OtherInfo)
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.True(t, ability.Enabled)
	cached, cacheErr := model.CacheGetChannel(channel.Id)
	require.NoError(t, cacheErr)
	assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
	assert.Equal(t, channel.OtherInfo, cached.OtherInfo)
}

func TestReconcileManagedUpstreamsBlocksHealthRecoveryDuringActivationWindow(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	setting.ShadowSuccessesRequired = 1
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4.1":1}`))
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		*setting = originalSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
	})

	_, _, channel, route := createPartialManagedRouteFixture(
		t,
		now,
		"activation-window",
		0.1,
		model.UpstreamHealthOperational,
		7,
	)
	require.NoError(t, model.DB.Model(&model.UpstreamManagedRoute{}).
		Where("id = ?", route.ID).
		Updates(map[string]any{
			"state":                 model.UpstreamRouteStateShadow,
			"consecutive_successes": 1,
		}).Error)
	require.NoError(t, model.DB.Model(&model.Channel{}).
		Where("id = ?", channel.Id).
		Update("status", common.ChannelStatusAutoDisabled).Error)
	require.NoError(t, model.DB.Model(&model.Ability{}).
		Where("channel_id = ?", channel.Id).
		Update("enabled", false).Error)
	var probeSnapshot model.Channel
	require.NoError(t, model.DB.First(&probeSnapshot, channel.Id).Error)
	model.InitChannelCache()

	injected := false
	windowRank := -1
	recovered := -1
	const callbackName = "test:recover_during_managed_activation_window"
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if injected || tx.Statement == nil || tx.Statement.Table != "upstream_managed_routes" {
			return
		}
		routes, ok := tx.Statement.Dest.(*[]model.UpstreamManagedRoute)
		if !ok {
			return
		}
		for _, current := range *routes {
			if current.ID != route.ID || current.State != model.UpstreamRouteStateActive {
				continue
			}
			injected = true
			windowRank = current.Rank
			recovered = EnableChannelForHealthCheck(&probeSnapshot, "")
			return
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Query().Remove(callbackName))
	})

	summary, err := ReconcileManagedUpstreams(now)

	require.NoError(t, err)
	require.True(t, injected)
	assert.Zero(t, windowRank)
	assert.Zero(t, recovered)
	assert.Equal(t, 1, summary.RoutesActivated)
	assert.Equal(t, 1, summary.PrioritiesUpdated)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateActive, storedRoute.State)
	assert.Equal(t, 1, storedRoute.Rank)
	var storedChannel model.Channel
	require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, storedChannel.Status)
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.True(t, ability.Enabled)
}

func TestManagedRouteProbeActivationWaitsForFencedReconcile(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))

	now := time.Unix(1_788_320_000, 0)
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	setting.ShadowSuccessesRequired = 1
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4.1":1}`))
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		*setting = originalSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
	})

	source := model.UpstreamSource{
		Key:              "probe-activation",
		Name:             "Probe activation",
		ConsoleURL:       "https://example.com",
		SelectedEndpoint: "https://api.example.com",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotAt:   now.Unix(),
		LastSuccessAt:    now.Unix(),
		UpdatedAt:        now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group",
		Name:                "Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		HealthStatus:        model.UpstreamHealthOperational,
		Models:              `["gpt-4.1"]`,
		ObservedAt:          now.Unix(),
		UpdatedAt:           now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&group).Error)
	channel := model.Channel{
		Name:   "probe-activation",
		Key:    "credential",
		Status: common.ChannelStatusAutoDisabled,
		Models: "gpt-4.1",
		Group:  "default",
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:        source.ID,
		ExternalGroupID: group.ExternalID,
		Platform:        group.Platform,
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateQuarantined,
		Rank:            7,
		NextProbeAt:     now.Unix(),
		UpdatedAt:       now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, model.DB.Create(&route).Error)
	model.InitChannelCache()

	transition, err := MarkManagedRouteProbeResult(&route, true, 123, "")
	require.NoError(t, err)
	require.True(t, transition.Applied)
	assert.Equal(t, model.UpstreamRouteStateActive, transition.State)

	var activatedRoute model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&activatedRoute, route.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateActive, activatedRoute.State)
	assert.Zero(t, activatedRoute.Rank)
	var beforeReconcile model.Channel
	require.NoError(t, model.DB.First(&beforeReconcile, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, beforeReconcile.Status)
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
	assert.Zero(t, EnableChannelForHealthCheck(&beforeReconcile, ""))
	require.NoError(t, model.DB.First(&beforeReconcile, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, beforeReconcile.Status)
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)

	summary, err := ReconcileManagedUpstreams(now)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.PrioritiesUpdated)
	require.NoError(t, model.DB.First(&activatedRoute, route.ID).Error)
	assert.Equal(t, 1, activatedRoute.Rank)
	var reconciled model.Channel
	require.NoError(t, model.DB.First(&reconciled, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, reconciled.Status)
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.True(t, ability.Enabled)
	cached, err := model.CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
}

func TestShadowProbeDoesNotEnableChannelBeforeOrchestration(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	original := *setting
	setting.Enabled = false
	setting.ShadowSuccessesRequired = 3
	t.Cleanup(func() {
		*setting = original
	})

	channel := model.Channel{
		Name:   "shadow-probe-disabled",
		Key:    "credential",
		Status: common.ChannelStatusAutoDisabled,
		Models: "gpt-4.1",
		Group:  "default",
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "20",
		Platform:        "openai",
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateShadow,
		NextProbeAt:     common.GetTimestamp(),
	}
	require.NoError(t, model.DB.Create(&route).Error)

	for attempt := 1; attempt <= 3; attempt++ {
		transition, err := MarkManagedRouteProbeResult(&route, true, int64(attempt), "")
		require.NoError(t, err)
		require.True(t, transition.Applied)
		require.NoError(t, model.DB.First(&route, route.ID).Error)
	}

	var stored model.UpstreamManagedRoute
	require.NoError(t, model.DB.First(&stored, route.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateShadow, stored.State)
	assert.Equal(t, 3, stored.ConsecutiveSuccesses)
	var storedChannel model.Channel
	require.NoError(t, model.DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannel.Status)
	var ability model.Ability
	require.NoError(t, model.DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
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
