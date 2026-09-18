package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AppExecutionService struct {
	db                   *gorm.DB
	options              AppPluginAuthOptions
	ArkSource            AppArkTaskSource
	ResponseRoundTripper http.RoundTripper
	ResponseTimeout      time.Duration
}

func NewAppExecutionService(db *gorm.DB, options AppPluginAuthOptions) *AppExecutionService {
	if options.Now == nil {
		options.Now = time.Now
	}
	options.DerivationKey = slices.Clone(options.DerivationKey)
	return &AppExecutionService{db: db, options: options}
}

type AppPassthroughSelection struct {
	JSONPointer string `json:"json_pointer"`
	RuleVersion int64  `json:"rule_version"`
}

type AppEffectivePassthrough struct {
	JSONPointer  string `json:"json_pointer"`
	RuleVersion  int64  `json:"rule_version"`
	SchemaSHA256 string `json:"schema_sha256"`
}

type AppEffectiveAssetInput struct {
	AssetURISHA256 string    `json:"asset_uri_sha256"`
	JSONPointer    string    `json:"json_pointer"`
	MediaType      string    `json:"media_type"`
	PublicModel    string    `json:"public_model"`
	Protocol       string    `json:"protocol"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type AppExecutionGrantRequest struct {
	RequestID             string                          `json:"request_id"`
	AppKey                string                          `json:"app_key"`
	AppSessionID          string                          `json:"app_session_id"`
	Subject               string                          `json:"subject"`
	RunID                 string                          `json:"run_id"`
	ExecutionRequestID    string                          `json:"execution_request_id"`
	Operation             string                          `json:"operation"`
	ModelPolicyVersion    int64                           `json:"model_policy_version"`
	RequestedModels       []string                        `json:"requested_models"`
	Strategy              string                          `json:"strategy"`
	ConversionPolicy      string                          `json:"conversion_policy"`
	PassthroughSelections []AppPassthroughSelection       `json:"passthrough_selections"`
	RegisteredAssetInputs []model.AppRegisteredAssetInput `json:"registered_asset_inputs,omitempty"`
}

type AppExecutionGrantResult struct {
	GrantID              string                    `json:"grant_id"`
	GrantToken           string                    `json:"grant_token"`
	ExpiresAt            time.Time                 `json:"expires_at"`
	EffectiveModelPool   []string                  `json:"effective_model_pool"`
	Strategy             string                    `json:"strategy"`
	ConversionPolicy     string                    `json:"conversion_policy"`
	EffectivePassthrough []AppEffectivePassthrough `json:"effective_passthrough"`
	EffectiveAssetInputs []AppEffectiveAssetInput  `json:"effective_asset_inputs"`
	AssetSnapshotSHA256  string                    `json:"asset_snapshot_sha256"`
	PayerRef             string                    `json:"payer_ref"`
	FundingSource        string                    `json:"funding_source"`
	PriceVersion         string                    `json:"price_version"`
}

func appExecutionGrantSnapshotHash(grant model.AppExecutionGrant,
	registeredAssets []model.AppRegisteredAssetInput, assetSnapshotHash string) (string, error) {
	var policy AppModelInvokePolicy
	var models []model.AppExecutionModel
	var rules []AppPassthroughRule
	var funding model.AppExecutionFunding
	var result AppExecutionGrantResult
	for _, frozen := range []struct {
		raw    string
		target any
	}{
		{grant.PolicyJSON, &policy},
		{grant.ModelsJSON, &models},
		{grant.PassthroughJSON, &rules},
		{grant.FundingSnapshotJSON, &funding},
		{grant.ResponseJSON, &result},
	} {
		if decodeAppExecutionJSON([]byte(frozen.raw), frozen.target) != nil {
			return "", appAuthError("invalid_grant")
		}
	}
	if result.GrantToken != "" {
		return "", appAuthError("invalid_grant")
	}
	return appPluginHash([]any{
		"new-api/app-plugin/execution-grant-snapshot/v1",
		grant.PolicyVersion, grant.PolicyDigest, grant.PolicyJSON,
		grant.ModelsJSON, grant.PassthroughJSON, grant.FundingSnapshotJSON, grant.ResponseJSON,
		grant.PriceVersion, grant.FundingSource, grant.PayerRef, grant.FundingRef,
		registeredAssets, assetSnapshotHash,
	})
}

func DecodeAppExecutionGrantRequest(raw []byte, request *AppExecutionGrantRequest) error {
	if err := decodeAppExecutionJSON(raw, request); err != nil {
		return err
	}
	normalizeAppRegisteredAssetInputs(request)
	return nil
}

var (
	appRegisteredAssetURI    = regexp.MustCompile(`^asset://[A-Za-z0-9._~-]{1,128}$`)
	appRegisteredAssetDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	appResponsesAssetPointer = regexp.MustCompile(`^/input/(0|[1-9][0-9]*)/content/(0|[1-9][0-9]*)/image_url$`)
	appVideoAssetPointer     = regexp.MustCompile(`^/content/(0|[1-9][0-9]*)/(image|video|audio)_url/url$`)
)

