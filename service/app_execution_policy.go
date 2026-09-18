package service

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

type AppModelAdmission struct {
	PublicModel    string `json:"public_model"`
	ActualModel    string `json:"actual_model"`
	PluginKey      string `json:"plugin_key"`
	PluginVersion  string `json:"plugin_version"`
	Protocol       string `json:"protocol"`
	ExecutionKind  string `json:"execution_kind" app_legacy_optional:"true"`
	ChannelTypes   []int  `json:"channel_types" app_legacy_optional:"true"`
	RequestProfile string `json:"request_profile" app_legacy_optional:"true"`
}

const (
	appExecutionKindTaskBacked     = model.AppExecutionModelKindTaskBacked
	appExecutionKindNativeResponse = model.AppExecutionModelKindNativeResponse
	appResponsesTextProfileV1      = "responses.text.v1"
)

func (admission *AppModelAdmission) UnmarshalJSON(raw []byte) error {
	type wireAdmission AppModelAdmission
	var decoded wireAdmission
	var fields map[string]json.RawMessage
	if common.Unmarshal(raw, &decoded) != nil || common.Unmarshal(raw, &fields) != nil {
		return appAuthError("invalid_request")
	}
	_, hasKind := fields["execution_kind"]
	_, hasChannelTypes := fields["channel_types"]
	_, hasRequestProfile := fields["request_profile"]
	if !hasKind {
		if hasChannelTypes || hasRequestProfile || decoded.PluginKey != "doubao" ||
			decoded.PluginVersion != "1.2.0" || !strings.HasPrefix(decoded.ActualModel, "doubao-seedance-") ||
			!slices.Contains([]string{"openai_video", "openai_responses"}, decoded.Protocol) {
			return appAuthError("invalid_request")
		}
		decoded.ExecutionKind = appExecutionKindTaskBacked
		decoded.ChannelTypes = []int{}
		decoded.RequestProfile = ""
	} else if !hasChannelTypes || !hasRequestProfile {
		return appAuthError("invalid_request")
	}
	*admission = AppModelAdmission(decoded)
	return nil
}

type AppPassthroughRule struct {
	PublicModel   string         `json:"public_model"`
	PluginKey     string         `json:"plugin_key"`
	PluginVersion string         `json:"plugin_version"`
	Protocol      string         `json:"protocol"`
	JSONPointer   string         `json:"json_pointer"`
	RuleVersion   int64          `json:"rule_version"`
	Schema        map[string]any `json:"schema"`
}

func (rule AppPassthroughRule) identityHash() (string, error) {
	return appPluginHash([]any{rule.PublicModel, rule.PluginKey,
		rule.Protocol, rule.JSONPointer, rule.RuleVersion})
}

type AppModelOperationPolicy struct {
	Models             []AppModelAdmission  `json:"models"`
	Strategies         []string             `json:"strategies"`
	ConversionPolicies []string             `json:"conversion_policies"`
	PassthroughRules   []AppPassthroughRule `json:"passthrough_rules"`
}

type AppModelInvokePolicy struct {
	Operations map[string]AppModelOperationPolicy `json:"operations"`
}

type AppArkImportDelegation struct {
	InstallationID string    `json:"installation_id"`
	UserID         int       `json:"user_id"`
	AccountRef     string    `json:"account_ref"`
	ProjectID      string    `json:"project_id"`
	StartAt        time.Time `json:"start_at"`
	EndAt          time.Time `json:"end_at"`
}

type AppArkImportDelegations struct {
	Delegations []AppArkImportDelegation `json:"delegations"`
}

// Decode via the host codec, then inspect the validated syntax for duplicate
// (including escaped) keys and exact DTO field names at every depth.
func decodeAppExecutionJSON(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > 64*1024 || common.Unmarshal(raw, target) != nil ||
		!appExecutionJSONShape(gjson.ParseBytes(raw), reflect.TypeOf(target).Elem()) {
		return appAuthError("invalid_request")
	}
	return nil
}

