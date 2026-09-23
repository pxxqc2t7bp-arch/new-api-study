package service

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrdinaryConversionCapabilitiesCoverEveryDirectedProtocolPair(t *testing.T) {
	t.Parallel()

	formats := []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatOpenAIResponses,
		types.RelayFormatClaude,
		types.RelayFormatGemini,
	}
	capabilities := OrdinaryConversionCapabilities()

	require.Len(t, capabilities, 12)
	for _, from := range formats {
		for _, to := range formats {
			if from == to {
				continue
			}
			capability, ok := LookupOrdinaryConversionCapability(from, to)
			require.True(t, ok, "%s -> %s", from, to)
			assert.Equal(t, from, capability.From)
			assert.Equal(t, to, capability.To)
			assert.NotEmpty(t, capability.Converter)
			assert.NotEmpty(t, capability.Supported)
		}
	}
}

func TestResolveOrdinaryConversionPolicyRequiresAuthorizationForLossyStrategies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		requested  types.ConversionLossPolicy
		authorized bool
		want       types.ConversionLossPolicy
		wantError  bool
	}{
		{name: "default", want: types.ConversionLossPolicyStrict},
		{name: "strict", requested: types.ConversionLossPolicyStrict, want: types.ConversionLossPolicyStrict},
		{name: "safe unauthorized", requested: types.ConversionLossPolicySafe, wantError: true},
		{name: "allow unauthorized", requested: types.ConversionLossPolicyAllow, wantError: true},
		{name: "safe authorized", requested: types.ConversionLossPolicySafe, authorized: true, want: types.ConversionLossPolicySafe},
		{name: "allow authorized", requested: types.ConversionLossPolicyAllow, authorized: true, want: types.ConversionLossPolicyAllow},
		{name: "invalid", requested: types.ConversionLossPolicy("invalid"), authorized: true, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveOrdinaryConversionPolicy(test.requested, test.authorized)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestOrdinaryConversionCapabilitiesMatchRuntimeDiagnostics(t *testing.T) {
	responsesToGemini, ok := LookupOrdinaryConversionCapability(types.RelayFormatOpenAIResponses, types.RelayFormatGemini)
	require.True(t, ok)

	result, err := relayconvert.ConvertRequest(context.Background(), conversionAllowInfo("gemini-test", "gpt-test"), types.RelayFormatGemini, &dto.OpenAIResponsesRequest{
		Model: "gemini-test",
		Input: []byte(`"hello"`),
		Tools: []byte(`[
			{"type":"custom","name":"apply_patch"},
			{"type":"unknown","name":"unknown"},
			{"type":"opaque_preview","name":"opaque"}
		]`),
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	for _, expected := range []struct {
		code       string
		rejections bool
	}{
		{code: types.ConversionDiagnosticCodeCustomToolOmitted},
		{code: types.ConversionDiagnosticCodeUnsupportedOpaqueTool, rejections: true},
	} {
		assert.True(t, hasConversionDiagnosticCode(result.Diagnostics, expected.code), "runtime diagnostic %s", expected.code)
		losses := responsesToGemini.Losses
		if expected.rejections {
			losses = responsesToGemini.Rejections
		}
		assert.Equal(t, ConversionFeatureFunctionTools, conversionLossByCode(t, losses, expected.code).Feature)
	}

	statefulRequest := &dto.OpenAIResponsesRequest{
		Model:              "gpt-test",
		Input:              []byte(`"hello"`),
		Conversation:       []byte(`"conv_1"`),
		PreviousResponseID: "resp_1",
		Prompt:             []byte(`{"id":"pmpt_1"}`),
		ContextManagement:  []byte(`{"type":"auto"}`),
	}
	wantPaths := []string{"conversation", "previous_response_id", "prompt", "context_management"}
	for _, target := range []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatClaude,
		types.RelayFormatGemini,
	} {
		t.Run("responses state to "+string(target), func(t *testing.T) {
			result, err := relayconvert.ConvertRequest(context.Background(), conversionAllowInfo(string(target), "gpt-test"), target, statefulRequest)
			require.Error(t, err)
			assert.Nil(t, result, "unsupported private state must fail before producing an upstream request")

			var loss *types.ConversionLossError
			require.ErrorAs(t, err, &loss)
			require.Len(t, loss.Diagnostics, len(wantPaths))
			for index, diagnostic := range loss.Diagnostics {
				assert.Equal(t, types.ConversionDiagnosticCodeSessionReferenceUnsupported, diagnostic.Code)
				assert.Equal(t, wantPaths[index], diagnostic.Path)
				assert.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), diagnostic.From)
				assert.Equal(t, target, diagnostic.To)
			}

			capability, ok := LookupOrdinaryConversionCapability(types.RelayFormatOpenAIResponses, target)
			require.True(t, ok)
			assert.Equal(t, ConversionFeatureSessionState, conversionLossByCode(
				t,
				capability.Rejections,
				types.ConversionDiagnosticCodeSessionReferenceUnsupported,
			).Feature)
		})
	}

	for _, sampling := range []struct {
		name  string
		model string
		code  string
	}{
		{
			name:  "removed",
			model: "claude-opus-4-8",
			code:  types.ConversionDiagnosticCodeClaudeSamplingRemoved,
		},
		{
			name:  "constrained",
			model: "claude-sonnet-4-6",
			code:  types.ConversionDiagnosticCodeClaudeSamplingConstrained,
		},
	} {
		t.Run("Claude sampling "+sampling.name, func(t *testing.T) {
			for _, from := range []types.RelayFormat{
				types.RelayFormatOpenAI,
				types.RelayFormatOpenAIResponses,
				types.RelayFormatGemini,
			} {
				t.Run(string(from), func(t *testing.T) {
					request, info := samplingConversionRequest(from, sampling.model, sampling.name == "constrained")
					result, err := relayconvert.ConvertRequest(context.Background(), info, types.RelayFormatClaude, request)
					require.NoError(t, err)
					require.NotNil(t, result)
					assert.True(t, hasConversionDiagnosticCode(result.Diagnostics, sampling.code), "runtime diagnostic %s; got %+v", sampling.code, result.Diagnostics)

					capability, ok := LookupOrdinaryConversionCapability(from, types.RelayFormatClaude)
					require.True(t, ok)
					assert.Equal(t, ConversionFeatureOptionalScalars, conversionLossByCode(t, capability.Losses, sampling.code).Feature)
				})
			}
		})
	}

	assert.ElementsMatch(t, []string{
		types.ConversionDiagnosticCodeUnsupportedFunctionStrict,
		types.ConversionDiagnosticCodeCustomToolOmitted,
	}, conversionLossCodes(responsesToGemini.Losses))
	assert.ElementsMatch(t, []string{
		types.ConversionDiagnosticCodeEncryptedReasoningUnsupported,
		types.ConversionDiagnosticCodeUnsupportedHostedTool,
		types.ConversionDiagnosticCodeUnverifiedToolMapping,
		types.ConversionDiagnosticCodeUnsupportedParallelToolControl,
		types.ConversionDiagnosticCodeUnsupportedOpaqueTool,
		types.ConversionDiagnosticCodeSessionReferenceUnsupported,
	}, conversionLossCodes(responsesToGemini.Rejections))

	geminiToChat, ok := LookupOrdinaryConversionCapability(types.RelayFormatGemini, types.RelayFormatOpenAI)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{types.ConversionDiagnosticCodeStructuredOutputUnsupported}, conversionLossCodes(geminiToChat.Losses))
	assert.ElementsMatch(t, []string{
		types.ConversionDiagnosticCodeEncryptedReasoningUnsupported,
		types.ConversionDiagnosticCodeSessionReferenceUnsupported,
		types.ConversionDiagnosticCodeUnsupportedHostedTool,
	}, conversionLossCodes(geminiToChat.Rejections))

	chatToResponses, ok := LookupOrdinaryConversionCapability(types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses)
	require.True(t, ok)
	assert.Empty(t, chatToResponses.Losses)
	assert.ElementsMatch(t, []string{types.ConversionDiagnosticCodeUnsupportedHostedTool}, conversionLossCodes(chatToResponses.Rejections))
	assert.Contains(t, chatToResponses.Supported, ConversionFeatureStructuredOutput)
	assert.Contains(t, chatToResponses.Supported, ConversionFeatureParallelToolCalls)
}