func normalizeAppRegisteredAssetInputs(request *AppExecutionGrantRequest) {
	if request.RegisteredAssetInputs == nil {
		request.RegisteredAssetInputs = []model.AppRegisteredAssetInput{}
	}
	for i := range request.RegisteredAssetInputs {
		request.RegisteredAssetInputs[i].ExpiresAt = request.RegisteredAssetInputs[i].ExpiresAt.UTC()
	}
	slices.SortFunc(request.RegisteredAssetInputs, func(left, right model.AppRegisteredAssetInput) int {
		leftKey := strings.Join([]string{left.PublicModel, left.Protocol, left.JSONPointer, left.MediaType,
			left.AssetURI, left.AuthorizationSHA256, left.OwnerSubject, left.ProjectID, left.Purpose,
			left.RoutingAccount, left.Status, left.ExpiresAt.Format(time.RFC3339Nano)}, "\x00")
		rightKey := strings.Join([]string{right.PublicModel, right.Protocol, right.JSONPointer, right.MediaType,
			right.AssetURI, right.AuthorizationSHA256, right.OwnerSubject, right.ProjectID, right.Purpose,
			right.RoutingAccount, right.Status, right.ExpiresAt.Format(time.RFC3339Nano)}, "\x00")
		return strings.Compare(leftKey, rightKey)
	})
}

func validateAppRegisteredAssetInputs(inputs []model.AppRegisteredAssetInput, subject string,
	candidates []model.AppExecutionModel, now time.Time) ([]AppEffectiveAssetInput, string, int64, error) {
	effective := make([]AppEffectiveAssetInput, 0, len(inputs))
	seenPointers := make(map[[3]string]struct{}, len(inputs))
	seenRegistrations := make(map[string]struct{}, len(inputs))
	var earliestExpiry int64
	for _, input := range inputs {
		if !appRegisteredAssetDigest.MatchString(input.AuthorizationSHA256) ||
			!appRegisteredAssetURI.MatchString(input.AssetURI) ||
			!appControlOpaque(input.JSONPointer, 256) ||
			!appControlOpaque(input.PublicModel, 128) ||
			!appControlOpaque(input.OwnerSubject, 64) ||
			!appControlOpaque(input.ProjectID, 128) ||
			input.OwnerSubject != subject || input.Purpose != "model.invoke" ||
			input.RoutingAccount != "cxy" || input.Status != "active" ||
			!slices.Contains([]string{"image", "video", "audio"}, input.MediaType) ||
			input.ExpiresAt.IsZero() || input.ExpiresAt.Nanosecond() != 0 ||
			input.ExpiresAt.Unix() <= now.Unix() {
			return nil, "", 0, appAuthError("invalid_request")
		}
		if !slices.ContainsFunc(candidates, func(candidate model.AppExecutionModel) bool {
			return candidate.IsTaskBacked() &&
				candidate.PublicModel == input.PublicModel && candidate.Protocol == input.Protocol
		}) {
			return nil, "", 0, appAuthError("invalid_request")
		}
		pointerValid := false
		switch input.Protocol {
		case "openai_responses":
			pointerValid = input.MediaType == "image" && appResponsesAssetPointer.MatchString(input.JSONPointer)
		case "openai_video":
			matches := appVideoAssetPointer.FindStringSubmatch(input.JSONPointer)
			pointerValid = len(matches) == 3 && matches[2] == input.MediaType
		}
		if !pointerValid {
			return nil, "", 0, appAuthError("invalid_request")
		}
		pointerKey := [3]string{input.PublicModel, input.Protocol, input.JSONPointer}
		if _, duplicate := seenPointers[pointerKey]; duplicate {
			return nil, "", 0, appAuthError("invalid_request")
		}
		seenPointers[pointerKey] = struct{}{}
		logicalKey := strings.Join([]string{input.PublicModel, input.Protocol, input.JSONPointer,
			input.MediaType, input.AssetURI}, "\x00")
		if _, duplicate := seenRegistrations[logicalKey]; duplicate {
			return nil, "", 0, appAuthError("invalid_request")
		}
		seenRegistrations[logicalKey] = struct{}{}
		expiry := input.ExpiresAt.Unix()
		if earliestExpiry == 0 || expiry < earliestExpiry {
			earliestExpiry = expiry
		}
		effective = append(effective, AppEffectiveAssetInput{
			AssetURISHA256: serviceDigestBytes([]byte(input.AssetURI)),
			JSONPointer:    input.JSONPointer,
			MediaType:      input.MediaType,
			PublicModel:    input.PublicModel,
			Protocol:       input.Protocol,
			ExpiresAt:      input.ExpiresAt,
		})
	}
	digest, err := appPluginHash(inputs)
	if err != nil {
		return nil, "", 0, err
	}
	return effective, digest, earliestExpiry, nil
}

