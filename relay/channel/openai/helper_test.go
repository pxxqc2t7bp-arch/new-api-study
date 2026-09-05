package openai

import (
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestHandleFinalResponseFinalizesClaudeStream(t *testing.T) {
	tests := []struct {
		name           string
		lastStreamData string
	}{
		{
			name: "final data chunk has no finish reason",
			lastStreamData: `{
				"id":"chatcmpl_1",
				"model":"glm-test",
				"choices":[{"index":0,"delta":{"content":"tail"},"finish_reason":null}]
			}`,
		},
		{
			name: "final data chunk already has finish reason",
			lastStreamData: `{
				"id":"chatcmpl_1",
				"model":"glm-test",
				"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			info := &relaycommon.RelayInfo{
				RelayFormat:       types.RelayFormatClaude,
				SendResponseCount: 2,
				ClaudeConvertInfo: &relaycommon.ClaudeConvertInfo{
					LastMessagesType: relaycommon.LastMessageTypeText,
				},
			}
			usage := &dto.Usage{
				PromptTokens:     4,
				CompletionTokens: 2,
				TotalTokens:      6,
			}

			HandleFinalResponse(c, info, tt.lastStreamData, "chatcmpl_1", 1, "glm-test", "", usage, true)

			body := recorder.Body.String()
			assert.Equal(t, 1, strings.Count(body, "event: content_block_stop\n"))
			assert.Equal(t, 1, strings.Count(body, "event: message_delta\n"))
			assert.Equal(t, 1, strings.Count(body, "event: message_stop\n"))
			assert.True(t, info.ClaudeConvertInfo.Done)
		})
	}
}
