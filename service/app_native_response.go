package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/expr-lang/expr/ast"
	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
	"golang.org/x/net/http/httpguts"
)

const appResponseMaxBodyBytes = 64 * 1024

type AppPreparedResponseRequest struct {
	PublicModel     string
	ActualModel     string
	Input           string
	MaxOutputTokens int
	Outbound        []byte
}

type appResponseTextRequest struct {
	Model           string `json:"model"`
	Input           string `json:"input"`
	MaxOutputTokens *int64 `json:"max_output_tokens"`
	Stream          *bool  `json:"stream,omitempty"`
	Store           *bool  `json:"store,omitempty"`
}

type appResponseTextOutbound struct {
	Model           string `json:"model"`
	Input           string `json:"input"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	Stream          bool   `json:"stream"`
	Store           bool   `json:"store"`
}

func PrepareAppResponseRequest(raw []byte, candidate model.AppExecutionModel) (AppPreparedResponseRequest, error) {
	if candidate.ExecutionKind != model.AppExecutionModelKindNativeResponse ||
		candidate.Protocol != "openai_responses" ||
		candidate.RequestProfile != appResponsesTextProfileV1 ||
		candidate.ChannelType != constant.ChannelTypeOpenAI ||
		candidate.APIType != constant.APITypeOpenAI ||
		candidate.PublicModel == "" || candidate.ActualModel == "" ||
		candidate.PluginKey != "" || candidate.PluginVersion != "" || candidate.PluginSHA256 != "" {
		return AppPreparedResponseRequest{}, appAuthError("invalid_grant")
	}
	if !utf8.Valid(raw) {
		return AppPreparedResponseRequest{}, appAuthError("invalid_request")
	}

	var request appResponseTextRequest
	if err := decodeAppExecutionJSON(raw, &request); err != nil {
		return AppPreparedResponseRequest{}, err
	}
	if !appResponseLiteralRequestKeys(raw) {
		return AppPreparedResponseRequest{}, appAuthError("invalid_request")
	}
	var fields map[string]json.RawMessage
	if common.Unmarshal(raw, &fields) != nil ||
		request.Model != candidate.PublicModel ||
		strings.TrimSpace(request.Input) == "" ||
		request.MaxOutputTokens == nil ||
		*request.MaxOutputTokens <= 0 ||
		*request.MaxOutputTokens > int64(constant.MaxTokensLimit) ||
		invalidOptionalFalse(fields, "stream", request.Stream) ||
		invalidOptionalFalse(fields, "store", request.Store) {
		return AppPreparedResponseRequest{}, appAuthError("invalid_request")
	}

	outbound, err := common.Marshal(appResponseTextOutbound{
		Model:           candidate.ActualModel,
		Input:           request.Input,
		MaxOutputTokens: *request.MaxOutputTokens,
	})
	if err != nil {
		return AppPreparedResponseRequest{}, err
	}
	return AppPreparedResponseRequest{
		PublicModel:     candidate.PublicModel,
		ActualModel:     candidate.ActualModel,
		Input:           request.Input,
		MaxOutputTokens: int(*request.MaxOutputTokens),
		Outbound:        outbound,
	}, nil
}

func invalidOptionalFalse(fields map[string]json.RawMessage, name string, value *bool) bool {
	raw, present := fields[name]
	return present && (value == nil || *value || bytes.Equal(bytes.TrimSpace(raw), []byte("null")))
}

type AppResponseUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	TotalTokens              int `json:"total_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	TextInputTokens          int `json:"text_input_tokens"`
	AudioInputTokens         int `json:"audio_input_tokens"`
	ImageInputTokens         int `json:"image_input_tokens"`
	TextOutputTokens         int `json:"text_output_tokens"`
	AudioOutputTokens        int `json:"audio_output_tokens"`
	ImageOutputTokens        int `json:"image_output_tokens"`
	ReasoningOutputTokens    int `json:"reasoning_output_tokens"`
}

type AppResponseRequestFacts struct {
	EstimatedInputTokens int `json:"estimated_input_tokens"`
	MaxOutputTokens      int `json:"max_output_tokens"`
}

type AppResponseQuotaQuote struct {
	Quota        int                            `json:"quota"`
	MatchedTier  string                         `json:"matched_tier,omitempty"`
	RequestRules []billingexpr.RequestRuleTrace `json:"request_rules,omitempty"`
}

