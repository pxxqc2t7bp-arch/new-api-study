package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	goversion "github.com/hashicorp/go-version"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrManifestCannotOwnEntitlementPolicy = errors.New("manifest_cannot_own_entitlement_policy")
	ErrManifestCannotOwnHostPolicy        = errors.New("manifest_cannot_own_host_policy")
	ErrEntitlementPolicyImmutable         = errors.New("app_entitlement_policy_immutable")
	errEntitlementVersionRace             = errors.New("app_entitlement_policy_version_race")
)

type AppPluginInstallationService struct {
	db                *gorm.DB
	taskPluginChecker AppTaskPluginChecker
	currentAuthz      map[string][]string
}

type AppAllowedUserPolicy = model.AppAllowedUserPolicy
type AppNetworkPolicy = model.AppNetworkPolicy

type AppPluginInstallationOptions struct {
	TaskPluginChecker AppTaskPluginChecker
	CurrentAuthz      map[string][]string
}

type AppTaskPluginChecker interface {
	CheckTaskPluginDependency(ctx context.Context, key, minimumVersion string) error
}

type AppInstallCommand struct {
	ActorID              int64
	IdempotencyKey       string
	ManifestJSON         []byte
	BaseURL              string
	EnabledSurfaces      []string
	AllowedParentOrigins []string
	AllowedOrigins       []string
	AllowedUserPolicy    model.AppAllowedUserPolicy
	NetworkPolicy        model.AppNetworkPolicy
	EntitlementPolicyID  string
	ServiceCredential    AppServiceCredentialInput
}

type AppServiceCredentialInput struct {
	ID        string `json:"credential_id"`
	Secret    string `json:"-"`
	Version   string `json:"version"`
	ExpiresAt int64  `json:"expires_at"`
}

type AppEntitlementPolicyDraft struct {
	Key   string
	Rules map[string][]string
}

type AppEntitlementPolicyResult struct {
	ID             string              `json:"id"`
	Key            string              `json:"key"`
	Version        int64               `json:"version"`
	EffectiveRules map[string][]string `json:"effective_rules"`
}

func NewAppPluginInstallationService(db *gorm.DB, opts AppPluginInstallationOptions) *AppPluginInstallationService {
	checker := opts.TaskPluginChecker
	if checker == nil {
		checker = dbTaskPluginChecker{db: db}
	}
	return &AppPluginInstallationService{
		db:                db,
		taskPluginChecker: checker,
		currentAuthz:      normalizeRules(opts.CurrentAuthz),
	}
}

