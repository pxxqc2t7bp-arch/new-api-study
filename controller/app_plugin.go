package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

const appPluginRequestBodyLimit = service.AppManifestMaxBytes + 64*1024

type appPluginInstallRequest struct {
	Manifest             json.RawMessage            `json:"manifest"`
	BaseURL              string                     `json:"base_url"`
	EnabledSurfaces      []string                   `json:"enabled_surfaces"`
	AllowedParentOrigins []string                   `json:"allowed_parent_origins"`
	AllowedOrigins       []string                   `json:"allowed_origins"`
	AllowedUserPolicy    model.AppAllowedUserPolicy `json:"allowed_user_policy"`
	NetworkPolicy        model.AppNetworkPolicy     `json:"network_policy"`
	RawEntitlementPolicy json.RawMessage            `json:"entitlement_policy"`
}

type appPluginPatchRequest struct {
	InstallationID string                     `json:"installation_id"`
	Revision       int64                      `json:"revision"`
	Changes        map[string]json.RawMessage `json:"changes"`
}

type appPluginEntitlementInput struct {
	ID    string              `json:"id"`
	Key   string              `json:"key"`
	Rules map[string][]string `json:"rules"`
}

type appPluginEntitlementSelection struct {
	reference string
	draft     *service.AppEntitlementPolicyDraft
}

type appPluginPatchValues struct {
	baseURL           *string
	enabledSurfaces   *[]string
	parentOrigins     *[]string
	allowedOrigins    *[]string
	allowedUserPolicy *model.AppAllowedUserPolicy
	networkPolicy     *model.AppNetworkPolicy
	entitlementPolicy *appPluginEntitlementSelection
	status            *string
}

type appPluginNavigationItem struct {
	Key             string                  `json:"key"`
	Name            service.AppManifestName `json:"name"`
	Version         string                  `json:"version"`
	EnabledSurfaces []string                `json:"enabled_surfaces"`
	DashboardPath   string                  `json:"dashboard_path"`
	DirectURL       string                  `json:"direct_url"`
	GrantedScopes   []string                `json:"granted_scopes"`
}

type appPluginAPIError struct {
	status int
	code   string
	path   string
}

func (e *appPluginAPIError) Error() string {
	return e.code
}

// ListAppPlugins returns the enabled App Plugin navigation visible to the
// authenticated dashboard user's group.
func ListAppPlugins(c *gin.Context) {
	if !appPluginFeatureEnabled(c) {
		return
	}

	var installations []model.AppInstallation
	if err := model.DB.WithContext(c.Request.Context()).
		Where("status = ?", model.AppInstallationStatusEnabled).
		Order("app_key").
		Find(&installations).Error; err != nil {
		writeAppPluginError(c, http.StatusServiceUnavailable, "service_unavailable", "")
		return
	}

	userGroup := c.GetString("group")
	items := make([]appPluginNavigationItem, 0, len(installations))
	for _, installation := range installations {
		if !appPluginUserPolicyAllows(installation.AllowedUserPolicy, userGroup) {
			continue
		}
		surfaces := make([]string, 0, len(installation.EnabledSurfaces))
		for _, surface := range installation.EnabledSurfaces {
			if appPluginEntryAllowed(installation.AppKey, surface) {
				surfaces = append(surfaces, surface)
			}
		}
		if len(surfaces) == 0 {
			continue
		}
		var version model.AppVersion
		if err := model.DB.WithContext(c.Request.Context()).
			Where("id = ?", installation.AppVersionID).
			First(&version).Error; err != nil {
			writeAppPluginError(c, http.StatusServiceUnavailable, "service_unavailable", "")
			return
		}
		manifest, err := service.ValidateAppManifest([]byte(version.CanonicalManifestJSON))
		if err != nil {
			writeAppPluginError(c, http.StatusServiceUnavailable, "service_unavailable", "")
			return
		}
		directURL := ""
		if slices.Contains(surfaces, "direct") {
			var err error
			directURL, err = appPluginEndpoint(installation.BaseURL, manifest.Surfaces.Direct.StartPath)
			if err != nil {
				writeAppPluginError(c, http.StatusServiceUnavailable, "service_unavailable", "")
				return
			}
		}
		items = append(items, appPluginNavigationItem{
			Key:             installation.AppKey,
			Name:            manifest.Name,
			Version:         installation.ManifestVersion,
			EnabledSurfaces: surfaces,
			DashboardPath:   "/apps/" + installation.AppKey,
			DirectURL:       directURL,
			GrantedScopes:   appPluginStrings(manifest.RequestedScopes),
		})
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items})
}

// ListAppPluginInstallations returns host-owned installation metadata without
// manifest bodies or credential material.
func ListAppPluginInstallations(c *gin.Context) {
	if !appPluginFeatureEnabled(c) {
		return
	}

	limit := 50
	if rawLimit := c.Query("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > 100 {
			writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "/limit")
			return
		}
		limit = parsed
	}
	status := c.Query("status")
	if status != "" &&
		status != model.AppInstallationStatusDisabled &&
		status != model.AppInstallationStatusEnabled &&
		status != model.AppInstallationStatusRevoked {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "/status")
		return
	}

	query := model.DB.WithContext(c.Request.Context()).Model(&model.AppInstallation{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if key := c.Query("key"); key != "" {
		query = query.Where("app_key = ?", key)
	}
	if cursor := c.Query("cursor"); cursor != "" {
		query = query.Where("installation_id > ?", cursor)
	}
	var installations []model.AppInstallation
	if err := query.Order("installation_id").Limit(limit + 1).Find(&installations).Error; err != nil {
		writeAppPluginError(c, http.StatusServiceUnavailable, "service_unavailable", "")
		return
	}

	nextCursor := ""
	if len(installations) > limit {
		nextCursor = installations[limit-1].InstallationID
		installations = installations[:limit]
	}
	items := make([]model.AppInstallResult, 0, len(installations))
	for _, installation := range installations {
		item, err := appPluginInstallationResult(model.DB.WithContext(c.Request.Context()), installation)
		if err != nil {
			writeAppPluginError(c, http.StatusServiceUnavailable, "service_unavailable", "")
			return
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"items":       items,
			"next_cursor": nextCursor,
		},
	})
}

