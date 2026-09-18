package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"gorm.io/gorm"
)

const (
	appTaskArtifactAccessVersion    = "app1"
	appTaskArtifactAccessLifetime   = 2 * time.Minute
	MaxAppTaskArtifactAccessLength  = 2048
	maxAppTaskArtifactPayloadLength = 1536
	appTaskArtifactAccessKeyDomain  = "new-api/app-plugin/task-content-key/app1"
	appTaskArtifactCipherKeyDomain  = "new-api/app-plugin/task-content-cipher-key/app1"
	appTaskArtifactAccessMACDomain  = "new-api/app-plugin/task-content-mac/app1"
)

var ErrAppTaskArtifactAccessInvalid = errors.New("app task artifact access is invalid")

type AppTaskArtifactAccess struct {
	TaskID            string `json:"t"`
	ArtifactKey       string `json:"a"`
	AppKey            string `json:"k"`
	InstallationID    string `json:"i"`
	GrantID           string `json:"g"`
	AppSessionID      string `json:"s"`
	CredentialID      string `json:"c"`
	CredentialVersion string `json:"v"`
	Subject           string `json:"o"`
	UserID            int    `json:"u"`
	ExpiresAt         int64  `json:"e"`
}

func (access AppTaskArtifactAccess) valid(now time.Time) bool {
	return appControlOpaque(access.TaskID, 64) &&
		appControlOpaque(access.ArtifactKey, maxTaskArtifactKeyLength) &&
		!strings.Contains(access.ArtifactKey, "/") &&
		appControlOpaque(access.AppKey, 128) &&
		appControlOpaque(access.InstallationID, 64) &&
		appControlOpaque(access.GrantID, 64) &&
		appControlOpaque(access.AppSessionID, 64) &&
		appControlOpaque(access.CredentialID, 64) &&
		appControlOpaque(access.CredentialVersion, 128) &&
		appControlOpaque(access.Subject, 64) &&
		access.UserID > 0 &&
		access.ExpiresAt > now.Unix() &&
		access.ExpiresAt <= now.Add(appTaskArtifactAccessLifetime).Unix()
}