func TestOrdinaryConversionCapabilitiesAttachDiagnosticsToMatchingFeatures(t *testing.T) {
	t.Parallel()

	claudeToGemini, ok := LookupOrdinaryConversionCapability(types.RelayFormatClaude, types.RelayFormatGemini)
	require.True(t, ok)

	assert.Equal(t, ConversionFeatureStructuredOutput, conversionLossByCode(t, claudeToGemini.Losses, types.ConversionDiagnosticCodeStructuredOutputUnsupported).Feature)
	assert.Equal(t, ConversionFeatureFunctionTools, conversionLossByCode(t, claudeToGemini.Losses, types.ConversionDiagnosticCodeUnsupportedFunctionStrict).Feature)
	assert.Equal(t, ConversionFeatureParallelToolCalls, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeUnsupportedParallelToolControl).Feature)
	assert.Equal(t, ConversionFeatureFunctionTools, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeUnverifiedToolMapping).Feature)
	assert.Equal(t, ConversionFeatureFunctionTools, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeUnsupportedHostedTool).Feature)
	assert.Equal(t, ConversionFeatureFunctionTools, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeVendorSpecificToolUnsupported).Feature)
	assert.Equal(t, ConversionFeatureReasoning, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeEncryptedReasoningUnsupported).Feature)
	assert.Equal(t, ConversionFeatureSessionState, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeSessionReferenceUnsupported).Feature)
}

