package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

type AppTaskRetrieval struct {
	Execution model.AppTaskExecution
	Task      model.Task
	Artifacts []types.TaskArtifact
}

func (s *AppExecutionService) appGrantAuthorityTx(tx *gorm.DB, subject types.AppRelaySubject,
	required []string) (model.AppExecutionGrant, model.User, error) {
	var grant model.AppExecutionGrant
	if len(subject.TokenHash) != 64 || len(s.options.DerivationKey) < 32 ||
		tx.Where("token_hash = ?", subject.TokenHash).First(&grant).Error != nil {
		return grant, model.User{}, appAuthError("invalid_grant")
	}
	if !hmac.Equal([]byte(grant.TokenHash), []byte(subject.TokenHash)) ||
		(subject.GrantID != "" && subject.GrantID != grant.GrantID) ||
		(subject.UserID != 0 && subject.UserID != grant.UserID) ||
		!hmac.Equal([]byte(serviceDigestBytes([]byte(s.executionGrantToken(grant)))), []byte(grant.TokenHash)) {
		return grant, model.User{}, appAuthError("invalid_grant")
	}
	var binding model.AppExecutionGrantBinding
	if common.UnmarshalJsonStr(grant.BindingJSON, &binding) != nil ||
		binding.GrantID != grant.GrantID || binding.ScopeHash != grant.ScopeHash ||
		binding.RequestHash != grant.RequestHash || binding.Issuer != s.options.Issuer ||
		binding.AppKey != grant.AppKey || binding.InstallationID != grant.InstallationID ||
		binding.AppSessionID != grant.AppSessionID || binding.Subject != grant.Subject ||
		binding.UserID != grant.UserID || binding.RunID != grant.RunID ||
		binding.ExecutionRequestID != grant.ExecutionRequestID || binding.Operation != grant.Operation ||
		binding.ExpiresAt != grant.ExpiresAt {
		return grant, model.User{}, appAuthError("invalid_grant")
	}
	snapshotHash, err := appExecutionGrantSnapshotHash(
		grant, binding.RegisteredAssetInputs, binding.AssetSnapshotSHA256)
	if err != nil || !hmac.Equal([]byte(snapshotHash), []byte(binding.SnapshotSHA256)) {
		return grant, model.User{}, appAuthError("invalid_grant")
	}
	now := s.options.Now()
	if !operation_setting.AppPluginV1Enabled || !operation_setting.AppExecutionGrantsEnabled ||
		(grant.AppKey == "seedance-repro" && !operation_setting.AppPluginSeedanceEnabled) {
		return grant, model.User{}, appAuthError("app_execution_disabled")
	}
	if grant.ExpiresAt <= now.Unix() {
		return grant, model.User{}, appAuthError("execution_grant_expired")
	}
	var candidates []model.AppExecutionModel
	if common.UnmarshalJsonStr(grant.ModelsJSON, &candidates) != nil {
		return grant, model.User{}, appAuthError("invalid_grant")
	}
	_, assetSnapshotHash, assetExpiry, err := validateAppRegisteredAssetInputs(
		binding.RegisteredAssetInputs, binding.Subject, candidates, now)
	if err != nil || !hmac.Equal([]byte(assetSnapshotHash), []byte(binding.AssetSnapshotSHA256)) ||
		(assetExpiry != 0 && grant.ExpiresAt > assetExpiry) {
		return grant, model.User{}, appAuthError("invalid_grant")
	}
	authority, err := s.taskAuthorityTx(tx, model.AppServiceIdentity{
		AppKey: grant.AppKey, InstallationID: grant.InstallationID,
		CredentialID: binding.IssuingCredentialID, Version: binding.IssuingCredentialVersion,
	}, grant.AppKey, grant.AppSessionID, grant.Subject, required, false)
	if err != nil {
		return grant, model.User{}, err
	}
	var current model.AppExecutionGrant
	if err := model.AppPluginCurrentRead(tx).Where(
		"token_hash = ?", subject.TokenHash,
	).First(&current).Error; err != nil || current != grant {
		return grant, model.User{}, appAuthError("invalid_grant")
	}
	grant = current
	if authority.user.Id != grant.UserID || authority.session.Generation != binding.Generation {
		return grant, model.User{}, appAuthError("invalid_grant")
	}
	return grant, authority.user, nil
}

func (s *AppExecutionService) appRelayAuthorityTx(tx *gorm.DB, subject types.AppRelaySubject) (model.AppExecutionGrant, model.User, error) {
	return s.appGrantAuthorityTx(tx, subject, []string{"model.invoke"})
}

func appGrantSubject(token string) (types.AppRelaySubject, error) {
	subject := types.AppRelaySubject{}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(token) != 43 || len(decoded) != 32 {
		return subject, appAuthError("invalid_grant")
	}
	subject.TokenHash = serviceDigestBytes([]byte(token))
	return subject, nil
}

