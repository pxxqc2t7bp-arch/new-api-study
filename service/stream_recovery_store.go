package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
)

var (
	ErrStreamRecoveryStoreUnavailable = errors.New("stream recovery store is unavailable")
	ErrStreamRecoveryEventLimit       = errors.New("stream recovery event limit exceeded")
	ErrStreamRecoveryByteLimit        = errors.New("stream recovery byte limit exceeded")
)

type StreamRecoveryStoreConfig struct {
	TTL                  time.Duration
	MaxEventsPerAttempt  int64
	MaxBytesPerExecution int64
}

type StreamRecoveryFrame struct {
	Sequence int64
	Kind     string
	Data     []byte
	Terminal bool
}

type StreamRecoveryAttemptState struct {
	Status       string
	LastSequence int64
	Error        string
}

type StreamRecoveryStore struct {
	client  *redis.Client
	keyring *StreamRecoveryKeyring
	config  StreamRecoveryStoreConfig
}

func NewStreamRecoveryStore(
	client *redis.Client,
	keyring *StreamRecoveryKeyring,
	config StreamRecoveryStoreConfig,
) (*StreamRecoveryStore, error) {
	if client == nil {
		return nil, ErrStreamRecoveryStoreUnavailable
	}
	if keyring == nil {
		return nil, errors.New("stream recovery keyring is required")
	}
	if config.TTL <= 0 {
		config.TTL = 24 * time.Hour
	}
	if config.MaxEventsPerAttempt <= 0 {
		config.MaxEventsPerAttempt = 100_000
	}
	if config.MaxBytesPerExecution <= 0 {
		config.MaxBytesPerExecution = 64 * 1024 * 1024
	}
	return &StreamRecoveryStore{
		client:  client,
		keyring: keyring,
		config:  config,
	}, nil
}

func (store *StreamRecoveryStore) SaveRequest(
	ctx context.Context,
	streamID string,
	userID int,
	tokenID int,
	requestDigest string,
	requestBody []byte,
) error {
	encrypted, err := store.keyring.Encrypt(
		requestBody,
		streamRecoveryAAD(streamID, userID, tokenID, requestDigest),
	)
	if err != nil {
		return err
	}
	return store.client.Set(
		ctx,
		store.requestKey(streamID),
		encrypted,
		store.config.TTL,
	).Err()
}

func (store *StreamRecoveryStore) LoadRequest(
	ctx context.Context,
	streamID string,
	userID int,
	tokenID int,
	requestDigest string,
) ([]byte, error) {
	encrypted, err := store.client.Get(ctx, store.requestKey(streamID)).Bytes()
	if err != nil {
		return nil, err
	}
	return store.keyring.Decrypt(
		encrypted,
		streamRecoveryAAD(streamID, userID, tokenID, requestDigest),
	)
}

func (store *StreamRecoveryStore) DeleteRequest(ctx context.Context, streamID string) error {
	return store.client.Del(ctx, store.requestKey(streamID)).Err()
}

