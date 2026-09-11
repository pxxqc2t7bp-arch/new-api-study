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

func TestOrdinaryConversionCapabilitiesUseEmittedDiagnosticCodes(t *testing.T) {
	t.Parallel()

	capabilities := OrdinaryConversionCapabilities()
	emittedCodes := []string{
		types.ConversionDiagnosticCodeStructuredOutputUnsupported,
		types.ConversionDiagnosticCodeUnsupportedFunctionStrict,
		types.ConversionDiagnosticCodeUnsupportedParallelToolControl,
		types.ConversionDiagnosticCodeUnverifiedToolMapping,
		types.ConversionDiagnosticCodeUnsupportedHostedTool,
		types.ConversionDiagnosticCodeVendorSpecificToolUnsupported,
		types.ConversionDiagnosticCodeEncryptedReasoningUnsupported,
		types.ConversionDiagnosticCodeSessionReferenceUnsupported,
	}
	for _, capability := range capabilities {
		for _, code := range conversionLossCodes(capability.Losses) {
			assert.Contains(t, emittedCodes, code)
		}
		for _, code := range conversionLossCodes(capability.Rejections) {
			assert.Contains(t, emittedCodes, code)
		}
	}

	responsesToGemini, ok := LookupOrdinaryConversionCapability(types.RelayFormatOpenAIResponses, types.RelayFormatGemini)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{types.ConversionDiagnosticCodeUnsupportedFunctionStrict}, conversionLossCodes(responsesToGemini.Losses))
	assert.ElementsMatch(t, []string{
		types.ConversionDiagnosticCodeEncryptedReasoningUnsupported,
		types.ConversionDiagnosticCodeUnsupportedHostedTool,
		types.ConversionDiagnosticCodeUnverifiedToolMapping,
		types.ConversionDiagnosticCodeUnsupportedParallelToolControl,
	}, conversionLossCodes(responsesToGemini.Rejections))

	geminiToChat, ok := LookupOrdinaryConversionCapability(types.RelayFormatGemini, types.RelayFormatOpenAI)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{types.ConversionDiagnosticCodeStructuredOutputUnsupported}, conversionLossCodes(geminiToChat.Losses))
	assert.ElementsMatch(t, []string{
		types.ConversionDiagnosticCodeEncryptedReasoningUnsupported,
		types.ConversionDiagnosticCodeSessionReferenceUnsupported,
		types.ConversionDiagnosticCodeUnsupportedHostedTool,
	}, conversionLossCodes(geminiToChat.Rejections))

	chatToResponses, ok := LookupOrdinaryConversionCapability(types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses)
	require.True(t, ok)
	assert.Empty(t, chatToResponses.Losses)
	assert.ElementsMatch(t, []string{types.ConversionDiagnosticCodeUnsupportedHostedTool}, conversionLossCodes(chatToResponses.Rejections))
	assert.Contains(t, chatToResponses.Supported, ConversionFeatureStructuredOutput)
	assert.Contains(t, chatToResponses.Supported, ConversionFeatureParallelToolCalls)
}

func TestOrdinaryConversionCapabilitiesAttachDiagnosticsToMatchingFeatures(t *testing.T) {
	t.Parallel()

	claudeToGemini, ok := LookupOrdinaryConversionCapability(types.RelayFormatClaude, types.RelayFormatGemini)
	require.True(t, ok)

	assert.Equal(t, ConversionFeatureStructuredOutput, conversionLossByCode(t, claudeToGemini.Losses, types.ConversionDiagnosticCodeStructuredOutputUnsupported).Feature)
	assert.Equal(t, ConversionFeatureFunctionTools, conversionLossByCode(t, claudeToGemini.Losses, types.ConversionDiagnosticCodeUnsupportedFunctionStrict).Feature)
	assert.Equal(t, ConversionFeatureParallelToolCalls, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeUnsupportedParallelToolControl).Feature)
	assert.Equal(t, ConversionFeatureFunctionTools, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeUnverifiedToolMapping).Feature)
	assert.Equal(t, ConversionFeatureFunctionTools, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeUnsupportedHostedTool).Feature)
	assert.Equal(t, ConversionFeatureFunctionTools, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeVendorSpecificToolUnsupported).Feature)
	assert.Equal(t, ConversionFeatureReasoning, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeEncryptedReasoningUnsupported).Feature)
	assert.Equal(t, ConversionFeatureSessionState, conversionLossByCode(t, claudeToGemini.Rejections, types.ConversionDiagnosticCodeSessionReferenceUnsupported).Feature)
}

func conversionLossCodes(losses []OrdinaryConversionLoss) []string {
	codes := make([]string, 0, len(losses))
	for _, loss := range losses {
		codes = append(codes, loss.Code)
	}
	return codes
}

func conversionLossByCode(t *testing.T, losses []OrdinaryConversionLoss, code string) OrdinaryConversionLoss {
	t.Helper()
	for _, loss := range losses {
		if loss.Code == code {
			return loss
		}
	}
	require.FailNow(t, "missing conversion loss", "code=%s", code)
	return OrdinaryConversionLoss{}
}
