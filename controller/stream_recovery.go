package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
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

var errStatefulReplayUnsafe = errors.New(
	"STATEFUL_REPLAY_UNSAFE: automatic generation replay is unavailable after execution started",
)

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

func capStreamRecoveryRetries(c *gin.Context, maxRetries int) int {
	writer := getStreamRecoveryWriter(c)
	if writer == nil {
		return maxRetries
	}
	remaining := operation_setting.GetStreamRecoverySetting().MaxAttempts - writer.Attempt()
	if remaining < 0 {
		return 0
	}
	return min(maxRetries, remaining)
}

func updateStreamRecoveryReplaySafetyForAttempt(
	c *gin.Context,
	relayFormat types.RelayFormat,
	info *relaycommon.RelayInfo,
	body []byte,
) error {
	projectedBody := body
	if info != nil && info.ChannelMeta != nil {
		var err error
		projectedBody, err = relaycommon.RemoveDisabledFields(
			projectedBody,
			info.ChannelOtherSettings,
			info.ChannelSetting.PassThroughBodyEnabled,
		)
		if err != nil {
			return err
		}
		projectedBody, err = relaycommon.ApplyParamOverride(
			projectedBody,
			info.ParamOverride,
			relaycommon.BuildParamOverrideContext(info),
		)
		if err != nil {
			return err
		}
	}
	safety, err := service.AssessStreamRecoveryReplaySafety(
		relayFormat,
		projectedBody,
	)
	if err != nil {
		return err
	}
	if !safety.Safe &&
		common.GetContextKeyString(
			c,
			constant.ContextKeyStreamRecoveryReplayUnsafe,
		) == "" {
		common.SetContextKey(
			c,
			constant.ContextKeyStreamRecoveryReplayUnsafe,
			safety.ReasonCode,
		)
	}
	return nil
}