type AppParsedResponse struct {
	Rejected            bool
	HTTPStatus          int
	SafeHeadersJSON     string
	Body                []byte
	BodySHA256          string
	ResponseID          string
	ProviderModel       string
	ProviderStatus      string
	RawUsageJSON        string
	NormalizedUsageJSON string
	Usage               AppResponseUsage
}

type appResponseEnvelope struct {
	ID                string          `json:"id"`
	Status            string          `json:"status"`
	Model             string          `json:"model"`
	Error             json.RawMessage `json:"error"`
	IncompleteDetails json.RawMessage `json:"incomplete_details"`
	Usage             json.RawMessage `json:"usage"`
}

type appResponseUsageDetails struct {
	CachedTokens         *int64 `json:"cached_tokens"`
	CachedCreationTokens *int64 `json:"cached_creation_tokens"`
	CacheWriteTokens     *int64 `json:"cache_write_tokens"`
	TextTokens           *int64 `json:"text_tokens"`
	AudioTokens          *int64 `json:"audio_tokens"`
	ImageTokens          *int64 `json:"image_tokens"`
	ReasoningTokens      *int64 `json:"reasoning_tokens"`
}

type appResponseUsageWire struct {
	InputTokens        *int64                   `json:"input_tokens"`
	InputTokenDetails  *appResponseUsageDetails `json:"input_tokens_details"`
	OutputTokens       *int64                   `json:"output_tokens"`
	OutputTokenDetails *appResponseUsageDetails `json:"output_tokens_details"`
	TotalTokens        *int64                   `json:"total_tokens"`
}

func ParseAppResponse(status int, headers http.Header, body []byte, actualModel string) (AppParsedResponse, error) {
	if status < 200 || status > 599 || len(body) > appResponseMaxBodyBytes || actualModel == "" {
		return AppParsedResponse{}, appAuthError("invalid_upstream_response")
	}
	safeHeaders, err := appResponseSafeHeaders(headers)
	if err != nil {
		return AppParsedResponse{}, err
	}
	captured := appResponseCapture(status, safeHeaders, body)
	if status >= 300 {
		captured.Rejected = true
		return captured, nil
	}
	if status != http.StatusOK || !appResponseJSONContentType(headers) || !utf8.Valid(body) ||
		len(body) == 0 || !appResponseUniqueJSON(gjson.ParseBytes(body)) {
		return AppParsedResponse{}, appAuthError("invalid_upstream_response")
	}

	var envelope appResponseEnvelope
	if common.Unmarshal(body, &envelope) != nil ||
		envelope.ID == "" || len(envelope.ID) > 255 ||
		envelope.Model != actualModel || envelope.Status != "completed" ||
		!appResponseNullOrMissing(envelope.Error) ||
		!appResponseNullOrMissing(envelope.IncompleteDetails) ||
		len(envelope.Usage) == 0 || appResponseNullOrMissing(envelope.Usage) {
		return AppParsedResponse{}, appAuthError("invalid_upstream_response")
	}
	usage, err := normalizeAppResponseUsage(envelope.Usage)
	if err != nil {
		return AppParsedResponse{}, err
	}
	normalized, err := common.Marshal(usage)
	if err != nil {
		return AppParsedResponse{}, err
	}
	captured.ResponseID = envelope.ID
	captured.ProviderModel = envelope.Model
	captured.ProviderStatus = envelope.Status
	captured.RawUsageJSON = string(envelope.Usage)
	captured.NormalizedUsageJSON = string(normalized)
	captured.Usage = usage
	return captured, nil
}

func appResponseCapture(status int, safeHeaders string, body []byte) AppParsedResponse {
	digest := sha256.Sum256(body)
	return AppParsedResponse{
		HTTPStatus:      status,
		SafeHeadersJSON: safeHeaders,
		Body:            bytes.Clone(body),
		BodySHA256:      hex.EncodeToString(digest[:]),
	}
}

func appResponseSafeHeaders(headers http.Header) (string, error) {
	safe := map[string][]string{}
	for _, name := range []string{"Content-Type", "OpenAI-Request-ID", "X-Request-ID"} {
		values := headers.Values(name)
		filtered := make([]string, 0, len(values))
		for _, value := range values {
			if len(value) > 8*1024 || !httpguts.ValidHeaderFieldValue(value) {
				return "", appAuthError("invalid_upstream_response")
			}
			filtered = append(filtered, value)
		}
		if len(filtered) > 0 {
			safe[name] = filtered
		}
	}
	raw, err := common.Marshal(safe)
	if err != nil {
		return "", err
	}
	if len(raw) > appResponseMaxBodyBytes {
		return "", appAuthError("invalid_upstream_response")
	}
	return string(raw), nil
}

