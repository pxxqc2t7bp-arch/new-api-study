package relayconvert

// golden_test.go pins the byte-level output of selected public conversion
// routes. Run with -update to regenerate testdata/golden.
//
// Volatile values (generated UUID-based ids, unix timestamps) are normalized
// before comparison so the snapshots are deterministic.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update", false, "update golden files")

// TestMain installs a deterministic media resolver so image-bearing fixtures
// convert without network access.
func TestMain(m *testing.M) {
	flag.Parse()
	SetMediaResolver(MediaResolver{
		GetBase64Data: func(c context.Context, source types.FileSource, reason ...string) (string, string, error) {
			return "aGVsbG8=", "image/png", nil
		},
		DecodeBase64FileData: func(base64String string) (string, string, error) {
			return "aGVsbG8=", "image/png", nil
		},
	})
	os.Exit(m.Run())
}

const goldenDir = "testdata/golden"

var (
	hex32Re     = regexp.MustCompile(`[0-9a-f]{32}`)
	timestampRe = regexp.MustCompile(`("created(_at)?"\s*:\s*)\d{9,}`)
)

func normalizeVolatile(data []byte) []byte {
	data = hex32Re.ReplaceAll(data, []byte("<uuid>"))
	data = timestampRe.ReplaceAll(data, []byte(`${1}0`))
	return data
}

func marshalGolden(t *testing.T, value any) []byte {
	t.Helper()
	data, err := kitutil.Marshal(value)
	require.NoError(t, err)
	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, data, "", "  "))
	return append(normalizeVolatile(indented.Bytes()), '\n')
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".golden.json")
	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "golden file missing, run: cd relaykit && GOWORK=off go test ./relayconvert -run TestGolden -update")
	require.Equal(t, string(want), string(got), "conversion output drifted from golden snapshot %s", path)
}

