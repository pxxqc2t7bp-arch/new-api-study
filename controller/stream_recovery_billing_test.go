package controller

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldRefundFailedRelayRejectsPartialRecoveryRefund(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	context, _ := newStreamRecoveryTestContext("query-partial-refund")
	writer := service.NewStreamRecoveryWriter(
		context.Request.Context(),
		runtime.Store,
		"stream-partial-refund",
		1,
		nil,
	)
	context.Writer = writer
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryBroker, writer)

	assert.True(t, shouldRefundFailedRelay(context))
	_, err = writer.WriteString(
		"event: response.output_text.delta\n" +
			"data: {\"delta\":\"billable\"}\n\n",
	)
	require.NoError(t, err)
	assert.False(t, shouldRefundFailedRelay(context))
	assert.True(t, common.GetContextKeyBool(
		context,
		constant.ContextKeyStreamRecoveryBillingUncertain,
	))
}

func TestStreamRecoveryPartialFailureMarksBillingUncertain(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	var workerRuns atomic.Int32
	run := func(context *gin.Context, _ relaytypes.RelayFormat) {
		workerRuns.Add(1)
		streamID := common.GetContextKeyString(
			context,
			constant.ContextKeyStreamRecoveryID,
		)
		won, err := model.UpdateStreamBillingState(
			streamID,
			[]string{model.StreamBillingNone},
			map[string]any{"billing_status": model.StreamBillingReserved},
		)
		require.NoError(t, err)
		require.True(t, won)
		_, err = context.Writer.Write([]byte(
			"event: response.output_text.delta\n" +
				"data: {\"delta\":\"partial\"}\n\n",
		))
		require.NoError(t, err)
		context.Writer.Flush()
		common.SetContextKey(
			context,
			constant.ContextKeyStreamRecoveryBillingUncertain,
			true,
		)
	}
	requestContext, recorder := newStreamRecoveryTestContext("query-partial-billing")

	require.True(t, handleStreamRecoveryRelay(
		requestContext,
		relaytypes.RelayFormatOpenAIResponses,
		run,
	))
	common.CleanupBodyStorage(requestContext)
	assert.Equal(t, int32(1), workerRuns.Load())
	assert.Contains(t, recorder.Body.String(), "\"delta\":\"partial\"")

	streamID := recorder.Header().Get(common.StreamRecoveryIDHeader)
	require.Eventually(t, func() bool {
		execution, err := model.GetStreamExecution(streamID)
		return err == nil &&
			execution.Status == model.StreamExecutionFailed &&
			execution.BillingStatus == model.StreamBillingUncertain
	}, time.Second, 10*time.Millisecond)
}

