package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
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

type deferredPlanQuotaDispatchFixture struct {
	database *gorm.DB
	user     model.User
	token    model.Token
	channel  model.Channel
	task     model.Task
}

func newDeferredPlanQuotaDispatchFixture(
	t *testing.T,
	channelKey string,
	channelInfo model.ChannelInfo,
	upstreamURL string,
	tag string,
) deferredPlanQuotaDispatchFixture {
	t.Helper()

	database := setupModelListControllerTestDB(t)
	require.NoError(t, database.AutoMigrate(
		&model.Token{},
		&model.Task{},
		&model.Log{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
	))

	previousMemoryCache := common.MemoryCacheEnabled
	previousRedisEnabled := common.RedisEnabled
	previousBatchUpdateEnabled := common.BatchUpdateEnabled
	previousAutomaticDisableEnabled := common.AutomaticDisableChannelEnabled
	previousErrorLogEnabled := constant.ErrorLogEnabled
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.AutomaticDisableChannelEnabled = true
	constant.ErrorLogEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCache
		common.RedisEnabled = previousRedisEnabled
		common.BatchUpdateEnabled = previousBatchUpdateEnabled
		common.AutomaticDisableChannelEnabled = previousAutomaticDisableEnabled
		constant.ErrorLogEnabled = previousErrorLogEnabled
	})

	user := model.User{
		Username:  "deferred-plan-user",
		Role:      common.RoleRootUser,
		Group:     "default",
		Status:    common.UserStatusEnabled,
		Quota:     900,
		UsedQuota: 100,
	}
	require.NoError(t, database.Create(&user).Error)
	token := model.Token{
		UserId:      user.Id,
		Key:         "deferred-plan-token",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 900,
		UsedQuota:   100,
	}
	require.NoError(t, database.Create(&token).Error)
	autoBan := 1
	channel := model.Channel{
		Type:        constant.ChannelTypeKling,
		Name:        "deferred-plan-channel",
		Key:         channelKey,
		BaseURL:     &upstreamURL,
		Status:      common.ChannelStatusEnabled,
		Models:      "kling-v1",
		Group:       "default",
		UsedQuota:   100,
		AutoBan:     &autoBan,
		Tag:         &tag,
		ChannelInfo: channelInfo,
	}
	require.NoError(t, database.Create(&channel).Error)

	task := model.Task{
		TaskID:         "task_deferred_plan",
		Platform:       constant.TaskPlatform("kling"),
		UserId:         user.Id,
		Group:          "default",
		ChannelId:      channel.Id,
		Action:         constant.TaskActionTextToVideo,
		Status:         model.TaskStatusNotStart,
		Progress:       "0%",
		Quota:          100,
		SubmitTime:     common.GetTimestamp(),
		ExecutionMode:  model.TaskExecutionModeDeferred,
		DispatchStatus: model.TaskDispatchStatusPending,
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
	require.NoError(t, database.Create(&task).Error)

	return deferredPlanQuotaDispatchFixture{
		database: database,
		user:     user,
		token:    token,
		channel:  channel,
		task:     task,
	}
}

func runDeferredDispatcherOnce(t *testing.T, runnerID string) deferredTaskDispatchSummary {
	t.Helper()

	systemTask, err := model.CreateSystemTask(model.SystemTaskTypeDeferredDispatch, nil, nil)
	require.NoError(t, err)
	claimedTask, claimed, err := model.ClaimSystemTask(
		systemTask.ID,
		model.SystemTaskTypeDeferredDispatch,
		runnerID,
		common.GetTimestamp()+60,
	)
	require.NoError(t, err)
	require.True(t, claimed)

	deferredTaskDispatchHandler{}.Run(context.Background(), claimedTask, runnerID)

	storedTask, err := model.GetSystemTaskByTaskID(systemTask.TaskID)
	require.NoError(t, err)
	require.NotNil(t, storedTask)
	require.Equal(t, model.SystemTaskStatusSucceeded, storedTask.Status)
	var summary deferredTaskDispatchSummary
	require.NoError(t, common.UnmarshalJsonStr(storedTask.Result, &summary))
	return summary
}

func countDeferredTaskRefunds(t *testing.T, database *gorm.DB, taskID string) int64 {
	t.Helper()

	var count int64
	require.NoError(t, database.Model(&model.Log{}).
		Where("type = ? AND other LIKE ?", model.LogTypeRefund, "%"+taskID+"%").
		Count(&count).Error)
	return count
}

