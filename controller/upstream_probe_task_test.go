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
