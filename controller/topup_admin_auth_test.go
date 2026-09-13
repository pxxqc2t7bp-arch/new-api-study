package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAdminCompleteTopUpFixture(t *testing.T, targetRole int, provider string) (*gorm.DB, model.User, model.TopUp) {
	t.Helper()
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.TopUp{}))

	previousQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 100
	t.Cleanup(func() { common.QuotaPerUnit = previousQuotaPerUnit })

	user := model.User{
		Username: "manual-topup-target", Password: "password", Role: targetRole,
		Status: common.UserStatusEnabled, Group: "default", Quota: 50, AffCode: "manual-topup-target",
	}
	require.NoError(t, db.Create(&user).Error)
	topUp := model.TopUp{
		UserId: user.Id, Amount: 2, Money: 1.25, TradeNo: fmt.Sprintf("MANUAL-%d-%s", targetRole, provider),
		PaymentMethod: provider, PaymentProvider: provider, CreateTime: common.GetTimestamp(),
		Status: common.TopUpStatusPending,
	}
	require.NoError(t, db.Create(&topUp).Error)
	return db, user, topUp
}

func completeTopUpAs(t *testing.T, actorRole int, tradeNo string) topUpAdminResponse {
	t.Helper()
	response := completeTopUpRequestAs(t, actorRole, tradeNo)
	var body topUpAdminResponse
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body), response.Body.String())
	return body
}

func completeTopUpRequestAs(t *testing.T, actorRole int, tradeNo string) *httptest.ResponseRecorder {
	t.Helper()
	response := performUserManagementRequest(
		t,
		actorRole,
		http.MethodPost,
		"/api/user/topup/complete",
		fmt.Sprintf(`{"trade_no":%q}`, tradeNo),
		nil,
		AdminCompleteTopUp,
	)
	require.Equal(t, http.StatusOK, response.Code)
	return response
}

type topUpAdminResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type topUpListingResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Total int           `json:"total"`
		Items []model.TopUp `json:"items"`
	} `json:"data"`
}

func getTopUpListingAs(t *testing.T, role int, path string) (*httptest.ResponseRecorder, topUpListingResponse) {
	t.Helper()
	recorder := performUserManagementRequest(t, role, http.MethodGet, path, "", nil, GetAllTopUps)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response topUpListingResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	require.True(t, response.Success, recorder.Body.String())
	return recorder, response
}

func listedTopUpIDs(topUps []model.TopUp) []int {
	ids := make([]int, 0, len(topUps))
	for _, topUp := range topUps {
		ids = append(ids, topUp.Id)
	}
	return ids
}

func assertManualTopUpState(t *testing.T, db *gorm.DB, userId int, tradeNo, wantStatus string, wantQuota int) {
	t.Helper()
	var user model.User
	require.NoError(t, db.First(&user, userId).Error)
	assert.Equal(t, wantQuota, user.Quota)
	var topUp model.TopUp
	require.NoError(t, db.First(&topUp, "trade_no = ?", tradeNo).Error)
	assert.Equal(t, wantStatus, topUp.Status)
}

func TestAdminCompleteTopUpRespectsTargetRole(t *testing.T) {
	testCases := []struct {
		name       string
		actorRole  int
		targetRole int
		provider   string
		allowed    bool
		credited   int
	}{
		{
			name: "admin cannot complete plugin admin topup", actorRole: common.RoleAdminUser,
			targetRole: common.RolePluginAdminUser, provider: model.PaymentProviderEpay,
		},
		{
			name: "root can complete plugin admin stripe topup", actorRole: common.RoleRootUser,
			targetRole: common.RolePluginAdminUser, provider: model.PaymentProviderStripe, allowed: true, credited: 125,
		},
		{
			name: "admin can complete common user epay topup", actorRole: common.RoleAdminUser,
			targetRole: common.RoleCommonUser, provider: model.PaymentProviderEpay, allowed: true, credited: 200,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			db, user, topUp := setupAdminCompleteTopUpFixture(t, tc.targetRole, tc.provider)

			response := completeTopUpAs(t, tc.actorRole, topUp.TradeNo)

			if !tc.allowed {
				assert.False(t, response.Success)
				assert.Equal(t, i18n.Translate(i18n.DefaultLang, i18n.MsgUserNoPermissionHigherLevel), response.Message)
				assert.NotContains(t, response.Message, topUp.TradeNo)
				assert.NotContains(t, response.Message, user.Username)
				assertManualTopUpState(t, db, user.Id, topUp.TradeNo, common.TopUpStatusPending, 50)
				return
			}

			assert.True(t, response.Success)
			assertManualTopUpState(t, db, user.Id, topUp.TradeNo, common.TopUpStatusSuccess, 50+tc.credited)

			response = completeTopUpAs(t, tc.actorRole, topUp.TradeNo)
			assert.True(t, response.Success)
			assertManualTopUpState(t, db, user.Id, topUp.TradeNo, common.TopUpStatusSuccess, 50+tc.credited)
		})
	}
}