// CreateAppPluginInstallation validates and installs one immutable manifest
// generation. Service credentials are intentionally not accepted here.
func CreateAppPluginInstallation(c *gin.Context) {
	middleware.SetAppPluginAuditTarget(c, "", 0)
	if !appPluginFeatureEnabled(c) {
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 255 {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "/headers/Idempotency-Key")
		return
	}

	var request appPluginInstallRequest
	fields, err := decodeAppPluginObject(c, map[string]struct{}{
		"manifest":               {},
		"base_url":               {},
		"enabled_surfaces":       {},
		"allowed_parent_origins": {},
		"allowed_origins":        {},
		"allowed_user_policy":    {},
		"network_policy":         {},
		"entitlement_policy":     {},
	}, &request)
	if err != nil {
		writeAppPluginRequestError(c, err)
		return
	}
	if len(request.Manifest) == 0 || strings.TrimSpace(request.BaseURL) == "" {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return
	}
	role := c.GetInt("role")
	if role < common.RoleRootUser {
		for _, field := range []string{"enabled_surfaces", "allowed_parent_origins", "network_policy"} {
			if _, exists := fields[field]; exists {
				writeAppPluginError(c, http.StatusForbidden, "forbidden", "/"+field)
				return
			}
		}
	}
	if err := normalizeAppPluginInstallRequest(&request); err != nil {
		writeAppPluginRequestError(c, err)
		return
	}

	entitlement, err := parseAppPluginEntitlement(request.RawEntitlementPolicy)
	if err != nil {
		writeAppPluginRequestError(c, err)
		return
	}
	if role < common.RoleRootUser && entitlement.draft != nil {
		writeAppPluginError(c, http.StatusForbidden, "forbidden", "/entitlement_policy")
		return
	}

	result, err := service.NewAppPluginInstallationService(model.DB, service.AppPluginInstallationOptions{
		CurrentAuthz: appPluginCurrentAuthz(),
	}).Install(c.Request.Context(), service.AppInstallCommand{
		DisallowUpgrade:        role < common.RoleRootUser,
		ActorID:                int64(c.GetInt("id")),
		IdempotencyKey:         idempotencyKey,
		ManifestJSON:           append([]byte(nil), request.Manifest...),
		BaseURL:                request.BaseURL,
		EnabledSurfaces:        request.EnabledSurfaces,
		AllowedParentOrigins:   request.AllowedParentOrigins,
		AllowedOrigins:         request.AllowedOrigins,
		AllowedUserPolicy:      request.AllowedUserPolicy,
		NetworkPolicy:          request.NetworkPolicy,
		EntitlementPolicyID:    entitlement.reference,
		EntitlementPolicyDraft: entitlement.draft,
	})
	if err != nil {
		writeAppPluginServiceError(c, err)
		return
	}

	middleware.SetAppPluginAuditTarget(c, result.InstallationID, result.Revision)
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": result})
}