func TestDeferredDispatcherPlanQuotaPersistenceFailureIsNotRequeued(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC.","type":"AccountQuotaExceeded","code":"AccountQuotaExceeded"}}`)
	}))
	t.Cleanup(upstream.Close)

	fixture := newDeferredPlanQuotaDispatchFixture(
		t,
		"sk-deferred-persistence-failure",
		model.ChannelInfo{},
		upstream.URL,
		"plan:deferred:persistence-failure",
	)
	forcedErr := errors.New("forced deferred Plan quota persistence failure")
	const callbackName = "test:deferred_plan_quota_persistence_failure"
	require.NoError(t, fixture.database.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil &&
			tx.Statement.Schema != nil &&
			tx.Statement.Schema.Name == "Ability" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, fixture.database.Callback().Update().Remove(callbackName))
	})

	result, taskErr := dispatchDeferredTask(context.Background(), &fixture.task)
	assert.Nil(t, result)
	require.NotNil(t, taskErr)
	assert.True(t, taskErr.LocalError)
	assert.Equal(t, string(relaytypes.ErrorCodeUpdateDataError), taskErr.Code)
	assert.ErrorIs(t, taskErr.Error, forcedErr)
	assert.True(t, taskErr.NoRetry)

	summary := runDeferredDispatcherOnce(t, "deferred-persistence-failure")
	assert.Equal(t, deferredTaskDispatchSummary{Found: 1, Claimed: 1, Failed: 1}, summary)

	var failedTask model.Task
	require.NoError(t, fixture.database.First(&failedTask, fixture.task.ID).Error)
	assert.Equal(t, model.TaskDispatchStatusFailed, failedTask.DispatchStatus)
	assert.Equal(t, 1, failedTask.DispatchAttempts)
	assert.Equal(t, 2, upstreamCalls)

	assert.Equal(t, deferredTaskDispatchSummary{}, runDeferredDispatcherOnce(t, "deferred-persistence-failure-recheck"))
	assert.Equal(t, 2, upstreamCalls)
}

func TestDeferredDispatcherSingleKeyPlanQuotaRequeuesThenFailsAndRefundsOnce(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC.","type":"AccountQuotaExceeded","code":"AccountQuotaExceeded"}}`)
	}))
	t.Cleanup(upstream.Close)

	fixture := newDeferredPlanQuotaDispatchFixture(
		t,
		"sk-deferred-single",
		model.ChannelInfo{},
		upstream.URL,
		"plan:deferred:single",
	)
	peerTag := "plan:deferred:single-peer"
	peer := model.Channel{
		Type:      constant.ChannelTypeKling,
		Name:      "deferred-plan-peer",
		Key:       fixture.channel.Key,
		BaseURL:   &upstream.URL,
		Status:    common.ChannelStatusEnabled,
		Models:    "kling-v1",
		Group:     "default",
		UsedQuota: 100,
		AutoBan:   fixture.channel.AutoBan,
		Tag:       &peerTag,
	}
	require.NoError(t, fixture.database.Create(&peer).Error)

	firstSummary := runDeferredDispatcherOnce(t, "deferred-single-first")
	assert.Equal(t, deferredTaskDispatchSummary{Found: 1, Claimed: 1, Requeued: 1}, firstSummary)

	var firstTask model.Task
	require.NoError(t, fixture.database.First(&firstTask, fixture.task.ID).Error)
	assert.Equal(t, model.TaskDispatchStatusPending, firstTask.DispatchStatus)
	assert.Equal(t, 1, firstTask.DispatchAttempts)
	assert.Equal(t, 100, firstTask.Quota)
	var firstChannels []model.Channel
	require.NoError(t, fixture.database.Order("id").Find(&firstChannels).Error)
	require.Len(t, firstChannels, 2)
	assert.Equal(t, common.ChannelStatusAutoDisabled, firstChannels[0].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, firstChannels[1].Status)
	assert.Zero(t, countDeferredTaskRefunds(t, fixture.database, fixture.task.TaskID))

	secondSummary := runDeferredDispatcherOnce(t, "deferred-single-second")
	assert.Equal(t, deferredTaskDispatchSummary{Found: 1, Claimed: 1, Failed: 1}, secondSummary)

	var failedTask model.Task
	require.NoError(t, fixture.database.First(&failedTask, fixture.task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), failedTask.Status)
	assert.Equal(t, model.TaskDispatchStatusFailed, failedTask.DispatchStatus)
	assert.Equal(t, 2, failedTask.DispatchAttempts)
	assert.Zero(t, failedTask.Quota)
	assert.Equal(t, 1, upstreamCalls)
	var refundedUser model.User
	require.NoError(t, fixture.database.First(&refundedUser, fixture.user.Id).Error)
	assert.Equal(t, 1_000, refundedUser.Quota)
	assert.Zero(t, refundedUser.UsedQuota)
	var refundedToken model.Token
	require.NoError(t, fixture.database.First(&refundedToken, fixture.token.Id).Error)
	assert.Equal(t, 1_000, refundedToken.RemainQuota)
	assert.Zero(t, refundedToken.UsedQuota)
	assert.Equal(t, int64(1), countDeferredTaskRefunds(t, fixture.database, fixture.task.TaskID))

	thirdSummary := runDeferredDispatcherOnce(t, "deferred-single-third")
	assert.Equal(t, deferredTaskDispatchSummary{}, thirdSummary)
	require.NoError(t, fixture.database.First(&refundedUser, fixture.user.Id).Error)
	assert.Equal(t, 1_000, refundedUser.Quota)
	assert.Equal(t, int64(1), countDeferredTaskRefunds(t, fixture.database, fixture.task.TaskID))
}