func (store *StreamRecoveryStore) AppendFrame(
	ctx context.Context,
	streamID string,
	attempt int,
	frame StreamRecoveryFrame,
) error {
	if frame.Sequence <= 0 {
		return errors.New("stream recovery frame sequence must be positive")
	}
	if frame.Sequence > store.config.MaxEventsPerAttempt {
		return ErrStreamRecoveryEventLimit
	}

	sizeKey := store.sizeKey(streamID)
	size, err := store.client.IncrBy(ctx, sizeKey, int64(len(frame.Data))).Result()
	if err != nil {
		return err
	}
	if size > store.config.MaxBytesPerExecution {
		_ = store.client.DecrBy(ctx, sizeKey, int64(len(frame.Data))).Err()
		return ErrStreamRecoveryByteLimit
	}

	streamKey := store.eventsKey(streamID, attempt)
	terminal := "0"
	if frame.Terminal {
		terminal = "1"
	}
	encrypted, err := store.keyring.Encrypt(
		frame.Data,
		streamRecoveryFrameAAD(streamID, attempt, frame.Sequence),
	)
	if err != nil {
		_ = store.client.DecrBy(ctx, sizeKey, int64(len(frame.Data))).Err()
		return err
	}
	err = store.client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamKey,
		ID:     fmt.Sprintf("%d-0", frame.Sequence),
		MaxLen: store.config.MaxEventsPerAttempt,
		Approx: true,
		Values: map[string]any{
			"kind":     frame.Kind,
			"data":     encrypted,
			"terminal": terminal,
		},
	}).Err()
	if err != nil {
		_ = store.client.DecrBy(ctx, sizeKey, int64(len(frame.Data))).Err()
		return err
	}
	pipe := store.client.TxPipeline()
	pipe.Expire(ctx, streamKey, store.config.TTL)
	pipe.Expire(ctx, sizeKey, store.config.TTL)
	_, err = pipe.Exec(ctx)
	return err
}

