package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupStreamRecoveryControllerTest(t *testing.T) {
	t.Helper()
	previousDB := model.DB
	database, err := gorm.Open(
		sqlite.Open(filepath.Join(t.TempDir(), "stream-recovery.db")),
		&gorm.Config{},
	)
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.StreamExecution{}))
	model.DB = database

	previousRedisEnabled := common.RedisEnabled
	previousRedis := common.RDB
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	common.RedisEnabled = true
	common.RDB = redisClient

	settings := operation_setting.GetStreamRecoverySetting()
	previousSettings := *settings
	settings.Enabled = true
	settings.IdentityMode = operation_setting.StreamRecoveryIdentityModeStable
	settings.AllowedModels = []string{"glm-5.3"}
	settings.HeartbeatSeconds = 1
	settings.LeaseSeconds = 3

	keyPath := filepath.Join(t.TempDir(), "stream-recovery.keys")
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	require.NoError(t, os.WriteFile(keyPath, []byte("current:"+key+"\n"), 0o600))
	t.Setenv("STREAM_RECOVERY_KEY_FILE", keyPath)
	service.ResetStreamRecoveryRuntimeForTest()

	t.Cleanup(func() {
		service.ResetStreamRecoveryRuntimeForTest()
		*settings = previousSettings
		common.RedisEnabled = previousRedisEnabled
		common.RDB = previousRedis
		model.DB = previousDB
		require.NoError(t, redisClient.Close())
	})
}

func newStreamRecoveryTestContext(queryID string) (*gin.Context, *httptest.ResponseRecorder) {
	body := `{"model":"glm-5.3","stream":true,"input":"probe"}`
	return newStreamRecoveryTestContextWithBody(queryID, body)
}

func newStreamRecoveryTestContextWithBody(
	queryID string,
	body string,
) (*gin.Context, *httptest.ResponseRecorder) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-query-id", queryID)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	common.SetContextKey(context, constant.ContextKeyTokenStreamRecovery, true)
	common.SetContextKey(context, constant.ContextKeyTokenId, 22)
	common.SetContextKey(context, constant.ContextKeyUserId, 11)
	common.SetContextKey(context, constant.ContextKeyOriginalModel, "glm-5.3")
	common.SetContextKey(context, constant.ContextKeyChannelId, 31)
	context.Set(common.RequestIdKey, "request-test")
	return context, recorder
}

type failRedisCommandOnceHook struct {
	command string
	err     error
	fired   atomic.Bool
}

func (hook *failRedisCommandOnceHook) BeforeProcess(
	ctx context.Context,
	cmd redis.Cmder,
) (context.Context, error) {
	if cmd.Name() == hook.command && hook.fired.CompareAndSwap(false, true) {
		return ctx, hook.err
	}
	return ctx, nil
}

func (*failRedisCommandOnceHook) AfterProcess(context.Context, redis.Cmder) error {
	return nil
}

func (*failRedisCommandOnceHook) BeforeProcessPipeline(
	ctx context.Context,
	_ []redis.Cmder,
) (context.Context, error) {
	return ctx, nil
}

func (*failRedisCommandOnceHook) AfterProcessPipeline(context.Context, []redis.Cmder) error {
	return nil
}

func TestCloneStreamRecoveryWorkerContextDetachesRequestState(t *testing.T) {
	parent, _ := newStreamRecoveryTestContext("query-context-copy")
	parent.Set("mutable", "before")
	parent.Request.Header.Set("X-Test-Value", "before")
	workerContext, cancel := context.WithCancel(context.Background())
	worker := cloneStreamRecoveryWorkerContext(parent, workerContext)

	parent.Set("mutable", "after")
	parent.Request.Header.Set("X-Test-Value", "after")
	assert.Equal(t, "before", worker.GetString("mutable"))
	assert.Equal(t, "before", worker.Request.Header.Get("X-Test-Value"))

	cancel()
	assert.ErrorIs(t, worker.Request.Context().Err(), context.Canceled)
}

