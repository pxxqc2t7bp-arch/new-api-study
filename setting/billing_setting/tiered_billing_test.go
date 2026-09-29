package billing_setting

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveModelBillingDecisionKeepsPublishedBundleCoherent(t *testing.T) {
	const (
		modelName   = "atomic-model-decision"
		requestExpr = `tier("request", p * 2)`
	)

	previous := config.GlobalConfig.ExportAllConfigs()
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(previous))
	})

	transitions := []struct {
		name       string
		mode       string
		expression string
		exists     bool
	}{
		{name: "ratio before publication", mode: BillingModeRatio},
		{name: "tiered publication", mode: BillingModeTieredExpr, expression: requestExpr, exists: true},
		{name: "ratio after publication", mode: BillingModeRatio},
	}
	for _, transition := range transitions {
		t.Run(transition.name, func(t *testing.T) {
			modes := fmt.Sprintf(`{%q:%q}`, modelName, transition.mode)
			expressions := `{}`
			if transition.exists {
				expressions = fmt.Sprintf(`{%q:%q}`, modelName, transition.expression)
			}
			require.NoError(t, UpdateBillingSettingOptions(map[string]string{
				BillingModeOption:       modes,
				BillingExprOption:       expressions,
				PluginBillingExprOption: `{}`,
			}))

			decision := ResolveModelBillingDecision(modelName)

			assert.Equal(t, transition.mode, decision.Mode)
			assert.Equal(t, transition.expression, decision.Expression)
			assert.Equal(t, transition.exists, decision.ExpressionExists)
		})
	}
}

