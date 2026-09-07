package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchCreateIdempotencyClaimAndFinish(t *testing.T) {
	truncateTables(t)
	scope := "7:11:idempotent"
	batch := &Batch{
		UserID:           7,
		TokenID:          11,
		InputFileID:      "file-input",
		Endpoint:         "/v1/responses",
		IdempotencyScope: &scope,
		RequestHash:      "hash-one",
	}
	items := []BatchItem{{
		LineNumber: 1,
		CustomID:   "request-one",
		Method:     "POST",
		URL:        "/v1/responses",
		Body:       json.RawMessage(`{"model":"gpt-test","input":"hello"}`),
	}}
	created, wasCreated, err := CreateBatchWithItems(batch, items)
	require.NoError(t, err)
	require.True(t, wasCreated)
	assert.NotEmpty(t, created.BatchID)

	duplicate := &Batch{
		UserID:           7,
		TokenID:          11,
		InputFileID:      "file-input",
		Endpoint:         "/v1/responses",
		IdempotencyScope: &scope,
		RequestHash:      "hash-one",
	}
	existing, wasCreated, err := CreateBatchWithItems(duplicate, items)
	require.NoError(t, err)
	assert.False(t, wasCreated)
	assert.Equal(t, created.BatchID, existing.BatchID)

	claimed, won, err := ClaimBatch(created.ID, "runner-a", 100, 200)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, BatchStatusInProgress, claimed.Status)

	_, won, err = ClaimBatch(created.ID, "runner-b", 150, 250)
	require.NoError(t, err)
	assert.False(t, won)

	pending, err := ListPendingBatchItems(created.ID, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	won, err = ClaimBatchItem(pending[0].ID, 110)
	require.NoError(t, err)
	require.True(t, won)
	response := json.RawMessage(`{"custom_id":"request-one","response":{"status_code":200}}`)
	won, err = FinishBatchItem(pending[0].ID, BatchItemStatusCompleted, response, nil, 120)
	require.NoError(t, err)
	require.True(t, won)

	won, err = MarkBatchFinalizing(claimed, "runner-a", 130)
	require.NoError(t, err)
	require.True(t, won)
	outputID := "file-output"
	won, err = FinishBatch(claimed, "runner-a", BatchStatusCompleted, &outputID, nil, nil, 140)
	require.NoError(t, err)
	require.True(t, won)

	stored, exists, err := GetOwnedBatch(7, 11, created.BatchID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, BatchStatusCompleted, stored.Status)
	assert.Equal(t, 1, stored.RequestCounts.Total)
	assert.Equal(t, 1, stored.RequestCounts.Completed)
	assert.Equal(t, outputID, *stored.OutputFileID)

	_, exists, err = GetOwnedBatch(8, 11, created.BatchID)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestBatchExpiredLeaseRecoversRunningItemsAndCancellation(t *testing.T) {
	truncateTables(t)
	batch, created, err := CreateBatchWithItems(&Batch{
		UserID:      7,
		TokenID:     11,
		InputFileID: "file-input",
		Endpoint:    "/v1/chat/completions",
	}, []BatchItem{{
		LineNumber: 1,
		CustomID:   "request-one",
		Method:     "POST",
		URL:        "/v1/chat/completions",
		Body:       json.RawMessage(`{"model":"gpt-test","messages":[]}`),
	}})
	require.NoError(t, err)
	require.True(t, created)
	claimed, won, err := ClaimBatch(batch.ID, "runner-a", 100, 200)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, "runner-a", claimed.LockedBy)
	items, err := ListPendingBatchItems(batch.ID, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	won, err = ClaimBatchItem(items[0].ID, 110)
	require.NoError(t, err)
	require.True(t, won)

	recovered, won, err := ClaimBatch(batch.ID, "runner-b", 201, 301)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, "runner-b", recovered.LockedBy)
	items, err = ListPendingBatchItems(batch.ID, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)

	cancelled, exists, err := RequestBatchCancellation(7, 11, batch.BatchID, 220)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, BatchStatusCancelling, cancelled.Status)
	count, err := CancelPendingBatchItems(batch.ID, 221)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}
