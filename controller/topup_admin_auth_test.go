package controller

import (
	"fmt"
	"net/http"
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
	var body topUpAdminResponse
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body), response.Body.String())
	return body
}

type topUpAdminResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
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
