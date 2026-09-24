package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunDueUpstreamProbeTaskCountsAndNotifiesOnlyPersistedRecoveries(t *testing.T) {
	tests := []struct {
		name              string
		planOwned         bool
		reconcileErr      error
		reconcileThenFail bool
		wantErr           bool
		wantEnabled       int
		wantNotifications int
	}{
		{
			name:              "enabled after reconcile",
			wantEnabled:       1,
			wantNotifications: 1,
		},
		{
			name:      "plan owner keeps channel disabled",
			planOwned: true,
		},
		{
			name:         "reconcile failure is not reported as recovery",
			reconcileErr: errors.New("forced managed reconcile failure"),
			wantErr:      true,
		},
		{
			name:              "reconcile partial commit is audited before returning its error",
			reconcileErr:      errors.New("forced stale reconcile failure"),
			reconcileThenFail: true,
			wantErr:           true,
			wantEnabled:       1,
			wantNotifications: 1,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(
				&model.UpstreamSource{},
				&model.UpstreamGroup{},
			))

			originalMemoryCacheEnabled := common.MemoryCacheEnabled
			originalLogConsumeEnabled := common.LogConsumeEnabled
			common.MemoryCacheEnabled = false
			common.LogConsumeEnabled = false
			setting := operation_setting.GetUpstreamOrchestrationSetting()
			originalSetting := *setting
			setting.Enabled = true
			setting.AutoEnroll = false
			setting.CandidateLimit = 5
			setting.MaxUpstreamMultiplier = 1
			setting.SyncIntervalHours = 4
			setting.ShadowSuccessesRequired = 1
			setting.ProbeTimeoutSeconds = 5
			originalModelRatios := ratio_setting.ModelRatio2JSONString()
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(
				`{"gpt-managed-probe-recovery":1}`,
			))
			t.Cleanup(func() {
				common.MemoryCacheEnabled = originalMemoryCacheEnabled
				common.LogConsumeEnabled = originalLogConsumeEnabled
				*setting = originalSetting
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
			})

			user := model.User{
				Username: "managed-probe-root",
				Role:     common.RoleRootUser,
				Status:   common.UserStatusEnabled,
				Group:    "default",
				Quota:    1_000_000,
			}
			require.NoError(t, db.Create(&user).Error)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{
					"id":"chatcmpl-managed-probe",
					"object":"chat.completion",
					"created":1,
					"model":"gpt-managed-probe-recovery",
					"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
					"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
				}`)
			}))
			t.Cleanup(upstream.Close)

			now := time.Now()
			source := model.UpstreamSource{
				Key:              "probe-source",
				Name:             "Probe source",
				ConsoleURL:       "https://example.com",
				SelectedEndpoint: upstream.URL,
				Status:           model.UpstreamHealthOperational,
				Enabled:          true,
				LastSnapshotAt:   now.Unix(),
				LastSuccessAt:    now.Unix(),
				UpdatedAt:        now.Add(-time.Minute).Unix(),
			}
			require.NoError(t, db.Create(&source).Error)
			group := model.UpstreamGroup{
				SourceID:            source.ID,
				ExternalID:          "probe-group",
				Name:                "Probe group",
				Platform:            "openai",
				EffectiveMultiplier: 0.1,
				HealthStatus:        model.UpstreamHealthOperational,
				Models:              `["gpt-managed-probe-recovery"]`,
				ObservedAt:          now.Unix(),
				UpdatedAt:           now.Add(-time.Minute).Unix(),
			}
			require.NoError(t, db.Create(&group).Error)
			tag := "managed:probe"
			channel := model.Channel{
				Name:    "managed-probe",
				Type:    constant.ChannelTypeOpenAI,
				Key:     "credential",
				BaseURL: &upstream.URL,
				Status:  common.ChannelStatusAutoDisabled,
				Tag:     &tag,
				Models:  "gpt-managed-probe-recovery",
				Group:   "default",
			}
			if testCase.planOwned {
				tag = "plan:managed:probe"
				channel.Tag = &tag
				channel.SetOtherInfo(map[string]any{
					"quota_domain": tag,
					"quota_type":   "plan",
				})
			}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			route := model.UpstreamManagedRoute{
				SourceID:        source.ID,
				ExternalGroupID: group.ExternalID,
				Platform:        group.Platform,
				Protocol:        model.UpstreamProtocolOpenAI,
				ChannelID:       channel.Id,
				State:           model.UpstreamRouteStateQuarantined,
				NextProbeAt:     now.Add(-time.Minute).Unix(),
				UpdatedAt:       now.Add(-time.Minute).Unix(),
			}
			require.NoError(t, db.Create(&route).Error)

			reconcile := service.ReconcileManagedUpstreams
			if testCase.reconcileErr != nil {
				reconcile = func(reconcileAt time.Time) (service.UpstreamReconcileSummary, error) {
					if testCase.reconcileThenFail {
						reconcileSummary, reconcileErr := service.ReconcileManagedUpstreams(reconcileAt)
						if reconcileErr != nil {
							return reconcileSummary, reconcileErr
						}
						return reconcileSummary, testCase.reconcileErr
					}
					return service.UpstreamReconcileSummary{}, testCase.reconcileErr
				}
			}
			notifications := 0
			notify := func(recovered *model.Channel) {
				notifications++
				assert.Equal(t, channel.Id, recovered.Id)
			}

			summary, err := runDueUpstreamProbeTaskWithDependencies(
				context.Background(),
				reconcile,
				notify,
			)

			if testCase.wantErr {
				require.ErrorIs(t, err, testCase.reconcileErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, 1, summary.Tested)
			assert.Equal(t, 1, summary.Succeeded)
			assert.Equal(t, testCase.wantEnabled, summary.Enabled)
			assert.Equal(t, testCase.wantNotifications, notifications)

			var storedChannel model.Channel
			require.NoError(t, db.First(&storedChannel, channel.Id).Error)
			var storedAbility model.Ability
			require.NoError(t, db.First(&storedAbility, "channel_id = ?", channel.Id).Error)
			if testCase.wantEnabled == 1 {
				assert.Equal(t, common.ChannelStatusEnabled, storedChannel.Status)
				assert.True(t, storedAbility.Enabled)
			} else {
				assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannel.Status)
				assert.False(t, storedAbility.Enabled)
			}
		})
	}
}

func TestRunDueUpstreamProbeTaskKeepsFailedSiblingQuarantinedAfterRealReconcile(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.UpstreamSource{},
		&model.UpstreamGroup{},
	))

	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalLogConsumeEnabled := common.LogConsumeEnabled
	common.MemoryCacheEnabled = false
	common.LogConsumeEnabled = false
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.AutoEnroll = false
	setting.CandidateLimit = 5
	setting.MaxUpstreamMultiplier = 1
	setting.SyncIntervalHours = 4
	setting.ShadowSuccessesRequired = 1
	setting.ProbeTimeoutSeconds = 5
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(
		`{"gpt-managed-probe-pair":1}`,
	))
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.LogConsumeEnabled = originalLogConsumeEnabled
		*setting = originalSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
	})

	user := model.User{
		Username: "managed-probe-pair-root",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1_000_000,
	}
	require.NoError(t, db.Create(&user).Error)

	successServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id":"chatcmpl-managed-probe-pair",
			"object":"chat.completion",
			"created":1,
			"model":"gpt-managed-probe-pair",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	t.Cleanup(successServer.Close)
	failureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":{"message":"probe failed","type":"server_error"}}`)
	}))
	t.Cleanup(failureServer.Close)

	now := time.Now()
	createRoute := func(suffix string, endpoint string) (model.Channel, model.UpstreamManagedRoute) {
		source := model.UpstreamSource{
			Key:              "probe-pair-" + suffix,
			Name:             "Probe pair " + suffix,
			ConsoleURL:       "https://example.com",
			SelectedEndpoint: endpoint,
			Status:           model.UpstreamHealthOperational,
			Enabled:          true,
			LastSnapshotAt:   now.Unix(),
			LastSuccessAt:    now.Unix(),
			UpdatedAt:        now.Add(-time.Minute).Unix(),
		}
		require.NoError(t, db.Create(&source).Error)
		group := model.UpstreamGroup{
			SourceID:            source.ID,
			ExternalID:          "probe-pair-" + suffix,
			Name:                "Probe pair " + suffix,
			Platform:            "openai",
			EffectiveMultiplier: 0.1,
			HealthStatus:        model.UpstreamHealthOperational,
			Models:              `["gpt-managed-probe-pair"]`,
			ObservedAt:          now.Unix(),
			UpdatedAt:           now.Add(-time.Minute).Unix(),
		}
		require.NoError(t, db.Create(&group).Error)
		tag := "managed:probe-pair:" + suffix
		channel := model.Channel{
			Name:    "managed-probe-pair-" + suffix,
			Type:    constant.ChannelTypeOpenAI,
			Key:     "credential-" + suffix,
			BaseURL: &endpoint,
			Status:  common.ChannelStatusAutoDisabled,
			Tag:     &tag,
			Models:  "gpt-managed-probe-pair",
			Group:   "default",
		}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
		route := model.UpstreamManagedRoute{
			SourceID:        source.ID,
			ExternalGroupID: group.ExternalID,
			Platform:        group.Platform,
			Protocol:        model.UpstreamProtocolOpenAI,
			ChannelID:       channel.Id,
			State:           model.UpstreamRouteStateQuarantined,
			NextProbeAt:     now.Add(-time.Minute).Unix(),
			UpdatedAt:       now.Add(-time.Minute).Unix(),
		}
		require.NoError(t, db.Create(&route).Error)
		return channel, route
	}
	successChannel, successRoute := createRoute("success", successServer.URL)
	failureChannel, failureRoute := createRoute("failure", failureServer.URL)

	notifications := 0
	summary, err := runDueUpstreamProbeTaskWithDependencies(
		context.Background(),
		service.ReconcileManagedUpstreams,
		func(*model.Channel) {
			notifications++
		},
	)

	require.NoError(t, err)
	assert.Equal(t, 2, summary.Tested)
	assert.Equal(t, 1, summary.Succeeded)
	assert.Equal(t, 1, summary.Failed)
	assert.Equal(t, 1, summary.Enabled)
	assert.Equal(t, 1, notifications)

	var storedSuccessRoute model.UpstreamManagedRoute
	require.NoError(t, db.First(&storedSuccessRoute, successRoute.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateActive, storedSuccessRoute.State)
	assert.Positive(t, storedSuccessRoute.Rank)
	var storedSuccessChannel model.Channel
	require.NoError(t, db.First(&storedSuccessChannel, successChannel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, storedSuccessChannel.Status)
	var successAbility model.Ability
	require.NoError(t, db.First(&successAbility, "channel_id = ?", successChannel.Id).Error)
	assert.True(t, successAbility.Enabled)

	var storedFailureRoute model.UpstreamManagedRoute
	require.NoError(t, db.First(&storedFailureRoute, failureRoute.ID).Error)
	assert.Equal(t, model.UpstreamRouteStateQuarantined, storedFailureRoute.State)
	assert.Zero(t, storedFailureRoute.Rank)
	var storedFailureChannel model.Channel
	require.NoError(t, db.First(&storedFailureChannel, failureChannel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedFailureChannel.Status)
	var failureAbility model.Ability
	require.NoError(t, db.First(&failureAbility, "channel_id = ?", failureChannel.Id).Error)
	assert.False(t, failureAbility.Enabled)
}

func TestRunDueUpstreamProbeTaskConsumesPlanQuotaOnceForActualCredential(t *testing.T) {
	tests := []struct {
		name             string
		multiKey         bool
		plan             bool
		automaticOff     bool
		failureThreshold int
		status           int
		body             string
	}{
		{
			name:             "single key isolates credential peers without route failure",
			plan:             true,
			failureThreshold: 1,
			status:           http.StatusTooManyRequests,
			body: `{"error":{"message":"You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC.",
				"type":"AccountQuotaExceeded","code":"AccountQuotaExceeded"}}`,
		},
		{
			name:             "multi key isolates selected key without route failure",
			multiKey:         true,
			plan:             true,
			failureThreshold: 1,
			status:           http.StatusTooManyRequests,
			body: `{"error":{"message":"You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC.",
				"type":"AccountQuotaExceeded","code":"AccountQuotaExceeded"}}`,
		},
		{
			name:             "single key automatic disable off leaves channel and route unchanged",
			plan:             true,
			automaticOff:     true,
			failureThreshold: 1,
			status:           http.StatusTooManyRequests,
			body: `{"error":{"message":"You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC.",
				"type":"AccountQuotaExceeded","code":"AccountQuotaExceeded"}}`,
		},
		{
			name:             "multi key automatic disable off leaves channel and route unchanged",
			multiKey:         true,
			plan:             true,
			automaticOff:     true,
			failureThreshold: 1,
			status:           http.StatusTooManyRequests,
			body: `{"error":{"message":"You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC.",
				"type":"AccountQuotaExceeded","code":"AccountQuotaExceeded"}}`,
		},
		{
			name:   "ordinary managed failure is counted once",
			status: http.StatusInternalServerError,
			body:   `{"error":{"message":"ordinary managed failure","type":"server_error","code":"server_error"}}`,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(
				&model.UpstreamSource{},
				&model.UpstreamGroup{},
			))

			originalMemoryCacheEnabled := common.MemoryCacheEnabled
			originalAutomaticDisableEnabled := common.AutomaticDisableChannelEnabled
			originalLogConsumeEnabled := common.LogConsumeEnabled
			common.MemoryCacheEnabled = false
			common.AutomaticDisableChannelEnabled = !testCase.automaticOff
			common.LogConsumeEnabled = false
			setting := operation_setting.GetUpstreamOrchestrationSetting()
			originalSetting := *setting
			setting.Enabled = true
			setting.FailureThreshold = testCase.failureThreshold
			if setting.FailureThreshold == 0 {
				setting.FailureThreshold = 2
			}
			setting.FailureWindowMinutes = 5
			setting.ProbeTimeoutSeconds = 5
			originalModelRatios := ratio_setting.ModelRatio2JSONString()
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(
				`{"gpt-managed-plan-probe":1}`,
			))
			t.Cleanup(func() {
				common.MemoryCacheEnabled = originalMemoryCacheEnabled
				common.AutomaticDisableChannelEnabled = originalAutomaticDisableEnabled
				common.LogConsumeEnabled = originalLogConsumeEnabled
				*setting = originalSetting
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
			})

			user := model.User{
				Username: "managed-plan-probe-root",
				Role:     common.RoleRootUser,
				Status:   common.UserStatusEnabled,
				Group:    "default",
				Quota:    1_000_000,
			}
			require.NoError(t, db.Create(&user).Error)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testCase.status)
				_, _ = fmt.Fprint(w, testCase.body)
			}))
			t.Cleanup(upstream.Close)

			now := time.Now()
			source := model.UpstreamSource{
				Key:              "managed-plan-probe-source",
				Name:             "Managed Plan probe source",
				ConsoleURL:       "https://example.com",
				SelectedEndpoint: upstream.URL,
				Status:           model.UpstreamHealthOperational,
				Enabled:          true,
				LastSnapshotAt:   now.Unix(),
				LastSuccessAt:    now.Unix(),
				UpdatedAt:        now.Add(-time.Minute).Unix(),
			}
			require.NoError(t, db.Create(&source).Error)
			group := model.UpstreamGroup{
				SourceID:            source.ID,
				ExternalID:          "managed-plan-probe-group",
				Name:                "Managed Plan probe group",
				Platform:            "openai",
				EffectiveMultiplier: 0.1,
				HealthStatus:        model.UpstreamHealthOperational,
				Models:              `["gpt-managed-plan-probe"]`,
				ObservedAt:          now.Unix(),
				UpdatedAt:           now.Add(-time.Minute).Unix(),
			}
			require.NoError(t, db.Create(&group).Error)

			autoBan := 1
			tag := "managed:ordinary-probe"
			if testCase.plan {
				tag = "plan:managed:probe"
			}
			key := "probe-key"
			channelInfo := model.ChannelInfo{}
			if testCase.multiKey {
				multiKeyMode := constant.MultiKeyModePolling
				if testCase.automaticOff {
					multiKeyMode = constant.MultiKeyModeRandom
				}
				key = "probe-key-a\nprobe-key-b"
				channelInfo = model.ChannelInfo{
					IsMultiKey:   true,
					MultiKeySize: 2,
					MultiKeyMode: multiKeyMode,
				}
			}
			channel := model.Channel{
				Name: "managed-plan-probe", Type: constant.ChannelTypeOpenAI,
				Key: key, BaseURL: &upstream.URL,
				Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan,
				Models: "gpt-managed-plan-probe", Group: "default",
				ChannelInfo: channelInfo,
			}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			route := model.UpstreamManagedRoute{
				SourceID: source.ID, ExternalGroupID: group.ExternalID,
				Platform: group.Platform, Protocol: model.UpstreamProtocolOpenAI,
				ChannelID: channel.Id, State: model.UpstreamRouteStateActive,
				Rank: 3, ConsecutiveSuccesses: 2,
				LastSuccessAt: now.Add(-2 * time.Minute).Unix(),
				NextProbeAt:   now.Add(-time.Minute).Unix(),
			}
			require.NoError(t, db.Create(&route).Error)

			var peer model.Channel
			if testCase.plan && !testCase.multiKey {
				peerTag := "plan:managed:probe-peer"
				peer = model.Channel{
					Name: "managed-plan-probe-peer", Type: constant.ChannelTypeOpenAI,
					Key: channel.Key, BaseURL: &upstream.URL,
					Status: common.ChannelStatusEnabled, Tag: &peerTag, AutoBan: &autoBan,
					Models: channel.Models, Group: channel.Group,
				}
				require.NoError(t, db.Create(&peer).Error)
				require.NoError(t, peer.AddAbilities(nil))
			}

			summary, err := runDueUpstreamProbeTaskWithDependencies(
				context.Background(),
				func(time.Time) (service.UpstreamReconcileSummary, error) {
					t.Fatal("failed probes must not reconcile managed upstreams")
					return service.UpstreamReconcileSummary{}, nil
				},
				func(*model.Channel) {
					t.Fatal("failed probes must not notify recovery")
				},
			)

			require.NoError(t, err)
			assert.Equal(t, upstreamProbeSummary{Tested: 1, Failed: 1}, summary)
			var storedRoute model.UpstreamManagedRoute
			require.NoError(t, db.First(&storedRoute, route.ID).Error)
			if testCase.plan {
				assert.Equal(t, route, storedRoute)
			} else {
				assert.Equal(t, model.UpstreamRouteStateActive, storedRoute.State)
				assert.Equal(t, 1, storedRoute.ConsecutiveFailures)
				assert.Zero(t, storedRoute.ConsecutiveSuccesses)
			}

			var stored model.Channel
			require.NoError(t, db.First(&stored, channel.Id).Error)
			var ability model.Ability
			require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
			if testCase.plan && !testCase.multiKey {
				domainHash, ok := model.PlanQuotaDomainHash(channel.Key)
				require.True(t, ok)
				var authority model.PlanQuotaDomain
				require.NoError(t, db.First(&authority, "credential_hash = ?", domainHash).Error)
				if testCase.automaticOff {
					assert.Equal(t, model.PlanQuotaDomainStateActive, authority.State)
				} else {
					assert.Equal(t, model.PlanQuotaDomainStateDisabled, authority.State)
					assert.Positive(t, authority.DisabledUntil)
				}
			}
			switch {
			case testCase.automaticOff:
				assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
				assert.Empty(t, stored.OtherInfo)
				assert.Equal(t, channel.ChannelInfo, stored.ChannelInfo)
				assert.True(t, ability.Enabled)
				if !testCase.multiKey {
					var storedPeer model.Channel
					require.NoError(t, db.First(&storedPeer, peer.Id).Error)
					assert.Equal(t, common.ChannelStatusEnabled, storedPeer.Status)
					assert.Empty(t, storedPeer.OtherInfo)
					var peerAbility model.Ability
					require.NoError(t, db.First(&peerAbility, "channel_id = ?", peer.Id).Error)
					assert.True(t, peerAbility.Enabled)
				}
			case !testCase.plan:
				assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
				assert.NotContains(t, stored.GetOtherInfo(), "quota_reset_at")
				assert.True(t, ability.Enabled)
			case testCase.multiKey:
				assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
				assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
				assert.NotContains(t, stored.ChannelInfo.MultiKeyStatusList, 1)
				assert.Positive(t, stored.ChannelInfo.MultiKeyDisabledUntil[0])
				assert.True(t, ability.Enabled)
				selected, selectErr := model.GetRandomSatisfiedChannel(
					stored.Group,
					stored.Models,
					0,
					nil,
				)
				require.NoError(t, selectErr)
				require.NotNil(t, selected)
				assert.Equal(t, stored.Id, selected.Id)
				selectedKey, _, keyErr := selected.GetNextEnabledKey()
				require.Nil(t, keyErr)
				assert.Equal(t, "probe-key-b", selectedKey)
			default:
				var storedPeer model.Channel
				require.NoError(t, db.First(&storedPeer, peer.Id).Error)
				assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
				assert.Equal(t, common.ChannelStatusAutoDisabled, storedPeer.Status)
				assert.NotZero(t, stored.GetOtherInfo()["quota_reset_at"])
				assert.Equal(t, stored.GetOtherInfo()["quota_reset_at"], storedPeer.GetOtherInfo()["quota_reset_at"])
				assert.NotEmpty(t, stored.GetOtherInfo()["quota_domain_id"])
				assert.Equal(t, stored.GetOtherInfo()["quota_domain_id"], storedPeer.GetOtherInfo()["quota_domain_id"])
				assert.False(t, ability.Enabled)
				var peerAbility model.Ability
				require.NoError(t, db.First(&peerAbility, "channel_id = ?", peer.Id).Error)
				assert.False(t, peerAbility.Enabled)
			}
		})
	}
}