func TestHandleStreamRecoveryRelayReplaysCompletedExecution(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	var workerRuns atomic.Int32
	run := func(context *gin.Context, _ relaytypes.RelayFormat) {
		workerRuns.Add(1)
		_, _ = context.Writer.Write([]byte(
			"event: response.completed\n" +
				"data: {\"type\":\"response.completed\"}\n\n",
		))
		context.Writer.Flush()
	}

	firstContext, firstRecorder := newStreamRecoveryTestContext("query-stable")
	require.True(t, handleStreamRecoveryRelay(
		firstContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(firstContext)
	assert.Equal(t, int32(1), workerRuns.Load())
	assert.Contains(t, firstRecorder.Body.String(), "id: 1")
	assert.Contains(t, firstRecorder.Body.String(), "response.completed")
	streamID := firstRecorder.Header().Get(common.StreamRecoveryIDHeader)
	require.NotEmpty(t, streamID)

	require.Eventually(t, func() bool {
		execution, err := model.GetStreamExecution(streamID)
		return err == nil && execution.Status == model.StreamExecutionCompleted
	}, time.Second, 10*time.Millisecond)

	secondContext, secondRecorder := newStreamRecoveryTestContext("query-stable")
	require.True(t, handleStreamRecoveryRelay(
		secondContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(secondContext)
	assert.Equal(t, int32(1), workerRuns.Load())
	assert.Equal(t, streamID, secondRecorder.Header().Get(common.StreamRecoveryIDHeader))
	assert.Equal(t, "true", secondRecorder.Header().Get(common.StreamRecoveryReplayedHeader))
	assert.Equal(t, firstRecorder.Body.String(), secondRecorder.Body.String())

	resumedContext, resumedRecorder := newStreamRecoveryTestContext("query-stable")
	resumedContext.Request.Header.Set("Last-Event-ID", "1:1")
	require.True(t, handleStreamRecoveryRelay(
		resumedContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(resumedContext)
	assert.Equal(t, int32(1), workerRuns.Load())
	assert.Empty(t, resumedRecorder.Body.String())
}

func TestHandleStreamRecoveryRelayRejectsReusedIdentityWithDifferentPayload(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	var workerRuns atomic.Int32
	run := func(context *gin.Context, _ relaytypes.RelayFormat) {
		workerRuns.Add(1)
		_, _ = context.Writer.Write([]byte(
			"event: response.completed\n" +
				"data: {\"type\":\"response.completed\"}\n\n",
		))
		context.Writer.Flush()
	}

	firstContext, firstRecorder := newStreamRecoveryTestContext("query-conflict")
	require.True(t, handleStreamRecoveryRelay(
		firstContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(firstContext)
	streamID := firstRecorder.Header().Get(common.StreamRecoveryIDHeader)
	require.NotEmpty(t, streamID)
	require.Eventually(t, func() bool {
		execution, loadErr := model.GetStreamExecution(streamID)
		return loadErr == nil && execution.Status == model.StreamExecutionCompleted
	}, time.Second, 10*time.Millisecond)

	secondContext, secondRecorder := newStreamRecoveryTestContextWithBody(
		"query-conflict",
		`{"model":"glm-5.3","stream":true,"input":"different"}`,
	)
	require.True(t, handleStreamRecoveryRelay(
		secondContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(secondContext)

	assert.Equal(t, int32(1), workerRuns.Load())
	assert.Equal(t, http.StatusConflict, secondRecorder.Code)
	assert.Contains(t, secondRecorder.Body.String(), "stream_recovery_identity_conflict")
}

func TestHandleStreamRecoveryRelayLegacyModeUsesDigestBoundIdentity(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	operation_setting.GetStreamRecoverySetting().IdentityMode =
		operation_setting.StreamRecoveryIdentityModeLegacy
	var workerRuns atomic.Int32
	run := func(context *gin.Context, _ relaytypes.RelayFormat) {
		workerRuns.Add(1)
		_, _ = context.Writer.Write([]byte(
			"event: response.completed\n" +
				"data: {\"type\":\"response.completed\"}\n\n",
		))
		context.Writer.Flush()
	}

	firstContext, firstRecorder := newStreamRecoveryTestContextWithBody(
		"query-legacy-rollout",
		`{"model":"glm-5.3","stream":true,"input":"first"}`,
	)
	require.True(t, handleStreamRecoveryRelay(
		firstContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(firstContext)
	firstStreamID := firstRecorder.Header().Get(common.StreamRecoveryIDHeader)
	require.NotEmpty(t, firstStreamID)
	require.Eventually(t, func() bool {
		execution, loadErr := model.GetStreamExecution(firstStreamID)
		return loadErr == nil && execution.Status == model.StreamExecutionCompleted
	}, time.Second, 10*time.Millisecond)

	secondContext, secondRecorder := newStreamRecoveryTestContextWithBody(
		"query-legacy-rollout",
		`{"model":"glm-5.3","stream":true,"input":"second"}`,
	)
	require.True(t, handleStreamRecoveryRelay(
		secondContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(secondContext)
	secondStreamID := secondRecorder.Header().Get(common.StreamRecoveryIDHeader)
	require.NotEmpty(t, secondStreamID)
	assert.NotEqual(t, firstStreamID, secondStreamID)
	require.Eventually(t, func() bool {
		return workerRuns.Load() == 2
	}, time.Second, 10*time.Millisecond)
}

func TestHandleStreamRecoveryRelayCarriesUnsafeReasonToWorker(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	reason := make(chan string, 1)
	run := func(context *gin.Context, _ relaytypes.RelayFormat) {
		reason <- common.GetContextKeyString(
			context,
			constant.ContextKeyStreamRecoveryReplayUnsafe,
		)
		_, _ = context.Writer.Write([]byte(
			"event: response.completed\n" +
				"data: {\"type\":\"response.completed\"}\n\n",
		))
		context.Writer.Flush()
	}
	requestContext, recorder := newStreamRecoveryTestContextWithBody(
		"query-stateful-context",
		`{"model":"glm-5.3","stream":true,"previous_response_id":"resp_1","input":"continue"}`,
	)

	require.True(t, handleStreamRecoveryRelay(
		requestContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(requestContext)
	assert.Equal(t, service.StreamReplayUnsafeProviderState, <-reason)
	streamID := recorder.Header().Get(common.StreamRecoveryIDHeader)
	require.Eventually(t, func() bool {
		execution, loadErr := model.GetStreamExecution(streamID)
		return loadErr == nil && execution.Status == model.StreamExecutionCompleted
	}, time.Second, 10*time.Millisecond)
}

func TestStreamRecoveryErrorTerminalFailsExecution(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	run := func(context *gin.Context, _ relaytypes.RelayFormat) {
		_, _ = context.Writer.Write([]byte(
			"event: error\n" +
				"data: {\"type\":\"error\",\"code\":\"STATEFUL_REPLAY_UNSAFE\"}\n\n",
		))
		context.Writer.Flush()
	}
	requestContext, recorder := newStreamRecoveryTestContext("query-error-terminal")

	require.True(t, handleStreamRecoveryRelay(
		requestContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(requestContext)
	assert.Contains(t, recorder.Body.String(), "event: error")
	streamID := recorder.Header().Get(common.StreamRecoveryIDHeader)
	require.Eventually(t, func() bool {
		execution, loadErr := model.GetStreamExecution(streamID)
		return loadErr == nil && execution.Status == model.StreamExecutionFailed
	}, time.Second, 10*time.Millisecond)
}

func TestHandleStreamRecoveryRelayRequiresStableIdentity(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	context, recorder := newStreamRecoveryTestContext("")

	handled := handleStreamRecoveryRelay(
		context,
		relaytypes.RelayFormatOpenAIResponses,
		func(*gin.Context, relaytypes.RelayFormat) {
			t.Fatal("worker must not start without a stable identity")
		},
	)
	common.CleanupBodyStorage(context)

	require.True(t, handled)
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "stream_recovery_identity")
}

func TestStreamRecoveryDrainingRejectsAdmissionAndKeepsReconcilerEnabled(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	settings := operation_setting.GetStreamRecoverySetting()
	settings.Enabled = false
	settings.IdentityMode = operation_setting.StreamRecoveryIdentityModeDraining
	context, recorder := newStreamRecoveryTestContext("query-draining")
	var workerRuns atomic.Int32

	handled := handleStreamRecoveryRelay(
		context,
		relaytypes.RelayFormatOpenAIResponses,
		func(*gin.Context, relaytypes.RelayFormat) {
			workerRuns.Add(1)
		},
	)
	common.CleanupBodyStorage(context)

	require.True(t, handled)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "stream_recovery_draining")
	assert.Zero(t, workerRuns.Load())
	assert.True(t, (streamRecoveryReconcileHandler{}).Enabled())
	_, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	var count int64
	require.NoError(t, model.DB.Model(&model.StreamExecution{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestShouldRetryStopsRecoveryWorkerAfterFirstPersistedFrame(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	context, _ := newStreamRecoveryTestContext("query-retry")
	writer := service.NewStreamRecoveryWriter(
		context.Request.Context(),
		runtime.Store,
		"stream-retry",
		1,
		nil,
	)
	context.Writer = writer
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryWorker, true)
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryBroker, writer)
	_, err = writer.WriteString("event: response.output_text.delta\ndata: {\"delta\":\"a\"}\n\n")
	require.NoError(t, err)
	writer.Flush()

	upstreamError := relaytypes.NewOpenAIError(
		errors.New("upstream disconnected"),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusInternalServerError,
	)
	assert.False(t, shouldRetry(context, upstreamError, 1))

	_, err = writer.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	require.NoError(t, err)
	writer.Flush()
	assert.False(t, shouldRetry(context, upstreamError, 1))
}

func TestShouldRetryStopsRecoveryWorkerAfterBufferedOutput(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	context, _ := newStreamRecoveryTestContext("query-buffered-output")
	writer := service.NewStreamRecoveryWriter(
		context.Request.Context(),
		runtime.Store,
		"stream-buffered-output",
		1,
		nil,
	)
	context.Writer = writer
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryWorker, true)
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryBroker, writer)
	_, err = writer.WriteString(
		"event: response.output_text.delta\n" +
			"data: {\"delta\":\"buffered\"}\n\n",
	)
	require.NoError(t, err)

	upstreamError := relaytypes.NewOpenAIError(
		errors.New("upstream disconnected"),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusInternalServerError,
	)
	assert.False(t, shouldRetry(context, upstreamError, 1))
}

func TestShouldRetryAllowsRecoveryWorkerBeforeFirstPersistedFrame(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	context, _ := newStreamRecoveryTestContext("query-retry-before-output")
	writer := service.NewStreamRecoveryWriter(
		context.Request.Context(),
		runtime.Store,
		"stream-retry-before-output",
		1,
		nil,
	)
	context.Writer = writer
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryWorker, true)
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryBroker, writer)
	writer.WriteHeader(http.StatusOK)

	upstreamError := relaytypes.NewOpenAIError(
		errors.New("upstream disconnected"),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusInternalServerError,
	)
	assert.True(t, shouldRetry(context, upstreamError, 1))
}

func TestStreamRecoveryRetryBudgetUsesConfiguredAttemptLimit(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	context, _ := newStreamRecoveryTestContext("query-attempt-budget")
	writer := service.NewStreamRecoveryWriter(
		context.Request.Context(),
		runtime.Store,
		"stream-attempt-budget",
		1,
		nil,
	)
	context.Writer = writer
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryBroker, writer)

	assert.Equal(t, 1, capStreamRecoveryRetries(context, 4))
	require.NoError(t, writer.RotateAttempt(2))
	assert.Zero(t, capStreamRecoveryRetries(context, 4))

	ordinary, _ := newStreamRecoveryTestContext("query-ordinary-budget")
	assert.Equal(t, 4, capStreamRecoveryRetries(ordinary, 4))
}

func TestRetryDecisionReturnsStatefulReplayUnsafe(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	context, _ := newStreamRecoveryTestContext("query-stateful-retry")
	writer := service.NewStreamRecoveryWriter(
		context.Request.Context(),
		runtime.Store,
		"stream-stateful-retry",
		1,
		nil,
	)
	context.Writer = writer
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryWorker, true)
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryBroker, writer)
	common.SetContextKey(
		context,
		constant.ContextKeyStreamRecoveryReplayUnsafe,
		service.StreamReplayUnsafeProviderState,
	)

	upstreamError := relaytypes.NewOpenAIError(
		errors.New("upstream disconnected"),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusInternalServerError,
	)
	retry, replacement := retryDecision(context, upstreamError, 1)

	assert.False(t, retry)
	require.NotNil(t, replacement)
	assert.Equal(t, relaytypes.ErrorCodeStatefulReplayUnsafe, replacement.GetErrorCode())
	assert.Equal(t, http.StatusConflict, replacement.StatusCode)
	assert.True(t, relaytypes.IsSkipRetryError(replacement))
	assert.Contains(t, replacement.Error(), service.StreamReplayUnsafeProviderState)

	retry, replacement = retryDecision(context, upstreamError, 0)
	assert.False(t, retry)
	assert.Nil(t, replacement)
}

func TestStreamRecoveryAttemptSafetyIncludesChannelMutations(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	tests := []struct {
		name       string
		info       *relaycommon.RelayInfo
		reasonCode string
	}{
		{
			name: "disabled store removal",
			info: &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelOtherSettings: relaydto.ChannelOtherSettings{
						DisableStore: true,
					},
				},
			},
			reasonCode: service.StreamReplayUnsafeProviderWrite,
		},
		{
			name: "hosted tool override",
			info: &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{
					ParamOverride: map[string]any{
						"tools": []any{
							map[string]any{"type": "web_search_preview"},
						},
					},
				},
			},
			reasonCode: service.StreamReplayUnsafeHostedTool,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requestContext, _ := newStreamRecoveryTestContext("query-channel-mutation")
			require.NoError(t, updateStreamRecoveryReplaySafetyForAttempt(
				requestContext,
				relaytypes.RelayFormatOpenAIResponses,
				test.info,
				[]byte(
					`{"model":"glm-5.3","stream":true,"store":false,"input":"hello"}`,
				),
			))
			assert.Equal(
				t,
				test.reasonCode,
				common.GetContextKeyString(
					requestContext,
					constant.ContextKeyStreamRecoveryReplayUnsafe,
				),
			)
			common.CleanupBodyStorage(requestContext)
		})
	}

	t.Run("protocol conversion is conservatively unsafe", func(t *testing.T) {
		requestContext, _ := newStreamRecoveryTestContext("query-converted-request")
		markStreamRecoveryConversionUnsafe(
			requestContext,
			relaytypes.RelayFormatOpenAIResponses,
			relaytypes.RelayFormatClaude,
		)
		assert.Equal(
			t,
			service.StreamReplayUnsafeConversion,
			common.GetContextKeyString(
				requestContext,
				constant.ContextKeyStreamRecoveryReplayUnsafe,
			),
		)
		common.CleanupBodyStorage(requestContext)
	})
}

func TestWriteStreamRecoveryRelayErrorUsesTerminalSSE(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	apiError := relaytypes.NewOpenAIError(
		errors.New("STATEFUL_REPLAY_UNSAFE: automatic replay rejected"),
		relaytypes.ErrorCodeStatefulReplayUnsafe,
		http.StatusConflict,
		relaytypes.ErrOptionWithSkipRetry(),
	)
	tests := []struct {
		name        string
		format      relaytypes.RelayFormat
		nestedError bool
	}{
		{name: "responses", format: relaytypes.RelayFormatOpenAIResponses},
		{name: "claude", format: relaytypes.RelayFormatClaude, nestedError: true},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			streamID := "stream-error-frame-" + strconv.Itoa(index)
			writer := service.NewStreamRecoveryWriter(
				context.Background(),
				runtime.Store,
				streamID,
				1,
				nil,
			)
			requestContext, _ := newStreamRecoveryTestContext("query-error-frame")
			requestContext.Writer = writer
			common.SetContextKey(
				requestContext,
				constant.ContextKeyStreamRecoveryBroker,
				writer,
			)

			require.NoError(t, writeStreamRecoveryRelayError(
				requestContext,
				test.format,
				apiError,
			))
			frames, readErr := runtime.Store.ReadFrames(
				context.Background(),
				streamID,
				1,
				0,
				10,
			)
			require.NoError(t, readErr)
			require.Len(t, frames, 1)
			assert.True(t, frames[0].Terminal)
			frameText := string(frames[0].Data)
			assert.Contains(t, frameText, "event: error")
			dataLine := ""
			for _, line := range strings.Split(frameText, "\n") {
				if strings.HasPrefix(line, "data: ") {
					dataLine = strings.TrimPrefix(line, "data: ")
					break
				}
			}
			require.NotEmpty(t, dataLine)
			var payload map[string]any
			require.NoError(t, common.Unmarshal([]byte(dataLine), &payload))
			assert.Equal(t, "error", payload["type"])
			if test.nestedError {
				nested, ok := payload["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "STATEFUL_REPLAY_UNSAFE", nested["type"])
			} else {
				assert.Equal(t, "STATEFUL_REPLAY_UNSAFE", payload["code"])
				assert.NotContains(t, payload, "error")
			}
		})
	}
}

func TestStreamRecoveryClientDisconnectDoesNotCancelWorker(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	firstFrame := make(chan struct{})
	continueWorker := make(chan struct{})
	var workerRuns atomic.Int32
	run := func(context *gin.Context, _ relaytypes.RelayFormat) {
		workerRuns.Add(1)
		_, _ = context.Writer.Write([]byte(
			"event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n",
		))
		context.Writer.Flush()
		close(firstFrame)
		<-continueWorker
		_, _ = context.Writer.Write([]byte(
			"event: response.completed\n" +
				"data: {\"type\":\"response.completed\"}\n\n",
		))
		context.Writer.Flush()
	}

	firstContext, firstRecorder := newStreamRecoveryTestContext("query-disconnect")
	requestContext, cancel := context.WithCancel(firstContext.Request.Context())
	firstContext.Request = firstContext.Request.WithContext(requestContext)
	handlerDone := make(chan struct{})
	go func() {
		handleStreamRecoveryRelay(
			firstContext,
			relaytypes.RelayFormatOpenAIResponses,
			run,
		)
		close(handlerDone)
	}()
	<-firstFrame
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("disconnected subscriber did not return")
	}
	close(continueWorker)
	streamID := firstRecorder.Header().Get(common.StreamRecoveryIDHeader)
	require.NotEmpty(t, streamID)
	require.Eventually(t, func() bool {
		execution, err := model.GetStreamExecution(streamID)
		return err == nil && execution.Status == model.StreamExecutionCompleted
	}, time.Second, 10*time.Millisecond)

	secondContext, secondRecorder := newStreamRecoveryTestContext("query-disconnect")
	require.True(t, handleStreamRecoveryRelay(
		secondContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(firstContext)
	common.CleanupBodyStorage(secondContext)

	assert.Equal(t, int32(1), workerRuns.Load())
	assert.Contains(t, secondRecorder.Body.String(), "\"delta\":\"first\"")
	assert.Contains(t, secondRecorder.Body.String(), "response.completed")
}

func TestStreamRecoveryRejectsExpiredStartedExecutionReplay(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	context, recorder := newStreamRecoveryTestContext("query-takeover")
	common.SetContextKey(context, constant.ContextKeyChannelId, 67)
	bodyStorage, err := common.GetBodyStorage(context)
	require.NoError(t, err)
	body, err := bodyStorage.Bytes()
	require.NoError(t, err)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	identity, err := service.BuildStreamRecoveryIdentity(
		runtime.Keyring,
		22,
		"/v1/responses",
		"glm-5.3",
		"",
		"query-takeover",
		"",
		body,
	)
	require.NoError(t, err)
	execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:        "stream_takeover",
		DedupeKey:       identity.DedupeKey,
		IdentityVersion: model.StreamRecoveryIdentityVersionStable,
		UserID:          11,
		TokenID:         22,
		ModelName:       "glm-5.3",
		RelayFormat:     string(relaytypes.RelayFormatOpenAIResponses),
		RequestPath:     "/v1/responses",
		RequestDigest:   identity.RequestDigest,
		Status:          model.StreamExecutionRunning,
		PublicAttempt:   1,
		AttemptCount:    1,
		ActiveChannelID: 31,
		LockedBy:        "dead-runner",
		LockedUntil:     common.GetTimestamp() - 1,
		ExpiresAt:       common.GetTimestamp() + 3600,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, runtime.Store.SetPublicAttempt(
		context.Request.Context(),
		execution.StreamID,
		1,
	))

	var workerRuns atomic.Int32
	run := func(*gin.Context, relaytypes.RelayFormat) {
		workerRuns.Add(1)
	}
	require.True(t, handleStreamRecoveryRelay(
		context,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(context)

	assert.Equal(t, int32(0), workerRuns.Load())
	assert.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "STATEFUL_REPLAY_UNSAFE")
	updated, err := model.GetStreamExecution(execution.StreamID)
	require.NoError(t, err)
	assert.Equal(t, 1, updated.AttemptCount)
	assert.Equal(t, 31, updated.ActiveChannelID)
	assert.Equal(t, model.StreamExecutionFailed, updated.Status)
}

func TestStreamRecoveryReplaysCommittedFramesWithoutRestartingExpiredExecution(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	requestContext, recorder := newStreamRecoveryTestContext("query-expired-frames")
	bodyStorage, err := common.GetBodyStorage(requestContext)
	require.NoError(t, err)
	body, err := bodyStorage.Bytes()
	require.NoError(t, err)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	identity, err := service.BuildStreamRecoveryIdentity(
		runtime.Keyring,
		22,
		"/v1/responses",
		"glm-5.3",
		"",
		"query-expired-frames",
		"",
		body,
	)
	require.NoError(t, err)
	execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:          "stream_expired_frames",
		DedupeKey:         identity.DedupeKey,
		IdentityVersion:   model.StreamRecoveryIdentityVersionStable,
		UserID:            11,
		TokenID:           22,
		ModelName:         "glm-5.3",
		RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
		RequestPath:       "/v1/responses",
		RequestDigest:     identity.RequestDigest,
		Status:            model.StreamExecutionRunning,
		PublicAttempt:     1,
		AttemptCount:      1,
		ActiveChannelID:   31,
		CommittedSequence: 1,
		LockedBy:          "dead-runner",
		LockedUntil:       common.GetTimestamp() - 1,
		ExpiresAt:         common.GetTimestamp() + 3600,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, runtime.Store.SetPublicAttempt(
		context.Background(),
		execution.StreamID,
		1,
	))
	require.NoError(t, runtime.Store.AppendFrame(
		context.Background(),
		execution.StreamID,
		1,
		service.StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data: []byte(
				"id: 1:1\n" +
					"event: response.output_text.delta\n" +
					"data: {\"delta\":\"persisted\"}\n\n",
			),
		},
	))

	var workerRuns atomic.Int32
	require.True(t, handleStreamRecoveryRelay(
		requestContext,
		relaytypes.RelayFormatOpenAIResponses,
		func(*gin.Context, relaytypes.RelayFormat) {
			workerRuns.Add(1)
		},
	))
	common.CleanupBodyStorage(requestContext)

	assert.Equal(t, int32(0), workerRuns.Load())
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, 1, strings.Count(recorder.Body.String(), "\"delta\":\"persisted\""))
	assert.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
	assert.Contains(t, recorder.Body.String(), "STATEFUL_REPLAY_UNSAFE")
	updated, err := model.GetStreamExecution(execution.StreamID)
	require.NoError(t, err)
	assert.Equal(t, model.StreamExecutionFailed, updated.Status)
	assert.Contains(t, updated.Error, "STATEFUL_REPLAY_UNSAFE")
	assert.Equal(t, int64(2), updated.CommittedSequence)
	assert.Equal(t, int64(2), updated.TerminalSequence)

	statusRequest := httptest.NewRequest(
		http.MethodGet,
		"/v1/stream-sessions/"+execution.StreamID,
		nil,
	)
	statusRecorder := httptest.NewRecorder()
	statusContext, _ := gin.CreateTestContext(statusRecorder)
	statusContext.Request = statusRequest
	statusContext.Params = gin.Params{{Key: "stream_id", Value: execution.StreamID}}
	common.SetContextKey(statusContext, constant.ContextKeyUserId, execution.UserID)
	common.SetContextKey(statusContext, constant.ContextKeyTokenId, execution.TokenID)
	GetStreamRecoverySession(statusContext)
	assert.Contains(t, statusRecorder.Body.String(), `"error_code":"STATEFUL_REPLAY_UNSAFE"`)

	resumeContext, resumeRecorder := newStreamRecoveryTestContext("query-expired-frames")
	resumeContext.Request.Header.Set("Last-Event-ID", "1:1")
	require.True(t, handleStreamRecoveryRelay(
		resumeContext,
		relaytypes.RelayFormatOpenAIResponses,
		func(*gin.Context, relaytypes.RelayFormat) {
			workerRuns.Add(1)
		},
	))
	common.CleanupBodyStorage(resumeContext)
	assert.NotContains(t, resumeRecorder.Body.String(), "\"delta\":\"persisted\"")
	assert.Equal(t, 1, strings.Count(resumeRecorder.Body.String(), "event: error"))

	doneContext, doneRecorder := newStreamRecoveryTestContext("query-expired-frames")
	doneContext.Request.Header.Set("Last-Event-ID", "1:2")
	require.True(t, handleStreamRecoveryRelay(
		doneContext,
		relaytypes.RelayFormatOpenAIResponses,
		func(*gin.Context, relaytypes.RelayFormat) {
			workerRuns.Add(1)
		},
	))
	common.CleanupBodyStorage(doneContext)
	assert.Empty(t, doneRecorder.Body.String())
	assert.Equal(t, int32(0), workerRuns.Load())
}

func TestStreamRecoveryResumeSequenceUsesStandardLastEventID(t *testing.T) {
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodGet, "/v1/stream-sessions/stream_test", nil)

	context.Request.Header.Set("Last-Event-ID", "7")
	assert.Equal(t, int64(7), streamRecoveryResumeSequence(context, 1))
	assert.Zero(t, streamRecoveryResumeSequence(context, 2))

	context.Request.Header.Set(common.StreamRecoveryAttemptHeader, "2")
	assert.Equal(t, int64(7), streamRecoveryResumeSequence(context, 2))
	context.Request.Header.Del(common.StreamRecoveryAttemptHeader)

	context.Request.Header.Set("Last-Event-ID", "2:9")
	assert.Equal(t, int64(9), streamRecoveryResumeSequence(context, 2))

	context.Request.Header.Set("Last-Event-ID", "1:9")
	assert.Zero(t, streamRecoveryResumeSequence(context, 2))

	context.Request.Header.Set("Last-Event-ID", "invalid")
	assert.Zero(t, streamRecoveryResumeSequence(context, 2))
}

func TestSubscribeStreamRecoveryDoesNotEmitUncommittedFrame(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:          "stream_uncommitted",
		DedupeKey:         "dedupe_uncommitted",
		UserID:            11,
		TokenID:           22,
		ModelName:         "glm-5.3",
		RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
		RequestPath:       "/v1/responses",
		RequestDigest:     "digest_uncommitted",
		Status:            model.StreamExecutionFailed,
		PublicAttempt:     1,
		AttemptCount:      1,
		CommittedSequence: 0,
		ExpiresAt:         common.GetTimestamp() + 3600,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, runtime.Store.SetPublicAttempt(
		context.Background(),
		execution.StreamID,
		1,
	))
	require.NoError(t, runtime.Store.AppendFrame(
		context.Background(),
		execution.StreamID,
		1,
		service.StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data: []byte(
				"id: 1:1\n" +
					"event: response.completed\n" +
					"data: {\"type\":\"response.completed\"}\n\n",
			),
			Terminal: true,
		},
	))

	requestContext, recorder := newStreamRecoveryTestContext("query-uncommitted")
	subscribeStreamRecovery(requestContext, runtime.Store, execution)
	common.CleanupBodyStorage(requestContext)

	assert.Equal(t, http.StatusBadGateway, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "response.completed")
	assert.Equal(
		t,
		1,
		strings.Count(recorder.Body.String(), "stream_recovery_protocol_error"),
	)
}

func TestSubscribeStreamRecoveryUsesDatabaseAttemptOverStaleRedisPointer(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:          "stream_attempt_fence",
		DedupeKey:         "dedupe_attempt_fence",
		UserID:            11,
		TokenID:           22,
		ModelName:         "glm-5.3",
		RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
		RequestPath:       "/v1/responses",
		RequestDigest:     "digest_attempt_fence",
		Status:            model.StreamExecutionCompleted,
		PublicAttempt:     2,
		AttemptCount:      2,
		CommittedSequence: 2,
		TerminalSequence:  2,
		ExpiresAt:         common.GetTimestamp() + 3600,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, runtime.Store.SetPublicAttempt(
		context.Background(),
		execution.StreamID,
		1,
	))
	require.NoError(t, runtime.Store.AppendFrame(
		context.Background(),
		execution.StreamID,
		1,
		service.StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data:     []byte("id: 1:1\ndata: stale\n\n"),
		},
	))
	require.NoError(t, runtime.Store.AppendFrame(
		context.Background(),
		execution.StreamID,
		2,
		service.StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data:     []byte("id: 2:1\ndata: already-delivered\n\n"),
		},
	))
	require.NoError(t, runtime.Store.AppendFrame(
		context.Background(),
		execution.StreamID,
		2,
		service.StreamRecoveryFrame{
			Sequence: 2,
			Kind:     "data",
			Data: []byte(
				"id: 2:2\n" +
					"event: response.completed\n" +
					"data: {\"type\":\"response.completed\"}\n\n",
			),
			Terminal: true,
		},
	))

	requestContext, recorder := newStreamRecoveryTestContext("query-attempt-fence")
	requestContext.Request.Header.Set("Last-Event-ID", "2:1")
	subscribeStreamRecovery(requestContext, runtime.Store, execution)
	common.CleanupBodyStorage(requestContext)

	assert.NotContains(t, recorder.Body.String(), "stale")
	assert.NotContains(t, recorder.Body.String(), "already-delivered")
	assert.Contains(t, recorder.Body.String(), "id: 2:2")
	assert.Equal(t, "2", recorder.Header().Get(common.StreamRecoveryAttemptHeader))
}