func TestAdminCompleteTopUpAuthorizesBeforeCompletedOrderIdempotency(t *testing.T) {
	db, user, topUp := setupAdminCompleteTopUpFixture(t, common.RolePluginAdminUser, model.PaymentProviderEpay)
	wantMessage := i18n.Translate(i18n.DefaultLang, i18n.MsgUserNoPermissionHigherLevel)

	pendingResponse := completeTopUpAs(t, common.RoleAdminUser, topUp.TradeNo)
	require.False(t, pendingResponse.Success)
	assert.Equal(t, wantMessage, pendingResponse.Message)
	assertManualTopUpState(t, db, user.Id, topUp.TradeNo, common.TopUpStatusPending, 50)

	require.NoError(t, db.Model(&model.TopUp{}).
		Where("id = ?", topUp.Id).
		Update("status", common.TopUpStatusSuccess).Error)

	completedResponse := completeTopUpAs(t, common.RoleAdminUser, topUp.TradeNo)
	assert.False(t, completedResponse.Success)
	assert.Equal(t, pendingResponse.Message, completedResponse.Message)
	assert.Equal(t, wantMessage, completedResponse.Message)
	assert.NotContains(t, completedResponse.Message, topUp.TradeNo)
	assert.NotContains(t, completedResponse.Message, user.Username)
	assertManualTopUpState(t, db, user.Id, topUp.TradeNo, common.TopUpStatusSuccess, 50)
}

func TestAdminCompleteTopUpHidesProtectedOrderExistence(t *testing.T) {
	db, user, pending := setupAdminCompleteTopUpFixture(t, common.RolePluginAdminUser, model.PaymentProviderEpay)
	completed := model.TopUp{
		UserId: user.Id, Amount: 2, TradeNo: "MANUAL-PROTECTED-COMPLETED",
		PaymentMethod: model.PaymentProviderEpay, PaymentProvider: model.PaymentProviderEpay,
		CreateTime: common.GetTimestamp(), Status: common.TopUpStatusSuccess,
	}
	missingOwner := model.TopUp{
		UserId: user.Id + 1_000_000, Amount: 2, TradeNo: "MANUAL-MISSING-OWNER",
		PaymentMethod: model.PaymentProviderEpay, PaymentProvider: model.PaymentProviderEpay,
		CreateTime: common.GetTimestamp(), Status: common.TopUpStatusPending,
	}
	require.NoError(t, db.Create(&completed).Error)
	require.NoError(t, db.Create(&missingOwner).Error)

	missing := completeTopUpRequestAs(t, common.RoleAdminUser, "MANUAL-MISSING-ORDER")
	for name, tradeNo := range map[string]string{
		"missing owner":       missingOwner.TradeNo,
		"protected pending":   pending.TradeNo,
		"protected completed": completed.TradeNo,
	} {
		t.Run(name, func(t *testing.T) {
			response := completeTopUpRequestAs(t, common.RoleAdminUser, tradeNo)
			assert.Equal(t, missing.Body.String(), response.Body.String())
		})
	}
	assert.JSONEq(t, fmt.Sprintf(
		`{"success":false,"message":%q}`,
		i18n.Translate(i18n.DefaultLang, i18n.MsgUserNoPermissionHigherLevel),
	), missing.Body.String())
	assertManualTopUpState(t, db, user.Id, pending.TradeNo, common.TopUpStatusPending, 50)
	assertManualTopUpState(t, db, user.Id, completed.TradeNo, common.TopUpStatusSuccess, 50)
}