// AuthenticateAppRelay accepts the opaque grant only over the exact model
// create boundary. It never authenticates an AppService credential or a PAT.
func (s *AppExecutionService) AuthenticateAppRelay(ctx context.Context, token, protocol string, raw []byte) (types.AppRelaySubject, model.User, error) {
	subject, err := appGrantSubject(token)
	if err != nil || !slices.Contains([]string{"openai_video", "openai_responses"}, protocol) {
		return subject, model.User{}, appAuthError("invalid_grant")
	}
	subject.Protocol = protocol
	var user model.User
	err = model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		grant, current, err := s.appRelayAuthorityTx(tx, subject)
		if err != nil {
			return err
		}
		var body map[string]any
		if err := decodeAppExecutionJSON(raw, &body); err != nil {
			return err
		}
		name, ok := body["model"].(string)
		if !ok || !appControlOpaque(name, 128) {
			return appAuthError("invalid_request")
		}
		var candidates []model.AppExecutionModel
		if common.UnmarshalJsonStr(grant.ModelsJSON, &candidates) != nil {
			return appAuthError("invalid_grant")
		}
		matchedKinds := map[string]bool{}
		var nativeOutbound []byte
		for _, candidate := range candidates {
			if candidate.PublicModel != name || candidate.Protocol != protocol {
				continue
			}
			switch candidate.ExecutionKind {
			case model.AppExecutionModelKindTaskBacked:
				if validateAppTaskBody(grant, protocol, name, body) == nil {
					matchedKinds[candidate.ExecutionKind] = true
				}
			case model.AppExecutionModelKindNativeResponse:
				prepared, prepareErr := PrepareAppResponseRequest(raw, candidate)
				if prepareErr == nil {
					if nativeOutbound != nil && !bytes.Equal(nativeOutbound, prepared.Outbound) {
						return appAuthError("invalid_grant")
					}
					matchedKinds[candidate.ExecutionKind] = true
					nativeOutbound = prepared.Outbound
				}
			}
		}
		if len(matchedKinds) == 0 {
			if slices.ContainsFunc(candidates, func(candidate model.AppExecutionModel) bool {
				return candidate.PublicModel == name && candidate.Protocol == protocol
			}) {
				return appAuthError("invalid_request")
			}
			return appAuthError("model_not_supported")
		}
		if len(matchedKinds) != 1 {
			return appAuthError("invalid_grant")
		}
		selectedKind := ""
		for kind := range matchedKinds {
			selectedKind = kind
		}
		var assets []types.AppRelayAssetInput
		var submission any = body
		if selectedKind == model.AppExecutionModelKindTaskBacked {
			assets, err = appRelayAssetInputs(grant, name, protocol)
			if err != nil {
				return err
			}
		} else {
			assets, err = appRelayAssetInputs(grant, name, protocol)
			if err != nil || len(assets) != 0 {
				return appAuthError("invalid_grant")
			}
			submission = nativeOutbound
		}
		hash, err := appPluginHash([]any{protocol, submission})
		if err != nil {
			return err
		}
		subject.GrantID, subject.UserID = grant.GrantID, current.Id
		subject.PublicModel, subject.ExecutionKind, subject.SubmissionHash =
			name, selectedKind, hash
		subject.AssetInputs = assets
		var response AppExecutionGrantResult
		if common.UnmarshalJsonStr(grant.ResponseJSON, &response) != nil {
			return appAuthError("invalid_grant")
		}
		subject.Strategy, subject.ConversionPolicy = response.Strategy, response.ConversionPolicy
		user = current
		return nil
	})
	return subject, user, err
}

