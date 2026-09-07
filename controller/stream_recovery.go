package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

type streamRecoveryRequestProbe struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type streamRecoveryRelay func(*gin.Context, types.RelayFormat)

func getStreamRecoveryWriter(c *gin.Context) *service.StreamRecoveryWriter {
	if c == nil {
		return nil
	}
	value, exists := common.GetContextKey(c, constant.ContextKeyStreamRecoveryBroker)
	if !exists {
		return nil
	}
	writer, _ := value.(*service.StreamRecoveryWriter)
	return writer
}

func handleStreamRecoveryRelay(
	c *gin.Context,
	relayFormat types.RelayFormat,
	run streamRecoveryRelay,
) bool {
	if !common.GetContextKeyBool(c, constant.ContextKeyTokenStreamRecovery) {
		return false
	}
	setting := operation_setting.GetStreamRecoverySetting()
	if !setting.Enabled {
		return false
	}
	if relayFormat != types.RelayFormatClaude &&
		relayFormat != types.RelayFormatOpenAIResponses {
		return false
	}

	bodyStorage, err := common.GetBodyStorage(c)
	if err != nil {
		writeStreamRecoveryError(c, http.StatusBadRequest, "stream_recovery_request_body", err)
		return true
	}
	body, err := bodyStorage.Bytes()
	if err != nil {
		writeStreamRecoveryError(c, http.StatusBadRequest, "stream_recovery_request_body", err)
		return true
	}
	var probe streamRecoveryRequestProbe
	if err = common.Unmarshal(body, &probe); err != nil {
		writeStreamRecoveryError(c, http.StatusBadRequest, "stream_recovery_invalid_json", err)
		return true
	}
	if !probe.Stream {
		return false
	}
	modelName := c.GetString(string(constant.ContextKeyOriginalModel))
	if modelName == "" {
		modelName = probe.Model
	}
	if !operation_setting.IsStreamRecoveryModelAllowed(modelName) {
		return false
	}

	runtime, err := service.GetStreamRecoveryRuntime()
	if err != nil {
		writeStreamRecoveryError(c, http.StatusServiceUnavailable, "stream_recovery_unavailable", err)
		return true
	}
	tokenID := c.GetInt(string(constant.ContextKeyTokenId))
	identity, err := service.BuildStreamRecoveryIdentity(
		runtime.Keyring,
		tokenID,
		c.Request.URL.Path,
		modelName,
		c.GetHeader("x-zcode-session-id"),
		c.GetHeader("x-query-id"),
		c.GetHeader(common.StreamRecoveryIdempotencyHeader),
		body,
	)
	if err != nil {
		writeStreamRecoveryError(c, http.StatusBadRequest, "stream_recovery_identity", err)
		return true
	}

	expiresAt := common.GetTimestamp() + int64(setting.TTLSeconds)
	execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		DedupeKey:     identity.DedupeKey,
		UserID:        c.GetInt(string(constant.ContextKeyUserId)),
		TokenID:       tokenID,
		ModelName:     modelName,
		RelayFormat:   string(relayFormat),
		RequestPath:   c.Request.URL.Path,
		RequestDigest: identity.RequestDigest,
		ExpiresAt:     expiresAt,
	})
	if err != nil {
		writeStreamRecoveryError(c, http.StatusInternalServerError, "stream_recovery_create", err)
		return true
	}
	if execution.UserID != c.GetInt(string(constant.ContextKeyUserId)) ||
		execution.TokenID != tokenID ||
		execution.RequestDigest != identity.RequestDigest {
		writeStreamRecoveryError(c, http.StatusConflict, "stream_recovery_identity_conflict", errors.New("stream identity conflict"))
		return true
	}

	launched, err := ensureStreamRecoveryWorker(
		c,
		relayFormat,
		run,
		runtime,
		execution,
		created,
		body,
		setting,
	)
	if err != nil {
		writeStreamRecoveryError(c, http.StatusServiceUnavailable, "stream_recovery_worker", err)
		return true
	}
	c.Header(common.StreamRecoveryIDHeader, execution.StreamID)
	c.Header(common.StreamRecoveryReplayedHeader, strconv.FormatBool(!created || !launched))
	subscribeStreamRecovery(c, runtime.Store, execution)
	return true
}

