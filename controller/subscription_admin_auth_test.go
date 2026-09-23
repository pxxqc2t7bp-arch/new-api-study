package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type subscriptionAdminFixture struct {
	db          *gorm.DB
	plan        model.SubscriptionPlan
	commonUser  model.User
	pluginAdmin model.User
	commonSub   model.UserSubscription
	pluginSub   model.UserSubscription
}

func setupSubscriptionAdminFixture(t *testing.T) *subscriptionAdminFixture {
	t.Helper()
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.UserSubscription{}))
	confirmPaymentComplianceForTest(t)

	fixture := &subscriptionAdminFixture{
		db: db,
		commonUser: model.User{
			Username: "subscription-common", Password: "password", Role: common.RoleCommonUser,
			Status: common.UserStatusEnabled, Group: "default", AffCode: "subscription-common",
		},
		pluginAdmin: model.User{
			Username: "subscription-plugin-admin", Password: "password", Role: common.RolePluginAdminUser,
			Status: common.UserStatusEnabled, Group: "default", AffCode: "subscription-plugin-admin",
		},
		plan: model.SubscriptionPlan{
			Title: "Protected subscription plan", DurationUnit: model.SubscriptionDurationMonth,
			DurationValue: 1, TotalAmount: 1000, Enabled: true,
		},
	}
	require.NoError(t, db.Create(&fixture.commonUser).Error)
	require.NoError(t, db.Create(&fixture.pluginAdmin).Error)
	require.NoError(t, db.Create(&fixture.plan).Error)
	model.InvalidateSubscriptionPlanCache(fixture.plan.Id)
	t.Cleanup(func() {
		model.InvalidateSubscriptionPlanCache(fixture.plan.Id)
	})

	now := model.GetDBTimestamp()
	fixture.commonSub = model.UserSubscription{
		UserId: fixture.commonUser.Id, PlanId: fixture.plan.Id, AmountTotal: 1000, AmountUsed: 77,
		StartTime: now - 3600, EndTime: now + 86400, Status: "active", Source: "test",
	}
	fixture.pluginSub = model.UserSubscription{
		UserId: fixture.pluginAdmin.Id, PlanId: fixture.plan.Id, AmountTotal: 1000, AmountUsed: 77,
		StartTime: now - 3600, EndTime: now + 86400, Status: "active", Source: "test",
	}
	require.NoError(t, db.Create(&fixture.commonSub).Error)
	require.NoError(t, db.Create(&fixture.pluginSub).Error)
	return fixture
}

func subscriptionTarget(fixture *subscriptionAdminFixture, role int) (*model.User, *model.UserSubscription) {
	if role == common.RolePluginAdminUser {
		return &fixture.pluginAdmin, &fixture.pluginSub
	}
	return &fixture.commonUser, &fixture.commonSub
}

type subscriptionAdminOperation struct {
	name   string
	invoke func(*testing.T, *subscriptionAdminFixture, int, *model.User, *model.UserSubscription) *httptest.ResponseRecorder
	verify func(*testing.T, *subscriptionAdminFixture, *model.User, *model.UserSubscription, bool)
}

