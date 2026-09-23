package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

const (
	StreamReplayUnsafeProviderState = "provider_state_reference"
	StreamReplayUnsafeProviderWrite = "provider_state_write"
	StreamReplayUnsafeHostedTool    = "hosted_tool_side_effect"
	StreamReplayUnsafeContext       = "incomplete_context"
	StreamReplayUnsafeConversion    = "request_conversion"
)

type StreamRecoveryReplaySafety struct {
	Safe       bool
	ReasonCode string
}

func MarkStreamRecoverySubmissionStarted(c *gin.Context) {
	if c == nil ||
		!common.GetContextKeyBool(c, constant.ContextKeyStreamRecoveryWorker) {
		return
	}
	common.SetContextKey(
		c,
		constant.ContextKeyStreamRecoverySubmissionStarted,
		true,
	)
}

func AssessStreamRecoveryReplaySafety(
	relayFormat types.RelayFormat,
	body []byte,
) (StreamRecoveryReplaySafety, error) {
	var request map[string]json.RawMessage
	if err := common.Unmarshal(body, &request); err != nil {
		return StreamRecoveryReplaySafety{}, fmt.Errorf(
			"parse stream recovery request safety: %w",
			err,
		)
	}

	switch relayFormat {
	case types.RelayFormatOpenAIResponses:
		store, hasStore := request["store"]
		if hasStore && !jsonBooleanExplicitlyFalse(store) {
			return unsafeStreamReplay(StreamReplayUnsafeProviderWrite), nil
		}
		if jsonBooleanEnabled(request["background"]) {
			return unsafeStreamReplay(StreamReplayUnsafeProviderWrite), nil
		}
		if hasJSONValue(request["previous_response_id"]) ||
			hasJSONValue(request["conversation"]) ||
			hasJSONValue(request["context_management"]) ||
			hasJSONValue(request["prompt"]) ||
			hasNestedJSONValue(request["reasoning"], "context") ||
			hasNestedJSONValue(request["reasoning"], "encrypted_content") {
			return unsafeStreamReplay(StreamReplayUnsafeProviderState), nil
		}
		if hasHostedTool(request["tools"]) {
			return unsafeStreamReplay(StreamReplayUnsafeHostedTool), nil
		}
		if containsStreamRecoveryType(request["input"], map[string]struct{}{
			"computer_call_output":    {},
			"custom_tool_call_output": {},
			"function_call_output":    {},
			"item_reference":          {},
			"local_shell_call_output": {},
			"mcp_approval_response":   {},
			"reasoning":               {},
		}) {
			return unsafeStreamReplay(StreamReplayUnsafeContext), nil
		}
		if !hasStore {
			return unsafeStreamReplay(StreamReplayUnsafeProviderWrite), nil
		}
	case types.RelayFormatClaude:
		if hasJSONValue(request["context_management"]) ||
			hasJSONValue(request["container"]) ||
			hasJSONValue(request["mcp_servers"]) {
			return unsafeStreamReplay(StreamReplayUnsafeProviderState), nil
		}
		if hasHostedTool(request["tools"]) {
			return unsafeStreamReplay(StreamReplayUnsafeHostedTool), nil
		}
		if containsStreamRecoveryType(request["messages"], map[string]struct{}{
			"thinking":          {},
			"redacted_thinking": {},
		}) {
			return unsafeStreamReplay(StreamReplayUnsafeProviderState), nil
		}
	default:
		return StreamRecoveryReplaySafety{}, fmt.Errorf(
			"unsupported stream recovery relay format %q",
			relayFormat,
		)
	}
	return StreamRecoveryReplaySafety{Safe: true}, nil
}

func unsafeStreamReplay(reasonCode string) StreamRecoveryReplaySafety {
	return StreamRecoveryReplaySafety{
		Safe:       false,
		ReasonCode: reasonCode,
	}
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 &&
		!bytes.Equal(trimmed, []byte("null")) &&
		!bytes.Equal(trimmed, []byte(`""`)) &&
		!bytes.Equal(trimmed, []byte("{}")) &&
		!bytes.Equal(trimmed, []byte("[]"))
}

func jsonBooleanEnabled(raw json.RawMessage) bool {
	if !hasJSONValue(raw) {
		return false
	}
	var enabled bool
	if err := common.Unmarshal(raw, &enabled); err != nil {
		return true
	}
	return enabled
}

func jsonBooleanExplicitlyFalse(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("false"))
}

func hasNestedJSONValue(raw json.RawMessage, key string) bool {
	if !hasJSONValue(raw) {
		return false
	}
	var object map[string]json.RawMessage
	if err := common.Unmarshal(raw, &object); err != nil {
		return true
	}
	return hasJSONValue(object[key])
}

func hasHostedTool(raw json.RawMessage) bool {
	if !hasJSONValue(raw) {
		return false
	}
	var tools []map[string]json.RawMessage
	if err := common.Unmarshal(raw, &tools); err != nil {
		return true
	}
	for _, tool := range tools {
		var toolType string
		_ = common.Unmarshal(tool["type"], &toolType)
		switch strings.TrimSpace(toolType) {
		case "", "function", "custom":
		default:
			return true
		}
	}
	return false
}

func containsStreamRecoveryType(
	raw json.RawMessage,
	blocked map[string]struct{},
) bool {
	if !hasJSONValue(raw) {
		return false
	}
	var value any
	if err := common.Unmarshal(raw, &value); err != nil {
		return true
	}
	var walk func(any) bool
	walk = func(current any) bool {
		switch typed := current.(type) {
		case []any:
			for _, item := range typed {
				if walk(item) {
					return true
				}
			}
		case map[string]any:
			if valueType, ok := typed["type"].(string); ok {
				if _, blockedType := blocked[valueType]; blockedType {
					return true
				}
			}
			for _, item := range typed {
				if walk(item) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}