func appExecutionModelsWithoutNativeAssetBindings(candidates []model.AppExecutionModel,
	inputs []model.AppRegisteredAssetInput) []model.AppExecutionModel {
	if len(inputs) == 0 {
		return candidates
	}
	filtered := make([]model.AppExecutionModel, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ExecutionKind == model.AppExecutionModelKindNativeResponse &&
			slices.ContainsFunc(inputs, func(input model.AppRegisteredAssetInput) bool {
				return input.PublicModel == candidate.PublicModel && input.Protocol == candidate.Protocol
			}) {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

func (s *AppExecutionService) IssueExecutionGrant(ctx context.Context, identity model.AppServiceIdentity, request AppExecutionGrantRequest) (AppExecutionGrantResult, error) {
	normalizeAppRegisteredAssetInputs(&request)
	for _, value := range []string{request.RequestID, request.RunID, request.ExecutionRequestID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return AppExecutionGrantResult{}, appAuthError("invalid_request")
		}
	}
	if request.AppKey != identity.AppKey || !appPluginOpaque(request.AppSessionID, 64) ||
		!appPluginOpaque(request.Subject, 64) || request.ModelPolicyVersion <= 0 ||
		!slices.Contains([]string{"model.generate", "model.evaluate", "model.agent"}, request.Operation) ||
		len(request.RequestedModels) == 0 || len(request.RequestedModels) > 128 ||
		len(request.PassthroughSelections) > 64 || len(request.RegisteredAssetInputs) > 64 {
		return AppExecutionGrantResult{}, appAuthError("invalid_request")
	}
	for _, name := range request.RequestedModels {
		if !appPluginOpaque(name, 128) {
			return AppExecutionGrantResult{}, appAuthError("invalid_request")
		}
	}
	if len(s.options.DerivationKey) < 32 || s.options.Issuer == "" {
		return AppExecutionGrantResult{}, appAuthError("identity_not_configured")
	}
	scope, err := appPluginHash([]string{"execution-grant-request/v1", identity.InstallationID, request.Subject, request.RequestID})
	if err != nil {
		return AppExecutionGrantResult{}, err
	}
	requestHash, err := appPluginHash(request)
	if err != nil {
		return AppExecutionGrantResult{}, err
	}
	var result AppExecutionGrantResult
	err = model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		result = AppExecutionGrantResult{}
		now := s.options.Now().UTC()
		installation, err := model.LockAppPluginInstallation(tx, identity.AppKey, identity.InstallationID)
		if err != nil {
			return err
		}
		currentService, err := model.ValidateAppServiceIdentity(tx, identity, now)
		if err != nil {
			return err
		}
		if installation.Status != model.AppInstallationStatusEnabled {
			return appAuthError("identity_inactive")
		}
		if !slices.Contains(currentService.Scopes, "model.invoke") {
			return appAuthError("scope_denied")
		}
		var session model.AppPluginSession
		if err := model.AppPluginCurrentRead(tx).Where("app_session_id = ? AND installation_id = ? AND app_key = ? AND subject = ?",
			request.AppSessionID, installation.InstallationID, request.AppKey, request.Subject).First(&session).Error; err != nil {
			return appAuthError("not_found")
		}
		user, dashboard, err := AppPluginDashboardIdentity(tx, AuthIdentity{UserID: session.UserID,
			SessionID: session.DashboardSessionID, UserAuthVersion: session.AuthVersion, SessionVersion: session.SessionVersion}, now)
		if err != nil {
			return err
		}
		if session.RevokedAt != 0 || session.UpstreamExpiresAt <= now.Unix() ||
			session.Generation != installation.AppVersionID || session.Issuer != s.options.Issuer {
			return appAuthError("unauthenticated")
		}
		if len(installation.AllowedUserPolicy.Groups) != 0 && !slices.Contains(installation.AllowedUserPolicy.Groups, user.Group) {
			return appAuthError("scope_denied")
		}
		manifest, _, err := ValidateAppPluginRegistration(tx, installation)
		if err != nil {
			return err
		}
		if !slices.Contains(session.GrantedScopes, "model.invoke") || !slices.Contains(manifest.RequestedScopes, "model.invoke") {
			return appAuthError("scope_denied")
		}
		// Replays require live authority, but never reprice, refinance, or renew.
		var stored model.AppExecutionGrant
		q := model.AppPluginCurrentRead(tx).Where("scope_hash = ?", scope).Limit(1).Find(&stored)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 0 {
			if stored.RequestHash != requestHash {
				return appAuthError("idempotency_conflict")
			}
			token := s.executionGrantToken(stored)
			if !hmac.Equal([]byte(serviceDigestBytes([]byte(token))), []byte(stored.TokenHash)) {
				return appAuthError("identity_not_configured")
			}
			var binding model.AppExecutionGrantBinding
			if common.UnmarshalJsonStr(stored.BindingJSON, &binding) != nil ||
				binding.GrantID != stored.GrantID || binding.UserID != user.Id {
				return appAuthError("invalid_grant")
			}
			snapshotHash, snapshotErr := appExecutionGrantSnapshotHash(
				stored, binding.RegisteredAssetInputs, binding.AssetSnapshotSHA256)
			if snapshotErr != nil || !hmac.Equal([]byte(snapshotHash), []byte(binding.SnapshotSHA256)) {
				return appAuthError("invalid_grant")
			}
			_, err := model.ValidateAppServiceIdentity(tx, model.AppServiceIdentity{AppKey: stored.AppKey,
				InstallationID: stored.InstallationID, CredentialID: binding.IssuingCredentialID,
				Version: binding.IssuingCredentialVersion}, now)
			if err != nil {
				return err
			}
			if common.UnmarshalJsonStr(stored.ResponseJSON, &result) != nil || result.GrantToken != "" {
				return appAuthError("invalid_grant")
			}
			result.GrantToken = token
			return nil
		}
		published, err := model.GetAppExecutionPolicyTx(tx, model.AppModelInvokePolicyKey)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return appAuthError("scope_denied")
			}
			return err
		}
		if published.Version != request.ModelPolicyVersion {
			return appAuthError("model_policy_version_conflict")
		}
		var policy AppModelInvokePolicy
		if err := decodeAppExecutionJSON([]byte(published.CanonicalJSON), &policy); err != nil {
			return err
		}
		operation, ok := policy.Operations[request.Operation]
		if !ok || !slices.Contains(operation.Strategies, request.Strategy) ||
			!slices.Contains(operation.ConversionPolicies, request.ConversionPolicy) {
			return appAuthError("scope_denied")
		}
		models, err := effectiveAppExecutionModels(
			tx, user, request.Operation, operation, request.RequestedModels, s.options.DerivationKey)
		if err != nil {
			return err
		}
		if len(models) == 0 {
			return appAuthError("model_not_supported")
		}
		effectiveAssets, assetSnapshotHash, assetExpiry, err := validateAppRegisteredAssetInputs(
			request.RegisteredAssetInputs, session.Subject, models, now)
		if err != nil {
			return err
		}
		models = appExecutionModelsWithoutNativeAssetBindings(models, request.RegisteredAssetInputs)
		passthrough := []AppEffectivePassthrough{}
		selectedRules := []AppPassthroughRule{}
		seen := map[string]bool{}
		for _, selection := range request.PassthroughSelections {
			if selection.RuleVersion <= 0 || seen[selection.JSONPointer] {
				return appAuthError("invalid_request")
			}
			seen[selection.JSONPointer] = true
			effective := AppEffectivePassthrough{JSONPointer: selection.JSONPointer, RuleVersion: selection.RuleVersion}
			for _, candidate := range models {
				index := slices.IndexFunc(operation.PassthroughRules, func(rule AppPassthroughRule) bool {
					return rule.PublicModel == candidate.PublicModel && rule.PluginKey == candidate.PluginKey &&
						rule.PluginVersion == candidate.PluginVersion && rule.Protocol == candidate.Protocol &&
						rule.JSONPointer == selection.JSONPointer && rule.RuleVersion == selection.RuleVersion
				})
				if index < 0 || !validAppPassthroughRule(operation.PassthroughRules[index]) {
					return appAuthError("scope_denied")
				}
				rule := operation.PassthroughRules[index]
				digest, err := appPluginHash(rule.Schema)
				if err != nil {
					return err
				}
				if effective.SchemaSHA256 != "" && effective.SchemaSHA256 != digest {
					return appAuthError("scope_denied")
				}
				effective.SchemaSHA256 = digest
				selectedRules = append(selectedRules, rule)
			}
			passthrough = append(passthrough, effective)
		}
		funding, err := appExecutionWalletEligibility(tx, user, now)
		if err != nil {
			return err
		}
		priceVersion, err := appPluginHash(models)
		if err != nil {
			return err
		}
		grantUUID, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		grantID := grantUUID.String()
		salt, err := model.NewAppPluginOpaqueID()
		if err != nil {
			return err
		}
		pool := []string{}
		for _, candidate := range models {
			if !slices.Contains(pool, candidate.PublicModel) {
				pool = append(pool, candidate.PublicModel)
			}
		}
		expiry := min(now.Add(15*time.Minute).Unix(), session.UpstreamExpiresAt, dashboard.ExpiresAt)
		if assetExpiry != 0 {
			expiry = min(expiry, assetExpiry)
		}
		result = AppExecutionGrantResult{GrantID: grantID, ExpiresAt: time.Unix(expiry, 0).UTC(),
			EffectiveModelPool: pool, Strategy: request.Strategy, ConversionPolicy: request.ConversionPolicy,
			EffectivePassthrough: passthrough, EffectiveAssetInputs: effectiveAssets,
			AssetSnapshotSHA256: assetSnapshotHash, PayerRef: fmt.Sprintf("user:%d", user.Id),
			FundingSource: funding.Source, PriceVersion: priceVersion}
		stored = model.AppExecutionGrant{GrantID: grantID, ScopeHash: scope, RequestHash: requestHash,
			AppKey: installation.AppKey, InstallationID: installation.InstallationID, AppSessionID: session.AppSessionID,
			Subject: session.Subject, UserID: user.Id, RunID: request.RunID, ExecutionRequestID: request.ExecutionRequestID,
			Operation: request.Operation, PolicyVersion: published.Version, PolicyDigest: published.DocumentSHA256,
			PolicyJSON: published.CanonicalJSON, PriceVersion: priceVersion, FundingSource: funding.Source,
			PayerRef: result.PayerRef, FundingRef: funding.Reference, TokenSalt: salt, CreatedAt: now.Unix(), ExpiresAt: expiry}
		for _, field := range []struct {
			target *string
			value  any
		}{{&stored.ModelsJSON, models}, {&stored.PassthroughJSON, selectedRules},
			{&stored.FundingSnapshotJSON, funding}, {&stored.ResponseJSON, result}} {
			raw, err := common.Marshal(field.value)
			if err != nil {
				return err
			}
			*field.target = string(raw)
		}
		snapshotHash, err := appExecutionGrantSnapshotHash(stored, request.RegisteredAssetInputs, assetSnapshotHash)
		if err != nil {
			return err
		}
		binding := model.AppExecutionGrantBinding{GrantID: grantID, ScopeHash: scope, RequestHash: requestHash,
			Issuer: session.Issuer, AppKey: installation.AppKey, InstallationID: installation.InstallationID,
			Generation: session.Generation, AppSessionID: session.AppSessionID, Subject: session.Subject, UserID: user.Id,
			IssuingCredentialID: currentService.CredentialID, IssuingCredentialVersion: currentService.Version,
			RunID: request.RunID, ExecutionRequestID: request.ExecutionRequestID, Operation: request.Operation,
			RegisteredAssetInputs: request.RegisteredAssetInputs, AssetSnapshotSHA256: assetSnapshotHash,
			SnapshotSHA256: snapshotHash, ExpiresAt: expiry}
		rawBinding, err := common.Marshal(binding)
		if err != nil {
			return err
		}
		stored.BindingJSON = string(rawBinding)
		result.GrantToken = s.executionGrantToken(stored)
		stored.TokenHash = serviceDigestBytes([]byte(result.GrantToken))
		return tx.Create(&stored).Error
	})
	if err != nil {
		return AppExecutionGrantResult{}, err
	}
	return result, nil
}

