package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"golang.org/x/net/html"
	"gorm.io/gorm"
)

const officialPricingMaxBodyBytes = 4 << 20

type OfficialPricingSyncSummary struct {
	Fetched   int `json:"fetched"`
	Parsed    int `json:"parsed"`
	Applied   int `json:"applied"`
	Rejected  int `json:"rejected"`
	Unchanged int `json:"unchanged"`
}

type officialTokenPrice struct {
	Vendor               string  `json:"vendor"`
	ModelName            string  `json:"model_name"`
	InputPerM            float64 `json:"input_per_m"`
	CachedReadPerM       float64 `json:"cached_read_per_m"`
	CacheWritePerM       float64 `json:"cache_write_per_m"`
	CacheWrite1hPerM     float64 `json:"cache_write_1h_per_m"`
	OutputPerM           float64 `json:"output_per_m"`
	LongContextThreshold int64   `json:"long_context_threshold,omitempty"`
	LongInputPerM        float64 `json:"long_input_per_m,omitempty"`
	LongCachedReadPerM   float64 `json:"long_cached_read_per_m,omitempty"`
	LongCacheWritePerM   float64 `json:"long_cache_write_per_m,omitempty"`
	LongCacheWrite1hPerM float64 `json:"long_cache_write_1h_per_m,omitempty"`
	LongOutputPerM       float64 `json:"long_output_per_m,omitempty"`
	SourceURL            string  `json:"source_url"`
	EvidenceHash         string  `json:"evidence_hash"`
}

var officialPricingSourceURLs = map[string][]string{
	"openai": {
		"https://developers.openai.com/api/docs/pricing",
		"https://openai.com/api/pricing/",
	},
	"anthropic": {
		"https://platform.claude.com/docs/en/about-claude/pricing",
	},
	"xai": {
		"https://docs.x.ai/developers/pricing",
		"https://docs.x.ai/developers/models",
		"https://x.ai/api",
	},
}

var officialPriceNumberPattern = regexp.MustCompile(`(?i)(?:USD\s*)?\$?\s*([0-9]+(?:\.[0-9]+)?)`)
var officialLongContextPattern = regexp.MustCompile(`(?i)(?:>=|>)\s*([0-9]+(?:\.[0-9]+)?)\s*k`)

func RunOfficialPricingSync(ctx context.Context, now time.Time) (OfficialPricingSyncSummary, error) {
	var summary OfficialPricingSyncSummary
	for _, group := range []string{"default", "cxy"} {
		if math.Abs(ratio_setting.GetGroupRatio(group)-1) > 1e-9 {
			return summary, fmt.Errorf("official pricing requires group %s ratio to equal 1.0", group)
		}
	}
	allowedModels, err := loadManagedModelVendors()
	if err != nil {
		return summary, err
	}
	aliases := operation_setting.GetUpstreamOrchestrationSetting().ModelAliases
	prices := make([]officialTokenPrice, 0)
	for vendor, sourceURLs := range officialPricingSourceURLs {
		var vendorPrices []officialTokenPrice
		var lastErr error
		for _, sourceURL := range sourceURLs {
			body, fetchErr := fetchOfficialPricingPage(ctx, sourceURL)
			if fetchErr != nil {
				lastErr = fetchErr
				continue
			}
			summary.Fetched++
			parsed, parseErr := parseOfficialPricingTables(vendor, sourceURL, body, allowedModels, aliases)
			if parseErr != nil {
				lastErr = parseErr
				continue
			}
			vendorPrices = parsed
			break
		}
		if len(vendorPrices) == 0 {
			summary.Rejected++
			_ = recordOfficialPricingRejection(vendor, sourceURLs, lastErr, now)
			continue
		}
		prices = append(prices, vendorPrices...)
	}
	summary.Parsed = len(prices)
	if len(prices) == 0 {
		return summary, nil
	}

	modes := billing_setting.GetBillingModeCopy()
	expressions := billing_setting.GetBillingExprCopy()
	evidence := make([]model.UpstreamPriceEvidence, 0, len(prices))
	acceptedPrices := make([]officialTokenPrice, 0, len(prices))
	for _, price := range prices {
		expression := officialPriceExpression(price)
		if err := billing_setting.SmokeTestExpr(expression); err != nil {
			summary.Rejected++
			evidence = append(evidence, newOfficialPriceEvidence(price, expressions[price.ModelName], expression, model.UpstreamPriceStatusRejected, err, now))
			continue
		}
		previous := expressions[price.ModelName]
		if previous == expression && modes[price.ModelName] == billing_setting.BillingModeTieredExpr {
			summary.Unchanged++
			acceptedPrices = append(acceptedPrices, price)
			evidence = append(evidence, newOfficialPriceEvidence(price, previous, expression, model.UpstreamPriceStatusUnchanged, nil, now))
			continue
		}
		modes[price.ModelName] = billing_setting.BillingModeTieredExpr
		expressions[price.ModelName] = expression
		summary.Applied++
		acceptedPrices = append(acceptedPrices, price)
		evidence = append(evidence, newOfficialPriceEvidence(price, previous, expression, model.UpstreamPriceStatusApplied, nil, now))
	}
	modeJSON, err := common.Marshal(modes)
	if err != nil {
		return summary, err
	}
	expressionJSON, err := common.Marshal(expressions)
	if err != nil {
		return summary, err
	}
	if summary.Applied > 0 {
		if err := model.UpdateOptionsBulk(map[string]string{
			"billing_setting.billing_mode": string(modeJSON),
			"billing_setting.billing_expr": string(expressionJSON),
		}); err != nil {
			return summary, err
		}
	}
	for _, price := range acceptedPrices {
		if err := ensureOfficialModelMetadata(price); err != nil {
			return summary, err
		}
	}
	if len(acceptedPrices) > 0 {
		model.RefreshPricing()
	}
	if len(evidence) > 0 {
		if err := model.DB.CreateInBatches(&evidence, 50).Error; err != nil {
			return summary, err
		}
	}
	return summary, nil
}

