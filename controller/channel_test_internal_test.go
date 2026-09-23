package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestValidateChannelProxy(t *testing.T) {
	tests := []struct {
		name    string
		proxy   string
		wantErr bool
	}{
		{name: "empty"},
		{name: "http", proxy: "http://proxy.example:8080"},
		{name: "https", proxy: "https://proxy.example:8443"},
		{name: "socks5", proxy: "socks5://proxy.example"},
		{name: "socks5h", proxy: "socks5h://proxy.example:1080/"},
		{name: "unsupported", proxy: "ftp://proxy.example", wantErr: true},
		{name: "path", proxy: "socks5://proxy.example:1080/path", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setting, err := common.Marshal(dto.ChannelSettings{Proxy: test.proxy})
			require.NoError(t, err)
			channel := &model.Channel{
				Type:    constant.ChannelTypeOpenAI,
				Setting: common.GetPointer(string(setting)),
			}

			err = validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "invalid channel proxy")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateChannelRequiresNewAPIBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL *string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "blank", baseURL: common.GetPointer("  "), wantErr: true},
		{name: "configured", baseURL: common.GetPointer("https://new-api.example")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Channel{
				Type:    constant.ChannelTypeNewAPI,
				BaseURL: test.baseURL,
			}

			err := validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "New API channel base URL cannot be empty")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestNewAPIChannelRegistration(t *testing.T) {
	apiType, ok := common.ChannelType2APIType(constant.ChannelTypeNewAPI)

	require.True(t, ok)
	assert.Equal(t, constant.APITypeNewAPI, apiType)
	assert.Equal(t, "New API", constant.GetChannelTypeName(constant.ChannelTypeNewAPI))
	require.Greater(t, len(constant.ChannelBaseURLs), constant.ChannelTypeNewAPI)
	assert.Empty(t, constant.ChannelBaseURLs[constant.ChannelTypeNewAPI])
}

func TestVolcEngine3DChannelRegistration(t *testing.T) {
	apiType, ok := common.ChannelType2APIType(constant.ChannelTypeVolcEngine3D)

	require.True(t, ok)
	assert.Equal(t, constant.APITypeVolcEngine, apiType)
	assert.Equal(t, "VolcEngine3D", constant.GetChannelTypeName(constant.ChannelTypeVolcEngine3D))
	require.Greater(t, len(constant.ChannelBaseURLs), constant.ChannelTypeVolcEngine3D)
	assert.Equal(t, "https://ark.cn-beijing.volces.com", constant.ChannelBaseURLs[constant.ChannelTypeVolcEngine3D])
	assert.Equal(t,
		[]constant.EndpointType{constant.EndpointTypeThreeDGeneration},
		common.GetEndpointTypesByChannelType(constant.ChannelTypeVolcEngine3D, "hyper3d-gen2-260112"),
	)
}

func TestResponsesCompactChannelSupport(t *testing.T) {
	tests := []struct {
		name        string
		channelType int
		apiType     int
		want        bool
	}{
		{name: "OpenAI", channelType: constant.ChannelTypeOpenAI, apiType: constant.APITypeOpenAI, want: true},
		{name: "Azure", channelType: constant.ChannelTypeAzure, apiType: constant.APITypeOpenAI, want: true},
		{name: "Codex", channelType: constant.ChannelTypeCodex, apiType: constant.APITypeCodex, want: true},
		{name: "Advanced Custom", channelType: constant.ChannelTypeAdvancedCustom, apiType: constant.APITypeAdvancedCustom, want: true},
		{name: "Sub2API", channelType: constant.ChannelTypeSub2API, apiType: constant.APITypeSub2API, want: true},
		{name: "New API", channelType: constant.ChannelTypeNewAPI, apiType: constant.APITypeNewAPI, want: true},
		{name: "Anthropic", channelType: constant.ChannelTypeAnthropic, apiType: constant.APITypeAnthropic, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, common.SupportsResponsesCompact(test.channelType, test.apiType))
		})
	}
}

func TestMultiprotocolGatewayEndpointTypes(t *testing.T) {
	want := []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
		constant.EndpointTypeOpenAIResponseCompact,
		constant.EndpointTypeAnthropic,
		constant.EndpointTypeGemini,
		constant.EndpointTypeOpenAIAlphaSearch,
	}

	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeNewAPI, "gpt-5"))
	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeSub2API, "gpt-5"))
}

func TestCopyChannelRejectsInvalidLegacyProxySettings(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	settingBytes, err := common.Marshal(dto.ChannelSettings{
		Proxy: "socks5://proxy.example/legacy-path",
	})
	require.NoError(t, err)
	setting := string(settingBytes)
	origin := &model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Name:    "legacy proxy channel",
		Key:     "test-key",
		Models:  "gpt-test",
		Group:   "default",
		Setting: &setting,
	}
	require.NoError(t, db.Create(origin).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", origin.Id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/copy", nil)

	CopyChannel(ctx)

	assert.Contains(t, recorder.Body.String(), "invalid channel settings")
	var channelCount int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&channelCount).Error)
	assert.Equal(t, int64(1), channelCount)
}

func TestDeleteChannelResetsProxyCacheWhenPreReadFails(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	service.ResetProxyClientCache()
	t.Cleanup(service.ResetProxyClientCache)

	proxyURL := "http://proxy.example:8080"
	beforeDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "999999"}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/999999", nil)

	DeleteChannel(ctx)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	afterDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)
	assert.NotSame(t, beforeDelete, afterDelete)
}

