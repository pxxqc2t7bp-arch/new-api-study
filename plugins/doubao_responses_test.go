package plugins_test

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoubaoResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "doubao",
		model:     "doubao-seedance-2-0-260128",
		requestBody: map[string]any{
			"model": "doubao-seedance-2-0-260128",
			"input": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "a running fox"},
				map[string]any{"type": "input_image", "image_url": "https://cdn.example/frame.png"},
			}}},
			"seconds": 6,
			"size":    "1920x1080",
		},
		wantAction: "image_to_video",
		wantRequest: map[string]any{
			"model":   "doubao-seedance-2-0-260128",
			"prompt":  "a running fox",
			"images":  []any{"https://cdn.example/frame.png"},
			"seconds": float64(6),
			"metadata": map[string]any{
				"resolution": "1080p",
			},
		},
		wantUsageKeys:       []string{"image_count", "reference_image_count", "resolution", "tokens", "video_input"},
		wantSubmitUsageKeys: []string{"resolution", "tokens", "video_input"},
		wantVendorName:      "doubao",
	})
}

func TestDoubaoSeedreamResponsesUseDeferredImageExecution(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	plugin, err := registry.RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)

	models := []string{
		"doubao-seedream-4-0-20260415",
		"doubao-seedream-4-0-250828",
		"doubao-seedream-4-5-251128",
		"doubao-seedream-5-0-260128",
		"doubao-seedream-5-0-pro-260628",
	}
	for _, model := range models {
		assert.Contains(t, plugin.Meta.Models, model)
		responses, found := registry.Generation().LookupEndpoint("POST", "/v1/responses", model)
		require.True(t, found, model)
		assert.Equal(t, "doubao", responses.Plugin.Meta.Key)
		_, videoFound := registry.Generation().LookupEndpoint("POST", "/v1/videos", model)
		assert.False(t, videoFound, model)
	}

	modelName := "doubao-seedream-5-0-pro-260628"
	decoded, err := plugin.Engine.CallPath(
		t.Context(),
		"protocols",
		[]string{"openai_responses", "decodeRequest"},
		map[string]any{
			"body": map[string]any{"kind": "json", "value": map[string]any{
				"model": modelName,
				"input": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "a red observatory"},
					map[string]any{"type": "input_image", "image_url": "https://input.example/reference.png"},
				}}},
				"size": "2048x2048",
				"n":    2,
			}},
			"model":  modelName,
			"stream": false,
		},
	)
	require.NoError(t, err)
	intent := jsonObject(t, decoded)
	assert.Equal(t, "deferred", intent["execution"])
	assert.Equal(t, "image_edit", intent["action"])
	requestBody := intent["requestBody"].(map[string]any)

	usageValue, err := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{
		"model":         modelName,
		"upstreamModel": modelName,
		"requestBody":   requestBody,
	})
	require.NoError(t, err)
	usage := jsonObject(t, usageValue)
	assert.Equal(t, float64(2), usage["image_count"])
	assert.Equal(t, "2K", usage["resolution"])
	assert.Equal(t, float64(1), usage["reference_image_count"])
	assert.Contains(t, plugin.Meta.UsageSchema["resolution"].Enum, usage["resolution"])

	descriptorValue, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
		"model":         modelName,
		"upstreamModel": modelName,
		"baseUrl":       "https://ark.example",
		"apiKey":        "secret-at-runtime",
		"publicTaskId":  "task_seedream_public",
		"requestBody":   requestBody,
	})
	require.NoError(t, err)
	descriptor := jsonObject(t, descriptorValue)
	assert.Equal(t, "https://ark.example/api/v3/images/generations", descriptor["url"])
	assert.Equal(t, "image_edit", descriptor["action"])
	body := descriptor["body"].(map[string]any)
	assert.Equal(t, modelName, body["model"])
	assert.Equal(t, "url", body["response_format"])
	assert.Equal(t, "https://input.example/reference.png", body["image"])

	parsedValue, err := plugin.Engine.Call(
		t.Context(),
		"parseSubmitResponse",
		map[string]any{"model": modelName, "upstreamModel": modelName, "publicTaskId": "task_seedream_public"},
		map[string]any{
			"statusCode": float64(200),
			"body": map[string]any{"created": float64(10), "data": []any{
				map[string]any{"url": "https://cdn.example/one.png"},
				map[string]any{"url": "https://cdn.example/two.png"},
			}},
		},
	)
	require.NoError(t, err)
	parsed := jsonObject(t, parsedValue)
	assert.Equal(t, "task_seedream_public", parsed["taskId"])
	immediate := parsed["immediate"].(map[string]any)
	assert.Equal(t, "SUCCESS", immediate["status"])

	taskData := parsed["taskData"].(map[string]any)
	artifactsValue, err := plugin.Engine.Call(t.Context(), "listArtifacts", map[string]any{
		"status": "SUCCESS",
		"data":   taskData,
	})
	require.NoError(t, err)
	artifacts := jsonArray(t, artifactsValue)
	require.Len(t, artifacts, 2)
	assert.Equal(t, "image_0", artifacts[0].(map[string]any)["key"])

	contentValue, err := plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{
		"artifactKey": "image_1",
		"data":        taskData,
		"clientRequest": map[string]any{
			"method": "GET",
		},
	})
	require.NoError(t, err)
	content := jsonObject(t, contentValue)
	assert.Equal(t, "https://cdn.example/two.png", content["url"])
	assert.Equal(t, true, content["credentialless"])

	renderedValue, err := plugin.Engine.CallPath(
		t.Context(),
		"protocols",
		[]string{"openai_responses", "renderFinal"},
		map[string]any{
			"artifacts": map[string]any{
				"image_0": map[string]any{"url": "https://gateway.example/image-0"},
				"image_1": map[string]any{"url": "https://gateway.example/image-1"},
			},
		},
		map[string]any{"status": "SUCCESS", "data": taskData},
	)
	require.NoError(t, err)
	rendered := jsonObject(t, renderedValue)
	output := rendered["output"].([]any)
	contentParts := output[0].(map[string]any)["content"].([]any)
	text := contentParts[0].(map[string]any)["text"].(string)
	assert.Contains(t, text, "https://gateway.example/image-0")
	assert.NotContains(t, text, "https://cdn.example")

	invalidDecoded, err := plugin.Engine.CallPath(
		t.Context(),
		"protocols",
		[]string{"openai_responses", "decodeRequest"},
		map[string]any{
			"body": map[string]any{"kind": "json", "value": map[string]any{
				"model": modelName,
				"input": "invalid count",
				"n":     129,
			}},
			"model": modelName,
		},
	)
	require.NoError(t, err)
	invalidIntent := jsonObject(t, invalidDecoded)
	_, err = plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{
		"model":         modelName,
		"upstreamModel": modelName,
		"requestBody":   invalidIntent["requestBody"],
	})
	assert.ErrorContains(t, err, "n must be an integer between 1 and 128")
}