func (s *AppExecutionService) executionGrantToken(grant model.AppExecutionGrant) string {
	key := hmac.New(sha256.New, s.options.DerivationKey)
	key.Write([]byte("new-api/app-plugin/execution-grant-key/v1"))
	mac := hmac.New(sha256.New, key.Sum(nil))
	mac.Write([]byte(grant.TokenSalt))
	mac.Write([]byte(grant.BindingJSON))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func appExecutionWalletEligibility(tx *gorm.DB, user model.User, now time.Time) (model.AppExecutionFunding, error) {
	var settings dto.UserSetting
	if user.Setting != "" && common.UnmarshalJsonStr(user.Setting, &settings) != nil {
		return model.AppExecutionFunding{}, appAuthError("funding_not_supported")
	}
	preference := common.NormalizeBillingPreference(settings.BillingPreference)
	var subscriptions []model.UserSubscription
	if err := model.AppPluginCurrentRead(tx).Where("user_id = ? AND status = ? AND end_time > ?",
		user.Id, "active", now.Unix()).Order("id").Find(&subscriptions).Error; err != nil {
		return model.AppExecutionFunding{}, err
	}
	if preference == "subscription_only" || (preference == "subscription_first" && len(subscriptions) > 0) {
		return model.AppExecutionFunding{}, appAuthError("funding_not_supported")
	}
	// Subscription-first is unsupported above, including overflow fallback.
	// An explicit wallet choice does not consume subscription overflow.
	if common.RedisEnabled || common.BatchUpdateEnabled {
		return model.AppExecutionFunding{}, appAuthError("wallet_accounting_unavailable")
	}
	if user.Quota < 0 || int64(user.Quota) > common.MaxWalletQuota ||
		(preference == "wallet_first" && user.Quota == 0 && len(subscriptions) > 0) {
		return model.AppExecutionFunding{}, appAuthError("funding_not_supported")
	}
	return model.AppExecutionFunding{Source: "wallet", Reference: fmt.Sprintf("wallet:user:%d", user.Id)}, nil
}

type appExecutionGroup struct {
	name  string
	ratio float64
}

func effectiveAppExecutionGroups(tx *gorm.DB, user model.User) ([]appExecutionGroup, error) {
	var rows []model.Option
	keys := []string{"UserUsableGroups", "AutoGroups", "GroupRatio", "GroupGroupRatio", "group_ratio_setting.group_ratio",
		"group_ratio_setting.group_special_usable_group", "group_ratio_setting.group_group_ratio"}
	if err := model.AppPluginCurrentRead(tx).Where(map[string]any{"key": keys}).Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).Find(&rows).Error; err != nil {
		return nil, err
	}
	usable := map[string]string{}
	var ratios map[string]float64
	special := map[string]map[string]string{}
	var groupRatios map[string]map[string]float64
	auto := []string{user.Group}
	for _, row := range rows {
		var target any
		var decodedRatios map[string]float64
		var decodedGroupRatios map[string]map[string]float64
		switch row.Key {
		case "UserUsableGroups":
			target = &usable
		case "AutoGroups":
			target = &auto
		case "GroupRatio", "group_ratio_setting.group_ratio":
			target = &decodedRatios
		case "group_ratio_setting.group_special_usable_group":
			target = &special
		case "GroupGroupRatio", "group_ratio_setting.group_group_ratio":
			target = &decodedGroupRatios
		}
		if common.UnmarshalJsonStr(row.Value, target) != nil || strings.TrimSpace(row.Value) == "null" {
			return nil, appAuthError("invalid_group_policy")
		}
		// Aliases represent one policy, not ordered patches or merged maps.
		switch row.Key {
		case "GroupRatio", "group_ratio_setting.group_ratio":
			if ratios != nil && !reflect.DeepEqual(ratios, decodedRatios) {
				return nil, appAuthError("invalid_group_policy")
			}
			ratios = decodedRatios
		case "GroupGroupRatio", "group_ratio_setting.group_group_ratio":
			if groupRatios != nil && !reflect.DeepEqual(groupRatios, decodedGroupRatios) {
				return nil, appAuthError("invalid_group_policy")
			}
			groupRatios = decodedGroupRatios
		}
	}
	for key, description := range special[user.Group] {
		if group, remove := strings.CutPrefix(key, "-:"); remove {
			delete(usable, group)
		} else {
			group, _ := strings.CutPrefix(key, "+:")
			usable[group] = description
		}
	}
	usable[user.Group] = user.Group
	groups := []appExecutionGroup{}
	seen := map[string]bool{}
	for _, name := range auto {
		_, allowed := usable[name]
		ratio, configured := ratios[name]
		if seen[name] || name == "" || name == "auto" || !allowed || !configured {
			continue
		}
		if specialRatio, ok := groupRatios[user.Group][name]; ok {
			ratio = specialRatio
		}
		if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 {
			return nil, appAuthError("invalid_group_policy")
		}
		seen[name] = true
		groups = append(groups, appExecutionGroup{name: name, ratio: ratio})
	}
	return groups, nil
}