// goldenInfo explicitly authorizes loss reporting so every matrix route can
// snapshot both its converted value and any non-portable semantics.
func goldenInfo() convmeta.Meta {
	return &convmeta.Values{
		ChannelMetaAttached: true,
		UpstreamModelName:   "upstream-model",
		ClaudeConvertInfo: &convmeta.ClaudeConvertInfo{
			LastMessagesType: convmeta.LastMessageTypeNone,
		},
		Options: &convmeta.Options{
			ToolLossPolicy: types.ConversionLossPolicyAllow,
			Gemini: convmeta.GeminiOptions{
				ThinkingAdapterBudgetTokensPercentage: 0.6,
				FunctionCallThoughtSignatureEnabled:   true,
				SafetySetting:                         func(string) string { return "OFF" },
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Fixtures: one representative rich request per source format
// ---------------------------------------------------------------------------

// Fixtures are built by unmarshalling wire-format JSON into the dto types —
// the same path production requests take — so they stay valid as struct
// internals evolve.
func fixtureRequests() map[types.RelayFormat]any {
	openai := &dto.GeneralOpenAIRequest{}
	mustUnmarshalFixture(`{
		"model": "gpt-test",
		"max_tokens": 1024,
		"temperature": 0,
		"top_p": 0,
		"parallel_tool_calls": false,
		"stream": true,
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": [
				{"type": "text", "text": "What is in this image?"},
				{"type": "image_url", "image_url": {"url": "https://example.com/cat.png", "detail": "high"}}
			]},
			{"role": "assistant", "tool_calls": [{"id": "call_abc", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}]},
			{"role": "tool", "tool_call_id": "call_abc", "content": "15 degrees"},
			{"role": "user", "content": "Summarize."}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "Get weather by city", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}],
		"tool_choice": {"type": "function", "function": {"name": "get_weather"}},
		"response_format": {"type": "json_schema", "json_schema": {"name": "weather", "strict": true, "schema": {"type": "object", "properties": {"summary": {"type": "string"}}, "required": ["summary"], "additionalProperties": false}}}
	}`, openai)

	claude := &dto.ClaudeRequest{}
	mustUnmarshalFixture(`{
		"model": "claude-test",
		"max_tokens": 1024,
		"temperature": 0,
		"top_p": 0,
		"stream": false,
		"system": "You are a helpful assistant.",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "What is in this image?"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}
			]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_abc", "name": "get_weather", "input": {"city": "Paris"}}
			]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_abc", "content": "15 degrees"}]}
		],
		"tools": [{"name": "get_weather", "description": "Get weather by city", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}, "strict": true}],
		"tool_choice": {"type": "tool", "name": "get_weather", "disable_parallel_tool_use": false},
		"output_format": {"type": "json_schema", "schema": {"type": "object", "properties": {"summary": {"type": "string"}}}}
	}`, claude)

	gemini := &dto.GeminiChatRequest{}
	mustUnmarshalFixture(`{
		"contents": [
			{"role": "user", "parts": [
				{"text": "What is in this image?"},
				{"inlineData": {"mimeType": "image/png", "data": "aGVsbG8="}}
			]},
			{"role": "model", "parts": [
				{"functionCall": {"id": "call_abc", "name": "get_weather", "args": {"city": "Paris"}}},
				{"functionCall": {"id": "call_xyz", "name": "get_weather", "args": {"city": "London"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"id": "call_abc", "name": "get_weather", "response": {"result": "15 degrees"}}},
				{"functionResponse": {"id": "call_xyz", "name": "get_weather", "response": {"result": "12 degrees"}}}
			]}
		],
		"systemInstruction": {"parts": [{"text": "You are a helpful assistant."}]},
		"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "Get weather by city", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["get_weather"]}},
		"generationConfig": {"maxOutputTokens": 1024, "temperature": 0, "topP": 0, "responseMimeType": "application/json", "responseSchema": {"type": "object", "properties": {"summary": {"type": "string"}}}}
	}`, gemini)

	responses := &dto.OpenAIResponsesRequest{}
	mustUnmarshalFixture(`{
		"model": "gpt-test",
		"stream": true,
		"max_output_tokens": 1024,
		"temperature": 0,
		"top_p": 0,
		"parallel_tool_calls": false,
		"instructions": "You are a helpful assistant.",
		"input": [
			{"type": "message", "role": "user", "content": [
				{"type": "input_text", "text": "What is in this image?"},
				{"type": "input_image", "image_url": "https://example.com/cat.png"}
			]},
			{"type": "function_call", "call_id": "call_abc", "name": "get_weather", "arguments": "{\"city\":\"Paris\"}"},
			{"type": "function_call_output", "call_id": "call_abc", "output": "15 degrees"}
		],
		"tools": [{"type": "function", "name": "get_weather", "description": "Get weather by city", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}, "strict": true}],
		"tool_choice": {"type": "function", "name": "get_weather"},
		"text": {"format": {"type": "json_schema", "name": "weather", "strict": true, "schema": {"type": "object", "properties": {"summary": {"type": "string"}}, "required": ["summary"], "additionalProperties": false}}}
	}`, responses)

	return map[types.RelayFormat]any{
		types.RelayFormatOpenAI:          openai,
		types.RelayFormatClaude:          claude,
		types.RelayFormatGemini:          gemini,
		types.RelayFormatOpenAIResponses: responses,
	}
}

// ---------------------------------------------------------------------------
// Fixtures: one representative non-stream response per source format
// ---------------------------------------------------------------------------

func fixtureResponses() map[types.RelayFormat]any {
	openai := &dto.OpenAITextResponse{}
	mustUnmarshalFixture(`{
		"id": "chatcmpl-fixed",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "gpt-test",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "The answer is 42.",
				"reasoning_content": "Deep thought.",
				"tool_calls": [{"id": "call_abc", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5,
			"total_tokens": 15,
			"prompt_tokens_details": {"cached_tokens": 3},
			"completion_tokens_details": {"reasoning_tokens": 2}
		}
	}`, openai)

	claude := &dto.ClaudeResponse{}
	mustUnmarshalFixture(`{
		"id": "msg_fixed",
		"type": "message",
		"role": "assistant",
		"model": "claude-test",
		"content": [
			{"type": "text", "text": "The answer is 42."},
			{"type": "tool_use", "id": "toolu_abc", "name": "get_weather", "input": {"city": "Paris"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 2}
	}`, claude)

	gemini := &dto.GeminiChatResponse{}
	mustUnmarshalFixture(`{
		"candidates": [{
			"finishReason": "STOP",
			"content": {
				"role": "model",
				"parts": [
					{"text": "The answer is 42."},
					{"functionCall": {"name": "get_weather", "args": {"city": "Paris"}}}
				]
			}
		}],
		"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "thoughtsTokenCount": 2, "totalTokenCount": 15}
	}`, gemini)

	responses := &dto.OpenAIResponsesResponse{}
	mustUnmarshalFixture(`{
		"id": "resp_fixed",
		"object": "response",
		"model": "gpt-test",
		"status": "completed",
		"output": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "Deep thought."}]},
			{"type": "message", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "The answer is 42."}]},
			{"type": "function_call", "call_id": "call_abc", "name": "get_weather", "arguments": "{\"city\":\"Paris\"}", "status": "completed"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
	}`, responses)

	return map[types.RelayFormat]any{
		types.RelayFormatOpenAI:          openai,
		types.RelayFormatClaude:          claude,
		types.RelayFormatGemini:          gemini,
		types.RelayFormatOpenAIResponses: responses,
	}
}

// ---------------------------------------------------------------------------
// Fixtures: stream chunk sequences per source format
// ---------------------------------------------------------------------------

func fixtureStreamChunks() map[types.RelayFormat][]any {
	return map[types.RelayFormat][]any{
		types.RelayFormatOpenAI: {
			chatStreamChunk(`{"id":"chatcmpl-fixed","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}`),
			chatStreamChunk(`{"id":"chatcmpl-fixed","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{"content":" world"}}]}`),
			chatStreamChunk(`{"id":"chatcmpl-fixed","object":"chat.completion.chunk","created":1700000000,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`),
		},
		types.RelayFormatClaude: {
			claudeStreamChunk(`{"type":"message_start","message":{"id":"msg_fixed","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}`),
			claudeStreamChunk(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			claudeStreamChunk(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello world"}}`),
			claudeStreamChunk(`{"type":"content_block_stop","index":0}`),
			claudeStreamChunk(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`),
			claudeStreamChunk(`{"type":"message_stop"}`),
		},
		types.RelayFormatGemini: {
			geminiStreamChunk(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`),
			geminiStreamChunk(`{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":" world"}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}`),
		},
		types.RelayFormatOpenAIResponses: {
			responsesStreamChunk(`{"type":"response.output_text.delta","delta":"Hello"}`),
			responsesStreamChunk(`{"type":"response.output_text.delta","delta":" world"}`),
			responsesStreamChunk(`{"type":"response.completed","response":{"id":"resp_fixed","object":"response","status":"completed","model":"gpt-test","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`),
		},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func allFormats() []types.RelayFormat {
	return []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatClaude,
		types.RelayFormatGemini,
		types.RelayFormatOpenAIResponses,
	}
}

func TestGoldenRequestConversionMatrix(t *testing.T) {
	requests := fixtureRequests()
	for _, from := range allFormats() {
		for _, to := range allFormats() {
			if from == to {
				continue
			}
			if _, ok := lookupRequestRoute(from, to); !ok {
				t.Fatalf("request route %s -> %s is no longer registered", from, to)
			}
			name := fmt.Sprintf("request/%s_to_%s", from, to)
			t.Run(name, func(t *testing.T) {
				result, err := ConvertRequest(nil, goldenInfo(), to, deepCopyFixture(t, requests[from]))
				require.NoError(t, err)
				checkGolden(t, name, marshalGolden(t, map[string]any{
					"value":  result.Value,
					"losses": result.Diagnostics,
				}))
			})
		}
	}
}

func TestGoldenResponseConversionMatrix(t *testing.T) {
	responses := fixtureResponses()
	for _, from := range allFormats() {
		for _, to := range allFormats() {
			if from == to {
				continue
			}
			name := fmt.Sprintf("response/%s_to_%s", from, to)
			t.Run(name, func(t *testing.T) {
				result, err := ConvertResponse(nil, goldenInfo(), to, deepCopyFixture(t, responses[from]))
				require.NoError(t, err)
				checkGolden(t, name, marshalGolden(t, map[string]any{
					"value":  result.Value,
					"losses": result.Diagnostics,
				}))
			})
		}
	}
}

func TestGoldenStreamConversionMatrix(t *testing.T) {
	chunkSets := fixtureStreamChunks()
	for _, from := range allFormats() {
		for _, to := range allFormats() {
			if from == to {
				continue
			}
			name := fmt.Sprintf("stream/%s_to_%s", from, to)
			t.Run(name, func(t *testing.T) {
				info := goldenInfo()
				state, err := NewResponseStreamState(from, to, ResponseStreamOptions{
					ID:    "stream_fixed",
					Model: "stream-model",
				})
				require.NoError(t, err)

				var outputs []any
				for _, chunk := range chunkSets[from] {
					results, err := ConvertStreamResponseChunk(nil, info, state, deepCopyFixture(t, chunk))
					require.NoError(t, err)
					for _, r := range results {
						outputs = append(outputs, r.Value)
					}
				}
				finals, err := FinalizeStreamResponse(nil, info, state)
				require.NoError(t, err)
				for _, r := range finals {
					outputs = append(outputs, r.Value)
				}

				checkGolden(t, name, marshalGolden(t, map[string]any{
					"events": outputs,
					"usage":  state.Usage(),
					"losses": state.Diagnostics(),
				}))
			})
		}
	}
}

func TestGoldenConversionErrors(t *testing.T) {
	stateful := &dto.OpenAIResponsesRequest{
		Model:              "gpt-test",
		Input:              rawJSON(`"hello"`),
		PreviousResponseID: "resp_previous",
	}
	_, statefulErr := ConvertRequest(context.Background(), goldenInfo(), types.RelayFormatOpenAI, stateful)
	require.Error(t, statefulErr)

	tools := rawJSON(`[{"codeExecution":{}}]`)
	lossy := &dto.GeminiChatRequest{
		Contents: []dto.GeminiChatContent{{
			Role:  "user",
			Parts: []dto.GeminiPart{{Text: "run this"}},
		}},
		Tools: tools,
	}
	strictInfo := goldenInfo().(*convmeta.Values)
	strictInfo.Options.ToolLossPolicy = types.ConversionLossPolicyStrict
	result, lossErr := ConvertRequest(context.Background(), strictInfo, types.RelayFormatOpenAI, lossy)
	require.Error(t, lossErr)
	require.NotNil(t, result)

	checkGolden(t, "errors/request_rejections", marshalGolden(t, map[string]any{
		"session_reference": map[string]any{
			"error": statefulErr.Error(),
		},
		"strict_vendor_tool": map[string]any{
			"error":  lossErr.Error(),
			"losses": result.Diagnostics,
		},
	}))
}

func TestGoldenExplicitZeroScalars(t *testing.T) {
	zeroTokens := uint(0)
	zeroFloat := float64(0)
	falseValue := false
	info := &convmeta.Values{
		Options: &convmeta.Options{
			ToolLossPolicy: types.ConversionLossPolicyAllow,
			Claude: convmeta.ClaudeOptions{
				DefaultMaxTokens: func(string) int { return 2048 },
			},
		},
	}

	chatResult, err := ConvertRequest(context.Background(), info, types.RelayFormatClaude, &dto.GeneralOpenAIRequest{
		Model:       "claude-test",
		MaxTokens:   &zeroTokens,
		Temperature: &zeroFloat,
		TopP:        &zeroFloat,
		Stream:      &falseValue,
		Messages:    []dto.Message{{Role: "user", Content: "hello"}},
	})
	require.NoError(t, err)
	chatClaude := chatResult.Value.(*dto.ClaudeRequest)
	require.NotNil(t, chatClaude.MaxTokens)
	require.Zero(t, *chatClaude.MaxTokens)
	require.NotNil(t, chatClaude.Stream)
	require.False(t, *chatClaude.Stream)

	responsesResult, err := ConvertRequest(context.Background(), info, types.RelayFormatClaude, &dto.OpenAIResponsesRequest{
		Model:           "claude-test",
		Input:           rawJSON(`"hello"`),
		MaxOutputTokens: &zeroTokens,
		Temperature:     &zeroFloat,
		TopP:            &zeroFloat,
		Stream:          &falseValue,
	})
	require.NoError(t, err)
	responsesClaude := responsesResult.Value.(*dto.ClaudeRequest)
	require.NotNil(t, responsesClaude.MaxTokens)
	require.Zero(t, *responsesClaude.MaxTokens)
	require.NotNil(t, responsesClaude.Stream)
	require.False(t, *responsesClaude.Stream)

	absentResult, err := ConvertRequest(context.Background(), info, types.RelayFormatClaude, &dto.GeneralOpenAIRequest{
		Model:    "claude-test",
		Messages: []dto.Message{{Role: "user", Content: "hello"}},
	})
	require.NoError(t, err)
	absentClaude := absentResult.Value.(*dto.ClaudeRequest)
	require.NotNil(t, absentClaude.MaxTokens)
	assert.Equal(t, uint(2048), *absentClaude.MaxTokens)
	assert.Nil(t, absentClaude.Temperature)
	assert.Nil(t, absentClaude.TopP)
	assert.Nil(t, absentClaude.Stream)

	checkGolden(t, "request/explicit_zero_to_claude", marshalGolden(t, map[string]any{
		"openai_chat":      chatResult.Value,
		"openai_responses": responsesResult.Value,
		"absent_defaults":  absentResult.Value,
	}))
}

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

func rawJSON(s string) json.RawMessage {
	return json.RawMessage(s)
}

// deepCopyFixture guards against converters mutating shared fixture state
// between subtests (JSON round-trip through the concrete type).
func deepCopyFixture(t *testing.T, v any) any {
	t.Helper()
	data, err := kitutil.Marshal(v)
	require.NoError(t, err)
	switch v.(type) {
	case *dto.GeneralOpenAIRequest:
		out := &dto.GeneralOpenAIRequest{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	case *dto.ClaudeRequest:
		out := &dto.ClaudeRequest{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	case *dto.GeminiChatRequest:
		out := &dto.GeminiChatRequest{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	case *dto.OpenAIResponsesRequest:
		out := &dto.OpenAIResponsesRequest{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	case *dto.OpenAITextResponse:
		out := &dto.OpenAITextResponse{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	case *dto.ClaudeResponse:
		out := &dto.ClaudeResponse{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	case *dto.GeminiChatResponse:
		out := &dto.GeminiChatResponse{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	case *dto.OpenAIResponsesResponse:
		out := &dto.OpenAIResponsesResponse{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	case *dto.ChatCompletionsStreamResponse:
		out := &dto.ChatCompletionsStreamResponse{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	case *dto.ResponsesStreamResponse:
		out := &dto.ResponsesStreamResponse{}
		require.NoError(t, kitutil.Unmarshal(data, out))
		return out
	default:
		t.Fatalf("deepCopyFixture: unsupported fixture type %T", v)
		return nil
	}
}

func chatStreamChunk(raw string) *dto.ChatCompletionsStreamResponse {
	var r dto.ChatCompletionsStreamResponse
	mustUnmarshalFixture(raw, &r)
	return &r
}

func claudeStreamChunk(raw string) *dto.ClaudeResponse {
	var r dto.ClaudeResponse
	mustUnmarshalFixture(raw, &r)
	return &r
}

func geminiStreamChunk(raw string) *dto.GeminiChatResponse {
	var r dto.GeminiChatResponse
	mustUnmarshalFixture(raw, &r)
	return &r
}

func responsesStreamChunk(raw string) *dto.ResponsesStreamResponse {
	var r dto.ResponsesStreamResponse
	mustUnmarshalFixture(raw, &r)
	return &r
}

func mustUnmarshalFixture(raw string, out any) {
	if err := kitutil.Unmarshal([]byte(raw), out); err != nil {
		panic(fmt.Sprintf("bad fixture JSON: %v", err))
	}
}
