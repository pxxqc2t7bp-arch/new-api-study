package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const relayFailoverModel = "gpt-3.5-turbo"

type failoverUpstream struct {
	name       string
	statusCode int
	disconnect bool
	trace      *failoverCallTrace
	mu         sync.Mutex
	calls      int
	paths      []string
	bodies     [][]byte
	server     *httptest.Server
}

type failoverCallTrace struct {
	mu    sync.Mutex
	names []string
}

func (t *failoverCallTrace) append(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.names = append(t.names, name)
}

func (t *failoverCallTrace) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.names...)
}

func newFailoverUpstream(t *testing.T, name string, statusCode int, traces ...*failoverCallTrace) *failoverUpstream {
	t.Helper()
	upstream := &failoverUpstream{name: name, statusCode: statusCode}
	if len(traces) > 0 {
		upstream.trace = traces[0]
	}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.calls++
		upstream.paths = append(upstream.paths, r.URL.Path)
		upstream.bodies = append(upstream.bodies, append([]byte(nil), body...))
		statusCode := upstream.statusCode
		disconnect := upstream.disconnect
		upstream.mu.Unlock()
		if upstream.trace != nil {
			upstream.trace.append(name)
		}

		if disconnect {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				return
			}
			_ = connection.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if statusCode >= http.StatusBadRequest {
			w.WriteHeader(statusCode)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"%s failure","type":"upstream_error","code":"ha_test"}}`, name)
			return
		}
		_, _ = fmt.Fprintf(w, `{
			"id":"chatcmpl-ha-test",
			"object":"chat.completion",
			"created":1,
			"model":"%s",
			"choices":[{"index":0,"message":{"role":"assistant","content":"%s"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
		}`, relayFailoverModel, name)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func newDisconnectingFailoverUpstream(t *testing.T, name string, trace *failoverCallTrace) *failoverUpstream {
	t.Helper()
	upstream := newFailoverUpstream(t, name, http.StatusOK, trace)
	upstream.disconnect = true
	return upstream
}

func (u *failoverUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *failoverUpstream) setStatus(statusCode int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.statusCode = statusCode
}

func (u *failoverUpstream) requestSnapshot() ([]string, [][]byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	paths := append([]string(nil), u.paths...)
	bodies := make([][]byte, len(u.bodies))
	for index := range u.bodies {
		bodies[index] = append([]byte(nil), u.bodies[index]...)
	}
	return paths, bodies
}

func setupRelayFailoverTest(t *testing.T, memoryCache bool) (*gin.Engine, *model.User) {
	t.Helper()
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Log{}, &model.UserSubscription{}))
	ratio_setting.InitRatioSettings()

	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalAutomaticDisable := common.AutomaticDisableChannelEnabled
	originalAutomaticEnable := common.AutomaticEnableChannelEnabled
	originalLogConsume := common.LogConsumeEnabled
	originalBatchUpdate := common.BatchUpdateEnabled
	originalRetryRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticRetryStatusCodeRanges...)
	originalDisableRanges := append([]operation_setting.StatusCodeRange(nil), operation_setting.AutomaticDisableStatusCodeRanges...)
	common.MemoryCacheEnabled = memoryCache
	common.AutomaticDisableChannelEnabled = true
	common.AutomaticEnableChannelEnabled = true
	common.LogConsumeEnabled = true
	common.BatchUpdateEnabled = false
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{
		{Start: http.StatusUnauthorized, End: http.StatusUnauthorized},
		{Start: http.StatusTooManyRequests, End: http.StatusTooManyRequests},
		{Start: http.StatusInternalServerError, End: http.StatusNetworkAuthenticationRequired},
	}
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{
		{Start: http.StatusTooManyRequests, End: http.StatusTooManyRequests},
	}
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.AutomaticDisableChannelEnabled = originalAutomaticDisable
		common.AutomaticEnableChannelEnabled = originalAutomaticEnable
		common.LogConsumeEnabled = originalLogConsume
		common.BatchUpdateEnabled = originalBatchUpdate
		operation_setting.AutomaticRetryStatusCodeRanges = originalRetryRanges
		operation_setting.AutomaticDisableStatusCodeRanges = originalDisableRanges
	})

	user := &model.User{
		Username: "ha-relay-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1_000_000,
	}
	require.NoError(t, model.DB.Create(user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:      user.Id,
		Key:         "harelaytoken",
		Name:        "ha-relay-token",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 1_000_000,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)
	return engine, user
}

func addRelayFailoverChannel(t *testing.T, id int, priority int64, upstream *failoverUpstream) model.Channel {
	t.Helper()
	weight := uint(100)
	autoBan := 1
	baseURL := upstream.server.URL
	channel := model.Channel{
		Id:       id,
		Type:     constant.ChannelTypeOpenAI,
		Key:      "test-key",
		Status:   common.ChannelStatusEnabled,
		Name:     upstream.name,
		Weight:   &weight,
		BaseURL:  &baseURL,
		Models:   relayFailoverModel,
		Group:    "default",
		Priority: &priority,
		AutoBan:  &autoBan,
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	return channel
}

func performRelayFailoverRequest(t *testing.T, engine *gin.Engine) *httptest.ResponseRecorder {
	t.Helper()
	body := bytes.NewBufferString(`{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":"ping"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	request.Header.Set("Authorization", "Bearer harelaytoken")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func initializeRelayFailoverChannels() {
	if common.MemoryCacheEnabled {
		model.InitChannelCache()
	}
}

func requireEventually(t *testing.T, assertion func() bool) {
	t.Helper()
	require.Eventually(t, assertion, 2*time.Second, 10*time.Millisecond)
}

type failoverAdminInfo struct {
	AttemptCount      int      `json:"attempt_count"`
	AttemptedChannels []string `json:"attempted_channels"`
	PriorityPath      []int64  `json:"priority_path"`
	FinalTier         int64    `json:"final_tier"`
	FallbackReason    string   `json:"fallback_reason"`
}

func latestConsumeLog(t *testing.T, userID int) (model.Log, failoverAdminInfo) {
	t.Helper()
	var log model.Log
	require.NoError(t, model.LOG_DB.Where("user_id = ? AND type = ?", userID, model.LogTypeConsume).
		Order("id DESC").First(&log).Error)
	var payload struct {
		AdminInfo failoverAdminInfo `json:"admin_info"`
	}
	require.NoError(t, common.Unmarshal([]byte(log.Other), &payload))
	return log, payload.AdminInfo
}

func requireRelayRefunded(t *testing.T, userID int) {
	t.Helper()
	requireEventually(t, func() bool {
		var user model.User
		var token model.Token
		if err := model.DB.First(&user, userID).Error; err != nil {
			return false
		}
		if err := model.DB.Where("user_id = ?", userID).First(&token).Error; err != nil {
			return false
		}
		return user.Quota == 1_000_000 && token.RemainQuota == 1_000_000
	})
}

func TestRelayChannelFailoverHealthyPrimary(t *testing.T) {
	engine, user := setupRelayFailoverTest(t, false)
	trace := &failoverCallTrace{}
	primary := newFailoverUpstream(t, "primary", http.StatusOK, trace)
	backup := newFailoverUpstream(t, "backup", http.StatusOK, trace)
	primaryChannel := addRelayFailoverChannel(t, 3101, 30, primary)
	addRelayFailoverChannel(t, 3102, 20, backup)
	initializeRelayFailoverChannels()

	response := performRelayFailoverRequest(t, engine)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "primary")
	assert.Equal(t, 1, primary.callCount())
	assert.Zero(t, backup.callCount())
	assert.Equal(t, []string{"primary"}, trace.snapshot())
	paths, bodies := primary.requestSnapshot()
	assert.Equal(t, []string{"/v1/chat/completions"}, paths)
	require.Len(t, bodies, 1)
	assert.Contains(t, string(bodies[0]), `"model":"gpt-3.5-turbo"`)
	log, adminInfo := latestConsumeLog(t, user.Id)
	assert.Equal(t, primaryChannel.Id, log.ChannelId)
	assert.Equal(t, 1, adminInfo.AttemptCount)
	assert.Equal(t, []string{"3101"}, adminInfo.AttemptedChannels)
	assert.Equal(t, []int64{30}, adminInfo.PriorityPath)
	assert.Equal(t, int64(30), adminInfo.FinalTier)
	assert.Empty(t, adminInfo.FallbackReason)
}

func TestRelayChannelFailoverFrom500(t *testing.T) {
	for _, memoryCache := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory_cache_%t", memoryCache), func(t *testing.T) {
			engine, user := setupRelayFailoverTest(t, memoryCache)
			trace := &failoverCallTrace{}
			primary := newFailoverUpstream(t, "primary", http.StatusInternalServerError, trace)
			backup := newFailoverUpstream(t, "backup", http.StatusOK, trace)
			addRelayFailoverChannel(t, 3201, 30, primary)
			backupChannel := addRelayFailoverChannel(t, 3202, 20, backup)
			initializeRelayFailoverChannels()

			response := performRelayFailoverRequest(t, engine)

			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "backup")
			assert.Equal(t, 1, primary.callCount())
			assert.Equal(t, 1, backup.callCount())
			assert.Equal(t, []string{"primary", "backup"}, trace.snapshot())
			log, adminInfo := latestConsumeLog(t, user.Id)
			assert.Equal(t, backupChannel.Id, log.ChannelId)
			assert.Equal(t, 2, adminInfo.AttemptCount)
			assert.Equal(t, []string{"3201", "3202"}, adminInfo.AttemptedChannels)
			assert.Equal(t, []int64{30, 20}, adminInfo.PriorityPath)
			assert.Equal(t, int64(20), adminInfo.FinalTier)
			assert.Equal(t, "status_500:ha_test", adminInfo.FallbackReason)
		})
	}
}

