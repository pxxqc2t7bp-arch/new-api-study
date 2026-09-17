package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamRecoveryWriterPublishesFlushBoundaries(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	var published []int64
	writer := NewStreamRecoveryWriter(
		context.Background(),
		store,
		"stream-writer",
		1,
		func(sequence int64) error {
			published = append(published, sequence)
			return nil
		},
	)

	_, err := writer.WriteString("event: response.output_text.delta\n")
	require.NoError(t, err)
	_, err = writer.WriteString("data: {\"delta\":\"ok\"}\n\n")
	require.NoError(t, err)
	writer.Flush()
	_, err = writer.WriteString("event: response.completed\n")
	require.NoError(t, err)
	_, err = writer.WriteString("data: {\"type\":\"response.completed\"}\n\n")
	require.NoError(t, err)
	require.NoError(t, writer.FlushError())

	frames, err := store.ReadFrames(context.Background(), "stream-writer", 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.Equal(t, "id: 1\nevent: response.output_text.delta\ndata: {\"delta\":\"ok\"}\n\n", string(frames[0].Data))
	assert.False(t, frames[0].Terminal)
	assert.Equal(t, int64(2), frames[1].Sequence)
	assert.True(t, frames[1].Terminal)
	assert.True(t, writer.Terminal())
	assert.Equal(t, []int64{1, 2}, published)
}

func TestStreamRecoveryWriterRotatesAttempts(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	writer := NewStreamRecoveryWriter(context.Background(), store, "stream-rotate", 1, nil)

	_, err := writer.WriteString("data: first\n\n")
	require.NoError(t, err)
	writer.Flush()
	require.NoError(t, writer.RotateAttempt(2))
	_, err = writer.WriteString("data: second\n\n")
	require.NoError(t, err)
	writer.Flush()

	attempt, err := store.GetPublicAttempt(context.Background(), "stream-rotate")
	require.NoError(t, err)
	assert.Equal(t, 2, attempt)
	assert.Equal(t, int64(1), writer.Sequence())
	frames, err := store.ReadFrames(context.Background(), "stream-rotate", 2, 0, 10)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.Contains(t, string(frames[0].Data), "data: second")
}

func TestStreamRecoveryWriterStopsAfterPublisherError(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	expected := errors.New("lease lost")
	writer := NewStreamRecoveryWriter(
		context.Background(),
		store,
		"stream-error",
		1,
		func(int64) error { return expected },
	)

	_, err := writer.WriteString("data: first\n\n")
	require.NoError(t, err)
	assert.ErrorIs(t, writer.FlushError(), expected)
	_, err = writer.WriteString("data: second\n\n")
	assert.ErrorIs(t, err, expected)
}