// PatchAppPluginInstallation applies one CAS update with enable prerequisites
// checked against the locked revision.
func PatchAppPluginInstallation(c *gin.Context) {
	middleware.SetAppPluginAuditTarget(c, "", 0)
	if !appPluginFeatureEnabled(c) {
		return
	}

	var request appPluginPatchRequest
	_, err := decodeAppPluginObject(c, map[string]struct{}{
		"installation_id": {},
		"revision":        {},
		"changes":         {},
	}, &request)
	if err != nil {
		writeAppPluginRequestError(c, err)
		return
	}
	middleware.SetAppPluginAuditTarget(c, request.InstallationID, request.Revision)
	if request.InstallationID == "" || request.Revision <= 0 || len(request.Changes) == 0 {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return
	}

	values, err := parseAppPluginPatchValues(request.Changes)
	if err != nil {
		writeAppPluginRequestError(c, err)
		return
	}
	role := c.GetInt("role")
	if role < common.RoleRootUser {
		if values.enabledSurfaces != nil {
			writeAppPluginError(c, http.StatusForbidden, "forbidden", "/changes/enabled_surfaces")
			return
		}
		if values.parentOrigins != nil {
			writeAppPluginError(c, http.StatusForbidden, "forbidden", "/changes/allowed_parent_origins")
			return
		}
		if values.entitlementPolicy != nil && values.entitlementPolicy.draft != nil {
			writeAppPluginError(c, http.StatusForbidden, "forbidden", "/changes/entitlement_policy")
			return
		}
		if values.status != nil && *values.status == model.AppInstallationStatusRevoked {
			writeAppPluginError(c, http.StatusForbidden, "forbidden", "/changes/status")
			return
		}
	}

	var updated model.AppInstallResult
	err = model.MutateAppInstallation(c.Request.Context(), model.DB, request.InstallationID, request.Revision, func(tx *gorm.DB, current model.AppInstallation) error {
		updated = model.AppInstallResult{}
		if values.status != nil {
			switch *values.status {
			case model.AppInstallationStatusEnabled:
				if current.Status != model.AppInstallationStatusDisabled || len(request.Changes) != 1 {
					return &appPluginAPIError{status: http.StatusConflict, code: "invalid_state_transition", path: "/changes/status"}
				}
				if err := validateAppPluginEnable(c, tx, current); err != nil {
					return err
				}
			case model.AppInstallationStatusDisabled:
				if current.Status == model.AppInstallationStatusDisabled && len(request.Changes) == 1 {
					return &appPluginAPIError{status: http.StatusConflict, code: "invalid_state_transition", path: "/changes/status"}
				}
			case model.AppInstallationStatusRevoked:
				if role < common.RoleRootUser {
					return &appPluginAPIError{status: http.StatusForbidden, code: "forbidden", path: "/changes/status"}
				}
			default:
				return &appPluginAPIError{status: http.StatusConflict, code: "invalid_state_transition", path: "/changes/status"}
			}
		}
		hasConfigurationChanges := values.status == nil || len(request.Changes) > 1
		if current.Status != model.AppInstallationStatusDisabled && hasConfigurationChanges {
			return &appPluginAPIError{status: http.StatusConflict, code: "invalid_state_transition", path: "/changes/status"}
		}
		if role < common.RoleRootUser && values.networkPolicy != nil &&
			!appPluginNetworkPolicyTightens(current.NetworkPolicy, *values.networkPolicy) {
			return &appPluginAPIError{status: http.StatusForbidden, code: "forbidden", path: "/changes/network_policy"}
		}

		updates := map[string]any{}
		if values.baseURL != nil {
			updates["base_url"] = *values.baseURL
		}
		if values.enabledSurfaces != nil {
			updates["enabled_surfaces"] = model.AppStringList(*values.enabledSurfaces)
		}
		if values.parentOrigins != nil {
			updates["allowed_parent_origins"] = model.AppStringList(*values.parentOrigins)
		}
		if values.allowedOrigins != nil {
			updates["allowed_origins"] = model.AppStringList(*values.allowedOrigins)
		}
		if values.allowedUserPolicy != nil {
			updates["allowed_user_policy"] = *values.allowedUserPolicy
		}
		if values.networkPolicy != nil {
			updates["network_policy"] = *values.networkPolicy
		}
		if values.entitlementPolicy != nil {
			entitlementID, resolveErr := resolveAppPluginEntitlement(c, tx, *values.entitlementPolicy)
			if resolveErr != nil {
				return resolveErr
			}
			updates["entitlement_policy_id"] = entitlementID
		}
		if values.status != nil {
			updates["status"] = *values.status
		}

		if values.baseURL != nil {
			if err := updateAppPluginRouteClaims(tx, current, *values.baseURL); err != nil {
				return err
			}
		}
		if values.status != nil && *values.status == model.AppInstallationStatusRevoked {
			if err := tx.Where("installation_id = ?", current.InstallationID).Delete(&model.AppRouteClaim{}).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.AppServiceCredential{}).
				Where("installation_id = ? AND status <> ?", current.InstallationID, model.AppInstallationStatusRevoked).
				Update("status", model.AppInstallationStatusRevoked).Error; err != nil {
				return err
			}
		}

		updates["revision"] = gorm.Expr("revision + ?", 1)
		update := tx.Model(&model.AppInstallation{}).
			Where("installation_id = ? AND revision = ? AND status <> ?",
				current.InstallationID,
				request.Revision,
				model.AppInstallationStatusRevoked,
			).
			Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return model.ErrAppInstallationRevisionConflict
		}
		if err := tx.Where("installation_id = ?", current.InstallationID).First(&current).Error; err != nil {
			return err
		}
		updated, err = appPluginInstallationResult(tx, current)
		return err
	})
	if err != nil {
		writeAppPluginServiceError(c, err)
		return
	}

	middleware.SetAppPluginAuditTarget(c, updated.InstallationID, updated.Revision)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": updated})
}

func decodeAppPluginObject(c *gin.Context, allowed map[string]struct{}, target any) (map[string]json.RawMessage, error) {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, appPluginRequestBodyLimit)
	data, err := io.ReadAll(reader)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return nil, &appPluginAPIError{status: http.StatusRequestEntityTooLarge, code: "payload_too_large"}
		}
		return nil, &appPluginAPIError{status: http.StatusBadRequest, code: "invalid_request"}
	}
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(data)) == 0 || common.Unmarshal(data, &fields) != nil || fields == nil {
		return nil, &appPluginAPIError{status: http.StatusBadRequest, code: "invalid_request"}
	}
	for field := range fields {
		if _, ok := allowed[field]; !ok {
			status := http.StatusBadRequest
			code := "invalid_request"
			normalized := strings.ToLower(strings.ReplaceAll(field, "_", ""))
			if strings.Contains(normalized, "secret") || strings.Contains(normalized, "credential") || strings.Contains(normalized, "token") {
				status = http.StatusForbidden
				code = "forbidden"
			}
			return nil, &appPluginAPIError{status: status, code: code, path: "/" + field}
		}
	}
	if err := common.Unmarshal(data, target); err != nil {
		return nil, &appPluginAPIError{status: http.StatusBadRequest, code: "invalid_request"}
	}
	return fields, nil
}

func normalizeAppPluginInstallRequest(request *appPluginInstallRequest) error {
	baseURL, err := normalizeAppPluginBaseURL(request.BaseURL)
	if err != nil {
		return err
	}
	request.BaseURL = baseURL
	request.EnabledSurfaces = normalizeAppPluginStrings(request.EnabledSurfaces)
	if slices.ContainsFunc(request.EnabledSurfaces, func(surface string) bool {
		return surface != "direct" && surface != "embedded"
	}) {
		return &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/enabled_surfaces"}
	}
	request.AllowedParentOrigins, err = normalizeAppPluginOrigins(request.AllowedParentOrigins)
	if err != nil {
		return &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/allowed_parent_origins"}
	}
	request.AllowedOrigins, err = normalizeAppPluginOrigins(request.AllowedOrigins)
	if err != nil {
		return &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/allowed_origins"}
	}
	if slices.ContainsFunc(request.AllowedUserPolicy.Groups, func(group string) bool {
		return group == "" || strings.TrimSpace(group) != group
	}) {
		return &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/allowed_user_policy"}
	}
	request.AllowedUserPolicy.Groups = normalizeAppPluginStrings(request.AllowedUserPolicy.Groups)
	if slices.ContainsFunc(request.NetworkPolicy.AllowHosts, func(host string) bool {
		return !validAppPluginHost(host)
	}) {
		return &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/network_policy"}
	}
	request.NetworkPolicy.AllowHosts = normalizeAppPluginStrings(request.NetworkPolicy.AllowHosts)
	return nil
}

