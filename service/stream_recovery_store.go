package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	ErrStreamRecoveryFrameNotFound    = errors.New("stream recovery frame not found")
	ErrStreamRecoveryFrameChanged     = errors.New("stream recovery frame changed")
)

var appendStreamRecoveryFrameScript = redis.NewScript(`
if redis.call("HEXISTS", KEYS[4], ARGV[1]) == 1 then
  return redis.error_reply("STREAM_RECOVERY_SEQUENCE_EXISTS")
end
local size = tonumber(redis.call("GET", KEYS[2]) or "0")
local next_size = size + tonumber(ARGV[6])
if next_size > tonumber(ARGV[7]) then
  return ""
end
local generation = redis.call("INCR", KEYS[3])
local redis_id = ARGV[1] .. "-" .. generation
redis.call(
  "XADD",
  KEYS[1],
  "MAXLEN",
  "~",
  ARGV[8],
  redis_id,
  "sequence",
  ARGV[1],
  "kind",
  ARGV[2],
  "data",
  ARGV[3],
  "terminal",
  ARGV[4],
  "rollback_token",
  ARGV[5]
)
redis.call("HSET", KEYS[4], ARGV[1], ARGV[5] .. "|" .. redis_id)
redis.call("SET", KEYS[2], next_size)
redis.call("PEXPIRE", KEYS[1], ARGV[9])
redis.call("PEXPIRE", KEYS[2], ARGV[9])
redis.call("PEXPIRE", KEYS[3], ARGV[9])
redis.call("PEXPIRE", KEYS[4], ARGV[9])
return redis_id
`)

var rollbackStreamRecoveryFrameScript = redis.NewScript(`
local owner = redis.call("HGET", KEYS[3], ARGV[1])
if owner == false then
  if ARGV[3] ~= "" or ARGV[2] ~= ARGV[1] .. "-0" then
    return 0
  end
elseif owner ~= ARGV[3] .. "|" .. ARGV[2] then
  return -1
end
local entries = redis.call("XRANGE", KEYS[1], ARGV[2], ARGV[2], "COUNT", 1)
if #entries ~= 1 then
  return 0
end
local fields = entries[1][2]
local rollback_token = nil
for index = 1, #fields, 2 do
  if fields[index] == "rollback_token" then
    rollback_token = fields[index + 1]
    break
  end
end
if owner ~= false and rollback_token ~= ARGV[3] then
  return -1
end
local removed = redis.call("XDEL", KEYS[1], ARGV[2])
if removed == 1 then
  redis.call("HDEL", KEYS[3], ARGV[1])
  local size = redis.call("DECRBY", KEYS[2], ARGV[4])
  if size < 0 then
    redis.call("SET", KEYS[2], 0)
  end
end
return removed
`)

var sealStreamRecoveryFrameScript = redis.NewScript(`
local sealed_owner = "sealed|" .. ARGV[2]
local owner = redis.call("HGET", KEYS[2], ARGV[1])
local already_sealed = owner == sealed_owner
if owner == false then
  if ARGV[3] ~= "" then
    return -1
  end
elseif not already_sealed and owner ~= ARGV[3] .. "|" .. ARGV[2] then
  return -1
end
local entries = redis.call("XRANGE", KEYS[1], ARGV[2], ARGV[2], "COUNT", 1)
if #entries ~= 1 then
  return 0
end
local fields = entries[1][2]
local sequence = string.match(ARGV[2], "^(%d+)-")
local rollback_token = ""
for index = 1, #fields, 2 do
  if fields[index] == "sequence" then
    sequence = fields[index + 1]
  elseif fields[index] == "rollback_token" then
    rollback_token = fields[index + 1]
  end
end
if sequence ~= ARGV[1] then
  return -1
end
if rollback_token ~= ARGV[3] then
  return -1
end
redis.call("HSET", KEYS[2], ARGV[1], sealed_owner)
redis.call("PEXPIRE", KEYS[2], ARGV[4])
return 1
`)

type StreamRecoveryStoreConfig struct {
	TTL                  time.Duration
	MaxEventsPerAttempt  int64
	MaxBytesPerExecution int64
}

type StreamRecoveryFrame struct {
	Sequence      int64
	Kind          string
	Data          []byte
	Terminal      bool
	RollbackToken string
	RedisID       string
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
	_, err := store.AppendFrameForRollback(ctx, streamID, attempt, frame)
	return err
}