func appResponseJSONContentType(headers http.Header) bool {
	values := headers.Values("Content-Type")
	if len(values) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(values[0])
	return err == nil && mediaType == "application/json"
}

func appResponseUniqueJSON(value gjson.Result) bool {
	if !value.IsObject() && !value.IsArray() {
		return value.Exists()
	}
	seen := map[string]struct{}{}
	valid := true
	value.ForEach(func(key, child gjson.Result) bool {
		if value.IsObject() {
			if _, duplicate := seen[key.Str]; duplicate {
				valid = false
				return false
			}
			seen[key.Str] = struct{}{}
		}
		valid = appResponseUniqueJSON(child)
		return valid
	})
	return valid
}

func appResponseNullOrMissing(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func normalizeAppResponseUsage(raw json.RawMessage) (AppResponseUsage, error) {
	var wire appResponseUsageWire
	if common.Unmarshal(raw, &wire) != nil ||
		wire.InputTokens == nil || wire.OutputTokens == nil || wire.TotalTokens == nil {
		return AppResponseUsage{}, appAuthError("invalid_upstream_response")
	}
	usageJSON := gjson.ParseBytes(raw)
	inputDetailsJSON, _ := appResponseJSONField(usageJSON, "input_tokens_details")
	outputDetailsJSON, _ := appResponseJSONField(usageJSON, "output_tokens_details")
	if appResponseDetailsHaveNullCount(inputDetailsJSON) ||
		appResponseDetailsHaveNullCount(outputDetailsJSON) {
		return AppResponseUsage{}, appAuthError("invalid_upstream_response")
	}
	if !validAppResponseUsageCount(*wire.InputTokens) ||
		!validAppResponseUsageCount(*wire.OutputTokens) ||
		!validAppResponseUsageCount(*wire.TotalTokens) ||
		*wire.InputTokens+*wire.OutputTokens > *wire.TotalTokens {
		return AppResponseUsage{}, appAuthError("invalid_upstream_response")
	}
	input, ok := normalizeAppResponseDetails(wire.InputTokenDetails, *wire.InputTokens)
	if !ok {
		return AppResponseUsage{}, appAuthError("invalid_upstream_response")
	}
	output, ok := normalizeAppResponseDetails(wire.OutputTokenDetails, *wire.OutputTokens)
	if !ok {
		return AppResponseUsage{}, appAuthError("invalid_upstream_response")
	}
	cacheCreation := max(input.cachedCreation, input.cacheWrite)
	if input.text+input.image+input.audio > *wire.InputTokens ||
		output.text+output.image+output.audio > *wire.OutputTokens {
		return AppResponseUsage{}, appAuthError("invalid_upstream_response")
	}
	return AppResponseUsage{
		InputTokens:              int(*wire.InputTokens),
		OutputTokens:             int(*wire.OutputTokens),
		TotalTokens:              int(*wire.TotalTokens),
		CacheReadInputTokens:     int(input.cached),
		CacheCreationInputTokens: int(cacheCreation),
		TextInputTokens:          int(input.text),
		AudioInputTokens:         int(input.audio),
		ImageInputTokens:         int(input.image),
		TextOutputTokens:         int(output.text),
		AudioOutputTokens:        int(output.audio),
		ImageOutputTokens:        int(output.image),
		ReasoningOutputTokens:    int(output.reasoning),
	}, nil
}

type appResponseDetails struct {
	cached, cachedCreation, cacheWrite, text, audio, image, reasoning int64
}

func normalizeAppResponseDetails(wire *appResponseUsageDetails, total int64) (appResponseDetails, bool) {
	if wire == nil {
		return appResponseDetails{}, true
	}
	values := []*int64{
		wire.CachedTokens, wire.CachedCreationTokens, wire.CacheWriteTokens,
		wire.TextTokens, wire.AudioTokens, wire.ImageTokens, wire.ReasoningTokens,
	}
	for _, value := range values {
		if value != nil && (*value < 0 || *value > total) {
			return appResponseDetails{}, false
		}
	}
	return appResponseDetails{
		cached:         appResponseCount(wire.CachedTokens),
		cachedCreation: appResponseCount(wire.CachedCreationTokens),
		cacheWrite:     appResponseCount(wire.CacheWriteTokens),
		text:           appResponseCount(wire.TextTokens),
		audio:          appResponseCount(wire.AudioTokens),
		image:          appResponseCount(wire.ImageTokens),
		reasoning:      appResponseCount(wire.ReasoningTokens),
	}, true
}

func appResponseCount(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func validAppResponseUsageCount(value int64) bool {
	return value >= 0 && value <= int64(common.MaxQuota)
}

func appResponseDetailsHaveNullCount(details gjson.Result) bool {
	if !details.IsObject() {
		return false
	}
	invalid := false
	details.ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "cached_tokens", "cached_creation_tokens", "cache_write_tokens",
			"text_tokens", "audio_tokens", "image_tokens", "reasoning_tokens":
			invalid = value.Type == gjson.Null
		}
		return !invalid
	})
	return invalid
}

