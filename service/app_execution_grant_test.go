package service

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const appExecutionTestModel = "doubao-seedance-2-5-260628"

type appExecutionFixture struct {
	*appLaunchFixture
	execution  *AppExecutionService
	appSession AppPluginExchangeResult
	request    AppExecutionGrantRequest
	publisher  AuthIdentity
}

func appExecutionTestAsset(subject, protocol, pointer, mediaType, uri string, expiresAt time.Time) map[string]any {
	return map[string]any{
		"authorization_sha256": strings.Repeat("a", 64),
		"asset_uri":            uri,
		"json_pointer":         pointer,
		"media_type":           mediaType,
		"public_model":         appExecutionTestModel,
		"protocol":             protocol,
		"owner_subject":        subject,
		"project_id":           "offline-project",
		"purpose":              "model.invoke",
		"routing_account":      "cxy",
		"status":               "active",
		"expires_at":           expiresAt.UTC().Format(time.RFC3339),
	}
}

func appExecutionRequestWithAssets(t *testing.T, request AppExecutionGrantRequest, assets []map[string]any) AppExecutionGrantRequest {
	t.Helper()
	raw, err := common.Marshal(request)
	require.NoError(t, err)
	var envelope map[string]any
	require.NoError(t, common.Unmarshal(raw, &envelope))
	envelope["registered_asset_inputs"] = assets
	raw, err = common.Marshal(envelope)
	require.NoError(t, err)
	var decoded AppExecutionGrantRequest
	require.NoError(t, DecodeAppExecutionGrantRequest(raw, &decoded))
	return decoded
}

func newAppExecutionFixture(t *testing.T) *appExecutionFixture {
	t.Helper()
	launch := newAppLaunchFixture(t)
	redis, batch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, common.BatchUpdateEnabled = redis, batch })
	require.NoError(t, launch.db.AutoMigrate(&model.Option{}, &model.Channel{}, &model.Ability{},
		&model.Task{}, &model.Token{}, &model.SystemTask{}, &model.UserSubscription{}, &model.SubscriptionPlan{}))
	require.NoError(t, model.MigrateAppExecutionTables(launch.db))
	previousDB, previousRegistry := model.DB, pluginruntime.DefaultRegistry
	model.DB, pluginruntime.DefaultRegistry = launch.db, pluginruntime.NewRegistry()
	t.Cleanup(func() { model.DB, pluginruntime.DefaultRegistry = previousDB, previousRegistry })
	source, err := os.ReadFile("../plugins/tasks/doubao/plugin.js")
	require.NoError(t, err)
	plugin, err := pluginruntime.DefaultRegistry.RegisterFactory(string(source), pluginruntime.Options{Key: "doubao"})
	require.NoError(t, err)
	require.Equal(t, "1.2.0", plugin.Meta.Version)
	require.Contains(t, plugin.Meta.Models, appExecutionTestModel)
	require.True(t, slices.ContainsFunc(plugin.Meta.Protocols, func(claim pluginruntime.ProtocolClaim) bool {
		return claim.Name == "openai_video" && slices.Contains(claim.Models, appExecutionTestModel)
	}))
	require.True(t, slices.ContainsFunc(plugin.Meta.Routes, func(route pluginruntime.Route) bool {
		return route.Type == "submit" && slices.Contains(route.Models, appExecutionTestModel)
	}))

	scopes := []string{"identity.read", "model.invoke", "task.read", "task.import"}
	var version model.AppVersion
	require.NoError(t, launch.db.Where("app_key = ?", launch.installation.AppKey).First(&version).Error)
	var manifest AppManifest
	require.NoError(t, common.Unmarshal([]byte(version.CanonicalManifestJSON), &manifest))
	manifest.RequestedScopes = scopes
	canonical, err := common.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, launch.db.Model(&version).Updates(map[string]any{
		"canonical_manifest_json": string(canonical), "manifest_sha256": serviceDigest(string(canonical)),
	}).Error)
	require.NoError(t, launch.db.Model(&model.AppInstallation{}).Where("installation_id = ?", launch.installation.InstallationID).
		Update("manifest_sha256", serviceDigest(string(canonical))).Error)
	launch.credential, err = model.IssueAppServiceCredential(t.Context(), launch.db, launch.installation.InstallationID,
		scopes, launch.options.Now(), launch.options.Now().Add(time.Hour), true)
	require.NoError(t, err)
	launch.serviceID, err = model.AuthenticateAppServiceCredential(t.Context(), launch.db, launch.credential.Credential, launch.options.Now())
	require.NoError(t, err)
	require.NoError(t, launch.db.Model(&launch.user).Updates(map[string]any{"quota": 1000000, "role": common.RoleCommonUser}).Error)
	channelSetting := `{"task_plugin_key":"doubao"}`
	channel := model.Channel{Type: constant.ChannelTypeDoubaoVideo, Name: "host-approved", Key: "offline-provider-key", Setting: &channelSetting,
		Status: common.ChannelStatusEnabled, Group: "default", Models: appExecutionTestModel}
	require.NoError(t, launch.db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(launch.db))
	require.NoError(t, launch.db.Create(&model.Option{Key: "ModelPrice",
		Value: `{"doubao-seedance-2-5-260628":0.01}`}).Error)
	require.NoError(t, launch.db.Create(&[]model.Option{
		{Key: "UserUsableGroups", Value: `{"default":"Default"}`},
		{Key: "GroupRatio", Value: `{"default":1}`},
		{Key: "AutoGroups", Value: `["default"]`},
	}).Error)
	// This global operation policy narrows the registered App/user/group and
	// real plugin capability intersection; it must never replace those checks.
	root := model.User{Username: "policy-root", AffCode: "policy-root", Role: common.RoleRootUser,
		Status: common.UserStatusEnabled, AuthVersion: 1, Group: "default"}
	require.NoError(t, launch.db.Create(&root).Error)
	rootSession := model.UserSession{SID: uuid.NewString(), UserID: root.Id, UserAuthVersion: 1, Version: 1,
		Status: model.UserSessionStatusActive, RefreshHash: "unusable-root-fixture",
		ExpiresAt: launch.options.Now().Add(time.Hour).Unix()}
	require.NoError(t, launch.db.Create(&rootSession).Error)
	publisher := AuthIdentity{UserID: root.Id, SessionID: rootSession.SID, UserAuthVersion: 1, SessionVersion: 1}
	policy, err := PublishAppModelInvokePolicy(t.Context(), launch.db, publisher, []byte(appExecutionPolicyJSON), launch.options.Now())
	require.NoError(t, err)
	f := &appExecutionFixture{appLaunchFixture: launch, execution: NewAppExecutionService(launch.db, launch.options), publisher: publisher}
	f.appSession = launch.exchange(t)
	f.request = AppExecutionGrantRequest{
		RequestID: uuid.NewString(), AppKey: launch.installation.AppKey, AppSessionID: f.appSession.AppSessionID,
		Subject: f.appSession.Subject, RunID: uuid.NewString(), ExecutionRequestID: uuid.NewString(),
		Operation: "model.generate", ModelPolicyVersion: policy,
		RequestedModels: []string{appExecutionTestModel}, Strategy: "stable", ConversionPolicy: "strict",
		PassthroughSelections: []AppPassthroughSelection{},
	}
	return f
}