func parseAppPluginPatchValues(changes map[string]json.RawMessage) (appPluginPatchValues, error) {
	values := appPluginPatchValues{}
	for field, raw := range changes {
		switch field {
		case "base_url":
			var value string
			if common.Unmarshal(raw, &value) != nil {
				return values, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/changes/base_url"}
			}
			normalized, err := normalizeAppPluginBaseURL(value)
			if err != nil {
				return values, err
			}
			values.baseURL = &normalized
		case "enabled_surfaces":
			var value []string
			if common.Unmarshal(raw, &value) != nil {
				return values, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/changes/enabled_surfaces"}
			}
			value = normalizeAppPluginStrings(value)
			if slices.ContainsFunc(value, func(surface string) bool {
				return surface != "direct" && surface != "embedded"
			}) {
				return values, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/changes/enabled_surfaces"}
			}
			values.enabledSurfaces = &value
		case "allowed_parent_origins":
			value, err := decodeAndNormalizeAppPluginOrigins(raw, "/changes/allowed_parent_origins")
			if err != nil {
				return values, err
			}
			values.parentOrigins = &value
		case "allowed_origins":
			value, err := decodeAndNormalizeAppPluginOrigins(raw, "/changes/allowed_origins")
			if err != nil {
				return values, err
			}
			values.allowedOrigins = &value
		case "allowed_user_policy":
			var value model.AppAllowedUserPolicy
			if common.Unmarshal(raw, &value) != nil {
				return values, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/changes/allowed_user_policy"}
			}
			value.Groups = normalizeAppPluginStrings(value.Groups)
			if slices.ContainsFunc(value.Groups, func(group string) bool { return strings.TrimSpace(group) != group || group == "" }) {
				return values, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/changes/allowed_user_policy"}
			}
			values.allowedUserPolicy = &value
		case "network_policy":
			var value model.AppNetworkPolicy
			if common.Unmarshal(raw, &value) != nil {
				return values, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/changes/network_policy"}
			}
			value.AllowHosts = normalizeAppPluginStrings(value.AllowHosts)
			if slices.ContainsFunc(value.AllowHosts, func(host string) bool { return !validAppPluginHost(host) }) {
				return values, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/changes/network_policy"}
			}
			values.networkPolicy = &value
		case "entitlement_policy":
			value, err := parseAppPluginEntitlement(raw)
			if err != nil {
				return values, err
			}
			values.entitlementPolicy = &value
		case "status":
			var value string
			if common.Unmarshal(raw, &value) != nil {
				return values, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/changes/status"}
			}
			values.status = &value
		default:
			status := http.StatusBadRequest
			code := "invalid_request"
			normalized := strings.ToLower(strings.ReplaceAll(field, "_", ""))
			if strings.Contains(normalized, "secret") || strings.Contains(normalized, "credential") || strings.Contains(normalized, "token") {
				status = http.StatusForbidden
				code = "forbidden"
			}
			return values, &appPluginAPIError{status: status, code: code, path: "/changes/" + field}
		}
	}
	return values, nil
}

func parseAppPluginEntitlement(raw json.RawMessage) (appPluginEntitlementSelection, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return appPluginEntitlementSelection{}, nil
	}
	var reference string
	if common.Unmarshal(raw, &reference) == nil {
		return appPluginEntitlementSelection{reference: strings.TrimSpace(reference)}, nil
	}
	var fields map[string]json.RawMessage
	if common.Unmarshal(raw, &fields) != nil || fields == nil {
		return appPluginEntitlementSelection{}, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/entitlement_policy"}
	}
	for field := range fields {
		if field != "id" && field != "key" && field != "rules" {
			return appPluginEntitlementSelection{}, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/entitlement_policy/" + field}
		}
	}
	var input appPluginEntitlementInput
	if common.Unmarshal(raw, &input) != nil {
		return appPluginEntitlementSelection{}, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/entitlement_policy"}
	}
	if input.ID != "" && input.Key == "" && input.Rules == nil {
		return appPluginEntitlementSelection{reference: strings.TrimSpace(input.ID)}, nil
	}
	if input.ID == "" && strings.TrimSpace(input.Key) != "" && input.Rules != nil {
		return appPluginEntitlementSelection{draft: &service.AppEntitlementPolicyDraft{
			Key:   strings.TrimSpace(input.Key),
			Rules: input.Rules,
		}}, nil
	}
	return appPluginEntitlementSelection{}, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/entitlement_policy"}
}

func resolveAppPluginEntitlement(c *gin.Context, tx *gorm.DB, selection appPluginEntitlementSelection) (string, error) {
	if selection.draft != nil {
		if c.GetInt("role") < common.RoleRootUser {
			return "", &appPluginAPIError{status: http.StatusForbidden, code: "forbidden", path: "/entitlement_policy"}
		}
		result, err := service.NewAppPluginInstallationService(tx, service.AppPluginInstallationOptions{
			CurrentAuthz: appPluginCurrentAuthz(),
		}).CreateEntitlementPolicy(c.Request.Context(), *selection.draft)
		if err != nil {
			return "", err
		}
		return result.ID, nil
	}
	if selection.reference == "" {
		return "", nil
	}
	var count int64
	if err := tx.Model(&model.AppEntitlementPolicy{}).
		Where("id = ?", selection.reference).
		Count(&count).Error; err != nil {
		return "", err
	}
	if count != 1 {
		return "", &appPluginAPIError{status: http.StatusNotFound, code: "not_found", path: "/entitlement_policy"}
	}
	return selection.reference, nil
}

func appPluginCurrentAuthz() map[string][]string {
	rules := map[string][]string{}
	for _, permission := range authz.AllPermissions() {
		rules[permission.Resource] = append(rules[permission.Resource], permission.Action)
	}
	return rules
}