func TestRelayChannelFailoverFrom429DisablesPrimary(t *testing.T) {
	engine, _ := setupRelayFailoverTest(t, true)
	trace := &failoverCallTrace{}
	primary := newFailoverUpstream(t, "primary", http.StatusTooManyRequests, trace)
	backup := newFailoverUpstream(t, "backup", http.StatusOK, trace)
	primaryChannel := addRelayFailoverChannel(t, 3301, 30, primary)
	addRelayFailoverChannel(t, 3302, 20, backup)
	initializeRelayFailoverChannels()

	firstResponse := performRelayFailoverRequest(t, engine)
	require.Equal(t, http.StatusOK, firstResponse.Code, firstResponse.Body.String())
	requireEventually(t, func() bool {
		var stored model.Channel
		if err := model.DB.First(&stored, primaryChannel.Id).Error; err != nil {
			return false
		}
		var ability model.Ability
		if err := model.DB.Where("channel_id = ?", primaryChannel.Id).First(&ability).Error; err != nil {
			return false
		}
		return stored.Status == common.ChannelStatusAutoDisabled && !ability.Enabled
	})

	secondResponse := performRelayFailoverRequest(t, engine)

	require.Equal(t, http.StatusOK, secondResponse.Code, secondResponse.Body.String())
	assert.Equal(t, []string{"primary", "backup", "backup"}, trace.snapshot())
	assert.Equal(t, 1, primary.callCount())
	assert.Equal(t, 2, backup.callCount())
}