func ensureStreamRecoveryWorker(
	c *gin.Context,
	relayFormat types.RelayFormat,
	run streamRecoveryRelay,
	runtime *service.StreamRecoveryRuntime,
	execution *model.StreamExecution,
	created bool,
	body []byte,
	setting *operation_setting.StreamRecoverySetting,
) (bool, error) {
	now := common.GetTimestamp()
	if !created &&
		execution.Status != model.StreamExecutionPending &&
		execution.LockedUntil >= now {
		return false, nil
	}
	if execution.Status == model.StreamExecutionCompleted ||
		execution.Status == model.StreamExecutionCancelled ||
		execution.Status == model.StreamExecutionFailed {
		return false, nil
	}
	if execution.AttemptCount >= setting.MaxAttempts {
		return false, errors.New("stream recovery attempt budget exhausted")
	}
	if execution.BillingStatus == model.StreamBillingReserving ||
		execution.BillingStatus == model.StreamBillingSettling ||
		execution.BillingStatus == model.StreamBillingRefunding ||
		execution.BillingStatus == model.StreamBillingUncertain {
		return false, errors.New("stream recovery billing state requires operator review")
	}

	runnerKey, err := common.GenerateRandomCharsKey(24)
	if err != nil {
		return false, err
	}
	runnerID := common.NodeName + ":" + runnerKey
	claimed, won, err := model.ClaimStreamExecution(
		execution.StreamID,
		runnerID,
		now,
		now+int64(setting.LeaseSeconds),
	)
	if err != nil || !won {
		return false, err
	}
	attempt := max(1, claimed.AttemptCount+1)
	if err := runtime.Store.SaveRequest(
		context.Background(),
		claimed.StreamID,
		claimed.UserID,
		claimed.TokenID,
		claimed.RequestDigest,
		body,
	); err != nil {
		_ = model.FinishStreamExecution(
			claimed.StreamID,
			runnerID,
			model.StreamExecutionFailed,
			0,
			"failed to persist encrypted request",
		)
		return false, err
	}
	if err := runtime.Store.SetPublicAttempt(context.Background(), claimed.StreamID, attempt); err != nil {
		return false, err
	}
	if err := model.StartStreamExecutionAttempt(
		claimed.StreamID,
		runnerID,
		attempt,
		c.GetInt(string(constant.ContextKeyChannelId)),
		streamRecoveryReason(created),
	); err != nil {
		return false, err
	}

	go runStreamRecoveryWorker(
		c,
		relayFormat,
		run,
		runtime,
		claimed,
		runnerID,
		attempt,
		body,
		setting,
	)
	return true, nil
}