func TestDeleteChannelBatchReportsAndAuditsActualDeletedCount(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	channel := &model.Channel{Name: "existing", Key: "test-key"}
	require.NoError(t, db.Create(channel).Error)

	requestBody, err := common.Marshal(ChannelBatch{Ids: []int{channel.Id, 999999}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/batch", bytes.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")

	DeleteChannelBatch(ctx)

	var response struct {
		Success bool  `json:"success"`
		Data    int64 `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, int64(1), response.Data)

	var auditLog model.Log
	require.NoError(t, db.Order("id desc").First(&auditLog).Error)
	var auditData struct {
		Operation struct {
			Params map[string]any `json:"params"`
		} `json:"op"`
	}
	require.NoError(t, common.UnmarshalJsonStr(auditLog.Other, &auditData))
	assert.Equal(t, float64(1), auditData.Operation.Params["count"])
}

func TestSettleTestQuotaUsesTieredBilling(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:   "tiered_expr",
			ExprString:    `param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`,
			ExprHash:      billingexpr.ExprHashString(`param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`),
			GroupRatio:    1,
			EstimatedTier: "stream",
			QuotaPerUnit:  common.QuotaPerUnit,
			ExprVersion:   1,
		},
		BillingRequestInput: &billingexpr.RequestInput{
			Body: []byte(`{"stream":true}`),
		},
	}

	quota, result := settleTestQuota(info, types.PriceData{
		ModelRatio:      1,
		CompletionRatio: 2,
	}, &dto.Usage{
		PromptTokens: 1000,
	})

	require.Equal(t, 1500, quota)
	require.NotNil(t, result)
	require.Equal(t, "stream", result.MatchedTier)
}

func TestBuildTestLogOtherInjectsTieredInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "tiered_expr",
			ExprString:  `tier("base", p * 2)`,
		},
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	priceData := types.PriceData{
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	usage := &dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 12,
		},
	}

	requestRules := []billingexpr.RequestRuleTrace{{
		Cond:       `param("service_tier") == "fast"`,
		Multiplier: 2,
		Matched:    true,
	}}
	other := buildTestLogOther(ctx, info, priceData, usage, &billingexpr.TieredResult{
		MatchedTier:  "base",
		RequestRules: requestRules,
	})

	require.Equal(t, "tiered_expr", other["billing_mode"])
	require.Equal(t, "base", other["matched_tier"])
	require.Equal(t, requestRules, other["request_rules"])
	require.NotEmpty(t, other["expr_b64"])
}

func TestResolveChannelTestUserIDUsesRequestUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("id", 2)

	userID, err := resolveChannelTestUserID(ctx)

	require.NoError(t, err)
	require.Equal(t, 2, userID)
}

func setupAutomaticChannelSelectionTestDB(t *testing.T) {
	t.Helper()

	originalDB := model.DB
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:%s?mode=memory&cache=shared",
		strings.ReplaceAll(t.Name(), "/", "_"),
	)), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.UpstreamManagedRoute{}))
	model.DB = db

	t.Cleanup(func() {
		model.DB = originalDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryOnlyUsesAutoDisabled(t *testing.T) {
	setupAutomaticChannelSelectionTestDB(t)

	future := time.Now().Add(time.Hour).Unix()
	planTag := "plan:support:coding"
	deferred := &model.Channel{Id: 4, Status: common.ChannelStatusAutoDisabled, Tag: &planTag}
	deferred.SetOtherInfo(map[string]any{"disabled_until": future})
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
		deferred,
	}

	selected, err := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModePassiveRecovery)

	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.Equal(t, 2, selected[0].Id)
}

func TestSelectChannelsForAutomaticTestDeduplicatesDuePlanDomain(t *testing.T) {
	setupAutomaticChannelSelectionTestDB(t)

	past := time.Now().Add(-time.Minute).Unix()
	codingTag := "plan:support:coding"
	analysisTag := "plan:support:analysis"
	ordinaryTag := "provider:support"
	first := &model.Channel{Id: 11, Status: common.ChannelStatusAutoDisabled, Tag: &codingTag}
	first.SetOtherInfo(map[string]any{"disabled_until": past, "quota_domain_id": "domain-a"})
	sameMarkedDomain := &model.Channel{Id: 12, Status: common.ChannelStatusAutoDisabled, Tag: &analysisTag}
	sameMarkedDomain.SetOtherInfo(map[string]any{"disabled_until": past, "quota_domain_id": "domain-a"})
	differentMarkedDomain := &model.Channel{Id: 13, Status: common.ChannelStatusAutoDisabled, Tag: &codingTag}
	differentMarkedDomain.SetOtherInfo(map[string]any{"disabled_until": past, "quota_domain_id": "domain-b"})
	legacyFirst := &model.Channel{Id: 14, Status: common.ChannelStatusAutoDisabled, Tag: &codingTag}
	legacyFirst.SetOtherInfo(map[string]any{
		"disabled_until": past,
		"quota_domain":   codingTag,
		"quota_type":     "plan",
	})
	legacySecond := &model.Channel{Id: 15, Status: common.ChannelStatusAutoDisabled, Tag: &codingTag}
	legacySecond.SetOtherInfo(map[string]any{
		"disabled_until": past,
		"quota_domain":   codingTag,
		"quota_type":     "plan",
	})
	genericFirst := &model.Channel{Id: 16, Status: common.ChannelStatusAutoDisabled, Tag: &codingTag}
	genericFirst.SetOtherInfo(map[string]any{"disabled_until": past, "status_reason": "authentication failed"})
	genericSecond := &model.Channel{Id: 17, Status: common.ChannelStatusAutoDisabled, Tag: &codingTag}
	genericSecond.SetOtherInfo(map[string]any{"disabled_until": past, "status_reason": "transport failed"})
	ordinary := &model.Channel{Id: 18, Status: common.ChannelStatusAutoDisabled, Tag: &ordinaryTag}
	ordinary.SetOtherInfo(map[string]any{"disabled_until": past})

	selected, err := selectChannelsForAutomaticTest(
		[]*model.Channel{
			first,
			sameMarkedDomain,
			differentMarkedDomain,
			legacyFirst,
			legacySecond,
			genericFirst,
			genericSecond,
			ordinary,
		},
		operation_setting.ChannelTestModePassiveRecovery,
	)

	require.NoError(t, err)
	selectedIDs := make([]int, len(selected))
	for i, channel := range selected {
		selectedIDs[i] = channel.Id
	}
	assert.Equal(t, []int{11, 13, 14, 16, 17, 18}, selectedIDs)
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryUsesOldestDomainPeer(t *testing.T) {
	setupAutomaticChannelSelectionTestDB(t)

	past := time.Now().Add(-time.Minute).Unix()
	tag := "plan:support:fairness"
	recent := &model.Channel{Id: 11, Status: common.ChannelStatusAutoDisabled, Tag: &tag, TestTime: 200}
	recent.SetOtherInfo(map[string]any{"disabled_until": past, "quota_domain_id": "domain-a"})
	older := &model.Channel{Id: 12, Status: common.ChannelStatusAutoDisabled, Tag: &tag, TestTime: 100}
	older.SetOtherInfo(map[string]any{"disabled_until": past, "quota_domain_id": "domain-a"})
	higherIDTie := &model.Channel{Id: 15, Status: common.ChannelStatusAutoDisabled, Tag: &tag, TestTime: 50}
	higherIDTie.SetOtherInfo(map[string]any{"disabled_until": past, "quota_domain_id": "domain-b"})
	lowerIDTie := &model.Channel{Id: 14, Status: common.ChannelStatusAutoDisabled, Tag: &tag, TestTime: 50}
	lowerIDTie.SetOtherInfo(map[string]any{"disabled_until": past, "quota_domain_id": "domain-b"})
	genericFirst := &model.Channel{Id: 16, Status: common.ChannelStatusAutoDisabled, Tag: &tag, TestTime: 300}
	genericFirst.SetOtherInfo(map[string]any{"disabled_until": past, "status_reason": "authentication failed"})
	genericSecond := &model.Channel{Id: 17, Status: common.ChannelStatusAutoDisabled, Tag: &tag, TestTime: 400}
	genericSecond.SetOtherInfo(map[string]any{"disabled_until": past, "status_reason": "transport failed"})

	selected, err := selectChannelsForAutomaticTest(
		[]*model.Channel{
			recent,
			older,
			higherIDTie,
			lowerIDTie,
			genericFirst,
			genericSecond,
		},
		operation_setting.ChannelTestModePassiveRecovery,
	)

	require.NoError(t, err)
	selectedIDs := make([]int, len(selected))
	for i, channel := range selected {
		selectedIDs[i] = channel.Id
	}
	assert.Equal(t, []int{12, 14, 16, 17}, selectedIDs)
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryIncludesManagedPlanQuota(t *testing.T) {
	setupAutomaticChannelSelectionTestDB(t)

	past := time.Now().Add(-time.Minute).Unix()
	future := time.Now().Add(time.Hour).Unix()
	markedTag := "plan:managed:marked"
	legacyTag := "plan:managed:legacy"
	ordinaryTag := "provider:managed"
	multiKeyTag := "plan:managed:multi-key"
	marked := &model.Channel{Id: 31, Status: common.ChannelStatusAutoDisabled, Tag: &markedTag}
	marked.SetOtherInfo(map[string]any{
		"disabled_until":  past,
		"quota_domain_id": "managed-domain",
	})
	legacy := &model.Channel{Id: 32, Status: common.ChannelStatusAutoDisabled, Tag: &legacyTag}
	legacy.SetOtherInfo(map[string]any{
		"disabled_until": past,
		"quota_domain":   legacyTag,
		"quota_type":     "plan",
	})
	ordinary := &model.Channel{Id: 33, Status: common.ChannelStatusAutoDisabled, Tag: &ordinaryTag}
	ordinary.SetOtherInfo(map[string]any{
		"disabled_until": past,
		"status_reason":  "ordinary managed failure",
	})
	allDisabledFirst := &model.Channel{
		Id:     34,
		Key:    "shared-key-a\nshared-key-b",
		Status: common.ChannelStatusAutoDisabled,
		Tag:    &multiKeyTag,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusAutoDisabled,
			},
		},
	}
	allDisabledFirst.SetOtherInfo(map[string]any{"disabled_until": past})
	allDisabledSecond := &model.Channel{
		Id:          35,
		Key:         allDisabledFirst.Key,
		Status:      common.ChannelStatusAutoDisabled,
		Tag:         &multiKeyTag,
		ChannelInfo: allDisabledFirst.ChannelInfo,
	}
	allDisabledSecond.SetOtherInfo(map[string]any{"disabled_until": past})
	notDue := &model.Channel{
		Id:          36,
		Key:         allDisabledFirst.Key,
		Status:      common.ChannelStatusAutoDisabled,
		Tag:         &multiKeyTag,
		ChannelInfo: allDisabledFirst.ChannelInfo,
	}
	notDue.SetOtherInfo(map[string]any{"disabled_until": future})
	hasEnabledKey := &model.Channel{
		Id:     37,
		Key:    allDisabledFirst.Key,
		Status: common.ChannelStatusAutoDisabled,
		Tag:    &multiKeyTag,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
			},
		},
	}
	hasEnabledKey.SetOtherInfo(map[string]any{"disabled_until": past})
	managed := []*model.Channel{
		marked,
		legacy,
		ordinary,
		allDisabledFirst,
		allDisabledSecond,
		notDue,
		hasEnabledKey,
	}
	for i, channel := range managed {
		require.NoError(t, model.DB.Create(&model.UpstreamManagedRoute{
			SourceID:        int64(i + 1),
			ExternalGroupID: fmt.Sprintf("managed-%d", channel.Id),
			Platform:        "plan",
			Protocol:        "openai",
			ChannelID:       channel.Id,
			State:           model.UpstreamRouteStateActive,
		}).Error)
	}

	selected, err := selectChannelsForAutomaticTest(
		managed,
		operation_setting.ChannelTestModePassiveRecovery,
	)

	require.NoError(t, err)
	selectedIDs := make([]int, len(selected))
	for i, channel := range selected {
		selectedIDs[i] = channel.Id
	}
	assert.Equal(t, []int{31, 32, 34, 35}, selectedIDs)
}

func TestSelectChannelsForAutomaticTestAlwaysSkipsManualDisabled(t *testing.T) {
	setupAutomaticChannelSelectionTestDB(t)

	autoBanEnabled := 1
	manual := &model.Channel{
		Id:      21,
		Status:  common.ChannelStatusManuallyDisabled,
		AutoBan: &autoBanEnabled,
	}

	for _, mode := range []string{
		operation_setting.ChannelTestModeScheduledAll,
		operation_setting.ChannelTestModeAutoBanOnly,
		operation_setting.ChannelTestModePassiveRecovery,
	} {
		t.Run(mode, func(t *testing.T) {
			selected, err := selectChannelsForAutomaticTest([]*model.Channel{manual}, mode)
			require.NoError(t, err)
			assert.Empty(t, selected)
		})
	}
}

func TestRunChannelTestTaskFailsClosedWhenManagedRouteQueryFails(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalLogConsumeEnabled := common.LogConsumeEnabled
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	common.MemoryCacheEnabled = false
	common.LogConsumeEnabled = false
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-fail-closed":1}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.LogConsumeEnabled = originalLogConsumeEnabled
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
	})

	user := model.User{
		Username: "passive-fail-closed-root",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1_000_000,
	}
	require.NoError(t, db.Create(&user).Error)

	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id":"chatcmpl-fail-closed",
			"object":"chat.completion",
			"created":1,
			"model":"gpt-fail-closed",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer upstream.Close()

	channel := model.Channel{
		Name: "ordinary-managed-unknown", Type: constant.ChannelTypeOpenAI,
		Key: "credential", BaseURL: &upstream.URL,
		Status: common.ChannelStatusAutoDisabled,
		Models: "gpt-fail-closed", Group: "default",
	}
	channel.SetOtherInfo(map[string]any{
		"disabled_until": time.Now().Add(-time.Minute).Unix(),
		"status_reason":  "ordinary failure",
	})
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	summary, err := runChannelTestTask(
		context.Background(),
		operation_setting.ChannelTestModePassiveRecovery,
		false,
		nil,
	)

	require.Error(t, err)
	assert.Zero(t, summary.Tested)
	assert.Zero(t, requests.Load())
}

func TestNormalizeChannelTestEndpointUsesAdvancedCustomRoute(t *testing.T) {
	channel := &model.Channel{
		Type: constant.ChannelTypeAdvancedCustom,
		OtherSettings: `{"advanced_custom":{"advanced_routes":[` +
			`{"incoming_path":"/v1/messages","upstream_path":"/v1/messages","converter":"none"}` +
			`]}}`,
	}

	endpoint := normalizeChannelTestEndpoint(channel, "glm-5.3", "")

	assert.Equal(t, string(constant.EndpointTypeAnthropic), endpoint)
}

func TestShouldRetryStopsAfterResponseWasWritten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Status(http.StatusOK)
	ctx.Writer.WriteHeaderNow()
	upstreamError := relaytypes.NewErrorWithStatusCode(
		errors.New("upstream failed"),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusBadGateway,
	)

	assert.False(t, shouldRetry(ctx, upstreamError, 3))
}

func TestShouldPrioritizePlanQuotaDisableForManagedChannel(t *testing.T) {
	planQuotaError := relaytypes.NewOpenAIError(
		errors.New("You have exceeded the monthly usage quota. It will reset at 2026-09-30 23:59:59 +0800 CST."),
		relaytypes.ErrorCode("AccountQuotaExceeded"),
		http.StatusTooManyRequests,
	)
	remainingTokensError := relaytypes.WithOpenAIError(relaytypes.OpenAIError{
		Message: "You have 7 weighted tokens left",
		Type:    "account_quota-exceeded",
	}, http.StatusTooManyRequests)
	wrongStatusError := relaytypes.NewOpenAIError(
		errors.New("You have exceeded the monthly usage quota."),
		relaytypes.ErrorCode("AccountQuotaExceeded"),
		http.StatusBadRequest,
	)
	wrongSemanticsError := relaytypes.NewOpenAIError(
		errors.New("You have exceeded the monthly usage quota."),
		relaytypes.ErrorCode("rate_limit_exceeded"),
		http.StatusTooManyRequests,
	)
	ordinaryManagedError := relaytypes.NewOpenAIError(
		errors.New("upstream unavailable"),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusBadGateway,
	)

	assert.True(t, shouldPrioritizePlanQuotaDisable(planQuotaError))
	assert.True(t, shouldPrioritizePlanQuotaDisable(remainingTokensError))
	assert.False(t, shouldPrioritizePlanQuotaDisable(wrongStatusError))
	assert.False(t, shouldPrioritizePlanQuotaDisable(wrongSemanticsError))
	assert.False(t, shouldPrioritizePlanQuotaDisable(ordinaryManagedError))
	assert.False(t, shouldPrioritizePlanQuotaDisable(nil))
}

func TestInitialSelectedChannelPlanQuotaDisablesSharedCredentialDomain(t *testing.T) {
	db := setupModelListControllerTestDB(t)

	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalAutomaticDisableEnabled := common.AutomaticDisableChannelEnabled
	originalErrorLogEnabled := constant.ErrorLogEnabled
	common.MemoryCacheEnabled = false
	common.AutomaticDisableChannelEnabled = true
	constant.ErrorLogEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.AutomaticDisableChannelEnabled = originalAutomaticDisableEnabled
		constant.ErrorLogEnabled = originalErrorLogEnabled
	})

	autoBan := 1
	sourceTag := "plan:initial:source"
	peerTag := "plan:initial:peer"
	channels := []model.Channel{
		{
			Name: "initial-source", Key: "shared-initial-credential",
			Status: common.ChannelStatusEnabled, Tag: &sourceTag, AutoBan: &autoBan,
			Models: "initial-model", Group: "default",
		},
		{
			Name: "initial-peer", Key: "shared-initial-credential",
			Status: common.ChannelStatusEnabled, Tag: &peerTag, AutoBan: &autoBan,
			Models: "initial-model", Group: "default",
		},
	}
	require.NoError(t, db.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	require.Nil(t, middleware.SetupContextForSelectedChannel(ctx, &channels[0], "initial-model"))
	selected, channelErr := getChannel(ctx, &relaycommon.RelayInfo{
		OriginModelName: "initial-model",
	}, &service.RetryParam{})
	require.Nil(t, channelErr)
	require.NotNil(t, selected)
	assert.Equal(t, sourceTag, selected.GetTag())
	assert.False(t, selected.ChannelInfo.IsMultiKey)

	const quotaMessage = "You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC."
	processChannelError(
		ctx,
		*relaytypes.NewChannelError(
			selected.Id,
			selected.Type,
			selected.Name,
			selected.ChannelInfo.IsMultiKey,
			common.GetContextKeyString(ctx, constant.ContextKeyChannelKey),
			selected.GetAutoBan(),
		),
		selected.GetTag(),
		relaytypes.WithOpenAIError(relaytypes.OpenAIError{
			Message: quotaMessage,
			Type:    "AccountQuotaExceeded",
			Code:    "AccountQuotaExceeded",
		}, http.StatusTooManyRequests),
	)

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 2)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[0].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored[1].Status)
	assert.NotEmpty(t, stored[0].GetOtherInfo()["quota_domain_id"])
	assert.Equal(t, stored[0].GetOtherInfo()["quota_domain_id"], stored[1].GetOtherInfo()["quota_domain_id"])
}

func TestInitialSelectedTaskMultiKeyPlanQuotaDisablesOnlyPrimaryKey(t *testing.T) {
	db := setupModelListControllerTestDB(t)

	originalRetryTimes := common.RetryTimes
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalAutomaticDisableEnabled := common.AutomaticDisableChannelEnabled
	originalErrorLogEnabled := constant.ErrorLogEnabled
	common.RetryTimes = 0
	common.MemoryCacheEnabled = false
	common.AutomaticDisableChannelEnabled = true
	constant.ErrorLogEnabled = false
	t.Cleanup(func() {
		common.RetryTimes = originalRetryTimes
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.AutomaticDisableChannelEnabled = originalAutomaticDisableEnabled
		constant.ErrorLogEnabled = originalErrorLogEnabled
	})

	autoBan := 1
	tag := "plan:initial:multi-key"
	channel := model.Channel{
		Name: "initial-multi-key", Key: "key-a\nkey-b",
		Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan,
		Models: "initial-task-model", Group: "default",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
		},
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(`{}`))
	require.Nil(t, middleware.SetupContextForSelectedChannel(ctx, &channel, "initial-task-model"))
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "initial-task-model",
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{},
	}
	selected, channelErr := getChannel(ctx, relayInfo, &service.RetryParam{})
	require.Nil(t, channelErr)
	require.NotNil(t, selected)
	assert.Equal(t, tag, selected.GetTag())
	assert.True(t, selected.ChannelInfo.IsMultiKey)

	const quotaMessage = "You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC."
	submitCount := 0
	outcome, taskErr := executeTaskSubmissionWith(
		ctx,
		relayInfo,
		func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *taskdto.TaskError) {
			submitCount++
			return nil, &taskdto.TaskError{
				Code:       "AccountQuotaExceeded",
				Message:    quotaMessage,
				StatusCode: http.StatusTooManyRequests,
				Error:      errors.New(quotaMessage),
			}
		},
	)

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, 1, submitCount)
	var stored model.Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.NotContains(t, stored.ChannelInfo.MultiKeyStatusList, 1)
}

func TestExecuteTaskSubmissionPreservesPlanQuotaErrorForChannelIsolation(t *testing.T) {
	db := setupModelListControllerTestDB(t)

	originalRetryTimes := common.RetryTimes
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalAutomaticDisableEnabled := common.AutomaticDisableChannelEnabled
	originalErrorLogEnabled := constant.ErrorLogEnabled
	originalDisableKeywords := operation_setting.AutomaticDisableKeywords
	originalDisableStatusCodes := operation_setting.AutomaticDisableStatusCodeRanges
	common.RetryTimes = 0
	common.MemoryCacheEnabled = false
	common.AutomaticDisableChannelEnabled = true
	constant.ErrorLogEnabled = false
	operation_setting.AutomaticDisableKeywords = []string{"unrelated"}
	operation_setting.AutomaticDisableStatusCodeRanges = nil
	t.Cleanup(func() {
		common.RetryTimes = originalRetryTimes
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.AutomaticDisableChannelEnabled = originalAutomaticDisableEnabled
		constant.ErrorLogEnabled = originalErrorLogEnabled
		operation_setting.AutomaticDisableKeywords = originalDisableKeywords
		operation_setting.AutomaticDisableStatusCodeRanges = originalDisableStatusCodes
	})

	autoBan := 1
	submitTag := "plan:task:submit"
	peerTag := "plan:task:peer"
	channels := []model.Channel{
		{
			Name: "task-submit-source", Key: "shared-task-credential",
			Status: common.ChannelStatusEnabled, Tag: &submitTag, AutoBan: &autoBan,
			Models: "task-model", Group: "default",
		},
		{
			Name: "task-submit-peer", Key: "shared-task-credential",
			Status: common.ChannelStatusEnabled, Tag: &peerTag, AutoBan: &autoBan,
			Models: "task-model", Group: "default",
		},
	}
	require.NoError(t, db.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(`{}`))
	common.SetContextKey(ctx, constant.ContextKeyChannelKey, channels[0].Key)
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "task-model",
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			LockedChannel: &channels[0],
		},
	}
	const quotaMessage = "You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC."

	outcome, taskErr := executeTaskSubmissionWith(
		ctx,
		relayInfo,
		func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *taskdto.TaskError) {
			return nil, service.TaskErrorFromAPIError(relaytypes.WithOpenAIError(relaytypes.OpenAIError{
				Message: quotaMessage,
				Type:    "AccountQuotaExceeded",
				Code:    "other_error",
			}, http.StatusTooManyRequests))
		},
	)

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "other_error", taskErr.Code)
	encoded, err := common.Marshal(taskErr)
	require.NoError(t, err)
	var response map[string]any
	require.NoError(t, common.Unmarshal(encoded, &response))
	assert.Equal(t, "AccountQuotaExceeded", response["type"])
	var storedChannels []model.Channel
	require.NoError(t, db.Order("id").Find(&storedChannels).Error)
	require.Len(t, storedChannels, 2)
	firstInfo := storedChannels[0].GetOtherInfo()
	secondInfo := storedChannels[1].GetOtherInfo()
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannels[0].Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannels[1].Status)
	assert.NotEmpty(t, firstInfo["quota_generation"])
	assert.Equal(t, firstInfo["quota_generation"], secondInfo["quota_generation"])
}

func TestExecuteTaskSubmissionDoesNotClassifyOrdinary429AsPlanQuota(t *testing.T) {
	db := setupModelListControllerTestDB(t)

	originalRetryTimes := common.RetryTimes
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalAutomaticDisableEnabled := common.AutomaticDisableChannelEnabled
	originalErrorLogEnabled := constant.ErrorLogEnabled
	originalDisableKeywords := operation_setting.AutomaticDisableKeywords
	originalDisableStatusCodes := operation_setting.AutomaticDisableStatusCodeRanges
	common.RetryTimes = 0
	common.MemoryCacheEnabled = false
	common.AutomaticDisableChannelEnabled = true
	constant.ErrorLogEnabled = false
	operation_setting.AutomaticDisableKeywords = []string{"unrelated"}
	operation_setting.AutomaticDisableStatusCodeRanges = nil
	t.Cleanup(func() {
		common.RetryTimes = originalRetryTimes
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.AutomaticDisableChannelEnabled = originalAutomaticDisableEnabled
		constant.ErrorLogEnabled = originalErrorLogEnabled
		operation_setting.AutomaticDisableKeywords = originalDisableKeywords
		operation_setting.AutomaticDisableStatusCodeRanges = originalDisableStatusCodes
	})

	autoBan := 1
	tag := "plan:task:ordinary-429"
	channel := model.Channel{
		Name: "task-ordinary-429", Key: "ordinary-credential",
		Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan,
		Models: "task-model", Group: "default",
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(`{}`))
	common.SetContextKey(ctx, constant.ContextKeyChannelKey, channel.Key)
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "task-model",
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			LockedChannel: &channel,
		},
	}
	const quotaLikeMessage = "You have exceeded the monthly usage quota."
	ordinaryTaskErr := service.TaskErrorWrapper(
		errors.New(quotaLikeMessage),
		"rate_limit_exceeded",
		http.StatusTooManyRequests,
	)
	encoded, err := common.Marshal(ordinaryTaskErr)
	require.NoError(t, err)
	var response map[string]any
	require.NoError(t, common.Unmarshal(encoded, &response))
	assert.NotContains(t, response, "type")

	outcome, taskErr := executeTaskSubmissionWith(
		ctx,
		relayInfo,
		func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *taskdto.TaskError) {
			return nil, ordinaryTaskErr
		},
	)

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "rate_limit_exceeded", taskErr.Code)
	var stored model.Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.NotContains(t, stored.GetOtherInfo(), "quota_domain_id")
}

func TestLockedTaskRetryStopsAfterSingleKeyPlanQuotaDisable(t *testing.T) {
	for _, memoryCacheEnabled := range []bool{false, true} {
		name := "without memory cache"
		if memoryCacheEnabled {
			name = "with memory cache"
		}
		t.Run(name, func(t *testing.T) {
			testLockedTaskRetryStopsAfterSingleKeyPlanQuotaDisable(t, memoryCacheEnabled)
		})
	}
}

func testLockedTaskRetryStopsAfterSingleKeyPlanQuotaDisable(t *testing.T, memoryCacheEnabled bool) {
	db := setupModelListControllerTestDB(t)

	originalRetryTimes := common.RetryTimes
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalAutomaticDisableEnabled := common.AutomaticDisableChannelEnabled
	originalErrorLogEnabled := constant.ErrorLogEnabled
	common.RetryTimes = 1
	common.MemoryCacheEnabled = memoryCacheEnabled
	common.AutomaticDisableChannelEnabled = true
	constant.ErrorLogEnabled = false
	t.Cleanup(func() {
		common.RetryTimes = originalRetryTimes
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.AutomaticDisableChannelEnabled = originalAutomaticDisableEnabled
		constant.ErrorLogEnabled = originalErrorLogEnabled
	})

	autoBan := 1
	tag := "plan:locked:single"
	channel := model.Channel{
		Name: "locked-single", Key: "single-key",
		Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan,
		Models: "locked-task-model", Group: "default",
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	if memoryCacheEnabled {
		model.InitChannelCache()
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(`{}`))
	require.Nil(t, middleware.SetupContextForSelectedChannel(ctx, &channel, "locked-task-model"))
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "locked-task-model",
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			LockedChannel: &channel,
		},
	}
	const quotaMessage = "You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC."
	submitCount := 0

	outcome, taskErr := executeTaskSubmissionWith(
		ctx,
		relayInfo,
		func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *taskdto.TaskError) {
			submitCount++
			return nil, service.TaskErrorFromAPIError(relaytypes.WithOpenAIError(relaytypes.OpenAIError{
				Message: quotaMessage,
				Type:    "AccountQuotaExceeded",
				Code:    "other_error",
			}, http.StatusTooManyRequests))
		},
	)

	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, 1, submitCount)
	assert.True(t, taskErr.LocalError)
	assert.Equal(t, "setup_locked_channel_disabled", taskErr.Code)
	assert.Contains(t, taskErr.Message, "disabled")
	refreshed, ok := relayInfo.LockedChannel.(*model.Channel)
	require.True(t, ok)
	if memoryCacheEnabled {
		cachedChannel, err := model.CacheGetChannel(channel.Id)
		require.NoError(t, err)
		assert.NotSame(t, cachedChannel, refreshed)
	}
	assert.Equal(t, common.ChannelStatusAutoDisabled, refreshed.Status)
	var stored model.Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
}

func TestLockedTaskRetryRefreshesMultiKeyAndUsesRemainingKey(t *testing.T) {
	for _, memoryCacheEnabled := range []bool{false, true} {
		name := "without memory cache"
		if memoryCacheEnabled {
			name = "with memory cache"
		}
		t.Run(name, func(t *testing.T) {
			testLockedTaskRetryRefreshesMultiKeyAndUsesRemainingKey(t, memoryCacheEnabled)
		})
	}
}

func testLockedTaskRetryRefreshesMultiKeyAndUsesRemainingKey(t *testing.T, memoryCacheEnabled bool) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Task{}))

	originalRetryTimes := common.RetryTimes
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalAutomaticDisableEnabled := common.AutomaticDisableChannelEnabled
	originalErrorLogEnabled := constant.ErrorLogEnabled
	originalLogConsumeEnabled := common.LogConsumeEnabled
	common.RetryTimes = 1
	common.MemoryCacheEnabled = memoryCacheEnabled
	common.AutomaticDisableChannelEnabled = true
	constant.ErrorLogEnabled = false
	common.LogConsumeEnabled = false
	t.Cleanup(func() {
		common.RetryTimes = originalRetryTimes
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.AutomaticDisableChannelEnabled = originalAutomaticDisableEnabled
		constant.ErrorLogEnabled = originalErrorLogEnabled
		common.LogConsumeEnabled = originalLogConsumeEnabled
	})

	autoBan := 1
	tag := "plan:locked:multi"
	channel := model.Channel{
		Name: "locked-multi", Key: "key-a\nkey-b",
		Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan,
		Models: "locked-task-model", Group: "default",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
		},
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	if memoryCacheEnabled {
		model.InitChannelCache()
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(`{}`))
	require.Nil(t, middleware.SetupContextForSelectedChannel(ctx, &channel, "locked-task-model"))
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "locked-task-model",
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID:  "task_locked_multi",
			LockedChannel: &channel,
		},
	}
	const quotaMessage = "You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC."
	usedKeys := make([]string, 0, 2)

	outcome, taskErr := executeTaskSubmissionWith(
		ctx,
		relayInfo,
		func(c *gin.Context, info *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *taskdto.TaskError) {
			info.InitChannelMeta(c)
			usingKey := common.GetContextKeyString(c, constant.ContextKeyChannelKey)
			usedKeys = append(usedKeys, usingKey)
			if usingKey == "key-a" {
				return nil, service.TaskErrorFromAPIError(relaytypes.WithOpenAIError(relaytypes.OpenAIError{
					Message: quotaMessage,
					Type:    "AccountQuotaExceeded",
					Code:    "other_error",
				}, http.StatusTooManyRequests))
			}
			return &relay.TaskSubmitResult{
				UpstreamTaskID: "upstream-locked-multi",
				Platform:       constant.TaskPlatform("test"),
			}, nil
		},
	)

	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	assert.Equal(t, []string{"key-a", "key-b"}, usedKeys)
	refreshed, ok := relayInfo.LockedChannel.(*model.Channel)
	require.True(t, ok)
	if memoryCacheEnabled {
		cachedChannel, err := model.CacheGetChannel(channel.Id)
		require.NoError(t, err)
		assert.NotSame(t, cachedChannel, refreshed)
	}
	assert.NotSame(t, &channel, refreshed)
	assert.Equal(t, common.ChannelStatusEnabled, refreshed.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, refreshed.ChannelInfo.MultiKeyStatusList[0])
	assert.NotContains(t, refreshed.ChannelInfo.MultiKeyStatusList, 1)
	var stored model.Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.NotContains(t, stored.ChannelInfo.MultiKeyStatusList, 1)
}

func TestProcessChannelErrorRecordsManagedFailureWhenAutomaticDisableIsOff(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.UpstreamManagedRoute{}))

	originalAutomaticDisable := common.AutomaticDisableChannelEnabled
	originalErrorLogEnabled := constant.ErrorLogEnabled
	orchestrationSetting := operation_setting.GetUpstreamOrchestrationSetting()
	originalOrchestrationSetting := *orchestrationSetting
	common.AutomaticDisableChannelEnabled = false
	constant.ErrorLogEnabled = false
	orchestrationSetting.Enabled = true
	orchestrationSetting.FailureThreshold = 2
	orchestrationSetting.FailureWindowMinutes = 5
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = originalAutomaticDisable
		constant.ErrorLogEnabled = originalErrorLogEnabled
		*orchestrationSetting = originalOrchestrationSetting
	})

	channel := model.Channel{
		Name:   "managed-plan-global-disable-off",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-3.5-turbo",
		Group:  "default",
	}
	require.NoError(t, db.Create(&channel).Error)
	route := model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "managed-plan-global-disable-off",
		Platform:        "openai",
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateActive,
	}
	require.NoError(t, db.Create(&route).Error)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	apiError := relaytypes.NewOpenAIError(
		errors.New("You have exceeded the monthly usage quota. It will reset at 2026-09-30 23:59:59 +0800 CST."),
		relaytypes.ErrorCode("AccountQuotaExceeded"),
		http.StatusTooManyRequests,
	)
	processChannelError(ctx, relaytypes.ChannelError{
		ChannelId:   channel.Id,
		ChannelName: channel.Name,
		AutoBan:     true,
	}, "", apiError)

	require.Eventually(t, func() bool {
		var stored model.UpstreamManagedRoute
		if err := db.First(&stored, route.ID).Error; err != nil {
			return false
		}
		return stored.ConsecutiveFailures == 1
	}, time.Second, 10*time.Millisecond)
}

func TestChannelForHealthCheckCountsOnlyCommittedRecoveries(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalLogConsumeEnabled := common.LogConsumeEnabled
	originalAutomaticEnableChannelEnabled := common.AutomaticEnableChannelEnabled
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	common.MemoryCacheEnabled = false
	common.LogConsumeEnabled = false
	common.AutomaticEnableChannelEnabled = true
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-health-recovery":1}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.LogConsumeEnabled = originalLogConsumeEnabled
		common.AutomaticEnableChannelEnabled = originalAutomaticEnableChannelEnabled
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
	})

	user := model.User{
		Username: "health-recovery-root",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1_000_000,
	}
	require.NoError(t, db.Create(&user).Error)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id":"chatcmpl-health",
			"object":"chat.completion",
			"created":1,
			"model":"gpt-health-recovery",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer upstream.Close()

	autoBan := 1
	tag := "plan:support:health-count"
	quotaInfo := map[string]any{
		"disabled_until": time.Now().Add(-time.Minute).Unix(),
		"quota_domain":   tag,
		"quota_type":     "plan",
	}
	channels := []model.Channel{
		{
			Name: "health-source", Type: constant.ChannelTypeOpenAI, Key: "credential",
			BaseURL: &upstream.URL, Status: common.ChannelStatusAutoDisabled,
			Tag: &tag, AutoBan: &autoBan, Models: "gpt-health-recovery", Group: "default",
		},
		{
			Name: "health-peer", Type: constant.ChannelTypeOpenAI, Key: "credential",
			BaseURL: &upstream.URL, Status: common.ChannelStatusAutoDisabled,
			Tag: &tag, AutoBan: &autoBan, Models: "gpt-health-recovery", Group: "default",
		},
	}
	channels[0].SetOtherInfo(quotaInfo)
	channels[1].SetOtherInfo(quotaInfo)
	require.NoError(t, db.Create(&channels).Error)
	for i := range channels {
		require.NoError(t, channels[i].AddAbilities(nil))
	}

	summary := testChannelForHealthCheck(context.Background(), &channels[0], user.Id, false, 10_000_000)
	staleSummary := testChannelForHealthCheck(context.Background(), &channels[0], user.Id, false, 10_000_000)

	assert.Equal(t, 2, summary.Enabled)
	assert.Zero(t, staleSummary.Enabled)
}

func TestBuildHealthCheckProbeChannelSelectsOldestAutoDisabledKey(t *testing.T) {
	channel := &model.Channel{
		Id:     71,
		Key:    "key-a\nkey-b\nkey-c\nkey-d",
		Status: common.ChannelStatusAutoDisabled,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 4,
			MultiKeyMode: constant.MultiKeyModePolling,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusAutoDisabled,
				2: common.ChannelStatusManuallyDisabled,
				3: common.ChannelStatusAutoDisabled,
			},
			MultiKeyDisabledReason: map[int]string{
				0: "newer",
				1: "oldest lower index",
				2: "manual",
				3: "oldest higher index",
			},
			MultiKeyDisabledTime: map[int]int64{
				0: 200,
				1: 100,
				2: 50,
				3: 100,
			},
		},
	}

	probe, selectedKey, ok := buildHealthCheckProbeChannel(channel)

	require.True(t, ok)
	require.NotSame(t, channel, probe)
	assert.Equal(t, "key-b", selectedKey)
	assert.Equal(t, common.ChannelStatusAutoDisabled, probe.ChannelInfo.MultiKeyStatusList[0])
	assert.NotContains(t, probe.ChannelInfo.MultiKeyStatusList, 1)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, probe.ChannelInfo.MultiKeyStatusList[2])
	assert.Equal(t, common.ChannelStatusAutoDisabled, probe.ChannelInfo.MultiKeyStatusList[3])

	probe.ChannelInfo.MultiKeyStatusList[0] = common.ChannelStatusEnabled
	probe.ChannelInfo.MultiKeyDisabledReason[0] = "probe mutation"
	probe.ChannelInfo.MultiKeyDisabledTime[0] = 1
	assert.Equal(t, common.ChannelStatusAutoDisabled, channel.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, "newer", channel.ChannelInfo.MultiKeyDisabledReason[0])
	assert.EqualValues(t, 200, channel.ChannelInfo.MultiKeyDisabledTime[0])
	assert.Equal(t, constant.MultiKeyModePolling, channel.ChannelInfo.MultiKeyMode)

	channel.ChannelInfo.MultiKeyRecoveryIndex = 1
	_, nextKey, ok := buildHealthCheckProbeChannel(channel)
	require.True(t, ok)
	assert.Equal(t, "key-d", nextKey)

	channel.ChannelInfo.MultiKeyRecoveryIndex = 3
	_, wrappedKey, ok := buildHealthCheckProbeChannel(channel)
	require.True(t, ok)
	assert.Equal(t, "key-b", wrappedKey)
}

func TestIsUpstreamProbeFailureRejectsCancellation(t *testing.T) {
	assert.True(t, isUpstreamProbeFailure(context.Background(), errors.New("upstream rejected request")))
	assert.False(t, isUpstreamProbeFailure(context.Background(), context.Canceled))
	assert.False(t, isUpstreamProbeFailure(context.Background(), context.DeadlineExceeded))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, isUpstreamProbeFailure(ctx, errors.New("transport returned after cancellation")))
}

func TestChannelForHealthCheckProbesFinalAutoDisabledMultiKey(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		upstreamOK    bool
		wantSucceeded int
		wantFailed    int
		wantEnabled   int
	}{
		{
			name:          "failed probe preserves every key",
			wantFailed:    1,
			wantEnabled:   0,
			wantSucceeded: 0,
		},
		{
			name:          "successful probe recovers selected key",
			upstreamOK:    true,
			wantSucceeded: 1,
			wantEnabled:   1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			originalMemoryCacheEnabled := common.MemoryCacheEnabled
			originalLogConsumeEnabled := common.LogConsumeEnabled
			originalAutomaticEnableChannelEnabled := common.AutomaticEnableChannelEnabled
			originalModelRatios := ratio_setting.ModelRatio2JSONString()
			originalGroupRatios := ratio_setting.GroupRatio2JSONString()
			common.MemoryCacheEnabled = false
			common.LogConsumeEnabled = false
			common.AutomaticEnableChannelEnabled = true
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-final-key-health":1}`))
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
			t.Cleanup(func() {
				common.MemoryCacheEnabled = originalMemoryCacheEnabled
				common.LogConsumeEnabled = originalLogConsumeEnabled
				common.AutomaticEnableChannelEnabled = originalAutomaticEnableChannelEnabled
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
				require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
			})

			user := model.User{
				Username: "final-key-health-root",
				Role:     common.RoleRootUser,
				Status:   common.UserStatusEnabled,
				Group:    "default",
				Quota:    1_000_000,
			}
			require.NoError(t, db.Create(&user).Error)

			requestKeys := make(chan string, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestKeys <- strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				w.Header().Set("Content-Type", "application/json")
				if !testCase.upstreamOK {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = fmt.Fprint(w, `{"error":{"message":"probe failed","type":"upstream_error"}}`)
					return
				}
				_, _ = fmt.Fprint(w, `{
					"id":"chatcmpl-final-key-health",
					"object":"chat.completion",
					"created":1,
					"model":"gpt-final-key-health",
					"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
					"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
				}`)
			}))
			defer upstream.Close()

			autoBan := 1
			channel := model.Channel{
				Name: "final-key-health", Type: constant.ChannelTypeOpenAI,
				Key: "key-a\nkey-b\nkey-c", BaseURL: &upstream.URL,
				Status: common.ChannelStatusAutoDisabled, AutoBan: &autoBan,
				Models: "gpt-final-key-health", Group: "default",
				ChannelInfo: model.ChannelInfo{
					IsMultiKey:   true,
					MultiKeySize: 3,
					MultiKeyMode: constant.MultiKeyModePolling,
					MultiKeyStatusList: map[int]int{
						0: common.ChannelStatusAutoDisabled,
						1: common.ChannelStatusAutoDisabled,
						2: common.ChannelStatusAutoDisabled,
					},
					MultiKeyDisabledReason: map[int]string{
						0: "newest",
						1: "oldest lower index",
						2: "oldest higher index",
					},
					MultiKeyDisabledTime: map[int]int64{
						0: 300,
						1: 100,
						2: 100,
					},
				},
			}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))

			summary := testChannelForHealthCheck(
				context.Background(),
				&channel,
				user.Id,
				false,
				10_000_000,
			)

			assert.Equal(t, 1, summary.Tested)
			assert.Equal(t, testCase.wantSucceeded, summary.Succeeded)
			assert.Equal(t, testCase.wantFailed, summary.Failed)
			assert.Equal(t, testCase.wantEnabled, summary.Enabled)
			require.Len(t, requestKeys, 1)
			assert.Equal(t, "key-b", <-requestKeys)

			assert.Equal(t, common.ChannelStatusAutoDisabled, channel.Status)
			assert.Equal(t, map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusAutoDisabled,
				2: common.ChannelStatusAutoDisabled,
			}, channel.ChannelInfo.MultiKeyStatusList)
			assert.Equal(t, constant.MultiKeyModePolling, channel.ChannelInfo.MultiKeyMode)

			var stored model.Channel
			require.NoError(t, db.First(&stored, channel.Id).Error)
			var ability model.Ability
			require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
			if testCase.upstreamOK {
				assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
				assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
				assert.NotContains(t, stored.ChannelInfo.MultiKeyStatusList, 1)
				assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[2])
				assert.Zero(t, stored.ChannelInfo.MultiKeyRecoveryIndex)
				assert.True(t, ability.Enabled)
				return
			}
			assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
			assert.Equal(t, 1, stored.ChannelInfo.MultiKeyRecoveryIndex)
			assert.False(t, ability.Enabled)

			secondSummary := testChannelForHealthCheck(
				context.Background(),
				&stored,
				user.Id,
				false,
				10_000_000,
			)
			assert.Equal(t, 1, secondSummary.Tested)
			assert.Equal(t, 1, secondSummary.Failed)
			assert.Zero(t, secondSummary.Enabled)
			require.Len(t, requestKeys, 1)
			assert.Equal(t, "key-c", <-requestKeys)
			require.NoError(t, db.First(&stored, channel.Id).Error)
			assert.Equal(t, 2, stored.ChannelInfo.MultiKeyRecoveryIndex)
		})
	}
}