// AuthenticateAppTaskRetrieval binds a short-lived create grant to the one
// accepted Task it created. Long-lived reads use the AppService task lookup.
func (s *AppExecutionService) AuthenticateAppTaskRetrieval(ctx context.Context, token, protocol,
	resourceID string) (AppTaskRetrieval, model.User, error) {
	var result AppTaskRetrieval
	subject, err := appGrantSubject(token)
	if err != nil || !slices.Contains([]string{"", "openai_video", "openai_responses"}, protocol) {
		return result, model.User{}, appAuthError("invalid_grant")
	}
	if !appControlOpaque(resourceID, 128) {
		return result, model.User{}, appAuthError("not_found")
	}
	var user model.User
	err = model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		grant, current, err := s.appGrantAuthorityTx(tx, subject, []string{"model.invoke", "task.read"})
		if err != nil {
			return err
		}
		var execution model.AppTaskExecution
		if err := model.AppPluginCurrentRead(tx).Where(
			"grant_id = ? AND execution_kind = ?", grant.GrantID, model.AppExecutionKindTask,
		).First(&execution).Error; err != nil {
			return appAuthError("not_found")
		}
		if execution.GrantID != grant.GrantID || execution.AppKey != grant.AppKey ||
			execution.InstallationID != grant.InstallationID || execution.AppSessionID != grant.AppSessionID ||
			execution.Subject != grant.Subject || execution.UserID != grant.UserID ||
			execution.RunID != grant.RunID || execution.ExecutionRequestID != grant.ExecutionRequestID ||
			execution.Operation != grant.Operation || !execution.ProviderAccepted || execution.ProviderTaskID == "" ||
			!slices.Contains([]string{"openai_video", "openai_responses"}, execution.Protocol) ||
			(protocol != "" && protocol != execution.Protocol) {
			return appAuthError("not_found")
		}
		expectedID := execution.TaskID
		if protocol == "openai_responses" {
			expectedID = "resp_" + strings.TrimPrefix(execution.TaskID, "task_")
		}
		if resourceID != expectedID {
			return appAuthError("not_found")
		}
		task, err := model.GetScopedAppTask(ctx, model.AppPluginCurrentRead(tx), model.AppTaskScope{
			AppKey: execution.AppKey, InstallationID: execution.InstallationID,
			Subject: execution.Subject, UserID: execution.UserID,
		}, execution.TaskID, "")
		if err != nil || task.GrantID != grant.GrantID {
			return appAuthError("not_found")
		}
		var projection model.Task
		if err := model.AppPluginCurrentRead(tx).Where(
			"task_id = ? AND user_id = ? AND execution_mode = ?",
			execution.TaskID, execution.UserID, model.TaskExecutionModeAppManaged,
		).First(&projection).Error; err != nil {
			return appAuthError("not_found")
		}
		if projection.TaskID != execution.TaskID || projection.UserId != execution.UserID ||
			projection.ChannelId != execution.ChannelID || string(projection.Platform) != execution.PluginKey ||
			projection.Group != execution.ActualGroup || projection.Properties.OriginModelName != execution.PublicModel ||
			projection.Properties.UpstreamModelName != execution.ActualModel ||
			projection.PrivateData.UpstreamTaskID != execution.ProviderTaskID ||
			projection.PrivateData.Key != "" || projection.PrivateData.TokenId != 0 ||
			projection.PrivateData.ResponsesBackground != (execution.Protocol == "openai_responses") {
			return appAuthError("invalid_evidence")
		}
		var expectedStatus model.TaskStatus
		switch execution.ProviderState {
		case "queued":
			expectedStatus = model.TaskStatusQueued
		case "running":
			expectedStatus = model.TaskStatusInProgress
		case "succeeded":
			expectedStatus = model.TaskStatusSuccess
		case "failed":
			expectedStatus = model.TaskStatusFailure
		case "cancelled":
			expectedStatus = model.TaskStatusCancelled
		default:
			return appAuthError("invalid_evidence")
		}
		if projection.Status != expectedStatus {
			return appAuthError("invalid_evidence")
		}
		artifacts := []types.TaskArtifact{}
		if strings.TrimSpace(execution.ArtifactsJSON) != "" {
			if common.UnmarshalJsonStr(execution.ArtifactsJSON, &artifacts) != nil || len(artifacts) > 64 {
				return appAuthError("invalid_evidence")
			}
		}
		seen := make(map[string]struct{}, len(artifacts))
		for _, artifact := range artifacts {
			if !appControlOpaque(artifact.Key, maxTaskArtifactKeyLength) || strings.Contains(artifact.Key, "/") ||
				!slices.Contains([]string{"video", "audio", "image", "file"}, artifact.Type) ||
				len(artifact.MimeType) > 255 || strings.ContainsAny(artifact.MimeType, "\r\n") {
				return appAuthError("invalid_evidence")
			}
			if _, exists := seen[artifact.Key]; exists {
				return appAuthError("invalid_evidence")
			}
			seen[artifact.Key] = struct{}{}
		}
		result = AppTaskRetrieval{Execution: execution, Task: projection, Artifacts: artifacts}
		user = current
		return nil
	})
	if err != nil {
		return AppTaskRetrieval{}, model.User{}, err
	}
	return result, user, nil
}

type appAssetObservation struct {
	PublicModel string
	Protocol    string
	JSONPointer string
	MediaType   string
	AssetURI    string
}

func appRelayAssetInputs(grant model.AppExecutionGrant, name, protocol string) ([]types.AppRelayAssetInput, error) {
	var binding model.AppExecutionGrantBinding
	if common.UnmarshalJsonStr(grant.BindingJSON, &binding) != nil {
		return nil, appAuthError("invalid_grant")
	}
	inputs := []types.AppRelayAssetInput{}
	for _, input := range binding.RegisteredAssetInputs {
		if input.PublicModel == name && input.Protocol == protocol {
			inputs = append(inputs, types.AppRelayAssetInput{
				MediaType: input.MediaType,
				AssetURI:  input.AssetURI,
				ExpiresAt: input.ExpiresAt.Unix(),
			})
		}
	}
	return inputs, nil
}

func appGrantAssetObservations(grant model.AppExecutionGrant, name, protocol string) ([]appAssetObservation, error) {
	var binding model.AppExecutionGrantBinding
	if common.UnmarshalJsonStr(grant.BindingJSON, &binding) != nil {
		return nil, appAuthError("invalid_grant")
	}
	inputs := []appAssetObservation{}
	for _, input := range binding.RegisteredAssetInputs {
		if input.PublicModel == name && input.Protocol == protocol {
			inputs = append(inputs, appAssetObservation{
				PublicModel: input.PublicModel,
				Protocol:    input.Protocol,
				JSONPointer: input.JSONPointer,
				MediaType:   input.MediaType,
				AssetURI:    input.AssetURI,
			})
		}
	}
	return inputs, nil
}

