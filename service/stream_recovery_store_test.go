package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStreamRecoveryStore(t *testing.T, config StreamRecoveryStoreConfig) (*StreamRecoveryStore, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	keyring, err := LoadStreamRecoveryKeyring(
		writeRecoveryKeyFile(t, "current:"+encodedRecoveryKey('a')),
	)
	require.NoError(t, err)
	store, err := NewStreamRecoveryStore(client, keyring, config)
	require.NoError(t, err)
	return store, server
}

func TestStreamRecoveryStoreEncryptsRequestAndBindsOwner(t *testing.T) {
	store, server := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	ctx := context.Background()

	require.NoError(t, store.SaveRequest(ctx, "stream-a", 11, 22, "digest", []byte("secret prompt")))
	raw, err := server.Get(store.requestKey("stream-a"))
	require.NoError(t, err)
	assert.NotContains(t, raw, "secret prompt")

	body, err := store.LoadRequest(ctx, "stream-a", 11, 22, "digest")
	require.NoError(t, err)
	assert.Equal(t, []byte("secret prompt"), body)

	_, err = store.LoadRequest(ctx, "stream-a", 12, 22, "digest")
	require.Error(t, err)
}

func TestStreamRecoveryStoreAppendsAndReadsOrderedFrames(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
	ctx := context.Background()

	for sequence, data := range []string{"first", "second", "third"} {
		require.NoError(t, store.AppendFrame(ctx, "stream-a", 1, StreamRecoveryFrame{
			Sequence: int64(sequence + 1),
			Kind:     "data",
			Data:     []byte(data),
			Terminal: sequence == 2,
		}))
	}
	raw, err := store.client.XRange(
		ctx,
		store.eventsKey("stream-a", 1),
		"-",
		"+",
	).Result()
	require.NoError(t, err)
	assert.NotContains(t, fmt.Sprint(raw), "first")

	frames, err := store.ReadFrames(ctx, "stream-a", 1, 1, 10)
	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.Equal(t, int64(2), frames[0].Sequence)
	assert.Equal(t, []byte("second"), frames[0].Data)
	assert.Equal(t, int64(3), frames[1].Sequence)
	assert.True(t, frames[1].Terminal)
}

func TestStreamRecoveryStoreEnforcesLimits(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{
		MaxEventsPerAttempt:  2,
		MaxBytesPerExecution: 5,
	})
	ctx := context.Background()

	require.NoError(t, store.AppendFrame(ctx, "stream-a", 1, StreamRecoveryFrame{
		Sequence: 1,
		Data:     []byte("1234"),
	}))
	err := store.AppendFrame(ctx, "stream-a", 1, StreamRecoveryFrame{
		Sequence: 2,
		Data:     []byte("56"),
	})
	require.ErrorIs(t, err, ErrStreamRecoveryByteLimit)

	err = store.AppendFrame(ctx, "stream-b", 1, StreamRecoveryFrame{
		Sequence: 3,
		Data:     []byte("1"),
	})
	require.ErrorIs(t, err, ErrStreamRecoveryEventLimit)
}

func TestStreamRecoveryStoreTracksAttemptAndDeletesPayload(t *testing.T) {
	store, _ := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{
		TTL: time.Hour,
	})
	ctx := context.Background()

	require.NoError(t, store.SaveRequest(ctx, "stream-a", 11, 22, "digest", []byte("body")))
	require.NoError(t, store.SetPublicAttempt(ctx, "stream-a", 2))
	require.NoError(t, store.SetAttemptState(ctx, "stream-a", 2, StreamRecoveryAttemptState{
		Status:       "completed",
		LastSequence: 9,
	}))
	require.NoError(t, store.AppendFrame(ctx, "stream-a", 2, StreamRecoveryFrame{
		Sequence: 1,
		Data:     []byte("data"),
	}))

	attempt, err := store.GetPublicAttempt(ctx, "stream-a")
	require.NoError(t, err)
	assert.Equal(t, 2, attempt)
	state, err := store.GetAttemptState(ctx, "stream-a", 2)
	require.NoError(t, err)
	assert.Equal(t, int64(9), state.LastSequence)

	require.NoError(t, store.DeleteExecution(ctx, "stream-a", 2))
	_, err = store.LoadRequest(ctx, "stream-a", 11, 22, "digest")
	assert.True(t, errors.Is(err, redis.Nil))
}
