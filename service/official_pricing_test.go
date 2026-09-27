package service

import (
	"context"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type officialPricingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f officialPricingRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

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

	t.Run("parses Astra short and long context pricing", func(t *testing.T) {
		html := []byte(`
			<p>Prices per 1M tokens.</p>
			<table>
				<tr><th></th><th>Short context</th><th>Long context</th></tr>
				<tr><th>Model</th><th>Input</th><th>Cached input</th><th>Cache writes</th><th>Output</th><th>Input</th><th>Cached input</th><th>Cache writes</th><th>Output</th></tr>
				<tr><td>gpt-6-astra</td><td>$10.00</td><td>$1.00</td><td>$12.50</td><td>$50.00</td><td>$20.00</td><td>$2.00</td><td>$25.00</td><td>$75.00</td></tr>
			</table>`)
		prices, err := parseOfficialPricingTables(
			"openai",
			"https://developers.openai.com/api/docs/pricing",
			html,
			map[string]string{"gpt-6-astra": "openai"},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, prices, 1)
		assert.Equal(t, "gpt-6-astra", prices[0].ModelName)
		assert.Equal(t, int64(272000), prices[0].LongContextThreshold)
		assert.Equal(t, 10.0, prices[0].InputPerM)
		assert.Equal(t, 12.5, prices[0].CacheWritePerM)
		assert.Equal(t, 20.0, prices[0].LongInputPerM)
		assert.Equal(t, 75.0, prices[0].LongOutputPerM)
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

	t.Run("resolves Fable through an explicit Anthropic alias", func(t *testing.T) {
		html := []byte(`
			<p>All prices are in USD per MTok.</p>
			<table>
				<tr><th>Model</th><th>Base Input Tokens</th><th>5m Cache Writes</th><th>1h Cache Writes</th><th>Cache Hits &amp; Refreshes</th><th>Output Tokens</th></tr>
				<tr><td>Claude Fable 5</td><td>$10</td><td>$12.50</td><td>$20</td><td>$1</td><td>$50</td></tr>
			</table>`)
		prices, err := parseOfficialPricingTables(
			"anthropic",
			"https://platform.claude.com/docs/en/about-claude/pricing",
			html,
			map[string]string{"claude-fable-5": "anthropic"},
			map[string]string{"Claude Fable 5": "claude-fable-5"},
		)
		require.NoError(t, err)
		require.Len(t, prices, 1)
		assert.Equal(t, "claude-fable-5", prices[0].ModelName)
		assert.Equal(t, 10.0, prices[0].InputPerM)
		assert.Equal(t, 50.0, prices[0].OutputPerM)
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
	assert.True(t, officialPricingHostAllowed("api-docs.deepseek.com"))
	assert.True(t, officialPricingHostAllowed("docs.volcengine.com"))
	assert.False(t, officialPricingHostAllowed("pricing.example.com"))

	t.Setenv("UPSTREAM_PRICING_PROXY_URL", "file:///tmp/proxy.sock")
	_, err := fetchOfficialPricingPage(
		context.Background(),
		"https://developers.openai.com/api/docs/pricing",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid official pricing proxy")
}

func TestParseDeepSeekPricingBuildsPeakAndOffPeakExpression(t *testing.T) {
	body := []byte(`<html><body>
deepseek-v4-flash deepseek-v4-pro off-peak peak
$0.007 $0.014 $0.22 $0.44 $0.66 $1.32 $1.98 $3.96
</body></html>`)
	prices, err := parseDeepSeekPricing(
		"https://api-docs.deepseek.com/quick_start/pricing/",
		body,
		map[string]string{"deepseek-v4-flash-原厂": "deepseek"},
	)
	require.NoError(t, err)
	require.Len(t, prices, 1)

	expression := officialPriceExpression(prices[0])
	_, err = billingexpr.CompileFromCache(expression)
	require.NoError(t, err)
	assert.Contains(t, expression, `tier("peak"`)
	assert.Contains(t, expression, `tier("off_peak"`)
}

func TestParseVolcenginePricingAppliesAndExpiresPromotion(t *testing.T) {
	body := []byte(`<html><body>
doubao-seedance-2.5 doubao-seedance-2.0-fast doubao-seedance-2.0-mini
70.00 46.00 37.00 23.00
</body></html>`)
	prices, err := parseVolcenginePricing(
		"https://docs.volcengine.com/docs/82379/1544106?lang=zh",
		body,
		map[string]string{
			"doubao-seedance-2-5-260628":               "字节跳动",
			"doubao-seedance-2-5-draft-preview-260828": "字节跳动",
			"doubao-seedance-2-0-260128":               "字节跳动",
			"doubao-seedance-2-0-fast-260128":          "字节跳动",
			"doubao-seedance-2-0-mini-260615":          "字节跳动",
		},
		time.Now(),
	)
	require.NoError(t, err)
	require.Len(t, prices, 5)

	byModel := make(map[string]officialTokenPrice, len(prices))
	for _, price := range prices {
		byModel[price.ModelName] = price
	}
	for _, modelName := range []string{
		"doubao-seedance-2-5-260628",
		"doubao-seedance-2-5-draft-preview-260828",
		"doubao-seedance-2-0-fast-260128",
		"doubao-seedance-2-0-mini-260615",
	} {
		generated := officialPriceExpression(byModel[modelName])
		assert.True(t, strings.HasPrefix(generated, "v2:"), modelName)
		assert.Equal(t, 1, strings.Count(generated, "v2:"), modelName)
		require.NoError(t, billing_setting.SmokeTestTaskExpr(generated, byModel[modelName].UsageSchema), modelName)
	}
	assert.False(t, strings.HasPrefix(
		officialPriceExpression(byModel["doubao-seedance-2-0-260128"]),
		"v2:",
	))

	expression := officialPriceExpression(byModel["doubao-seedance-2-5-260628"])
	assert.Equal(
		t,
		expression,
		officialPriceExpression(byModel["doubao-seedance-2-5-draft-preview-260828"]),
	)
	_, err = billingexpr.CompileFromCache(expression)
	require.NoError(t, err)

	promotionStart := time.Date(2026, time.August, 14, 14, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60)).Unix()
	promotionEnd := time.Date(2026, time.September, 17, 14, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60)).Unix()
	usage := map[string]any{
		"tokens":      1_000_000.0,
		"resolution":  "1080p",
		"video_input": "video",
	}
	for _, testCase := range []struct {
		name      string
		evaluated int64
		wantTier  string
		wantCNY   float64
	}{
		{name: "before promotion", evaluated: promotionStart - 1, wantTier: "list_1080p", wantCNY: 46},
		{name: "inside promotion", evaluated: promotionStart, wantTier: "promotion_1080p", wantCNY: 33.12},
		{name: "after promotion", evaluated: promotionEnd, wantTier: "list_1080p", wantCNY: 46},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			value, trace, runErr := billingexpr.RunExprWithRequest(
				expression,
				billingexpr.TokenParams{},
				billingexpr.RequestInput{Usage: usage, EvaluatedAtUnix: testCase.evaluated},
			)
			require.NoError(t, runErr)
			assert.InDelta(t, testCase.wantCNY/officialCNYPerUSD, value, 1e-9)
			assert.Equal(t, testCase.wantTier, trace.MatchedTier)
		})
	}

	snapshot := &billingexpr.BillingSnapshot{
		BillingMode:      billing_setting.BillingModeTieredExpr,
		BillingBasis:     billingexpr.BillingBasisTask,
		ExprString:       expression,
		ExprHash:         billingexpr.ExprHashString(expression),
		ExprVersion:      billingexpr.ExprVersion(expression),
		GroupRatio:       1,
		QuotaPerUnit:     1,
		PricingTimeUnix:  promotionStart,
		TaskUsageBilling: true,
	}
	settled, _, err := EvaluateTaskCompletionUsage(snapshot, usage)
	require.NoError(t, err)
	assert.InDelta(t, 33.12/officialCNYPerUSD, settled.ActualQuotaBeforeGroup, 1e-9)
	assert.Equal(t, "promotion_1080p", settled.MatchedTier)
}

func TestOfficialPricingSyncPersistsVolcengineV2ExpressionOnce(t *testing.T) {
	const modelName = "doubao-seedance-2-5-260628"
	body := []byte(`<html><body>
doubao-seedance-2.5 doubao-seedance-2.0-fast doubao-seedance-2.0-mini
70.00 46.00 37.00 23.00
</body></html>`)

	previousDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(
		&model.Option{},
		&model.Vendor{},
		&model.Model{},
		&model.UpstreamGroup{},
		&model.UpstreamPriceEvidence{},
		&model.Ability{},
		&model.Channel{},
	))
	model.DB = database

	previousHTTPClient := httpClient
	previousSources := officialPricingSourceURLs
	requests := 0
	httpClient = &http.Client{Transport: officialPricingRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Request:    request,
		}, nil
	})}
	officialPricingSourceURLs = map[string][]string{
		"volcengine": {"https://docs.volcengine.com/docs/82379/1544106?lang=zh"},
	}
	t.Setenv("UPSTREAM_PRICING_PROXY_URL", "")

	savedConfig := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		savedConfig[key] = value
		return nil
	}))
	common.OptionMapRWMutex.Lock()
	previousOptionMap := common.OptionMap
	common.OptionMap = maps.Clone(common.OptionMap)
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		model.DB = previousDB
		httpClient = previousHTTPClient
		officialPricingSourceURLs = previousSources
		model.InvalidatePricingCache()
		require.NoError(t, config.GlobalConfig.LoadFromDB(savedConfig))
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptionMap
		common.OptionMapRWMutex.Unlock()
	})
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    `{}`,
		"billing_setting.billing_expr":    `{}`,
		"group_ratio_setting.group_ratio": `{"default":1,"cxy":1}`,
	}))

	vendor := model.Vendor{Name: "字节跳动", Status: 1}
	require.NoError(t, database.Create(&vendor).Error)
	require.NoError(t, database.Create(&model.Model{
		ModelName:    modelName,
		VendorID:     vendor.Id,
		Status:       1,
		SyncOfficial: 1,
	}).Error)

	prices, err := parseVolcenginePricing(
		officialPricingSourceURLs["volcengine"][0],
		body,
		map[string]string{modelName: "字节跳动"},
		time.Unix(1_800_000_000, 0),
	)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	validatedExpression := officialPriceExpression(prices[0])

	first, err := RunOfficialPricingSync(t.Context(), time.Unix(1_800_000_000, 0))
	require.NoError(t, err)
	assert.Equal(t, OfficialPricingSyncSummary{
		Fetched: 1,
		Parsed:  1,
		Applied: 1,
	}, first)

	second, err := RunOfficialPricingSync(t.Context(), time.Unix(1_800_000_001, 0))
	require.NoError(t, err)
	assert.Equal(t, OfficialPricingSyncSummary{
		Fetched:   1,
		Parsed:    1,
		Unchanged: 1,
	}, second)
	assert.Equal(t, 2, requests)

	var expressionOption model.Option
	require.NoError(t, database.Where("key = ?", "billing_setting.billing_expr").First(&expressionOption).Error)
	var persistedExpressions map[string]string
	require.NoError(t, common.Unmarshal([]byte(expressionOption.Value), &persistedExpressions))
	assert.Equal(t, validatedExpression, persistedExpressions[modelName])
	assert.Equal(t, validatedExpression, func() string {
		expression, _ := billing_setting.GetBillingExpr(modelName)
		return expression
	}())

	var expressionRows int64
	require.NoError(t, database.Model(&model.Option{}).
		Where("key = ?", "billing_setting.billing_expr").
		Count(&expressionRows).Error)
	assert.Equal(t, int64(1), expressionRows)

	var evidence []model.UpstreamPriceEvidence
	require.NoError(t, database.Order("id").Find(&evidence).Error)
	require.Len(t, evidence, 2)
	assert.Equal(t, model.UpstreamPriceStatusApplied, evidence[0].Status)
	assert.Equal(t, model.UpstreamPriceStatusUnchanged, evidence[1].Status)
	assert.Empty(t, evidence[0].Error)
	assert.Empty(t, evidence[1].Error)
}

