package service

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/expr-lang/expr/ast"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AppTaskBillingInputs is constructed by the Host before dispatch. Headers and
// body contain only validated pricing inputs, never the authentication headers.
type AppTaskBillingInputs struct {
	Usage   map[string]any    `json:"usage"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

type AppTaskPriceQuote struct {
	Quota        int                            `json:"quota"`
	MatchedTier  string                         `json:"matched_tier,omitempty"`
	RequestRules []billingexpr.RequestRuleTrace `json:"request_rules,omitempty"`
	Clamp        *common.QuotaClamp             `json:"-"`
}

// CaptureAppTaskBillingInputs stores only literal, non-secret pricing probes.
// Dynamic probe paths and content/authentication fields fail closed.
func CaptureAppTaskBillingInputs(c *gin.Context, info *relaycommon.RelayInfo, usage map[string]any) (AppTaskBillingInputs, error) {
	inputs := AppTaskBillingInputs{Usage: usage, Headers: map[string]string{}, Body: json.RawMessage(`{}`)}
	if inputs.Usage == nil {
		inputs.Usage = map[string]any{}
	}
	var grant model.AppExecutionGrant
	if err := model.DB.WithContext(c.Request.Context()).Where("grant_id = ?", info.AppSubject.GrantID).First(&grant).Error; err != nil {
		return inputs, classifyDBLookup(err, "invalid_grant")
	}
	var candidates []model.AppExecutionModel
	if common.UnmarshalJsonStr(grant.ModelsJSON, &candidates) != nil {
		return inputs, appAuthError("invalid_grant")
	}
	for _, candidate := range candidates {
		if candidate.ChannelID != info.AppSubject.ChannelID || candidate.Group != info.AppSubject.Group ||
			candidate.PublicModel != info.AppSubject.PublicModel || candidate.Protocol != info.AppSubject.Protocol {
			continue
		}
		if candidate.BillingBasis != billingexpr.BillingBasisTask {
			return inputs, nil
		}
		expression, _ := candidate.Price["billing_setting.billing_expr"].(string)
		program, err := billingexpr.CompileFromCache(expression)
		if err != nil {
			return inputs, appAuthError("invalid_price_inputs")
		}
		probes := &appPriceProbes{}
		node := program.Node()
		ast.Walk(&node, probes)
		if probes.invalid {
			return inputs, appAuthError("invalid_price_inputs")
		}
		protocol, ok := c.MustGet(jsplugin.ContextKeyProtocolRequest).(jsplugin.ProtocolRequestContext)
		if !ok {
			return inputs, appAuthError("invalid_price_inputs")
		}
		raw, err := common.Marshal(protocol.Body)
		if err != nil {
			return inputs, appAuthError("invalid_price_inputs")
		}
		body := gjson.GetBytes(raw, "value")
		for _, path := range probes.params {
			for _, segment := range strings.Split(path, ".") {
				if !appPassthroughSegment.MatchString(segment) ||
					!slicesContainsAppPriceField(segment) {
					return inputs, appAuthError("invalid_price_inputs")
				}
			}
			value := body.Get(path)
			if !value.Exists() {
				continue
			}
			if value.IsObject() || value.IsArray() || (value.Type == gjson.String && len(value.Str) > 64) {
				return inputs, appAuthError("invalid_price_inputs")
			}
			inputs.Body, err = sjson.SetRawBytes(inputs.Body, path, []byte(value.Raw))
			if err != nil {
				return inputs, appAuthError("invalid_price_inputs")
			}
		}
		for _, header := range probes.headers {
			name := http.CanonicalHeaderKey(header)
			switch name {
			case "Content-Type", "Anthropic-Beta", "X-Service-Tier":
			default:
				return inputs, appAuthError("invalid_price_inputs")
			}
			if len(c.Request.Header.Values(name)) > 1 || len(c.GetHeader(name)) > 128 {
				return inputs, appAuthError("invalid_price_inputs")
			}
			inputs.Headers[name] = c.GetHeader(name)
		}
		return inputs, nil
	}
	return inputs, appAuthError("invalid_grant")
}

func slicesContainsAppPriceField(field string) bool {
	switch field {
	case "metadata", "seconds", "duration", "resolution", "size", "service_tier", "watermark", "generate_audio", "seed", "ratio":
		return true
	default:
		return false
	}
}

type appPriceProbes struct {
	params, headers []string
	invalid         bool
}

func (p *appPriceProbes) Visit(node *ast.Node) {
	call, ok := (*node).(*ast.CallNode)
	if !ok {
		return
	}
	callee, ok := call.Callee.(*ast.IdentifierNode)
	if !ok || (callee.Value != "param" && callee.Value != "header") {
		return
	}
	if len(call.Arguments) != 1 {
		p.invalid = true
		return
	}
	literal, ok := call.Arguments[0].(*ast.StringNode)
	if !ok || literal.Value == "" {
		p.invalid = true
		return
	}
	if callee.Value == "param" {
		p.params = append(p.params, literal.Value)
	} else {
		p.headers = append(p.headers, literal.Value)
	}
}

// ComputeAppTaskPrice is shared by reservation and completion. It never reads
// live model/group prices, and both calls use the captured pricing instant.
func ComputeAppTaskPrice(snapshot model.AppExecutionModel, inputs AppTaskBillingInputs, pricedAt int64) (AppTaskPriceQuote, error) {
	if snapshot.QuotaPerUnit <= 0 || snapshot.GroupRatio < 0 || pricedAt <= 0 ||
		math.IsNaN(snapshot.QuotaPerUnit) || math.IsInf(snapshot.QuotaPerUnit, 0) ||
		math.IsNaN(snapshot.GroupRatio) || math.IsInf(snapshot.GroupRatio, 0) {
		return AppTaskPriceQuote{}, errors.New("invalid_price_snapshot")
	}
	switch snapshot.BillingBasis {
	case billingexpr.BillingBasisRequest:
		price, ok := snapshot.Price["ModelPrice"].(float64)
		if !ok || price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return AppTaskPriceQuote{}, errors.New("invalid_price_snapshot")
		}
		quota, clamp := common.QuotaFromFloatChecked(price * snapshot.QuotaPerUnit * snapshot.GroupRatio)
		return AppTaskPriceQuote{Quota: quota, Clamp: clamp}, nil
	case billingexpr.BillingBasisTask:
		expression, ok := snapshot.Price["billing_setting.billing_expr"].(string)
		if !ok || expression == "" || snapshot.ExprVersion != billingexpr.ExprVersion(expression) {
			return AppTaskPriceQuote{}, errors.New("invalid_price_snapshot")
		}
		billing := &billingexpr.BillingSnapshot{ExprString: expression, ExprHash: billingexpr.ExprHashString(expression),
			GroupRatio: snapshot.GroupRatio, QuotaPerUnit: snapshot.QuotaPerUnit,
			ExprVersion: snapshot.ExprVersion, BillingBasis: billingexpr.BillingBasisTask, PricingTimeUnix: pricedAt}
		result, err := billingexpr.ComputeTieredQuotaWithRequest(billing, billingexpr.TokenParams{},
			billingexpr.RequestInput{Headers: inputs.Headers, Body: inputs.Body, Usage: inputs.Usage, EvaluatedAtUnix: pricedAt})
		if err != nil {
			return AppTaskPriceQuote{}, err
		}
		return AppTaskPriceQuote{Quota: result.ActualQuotaAfterGroup, Clamp: result.Clamp,
			MatchedTier: result.MatchedTier, RequestRules: result.RequestRules}, nil
	default:
		return AppTaskPriceQuote{}, errors.New("unsupported_price_basis")
	}
}
