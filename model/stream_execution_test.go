package model

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStreamExecution(suffix string) *StreamExecution {
	return &StreamExecution{
		StreamID:      "stream_" + suffix,
		DedupeKey:     "dedupe_" + suffix,
		UserID:        11,
		TokenID:       22,
		ModelName:     "glm-5.3",
		RelayFormat:   "openai_responses",
		RequestPath:   "/v1/responses",
		RequestDigest: "digest_" + suffix,
		ExpiresAt:     2_000,
	}
}

func TestCreateOrGetStreamExecutionDeduplicates(t *testing.T) {
	truncateTables(t)

	first, created, err := CreateOrGetStreamExecution(newTestStreamExecution("same"))
	require.NoError(t, err)
	require.True(t, created)

	duplicate := newTestStreamExecution("different-id")
	duplicate.DedupeKey = first.DedupeKey
	got, created, err := CreateOrGetStreamExecution(duplicate)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.StreamID, got.StreamID)
	assert.Equal(t, StreamExecutionPending, got.Status)
	assert.Equal(t, StreamBillingNone, got.BillingStatus)
}

func TestStreamExecutionLeaseFencesConcurrentRunner(t *testing.T) {
	truncateTables(t)
	execution, _, err := CreateOrGetStreamExecution(newTestStreamExecution("lease"))
	require.NoError(t, err)

	claimed, won, err := ClaimStreamExecution(execution.StreamID, "runner-a", 100, 130)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, "runner-a", claimed.LockedBy)

	_, won, err = ClaimStreamExecution(execution.StreamID, "runner-b", 110, 140)
	require.NoError(t, err)
	assert.False(t, won)

	claimed, won, err = ClaimStreamExecution(execution.StreamID, "runner-b", 131, 161)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, "runner-b", claimed.LockedBy)

	require.ErrorIs(
		t,
		RenewStreamExecutionLease(execution.StreamID, "runner-a", 131, 170),
		ErrStreamExecutionLeaseLost,
	)
	require.NoError(
		t,
		RenewStreamExecutionLease(execution.StreamID, "runner-b", 131, 170),
	)
}

func TestStreamExecutionAttemptAndTerminalStateRequireLease(t *testing.T) {
	truncateTables(t)
	execution, _, err := CreateOrGetStreamExecution(newTestStreamExecution("attempt"))
	require.NoError(t, err)
	now := common.GetTimestamp()
	_, won, err := ClaimStreamExecution(execution.StreamID, "runner", now, now+100)
	require.NoError(t, err)
	require.True(t, won)

	require.NoError(t, StartStreamExecutionAttempt(execution.StreamID, "runner", 1, 31, ""))
	require.NoError(t, UpdateStreamExecutionSequence(execution.StreamID, "runner", 9))
	require.ErrorIs(
		t,
		FinishStreamExecution(execution.StreamID, "other", StreamExecutionCompleted, 9, ""),
		ErrStreamExecutionLeaseLost,
	)
	require.NoError(
		t,
		FinishStreamExecution(execution.StreamID, "runner", StreamExecutionCompleted, 9, ""),
	)

	got, err := GetStreamExecution(execution.StreamID)
	require.NoError(t, err)
	assert.Equal(t, StreamExecutionCompleted, got.Status)
	assert.Equal(t, 1, got.PublicAttempt)
	assert.Equal(t, 9, int(got.TerminalSequence))
	assert.Empty(t, got.LockedBy)
}

func TestOwnedStreamExecutionAndCancelAreScoped(t *testing.T) {
	truncateTables(t)
	execution, _, err := CreateOrGetStreamExecution(newTestStreamExecution("owner"))
	require.NoError(t, err)

	_, err = GetOwnedStreamExecution(execution.StreamID, execution.UserID+1, execution.TokenID)
	require.Error(t, err)

	cancelled, err := CancelOwnedStreamExecution(
		execution.StreamID,
		execution.UserID+1,
		execution.TokenID,
	)
	require.NoError(t, err)
	assert.False(t, cancelled)

	cancelled, err = CancelOwnedStreamExecution(
		execution.StreamID,
		execution.UserID,
		execution.TokenID,
	)
	require.NoError(t, err)
	assert.True(t, cancelled)
}

func TestUpdateStreamBillingStateIsCompareAndSwap(t *testing.T) {
	truncateTables(t)
	execution, _, err := CreateOrGetStreamExecution(newTestStreamExecution("billing"))
	require.NoError(t, err)

	won, err := UpdateStreamBillingState(
		execution.StreamID,
		[]string{StreamBillingNone},
		map[string]any{
			"billing_status": StreamBillingReserving,
			"reserved_quota": 42,
		},
	)
	require.NoError(t, err)
	assert.True(t, won)

	won, err = UpdateStreamBillingState(
		execution.StreamID,
		[]string{StreamBillingNone},
		map[string]any{"billing_status": StreamBillingReserved},
	)
	require.NoError(t, err)
	assert.False(t, won)
}

func TestStreamConsumeLogClaimIsExactlyOnce(t *testing.T) {
	truncateTables(t)
	execution, _, err := CreateOrGetStreamExecution(newTestStreamExecution("log"))
	require.NoError(t, err)

	claimed, err := ClaimStreamConsumeLog(execution.StreamID)
	require.NoError(t, err)
	assert.True(t, claimed)
	claimed, err = ClaimStreamConsumeLog(execution.StreamID)
	require.NoError(t, err)
	assert.False(t, claimed)

	require.NoError(t, FinishStreamConsumeLog(execution.StreamID, true))
	updated, err := GetStreamExecution(execution.StreamID)
	require.NoError(t, err)
	assert.Equal(t, StreamConsumeLogRecorded, updated.ConsumeLogStatus)
}

func TestRecordConsumeLogUsesStreamClaim(t *testing.T) {
	truncateTables(t)
	execution, _, err := CreateOrGetStreamExecution(newTestStreamExecution("consume"))
	require.NoError(t, err)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Set(common.RequestIdKey, execution.StreamID)
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryID, execution.StreamID)
	params := RecordConsumeLogParams{
		ModelName: "glm-5.3",
		TokenId:   execution.TokenID,
		IsStream:  true,
	}

	RecordConsumeLog(context, execution.UserID, params)
	RecordConsumeLog(context, execution.UserID, params)

	var count int64
	require.NoError(t, DB.Model(&Log{}).
		Where("request_id = ? AND type = ?", execution.StreamID, LogTypeConsume).
		Count(&count).Error)
	assert.Equal(t, int64(1), count)
	updated, err := GetStreamExecution(execution.StreamID)
	require.NoError(t, err)
	assert.Equal(t, StreamConsumeLogRecorded, updated.ConsumeLogStatus)
}