func updateAppPluginRouteClaims(tx *gorm.DB, installation model.AppInstallation, baseURL string) error {
	version, err := model.GetAppInstallationVersion(tx, installation)
	if err != nil {
		return err
	}
	manifest, err := service.ValidateAppManifest([]byte(version.CanonicalManifestJSON))
	if err != nil {
		return err
	}
	callback, err := appPluginEndpoint(baseURL, manifest.CallbackPath)
	if err != nil {
		return err
	}
	direct, err := appPluginEndpoint(baseURL, manifest.Surfaces.Direct.StartPath)
	if err != nil {
		return err
	}
	embedded, err := appPluginEndpoint(baseURL, manifest.Surfaces.Embedded.StartPath)
	if err != nil {
		return err
	}
	return model.UpdateAppRouteClaims(tx, installation, model.AppRouteEndpoints{
		Callback: callback, Direct: direct, Embedded: embedded,
	})
}

func appPluginInstallationResult(db *gorm.DB, installation model.AppInstallation) (model.AppInstallResult, error) {
	credential := model.AppCredentialMeta{}
	var stored model.AppServiceCredential
	query := db.Where("installation_id = ?", installation.InstallationID).
		Order("id DESC").
		Limit(1).
		Find(&stored)
	if query.Error != nil {
		return model.AppInstallResult{}, query.Error
	}
	if query.RowsAffected == 1 {
		credential = model.AppCredentialMeta{
			CredentialID: stored.CredentialID,
			Version:      stored.CredentialVersion,
			Status:       stored.Status,
			ExpiresAt:    stored.ExpiresAt,
		}
	}
	return model.AppInstallResult{
		AppVersionID:             installation.AppVersionID,
		InstallationID:           installation.InstallationID,
		AppKey:                   installation.AppKey,
		ManifestVersion:          installation.ManifestVersion,
		ManifestSHA256:           installation.ManifestSHA256,
		BaseURL:                  installation.BaseURL,
		EnabledSurfaces:          appPluginStrings(installation.EnabledSurfaces),
		AllowedParentOrigins:     appPluginStrings(installation.AllowedParentOrigins),
		ServiceCredentialSet:     credential,
		AllowedOrigins:           appPluginStrings(installation.AllowedOrigins),
		AllowedUserPolicy:        installation.AllowedUserPolicy,
		NetworkPolicy:            installation.NetworkPolicy,
		EntitlementPolicyVersion: installation.EntitlementPolicyID,
		Status:                   installation.Status,
		Revision:                 installation.Revision,
		CreatedAt:                installation.CreatedAt,
		UpdatedAt:                installation.UpdatedAt,
	}, nil
}

func appPluginUserPolicyAllows(policy model.AppAllowedUserPolicy, group string) bool {
	return len(policy.Groups) == 0 || slices.Contains(policy.Groups, group)
}

func appPluginNetworkPolicyTightens(current, next model.AppNetworkPolicy) bool {
	if current.DenyPrivateIPRanges && !next.DenyPrivateIPRanges {
		return false
	}
	for _, host := range next.AllowHosts {
		if !slices.Contains(current.AllowHosts, host) {
			return false
		}
	}
	return true
}

func decodeAndNormalizeAppPluginOrigins(raw json.RawMessage, fieldPath string) ([]string, error) {
	var values []string
	if common.Unmarshal(raw, &values) != nil {
		return nil, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: fieldPath}
	}
	normalized, err := normalizeAppPluginOrigins(values)
	if err != nil {
		return nil, &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: fieldPath}
	}
	return normalized, nil
}

func normalizeAppPluginBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil {
		return "", &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/base_url"}
	}
	parsed.Scheme = "https"
	if err := normalizeAppPluginHost(parsed); err != nil {
		return "", &appPluginAPIError{status: http.StatusUnprocessableEntity, code: "validation_error", path: "/base_url"}
	}
	parsed.Path = path.Clean("/" + parsed.Path)
	if !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func normalizeAppPluginOrigins(values []string) ([]string, error) {
	origins := make([]string, 0, len(values))
	for _, value := range values {
		parsed, err := url.Parse(value)
		if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil {
			return nil, errors.New("invalid origin")
		}
		parsed.Scheme = "https"
		if err := normalizeAppPluginHost(parsed); err != nil {
			return nil, err
		}
		parsed.Path = ""
		parsed.RawPath = ""
		parsed.RawQuery = ""
		parsed.Fragment = ""
		origins = append(origins, parsed.String())
	}
	return normalizeAppPluginStrings(origins), nil
}

func normalizeAppPluginHost(parsed *url.URL) error {
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return errors.New("missing host")
	}
	port := parsed.Port()
	if port == "443" {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	return nil
}

func normalizeAppPluginStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func appPluginStrings(values []string) []string {
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func validAppPluginHost(host string) bool {
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/?#@") {
		return false
	}
	parsed, err := url.Parse("https://" + host)
	return err == nil && parsed.Host == host && parsed.Hostname() != ""
}

func appPluginEndpoint(baseURL, manifestPath string) (string, error) {
	normalized, err := normalizeAppPluginBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimSuffix(parsed.Path, "/")
	parsed.Path = path.Clean(basePath + "/" + strings.TrimPrefix(manifestPath, "/"))
	return parsed.String(), nil
}

func appPluginFeatureEnabled(c *gin.Context) bool {
	if operation_setting.AppPluginV1Enabled {
		return true
	}
	writeAppPluginError(c, http.StatusForbidden, "app_plugin_disabled", "")
	return false
}

// Entry gates stop new work without changing existing Task/session records.
// Installation, user and surface authorization remain separate service checks.
func appPluginEntryAllowed(appKey, surface string) bool {
	return operation_setting.AppPluginV1Enabled &&
		(appKey != "seedance-repro" || operation_setting.AppPluginSeedanceEnabled) &&
		(surface != "embedded" || operation_setting.AppPluginEmbeddedSurfaceEnabled)
}

func writeAppPluginRequestError(c *gin.Context, err error) {
	var apiErr *appPluginAPIError
	if errors.As(err, &apiErr) {
		writeAppPluginError(c, apiErr.status, apiErr.code, apiErr.path)
		return
	}
	writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
}

