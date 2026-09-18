package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestDispatchDeferredTaskRebuildsCredentialsAndSubmits(t *testing.T) {
	previousDB := model.DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousRedisEnabled := common.RedisEnabled
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Task{}))
	model.DB = database
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		common.MemoryCacheEnabled = previousMemoryCache
		common.RedisEnabled = previousRedisEnabled
	})
	require.NoError(t, database.Create(&model.User{
		Id:       71,
		Username: "deferred-user",
		Group:    "default",
		Status:   common.UserStatusEnabled,
	}).Error)
	token := &model.Token{
		UserId:      71,
		Key:         "deferred-token",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
	}
	require.NoError(t, database.Create(token).Error)

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		assert.Equal(t, "/kling/v1/videos/text2video", r.URL.Path)
		assert.Equal(t, "Bearer sk-current", r.Header.Get("Authorization"))
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)
		assert.Contains(t, string(body), `"model_name":"kling-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"upstream-deferred"}}`)
	}))
	defer upstream.Close()

	channel := &model.Channel{
		Type:    constant.ChannelTypeKling,
		Name:    "deferred-kling",
		Key:     "sk-current",
		BaseURL: &upstream.URL,
		Status:  common.ChannelStatusEnabled,
		Models:  "kling-v1",
		Group:   "default",
	}
	require.NoError(t, database.Create(channel).Error)

	task := &model.Task{
		TaskID:         "task_deferred_public",
		Platform:       constant.TaskPlatform("kling"),
		UserId:         71,
		Group:          "default",
		ChannelId:      channel.Id,
		Action:         constant.TaskActionTextToVideo,
		Status:         model.TaskStatusNotStart,
		Progress:       "0%",
		Quota:          123,
		ExecutionMode:  model.TaskExecutionModeDeferred,
		DispatchStatus: model.TaskDispatchStatusRunning,
		DispatchOwner:  "runner",
		Properties: model.Properties{
			OriginModelName:   "kling-v1",
			UpstreamModelName: "kling-v1",
		},
		PrivateData: model.TaskPrivateData{
			TokenId: token.Id,
			Execution: &model.TaskExecutionSnapshot{
				TaskPlugin: &model.TaskPluginSnapshot{Key: "kling", Version: "1.0.0"},
			},
			DeferredRequest: &model.TaskDeferredRequest{
				Path:        "/v1/responses",
				Method:      http.MethodPost,
				Headers:     map[string]string{"Content-Type": "application/json"},
				RouteBody:   json.RawMessage(`{"kind":"json","value":{"model":"kling-v1"}}`),
				RequestBody: json.RawMessage(`{"model":"kling-v1","prompt":"a lighthouse","duration":5}`),
			},
			BillingContext: &model.TaskBillingContext{
				OriginModelName: "kling-v1",
				GroupRatio:      1,
				PerCallBilling:  true,
			},
		},
	}

	privateJSON, err := json.Marshal(task.PrivateData)
	require.NoError(t, err)
	assert.NotContains(t, string(privateJSON), "sk-current")

	result, taskErr := dispatchDeferredTask(context.Background(), task)
	require.Nil(t, taskErr)
	require.NotNil(t, result)
	assert.Equal(t, "upstream-deferred", result.UpstreamTaskID)
	assert.Equal(t, 123, result.Quota)
	assert.Equal(t, 1, upstreamCalls)
}

func TestApplyDeferredTaskResultPersistsPluginState(t *testing.T) {
	task := &model.Task{
		PrivateData: model.TaskPrivateData{
			DeferredRequest: &model.TaskDeferredRequest{RequestBody: json.RawMessage(`{}`)},
			PluginState:     json.RawMessage(`{"round":"queued"}`),
		},
	}
	result := &relay.TaskSubmitResult{
		UpstreamTaskID: "upstream-task",
		PluginState:    []byte(`{"round":"submitted"}`),
	}

	applyDeferredTaskResult(task, result)

	assert.JSONEq(t, `{"round":"submitted"}`, string(task.PrivateData.PluginState))
	assert.Nil(t, task.PrivateData.DeferredRequest)
}