func TestChannelForHealthCheckDoesNotAdvanceRecoveryCursorWithoutUpstreamFailure(t *testing.T) {
	tests := []struct {
		name      string
		testCtx   func() context.Context
		channelID int
	}{
		{
			name: "local setup failure",
			testCtx: func() context.Context {
				return context.Background()
			},
			channelID: 81,
		},
		{
			name: "cancellation",
			testCtx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			channelID: 82,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			channel := model.Channel{
				Id: testCase.channelID, Name: testCase.name,
				Type: constant.ChannelTypeTaskPlugin,
				Key:  "key-a\nkey-b", Status: common.ChannelStatusAutoDisabled,
				Models: "unsupported-health-test", Group: "default",
				ChannelInfo: model.ChannelInfo{
					IsMultiKey:   true,
					MultiKeySize: 2,
					MultiKeyStatusList: map[int]int{
						0: common.ChannelStatusAutoDisabled,
						1: common.ChannelStatusAutoDisabled,
					},
					MultiKeyDisabledTime: map[int]int64{0: 100, 1: 200},
				},
			}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))

			testChannelForHealthCheck(testCase.testCtx(), &channel, 0, false, 10_000_000)

			var stored model.Channel
			require.NoError(t, db.First(&stored, channel.Id).Error)
			assert.Zero(t, stored.ChannelInfo.MultiKeyRecoveryIndex)
		})
	}
}