func writeAppPluginServiceError(c *gin.Context, err error) {
	var authErr *service.AppPluginAuthError
	if errors.As(err, &authErr) {
		writeAppPluginError(c, appPluginAuthStatus(authErr.Code), authErr.Code, "")
		return
	}
	var apiErr *appPluginAPIError
	if errors.As(err, &apiErr) {
		writeAppPluginError(c, apiErr.status, apiErr.code, apiErr.path)
		return
	}
	var manifestErr *service.AppManifestError
	if errors.As(err, &manifestErr) {
		writeAppPluginError(c, http.StatusUnprocessableEntity, manifestErr.Code, "/manifest")
		return
	}
	switch {
	case errors.Is(err, model.ErrAppEntitlementPolicyNotFound):
		writeAppPluginError(c, http.StatusNotFound, "not_found", "/entitlement_policy")
	case errors.Is(err, model.ErrAppInstallationUpgradeForbidden):
		writeAppPluginError(c, http.StatusForbidden, "forbidden", "/manifest")
	case errors.Is(err, service.ErrManifestCannotOwnEntitlementPolicy),
		errors.Is(err, service.ErrManifestCannotOwnHostPolicy):
		writeAppPluginError(c, http.StatusUnprocessableEntity, service.AppManifestForbiddenFieldErrorCode, "/manifest")
	case errors.Is(err, model.ErrAppIdempotencyConflict):
		writeAppPluginError(c, http.StatusConflict, "idempotency_conflict", "")
	case errors.Is(err, model.ErrAppVersionConflict):
		writeAppPluginError(c, http.StatusConflict, "app_version_conflict", "")
	case errors.Is(err, model.ErrAppRouteClaimConflict):
		writeAppPluginError(c, http.StatusConflict, "app_route_collision", "")
	case errors.Is(err, model.ErrAppInstallationRevisionConflict):
		writeAppPluginError(c, http.StatusConflict, "version_conflict", "")
	case errors.Is(err, model.ErrAppInstallationRevoked),
		errors.Is(err, model.ErrAppInstallationStatusInvalid):
		writeAppPluginError(c, http.StatusConflict, "invalid_state_transition", "")
	case errors.Is(err, model.ErrAppInstallRequestInvalid):
		writeAppPluginError(c, http.StatusUnprocessableEntity, "validation_error", "")
	case errors.Is(err, gorm.ErrRecordNotFound):
		writeAppPluginError(c, http.StatusNotFound, "not_found", "")
	default:
		switch err.Error() {
		case "not_found", "service_identity_invalid", "scope_denied", "invalid_request", "invalid_state_transition":
			writeAppPluginError(c, appPluginAuthStatus(err.Error()), err.Error(), "")
		default:
			writeAppPluginError(c, http.StatusServiceUnavailable, "service_unavailable", "")
		}
	}
}

func writeAppPluginError(c *gin.Context, status int, code, fieldPath string) {
	requestID := c.GetString(common.RequestIdKey)
	if requestID == "" {
		requestID = common.NewRequestId()
		c.Set(common.RequestIdKey, requestID)
		c.Header(common.RequestIdKey, requestID)
	}
	fieldErrors := make([]gin.H, 0, 1)
	if fieldPath != "" {
		fieldErrors = append(fieldErrors, gin.H{"path": fieldPath, "code": "invalid"})
	}
	message := map[string]string{
		"unauthenticated":                 "Authentication required",
		"identity_inactive":               "Identity is inactive",
		"identity_not_configured":         "App identity is not configured",
		"service_identity_invalid":        "App service identity is invalid",
		"scope_denied":                    "App permission denied",
		"model_policy_denied":             "Model policy denied",
		"price_version_unavailable":       "Model pricing is unavailable",
		"launch_code_expired":             "Launch code expired",
		"launch_code_replayed":            "Launch code already consumed",
		"state_mismatch":                  "Launch state does not match",
		"nonce_mismatch":                  "Launch nonce does not match",
		"pkce_verification_failed":        "Launch verification failed",
		"approval_required":               "Fresh operation verification required",
		"app_plugin_disabled":             "App plugin API is disabled",
		"invalid_request":                 "Invalid app plugin request",
		"forbidden":                       "App plugin request is forbidden",
		"not_found":                       "App plugin not found",
		"app_plugin_not_found":            "App plugin not found",
		"version_conflict":                "App plugin revision conflict",
		"idempotency_conflict":            "App plugin idempotency conflict",
		"app_version_conflict":            "App plugin version conflict",
		"app_route_collision":             "App plugin route collision",
		"app_enable_prerequisite_missing": "App plugin enable prerequisites are missing",
		"invalid_state_transition":        "Invalid app plugin state transition",
		"payload_too_large":               "App plugin request is too large",
		"validation_error":                "App plugin validation failed",
		"app_manifest_invalid":            "App plugin manifest is invalid",
		"app_manifest_forbidden_field":    "App plugin manifest contains a forbidden field",
		"service_unavailable":             "App plugin request failed",
	}[code]
	if message == "" {
		status = http.StatusServiceUnavailable
		code = "service_unavailable"
		message = "App plugin request failed"
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code":         code,
		"message":      message,
		"field_errors": fieldErrors,
		"retryable":    status >= http.StatusInternalServerError,
		"request_id":   requestID,
	}})
}

var appPluginEnableNetworkProbe = service.ProbeAppPluginNetwork