func TestExecutionGrantBindsRegisteredAssetInputs(t *testing.T) {
	f := newAppExecutionFixture(t)
	assetExpiry := f.options.Now().Add(4 * time.Minute)
	asset := appExecutionTestAsset(f.request.Subject, "openai_video",
		"/content/1/video_url/url", "video", "asset://video_01", assetExpiry)
	request := appExecutionRequestWithAssets(t, f.request, []map[string]any{asset})

	result, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, request)
	require.NoError(t, err)
	assert.Equal(t, assetExpiry, result.ExpiresAt)

	rawResult, err := common.Marshal(result)
	require.NoError(t, err)
	var resultJSON map[string]any
	require.NoError(t, common.Unmarshal(rawResult, &resultJSON))
	effective, ok := resultJSON["effective_asset_inputs"].([]any)
	require.True(t, ok)
	require.Len(t, effective, 1)
	entry, ok := effective[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "/content/1/video_url/url", entry["json_pointer"])
	assert.Equal(t, "video", entry["media_type"])
	assert.Equal(t, appExecutionTestModel, entry["public_model"])
	assert.Equal(t, "openai_video", entry["protocol"])
	assert.Equal(t, serviceDigest("asset://video_01"), entry["asset_uri_sha256"])
	assert.NotEmpty(t, resultJSON["asset_snapshot_sha256"])
	assert.NotContains(t, string(rawResult), "asset://video_01")
	assert.NotContains(t, string(rawResult), strings.Repeat("a", 64))

	var stored model.AppExecutionGrant
	require.NoError(t, f.db.Where("grant_id = ?", result.GrantID).First(&stored).Error)
	var binding map[string]any
	require.NoError(t, common.UnmarshalJsonStr(stored.BindingJSON, &binding))
	registered, ok := binding["registered_asset_inputs"].([]any)
	require.True(t, ok)
	require.Len(t, registered, 1)
	assert.Equal(t, "asset://video_01", registered[0].(map[string]any)["asset_uri"])
	assert.Equal(t, resultJSON["asset_snapshot_sha256"], binding["asset_snapshot_sha256"])
	assert.Equal(t, strings.Repeat("a", 64), registered[0].(map[string]any)["authorization_sha256"])

	changedAsset := appExecutionTestAsset(f.request.Subject, "openai_video",
		"/content/1/video_url/url", "video", "asset://video_02", assetExpiry)
	changed := appExecutionRequestWithAssets(t, f.request, []map[string]any{changedAsset})
	_, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, changed)
	require.ErrorContains(t, err, "idempotency_conflict")
}

func TestValidateAppRegisteredAssetInputsScopesPointersByModelAndProtocol(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	first := model.AppRegisteredAssetInput{
		AuthorizationSHA256: strings.Repeat("a", 64),
		AssetURI:            "asset://model-a-image",
		JSONPointer:         "/input/0/content/1/image_url",
		MediaType:           "image",
		PublicModel:         "model-a",
		Protocol:            "openai_responses",
		OwnerSubject:        "subject",
		ProjectID:           "offline-project",
		Purpose:             "model.invoke",
		RoutingAccount:      "cxy",
		Status:              "active",
		ExpiresAt:           now.Add(time.Minute),
	}
	second := first
	second.AssetURI = "asset://model-b-image"
	second.PublicModel = "model-b"
	candidates := []model.AppExecutionModel{
		{PublicModel: "model-a", Protocol: "openai_responses", ExecutionKind: model.AppExecutionModelKindTaskBacked},
		{PublicModel: "model-b", Protocol: "openai_responses", ExecutionKind: model.AppExecutionModelKindTaskBacked},
	}

	effective, _, _, err := validateAppRegisteredAssetInputs(
		[]model.AppRegisteredAssetInput{first, second}, "subject", candidates, now)

	require.NoError(t, err)
	require.Len(t, effective, 2)
	assert.Equal(t, "model-a", effective[0].PublicModel)
	assert.Equal(t, "model-b", effective[1].PublicModel)
}

func TestExecutionGrantRejectsTamperedFrozenSnapshot(t *testing.T) {
	pluginEnabled, grantsEnabled := operation_setting.AppPluginV1Enabled, operation_setting.AppExecutionGrantsEnabled
	operation_setting.AppPluginV1Enabled, operation_setting.AppExecutionGrantsEnabled = true, true
	t.Cleanup(func() {
		operation_setting.AppPluginV1Enabled, operation_setting.AppExecutionGrantsEnabled = pluginEnabled, grantsEnabled
	})
	tests := []struct {
		name   string
		column string
		tamper func(*testing.T, model.AppExecutionGrant) string
	}{
		{"models", "models_json", func(t *testing.T, grant model.AppExecutionGrant) string {
			var models []model.AppExecutionModel
			require.NoError(t, common.UnmarshalJsonStr(grant.ModelsJSON, &models))
			require.NotEmpty(t, models)
			models[0].ActualModel = "tampered-model"
			raw, err := common.Marshal(models)
			require.NoError(t, err)
			return string(raw)
		}},
		{"passthrough", "passthrough_json", func(t *testing.T, grant model.AppExecutionGrant) string {
			var rules []AppPassthroughRule
			require.NoError(t, common.UnmarshalJsonStr(grant.PassthroughJSON, &rules))
			rules = append(rules, AppPassthroughRule{
				PublicModel: appExecutionTestModel, PluginKey: "doubao", PluginVersion: "1.2.0",
				Protocol: "openai_video", JSONPointer: "/tampered", RuleVersion: 1,
				Schema: map[string]any{"type": "boolean"},
			})
			raw, err := common.Marshal(rules)
			require.NoError(t, err)
			return string(raw)
		}},
		{"funding", "funding_snapshot_json", func(t *testing.T, grant model.AppExecutionGrant) string {
			var funding model.AppExecutionFunding
			require.NoError(t, common.UnmarshalJsonStr(grant.FundingSnapshotJSON, &funding))
			funding.Reference = "wallet:user:999999"
			raw, err := common.Marshal(funding)
			require.NoError(t, err)
			return string(raw)
		}},
		{"response", "response_json", func(t *testing.T, grant model.AppExecutionGrant) string {
			var response AppExecutionGrantResult
			require.NoError(t, common.UnmarshalJsonStr(grant.ResponseJSON, &response))
			response.Strategy = "tampered"
			raw, err := common.Marshal(response)
			require.NoError(t, err)
			return string(raw)
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			for _, variant := range []struct {
				name   string
				tamper func(*testing.T, model.AppExecutionGrant) string
			}{
				{"changed", testCase.tamper},
				{"malformed", func(*testing.T, model.AppExecutionGrant) string { return "{" }},
			} {
				t.Run(variant.name, func(t *testing.T) {
					f := newAppExecutionFixture(t)
					result, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
					require.NoError(t, err)
					var grant model.AppExecutionGrant
					require.NoError(t, f.db.Where("grant_id = ?", result.GrantID).First(&grant).Error)
					_, _, err = f.execution.AuthenticateAppRelay(t.Context(), result.GrantToken,
						"openai_video", []byte(`{"model":"`+appExecutionTestModel+`","prompt":"test"}`))
					require.NoError(t, err)
					require.NoError(t, f.db.Model(&grant).Update(testCase.column, variant.tamper(t, grant)).Error)

					_, _, err = f.execution.AuthenticateAppRelay(t.Context(), result.GrantToken,
						"openai_video", []byte(`{"model":"`+appExecutionTestModel+`","prompt":"test"}`))
					require.ErrorContains(t, err, "invalid_grant")

					replay, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
					assert.ErrorContains(t, err, "invalid_grant")
					assert.Empty(t, replay)
				})
			}
		})
	}
}