func TestManagedFinalKeyDisableSurvivesReconciliationAndRecoversThroughIsolatedProbe(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.UpstreamSource{},
		&model.UpstreamGroup{},
		&model.UpstreamManagedRoute{},
	))

	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalLogConsumeEnabled := common.LogConsumeEnabled
	originalAutomaticDisableChannelEnabled := common.AutomaticDisableChannelEnabled
	originalAutomaticEnableChannelEnabled := common.AutomaticEnableChannelEnabled
	originalModelRatios := ratio_setting.ModelRatio2JSONString()
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	orchestrationSetting := operation_setting.GetUpstreamOrchestrationSetting()
	originalOrchestrationSetting := *orchestrationSetting
	common.MemoryCacheEnabled = true
	common.LogConsumeEnabled = false
	common.AutomaticDisableChannelEnabled = true
	common.AutomaticEnableChannelEnabled = true
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-managed-final-key":1}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	orchestrationSetting.Enabled = true
	orchestrationSetting.AutoEnroll = false
	orchestrationSetting.CandidateLimit = 5
	orchestrationSetting.MaxUpstreamMultiplier = 1
	orchestrationSetting.SyncIntervalHours = 4
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.LogConsumeEnabled = originalLogConsumeEnabled
		common.AutomaticDisableChannelEnabled = originalAutomaticDisableChannelEnabled
		common.AutomaticEnableChannelEnabled = originalAutomaticEnableChannelEnabled
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(originalModelRatios))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
		*orchestrationSetting = originalOrchestrationSetting
	})

	user := model.User{
		Username: "managed-final-key-root",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1_000_000,
	}
	require.NoError(t, db.Create(&user).Error)

	requestKeys := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestKeys <- strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id":"chatcmpl-managed-final-key",
			"object":"chat.completion",
			"created":1,
			"model":"gpt-managed-final-key",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer upstream.Close()

	now := time.Now()
	source := model.UpstreamSource{
		Key:              "managed-final-key-source",
		Name:             "Managed Final Key Source",
		ConsoleURL:       "https://example.com",
		SelectedEndpoint: upstream.URL,
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
		LastSnapshotAt:   now.Unix(),
		LastSuccessAt:    now.Unix(),
	}
	require.NoError(t, db.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "managed-final-key-group",
		Name:                "Managed Final Key Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		HealthStatus:        model.UpstreamHealthOperational,
		Models:              `["gpt-managed-final-key"]`,
		ObservedAt:          now.Unix(),
	}
	require.NoError(t, db.Create(&group).Error)

	autoBan := 1
	tag := "plan:managed:final-key"
	channel := model.Channel{
		Name: "managed-final-key", Type: constant.ChannelTypeOpenAI,
		Key: "key-a\nkey-b", BaseURL: &upstream.URL,
		Status: common.ChannelStatusEnabled, Tag: &tag, AutoBan: &autoBan,
		Models: "gpt-managed-final-key", Group: "default",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
			MultiKeyMode: constant.MultiKeyModePolling,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
			},
			MultiKeyDisabledReason: map[int]string{
				0: "previous structured quota failure",
			},
			MultiKeyDisabledTime: map[int]int64{
				0: now.Add(-time.Hour).Unix(),
			},
		},
	}
	channel.SetOtherInfo(map[string]any{
		"owner": "preserved",
	})
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID: source.ID, ExternalGroupID: group.ExternalID,
		Platform: group.Platform, Protocol: model.UpstreamProtocolOpenAI,
		ChannelID: channel.Id, State: model.UpstreamRouteStateActive,
	}
	require.NoError(t, db.Create(&route).Error)
	model.InitChannelCache()

	resetAt := now.Add(time.Hour).Truncate(time.Second)
	apiError := relaytypes.NewOpenAIError(
		fmt.Errorf(
			"You have exceeded the monthly usage quota. It will reset at %s.",
			resetAt.Format("2006-01-02 15:04:05 -0700 MST"),
		),
		relaytypes.ErrorCode("AccountQuotaExceeded"),
		http.StatusTooManyRequests,
	)
	require.True(t, service.DisableChannelForAPIError(relaytypes.ChannelError{
		ChannelId:   channel.Id,
		ChannelName: channel.Name,
		IsMultiKey:  true,
		AutoBan:     true,
		UsingKey:    "key-b",
	}, tag, apiError))

	var disabled model.Channel
	require.NoError(t, db.First(&disabled, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, disabled.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, disabled.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, common.ChannelStatusAutoDisabled, disabled.ChannelInfo.MultiKeyStatusList[1])
	disabledInfo := disabled.GetOtherInfo()
	assert.Equal(t, float64(resetAt.Unix()), disabledInfo["quota_reset_at"])
	assert.Equal(t, resetAt.Unix()+60, disabled.GetDisabledUntil())
	assert.Equal(t, "preserved", disabledInfo["owner"])
	assert.NotContains(t, disabledInfo, "quota_domain")
	assert.NotContains(t, disabledInfo, "quota_domain_id")
	assert.NotContains(t, disabledInfo, "quota_generation")
	assert.NotContains(t, disabledInfo, "quota_type")
	var ability model.Ability
	require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
	selected, err := model.GetRandomSatisfiedChannel("default", channel.Models, 0, nil)
	require.NoError(t, err)
	assert.Nil(t, selected)

	summary, err := service.ReconcileManagedUpstreams(now)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.PrioritiesUpdated)

	var reconciled model.Channel
	require.NoError(t, db.First(&reconciled, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, reconciled.Status)
	assert.Equal(t, float64(resetAt.Unix()), reconciled.GetOtherInfo()["quota_reset_at"])
	assert.Equal(t, resetAt.Unix()+60, reconciled.GetDisabledUntil())
	require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.False(t, ability.Enabled)
	selected, err = model.GetRandomSatisfiedChannel("default", channel.Models, 0, nil)
	require.NoError(t, err)
	assert.Nil(t, selected)

	_, sharedOwner := service.PlanQuotaRecoveryDomainKey(&reconciled)
	assert.False(t, sharedOwner)

	notDueSummary, err := runChannelTestTask(
		context.Background(),
		operation_setting.ChannelTestModePassiveRecovery,
		false,
		nil,
	)
	require.NoError(t, err)
	assert.Zero(t, notDueSummary.Tested)
	assert.Empty(t, requestKeys)

	dueInfo := reconciled.GetOtherInfo()
	dueInfo["disabled_until"] = now.Add(-time.Minute).Unix()
	reconciled.SetOtherInfo(dueInfo)
	require.NoError(t, db.Model(&model.Channel{}).
		Where("id = ?", channel.Id).
		Update("other_info", reconciled.OtherInfo).Error)

	var progress []string
	probeSummary, err := runChannelTestTask(
		context.Background(),
		operation_setting.ChannelTestModePassiveRecovery,
		false,
		func(processed, total int) {
			progress = append(progress, fmt.Sprintf("%d/%d", processed, total))
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, probeSummary.Tested)
	assert.Equal(t, 1, probeSummary.Succeeded)
	assert.Equal(t, 1, probeSummary.Enabled)
	assert.Equal(t, []string{"0/1", "1/1"}, progress)
	require.Len(t, requestKeys, 1)
	assert.Equal(t, "key-a", <-requestKeys)

	var recovered model.Channel
	require.NoError(t, db.First(&recovered, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, recovered.Status)
	assert.NotContains(t, recovered.ChannelInfo.MultiKeyStatusList, 0)
	assert.Equal(t, common.ChannelStatusAutoDisabled, recovered.ChannelInfo.MultiKeyStatusList[1])
	recoveredInfo := recovered.GetOtherInfo()
	assert.Equal(t, "preserved", recoveredInfo["owner"])
	assert.NotContains(t, recoveredInfo, "quota_reset_at")
	assert.NotContains(t, recoveredInfo, "disabled_until")
	require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.True(t, ability.Enabled)
	selected, err = model.GetRandomSatisfiedChannel("default", channel.Models, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, channel.Id, selected.Id)
	selectedKey, _, keyErr := selected.GetNextEnabledKey()
	require.Nil(t, keyErr)
	assert.Equal(t, "key-a", selectedKey)
}

func TestSelectChannelsForAutomaticTestScheduledSkipsManualDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected, err := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeScheduledAll)

	require.NoError(t, err)
	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 2, selected[1].Id)
}