func exactAppObject(value any, fields ...string) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != len(fields) {
		return nil, false
	}
	for _, field := range fields {
		if _, present := object[field]; !present {
			return nil, false
		}
	}
	return object, true
}

func appResponsesAssetInputs(value any, name string) ([]appAssetObservation, error) {
	messages, ok := value.([]any)
	if !ok || len(messages) == 0 {
		return nil, appAuthError("invalid_request")
	}
	observed := []appAssetObservation{}
	for messageIndex, rawMessage := range messages {
		message, ok := exactAppObject(rawMessage, "role", "content")
		if !ok || message["role"] != "user" {
			return nil, appAuthError("invalid_request")
		}
		content, ok := message["content"].([]any)
		if !ok || len(content) == 0 {
			return nil, appAuthError("invalid_request")
		}
		for contentIndex, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return nil, appAuthError("invalid_request")
			}
			switch part["type"] {
			case "input_text":
				exact, ok := exactAppObject(part, "type", "text")
				if !ok {
					return nil, appAuthError("invalid_request")
				}
				if _, ok := exact["text"].(string); !ok {
					return nil, appAuthError("invalid_request")
				}
			case "input_image":
				exact, ok := exactAppObject(part, "type", "image_url")
				uri, uriOK := exact["image_url"].(string)
				if !ok || !uriOK || !appRegisteredAssetURI.MatchString(uri) {
					return nil, appAuthError("invalid_request")
				}
				observed = append(observed, appAssetObservation{
					PublicModel: name,
					Protocol:    "openai_responses",
					JSONPointer: fmt.Sprintf("/input/%d/content/%d/image_url", messageIndex, contentIndex),
					MediaType:   "image",
					AssetURI:    uri,
				})
			default:
				return nil, appAuthError("invalid_request")
			}
		}
	}
	return observed, nil
}

func appVideoAssetInputs(value any, name string) ([]appAssetObservation, error) {
	content, ok := value.([]any)
	if !ok {
		return nil, appAuthError("invalid_request")
	}
	observed := []appAssetObservation{}
	for index, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, appAuthError("invalid_request")
		}
		partType, _ := part["type"].(string)
		if partType == "text" {
			exact, ok := exactAppObject(part, "type", "text")
			if !ok {
				return nil, appAuthError("invalid_request")
			}
			if _, ok := exact["text"].(string); !ok {
				return nil, appAuthError("invalid_request")
			}
			continue
		}
		mediaType, ok := strings.CutSuffix(partType, "_url")
		if !ok || !slices.Contains([]string{"image", "video", "audio"}, mediaType) {
			return nil, appAuthError("invalid_request")
		}
		exact, ok := exactAppObject(part, "type", partType)
		if !ok {
			return nil, appAuthError("invalid_request")
		}
		reference, ok := exactAppObject(exact[partType], "url")
		uri, uriOK := reference["url"].(string)
		if !ok || !uriOK || !appRegisteredAssetURI.MatchString(uri) {
			return nil, appAuthError("invalid_request")
		}
		observed = append(observed, appAssetObservation{
			PublicModel: name,
			Protocol:    "openai_video",
			JSONPointer: fmt.Sprintf("/content/%d/%s_url/url", index, mediaType),
			MediaType:   mediaType,
			AssetURI:    uri,
		})
	}
	return observed, nil
}

func appAssetObservationKey(input appAssetObservation) string {
	return strings.Join([]string{input.PublicModel, input.Protocol, input.JSONPointer,
		input.MediaType, input.AssetURI}, "\x00")
}

