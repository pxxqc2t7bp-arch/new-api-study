package service

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeRecoveryKeyFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stream-recovery.keys")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600))
	return path
}

func encodedRecoveryKey(fill byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(fill), 32)))
}

func TestStreamRecoveryKeyringEncryptDecrypt(t *testing.T) {
	path := writeRecoveryKeyFile(t, "current:"+encodedRecoveryKey('a'))
	keyring, err := LoadStreamRecoveryKeyring(path)
	require.NoError(t, err)

	aad := []byte("stream-id:user-id:token-id:digest")
	encrypted, err := keyring.Encrypt([]byte("sensitive request"), aad)
	require.NoError(t, err)
	assert.NotContains(t, string(encrypted), "sensitive request")

	plaintext, err := keyring.Decrypt(encrypted, aad)
	require.NoError(t, err)
	assert.Equal(t, []byte("sensitive request"), plaintext)

	_, err = keyring.Decrypt(encrypted, []byte("wrong-aad"))
	require.Error(t, err)
}

func TestStreamRecoveryKeyringReadsPreviousKey(t *testing.T) {
	oldPath := writeRecoveryKeyFile(t, "old:"+encodedRecoveryKey('o'))
	oldKeyring, err := LoadStreamRecoveryKeyring(oldPath)
	require.NoError(t, err)
	encrypted, err := oldKeyring.Encrypt([]byte("payload"), []byte("aad"))
	require.NoError(t, err)

	rotatedPath := writeRecoveryKeyFile(
		t,
		"current:"+encodedRecoveryKey('n'),
		"old:"+encodedRecoveryKey('o'),
	)
	rotated, err := LoadStreamRecoveryKeyring(rotatedPath)
	require.NoError(t, err)
	assert.Equal(t, "current", rotated.CurrentKeyID())

	plaintext, err := rotated.Decrypt(encrypted, []byte("aad"))
	require.NoError(t, err)
	assert.Equal(t, []byte("payload"), plaintext)

	oldDedupe, err := oldKeyring.DedupeKey("token", "query", "body")
	require.NoError(t, err)
	rotatedDedupe, err := rotated.DedupeKey("token", "query", "body")
	require.NoError(t, err)
	assert.Equal(t, oldDedupe, rotatedDedupe)
}

func TestStreamRecoveryKeyringRejectsWeakFilePermissions(t *testing.T) {
	path := writeRecoveryKeyFile(t, "current:"+encodedRecoveryKey('a'))
	require.NoError(t, os.Chmod(path, 0o644))

	_, err := LoadStreamRecoveryKeyring(path)
	require.ErrorContains(t, err, "permissions")
}

func TestStreamRecoveryKeyringRejectsInvalidKeyLength(t *testing.T) {
	path := writeRecoveryKeyFile(
		t,
		"current:"+base64.StdEncoding.EncodeToString([]byte("too-short")),
	)

	_, err := LoadStreamRecoveryKeyring(path)
	require.ErrorContains(t, err, "32 bytes")
}

func TestBuildStreamRecoveryIdentityUsesStableQueryAndBody(t *testing.T) {
	keyring, err := LoadStreamRecoveryKeyring(
		writeRecoveryKeyFile(t, "current:"+encodedRecoveryKey('a')),
	)
	require.NoError(t, err)

	first, err := BuildStreamRecoveryIdentity(
		keyring,
		22,
		"/v1/responses",
		"glm-5.3",
		"",
		"query-stable",
		"",
		[]byte(`{"model":"glm-5.3","input":"hello"}`),
	)
	require.NoError(t, err)
	second, err := BuildStreamRecoveryIdentity(
		keyring,
		22,
		"/v1/responses",
		"glm-5.3",
		"",
		"query-stable",
		"",
		[]byte(`{"model":"glm-5.3","input":"hello"}`),
	)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	changedBody, err := BuildStreamRecoveryIdentity(
		keyring,
		22,
		"/v1/responses",
		"glm-5.3",
		"",
		"query-stable",
		"",
		[]byte(`{"model":"glm-5.3","input":"different"}`),
	)
	require.NoError(t, err)
	assert.NotEqual(t, first.DedupeKey, changedBody.DedupeKey)
}

func TestBuildStreamRecoveryIdentityRequiresStableClientIdentity(t *testing.T) {
	keyring, err := LoadStreamRecoveryKeyring(
		writeRecoveryKeyFile(t, "current:"+encodedRecoveryKey('a')),
	)
	require.NoError(t, err)

	_, err = BuildStreamRecoveryIdentity(
		keyring,
		22,
		"/v1/messages",
		"glm-5.3",
		"",
		"",
		"",
		[]byte(`{"stream":true}`),
	)
	require.ErrorContains(t, err, "x-query-id")
}