func TestSelectChannelsForAutomaticTestAutoBanOnlyUsesEligibleChannels(t *testing.T) {
	autoBanEnabled := 1
	autoBanDisabled := 0
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled, AutoBan: &autoBanEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled, AutoBan: &autoBanDisabled},
		{Id: 3, Status: common.ChannelStatusAutoDisabled, AutoBan: &autoBanEnabled},
		{Id: 4, Status: common.ChannelStatusManuallyDisabled, AutoBan: &autoBanEnabled},
		{Id: 5, Status: common.ChannelStatusEnabled},
	}

	selected, err := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeAutoBanOnly)

	require.NoError(t, err)
	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 3, selected[1].Id)
}

func TestRunChannelTestWorkersHonorsConfiguredConcurrency(t *testing.T) {
	originalInterval := common.RequestInterval
	common.RequestInterval = 0
	t.Cleanup(func() { common.RequestInterval = originalInterval })

	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled},
		{Id: 3, Status: common.ChannelStatusEnabled},
		{Id: 4, Status: common.ChannelStatusEnabled},
	}
	started := make(chan struct{}, len(channels))
	release := make(chan struct{})
	var active atomic.Int32
	var maxActive atomic.Int32
	progress := make([]int, 0, len(channels)+1)
	summaryResult := make(chan channelTestSummary, 1)

	go func() {
		summaryResult <- runChannelTestWorkers(
			context.Background(),
			channels,
			2,
			func(_ context.Context, _ *model.Channel) channelTestSummary {
				current := active.Add(1)
				defer active.Add(-1)
				for {
					observed := maxActive.Load()
					if current <= observed || maxActive.CompareAndSwap(observed, current) {
						break
					}
				}
				started <- struct{}{}
				<-release
				return channelTestSummary{Tested: 1, Succeeded: 1}
			},
			func(processed, _ int) {
				progress = append(progress, processed)
			},
		)
	}()

	<-started
	<-started
	select {
	case <-started:
		t.Fatal("started more channel tests than the configured concurrency")
	default:
	}
	close(release)

	summary := <-summaryResult

	assert.Equal(t, int32(2), maxActive.Load())
	assert.Equal(t, channelTestSummary{Tested: 4, Succeeded: 4}, summary)
	assert.Equal(t, []int{0, 1, 2, 3, 4}, progress)
}

