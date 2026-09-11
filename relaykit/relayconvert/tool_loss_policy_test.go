package relayconvert

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertRequestAuthorizedAllowPolicyReturnsGeminiCodeExecutionLoss(t *testing.T) {
	t.Parallel()

	tools, err := kitutil.Marshal([]map[string]any{{"codeExecution": map[string]any{}}})
	require.NoError(t, err)
	req := &dto.GeminiChatRequest{
		Contents: []dto.GeminiChatContent{
			{Role: "user", Parts: []dto.GeminiPart{{Text: "run this"}}},
		},
		Tools: tools,
	}

	info := &convmeta.Values{
		Options: &convmeta.Options{ToolLossPolicy: types.ConversionLossPolicyAllow},
	}
	result, err := ConvertRequest(nil, info, types.RelayFormatOpenAI, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.IsType(t, &dto.GeneralOpenAIRequest{}, result.Value)
	assert.True(t, hasConversionDiagnosticCode(result.Diagnostics, "unsupported_hosted_tool"))
}

func TestConvertResponseStrictPolicyStillSucceedsOnContinuationLoss(t *testing.T) {
	t.Parallel()

	text := "hello"
	resp := &dto.ClaudeResponse{
		Id:         "msg_1",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-test",
		StopReason: "pause_turn",
		Content: []dto.ClaudeMediaMessage{
			{Type: "redacted_thinking", Data: "secret"},
			{Type: "text", Text: &text},
		},
	}
	info := &convmeta.Values{
		Options: &convmeta.Options{ToolLossPolicy: types.ConversionLossPolicyStrict},
	}

	result, err := ConvertResponse(nil, info, types.RelayFormatOpenAI, resp)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.IsType(t, &dto.OpenAITextResponse{}, result.Value)
	assert.True(t, hasConversionDiagnosticCode(result.Diagnostics, "continuation_state_lost"))
}

func TestConvertRequestSafePolicyReturnsConversionLossError(t *testing.T) {
	t.Parallel()

	tools, err := kitutil.Marshal([]map[string]any{{"codeExecution": map[string]any{}}})
	require.NoError(t, err)
	req := &dto.GeminiChatRequest{
		Contents: []dto.GeminiChatContent{
			{Role: "user", Parts: []dto.GeminiPart{{Text: "run this"}}},
		},
		Tools: tools,
	}
	info := &convmeta.Values{
		Options: &convmeta.Options{ToolLossPolicy: types.ConversionLossPolicySafe},
	}

	result, err := ConvertRequest(nil, info, types.RelayFormatOpenAI, req)
	require.Error(t, err)
	var loss *types.ConversionLossError
	require.ErrorAs(t, err, &loss)
	require.NotEmpty(t, loss.Diagnostics)
	require.NotNil(t, result)
	assert.True(t, hasConversionDiagnosticCode(loss.Diagnostics, "unsupported_hosted_tool"))
	assert.True(t, hasConversionDiagnosticCode(result.Diagnostics, "unsupported_hosted_tool"))
}

func TestConvertRequestDefaultStrictRejectsEncryptedReasoning(t *testing.T) {
	t.Parallel()

	req := &dto.ClaudeRequest{
		Model: "claude-test",
		Messages: []dto.ClaudeMessage{{
			Role: "assistant",
			Content: []dto.ClaudeMediaMessage{{
				Type: "redacted_thinking",
				Data: "encrypted-state",
			}},
		}},
	}

	result, err := ConvertRequest(nil, &convmeta.Values{}, types.RelayFormatOpenAI, req)
	require.Error(t, err)
	var loss *types.ConversionLossError
	require.ErrorAs(t, err, &loss)
	require.NotNil(t, result)
	assert.True(t, hasConversionDiagnosticCode(loss.Diagnostics, "encrypted_reasoning_unsupported"))
	assert.True(t, hasConversionDiagnosticCode(result.Diagnostics, "encrypted_reasoning_unsupported"))
}

func TestConvertRequestDefaultStrictRejectsProviderSessionReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request any
		target  types.RelayFormat
	}{
		{
			name: "Claude context management",
			request: &dto.ClaudeRequest{
				Model:             "claude-test",
				ContextManagement: []byte(`{"edits":[{"type":"clear_tool_uses_20250919"}]}`),
				Messages:          []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
			},
			target: types.RelayFormatOpenAI,
		},
		{
			name: "Gemini cached content",
			request: &dto.GeminiChatRequest{
				CachedContent: "cachedContents/session-1",
				Contents:      []dto.GeminiChatContent{{Role: "user", Parts: []dto.GeminiPart{{Text: "hello"}}}},
			},
			target: types.RelayFormatOpenAI,
		},
		{
			name: "Claude container",
			request: &dto.ClaudeRequest{
				Model:     "claude-test",
				Container: []byte(`"container_1"`),
				Messages:  []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
			},
			target: types.RelayFormatOpenAI,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ConvertRequest(nil, &convmeta.Values{}, test.target, test.request)
			require.Error(t, err)
			var loss *types.ConversionLossError
			require.ErrorAs(t, err, &loss)
			require.NotNil(t, result)
			assert.True(t, hasConversionDiagnosticCode(result.Diagnostics, "session_reference_unsupported"))
		})
	}
}

