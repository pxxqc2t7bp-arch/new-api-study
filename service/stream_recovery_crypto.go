package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

const streamRecoveryEnvelopeVersion = 1

type streamRecoveryKey struct {
	id  string
	key []byte
}

type StreamRecoveryKeyring struct {
	keys  []streamRecoveryKey
	byID  map[string][]byte
	mutex sync.RWMutex
}

type streamRecoveryEnvelope struct {
	Version    int    `json:"version"`
	KeyID      string `json:"key_id"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func LoadStreamRecoveryKeyring(path string) (*StreamRecoveryKeyring, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("stream recovery key file is not configured")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat stream recovery key file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("stream recovery key file permissions must be 0600 or stricter")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read stream recovery key file: %w", err)
	}
	keyring := &StreamRecoveryKeyring{byID: map[string][]byte{}}
	for lineNumber, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keyID, encoded, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(keyID) == "" {
			return nil, fmt.Errorf("invalid stream recovery key at line %d", lineNumber+1)
		}
		key, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if decodeErr != nil {
			return nil, fmt.Errorf("decode stream recovery key at line %d: %w", lineNumber+1, decodeErr)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("stream recovery key at line %d must decode to 32 bytes", lineNumber+1)
		}
		keyID = strings.TrimSpace(keyID)
		if _, exists := keyring.byID[keyID]; exists {
			return nil, fmt.Errorf("duplicate stream recovery key id %q", keyID)
		}
		keyCopy := append([]byte(nil), key...)
		keyring.keys = append(keyring.keys, streamRecoveryKey{id: keyID, key: keyCopy})
		keyring.byID[keyID] = keyCopy
	}
	if len(keyring.keys) == 0 {
		return nil, errors.New("stream recovery key file contains no keys")
	}
	return keyring, nil
}

func (keyring *StreamRecoveryKeyring) Encrypt(plaintext []byte, aad []byte) ([]byte, error) {
	if keyring == nil {
		return nil, errors.New("stream recovery keyring is nil")
	}
	keyring.mutex.RLock()
	current := keyring.keys[0]
	keyring.mutex.RUnlock()

	aead, err := newStreamRecoveryAEAD(current.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate stream recovery nonce: %w", err)
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	envelope := streamRecoveryEnvelope{
		Version:    streamRecoveryEnvelopeVersion,
		KeyID:      current.id,
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	encoded, err := common.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal stream recovery envelope: %w", err)
	}
	return encoded, nil
}

func (keyring *StreamRecoveryKeyring) Decrypt(encoded []byte, aad []byte) ([]byte, error) {
	if keyring == nil {
		return nil, errors.New("stream recovery keyring is nil")
	}
	var envelope streamRecoveryEnvelope
	if err := common.Unmarshal(encoded, &envelope); err != nil {
		return nil, fmt.Errorf("decode stream recovery envelope: %w", err)
	}
	if envelope.Version != streamRecoveryEnvelopeVersion {
		return nil, fmt.Errorf("unsupported stream recovery envelope version %d", envelope.Version)
	}
	keyring.mutex.RLock()
	key, ok := keyring.byID[envelope.KeyID]
	keyring.mutex.RUnlock()
	if !ok {
		return nil, fmt.Errorf("stream recovery key %q is unavailable", envelope.KeyID)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decode stream recovery nonce: %w", err)
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode stream recovery ciphertext: %w", err)
	}
	aead, err := newStreamRecoveryAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("invalid stream recovery nonce length")
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt stream recovery envelope: %w", err)
	}
	return plaintext, nil
}

func (keyring *StreamRecoveryKeyring) CurrentKeyID() string {
	if keyring == nil {
		return ""
	}
	keyring.mutex.RLock()
	defer keyring.mutex.RUnlock()
	if len(keyring.keys) == 0 {
		return ""
	}
	return keyring.keys[0].id
}

func (keyring *StreamRecoveryKeyring) DedupeKey(parts ...string) (string, error) {
	if keyring == nil {
		return "", errors.New("stream recovery keyring is nil")
	}
	keyring.mutex.RLock()
	if len(keyring.keys) == 0 {
		keyring.mutex.RUnlock()
		return "", errors.New("stream recovery keyring contains no keys")
	}
	key := append([]byte(nil), keyring.keys[len(keyring.keys)-1].key...)
	keyring.mutex.RUnlock()

	mac := hmac.New(sha256.New, key)
	for _, part := range parts {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(part))
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func newStreamRecoveryAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create stream recovery cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create stream recovery AEAD: %w", err)
	}
	return aead, nil
}