func validateAppPluginEnable(c *gin.Context, tx *gorm.DB, installation model.AppInstallation) error {
	missing := &appPluginAPIError{status: http.StatusConflict, code: "app_enable_prerequisite_missing", path: "/changes/status"}
	manifest, _, err := service.ValidateAppPluginRegistration(tx, installation)
	if err != nil || len(installation.EnabledSurfaces) == 0 {
		return missing
	}
	for _, surface := range installation.EnabledSurfaces {
		if surface != "direct" && surface != "embedded" {
			return missing
		}
	}
	if slices.Contains(installation.EnabledSurfaces, "embedded") {
		if len(installation.AllowedParentOrigins) == 0 {
			return missing
		}
		for _, origin := range installation.AllowedParentOrigins {
			if !service.AppPluginSameSiteHTTPS(installation.BaseURL, origin) ||
				!service.AppPluginTrustedOrigin(system_setting.ServerAddress, origin) {
				return missing
			}
		}
	}
	scopes, err := model.AppPluginApprovedScopes(tx, installation, time.Now())
	if err != nil || !slices.Contains(scopes, "identity.read") {
		return missing
	}
	for _, scope := range manifest.RequestedScopes {
		if !slices.Contains(scopes, scope) {
			return missing
		}
	}
	if err := appPluginEnableNetworkProbe(c.Request.Context(), installation); err != nil {
		return missing
	}
	return nil
}

func appPluginAuthStatus(code string) int {
	switch code {
	case "invalid_request", "state_mismatch", "nonce_mismatch", "pkce_verification_failed":
		return http.StatusBadRequest
	case "unauthenticated", "service_identity_invalid", "launch_code_expired", "launch_code_replayed":
		return http.StatusUnauthorized
	case "identity_inactive", "app_plugin_disabled", "scope_denied", "forbidden", "approval_required":
		return http.StatusForbidden
	case "not_found":
		return http.StatusNotFound
	case "idempotency_conflict", "invalid_state_transition":
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

func appPluginSensitiveResponse(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
}

func appPluginBrowserOrigin(c *gin.Context) bool {
	if c.Request.TLS == nil || len(c.Request.Header.Values("Origin")) != 1 ||
		!service.AppPluginTrustedOrigin(system_setting.ServerAddress, c.GetHeader("Origin")) {
		writeAppPluginError(c, http.StatusForbidden, "forbidden", "")
		return false
	}
	return true
}

// Decode with the common codec first. GJSON only visits the validated structure
// to detect duplicate names (including escaped equivalents) before binding it.
func decodeAppPluginAuthObject(c *gin.Context, required []string, target any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024))
	if err != nil {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return false
	}
	var fields map[string]json.RawMessage
	if common.Unmarshal(data, &fields) != nil || fields == nil || len(fields) != len(required) ||
		!appPluginUniqueJSONFields(gjson.ParseBytes(data)) {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return false
	}
	for _, field := range required {
		value, exists := fields[field]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
			return false
		}
	}
	if common.Unmarshal(data, target) != nil {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return false
	}
	return true
}

func appPluginUniqueJSONFields(value gjson.Result) bool {
	if !value.IsObject() && !value.IsArray() {
		return true
	}
	seen := map[string]bool{}
	valid := true
	value.ForEach(func(key, child gjson.Result) bool {
		if value.IsObject() && seen[key.Str] {
			valid = false
			return false
		}
		seen[key.Str] = true
		valid = appPluginUniqueJSONFields(child)
		return valid
	})
	return valid
}