func effectiveAppExecutionModels(tx *gorm.DB, user model.User, operation string, policy AppModelOperationPolicy,
	requested []string, derivationKey []byte) ([]model.AppExecutionModel, error) {
	groups, err := effectiveAppExecutionGroups(tx, user)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, admission := range policy.Models {
		names = append(names, admission.PublicModel, admission.ActualModel)
	}
	prices, err := model.GetEffectiveModelPricingTx(tx, names)
	if err != nil {
		return nil, err
	}
	generation := jsplugin.DefaultRegistry.Generation()
	candidates := []model.AppExecutionModel{}
	for _, admission := range policy.Models {
		if !slices.Contains(requested, admission.PublicModel) ||
			!validAppModelAdmission(operation, admission) {
			continue
		}
		if admission.ExecutionKind == appExecutionKindNativeResponse {
			native, err := effectiveNativeAppExecutionModels(tx, admission, groups, prices[admission.PublicModel], derivationKey)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, native...)
			continue
		}
		if admission.ExecutionKind != appExecutionKindTaskBacked || admission.PluginKey != "doubao" ||
			admission.PluginVersion != "1.2.0" || !strings.HasPrefix(admission.ActualModel, "doubao-seedance-") {
			continue
		}
		plugin, ok := generation.Get(admission.PluginKey)
		if !ok || plugin.Meta.Version != admission.PluginVersion || plugin.SourceSHA256() == "" ||
			!slices.Contains(plugin.Meta.Models, admission.ActualModel) {
			continue
		}
		if !slices.ContainsFunc(plugin.Meta.Protocols, func(claim jsplugin.ProtocolClaim) bool {
			return claim.Name == admission.Protocol && (len(claim.Models) == 0 || slices.Contains(claim.Models, admission.ActualModel)) &&
				(claim.Name == "openai_video" || (claim.Name == "openai_responses" && slices.Contains(claim.Supports, "background")))
		}) {
			continue
		}
		price := prices[admission.PublicModel]
		_, hasPublicPrice := price["ModelPrice"]
		_, hasPublicRatio := price["ModelRatio"]
		_, hasPublicMode := price["billing_setting.billing_mode"]
		if !hasPublicPrice && !hasPublicRatio && !hasPublicMode &&
			prices[admission.ActualModel]["billing_setting.billing_mode"] == "tiered_expr" {
			price = prices[admission.ActualModel]
		}
		if rawMode, present := price["billing_setting.billing_mode"]; present {
			mode, valid := rawMode.(string)
			if !valid || (mode != "ratio" && mode != "tiered_expr") {
				continue
			}
		}
		basis, exprVersion := billingexpr.BillingBasisRequest, 0
		frozenPrice := model.PricingValues{}
		if price["billing_setting.billing_mode"] == "tiered_expr" {
			expression, ok := price["billing_setting.billing_expr"].(string)
			if !ok || len(plugin.Meta.UsageSchema) == 0 || billing_setting.SmokeTestTaskExpr(expression, plugin.Meta.UsageSchema) != nil ||
				len(billingexpr.UsedUsageKeys(expression)) == 0 {
				continue
			}
			frozenPrice["billing_setting.billing_mode"] = "tiered_expr"
			frozenPrice["billing_setting.billing_expr"] = expression
			basis, exprVersion = billingexpr.BillingBasisTask, billingexpr.ExprVersion(expression)
		} else {
			mode, _ := price["billing_setting.billing_mode"].(string)
			value, ok := price["ModelPrice"].(float64)
			if (mode != "" && mode != "ratio") || !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				continue
			}
			frozenPrice["ModelPrice"] = value
		}
		schema, err := common.Marshal(plugin.Meta.UsageSchema)
		if err != nil {
			return nil, err
		}
		if common.QuotaPerUnit <= 0 || math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) {
			return nil, appAuthError("pricing_not_supported")
		}
		for _, group := range groups {
			var abilities []model.Ability
			if err := model.AppPluginCurrentRead(tx).Where(map[string]any{"group": group.name, "model": admission.PublicModel, "enabled": true}).
				Order("channel_id").Find(&abilities).Error; err != nil {
				return nil, err
			}
			for _, ability := range abilities {
				var channel model.Channel
				q := model.AppPluginCurrentRead(tx).Where("id = ? AND status = ?", ability.ChannelId, common.ChannelStatusEnabled).Limit(1).Find(&channel)
				if q.Error != nil {
					return nil, q.Error
				}
				if q.RowsAffected == 0 || !slices.Contains(channel.GetModels(), admission.PublicModel) ||
					!slices.Contains(channel.GetGroups(), group.name) {
					continue
				}
				var settings dto.ChannelSettings
				if channel.Setting != nil && *channel.Setting != "" && common.UnmarshalJsonStr(*channel.Setting, &settings) != nil {
					continue
				}
				if channel.Type == constant.ChannelTypeTaskPlugin {
					if settings.TaskPluginKey != admission.PluginKey {
						continue
					}
				} else if owner, ok := generation.GetByChannelType(channel.Type); !ok || owner != plugin ||
					(settings.TaskPluginKey != "" && settings.TaskPluginKey != admission.PluginKey) {
					continue
				}
				mapping := map[string]string{}
				if channel.ModelMapping != nil && *channel.ModelMapping != "" && common.UnmarshalJsonStr(*channel.ModelMapping, &mapping) != nil {
					continue
				}
				actual := admission.PublicModel
				visited := map[string]bool{actual: true}
				for {
					mapped := mapping[actual]
					if mapped == "" || mapped == actual {
						break
					}
					if visited[mapped] {
						actual = ""
						break
					}
					visited[mapped] = true
					actual = mapped
				}
				if actual != admission.ActualModel {
					continue
				}
				candidates = append(candidates, model.AppExecutionModel{PublicModel: admission.PublicModel, ActualModel: actual,
					ExecutionKind: appExecutionKindTaskBacked,
					PluginKey:     admission.PluginKey, PluginVersion: admission.PluginVersion, PluginSHA256: plugin.SourceSHA256(),
					Protocol: admission.Protocol, ChannelID: channel.Id, Group: group.name, GroupRatio: group.ratio,
					Price: frozenPrice, BillingBasis: basis, ExprVersion: exprVersion, UsageSchemaJSON: string(schema), QuotaPerUnit: common.QuotaPerUnit})
			}
		}
	}
	return candidates, nil
}