func TestRelayChannelFailoverFromConnectionFailure(t *testing.T) {
	engine, _ := setupRelayFailoverTest(t, false)
	trace := &failoverCallTrace{}
	primary := newDisconnectingFailoverUpstream(t, "disconnect", trace)
	backup := newFailoverUpstream(t, "backup", http.StatusOK, trace)
	addRelayFailoverChannel(t, 3401, 30, primary)
	addRelayFailoverChannel(t, 3402, 20, backup)
	initializeRelayFailoverChannels()

	response := performRelayFailoverRequest(t, engine)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "backup")
	assert.Equal(t, []string{"disconnect", "backup"}, trace.snapshot())
}

func TestRelayChannelFailoverDoesNotRetry400(t *testing.T) {
	engine, user := setupRelayFailoverTest(t, false)
	trace := &failoverCallTrace{}
	primary := newFailoverUpstream(t, "primary", http.StatusBadRequest, trace)
	backup := newFailoverUpstream(t, "backup", http.StatusOK, trace)
	addRelayFailoverChannel(t, 3501, 30, primary)
	addRelayFailoverChannel(t, 3502, 20, backup)
	initializeRelayFailoverChannels()

	response := performRelayFailoverRequest(t, engine)

	assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Equal(t, []string{"primary"}, trace.snapshot())
	assert.Zero(t, backup.callCount())
	requireRelayRefunded(t, user.Id)
}

func TestRelayChannelFailoverCapsAttemptsAtFivePriorities(t *testing.T) {
	engine, user := setupRelayFailoverTest(t, false)
	trace := &failoverCallTrace{}
	priorities := []int64{30, 20, 10, 0, -10, -20}
	upstreams := make([]*failoverUpstream, 0, len(priorities))
	for index, priority := range priorities {
		name := fmt.Sprintf("tier-%d", priority)
		upstream := newFailoverUpstream(t, name, http.StatusInternalServerError, trace)
		upstreams = append(upstreams, upstream)
		addRelayFailoverChannel(t, 3601+index, priority, upstream)
	}
	initializeRelayFailoverChannels()

	response := performRelayFailoverRequest(t, engine)

	assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	assert.Equal(t, []string{"tier-30", "tier-20", "tier-10", "tier-0", "tier--10"}, trace.snapshot())
	for index, upstream := range upstreams {
		if index < 5 {
			assert.Equal(t, 1, upstream.callCount())
		} else {
			assert.Zero(t, upstream.callCount())
		}
	}
	requireRelayRefunded(t, user.Id)
}

func TestRelayChannelFailoverBillsLikeHealthyRequest(t *testing.T) {
	engine, user := setupRelayFailoverTest(t, false)
	primary := newFailoverUpstream(t, "primary", http.StatusOK)
	backup := newFailoverUpstream(t, "backup", http.StatusOK)
	addRelayFailoverChannel(t, 3701, 30, primary)
	addRelayFailoverChannel(t, 3702, 20, backup)
	initializeRelayFailoverChannels()

	quotaBefore := user.Quota
	baselineResponse := performRelayFailoverRequest(t, engine)
	require.Equal(t, http.StatusOK, baselineResponse.Code, baselineResponse.Body.String())
	var afterBaseline model.User
	require.NoError(t, model.DB.First(&afterBaseline, user.Id).Error)
	baselineCost := quotaBefore - afterBaseline.Quota
	require.Positive(t, baselineCost)

	primary.setStatus(http.StatusInternalServerError)
	failoverResponse := performRelayFailoverRequest(t, engine)
	require.Equal(t, http.StatusOK, failoverResponse.Code, failoverResponse.Body.String())
	var afterFailover model.User
	require.NoError(t, model.DB.First(&afterFailover, user.Id).Error)
	failoverCost := afterBaseline.Quota - afterFailover.Quota

	assert.Equal(t, baselineCost, failoverCost)
	var consumeCount int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).
		Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).
		Count(&consumeCount).Error)
	assert.Equal(t, int64(2), consumeCount)
}
