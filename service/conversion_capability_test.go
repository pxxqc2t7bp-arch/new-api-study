package service

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrdinaryConversionCapabilitiesCoverEveryDirectedProtocolPair(t *testing.T) {
	t.Parallel()

	formats := []types.RelayFormat{
		types.RelayFormatOpenAI,
		types.RelayFormatOpenAIResponses,
		types.RelayFormatClaude,
		types.RelayFormatGemini,
	}
	capabilities := OrdinaryConversionCapabilities()

	require.Len(t, capabilities, 12)
	for _, from := range formats {
		for _, to := range formats {
			if from == to {
				continue
			}
			capability, ok := LookupOrdinaryConversionCapability(from, to)
			require.True(t, ok, "%s -> %s", from, to)
			assert.Equal(t, from, capability.From)
			assert.Equal(t, to, capability.To)
			assert.NotEmpty(t, capability.Converter)
			assert.NotEmpty(t, capability.Supported)
			assert.NotEmpty(t, capability.Losses)
			assert.NotEmpty(t, capability.Rejections)
		}
	}
}

func TestResolveOrdinaryConversionPolicyRequiresAuthorizationForLossyStrategies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		requested  types.ConversionLossPolicy
		authorized bool
		want       types.ConversionLossPolicy
		wantError  bool
	}{
		{name: "default", want: types.ConversionLossPolicyStrict},
		{name: "strict", requested: types.ConversionLossPolicyStrict, want: types.ConversionLossPolicyStrict},
		{name: "safe unauthorized", requested: types.ConversionLossPolicySafe, wantError: true},
		{name: "allow unauthorized", requested: types.ConversionLossPolicyAllow, wantError: true},
		{name: "safe authorized", requested: types.ConversionLossPolicySafe, authorized: true, want: types.ConversionLossPolicySafe},
		{name: "allow authorized", requested: types.ConversionLossPolicyAllow, authorized: true, want: types.ConversionLossPolicyAllow},
		{name: "invalid", requested: types.ConversionLossPolicy("invalid"), authorized: true, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveOrdinaryConversionPolicy(test.requested, test.authorized)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestOrdinaryConversionCapabilitiesDoNotOverstateStructuredOrParallelSupport(t *testing.T) {
	t.Parallel()

	chatToResponses, ok := LookupOrdinaryConversionCapability(types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses)
	require.True(t, ok)
	assert.Contains(t, chatToResponses.Supported, ConversionFeatureStructuredOutput)
	assert.Contains(t, chatToResponses.Supported, ConversionFeatureParallelToolCalls)
	assert.NotContains(t, conversionLossCodes(chatToResponses.Losses), "structured_output_unsupported")

	geminiToChat, ok := LookupOrdinaryConversionCapability(types.RelayFormatGemini, types.RelayFormatOpenAI)
	require.True(t, ok)
	assert.NotContains(t, geminiToChat.Supported, ConversionFeatureStructuredOutput)
	assert.NotContains(t, geminiToChat.Supported, ConversionFeatureParallelToolCalls)
	assert.Contains(t, conversionLossCodes(geminiToChat.Losses), "structured_output_unsupported")
	assert.Contains(t, conversionLossCodes(geminiToChat.Losses), "parallel_tool_calls_unsupported")
}

func conversionLossCodes(losses []OrdinaryConversionLoss) []string {
	codes := make([]string, 0, len(losses))
	for _, loss := range losses {
		codes = append(codes, loss.Code)
	}
	return codes
}