func (store *StreamRecoveryStore) AppendFrameForRollback(
	ctx context.Context,
	streamID string,
	attempt int,
	frame StreamRecoveryFrame,
) (StreamRecoveryFrame, error) {
	if frame.Sequence <= 0 {
		return StreamRecoveryFrame{}, errors.New(
			"stream recovery frame sequence must be positive",
		)
	}
	if frame.Sequence > store.config.MaxEventsPerAttempt {
		return StreamRecoveryFrame{}, ErrStreamRecoveryEventLimit
	}
	rollbackTokenBytes := make([]byte, 32)
	if _, err := rand.Read(rollbackTokenBytes); err != nil {
		return StreamRecoveryFrame{}, fmt.Errorf(
			"generate stream recovery rollback token: %w",
			err,
		)
	}
	frame.RollbackToken = hex.EncodeToString(rollbackTokenBytes)

	sizeKey := store.sizeKey(streamID)
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
		return StreamRecoveryFrame{}, err
	}
	redisID, err := appendStreamRecoveryFrameScript.Run(
		ctx,
		store.client,
		[]string{
			streamKey,
			sizeKey,
			store.generationKey(streamID, attempt),
			store.sequenceOwnerKey(streamID, attempt),
		},
		frame.Sequence,
		frame.Kind,
		encrypted,
		terminal,
		frame.RollbackToken,
		len(frame.Data),
		store.config.MaxBytesPerExecution,
		store.config.MaxEventsPerAttempt,
		store.config.TTL.Milliseconds(),
	).Text()
	if err != nil {
		if strings.Contains(err.Error(), "STREAM_RECOVERY_SEQUENCE_EXISTS") {
			return StreamRecoveryFrame{}, ErrStreamRecoveryFrameChanged
		}
		return StreamRecoveryFrame{}, err
	}
	if redisID == "" {
		return StreamRecoveryFrame{}, ErrStreamRecoveryByteLimit
	}
	frame.RedisID = redisID
	return frame, nil
}

func (store *StreamRecoveryStore) RollbackFrame(
	ctx context.Context,
	streamID string,
	attempt int,
	frame StreamRecoveryFrame,
) error {
	if frame.Sequence <= 0 {
		return errors.New("stream recovery frame sequence must be positive")
	}
	if frame.RedisID == "" {
		return ErrStreamRecoveryFrameChanged
	}
	removed, err := rollbackStreamRecoveryFrameScript.Run(
		ctx,
		store.client,
		[]string{
			store.eventsKey(streamID, attempt),
			store.sizeKey(streamID),
			store.sequenceOwnerKey(streamID, attempt),
		},
		frame.Sequence,
		frame.RedisID,
		frame.RollbackToken,
		len(frame.Data),
	).Int64()
	if err != nil {
		return err
	}
	if removed == -1 {
		return ErrStreamRecoveryFrameChanged
	}
	if removed != 1 {
		return ErrStreamRecoveryFrameNotFound
	}
	return nil
}

func (store *StreamRecoveryStore) SealFrame(
	ctx context.Context,
	streamID string,
	attempt int,
	frame StreamRecoveryFrame,
) error {
	if frame.Sequence <= 0 {
		return errors.New("stream recovery frame sequence must be positive")
	}
	if frame.RedisID == "" {
		return ErrStreamRecoveryFrameChanged
	}
	sealed, err := sealStreamRecoveryFrameScript.Run(
		ctx,
		store.client,
		[]string{
			store.eventsKey(streamID, attempt),
			store.sequenceOwnerKey(streamID, attempt),
		},
		frame.Sequence,
		frame.RedisID,
		frame.RollbackToken,
		store.config.TTL.Milliseconds(),
	).Int64()
	if err != nil {
		return err
	}
	if sealed == -1 {
		return ErrStreamRecoveryFrameChanged
	}
	if sealed != 1 {
		return ErrStreamRecoveryFrameNotFound
	}
	return nil
}

func (store *StreamRecoveryStore) HasBusinessOutputThrough(
	ctx context.Context,
	streamID string,
	attempt int,
	throughSequence int64,
) (bool, bool, error) {
	if throughSequence <= 0 {
		return false, true, nil
	}
	var afterSequence int64
	expectedSequence := int64(1)
	for afterSequence < throughSequence {
		limit := min(int64(256), throughSequence-afterSequence)
		frames, err := store.ReadFrames(
			ctx,
			streamID,
			attempt,
			afterSequence,
			limit,
		)
		if err != nil {
			return false, false, err
		}
		if len(frames) == 0 {
			return false, false, nil
		}
		for _, frame := range frames {
			if frame.Sequence != expectedSequence ||
				frame.Sequence > throughSequence {
				return false, false, nil
			}
			if StreamRecoveryFrameHasBusinessData(frame.Data) {
				return true, true, nil
			}
			afterSequence = frame.Sequence
			expectedSequence++
			if afterSequence == throughSequence {
				return false, true, nil
			}
		}
	}
	return false, true, nil
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
		start = fmt.Sprintf("(%d-%d", afterSequence, ^uint64(0))
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
		lastID = fmt.Sprintf("%d-%d", afterSequence, ^uint64(0))
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
			store.generationKey(streamID, attempt),
			store.sequenceOwnerKey(streamID, attempt),
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
	sequenceText, ok := redisString(message.Values["sequence"])
	if !ok {
		sequenceText, _, ok = strings.Cut(message.ID, "-")
		if !ok {
			return StreamRecoveryFrame{}, fmt.Errorf(
				"invalid stream recovery event id %q",
				message.ID,
			)
		}
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
	rollbackToken, _ := redisString(message.Values["rollback_token"])
	return StreamRecoveryFrame{
		Sequence:      sequence,
		Kind:          kind,
		Data:          plaintext,
		Terminal:      terminal == "1",
		RollbackToken: rollbackToken,
		RedisID:       message.ID,
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

func (store *StreamRecoveryStore) generationKey(streamID string, attempt int) string {
	return fmt.Sprintf("newapi:stream:v1:%s:attempt:%d:generation", streamID, attempt)
}

func (store *StreamRecoveryStore) sequenceOwnerKey(streamID string, attempt int) string {
	return fmt.Sprintf("newapi:stream:v1:%s:attempt:%d:owners", streamID, attempt)
}