func TestConvertRequestDefaultStrictRejectsOpaqueVendorState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		request  any
		target   types.RelayFormat
		lossCode string
	}{
		{
			name: "Claude MCP servers",
			request: &dto.ClaudeRequest{
				Model:      "claude-test",
				McpServers: []byte(`[{"type":"url","url":"https://mcp.example"}]`),
				Messages:   []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
			},
			target:   types.RelayFormatOpenAI,
			lossCode: "vendor_specific_tool_unsupported",
		},
		{
			name: "Gemini thought signature",
			request: &dto.GeminiChatRequest{
				Contents: []dto.GeminiChatContent{{
					Role: "model",
					Parts: []dto.GeminiPart{{
						Text:             "thought",
						ThoughtSignature: []byte(`"signed-state"`),
					}},
				}},
			},
			target:   types.RelayFormatOpenAI,
			lossCode: "encrypted_reasoning_unsupported",
		},
		{
			name: "OpenAI Responses encrypted reasoning",
			request: &dto.OpenAIResponsesRequest{
				Model: "gpt-test",
				Input: []byte(`[{"type":"reasoning","encrypted_content":"opaque-state"}]`),
			},
			target:   types.RelayFormatOpenAI,
			lossCode: "encrypted_reasoning_unsupported",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ConvertRequest(nil, &convmeta.Values{}, test.target, test.request)
			require.Error(t, err)
			var loss *types.ConversionLossError
			require.ErrorAs(t, err, &loss)
			require.NotNil(t, result)
			assert.True(t, hasConversionDiagnosticCode(result.Diagnostics, test.lossCode))
		})
	}
}

func TestConvertRequestAllowReportsUnsupportedStructuredOutput(t *testing.T) {
	t.Parallel()

	maxTokens := uint(1024)
	tests := []struct {
		name    string
		request any
		target  types.RelayFormat
		policy  types.ConversionLossPolicy
	}{
		{
			name: "OpenAI Chat to Claude",
			request: &dto.GeneralOpenAIRequest{
				Model: "claude-test", MaxTokens: &maxTokens, Messages: []dto.Message{{Role: "user", Content: "hello"}},
				ResponseFormat: &dto.ResponseFormat{Type: "json_object"},
			},
			target: types.RelayFormatClaude,
			policy: types.ConversionLossPolicySafe,
		},
		{
			name: "OpenAI Responses to Claude",
			request: &dto.OpenAIResponsesRequest{
				Model: "claude-test", MaxOutputTokens: &maxTokens, Input: []byte(`"hello"`),
				Text: []byte(`{"format":{"type":"json_object"}}`),
			},
			target: types.RelayFormatClaude,
			policy: types.ConversionLossPolicyAllow,
		},
		{
			name: "Claude to OpenAI Chat",
			request: &dto.ClaudeRequest{
				Model: "gpt-test", MaxTokens: &maxTokens, Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
				OutputFormat: []byte(`{"type":"json_schema","schema":{"type":"object"}}`),
			},
			target: types.RelayFormatOpenAI,
			policy: types.ConversionLossPolicyAllow,
		},
		{
			name: "Gemini to OpenAI Chat",
			request: &dto.GeminiChatRequest{
				Contents: []dto.GeminiChatContent{{Role: "user", Parts: []dto.GeminiPart{{Text: "hello"}}}},
				GenerationConfig: dto.GeminiChatGenerationConfig{
					ResponseMimeType: "application/json",
					ResponseSchema:   map[string]any{"type": "object"},
				},
			},
			target: types.RelayFormatOpenAI,
			policy: types.ConversionLossPolicyAllow,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := &convmeta.Values{Options: &convmeta.Options{ToolLossPolicy: test.policy}}
			result, err := ConvertRequest(nil, info, test.target, test.request)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.True(t, hasConversionDiagnosticCode(result.Diagnostics, "structured_output_unsupported"))
		})
	}
}

func hasConversionDiagnosticCode(diagnostics []types.ConversionDiagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