func ensureOfficialModelMetadata(price officialTokenPrice) error {
	var count int64
	if err := model.DB.Model(&model.Model{}).
		Where("model_name = ?", price.ModelName).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	vendorName := map[string]string{
		"openai":    "OpenAI",
		"anthropic": "Anthropic",
		"xai":       "xAI",
	}[price.Vendor]
	if vendorName == "" {
		return fmt.Errorf("unsupported official pricing vendor %q", price.Vendor)
	}
	var vendor model.Vendor
	err := model.DB.Where("LOWER(name) = ?", strings.ToLower(vendorName)).First(&vendor).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		vendor = model.Vendor{Name: vendorName, Status: 1}
		if err := vendor.Insert(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	endpoints, _ := common.Marshal([]string{
		"/v1/chat/completions",
		"/v1/responses",
		"/v1/messages",
	})
	icon := strings.ToLower(vendorName)
	return (&model.Model{
		ModelName:    price.ModelName,
		Description:  fmt.Sprintf("Official %s model", vendorName),
		Icon:         icon,
		VendorID:     vendor.Id,
		Endpoints:    string(endpoints),
		Status:       1,
		SyncOfficial: 1,
	}).Insert()
}

func fetchOfficialPricingPage(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || !officialPricingHostAllowed(parsed.Hostname()) {
		return nil, errors.New("official pricing URL is not allowlisted")
	}
	baseClient := GetHttpClient()
	if proxyURL := strings.TrimSpace(os.Getenv("UPSTREAM_PRICING_PROXY_URL")); proxyURL != "" {
		baseClient, err = GetHttpClientWithProxy(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid official pricing proxy: %w", err)
		}
	}
	client := &http.Client{
		Timeout: 20 * time.Second,
	}
	if baseClient != nil {
		client.Transport = baseClient.Transport
	}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !officialPricingHostAllowed(request.URL.Hostname()) {
			return errors.New("official pricing redirect is not allowed")
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("User-Agent", "Mozilla/5.0 (compatible; NewAPI-Upstream-Orchestrator/1.0)")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("official pricing fetch returned HTTP %d", response.StatusCode)
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType != "" && mediaType != "text/html" && mediaType != "application/xhtml+xml" {
		return nil, fmt.Errorf("official pricing content type %q is not HTML", mediaType)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, officialPricingMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > officialPricingMaxBodyBytes {
		return nil, errors.New("official pricing page exceeds size limit")
	}
	return data, nil
}

func officialPricingHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "openai.com" ||
		host == "www.openai.com" ||
		host == "developers.openai.com" ||
		host == "platform.claude.com" ||
		host == "docs.x.ai" ||
		host == "x.ai" ||
		host == "www.x.ai"
}

func parseOfficialPricingTables(
	vendor string,
	sourceURL string,
	body []byte,
	allowedModels map[string]string,
	aliases map[string]string,
) ([]officialTokenPrice, error) {
	document, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	var prices []officialTokenPrice
	seenModels := make(map[string]struct{})
	documentHasPerMillionUnit := hasOfficialPerMillionUnit(normalizedNodeText(document))
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "table" {
			for _, price := range parseOfficialPricingTable(
				vendor,
				sourceURL,
				node,
				allowedModels,
				aliases,
				documentHasPerMillionUnit,
			) {
				if _, exists := seenModels[price.ModelName]; exists {
					continue
				}
				seenModels[price.ModelName] = struct{}{}
				prices = append(prices, price)
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	if len(prices) == 0 {
		return nil, errors.New("official pricing page contains no complete supported pricing table")
	}
	return prices, nil
}

type officialPriceColumns struct {
	model          int
	input          []int
	cachedRead     []int
	cacheWrite     []int
	cacheWrite1h   int
	output         []int
	headerEvidence string
}

func parseOfficialPricingTable(
	vendor string,
	sourceURL string,
	table *html.Node,
	allowedModels map[string]string,
	aliases map[string]string,
	documentHasPerMillionUnit bool,
) []officialTokenPrice {
	rows := tableRows(table)
	if len(rows) < 2 {
		return nil
	}
	tableText := strings.ToLower(normalizedNodeText(table))
	if (!hasOfficialPerMillionUnit(tableText) && !documentHasPerMillionUnit) ||
		strings.Contains(tableText, "price per second") ||
		strings.Contains(tableText, "estimated cost") {
		return nil
	}
	headerIndex, columns, ok := officialPricingColumnsForRows(rows)
	if !ok {
		return nil
	}
	var prices []officialTokenPrice
	for _, row := range rows[headerIndex+1:] {
		requiredColumn := max(columns.model, max(columns.input[0], columns.output[0]))
		if len(row) <= requiredColumn {
			continue
		}
		rawModelName := strings.TrimSpace(row[columns.model])
		modelName, matched := resolveOfficialModelName(rawModelName, aliases, allowedModels)
		if !matched || !officialModelMatchesVendor(modelName, vendor, allowedModels) {
			continue
		}
		input, inputOK := parseOfficialPriceCell(row[columns.input[0]])
		output, outputOK := parseOfficialPriceCell(row[columns.output[0]])
		if !inputOK || !outputOK {
			continue
		}
		cached := input
		if len(columns.cachedRead) > 0 && len(row) > columns.cachedRead[0] {
			if parsed, ok := parseOfficialPriceCell(row[columns.cachedRead[0]]); ok {
				cached = parsed
			}
		}
		cacheWrite := input
		if len(columns.cacheWrite) > 0 && len(row) > columns.cacheWrite[0] {
			if parsed, ok := parseOfficialPriceCell(row[columns.cacheWrite[0]]); ok {
				cacheWrite = parsed
			}
		}
		cacheWrite1h := cacheWrite
		if columns.cacheWrite1h >= 0 && len(row) > columns.cacheWrite1h {
			if parsed, ok := parseOfficialPriceCell(row[columns.cacheWrite1h]); ok {
				cacheWrite1h = parsed
			}
		}
		price := officialTokenPrice{
			Vendor:           vendor,
			ModelName:        modelName,
			InputPerM:        input,
			CachedReadPerM:   cached,
			CacheWritePerM:   cacheWrite,
			CacheWrite1hPerM: cacheWrite1h,
			OutputPerM:       output,
			SourceURL:        sourceURL,
		}
		if len(columns.input) > 1 &&
			len(columns.output) > 1 &&
			len(row) > columns.input[1] &&
			len(row) > columns.output[1] {
			longInput, longInputOK := parseOfficialPriceCell(row[columns.input[1]])
			longOutput, longOutputOK := parseOfficialPriceCell(row[columns.output[1]])
			threshold := officialLongContextThresholdFor(vendor, rawModelName)
			if longInputOK && longOutputOK && threshold > 0 {
				price.LongContextThreshold = threshold
				price.LongInputPerM = longInput
				price.LongOutputPerM = longOutput
				price.LongCachedReadPerM = longInput
				if len(columns.cachedRead) > 1 && len(row) > columns.cachedRead[1] {
					if parsed, ok := parseOfficialPriceCell(row[columns.cachedRead[1]]); ok {
						price.LongCachedReadPerM = parsed
					}
				}
				price.LongCacheWritePerM = longInput
				if len(columns.cacheWrite) > 1 && len(row) > columns.cacheWrite[1] {
					if parsed, ok := parseOfficialPriceCell(row[columns.cacheWrite[1]]); ok {
						price.LongCacheWritePerM = parsed
					}
				}
				price.LongCacheWrite1hPerM = price.LongCacheWritePerM
			}
		}
		evidence := strings.Join([]string{
			sourceURL,
			columns.headerEvidence,
			strings.Join(row, "\x1f"),
			strconv.FormatInt(price.LongContextThreshold, 10),
		}, "\x00")
		hash := sha256.Sum256([]byte(evidence))
		price.EvidenceHash = hex.EncodeToString(hash[:])
		prices = append(prices, price)
	}
	return prices
}

func hasOfficialPerMillionUnit(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "1m token") ||
		strings.Contains(value, "million token") ||
		strings.Contains(value, "mtok")
}

func officialPricingColumnsForRows(rows [][]string) (int, officialPriceColumns, bool) {
	for headerIndex, header := range rows {
		maxDataWidth := 0
		for _, row := range rows[headerIndex+1:] {
			if len(row) > maxDataWidth {
				maxDataWidth = len(row)
			}
		}
		offset := maxDataWidth - len(header)
		if offset < 0 {
			offset = 0
		}
		columns := officialPriceColumns{
			model:          -1,
			cacheWrite1h:   -1,
			headerEvidence: strings.Join(header, "\x1f"),
		}
		for rowIndex := headerIndex; rowIndex >= 0; rowIndex-- {
			for column, cell := range rows[rowIndex] {
				if strings.EqualFold(strings.TrimSpace(cell), "model") {
					columns.model = column
					break
				}
			}
			if columns.model >= 0 {
				break
			}
		}
		for column, cell := range header {
			value := strings.ToLower(strings.TrimSpace(cell))
			alignedColumn := column + offset
			switch {
			case strings.Contains(value, "cache") && strings.Contains(value, "write") && strings.Contains(value, "1h"):
				columns.cacheWrite1h = alignedColumn
			case strings.Contains(value, "cache") && strings.Contains(value, "write"):
				columns.cacheWrite = append(columns.cacheWrite, alignedColumn)
			case strings.Contains(value, "cached") && strings.Contains(value, "input"):
				columns.cachedRead = append(columns.cachedRead, alignedColumn)
			case value == "cached":
				columns.cachedRead = append(columns.cachedRead, alignedColumn)
			case strings.Contains(value, "cache") && (strings.Contains(value, "read") || strings.Contains(value, "hit")):
				columns.cachedRead = append(columns.cachedRead, alignedColumn)
			case strings.Contains(value, "input"):
				columns.input = append(columns.input, alignedColumn)
			case strings.Contains(value, "output"):
				columns.output = append(columns.output, alignedColumn)
			}
		}
		if columns.model >= 0 && len(columns.input) > 0 && len(columns.output) > 0 {
			if headerIndex > 0 {
				columns.headerEvidence = strings.Join(rows[headerIndex-1], "\x1f") + "\x1e" + columns.headerEvidence
			}
			return headerIndex, columns, true
		}
	}
	return -1, officialPriceColumns{}, false
}

func officialLongContextThresholdFor(vendor string, rawModelName string) int64 {
	normalized := strings.ReplaceAll(rawModelName, "≥", ">=")
	if match := officialLongContextPattern.FindStringSubmatch(normalized); len(match) == 2 {
		value, err := strconv.ParseFloat(match[1], 64)
		if err == nil && value > 0 {
			return int64(value * 1000)
		}
	}
	if vendor == "openai" {
		return 272000
	}
	return 0
}

func resolveOfficialModelName(raw string, aliases map[string]string, allowedModels map[string]string) (string, bool) {
	raw = strings.TrimSpace(raw)
	normalized := normalizeOfficialModelLabel(raw)
	for _, candidate := range []string{raw, normalized} {
		if alias, ok := aliases[candidate]; ok {
			canonical := strings.TrimSpace(alias)
			_, exists := allowedModels[canonical]
			return canonical, exists
		}
		if _, exists := allowedModels[candidate]; exists {
			return candidate, true
		}
	}
	for modelName := range allowedModels {
		if strings.EqualFold(modelName, normalized) {
			return modelName, true
		}
	}
	return "", false
}

func normalizeOfficialModelLabel(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if index := strings.Index(lower, "long context"); index > 0 {
		value = strings.TrimSpace(value[:index])
	}
	lower = strings.ToLower(value)
	if index := strings.Index(lower, " context length)"); index > 0 {
		if open := strings.LastIndex(value[:index], "("); open > 0 {
			value = strings.TrimSpace(value[:open])
		}
	}
	return value
}

func tableRows(table *html.Node) [][]string {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "tr" {
			var cells []string
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				if child.Type == html.ElementNode && (child.Data == "th" || child.Data == "td") {
					cells = append(cells, normalizedNodeText(child))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(table)
	return rows
}

func normalizedNodeText(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
			builder.WriteByte(' ')
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(builder.String()), " ")
}

func parseOfficialPriceCell(value string) (float64, bool) {
	match := officialPriceNumberPattern.FindStringSubmatch(strings.ReplaceAll(value, ",", ""))
	if len(match) != 2 {
		return 0, false
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

func officialModelMatchesVendor(modelName string, vendor string, allowedModels map[string]string) bool {
	actualVendor, ok := allowedModels[modelName]
	if !ok {
		return false
	}
	switch vendor {
	case "openai":
		return strings.Contains(actualVendor, "openai") || strings.Contains(actualVendor, "chatgpt")
	case "anthropic":
		return strings.Contains(actualVendor, "anthropic") || strings.Contains(actualVendor, "claude")
	case "xai":
		return strings.Contains(actualVendor, "xai") || strings.Contains(actualVendor, "grok")
	default:
		return false
	}
}

func officialPriceExpression(price officialTokenPrice) string {
	standard := fmt.Sprintf(
		`tier("official", p*%.10g + cr*%.10g + cc*%.10g + cc1h*%.10g + c*%.10g)`,
		price.InputPerM,
		price.CachedReadPerM,
		price.CacheWritePerM,
		price.CacheWrite1hPerM,
		price.OutputPerM,
	)
	if price.LongContextThreshold <= 0 ||
		price.LongInputPerM <= 0 ||
		price.LongOutputPerM <= 0 {
		return standard
	}
	longContext := fmt.Sprintf(
		`tier("official_long_context", p*%.10g + cr*%.10g + cc*%.10g + cc1h*%.10g + c*%.10g)`,
		price.LongInputPerM,
		price.LongCachedReadPerM,
		price.LongCacheWritePerM,
		price.LongCacheWrite1hPerM,
		price.LongOutputPerM,
	)
	return fmt.Sprintf("len <= %d ? %s : %s", price.LongContextThreshold, standard, longContext)
}

func newOfficialPriceEvidence(
	price officialTokenPrice,
	previous string,
	current string,
	status string,
	priceErr error,
	now time.Time,
) model.UpstreamPriceEvidence {
	normalized, _ := common.Marshal(price)
	errorMessage := ""
	if priceErr != nil {
		errorMessage = priceErr.Error()
	}
	evidence := model.UpstreamPriceEvidence{
		Vendor:          price.Vendor,
		ModelName:       price.ModelName,
		Currency:        "USD",
		Unit:            "per_1m_tokens",
		NormalizedPrice: string(normalized),
		PreviousPrice:   previous,
		SourceURL:       price.SourceURL,
		EvidenceHash:    price.EvidenceHash,
		Status:          status,
		Error:           errorMessage,
		CapturedAt:      now.Unix(),
	}
	if status == model.UpstreamPriceStatusApplied {
		evidence.AppliedAt = now.Unix()
	}
	_ = current
	return evidence
}

func recordOfficialPricingRejection(vendor string, sourceURLs []string, rejectedErr error, now time.Time) error {
	errorMessage := "no official pricing source succeeded"
	if rejectedErr != nil {
		errorMessage = rejectedErr.Error()
	}
	sourceURL := ""
	if len(sourceURLs) > 0 {
		sourceURL = sourceURLs[0]
	}
	hash := sha256.Sum256([]byte(vendor + "\x00" + sourceURL + "\x00" + errorMessage))
	return model.DB.Create(&model.UpstreamPriceEvidence{
		Vendor:          vendor,
		ModelName:       "*",
		Currency:        "USD",
		Unit:            "per_1m_tokens",
		NormalizedPrice: "{}",
		SourceURL:       sourceURL,
		EvidenceHash:    hex.EncodeToString(hash[:]),
		Status:          model.UpstreamPriceStatusRejected,
		Error:           common.LocalLogPreview(errorMessage),
		CapturedAt:      now.Unix(),
	}).Error
}