func TestParseVolcenginePricingBuildsSeedreamRequestPrices(t *testing.T) {
	body := []byte(`<html><body>
doubao-seedream-5-0-pro doubao-seedream-5-0
doubao-seedream-4-5 doubao-seedream-4-0
0.30 0.22 0.25 0.20
</body></html>`)
	allowed := map[string]string{
		"doubao-seedream-5-0-pro-260628": "字节跳动",
		"doubao-seedream-5-0-260128":     "字节跳动",
		"doubao-seedream-4-5-251128":     "字节跳动",
		"doubao-seedream-4-0-250828":     "字节跳动",
		"doubao-seedream-4-0-20260415":   "字节跳动",
	}
	prices, err := parseVolcenginePricing(
		"https://docs.volcengine.com/docs/82379/1544106?lang=zh",
		body,
		allowed,
		time.Now(),
	)
	require.NoError(t, err)
	require.Len(t, prices, 5)

	byModel := make(map[string]officialTokenPrice, len(prices))
	for _, price := range prices {
		byModel[price.ModelName] = price
		assert.Equal(t, billingexpr.BillingBasisRequest, price.BillingBasis)
		assert.Equal(t, "count", price.UsageSchema["image_count"].Unit)
		assert.False(t, strings.HasPrefix(officialPriceExpression(price), "v2:"))
	}
	evaluate := func(modelName string, facts map[string]any) (float64, string) {
		t.Helper()
		value, trace, runErr := billingexpr.RunExprWithRequest(
			officialPriceExpression(byModel[modelName]),
			billingexpr.TokenParams{},
			billingexpr.RequestInput{Usage: facts},
		)
		require.NoError(t, runErr)
		return value, trace.MatchedTier
	}

	value, tier := evaluate("doubao-seedream-5-0-pro-260628", map[string]any{
		"image_count":           2.0,
		"resolution":            "1K",
		"reference_image_count": 3.0,
	})
	assert.InDelta(t, (2*0.30+2*0.02)/officialCNYPerUSD, value, 1e-9)
	assert.Equal(t, "up_to_1_5k", tier)

	value, tier = evaluate("doubao-seedream-5-0-pro-260628", map[string]any{
		"image_count":           1.0,
		"resolution":            "4K",
		"reference_image_count": 1.0,
	})
	assert.InDelta(t, 0.60/officialCNYPerUSD, value, 1e-9)
	assert.Equal(t, "over_1_5k", tier)

	for modelName, cnyPerImage := range map[string]float64{
		"doubao-seedream-5-0-260128":   0.22,
		"doubao-seedream-4-5-251128":   0.25,
		"doubao-seedream-4-0-250828":   0.20,
		"doubao-seedream-4-0-20260415": 0.20,
	} {
		value, tier = evaluate(modelName, map[string]any{
			"image_count":           3.0,
			"resolution":            "2K",
			"reference_image_count": 4.0,
		})
		assert.InDelta(t, 3*cnyPerImage/officialCNYPerUSD, value, 1e-9, modelName)
		assert.Equal(t, "per_image", tier, modelName)
	}
}