func exactAppAssetObservations(expected, observed []appAssetObservation) bool {
	if len(expected) != len(observed) {
		return false
	}
	counts := make(map[string]int, len(expected))
	for _, input := range expected {
		counts[appAssetObservationKey(input)]++
	}
	for _, input := range observed {
		key := appAssetObservationKey(input)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

func appProviderAssetKey(mediaType, uri string) string {
	return mediaType + "\x00" + uri
}

func countAppProviderAssetURIs(value any, field string) int {
	switch typed := value.(type) {
	case string:
		if field != "text" && field != "prompt" && strings.HasPrefix(typed, "asset://") {
			return 1
		}
	case map[string]any:
		count := 0
		for key, nested := range typed {
			count += countAppProviderAssetURIs(nested, key)
		}
		return count
	case []any:
		count := 0
		for _, nested := range typed {
			count += countAppProviderAssetURIs(nested, field)
		}
		return count
	}
	return 0
}

// ValidateAppProviderAssetInputs fences the final serialized provider body.
// Provider projection may reorder content, so this boundary compares typed
// media identities as a multiset while rejecting untyped asset references.
func ValidateAppProviderAssetInputs(raw []byte, expected []types.AppRelayAssetInput, now time.Time) error {
	var body map[string]any
	if err := decodeAppExecutionJSON(raw, &body); err != nil {
		return err
	}
	content, ok := body["content"].([]any)
	if !ok || len(content) == 0 {
		return appAuthError("invalid_request")
	}
	observed := make(map[string]int, len(expected))
	typedCount := 0
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return appAuthError("invalid_request")
		}
		partType, _ := part["type"].(string)
		if partType == "text" {
			exact, ok := exactAppObject(part, "type", "text")
			if !ok {
				return appAuthError("invalid_request")
			}
			if _, ok := exact["text"].(string); !ok {
				return appAuthError("invalid_request")
			}
			continue
		}
		mediaType, ok := strings.CutSuffix(partType, "_url")
		if !ok || !slices.Contains([]string{"image", "video", "audio"}, mediaType) {
			return appAuthError("invalid_request")
		}
		exact, ok := exactAppObject(part, "type", partType)
		if !ok {
			return appAuthError("invalid_request")
		}
		reference, ok := exactAppObject(exact[partType], "url")
		uri, uriOK := reference["url"].(string)
		if !ok || !uriOK || !appRegisteredAssetURI.MatchString(uri) {
			return appAuthError("invalid_request")
		}
		observed[appProviderAssetKey(mediaType, uri)]++
		typedCount++
	}
	if countAppProviderAssetURIs(body, "") != typedCount || typedCount != len(expected) {
		return appAuthError("scope_denied")
	}
	for _, input := range expected {
		if input.ExpiresAt <= now.Unix() || !appRegisteredAssetURI.MatchString(input.AssetURI) ||
			!slices.Contains([]string{"image", "video", "audio"}, input.MediaType) {
			return appAuthError("scope_denied")
		}
		key := appProviderAssetKey(input.MediaType, input.AssetURI)
		if observed[key] == 0 {
			return appAuthError("scope_denied")
		}
		observed[key]--
	}
	for _, count := range observed {
		if count != 0 {
			return appAuthError("scope_denied")
		}
	}
	return nil
}

// Only declared model inputs are accepted. Unknown primitive options require
// the frozen per-field rule; transport and identity containers never pass.
func validateAppTaskBody(grant model.AppExecutionGrant, protocol, name string, body map[string]any) error {
	var rules []AppPassthroughRule
	if common.UnmarshalJsonStr(grant.PassthroughJSON, &rules) != nil {
		return appAuthError("invalid_grant")
	}
	expectedAssets, err := appGrantAssetObservations(grant, name, protocol)
	if err != nil {
		return err
	}
	observedAssets := []appAssetObservation{}
	for key, value := range body {
		switch key {
		case "model":
			if value != name {
				return appAuthError("invalid_request")
			}
		case "prompt":
			if _, ok := value.(string); !ok {
				return appAuthError("invalid_request")
			}
		case "size":
			size, ok := value.(string)
			if !ok {
				return appAuthError("invalid_request")
			}
			if _, err := appTaskVideoResolution(size); err != nil {
				return err
			}
		case "seconds", "duration":
			raw, _ := common.Marshal(value)
			number := gjson.ParseBytes(raw).Float()
			if text, ok := value.(string); ok {
				number = gjson.Parse(text).Float()
			}
			if number <= 0 || number > 3600 || math.Trunc(number) != number {
				return appAuthError("invalid_request")
			}
		case "input":
			if protocol != "openai_responses" {
				return appAuthError("invalid_request")
			}
			if _, ok := value.(string); !ok {
				var err error
				observedAssets, err = appResponsesAssetInputs(value, name)
				if err != nil {
					return err
				}
			}
		case "content":
			if protocol != "openai_video" {
				return appAuthError("invalid_request")
			}
			var err error
			observedAssets, err = appVideoAssetInputs(value, name)
			if err != nil {
				return err
			}
		case "stream":
			if value != false {
				return appAuthError("scope_denied")
			}
		case "background":
			if protocol != "openai_responses" || value != true {
				return appAuthError("scope_denied")
			}
		case "metadata":
			fields, ok := value.(map[string]any)
			if !ok {
				return appAuthError("invalid_request")
			}
			for field, option := range fields {
				if !appTaskOptionAllowed(rules, protocol, name, "/metadata/"+field, option) {
					return appAuthError("scope_denied")
				}
			}
		default:
			if !appTaskOptionAllowed(rules, protocol, name, "/"+key, value) {
				return appAuthError("scope_denied")
			}
		}
	}
	if protocol == "openai_responses" && body["background"] != true {
		return appAuthError("scope_denied")
	}
	if !exactAppAssetObservations(expectedAssets, observedAssets) {
		return appAuthError("scope_denied")
	}
	return nil
}