func TestSubscribeStreamRecoveryFollowsPreOutputAttemptChange(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:          "stream_pre_output_retry",
		DedupeKey:         "dedupe_pre_output_retry",
		UserID:            11,
		TokenID:           22,
		ModelName:         "glm-5.3",
		RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
		RequestPath:       "/v1/responses",
		RequestDigest:     "digest_pre_output_retry",
		Status:            model.StreamExecutionCompleted,
		PublicAttempt:     2,
		AttemptCount:      2,
		CommittedSequence: 1,
		TerminalSequence:  1,
		ExpiresAt:         common.GetTimestamp() + 3600,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, runtime.Store.SetPublicAttempt(
		context.Background(),
		execution.StreamID,
		1,
	))
	require.NoError(t, runtime.Store.AppendFrame(
		context.Background(),
		execution.StreamID,
		1,
		service.StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data:     []byte("id: 1:1\n: keepalive\n\n"),
		},
	))
	require.NoError(t, runtime.Store.AppendFrame(
		context.Background(),
		execution.StreamID,
		2,
		service.StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data: []byte(
				"id: 2:1\n" +
					"event: response.completed\n" +
					"data: {\"type\":\"response.completed\"}\n\n",
			),
			Terminal: true,
		},
	))

	requestContext, recorder := newStreamRecoveryTestContext("query-pre-output-retry")
	requestContext.Request.Header.Set("Last-Event-ID", "1:1")
	subscribeStreamRecovery(requestContext, runtime.Store, execution)
	common.CleanupBodyStorage(requestContext)

	assert.NotContains(t, recorder.Body.String(), "keepalive")
	assert.Contains(t, recorder.Body.String(), "id: 2:1")
	assert.Equal(t, "2", recorder.Header().Get(common.StreamRecoveryAttemptHeader))
}

