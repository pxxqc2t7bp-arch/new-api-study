package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersistBatchFileIsPrivateAtomicAndHashed(t *testing.T) {
	previousRoot := constant.BatchStorageDir
	constant.BatchStorageDir = t.TempDir()
	t.Cleanup(func() { constant.BatchStorageDir = previousRoot })

	content := "{\"custom_id\":\"one\"}\n"
	storageKey, size, digest, err := PersistBatchFile(
		context.Background(),
		7,
		"file-test",
		strings.NewReader(content),
		1024,
	)
	require.NoError(t, err)
	assert.NotContains(t, storageKey, "..")
	assert.Equal(t, int64(len(content)), size)
	expected := sha256.Sum256([]byte(content))
	assert.Equal(t, hex.EncodeToString(expected[:]), digest)

	file, err := OpenBatchFile(storageKey)
	require.NoError(t, err)
	defer file.Close()
	stored, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, content, string(stored))
	info, err := file.Stat()
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestPersistBatchFileRejectsLimitAndTraversal(t *testing.T) {
	previousRoot := constant.BatchStorageDir
	constant.BatchStorageDir = t.TempDir()
	t.Cleanup(func() { constant.BatchStorageDir = previousRoot })

	_, _, _, err := PersistBatchFile(
		context.Background(),
		7,
		"file-test",
		strings.NewReader("too large"),
		3,
	)
	assert.ErrorIs(t, err, ErrBatchFileTooLarge)

	_, err = OpenBatchFile("../outside")
	require.Error(t, err)
}
