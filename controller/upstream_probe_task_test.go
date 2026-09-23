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
				reconcile = func(time.Time) (service.UpstreamReconcileSummary, error) {
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
