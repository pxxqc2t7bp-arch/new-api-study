package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
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

func TestShouldRetryAllowsRecoverableWriterAfterPartialOutput(t *testing.T) {
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
	assert.True(t, shouldRetry(context, upstreamError, 1))

	_, err = writer.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	require.NoError(t, err)
	writer.Flush()
	assert.False(t, shouldRetry(context, upstreamError, 1))
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

func TestStreamRecoveryTakesOverExpiredExecutionAsNewAttempt(t *testing.T) {
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
	run := func(worker *gin.Context, _ relaytypes.RelayFormat) {
		workerRuns.Add(1)
		_, _ = worker.Writer.Write([]byte(
			"event: response.completed\n" +
				"data: {\"type\":\"response.completed\"}\n\n",
		))
		worker.Writer.Flush()
	}
	require.True(t, handleStreamRecoveryRelay(
		context,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(context)

	assert.Equal(t, int32(1), workerRuns.Load())
	assert.Equal(t, "2", recorder.Header().Get(common.StreamRecoveryAttemptHeader))
	var updated *model.StreamExecution
	require.Eventually(t, func() bool {
		updated, err = model.GetStreamExecution(execution.StreamID)
		return err == nil && updated.Status == model.StreamExecutionCompleted
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, 2, updated.AttemptCount)
	assert.Equal(t, 67, updated.ActiveChannelID)
	assert.Equal(t, model.StreamExecutionCompleted, updated.Status)
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
	assert.NotContains(t, recorder.Body.String(), "internal-runner")
	assert.NotContains(t, recorder.Body.String(), "billing_status")
	assert.NotContains(t, recorder.Body.String(), "dedupe")
}
