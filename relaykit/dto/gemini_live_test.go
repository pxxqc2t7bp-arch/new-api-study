package dto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGeminiLiveSetupCanonicalizesModel(t *testing.T) {
	setup, canonical, err := ParseGeminiLiveSetup([]byte(`{
		"setup": {
			"model": "models/gemini-live-test",
			"generationConfig": {"responseModalities": ["AUDIO"]},
			"tools": [{"functionDeclarations": []}]
		}
	}`))
	require.NoError(t, err)
	assert.Equal(t, "gemini-live-test", setup.Model)
	assert.JSONEq(t, `{
		"setup": {
			"model": "models/gemini-live-test",
			"generationConfig": {"responseModalities": ["AUDIO"]},
			"tools": [{"functionDeclarations": []}]
		}
	}`, string(canonical))
}

func TestParseGeminiLiveSetupRejectsNonSetupFirstFrame(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "realtime input", payload: `{"realtimeInput":{"mediaChunks":[]}}`},
		{name: "client content", payload: `{"clientContent":{"turnComplete":true}}`},
		{name: "setup mixed with media", payload: `{"setup":{"model":"models/gemini-live-test"},"realtimeInput":{"mediaChunks":[]}}`},
		{name: "missing model", payload: `{"setup":{"generationConfig":{}}}`},
		{name: "invalid json", payload: `{`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := ParseGeminiLiveSetup([]byte(test.payload))
			require.Error(t, err)
		})
	}
}

func TestNormalizeGeminiLiveModel(t *testing.T) {
	assert.Equal(t, "gemini-live-test", NormalizeGeminiLiveModel(" models/gemini-live-test "))
	assert.Equal(t, "gemini-live-test", NormalizeGeminiLiveModel("gemini-live-test"))
	assert.Empty(t, NormalizeGeminiLiveModel("models/"))
}

func TestRewriteGeminiLiveSetupModelPreservesSetupFields(t *testing.T) {
	rewritten, err := RewriteGeminiLiveSetupModel([]byte(`{
		"setup":{
			"model":"models/client-alias",
			"generationConfig":{"responseModalities":["AUDIO"]},
			"systemInstruction":{"parts":[{"text":"hello"}]}
		}
	}`), "models/upstream-model")

	require.NoError(t, err)
	assert.JSONEq(t, `{
		"setup":{
			"model":"models/upstream-model",
			"generationConfig":{"responseModalities":["AUDIO"]},
			"systemInstruction":{"parts":[{"text":"hello"}]}
		}
	}`, string(rewritten))
}
