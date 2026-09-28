package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

const (
	seedreamBillingOrderModel = "doubao-seedream-5-0-pro-260628"
	seedreamBillingOrderExpr  = `tier("per_image", fixed(images_up_to_1_5k * 0.04109589041 + images_above_1_5k * 0.08219178082 + max(input_images - 1, 0) * 0.002739726027))`
)

type seedreamBillingOrderFixture struct {
	db    *gorm.DB
	user  *model.User
	token *model.Token
}

func newSeedreamBillingOrderFixture(t *testing.T) *seedreamBillingOrderFixture {
	t.Helper()

	db := modelManagementDB(t, "sqlite", "")
	require.NoError(t, db.AutoMigrate(
		&model.Token{},
		&model.Log{},
		&model.UpstreamSource{},
		&model.UpstreamGroup{},
		&model.UpstreamManagedRoute{},
	))

	previousBatch := common.BatchUpdateEnabled
	previousLogs := common.LogConsumeEnabled
	previousCountTokens := constant.CountToken
	previousQuotaPerUnit := common.QuotaPerUnit
	previousRetries := common.RetryTimes
	previousQuotaSetting := *operation_setting.GetQuotaSetting()
	previousOrchestration := *operation_setting.GetUpstreamOrchestrationSetting()
	previousPassThrough := model_setting.GetGlobalSettings().PassThroughRequestEnabled
	previousGinMode := gin.Mode()
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = true
	constant.CountToken = false
	common.QuotaPerUnit = 500_000
	common.RetryTimes = 0
	operation_setting.GetQuotaSetting().TrustQuotaUSD = 0
	operation_setting.GetQuotaSetting().PreConsumeMultiplier = 1
	operation_setting.GetUpstreamOrchestrationSetting().Enabled = false
	model_setting.GetGlobalSettings().PassThroughRequestEnabled = false
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() {
		common.BatchUpdateEnabled = previousBatch
		common.LogConsumeEnabled = previousLogs
		constant.CountToken = previousCountTokens
		common.QuotaPerUnit = previousQuotaPerUnit
		common.RetryTimes = previousRetries
		*operation_setting.GetQuotaSetting() = previousQuotaSetting
		*operation_setting.GetUpstreamOrchestrationSetting() = previousOrchestration
		model_setting.GetGlobalSettings().PassThroughRequestEnabled = previousPassThrough
		gin.SetMode(previousGinMode)
	})

	expressions, err := common.Marshal(map[string]string{
		seedreamBillingOrderModel: seedreamBillingOrderExpr,
	})
	require.NoError(t, err)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"` + seedreamBillingOrderModel + `":"tiered_expr"}`,
		"billing_setting.billing_expr": string(expressions),
	}))

	user := &model.User{
		Id:       7_501,
		Username: "seedream-billing-order",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Setting:  `{"billing_preference":"wallet_only"}`,
		Quota:    500_000,
	}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{
		Id:          7_502,
		UserId:      user.Id,
		Key:         "seedream-billing-order-token",
		Name:        "seedream-billing-order",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 500_000,
	}
	require.NoError(t, db.Create(token).Error)
	service.InitHttpClient()

	return &seedreamBillingOrderFixture{db: db, user: user, token: token}
}

func (f *seedreamBillingOrderFixture) resetQuota(t *testing.T, quota int) {
	t.Helper()
	require.NoError(t, f.db.Model(f.user).Updates(map[string]any{
		"quota": quota, "used_quota": 0, "request_count": 0,
	}).Error)
	require.NoError(t, f.db.Model(f.token).Updates(map[string]any{
		"remain_quota": quota, "used_quota": 0,
	}).Error)
	require.NoError(t, model.LOG_DB.Where("user_id = ?", f.user.Id).Delete(&model.Log{}).Error)
}

func (f *seedreamBillingOrderFixture) relay(
	t *testing.T,
	requestID string,
	body string,
	baseURL string,
	passThrough bool,
	paramOverride map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(common.RequestIdKey, requestID)
	c.Set("username", f.user.Username)
	c.Set("token_name", f.token.Name)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, seedreamBillingOrderModel)
	common.SetContextKey(c, constant.ContextKeyUserId, f.user.Id)
	common.SetContextKey(c, constant.ContextKeyUserQuota, f.user.Quota)
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
	common.SetContextKey(c, constant.ContextKeyTokenId, f.token.Id)
	common.SetContextKey(c, constant.ContextKeyTokenKey, f.token.Key)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyTokenUnlimited, false)
	c.Set("token_quota", f.token.RemainQuota)
	common.SetContextKey(c, constant.ContextKeyChannelId, 7_503)
	common.SetContextKey(c, constant.ContextKeyChannelName, "seedream-billing-order")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, baseURL)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "local-test-key")
	common.SetContextKey(c, constant.ContextKeyChannelAutoBan, false)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{
		PassThroughBodyEnabled: passThrough,
	})
	if paramOverride != nil {
		common.SetContextKey(c, constant.ContextKeyChannelParamOverride, paramOverride)
	}

	Relay(c, types.RelayFormatOpenAIImage)
	return recorder
}

func recordUserQuotaMutations(t *testing.T, db *gorm.DB) *atomic.Int32 {
	t.Helper()

	var count atomic.Int32
	name := fmt.Sprintf("test:seedream_billing_order:%p", &count)
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		table := tx.Statement.Table
		if tx.Statement.Schema != nil {
			table = tx.Statement.Schema.Table
		}
		if table != "users" {
			return
		}
		values, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		if _, changesQuota := values["quota"]; changesQuota {
			count.Add(1)
		}
	}))
	return &count
}

func TestRelaySeedreamRejectsFinalZeroBeforeBillingAndDispatch(t *testing.T) {
	fixture := newSeedreamBillingOrderFixture(t)
	quotaMutations := recordUserQuotaMutations(t, fixture.db)
	var dispatches atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dispatches.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	tests := []struct {
		name          string
		body          string
		quota         int
		passThrough   bool
		paramOverride map[string]any
	}{
		{
			name:  "channel override with insufficient balance",
			body:  `{"model":"` + seedreamBillingOrderModel + `","prompt":"test","size":"1K","n":1}`,
			quota: 1,
			paramOverride: map[string]any{"operations": []any{
				map[string]any{"path": "n", "mode": "set", "value": 0},
			}},
		},
		{
			name:        "pass-through with insufficient balance",
			body:        `{"model":"` + seedreamBillingOrderModel + `","prompt":"test","size":"1K","n":0}`,
			quota:       1,
			passThrough: true,
		},
		{
			name:  "channel override with sufficient balance",
			body:  `{"model":"` + seedreamBillingOrderModel + `","prompt":"test","size":"1K","n":1}`,
			quota: 500_000,
			paramOverride: map[string]any{"operations": []any{
				map[string]any{"path": "n", "mode": "set", "value": 0},
			}},
		},
	}

	for index, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture.user.Quota = testCase.quota
			fixture.token.RemainQuota = testCase.quota
			fixture.resetQuota(t, testCase.quota)
			quotaMutations.Store(0)
			dispatches.Store(0)

			recorder := fixture.relay(
				t,
				fmt.Sprintf("seedream-invalid-%d", index),
				testCase.body,
				upstream.URL,
				testCase.passThrough,
				testCase.paramOverride,
			)

			assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			assert.Contains(t, recorder.Body.String(), "invalid_request")
			assert.Zero(t, dispatches.Load(), "invalid final request must not reach the provider")
			assert.Zero(t, quotaMutations.Load(), "invalid final request must not reserve or refund quota")

			require.Eventually(t, func() bool {
				var user model.User
				var token model.Token
				if fixture.db.First(&user, fixture.user.Id).Error != nil ||
					fixture.db.First(&token, fixture.token.Id).Error != nil {
					return false
				}
				return user.Quota == testCase.quota && token.RemainQuota == testCase.quota
			}, 3*time.Second, 10*time.Millisecond)
			assert.Zero(t, quotaMutations.Load(), "invalid final request must not reserve or refund quota")
		})
	}
}

func TestRelaySeedreamValidRequestBillsOnceAroundProviderDispatch(t *testing.T) {
	fixture := newSeedreamBillingOrderFixture(t)
	quotaMutations := recordUserQuotaMutations(t, fixture.db)
	const initialQuota = 500_000
	expectedQuota := billingexpr.QuotaRound(2 * 0.04109589041 * common.QuotaPerUnit)
	override := map[string]any{"operations": []any{
		map[string]any{"path": "n", "mode": "set", "value": 2},
	}}

	t.Run("settles once after successful dispatch", func(t *testing.T) {
		fixture.user.Quota = initialQuota
		fixture.token.RemainQuota = initialQuota
		fixture.resetQuota(t, initialQuota)
		quotaMutations.Store(0)
		type dispatchSnapshot struct {
			userQuota  int
			tokenQuota int
			body       []byte
			err        error
		}
		dispatched := make(chan dispatchSnapshot, 1)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			var user model.User
			var token model.Token
			userErr := fixture.db.First(&user, fixture.user.Id).Error
			tokenErr := fixture.db.First(&token, fixture.token.Id).Error
			body, bodyErr := io.ReadAll(request.Body)
			dispatched <- dispatchSnapshot{
				userQuota:  user.Quota,
				tokenQuota: token.RemainQuota,
				body:       body,
				err:        firstError(userErr, tokenErr, bodyErr),
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"https://output.invalid/image.png"}]}`)
		}))
		t.Cleanup(upstream.Close)

		recorder := fixture.relay(
			t,
			"seedream-valid-settle",
			`{"model":"`+seedreamBillingOrderModel+`","prompt":"test","size":"1K","n":1}`,
			upstream.URL,
			false,
			override,
		)

		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Len(t, dispatched, 1)
		snapshot := <-dispatched
		require.NoError(t, snapshot.err)
		assert.Equal(t, int64(2), gjson.GetBytes(snapshot.body, "n").Int())
		assert.Equal(t, initialQuota-expectedQuota, snapshot.userQuota)
		assert.Equal(t, initialQuota-expectedQuota, snapshot.tokenQuota)

		var user model.User
		var token model.Token
		require.NoError(t, fixture.db.First(&user, fixture.user.Id).Error)
		require.NoError(t, fixture.db.First(&token, fixture.token.Id).Error)
		assert.Equal(t, initialQuota-expectedQuota, user.Quota)
		assert.Equal(t, expectedQuota, user.UsedQuota)
		assert.Equal(t, 1, user.RequestCount)
		assert.Equal(t, initialQuota-expectedQuota, token.RemainQuota)
		assert.Equal(t, expectedQuota, token.UsedQuota)
		assert.Equal(t, int32(1), quotaMutations.Load(), "one wallet reservation must cover the request")

		var consumeLogs int64
		require.NoError(t, model.LOG_DB.Model(&model.Log{}).
			Where("request_id = ? AND type = ?", "seedream-valid-settle", model.LogTypeConsume).
			Count(&consumeLogs).Error)
		assert.Equal(t, int64(1), consumeLogs, "successful request must settle and log exactly once")
	})

	t.Run("refunds once after failed dispatch", func(t *testing.T) {
		fixture.user.Quota = initialQuota
		fixture.token.RemainQuota = initialQuota
		fixture.resetQuota(t, initialQuota)
		quotaMutations.Store(0)
		type dispatchSnapshot struct {
			userQuota  int
			tokenQuota int
			err        error
		}
		dispatched := make(chan dispatchSnapshot, 1)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			var user model.User
			var token model.Token
			userErr := fixture.db.First(&user, fixture.user.Id).Error
			tokenErr := fixture.db.First(&token, fixture.token.Id).Error
			dispatched <- dispatchSnapshot{
				userQuota:  user.Quota,
				tokenQuota: token.RemainQuota,
				err:        firstError(userErr, tokenErr),
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"fixture failure","type":"upstream_error"}}`)
		}))
		t.Cleanup(upstream.Close)

		recorder := fixture.relay(
			t,
			"seedream-valid-refund",
			`{"model":"`+seedreamBillingOrderModel+`","prompt":"test","size":"1K","n":1}`,
			upstream.URL,
			false,
			override,
		)

		require.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
		require.Len(t, dispatched, 1)
		snapshot := <-dispatched
		require.NoError(t, snapshot.err)
		assert.Equal(t, initialQuota-expectedQuota, snapshot.userQuota)
		assert.Equal(t, initialQuota-expectedQuota, snapshot.tokenQuota)
		require.Eventually(t, func() bool {
			var user model.User
			var token model.Token
			if fixture.db.First(&user, fixture.user.Id).Error != nil ||
				fixture.db.First(&token, fixture.token.Id).Error != nil {
				return false
			}
			return user.Quota == initialQuota && token.RemainQuota == initialQuota
		}, 3*time.Second, 10*time.Millisecond)
		assert.Equal(t, int32(2), quotaMutations.Load(), "one reservation and one refund must restore the wallet")

		var consumeLogs int64
		require.NoError(t, model.LOG_DB.Model(&model.Log{}).
			Where("request_id = ? AND type = ?", "seedream-valid-refund", model.LogTypeConsume).
			Count(&consumeLogs).Error)
		assert.Zero(t, consumeLogs)
	})
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
