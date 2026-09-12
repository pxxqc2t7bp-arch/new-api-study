package gemini

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func GeminiLiveWebSocketURL(baseURL string) (string, error) {
	target, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(target.Scheme) {
	case "https", "wss":
		target.Scheme = "wss"
	case "http", "ws":
		target.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported Gemini Live base URL scheme: %s", target.Scheme)
	}
	if target.Host == "" || target.User != nil {
		return "", errors.New("invalid Gemini Live base URL")
	}
	target.Path = path.Join("/", target.Path, relayconstant.GeminiLivePath)
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	return target.String(), nil
}

func GeminiLiveHandler(c *gin.Context, info *relaycommon.RelayInfo) (*dto.RealtimeUsage, *types.NewAPIError) {
	if c == nil || info == nil || info.ClientWs == nil || info.TargetWs == nil {
		return nil, types.NewError(
			errors.New("invalid Gemini Live websocket connection"),
			types.ErrorCodeBadResponse,
			types.ErrOptionWithSkipRetry(),
		)
	}
	setup, ok := common.GetContextKeyType[[]byte](c, constant.ContextKeyRealtimeSetup)
	if !ok || len(setup) == 0 {
		return nil, types.NewError(
			errors.New("missing Gemini Live setup"),
			types.ErrorCodeInvalidRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	messageType := common.GetContextKeyInt(c, constant.ContextKeyRealtimeMessageType)
	if messageType != websocket.TextMessage {
		return nil, types.NewError(
			errors.New("invalid Gemini Live setup frame type"),
			types.ErrorCodeInvalidRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}

	accumulator := &geminiLiveUsageAccumulator{}
	if err := observeAndReserveGeminiLiveClient(c, info, accumulator, setup); err != nil {
		common.SetContextKey(
			c,
			constant.ContextKeyRealtimeQuotaLimit,
			currentGeminiLiveQuotaLimit(info),
		)
		return accumulator.Usage(c), geminiLiveReservationError(
			fmt.Errorf("reserve Gemini Live setup quota: %w", err),
		)
	}

	common.SetContextKey(c, constant.ContextKeyRealtimeSetupForwarded, true)
	if err := info.TargetWs.WriteMessage(messageType, setup); err != nil {
		return nil, types.NewError(
			fmt.Errorf("forward Gemini Live setup: %w", err),
			types.ErrorCodeDoRequestFailed,
			types.ErrOptionWithSkipRetry(),
		)
	}

	results := make(chan geminiLivePumpResult, 2)
	go pumpGeminiLiveClient(c, info.ClientWs, info.TargetWs, accumulator, results, info)
	go pumpGeminiLiveTarget(c, info.TargetWs, info.ClientWs, accumulator, results, info)

	first := <-results
	if first.source == geminiLivePumpTarget {
		forwardGeminiLiveClose(info.ClientWs, first.rawErr)
		_ = info.ClientWs.Close()
		common.SetContextKey(c, constant.ContextKeyRealtimeWSTerminated, true)
	} else {
		forwardGeminiLiveClose(info.TargetWs, first.rawErr)
		_ = info.TargetWs.Close()
		if first.rawErr != nil {
			common.SetContextKey(c, constant.ContextKeyRealtimeWSTerminated, true)
		}
	}
	if first.quotaLimited {
		common.SetContextKey(c, constant.ContextKeyRealtimeQuotaLimit, first.quotaLimit)
	}
	<-results

	usage := accumulator.Usage(c)
	if first.err != nil {
		if first.quotaLimited {
			return usage, geminiLiveReservationError(first.err)
		}
		return usage, types.NewError(
			first.err,
			types.ErrorCodeBadResponse,
			types.ErrOptionWithSkipRetry(),
		)
	}
	return usage, nil
}

func geminiLiveReservationError(err error) *types.NewAPIError {
	var apiErr *types.NewAPIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return types.NewError(
		err,
		types.ErrorCodeUpdateDataError,
		types.ErrOptionWithSkipRetry(),
	)
}

type geminiLivePumpSource int

const (
	geminiLivePumpClient geminiLivePumpSource = iota
	geminiLivePumpTarget
)

type geminiLivePumpResult struct {
	source       geminiLivePumpSource
	rawErr       error
	err          error
	quotaLimit   int
	quotaLimited bool
}

func pumpGeminiLiveClient(
	c *gin.Context,
	client *websocket.Conn,
	target *websocket.Conn,
	accumulator *geminiLiveUsageAccumulator,
	results chan<- geminiLivePumpResult,
	info *relaycommon.RelayInfo,
) {
	for {
		messageType, message, err := client.ReadMessage()
		if err != nil {
			results <- geminiLivePumpResult{
				source: geminiLivePumpClient,
				rawErr: err,
			}
			return
		}
		if err = observeAndReserveGeminiLiveClient(c, info, accumulator, message); err != nil {
			results <- geminiLivePumpResult{
				err:          fmt.Errorf("reserve Gemini Live quota: %w", err),
				source:       geminiLivePumpClient,
				quotaLimit:   currentGeminiLiveQuotaLimit(info),
				quotaLimited: true,
			}
			return
		}
		if err = target.WriteMessage(messageType, message); err != nil {
			results <- geminiLivePumpResult{
				source: geminiLivePumpClient,
				err:    fmt.Errorf("write Gemini Live upstream: %w", err),
			}
			return
		}
	}
}

func pumpGeminiLiveTarget(
	c *gin.Context,
	target *websocket.Conn,
	client *websocket.Conn,
	accumulator *geminiLiveUsageAccumulator,
	results chan<- geminiLivePumpResult,
	info *relaycommon.RelayInfo,
) {
	for {
		messageType, message, err := target.ReadMessage()
		if err != nil {
			results <- geminiLivePumpResult{
				source: geminiLivePumpTarget,
				rawErr: err,
				err:    geminiLiveReadError("upstream", err),
			}
			return
		}
		info.SetFirstResponseTime()
		if err = observeAndReserveGeminiLiveServer(c, info, accumulator, message); err != nil {
			results <- geminiLivePumpResult{
				err:          fmt.Errorf("reserve Gemini Live quota: %w", err),
				source:       geminiLivePumpTarget,
				quotaLimit:   currentGeminiLiveQuotaLimit(info),
				quotaLimited: true,
			}
			return
		}
		if err = client.WriteMessage(messageType, message); err != nil {
			results <- geminiLivePumpResult{
				source: geminiLivePumpTarget,
				err:    fmt.Errorf("write Gemini Live downstream: %w", err),
			}
			return
		}
	}
}

func geminiLiveReadError(side string, err error) error {
	if websocket.IsCloseError(
		err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
	) {
		return nil
	}
	return fmt.Errorf("read Gemini Live %s: %w", side, err)
}

func observeAndReserveGeminiLiveClient(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	accumulator *geminiLiveUsageAccumulator,
	message []byte,
) error {
	accumulator.reserveMu.Lock()
	defer accumulator.reserveMu.Unlock()
	accumulator.ObserveClient(message)
	usage, _ := accumulator.Snapshot()
	if err := service.ReserveWssConsumeQuota(c, info, info.OriginModelName, usage); err != nil {
		accumulator.RollbackClient(message)
		return err
	}
	return nil
}

func observeAndReserveGeminiLiveServer(
	c *gin.Context,
	info *relaycommon.RelayInfo,
	accumulator *geminiLiveUsageAccumulator,
	message []byte,
) error {
	accumulator.reserveMu.Lock()
	defer accumulator.reserveMu.Unlock()
	accumulator.ObserveServer(message)
	usage, _ := accumulator.Snapshot()
	return service.ReserveWssConsumeQuota(c, info, info.OriginModelName, usage)
}

func currentGeminiLiveQuotaLimit(info *relaycommon.RelayInfo) int {
	if info == nil || info.Billing == nil {
		return 0
	}
	return info.Billing.GetPreConsumedQuota()
}

func forwardGeminiLiveClose(target *websocket.Conn, err error) {
	var closeError *websocket.CloseError
	if target == nil {
		return
	}
	code := websocket.CloseInternalServerErr
	message := "Gemini Live relay closed"
	if errors.As(err, &closeError) {
		switch closeError.Code {
		case websocket.CloseAbnormalClosure,
			websocket.CloseNoStatusReceived,
			websocket.CloseTLSHandshake:
		default:
			code = closeError.Code
			message = closeError.Text
		}
	}
	_ = target.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, message),
		time.Now().Add(time.Second),
	)
}

type geminiLiveUsageAccumulator struct {
	mu            sync.Mutex
	reserveMu     sync.Mutex
	authoritative dto.RealtimeUsage
	estimated     dto.RealtimeUsage
	hasMetadata   bool
}

func (a *geminiLiveUsageAccumulator) ObserveClient(message []byte) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	textTokens, audioTokens := estimateGeminiLiveUsage(message, false)
	a.estimated.InputTokens += textTokens + audioTokens
	a.estimated.TotalTokens += textTokens + audioTokens
	a.estimated.InputTokenDetails.TextTokens += textTokens
	a.estimated.InputTokenDetails.AudioTokens += audioTokens
}

