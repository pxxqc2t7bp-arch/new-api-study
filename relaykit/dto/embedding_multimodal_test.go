package dto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateMultimodalEmbeddingInput(t *testing.T) {
	request := EmbeddingRequest{
		Model: "doubao-embedding-vision-251215",
		Input: []any{
			map[string]any{"type": "text", "text": "red shoes"},
			map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "https://example.com/shoes.png"},
			},
			map[string]any{
				"type": "video_url",
				"video_url": map[string]any{
					"url":              "https://example.com/shoes.mp4",
					"fps":              1.0,
					"max_video_tokens": 10240.0,
					"min_frame_tokens": 32.0,
					"max_frame_tokens": 256.0,
					"min_frames":       5.0,
				},
			},
		},
	}

	require.NoError(t, request.ValidateMultimodalInput())
	meta := request.GetTokenCountMeta()
	assert.Equal(t, "red shoes", meta.CombineText)
	assert.Len(t, meta.Files, 2)
}

func TestValidateMultimodalEmbeddingRejectsNewSamplingOptionsOn250615(t *testing.T) {
	request := EmbeddingRequest{
		Model: "doubao-embedding-vision-250615",
		Input: []any{
			map[string]any{
				"type": "video_url",
				"video_url": map[string]any{
					"url": "https://example.com/video.mp4",
					"fps": 1.0,
				},
			},
		},
	}

	err := request.ValidateMultimodalInput()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "251215")
}

func TestValidateMultimodalEmbeddingRejectsInvalidBounds(t *testing.T) {
	request := EmbeddingRequest{
		Model: "doubao-embedding-vision-251215",
		Input: []any{
			map[string]any{
				"type": "video_url",
				"video_url": map[string]any{
					"url":              "https://example.com/video.mp4",
					"max_video_tokens": 999.0,
				},
			},
		},
	}

	err := request.ValidateMultimodalInput()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_video_tokens")
}