func TestStreamRecoveryStatusDoesNotExposeInternalState(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	gin.SetMode(gin.TestMode)
	execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:          "stream_status",
		DedupeKey:         "dedupe_status",
		UserID:            11,
		TokenID:           22,
		ModelName:         "glm-5.3",
		RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
		RequestPath:       "/v1/responses",
		RequestDigest:     "digest_status",
		Status:            model.StreamExecutionRunning,
		PublicAttempt:     1,
		LockedBy:          "internal-runner",
		BillingStatus:     model.StreamBillingReserved,
		ReservedQuota:     42,
		ConsumeLogStatus:  model.StreamConsumeLogRecording,
		CommittedSequence: 3,
		ExpiresAt:         common.GetTimestamp() + 3600,
	})
	require.NoError(t, err)
	require.True(t, created)

	request := httptest.NewRequest(http.MethodGet, "/v1/stream-sessions/"+execution.StreamID, nil)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	context.Params = gin.Params{{Key: "stream_id", Value: execution.StreamID}}
	common.SetContextKey(context, constant.ContextKeyUserId, execution.UserID)
	common.SetContextKey(context, constant.ContextKeyTokenId, execution.TokenID)

	GetStreamRecoverySession(context)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), execution.StreamID)
	assert.Contains(t, recorder.Body.String(), `"last_event_id":3`)
	assert.NotContains(t, recorder.Body.String(), "internal-runner")
	assert.NotContains(t, recorder.Body.String(), "billing_status")
	assert.NotContains(t, recorder.Body.String(), "dedupe")
}