func TestExecutionGrantRejectsInvalidRegisteredAssetInputs(t *testing.T) {
	tests := []struct {
		name   string
		assets func(*appExecutionFixture) []map[string]any
	}{
		{"owner", func(f *appExecutionFixture) []map[string]any {
			asset := appExecutionTestAsset("other-subject", "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			return []map[string]any{asset}
		}},
		{"status", func(f *appExecutionFixture) []map[string]any {
			asset := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			asset["status"] = "revoked"
			return []map[string]any{asset}
		}},
		{"purpose", func(f *appExecutionFixture) []map[string]any {
			asset := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			asset["purpose"] = "asset.read"
			return []map[string]any{asset}
		}},
		{"routing account", func(f *appExecutionFixture) []map[string]any {
			asset := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			asset["routing_account"] = "support"
			return []map[string]any{asset}
		}},
		{"model", func(f *appExecutionFixture) []map[string]any {
			asset := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			asset["public_model"] = "unapproved"
			return []map[string]any{asset}
		}},
		{"protocol", func(f *appExecutionFixture) []map[string]any {
			return []map[string]any{appExecutionTestAsset(f.request.Subject, "openai_responses",
				"/input/0/content/0/image_url", "image", "asset://image-1", f.options.Now().Add(time.Minute))}
		}},
		{"media pointer mismatch", func(f *appExecutionFixture) []map[string]any {
			return []map[string]any{appExecutionTestAsset(f.request.Subject, "openai_video",
				"/content/0/video_url/url", "image", "asset://image-1", f.options.Now().Add(time.Minute))}
		}},
		{"invalid authorization digest", func(f *appExecutionFixture) []map[string]any {
			asset := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			asset["authorization_sha256"] = strings.Repeat("A", 64)
			return []map[string]any{asset}
		}},
		{"invalid project", func(f *appExecutionFixture) []map[string]any {
			asset := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			asset["project_id"] = "bad project"
			return []map[string]any{asset}
		}},
		{"expired", func(f *appExecutionFixture) []map[string]any {
			return []map[string]any{appExecutionTestAsset(f.request.Subject, "openai_video",
				"/content/0/image_url/url", "image", "asset://image-1", f.options.Now())}
		}},
		{"fractional expiry", func(f *appExecutionFixture) []map[string]any {
			asset := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			asset["expires_at"] = f.options.Now().Add(time.Minute + time.Nanosecond).Format(time.RFC3339Nano)
			return []map[string]any{asset}
		}},
		{"duplicate pointer", func(f *appExecutionFixture) []map[string]any {
			first := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			second := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-2", f.options.Now().Add(time.Minute))
			return []map[string]any{first, second}
		}},
		{"duplicate registration", func(f *appExecutionFixture) []map[string]any {
			asset := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
				"image", "asset://image-1", f.options.Now().Add(time.Minute))
			return []map[string]any{asset, maps.Clone(asset)}
		}},
		{"too many", func(f *appExecutionFixture) []map[string]any {
			assets := make([]map[string]any, 65)
			for i := range assets {
				assets[i] = appExecutionTestAsset(f.request.Subject, "openai_video",
					fmt.Sprintf("/content/%d/image_url/url", i), "image",
					fmt.Sprintf("asset://image-%d", i), f.options.Now().Add(time.Minute))
			}
			return assets
		}},
	}
	for _, uri := range []string{
		"https://example.com/image.png",
		"data:image/png;base64,AAAA",
		"asset://",
		"asset://bad/path",
		"asset://bad?query",
		"asset://bad#fragment",
		"asset://bad%20id",
		" asset://image-1",
		"asset://" + strings.Repeat("x", 129),
	} {
		tests = append(tests, struct {
			name   string
			assets func(*appExecutionFixture) []map[string]any
		}{
			name: "uri " + uri,
			assets: func(f *appExecutionFixture) []map[string]any {
				return []map[string]any{appExecutionTestAsset(f.request.Subject, "openai_video",
					"/content/0/image_url/url", "image", uri, f.options.Now().Add(time.Minute))}
			},
		})
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			request := appExecutionRequestWithAssets(t, f.request, testCase.assets(f))
			result, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, request)
			require.ErrorContains(t, err, "invalid_request")
			assert.Empty(t, result.GrantID)
			var count int64
			require.NoError(t, f.db.Model(&model.AppExecutionGrant{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}

	t.Run("strict JSON", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		asset := appExecutionTestAsset(f.request.Subject, "openai_video", "/content/0/image_url/url",
			"image", "asset://image-1", f.options.Now().Add(time.Minute))
		raw, err := common.Marshal(f.request)
		require.NoError(t, err)
		var envelope map[string]any
		require.NoError(t, common.Unmarshal(raw, &envelope))
		envelope["registered_asset_inputs"] = []map[string]any{asset}
		raw, err = common.Marshal(envelope)
		require.NoError(t, err)
		for _, invalid := range [][]byte{
			bytes.Replace(raw, []byte(`"asset_uri":"asset://image-1"`),
				[]byte(`"asset_uri":"asset://image-1","asset_uri":"asset://image-1"`), 1),
			bytes.Replace(raw, []byte(`"asset_uri":"asset://image-1"`),
				[]byte(`"asset_uri":"asset://image-1","unknown":true`), 1),
		} {
			var request AppExecutionGrantRequest
			require.Error(t, DecodeAppExecutionGrantRequest(invalid, &request))
		}
	})
}

func TestExecutionGrantServerOwnsPayerPriceAndModelPool(t *testing.T) {
	f := newAppExecutionFixture(t)
	f.request.RequestedModels = append(f.request.RequestedModels, "unapproved-model")
	first, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
	require.NoError(t, err)
	require.NotEmpty(t, first.GrantID)
	grantUUID, err := uuid.Parse(first.GrantID)
	require.NoError(t, err, "public grant_id follows the UUID contract")
	assert.NotEqual(t, uuid.Nil, grantUUID)
	require.NotEmpty(t, first.GrantToken)
	assert.Equal(t, []string{appExecutionTestModel}, first.EffectiveModelPool)
	assert.Equal(t, "wallet", first.FundingSource)
	assert.NotEmpty(t, first.PayerRef)
	assert.NotEmpty(t, first.PriceVersion)
	assert.Equal(t, "stable", first.Strategy)
	assert.Equal(t, "strict", first.ConversionPolicy)
	assert.Empty(t, first.EffectivePassthrough)
	rawFirst, err := common.Marshal(first)
	require.NoError(t, err)
	assert.Contains(t, string(rawFirst), `"effective_asset_inputs":[]`,
		"omitted registrations are normalized to an empty effective array")
	assert.Contains(t, string(rawFirst), `"asset_snapshot_sha256":"`,
		"even an empty registration set has a bound snapshot digest")
	assert.True(t, first.ExpiresAt.After(f.options.Now()))
	assert.LessOrEqual(t, first.ExpiresAt.Sub(f.options.Now()), 15*time.Minute)
	f.now.Add(int64(time.Second))
	replay, err := NewAppExecutionService(f.db, f.options).IssueExecutionGrant(t.Context(), f.serviceID, f.request)
	require.NoError(t, err)
	assert.True(t, first.GrantToken == replay.GrantToken, "replay preserves the token without printing it")
	assert.Equal(t, first.ExpiresAt, replay.ExpiresAt)
	assert.Equal(t, first.GrantID, replay.GrantID)
	changed := f.request
	changed.RunID = uuid.NewString()
	_, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, changed)
	require.ErrorContains(t, err, "idempotency_conflict")

	require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": "ModelPrice"}).
		Update("value", `{"doubao-seedance-2-5-260628":0.02}`).Error)
	changed = f.request
	changed.RequestID = uuid.NewString()
	next, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, changed)
	require.NoError(t, err)
	assert.NotEqual(t, first.PriceVersion, next.PriceVersion, "hash actual price rules, not configured-only metadata")
	var stored model.AppExecutionGrant
	require.NoError(t, f.db.Where("grant_id = ?", first.GrantID).First(&stored).Error)
	assert.Equal(t, f.user.Id, stored.UserID)
	assert.Equal(t, first.PriceVersion, stored.PriceVersion)
	assert.NotEmpty(t, stored.ScopeHash)
	assert.NotEmpty(t, stored.TokenHash)
	assert.NotEmpty(t, stored.TokenSalt)
	var binding model.AppExecutionGrantBinding
	require.NoError(t, common.UnmarshalJsonStr(stored.BindingJSON, &binding))
	assert.Equal(t, f.serviceID.CredentialID, binding.IssuingCredentialID)
	assert.Equal(t, f.serviceID.Version, binding.IssuingCredentialVersion)
	assert.Equal(t, stored.ExpiresAt, binding.ExpiresAt)
	assert.Equal(t, stored.RequestHash, binding.RequestHash)
	assert.NotEmpty(t, binding.SnapshotSHA256)
	var models []model.AppExecutionModel
	require.NoError(t, common.UnmarshalJsonStr(stored.ModelsJSON, &models))
	require.Len(t, models, 1)
	loaded, ok := pluginruntime.DefaultRegistry.Get("doubao")
	require.True(t, ok)
	assert.Equal(t, loaded.SourceSHA256(), models[0].PluginSHA256)
	assert.Equal(t, appExecutionTestModel, models[0].ActualModel)
	assert.Equal(t, float64(0.01), models[0].Price["ModelPrice"])
	assert.Equal(t, float64(1), models[0].GroupRatio)
	assert.Positive(t, models[0].QuotaPerUnit)
	var rows []map[string]any
	require.NoError(t, f.db.Table("app_execution_grants").Find(&rows).Error)
	raw, err := common.Marshal(rows)
	require.NoError(t, err)
	assert.False(t, bytes.Contains(raw, []byte(first.GrantToken)), "usable grant tokens must never persist")
	var payer model.User
	require.NoError(t, f.db.First(&payer, f.user.Id).Error)
	assert.Equal(t, 1000000, payer.Quota, "issuance does not reserve or debit")
	assert.Nil(t, payer.AccessToken)
	var tokenCount int64
	require.NoError(t, f.db.Model(&model.Token{}).Count(&tokenCount).Error)
	assert.Zero(t, tokenCount, "App identity must not fabricate an API token")
}