func TestCommentOnlyStreamStillRefundable(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	runtime, err := service.GetStreamRecoveryRuntime()
	require.NoError(t, err)
	context, _ := newStreamRecoveryTestContext("query-comment-refund")
	writer := service.NewStreamRecoveryWriter(
		context.Request.Context(),
		runtime.Store,
		"stream-comment-refund",
		1,
		nil,
	)
	context.Writer = writer
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryBroker, writer)

	_, err = writer.WriteString(": PING\n\n")
	require.NoError(t, err)
	assert.True(t, shouldRefundFailedRelay(context))
	assert.False(t, common.GetContextKeyBool(
		context,
		constant.ContextKeyStreamRecoveryBillingUncertain,
	))

	writer.Flush()
	assert.True(t, shouldRefundFailedRelay(context))
	assert.False(t, common.GetContextKeyBool(
		context,
		constant.ContextKeyStreamRecoveryBillingUncertain,
	))

	_, err = writer.WriteString(
		"event: response.output_text.delta\n" +
			"data: {\"delta\":\"billable\"}\n\n",
	)
	require.NoError(t, err)
	assert.False(t, shouldRefundFailedRelay(context))
	assert.True(t, common.GetContextKeyBool(
		context,
		constant.ContextKeyStreamRecoveryBillingUncertain,
	))

	t.Run("provider failure terminal is non-business and refundable", func(t *testing.T) {
		failureContext, _ := newStreamRecoveryTestContext("query-failed-terminal-refund")
		failureWriter := service.NewStreamRecoveryWriter(
			failureContext.Request.Context(),
			runtime.Store,
			"stream-failed-terminal-refund",
			1,
			nil,
		)
		failureContext.Writer = failureWriter
		common.SetContextKey(
			failureContext,
			constant.ContextKeyStreamRecoveryBroker,
			failureWriter,
		)

		_, writeErr := failureWriter.WriteString(
			"event: response.failed\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n",
		)
		require.NoError(t, writeErr)
		require.NoError(t, failureWriter.FlushError())
		assert.True(t, failureWriter.Terminal())
		assert.True(t, failureWriter.FailedTerminal())
		assert.False(t, failureWriter.HasBusinessOutput())
		assert.True(t, shouldRefundFailedRelay(failureContext))
	})

	t.Run("unsafe request without output remains uncertain", func(t *testing.T) {
		unsafeContext, _ := newStreamRecoveryTestContext("query-unsafe-refund")
		unsafeWriter := service.NewStreamRecoveryWriter(
			unsafeContext.Request.Context(),
			runtime.Store,
			"stream-unsafe-refund",
			1,
			nil,
		)
		unsafeContext.Writer = unsafeWriter
		common.SetContextKey(
			unsafeContext,
			constant.ContextKeyStreamRecoveryBroker,
			unsafeWriter,
		)
		common.SetContextKey(
			unsafeContext,
			constant.ContextKeyStreamRecoveryReplayUnsafe,
			service.StreamReplayUnsafeProviderState,
		)
		common.SetContextKey(
			unsafeContext,
			constant.ContextKeyStreamRecoverySubmissionStarted,
			true,
		)

		assert.False(t, shouldRefundFailedRelay(unsafeContext))
		assert.True(t, common.GetContextKeyBool(
			unsafeContext,
			constant.ContextKeyStreamRecoveryBillingUncertain,
		))
	})

	t.Run("unsafe request rejected before submission remains refundable", func(t *testing.T) {
		rejectedContext, _ := newStreamRecoveryTestContext("query-unsafe-pre-submit")
		rejectedWriter := service.NewStreamRecoveryWriter(
			rejectedContext.Request.Context(),
			runtime.Store,
			"stream-unsafe-pre-submit",
			1,
			nil,
		)
		rejectedContext.Writer = rejectedWriter
		common.SetContextKey(
			rejectedContext,
			constant.ContextKeyStreamRecoveryBroker,
			rejectedWriter,
		)
		common.SetContextKey(
			rejectedContext,
			constant.ContextKeyStreamRecoveryReplayUnsafe,
			service.StreamReplayUnsafeProviderState,
		)

		assert.True(t, shouldRefundFailedRelay(rejectedContext))
		assert.False(t, common.GetContextKeyBool(
			rejectedContext,
			constant.ContextKeyStreamRecoveryBillingUncertain,
		))
	})
}