func markStreamRecoveryConversionUnsafe(
	c *gin.Context,
	inboundFormat types.RelayFormat,
	outboundFormat types.RelayFormat,
) {
	if outboundFormat == "" ||
		outboundFormat == inboundFormat ||
		common.GetContextKeyString(
			c,
			constant.ContextKeyStreamRecoveryReplayUnsafe,
		) != "" {
		return
	}
	common.SetContextKey(
		c,
		constant.ContextKeyStreamRecoveryReplayUnsafe,
		service.StreamReplayUnsafeConversion,
	)
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
	if !setting.Enabled &&
		setting.IdentityMode != operation_setting.StreamRecoveryIdentityModeDraining {
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
	safety, err := service.AssessStreamRecoveryReplaySafety(relayFormat, body)
	if err != nil {
		writeStreamRecoveryError(c, http.StatusBadRequest, "stream_recovery_safety", err)
		return true
	}
	if !safety.Safe {
		common.SetContextKey(
			c,
			constant.ContextKeyStreamRecoveryReplayUnsafe,
			safety.ReasonCode,
		)
	}
	modelName := c.GetString(string(constant.ContextKeyOriginalModel))
	if modelName == "" {
		modelName = probe.Model
	}
	if !operation_setting.IsStreamRecoveryModelAllowed(modelName) {
		return false
	}
	if setting.IdentityMode == operation_setting.StreamRecoveryIdentityModeDraining {
		writeStreamRecoveryError(
			c,
			http.StatusServiceUnavailable,
			"stream_recovery_draining",
			errors.New("stream recovery is draining before identity migration"),
		)
		return true
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
	dedupeKey := identity.LegacyDedupeKey
	identityVersion := model.StreamRecoveryIdentityVersionLegacy
	var legacyDedupeKeys []string
	if setting.IdentityMode == operation_setting.StreamRecoveryIdentityModeStable {
		dedupeKey = identity.DedupeKey
		identityVersion = model.StreamRecoveryIdentityVersionStable
		legacyDedupeKeys = []string{identity.LegacyDedupeKey}
	}
	execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		DedupeKey:       dedupeKey,
		IdentityVersion: identityVersion,
		UserID:          c.GetInt(string(constant.ContextKeyUserId)),
		TokenID:         tokenID,
		ModelName:       modelName,
		RelayFormat:     string(relayFormat),
		RequestPath:     c.Request.URL.Path,
		RequestDigest:   identity.RequestDigest,
		ExpiresAt:       expiresAt,
	}, legacyDedupeKeys...)
	if err != nil {
		status := http.StatusInternalServerError
		code := "stream_recovery_create"
		if errors.Is(err, model.ErrStreamExecutionLegacyIdentityAmbiguous) {
			status = http.StatusConflict
			code = "stream_recovery_identity_conflict"
		}
		writeStreamRecoveryError(c, status, code, err)
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
		status := http.StatusServiceUnavailable
		code := "stream_recovery_worker"
		if errors.Is(err, errStatefulReplayUnsafe) {
			status = http.StatusConflict
			code = string(types.ErrorCodeStatefulReplayUnsafe)
		}
		writeStreamRecoveryError(c, status, code, err)
		return true
	}
	c.Header(common.StreamRecoveryIDHeader, execution.StreamID)
	c.Header(common.StreamRecoveryReplayedHeader, strconv.FormatBool(!launched))
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
		execution.Status == model.StreamExecutionCancelled {
		return false, nil
	}
	if execution.Status == model.StreamExecutionFailed {
		if strings.HasPrefix(
			execution.Error,
			string(types.ErrorCodeStatefulReplayUnsafe),
		) && execution.TerminalSequence == 0 {
			return false, errStatefulReplayUnsafe
		}
		return false, nil
	}
	if !created && execution.AttemptCount > 0 {
		reconciled, terminal, err := reconcileExpiredStreamExecution(
			context.Background(),
			runtime.Store,
			execution,
			now,
		)
		if err != nil {
			return false, err
		}
		if reconciled && !terminal {
			return false, errStatefulReplayUnsafe
		}
		if reconciled {
			return false, nil
		}
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
			claimed.PublicAttempt,
			model.StreamExecutionFailed,
			0,
			"failed to persist encrypted request",
		)
		return false, err
	}
	if err := model.StartStreamExecutionAttempt(
		claimed.StreamID,
		runnerID,
		attempt,
		c.GetInt(string(constant.ContextKeyChannelId)),
		streamRecoveryReason(created),
	); err != nil {
		_ = runtime.Store.DeleteRequest(context.Background(), claimed.StreamID)
		return false, err
	}
	if err := runtime.Store.SetPublicAttempt(context.Background(), claimed.StreamID, attempt); err != nil {
		_ = model.FinishStreamExecution(
			claimed.StreamID,
			runnerID,
			attempt,
			model.StreamExecutionFailed,
			0,
			"failed to publish stream recovery attempt",
		)
		_ = runtime.Store.DeleteRequest(context.Background(), claimed.StreamID)
		return false, err
	}

	workerContext, cancel := context.WithCancel(context.Background())
	worker := cloneStreamRecoveryWorkerContext(c, workerContext)
	heartbeatSeconds := setting.HeartbeatSeconds
	leaseSeconds := setting.LeaseSeconds
	go runStreamRecoveryWorker(
		worker,
		cancel,
		relayFormat,
		run,
		runtime,
		claimed,
		runnerID,
		attempt,
		body,
		heartbeatSeconds,
		leaseSeconds,
	)
	return true, nil
}

func cloneStreamRecoveryWorkerContext(
	parent *gin.Context,
	workerContext context.Context,
) *gin.Context {
	worker := parent.Copy()
	if parent.Request != nil {
		worker.Request = parent.Request.Clone(workerContext)
	}
	return worker
}