const appExecutionPolicyJSON = `{"operations":{"model.generate":{
	"models":[{"public_model":"doubao-seedance-2-5-260628","actual_model":"doubao-seedance-2-5-260628",
	"plugin_key":"doubao","plugin_version":"1.2.0","protocol":"openai_video"}],
	"strategies":["stable"],"conversion_policies":["strict"],"passthrough_rules":[]}}}`

func TestAppExecutionPolicyPublicationRequiresCurrentRoot(t *testing.T) {
	f := newAppLaunchFixture(t)
	require.NoError(t, f.db.AutoMigrate(&model.Option{}))
	require.NoError(t, model.MigrateAppExecutionTables(f.db))
	version, err := PublishAppModelInvokePolicy(t.Context(), f.db, f.identity, []byte(appExecutionPolicyJSON), f.options.Now())
	require.NoError(t, err)
	require.NotEmpty(t, version)
	for _, raw := range []string{
		`{"operations":{},"operations":{}}`,
		`{"operations":{},"unexpected":true}`,
		`{"operations":{"model.generate":{"models":[],"strategies":["stable"],"conversion_policies":["strict"],
		"passthrough_rules":[{"public_model":"x","plugin_key":"doubao","plugin_version":"1.2.0","protocol":"openai_video",
		"json_pointer":"/metadata/%61uthorization","rule_version":1,"schema":{"type":"string"}}]}}}`,
	} {
		_, err := PublishAppModelInvokePolicy(t.Context(), f.db, f.identity, []byte(raw), f.options.Now())
		require.Error(t, err)
	}
	oldDB := model.DB
	model.DB = f.db
	t.Cleanup(func() { model.DB = oldDB })
	for _, key := range []string{"AppModelInvokePolicy", "AppArkImportDelegations"} {
		require.ErrorContains(t, model.UpdateOption(key, "{}"), "controlled")
		require.ErrorContains(t, model.UpdateOptionsBulk(map[string]string{key: "{}", "unrelated": "value"}), "controlled")
	}
	var unrelated int64
	require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": "unrelated"}).Count(&unrelated).Error)
	assert.Zero(t, unrelated)
	require.NoError(t, f.db.Model(&f.user).Update("role", common.RoleCommonUser).Error)
	_, err = PublishAppModelInvokePolicy(t.Context(), f.db, f.identity, []byte(`{"operations":{}}`), f.options.Now())
	require.ErrorContains(t, err, "forbidden")
	require.NoError(t, f.db.Model(&f.user).Update("role", common.RoleRootUser).Error)
	require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", f.session.SID).Update("status", model.UserSessionStatusRevoked).Error)
	_, err = PublishAppModelInvokePolicy(t.Context(), f.db, f.identity, []byte(`{"operations":{}}`), f.options.Now())
	require.Error(t, err)
	var head model.Option
	require.NoError(t, f.db.Where(map[string]any{"key": "AppModelInvokePolicy"}).First(&head).Error)
	assert.Equal(t, strconv.FormatInt(version, 10), head.Value, "invalid publication must not advance the immutable policy head")
}

func TestAppExecutionPolicyGenericWritersRejectAliases(t *testing.T) {
	for _, key := range []string{model.AppModelInvokePolicyKey, model.AppArkImportDelegationsKey} {
		for _, alias := range []struct {
			name string
			key  string
		}{
			{"case", strings.ToLower(key)},
			{"trailing-space", key + " "},
			{"accent", "\u00c1" + key[1:]},
		} {
			for _, bulk := range []bool{false, true} {
				for _, state := range []string{"published", "unpublished", "preexisting-alias"} {
					t.Run(fmt.Sprintf("%s/%s/bulk=%t/%s", key, alias.name, bulk, state), func(t *testing.T) {
						f := newAppLaunchFixture(t)
						require.NoError(t, f.db.AutoMigrate(&model.Option{}))
						require.NoError(t, model.MigrateAppExecutionTables(f.db))
						previousDB := model.DB
						model.DB = f.db
						t.Cleanup(func() { model.DB = previousDB })
						common.OptionMapRWMutex.Lock()
						previousMap := common.OptionMap
						common.OptionMap = map[string]string{}
						common.OptionMapRWMutex.Unlock()
						t.Cleanup(func() {
							common.OptionMapRWMutex.Lock()
							common.OptionMap = previousMap
							common.OptionMapRWMutex.Unlock()
						})

						// Ask the real key column, not Go case folding or a SQL literal's collation.
						require.NoError(t, f.db.Create(&model.Option{Key: key, Value: "0"}).Error)
						var matches int64
						require.NoError(t, f.db.Model(&model.Option{}).
							Where(clause.Eq{Column: "key", Value: alias.key}).Count(&matches).Error)
						require.NoError(t, f.db.Where(clause.Eq{Column: "key", Value: key}).Delete(&model.Option{}).Error)
						t.Logf("option key equivalence: canonical=%q alias=%q matches=%d", key, alias.key, matches)
						if (alias.name == "accent" || state == "preexisting-alias") && matches == 0 {
							t.Log("collation-specific scenario does not apply to this key column")
							return
						}
						publish := func(raw string) (int64, error) {
							if key == model.AppModelInvokePolicyKey {
								return PublishAppModelInvokePolicy(t.Context(), f.db, f.identity, []byte(raw), f.options.Now())
							}
							return PublishAppArkImportDelegations(t.Context(), f.db, f.identity, []byte(raw), f.options.Now())
						}
						allowed, denied := appExecutionPolicyJSON, `{"operations":{}}`
						if key == model.AppArkImportDelegationsKey {
							raw, err := common.Marshal(AppArkImportDelegations{Delegations: []AppArkImportDelegation{{
								InstallationID: f.installation.InstallationID, UserID: f.user.Id,
								AccountRef: "offline-account", ProjectID: "offline-project",
								StartAt: f.options.Now(), EndAt: f.options.Now().Add(time.Hour),
							}}})
							require.NoError(t, err)
							allowed, denied = string(raw), `{"delegations":[]}`
						}
						if state == "preexisting-alias" {
							require.NoError(t, f.db.Create(&model.Option{Key: alias.key, Value: "0"}).Error)
						}
						if state != "unpublished" {
							version, err := publish(allowed)
							require.NoError(t, err)
							require.Equal(t, int64(1), version)
							version, err = publish(denied)
							require.NoError(t, err)
							require.Equal(t, int64(2), version)
							common.OptionMapRWMutex.Lock()
							common.OptionMap[key] = "2"
							common.OptionMapRWMutex.Unlock()
						}
						require.ErrorContains(t, f.db.Transaction(func(tx *gorm.DB) error {
							_, err := model.PublishAppExecutionPolicyTx(tx, alias.key, allowed, f.user.Id, f.options.Now())
							return err
						}), "invalid_policy", "publication must still require an exact canonical key")

						write := func(values map[string]string) error {
							if bulk {
								return model.UpdateOptionsBulk(values)
							}
							for k, v := range values {
								if err := model.UpdateOption(k, v); err != nil {
									return err
								}
							}
							return nil
						}
						require.NoError(t, write(map[string]string{"quality-unrelated": "before"}))
						require.NoError(t, write(map[string]string{"quality-unrelated": "stable"}))
						var before []model.Option
						require.NoError(t, f.db.Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).Find(&before).Error)
						common.OptionMapRWMutex.RLock()
						beforeMap := maps.Clone(common.OptionMap)
						common.OptionMapRWMutex.RUnlock()
						assert.Equal(t, "stable", beforeMap["quality-unrelated"], "normal generic writes still update cache")
						var versions int64
						require.NoError(t, f.db.Model(&model.AppExecutionPolicyVersion{}).Count(&versions).Error)

						values := map[string]string{alias.key: "1"}
						if bulk {
							values["quality-unrelated"] = "changed"
							values["quality-new-unrelated"] = "new"
						}
						assert.ErrorContains(t, write(values), "controlled")
						var after []model.Option
						require.NoError(t, f.db.Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).Find(&after).Error)
						assert.Equal(t, before, after, "no head rollback, orphan alias, or partial generic write")
						common.OptionMapRWMutex.RLock()
						afterMap := maps.Clone(common.OptionMap)
						common.OptionMapRWMutex.RUnlock()
						assert.Equal(t, beforeMap, afterMap, "rejection must not update OptionMap")
						var afterVersions int64
						require.NoError(t, f.db.Model(&model.AppExecutionPolicyVersion{}).Count(&afterVersions).Error)
						assert.Equal(t, versions, afterVersions)
						if state != "unpublished" {
							head, err := model.GetAppExecutionPolicyTx(f.db, key)
							require.NoError(t, err)
							assert.Equal(t, int64(2), head.Version, "generic aliases must not reactivate allowed v1")
							assert.JSONEq(t, denied, head.CanonicalJSON)
						}
					})
				}
			}
		}
	}
}

