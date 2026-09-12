package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
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

func TestFrameCASRollbackIsInvisibleAndRetryable(t *testing.T) {
	t.Run("sealed frame must still exist", func(t *testing.T) {
		store, server := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
		ctx := context.Background()
		frame, err := store.AppendFrameForRollback(
			ctx,
			"stream-sealed-frame",
			1,
			StreamRecoveryFrame{
				Sequence: 1,
				Kind:     "data",
				Data:     []byte("event: response.completed\ndata: [DONE]\n\n"),
				Terminal: true,
			},
		)
		require.NoError(t, err)
		require.NoError(t, store.SealFrame(ctx, "stream-sealed-frame", 1, frame))
		require.ErrorIs(
			t,
			store.RollbackFrame(ctx, "stream-sealed-frame", 1, frame),
			ErrStreamRecoveryFrameChanged,
		)
		server.Del(store.eventsKey("stream-sealed-frame", 1))
		require.ErrorIs(
			t,
			store.SealFrame(ctx, "stream-sealed-frame", 1, frame),
			ErrStreamRecoveryFrameNotFound,
		)
	})

	t.Run("stale rollback cannot delete a replacement frame", func(t *testing.T) {
		store, server := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
		ctx := context.Background()
		stale := StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data:     []byte("data: stale\n\n"),
			Terminal: true,
		}
		replacement := StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data:     append([]byte(nil), stale.Data...),
			Terminal: true,
		}

		var err error
		stale, err = store.AppendFrameForRollback(
			ctx,
			"stream-rollback-aba",
			1,
			stale,
		)
		require.NoError(t, err)
		require.NoError(t, store.RollbackFrame(ctx, "stream-rollback-aba", 1, stale))
		replacement, err = store.AppendFrameForRollback(
			ctx,
			"stream-rollback-aba",
			1,
			replacement,
		)
		require.NoError(t, err)

		require.Error(t, store.RollbackFrame(ctx, "stream-rollback-aba", 1, stale))
		frames, err := store.ReadFrames(ctx, "stream-rollback-aba", 1, 0, 10)
		require.NoError(t, err)
		require.Len(t, frames, 1)
		assert.Equal(t, replacement.Data, frames[0].Data)
		assert.True(t, frames[0].Terminal)
		size, err := server.Get(store.sizeKey("stream-rollback-aba"))
		require.NoError(t, err)
		assert.Equal(t, strconv.Itoa(len(replacement.Data)), size)
	})

	t.Run("successful rollback", func(t *testing.T) {
		store, server := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
		ctx := context.Background()
		sentinel := StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data:     []byte("data: unrelated\n\n"),
		}
		require.NoError(t, store.AppendFrame(ctx, "stream-cas-rollback", 3, sentinel))

		casErr := errors.New("sequence CAS lost")
		failCAS := true
		writer := NewStreamRecoveryWriter(
			ctx,
			store,
			"stream-cas-rollback",
			1,
			func(int, int64) error {
				if failCAS {
					return fmt.Errorf(
						"%w: %w",
						ErrStreamRecoveryFrameRejected,
						casErr,
					)
				}
				return nil
			},
		)
		_, err := writer.WriteString(
			"event: response.output_text.delta\n" +
				"data: {\"delta\":\"must-be-invisible\"}\n\n",
		)
		require.NoError(t, err)
		assert.ErrorIs(t, writer.FlushError(), casErr)

		frames, err := store.ReadFrames(ctx, "stream-cas-rollback", 1, 0, 10)
		require.NoError(t, err)
		assert.Empty(t, frames)
		unrelated, err := store.ReadFrames(ctx, "stream-cas-rollback", 3, 0, 10)
		require.NoError(t, err)
		require.Len(t, unrelated, 1)
		assert.Equal(t, sentinel.Data, unrelated[0].Data)
		size, err := server.Get(store.sizeKey("stream-cas-rollback"))
		require.NoError(t, err)
		assert.Equal(t, strconv.Itoa(len(sentinel.Data)), size)

		failCAS = false
		require.NoError(t, writer.RotateAttempt(2))
		_, err = writer.WriteString(
			"event: response.completed\n" +
				"data: {\"type\":\"response.completed\"}\n\n",
		)
		require.NoError(t, err)
		require.NoError(t, writer.FlushError())
		retried, err := store.ReadFrames(ctx, "stream-cas-rollback", 2, 0, 10)
		require.NoError(t, err)
		require.Len(t, retried, 1)
		assert.True(t, retried[0].Terminal)
	})

	t.Run("rollback failure fails closed", func(t *testing.T) {
		store, server := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
		casErr := errors.New("sequence CAS lost")
		writer := NewStreamRecoveryWriter(
			context.Background(),
			store,
			"stream-cas-rollback-failure",
			1,
			func(int, int64) error {
				server.SetError("LOADING rollback unavailable")
				return fmt.Errorf(
					"%w: %w",
					ErrStreamRecoveryFrameRejected,
					casErr,
				)
			},
		)
		_, err := writer.WriteString("data: must-fail-closed\n\n")
		require.NoError(t, err)
		flushErr := writer.FlushError()
		require.Error(t, flushErr)
		assert.ErrorContains(t, flushErr, "sequence CAS lost")
		assert.ErrorContains(t, flushErr, "rollback")

		server.SetError("")
		assert.Error(t, writer.RotateAttempt(2))
		_, err = writer.WriteString("data: must-not-retry\n\n")
		assert.Error(t, err)
	})

	t.Run("missing frame rollback fails closed without changing accounting", func(t *testing.T) {
		store, server := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
		ctx := context.Background()
		const streamID = "stream-cas-missing-rollback"
		casErr := errors.New("sequence CAS lost")
		accountedBytes := ""
		writer := NewStreamRecoveryWriter(
			ctx,
			store,
			streamID,
			1,
			func(int, int64) error {
				var readErr error
				accountedBytes, readErr = server.Get(store.sizeKey(streamID))
				require.NoError(t, readErr)
				frames, readErr := store.ReadFrames(ctx, streamID, 1, 0, 1)
				require.NoError(t, readErr)
				require.Len(t, frames, 1)
				removed, deleteErr := store.client.XDel(
					ctx,
					store.eventsKey(streamID, 1),
					frames[0].RedisID,
				).Result()
				require.NoError(t, deleteErr)
				require.Equal(t, int64(1), removed)
				return fmt.Errorf(
					"%w: %w",
					ErrStreamRecoveryFrameRejected,
					casErr,
				)
			},
		)

		_, err := writer.WriteString("data: externally-deleted\n\n")
		require.NoError(t, err)
		flushErr := writer.FlushError()
		require.Error(t, flushErr)
		assert.ErrorContains(t, flushErr, "sequence CAS lost")
		assert.ErrorContains(t, flushErr, "rollback")

		sizeAfter, err := server.Get(store.sizeKey(streamID))
		require.NoError(t, err)
		assert.Equal(t, accountedBytes, sizeAfter)
		assert.Error(t, writer.RotateAttempt(2))
	})

	t.Run("ambiguous commit acknowledgement preserves the frame", func(t *testing.T) {
		store, server := newTestStreamRecoveryStore(t, StreamRecoveryStoreConfig{})
		ctx := context.Background()
		ackErr := errors.New("database acknowledgement lost")
		writer := NewStreamRecoveryWriter(
			ctx,
			store,
			"stream-cas-ack-lost",
			1,
			func(int, int64) error {
				return fmt.Errorf(
					"%w: %w",
					ErrStreamRecoveryCommitUncertain,
					ackErr,
				)
			},
		)

		_, err := writer.WriteString("data: durable-before-ack\n\n")
		require.NoError(t, err)
		assert.ErrorIs(t, writer.FlushError(), ackErr)
		frames, err := store.ReadFrames(ctx, "stream-cas-ack-lost", 1, 0, 10)
		require.NoError(t, err)
		require.Len(t, frames, 1)
		assert.Contains(t, string(frames[0].Data), "durable-before-ack")
		size, err := server.Get(store.sizeKey("stream-cas-ack-lost"))
		require.NoError(t, err)
		assert.Equal(t, strconv.Itoa(len(frames[0].Data)), size)
		assert.Error(t, writer.RotateAttempt(2))
	})
}