func TestStreamRecoveryReconcilerWithoutSubscriber(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	now := common.GetTimestamp()

	withFrame, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:          "stream_reconcile_with_frame",
		DedupeKey:         "dedupe_reconcile_with_frame",
		UserID:            11,
		TokenID:           22,
		ModelName:         "glm-5.3",
		RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
		RequestPath:       "/v1/responses",
		RequestDigest:     "digest_reconcile_with_frame",
		Status:            model.StreamExecutionRunning,
		PublicAttempt:     1,
		AttemptCount:      1,
		CommittedSequence: 1,
		LockedBy:          "dead-runner-with-frame",
		LockedUntil:       now - 1,
		ExpiresAt:         now + 3600,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, runtime.Store.SetPublicAttempt(
		context.Background(),
		withFrame.StreamID,
		1,
	))
	require.NoError(t, runtime.Store.AppendFrame(
		context.Background(),
		withFrame.StreamID,
		1,
		service.StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data: []byte(
				"id: 1:1\n" +
					"event: response.output_text.delta\n" +
					"data: {\"delta\":\"committed\"}\n\n",
			),
		},
	))

	withoutFrame, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:      "stream_reconcile_without_frame",
		DedupeKey:     "dedupe_reconcile_without_frame",
		UserID:        11,
		TokenID:       22,
		ModelName:     "glm-5.3",
		RelayFormat:   string(relaytypes.RelayFormatOpenAIResponses),
		RequestPath:   "/v1/responses",
		RequestDigest: "digest_reconcile_without_frame",
		Status:        model.StreamExecutionRunning,
		PublicAttempt: 1,
		AttemptCount:  1,
		LockedBy:      "dead-runner-without-frame",
		LockedUntil:   now - 1,
		ExpiresAt:     now + 3600,
	})
	require.NoError(t, err)
	require.True(t, created)

	pending, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:      "stream_reconcile_pending",
		DedupeKey:     "dedupe_reconcile_pending",
		UserID:        11,
		TokenID:       22,
		ModelName:     "glm-5.3",
		RelayFormat:   string(relaytypes.RelayFormatOpenAIResponses),
		RequestPath:   "/v1/responses",
		RequestDigest: "digest_reconcile_pending",
		Status:        model.StreamExecutionPending,
		ExpiresAt:     now + 3600,
	})
	require.NoError(t, err)
	require.True(t, created)
	expiredBilling, created, err := model.CreateOrGetStreamExecution(
		&model.StreamExecution{
			StreamID:      "stream_reconcile_expired_billing",
			DedupeKey:     "dedupe_reconcile_expired_billing",
			UserID:        11,
			TokenID:       22,
			ModelName:     "glm-5.3",
			RelayFormat:   string(relaytypes.RelayFormatOpenAIResponses),
			RequestPath:   "/v1/responses",
			RequestDigest: "digest_reconcile_expired_billing",
			Status:        model.StreamExecutionFailed,
			BillingStatus: model.StreamBillingReserved,
			ExpiresAt:     now - 1,
		},
	)
	require.NoError(t, err)
	require.True(t, created)

	recoverable, err := model.FindRecoverableStreamExecutions(now, 100)
	require.NoError(t, err)
	require.Len(t, recoverable, 2)
	for _, execution := range recoverable {
		assert.Positive(t, execution.AttemptCount)
	}

	handler := streamRecoveryReconcileHandler{}
	assert.Equal(t, model.SystemTaskTypeStreamRecoveryReconcile, handler.Type())
	assert.True(t, handler.Enabled())
	assert.Greater(t, handler.Interval(), time.Duration(0))

	first, err := reconcileExpiredStreamExecutions(
		context.Background(),
		runtime.Store,
		now,
		100,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, first.Reconciled)
	assert.Equal(t, 1, first.TerminalFrames)

	second, err := reconcileExpiredStreamExecutions(
		context.Background(),
		runtime.Store,
		now+1,
		100,
	)
	require.NoError(t, err)
	assert.Zero(t, second.Reconciled)
	assert.Zero(t, second.TerminalFrames)

	withFrameResult, err := model.GetStreamExecution(withFrame.StreamID)
	require.NoError(t, err)
	assert.Equal(t, model.StreamExecutionFailed, withFrameResult.Status)
	assert.Equal(t, int64(2), withFrameResult.CommittedSequence)
	assert.Equal(t, int64(2), withFrameResult.TerminalSequence)
	assert.Contains(t, withFrameResult.Error, "STATEFUL_REPLAY_UNSAFE")

	frames, err := runtime.Store.ReadFrames(
		context.Background(),
		withFrame.StreamID,
		1,
		0,
		10,
	)
	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.Contains(t, string(frames[0].Data), "\"delta\":\"committed\"")
	assert.Equal(t, 1, strings.Count(string(frames[1].Data), "event: error"))
	assert.Contains(t, string(frames[1].Data), "STATEFUL_REPLAY_UNSAFE")
	assert.True(t, frames[1].Terminal)

	withoutFrameResult, err := model.GetStreamExecution(withoutFrame.StreamID)
	require.NoError(t, err)
	assert.Equal(t, model.StreamExecutionFailed, withoutFrameResult.Status)
	assert.Zero(t, withoutFrameResult.CommittedSequence)
	assert.Zero(t, withoutFrameResult.TerminalSequence)
	assert.Contains(t, withoutFrameResult.Error, "STATEFUL_REPLAY_UNSAFE")
	pendingResult, err := model.GetStreamExecution(pending.StreamID)
	require.NoError(t, err)
	assert.Equal(t, model.StreamExecutionPending, pendingResult.Status)
	expiredBillingResult, err := model.GetStreamExecution(expiredBilling.StreamID)
	require.NoError(t, err)
	assert.Equal(t, model.StreamBillingUncertain, expiredBillingResult.BillingStatus)

	t.Run("faults remain recoverable and retries converge", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		now := common.GetTimestamp()
		createExpired := func(streamID string, at int64) *model.StreamExecution {
			t.Helper()
			execution, wasCreated, createErr := model.CreateOrGetStreamExecution(
				&model.StreamExecution{
					StreamID:          streamID,
					DedupeKey:         "dedupe_" + streamID,
					UserID:            11,
					TokenID:           22,
					ModelName:         "glm-5.3",
					RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
					RequestPath:       "/v1/responses",
					RequestDigest:     "digest_" + streamID,
					Status:            model.StreamExecutionRunning,
					PublicAttempt:     1,
					AttemptCount:      1,
					CommittedSequence: 1,
					BillingStatus:     model.StreamBillingReserved,
					LockedBy:          "dead-" + streamID,
					LockedUntil:       at - 1,
					ExpiresAt:         at + 3600,
				},
			)
			require.NoError(t, createErr)
			require.True(t, wasCreated)
			require.NoError(t, runtime.Store.AppendFrame(
				context.Background(),
				execution.StreamID,
				1,
				service.StreamRecoveryFrame{
					Sequence: 1,
					Kind:     "data",
					Data:     []byte("id: 1:1\ndata: {\"delta\":\"committed\"}\n\n"),
				},
			))
			return execution
		}
		assertSingleTerminal := func(streamID string) {
			t.Helper()
			frames, readErr := runtime.Store.ReadFrames(
				context.Background(),
				streamID,
				1,
				0,
				10,
			)
			require.NoError(t, readErr)
			require.Len(t, frames, 2)
			assert.Equal(t, 1, strings.Count(string(frames[1].Data), "event: error"))
			assert.True(t, frames[1].Terminal)
		}

		redisFailure := createExpired("stream_reconcile_redis_failure", now)
		forcedRedisErr := errors.New("forced terminal append failure")
		common.RDB.AddHook(&failRedisCommandOnceHook{
			command: "evalsha",
			err:     forcedRedisErr,
		})
		_, err = reconcileExpiredStreamExecutions(
			context.Background(),
			runtime.Store,
			now,
			100,
		)
		require.ErrorIs(t, err, forcedRedisErr)
		redisState, err := model.GetStreamExecution(redisFailure.StreamID)
		require.NoError(t, err)
		assert.Equal(t, model.StreamExecutionRecovering, redisState.Status)
		assert.Equal(t, model.StreamBillingUncertain, redisState.BillingStatus)
		assert.NotEmpty(t, redisState.LockedBy)
		assert.Greater(t, redisState.LockedUntil, now)
		assert.Zero(t, redisState.TerminalSequence)

		redisRetryAt := redisState.LockedUntil + 1
		summary, err := reconcileExpiredStreamExecutions(
			context.Background(),
			runtime.Store,
			redisRetryAt,
			100,
		)
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Reconciled)
		assertSingleTerminal(redisFailure.StreamID)

		dbFailure := createExpired("stream_reconcile_db_failure", redisRetryAt)
		forcedDBErr := errors.New("forced terminal CAS failure")
		const callbackName = "test:fail_stream_recovery_terminal_cas"
		callbackRegistered := true
		require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(
			callbackName,
			func(tx *gorm.DB) {
				updates, ok := tx.Statement.Dest.(map[string]any)
				if !ok || tx.Statement.Table != "stream_executions" {
					return
				}
				if status, exists := updates["status"]; exists &&
					status == model.StreamExecutionFailed {
					tx.AddError(forcedDBErr)
				}
			},
		))
		t.Cleanup(func() {
			if callbackRegistered {
				_ = model.DB.Callback().Update().Remove(callbackName)
			}
		})

		_, err = reconcileExpiredStreamExecutions(
			context.Background(),
			runtime.Store,
			redisRetryAt,
			100,
		)
		require.ErrorIs(t, err, forcedDBErr)
		dbState, err := model.GetStreamExecution(dbFailure.StreamID)
		require.NoError(t, err)
		assert.Equal(t, model.StreamExecutionRecovering, dbState.Status)
		assert.NotEmpty(t, dbState.LockedBy)
		frames, err := runtime.Store.ReadFrames(
			context.Background(),
			dbFailure.StreamID,
			1,
			0,
			10,
		)
		require.NoError(t, err)
		require.Len(t, frames, 2)
		assert.True(t, frames[1].Terminal)
		require.NoError(t, model.DB.Callback().Update().Remove(callbackName))
		callbackRegistered = false

		dbRetryAt := dbState.LockedUntil + 1
		summary, err = reconcileExpiredStreamExecutions(
			context.Background(),
			runtime.Store,
			dbRetryAt,
			100,
		)
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Reconciled)
		assertSingleTerminal(dbFailure.StreamID)

		crash := createExpired("stream_reconcile_crash_retry", dbRetryAt)
		apiError := newStatefulReplayUnsafeAPIError("lease_expired")
		terminalData, err := streamRecoveryRelayErrorEvent(
			relaytypes.RelayFormatOpenAIResponses,
			apiError,
		)
		require.NoError(t, err)
		terminalData = append([]byte("id: 1:2\n"), terminalData...)
		require.NoError(t, runtime.Store.AppendFrame(
			context.Background(),
			crash.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 2,
				Kind:     "data",
				Data:     terminalData,
				Terminal: true,
			},
		))
		require.NoError(t, model.DB.Model(&model.StreamExecution{}).
			Where("stream_id = ?", crash.StreamID).
			Updates(map[string]any{
				"status":       model.StreamExecutionRecovering,
				"locked_by":    "crashed-reconciler",
				"locked_until": dbRetryAt - 1,
			}).Error)

		summary, err = reconcileExpiredStreamExecutions(
			context.Background(),
			runtime.Store,
			dbRetryAt,
			100,
		)
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Reconciled)
		assertSingleTerminal(crash.StreamID)
	})

	t.Run("uncommitted ordinary frame is replaced by the unsafe terminal", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		now := common.GetTimestamp()
		execution, created, err := model.CreateOrGetStreamExecution(
			&model.StreamExecution{
				StreamID:          "stream_reconcile_uncommitted_frame",
				DedupeKey:         "dedupe_reconcile_uncommitted_frame",
				UserID:            11,
				TokenID:           22,
				ModelName:         "glm-5.3",
				RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
				RequestPath:       "/v1/responses",
				RequestDigest:     "digest_reconcile_uncommitted_frame",
				Status:            model.StreamExecutionRunning,
				PublicAttempt:     1,
				AttemptCount:      1,
				CommittedSequence: 1,
				LockedBy:          "crashed-after-xadd",
				LockedUntil:       now - 1,
				ExpiresAt:         now + 3600,
			},
		)
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, runtime.Store.AppendFrame(
			context.Background(),
			execution.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 1,
				Kind:     "data",
				Data:     []byte("id: 1:1\ndata: {\"delta\":\"committed\"}\n\n"),
			},
		))
		require.NoError(t, runtime.Store.AppendFrame(
			context.Background(),
			execution.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 2,
				Kind:     "data",
				Data:     []byte("id: 1:2\ndata: {\"delta\":\"uncommitted\"}\n\n"),
			},
		))

		summary, err := reconcileExpiredStreamExecutions(
			context.Background(),
			runtime.Store,
			now,
			100,
		)
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Reconciled)
		assert.Equal(t, 1, summary.TerminalFrames)

		frames, err := runtime.Store.ReadFrames(
			context.Background(),
			execution.StreamID,
			1,
			0,
			10,
		)
		require.NoError(t, err)
		require.Len(t, frames, 2)
		assert.Contains(t, string(frames[0].Data), "\"delta\":\"committed\"")
		assert.NotContains(t, string(frames[1].Data), "uncommitted")
		assert.Contains(t, string(frames[1].Data), "STATEFUL_REPLAY_UNSAFE")
		assert.True(t, frames[1].Terminal)

		updated, err := model.GetStreamExecution(execution.StreamID)
		require.NoError(t, err)
		assert.Equal(t, model.StreamExecutionFailed, updated.Status)
		assert.Equal(t, int64(2), updated.CommittedSequence)
		assert.Equal(t, int64(2), updated.TerminalSequence)
	})

	t.Run("committed terminal is finalized without a second terminal", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		now := common.GetTimestamp()
		execution, created, err := model.CreateOrGetStreamExecution(
			&model.StreamExecution{
				StreamID:          "stream_reconcile_committed_terminal",
				DedupeKey:         "dedupe_reconcile_committed_terminal",
				UserID:            11,
				TokenID:           22,
				ModelName:         "glm-5.3",
				RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
				RequestPath:       "/v1/responses",
				RequestDigest:     "digest_reconcile_committed_terminal",
				Status:            model.StreamExecutionRunning,
				PublicAttempt:     1,
				AttemptCount:      1,
				CommittedSequence: 1,
				LockedBy:          "crashed-after-terminal-cas",
				LockedUntil:       now - 1,
				ExpiresAt:         now + 3600,
			},
		)
		require.NoError(t, err)
		require.True(t, created)
		terminalFrame, err := runtime.Store.AppendFrameForRollback(
			context.Background(),
			execution.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 1,
				Kind:     "data",
				Data: []byte(
					"id: 1:1\n" +
						"event: response.completed\n" +
						"data: {\"type\":\"response.completed\"}\n\n",
				),
				Terminal: true,
			},
		)
		require.NoError(t, err)

		summary, err := reconcileExpiredStreamExecutions(
			context.Background(),
			runtime.Store,
			now,
			100,
		)
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Reconciled)
		assert.Equal(t, 1, summary.TerminalFrames)
		require.ErrorIs(
			t,
			runtime.Store.RollbackFrame(
				context.Background(),
				execution.StreamID,
				1,
				terminalFrame,
			),
			service.ErrStreamRecoveryFrameChanged,
		)

		frames, err := runtime.Store.ReadFrames(
			context.Background(),
			execution.StreamID,
			1,
			0,
			10,
		)
		require.NoError(t, err)
		require.Len(t, frames, 1)
		assert.Contains(t, string(frames[0].Data), "response.completed")
		updated, err := model.GetStreamExecution(execution.StreamID)
		require.NoError(t, err)
		assert.Equal(t, model.StreamExecutionCompleted, updated.Status)
		assert.Equal(t, int64(1), updated.TerminalSequence)
	})

	t.Run("uncommitted terminal is adopted without duplication", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		now := common.GetTimestamp()
		execution, created, err := model.CreateOrGetStreamExecution(
			&model.StreamExecution{
				StreamID:          "stream_reconcile_uncommitted_terminal",
				DedupeKey:         "dedupe_reconcile_uncommitted_terminal",
				UserID:            11,
				TokenID:           22,
				ModelName:         "glm-5.3",
				RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
				RequestPath:       "/v1/responses",
				RequestDigest:     "digest_reconcile_uncommitted_terminal",
				Status:            model.StreamExecutionRunning,
				PublicAttempt:     1,
				AttemptCount:      1,
				CommittedSequence: 1,
				LockedBy:          "crashed-before-terminal-cas",
				LockedUntil:       now - 1,
				ExpiresAt:         now + 3600,
			},
		)
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, runtime.Store.AppendFrame(
			context.Background(),
			execution.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 1,
				Kind:     "data",
				Data:     []byte("id: 1:1\ndata: {\"delta\":\"committed\"}\n\n"),
			},
		))
		terminalFrame, err := runtime.Store.AppendFrameForRollback(
			context.Background(),
			execution.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 2,
				Kind:     "data",
				Data: []byte(
					"id: 1:2\n" +
						"event: response.failed\n" +
						"data: {\"type\":\"response.failed\"}\n\n",
				),
				Terminal: true,
			},
		)
		require.NoError(t, err)

		summary, err := reconcileExpiredStreamExecutions(
			context.Background(),
			runtime.Store,
			now,
			100,
		)
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Reconciled)
		assert.Equal(t, 1, summary.TerminalFrames)
		require.ErrorIs(
			t,
			runtime.Store.RollbackFrame(
				context.Background(),
				execution.StreamID,
				1,
				terminalFrame,
			),
			service.ErrStreamRecoveryFrameChanged,
		)

		frames, err := runtime.Store.ReadFrames(
			context.Background(),
			execution.StreamID,
			1,
			0,
			10,
		)
		require.NoError(t, err)
		require.Len(t, frames, 2)
		assert.Contains(t, string(frames[1].Data), "response.failed")
		updated, err := model.GetStreamExecution(execution.StreamID)
		require.NoError(t, err)
		assert.Equal(t, model.StreamExecutionFailed, updated.Status)
		assert.Equal(t, int64(2), updated.CommittedSequence)
		assert.Equal(t, int64(2), updated.TerminalSequence)
	})

	t.Run("lost reconciliation lease leaves terminal for the new owner", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		now := common.GetTimestamp()
		execution, created, err := model.CreateOrGetStreamExecution(
			&model.StreamExecution{
				StreamID:          "stream_reconcile_lease_turnover",
				DedupeKey:         "dedupe_reconcile_lease_turnover",
				UserID:            11,
				TokenID:           22,
				ModelName:         "glm-5.3",
				RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
				RequestPath:       "/v1/responses",
				RequestDigest:     "digest_reconcile_lease_turnover",
				Status:            model.StreamExecutionRecovering,
				PublicAttempt:     1,
				AttemptCount:      1,
				CommittedSequence: 1,
				LockedBy:          "new-reconciler",
				LockedUntil:       now + 30,
				ExpiresAt:         now + 3600,
			},
		)
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, runtime.Store.AppendFrame(
			context.Background(),
			execution.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 1,
				Kind:     "data",
				Data:     []byte("id: 1:1\ndata: {\"delta\":\"committed\"}\n\n"),
			},
		))
		terminalData, err := statefulReplayUnsafeTerminalFrame(
			relaytypes.RelayFormatOpenAIResponses,
			1,
			2,
		)
		require.NoError(t, err)
		terminalFrame, err := runtime.Store.AppendFrameForRollback(
			context.Background(),
			execution.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 2,
				Kind:     "data",
				Data:     terminalData,
				Terminal: true,
			},
		)
		require.NoError(t, err)
		staleClaim := *execution
		staleClaim.LockedBy = "old-reconciler"

		_, _, err = finishReconciledStreamExecution(
			context.Background(),
			runtime.Store,
			&staleClaim,
			model.StreamExecutionFailed,
			2,
			errStatefulReplayUnsafe.Error(),
		)
		require.ErrorIs(t, err, model.ErrStreamExecutionLeaseLost)
		frames, err := runtime.Store.ReadFrames(
			context.Background(),
			execution.StreamID,
			1,
			0,
			10,
		)
		require.NoError(t, err)
		require.Len(t, frames, 2)
		assert.Equal(t, terminalFrame.RedisID, frames[1].RedisID)
		assert.True(t, frames[1].Terminal)

		ordinaryExecution, created, err := model.CreateOrGetStreamExecution(
			&model.StreamExecution{
				StreamID:      "stream_reconcile_rollback_lease_turnover",
				DedupeKey:     "dedupe_reconcile_rollback_lease_turnover",
				UserID:        11,
				TokenID:       22,
				ModelName:     "glm-5.3",
				RelayFormat:   string(relaytypes.RelayFormatOpenAIResponses),
				RequestPath:   "/v1/responses",
				RequestDigest: "digest_reconcile_rollback_lease_turnover",
				Status:        model.StreamExecutionRecovering,
				PublicAttempt: 1,
				AttemptCount:  1,
				LockedBy:      "new-reconciler",
				LockedUntil:   now + 30,
				ExpiresAt:     now + 3600,
			},
		)
		require.NoError(t, err)
		require.True(t, created)
		ordinaryFrame, err := runtime.Store.AppendFrameForRollback(
			context.Background(),
			ordinaryExecution.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 1,
				Kind:     "data",
				Data:     []byte("data: uncommitted\n\n"),
			},
		)
		require.NoError(t, err)
		staleOrdinaryClaim := *ordinaryExecution
		staleOrdinaryClaim.LockedBy = "old-reconciler"
		err = rollbackUncommittedStreamRecoveryFrame(
			context.Background(),
			runtime.Store,
			&staleOrdinaryClaim,
			ordinaryFrame,
		)
		require.ErrorIs(t, err, model.ErrStreamExecutionLeaseLost)
		ordinaryFrames, err := runtime.Store.ReadFrames(
			context.Background(),
			ordinaryExecution.StreamID,
			1,
			0,
			10,
		)
		require.NoError(t, err)
		require.Len(t, ordinaryFrames, 1)
		assert.Equal(t, ordinaryFrame.RedisID, ordinaryFrames[0].RedisID)
	})
}