func TestAdminCompleteTopUpPreservesInternalDatabaseErrors(t *testing.T) {
	db, _, topUp := setupAdminCompleteTopUpFixture(t, common.RoleCommonUser, model.PaymentProviderEpay)
	forcedErr := errors.New("forced topup database failure")
	callbackName := "test:fail_manual_topup_query"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "TopUp" {
			_ = tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	response := completeTopUpRequestAs(t, common.RoleAdminUser, topUp.TradeNo)

	assert.JSONEq(t, `{"success":false,"message":"forced topup database failure"}`, response.Body.String())
}

func TestTopUpListingsRespectViewerRole(t *testing.T) {
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.TopUp{}))

	commonUser := model.User{
		Username: "topup-listing-common", Password: "password", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AffCode: "topup-listing-common",
	}
	pluginAdmin := model.User{
		Username: "topup-listing-plugin-admin", Password: "password", Role: common.RolePluginAdminUser,
		Status: common.UserStatusEnabled, Group: "default", AffCode: "topup-listing-plugin-admin",
	}
	require.NoError(t, db.Create(&commonUser).Error)
	require.NoError(t, db.Create(&pluginAdmin).Error)

	topUps := []model.TopUp{
		{UserId: commonUser.Id, TradeNo: "TOPUP-COMMON-OLD", CreateTime: 1, Status: common.TopUpStatusSuccess},
		{UserId: pluginAdmin.Id, TradeNo: "TOPUP-PLUGIN-OLD", CreateTime: 2, Status: common.TopUpStatusSuccess},
		{UserId: commonUser.Id, TradeNo: "TOPUP-COMMON-MIDDLE", CreateTime: 3, Status: common.TopUpStatusSuccess},
		{UserId: pluginAdmin.Id, TradeNo: "TOPUP-PLUGIN-PROTECTED-EXACT", CreateTime: 4, Status: common.TopUpStatusSuccess},
		{UserId: commonUser.Id, TradeNo: "TOPUP-COMMON-NEW", CreateTime: 5, Status: common.TopUpStatusSuccess},
	}
	require.NoError(t, db.Create(&topUps).Error)

	_, adminPageOne := getTopUpListingAs(t, common.RoleAdminUser, "/api/user/topup?p=1&page_size=2")
	assert.Equal(t, 3, adminPageOne.Data.Total)
	assert.Equal(t, []int{topUps[4].Id, topUps[2].Id}, listedTopUpIDs(adminPageOne.Data.Items))

	_, adminPageTwo := getTopUpListingAs(t, common.RoleAdminUser, "/api/user/topup?p=2&page_size=2")
	assert.Equal(t, 3, adminPageTwo.Data.Total)
	assert.Equal(t, []int{topUps[0].Id}, listedTopUpIDs(adminPageTwo.Data.Items))

	_, rootList := getTopUpListingAs(t, common.RoleRootUser, "/api/user/topup?p=1&page_size=5")
	assert.Equal(t, 5, rootList.Data.Total)
	assert.Equal(
		t,
		[]int{topUps[4].Id, topUps[3].Id, topUps[2].Id, topUps[1].Id, topUps[0].Id},
		listedTopUpIDs(rootList.Data.Items),
	)

	adminRecorder, adminProtectedSearch := getTopUpListingAs(
		t,
		common.RoleAdminUser,
		"/api/user/topup?keyword=TOPUP-PLUGIN-PROTECTED-EXACT&p=1&page_size=10",
	)
	assert.Zero(t, adminProtectedSearch.Data.Total)
	assert.Empty(t, adminProtectedSearch.Data.Items)
	assert.NotContains(t, adminRecorder.Body.String(), topUps[3].TradeNo)

	_, rootProtectedSearch := getTopUpListingAs(
		t,
		common.RoleRootUser,
		"/api/user/topup?keyword=TOPUP-PLUGIN-PROTECTED-EXACT&p=1&page_size=10",
	)
	assert.Equal(t, 1, rootProtectedSearch.Data.Total)
	assert.Equal(t, []int{topUps[3].Id}, listedTopUpIDs(rootProtectedSearch.Data.Items))

	_, adminCommonSearch := getTopUpListingAs(
		t,
		common.RoleAdminUser,
		"/api/user/topup?keyword=TOPUP-COMMON-MIDDLE&p=1&page_size=10",
	)
	assert.Equal(t, 1, adminCommonSearch.Data.Total)
	assert.Equal(t, []int{topUps[2].Id}, listedTopUpIDs(adminCommonSearch.Data.Items))
}

func TestAdminCompleteTopUpReauthorizesAfterTransactionTimePromotion(t *testing.T) {
	db, user, topUp := setupAdminCompleteTopUpFixture(t, common.RoleCommonUser, model.PaymentProviderEpay)
	callbackName := "test:promote_manual_topup_owner"
	promoted := false
	var promotionErr error
	var promotionRows int64
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if promoted || tx.Statement.Schema == nil || tx.Statement.Schema.Name != "TopUp" {
			return
		}
		loaded, ok := tx.Statement.Dest.(*model.TopUp)
		if !ok || loaded.TradeNo != topUp.TradeNo {
			return
		}
		promoted = true
		result := tx.Session(&gorm.Session{NewDB: true}).Exec(
			"UPDATE users SET role = ? WHERE id = ?",
			common.RolePluginAdminUser,
			user.Id,
		)
		promotionErr = result.Error
		promotionRows = result.RowsAffected
	}))

	response := completeTopUpAs(t, common.RoleAdminUser, topUp.TradeNo)

	require.True(t, promoted)
	require.NoError(t, promotionErr)
	require.EqualValues(t, 1, promotionRows)
	assert.False(t, response.Success)
	assert.Equal(t, i18n.Translate(i18n.DefaultLang, i18n.MsgUserNoPermissionHigherLevel), response.Message)
	assertManualTopUpState(t, db, user.Id, topUp.TradeNo, common.TopUpStatusPending, 50)
}
