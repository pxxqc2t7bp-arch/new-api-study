package service

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssessStreamRecoveryReplaySafety(t *testing.T) {
	tests := []struct {
		name       string
		format     types.RelayFormat
		body       string
		safe       bool
		reasonCode string
	}{
		{
			name:   "responses text is self contained",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"glm-5.3","stream":true,"store":false,"input":"hello"}`,
			safe:   true,
		},
		{
			name:       "responses previous response is provider state",
			format:     types.RelayFormatOpenAIResponses,
			body:       `{"model":"glm-5.3","stream":true,"previous_response_id":"resp_1","input":"continue"}`,
			reasonCode: "provider_state_reference",
		},
		{
			name:       "responses conversation is provider state",
			format:     types.RelayFormatOpenAIResponses,
			body:       `{"model":"glm-5.3","stream":true,"conversation":"conv_1","input":"continue"}`,
			reasonCode: "provider_state_reference",
		},
		{
			name:       "responses hosted tool may execute upstream",
			format:     types.RelayFormatOpenAIResponses,
			body:       `{"model":"glm-5.3","stream":true,"input":"search","tools":[{"type":"web_search_preview"}]}`,
			reasonCode: "hosted_tool_side_effect",
		},
		{
			name:       "responses store creates provider state",
			format:     types.RelayFormatOpenAIResponses,
			body:       `{"model":"glm-5.3","stream":true,"store":true,"input":"hello"}`,
			reasonCode: "provider_state_write",
		},
		{
			name:       "responses background creates provider state",
			format:     types.RelayFormatOpenAIResponses,
			body:       `{"model":"glm-5.3","stream":true,"background":true,"input":"hello"}`,
			reasonCode: "provider_state_write",
		},
		{
			name:   "responses explicit no-store remains self contained",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"glm-5.3","stream":true,"store":false,"input":"hello"}`,
			safe:   true,
		},
		{
			name:   "responses client function waits for visible output",
			format: types.RelayFormatOpenAIResponses,
			body:   `{"model":"glm-5.3","stream":true,"store":false,"input":"lookup","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
			safe:   true,
		},
		{
			name:       "responses item reference is incomplete context",
			format:     types.RelayFormatOpenAIResponses,
			body:       `{"model":"glm-5.3","stream":true,"input":[{"type":"item_reference","id":"item_1"}]}`,
			reasonCode: "incomplete_context",
		},
		{
			name:       "responses reasoning context is provider bound",
			format:     types.RelayFormatOpenAIResponses,
			body:       `{"model":"glm-5.3","stream":true,"input":"continue","reasoning":{"context":"encrypted-state"}}`,
			reasonCode: "provider_state_reference",
		},
		{
			name:   "claude text is self contained",
			format: types.RelayFormatClaude,
			body:   `{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			safe:   true,
		},
		{
			name:       "claude container is provider state",
			format:     types.RelayFormatClaude,
			body:       `{"model":"glm-5.3","stream":true,"container":"container_1","messages":[{"role":"user","content":"continue"}]}`,
			reasonCode: "provider_state_reference",
		},
		{
			name:       "claude server tool may execute upstream",
			format:     types.RelayFormatClaude,
			body:       `{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
			reasonCode: "hosted_tool_side_effect",
		},
		{
			name:       "claude signed thinking is provider bound",
			format:     types.RelayFormatClaude,
			body:       `{"model":"glm-5.3","stream":true,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"signed"}]},{"role":"user","content":"continue"}]}`,
			reasonCode: "provider_state_reference",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			safety, err := AssessStreamRecoveryReplaySafety(
				test.format,
				[]byte(test.body),
			)
			require.NoError(t, err)
			assert.Equal(t, test.safe, safety.Safe)
			assert.Equal(t, test.reasonCode, safety.ReasonCode)
		})
	}
}

func TestAssessStreamRecoveryReplaySafetyRejectsInvalidJSON(t *testing.T) {
	_, err := AssessStreamRecoveryReplaySafety(
		types.RelayFormatOpenAIResponses,
		[]byte(`{"stream":`),
	)
	require.Error(t, err)
}

func TestResponsesOmittedStoreIsUnsafe(t *testing.T) {
	tests := []struct {
		name string
		body string
		safe bool
	}{
		{
			name: "explicit false",
			body: `{"model":"glm-5.3","stream":true,"store":false,"input":"hello"}`,
			safe: true,
		},
		{
			name: "omitted",
			body: `{"model":"glm-5.3","stream":true,"input":"hello"}`,
		},
		{
			name: "null",
			body: `{"model":"glm-5.3","stream":true,"store":null,"input":"hello"}`,
		},
		{
			name: "string false",
			body: `{"model":"glm-5.3","stream":true,"store":"false","input":"hello"}`,
		},
		{
			name: "object",
			body: `{"model":"glm-5.3","stream":true,"store":{},"input":"hello"}`,
		},
		{
			name: "true",
			body: `{"model":"glm-5.3","stream":true,"store":true,"input":"hello"}`,
		},
		{
			name: "explicit false still rejects provider state",
			body: `{"model":"glm-5.3","stream":true,"store":false,"previous_response_id":"resp_1","input":"continue"}`,
		},
		{
			name: "explicit false still rejects hosted tools",
			body: `{"model":"glm-5.3","stream":true,"store":false,"input":"search","tools":[{"type":"web_search_preview"}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			safety, err := AssessStreamRecoveryReplaySafety(
				types.RelayFormatOpenAIResponses,
				[]byte(test.body),
			)
			require.NoError(t, err)
			assert.Equal(t, test.safe, safety.Safe)
			if !test.safe {
				assert.NotEmpty(t, safety.ReasonCode)
			}
		})
	}
}