func TestAttemptZeroAndEmptyStreamReturnStableError(t *testing.T) {
	t.Run("attempt zero", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
			StreamID:      "stream_attempt_zero",
			DedupeKey:     "dedupe_attempt_zero",
			UserID:        11,
			TokenID:       22,
			ModelName:     "glm-5.3",
			RelayFormat:   string(relaytypes.RelayFormatOpenAIResponses),
			RequestPath:   "/v1/responses",
			RequestDigest: "digest_attempt_zero",
			Status:        model.StreamExecutionFailed,
			Error:         "stream recovery worker failed before attempt start",
			ExpiresAt:     common.GetTimestamp() + 3600,
		})
		require.NoError(t, err)
		require.True(t, created)

		requestContext, recorder := newStreamRecoveryTestContext("query-attempt-zero")
		requestContext.Request = requestContext.Request.WithContext(context.Background())
		subscribeStreamRecovery(requestContext, runtime.Store, execution)
		common.CleanupBodyStorage(requestContext)

		assert.Equal(t, http.StatusBadGateway, recorder.Code)
		assert.Equal(t, 1, strings.Count(recorder.Body.String(), "stream_recovery_protocol_error"))
	})

	t.Run("failed published attempt without frames", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
			StreamID:      "stream_failed_empty_attempt",
			DedupeKey:     "dedupe_failed_empty_attempt",
			UserID:        11,
			TokenID:       22,
			ModelName:     "glm-5.3",
			RelayFormat:   string(relaytypes.RelayFormatOpenAIResponses),
			RequestPath:   "/v1/responses",
			RequestDigest: "digest_failed_empty_attempt",
			Status:        model.StreamExecutionFailed,
			PublicAttempt: 1,
			AttemptCount:  1,
			Error:         "worker exited before committing a frame",
			ExpiresAt:     common.GetTimestamp() + 3600,
		})
		require.NoError(t, err)
		require.True(t, created)

		requestContext, recorder := newStreamRecoveryTestContext("query-failed-empty-attempt")
		subscribeStreamRecovery(requestContext, runtime.Store, execution)
		common.CleanupBodyStorage(requestContext)

		assert.Equal(t, http.StatusBadGateway, recorder.Code)
		assert.Equal(t, 1, strings.Count(recorder.Body.String(), "stream_recovery_protocol_error"))
	})

	t.Run("terminal database cursor without retained frames", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
			StreamID:          "stream_terminal_missing_frames",
			DedupeKey:         "dedupe_terminal_missing_frames",
			UserID:            11,
			TokenID:           22,
			ModelName:         "glm-5.3",
			RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
			RequestPath:       "/v1/responses",
			RequestDigest:     "digest_terminal_missing_frames",
			Status:            model.StreamExecutionFailed,
			PublicAttempt:     1,
			AttemptCount:      1,
			CommittedSequence: 1,
			TerminalSequence:  1,
			Error:             errStatefulReplayUnsafe.Error(),
			ExpiresAt:         common.GetTimestamp() - 1,
		})
		require.NoError(t, err)
		require.True(t, created)

		requestContext, recorder := newStreamRecoveryTestContext(
			"query-terminal-missing-frames",
		)
		subscribeStreamRecovery(requestContext, runtime.Store, execution)
		common.CleanupBodyStorage(requestContext)

		assert.Equal(t, http.StatusBadGateway, recorder.Code)
		assert.Equal(
			t,
			1,
			strings.Count(recorder.Body.String(), "stream_recovery_protocol_error"),
		)
	})

	t.Run("partial replay reports a missing committed tail as SSE", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		now := common.GetTimestamp()
		execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
			StreamID:          "stream_partial_missing_tail",
			DedupeKey:         "dedupe_partial_missing_tail",
			UserID:            11,
			TokenID:           22,
			ModelName:         "glm-5.3",
			RelayFormat:       string(relaytypes.RelayFormatOpenAIResponses),
			RequestPath:       "/v1/responses",
			RequestDigest:     "digest_partial_missing_tail",
			Status:            model.StreamExecutionRunning,
			PublicAttempt:     1,
			AttemptCount:      1,
			CommittedSequence: 2,
			LockedBy:          "active-with-missing-frame",
			LockedUntil:       now + 30,
			ExpiresAt:         now + 3600,
		})
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, runtime.Store.AppendFrame(
			context.Background(),
			execution.StreamID,
			1,
			service.StreamRecoveryFrame{
				Sequence: 1,
				Kind:     "data",
				Data:     []byte("id: 1:1\ndata: visible\n\n"),
			},
		))

		requestContext, recorder := newStreamRecoveryTestContext(
			"query-partial-missing-tail",
		)
		subscribeStreamRecovery(requestContext, runtime.Store, execution)
		common.CleanupBodyStorage(requestContext)

		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, 1, strings.Count(recorder.Body.String(), "data: visible"))
		assert.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
		assert.Equal(
			t,
			1,
			strings.Count(recorder.Body.String(), "stream_recovery_protocol_error"),
		)
	})

	t.Run("orphan pending attempt zero", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		runtime, err := service.GetStreamRecoveryRuntime()
		require.NoError(t, err)
		execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
			StreamID:      "stream_attempt_zero_pending",
			DedupeKey:     "dedupe_attempt_zero_pending",
			UserID:        11,
			TokenID:       22,
			ModelName:     "glm-5.3",
			RelayFormat:   string(relaytypes.RelayFormatOpenAIResponses),
			RequestPath:   "/v1/responses",
			RequestDigest: "digest_attempt_zero_pending",
			Status:        model.StreamExecutionPending,
			ExpiresAt:     common.GetTimestamp() + 3600,
		})
		require.NoError(t, err)
		require.True(t, created)

		requestContext, recorder := newStreamRecoveryTestContext("query-attempt-zero-pending")
		deadlineContext, cancel := context.WithTimeout(
			requestContext.Request.Context(),
			250*time.Millisecond,
		)
		defer cancel()
		requestContext.Request = requestContext.Request.WithContext(deadlineContext)
		subscribeStreamRecovery(requestContext, runtime.Store, execution)
		common.CleanupBodyStorage(requestContext)

		assert.Equal(t, http.StatusBadGateway, recorder.Code)
		assert.Equal(t, 1, strings.Count(recorder.Body.String(), "stream_recovery_protocol_error"))
	})

	t.Run("empty upstream stream", func(t *testing.T) {
		setupStreamRecoveryControllerTest(t)
		gin.SetMode(gin.TestMode)
		requestContext, recorder := newStreamRecoveryTestContextWithBody(
			"query-empty-stream",
			`{"model":"glm-5.3","stream":true,"store":false,"input":"probe"}`,
		)
		var workerRuns atomic.Int32

		require.True(t, handleStreamRecoveryRelay(
			requestContext,
			relaytypes.RelayFormatOpenAIResponses,
			func(*gin.Context, relaytypes.RelayFormat) {
				workerRuns.Add(1)
			},
		))
		common.CleanupBodyStorage(requestContext)

		assert.Equal(t, int32(1), workerRuns.Load())
		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"))
		assert.Equal(t, 1, strings.Count(recorder.Body.String(), `"code":"bad_response"`))
		streamID := recorder.Header().Get(common.StreamRecoveryIDHeader)
		require.Eventually(t, func() bool {
			execution, loadErr := model.GetStreamExecution(streamID)
			return loadErr == nil &&
				execution.Status == model.StreamExecutionFailed &&
				execution.CommittedSequence == 1 &&
				execution.TerminalSequence == 1
		}, time.Second, 10*time.Millisecond)
	})
}