func TestAppExecutionNumericVersionsAndImmutableUnknownFieldRules(t *testing.T) {
	f := newAppLaunchFixture(t)
	require.NoError(t, f.db.AutoMigrate(&model.Option{}))
	require.NoError(t, model.MigrateAppExecutionTables(f.db))
	var policy AppModelInvokePolicy
	require.NoError(t, common.Unmarshal([]byte(appExecutionPolicyJSON), &policy))
	op := policy.Operations["model.generate"]
	rule := AppPassthroughRule{PublicModel: appExecutionTestModel, PluginKey: "doubao", PluginVersion: "1.2.0",
		Protocol: "openai_video", JSONPointer: "/vendor_option", RuleVersion: 3,
		Schema: map[string]any{"type": "boolean"}}
	op.PassthroughRules = []AppPassthroughRule{rule}
	policy.Operations["model.generate"] = op
	publish := func() (int64, error) {
		raw, err := common.Marshal(policy)
		require.NoError(t, err)
		return PublishAppModelInvokePolicy(t.Context(), f.db, f.identity, raw, f.options.Now())
	}
	first, err := publish()
	require.NoError(t, err, "explicit root admission can allow a benign unknown primitive")
	second, err := publish()
	require.NoError(t, err)
	assert.Equal(t, first+1, second)
	for _, pointer := range []string{"/authorization", "/metadata/%61uthorization", "/metadata/headers",
		"/metadata/api_key", "/metadata/base_url", "/metadata/model", "/metadata/host",
		"/metadata/\\u0068eaders", "/metadata/~1headers", "/execution_request_id", "/callback_url",
		"/baseurl", "/metadata/apikey", "/metadata/accesskey", "/metadata/callbackurl",
		"/modelname", "/subjectid", "/runid", "/appkey", "/requestbody", "/timeoutms", "/priceversion"} {
		op.PassthroughRules[0].JSONPointer = pointer
		policy.Operations["model.generate"] = op
		_, err = publish()
		require.Error(t, err, pointer)
	}
	op.PassthroughRules[0] = rule
	op.PassthroughRules[0].Schema = map[string]any{"type": "integer", "minimum": float64(0), "maximum": float64(5)}
	policy.Operations["model.generate"] = op
	_, err = publish()
	require.ErrorContains(t, err, "immutable_rule")
	op.PassthroughRules[0].RuleVersion = 4
	policy.Operations["model.generate"] = op
	third, err := publish()
	require.NoError(t, err)
	assert.Equal(t, second+1, third, "rejections roll back the policy version")

	requestJSON := `{"request_id":"d2a2e019-0c3b-4f62-a5ed-43fc972ba1a9","app_key":"test",
	"app_session_id":"session","subject":"subject","run_id":"c3ddbe3f-5f80-4b12-bf36-7ec08b3c0911",
	"execution_request_id":"cbb8e8e1-67f3-4cb8-8944-0029f5e69cec","operation":"model.generate",
	"model_policy_version":7,"requested_models":["test"],"strategy":"stable","conversion_policy":"strict",
	"passthrough_selections":[{"json_pointer":"/vendor_option","rule_version":3}]}`
	var request AppExecutionGrantRequest
	require.NoError(t, DecodeAppExecutionGrantRequest([]byte(requestJSON), &request))
	assert.Equal(t, int64(7), request.ModelPolicyVersion)
	assert.Equal(t, int64(3), request.PassthroughSelections[0].RuleVersion)
	for _, raw := range []string{
		string(bytes.ReplaceAll([]byte(requestJSON), []byte(`"model_policy_version":7`), []byte(`"model_policy_version":"7"`))),
		string(bytes.ReplaceAll([]byte(requestJSON), []byte(`"rule_version":3`), []byte(`"rule_version":"3"`))),
		string(bytes.ReplaceAll([]byte(requestJSON), []byte(`"strategy":"stable"`), []byte(`"strategy":"stable","strategy":"stable"`))),
	} {
		require.Error(t, DecodeAppExecutionGrantRequest([]byte(raw), &request))
	}
	response, err := common.Marshal(AppEffectivePassthrough{JSONPointer: "/vendor_option", RuleVersion: 3, SchemaSHA256: "digest"})
	require.NoError(t, err)
	assert.Contains(t, string(response), `"rule_version":3`)
}

func TestAppExecutionPassthroughRulesUseProtocolIdentity(t *testing.T) {
	f := newAppExecutionFixture(t)
	var policy AppModelInvokePolicy
	require.NoError(t, common.UnmarshalJsonStr(appExecutionPolicyJSON, &policy))
	op := policy.Operations["model.generate"]
	responses := op.Models[0]
	responses.Protocol = "openai_responses"
	op.Models = append(op.Models, responses)
	rules := []AppPassthroughRule{
		{PublicModel: appExecutionTestModel, PluginKey: "doubao", PluginVersion: "1.2.0",
			Protocol: "openai_video", JSONPointer: "/vendor_option", RuleVersion: 3, Schema: map[string]any{"type": "boolean"}},
		{PublicModel: appExecutionTestModel, PluginKey: "doubao", PluginVersion: "1.2.0",
			Protocol: "openai_responses", JSONPointer: "/vendor_option", RuleVersion: 3, Schema: map[string]any{"type": "boolean"}},
	}
	publish := func(selected []AppPassthroughRule) (int64, error) {
		op.PassthroughRules = selected
		policy.Operations["model.generate"] = op
		raw, err := common.Marshal(policy)
		require.NoError(t, err)
		return PublishAppModelInvokePolicy(t.Context(), f.db, f.publisher, raw, f.options.Now())
	}
	version, err := publish(rules)
	require.NoError(t, err, "the same public field can be admitted for both native protocols")
	require.Equal(t, int64(2), version)
	f.request.ModelPolicyVersion = version
	f.request.PassthroughSelections = []AppPassthroughSelection{{JSONPointer: "/vendor_option", RuleVersion: 3}}
	result, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
	require.NoError(t, err)
	schemaHash, err := appPluginHash(map[string]any{"type": "boolean"})
	require.NoError(t, err)
	assert.Equal(t, []AppEffectivePassthrough{{JSONPointer: "/vendor_option", RuleVersion: 3, SchemaSHA256: schemaHash}},
		result.EffectivePassthrough)
	var grant model.AppExecutionGrant
	require.NoError(t, f.db.Where("grant_id = ?", result.GrantID).First(&grant).Error)
	var candidates []model.AppExecutionModel
	require.NoError(t, common.UnmarshalJsonStr(grant.ModelsJSON, &candidates))
	require.Len(t, candidates, 2, "issuance must actually intersect both protocol admissions")
	assert.ElementsMatch(t, []string{"openai_video", "openai_responses"}, []string{candidates[0].Protocol, candidates[1].Protocol})
	var storedRules []AppPassthroughRule
	require.NoError(t, common.UnmarshalJsonStr(grant.PassthroughJSON, &storedRules))
	assert.ElementsMatch(t, rules, storedRules)

	for _, rejection := range []string{"duplicate identity", "immutable schema", "plugin version"} {
		t.Run(rejection, func(t *testing.T) {
			changed := slices.Clone(rules)
			expected := "invalid_policy"
			switch rejection {
			case "duplicate identity":
				changed = append(changed, rules[0])
			case "immutable schema":
				changed[0].Schema = map[string]any{"type": "integer", "minimum": float64(0), "maximum": float64(5)}
				expected = "immutable_rule"
			case "plugin version":
				changed[0].PluginVersion = "1.2.1"
			}
			_, err := publish(changed)
			require.ErrorContains(t, err, expected)
			head, err := model.GetAppExecutionPolicyTx(f.db, model.AppModelInvokePolicyKey)
			require.NoError(t, err)
			assert.Equal(t, version, head.Version)
			var policyCount, ruleCount int64
			require.NoError(t, f.db.Model(&model.AppExecutionPolicyVersion{}).Count(&policyCount).Error)
			require.NoError(t, f.db.Model(&model.AppPassthroughRuleVersion{}).Count(&ruleCount).Error)
			assert.Equal(t, int64(2), policyCount, "failed publication leaves no policy version")
			assert.Equal(t, int64(2), ruleCount, "failed publication leaves no immutable rule")
		})
	}

	nextRules := slices.Clone(rules)
	for i := range nextRules {
		nextRules[i].RuleVersion = 4
	}
	version, err = publish(append(slices.Clone(rules), nextRules...))
	require.NoError(t, err, "numeric rule_version is part of the identity")
	assert.Equal(t, int64(3), version)
	f.request.ModelPolicyVersion = version
	f.request.RequestID = uuid.NewString()
	f.request.PassthroughSelections[0].RuleVersion = 4
	result, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
	require.NoError(t, err)
	assert.Equal(t, []AppEffectivePassthrough{{JSONPointer: "/vendor_option", RuleVersion: 4, SchemaSHA256: schemaHash}},
		result.EffectivePassthrough)

	for i := range nextRules {
		nextRules[i].RuleVersion = 5
	}
	nextRules[1].Schema = map[string]any{"type": "integer", "minimum": float64(0), "maximum": float64(5)}
	version, err = publish(nextRules)
	require.NoError(t, err, "distinct protocol identities may have distinct immutable schemas")
	assert.Equal(t, int64(4), version)
	f.request.ModelPolicyVersion = version
	f.request.RequestID = uuid.NewString()
	f.request.PassthroughSelections[0].RuleVersion = 5
	result, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
	assert.ErrorContains(t, err, "scope_denied", "one public effective field cannot represent two schema hashes")
	assert.Empty(t, result.GrantID)
	assert.Empty(t, result.EffectivePassthrough)
	var grantCount int64
	require.NoError(t, f.db.Model(&model.AppExecutionGrant{}).Count(&grantCount).Error)
	assert.Equal(t, int64(2), grantCount, "incompatible selection must not persist a grant")
}

