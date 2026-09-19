package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRejectConversionLossStrictSkipsOnlyKnownBenignNormalizations(t *testing.T) {
	benign := []ConversionDiagnostic{
		{Code: ConversionDiagnosticCodeClaudeThinkingTypeCoerced, Severity: ConversionDiagnosticWarning},
		{Code: ConversionDiagnosticCodeClaudeBudgetAdjusted, Severity: ConversionDiagnosticWarning},
		{Code: ConversionDiagnosticCodeClaudeMaxTokensRaised, Severity: ConversionDiagnosticWarning},
		{Code: ConversionDiagnosticCodeGeminiLevelAdjusted, Severity: ConversionDiagnosticWarning},
		{Code: ConversionDiagnosticCodeGeminiBudgetToLevel, Severity: ConversionDiagnosticWarning},
	}
	require.NoError(t, RejectConversionLoss(ConversionLossPolicyStrict, benign))

	rejected := []ConversionDiagnostic{
		{Code: "gemini_budget_level_conflict", Severity: ConversionDiagnosticWarning},
		{Code: "gemini_budget_clamped", Severity: ConversionDiagnosticWarning},
		{Code: "gemini_thinking_unsupported", Severity: ConversionDiagnosticWarning},
		{Code: "gemini_unknown_capability", Severity: ConversionDiagnosticWarning},
		{Code: ConversionDiagnosticCodeClaudeSamplingRemoved, Severity: ConversionDiagnosticWarning},
		{Code: "future_converter_warning", Severity: ConversionDiagnosticWarning},
	}

	var loss *ConversionLossError
	require.ErrorAs(t, RejectConversionLoss(ConversionLossPolicyStrict, rejected), &loss)
	assert.Equal(t, rejected, loss.Diagnostics)
}