func TestDeferredDispatcherMultiKeyPlanQuotaUsesRemainingKeyWithoutRefund(t *testing.T) {
	usedKeys := make([]string, 0, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("Authorization") {
		case "Bearer sk-deferred-a":
			usedKeys = append(usedKeys, "first")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC.","type":"AccountQuotaExceeded","code":"AccountQuotaExceeded"}}`)
		case "Bearer sk-deferred-b":
			usedKeys = append(usedKeys, "second")
			_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"upstream-deferred-plan"}}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"unexpected credential","type":"authentication_error","code":"invalid_api_key"}}`)
		}
	}))
	t.Cleanup(upstream.Close)

	fixture := newDeferredPlanQuotaDispatchFixture(
		t,
		"sk-deferred-a\nsk-deferred-b",
		model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
		upstream.URL,
		"plan:deferred:multi",
	)

	firstSummary := runDeferredDispatcherOnce(t, "deferred-multi-first")
	assert.Equal(t, deferredTaskDispatchSummary{Found: 1, Claimed: 1, Requeued: 1}, firstSummary)

	var firstTask model.Task
	require.NoError(t, fixture.database.First(&firstTask, fixture.task.ID).Error)
	assert.Equal(t, model.TaskDispatchStatusPending, firstTask.DispatchStatus)
	assert.Equal(t, 1, firstTask.DispatchAttempts)
	assert.Equal(t, 100, firstTask.Quota)
	var firstChannel model.Channel
	require.NoError(t, fixture.database.First(&firstChannel, fixture.channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, firstChannel.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, firstChannel.ChannelInfo.MultiKeyStatusList[0])
	assert.NotContains(t, firstChannel.ChannelInfo.MultiKeyStatusList, 1)
	assert.Zero(t, countDeferredTaskRefunds(t, fixture.database, fixture.task.TaskID))

	secondSummary := runDeferredDispatcherOnce(t, "deferred-multi-second")
	assert.Equal(t, deferredTaskDispatchSummary{Found: 1, Claimed: 1, Dispatched: 1}, secondSummary)

	var completedTask model.Task
	require.NoError(t, fixture.database.First(&completedTask, fixture.task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), completedTask.Status)
	assert.Equal(t, model.TaskDispatchStatusDispatched, completedTask.DispatchStatus)
	assert.Equal(t, 2, completedTask.DispatchAttempts)
	assert.Equal(t, 100, completedTask.Quota)
	assert.Equal(t, []string{"first", "second"}, usedKeys)
	var unrefundedUser model.User
	require.NoError(t, fixture.database.First(&unrefundedUser, fixture.user.Id).Error)
	assert.Equal(t, 900, unrefundedUser.Quota)
	assert.Equal(t, 100, unrefundedUser.UsedQuota)
	var unrefundedToken model.Token
	require.NoError(t, fixture.database.First(&unrefundedToken, fixture.token.Id).Error)
	assert.Equal(t, 900, unrefundedToken.RemainQuota)
	assert.Equal(t, 100, unrefundedToken.UsedQuota)
	assert.Zero(t, countDeferredTaskRefunds(t, fixture.database, fixture.task.TaskID))
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
