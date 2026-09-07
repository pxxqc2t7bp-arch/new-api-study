package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	batchDispatchBatchSize = 4
	batchDispatchLease     = 20 * time.Minute
)

type batchDispatchHandler struct{}

type batchDispatchSummary struct {
	Found     int `json:"found"`
	Claimed   int `json:"claimed"`
	Completed int `json:"completed"`
	Cancelled int `json:"cancelled"`
	Failed    int `json:"failed"`
}

func (batchDispatchHandler) Type() string {
	return model.SystemTaskTypeBatchDispatch
}

func (batchDispatchHandler) Enabled() bool {
	return model.HasDispatchableBatches(common.GetTimestamp())
}

func (batchDispatchHandler) Interval() time.Duration {
	return 5 * time.Second
}

func (batchDispatchHandler) NewPayload() any {
	return nil
}

func (batchDispatchHandler) Run(ctx context.Context, systemTask *model.SystemTask, runnerID string) {
	summary := batchDispatchSummary{}
	batches, err := model.FindDispatchableBatches(common.GetTimestamp(), batchDispatchBatchSize)
	if err != nil {
		finishSystemTaskHandler(systemTask, runnerID, model.SystemTaskStatusFailed, summary, err)
		return
	}
	summary.Found = len(batches)
	for _, candidate := range batches {
		if ctx.Err() != nil {
			finishSystemTaskHandler(systemTask, runnerID, model.SystemTaskStatusFailed, summary, ctx.Err())
			return
		}
		owner := deferredDispatchOwner(runnerID, candidate.ID)
		now := common.GetTimestamp()
		batch, claimed, claimErr := model.ClaimBatch(
			candidate.ID,
			owner,
			now,
			now+int64(batchDispatchLease.Seconds()),
		)
		if claimErr != nil {
			logger.LogWarn(ctx, fmt.Sprintf("claim batch %s failed: %v", candidate.BatchID, claimErr))
			continue
		}
		if !claimed {
			continue
		}
		summary.Claimed++
		status, runErr := runClaimedBatch(ctx, batch, owner)
		switch status {
		case model.BatchStatusCompleted:
			summary.Completed++
		case model.BatchStatusCancelled:
			summary.Cancelled++
		case model.BatchStatusFailed:
			summary.Failed++
		}
		if runErr != nil {
			logger.LogWarn(ctx, fmt.Sprintf("run batch %s failed: %v", batch.BatchID, runErr))
			if releaseErr := model.ReleaseBatchLease(batch.ID, owner); releaseErr != nil {
				logger.LogWarn(ctx, fmt.Sprintf("release batch %s lease failed: %v", batch.BatchID, releaseErr))
			}
		}
	}
	finishSystemTaskHandler(systemTask, runnerID, model.SystemTaskStatusSucceeded, summary, nil)
}

func runClaimedBatch(ctx context.Context, batch *model.Batch, owner string) (model.BatchStatus, error) {
	if batch == nil {
		return model.BatchStatusFailed, errors.New("batch is required")
	}
	if batch.Status == model.BatchStatusCancelling {
		return cancelClaimedBatch(ctx, batch, owner)
	}
	if batch.ExpiresAt > 0 && batch.ExpiresAt <= common.GetTimestamp() {
		_, err := model.CancelPendingBatchItems(batch.ID, common.GetTimestamp())
		if err != nil {
			return model.BatchStatusFailed, err
		}
		_, err = model.FinishBatch(batch, owner, model.BatchStatusExpired, nil, nil, nil, common.GetTimestamp())
		return model.BatchStatusExpired, err
	}
	if batch.Status != model.BatchStatusFinalizing {
		concurrency := constant.BatchWorkerConcurrency
		if concurrency <= 0 {
			concurrency = 4
		}
		for {
			items, err := model.ListPendingBatchItems(batch.ID, concurrency)
			if err != nil {
				return model.BatchStatusFailed, err
			}
			if len(items) == 0 {
				break
			}
			current, err := model.GetBatchByID(batch.ID)
			if err != nil {
				return model.BatchStatusFailed, err
			}
			if current.Status == model.BatchStatusCancelling {
				return cancelClaimedBatch(ctx, current, owner)
			}
			if err = executeBatchItemChunk(ctx, batch, items); err != nil {
				return model.BatchStatusFailed, err
			}
			now := common.GetTimestamp()
			if err = model.RenewBatchLease(batch.ID, owner, now, now+int64(batchDispatchLease.Seconds())); err != nil {
				return model.BatchStatusFailed, err
			}
		}
		current, err := model.GetBatchByID(batch.ID)
		if err != nil {
			return model.BatchStatusFailed, err
		}
		if current.Status == model.BatchStatusCancelling {
			return cancelClaimedBatch(ctx, current, owner)
		}
		won, err := model.MarkBatchFinalizing(batch, owner, common.GetTimestamp())
		if err != nil {
			return model.BatchStatusFailed, err
		}
		if !won {
			return model.BatchStatusFailed, errors.New("batch lease lost before finalization")
		}
		batch.Status = model.BatchStatusFinalizing
	}
	return finalizeClaimedBatch(ctx, batch, owner, model.BatchStatusCompleted)
}