func effectiveNativeAppExecutionModels(tx *gorm.DB, admission AppModelAdmission, groups []appExecutionGroup,
	price model.PricingValues, derivationKey []byte) ([]model.AppExecutionModel, error) {
	if len(derivationKey) < 32 ||
		common.QuotaPerUnit <= 0 || math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) {
		return nil, appAuthError("pricing_not_supported")
	}
	frozenPrice, basis, exprVersion, priced := freezeNativeAppExecutionPrice(price)
	if !priced {
		return nil, nil
	}
	candidates := []model.AppExecutionModel{}
	for _, group := range groups {
		var abilities []model.Ability
		if err := model.AppPluginCurrentRead(tx).Where(map[string]any{
			"group": group.name, "model": admission.PublicModel, "enabled": true,
		}).Order("channel_id").Find(&abilities).Error; err != nil {
			return nil, err
		}
		for _, ability := range abilities {
			var channel model.Channel
			q := model.AppPluginCurrentRead(tx).Where("id = ? AND status = ?", ability.ChannelId,
				common.ChannelStatusEnabled).Limit(1).Find(&channel)
			if q.Error != nil {
				return nil, q.Error
			}
			if q.RowsAffected == 0 || !slices.Contains(admission.ChannelTypes, channel.Type) ||
				!slices.Contains(channel.GetModels(), admission.PublicModel) ||
				!slices.Contains(channel.GetGroups(), group.name) {
				continue
			}
			apiType, ok := common.ChannelType2APIType(channel.Type)
			if !ok || apiType != constant.APITypeOpenAI || !validNativeAppChannelSettings(channel) {
				continue
			}
			actual, ok := resolveAppExecutionActualModel(channel, admission.PublicModel)
			if !ok || actual != admission.ActualModel {
				continue
			}
			baseURL := channel.GetBaseURL()
			if baseURL == "" {
				baseURL = constant.GetChannelBaseURL(channel.Type)
			}
			if !validNativeAppBaseURL(baseURL) {
				continue
			}
			keys := channel.GetKeys()
			if channel.ChannelInfo.IsMultiKey || len(keys) != 1 || keys[0] == "" || keys[0] != channel.Key {
				continue
			}
			connection, err := appNativeExecutionDigest(derivationKey, "connection",
				[]any{channel.Type, apiType, baseURL, channel.OpenAIOrganization})
			if err != nil {
				return nil, err
			}
			credential, err := appNativeExecutionDigest(derivationKey, "credential", keys[0])
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, model.AppExecutionModel{
				PublicModel: admission.PublicModel, ActualModel: actual, ExecutionKind: appExecutionKindNativeResponse,
				Protocol: admission.Protocol, ChannelID: channel.Id, ChannelType: channel.Type, APIType: apiType,
				BaseURL: baseURL, ConnectionDigest: connection, CredentialDigest: credential, CredentialIndex: 0,
				RequestProfile: admission.RequestProfile, Group: group.name, GroupRatio: group.ratio,
				Price: frozenPrice, BillingBasis: basis, ExprVersion: exprVersion, QuotaPerUnit: common.QuotaPerUnit,
			})
		}
	}
	return candidates, nil
}