func TestResolveTaskBillingDecisionKeepsModeAndSelectedExpressionCoherent(t *testing.T) {
	const (
		pluginKey          = "atomic-provider"
		modelName          = "atomic-task-alias"
		mappedModel        = "atomic-task-target"
		mainExpr           = `tier("main", u("image_count") * 0.01)`
		mappedExpr         = `tier("mapped", u("image_count") * 0.02)`
		providerExpr       = `tier("provider", u("image_count") * 0.03)`
		mappedProviderExpr = `tier("mapped-provider", u("image_count") * 0.04)`
	)

	previous := config.GlobalConfig.ExportAllConfigs()
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(previous))
	})

	transitions := []struct {
		name              string
		modes             string
		expressions       string
		pluginExpressions string
		expected          BillingDecision
	}{
		{
			name:              "ratio before publication",
			modes:             fmt.Sprintf(`{%q:%q}`, modelName, BillingModeRatio),
			expressions:       `{}`,
			pluginExpressions: `{}`,
			expected:          BillingDecision{Mode: BillingModeRatio},
		},
		{
			name:              "model tiered publication",
			modes:             fmt.Sprintf(`{%q:%q}`, modelName, BillingModeTieredExpr),
			expressions:       fmt.Sprintf(`{%q:%q}`, modelName, mainExpr),
			pluginExpressions: `{}`,
			expected:          BillingDecision{Mode: BillingModeTieredExpr, Expression: mainExpr, ExpressionExists: true},
		},
		{
			name:        "provider override ignores ratio mode",
			modes:       fmt.Sprintf(`{%q:%q}`, modelName, BillingModeRatio),
			expressions: `{}`,
			pluginExpressions: fmt.Sprintf(`{%q:%q}`,
				PluginBillingExprKey(pluginKey, modelName), providerExpr),
			expected: BillingDecision{Mode: BillingModeTieredExpr, Expression: providerExpr, ExpressionExists: true},
		},
		{
			name:  "alias provider override precedes mapped override",
			modes: fmt.Sprintf(`{%q:%q,%q:%q}`, modelName, BillingModeTieredExpr, mappedModel, BillingModeTieredExpr),
			expressions: fmt.Sprintf(`{%q:%q,%q:%q}`,
				modelName, mainExpr, mappedModel, mappedExpr),
			pluginExpressions: fmt.Sprintf(`{%q:%q,%q:%q}`,
				PluginBillingExprKey(pluginKey, modelName), providerExpr,
				PluginBillingExprKey(pluginKey, mappedModel), mappedProviderExpr),
			expected: BillingDecision{Mode: BillingModeTieredExpr, Expression: providerExpr, ExpressionExists: true},
		},
		{
			name:  "mapped provider override precedes alias main expression",
			modes: fmt.Sprintf(`{%q:%q,%q:%q}`, modelName, BillingModeTieredExpr, mappedModel, BillingModeTieredExpr),
			expressions: fmt.Sprintf(`{%q:%q,%q:%q}`,
				modelName, mainExpr, mappedModel, mappedExpr),
			pluginExpressions: fmt.Sprintf(`{%q:%q}`,
				PluginBillingExprKey(pluginKey, mappedModel), mappedProviderExpr),
			expected: BillingDecision{Mode: BillingModeTieredExpr, Expression: mappedProviderExpr, ExpressionExists: true},
		},
		{
			name:  "alias main expression precedes mapped main expression",
			modes: fmt.Sprintf(`{%q:%q,%q:%q}`, modelName, BillingModeTieredExpr, mappedModel, BillingModeTieredExpr),
			expressions: fmt.Sprintf(`{%q:%q,%q:%q}`,
				modelName, mainExpr, mappedModel, mappedExpr),
			pluginExpressions: `{}`,
			expected:          BillingDecision{Mode: BillingModeTieredExpr, Expression: mainExpr, ExpressionExists: true},
		},
		{
			name:              "tiered alias with missing expression fails closed",
			modes:             fmt.Sprintf(`{%q:%q,%q:%q}`, modelName, BillingModeTieredExpr, mappedModel, BillingModeTieredExpr),
			expressions:       fmt.Sprintf(`{%q:%q}`, mappedModel, mappedExpr),
			pluginExpressions: `{}`,
			expected:          BillingDecision{Mode: BillingModeTieredExpr},
		},
		{
			name:              "mapped provider override forces tiered mode",
			modes:             fmt.Sprintf(`{%q:%q,%q:%q}`, modelName, BillingModeRatio, mappedModel, BillingModeRatio),
			expressions:       `{}`,
			pluginExpressions: fmt.Sprintf(`{%q:%q}`, PluginBillingExprKey(pluginKey, mappedModel), mappedProviderExpr),
			expected:          BillingDecision{Mode: BillingModeTieredExpr, Expression: mappedProviderExpr, ExpressionExists: true},
		},
		{
			name:              "mapped target fallback",
			modes:             fmt.Sprintf(`{%q:%q,%q:%q}`, modelName, BillingModeRatio, mappedModel, BillingModeTieredExpr),
			expressions:       fmt.Sprintf(`{%q:%q}`, mappedModel, mappedExpr),
			pluginExpressions: `{}`,
			expected:          BillingDecision{Mode: BillingModeTieredExpr, Expression: mappedExpr, ExpressionExists: true},
		},
		{
			name:              "tiered mapped target with missing expression fails closed",
			modes:             fmt.Sprintf(`{%q:%q,%q:%q}`, modelName, BillingModeRatio, mappedModel, BillingModeTieredExpr),
			expressions:       `{}`,
			pluginExpressions: `{}`,
			expected:          BillingDecision{Mode: BillingModeTieredExpr},
		},
		{
			name:              "ratio after publication",
			modes:             fmt.Sprintf(`{%q:%q,%q:%q}`, modelName, BillingModeRatio, mappedModel, BillingModeRatio),
			expressions:       `{}`,
			pluginExpressions: `{}`,
			expected:          BillingDecision{Mode: BillingModeRatio},
		},
	}
	for _, transition := range transitions {
		t.Run(transition.name, func(t *testing.T) {
			require.NoError(t, UpdateBillingSettingOptions(map[string]string{
				BillingModeOption:       transition.modes,
				BillingExprOption:       transition.expressions,
				PluginBillingExprOption: transition.pluginExpressions,
			}))

			assert.Equal(t, transition.expected, ResolveTaskBillingDecision(pluginKey, modelName, mappedModel))
		})
	}
}

