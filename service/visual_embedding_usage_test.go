package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestEnsureVisualEmbeddingUsageUsesConservativeFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	request := &dto.EmbeddingRequest{
		Model: "doubao-embedding-vision-251215",
		Input: []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "image"}},
			map[string]any{
				"type": "video_url",
				"video_url": map[string]any{
					"url":              "video",
					"max_video_tokens": 12000.0,
				},
			},
		},
	}
	usage := &dto.Usage{PromptTokens: 10, TotalTokens: 10}

	EnsureVisualEmbeddingUsage(ctx, request, usage)

	assert.Equal(t, 13312, usage.PromptTokensDetails.ImageTokens)
	assert.Equal(t, 13312, usage.PromptTokens)
	assert.Equal(t, 13312, usage.TotalTokens)
	assert.True(t, ctx.GetBool("visual_usage_missing"))
}

func TestEnsureVisualEmbeddingUsageKeepsUpstreamDetails(t *testing.T) {
	request := &dto.EmbeddingRequest{
		Model: "doubao-embedding-vision-251215",
		Input: []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "image"}},
		},
	}
	usage := &dto.Usage{PromptTokens: 100, TotalTokens: 100}
	usage.PromptTokensDetails.ImageTokens = 80

	EnsureVisualEmbeddingUsage(nil, request, usage)

	assert.Equal(t, 80, usage.PromptTokensDetails.ImageTokens)
	assert.Equal(t, 100, usage.PromptTokens)
}