func (a *geminiLiveUsageAccumulator) RollbackClient(message []byte) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	textTokens, audioTokens := estimateGeminiLiveUsage(message, false)
	totalTokens := textTokens + audioTokens
	a.estimated.InputTokens = max(a.estimated.InputTokens-totalTokens, 0)
	a.estimated.TotalTokens = max(a.estimated.TotalTokens-totalTokens, 0)
	a.estimated.InputTokenDetails.TextTokens = max(
		a.estimated.InputTokenDetails.TextTokens-textTokens,
		0,
	)
	a.estimated.InputTokenDetails.AudioTokens = max(
		a.estimated.InputTokenDetails.AudioTokens-audioTokens,
		0,
	)
}

func (a *geminiLiveUsageAccumulator) ObserveServer(message []byte) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var response dto.GeminiLiveServerMessage
	if err := common.Unmarshal(message, &response); err == nil &&
		dto.HasGeminiLiveUsageMetadataTokens(response.UsageMetadata) {
		a.hasMetadata = true
		mergeGeminiLiveCumulativeUsage(
			&a.authoritative,
			geminiLiveUsageFromMetadata(response.UsageMetadata),
		)
		return
	}
	textTokens, audioTokens := estimateGeminiLiveUsage(message, true)
	a.estimated.OutputTokens += textTokens + audioTokens
	a.estimated.TotalTokens += textTokens + audioTokens
	a.estimated.OutputTokenDetails.TextTokens += textTokens
	a.estimated.OutputTokenDetails.AudioTokens += audioTokens
}

