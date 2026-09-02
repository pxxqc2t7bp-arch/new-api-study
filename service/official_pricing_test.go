package service

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/billing_setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOfficialPricingTables(t *testing.T) {
	t.Run("parses a complete USD per million token table", func(t *testing.T) {
		html := []byte(`
			<table>
				<caption>USD per 1M tokens</caption>
				<tr><th>Model</th><th>Input</th><th>Cached input</th><th>Output</th></tr>
				<tr><td>gpt-test</td><td>$1.25</td><td>$0.25</td><td>$5.00</td></tr>
			</table>`)
		prices, err := parseOfficialPricingTables(
			"openai",
			"https://openai.com/api/pricing/",
			html,
			map[string]string{"gpt-test": "openai"},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, prices, 1)
		assert.Equal(t, 1.25, prices[0].InputPerM)
		assert.Equal(t, 0.25, prices[0].CachedReadPerM)
		assert.Equal(t, 5.0, prices[0].OutputPerM)
		assert.Len(t, prices[0].EvidenceHash, 64)
		require.NoError(t, billing_setting.SmokeTestExpr(officialPriceExpression(prices[0])))
	})

	t.Run("rejects tables without a per million unit", func(t *testing.T) {
		html := []byte(`
			<table>
				<tr><th>Model</th><th>Input</th><th>Output</th></tr>
				<tr><td>gpt-test</td><td>$1.25</td><td>$5.00</td></tr>
			</table>`)
		_, err := parseOfficialPricingTables(
			"openai",
			"https://openai.com/api/pricing/",
			html,
			map[string]string{"gpt-test": "openai"},
			nil,
		)
		assert.Error(t, err)
	})

	t.Run("requires exact or explicit alias model names", func(t *testing.T) {
		html := []byte(`
			<table>
				<caption>USD per 1M tokens</caption>
				<tr><th>Model</th><th>Input</th><th>Output</th></tr>
				<tr><td>GPT Test Display</td><td>$1.25</td><td>$5.00</td></tr>
			</table>`)
		_, err := parseOfficialPricingTables(
			"openai",
			"https://openai.com/api/pricing/",
			html,
			map[string]string{"gpt-test": "openai"},
			nil,
		)
		assert.Error(t, err)

		prices, err := parseOfficialPricingTables(
			"openai",
			"https://openai.com/api/pricing/",
			html,
			map[string]string{"gpt-test": "openai"},
			map[string]string{"GPT Test Display": "gpt-test"},
		)
		require.NoError(t, err)
		require.Len(t, prices, 1)
		assert.Equal(t, "gpt-test", prices[0].ModelName)
	})
}