func TestAppExecutionPassthroughRejectsTransportControls(t *testing.T) {
	f := newAppExecutionFixture(t)
	var policy AppModelInvokePolicy
	require.NoError(t, common.UnmarshalJsonStr(appExecutionPolicyJSON, &policy))
	op := policy.Operations["model.generate"]
	op.PassthroughRules = []AppPassthroughRule{{PublicModel: appExecutionTestModel, PluginKey: "doubao",
		PluginVersion: "1.2.0", Protocol: "openai_video", JSONPointer: "/transport_field", RuleVersion: 1,
		Schema: map[string]any{"type": "integer", "minimum": float64(1), "maximum": float64(65535)}}}
	policy.Operations["model.generate"] = op
	template, err := common.Marshal(policy)
	require.NoError(t, err)
	for _, field := range []string{"scheme", "port", "query", "dns"} {
		for _, encoding := range []struct {
			name        string
			pointerJSON string
		}{
			{"top-level", `"/` + field + `"`},
			{"metadata", `"/metadata/` + field + `"`},
			{"nested-container", `"/` + field + `/vendor_option"`},
			{"deep", `"/metadata/nested/` + field + `"`},
			{"json-escaped", fmt.Sprintf(`"/\u%04x%s"`, field[0], field[1:])},
			{"metadata-json-escaped", fmt.Sprintf(`"/metadata/\u%04x%s"`, field[0], field[1:])},
			{"percent-encoded", fmt.Sprintf(`"/metadata/%%%02x%s"`, field[0], field[1:])},
			{"double-percent-encoded", fmt.Sprintf(`"/metadata/%%25%02x%s"`, field[0], field[1:])},
			{"pointer-escaped", `"/metadata/~1` + field + `"`},
			{"pointer-tilde-escaped", `"/metadata/~0` + field + `"`},
		} {
			t.Run(field+"/"+encoding.name, func(t *testing.T) {
				raw := bytes.Replace(template, []byte(`"/transport_field"`), []byte(encoding.pointerJSON), 1)
				head, err := model.GetAppExecutionPolicyTx(f.db, model.AppModelInvokePolicyKey)
				require.NoError(t, err)
				_, err = PublishAppModelInvokePolicy(t.Context(), f.db, f.publisher, raw, f.options.Now())
				assert.ErrorContains(t, err, "invalid_policy", "publication must reject transport controls")
				after, err := model.GetAppExecutionPolicyTx(f.db, model.AppModelInvokePolicyKey)
				require.NoError(t, err)
				assert.Equal(t, head.Version, after.Version, "rejected publication must not advance the head")

				// Seed an older policy through storage to exercise issuance independently of publication.
				require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
					var err error
					f.request.ModelPolicyVersion, err = model.PublishAppExecutionPolicyTx(tx,
						model.AppModelInvokePolicyKey, string(raw), f.publisher.UserID, f.options.Now())
					return err
				}))
				var pointer string
				require.NoError(t, common.UnmarshalJsonStr(encoding.pointerJSON, &pointer))
				f.request.RequestID = uuid.NewString()
				f.request.PassthroughSelections = []AppPassthroughSelection{{JSONPointer: pointer, RuleVersion: 1}}
				var beforeCount int64
				require.NoError(t, f.db.Model(&model.AppExecutionGrant{}).Count(&beforeCount).Error)
				result, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
				assert.ErrorContains(t, err, "scope_denied", "stored transport rules must not enter a grant")
				assert.Empty(t, result.GrantID)
				assert.Empty(t, result.EffectivePassthrough)
				var afterCount int64
				require.NoError(t, f.db.Model(&model.AppExecutionGrant{}).Count(&afterCount).Error)
				assert.Equal(t, beforeCount, afterCount, "denied selection must not persist a grant")
			})
		}
	}
	t.Run("vendor option remains allowed", func(t *testing.T) {
		raw := bytes.Replace(template, []byte(`"/transport_field"`), []byte(`"/vendor_option"`), 1)
		var err error
		f.request.ModelPolicyVersion, err = PublishAppModelInvokePolicy(t.Context(), f.db, f.publisher, raw, f.options.Now())
		require.NoError(t, err)
		f.request.RequestID = uuid.NewString()
		f.request.PassthroughSelections = []AppPassthroughSelection{{JSONPointer: "/vendor_option", RuleVersion: 1}}
		result, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
		require.NoError(t, err)
		require.Len(t, result.EffectivePassthrough, 1)
		assert.Equal(t, "/vendor_option", result.EffectivePassthrough[0].JSONPointer)
		assert.Equal(t, int64(1), result.EffectivePassthrough[0].RuleVersion)
		assert.NotEmpty(t, result.EffectivePassthrough[0].SchemaSHA256)
	})
}