func TestRunChannelTestWorkersStopsAfterCancellation(t *testing.T) {
	originalInterval := common.RequestInterval
	common.RequestInterval = 0
	t.Cleanup(func() { common.RequestInterval = originalInterval })

	ctx, cancel := context.WithCancel(context.Background())
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled},
		{Id: 3, Status: common.ChannelStatusEnabled},
		{Id: 4, Status: common.ChannelStatusEnabled},
	}
	started := make(chan struct{}, len(channels))
	progress := make([]int, 0, 1)
	summaryResult := make(chan channelTestSummary, 1)

	go func() {
		summaryResult <- runChannelTestWorkers(
			ctx,
			channels,
			2,
			func(ctx context.Context, _ *model.Channel) channelTestSummary {
				started <- struct{}{}
				<-ctx.Done()
				return channelTestSummary{Tested: 1, Succeeded: 1}
			},
			func(processed, _ int) {
				progress = append(progress, processed)
			},
		)
	}()

	<-started
	<-started
	cancel()

	summary := <-summaryResult

	select {
	case <-started:
		t.Fatal("started another channel test after cancellation")
	default:
	}
	assert.Equal(t, channelTestSummary{Tested: 2, Succeeded: 2}, summary)
	assert.Equal(t, []int{0}, progress)
}

func TestTestAllChannelsRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeChannelTest, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/test", nil)

	TestAllChannels(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "已有通道测试任务正在运行或等待中")
}