func appTaskArtifactAccessMAC(key, payload []byte) []byte {
	derived := hmac.New(sha256.New, key)
	_, _ = derived.Write([]byte(appTaskArtifactAccessKeyDomain))
	mac := hmac.New(sha256.New, derived.Sum(nil))
	_, _ = mac.Write([]byte(appTaskArtifactAccessMACDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func appTaskArtifactCipherKey(key []byte) []byte {
	derived := hmac.New(sha256.New, key)
	_, _ = derived.Write([]byte(appTaskArtifactCipherKeyDomain))
	return derived.Sum(nil)
}

func (s *AppExecutionService) issueAppTaskArtifactAccess(access AppTaskArtifactAccess) (string, error) {
	now := s.options.Now().UTC()
	if len(s.options.DerivationKey) < 32 || !access.valid(now) {
		return "", ErrAppTaskArtifactAccessInvalid
	}
	payload, err := common.Marshal(access)
	if err != nil || len(payload) == 0 || len(payload) > maxAppTaskArtifactPayloadLength {
		return "", ErrAppTaskArtifactAccessInvalid
	}
	block, err := aes.NewCipher(appTaskArtifactCipherKey(s.options.DerivationKey))
	if err != nil {
		return "", ErrAppTaskArtifactAccessInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", ErrAppTaskArtifactAccessInvalid
	}
	opaque := make([]byte, aead.NonceSize(), aead.NonceSize()+len(payload)+aead.Overhead())
	if _, err := rand.Read(opaque); err != nil {
		return "", ErrAppTaskArtifactAccessInvalid
	}
	opaque = aead.Seal(opaque, opaque, payload, []byte(appTaskArtifactAccessVersion))
	signature := appTaskArtifactAccessMAC(s.options.DerivationKey, opaque)
	token := appTaskArtifactAccessVersion + "." +
		base64.RawURLEncoding.EncodeToString(opaque) + "." +
		base64.RawURLEncoding.EncodeToString(signature)
	if len(token) > MaxAppTaskArtifactAccessLength {
		return "", ErrAppTaskArtifactAccessInvalid
	}
	return token, nil
}

// VerifyAppTaskArtifactAccess authenticates and route-binds an app1 capability
// without consulting storage. Callers must still recheck all live authority.
func VerifyAppTaskArtifactAccess(raw, taskID, artifactKey string, options AppPluginAuthOptions) (AppTaskArtifactAccess, bool) {
	var access AppTaskArtifactAccess
	if options.Now == nil {
		options.Now = time.Now
	}
	if len(options.DerivationKey) < 32 || len(raw) > MaxAppTaskArtifactAccessLength {
		return access, false
	}
	payloadPart, signaturePart, ok := strings.Cut(strings.TrimPrefix(raw, appTaskArtifactAccessVersion+"."), ".")
	if !ok || !strings.HasPrefix(raw, appTaskArtifactAccessVersion+".") ||
		payloadPart == "" || len(payloadPart) > maxAppTaskArtifactPayloadLength*2 ||
		len(signaturePart) != 43 || strings.Contains(signaturePart, ".") {
		return access, false
	}
	opaque, err := base64.RawURLEncoding.Strict().DecodeString(payloadPart)
	if err != nil || len(opaque) == 0 || len(opaque) > maxAppTaskArtifactPayloadLength+64 {
		return access, false
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(signaturePart)
	if err != nil || len(signature) != sha256.Size ||
		!hmac.Equal(signature, appTaskArtifactAccessMAC(options.DerivationKey, opaque)) {
		return access, false
	}
	block, err := aes.NewCipher(appTaskArtifactCipherKey(options.DerivationKey))
	if err != nil {
		return access, false
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(opaque) < aead.NonceSize()+aead.Overhead() {
		return access, false
	}
	payload, err := aead.Open(nil, opaque[:aead.NonceSize()], opaque[aead.NonceSize():],
		[]byte(appTaskArtifactAccessVersion))
	if err != nil || len(payload) == 0 || len(payload) > maxAppTaskArtifactPayloadLength {
		return access, false
	}
	if common.Unmarshal(payload, &access) != nil || !access.valid(options.Now().UTC()) ||
		access.TaskID != taskID || access.ArtifactKey != artifactKey {
		return AppTaskArtifactAccess{}, false
	}
	return access, true
}

func (s *AppExecutionService) appTaskArtifactContentURL(identity model.AppServiceIdentity,
	authority appTaskAuthority, task model.AppTaskExecution, artifactKey string) (string, time.Time, error) {
	now := s.options.Now().UTC()
	expiresAt := min(now.Add(appTaskArtifactAccessLifetime).Unix(), authority.session.UpstreamExpiresAt)
	access := AppTaskArtifactAccess{
		TaskID: task.TaskID, ArtifactKey: artifactKey, AppKey: task.AppKey,
		InstallationID: task.InstallationID, GrantID: task.GrantID,
		AppSessionID: authority.session.AppSessionID, CredentialID: identity.CredentialID,
		CredentialVersion: identity.Version, Subject: task.Subject, UserID: task.UserID,
		ExpiresAt: expiresAt,
	}
	token, err := s.issueAppTaskArtifactAccess(access)
	if err != nil {
		return "", time.Time{}, err
	}
	baseAddress := strings.TrimSpace(system_setting.TaskPublicAddress)
	if baseAddress == "" {
		baseAddress = strings.TrimSpace(system_setting.ServerAddress)
	}
	if validationErr := ValidateTaskArtifactBaseURL(baseAddress); validationErr != nil {
		return "", time.Time{}, validationErr
	}
	baseURL, err := url.Parse(baseAddress)
	if err != nil {
		return "", time.Time{}, err
	}
	return buildTaskArtifactContentURL(baseURL, task.TaskID, artifactKey, token),
		time.Unix(expiresAt, 0).UTC(), nil
}

func (s *AppExecutionService) enrichAppTaskArtifactsTx(tx *gorm.DB, identity model.AppServiceIdentity,
	authority appTaskAuthority, task model.AppTaskExecution, artifacts []any) ([]any, error) {
	if authority.reconciliationOnly || !task.ProviderAccepted || task.ProviderState != "succeeded" || len(artifacts) == 0 {
		return artifacts, nil
	}
	var projection model.Task
	if err := model.AppPluginCurrentRead(tx).Where("task_id = ? AND user_id = ? AND execution_mode = ?",
		task.TaskID, task.UserID, model.TaskExecutionModeAppManaged).First(&projection).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return artifacts, nil
		}
		return nil, appAuthError("invalid_evidence")
	}
	if projection.TaskID != task.TaskID || projection.UserId != task.UserID ||
		projection.Status != model.TaskStatusSuccess || projection.ExecutionMode != model.TaskExecutionModeAppManaged {
		return nil, appAuthError("invalid_evidence")
	}
	enriched := make([]any, len(artifacts))
	for i, item := range artifacts {
		artifact, ok := item.(map[string]any)
		if !ok {
			return nil, appAuthError("invalid_evidence")
		}
		key, ok := artifact["key"].(string)
		if !ok || !appControlOpaque(key, maxTaskArtifactKeyLength) || strings.Contains(key, "/") {
			return nil, appAuthError("invalid_evidence")
		}
		enriched[i] = artifact
		if source, exists := projection.PrivateData.AppArtifactURLs[key]; !exists ||
			source == "" || source != strings.TrimSpace(source) {
			continue
		}
		contentURL, expiresAt, err := s.appTaskArtifactContentURL(identity, authority, task, key)
		if err != nil {
			return nil, err
		}
		copy := make(map[string]any, len(artifact)+2)
		maps.Copy(copy, artifact)
		copy["content_url"] = contentURL
		copy["expires_at"] = expiresAt.Format(time.RFC3339Nano)
		enriched[i] = copy
	}
	return enriched, nil
}

func appTaskArtifactExists(artifacts []any, artifactKey string) bool {
	return slices.ContainsFunc(artifacts, func(item any) bool {
		artifact, ok := item.(map[string]any)
		key, keyOK := artifact["key"].(string)
		return ok && keyOK && key == artifactKey
	})
}

// ResolveAppTaskArtifactContent rechecks all live authority and returns only
// the host-observed credentialless descriptor saved by the App worker.
func (s *AppExecutionService) ResolveAppTaskArtifactContent(ctx context.Context, access AppTaskArtifactAccess,
	method string) (*model.Task, string, error) {
	if !access.valid(s.options.Now().UTC()) || (method != http.MethodGet && method != http.MethodHead) {
		return nil, "", ErrAppTaskArtifactAccessInvalid
	}
	var projection model.Task
	var source string
	err := model.RunAppPluginTransaction(s.db.WithContext(ctx), func(tx *gorm.DB) error {
		identity := model.AppServiceIdentity{AppKey: access.AppKey, InstallationID: access.InstallationID,
			CredentialID: access.CredentialID, Version: access.CredentialVersion}
		authority, err := s.taskAuthorityTx(tx, identity, access.AppKey, access.AppSessionID,
			access.Subject, []string{"task.read"}, false)
		if err != nil || authority.session.AppSessionID != access.AppSessionID ||
			authority.user.Id != access.UserID {
			return appAuthError("not_found")
		}
		task, err := ownedAppTaskTx(ctx, tx, authority, access.TaskID, "")
		if err != nil || task.TaskID != access.TaskID || task.GrantID != access.GrantID ||
			!task.ProviderAccepted || task.ProviderState != "succeeded" {
			return appAuthError("not_found")
		}
		artifacts, _, err := appTaskEvidenceJSON(task.ArtifactsJSON)
		if err != nil {
			return appAuthError("not_found")
		}
		items, ok := artifacts.([]any)
		if !ok || !appTaskArtifactExists(items, access.ArtifactKey) {
			return appAuthError("not_found")
		}
		if err := model.AppPluginCurrentRead(tx).Where("task_id = ? AND user_id = ? AND execution_mode = ?",
			task.TaskID, task.UserID, model.TaskExecutionModeAppManaged).First(&projection).Error; err != nil {
			return appAuthError("not_found")
		}
		source = projection.PrivateData.AppArtifactURLs[access.ArtifactKey]
		if projection.TaskID != task.TaskID || projection.UserId != task.UserID ||
			projection.Status != model.TaskStatusSuccess || projection.ExecutionMode != model.TaskExecutionModeAppManaged ||
			source == "" || source != strings.TrimSpace(source) {
			return appAuthError("not_found")
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return &projection, source, nil
}