func executeBatchItemChunk(ctx context.Context, batch *model.Batch, items []*model.BatchItem) error {
	claimed := make([]*model.BatchItem, 0, len(items))
	for _, item := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		won, err := model.ClaimBatchItem(item.ID, common.GetTimestamp())
		if err != nil {
			return err
		}
		if won {
			claimed = append(claimed, item)
		}
	}
	if len(claimed) == 0 {
		return nil
	}

	var wait sync.WaitGroup
	errorsFound := make(chan error, len(claimed))
	for _, item := range claimed {
		item := item
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, itemError, success := executeBatchItem(ctx, batch, item)
			status := model.BatchItemStatusFailed
			if success {
				status = model.BatchItemStatusCompleted
			}
			if _, err := model.FinishBatchItem(
				item.ID,
				status,
				response,
				itemError,
				common.GetTimestamp(),
			); err != nil {
				errorsFound <- err
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			return err
		}
	}
	return nil
}

func cancelClaimedBatch(ctx context.Context, batch *model.Batch, owner string) (model.BatchStatus, error) {
	if _, err := model.CancelPendingBatchItems(batch.ID, common.GetTimestamp()); err != nil {
		return model.BatchStatusFailed, err
	}
	return finalizeClaimedBatch(ctx, batch, owner, model.BatchStatusCancelled)
}

func finalizeClaimedBatch(
	ctx context.Context,
	batch *model.Batch,
	owner string,
	status model.BatchStatus,
) (model.BatchStatus, error) {
	items, err := model.ListBatchItems(batch.ID)
	if err != nil {
		return model.BatchStatusFailed, err
	}
	output, failures, err := encodeBatchResultFiles(batch, items)
	if err != nil {
		return model.BatchStatusFailed, err
	}
	var outputFileID *string
	if output.Len() > 0 {
		file, createErr := service.CreateBatchAPIFile(
			ctx,
			batch.UserID,
			batch.TokenID,
			model.APIFilePurposeBatchOutput,
			batch.BatchID+"-output.jsonl",
			&output,
		)
		if createErr != nil {
			return model.BatchStatusFailed, createErr
		}
		outputFileID = &file.FileID
	}
	var errorFileID *string
	if failures.Len() > 0 {
		file, createErr := service.CreateBatchAPIFile(
			ctx,
			batch.UserID,
			batch.TokenID,
			model.APIFilePurposeBatchError,
			batch.BatchID+"-errors.jsonl",
			&failures,
		)
		if createErr != nil {
			return model.BatchStatusFailed, createErr
		}
		errorFileID = &file.FileID
	}
	won, err := model.FinishBatch(
		batch,
		owner,
		status,
		outputFileID,
		errorFileID,
		nil,
		common.GetTimestamp(),
	)
	if err != nil {
		return model.BatchStatusFailed, err
	}
	if !won {
		return model.BatchStatusFailed, errors.New("batch lease lost during finalization")
	}
	return status, nil
}

