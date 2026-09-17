package service

import (
	"context"
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

	t.Run("parses multi-row short and long context pricing", func(t *testing.T) {
		html := []byte(`
			<p>Prices per 1M tokens.</p>
			<table>
				<tr><th></th><th>Short context</th><th>Long context</th></tr>
				<tr><th>Model</th><th>Input</th><th>Cached input</th><th>Cache writes</th><th>Output</th><th>Input</th><th>Cached input</th><th>Cache writes</th><th>Output</th></tr>
				<tr><td>gpt-5.6-sol</td><td>$4.00</td><td>$0.40</td><td>$5.00</td><td>$20.00</td><td>$8.00</td><td>$0.80</td><td>$10.00</td><td>$30.00</td></tr>
			</table>`)
		prices, err := parseOfficialPricingTables(
			"openai",
			"https://developers.openai.com/api/docs/pricing",
			html,
			map[string]string{"gpt-5.6-sol": "openai"},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, prices, 1)
		assert.Equal(t, int64(272000), prices[0].LongContextThreshold)
		assert.Equal(t, 4.0, prices[0].InputPerM)
		assert.Equal(t, 8.0, prices[0].LongInputPerM)
		assert.Contains(t, officialPriceExpression(prices[0]), "len <= 272000")
		require.NoError(t, billing_setting.SmokeTestExpr(officialPriceExpression(prices[0])))
	})

	t.Run("aligns xAI rowspans and strips context annotations", func(t *testing.T) {
		html := []byte(`
			<p>Prices per 1M tokens.</p>
			<table>
				<tr><th>Model</th><th>Context</th><th>Short context</th><th>Long context</th></tr>
				<tr><th>Input</th><th>Cached</th><th>Output</th><th>Input</th><th>Cached</th><th>Output</th></tr>
				<tr><td>grok-4.5 Long context >= 200k tokens</td><td>256k</td><td>$1.50</td><td>$0.30</td><td>$4.50</td><td>$3.00</td><td>$0.60</td><td>$9.00</td></tr>
				<tr><td>grok-4.6 Long context >= 200k tokens</td><td>500k</td><td>$2.00</td><td>$0.50</td><td>$6.00</td><td>$4.00</td><td>$1.00</td><td>$12.00</td></tr>
			</table>`)
		prices, err := parseOfficialPricingTables(
			"xai",
			"https://docs.x.ai/developers/pricing",
			html,
			map[string]string{"grok-4.5": "xai", "grok-4.6": "xai"},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, prices, 2)
		assert.Equal(t, "grok-4.5", prices[0].ModelName)
		assert.Equal(t, int64(200000), prices[0].LongContextThreshold)
		assert.Equal(t, 1.5, prices[0].InputPerM)
		assert.Equal(t, 0.3, prices[0].CachedReadPerM)
		assert.Equal(t, 3.0, prices[0].LongInputPerM)
		assert.Equal(t, 0.6, prices[0].LongCachedReadPerM)
		assert.Equal(t, "grok-4.6", prices[1].ModelName)
		assert.Equal(t, int64(200000), prices[1].LongContextThreshold)
		assert.Equal(t, 2.0, prices[1].InputPerM)
		assert.Equal(t, 0.5, prices[1].CachedReadPerM)
		assert.Equal(t, 4.0, prices[1].LongInputPerM)
		assert.Equal(t, 1.0, prices[1].LongCachedReadPerM)
	})

	t.Run("uses separate Anthropic cache write prices", func(t *testing.T) {
		html := []byte(`
			<p>All prices are in USD per MTok.</p>
			<table>
				<tr><th>Model</th><th>Base Input Tokens</th><th>5m Cache Writes</th><th>1h Cache Writes</th><th>Cache Hits &amp; Refreshes</th><th>Output Tokens</th></tr>
				<tr><td>Claude Opus 5</td><td>$5</td><td>$6.25</td><td>$10</td><td>$0.50</td><td>$25</td></tr>
			</table>`)
		prices, err := parseOfficialPricingTables(
			"anthropic",
			"https://platform.claude.com/docs/en/about-claude/pricing",
			html,
			map[string]string{"claude-opus-5": "anthropic"},
			map[string]string{"Claude Opus 5": "claude-opus-5"},
		)
		require.NoError(t, err)
		require.Len(t, prices, 1)
		assert.Equal(t, 6.25, prices[0].CacheWritePerM)
		assert.Equal(t, 10.0, prices[0].CacheWrite1hPerM)
		assert.Equal(t, 0.5, prices[0].CachedReadPerM)
	})

	t.Run("keeps the first standard price when later modes repeat a model", func(t *testing.T) {
		html := []byte(`
			<p>Prices per 1M tokens.</p>
			<table>
				<tr><th>Model</th><th>Input</th><th>Output</th></tr>
				<tr><td>gpt-test</td><td>$1</td><td>$4</td></tr>
			</table>
			<table>
				<tr><th>Model</th><th>Input</th><th>Output</th></tr>
				<tr><td>gpt-test</td><td>$2</td><td>$8</td></tr>
			</table>`)
		prices, err := parseOfficialPricingTables(
			"openai",
			"https://developers.openai.com/api/docs/pricing",
			html,
			map[string]string{"gpt-test": "openai"},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, prices, 1)
		assert.Equal(t, 1.0, prices[0].InputPerM)
		assert.Equal(t, 4.0, prices[0].OutputPerM)
	})
}

func TestOfficialPricingSourceAndProxyValidation(t *testing.T) {
	assert.True(t, officialPricingHostAllowed("developers.openai.com"))
	assert.False(t, officialPricingHostAllowed("pricing.example.com"))

	t.Setenv("UPSTREAM_PRICING_PROXY_URL", "file:///tmp/proxy.sock")
	_, err := fetchOfficialPricingPage(
		context.Background(),
		"https://developers.openai.com/api/docs/pricing",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid official pricing proxy")
}