func appResponseJSONField(object gjson.Result, name string) (gjson.Result, bool) {
	var result gjson.Result
	found := false
	object.ForEach(func(key, value gjson.Result) bool {
		if key.Str == name {
			result, found = value, true
			return false
		}
		return true
	})
	return result, found
}

func ComputeAppResponseMaximumQuota(snapshot model.AppExecutionModel, facts AppResponseRequestFacts,
	pricedAt int64) (AppResponseQuotaQuote, error) {
	if !validAppResponsePriceInputs(snapshot, facts, pricedAt) {
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
	switch snapshot.BillingBasis {
	case billingexpr.BillingBasisRequest:
		return appResponsePerRequestQuota(snapshot)
	case billingexpr.BillingBasisToken:
		if appResponseTieredPrice(snapshot) {
			params := billingexpr.TokenParams{
				P: float64(facts.EstimatedInputTokens), C: float64(facts.MaxOutputTokens),
				Len: float64(facts.EstimatedInputTokens),
				CR:  float64(facts.EstimatedInputTokens), CC: float64(facts.EstimatedInputTokens),
				CC1h: float64(facts.EstimatedInputTokens), Img: float64(facts.EstimatedInputTokens),
				AI: float64(facts.EstimatedInputTokens), ImgO: float64(facts.MaxOutputTokens),
				AO: float64(facts.MaxOutputTokens),
			}
			return appResponseTieredMaximumQuota(snapshot, facts, params, pricedAt)
		}
		return appResponseRatioQuota(snapshot, AppResponseUsage{
			InputTokens:  facts.EstimatedInputTokens,
			OutputTokens: facts.MaxOutputTokens,
			TotalTokens:  facts.EstimatedInputTokens + facts.MaxOutputTokens,
		}, true)
	default:
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
}

func ComputeAppResponseFinalQuota(snapshot model.AppExecutionModel, facts AppResponseRequestFacts,
	usage AppResponseUsage, reservedQuota int, pricedAt int64) (AppResponseQuotaQuote, error) {
	maximum, err := ComputeAppResponseMaximumQuota(snapshot, facts, pricedAt)
	if err != nil {
		return AppResponseQuotaQuote{}, err
	}
	if reservedQuota != maximum.Quota ||
		!validNormalizedAppResponseUsage(usage) ||
		usage.InputTokens > facts.EstimatedInputTokens ||
		usage.OutputTokens > facts.MaxOutputTokens {
		return AppResponseQuotaQuote{}, appAuthError("invalid_usage_evidence")
	}

	var final AppResponseQuotaQuote
	switch snapshot.BillingBasis {
	case billingexpr.BillingBasisRequest:
		final, err = appResponsePerRequestQuota(snapshot)
	case billingexpr.BillingBasisToken:
		if appResponseTieredPrice(snapshot) {
			expression, _ := snapshot.Price["billing_setting.billing_expr"].(string)
			details := dto.InputTokenDetails{
				CachedTokens:         usage.CacheReadInputTokens,
				CachedCreationTokens: usage.CacheCreationInputTokens,
				TextTokens:           usage.TextInputTokens,
				AudioTokens:          usage.AudioInputTokens,
				ImageTokens:          usage.ImageInputTokens,
			}
			params := BuildTieredTokenParams(&dto.Usage{
				PromptTokens:        usage.InputTokens,
				CompletionTokens:    usage.OutputTokens,
				TotalTokens:         usage.TotalTokens,
				PromptTokensDetails: details,
				CompletionTokenDetails: dto.OutputTokenDetails{
					TextTokens:      usage.TextOutputTokens,
					AudioTokens:     usage.AudioOutputTokens,
					ImageTokens:     usage.ImageOutputTokens,
					ReasoningTokens: usage.ReasoningOutputTokens,
				},
			}, false, billingexpr.UsedVars(expression))
			final, err = appResponseTieredQuota(snapshot, params, pricedAt, "invalid_usage_evidence")
		} else {
			final, err = appResponseRatioQuota(snapshot, usage, false)
		}
	default:
		err = appAuthError("pricing_not_supported")
	}
	if err != nil {
		return AppResponseQuotaQuote{}, err
	}
	if final.Quota > reservedQuota {
		return AppResponseQuotaQuote{}, appAuthError("invalid_usage_evidence")
	}
	return final, nil
}

func validAppResponsePriceInputs(snapshot model.AppExecutionModel, facts AppResponseRequestFacts, pricedAt int64) bool {
	return snapshot.ExecutionKind == model.AppExecutionModelKindNativeResponse &&
		snapshot.Protocol == "openai_responses" &&
		snapshot.RequestProfile == appResponsesTextProfileV1 &&
		snapshot.ChannelType == constant.ChannelTypeOpenAI &&
		snapshot.APIType == constant.APITypeOpenAI &&
		snapshot.PublicModel != "" && snapshot.ActualModel != "" &&
		facts.EstimatedInputTokens > 0 && facts.EstimatedInputTokens <= constant.MaxTokensLimit &&
		facts.MaxOutputTokens > 0 && facts.MaxOutputTokens <= constant.MaxTokensLimit &&
		pricedAt > 0 &&
		snapshot.GroupRatio >= 0 && !math.IsNaN(snapshot.GroupRatio) && !math.IsInf(snapshot.GroupRatio, 0) &&
		snapshot.QuotaPerUnit > 0 && !math.IsNaN(snapshot.QuotaPerUnit) && !math.IsInf(snapshot.QuotaPerUnit, 0)
}

func validNormalizedAppResponseUsage(usage AppResponseUsage) bool {
	counts := []int{
		usage.InputTokens, usage.OutputTokens, usage.TotalTokens,
		usage.CacheReadInputTokens, usage.CacheCreationInputTokens,
		usage.TextInputTokens, usage.AudioInputTokens, usage.ImageInputTokens,
		usage.TextOutputTokens, usage.AudioOutputTokens, usage.ImageOutputTokens,
		usage.ReasoningOutputTokens,
	}
	for _, count := range counts {
		if count < 0 || count > common.MaxQuota {
			return false
		}
	}
	return int64(usage.InputTokens)+int64(usage.OutputTokens) <= int64(usage.TotalTokens) &&
		usage.CacheReadInputTokens <= usage.InputTokens &&
		usage.CacheCreationInputTokens <= usage.InputTokens &&
		usage.AudioInputTokens <= usage.InputTokens &&
		usage.ImageInputTokens <= usage.InputTokens &&
		usage.TextInputTokens <= usage.InputTokens &&
		usage.TextInputTokens+usage.AudioInputTokens+usage.ImageInputTokens <= usage.InputTokens &&
		usage.AudioOutputTokens <= usage.OutputTokens &&
		usage.ImageOutputTokens <= usage.OutputTokens &&
		usage.TextOutputTokens <= usage.OutputTokens &&
		usage.TextOutputTokens+usage.AudioOutputTokens+usage.ImageOutputTokens <= usage.OutputTokens &&
		usage.ReasoningOutputTokens <= usage.OutputTokens
}

func appResponsePerRequestQuota(snapshot model.AppExecutionModel) (AppResponseQuotaQuote, error) {
	if snapshot.ExprVersion != 0 || !exactAppResponsePrice(snapshot.Price, "ModelPrice") {
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
	price, ok := finiteAppExecutionPrice(snapshot.Price["ModelPrice"])
	if !ok {
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
	value := decimal.NewFromFloat(price).
		Mul(decimal.NewFromFloat(snapshot.QuotaPerUnit)).
		Mul(decimal.NewFromFloat(snapshot.GroupRatio))
	quota, clamp := common.QuotaFromDecimalChecked(value)
	if clamp != nil || quota < 0 {
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
	return AppResponseQuotaQuote{Quota: quota}, nil
}

func appResponseRatioQuota(snapshot model.AppExecutionModel, usage AppResponseUsage,
	maximum bool) (AppResponseQuotaQuote, error) {
	keys := []string{
		"ModelRatio", "CompletionRatio", "CacheRatio", "CreateCacheRatio",
		"ImageRatio", "AudioRatio", "AudioCompletionRatio",
	}
	if snapshot.ExprVersion != 0 || !exactAppResponsePrice(snapshot.Price, keys...) {
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
	values := make(map[string]decimal.Decimal, len(keys))
	for _, key := range keys {
		value, ok := finiteAppExecutionPrice(snapshot.Price[key])
		if !ok {
			return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
		}
		values[key] = decimal.NewFromFloat(value)
	}
	imageOutputFactor := values["ImageRatio"].Mul(values["CompletionRatio"])
	audioOutputFactor := values["AudioRatio"].Mul(values["AudioCompletionRatio"])
	inputDetailFactor := values["CacheRatio"].Add(values["CreateCacheRatio"]).
		Add(values["ImageRatio"]).Add(values["AudioRatio"])
	outputDetailFactor := imageOutputFactor.Add(audioOutputFactor)

	var weighted decimal.Decimal
	if maximum {
		inputFactor := decimal.NewFromInt(1)
		if inputDetailFactor.GreaterThan(inputFactor) {
			inputFactor = inputDetailFactor
		}
		outputFactor := values["CompletionRatio"]
		if outputDetailFactor.GreaterThan(outputFactor) {
			outputFactor = outputDetailFactor
		}
		weighted = decimal.NewFromInt(int64(usage.InputTokens)).Mul(inputFactor).
			Add(decimal.NewFromInt(int64(usage.OutputTokens)).Mul(outputFactor))
	} else {
		baseInput := max(usage.InputTokens-usage.CacheReadInputTokens-usage.CacheCreationInputTokens-
			usage.ImageInputTokens-usage.AudioInputTokens, 0)
		baseOutput := max(usage.OutputTokens-usage.ImageOutputTokens-usage.AudioOutputTokens, 0)
		weighted = decimal.NewFromInt(int64(baseInput)).
			Add(decimal.NewFromInt(int64(usage.CacheReadInputTokens)).Mul(values["CacheRatio"])).
			Add(decimal.NewFromInt(int64(usage.CacheCreationInputTokens)).Mul(values["CreateCacheRatio"])).
			Add(decimal.NewFromInt(int64(usage.ImageInputTokens)).Mul(values["ImageRatio"])).
			Add(decimal.NewFromInt(int64(usage.AudioInputTokens)).Mul(values["AudioRatio"])).
			Add(decimal.NewFromInt(int64(baseOutput)).Mul(values["CompletionRatio"])).
			Add(decimal.NewFromInt(int64(usage.ImageOutputTokens)).Mul(imageOutputFactor)).
			Add(decimal.NewFromInt(int64(usage.AudioOutputTokens)).Mul(audioOutputFactor))
	}
	quotaValue := weighted.
		Mul(values["ModelRatio"]).
		Mul(decimal.NewFromFloat(snapshot.GroupRatio))
	quota, clamp := common.QuotaFromDecimalChecked(quotaValue)
	if clamp != nil || quota < 0 {
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
	if quota == 0 && (usage.InputTokens > 0 || usage.OutputTokens > 0) &&
		values["ModelRatio"].IsPositive() && snapshot.GroupRatio > 0 {
		quota = 1
	}
	return AppResponseQuotaQuote{Quota: quota}, nil
}

func appResponseTieredPrice(snapshot model.AppExecutionModel) bool {
	mode, _ := snapshot.Price["billing_setting.billing_mode"].(string)
	return mode == "tiered_expr"
}

func appResponseTieredQuota(snapshot model.AppExecutionModel, params billingexpr.TokenParams,
	pricedAt int64, errorCode string) (AppResponseQuotaQuote, error) {
	if !exactAppResponsePrice(snapshot.Price, "billing_setting.billing_mode",
		"billing_setting.billing_expr", "billing_setting.billing_expr_sha256") {
		return AppResponseQuotaQuote{}, appAuthError(errorCode)
	}
	expression, expressionOK := snapshot.Price["billing_setting.billing_expr"].(string)
	hash, hashOK := snapshot.Price["billing_setting.billing_expr_sha256"].(string)
	if !expressionOK || expression == "" || !hashOK ||
		hash != billingexpr.ExprHashString(expression) ||
		snapshot.ExprVersion != billingexpr.ExprVersion(expression) ||
		!appResponseTieredExpressionSupported(expression) {
		return AppResponseQuotaQuote{}, appAuthError(errorCode)
	}
	result, err := billingexpr.ComputeTieredQuotaWithRequest(&billingexpr.BillingSnapshot{
		BillingMode:     "tiered_expr",
		ModelName:       snapshot.PublicModel,
		ExprString:      expression,
		ExprHash:        hash,
		GroupRatio:      snapshot.GroupRatio,
		QuotaPerUnit:    snapshot.QuotaPerUnit,
		ExprVersion:     snapshot.ExprVersion,
		BillingBasis:    billingexpr.BillingBasisToken,
		PricingTimeUnix: pricedAt,
	}, params, billingexpr.RequestInput{EvaluatedAtUnix: pricedAt})
	if err != nil || result.Clamp != nil || result.ActualQuotaAfterGroup < 0 {
		return AppResponseQuotaQuote{}, appAuthError(errorCode)
	}
	return AppResponseQuotaQuote{
		Quota:        result.ActualQuotaAfterGroup,
		MatchedTier:  result.MatchedTier,
		RequestRules: result.RequestRules,
	}, nil
}

func appResponseTieredMaximumQuota(snapshot model.AppExecutionModel, facts AppResponseRequestFacts,
	params billingexpr.TokenParams, pricedAt int64) (AppResponseQuotaQuote, error) {
	evaluated, err := appResponseTieredQuota(snapshot, params, pricedAt, "pricing_not_supported")
	if err != nil {
		return AppResponseQuotaQuote{}, err
	}
	expression, _ := snapshot.Price["billing_setting.billing_expr"].(string)
	hash, _ := snapshot.Price["billing_setting.billing_expr_sha256"].(string)
	program, err := billingexpr.CompileFromCacheByHash(expression, hash)
	if err != nil {
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
	inputMax := float64(facts.EstimatedInputTokens)
	outputMax := float64(facts.MaxOutputTokens)
	bounds := map[string]appResponseInterval{
		"p": {high: inputMax}, "len": {high: inputMax},
		"cr": {high: inputMax}, "cc": {high: inputMax}, "cc1h": {high: inputMax},
		"img": {high: inputMax}, "ai": {high: inputMax},
		"c": {high: outputMax}, "img_o": {high: outputMax}, "ao": {high: outputMax},
	}
	interval, ok := appResponseTokenInterval(program.Node(), bounds)
	if !ok || interval.high < 0 {
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
	quota, clamp := common.QuotaRoundChecked(interval.high / 1_000_000 *
		snapshot.QuotaPerUnit * snapshot.GroupRatio)
	if clamp != nil || quota < evaluated.Quota {
		return AppResponseQuotaQuote{}, appAuthError("pricing_not_supported")
	}
	evaluated.Quota = quota
	return evaluated, nil
}

type appResponseInterval struct {
	low  float64
	high float64
}

func appResponseTieredExpressionSupported(expression string) bool {
	used := billingexpr.UsedVars(expression)
	for _, name := range []string{
		"param", "header", "has", "unix", "hour", "minute", "weekday", "month", "day", "u",
	} {
		if used[name] {
			return false
		}
	}
	program, err := billingexpr.CompileFromCache(expression)
	if err != nil {
		return false
	}
	unitBounds := map[string]appResponseInterval{}
	for _, name := range []string{"p", "c", "len", "cr", "cc", "cc1h", "img", "img_o", "ai", "ao"} {
		unitBounds[name] = appResponseInterval{high: float64(constant.MaxTokensLimit)}
	}
	interval, ok := appResponseTokenInterval(program.Node(), unitBounds)
	return ok && interval.low >= 0
}

func appResponseTokenInterval(node ast.Node, bounds map[string]appResponseInterval) (appResponseInterval, bool) {
	switch value := node.(type) {
	case *ast.IntegerNode:
		return appResponseFiniteInterval(float64(value.Value), float64(value.Value))
	case *ast.FloatNode:
		return appResponseFiniteInterval(value.Value, value.Value)
	case *ast.ConstantNode:
		number, ok := appResponseConstantNumber(value.Value)
		if !ok {
			return appResponseInterval{}, false
		}
		return appResponseFiniteInterval(number, number)
	case *ast.IdentifierNode:
		bound, ok := bounds[value.Value]
		return bound, ok
	case *ast.UnaryNode:
		inner, ok := appResponseTokenInterval(value.Node, bounds)
		if !ok {
			return appResponseInterval{}, false
		}
		switch value.Operator {
		case "+":
			return inner, true
		case "-":
			return appResponseFiniteInterval(-inner.high, -inner.low)
		default:
			return appResponseInterval{}, false
		}
	case *ast.BinaryNode:
		left, leftOK := appResponseTokenInterval(value.Left, bounds)
		right, rightOK := appResponseTokenInterval(value.Right, bounds)
		if !leftOK || !rightOK {
			return appResponseInterval{}, false
		}
		switch value.Operator {
		case "+":
			return appResponseFiniteInterval(left.low+right.low, left.high+right.high)
		case "-":
			return appResponseFiniteInterval(left.low-right.high, left.high-right.low)
		case "*":
			products := []float64{
				left.low * right.low, left.low * right.high,
				left.high * right.low, left.high * right.high,
			}
			return appResponseFiniteInterval(
				min(products[0], products[1], products[2], products[3]),
				max(products[0], products[1], products[2], products[3]),
			)
		case "/":
			if right.low != right.high || right.low == 0 {
				return appResponseInterval{}, false
			}
			quotients := []float64{
				left.low / right.low, left.low / right.high,
				left.high / right.low, left.high / right.high,
			}
			return appResponseFiniteInterval(
				min(quotients[0], quotients[1], quotients[2], quotients[3]),
				max(quotients[0], quotients[1], quotients[2], quotients[3]),
			)
		default:
			return appResponseInterval{}, false
		}
	case *ast.ConditionalNode:
		first, firstOK := appResponseTokenInterval(value.Exp1, bounds)
		second, secondOK := appResponseTokenInterval(value.Exp2, bounds)
		if !firstOK || !secondOK {
			return appResponseInterval{}, false
		}
		return appResponseFiniteInterval(min(first.low, second.low), max(first.high, second.high))
	case *ast.CallNode:
		callee, ok := value.Callee.(*ast.IdentifierNode)
		if !ok {
			return appResponseInterval{}, false
		}
		return appResponseCallInterval(callee.Value, value.Arguments, bounds)
	case *ast.BuiltinNode:
		return appResponseCallInterval(value.Name, value.Arguments, bounds)
	default:
		return appResponseInterval{}, false
	}
}

func appResponseCallInterval(name string, arguments []ast.Node,
	bounds map[string]appResponseInterval) (appResponseInterval, bool) {
	if name == "tier" {
		if len(arguments) != 2 {
			return appResponseInterval{}, false
		}
		return appResponseTokenInterval(arguments[1], bounds)
	}
	if len(arguments) == 1 {
		value, ok := appResponseTokenInterval(arguments[0], bounds)
		if !ok {
			return appResponseInterval{}, false
		}
		switch name {
		case "abs":
			low := min(math.Abs(value.low), math.Abs(value.high))
			if value.low <= 0 && value.high >= 0 {
				low = 0
			}
			return appResponseFiniteInterval(low, max(math.Abs(value.low), math.Abs(value.high)))
		case "ceil":
			return appResponseFiniteInterval(math.Ceil(value.low), math.Ceil(value.high))
		case "floor":
			return appResponseFiniteInterval(math.Floor(value.low), math.Floor(value.high))
		}
	}
	if len(arguments) == 2 && (name == "max" || name == "min") {
		left, leftOK := appResponseTokenInterval(arguments[0], bounds)
		right, rightOK := appResponseTokenInterval(arguments[1], bounds)
		if !leftOK || !rightOK {
			return appResponseInterval{}, false
		}
		if name == "max" {
			return appResponseFiniteInterval(max(left.low, right.low), max(left.high, right.high))
		}
		return appResponseFiniteInterval(min(left.low, right.low), min(left.high, right.high))
	}
	return appResponseInterval{}, false
}

func appResponseFiniteInterval(low, high float64) (appResponseInterval, bool) {
	if math.IsNaN(low) || math.IsNaN(high) || math.IsInf(low, 0) || math.IsInf(high, 0) || low > high {
		return appResponseInterval{}, false
	}
	return appResponseInterval{low: low, high: high}, true
}

func appResponseConstantNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}

func appResponseLiteralRequestKeys(raw []byte) bool {
	value := gjson.ParseBytes(raw)
	valid := true
	value.ForEach(func(key, _ gjson.Result) bool {
		switch key.Raw {
		case `"model"`, `"input"`, `"max_output_tokens"`, `"stream"`, `"store"`:
			return true
		default:
			valid = false
			return false
		}
	})
	return valid
}

func exactAppResponsePrice(price model.PricingValues, keys ...string) bool {
	if len(price) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, present := price[key]; !present {
			return false
		}
	}
	return true
}