func runStreamRecoveryWorker(
	parent *gin.Context,
	relayFormat types.RelayFormat,
	run streamRecoveryRelay,
	runtime *service.StreamRecoveryRuntime,
	execution *model.StreamExecution,
	runnerID string,
	attempt int,
	body []byte,
	setting *operation_setting.StreamRecoverySetting,
) {
	workerContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage, err := common.CreateBodyStorage(append([]byte(nil), body...))
	if err != nil {
		_ = model.FinishStreamExecution(
			execution.StreamID,
			runnerID,
			model.StreamExecutionFailed,
			0,
			"failed to create worker body storage",
		)
		return
	}
	defer storage.Close()

	writer := service.NewStreamRecoveryWriter(
		workerContext,
		runtime.Store,
		execution.StreamID,
		attempt,
		func(sequence int64) error {
			return model.UpdateStreamExecutionSequence(execution.StreamID, runnerID, sequence)
		},
	)
	worker := parent.Copy()
	worker.Writer = writer
	worker.Request = parent.Request.Clone(workerContext)
	reader, err := storage.NewReader()
	if err != nil {
		_ = model.FinishStreamExecution(
			execution.StreamID,
			runnerID,
			model.StreamExecutionFailed,
			0,
			"failed to open worker request body",
		)
		return
	}
	defer reader.Close()
	worker.Request.Body = reader
	worker.Request.ContentLength = int64(len(body))
	worker.Set(common.KeyBodyStorage, storage)
	worker.Set(common.RequestIdKey, execution.StreamID)
	common.SetContextKey(worker, constant.ContextKeyStreamRecoveryWorker, true)
	common.SetContextKey(worker, constant.ContextKeyStreamRecoveryID, execution.StreamID)
	common.SetContextKey(worker, constant.ContextKeyStreamRecoveryRunner, runnerID)
	common.SetContextKey(worker, constant.ContextKeyStreamRecoveryBroker, writer)
	if attempt > 1 && execution.ActiveChannelID > 0 {
		worker.Set("use_channel", []string{strconv.Itoa(execution.ActiveChannelID)})
	}

	heartbeatDone := make(chan struct{})
	go renewStreamRecoveryLease(
		workerContext,
		cancel,
		execution.StreamID,
		runnerID,
		setting,
		heartbeatDone,
	)
	run(worker, relayFormat)
	close(heartbeatDone)

	writeErr := writer.FlushError()
	if writer.Terminal() && writeErr == nil {
		_ = writer.MarkAttemptState("completed", "")
		_ = model.FinishStreamExecution(
			execution.StreamID,
			runnerID,
			model.StreamExecutionCompleted,
			writer.Sequence(),
			"",
		)
		_ = runtime.Store.DeleteRequest(context.Background(), execution.StreamID)
		return
	}
	errorMessage := "stream ended without a protocol terminal event"
	if writeErr != nil {
		errorMessage = writeErr.Error()
	}
	_ = writer.MarkAttemptState("failed", errorMessage)
	_ = model.FinishStreamExecution(
		execution.StreamID,
		runnerID,
		model.StreamExecutionFailed,
		writer.Sequence(),
		errorMessage,
	)
}

func renewStreamRecoveryLease(
	ctx context.Context,
	cancel context.CancelFunc,
	streamID string,
	runnerID string,
	setting *operation_setting.StreamRecoverySetting,
	done <-chan struct{},
) {
	ticker := time.NewTicker(time.Duration(setting.HeartbeatSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := common.GetTimestamp()
			if err := model.RenewStreamExecutionLease(
				streamID,
				runnerID,
				now,
				now+int64(setting.LeaseSeconds),
			); err != nil {
				cancel()
				return
			}
		}
	}
}

func subscribeStreamRecovery(
	c *gin.Context,
	store *service.StreamRecoveryStore,
	execution *model.StreamExecution,
) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	attempt, err := store.GetPublicAttempt(c.Request.Context(), execution.StreamID)
	if err != nil || attempt == 0 {
		attempt = execution.PublicAttempt
	}
	for attempt == 0 {
		select {
		case <-c.Request.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
			attempt, _ = store.GetPublicAttempt(c.Request.Context(), execution.StreamID)
		}
	}
	c.Header(common.StreamRecoveryAttemptHeader, strconv.Itoa(attempt))

	sequence := streamRecoveryResumeSequence(c, attempt)
	for {
		frames, readErr := store.WaitFrames(
			c.Request.Context(),
			execution.StreamID,
			attempt,
			sequence,
			time.Second,
		)
		if readErr != nil {
			if c.Request.Context().Err() != nil {
				return
			}
			return
		}
		for _, frame := range frames {
			if _, writeErr := c.Writer.Write(frame.Data); writeErr != nil {
				return
			}
			if flusher, ok := c.Writer.(http.Flusher); ok {
				flusher.Flush()
			}
			sequence = frame.Sequence
			if frame.Terminal {
				return
			}
		}

		publicAttempt, publicErr := store.GetPublicAttempt(c.Request.Context(), execution.StreamID)
		if publicErr == nil && publicAttempt > 0 && publicAttempt != attempt {
			return
		}
		current, currentErr := model.GetOwnedStreamExecution(
			execution.StreamID,
			execution.UserID,
			execution.TokenID,
		)
		if currentErr != nil {
			return
		}
		if current.Status == model.StreamExecutionCompleted ||
			current.Status == model.StreamExecutionFailed ||
			current.Status == model.StreamExecutionCancelled {
			return
		}
		if current.LockedUntil > 0 && current.LockedUntil < common.GetTimestamp() {
			return
		}
	}
}