func (s *AppPluginInstallationService) Install(ctx context.Context, cmd AppInstallCommand) (model.AppInstallResult, error) {
	var raw map[string]any
	if err := common.Unmarshal(cmd.ManifestJSON, &raw); err != nil {
		return model.AppInstallResult{}, err
	}
	if _, ok := raw["entitlementPolicy"]; ok {
		return model.AppInstallResult{}, ErrManifestCannotOwnEntitlementPolicy
	}
	if _, ok := raw["baseUrl"]; ok {
		return model.AppInstallResult{}, ErrManifestCannotOwnHostPolicy
	}
	if _, ok := raw["allowedOrigins"]; ok {
		return model.AppInstallResult{}, ErrManifestCannotOwnHostPolicy
	}
	manifest, err := ValidateAppManifest(cmd.ManifestJSON)
	if err != nil {
		return model.AppInstallResult{}, err
	}
	canonicalizeAppManifest(&manifest)
	requirements := mergeTaskPluginRequirements(manifest.Requires.TaskPlugins, []AppManifestTaskPluginRequirement{{Key: "doubao", MinimumVersion: "1.2.0"}})
	for _, requirement := range requirements {
		if err := s.taskPluginChecker.CheckTaskPluginDependency(ctx, requirement.Key, requirement.MinimumVersion); err != nil {
			return model.AppInstallResult{}, err
		}
	}
	canonicalManifestJSON, err := common.Marshal(manifest)
	if err != nil {
		return model.AppInstallResult{}, err
	}
	baseURL, err := normalizeHTTPSBaseURL(cmd.BaseURL)
	if err != nil {
		return model.AppInstallResult{}, err
	}
	enabledSurfaces := normalizeStringSet(cmd.EnabledSurfaces)
	for _, surface := range enabledSurfaces {
		if surface != "direct" && surface != "embedded" {
			return model.AppInstallResult{}, fmt.Errorf("invalid enabled surface")
		}
	}
	allowedParents, err := normalizeOrigins(cmd.AllowedParentOrigins)
	if err != nil {
		return model.AppInstallResult{}, err
	}
	allowedOrigins, err := normalizeOrigins(cmd.AllowedOrigins)
	if err != nil {
		return model.AppInstallResult{}, err
	}
	allowedUserPolicy := model.AppAllowedUserPolicy{Groups: normalizeStringSet(cmd.AllowedUserPolicy.Groups)}
	networkPolicy := model.AppNetworkPolicy{AllowHosts: normalizeStringSet(cmd.NetworkPolicy.AllowHosts), DenyPrivateIPRanges: cmd.NetworkPolicy.DenyPrivateIPRanges}
	entitlementPolicyID := cmd.EntitlementPolicyID
	if entitlementPolicyID == "" {
		entitlementPolicyID = "host-default"
	}
	req := model.AppInstallRequest{
		AppKey:                   manifest.Key,
		ManifestVersion:          manifest.Version,
		ManifestSHA256:           serviceDigestBytes(canonicalManifestJSON),
		CanonicalManifestJSON:    canonicalManifestJSON,
		BaseURL:                  baseURL,
		CallbackURL:              absoluteManifestEndpoint(baseURL, manifest.CallbackPath),
		DirectURL:                absoluteManifestEndpoint(baseURL, manifest.Surfaces.Direct.StartPath),
		EmbeddedURL:              absoluteManifestEndpoint(baseURL, manifest.Surfaces.Embedded.StartPath),
		EnabledSurfaces:          enabledSurfaces,
		AllowedParentOrigins:     allowedParents,
		AllowedOrigins:           allowedOrigins,
		AllowedUserPolicy:        allowedUserPolicy,
		NetworkPolicy:            networkPolicy,
		EntitlementPolicyID:      entitlementPolicyID,
		ServiceCredentialHash:    serviceDigestBytes([]byte(cmd.ServiceCredential.Secret)),
		ServiceCredentialID:      cmd.ServiceCredential.ID,
		ServiceCredentialVersion: cmd.ServiceCredential.Version,
		ServiceCredentialExpiry:  cmd.ServiceCredential.ExpiresAt,
	}
	return model.InstallAppVersion(ctx, s.db, model.AppIdempotencyScope{
		ActorID: cmd.ActorID,
		Key:     cmd.IdempotencyKey,
	}, req)
}

func (s *AppPluginInstallationService) CreateEntitlementPolicy(ctx context.Context, draft AppEntitlementPolicyDraft) (AppEntitlementPolicyResult, error) {
	effective := intersectRules(draft.Rules, s.currentAuthz)
	keyHash := serviceDigestBytes([]byte(draft.Key))
	creationToken, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return AppEntitlementPolicyResult{}, err
	}
	for range 8 {
		var result AppEntitlementPolicyResult
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var maxVersion int64
			if err := tx.Model(&model.AppEntitlementPolicy{}).
				Where("key_hash = ?", keyHash).
				Select("COALESCE(MAX(version), 0)").
				Scan(&maxVersion).Error; err != nil {
				return err
			}
			versionKey := serviceDigestBytes([]byte(fmt.Sprintf("%s\x00%d", draft.Key, maxVersion+1)))
			policy := model.AppEntitlementPolicy{
				ID:             appPolicyID(versionKey),
				KeyHash:        keyHash,
				VersionKey:     versionKey,
				CreationToken:  creationToken,
				Key:            draft.Key,
				Version:        maxVersion + 1,
				EffectiveRules: model.AppJSONMap(effective),
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&policy).Error; err != nil {
				return err
			}
			var stored model.AppEntitlementPolicy
			if err := tx.Where("version_key = ?", versionKey).First(&stored).Error; err != nil {
				return err
			}
			if stored.CreationToken != creationToken {
				return errEntitlementVersionRace
			}
			result = AppEntitlementPolicyResult{
				ID:             stored.ID,
				Key:            stored.Key,
				Version:        stored.Version,
				EffectiveRules: map[string][]string(stored.EffectiveRules),
			}
			return nil
		})
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, errEntitlementVersionRace) {
			return AppEntitlementPolicyResult{}, err
		}
	}
	return AppEntitlementPolicyResult{}, fmt.Errorf("entitlement policy version allocation failed")
}

func (s *AppPluginInstallationService) UpdateEntitlementPolicy(context.Context, string, AppEntitlementPolicyDraft) error {
	return ErrEntitlementPolicyImmutable
}

func (s *AppPluginInstallationService) DeleteEntitlementPolicy(context.Context, string) error {
	return ErrEntitlementPolicyImmutable
}

type dbTaskPluginChecker struct {
	db *gorm.DB
}