func freezeNativeAppExecutionPrice(price model.PricingValues) (model.PricingValues, string, int, bool) {
	mode := ""
	if rawMode, present := price["billing_setting.billing_mode"]; present {
		var valid bool
		mode, valid = rawMode.(string)
		if !valid || (mode != "ratio" && mode != "tiered_expr") {
			return nil, "", 0, false
		}
	}
	if mode == "tiered_expr" {
		expression, ok := price["billing_setting.billing_expr"].(string)
		if !ok || expression == "" || billing_setting.SmokeTestExpr(expression) != nil ||
			!appResponseTieredExpressionSupported(expression) {
			return nil, "", 0, false
		}
		used := billingexpr.UsedVars(expression)
		hasTokenDimension := false
		for _, name := range []string{"p", "c", "len", "cr", "cc", "cc1h", "img", "img_o", "ai", "ao"} {
			hasTokenDimension = hasTokenDimension || used[name]
		}
		if !hasTokenDimension {
			return nil, "", 0, false
		}
		return model.PricingValues{
			"billing_setting.billing_mode":        "tiered_expr",
			"billing_setting.billing_expr":        expression,
			"billing_setting.billing_expr_sha256": billingexpr.ExprHashString(expression),
		}, billingexpr.BillingBasisToken, billingexpr.ExprVersion(expression), true
	}
	if mode != "" && mode != "ratio" {
		return nil, "", 0, false
	}
	if value, ok := finiteAppExecutionPrice(price["ModelPrice"]); ok {
		return model.PricingValues{"ModelPrice": value}, billingexpr.BillingBasisRequest, 0, true
	}
	modelRatio, ok := finiteAppExecutionPrice(price["ModelRatio"])
	if !ok {
		return nil, "", 0, false
	}
	frozen := model.PricingValues{"ModelRatio": modelRatio}
	for key, fallback := range map[string]float64{
		"CompletionRatio":      1,
		"CacheRatio":           1,
		"CreateCacheRatio":     1.25,
		"ImageRatio":           1,
		"AudioRatio":           1,
		"AudioCompletionRatio": 1,
	} {
		value := fallback
		if configured, present := price[key]; present {
			var valid bool
			value, valid = finiteAppExecutionPrice(configured)
			if !valid {
				return nil, "", 0, false
			}
		}
		frozen[key] = value
	}
	return frozen, billingexpr.BillingBasisToken, 0, true
}

