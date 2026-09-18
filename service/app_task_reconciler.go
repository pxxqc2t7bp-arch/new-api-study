package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type AppTaskLookupRequest struct {
	RequestID    string  `json:"request_id"`
	AppKey       string  `json:"app_key"`
	AppSessionID string  `json:"app_session_id"`
	Subject      string  `json:"subject"`
	TaskID       *string `json:"task_id"`
	GrantID      *string `json:"grant_id"`
}

type AppTaskLookupResult struct {
	TaskID             string `json:"task_id"`
	GrantID            string `json:"grant_id"`
	Status             string `json:"status"`
	ActualModel        string `json:"actual_model"`
	ProviderAccepted   bool   `json:"provider_accepted"`
	Terminal           bool   `json:"terminal"`
	PluginKey          string `json:"plugin_key"`
	PluginVersion      string `json:"plugin_version"`
	Usage              any    `json:"usage"`
	Artifacts          []any  `json:"artifacts"`
	Error              any    `json:"error"`
	UpdatedAt          string `json:"updated_at"`
	ReconciliationOnly bool   `json:"reconciliation_only"`
}

type AppTaskCancelRequest struct {
	RequestID    string `json:"request_id"`
	AppKey       string `json:"app_key"`
	AppSessionID string `json:"app_session_id"`
	Subject      string `json:"subject"`
	TaskID       string `json:"task_id"`
	Reason       string `json:"reason"`
}

type AppTaskCancelResult struct {
	TaskID          string  `json:"task_id"`
	CancelRequestID string  `json:"cancel_request_id"`
	State           string  `json:"state"`
	ProviderStatus  string  `json:"provider_status"`
	CancelledAt     *string `json:"cancelled_at"`
}

type AppArkQueryScope struct {
	ProjectID string    `json:"project_id"`
	StartAt   time.Time `json:"start_at"`
	EndAt     time.Time `json:"end_at"`
}

type AppArkImportLookupRequest struct {
	RequestID    string           `json:"request_id"`
	AppKey       string           `json:"app_key"`
	AppSessionID string           `json:"app_session_id"`
	Subject      string           `json:"subject"`
	TaskID       string           `json:"task_id"`
	QueryScope   AppArkQueryScope `json:"query_scope"`
}

type AppArkTaskQuery struct {
	TaskID     string    `json:"task_id"`
	AccountRef string    `json:"account_ref"`
	ProjectID  string    `json:"project_id"`
	StartAt    time.Time `json:"start_at"`
	EndAt      time.Time `json:"end_at"`
}

type AppArkTaskEvidence struct {
	TaskID         string    `json:"task_id"`
	AccountRef     string    `json:"account_ref"`
	ProjectID      string    `json:"project_id"`
	CreatedAt      time.Time `json:"created_at"`
	RequestJSON    string    `json:"request_json"`
	Source         string    `json:"source,omitempty"`
	ModelVersion   string    `json:"model_version,omitempty"`
	GatewayVersion string    `json:"gateway_version,omitempty"`
	AdapterVersion string    `json:"adapter_version,omitempty"`
}

type AppArkTaskSource interface {
	Lookup(context.Context, AppArkTaskQuery) ([]AppArkTaskEvidence, error)
}

type AppArkImportLookupResult struct {
	PolicyVersion  int64                `json:"policy_version"`
	Authorization  []AppArkTaskQuery    `json:"authorization"`
	Candidates     []AppArkTaskEvidence `json:"candidates"`
	MissingFields  []string             `json:"missing_fields"`
	Warnings       []string             `json:"warnings"`
	EvidenceDigest string               `json:"evidence_digest"`
}

func DecodeAppTaskLookupRequest(raw []byte, request *AppTaskLookupRequest) error {
	if err := decodeAppExecutionJSON(raw, request); err != nil {
		return err
	}
	return request.validate()
}

func DecodeAppTaskCancelRequest(raw []byte, request *AppTaskCancelRequest) error {
	if err := decodeAppExecutionJSON(raw, request); err != nil {
		return err
	}
	return request.validate()
}