func (store *StreamRecoveryStore) ReadFrames(
	ctx context.Context,
	streamID string,
	attempt int,
	afterSequence int64,
	limit int64,
) ([]StreamRecoveryFrame, error) {
	if limit <= 0 {
		limit = 256
	}
	start := "-"
	if afterSequence > 0 {
		start = fmt.Sprintf("(%d-0", afterSequence)
	}
	messages, err := store.client.XRangeN(
		ctx,
		store.eventsKey(streamID, attempt),
		start,
		"+",
		limit,
	).Result()
	if err != nil {
		return nil, err
	}
	frames := make([]StreamRecoveryFrame, 0, len(messages))
	for _, message := range messages {
		frame, parseErr := store.decodeStreamRecoveryFrame(streamID, attempt, message)
		if parseErr != nil {
			return nil, parseErr
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func (store *StreamRecoveryStore) WaitFrames(
	ctx context.Context,
	streamID string,
	attempt int,
	afterSequence int64,
	block time.Duration,
) ([]StreamRecoveryFrame, error) {
	lastID := "0-0"
	if afterSequence > 0 {
		lastID = fmt.Sprintf("%d-0", afterSequence)
	}
	results, err := store.client.XRead(ctx, &redis.XReadArgs{
		Streams: []string{store.eventsKey(streamID, attempt), lastID},
		Count:   256,
		Block:   block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var frames []StreamRecoveryFrame
	for _, result := range results {
		for _, message := range result.Messages {
			frame, parseErr := store.decodeStreamRecoveryFrame(streamID, attempt, message)
			if parseErr != nil {
				return nil, parseErr
			}
			frames = append(frames, frame)
		}
	}
	return frames, nil
}

func (store *StreamRecoveryStore) SetPublicAttempt(
	ctx context.Context,
	streamID string,
	attempt int,
) error {
	return store.client.Set(
		ctx,
		store.publicAttemptKey(streamID),
		attempt,
		store.config.TTL,
	).Err()
}

func (store *StreamRecoveryStore) GetPublicAttempt(ctx context.Context, streamID string) (int, error) {
	value, err := store.client.Get(ctx, store.publicAttemptKey(streamID)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return value, err
}

func (store *StreamRecoveryStore) SetAttemptState(
	ctx context.Context,
	streamID string,
	attempt int,
	state StreamRecoveryAttemptState,
) error {
	key := store.attemptStateKey(streamID, attempt)
	pipe := store.client.TxPipeline()
	pipe.HSet(ctx, key, map[string]any{
		"status":        state.Status,
		"last_sequence": state.LastSequence,
		"error":         state.Error,
	})
	pipe.Expire(ctx, key, store.config.TTL)
	_, err := pipe.Exec(ctx)
	return err
}

func (store *StreamRecoveryStore) GetAttemptState(
	ctx context.Context,
	streamID string,
	attempt int,
) (StreamRecoveryAttemptState, error) {
	values, err := store.client.HGetAll(ctx, store.attemptStateKey(streamID, attempt)).Result()
	if err != nil {
		return StreamRecoveryAttemptState{}, err
	}
	if len(values) == 0 {
		return StreamRecoveryAttemptState{}, redis.Nil
	}
	lastSequence, err := strconv.ParseInt(values["last_sequence"], 10, 64)
	if err != nil {
		return StreamRecoveryAttemptState{}, fmt.Errorf("parse stream recovery last sequence: %w", err)
	}
	return StreamRecoveryAttemptState{
		Status:       values["status"],
		LastSequence: lastSequence,
		Error:        values["error"],
	}, nil
}

func (store *StreamRecoveryStore) DeleteExecution(
	ctx context.Context,
	streamID string,
	attemptCount int,
) error {
	keys := []string{
		store.requestKey(streamID),
		store.publicAttemptKey(streamID),
		store.sizeKey(streamID),
	}
	for attempt := 1; attempt <= attemptCount; attempt++ {
		keys = append(
			keys,
			store.eventsKey(streamID, attempt),
			store.attemptStateKey(streamID, attempt),
		)
	}
	return store.client.Del(ctx, keys...).Err()
}

func streamRecoveryAAD(streamID string, userID int, tokenID int, requestDigest string) []byte {
	return fmt.Appendf(nil, "%s:%d:%d:%s", streamID, userID, tokenID, requestDigest)
}

func streamRecoveryFrameAAD(streamID string, attempt int, sequence int64) []byte {
	return fmt.Appendf(nil, "%s:%d:%d", streamID, attempt, sequence)
}

func (store *StreamRecoveryStore) decodeStreamRecoveryFrame(
	streamID string,
	attempt int,
	message redis.XMessage,
) (StreamRecoveryFrame, error) {
	sequenceText, _, ok := strings.Cut(message.ID, "-")
	if !ok {
		return StreamRecoveryFrame{}, fmt.Errorf("invalid stream recovery event id %q", message.ID)
	}
	sequence, err := strconv.ParseInt(sequenceText, 10, 64)
	if err != nil {
		return StreamRecoveryFrame{}, fmt.Errorf("parse stream recovery event id: %w", err)
	}
	data, ok := redisString(message.Values["data"])
	if !ok {
		return StreamRecoveryFrame{}, errors.New("stream recovery event data is missing")
	}
	plaintext, err := store.keyring.Decrypt(
		[]byte(data),
		streamRecoveryFrameAAD(streamID, attempt, sequence),
	)
	if err != nil {
		return StreamRecoveryFrame{}, err
	}
	kind, _ := redisString(message.Values["kind"])
	terminal, _ := redisString(message.Values["terminal"])
	return StreamRecoveryFrame{
		Sequence: sequence,
		Kind:     kind,
		Data:     plaintext,
		Terminal: terminal == "1",
	}, nil
}

func redisString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case []byte:
		return string(typed), true
	default:
		return "", false
	}
}

func (store *StreamRecoveryStore) requestKey(streamID string) string {
	return "newapi:stream:v1:" + streamID + ":request"
}

func (store *StreamRecoveryStore) eventsKey(streamID string, attempt int) string {
	return fmt.Sprintf("newapi:stream:v1:%s:attempt:%d:events", streamID, attempt)
}

func (store *StreamRecoveryStore) attemptStateKey(streamID string, attempt int) string {
	return fmt.Sprintf("newapi:stream:v1:%s:attempt:%d:state", streamID, attempt)
}

func (store *StreamRecoveryStore) publicAttemptKey(streamID string) string {
	return "newapi:stream:v1:" + streamID + ":public"
}

func (store *StreamRecoveryStore) sizeKey(streamID string) string {
	return "newapi:stream:v1:" + streamID + ":bytes"
}