func TestAppExecutionGroupRatioAliasesFreezeSnapshots(t *testing.T) {
	for _, key := range []string{"GroupGroupRatio", "group_ratio_setting.group_group_ratio"} {
		t.Run(key, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": "GroupRatio"}).
				Update("value", `{"default":2}`).Error)
			require.NoError(t, f.db.Create(&model.Option{Key: key, Value: `{"default":{"default":0.5}}`}).Error)
			first, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
			require.NoError(t, err)
			var stored model.AppExecutionGrant
			require.NoError(t, f.db.Where("grant_id = ?", first.GrantID).First(&stored).Error)
			var candidates []model.AppExecutionModel
			require.NoError(t, common.UnmarshalJsonStr(stored.ModelsJSON, &candidates))
			require.Len(t, candidates, 1)
			assert.Equal(t, 0.5, candidates[0].GroupRatio, "the database override replaces the base ratio")

			require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": key}).
				Update("value", `{"default":{"default":0.25}}`).Error)
			nextRequest := f.request
			nextRequest.RequestID = uuid.NewString()
			next, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, nextRequest)
			require.NoError(t, err)
			assert.NotEqual(t, first.PriceVersion, next.PriceVersion, "override changes must change price_version")
			var nextStored model.AppExecutionGrant
			require.NoError(t, f.db.Where("grant_id = ?", next.GrantID).First(&nextStored).Error)
			candidates = nil
			require.NoError(t, common.UnmarshalJsonStr(nextStored.ModelsJSON, &candidates))
			require.Len(t, candidates, 1)
			assert.Equal(t, 0.25, candidates[0].GroupRatio)

			replay, err := NewAppExecutionService(f.db, f.options).IssueExecutionGrant(t.Context(), f.serviceID, f.request)
			require.NoError(t, err)
			assert.True(t, first.GrantToken == replay.GrantToken, "replay preserves the token without printing it")
			first.GrantToken, replay.GrantToken = "", ""
			assert.Equal(t, first, replay, "old replay must not reprice or renew")
			var replayStored model.AppExecutionGrant
			require.NoError(t, f.db.Where("grant_id = ?", first.GrantID).First(&replayStored).Error)
			assert.Equal(t, stored, replayStored, "all persisted grant snapshots remain immutable")
		})
	}
	for _, tc := range []struct {
		name     string
		options  []model.Option
		conflict bool
	}{
		{"equivalent aliases", []model.Option{
			{Key: "group_ratio_setting.group_ratio", Value: `{"default":1.0}`},
			{Key: "GroupGroupRatio", Value: `{"default":{"default":0.5,"unused":3}}`},
			{Key: "group_ratio_setting.group_group_ratio", Value: `{ "default": { "unused": 3.0, "default": 0.50 } }`},
		}, false},
		{"base ratio conflict", []model.Option{
			{Key: "group_ratio_setting.group_ratio", Value: `{"default":2}`},
		}, true},
		{"base ratio disjoint aliases", []model.Option{
			{Key: "group_ratio_setting.group_ratio", Value: `{"other":2}`},
		}, true},
		{"override conflict", []model.Option{
			{Key: "GroupGroupRatio", Value: `{"default":{"default":0.5}}`},
			{Key: "group_ratio_setting.group_group_ratio", Value: `{"default":{"default":0.25}}`},
		}, true},
		{"override disjoint aliases", []model.Option{
			{Key: "GroupGroupRatio", Value: `{"default":{"default":0.5}}`},
			{Key: "group_ratio_setting.group_group_ratio", Value: `{"other":{"default":0.5}}`},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			require.NoError(t, f.db.Create(&tc.options).Error)
			result, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
			if tc.conflict {
				assert.ErrorContains(t, err, "invalid_group_policy")
				assert.Empty(t, result.GrantID)
				var count int64
				require.NoError(t, f.db.Model(&model.AppExecutionGrant{}).Count(&count).Error)
				assert.Zero(t, count, "conflicting option aliases must not mint a grant")
				return
			}
			require.NoError(t, err, "equivalent aliases are compared by parsed values, not JSON spelling")
			var stored model.AppExecutionGrant
			require.NoError(t, f.db.Where("grant_id = ?", result.GrantID).First(&stored).Error)
			var candidates []model.AppExecutionModel
			require.NoError(t, common.UnmarshalJsonStr(stored.ModelsJSON, &candidates))
			require.Len(t, candidates, 1)
			assert.Equal(t, 0.5, candidates[0].GroupRatio)
		})
	}
}