func appExecutionJSONShape(value gjson.Result, typ reflect.Type) bool {
	if typ.Kind() == reflect.Pointer {
		if value.Type == gjson.Null {
			return true
		}
		return appExecutionJSONShape(value, typ.Elem())
	}
	if value.Type == gjson.Null {
		return false
	}
	if typ == reflect.TypeFor[time.Time]() || typ == reflect.TypeFor[json.RawMessage]() {
		return true
	}
	if !value.IsObject() && !value.IsArray() {
		return typ.Kind() != reflect.Struct && typ.Kind() != reflect.Map && typ.Kind() != reflect.Slice
	}
	fields := map[string]reflect.Type{}
	required := map[string]bool{}
	if typ.Kind() == reflect.Struct {
		for i := range typ.NumField() {
			field := typ.Field(i)
			name, rest, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" || name == "" {
				continue
			}
			fields[name] = field.Type
			if rest != "omitempty" && field.Tag.Get("app_legacy_optional") != "true" {
				required[name] = true
			}
		}
	}
	seen := map[string]bool{}
	valid := true
	value.ForEach(func(key, child gjson.Result) bool {
		if value.IsObject() {
			if seen[key.Str] {
				valid = false
				return false
			}
			seen[key.Str] = true
			delete(required, key.Str)
		}
		childType := reflect.TypeFor[any]()
		switch typ.Kind() {
		case reflect.Struct:
			var ok bool
			childType, ok = fields[key.Str]
			if !ok {
				valid = false
				return false
			}
		case reflect.Map, reflect.Slice:
			childType = typ.Elem()
		}
		valid = appExecutionJSONShape(child, childType)
		return valid
	})
	return valid && len(required) == 0
}

func PublishAppModelInvokePolicy(ctx context.Context, db *gorm.DB, identity AuthIdentity, raw []byte, now time.Time) (int64, error) {
	var policy AppModelInvokePolicy
	if err := decodeAppExecutionJSON(raw, &policy); err != nil {
		return 0, err
	}
	for operation, rules := range policy.Operations {
		if !slices.Contains([]string{"model.generate", "model.evaluate", "model.agent"}, operation) ||
			len(rules.Models) > 128 || len(rules.PassthroughRules) > 64 {
			return 0, appAuthError("invalid_policy")
		}
		seen := map[string]bool{}
		for _, admission := range rules.Models {
			hash, _ := appPluginHash(admission)
			if seen[hash] || !appPluginOpaque(admission.PublicModel, 128) ||
				!validAppModelAdmission(operation, admission) {
				return 0, appAuthError("invalid_policy")
			}
			seen[hash] = true
		}
		// No dynamic scoring or implicit conversion path is admitted in B1.6.
		if len(rules.Strategies) != 1 || rules.Strategies[0] != "stable" ||
			len(rules.ConversionPolicies) != 1 || rules.ConversionPolicies[0] != "strict" {
			return 0, appAuthError("invalid_policy")
		}
		ruleKeys := map[string]bool{}
		for _, rule := range rules.PassthroughRules {
			key, err := rule.identityHash()
			if err != nil {
				return 0, err
			}
			if ruleKeys[key] || rule.RuleVersion <= 0 || !validAppPassthroughRule(rule) ||
				!slices.ContainsFunc(rules.Models, func(a AppModelAdmission) bool {
					return a.ExecutionKind == appExecutionKindTaskBacked &&
						a.PublicModel == rule.PublicModel && a.PluginKey == rule.PluginKey &&
						a.PluginVersion == rule.PluginVersion && a.Protocol == rule.Protocol
				}) {
				return 0, appAuthError("invalid_policy")
			}
			ruleKeys[key] = true
		}
	}
	return publishAppExecutionPolicy(ctx, db, identity, model.AppModelInvokePolicyKey, policy, now)
}

func validAppModelAdmission(operation string, admission AppModelAdmission) bool {
	switch admission.ExecutionKind {
	case appExecutionKindTaskBacked:
		return strings.HasPrefix(admission.ActualModel, "doubao-seedance-") &&
			admission.PluginKey == "doubao" && admission.PluginVersion == "1.2.0" &&
			slices.Contains([]string{"openai_video", "openai_responses"}, admission.Protocol) &&
			len(admission.ChannelTypes) == 0 && admission.RequestProfile == ""
	case appExecutionKindNativeResponse:
		return operation != "model.agent" && appPluginOpaque(admission.ActualModel, 128) &&
			admission.PluginKey == "" && admission.PluginVersion == "" &&
			admission.Protocol == "openai_responses" &&
			slices.Equal(admission.ChannelTypes, []int{constant.ChannelTypeOpenAI}) &&
			admission.RequestProfile == appResponsesTextProfileV1
	default:
		return false
	}
}