func TestStreamRecoveryRollbackAllowsSequenceReplacementOnRedis(t *testing.T) {
	address := os.Getenv("STREAM_RECOVERY_REDIS_ADDR")
	if address == "" {
		t.Skip("external Redis is not configured")
	}
	client := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.NoError(t, client.FlushDB(context.Background()).Err())
	keyring, err := LoadStreamRecoveryKeyring(
		writeRecoveryKeyFile(t, "current:"+encodedRecoveryKey('r')),
	)
	require.NoError(t, err)
	store, err := NewStreamRecoveryStore(
		client,
		keyring,
		StreamRecoveryStoreConfig{},
	)
	require.NoError(t, err)
	frame := StreamRecoveryFrame{
		Sequence: 1,
		Kind:     "data",
		Data:     []byte("data: same logical frame\n\n"),
	}

	first, err := store.AppendFrameForRollback(
		context.Background(),
		"stream-real-redis-replacement",
		1,
		frame,
	)
	require.NoError(t, err)
	require.NoError(t, store.RollbackFrame(
		context.Background(),
		"stream-real-redis-replacement",
		1,
		first,
	))
	second, err := store.AppendFrameForRollback(
		context.Background(),
		"stream-real-redis-replacement",
		1,
		frame,
	)
	require.NoError(t, err)
	require.Error(t, store.RollbackFrame(
		context.Background(),
		"stream-real-redis-replacement",
		1,
		first,
	))
	frames, err := store.ReadFrames(
		context.Background(),
		"stream-real-redis-replacement",
		1,
		0,
		10,
	)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.Equal(t, second.RollbackToken, frames[0].RollbackToken)

	legacyStreamID := "stream-real-redis-legacy"
	legacyData := []byte("data: legacy uncommitted\n\n")
	encrypted, err := keyring.Encrypt(
		legacyData,
		streamRecoveryFrameAAD(legacyStreamID, 1, 1),
	)
	require.NoError(t, err)
	require.NoError(t, client.Set(
		context.Background(),
		store.sizeKey(legacyStreamID),
		len(legacyData),
		time.Hour,
	).Err())
	require.NoError(t, client.XAdd(context.Background(), &redis.XAddArgs{
		Stream: store.eventsKey(legacyStreamID, 1),
		ID:     "1-0",
		Values: map[string]any{
			"kind":     "data",
			"data":     encrypted,
			"terminal": "0",
		},
	}).Err())
	legacyFrames, err := store.ReadFrames(
		context.Background(),
		legacyStreamID,
		1,
		0,
		10,
	)
	require.NoError(t, err)
	require.Len(t, legacyFrames, 1)
	require.NoError(t, store.RollbackFrame(
		context.Background(),
		legacyStreamID,
		1,
		legacyFrames[0],
	))
	replacement, err := store.AppendFrameForRollback(
		context.Background(),
		legacyStreamID,
		1,
		StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data:     []byte("event: error\ndata: replacement\n\n"),
			Terminal: true,
		},
	)
	require.NoError(t, err)
	assert.NotEqual(t, "1-0", replacement.RedisID)
	require.NoError(t, store.AppendFrame(
		context.Background(),
		legacyStreamID,
		1,
		StreamRecoveryFrame{
			Sequence: 2,
			Kind:     "data",
			Data:     []byte("data: after replacement\n\n"),
		},
	))
	afterReplacement, err := store.WaitFrames(
		context.Background(),
		legacyStreamID,
		1,
		1,
		time.Second,
	)
	require.NoError(t, err)
	require.Len(t, afterReplacement, 1)
	assert.Equal(t, int64(2), afterReplacement[0].Sequence)

	sealed, err := store.AppendFrameForRollback(
		context.Background(),
		"stream-real-redis-sealed",
		1,
		StreamRecoveryFrame{
			Sequence: 1,
			Kind:     "data",
			Data:     []byte("event: response.completed\ndata: [DONE]\n\n"),
			Terminal: true,
		},
	)
	require.NoError(t, err)
	require.NoError(t, store.SealFrame(
		context.Background(),
		"stream-real-redis-sealed",
		1,
		sealed,
	))
	require.ErrorIs(t, store.RollbackFrame(
		context.Background(),
		"stream-real-redis-sealed",
		1,
		sealed,
	), ErrStreamRecoveryFrameChanged)
	sealedFrames, err := store.ReadFrames(
		context.Background(),
		"stream-real-redis-sealed",
		1,
		0,
		10,
	)
	require.NoError(t, err)
	require.Len(t, sealedFrames, 1)
	assert.Equal(t, sealed.RedisID, sealedFrames[0].RedisID)
}