var appPassthroughURIScheme = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*:`)

func appTaskOptionAllowed(rules []AppPassthroughRule, protocol, name, pointer string, value any) bool {
	for _, rule := range rules {
		if rule.PublicModel != name || rule.Protocol != protocol || rule.JSONPointer != pointer || !validAppPassthroughRule(rule) {
			continue
		}
		switch rule.Schema["type"] {
		case "boolean":
			if _, ok := value.(bool); !ok {
				return false
			}
		case "string":
			text, ok := value.(string)
			if !ok || len(text) > 64 || appPassthroughURIScheme.MatchString(text) {
				return false
			}
		case "number", "integer":
			number, ok := value.(float64)
			if !ok || math.IsNaN(number) || math.IsInf(number, 0) ||
				number < rule.Schema["minimum"].(float64) || number > rule.Schema["maximum"].(float64) ||
				(rule.Schema["type"] == "integer" && math.Trunc(number) != number) {
				return false
			}
		default:
			return false
		}
		if values, ok := rule.Schema["enum"].([]any); ok &&
			!slices.ContainsFunc(values, func(candidate any) bool { return reflect.DeepEqual(candidate, value) }) {
			return false
		}
		return true
	}
	return false
}

// PrepareAppTaskPassthrough maps the explicitly selected primitive fields to
// Doubao's metadata-to-provider projection after the final protocol decoder.
func PrepareAppTaskPassthrough(c *gin.Context, info *relaycommon.RelayInfo) error {
	var grant model.AppExecutionGrant
	if err := model.DB.WithContext(c.Request.Context()).Where("grant_id = ?", info.AppSubject.GrantID).First(&grant).Error; err != nil {
		return err
	}
	value, exists := c.Get(jsplugin.ContextKeyProtocolRequest)
	protocol, ok := value.(jsplugin.ProtocolRequestContext)
	envelope, object := protocol.Body.(map[string]any)
	if !exists || !ok || !object {
		return appAuthError("invalid_request")
	}
	original, ok := envelope["value"].(map[string]any)
	if !ok {
		return appAuthError("invalid_request")
	}
	options, _, err := appTaskPassthroughOptions(grant, *info.AppSubject, original)
	if err != nil {
		return err
	}
	request, exists := c.Get("task_request")
	raw, err := common.Marshal(request)
	var normalized map[string]any
	if !exists || err != nil || common.Unmarshal(raw, &normalized) != nil {
		return appAuthError("invalid_request")
	}
	metadata, _ := normalized["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	for field, option := range options {
		if old, exists := metadata[field]; exists && !reflect.DeepEqual(old, option) {
			return appAuthError("scope_denied")
		}
		metadata[field] = option
	}
	if size, ok := original["size"].(string); ok {
		resolution, err := appTaskVideoResolution(size)
		if err != nil {
			return err
		}
		if configured, exists := metadata["resolution"]; exists && configured != resolution {
			return appAuthError("scope_denied")
		}
		metadata["resolution"] = resolution
	}
	if _, present := original["seconds"]; !present {
		if duration, exists := original["duration"]; exists {
			normalized["seconds"] = duration
		}
	}
	if info.AppSubject.Protocol == "openai_responses" {
		if input, rich := original["input"].([]any); rich {
			observedAssets, err := appResponsesAssetInputs(input, info.AppSubject.PublicModel)
			if err != nil {
				return err
			}
			images := make([]any, 0, len(observedAssets))
			for _, asset := range observedAssets {
				images = append(images, asset.AssetURI)
			}
			normalized["images"] = images
		}
	}
	if info.AppSubject.Protocol == "openai_video" {
		if content, exists := original["content"]; exists {
			rawContent, err := common.Marshal(content)
			var projected []any
			if err != nil || common.Unmarshal(rawContent, &projected) != nil {
				return appAuthError("invalid_request")
			}
			metadata["content"] = projected
			texts := []string{}
			if prompt, ok := original["prompt"].(string); ok && prompt != "" {
				texts = append(texts, prompt)
			}
			for _, rawPart := range projected {
				part, _ := rawPart.(map[string]any)
				if part["type"] == "text" {
					text, _ := part["text"].(string)
					if text != "" {
						texts = append(texts, text)
					}
				}
			}
			normalized["prompt"] = strings.Join(texts, "\n")
			delete(normalized, "content")
		}
	}
	normalized["metadata"] = metadata
	c.Set("task_request", normalized)
	return nil
}

// Match the existing Doubao plugin's resolution convention, while rejecting
// malformed or unbounded dimensions instead of silently selecting a default.
func appTaskVideoResolution(size string) (string, error) {
	if slices.Contains([]string{"480p", "720p", "1080p", "4k"}, size) {
		return size, nil
	}
	width, height, ok := strings.Cut(strings.ReplaceAll(size, "*", "x"), "x")
	w, widthErr := strconv.Atoi(width)
	h, heightErr := strconv.Atoi(height)
	if !ok || widthErr != nil || heightErr != nil || w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return "", appAuthError("invalid_request")
	}
	switch dimension := max(w, h); {
	case dimension >= 3840:
		return "4k", nil
	case dimension >= 1920:
		return "1080p", nil
	case dimension >= 1280:
		return "720p", nil
	default:
		return "480p", nil
	}
}

func appTaskPassthroughOptions(grant model.AppExecutionGrant, subject types.AppRelaySubject, body map[string]any) (map[string]any, map[string]string, error) {
	var rules []AppPassthroughRule
	if common.UnmarshalJsonStr(grant.PassthroughJSON, &rules) != nil {
		return nil, nil, appAuthError("invalid_grant")
	}
	options, paths := map[string]any{}, map[string]string{}
	for _, rule := range rules {
		if rule.PublicModel != subject.PublicModel || rule.Protocol != subject.Protocol {
			continue
		}
		if !validAppPassthroughRule(rule) {
			return nil, nil, appAuthError("invalid_grant")
		}
		field := strings.TrimPrefix(rule.JSONPointer, "/")
		source := body
		if child, nested := strings.CutPrefix(field, "metadata/"); nested {
			source, _ = body["metadata"].(map[string]any)
			field = child
		}
		option, present := source[field]
		if !present {
			continue
		}
		if _, collision := options[field]; collision || !appTaskOptionAllowed(rules, subject.Protocol, subject.PublicModel, rule.JSONPointer, option) {
			return nil, nil, appAuthError("scope_denied")
		}
		options[field], paths[field] = option, rule.JSONPointer
	}
	return options, paths, nil
}

func (s *AppExecutionService) SelectAppRelayChannel(ctx context.Context, subject types.AppRelaySubject, constraints *taskdto.ChannelConstraints) (*model.Channel, model.AppExecutionModel, error) {
	var selected model.AppExecutionModel
	var channel *model.Channel
	err := model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		grant, user, err := s.appRelayAuthorityTx(tx, subject)
		if err != nil {
			return err
		}
		var candidates []model.AppExecutionModel
		if common.UnmarshalJsonStr(grant.ModelsJSON, &candidates) != nil {
			return appAuthError("invalid_grant")
		}
		pin, pinned, _ := constraints.ResolvedPin()
		for _, candidate := range candidates {
			if candidate.ExecutionKind != subject.ExecutionKind ||
				candidate.PublicModel != subject.PublicModel || candidate.Protocol != subject.Protocol ||
				(pinned && pin.ChannelId != candidate.ChannelID) ||
				(candidate.ExecutionKind == model.AppExecutionModelKindNativeResponse && pinned) {
				continue
			}
			var current model.Channel
			var err error
			if candidate.ExecutionKind == model.AppExecutionModelKindNativeResponse {
				current, err = validateNativeAppRelayChannelTx(
					tx, grant, user, candidate, constraints.Filters, s.options.DerivationKey,
				)
			} else {
				current, err = validateAppRelayChannelTx(tx, grant, user, candidate, constraints.Filters)
			}
			if err != nil {
				continue
			}
			channel, selected = &current, candidate
			return nil
		}
		return appAuthError("model_not_supported")
	})
	return channel, selected, err
}

func validateNativeAppRelayChannelTx(tx *gorm.DB, grant model.AppExecutionGrant, user model.User,
	candidate model.AppExecutionModel, filters []taskdto.ChannelFilter,
	derivationKey []byte) (model.Channel, error) {
	var channel model.Channel
	if candidate.ExecutionKind != model.AppExecutionModelKindNativeResponse ||
		candidate.Protocol != "openai_responses" ||
		candidate.RequestProfile != appResponsesTextProfileV1 ||
		candidate.ChannelType != constant.ChannelTypeOpenAI ||
		candidate.APIType != constant.APITypeOpenAI ||
		candidate.PluginKey != "" || candidate.PluginVersion != "" || candidate.PluginSHA256 != "" {
		return channel, appAuthError("scope_denied")
	}
	head, err := model.GetAppExecutionPolicyTx(tx, model.AppModelInvokePolicyKey)
	if err != nil || head.Version != grant.PolicyVersion || head.DocumentSHA256 != grant.PolicyDigest {
		return channel, appAuthError("model_policy_version_conflict")
	}
	groups, err := effectiveAppExecutionGroups(tx, user)
	if err != nil || !slices.ContainsFunc(groups, func(group appExecutionGroup) bool {
		return group.name == candidate.Group && group.ratio == candidate.GroupRatio
	}) {
		return channel, appAuthError("scope_denied")
	}
	if err := model.AppPluginCurrentRead(tx).Where(
		"id = ? AND status = ?", candidate.ChannelID, common.ChannelStatusEnabled,
	).First(&channel).Error; err != nil {
		return channel, appAuthError("model_not_supported")
	}
	var ability model.Ability
	if model.AppPluginCurrentRead(tx).Where(map[string]any{
		"channel_id": channel.Id, "group": candidate.Group,
		"model": candidate.PublicModel, "enabled": true,
	}).First(&ability).Error != nil ||
		!slices.Contains(channel.GetModels(), candidate.PublicModel) ||
		!slices.Contains(channel.GetGroups(), candidate.Group) {
		return channel, appAuthError("scope_denied")
	}
	apiType, ok := common.ChannelType2APIType(channel.Type)
	if !ok || channel.Type != candidate.ChannelType || apiType != candidate.APIType ||
		!validNativeAppChannelSettings(channel) {
		return channel, appAuthError("channel_changed")
	}
	actual, ok := resolveAppExecutionActualModel(channel, candidate.PublicModel)
	if !ok || actual != candidate.ActualModel {
		return channel, appAuthError("channel_changed")
	}
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(channel.Type)
	}
	if baseURL != candidate.BaseURL || !validNativeAppBaseURL(baseURL) {
		return channel, appAuthError("channel_changed")
	}
	keys := channel.GetKeys()
	if channel.ChannelInfo.IsMultiKey || len(keys) != 1 ||
		keys[0] == "" || keys[0] != channel.Key || candidate.CredentialIndex != 0 {
		return channel, appAuthError("channel_changed")
	}
	connection, err := appNativeExecutionDigest(derivationKey, "connection",
		[]any{channel.Type, apiType, baseURL, channel.OpenAIOrganization})
	if err != nil || !hmac.Equal([]byte(connection), []byte(candidate.ConnectionDigest)) {
		return channel, appAuthError("channel_changed")
	}
	credential, err := appNativeExecutionDigest(derivationKey, "credential", keys[0])
	if err != nil || !hmac.Equal([]byte(credential), []byte(candidate.CredentialDigest)) {
		return channel, appAuthError("channel_changed")
	}
	if ok, _ := model.ChannelSatisfiesFilters(&channel, candidate.PublicModel, filters); !ok {
		return channel, appAuthError("scope_denied")
	}
	return channel, nil
}

func validateAppRelayChannelTx(tx *gorm.DB, grant model.AppExecutionGrant, user model.User, candidate model.AppExecutionModel, filters []taskdto.ChannelFilter) (model.Channel, error) {
	var channel model.Channel
	if candidate.ExecutionKind != model.AppExecutionModelKindTaskBacked {
		return channel, appAuthError("scope_denied")
	}
	head, err := model.GetAppExecutionPolicyTx(tx, model.AppModelInvokePolicyKey)
	if err != nil || head.Version != grant.PolicyVersion || head.DocumentSHA256 != grant.PolicyDigest {
		return channel, appAuthError("model_policy_version_conflict")
	}
	groups, err := effectiveAppExecutionGroups(tx, user)
	if err != nil || !slices.ContainsFunc(groups, func(group appExecutionGroup) bool { return group.name == candidate.Group }) {
		return channel, appAuthError("scope_denied")
	}
	if err := model.AppPluginCurrentRead(tx).Where("id = ? AND status = ?", candidate.ChannelID, common.ChannelStatusEnabled).First(&channel).Error; err != nil {
		return channel, appAuthError("model_not_supported")
	}
	var ability model.Ability
	if model.AppPluginCurrentRead(tx).Where(map[string]any{"channel_id": channel.Id, "group": candidate.Group,
		"model": candidate.PublicModel, "enabled": true}).First(&ability).Error != nil ||
		!slices.Contains(channel.GetModels(), candidate.PublicModel) || !slices.Contains(channel.GetGroups(), candidate.Group) {
		return channel, appAuthError("scope_denied")
	}
	plugin, ok := jsplugin.DefaultRegistry.Generation().Get(candidate.PluginKey)
	if !ok || plugin.Meta.Version != candidate.PluginVersion || plugin.SourceSHA256() != candidate.PluginSHA256 {
		return channel, appAuthError("model_not_supported")
	}
	settings := channel.GetSetting()
	if channel.Type == constant.ChannelTypeTaskPlugin {
		if settings.TaskPluginKey != candidate.PluginKey {
			return channel, appAuthError("scope_denied")
		}
	} else if owner, ok := jsplugin.DefaultRegistry.Generation().GetByChannelType(channel.Type); !ok || owner != plugin ||
		(settings.TaskPluginKey != "" && settings.TaskPluginKey != candidate.PluginKey) {
		return channel, appAuthError("scope_denied")
	}
	if ok, _ := model.ChannelSatisfiesFilters(&channel, candidate.PublicModel, filters); !ok ||
		len(channel.GetParamOverride()) != 0 || len(channel.GetHeaderOverride()) != 0 {
		return channel, appAuthError("scope_denied")
	}
	mapping := map[string]string{}
	if raw := channel.GetModelMapping(); raw != "" && common.UnmarshalJsonStr(raw, &mapping) != nil {
		return channel, appAuthError("scope_denied")
	}
	actual := candidate.PublicModel
	seen := map[string]bool{}
	for mapping[actual] != "" && mapping[actual] != actual {
		if seen[actual] {
			return channel, appAuthError("scope_denied")
		}
		seen[actual] = true
		actual = mapping[actual]
	}
	if actual != candidate.ActualModel {
		return channel, appAuthError("scope_denied")
	}
	return channel, nil
}

func AppRelayErrorStatus(err error) int {
	var authErr *AppPluginAuthError
	if errors.As(err, &authErr) {
		switch authErr.Code {
		case "invalid_request":
			return http.StatusBadRequest
		case "idempotency_conflict", "execution_outcome_unknown":
			return http.StatusConflict
		case "insufficient_quota":
			return http.StatusPaymentRequired
		case "not_found":
			return http.StatusNotFound
		case "invalid_grant", "execution_grant_expired", "unauthenticated", "service_identity_invalid":
			return http.StatusUnauthorized
		}
	}
	if strings.Contains(err.Error(), "idempotency_conflict") {
		return http.StatusConflict
	}
	return http.StatusForbidden
}