func DecodeAppArkImportLookupRequest(raw []byte, request *AppArkImportLookupRequest) error {
	if err := decodeAppExecutionJSON(raw, request); err != nil {
		return err
	}
	return request.validate()
}

func validateAppTaskControlIdentity(requestID, appKey, sessionID, subject string) error {
	id, err := uuid.Parse(requestID)
	if err != nil || id == uuid.Nil || id.String() != requestID ||
		!appControlOpaque(appKey, 128) || !appControlOpaque(sessionID, 64) || !appControlOpaque(subject, 64) {
		return appAuthError("invalid_request")
	}
	return nil
}

func appControlOpaque(value string, limit int) bool {
	return appPluginOpaque(value, limit) && !strings.ContainsFunc(value, unicode.IsControl)
}

func (r AppTaskLookupRequest) validate() error {
	if err := validateAppTaskControlIdentity(r.RequestID, r.AppKey, r.AppSessionID, r.Subject); err != nil {
		return err
	}
	if (r.TaskID == nil) == (r.GrantID == nil) ||
		(r.TaskID != nil && !appControlOpaque(*r.TaskID, 64)) || (r.GrantID != nil && !appControlOpaque(*r.GrantID, 64)) {
		return appAuthError("invalid_request")
	}
	return nil
}

func (r AppTaskCancelRequest) validate() error {
	if err := validateAppTaskControlIdentity(r.RequestID, r.AppKey, r.AppSessionID, r.Subject); err != nil {
		return err
	}
	if !appControlOpaque(r.TaskID, 64) || !appControlOpaque(r.Reason, 128) {
		return appAuthError("invalid_request")
	}
	return nil
}

type appTaskAuthority struct {
	session            model.AppPluginSession
	user               model.User
	reconciliationOnly bool
}

// Caller holds this transaction through its object read or frozen replay.
// Introspection's identity.read requirement is intentionally not used here.
func (s *AppExecutionService) taskAuthorityTx(tx *gorm.DB, identity model.AppServiceIdentity,
	appKey, sessionID, subject string, required []string, allowLogout bool) (appTaskAuthority, error) {
	var authority appTaskAuthority
	if appKey != identity.AppKey {
		return authority, appAuthError("not_found")
	}
	if s.options.Issuer == "" {
		return authority, appAuthError("identity_not_configured")
	}
	now := s.options.Now().UTC()
	installation, err := model.LockAppPluginInstallation(tx, identity.AppKey, identity.InstallationID)
	if err != nil || installation.AppKey != identity.AppKey || installation.InstallationID != identity.InstallationID {
		return authority, appAuthError("not_found")
	}
	current, err := model.ValidateAppServiceIdentity(tx, identity, now)
	if err != nil {
		return authority, appAuthError("service_identity_invalid")
	}
	if installation.Status != model.AppInstallationStatusEnabled {
		return authority, appAuthError("identity_inactive")
	}
	if err := model.AppPluginCurrentRead(tx).Where("app_session_id = ? AND installation_id = ? AND app_key = ? AND subject = ?",
		sessionID, installation.InstallationID, appKey, subject).First(&authority.session).Error; err != nil {
		return authority, appAuthError("not_found")
	}
	session := authority.session
	if session.AppSessionID != sessionID || session.AppKey != appKey ||
		session.InstallationID != identity.InstallationID || session.Subject != subject {
		return authority, appAuthError("not_found")
	}
	user, dashboard, identityErr := AppPluginDashboardIdentity(tx, AuthIdentity{UserID: session.UserID,
		SessionID: session.DashboardSessionID, UserAuthVersion: session.AuthVersion, SessionVersion: session.SessionVersion}, now)
	authority.user = user
	if identityErr != nil {
		var authErr *AppPluginAuthError
		// Logout does not advance either version. Any version mismatch, expiry,
		// deletion, disable or different revocation reason remains a hard deny.
		if !allowLogout || !errors.As(identityErr, &authErr) || authErr.Code != "unauthenticated" ||
			user.Id != session.UserID || user.Status != common.UserStatusEnabled ||
			session.AuthVersion <= 0 || session.SessionVersion <= 0 ||
			user.AuthVersion != session.AuthVersion || dashboard.UserAuthVersion != session.AuthVersion ||
			dashboard.Version != session.SessionVersion || dashboard.UserID != user.Id ||
			dashboard.SID != session.DashboardSessionID || dashboard.ExpiresAt <= now.Unix() ||
			dashboard.Status != model.UserSessionStatusRevoked || dashboard.RevokedAt == 0 || dashboard.RevokedReason != "logout" {
			return authority, identityErr
		}
		authority.reconciliationOnly = true
	}
	if dashboard.SID != session.DashboardSessionID || dashboard.UserID != session.UserID || user.Id != session.UserID {
		return authority, appAuthError("unauthenticated")
	}
	if session.RevokedAt != 0 || session.UpstreamExpiresAt <= now.Unix() ||
		session.Generation != installation.AppVersionID || session.Issuer != s.options.Issuer {
		return authority, appAuthError("unauthenticated")
	}
	if len(installation.AllowedUserPolicy.Groups) != 0 && !slices.Contains(installation.AllowedUserPolicy.Groups, user.Group) {
		return authority, appAuthError("scope_denied")
	}
	manifest, _, err := ValidateAppPluginRegistration(tx, installation)
	if err != nil {
		return authority, appAuthError("scope_denied")
	}
	for _, scope := range required {
		if !slices.Contains(current.Scopes, scope) || !slices.Contains(session.GrantedScopes, scope) ||
			!slices.Contains(manifest.RequestedScopes, scope) {
			return authority, appAuthError("scope_denied")
		}
	}
	return authority, nil
}

