package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

func (r AppArkImportLookupRequest) validate() error {
	if err := validateAppTaskControlIdentity(r.RequestID, r.AppKey, r.AppSessionID, r.Subject); err != nil {
		return err
	}
	if !appControlOpaque(r.TaskID, 255) || !strings.HasPrefix(r.TaskID, "cgt-") || len(r.TaskID) <= 4 ||
		!appControlOpaque(r.QueryScope.ProjectID, 128) || r.QueryScope.StartAt.IsZero() ||
		!r.QueryScope.EndAt.After(r.QueryScope.StartAt) {
		return appAuthError("invalid_request")
	}
	return nil
}

func (s *AppExecutionService) LookupArkImport(ctx context.Context, identity model.AppServiceIdentity, request AppArkImportLookupRequest) (AppArkImportLookupResult, error) {
	if err := request.validate(); err != nil {
		return AppArkImportLookupResult{}, err
	}
	result := AppArkImportLookupResult{Authorization: []AppArkTaskQuery{}, Candidates: []AppArkTaskEvidence{},
		MissingFields: []string{}, Warnings: []string{}}
	err := model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		result.Authorization = []AppArkTaskQuery{}
		authority, err := s.taskAuthorityTx(tx, identity, request.AppKey, request.AppSessionID, request.Subject, []string{"task.import"}, false)
		if err != nil {
			return err
		}
		head, err := model.GetAppExecutionPolicyTx(tx, model.AppArkImportDelegationsKey)
		if err != nil {
			return classifyDBLookup(err, "not_found")
		}
		var policy AppArkImportDelegations
		if decodeAppExecutionJSON([]byte(head.CanonicalJSON), &policy) != nil {
			return appAuthError("scope_denied")
		}
		mapped := false
		for _, d := range policy.Delegations {
			if d.InstallationID != authority.session.InstallationID || d.UserID != authority.user.Id {
				continue
			}
			mapped = true
			if d.AccountRef == "" || d.ProjectID != request.QueryScope.ProjectID ||
				request.QueryScope.StartAt.Before(d.StartAt) || request.QueryScope.EndAt.After(d.EndAt) {
				continue
			}
			query := AppArkTaskQuery{TaskID: request.TaskID, AccountRef: d.AccountRef, ProjectID: d.ProjectID,
				StartAt: request.QueryScope.StartAt.UTC(), EndAt: request.QueryScope.EndAt.UTC()}
			if !slices.Contains(result.Authorization, query) {
				result.Authorization = append(result.Authorization, query)
			}
		}
		if len(result.Authorization) == 0 {
			if mapped {
				return appAuthError("scope_denied")
			}
			return appAuthError("not_found")
		}
		result.PolicyVersion = head.Version
		return nil
	})
	if err != nil {
		return AppArkImportLookupResult{}, appExecutionBoundaryError(err)
	}
	if s.ArkSource == nil {
		return AppArkImportLookupResult{}, appAuthError("service_unavailable")
	}
	// External observation stays outside retryable database transactions.
	totalBytes := 0
	for _, query := range result.Authorization {
		candidates, err := s.ArkSource.Lookup(ctx, query)
		if err != nil {
			return AppArkImportLookupResult{}, appAuthError("service_unavailable")
		}
		if len(candidates) > 256 {
			return AppArkImportLookupResult{}, appAuthError("invalid_evidence")
		}
		for _, candidate := range candidates {
			totalBytes += len(candidate.RequestJSON) + len(candidate.TaskID) + len(candidate.AccountRef) + len(candidate.ProjectID) +
				len(candidate.Source) + len(candidate.ModelVersion) + len(candidate.GatewayVersion) + len(candidate.AdapterVersion)
			if totalBytes > 64*1024 {
				return AppArkImportLookupResult{}, appAuthError("invalid_evidence")
			}
			if candidate.TaskID != query.TaskID || candidate.AccountRef != query.AccountRef ||
				candidate.ProjectID != query.ProjectID || candidate.CreatedAt.IsZero() ||
				candidate.CreatedAt.Before(query.StartAt) || candidate.CreatedAt.After(query.EndAt) {
				continue
			}
			evidence, redacted, err := appTaskEvidenceJSON(candidate.RequestJSON)
			if err != nil {
				return AppArkImportLookupResult{}, err
			}
			if evidence == nil {
				result.MissingFields = append(result.MissingFields, fmt.Sprintf("candidates[%d].request_json", len(result.Candidates)))
				result.Warnings = append(result.Warnings, "original_input_missing")
				candidate.RequestJSON = ""
			} else {
				if _, ok := evidence.(map[string]any); !ok {
					return AppArkImportLookupResult{}, appAuthError("invalid_evidence")
				}
				raw, err := common.Marshal(evidence)
				if err != nil {
					return AppArkImportLookupResult{}, appAuthError("invalid_evidence")
				}
				candidate.RequestJSON = string(raw)
			}
			if redacted {
				result.Warnings = append(result.Warnings, "credential_fields_removed")
			}
			for _, field := range []struct {
				name  string
				value *string
			}{{"source", &candidate.Source}, {"model_version", &candidate.ModelVersion},
				{"gateway_version", &candidate.GatewayVersion}, {"adapter_version", &candidate.AdapterVersion}} {
				if *field.value != "" {
					raw, err := common.Marshal(*field.value)
					if err != nil {
						return AppArkImportLookupResult{}, appAuthError("invalid_evidence")
					}
					_, removed, err := appTaskEvidenceJSON(string(raw))
					if err != nil || !appControlOpaque(*field.value, 128) {
						return AppArkImportLookupResult{}, appAuthError("invalid_evidence")
					}
					if removed {
						*field.value = ""
						result.Warnings = append(result.Warnings, "credential_fields_removed")
					}
				}
				if *field.value == "" {
					result.MissingFields = append(result.MissingFields, fmt.Sprintf("candidates[%d].%s", len(result.Candidates), field.name))
				}
			}
			candidate.CreatedAt = candidate.CreatedAt.UTC()
			result.Candidates = append(result.Candidates, candidate)
		}
	}
	if len(result.Candidates) == 0 {
		return AppArkImportLookupResult{}, appAuthError("not_found")
	}
	if len(result.Candidates) > 1 {
		result.Warnings = append(result.Warnings, "human_selection_required")
	}
	slices.Sort(result.Warnings)
	result.Warnings = slices.Compact(result.Warnings)
	// Revocation or delegation replacement during external I/O cannot return
	// evidence under the previously authorized snapshot.
	err = model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		if _, err := s.taskAuthorityTx(tx, identity, request.AppKey, request.AppSessionID, request.Subject, []string{"task.import"}, false); err != nil {
			return err
		}
		head, err := model.GetAppExecutionPolicyTx(tx, model.AppArkImportDelegationsKey)
		if err != nil {
			return classifyDBLookup(err, "scope_denied")
		}
		if head.Version != result.PolicyVersion {
			return appAuthError("scope_denied")
		}
		return nil
	})
	if err != nil {
		return AppArkImportLookupResult{}, appExecutionBoundaryError(err)
	}
	result.EvidenceDigest, err = appPluginHash(result)
	if err != nil {
		return AppArkImportLookupResult{}, appAuthError("invalid_evidence")
	}
	return result, nil
}

