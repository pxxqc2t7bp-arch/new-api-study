package dto

import (
	"encoding/json"
	"errors"
	"strings"
)

type GeminiLiveSetup struct {
	Model string `json:"model"`
}

type GeminiLiveClientMessage struct {
	Setup          *GeminiLiveSetup `json:"setup,omitempty"`
	ClientContent  json.RawMessage  `json:"clientContent,omitempty"`
	RealtimeInput  json.RawMessage  `json:"realtimeInput,omitempty"`
	ToolResponse   json.RawMessage  `json:"toolResponse,omitempty"`
	ActivityStart  json.RawMessage  `json:"activityStart,omitempty"`
	ActivityEnd    json.RawMessage  `json:"activityEnd,omitempty"`
	SessionControl json.RawMessage  `json:"sessionResumption,omitempty"`
}

type GeminiLiveUsageMetadata struct {
	PromptTokenCount           int                         `json:"promptTokenCount"`
	ToolUsePromptTokenCount    int                         `json:"toolUsePromptTokenCount"`
	ResponseTokenCount         int                         `json:"responseTokenCount"`
	ThoughtsTokenCount         int                         `json:"thoughtsTokenCount"`
	TotalTokenCount            int                         `json:"totalTokenCount"`
	CachedContentTokenCount    int                         `json:"cachedContentTokenCount"`
	PromptTokensDetails        []GeminiPromptTokensDetails `json:"promptTokensDetails"`
	ToolUsePromptTokensDetails []GeminiPromptTokensDetails `json:"toolUsePromptTokensDetails"`
	ResponseTokensDetails      []GeminiPromptTokensDetails `json:"responseTokensDetails"`
}

type GeminiLiveServerMessage struct {
	UsageMetadata *GeminiLiveUsageMetadata `json:"usageMetadata,omitempty"`
}

func HasGeminiLiveUsageMetadataTokens(metadata *GeminiLiveUsageMetadata) bool {
	if metadata == nil {
		return false
	}
	return metadata.PromptTokenCount != 0 ||
		metadata.ToolUsePromptTokenCount != 0 ||
		metadata.ResponseTokenCount != 0 ||
		metadata.ThoughtsTokenCount != 0 ||
		metadata.TotalTokenCount != 0 ||
		metadata.CachedContentTokenCount != 0 ||
		len(metadata.PromptTokensDetails) != 0 ||
		len(metadata.ToolUsePromptTokensDetails) != 0 ||
		len(metadata.ResponseTokensDetails) != 0
}

func NormalizeGeminiLiveModel(model string) string {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "models/")
	return strings.TrimSpace(model)
}

func ParseGeminiLiveSetup(message []byte) (*GeminiLiveSetup, []byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(message, &envelope); err != nil {
		return nil, nil, err
	}
	rawSetup, ok := envelope["setup"]
	if !ok || len(envelope) != 1 {
		return nil, nil, errors.New("first Gemini Live message must contain setup only")
	}

	var setupFields map[string]json.RawMessage
	if err := json.Unmarshal(rawSetup, &setupFields); err != nil {
		return nil, nil, errors.New("Gemini Live setup must be an object")
	}
	rawModel, ok := setupFields["model"]
	if !ok {
		return nil, nil, errors.New("Gemini Live setup model is required")
	}
	var upstreamModel string
	if err := json.Unmarshal(rawModel, &upstreamModel); err != nil {
		return nil, nil, errors.New("Gemini Live setup model must be a string")
	}
	model := NormalizeGeminiLiveModel(upstreamModel)
	if model == "" {
		return nil, nil, errors.New("Gemini Live setup model is required")
	}
	canonicalModel, err := json.Marshal("models/" + model)
	if err != nil {
		return nil, nil, err
	}
	setupFields["model"] = canonicalModel
	canonicalSetup, err := json.Marshal(setupFields)
	if err != nil {
		return nil, nil, err
	}
	canonical, err := json.Marshal(map[string]json.RawMessage{"setup": canonicalSetup})
	if err != nil {
		return nil, nil, err
	}
	return &GeminiLiveSetup{Model: model}, canonical, nil
}

func RewriteGeminiLiveSetupModel(message []byte, model string) ([]byte, error) {
	model = NormalizeGeminiLiveModel(model)
	if model == "" {
		return nil, errors.New("Gemini Live setup model is required")
	}
	_, canonical, err := ParseGeminiLiveSetup(message)
	if err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(canonical, &envelope); err != nil {
		return nil, err
	}
	var setupFields map[string]json.RawMessage
	if err = json.Unmarshal(envelope["setup"], &setupFields); err != nil {
		return nil, err
	}
	setupFields["model"], err = json.Marshal("models/" + model)
	if err != nil {
		return nil, err
	}
	envelope["setup"], err = json.Marshal(setupFields)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}