func ownedAppTaskTx(ctx context.Context, tx *gorm.DB, authority appTaskAuthority, taskID, grantID string) (model.AppTaskExecution, error) {
	session := authority.session
	task, err := model.GetScopedAppTask(ctx, model.AppPluginCurrentRead(tx), model.AppTaskScope{
		AppKey: session.AppKey, InstallationID: session.InstallationID, Subject: session.Subject, UserID: authority.user.Id,
	}, taskID, grantID)
	if err != nil {
		return model.AppTaskExecution{}, appAuthError("not_found")
	}
	var grant model.AppExecutionGrant
	if err := model.AppPluginCurrentRead(tx).Where("grant_id = ?", task.GrantID).First(&grant).Error; err != nil {
		return model.AppTaskExecution{}, appAuthError("not_found")
	}
	if task.GrantID != grant.GrantID || task.AppKey != grant.AppKey || task.InstallationID != grant.InstallationID ||
		task.Subject != grant.Subject || task.UserID != grant.UserID || task.AppSessionID != grant.AppSessionID ||
		task.RunID != grant.RunID || task.ExecutionRequestID != grant.ExecutionRequestID || task.Operation != grant.Operation ||
		(authority.reconciliationOnly && (!task.ProviderAccepted || task.AppSessionID != session.AppSessionID)) {
		return model.AppTaskExecution{}, appAuthError("not_found")
	}
	return task, nil
}

