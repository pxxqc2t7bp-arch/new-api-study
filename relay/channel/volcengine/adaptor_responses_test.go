package volcengine

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertOpenAIResponsesRequestNormalizesHistoricalInputItems(t *testing.T) {
	input := json.RawMessage(`[
		{"role":"user","content":"start"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}]},
		{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"result"},
		{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{}","status":"in_progress"},
		{"type":"custom_item","role":"assistant","content":"custom"}
	]`)

	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, nil, dto.OpenAIResponsesRequest{
		Model: "glm-5.3",
		Input: input,
	})
	require.NoError(t, err)

	request, ok := converted.(dto.OpenAIResponsesRequest)
	require.True(t, ok)

	var items []map[string]any
	require.NoError(t, common.Unmarshal(request.Input, &items))
	require.Len(t, items, 6)
	assert.Equal(t, "message", items[0]["type"])
	assert.Equal(t, "message", items[1]["type"])
	assert.Equal(t, "completed", items[2]["status"])
	assert.Equal(t, "completed", items[3]["status"])
	assert.Equal(t, "in_progress", items[4]["status"])
	assert.Equal(t, "custom_item", items[5]["type"])
}

func TestConvertOpenAIResponsesRequestPreservesScalarInput(t *testing.T) {
	input := json.RawMessage(`"plain prompt"`)

	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, nil, dto.OpenAIResponsesRequest{
		Model: "glm-5.3",
		Input: input,
	})
	require.NoError(t, err)

	request, ok := converted.(dto.OpenAIResponsesRequest)
	require.True(t, ok)
	assert.True(t, bytes.Equal(input, request.Input))
}