func subscriptionAdminOperations() []subscriptionAdminOperation {
	return []subscriptionAdminOperation{
		{
			name: "bind subscription",
			invoke: func(t *testing.T, f *subscriptionAdminFixture, actorRole int, user *model.User, _ *model.UserSubscription) *httptest.ResponseRecorder {
				body := fmt.Sprintf(`{"user_id":%d,"plan_id":%d}`, user.Id, f.plan.Id)
				return performUserManagementRequest(t, actorRole, http.MethodPost, "/api/subscription/admin/bind", body, nil, AdminBindSubscription)
			},
			verify: func(t *testing.T, f *subscriptionAdminFixture, user *model.User, _ *model.UserSubscription, allowed bool) {
				var count int64
				require.NoError(t, f.db.Model(&model.UserSubscription{}).Where("user_id = ?", user.Id).Count(&count).Error)
				if allowed {
					assert.EqualValues(t, 2, count)
				} else {
					assert.EqualValues(t, 1, count)
				}
			},
		},
		{
			name: "list subscriptions",
			invoke: func(t *testing.T, _ *subscriptionAdminFixture, actorRole int, user *model.User, _ *model.UserSubscription) *httptest.ResponseRecorder {
				params := gin.Params{{Key: "id", Value: strconv.Itoa(user.Id)}}
				return performUserManagementRequest(t, actorRole, http.MethodGet, "/api/subscription/admin/users/1/subscriptions", "", params, AdminListUserSubscriptions)
			},
		},
		{
			name: "create subscription",
			invoke: func(t *testing.T, f *subscriptionAdminFixture, actorRole int, user *model.User, _ *model.UserSubscription) *httptest.ResponseRecorder {
				params := gin.Params{{Key: "id", Value: strconv.Itoa(user.Id)}}
				body := fmt.Sprintf(`{"plan_id":%d}`, f.plan.Id)
				return performUserManagementRequest(t, actorRole, http.MethodPost, "/api/subscription/admin/users/1/subscriptions", body, params, AdminCreateUserSubscription)
			},
			verify: func(t *testing.T, f *subscriptionAdminFixture, user *model.User, _ *model.UserSubscription, allowed bool) {
				var count int64
				require.NoError(t, f.db.Model(&model.UserSubscription{}).Where("user_id = ?", user.Id).Count(&count).Error)
				if allowed {
					assert.EqualValues(t, 2, count)
				} else {
					assert.EqualValues(t, 1, count)
				}
			},
		},
		{
			name: "reset subscriptions",
			invoke: func(t *testing.T, f *subscriptionAdminFixture, actorRole int, user *model.User, _ *model.UserSubscription) *httptest.ResponseRecorder {
				params := gin.Params{{Key: "id", Value: strconv.Itoa(user.Id)}}
				body := fmt.Sprintf(`{"plan_id":%d,"advance_reset_time":false}`, f.plan.Id)
				return performUserManagementRequest(t, actorRole, http.MethodPost, "/api/subscription/admin/users/1/subscriptions/reset", body, params, AdminResetUserSubscriptionsByPlan)
			},
			verify: func(t *testing.T, f *subscriptionAdminFixture, _ *model.User, sub *model.UserSubscription, allowed bool) {
				var stored model.UserSubscription
				require.NoError(t, f.db.First(&stored, sub.Id).Error)
				if allowed {
					assert.Zero(t, stored.AmountUsed)
				} else {
					assert.EqualValues(t, 77, stored.AmountUsed)
				}
			},
		},
		{
			name: "invalidate subscription",
			invoke: func(t *testing.T, f *subscriptionAdminFixture, actorRole int, _ *model.User, sub *model.UserSubscription) *httptest.ResponseRecorder {
				params := gin.Params{{Key: "id", Value: strconv.Itoa(sub.Id)}}
				body := fmt.Sprintf(`{"user_id":%d}`, f.commonUser.Id)
				return performUserManagementRequest(t, actorRole, http.MethodPost, "/api/subscription/admin/user_subscriptions/1/invalidate", body, params, AdminInvalidateUserSubscription)
			},
			verify: func(t *testing.T, f *subscriptionAdminFixture, _ *model.User, sub *model.UserSubscription, allowed bool) {
				var stored model.UserSubscription
				require.NoError(t, f.db.First(&stored, sub.Id).Error)
				if allowed {
					assert.Equal(t, "cancelled", stored.Status)
				} else {
					assert.Equal(t, "active", stored.Status)
				}
			},
		},
		{
			name: "delete subscription",
			invoke: func(t *testing.T, f *subscriptionAdminFixture, actorRole int, _ *model.User, sub *model.UserSubscription) *httptest.ResponseRecorder {
				params := gin.Params{{Key: "id", Value: strconv.Itoa(sub.Id)}}
				body := fmt.Sprintf(`{"user_id":%d}`, f.commonUser.Id)
				return performUserManagementRequest(t, actorRole, http.MethodDelete, "/api/subscription/admin/user_subscriptions/1", body, params, AdminDeleteUserSubscription)
			},
			verify: func(t *testing.T, f *subscriptionAdminFixture, _ *model.User, sub *model.UserSubscription, allowed bool) {
				var count int64
				require.NoError(t, f.db.Model(&model.UserSubscription{}).Where("id = ?", sub.Id).Count(&count).Error)
				if allowed {
					assert.Zero(t, count)
				} else {
					assert.EqualValues(t, 1, count)
				}
			},
		},
	}
}

func TestSubscriptionManagementRespectsTargetRole(t *testing.T) {
	operations := subscriptionAdminOperations()
	actors := []struct {
		name       string
		actorRole  int
		targetRole int
		allowed    bool
	}{
		{name: "admin plugin admin target", actorRole: common.RoleAdminUser, targetRole: common.RolePluginAdminUser, allowed: false},
		{name: "root plugin admin target", actorRole: common.RoleRootUser, targetRole: common.RolePluginAdminUser, allowed: true},
		{name: "admin common target", actorRole: common.RoleAdminUser, targetRole: common.RoleCommonUser, allowed: true},
	}

	for _, actor := range actors {
		for _, operation := range operations {
			t.Run(actor.name+"/"+operation.name, func(t *testing.T) {
				fixture := setupSubscriptionAdminFixture(t)
				user, sub := subscriptionTarget(fixture, actor.targetRole)

				response := operation.invoke(t, fixture, actor.actorRole, user, sub)

				assert.Equal(t, http.StatusOK, response.Code)
				if actor.allowed {
					assert.Contains(t, response.Body.String(), `"success":true`)
					if operation.name == "list subscriptions" {
						assert.Contains(t, response.Body.String(), fmt.Sprintf(`"user_id":%d`, user.Id))
					}
				} else {
					assert.Contains(t, response.Body.String(), `"success":false`)
					assert.NotContains(t, response.Body.String(), fmt.Sprintf(`"user_id":%d`, user.Id))
				}
				if operation.verify != nil {
					operation.verify(t, fixture, user, sub, actor.allowed)
				}
			})
		}
	}
}

