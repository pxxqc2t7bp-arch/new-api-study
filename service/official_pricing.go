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
	Vendor         string  `json:"vendor"`
	ModelName      string  `json:"model_name"`
	InputPerM      float64 `json:"input_per_m"`
	CachedReadPerM float64 `json:"cached_read_per_m"`
	CacheWritePerM float64 `json:"cache_write_per_m"`
	OutputPerM     float64 `json:"output_per_m"`
	SourceURL      string  `json:"source_url"`
	EvidenceHash   string  `json:"evidence_hash"`
}

var officialPricingSourceURLs = map[string][]string{
	"openai": {
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
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "table" {
			prices = append(prices, parseOfficialPricingTable(vendor, sourceURL, node, allowedModels, aliases)...)
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

func parseOfficialPricingTable(
	vendor string,
	sourceURL string,
	table *html.Node,
	allowedModels map[string]string,
	aliases map[string]string,
) []officialTokenPrice {
	rows := tableRows(table)
	if len(rows) < 2 {
		return nil
	}
	tableText := strings.ToLower(normalizedNodeText(table))
	if !strings.Contains(tableText, "1m") &&
		!strings.Contains(tableText, "million") &&
		!strings.Contains(tableText, "mtok") {
		return nil
	}
	headerIndex := -1
	inputColumn, cachedColumn, outputColumn := -1, -1, -1
	for index, row := range rows {
		for column, cell := range row {
			value := strings.ToLower(cell)
			switch {
			case strings.Contains(value, "cached") && strings.Contains(value, "input"):
				cachedColumn = column
			case strings.Contains(value, "cache") && strings.Contains(value, "read"):
				cachedColumn = column
			case strings.Contains(value, "input"):
				inputColumn = column
			case strings.Contains(value, "output"):
				outputColumn = column
			}
		}
		if inputColumn >= 0 && outputColumn >= 0 {
			headerIndex = index
			break
		}
	}
	if headerIndex < 0 {
		return nil
	}
	var prices []officialTokenPrice
	for _, row := range rows[headerIndex+1:] {
		maxColumn := max(inputColumn, outputColumn)
		if cachedColumn > maxColumn {
			maxColumn = cachedColumn
		}
		if len(row) <= maxColumn || len(row) == 0 {
			continue
		}
		modelName, matched := resolveOfficialModelName(strings.TrimSpace(row[0]), aliases, allowedModels)
		if !matched || !officialModelMatchesVendor(modelName, vendor, allowedModels) {
			continue
		}
		input, inputOK := parseOfficialPriceCell(row[inputColumn])
		output, outputOK := parseOfficialPriceCell(row[outputColumn])
		if !inputOK || !outputOK {
			continue
		}
		cached := input
		if cachedColumn >= 0 {
			if parsed, ok := parseOfficialPriceCell(row[cachedColumn]); ok {
				cached = parsed
			}
		}
		normalizedRow := strings.Join(row, "\x1f")
		hash := sha256.Sum256([]byte(sourceURL + "\x00" + normalizedRow))
		prices = append(prices, officialTokenPrice{
			Vendor:         vendor,
			ModelName:      modelName,
			InputPerM:      input,
			CachedReadPerM: cached,
			CacheWritePerM: input,
			OutputPerM:     output,
			SourceURL:      sourceURL,
			EvidenceHash:   hex.EncodeToString(hash[:]),
		})
	}
	return prices
}

func resolveOfficialModelName(raw string, aliases map[string]string, allowedModels map[string]string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if alias, ok := aliases[raw]; ok {
		canonical := strings.TrimSpace(alias)
		_, exists := allowedModels[canonical]
		return canonical, exists
	}
	if _, exists := allowedModels[raw]; exists {
		return raw, true
	}
	for modelName := range allowedModels {
		if strings.EqualFold(modelName, raw) {
			return modelName, true
		}
	}
	return "", false
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
	return fmt.Sprintf(
		`tier("official", p*%.10g + cr*%.10g + cc*%.10g + cc1h*%.10g + c*%.10g)`,
		price.InputPerM,
		price.CachedReadPerM,
		price.CacheWritePerM,
		price.CacheWritePerM,
		price.OutputPerM,
	)
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