func TestIssueExecutionGrantCurrentAuthorityAndFunding(t *testing.T) {
	for _, mode := range []string{"redis", "batch", "subscription_only", "subscription_first", "overflow_disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			switch mode {
			case "redis":
				common.RedisEnabled = true
			case "batch":
				common.BatchUpdateEnabled = true
			case "subscription_only":
				require.NoError(t, f.db.Model(&f.user).Update("setting", `{"billing_preference":"subscription_only"}`).Error)
			default:
				sub := model.UserSubscription{UserId: f.user.Id, Status: "active",
					StartTime: f.options.Now().Add(-time.Hour).Unix(), EndTime: f.options.Now().Add(time.Hour).Unix(),
					AmountTotal: 1000, AllowWalletOverflow: mode != "overflow_disabled"}
				require.NoError(t, f.db.Create(&sub).Error)
				if mode == "overflow_disabled" {
					require.NoError(t, f.db.Model(&f.user).Update("setting", `{"billing_preference":"subscription_first"}`).Error)
				}
			}
			_, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
			require.Error(t, err)
			if mode == "redis" || mode == "batch" {
				require.ErrorContains(t, err, "wallet_accounting_unavailable")
			} else {
				require.ErrorContains(t, err, "funding_not_supported")
			}
			var count int64
			require.NoError(t, f.db.Model(&model.AppExecutionGrant{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
	for _, preference := range []string{"wallet_only", "wallet_first"} {
		t.Run("explicit "+preference, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			require.NoError(t, f.db.Model(&f.user).Update("setting", `{"billing_preference":"`+preference+`"}`).Error)
			require.NoError(t, f.db.Create(&model.UserSubscription{UserId: f.user.Id, Status: "active",
				StartTime: f.options.Now().Add(-time.Hour).Unix(), EndTime: f.options.Now().Add(time.Hour).Unix(),
				AmountTotal: 1000, AllowWalletOverflow: false}).Error)
			result, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
			require.NoError(t, err, "explicit funded wallet choice is not subscription overflow")
			assert.Equal(t, "wallet", result.FundingSource)
		})
	}
	t.Run("model invoke does not require identity read", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		credential, err := model.IssueAppServiceCredential(t.Context(), f.db, f.installation.InstallationID,
			[]string{"model.invoke"}, f.options.Now(), f.options.Now().Add(time.Hour), true)
		require.NoError(t, err)
		identity, err := model.AuthenticateAppServiceCredential(t.Context(), f.db, credential.Credential, f.options.Now())
		require.NoError(t, err)
		first, err := f.execution.IssueExecutionGrant(t.Context(), identity, f.request)
		require.NoError(t, err)
		options := f.options
		options.DerivationKey = []byte("different-test-only-shared-key-32-bytes")
		_, err = NewAppExecutionService(f.db, options).IssueExecutionGrant(t.Context(), identity, f.request)
		require.ErrorContains(t, err, "identity_not_configured")
		require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": "ModelPrice"}).Update("value", `{}`).Error)
		common.RedisEnabled = true
		replay, err := f.execution.IssueExecutionGrant(t.Context(), identity, f.request)
		require.NoError(t, err)
		assert.True(t, first.GrantToken == replay.GrantToken)
		assert.Equal(t, first.PriceVersion, replay.PriceVersion)
		assert.Equal(t, first.ExpiresAt, replay.ExpiresAt)
		require.NoError(t, f.db.Model(&model.AppPluginSession{}).Where("app_session_id = ?", f.request.AppSessionID).
			Update("revoked_at", f.options.Now().UnixNano()).Error)
		_, err = f.execution.IssueExecutionGrant(t.Context(), identity, f.request)
		require.Error(t, err, "replay still requires current authorization")
	})
	for _, change := range []string{"service", "scope", "session_scope", "user", "dashboard", "generation", "issuer",
		"expired", "installation", "allowed_group", "policy", "channel", "ability", "price"} {
		t.Run("current "+change, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			_, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
			require.NoError(t, err)
			switch change {
			case "service":
				require.NoError(t, f.db.Model(&model.AppServiceCredential{}).Where("credential_id = ?", f.serviceID.CredentialID).Update("status", "revoked").Error)
			case "scope":
				require.NoError(t, f.db.Model(&model.AppServiceCredentialBinding{}).Where("credential_id = ?", f.serviceID.CredentialID).Update("scopes", `["identity.read"]`).Error)
			case "session_scope":
				require.NoError(t, f.db.Model(&model.AppPluginSession{}).Where("app_session_id = ?", f.request.AppSessionID).Update("granted_scopes", `["identity.read"]`).Error)
			case "user":
				require.NoError(t, f.db.Model(&f.user).Update("auth_version", 2).Error)
			case "dashboard":
				require.NoError(t, f.db.Model(&model.UserSession{}).Where("sid = ?", f.session.SID).Update("status", model.UserSessionStatusRevoked).Error)
			case "generation", "issuer":
				require.NoError(t, f.db.Model(&model.AppPluginSession{}).Where("app_session_id = ?", f.request.AppSessionID).Update(change, "changed").Error)
			case "expired":
				require.NoError(t, f.db.Model(&model.AppPluginSession{}).Where("app_session_id = ?", f.request.AppSessionID).Update("upstream_expires_at", f.options.Now().Unix()).Error)
			case "installation":
				require.NoError(t, f.db.Model(&model.AppInstallation{}).Where("installation_id = ?", f.installation.InstallationID).Update("status", model.AppInstallationStatusDisabled).Error)
			case "allowed_group":
				require.NoError(t, f.db.Model(&model.AppInstallation{}).Where("installation_id = ?", f.installation.InstallationID).Update("allowed_user_policy", `{"groups":["other"]}`).Error)
			case "policy":
				_, err := PublishAppModelInvokePolicy(t.Context(), f.db, f.publisher, []byte(`{"operations":{}}`), f.options.Now())
				require.NoError(t, err)
				f.request.RequestID = uuid.NewString()
			case "channel":
				require.NoError(t, f.db.Model(&model.Channel{}).Where("name = ?", "host-approved").Update("status", common.ChannelStatusManuallyDisabled).Error)
				f.request.RequestID = uuid.NewString()
			case "ability":
				require.NoError(t, f.db.Model(&model.Ability{}).Where("model = ?", appExecutionTestModel).Update("enabled", false).Error)
				f.request.RequestID = uuid.NewString()
			case "price":
				require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": "ModelPrice"}).Update("value", `{}`).Error)
				f.request.RequestID = uuid.NewString()
			}
			_, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
			require.Error(t, err)
		})
	}
	t.Run("mapped task expression and current usable groups", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		var policy AppModelInvokePolicy
		require.NoError(t, common.UnmarshalJsonStr(appExecutionPolicyJSON, &policy))
		op := policy.Operations["model.generate"]
		op.Models[0].PublicModel = "host-video"
		op.PassthroughRules = []AppPassthroughRule{{PublicModel: "host-video", PluginKey: "doubao", PluginVersion: "1.2.0",
			Protocol: "openai_video", JSONPointer: "/vendor_option", RuleVersion: 3, Schema: map[string]any{"type": "boolean"}}}
		policy.Operations["model.generate"] = op
		raw, err := common.Marshal(policy)
		require.NoError(t, err)
		f.request.ModelPolicyVersion, err = PublishAppModelInvokePolicy(t.Context(), f.db, f.publisher, raw, f.options.Now())
		require.NoError(t, err)
		f.request.RequestedModels = []string{"host-video"}
		f.request.PassthroughSelections = []AppPassthroughSelection{{JSONPointer: "/vendor_option", RuleVersion: 3}}
		var channel model.Channel
		require.NoError(t, f.db.Where("name = ?", "host-approved").First(&channel).Error)
		require.NoError(t, f.db.Model(&channel).Updates(map[string]any{"models": "host-video", "group": "paid",
			"model_mapping": `{"host-video":"` + appExecutionTestModel + `"}`}).Error)
		require.NoError(t, f.db.Where("channel_id = ?", channel.Id).Delete(&model.Ability{}).Error)
		require.NoError(t, f.db.Create(&model.Ability{ChannelId: channel.Id, Group: "paid", Model: "host-video", Enabled: true}).Error)
		require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": "GroupRatio"}).Update("value", `{"default":1,"paid":2}`).Error)
		require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": "AutoGroups"}).Update("value", `["paid"]`).Error)
		require.NoError(t, f.db.Create(&[]model.Option{
			{Key: "group_ratio_setting.group_special_usable_group", Value: `{"default":{"+:paid":"Paid"}}`},
			{Key: "group_ratio_setting.group_group_ratio", Value: `{"default":{"paid":0.5}}`},
			{Key: "billing_setting.billing_mode", Value: `{"` + appExecutionTestModel + `":"tiered_expr"}`},
			{Key: "billing_setting.billing_expr", Value: `{"` + appExecutionTestModel + `":"v1:tier(\"base\", u(\"tokens\") * 9.8 / 1000000)"}`},
		}).Error)
		first, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
		require.NoError(t, err)
		require.Len(t, first.EffectivePassthrough, 1)
		assert.Equal(t, int64(3), first.EffectivePassthrough[0].RuleVersion)
		var stored model.AppExecutionGrant
		require.NoError(t, f.db.Where("grant_id = ?", first.GrantID).First(&stored).Error)
		var candidates []model.AppExecutionModel
		require.NoError(t, common.UnmarshalJsonStr(stored.ModelsJSON, &candidates))
		require.Len(t, candidates, 1)
		assert.Equal(t, "host-video", candidates[0].PublicModel)
		assert.Equal(t, appExecutionTestModel, candidates[0].ActualModel)
		assert.Equal(t, "task", candidates[0].BillingBasis)
		assert.Equal(t, 1, candidates[0].ExprVersion)
		assert.Equal(t, 0.5, candidates[0].GroupRatio)
		assert.Contains(t, candidates[0].UsageSchemaJSON, `"tokens"`)
		assert.NotContains(t, candidates[0].Price, "ModelPrice")
		require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": "ModelPrice"}).Update("value", `{"host-video":0.25}`).Error)
		priceMap, err := model.GetEffectiveModelPricingTx(f.db, []string{"host-video", "gpt-6-astra"})
		require.NoError(t, err)
		assert.Equal(t, float64(0.25), priceMap["host-video"]["ModelPrice"])
		assert.Equal(t, "tiered_expr", priceMap["gpt-6-astra"]["billing_setting.billing_mode"])
		f.request.RequestID = uuid.NewString()
		explicit, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
		require.NoError(t, err)
		stored = model.AppExecutionGrant{}
		require.NoError(t, f.db.Where("grant_id = ?", explicit.GrantID).First(&stored).Error)
		candidates = nil
		require.NoError(t, common.UnmarshalJsonStr(stored.ModelsJSON, &candidates))
		require.Len(t, candidates, 1)
		assert.Equal(t, float64(0.25), candidates[0].Price["ModelPrice"], "public administrator pricing wins over upstream expression")
		assert.Equal(t, "request", candidates[0].BillingBasis)
		assert.NotContains(t, candidates[0].Price, "billing_setting.billing_expr")
		require.NoError(t, f.db.Model(&channel).Update("model_mapping",
			`{"host-video":"`+appExecutionTestModel+`","`+appExecutionTestModel+`":"unapproved"}`).Error)
		f.request.RequestID = uuid.NewString()
		_, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
		require.Error(t, err, "an approved intermediate name is not the actual upstream model")
		require.NoError(t, f.db.Model(&channel).Update("model_mapping",
			`{"host-video":"intermediate","intermediate":"`+appExecutionTestModel+`"}`).Error)
		f.request.RequestID = uuid.NewString()
		_, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
		require.NoError(t, err, "a mapping chain ending at the admitted model is allowed")
		require.NoError(t, f.db.Model(&model.Option{}).Where(map[string]any{"key": "group_ratio_setting.group_special_usable_group"}).
			Update("value", `{"default":{"-:paid":"removed"}}`).Error)
		f.request.RequestID = uuid.NewString()
		_, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
		require.Error(t, err, "current group revocation cannot be bypassed by cached settings")
	})
	t.Run("invalid pricing mode cannot become per-call", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		require.NoError(t, f.db.Create(&model.Option{Key: "billing_setting.billing_mode",
			Value: `{"` + appExecutionTestModel + `":42}`}).Error)
		_, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
		require.Error(t, err)
	})
	for _, expression := range []string{`tier("base", p * 2 + c * 3)`, `tier("base", 0.01)`} {
		t.Run("non-task expression "+expression, func(t *testing.T) {
			f := newAppExecutionFixture(t)
			expressions, err := common.Marshal(map[string]string{appExecutionTestModel: expression})
			require.NoError(t, err)
			require.NoError(t, f.db.Create(&[]model.Option{
				{Key: "billing_setting.billing_mode", Value: `{"` + appExecutionTestModel + `":"tiered_expr"}`},
				{Key: "billing_setting.billing_expr", Value: string(expressions)},
			}).Error)
			_, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
			require.Error(t, err, "expressions without u() cannot use task/USD quota conversion")
		})
	}
	t.Run("mapping must resolve to actual terminal model", func(t *testing.T) {
		f := newAppExecutionFixture(t)
		require.NoError(t, f.db.Model(&model.Channel{}).Where("name = ?", "host-approved").
			Update("model_mapping", `{"`+appExecutionTestModel+`":"`+appExecutionTestModel+`","unused":"other"}`).Error)
		first, err := f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
		require.NoError(t, err)
		assert.Equal(t, []string{appExecutionTestModel}, first.EffectiveModelPool)
		require.NoError(t, f.db.Model(&model.Channel{}).Where("name = ?", "host-approved").
			Update("model_mapping", `{"`+appExecutionTestModel+`":"intermediate","intermediate":"`+appExecutionTestModel+`"}`).Error)
		f.request.RequestID = uuid.NewString()
		_, err = f.execution.IssueExecutionGrant(t.Context(), f.serviceID, f.request)
		require.Error(t, err, "a mapping cycle cannot be granted")
	})
}