func TestDoubaoSeedanceDraftPreviewUsesSeedance25Protocol(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	plugin, err := registry.RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)

	const modelName = "doubao-seedance-2-5-draft-preview-260828"
	assert.Contains(t, plugin.Meta.Models, modelName)
	_, found := registry.Generation().LookupEndpoint("POST", "/v1/videos", modelName)
	require.True(t, found)

	usageValue, err := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{
		"model":         modelName,
		"upstreamModel": modelName,
		"requestBody": map[string]any{
			"model":   modelName,
			"seconds": "5",
			"metadata": map[string]any{
				"resolution": "1080p",
				"content":    []any{},
			},
		},
	})
	require.NoError(t, err)
	usage := jsonObject(t, usageValue)
	assert.Equal(t, "1080p", usage["resolution"])
	assert.Equal(t, "none", usage["video_input"])
	assert.Equal(t, float64(243000), usage["tokens"])

	ratioValue, err := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{
		"model":         modelName,
		"upstreamModel": modelName,
		"usagePurpose":  "billing_ratios",
		"requestBody": map[string]any{
			"metadata": map[string]any{
				"resolution": "1080p",
				"content": []any{
					map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.com/ref.mp4"}},
				},
			},
		},
	})
	require.NoError(t, err)
	ratios := jsonObject(t, ratioValue)
	assert.InDelta(t, 7.0/10.7, ratios["video_input_ratio"], 1e-12)
}

func jsonObject(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, common.Unmarshal(encoded, &decoded))
	return decoded
}

func jsonArray(t *testing.T, value any) []any {
	t.Helper()
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var decoded []any
	require.NoError(t, common.Unmarshal(encoded, &decoded))
	return decoded
}

func TestDoubaoDraftPreviewResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "doubao",
		model:     "doubao-seedance-2-5-draft-preview-260828",
		requestBody: map[string]any{
			"model":   "doubao-seedance-2-5-draft-preview-260828",
			"input":   "a rotating red cube",
			"seconds": 5,
			"size":    "1920x1080",
		},
		wantAction: "text_to_video",
		wantRequest: map[string]any{
			"model":   "doubao-seedance-2-5-draft-preview-260828",
			"prompt":  "a rotating red cube",
			"seconds": float64(5),
			"metadata": map[string]any{
				"resolution": "1080p",
			},
		},
		wantUsageKeys:       []string{"image_count", "reference_image_count", "resolution", "tokens", "video_input"},
		wantSubmitUsageKeys: []string{"resolution", "tokens", "video_input"},
		wantVendorName:      "doubao",
	})
}