func runStreamRecoveryWorker(
	worker *gin.Context,
	cancel context.CancelFunc,
	relayFormat types.RelayFormat,
	run streamRecoveryRelay,
	runtime *service.StreamRecoveryRuntime,
	execution *model.StreamExecution,
	runnerID string,
	attempt int,
	body []byte,
	heartbeatSeconds int,
	leaseSeconds int,
) {
	defer cancel()
	workerContext := worker.Request.Context()
	storage, err := common.CreateBodyStorage(append([]byte(nil), body...))
	if err != nil {
		_ = model.FinishStreamExecution(
			execution.StreamID,
			runnerID,
			attempt,
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
		func(frameAttempt int, sequence int64) error {
			return commitStreamRecoveryFrame(
				execution.StreamID,
				runnerID,
				frameAttempt,
				sequence,
			)
		},
	)
	worker.Writer = writer
	reader, err := storage.NewReader()
	if err != nil {
		_ = model.FinishStreamExecution(
			execution.StreamID,
			runnerID,
			attempt,
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
	heartbeatStopped := make(chan struct{})
	go func() {
		defer close(heartbeatStopped)
		renewStreamRecoveryLease(
			workerContext,
			cancel,
			execution.StreamID,
			runnerID,
			heartbeatSeconds,
			leaseSeconds,
			heartbeatDone,
		)
	}()
	run(worker, relayFormat)
	close(heartbeatDone)
	<-heartbeatStopped

	writeErr := writer.FlushError()
	if errors.Is(writeErr, service.ErrStreamRecoveryCommitUncertain) {
		_, _ = model.UpdateStreamBillingState(
			execution.StreamID,
			[]string{
				model.StreamBillingReserving,
				model.StreamBillingReserved,
				model.StreamBillingSettling,
				model.StreamBillingRefunding,
			},
			map[string]any{
				"billing_status": model.StreamBillingUncertain,
			},
		)
		return
	}
	if !writer.Terminal() && writeErr == nil {
		protocolError := types.NewErrorWithStatusCode(
			errors.New("stream ended without a protocol terminal event"),
			types.ErrorCodeBadResponse,
			http.StatusBadGateway,
			types.ErrOptionWithSkipRetry(),
		)
		if err := writeStreamRecoveryRelayError(
			worker,
			relayFormat,
			protocolError,
		); err != nil {
			writeErr = err
		}
	}
	finalAttempt := writer.Attempt()
	if writer.Terminal() && !writer.FailedTerminal() && writeErr == nil {
		_ = writer.MarkAttemptState("completed", "")
		_ = model.FinishStreamExecution(
			execution.StreamID,
			runnerID,
			finalAttempt,
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
	} else if writer.FailedTerminal() {
		errorMessage = "stream ended with a protocol error event"
	}
	if common.GetContextKeyBool(
		worker,
		constant.ContextKeyStreamRecoveryBillingUncertain,
	) {
		_, _ = model.UpdateStreamBillingState(
			execution.StreamID,
			[]string{
				model.StreamBillingReserving,
				model.StreamBillingReserved,
				model.StreamBillingSettling,
				model.StreamBillingRefunding,
			},
			map[string]any{
				"billing_status": model.StreamBillingUncertain,
			},
		)
	}
	_ = writer.MarkAttemptState("failed", errorMessage)
	_ = model.FinishStreamExecution(
		execution.StreamID,
		runnerID,
		finalAttempt,
		model.StreamExecutionFailed,
		writer.Sequence(),
		errorMessage,
	)
}

func commitStreamRecoveryFrame(
	streamID string,
	runnerID string,
	attempt int,
	sequence int64,
) error {
	err := model.UpdateStreamExecutionSequence(
		streamID,
		runnerID,
		attempt,
		sequence,
	)
	if err == nil {
		return nil
	}
	current, readErr := model.GetStreamExecution(streamID)
	if readErr != nil {
		return fmt.Errorf(
			"%w: update failed: %v; verification failed: %v",
			service.ErrStreamRecoveryCommitUncertain,
			err,
			readErr,
		)
	}
	if current.PublicAttempt == attempt &&
		current.AttemptCount == attempt &&
		current.CommittedSequence >= sequence {
		return nil
	}
	if errors.Is(err, model.ErrStreamExecutionLeaseLost) {
		return fmt.Errorf("%w: %v", service.ErrStreamRecoveryFrameRejected, err)
	}
	return fmt.Errorf(
		"%w: update failed: %v",
		service.ErrStreamRecoveryCommitUncertain,
		err,
	)
}

func renewStreamRecoveryLease(
	ctx context.Context,
	cancel context.CancelFunc,
	streamID string,
	runnerID string,
	heartbeatSeconds int,
	leaseSeconds int,
	done <-chan struct{},
) {
	ticker := time.NewTicker(time.Duration(heartbeatSeconds) * time.Second)
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
				now+int64(leaseSeconds),
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
	attempt, err := store.GetPublicAttempt(c.Request.Context(), execution.StreamID)
	if err != nil || attempt == 0 {
		attempt = execution.PublicAttempt
	}
	for attempt == 0 {
		current, currentErr := model.GetOwnedStreamExecution(
			execution.StreamID,
			execution.UserID,
			execution.TokenID,
		)
		if currentErr != nil {
			writeStreamRecoveryError(
				c,
				http.StatusBadGateway,
				"stream_recovery_protocol_error",
				currentErr,
			)
			return
		}
		if current.PublicAttempt > 0 {
			attempt = current.PublicAttempt
			break
		}
		if current.Status == model.StreamExecutionCompleted ||
			current.Status == model.StreamExecutionFailed ||
			current.Status == model.StreamExecutionCancelled ||
			current.LockedUntil < common.GetTimestamp() {
			writeStreamRecoveryError(
				c,
				http.StatusBadGateway,
				"stream_recovery_protocol_error",
				errors.New("stream recovery terminated before an attempt was published"),
			)
			return
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
			attempt, _ = store.GetPublicAttempt(c.Request.Context(), execution.StreamID)
		}
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Header(common.StreamRecoveryAttemptHeader, strconv.Itoa(attempt))

	sequence := streamRecoveryResumeSequence(c, attempt)
	businessOutput := false
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
		current, currentErr := model.GetOwnedStreamExecution(
			execution.StreamID,
			execution.UserID,
			execution.TokenID,
		)
		if currentErr != nil {
			return
		}
		if current.PublicAttempt > 0 && current.PublicAttempt != attempt {
			if !businessOutput && sequence > 0 {
				var complete bool
				businessOutput, complete, readErr =
					store.HasBusinessOutputThrough(
						c.Request.Context(),
						execution.StreamID,
						attempt,
						sequence,
					)
				if readErr != nil || !complete {
					return
				}
			}
			if businessOutput {
				return
			}
			attempt = current.PublicAttempt
			sequence = streamRecoveryResumeSequence(c, attempt)
			businessOutput = false
			if sequence > 0 {
				var complete bool
				businessOutput, complete, readErr =
					store.HasBusinessOutputThrough(
						c.Request.Context(),
						execution.StreamID,
						attempt,
						sequence,
					)
				if readErr != nil || !complete {
					return
				}
			}
			c.Header(common.StreamRecoveryAttemptHeader, strconv.Itoa(attempt))
			continue
		}
		wroteFrame := false
		for _, frame := range frames {
			if frame.Sequence > current.CommittedSequence {
				break
			}
			if frame.Sequence != sequence+1 {
				writeStreamRecoverySubscriptionError(
					c,
					types.RelayFormat(execution.RelayFormat),
					errors.New("stream recovery committed frame sequence is unavailable"),
				)
				return
			}
			if _, writeErr := c.Writer.Write(frame.Data); writeErr != nil {
				return
			}
			if flusher, ok := c.Writer.(http.Flusher); ok {
				flusher.Flush()
			}
			sequence = frame.Sequence
			wroteFrame = true
			businessOutput = businessOutput ||
				service.StreamRecoveryFrameHasBusinessData(frame.Data)
			if frame.Terminal {
				return
			}
		}
		if !wroteFrame && sequence < current.CommittedSequence {
			writeStreamRecoverySubscriptionError(
				c,
				types.RelayFormat(execution.RelayFormat),
				errors.New("stream recovery committed frames are unavailable"),
			)
			return
		}

		if current.Status == model.StreamExecutionCompleted ||
			current.Status == model.StreamExecutionFailed ||
			current.Status == model.StreamExecutionCancelled {
			if sequence == 0 && current.CommittedSequence == 0 {
				writeStreamRecoveryError(
					c,
					http.StatusBadGateway,
					"stream_recovery_protocol_error",
					errors.New("stream recovery terminated without a committed frame"),
				)
			}
			return
		}
		if current.LockedUntil > 0 && current.LockedUntil < common.GetTimestamp() {
			return
		}
		if len(frames) > 0 && !wroteFrame {
			select {
			case <-c.Request.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}

func streamRecoveryResumeSequence(c *gin.Context, attempt int) int64 {
	value := strings.TrimSpace(c.GetHeader("Last-Event-ID"))
	if cursorAttempt, cursorSequence, found := strings.Cut(value, ":"); found {
		parsedAttempt, attemptErr := strconv.Atoi(cursorAttempt)
		sequence, sequenceErr := strconv.ParseInt(cursorSequence, 10, 64)
		if attemptErr != nil || sequenceErr != nil ||
			parsedAttempt != attempt || sequence < 0 {
			return 0
		}
		return sequence
	}
	requestAttemptHeader := strings.TrimSpace(
		c.GetHeader(common.StreamRecoveryAttemptHeader),
	)
	if requestAttemptHeader == "" {
		if attempt != 1 {
			return 0
		}
	} else {
		requestAttempt, attemptErr := strconv.Atoi(requestAttemptHeader)
		if attemptErr != nil || requestAttempt != attempt {
			return 0
		}
	}
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

type streamRecoveryReconcileSummary struct {
	Reconciled     int `json:"reconciled"`
	TerminalFrames int `json:"terminal_frames"`
}

func reconcileExpiredStreamExecutions(
	ctx context.Context,
	store *service.StreamRecoveryStore,
	now int64,
	limit int,
) (streamRecoveryReconcileSummary, error) {
	summary := streamRecoveryReconcileSummary{}
	if store == nil {
		return summary, errors.New("stream recovery store is nil")
	}
	for {
		executions, err := model.FindRecoverableStreamExecutions(now, limit)
		if err != nil {
			return summary, err
		}
		if len(executions) == 0 {
			break
		}
		for _, execution := range executions {
			if err = ctx.Err(); err != nil {
				return summary, err
			}
			reconciled, terminalFrame, reconcileErr := reconcileExpiredStreamExecution(
				ctx,
				store,
				execution,
				now,
			)
			if reconcileErr != nil {
				return summary, reconcileErr
			}
			if !reconciled {
				continue
			}
			summary.Reconciled++
			if terminalFrame {
				summary.TerminalFrames++
			}
		}
		if len(executions) < limit {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if err := model.DeleteExpiredStreamExecutions(now); err != nil {
		return summary, err
	}
	return summary, nil
}

func reconcileExpiredStreamExecution(
	ctx context.Context,
	store *service.StreamRecoveryStore,
	execution *model.StreamExecution,
	now int64,
) (bool, bool, error) {
	if execution == nil || execution.AttemptCount <= 0 {
		return false, false, nil
	}
	runnerKey, err := common.GenerateRandomCharsKey(24)
	if err != nil {
		return false, false, err
	}
	leaseSeconds := operation_setting.GetStreamRecoverySetting().LeaseSeconds
	claimed, won, err := model.ClaimExpiredStreamExecutionForReconciliation(
		execution.StreamID,
		execution.AttemptCount,
		common.NodeName+":stream-reconcile:"+runnerKey,
		now,
		now+int64(leaseSeconds),
	)
	if err != nil || !won {
		return false, false, err
	}

	status := model.StreamExecutionFailed
	terminalSequence := claimed.CommittedSequence
	errorMessage := errStatefulReplayUnsafe.Error()
	var rollbackFrame service.StreamRecoveryFrame

	if claimed.PublicAttempt > 0 && claimed.CommittedSequence > 0 {
		committedTail, readErr := store.ReadFrames(
			ctx,
			claimed.StreamID,
			claimed.PublicAttempt,
			claimed.CommittedSequence-1,
			2,
		)
		if readErr != nil {
			return false, false, readErr
		}
		if len(committedTail) == 0 ||
			committedTail[0].Sequence != claimed.CommittedSequence {
			return false, false, errors.New(
				"stream recovery reconciliation is missing the committed tail",
			)
		}
		if committedTail[0].Terminal {
			terminal, failed := service.StreamRecoveryTerminalState(
				committedTail[0].Data,
			)
			if !terminal || len(committedTail) != 1 {
				return false, false, errors.New(
					"stream recovery reconciliation found frames after a committed terminal",
				)
			}
			if !failed {
				status = model.StreamExecutionCompleted
				errorMessage = ""
			} else if !bytes.Contains(
				committedTail[0].Data,
				[]byte(types.ErrorCodeStatefulReplayUnsafe),
			) {
				errorMessage = "stream ended with a protocol failure terminal"
			}
			return finishReconciledStreamExecution(
				ctx,
				store,
				claimed,
				status,
				claimed.CommittedSequence,
				errorMessage,
			)
		}
	}

	if claimed.PublicAttempt > 0 {
		frame, frameErr := statefulReplayUnsafeTerminalFrame(
			types.RelayFormat(claimed.RelayFormat),
			claimed.PublicAttempt,
			claimed.CommittedSequence+1,
		)
		if frameErr != nil {
			return false, false, frameErr
		}
		rollbackFrame = service.StreamRecoveryFrame{
			Sequence: claimed.CommittedSequence + 1,
			Kind:     "data",
			Data:     frame,
			Terminal: true,
		}

		frameResolved := false
		for range 3 {
			frames, readErr := store.ReadFrames(
				ctx,
				claimed.StreamID,
				claimed.PublicAttempt,
				claimed.CommittedSequence,
				2,
			)
			if readErr != nil {
				return false, false, readErr
			}
			switch {
			case len(frames) == 0 && claimed.CommittedSequence == 0:
				frameResolved = true
				rollbackFrame = service.StreamRecoveryFrame{}
			case len(frames) == 0:
				rollbackFrame, err = store.AppendFrameForRollback(
					ctx,
					claimed.StreamID,
					claimed.PublicAttempt,
					rollbackFrame,
				)
				if err != nil {
					return false, false, err
				}
				terminalSequence = rollbackFrame.Sequence
				frameResolved = true
			case len(frames) == 1 &&
				frames[0].Sequence == claimed.CommittedSequence+1 &&
				frames[0].Terminal:
				terminal, failed := service.StreamRecoveryTerminalState(
					frames[0].Data,
				)
				if !terminal {
					return false, false, errors.New(
						"stream recovery terminal metadata does not match its payload",
					)
				}
				rollbackFrame = frames[0]
				terminalSequence = frames[0].Sequence
				if !failed {
					status = model.StreamExecutionCompleted
					errorMessage = ""
				} else if !bytes.Contains(
					frames[0].Data,
					[]byte(types.ErrorCodeStatefulReplayUnsafe),
				) {
					errorMessage = "stream ended with a protocol failure terminal"
				}
				frameResolved = true
			case len(frames) == 1 &&
				frames[0].Sequence == claimed.CommittedSequence+1:
				rollbackErr := rollbackUncommittedStreamRecoveryFrame(
					ctx,
					store,
					claimed,
					frames[0],
				)
				if rollbackErr != nil &&
					!errors.Is(rollbackErr, service.ErrStreamRecoveryFrameNotFound) &&
					!errors.Is(rollbackErr, service.ErrStreamRecoveryFrameChanged) {
					return false, false, rollbackErr
				}
			default:
				return false, false, errors.New(
					"stream recovery reconciliation found conflicting frames",
				)
			}
			if frameResolved {
				break
			}
		}
		if !frameResolved {
			return false, false, errors.New(
				"stream recovery reconciliation frame changed repeatedly",
			)
		}
	}

	return finishReconciledStreamExecution(
		ctx,
		store,
		claimed,
		status,
		terminalSequence,
		errorMessage,
	)
}

func rollbackUncommittedStreamRecoveryFrame(
	ctx context.Context,
	store *service.StreamRecoveryStore,
	claimed *model.StreamExecution,
	frame service.StreamRecoveryFrame,
) error {
	owned, err := model.OwnsStreamExecutionReconciliationLease(
		claimed.StreamID,
		claimed.LockedBy,
		claimed.PublicAttempt,
		claimed.CommittedSequence,
	)
	if err != nil {
		return err
	}
	if !owned {
		return model.ErrStreamExecutionLeaseLost
	}
	return store.RollbackFrame(
		ctx,
		claimed.StreamID,
		claimed.PublicAttempt,
		frame,
	)
}

func finishReconciledStreamExecution(
	ctx context.Context,
	store *service.StreamRecoveryStore,
	claimed *model.StreamExecution,
	status model.StreamExecutionStatus,
	terminalSequence int64,
	errorMessage string,
) (bool, bool, error) {
	owned, ownershipErr := model.OwnsStreamExecutionReconciliationLease(
		claimed.StreamID,
		claimed.LockedBy,
		claimed.PublicAttempt,
		claimed.CommittedSequence,
	)
	if ownershipErr != nil {
		return false, false, ownershipErr
	}
	if !owned {
		return false, false, model.ErrStreamExecutionLeaseLost
	}
	if terminalSequence > 0 {
		frames, readErr := store.ReadFrames(
			ctx,
			claimed.StreamID,
			claimed.PublicAttempt,
			terminalSequence-1,
			1,
		)
		if readErr != nil {
			return false, false, readErr
		}
		if len(frames) != 1 ||
			frames[0].Sequence != terminalSequence ||
			!frames[0].Terminal {
			return false, false, errors.New(
				"stream recovery reconciliation terminal frame is unavailable",
			)
		}
		if sealErr := store.SealFrame(
			ctx,
			claimed.StreamID,
			claimed.PublicAttempt,
			frames[0],
		); sealErr != nil {
			return false, false, sealErr
		}
	}
	finished, finishErr := model.FinishStreamExecutionReconciliation(
		claimed.StreamID,
		claimed.LockedBy,
		claimed.PublicAttempt,
		status,
		claimed.CommittedSequence,
		terminalSequence,
		errorMessage,
	)
	if finishErr == nil && !finished {
		return false, false, model.ErrStreamExecutionLeaseLost
	}
	if finishErr != nil {
		return false, false, finishErr
	}
	if err := store.SetAttemptState(
		ctx,
		claimed.StreamID,
		claimed.PublicAttempt,
		service.StreamRecoveryAttemptState{
			Status:       string(status),
			LastSequence: terminalSequence,
			Error:        errorMessage,
		},
	); err != nil {
		common.SysLog(fmt.Sprintf(
			"stream recovery attempt state update failed after database finalization: stream_id=%s attempt=%d error=%v",
			claimed.StreamID,
			claimed.PublicAttempt,
			err,
		))
	}
	return true, terminalSequence > 0, nil
}

func newStatefulReplayUnsafeAPIError(reason string) *types.NewAPIError {
	return types.NewOpenAIError(
		fmt.Errorf(
			"%s: automatic replay rejected (%s)",
			types.ErrorCodeStatefulReplayUnsafe,
			reason,
		),
		types.ErrorCodeStatefulReplayUnsafe,
		http.StatusConflict,
		types.ErrOptionWithSkipRetry(),
	)
}

func streamRecoveryRelayErrorEvent(
	relayFormat types.RelayFormat,
	apiError *types.NewAPIError,
) ([]byte, error) {
	var payload any
	if relayFormat == types.RelayFormatClaude {
		payload = gin.H{
			"type":  "error",
			"error": apiError.ToClaudeError(),
		}
	} else {
		openAIError := apiError.ToOpenAIError()
		payload = gin.H{
			"type":    "error",
			"code":    openAIError.Code,
			"message": openAIError.Message,
			"param":   openAIError.Param,
		}
	}
	data, err := common.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []byte("event: error\ndata: " + string(data) + "\n\n"), nil
}

func statefulReplayUnsafeTerminalFrame(
	relayFormat types.RelayFormat,
	attempt int,
	sequence int64,
) ([]byte, error) {
	apiError := newStatefulReplayUnsafeAPIError("lease_expired")
	frame, err := streamRecoveryRelayErrorEvent(relayFormat, apiError)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(
		nil,
		"id: %d:%d\n%s",
		attempt,
		sequence,
		frame,
	), nil
}

func writeStreamRecoveryRelayError(
	c *gin.Context,
	relayFormat types.RelayFormat,
	apiError *types.NewAPIError,
) error {
	writer := getStreamRecoveryWriter(c)
	if writer == nil || apiError == nil || writer.Terminal() {
		return nil
	}
	data, err := streamRecoveryRelayErrorEvent(relayFormat, apiError)
	if err != nil {
		return err
	}
	if _, err = writer.Write(data); err != nil {
		return err
	}
	return writer.FlushError()
}

func writeStreamRecoverySubscriptionError(
	c *gin.Context,
	relayFormat types.RelayFormat,
	err error,
) {
	const code = "stream_recovery_protocol_error"
	if !c.Writer.Written() {
		writeStreamRecoveryError(c, http.StatusBadGateway, code, err)
		return
	}
	apiError := types.NewOpenAIError(
		err,
		types.ErrorCode(code),
		http.StatusBadGateway,
		types.ErrOptionWithSkipRetry(),
	)
	frame, marshalErr := streamRecoveryRelayErrorEvent(relayFormat, apiError)
	if marshalErr != nil {
		common.SysError(
			"stream recovery subscription error marshal failed: " +
				marshalErr.Error(),
		)
		return
	}
	_, _ = c.Writer.Write(frame)
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
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
	lastEventID := max(attemptState.LastSequence, execution.CommittedSequence)
	response := gin.H{
		"id":            execution.StreamID,
		"object":        "stream_session",
		"model":         execution.ModelName,
		"status":        execution.Status,
		"attempt":       execution.PublicAttempt,
		"last_event_id": lastEventID,
		"created_at":    execution.CreatedAt,
		"updated_at":    execution.UpdatedAt,
		"expires_at":    execution.ExpiresAt,
	}
	if strings.HasPrefix(
		execution.Error,
		string(types.ErrorCodeStatefulReplayUnsafe),
	) {
		response["error_code"] = types.ErrorCodeStatefulReplayUnsafe
	}
	c.JSON(http.StatusOK, response)
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