func TestSmokeTestTaskExprValidatesDeclaredUsageVectors(t *testing.T) {
	videoSchema := map[string]jsplugin.UsageFieldSchema{
		"seconds": {Type: "number", Unit: "second"},
		"mode":    {Enum: []string{"std", "pro"}},
		"quality": {Enum: []string{"sd", "hd"}},
	}

	tests := []struct {
		name          string
		schema        map[string]jsplugin.UsageFieldSchema
		expression    string
		expectedError string
	}{
		{
			name:          "fixed prices are not task usage prices",
			schema:        videoSchema,
			expression:    `true ? tier("normal", u("seconds") * 0.4) : tier("fixed", fixed(0.01))`,
			expectedError: "fixed pricing is not supported for task usage expressions",
		},
		{
			name:       "declared numeric and enum facts",
			schema:     videoSchema,
			expression: `u("mode") == "pro" ? tier("pro", u("seconds") * 0.8) : tier("std", u("seconds") * 0.4)`,
		},
		{
			name:          "undeclared literal key",
			schema:        videoSchema,
			expression:    `tier("base", u("clips") * 0.1)`,
			expectedError: `usage key "clips" is not declared`,
		},
		{
			name:          "negative duration boundary",
			schema:        videoSchema,
			expression:    fmt.Sprintf(`u("seconds") == %d ? -1 : 0`, relaycommon.MaxTaskDurationSeconds),
			expectedError: "result must be finite and non-negative",
		},
		{
			name:          "negative count boundary",
			schema:        map[string]jsplugin.UsageFieldSchema{"clips": {Type: "number", Unit: "count"}},
			expression:    fmt.Sprintf(`u("clips") == %d ? -1 : 0`, dto.MaxImageN),
			expectedError: "result must be finite and non-negative",
		},
		{
			name:          "negative token boundary",
			schema:        map[string]jsplugin.UsageFieldSchema{"tokens": {Type: "number", Unit: "token"}},
			expression:    fmt.Sprintf(`u("tokens") == %d ? -1 : 0`, common.MaxQuota),
			expectedError: "result must be finite and non-negative",
		},
		{
			name:          "negative credit boundary",
			schema:        map[string]jsplugin.UsageFieldSchema{"units": {Type: "number", Unit: "credit"}},
			expression:    fmt.Sprintf(`u("units") == %d ? -1 : 0`, common.MaxQuota),
			expectedError: "result must be finite and non-negative",
		},
		{
			name:          "negative enum combination",
			schema:        videoSchema,
			expression:    `u("mode") == "pro" && u("quality") == "hd" ? -1 : 0`,
			expectedError: "result must be finite and non-negative",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := SmokeTestTaskExpr(testCase.expression, testCase.schema)
			if testCase.expectedError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, testCase.expectedError)
		})
	}
}

func TestSmokeTestTaskExprCapsOversizedEnumProductsAtLastCombination(t *testing.T) {
	schema := make(map[string]jsplugin.UsageFieldSchema, 7)
	condition := ""
	for index := range 7 {
		schema[fmt.Sprintf("enum_%d", index)] = jsplugin.UsageFieldSchema{Enum: []string{"first", "middle", "last"}}
		if condition != "" {
			condition += " && "
		}
		condition += fmt.Sprintf(`u("enum_%d") == "last"`, index)
	}

	err := SmokeTestTaskExpr(condition+" ? -1 : 0", schema)
	require.ErrorContains(t, err, "result must be finite and non-negative")
}

func TestSmokeTestExprRejectsTaskUsageWithoutSchema(t *testing.T) {
	err := SmokeTestExpr(`u("mode") == "std" ? 1 : 2`)
	require.Error(t, err)
	assert.ErrorContains(t, err, "mode")
	assert.ErrorContains(t, err, "no task plugin usage schema")

	require.NoError(t, SmokeTestExpr(`tier("base", p * 2 + c * 8)`))
}