func AuthorizeAppPlugin(c *gin.Context) {
	appPluginSensitiveResponse(c)
	if !appPluginFeatureEnabled(c) || !appPluginBrowserOrigin(c) {
		return
	}
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok {
		writeAppPluginError(c, http.StatusUnauthorized, "unauthenticated", "")
		return
	}
	var request service.AppPluginAuthorizeRequest
	if !decodeAppPluginAuthObject(c, []string{"surface", "transaction_id", "state", "code_challenge", "code_challenge_method", "nonce"}, &request) {
		return
	}
	if !appPluginEntryAllowed(c.Param("key"), request.Surface) {
		writeAppPluginError(c, http.StatusForbidden, "app_plugin_disabled", "")
		return
	}
	result, err := service.NewAppPluginAuthService(model.DB, service.ConfiguredAppPluginAuthOptions()).
		Authorize(c.Request.Context(), identity, c.Param("key"), c.GetHeader("Origin"), c.GetHeader("Idempotency-Key"), request)
	if err != nil {
		writeAppPluginServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func GetAppPluginLaunchContext(c *gin.Context) {
	appPluginSensitiveResponse(c)
	if !appPluginFeatureEnabled(c) {
		return
	}
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok {
		writeAppPluginError(c, http.StatusUnauthorized, "unauthenticated", "")
		return
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["surface"]) != 1 {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if !appPluginEntryAllowed(c.Param("key"), query.Get("surface")) {
		writeAppPluginError(c, http.StatusForbidden, "app_plugin_disabled", "")
		return
	}
	result, err := service.NewAppPluginAuthService(model.DB, service.ConfiguredAppPluginAuthOptions()).
		LaunchContext(c.Request.Context(), identity, c.Param("key"), query.Get("surface"))
	if err != nil {
		writeAppPluginServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func ExchangeAppPluginLaunchCode(c *gin.Context) {
	appPluginSensitiveResponse(c)
	identity, ok := middleware.GetAppServiceIdentity(c)
	if !ok {
		writeAppPluginError(c, http.StatusUnauthorized, "service_identity_invalid", "")
		return
	}
	var request service.AppPluginExchangeRequest
	if !decodeAppPluginAuthObject(c, []string{"exchange_request_id", "app_key", "surface", "transaction_id", "code", "code_verifier", "state", "nonce"}, &request) {
		return
	}
	result, err := service.NewAppPluginAuthService(model.DB, service.ConfiguredAppPluginAuthOptions()).Exchange(c.Request.Context(), identity, request)
	if err != nil {
		writeAppPluginServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func IntrospectAppPluginSession(c *gin.Context) {
	appPluginSensitiveResponse(c)
	identity, ok := middleware.GetAppServiceIdentity(c)
	if !ok {
		writeAppPluginError(c, http.StatusUnauthorized, "service_identity_invalid", "")
		return
	}
	var request service.AppPluginIntrospectRequest
	if !decodeAppPluginAuthObject(c, []string{"app_key", "app_session_id", "subject", "required_scopes", "required_entitlements"}, &request) {
		return
	}
	result, err := service.NewAppPluginAuthService(model.DB, service.ConfiguredAppPluginAuthOptions()).Introspect(c.Request.Context(), identity, request)
	if err != nil {
		writeAppPluginServiceError(c, err)
		return
	}
	if result.Active && result.ModelPolicyVersion > 0 && operation_setting.AppExecutionGrantsEnabled &&
		appPluginEntryAllowed(request.AppKey, "") {
		c.Header("X-App-Model-Policy-Version", strconv.FormatInt(result.ModelPolicyVersion, 10))
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func RevokeAppPluginSession(c *gin.Context) {
	appPluginSensitiveResponse(c)
	identity, ok := middleware.GetAppServiceIdentity(c)
	if !ok {
		writeAppPluginError(c, http.StatusUnauthorized, "service_identity_invalid", "")
		return
	}
	var request service.AppPluginSessionRevokeRequest
	if !decodeAppPluginAuthObject(c, []string{"request_id", "app_key", "app_session_id", "subject", "reason"}, &request) {
		return
	}
	result, err := service.NewAppPluginAuthService(model.DB, service.ConfiguredAppPluginAuthOptions()).
		RevokeSession(c.Request.Context(), identity, c.GetHeader("Idempotency-Key"), request)
	if err != nil {
		writeAppPluginServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func CreateAppPluginServiceCredential(c *gin.Context) { issueAppPluginServiceCredential(c, "create") }

func RotateAppPluginServiceCredential(c *gin.Context) { issueAppPluginServiceCredential(c, "rotate") }

func appPluginCredentialRoot(c *gin.Context) (service.AuthIdentity, bool) {
	appPluginSensitiveResponse(c)
	middleware.SetAppPluginAuditTarget(c, c.Param("id"), 0)
	if c.GetInt("role") != common.RoleRootUser {
		writeAppPluginError(c, http.StatusForbidden, "forbidden", "")
		return service.AuthIdentity{}, false
	}
	if !appPluginBrowserOrigin(c) {
		return service.AuthIdentity{}, false
	}
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok {
		writeAppPluginError(c, http.StatusUnauthorized, "unauthenticated", "")
	}
	return identity, ok
}

func issueAppPluginServiceCredential(c *gin.Context, action string) {
	defer auditAppPluginCredential(c, action)
	identity, ok := appPluginCredentialRoot(c)
	if !ok || !appPluginFeatureEnabled(c) {
		return
	}
	var request struct {
		Scopes    []string `json:"scopes"`
		ExpiresAt int64    `json:"expires_at"`
	}
	if !decodeAppPluginAuthObject(c, []string{"scopes", "expires_at"}, &request) {
		return
	}
	operation, err := service.AppPluginCredentialOperation(c.Param("id"), action)
	if err != nil {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return
	}
	// Consume before the action, so failed issuance cannot restore the proof.
	if _, err := service.ConsumeOperationProof(c.GetHeader("X-Security-Proof"), identity, operation); err != nil {
		writeAppPluginError(c, http.StatusForbidden, "approval_required", "")
		return
	}
	var result model.AppServiceCredentialIssued
	err = model.RunAppPluginTransaction(model.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		result = model.AppServiceCredentialIssued{}
		if err := lockAppPluginCredentialAuthority(tx, identity, c.Param("id")); err != nil {
			return err
		}
		result, err = model.IssueAppServiceCredential(c.Request.Context(), tx, c.Param("id"),
			request.Scopes, time.Now(), time.Unix(request.ExpiresAt, 0), action == "rotate")
		return err
	})
	if err != nil {
		writeAppPluginServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": result})
}

func RevokeAppPluginServiceCredential(c *gin.Context) {
	defer auditAppPluginCredential(c, "revoke")
	identity, ok := appPluginCredentialRoot(c)
	if !ok {
		return
	}
	err := model.RunAppPluginTransaction(model.DB.WithContext(c.Request.Context()), func(tx *gorm.DB) error {
		if err := lockAppPluginCredentialAuthority(tx, identity, c.Param("id")); err != nil {
			return err
		}
		return model.RevokeAppServiceCredential(c.Request.Context(), tx, c.Param("id"), c.Param("credential_id"), time.Now())
	})
	if err != nil {
		writeAppPluginServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"credential_id": c.Param("credential_id"), "state": "revoked"}})
}

func lockAppPluginCredentialAuthority(tx *gorm.DB, identity service.AuthIdentity, installationID string) error {
	var locator model.AppInstallation
	if err := tx.Select("app_key").Where("installation_id = ?", installationID).First(&locator).Error; err != nil {
		return err
	}
	// Match launch operations: ownership, installation, then identity children.
	if _, err := model.LockAppPluginInstallation(tx, locator.AppKey, installationID); err != nil {
		return err
	}
	user, _, err := service.AppPluginDashboardIdentity(tx, identity, time.Now())
	if err != nil {
		return err
	}
	if user.Role != common.RoleRootUser {
		return &appPluginAPIError{status: http.StatusForbidden, code: "forbidden"}
	}
	return nil
}

// Credential endpoints are not in the legacy installation audit route map.
// Record only fixed action/target metadata, never request or response bodies.
func auditAppPluginCredential(c *gin.Context, action string) {
	if c.GetInt("id") <= 0 {
		return
	}
	status := c.Writer.Status()
	success := status >= 200 && status < 300
	model.RecordOperationAuditLog(c.GetInt("id"), c.GetInt("role"), "app_plugin.credential."+action,
		c.ClientIP(), "app_plugin.credential."+action, map[string]any{
			"object": "app_installation", "object_id": c.Param("id"), "credential_id": c.Param("credential_id"),
		}, &model.AuditAdminInfo{AdminID: c.GetInt("id"), AdminRole: c.GetInt("role"), AuthMethod: "session"},
		&model.AuditRequestInfo{Method: c.Request.Method, Route: c.FullPath(), Path: c.FullPath(), Status: status, Success: success}, c)
}
