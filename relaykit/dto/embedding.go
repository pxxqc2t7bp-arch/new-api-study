package dto

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/types"
)

type EmbeddingOptions struct {
	Seed             int      `json:"seed,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopK             int      `json:"top_k,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	NumPredict       int      `json:"num_predict,omitempty"`
	NumCtx           int      `json:"num_ctx,omitempty"`
}

type EmbeddingRequest struct {
	Model            string   `json:"model"`
	Input            any      `json:"input"`
	EncodingFormat   string   `json:"encoding_format,omitempty"`
	Dimensions       *int     `json:"dimensions,omitempty"`
	User             string   `json:"user,omitempty"`
	Seed             *float64 `json:"seed,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
}

func (r *EmbeddingRequest) GetTokenCountMeta() *types.TokenCountMeta {
	var texts = make([]string, 0)
	var files []*types.FileMeta

	switch inputs := r.Input.(type) {
	case []any:
		for _, raw := range inputs {
			if text, ok := raw.(string); ok {
				texts = append(texts, text)
				continue
			}
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "text":
				if text, ok := item["text"].(string); ok {
					texts = append(texts, text)
				}
			case "image_url":
				if url := nestedURL(item, "image_url"); url != "" {
					files = append(files, types.NewFileMeta(
						types.FileTypeImage,
						types.NewFileSourceFromData(url, ""),
					))
				}
			case "video_url":
				if url := nestedURL(item, "video_url"); url != "" {
					files = append(files, types.NewFileMeta(
						types.FileTypeVideo,
						types.NewFileSourceFromData(url, ""),
					))
				}
			}
		}
	default:
		texts = append(texts, r.ParseInput()...)
	}

	return &types.TokenCountMeta{
		CombineText: strings.Join(texts, "\n"),
		Files:       files,
	}
}

func nestedURL(item map[string]any, field string) string {
	value, _ := item[field].(map[string]any)
	url, _ := value["url"].(string)
	return strings.TrimSpace(url)
}

func (r *EmbeddingRequest) IsStream(c *http.Request) bool {
	return false
}

func (r *EmbeddingRequest) SetModelName(modelName string) {
	if modelName != "" {
		r.Model = modelName
	}
}

func (r *EmbeddingRequest) ParseInput() []string {
	if r.Input == nil {
		return make([]string, 0)
	}
	var input []string
	switch r.Input.(type) {
	case string:
		input = []string{r.Input.(string)}
	case []any:
		input = make([]string, 0, len(r.Input.([]any)))
		for _, item := range r.Input.([]any) {
			if str, ok := item.(string); ok {
				input = append(input, str)
			}
		}
	}
	return input
}

func (r *EmbeddingRequest) ValidateMultimodalInput() error {
	items, ok := r.Input.([]any)
	if !ok {
		return fmt.Errorf("multimodal embedding input must be an array")
	}
	if len(items) == 0 {
		return fmt.Errorf("multimodal embedding input is empty")
	}
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("multimodal embedding input[%d] must be an object", index)
		}
		inputType, _ := item["type"].(string)
		switch inputType {
		case "text":
			if text, _ := item["text"].(string); strings.TrimSpace(text) == "" {
				return fmt.Errorf("multimodal embedding input[%d].text is required", index)
			}
		case "image_url":
			if err := validateMultimodalURL(item, "image_url", index); err != nil {
				return err
			}
		case "video_url":
			if err := validateMultimodalURL(item, "video_url", index); err != nil {
				return err
			}
			video, _ := item["video_url"].(map[string]any)
			if r.Model == "doubao-embedding-vision-250615" && hasVideoSamplingOptions(video) {
				return fmt.Errorf("video sampling options require doubao-embedding-vision-251215 or newer")
			}
			if err := validateVideoSamplingOptions(video); err != nil {
				return fmt.Errorf("multimodal embedding input[%d]: %w", index, err)
			}
		default:
			return fmt.Errorf("multimodal embedding input[%d].type must be text, image_url, or video_url", index)
		}
	}
	return nil
}

func validateMultimodalURL(item map[string]any, field string, index int) error {
	value, ok := item[field].(map[string]any)
	if !ok {
		return fmt.Errorf("multimodal embedding input[%d].%s is required", index, field)
	}
	url, _ := value["url"].(string)
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("multimodal embedding input[%d].%s.url is required", index, field)
	}
	return nil
}

func hasVideoSamplingOptions(video map[string]any) bool {
	for _, key := range []string{
		"fps", "max_video_tokens", "min_frame_tokens", "max_frame_tokens", "min_frames",
	} {
		if _, ok := video[key]; ok {
			return true
		}
	}
	return false
}

func validateVideoSamplingOptions(video map[string]any) error {
	if value, ok := numberValue(video["fps"]); ok && (value < 0.2 || value > 5) {
		return fmt.Errorf("fps must be between 0.2 and 5")
	}
	if value, ok := numberValue(video["max_video_tokens"]); ok && (value < 10240 || value > 204800) {
		return fmt.Errorf("max_video_tokens must be between 10240 and 204800")
	}
	minFrame, minSet := numberValue(video["min_frame_tokens"])
	if minSet && (minFrame < 16 || minFrame > 128) {
		return fmt.Errorf("min_frame_tokens must be between 16 and 128")
	}
	maxFrame, maxSet := numberValue(video["max_frame_tokens"])
	if maxSet && (maxFrame < 128 || maxFrame > 640) {
		return fmt.Errorf("max_frame_tokens must be between 128 and 640")
	}
	if minSet && maxSet && minFrame > maxFrame {
		return fmt.Errorf("min_frame_tokens must not exceed max_frame_tokens")
	}
	if value, ok := numberValue(video["min_frames"]); ok && (value < 5 || value > 16) {
		return fmt.Errorf("min_frames must be between 5 and 16")
	}
	return nil
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	default:
		return 0, false
	}
}

type EmbeddingResponseItem struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type EmbeddingResponse struct {
	Object string                  `json:"object"`
	Data   []EmbeddingResponseItem `json:"data"`
	Model  string                  `json:"model"`
	Usage  `json:"usage"`
}
