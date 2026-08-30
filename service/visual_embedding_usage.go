package service

import (
	"strings"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

const (
	maxArkEmbeddingImageTokens = 1312
	maxArkEmbeddingVideoTokens = 204800
)

// EnsureVisualEmbeddingUsage prevents multimodal embedding requests from
// silently settling at the cheaper text-only rate when Ark omits token details.
func EnsureVisualEmbeddingUsage(c *gin.Context, request *dto.EmbeddingRequest, usage *dto.Usage) {
	if request == nil || usage == nil ||
		!strings.HasPrefix(request.Model, "doubao-embedding-vision-") ||
		usage.PromptTokensDetails.ImageTokens > 0 {
		return
	}
	items, ok := request.Input.([]any)
	if !ok {
		return
	}
	estimated := 0
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "image_url":
			estimated += maxArkEmbeddingImageTokens
		case "video_url":
			estimated += visualEmbeddingVideoTokenLimit(item)
		}
	}
	if estimated == 0 {
		return
	}
	usage.PromptTokensDetails.ImageTokens = estimated
	if usage.PromptTokens < estimated {
		usage.PromptTokens = estimated
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if c != nil {
		c.Set("visual_usage_missing", true)
		c.Set("visual_usage_estimated", estimated)
	}
}

func visualEmbeddingVideoTokenLimit(item map[string]any) int {
	video, _ := item["video_url"].(map[string]any)
	value, ok := video["max_video_tokens"].(float64)
	if !ok {
		return maxArkEmbeddingVideoTokens
	}
	limit := int(value)
	if limit < 10240 || limit > maxArkEmbeddingVideoTokens {
		return maxArkEmbeddingVideoTokens
	}
	return limit
}
