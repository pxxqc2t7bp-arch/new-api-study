package controller

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const maxBatchJSONLLineBytes = 16 << 20

var supportedBatchEndpoints = map[string]struct{}{
	"/v1/chat/completions":   {},
	"/v1/responses":          {},
	"/v1/embeddings":         {},
	"/v1/images/generations": {},
	"/v1/videos":             {},
}

type createBatchRequest struct {
	InputFileID      string         `json:"input_file_id"`
	Endpoint         string         `json:"endpoint"`
	CompletionWindow string         `json:"completion_window"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

type batchJSONLRequest struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

func CreateBatch(c *gin.Context) {
	var request createBatchRequest
	if err := common.UnmarshalBodyReusable(c, &request); err != nil {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "invalid batch request")
		return
	}
	request.InputFileID = strings.TrimSpace(request.InputFileID)
	request.Endpoint = normalizeBatchEndpoint(request.Endpoint)
	if request.CompletionWindow == "" {
		request.CompletionWindow = "24h"
	}
	if request.InputFileID == "" {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "input_file_id is required")
		return
	}
	if _, ok := supportedBatchEndpoints[request.Endpoint]; !ok {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "unsupported batch endpoint")
		return
	}
	if request.CompletionWindow != "24h" {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", `completion_window must be "24h"`)
		return
	}
	if len(request.Metadata) > 16 {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "metadata supports at most 16 keys")
		return
	}

	userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	tokenID := common.GetContextKeyInt(c, constant.ContextKeyTokenId)
	inputFile, exists, err := model.GetOwnedAPIFile(userID, tokenID, request.InputFileID)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to load input file")
		return
	}
	if !exists || inputFile.Purpose != model.APIFilePurposeBatch {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "input file not found or has the wrong purpose")
		return
	}
	content, err := service.OpenBatchFile(inputFile.StorageKey)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "file_storage_error", "failed to open input file")
		return
	}
	inputHasher := sha256.New()
	items, err := parseBatchJSONL(io.TeeReader(content, inputHasher), request.Endpoint)
	content.Close()
	if err != nil {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_batch_file", err.Error())
		return
	}
	if digest := hex.EncodeToString(inputHasher.Sum(nil)); digest != inputFile.SHA256 {
		writeOpenAIResourceError(c, http.StatusConflict, "file_integrity_error", "input file integrity check failed")
		return
	}

	metadata, err := common.Marshal(request.Metadata)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "metadata is invalid")
		return
	}
	requestHash := batchCreateRequestHash(request, inputFile.SHA256, metadata)
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(idempotencyKey) > 128 {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "Idempotency-Key is too long")
		return
	}
	var idempotencyScope *string
	if idempotencyKey != "" {
		scope := fmt.Sprintf("%d:%d:%s", userID, tokenID, idempotencyKey)
		idempotencyScope = &scope
	}
	now := time.Now()
	batch := &model.Batch{
		UserID:           userID,
		TokenID:          tokenID,
		ClientIP:         c.ClientIP(),
		InputFileID:      inputFile.FileID,
		Endpoint:         request.Endpoint,
		CompletionWindow: request.CompletionWindow,
		Metadata:         json.RawMessage(metadata),
		IdempotencyScope: idempotencyScope,
		RequestHash:      requestHash,
		ExpiresAt:        now.Add(24 * time.Hour).Unix(),
	}
	batch, created, err := model.CreateBatchWithItems(batch, items)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to create batch")
		return
	}
	if !created {
		if batch.RequestHash != requestHash {
			writeOpenAIResourceError(c, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used with a different request")
			return
		}
		batch, _, err = model.GetOwnedBatch(userID, tokenID, batch.BatchID)
		if err != nil {
			writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to retrieve existing batch")
			return
		}
	} else {
		batch.RequestCounts.Total = len(items)
	}
	if _, _, enqueueErr := service.EnqueueSystemTask(model.SystemTaskTypeBatchDispatch, nil); enqueueErr != nil {
		logger.LogWarn(c, "enqueue batch dispatcher failed: "+enqueueErr.Error())
	}
	c.JSON(http.StatusOK, batch)
}

func RetrieveBatch(c *gin.Context) {
	batch, exists, err := ownedBatch(c)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to retrieve batch")
		return
	}
	if !exists {
		writeOpenAIResourceError(c, http.StatusNotFound, "not_found", "batch not found")
		return
	}
	c.JSON(http.StatusOK, batch)
}

func ListBatches(c *gin.Context) {
	limit, err := parseResourceListLimit(c.Query("limit"))
	if err != nil {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	batches, hasMore, err := model.ListOwnedBatches(
		common.GetContextKeyInt(c, constant.ContextKeyUserId),
		common.GetContextKeyInt(c, constant.ContextKeyTokenId),
		strings.TrimSpace(c.Query("after")),
		limit,
	)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to list batches")
		return
	}
	firstID, lastID := "", ""
	if len(batches) > 0 {
		firstID = batches[0].BatchID
		lastID = batches[len(batches)-1].BatchID
	}
	c.JSON(http.StatusOK, gin.H{
		"object":   "list",
		"data":     batches,
		"has_more": hasMore,
		"first_id": firstID,
		"last_id":  lastID,
	})
}

func CancelBatch(c *gin.Context) {
	batch, exists, err := model.RequestBatchCancellation(
		common.GetContextKeyInt(c, constant.ContextKeyUserId),
		common.GetContextKeyInt(c, constant.ContextKeyTokenId),
		c.Param("id"),
		common.GetTimestamp(),
	)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to cancel batch")
		return
	}
	if !exists {
		writeOpenAIResourceError(c, http.StatusNotFound, "not_found", "batch not found")
		return
	}
	if _, _, enqueueErr := service.EnqueueSystemTask(model.SystemTaskTypeBatchDispatch, nil); enqueueErr != nil {
		logger.LogWarn(c, "enqueue batch cancellation failed: "+enqueueErr.Error())
	}
	c.JSON(http.StatusOK, batch)
}

func ownedBatch(c *gin.Context) (*model.Batch, bool, error) {
	return model.GetOwnedBatch(
		common.GetContextKeyInt(c, constant.ContextKeyUserId),
		common.GetContextKeyInt(c, constant.ContextKeyTokenId),
		c.Param("id"),
	)
}

func parseBatchJSONL(reader io.Reader, endpoint string) ([]model.BatchItem, error) {
	maxLines := constant.BatchMaxLines
	if maxLines <= 0 {
		maxLines = 50_000
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxBatchJSONLLineBytes)
	items := make([]model.BatchItem, 0)
	customIDs := make(map[string]struct{})
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if len(items) >= maxLines {
			return nil, fmt.Errorf("batch file exceeds %d requests", maxLines)
		}
		var request batchJSONLRequest
		var fields map[string]json.RawMessage
		if err := common.Unmarshal(line, &fields); err != nil {
			return nil, fmt.Errorf("line %d is invalid JSON: %w", lineNumber, err)
		}
		for key := range fields {
			switch key {
			case "custom_id", "method", "url", "body":
			default:
				return nil, fmt.Errorf("line %d contains unsupported field %q", lineNumber, key)
			}
		}
		if err := common.Unmarshal(line, &request); err != nil {
			return nil, fmt.Errorf("line %d is invalid JSON: %w", lineNumber, err)
		}
		request.CustomID = strings.TrimSpace(request.CustomID)
		if request.CustomID == "" || len(request.CustomID) > 128 {
			return nil, fmt.Errorf("line %d has an invalid custom_id", lineNumber)
		}
		if _, duplicate := customIDs[request.CustomID]; duplicate {
			return nil, fmt.Errorf("line %d has duplicate custom_id %q", lineNumber, request.CustomID)
		}
		customIDs[request.CustomID] = struct{}{}
		if strings.ToUpper(strings.TrimSpace(request.Method)) != http.MethodPost {
			return nil, fmt.Errorf("line %d method must be POST", lineNumber)
		}
		if normalizeBatchEndpoint(request.URL) != endpoint {
			return nil, fmt.Errorf("line %d URL must match batch endpoint %s", lineNumber, endpoint)
		}
		var body map[string]any
		if len(request.Body) == 0 || common.Unmarshal(request.Body, &body) != nil {
			return nil, fmt.Errorf("line %d body must be a JSON object", lineNumber)
		}
		modelName, _ := body["model"].(string)
		if strings.TrimSpace(modelName) == "" {
			return nil, fmt.Errorf("line %d body.model is required", lineNumber)
		}
		if stream, exists := body["stream"]; exists && stream != false {
			return nil, fmt.Errorf("line %d streaming requests are not supported in batches", lineNumber)
		}
		canonicalBody, err := common.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("line %d body is invalid", lineNumber)
		}
		items = append(items, model.BatchItem{
			LineNumber: lineNumber,
			CustomID:   request.CustomID,
			Method:     http.MethodPost,
			URL:        endpoint,
			Body:       json.RawMessage(canonicalBody),
		})
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) || strings.Contains(err.Error(), "token too long") {
			return nil, fmt.Errorf("a JSONL line exceeds %d bytes", maxBatchJSONLLineBytes)
		}
		return nil, err
	}
	if len(items) == 0 {
		return nil, errors.New("batch file contains no requests")
	}
	return items, nil
}

func normalizeBatchEndpoint(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if path == "" {
		path = "/"
	}
	return path
}

func batchCreateRequestHash(request createBatchRequest, inputHash string, metadata []byte) string {
	payload := strings.Join([]string{
		request.InputFileID,
		inputHash,
		request.Endpoint,
		request.CompletionWindow,
		string(metadata),
	}, "\x00")
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}