func finiteAppExecutionPrice(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok && number >= 0 && !math.IsNaN(number) && !math.IsInf(number, 0)
}

func validNativeAppChannelSettings(channel model.Channel) bool {
	var settings dto.ChannelSettings
	if channel.Setting != nil && !decodeStrictNativeChannelSettings(*channel.Setting, &settings) {
		return false
	}
	var other dto.ChannelOtherSettings
	if !decodeStrictNativeChannelSettings(channel.OtherSettings, &other) {
		return false
	}
	emptyOverride := func(value *string) bool {
		return value == nil || *value == ""
	}
	return reflect.DeepEqual(settings, dto.ChannelSettings{}) &&
		reflect.DeepEqual(other, dto.ChannelOtherSettings{}) &&
		emptyOverride(channel.ParamOverride) && emptyOverride(channel.HeaderOverride)
}

func decodeStrictNativeChannelSettings(raw string, target any) bool {
	if raw == "" {
		return true
	}
	typ := reflect.TypeOf(target)
	if len(raw) > 64*1024 || typ.Kind() != reflect.Pointer || typ.Elem().Kind() != reflect.Struct ||
		common.UnmarshalJsonStr(raw, target) != nil {
		return false
	}
	value := gjson.Parse(raw)
	if !value.IsObject() {
		return false
	}
	allowed := make(map[string]struct{}, typ.Elem().NumField())
	for i := range typ.Elem().NumField() {
		name, _, _ := strings.Cut(typ.Elem().Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			allowed[name] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(allowed))
	valid := true
	value.ForEach(func(key, _ gjson.Result) bool {
		if _, ok := allowed[key.Str]; !ok {
			valid = false
			return false
		}
		if _, duplicate := seen[key.Str]; duplicate {
			valid = false
			return false
		}
		seen[key.Str] = struct{}{}
		return true
	})
	return valid
}

func resolveAppExecutionActualModel(channel model.Channel, public string) (string, bool) {
	mapping := map[string]string{}
	if channel.ModelMapping != nil && *channel.ModelMapping != "" &&
		common.UnmarshalJsonStr(*channel.ModelMapping, &mapping) != nil {
		return "", false
	}
	actual := public
	visited := map[string]bool{actual: true}
	for {
		mapped := mapping[actual]
		if mapped == "" || mapped == actual {
			return actual, true
		}
		if visited[mapped] {
			return "", false
		}
		visited[mapped] = true
		actual = mapped
	}
}

func validNativeAppBaseURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && raw != "" && strings.TrimSpace(raw) == raw &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" &&
		parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == ""
}

func appNativeExecutionDigest(key []byte, purpose string, value any) (string, error) {
	if len(key) < 32 || !slices.Contains([]string{"connection", "credential"}, purpose) {
		return "", errors.New("invalid_native_identity")
	}
	raw, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("new-api/app-native-response/" + purpose + "/v1\x00"))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