func TestDeferredTaskErrorRetryable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		taskErr  *dto.TaskError
		expected bool
	}{
		{"accepted stream 502", &dto.TaskError{StatusCode: http.StatusBadGateway, NoRetry: true}, false},
		{"ordinary 502", &dto.TaskError{StatusCode: http.StatusBadGateway}, true},
		{"ordinary 400", &dto.TaskError{StatusCode: http.StatusBadRequest}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, deferredTaskErrorRetryable(tc.taskErr))
		})
	}
}

func TestDeferredSubmissionPersistsBeforeCallingUpstream(t *testing.T) {
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousRedisEnabled := common.RedisEnabled
	previousLogConsume := common.LogConsumeEnabled
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(
		&model.User{},
		&model.Channel{},
		&model.Task{},
		&model.Log{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
	))
	model.DB = database
	model.LOG_DB = database
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	common.LogConsumeEnabled = false
	previousRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"kling-v1":1}`))
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.MemoryCacheEnabled = previousMemoryCache
		common.RedisEnabled = previousRedisEnabled
		common.LogConsumeEnabled = previousLogConsume
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios))
	})
	require.NoError(t, database.Create(&model.User{
		Id:       72,
		Username: "deferred-submit-user",
		Group:    "default",
		Quota:    1_000_000,
	}).Error)

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		http.Error(w, "must not be called during enqueue", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	channel := &model.Channel{
		Type:    constant.ChannelTypeKling,
		Name:    "deferred-kling-submit",
		Key:     "sk-current",
		BaseURL: &upstream.URL,
		Status:  common.ChannelStatusEnabled,
		Models:  "kling-v1",
		Group:   "default",
	}
	require.NoError(t, database.Create(channel).Error)

	generation := pluginruntime.DefaultRegistry.Generation()
	require.NotNil(t, generation)
	binding, found := generation.LookupDeclaredRoute(http.MethodPost, "/kling/v1/videos/text2video")
	require.True(t, found)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/kling/v1/videos/text2video",
		bytes.NewBufferString(`{"model_name":"kling-v1","prompt":"queued lighthouse"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: generation,
		Plugin:     binding.Plugin,
		Route:      binding.Route,
	})
	common.SetContextKey(c, constant.ContextKeyUserId, 72)
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserQuota, 1_000_000)
	middleware.PrepareTaskPluginRoute()(c)
	require.False(t, c.IsAborted(), recorder.Body.String())
	c.Set(pluginruntime.ContextKeyExecutionMode, pluginruntime.ExecutionModeDeferred)
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, channel, "kling-v1"))

	events := make([]string, 0, 2)
	billing := &taskSubmissionTestBilling{events: &events}
	info := &relaycommon.RelayInfo{
		UserId:          72,
		UserGroup:       "default",
		UsingGroup:      "default",
		UserQuota:       1_000_000,
		TokenGroup:      "default",
		OriginModelName: "kling-v1",
		Billing:         billing,
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			Action:        c.GetString("task_action"),
			PublicTaskID:  "task_deferred_submit",
			LockedChannel: channel,
		},
	}

	outcome, taskErr := executeTaskSubmissionWith(c, info, relay.RelayTaskSubmit)
	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	assert.Equal(t, 0, upstreamCalls)
	assert.Equal(t, []string{"reserve", "settle"}, events)
	assert.Equal(t, model.TaskExecutionModeDeferred, outcome.Task.ExecutionMode)
	assert.Equal(t, model.TaskDispatchStatusPending, outcome.Task.DispatchStatus)
	require.NotNil(t, outcome.Task.PrivateData.DeferredRequest)

	var stored model.Task
	require.NoError(t, database.Where("task_id = ?", "task_deferred_submit").First(&stored).Error)
	assert.Equal(t, model.TaskDispatchStatusPending, stored.DispatchStatus)
	require.NotNil(t, stored.PrivateData.DeferredRequest)
	privateJSON, err := json.Marshal(stored.PrivateData)
	require.NoError(t, err)
	assert.NotContains(t, string(privateJSON), "sk-current")
}