// Retain original numeric literals and useful evidence while removing
// credential-bearing fields at every depth. Syntax validation precedes filtering.
func appTaskEvidenceJSON(raw string) (any, bool, error) {
	if raw == "" {
		return nil, false, nil
	}
	var valid any
	if len(raw) > 64*1024 || common.UnmarshalJsonStr(raw, &valid) != nil {
		return nil, false, appAuthError("invalid_evidence")
	}
	return sanitizeAppTaskEvidence(gjson.Parse(raw), 0)
}

func appTaskSecretField(key string) bool {
	key = strings.ToLower(strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == ' ' {
			return -1
		}
		return r
	}, key))
	return strings.Contains(key, "credential") || strings.Contains(key, "header") ||
		strings.Contains(key, "cookie") || strings.Contains(key, "privatekey") ||
		strings.Contains(key, "password") || strings.Contains(key, "secret") ||
		strings.HasSuffix(key, "token") || strings.HasSuffix(key, "apikey") ||
		slices.Contains([]string{"authorization", "auth", "apikey", "accesskey", "accesskeyid", "token",
			"accesstoken", "refreshtoken", "idtoken", "sessiontoken", "securitytoken", "granttoken", "signature"}, key)
}

func sanitizeAppTaskEvidence(value gjson.Result, depth int) (any, bool, error) {
	if depth > 64 {
		return nil, false, appAuthError("invalid_evidence")
	}
	redacted := false
	if value.IsObject() || value.IsArray() {
		object, array := map[string]any{}, []any{}
		seen := map[string]bool{}
		var failure error
		value.ForEach(func(key, child gjson.Result) bool {
			if value.IsObject() {
				if seen[key.Str] {
					failure = appAuthError("invalid_evidence")
					return false
				}
				seen[key.Str] = true
			}
			clean, removed, err := sanitizeAppTaskEvidence(child, depth+1)
			if err != nil {
				failure = err
				return false
			}
			redacted = redacted || removed
			if value.IsObject() {
				if appTaskSecretField(key.Str) {
					redacted = true
				} else {
					object[key.Str] = clean
				}
			} else {
				array = append(array, clean)
			}
			return true
		})
		if failure != nil {
			return nil, false, failure
		}
		if value.IsObject() {
			return object, redacted, nil
		}
		return array, redacted, nil
	}
	if value.Type == gjson.Null {
		return nil, false, nil
	}
	if value.Type == gjson.String {
		lower := strings.ToLower(value.Str)
		if strings.Contains(lower, "bearer ") || strings.Contains(lower, "authorization:") ||
			strings.Contains(lower, "private key-----") {
			return "[redacted]", true, nil
		}
		if u, err := url.Parse(value.Str); err == nil && u.IsAbs() {
			if u.User != nil {
				return "[redacted]", true, nil
			}
			for key := range u.Query() {
				if appTaskSecretField(key) || strings.HasPrefix(strings.ToLower(key), "x-amz-") ||
					strings.HasPrefix(strings.ToLower(key), "x-tos-") {
					return "[redacted]", true, nil
				}
			}
		}
		return value.Str, false, nil
	}
	return json.RawMessage(value.Raw), false, nil
}