func streamRecoveryResumeSequence(c *gin.Context, attempt int) int64 {
	requestAttempt, _ := strconv.Atoi(c.GetHeader(common.StreamRecoveryAttemptHeader))
	if requestAttempt != attempt {
		return 0
	}
	value := strings.TrimSpace(c.GetHeader("Last-Event-ID"))
	sequence, err := strconv.ParseInt(value, 10, 64)
	if err != nil || sequence < 0 {
		return 0
	}
	return sequence
}

func streamRecoveryReason(created bool) string {
	if created {
		return ""
	}
	return "lease_expired"
}

func writeStreamRecoveryError(c *gin.Context, status int, code string, err error) {
	common.SysError("stream recovery error (" + code + "): " + err.Error())
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": common.MessageWithRequestId(
				"stream recovery request failed",
				c.GetString(common.RequestIdKey),
			),
			"type": "new_api_error",
			"code": code,
		},
	})
}

func GetStreamRecoverySession(c *gin.Context) {
	runtime, err := service.GetStreamRecoveryRuntime()
	if err != nil {
		writeStreamRecoveryError(c, http.StatusServiceUnavailable, "stream_recovery_unavailable", err)
		return
	}
	execution, err := model.GetOwnedStreamExecution(
		c.Param("stream_id"),
		c.GetInt(string(constant.ContextKeyUserId)),
		c.GetInt(string(constant.ContextKeyTokenId)),
	)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
			"message": "stream session not found",
			"type":    "new_api_error",
			"code":    "stream_recovery_not_found",
		}})
		return
	}
	attemptState, stateErr := runtime.Store.GetAttemptState(
		c.Request.Context(),
		execution.StreamID,
		execution.PublicAttempt,
	)
	if stateErr != nil {
		attemptState = service.StreamRecoveryAttemptState{}
	}
	c.JSON(http.StatusOK, gin.H{
		"id":            execution.StreamID,
		"object":        "stream_session",
		"model":         execution.ModelName,
		"status":        execution.Status,
		"attempt":       execution.PublicAttempt,
		"last_event_id": attemptState.LastSequence,
		"created_at":    execution.CreatedAt,
		"updated_at":    execution.UpdatedAt,
		"expires_at":    execution.ExpiresAt,
	})
}

func CancelStreamRecoverySession(c *gin.Context) {
	runtime, err := service.GetStreamRecoveryRuntime()
	if err != nil {
		writeStreamRecoveryError(c, http.StatusServiceUnavailable, "stream_recovery_unavailable", err)
		return
	}
	execution, err := model.GetOwnedStreamExecution(
		c.Param("stream_id"),
		c.GetInt(string(constant.ContextKeyUserId)),
		c.GetInt(string(constant.ContextKeyTokenId)),
	)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
			"message": "stream session not found",
			"type":    "new_api_error",
			"code":    "stream_recovery_not_found",
		}})
		return
	}
	cancelled, err := model.CancelOwnedStreamExecution(
		execution.StreamID,
		execution.UserID,
		execution.TokenID,
	)
	if err != nil {
		writeStreamRecoveryError(c, http.StatusInternalServerError, "stream_recovery_cancel", err)
		return
	}
	if cancelled {
		_ = runtime.Store.DeleteRequest(context.Background(), execution.StreamID)
	}
	c.JSON(http.StatusOK, gin.H{
		"object":    "stream_session",
		"stream_id": execution.StreamID,
		"cancelled": cancelled,
	})
}