func TestSubscriptionMutationsReauthorizeAfterConcurrentPromotion(t *testing.T) {
	for index, operation := range subscriptionAdminOperations() {
		if operation.name == "list subscriptions" {
			continue
		}
		t.Run(operation.name, func(t *testing.T) {
			fixture := setupSubscriptionAdminFixture(t)
			callbackName := fmt.Sprintf("test:promote_subscription_target_%d", index)
			promoted := false
			var promotionErr error
			require.NoError(t, fixture.db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if promoted || tx.Statement.Schema == nil || tx.Statement.Schema.Name != "User" {
					return
				}
				user, ok := tx.Statement.Dest.(*model.User)
				if !ok || user.Id != fixture.commonUser.Id {
					return
				}
				promoted = true
				promotionErr = fixture.db.Exec(
					"UPDATE users SET role = ? WHERE id = ?",
					common.RolePluginAdminUser,
					fixture.commonUser.Id,
				).Error
			}))

			response := operation.invoke(t, fixture, common.RoleAdminUser, &fixture.commonUser, &fixture.commonSub)

			require.True(t, promoted)
			require.NoError(t, promotionErr)
			assert.Contains(t, response.Body.String(), `"success":false`)
			operation.verify(t, fixture, &fixture.commonUser, &fixture.commonSub, false)
			var storedUser model.User
			require.NoError(t, fixture.db.First(&storedUser, fixture.commonUser.Id).Error)
			assert.Equal(t, common.RolePluginAdminUser, storedUser.Role)
		})
	}
}

func TestAdminResetPlanSubscriptionsRespectsTargetRole(t *testing.T) {
	for _, tc := range []struct {
		name             string
		actorRole        int
		wantPluginAmount int64
		wantResetCount   string
	}{
		{name: "admin skips plugin admin", actorRole: common.RoleAdminUser, wantPluginAmount: 77, wantResetCount: `"reset_count":1`},
		{name: "root resets plugin admin", actorRole: common.RoleRootUser, wantPluginAmount: 0, wantResetCount: `"reset_count":2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := setupSubscriptionAdminFixture(t)
			params := gin.Params{{Key: "id", Value: strconv.Itoa(fixture.plan.Id)}}

			response := performUserManagementRequest(t, tc.actorRole, http.MethodPost, "/api/subscription/admin/plans/1/subscriptions/reset", `{"advance_reset_time":false}`, params, AdminResetPlanSubscriptions)

			assert.Equal(t, http.StatusOK, response.Code)
			assert.Contains(t, response.Body.String(), `"success":true`)
			assert.Contains(t, response.Body.String(), tc.wantResetCount)
			var commonSub model.UserSubscription
			require.NoError(t, fixture.db.First(&commonSub, fixture.commonSub.Id).Error)
			assert.Zero(t, commonSub.AmountUsed)
			var pluginSub model.UserSubscription
			require.NoError(t, fixture.db.First(&pluginSub, fixture.pluginSub.Id).Error)
			assert.Equal(t, tc.wantPluginAmount, pluginSub.AmountUsed)
		})
	}
}

func TestSubscriptionAuthorizationDoesNotEnumerateProtectedTargets(t *testing.T) {
	fixture := setupSubscriptionAdminFixture(t)

	pluginResponse := performUserManagementRequest(
		t,
		common.RoleAdminUser,
		http.MethodGet,
		"/api/subscription/admin/users/1/subscriptions",
		"",
		gin.Params{{Key: "id", Value: strconv.Itoa(fixture.pluginAdmin.Id)}},
		AdminListUserSubscriptions,
	)
	missingResponse := performUserManagementRequest(
		t,
		common.RoleAdminUser,
		http.MethodGet,
		"/api/subscription/admin/users/999999/subscriptions",
		"",
		gin.Params{{Key: "id", Value: "999999"}},
		AdminListUserSubscriptions,
	)

	type errorResponse struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	var protected, missing errorResponse
	require.NoError(t, common.Unmarshal(pluginResponse.Body.Bytes(), &protected))
	require.NoError(t, common.Unmarshal(missingResponse.Body.Bytes(), &missing))
	assert.False(t, protected.Success)
	assert.False(t, missing.Success)
	assert.Equal(t, protected.Message, missing.Message)
}
