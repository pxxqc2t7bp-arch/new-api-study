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

func TestCreateOrGetStreamExecutionFindsExactLegacyDedupeKey(t *testing.T) {
	truncateTables(t)
	legacy := newTestStreamExecution("legacy")
	legacy.DedupeKey = "legacy-digest-bound-key"
	first, created, err := CreateOrGetStreamExecution(legacy)
	require.NoError(t, err)
	require.True(t, created)

	replacement := newTestStreamExecution("replacement")
	replacement.DedupeKey = "stable-client-key"
	got, created, err := CreateOrGetStreamExecution(
		replacement,
		legacy.DedupeKey,
	)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.StreamID, got.StreamID)
	assert.Equal(t, legacy.DedupeKey, got.DedupeKey)

	var count int64
	require.NoError(t, DB.Model(&StreamExecution{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestCreateOrGetStreamExecutionClaimsLegacyKeyForRollingUpgrade(t *testing.T) {
	truncateTables(t)
	stable := newTestStreamExecution("stable-first")
	stable.DedupeKey = "stable-client-key"
	stable.IdentityVersion = StreamRecoveryIdentityVersionStable
	legacyDedupeKey := "legacy-digest-bound-key"

	first, created, err := CreateOrGetStreamExecution(stable, legacyDedupeKey)
	require.NoError(t, err)
	require.True(t, created)

	legacy := newTestStreamExecution("legacy-second")
	legacy.DedupeKey = legacyDedupeKey
	got, created, err := CreateOrGetStreamExecution(legacy)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.StreamID, got.StreamID)

	var count int64
	require.NoError(t, DB.Model(&StreamExecution{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestCreateOrGetStreamExecutionBlocksAmbiguousLegacyIdentity(t *testing.T) {
	truncateTables(t)
	legacy := newTestStreamExecution("legacy-ambiguous")
	legacy.DedupeKey = "legacy-first-payload"
	legacy.ExpiresAt = common.GetTimestamp() + 3600
	first, created, err := CreateOrGetStreamExecution(legacy)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, StreamRecoveryIdentityVersionLegacy, first.IdentityVersion)

	replacement := newTestStreamExecution("stable-ambiguous")
	replacement.DedupeKey = "stable-client-key"
	replacement.IdentityVersion = StreamRecoveryIdentityVersionStable
	replacement.ExpiresAt = common.GetTimestamp() + 3600
	_, created, err = CreateOrGetStreamExecution(
		replacement,
		"legacy-different-payload",
	)
	require.ErrorIs(t, err, ErrStreamExecutionLegacyIdentityAmbiguous)
	assert.False(t, created)

	var count int64
	require.NoError(t, DB.Model(&StreamExecution{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
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
	require.NoError(t, StartStreamExecutionAttempt(execution.StreamID, "runner", 1, 31, ""))
	require.ErrorIs(
		t,
		UpdateStreamExecutionSequence(execution.StreamID, "runner", 1, 2),
		ErrStreamExecutionLeaseLost,
	)
	require.NoError(t, UpdateStreamExecutionSequence(execution.StreamID, "runner", 1, 1))
	require.ErrorIs(
		t,
		FinishStreamExecution(execution.StreamID, "other", 1, StreamExecutionCompleted, 1, ""),
		ErrStreamExecutionLeaseLost,
	)
	require.NoError(
		t,
		FinishStreamExecution(execution.StreamID, "runner", 1, StreamExecutionCompleted, 1, ""),
	)

	got, err := GetStreamExecution(execution.StreamID)
	require.NoError(t, err)
	assert.Equal(t, StreamExecutionCompleted, got.Status)
	assert.Equal(t, 1, got.PublicAttempt)
	assert.Equal(t, 1, int(got.TerminalSequence))
	assert.Empty(t, got.LockedBy)
}

func TestStreamExecutionAttemptFencesStaleWriter(t *testing.T) {
	truncateTables(t)
	execution, _, err := CreateOrGetStreamExecution(newTestStreamExecution("attempt-fence"))
	require.NoError(t, err)
	now := common.GetTimestamp()
	_, won, err := ClaimStreamExecution(execution.StreamID, "runner", now, now+100)
	require.NoError(t, err)
	require.True(t, won)

	require.NoError(t, StartStreamExecutionAttempt(execution.StreamID, "runner", 1, 31, ""))
	require.NoError(t, StartStreamExecutionAttempt(execution.StreamID, "runner", 2, 67, "pre_output_retry"))
	require.ErrorIs(
		t,
		StartStreamExecutionAttempt(execution.StreamID, "runner", 1, 31, "stale"),
		ErrStreamExecutionLeaseLost,
	)
	require.ErrorIs(
		t,
		UpdateStreamExecutionSequence(execution.StreamID, "runner", 1, 1),
		ErrStreamExecutionLeaseLost,
	)
	require.ErrorIs(
		t,
		FinishStreamExecution(execution.StreamID, "runner", 1, StreamExecutionCompleted, 1, ""),
		ErrStreamExecutionLeaseLost,
	)
	require.NoError(t, UpdateStreamExecutionSequence(execution.StreamID, "runner", 2, 1))
	require.NoError(
		t,
		FinishStreamExecution(execution.StreamID, "runner", 2, StreamExecutionCompleted, 1, ""),
	)
}

func TestFailExpiredStreamExecutionRequiresObservedAttemptAndExpiredLease(t *testing.T) {
	truncateTables(t)
	expired, _, err := CreateOrGetStreamExecution(newTestStreamExecution("expired"))
	require.NoError(t, err)
	require.NoError(t, DB.Model(&StreamExecution{}).
		Where("stream_id = ?", expired.StreamID).
		Updates(map[string]any{
			"status":         StreamExecutionRunning,
			"attempt_count":  1,
			"public_attempt": 1,
			"locked_by":      "dead-runner",
			"locked_until":   100,
			"billing_status": StreamBillingReserved,
		}).Error)

	failed, err := FailExpiredStreamExecution(
		expired.StreamID,
		1,
		101,
		"STATEFUL_REPLAY_UNSAFE",
	)
	require.NoError(t, err)
	assert.True(t, failed)

	updated, err := GetStreamExecution(expired.StreamID)
	require.NoError(t, err)
	assert.Equal(t, StreamExecutionFailed, updated.Status)
	assert.Equal(t, "STATEFUL_REPLAY_UNSAFE", updated.Error)
	assert.Equal(t, StreamBillingUncertain, updated.BillingStatus)
	assert.Empty(t, updated.LockedBy)
	assert.Zero(t, updated.LockedUntil)

	failed, err = FailExpiredStreamExecution(
		expired.StreamID,
		1,
		102,
		"must not replace terminal state",
	)
	require.NoError(t, err)
	assert.False(t, failed)

	active, _, err := CreateOrGetStreamExecution(newTestStreamExecution("active"))
	require.NoError(t, err)
	require.NoError(t, DB.Model(&StreamExecution{}).
		Where("stream_id = ?", active.StreamID).
		Updates(map[string]any{
			"status":        StreamExecutionRunning,
			"attempt_count": 1,
			"locked_by":     "live-runner",
			"locked_until":  200,
		}).Error)
	failed, err = FailExpiredStreamExecution(
		active.StreamID,
		1,
		101,
		"must not fence a live runner",
	)
	require.NoError(t, err)
	assert.False(t, failed)
}

func TestCommitFailedStreamExecutionTerminalUsesSequenceCAS(t *testing.T) {
	truncateTables(t)
	execution, _, err := CreateOrGetStreamExecution(newTestStreamExecution("failed-terminal"))
	require.NoError(t, err)
	require.NoError(t, DB.Model(&StreamExecution{}).
		Where("stream_id = ?", execution.StreamID).
		Updates(map[string]any{
			"status":             StreamExecutionFailed,
			"attempt_count":      1,
			"public_attempt":     1,
			"committed_sequence": 2,
			"error":              "STATEFUL_REPLAY_UNSAFE",
		}).Error)

	committed, err := CommitFailedStreamExecutionTerminal(
		execution.StreamID,
		1,
		2,
		3,
	)
	require.NoError(t, err)
	assert.True(t, committed)

	updated, err := GetStreamExecution(execution.StreamID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), updated.CommittedSequence)
	assert.Equal(t, int64(3), updated.TerminalSequence)

	committed, err = CommitFailedStreamExecutionTerminal(
		execution.StreamID,
		1,
		2,
		4,
	)
	require.NoError(t, err)
	assert.False(t, committed)
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
