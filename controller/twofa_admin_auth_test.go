package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type adminTwoFAStatsResponse struct {
	Success bool `json:"success"`
	Data    struct {
		TotalUsers   int64  `json:"total_users"`
		EnabledUsers int64  `json:"enabled_users"`
		EnabledRate  string `json:"enabled_rate"`
	} `json:"data"`
}

func getAdminTwoFAStatsAs(t *testing.T, role int) adminTwoFAStatsResponse {
	t.Helper()
	recorder := performUserManagementRequest(
		t,
		role,
		http.MethodGet,
		"/api/user/2fa/stats",
		"",
		nil,
		Admin2FAStats,
	)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response adminTwoFAStatsResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	require.True(t, response.Success, recorder.Body.String())
	return response
}

func TestAdminTwoFAStatsRespectViewerRole(t *testing.T) {
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.TwoFA{}))

	users := []model.User{
		{Username: "twofa-common-enabled", Password: "password", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "twofa-common-enabled"},
		{Username: "twofa-common-disabled", Password: "password", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "twofa-common-disabled"},
		{Username: "twofa-plugin-enabled-one", Password: "password", Role: common.RolePluginAdminUser, Status: common.UserStatusEnabled, AffCode: "twofa-plugin-enabled-one"},
		{Username: "twofa-plugin-enabled-two", Password: "password", Role: common.RolePluginAdminUser, Status: common.UserStatusEnabled, AffCode: "twofa-plugin-enabled-two"},
		{Username: "twofa-deleted-common", Password: "password", Role: common.RoleCommonUser, Status: common.UserStatusDisabled, AffCode: "twofa-deleted-common"},
		{Username: "twofa-deleted-plugin", Password: "password", Role: common.RolePluginAdminUser, Status: common.UserStatusDisabled, AffCode: "twofa-deleted-plugin"},
	}
	require.NoError(t, db.Create(&users).Error)
	for index, user := range users {
		require.NoError(t, db.Create(&model.TwoFA{
			UserId: user.Id, Secret: "twofa-secret", IsEnabled: index != 1,
		}).Error)
	}
	require.NoError(t, db.Delete(&users[4]).Error)
	require.NoError(t, db.Delete(&users[5]).Error)

	admin := getAdminTwoFAStatsAs(t, common.RoleAdminUser)
	assert.EqualValues(t, 2, admin.Data.TotalUsers)
	assert.EqualValues(t, 1, admin.Data.EnabledUsers)
	assert.Equal(t, "50.0%", admin.Data.EnabledRate)

	root := getAdminTwoFAStatsAs(t, common.RoleRootUser)
	assert.EqualValues(t, 4, root.Data.TotalUsers)
	assert.EqualValues(t, 3, root.Data.EnabledUsers)
	assert.Equal(t, "75.0%", root.Data.EnabledRate)

	trustedStats, err := model.GetTwoFAStats()
	require.NoError(t, err)
	assert.Equal(t, root.Data.TotalUsers, trustedStats["total_users"])
	assert.Equal(t, root.Data.EnabledUsers, trustedStats["enabled_users"])
	assert.Equal(t, root.Data.EnabledRate, trustedStats["enabled_rate"])
}
