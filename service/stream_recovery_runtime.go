package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

type StreamRecoveryIdentity struct {
	DedupeKey       string
	LegacyDedupeKey string
	RequestDigest   string
	QueryIDSource   string
}

type StreamRecoveryRuntime struct {
	Store   *StreamRecoveryStore
	Keyring *StreamRecoveryKeyring
}

var streamRecoveryRuntimeCache struct {
	sync.Mutex
	keyPath string
	runtime *StreamRecoveryRuntime
}

func GetStreamRecoveryRuntime() (*StreamRecoveryRuntime, error) {
	setting := operation_setting.GetStreamRecoverySetting()
	if !setting.Enabled &&
		setting.IdentityMode != operation_setting.StreamRecoveryIdentityModeDraining {
		return nil, errors.New("stream recovery is disabled")
	}
	if !common.RedisEnabled || common.RDB == nil {
		return nil, ErrStreamRecoveryStoreUnavailable
	}
	keyPath := strings.TrimSpace(os.Getenv("STREAM_RECOVERY_KEY_FILE"))
	if keyPath == "" {
		return nil, errors.New("STREAM_RECOVERY_KEY_FILE is not configured")
	}

	streamRecoveryRuntimeCache.Lock()
	defer streamRecoveryRuntimeCache.Unlock()
	if streamRecoveryRuntimeCache.runtime != nil &&
		streamRecoveryRuntimeCache.keyPath == keyPath &&
		streamRecoveryRuntimeCache.runtime.Store.client == common.RDB {
		return streamRecoveryRuntimeCache.runtime, nil
	}

	keyring, err := LoadStreamRecoveryKeyring(keyPath)
	if err != nil {
		return nil, err
	}
	store, err := NewStreamRecoveryStore(common.RDB, keyring, StreamRecoveryStoreConfig{
		TTL:                  time.Duration(setting.TTLSeconds) * time.Second,
		MaxEventsPerAttempt:  setting.MaxEventsPerAttempt,
		MaxBytesPerExecution: setting.MaxBytesPerExecution,
	})
	if err != nil {
		return nil, err
	}
	runtime := &StreamRecoveryRuntime{Store: store, Keyring: keyring}
	streamRecoveryRuntimeCache.keyPath = keyPath
	streamRecoveryRuntimeCache.runtime = runtime
	return runtime, nil
}

func BuildStreamRecoveryIdentity(
	keyring *StreamRecoveryKeyring,
	tokenID int,
	requestPath string,
	modelName string,
	zcodeSessionID string,
	queryID string,
	explicitKey string,
	requestBody []byte,
) (StreamRecoveryIdentity, error) {
	if keyring == nil {
		return StreamRecoveryIdentity{}, errors.New("stream recovery keyring is nil")
	}
	requestPath = strings.TrimSpace(requestPath)
	modelName = strings.TrimSpace(modelName)
	queryID = strings.TrimSpace(queryID)
	explicitKey = strings.TrimSpace(explicitKey)
	zcodeSessionID = strings.TrimSpace(zcodeSessionID)
	if tokenID <= 0 || requestPath == "" || modelName == "" {
		return StreamRecoveryIdentity{}, errors.New("stream recovery identity is incomplete")
	}
	identitySource := queryID
	sourceName := "x-query-id"
	if explicitKey != "" {
		identitySource = explicitKey
		sourceName = "idempotency-key"
	}
	if identitySource == "" {
		return StreamRecoveryIdentity{}, errors.New("stream recovery requires x-query-id or an idempotency key")
	}

	digest := sha256.Sum256(requestBody)
	requestDigest := hex.EncodeToString(digest[:])
	dedupeKey, err := keyring.DedupeKey(
		strconv.Itoa(tokenID),
		requestPath,
		modelName,
		zcodeSessionID,
		sourceName,
		identitySource,
	)
	if err != nil {
		return StreamRecoveryIdentity{}, fmt.Errorf("build stream recovery dedupe key: %w", err)
	}
	legacyDedupeKey, err := keyring.DedupeKey(
		strconv.Itoa(tokenID),
		requestPath,
		modelName,
		zcodeSessionID,
		identitySource,
		requestDigest,
	)
	if err != nil {
		return StreamRecoveryIdentity{}, fmt.Errorf("build legacy stream recovery dedupe key: %w", err)
	}
	return StreamRecoveryIdentity{
		DedupeKey:       dedupeKey,
		LegacyDedupeKey: legacyDedupeKey,
		RequestDigest:   requestDigest,
		QueryIDSource:   sourceName,
	}, nil
}

func ResetStreamRecoveryRuntimeForTest() {
	streamRecoveryRuntimeCache.Lock()
	defer streamRecoveryRuntimeCache.Unlock()
	streamRecoveryRuntimeCache.keyPath = ""
	streamRecoveryRuntimeCache.runtime = nil
}