func conversionLossCodes(losses []OrdinaryConversionLoss) []string {
	codes := make([]string, 0, len(losses))
	for _, loss := range losses {
		codes = append(codes, loss.Code)
	}
	return codes
}

func conversionLossByCode(t *testing.T, losses []OrdinaryConversionLoss, code string) OrdinaryConversionLoss {
	t.Helper()
	for _, loss := range losses {
		if loss.Code == code {
			return loss
		}
	}
	require.FailNow(t, "missing conversion loss", "code=%s", code)
	return OrdinaryConversionLoss{}
}

func hasConversionDiagnosticCode(diagnostics []types.ConversionDiagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func conversionAllowInfo(upstreamModel string, originModel string) *convmeta.Values {
	return &convmeta.Values{
		OriginModelName:     originModel,
		UpstreamModelName:   upstreamModel,
		ChannelMetaAttached: true,
		Options: &convmeta.Options{
			ToolLossPolicy: types.ConversionLossPolicyAllow,
			Claude: convmeta.ClaudeOptions{
				DefaultMaxTokens: func(string) int { return 4096 },
			},
		},
	}
}

func samplingConversionRequest(from types.RelayFormat, model string, constrained bool) (any, *convmeta.Values) {
	temperature := 0.7
	maxTokens := uint(4096)
	info := conversionAllowInfo(model, model)

	switch from {
	case types.RelayFormatOpenAI:
		request := &dto.GeneralOpenAIRequest{
			Model:       model,
			Messages:    []dto.Message{{Role: "user", Content: "hello"}},
			MaxTokens:   &maxTokens,
			Temperature: &temperature,
		}
		if constrained {
			request.ReasoningEffort = "high"
		}
		return request, info
	case types.RelayFormatOpenAIResponses:
		request := &dto.OpenAIResponsesRequest{
			Model:           model,
			Input:           []byte(`"hello"`),
			MaxOutputTokens: &maxTokens,
			Temperature:     &temperature,
		}
		if constrained {
			request.Reasoning = &dto.Reasoning{Effort: "high"}
		}
		return request, info
	case types.RelayFormatGemini:
		info.OriginModelName = "gemini-2.5-pro"
		request := &dto.GeminiChatRequest{
			Contents: []dto.GeminiChatContent{{Role: "user", Parts: []dto.GeminiPart{{Text: "hello"}}}},
			GenerationConfig: dto.GeminiChatGenerationConfig{
				MaxOutputTokens: &maxTokens,
				Temperature:     &temperature,
			},
		}
		if constrained {
			budget := 2048
			request.GenerationConfig.ThinkingConfig = &dto.GeminiThinkingConfig{ThinkingBudget: &budget}
		}
		return request, info
	default:
		panic("unsupported sampling conversion source")
	}
}