func (a *geminiLiveUsageAccumulator) Usage(c *gin.Context) *dto.RealtimeUsage {
	if a == nil {
		return &dto.RealtimeUsage{}
	}
	usage, estimated := a.Snapshot()
	if estimated {
		common.SetContextKey(c, constant.ContextKeyGeminiLiveUsageEstimated, true)
	}
	return usage
}

func (a *geminiLiveUsageAccumulator) Snapshot() (*dto.RealtimeUsage, bool) {
	if a == nil {
		return &dto.RealtimeUsage{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hasMetadata {
		usage, estimated := mergeGeminiLiveUsageEstimate(
			a.authoritative,
			a.estimated,
		)
		return &usage, estimated
	}
	usage := a.estimated
	return &usage, true
}

func mergeGeminiLiveUsageEstimate(
	authoritative dto.RealtimeUsage,
	estimated dto.RealtimeUsage,
) (dto.RealtimeUsage, bool) {
	usage := authoritative
	estimatedUsed := estimated.InputTokens > authoritative.InputTokens ||
		estimated.OutputTokens > authoritative.OutputTokens
	usage.InputTokenDetails.TextTokens = max(
		usage.InputTokenDetails.TextTokens,
		estimated.InputTokenDetails.TextTokens,
	)
	usage.InputTokenDetails.AudioTokens = max(
		usage.InputTokenDetails.AudioTokens,
		estimated.InputTokenDetails.AudioTokens,
	)
	usage.OutputTokenDetails.TextTokens = max(
		usage.OutputTokenDetails.TextTokens,
		estimated.OutputTokenDetails.TextTokens,
	)
	usage.OutputTokenDetails.AudioTokens = max(
		usage.OutputTokenDetails.AudioTokens,
		estimated.OutputTokenDetails.AudioTokens,
	)
	usage.InputTokens = max(
		usage.InputTokens,
		usage.InputTokenDetails.TextTokens+usage.InputTokenDetails.AudioTokens,
		estimated.InputTokens,
	)
	usage.OutputTokens = max(
		usage.OutputTokens,
		usage.OutputTokenDetails.TextTokens+usage.OutputTokenDetails.AudioTokens,
		estimated.OutputTokens,
	)
	usage.TotalTokens = max(
		usage.TotalTokens,
		usage.InputTokens+usage.OutputTokens,
		estimated.TotalTokens,
	)
	return usage, estimatedUsed
}

func mergeGeminiLiveCumulativeUsage(
	current *dto.RealtimeUsage,
	next dto.RealtimeUsage,
) {
	if current == nil {
		return
	}
	current.TotalTokens = max(current.TotalTokens, next.TotalTokens)
	current.InputTokens = max(current.InputTokens, next.InputTokens)
	current.OutputTokens = max(current.OutputTokens, next.OutputTokens)
	current.InputTokenDetails.CachedTokens = max(
		current.InputTokenDetails.CachedTokens,
		next.InputTokenDetails.CachedTokens,
	)
	current.InputTokenDetails.TextTokens = max(
		current.InputTokenDetails.TextTokens,
		next.InputTokenDetails.TextTokens,
	)
	current.InputTokenDetails.AudioTokens = max(
		current.InputTokenDetails.AudioTokens,
		next.InputTokenDetails.AudioTokens,
	)
	current.OutputTokenDetails.TextTokens = max(
		current.OutputTokenDetails.TextTokens,
		next.OutputTokenDetails.TextTokens,
	)
	current.OutputTokenDetails.AudioTokens = max(
		current.OutputTokenDetails.AudioTokens,
		next.OutputTokenDetails.AudioTokens,
	)
}

func geminiLiveUsageFromMetadata(metadata *dto.GeminiLiveUsageMetadata) dto.RealtimeUsage {
	if metadata == nil {
		return dto.RealtimeUsage{}
	}
	inputTokens := metadata.PromptTokenCount + metadata.ToolUsePromptTokenCount
	outputTokens := metadata.ResponseTokenCount + metadata.ThoughtsTokenCount
	totalTokens := max(metadata.TotalTokenCount, inputTokens+outputTokens)
	usage := dto.RealtimeUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
	}
	usage.InputTokenDetails.CachedTokens = metadata.CachedContentTokenCount

	inputDetailed := addGeminiLiveInputDetails(
		&usage.InputTokenDetails,
		metadata.PromptTokensDetails,
	)
	inputDetailed += addGeminiLiveInputDetails(
		&usage.InputTokenDetails,
		metadata.ToolUsePromptTokensDetails,
	)
	if remainder := inputTokens - inputDetailed; remainder > 0 {
		usage.InputTokenDetails.TextTokens += remainder
	}

	outputDetailed := addGeminiLiveOutputDetails(
		&usage.OutputTokenDetails,
		metadata.ResponseTokensDetails,
	)
	if remainder := outputTokens - outputDetailed; remainder > 0 {
		usage.OutputTokenDetails.TextTokens += remainder
	}
	return usage
}

func addGeminiLiveInputDetails(
	target *dto.InputTokenDetails,
	details []dto.GeminiPromptTokensDetails,
) int {
	total := 0
	for _, detail := range details {
		count := max(detail.TokenCount, 0)
		total += count
		if strings.EqualFold(detail.Modality, "AUDIO") {
			target.AudioTokens += count
		} else {
			target.TextTokens += count
		}
	}
	return total
}

func addGeminiLiveOutputDetails(
	target *dto.OutputTokenDetails,
	details []dto.GeminiPromptTokensDetails,
) int {
	total := 0
	for _, detail := range details {
		count := max(detail.TokenCount, 0)
		total += count
		if strings.EqualFold(detail.Modality, "AUDIO") {
			target.AudioTokens += count
		} else {
			target.TextTokens += count
		}
	}
	return total
}

func estimateGeminiLiveTokens(message []byte) int {
	return max((len(message)+3)/4, 1)
}

func estimateGeminiLiveUsage(message []byte, output bool) (int, int) {
	var payload any
	if err := common.Unmarshal(message, &payload); err != nil {
		return estimateGeminiLiveTokens(message), 0
	}
	var text strings.Builder
	audioTokens := collectGeminiLiveFallbackUsage(payload, output, &text)
	textTokens := estimateGeminiLiveTokens([]byte(text.String()))
	return textTokens, audioTokens
}

func collectGeminiLiveFallbackUsage(
	value any,
	output bool,
	text *strings.Builder,
) int {
	switch typed := value.(type) {
	case []any:
		total := 0
		for _, item := range typed {
			total += collectGeminiLiveFallbackUsage(item, output, text)
		}
		return total
	case map[string]any:
		mimeType, _ := typed["mimeType"].(string)
		if mimeType == "" {
			mimeType, _ = typed["mime_type"].(string)
		}
		data, _ := typed["data"].(string)
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "audio/") &&
			data != "" {
			var (
				tokens int
				err    error
			)
			if output {
				tokens, err = service.CountAudioTokenOutput(data, "pcm16")
			} else {
				tokens, err = service.CountAudioTokenInput(data, "pcm16")
			}
			if err == nil {
				return tokens
			}
		}
		total := 0
		for key, item := range typed {
			if key == "mimeType" || key == "mime_type" {
				continue
			}
			total += collectGeminiLiveFallbackUsage(item, output, text)
		}
		return total
	case string:
		text.WriteString(typed)
		text.WriteByte(' ')
		return 0
	default:
		return 0
	}
}
