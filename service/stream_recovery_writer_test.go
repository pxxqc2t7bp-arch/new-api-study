package service

import (
	"context"
	"errors"
	"strings"
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
		func(attempt int, sequence int64) error {
			assert.Equal(t, 1, attempt)
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
	assert.Equal(t, "id: 1:1\nevent: response.output_text.delta\ndata: {\"delta\":\"ok\"}\n\n", string(frames[0].Data))
	assert.False(t, frames[0].Terminal)
	assert.Equal(t, int64(2), frames[1].Sequence)
	assert.True(t, frames[1].Terminal)
	assert.True(t, writer.Terminal())
	assert.Equal(t, []int64{1, 2}, published)
}

func TestStreamRecoveryWriterOwnsCursorAndParsesTerminalFields(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	writer := NewStreamRecoveryWriter(context.Background(), store, "stream-fields", 2, nil)

	_, err := writer.WriteString(
		"id: upstream-event\n" +
			"event: response.output_text.delta\n" +
			"data: {\"delta\":\"show event: response.completed and data: [DONE] literally\"}\n\n",
	)
	require.NoError(t, err)
	writer.Flush()
	assert.False(t, writer.Terminal())

	frames, err := store.ReadFrames(context.Background(), "stream-fields", 2, 0, 10)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.Equal(
		t,
		"id: 2:1\n"+
			"event: response.output_text.delta\n"+
			"data: {\"delta\":\"show event: response.completed and data: [DONE] literally\"}\n\n",
		string(frames[0].Data),
	)
	assert.Equal(t, 1, strings.Count(string(frames[0].Data), "id: "))
}

func TestStreamRecoveryWriterTracksErrorTerminal(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	writer := NewStreamRecoveryWriter(context.Background(), store, "stream-error-terminal", 1, nil)

	_, err := writer.WriteString(
		"event: error\n" +
			"data: {\"type\":\"error\",\"code\":\"STATEFUL_REPLAY_UNSAFE\"}\n\n",
	)
	require.NoError(t, err)
	writer.Flush()

	assert.True(t, writer.Terminal())
	assert.True(t, writer.FailedTerminal())
}

func TestStreamRecoveryWriterAddsCursorAfterLeadingComment(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	writer := NewStreamRecoveryWriter(context.Background(), store, "stream-comment", 1, nil)

	_, err := writer.WriteString(
		": keepalive\n" +
			"event: response.output_text.delta\n" +
			"data: {\"delta\":\"visible\"}\n\n",
	)
	require.NoError(t, err)
	writer.Flush()

	frames, err := store.ReadFrames(context.Background(), "stream-comment", 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.True(t, strings.HasPrefix(string(frames[0].Data), "id: 1:1\n"))
}

func TestStreamRecoveryWriterFindsTerminalInMultiEventFlush(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	writer := NewStreamRecoveryWriter(context.Background(), store, "stream-multi-event", 1, nil)

	_, err := writer.WriteString(
		"data: {\"delta\":\"last\"}\n\n" +
			"data: [DONE]\n\n",
	)
	require.NoError(t, err)
	writer.Flush()

	assert.True(t, writer.Terminal())
	assert.False(t, writer.FailedTerminal())
	frames, err := store.ReadFrames(context.Background(), "stream-multi-event", 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.Contains(t, string(frames[0].Data), "id: 1:1")
	assert.Contains(t, string(frames[1].Data), "id: 1:2")
	assert.False(t, frames[0].Terminal)
	assert.True(t, frames[1].Terminal)
}

func TestStreamRecoveryWriterStopsAtFirstTerminalEvent(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	writer := NewStreamRecoveryWriter(context.Background(), store, "stream-single-terminal", 1, nil)

	_, err := writer.WriteString(
		"event: response.completed\n" +
			"data: {\"type\":\"response.completed\"}\n\n" +
			"data: {\"delta\":\"must-not-be-stored\"}\n\n",
	)
	require.NoError(t, err)
	writer.Flush()

	frames, err := store.ReadFrames(context.Background(), "stream-single-terminal", 1, 0, 10)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.True(t, frames[0].Terminal)
	assert.NotContains(t, string(frames[0].Data), "must-not-be-stored")

	_, err = writer.WriteString("data: after-terminal\n\n")
	require.Error(t, err)
}

func TestStreamRecoveryWriterRotatesAttempts(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	writer := NewStreamRecoveryWriter(context.Background(), store, "stream-rotate", 1, nil)

	_, err := writer.WriteString("data: first\n\n")
	require.NoError(t, err)
	writer.Flush()
	require.NoError(t, writer.RotateAttempt(2))
	attempt, err := store.GetPublicAttempt(context.Background(), "stream-rotate")
	require.NoError(t, err)
	assert.Zero(t, attempt)
	require.NoError(t, writer.PublishAttempt())
	_, err = writer.WriteString("data: second\n\n")
	require.NoError(t, err)
	writer.Flush()

	attempt, err = store.GetPublicAttempt(context.Background(), "stream-rotate")
	require.NoError(t, err)
	assert.Equal(t, 2, attempt)
	assert.Equal(t, int64(1), writer.Sequence())
	frames, err := store.ReadFrames(context.Background(), "stream-rotate", 2, 0, 10)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.Contains(t, string(frames[0].Data), "data: second")
}

func TestStreamRecoveryWriterPublishesRotatedAttemptIdentity(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	var attempts []int
	var sequences []int64
	writer := NewStreamRecoveryWriter(
		context.Background(),
		store,
		"stream-attempt-callback",
		1,
		func(attempt int, sequence int64) error {
			attempts = append(attempts, attempt)
			sequences = append(sequences, sequence)
			return nil
		},
	)

	_, err := writer.WriteString("data: first\n\n")
	require.NoError(t, err)
	writer.Flush()
	require.NoError(t, writer.RotateAttempt(2))
	_, err = writer.WriteString("data: second\n\n")
	require.NoError(t, err)
	writer.Flush()

	assert.Equal(t, []int{1, 2}, attempts)
	assert.Equal(t, []int64{1, 1}, sequences)
}

func TestStreamRecoveryWriterStopsAfterPublisherError(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	expected := errors.New("lease lost")
	writer := NewStreamRecoveryWriter(
		context.Background(),
		store,
		"stream-error",
		1,
		func(int, int64) error { return expected },
	)

	_, err := writer.WriteString("data: first\n\n")
	require.NoError(t, err)
	assert.ErrorIs(t, writer.FlushError(), expected)
	_, err = writer.WriteString("data: second\n\n")
	assert.ErrorIs(t, err, expected)
}