func executeBatchItem(
	ctx context.Context,
	batch *model.Batch,
	item *model.BatchItem,
) (json.RawMessage, json.RawMessage, bool) {
	token, err := model.GetTokenById(batch.TokenID)
	if err != nil || token == nil || token.UserId != batch.UserID {
		return nil, batchItemErrorLine(item, "authentication_error", "batch token is unavailable"), false
	}
	timeoutSeconds := constant.BatchRequestTimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 900
	}
	requestContext, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	engine := gin.New()
	engine.Use(gin.Recovery(), middleware.BodyStorageCleanup())
	registerBatchExecutionRoute(engine, item.URL)
	request := httptest.NewRequest(item.Method, item.URL, bytes.NewReader(item.Body)).WithContext(requestContext)
	if ip := strings.TrimSpace(batch.ClientIP); ip != "" {
		request.RemoteAddr = net.JoinHostPort(ip, "0")
	}
	key := token.Key
	if !strings.HasPrefix(key, "sk-") {
		key = "sk-" + key
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", batch.BatchID+":"+item.CustomID)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	statusCode := recorder.Code
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	requestID := strings.TrimSpace(recorder.Header().Get("X-Request-Id"))
	var body any
	if len(recorder.Body.Bytes()) > 0 && common.Unmarshal(recorder.Body.Bytes(), &body) == nil {
		// Preserve the endpoint's JSON response without double encoding.
	} else {
		body = recorder.Body.String()
	}
	if statusCode >= 200 && statusCode < 300 {
		line := map[string]any{
			"id":        "batch_req_" + common.GetRandomString(24),
			"custom_id": item.CustomID,
			"response": map[string]any{
				"status_code": statusCode,
				"request_id":  requestID,
				"body":        body,
			},
			"error": nil,
		}
		encoded, _ := common.Marshal(line)
		return json.RawMessage(encoded), nil, true
	}
	message := fmt.Sprintf("request failed with status %d", statusCode)
	if bodyMap, ok := body.(map[string]any); ok {
		if errorMap, ok := bodyMap["error"].(map[string]any); ok {
			if value, ok := errorMap["message"].(string); ok && strings.TrimSpace(value) != "" {
				message = value
			}
		}
	}
	return nil, batchItemErrorLine(item, "request_failed", message), false
}

func registerBatchExecutionRoute(engine *gin.Engine, endpoint string) {
	authHandlers := []gin.HandlerFunc{middleware.TokenAuth()}
	switch endpoint {
	case "/v1/responses":
		engine.POST(
			endpoint,
			append(authHandlers,
				middleware.ModelRequestRateLimit(),
				middleware.PinTaskPluginEndpoint(),
				middleware.PrepareTaskPluginEndpoint(),
				middleware.Distribute(),
				func(c *gin.Context) {
					RelayTaskPluginEndpoint(c, func(c *gin.Context) {
						Relay(c, relaytypes.RelayFormatOpenAIResponses)
					})
				},
			)...,
		)
	case "/v1/videos":
		engine.POST(
			endpoint,
			append(authHandlers,
				middleware.PinTaskPluginEndpoint(),
				middleware.TaskPluginEndpointOnly(middleware.ModelRequestRateLimit()),
				middleware.PrepareTaskPluginEndpoint(),
				middleware.Distribute(),
				func(c *gin.Context) { RelayTaskPluginEndpoint(c, RelayTask) },
			)...,
		)
	case "/v1/embeddings":
		engine.POST(endpoint, append(authHandlers, middleware.ModelRequestRateLimit(), middleware.Distribute(), func(c *gin.Context) {
			Relay(c, relaytypes.RelayFormatEmbedding)
		})...)
	case "/v1/images/generations":
		engine.POST(endpoint, append(authHandlers, middleware.ModelRequestRateLimit(), middleware.Distribute(), func(c *gin.Context) {
			Relay(c, relaytypes.RelayFormatOpenAIImage)
		})...)
	default:
		engine.POST(endpoint, append(authHandlers, middleware.ModelRequestRateLimit(), middleware.Distribute(), func(c *gin.Context) {
			Relay(c, relaytypes.RelayFormatOpenAI)
		})...)
	}
}

func batchItemErrorLine(item *model.BatchItem, code, message string) json.RawMessage {
	line := map[string]any{
		"id":        "batch_req_" + common.GetRandomString(24),
		"custom_id": item.CustomID,
		"response":  nil,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	encoded, _ := common.Marshal(line)
	return json.RawMessage(encoded)
}

func encodeBatchResultFiles(batch *model.Batch, items []model.BatchItem) (bytes.Buffer, bytes.Buffer, error) {
	var output bytes.Buffer
	var failures bytes.Buffer
	for _, item := range items {
		var line json.RawMessage
		switch item.Status {
		case model.BatchItemStatusCompleted:
			line = item.Response
		case model.BatchItemStatusFailed:
			line = item.Error
		case model.BatchItemStatusCancelled:
			line = batchItemErrorLine(&item, "batch_cancelled", "request was cancelled before execution")
		default:
			return output, failures, fmt.Errorf("batch %s contains non-terminal item %s", batch.BatchID, item.CustomID)
		}
		if len(line) == 0 {
			return output, failures, fmt.Errorf("batch %s item %s has no result", batch.BatchID, item.CustomID)
		}
		target := &output
		if item.Status != model.BatchItemStatusCompleted {
			target = &failures
		}
		if _, err := target.Write(line); err != nil {
			return output, failures, err
		}
		if err := target.WriteByte('\n'); err != nil {
			return output, failures, err
		}
	}
	return output, failures, nil
}