func TestExpiredIdentityPreservesUncertainBilling(t *testing.T) {
	setupStreamRecoveryControllerTest(t)
	now := int64(10_000)
	newExecution := func(suffix string) *model.StreamExecution {
		return &model.StreamExecution{
			StreamID:      "stream_" + suffix,
			DedupeKey:     "dedupe_" + suffix,
			UserID:        11,
			TokenID:       22,
			ModelName:     "glm-5.3",
			RelayFormat:   string(relaytypes.RelayFormatOpenAIResponses),
			RequestPath:   "/v1/responses",
			RequestDigest: "digest_" + suffix,
			ExpiresAt:     2_000,
		}
	}
	unresolved := []string{
		model.StreamBillingReserving,
		model.StreamBillingReserved,
		model.StreamBillingSettling,
		model.StreamBillingRefunding,
		model.StreamBillingUncertain,
	}
	for index, billingStatus := range unresolved {
		execution := newExecution("expired-unresolved-" + billingStatus)
		execution.BillingStatus = billingStatus
		execution.ExpiresAt = now - int64(index+1)
		created, wasCreated, err := model.CreateOrGetStreamExecution(execution)
		require.NoError(t, err)
		require.True(t, wasCreated)
		require.Equal(t, execution.StreamID, created.StreamID)
	}

	safelyTerminal := []string{
		model.StreamBillingNone,
		model.StreamBillingSettled,
		model.StreamBillingRefunded,
	}
	for index, billingStatus := range safelyTerminal {
		execution := newExecution("expired-terminal-" + billingStatus)
		execution.Status = model.StreamExecutionCompleted
		execution.BillingStatus = billingStatus
		execution.ExpiresAt = now - int64(index+20)
		_, wasCreated, err := model.CreateOrGetStreamExecution(execution)
		require.NoError(t, err)
		require.True(t, wasCreated)
	}
	activeExpired := newExecution("expired-active-identity")
	activeExpired.Status = model.StreamExecutionRunning
	activeExpired.PublicAttempt = 1
	activeExpired.AttemptCount = 1
	activeExpired.LockedBy = "expired-active-runner"
	activeExpired.LockedUntil = now - 1
	activeExpired.ExpiresAt = now - 1
	_, wasCreated, err := model.CreateOrGetStreamExecution(activeExpired)
	require.NoError(t, err)
	require.True(t, wasCreated)

	require.NoError(t, model.DeleteExpiredStreamExecutions(now))

	for _, billingStatus := range unresolved {
		suffix := "expired-unresolved-" + billingStatus
		preserved, err := model.GetStreamExecution("stream_" + suffix)
		require.NoError(t, err)
		assert.Equal(t, model.StreamBillingUncertain, preserved.BillingStatus)
		won, err := model.UpdateStreamBillingState(
			preserved.StreamID,
			[]string{model.StreamBillingUncertain},
			map[string]any{"billing_status": model.StreamBillingRefunding},
		)
		require.NoError(t, err)
		assert.False(t, won)
		reloaded, err := model.GetStreamExecution(preserved.StreamID)
		require.NoError(t, err)
		assert.Equal(t, model.StreamBillingUncertain, reloaded.BillingStatus)

		replacement := newExecution("replacement-" + billingStatus)
		replacement.DedupeKey = preserved.DedupeKey
		replacement.ExpiresAt = now + 3600
		existing, wasCreated, err := model.CreateOrGetStreamExecution(replacement)
		require.NoError(t, err)
		assert.False(t, wasCreated)
		assert.Equal(t, preserved.StreamID, existing.StreamID)
	}

	for _, billingStatus := range safelyTerminal {
		suffix := "expired-terminal-" + billingStatus
		_, err := model.GetStreamExecution("stream_" + suffix)
		require.Error(t, err)

		replacement := newExecution("replacement-terminal-" + billingStatus)
		replacement.DedupeKey = "dedupe_" + suffix
		replacement.ExpiresAt = now + 3600
		inserted, wasCreated, err := model.CreateOrGetStreamExecution(replacement)
		require.NoError(t, err)
		assert.True(t, wasCreated)
		assert.Equal(t, replacement.StreamID, inserted.StreamID)
	}
	activeExpiredResult, err := model.GetStreamExecution(activeExpired.StreamID)
	require.NoError(t, err)
	assert.Equal(t, model.StreamExecutionFailed, activeExpiredResult.Status)
	assert.Contains(t, activeExpiredResult.Error, "STATEFUL_REPLAY_UNSAFE")

	stale := newExecution("stale-worker-refund")
	stale.Status = model.StreamExecutionRunning
	stale.PublicAttempt = 1
	stale.AttemptCount = 1
	stale.LockedBy = "expired-runner"
	stale.LockedUntil = now - 1
	stale.BillingStatus = model.StreamBillingReserved
	stale.ExpiresAt = now + 3600
	_, wasCreated, err = model.CreateOrGetStreamExecution(stale)
	require.NoError(t, err)
	require.True(t, wasCreated)
	won, err := model.UpdateStreamBillingState(
		stale.StreamID,
		[]string{model.StreamBillingReserved},
		map[string]any{"billing_status": model.StreamBillingRefunding},
	)
	require.NoError(t, err)
	assert.False(t, won)

	staleReservation := newExecution("stale-reservation-persist")
	staleReservation.Status = model.StreamExecutionRunning
	staleReservation.PublicAttempt = 1
	staleReservation.AttemptCount = 1
	staleReservation.LockedBy = "expired-reservation-runner"
	staleReservation.LockedUntil = now - 1
	staleReservation.BillingStatus = model.StreamBillingReserving
	staleReservation.ExpiresAt = now + 3600
	_, wasCreated, err = model.CreateOrGetStreamExecution(staleReservation)
	require.NoError(t, err)
	require.True(t, wasCreated)
	won, err = model.UpdateStreamBillingState(
		staleReservation.StreamID,
		[]string{model.StreamBillingReserving},
		map[string]any{
			"billing_status": model.StreamBillingReserved,
			"reserved_quota": 42,
			"token_consumed": 42,
		},
	)
	require.NoError(t, err)
	assert.False(t, won)
	preservedReservation, err := model.GetStreamExecution(staleReservation.StreamID)
	require.NoError(t, err)
	assert.Equal(t, model.StreamBillingUncertain, preservedReservation.BillingStatus)
	assert.Equal(t, 42, preservedReservation.ReservedQuota)
	assert.Equal(t, 42, preservedReservation.TokenConsumed)
}