func (s *AppExecutionService) LookupScopedTask(ctx context.Context, identity model.AppServiceIdentity, request AppTaskLookupRequest) (AppTaskLookupResult, error) {
	if err := request.validate(); err != nil {
		return AppTaskLookupResult{}, err
	}
	var result AppTaskLookupResult
	err := model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		result = AppTaskLookupResult{}
		authority, err := s.taskAuthorityTx(tx, identity, request.AppKey, request.AppSessionID, request.Subject, []string{"task.read"}, true)
		if err != nil {
			return err
		}
		var taskID, grantID string
		if request.TaskID != nil {
			taskID = *request.TaskID
		} else {
			grantID = *request.GrantID
		}
		task, err := ownedAppTaskTx(ctx, tx, authority, taskID, grantID)
		if err != nil {
			return err
		}
		if !validAppTaskProviderState(task.ProviderState) || task.UpdatedAt <= 0 {
			return appAuthError("invalid_evidence")
		}
		usage, _, err := appTaskEvidenceJSON(task.UsageJSON)
		if err != nil {
			return err
		}
		artifacts, _, err := appTaskEvidenceJSON(task.ArtifactsJSON)
		if err != nil {
			return err
		}
		saved, _, err := appTaskEvidenceJSON(task.ResultJSON)
		if err != nil {
			return err
		}
		result = AppTaskLookupResult{TaskID: task.TaskID, GrantID: task.GrantID, Status: task.ProviderState,
			ActualModel: task.ActualModel, ProviderAccepted: task.ProviderAccepted,
			Terminal:  slices.Contains([]string{"succeeded", "failed", "cancelled"}, task.ProviderState),
			PluginKey: task.PluginKey, PluginVersion: task.PluginVersion, Usage: usage, Artifacts: []any{},
			UpdatedAt: time.Unix(task.UpdatedAt, 0).UTC().Format(time.RFC3339Nano), ReconciliationOnly: authority.reconciliationOnly}
		if artifacts != nil {
			var ok bool
			result.Artifacts, ok = artifacts.([]any)
			if !ok {
				return appAuthError("invalid_evidence")
			}
		}
		result.Artifacts, err = s.enrichAppTaskArtifactsTx(tx, identity, authority, task, result.Artifacts)
		if err != nil {
			return err
		}
		if facts, ok := saved.(map[string]any); ok {
			result.Error = facts["error"]
		}
		return nil
	})
	if err != nil {
		return AppTaskLookupResult{}, err
	}
	return result, nil
}

func validAppTaskProviderState(state string) bool {
	return slices.Contains([]string{"queued", "running", "succeeded", "failed", "cancelled"}, state)
}

func (s *AppExecutionService) CancelTask(ctx context.Context, identity model.AppServiceIdentity, key string, request AppTaskCancelRequest) (AppTaskCancelResult, error) {
	if err := request.validate(); err != nil {
		return AppTaskCancelResult{}, err
	}
	if !appControlOpaque(key, 128) {
		return AppTaskCancelResult{}, appAuthError("invalid_request")
	}
	scope, err := appPluginHash([]string{"task-cancel/v1", identity.AppKey, identity.InstallationID, identity.CredentialID, identity.Version, key})
	if err != nil {
		return AppTaskCancelResult{}, err
	}
	hash, err := appPluginHash(request)
	if err != nil {
		return AppTaskCancelResult{}, err
	}
	var result AppTaskCancelResult
	err = model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		result = AppTaskCancelResult{}
		authority, err := s.taskAuthorityTx(tx, identity, request.AppKey, request.AppSessionID, request.Subject, []string{"task.read", "model.invoke"}, false)
		if err != nil {
			return err
		}
		task, err := ownedAppTaskTx(ctx, tx, authority, request.TaskID, "")
		if err != nil {
			return err
		}
		var replay model.AppTaskCancelReplay
		q := model.AppPluginCurrentRead(tx).Where("scope_hash = ?", scope).Limit(1).Find(&replay)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected != 0 {
			if replay.RequestHash != hash {
				return appAuthError("idempotency_conflict")
			}
			if decodeAppExecutionJSON([]byte(replay.ResponseJSON), &result) != nil {
				return appAuthError("invalid_evidence")
			}
			return nil
		}
		if !validAppTaskProviderState(task.ProviderState) {
			return appAuthError("invalid_evidence")
		}
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		// Doubao exposes no cancellation operation. Never turn an intent into
		// a terminal provider fact or infer a cancellation time from UpdatedAt.
		result = AppTaskCancelResult{TaskID: task.TaskID, CancelRequestID: id.String(),
			State: "not_cancellable", ProviderStatus: task.ProviderState}
		if task.ProviderState == "cancelled" {
			result.State = "cancelled"
		}
		raw, err := common.Marshal(result)
		if err != nil {
			return err
		}
		return tx.Create(&model.AppTaskCancelReplay{ScopeHash: scope, RequestHash: hash,
			InstallationID: identity.InstallationID, ResponseJSON: string(raw)}).Error
	})
	if err != nil {
		return AppTaskCancelResult{}, err
	}
	return result, nil
}