var appPassthroughSegment = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Only canonical primitive field pointers are supported initially. Every
// segment is checked, so unknown vendor options are possible without admitting
// transport or identity containers, encoded names, or opaque nested objects.
func validAppPassthroughRule(rule AppPassthroughRule) bool {
	if !strings.HasPrefix(rule.JSONPointer, "/") || strings.ContainsAny(rule.JSONPointer, "%~\\.") {
		return false
	}
	parts := strings.Split(rule.JSONPointer[1:], "/")
	if len(parts) > 2 || (len(parts) == 2 && parts[0] != "metadata") {
		return false
	}
	for _, part := range parts {
		if !appPassthroughSegment.MatchString(part) {
			return false
		}
		compact := strings.ReplaceAll(part, "_", "")
		for _, forbidden := range []string{"auth", "authorization", "authentication", "credential", "credentials",
			"token", "key", "secret", "password", "cookie", "headers", "header", "host", "url", "uri",
			"scheme", "port", "query", "dns",
			"endpoint", "proxy", "transport", "routing", "route", "channel", "model", "plugin", "protocol",
			"payer", "funding", "quota", "price", "billing", "user", "subject", "session", "installation",
			"app", "grant", "run", "execution", "request", "callback", "webhook", "method", "path",
			"body", "input", "prompt", "messages", "content", "tools", "stream", "timeout", "retry"} {
			if strings.Contains(compact, forbidden) {
				return false
			}
		}
	}
	kind, ok := rule.Schema["type"].(string)
	if !ok || !slices.Contains([]string{"boolean", "string", "number", "integer"}, kind) {
		return false
	}
	for key := range rule.Schema {
		if !slices.Contains([]string{"type", "enum", "minimum", "maximum", "maxLength"}, key) {
			return false
		}
	}
	// Bound numbers and strings now; outbound validation remains mandatory.
	if kind == "number" || kind == "integer" {
		minimum, minOK := rule.Schema["minimum"].(float64)
		maximum, maxOK := rule.Schema["maximum"].(float64)
		if !minOK || !maxOK || minimum < 0 || maximum < minimum || maximum > 2147483647 {
			return false
		}
		if parts[len(parts)-1] == "duration" && maximum > 3600 {
			return false
		}
	}
	if kind == "string" {
		values, ok := rule.Schema["enum"].([]any)
		if !ok || len(values) == 0 || len(values) > 32 {
			return false
		}
		for _, value := range values {
			text, ok := value.(string)
			if !ok || !appPluginOpaque(text, 64) {
				return false
			}
		}
	}
	return true
}

func PublishAppArkImportDelegations(ctx context.Context, db *gorm.DB, identity AuthIdentity, raw []byte, now time.Time) (int64, error) {
	var policy AppArkImportDelegations
	if err := decodeAppExecutionJSON(raw, &policy); err != nil {
		return 0, err
	}
	if len(policy.Delegations) > 256 {
		return 0, appAuthError("invalid_policy")
	}
	for _, d := range policy.Delegations {
		if !appPluginOpaque(d.InstallationID, 64) || d.UserID <= 0 || !appPluginOpaque(d.AccountRef, 128) ||
			!appPluginOpaque(d.ProjectID, 128) || d.StartAt.IsZero() || !d.EndAt.After(d.StartAt) {
			return 0, appAuthError("invalid_policy")
		}
	}
	return publishAppExecutionPolicy(ctx, db, identity, model.AppArkImportDelegationsKey, policy, now)
}

func publishAppExecutionPolicy(ctx context.Context, db *gorm.DB, identity AuthIdentity, key string, policy any, now time.Time) (int64, error) {
	raw, err := common.Marshal(policy)
	if err != nil {
		return 0, err
	}
	var version int64
	err = model.RunAppPluginTransaction(db.WithContext(ctx), func(tx *gorm.DB) error {
		user, _, err := AppPluginDashboardIdentity(tx, identity, now)
		if err != nil {
			return err
		}
		if user.Role != common.RoleRootUser {
			return appAuthError("forbidden")
		}
		version, err = model.PublishAppExecutionPolicyTx(tx, key, string(raw), user.Id, now)
		if err != nil {
			return err
		}
		if invoke, ok := policy.(AppModelInvokePolicy); ok {
			for _, operation := range invoke.Operations {
				for _, rule := range operation.PassthroughRules {
					identity, err := rule.identityHash()
					if err != nil {
						return err
					}
					schema, err := appPluginHash(rule.Schema)
					if err != nil {
						return err
					}
					if err := model.PublishAppPassthroughRuleTx(tx, identity, schema); err != nil {
						return err
					}
				}
			}
		}
		return err
	})
	return version, err
}