func (c dbTaskPluginChecker) CheckTaskPluginDependency(ctx context.Context, key, minimumVersion string) error {
	var plugins []model.TaskPlugin
	keyColumn := "`key`"
	if c.db.Dialector.Name() == "postgres" {
		keyColumn = `"key"`
	}
	if err := c.db.WithContext(ctx).
		Where(keyColumn+" = ? AND active = ? AND enabled = ?", key, true, true).
		Find(&plugins).Error; err != nil {
		return err
	}
	minimum, err := goversion.NewSemver(minimumVersion)
	if err != nil {
		return err
	}
	for _, plugin := range plugins {
		current, parseErr := goversion.NewSemver(plugin.Version)
		if parseErr != nil {
			return parseErr
		}
		if !current.LessThan(minimum) {
			return nil
		}
	}
	return fmt.Errorf("task plugin %s below %s", key, minimumVersion)
}

func normalizeHTTPSBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("invalid base URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("base URL must be HTTPS")
	}
	u.Scheme = "https"
	if err := normalizeHTTPSHost(u); err != nil {
		return "", fmt.Errorf("invalid base URL")
	}
	u.Path = path.Clean("/" + u.Path)
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func normalizeOrigins(values []string) ([]string, error) {
	origins := make([]string, 0, len(values))
	for _, value := range values {
		u, err := url.Parse(value)
		if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("invalid origin")
		}
		if !strings.EqualFold(u.Scheme, "https") {
			return nil, fmt.Errorf("origin must be HTTPS")
		}
		u.Scheme = "https"
		if err := normalizeHTTPSHost(u); err != nil {
			return nil, fmt.Errorf("invalid origin")
		}
		u.Path = ""
		u.RawQuery = ""
		u.Fragment = ""
		origins = append(origins, u.String())
	}
	return normalizeStringSet(origins), nil
}

func normalizeHTTPSHost(u *url.URL) error {
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("missing host")
	}
	port := u.Port()
	if port == "443" {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	return nil
}

func normalizeStringSet(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		seen[value] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func canonicalizeAppManifest(manifest *AppManifest) {
	manifest.RequestedScopes = normalizeStringSet(manifest.RequestedScopes)
	sort.Slice(manifest.Requires.TaskPlugins, func(i, j int) bool {
		left := manifest.Requires.TaskPlugins[i]
		right := manifest.Requires.TaskPlugins[j]
		if left.Key != right.Key {
			return left.Key < right.Key
		}
		return semverLess(left.MinimumVersion, right.MinimumVersion)
	})
}

func absoluteManifestEndpoint(baseURL, manifestPath string) string {
	u, _ := url.Parse(baseURL)
	basePath := strings.TrimSuffix(u.Path, "/")
	u.Path = path.Clean(basePath + "/" + strings.TrimPrefix(manifestPath, "/"))
	return u.String()
}

func normalizeRules(rules map[string][]string) map[string][]string {
	normalized := make(map[string][]string, len(rules))
	for resource, actions := range rules {
		normalized[resource] = normalizeStringSet(actions)
	}
	return normalized
}

func intersectRules(requested, allowed map[string][]string) map[string][]string {
	effective := make(map[string][]string)
	for resource, requestedActions := range requested {
		allowedSet := make(map[string]struct{}, len(allowed[resource]))
		for _, action := range allowed[resource] {
			allowedSet[action] = struct{}{}
		}
		for _, action := range normalizeStringSet(requestedActions) {
			if _, ok := allowedSet[action]; ok {
				effective[resource] = append(effective[resource], action)
			}
		}
		if len(effective[resource]) == 0 {
			delete(effective, resource)
		}
	}
	return effective
}

func appPolicyID(versionKey string) string {
	return "policy_" + versionKey[:32]
}

func mergeTaskPluginRequirements(lists ...[]AppManifestTaskPluginRequirement) []AppManifestTaskPluginRequirement {
	merged := map[string]string{}
	for _, requirements := range lists {
		for _, requirement := range requirements {
			existing, ok := merged[requirement.Key]
			if !ok || semverLess(existing, requirement.MinimumVersion) {
				merged[requirement.Key] = requirement.MinimumVersion
			}
		}
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]AppManifestTaskPluginRequirement, 0, len(keys))
	for _, key := range keys {
		result = append(result, AppManifestTaskPluginRequirement{Key: key, MinimumVersion: merged[key]})
	}
	return result
}

func semverLess(a, b string) bool {
	left, leftErr := goversion.NewSemver(a)
	right, rightErr := goversion.NewSemver(b)
	if leftErr != nil || rightErr != nil {
		return a < b
	}
	return left.LessThan(right)
}

func serviceDigestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return fmt.Sprintf("%x", sum)
}
