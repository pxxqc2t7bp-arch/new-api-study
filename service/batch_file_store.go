package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

var ErrBatchFileTooLarge = errors.New("batch file exceeds the configured size limit")

func BatchStorageRoot() string {
	root := strings.TrimSpace(constant.BatchStorageDir)
	if root == "" {
		root = "/data/new-api/batches"
	}
	return filepath.Clean(root)
}

func BatchMaxFileBytes() int64 {
	maxMB := constant.BatchMaxFileMB
	if maxMB <= 0 {
		maxMB = 200
	}
	return int64(maxMB) << 20
}

func CreateBatchAPIFile(
	ctx context.Context,
	userID, tokenID int,
	purpose, filename string,
	reader io.Reader,
) (*model.APIFile, error) {
	fileID, err := model.GenerateAPIFileID()
	if err != nil {
		return nil, err
	}
	storageKey, size, digest, err := PersistBatchFile(ctx, userID, fileID, reader, BatchMaxFileBytes())
	if err != nil {
		return nil, err
	}
	retentionHours := constant.BatchRetentionHours
	if retentionHours <= 0 {
		retentionHours = 24 * 30
	}
	file := &model.APIFile{
		FileID:     fileID,
		UserID:     userID,
		TokenID:    tokenID,
		Purpose:    purpose,
		Filename:   filename,
		Bytes:      size,
		SHA256:     digest,
		StorageKey: storageKey,
		ExpiresAt:  time.Now().Add(time.Duration(retentionHours) * time.Hour).Unix(),
	}
	if err = model.CreateAPIFile(file); err != nil {
		_ = DeleteBatchFile(storageKey)
		return nil, err
	}
	return file, nil
}

// PersistBatchFile writes a private file atomically and returns its opaque
// storage key, byte count, and SHA-256 digest.
func PersistBatchFile(
	ctx context.Context,
	userID int,
	fileID string,
	reader io.Reader,
	maxBytes int64,
) (string, int64, string, error) {
	if userID <= 0 || strings.TrimSpace(fileID) == "" || reader == nil {
		return "", 0, "", errors.New("invalid batch file input")
	}
	if maxBytes <= 0 {
		maxBytes = BatchMaxFileBytes()
	}
	random, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return "", 0, "", err
	}
	storageKey := filepath.Join(strconv.Itoa(userID), fileID+"-"+random+".jsonl")
	path, err := batchStoragePath(storageKey)
	if err != nil {
		return "", 0, "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", 0, "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return "", 0, "", err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0o600); err != nil {
		temp.Close()
		return "", 0, "", err
	}

	hasher := sha256.New()
	written, copyErr := copyBatchFile(ctx, io.MultiWriter(temp, hasher), reader, maxBytes)
	if copyErr == nil {
		copyErr = temp.Sync()
	}
	closeErr := temp.Close()
	if copyErr != nil {
		return "", 0, "", copyErr
	}
	if closeErr != nil {
		return "", 0, "", closeErr
	}
	if err = os.Rename(tempPath, path); err != nil {
		return "", 0, "", err
	}
	return storageKey, written, hex.EncodeToString(hasher.Sum(nil)), nil
}

func copyBatchFile(ctx context.Context, destination io.Writer, source io.Reader, maxBytes int64) (int64, error) {
	buffer := make([]byte, 64*1024)
	reader := io.LimitReader(source, maxBytes+1)
	var written int64
	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return written, err
			}
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if written+int64(count) > maxBytes {
				return written, ErrBatchFileTooLarge
			}
			n, writeErr := destination.Write(buffer[:count])
			written += int64(n)
			if writeErr != nil {
				return written, writeErr
			}
			if n != count {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func OpenBatchFile(storageKey string) (*os.File, error) {
	path, err := batchStoragePath(storageKey)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

func DeleteBatchFile(storageKey string) error {
	path, err := batchStoragePath(storageKey)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func batchStoragePath(storageKey string) (string, error) {
	storageKey = strings.TrimSpace(storageKey)
	if storageKey == "" || filepath.IsAbs(storageKey) {
		return "", errors.New("invalid batch storage key")
	}
	root := BatchStorageRoot()
	path := filepath.Clean(filepath.Join(root, storageKey))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return "", fmt.Errorf("invalid batch storage key")
	}
	return path, nil
}
