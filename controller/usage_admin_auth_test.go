package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type usageVisibilityFixture struct {
	startTimestamp        int64
	endTimestamp          int64
	commonUsername        string
	pluginUsername        string
	deletedPluginUsername string
	commonLogIDs          []int
}

type usageLogListingResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Total int         `json:"total"`
		Items []model.Log `json:"items"`
	} `json:"data"`
}

type usageLogStatResponse struct {
	Success bool       `json:"success"`
	Data    model.Stat `json:"data"`
}

type usageQuotaResponse struct {
	Success bool              `json:"success"`
	Data    []model.QuotaData `json:"data"`
}

type usageFlowResponse struct {
	Success bool                  `json:"success"`
	Data    []model.FlowQuotaData `json:"data"`
}

func setupUsageVisibilityFixture(t *testing.T, separateLogDB bool) usageVisibilityFixture {
	t.Helper()
	if separateLogDB {
		t.Setenv("TEST_MANAGE_USER_SEPARATE_LOG_DB", "1")
	} else {
		t.Setenv("TEST_MANAGE_USER_SEPARATE_LOG_DB", "0")
	}
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.QuotaData{}))

	commonUser := model.User{
		Username: "usage-common", Password: "password", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AffCode: "usage-common",
	}
	pluginAdmin := model.User{
		Username: "usage-plugin-admin", Password: "password", Role: common.RolePluginAdminUser,
		Status: common.UserStatusEnabled, Group: "default", AffCode: "usage-plugin-admin",
	}
	deletedPluginAdmin := model.User{
		Username: "usage-deleted-plugin-admin", Password: "password", Role: common.RolePluginAdminUser,
		Status: common.UserStatusEnabled, Group: "default", AffCode: "usage-deleted-plugin-admin",
	}
	require.NoError(t, db.Create(&commonUser).Error)
	require.NoError(t, db.Create(&pluginAdmin).Error)
	require.NoError(t, db.Create(&deletedPluginAdmin).Error)

	now := time.Now().Unix()
	logs := []model.Log{
		{
			UserId: commonUser.Id, Username: commonUser.Username, CreatedAt: now - 4,
			Type: model.LogTypeConsume, Quota: 10, PromptTokens: 1, CompletionTokens: 2,
		},
		{
			UserId: pluginAdmin.Id, Username: pluginAdmin.Username, CreatedAt: now - 3,
			Type: model.LogTypeConsume, Quota: 100, PromptTokens: 10, CompletionTokens: 20,
		},
		{
			UserId: commonUser.Id, Username: commonUser.Username, CreatedAt: now - 2,
			Type: model.LogTypeConsume, Quota: 15, PromptTokens: 4, CompletionTokens: 5,
		},
		{
			UserId: deletedPluginAdmin.Id, Username: deletedPluginAdmin.Username, CreatedAt: now - 1,
			Type: model.LogTypeConsume, Quota: 200, PromptTokens: 20, CompletionTokens: 30,
		},
	}
	require.NoError(t, model.LOG_DB.Create(&logs).Error)

	quotaRows := []model.QuotaData{
		{
			UserID: commonUser.Id, Username: commonUser.Username, ModelName: "shared-model",
			CreatedAt: 1500, UseGroup: "default", Count: 1, Quota: 10, TokenUsed: 3,
		},
		{
			UserID: commonUser.Id, Username: commonUser.Username, ModelName: "shared-model",
			CreatedAt: 1500, UseGroup: "default", Count: 1, Quota: 15, TokenUsed: 4,
		},
		{
			UserID: pluginAdmin.Id, Username: pluginAdmin.Username, ModelName: "shared-model",
			CreatedAt: 1500, UseGroup: "plugin", Count: 1, Quota: 100, TokenUsed: 30,
		},
		{
			UserID: deletedPluginAdmin.Id, Username: deletedPluginAdmin.Username, ModelName: "shared-model",
			CreatedAt: 1500, UseGroup: "deleted-plugin", Count: 1, Quota: 200, TokenUsed: 50,
		},
	}
	require.NoError(t, db.Create(&quotaRows).Error)
	require.NoError(t, db.Delete(&deletedPluginAdmin).Error)

	return usageVisibilityFixture{
		startTimestamp:        now - 10,
		endTimestamp:          now + 10,
		commonUsername:        commonUser.Username,
		pluginUsername:        pluginAdmin.Username,
		deletedPluginUsername: deletedPluginAdmin.Username,
		commonLogIDs:          []int{logs[2].Id, logs[0].Id},
	}
}

func decodeUsageResponse[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code)
	var response T
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	return response
}

func usageLogUsernames(logs []model.Log) []string {
	usernames := make([]string, 0, len(logs))
	for _, log := range logs {
		usernames = append(usernames, log.Username)
	}
	return usernames
}

func usageQuotaUsernames(rows []model.QuotaData) []string {
	usernames := make([]string, 0, len(rows))
	for _, row := range rows {
		usernames = append(usernames, row.Username)
	}
	return usernames
}

func usageFlowUsernames(rows []model.FlowQuotaData) []string {
	usernames := make([]string, 0, len(rows))
	for _, row := range rows {
		usernames = append(usernames, row.Username)
	}
	return usernames
}

func TestAdminUsageEndpointsRespectViewerRole(t *testing.T) {
	for _, tc := range []struct {
		name          string
		separateLogDB bool
	}{
		{name: "shared log database"},
		{name: "separate log database", separateLogDB: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := setupUsageVisibilityFixture(t, tc.separateLogDB)

			t.Run("logs filter before pagination and exact username lookup", func(t *testing.T) {
				basePath := fmt.Sprintf(
					"/api/log/?start_timestamp=%d&end_timestamp=%d",
					fixture.startTimestamp,
					fixture.endTimestamp,
				)
				adminPageOne := decodeUsageResponse[usageLogListingResponse](
					t,
					performUserManagementRequest(t, common.RoleAdminUser, http.MethodGet, basePath+"&p=1&page_size=1", "", nil, GetAllLogs),
				)
				assert.Equal(t, 2, adminPageOne.Data.Total)
				require.Len(t, adminPageOne.Data.Items, 1)
				assert.Equal(t, fixture.commonUsername, adminPageOne.Data.Items[0].Username)
				assert.Equal(t, fixture.commonLogIDs[0], adminPageOne.Data.Items[0].Id)

				adminPageTwo := decodeUsageResponse[usageLogListingResponse](
					t,
					performUserManagementRequest(t, common.RoleAdminUser, http.MethodGet, basePath+"&p=2&page_size=1", "", nil, GetAllLogs),
				)
				assert.Equal(t, 2, adminPageTwo.Data.Total)
				require.Len(t, adminPageTwo.Data.Items, 1)
				assert.Equal(t, fixture.commonUsername, adminPageTwo.Data.Items[0].Username)
				assert.Equal(t, fixture.commonLogIDs[1], adminPageTwo.Data.Items[0].Id)

				adminProtected := decodeUsageResponse[usageLogListingResponse](
					t,
					performUserManagementRequest(
						t,
						common.RoleAdminUser,
						http.MethodGet,
						basePath+"&p=1&page_size=10&username="+fixture.deletedPluginUsername,
						"",
						nil,
						GetAllLogs,
					),
				)
				assert.Zero(t, adminProtected.Data.Total)
				assert.Empty(t, adminProtected.Data.Items)

				adminCommon := decodeUsageResponse[usageLogListingResponse](
					t,
					performUserManagementRequest(
						t,
						common.RoleAdminUser,
						http.MethodGet,
						basePath+"&p=1&page_size=10&username="+fixture.commonUsername,
						"",
						nil,
						GetAllLogs,
					),
				)
				assert.Equal(t, 2, adminCommon.Data.Total)
				assert.Equal(t, []string{fixture.commonUsername, fixture.commonUsername}, usageLogUsernames(adminCommon.Data.Items))

				rootPage := decodeUsageResponse[usageLogListingResponse](
					t,
					performUserManagementRequest(t, common.RoleRootUser, http.MethodGet, basePath+"&p=1&page_size=10", "", nil, GetAllLogs),
				)
				assert.Equal(t, 4, rootPage.Data.Total)
				assert.ElementsMatch(
					t,
					[]string{fixture.commonUsername, fixture.commonUsername, fixture.pluginUsername, fixture.deletedPluginUsername},
					usageLogUsernames(rootPage.Data.Items),
				)
			})

			t.Run("log stats filter quota rpm and tpm", func(t *testing.T) {
				path := fmt.Sprintf(
					"/api/log/stat?start_timestamp=%d&end_timestamp=%d",
					fixture.startTimestamp,
					fixture.endTimestamp,
				)
				adminStat := decodeUsageResponse[usageLogStatResponse](
					t,
					performUserManagementRequest(t, common.RoleAdminUser, http.MethodGet, path, "", nil, GetLogsStat),
				)
				assert.Equal(t, model.Stat{Quota: 25, Rpm: 2, Tpm: 12}, adminStat.Data)

				adminProtected := decodeUsageResponse[usageLogStatResponse](
					t,
					performUserManagementRequest(
						t,
						common.RoleAdminUser,
						http.MethodGet,
						path+"&username="+fixture.deletedPluginUsername,
						"",
						nil,
						GetLogsStat,
					),
				)
				assert.Equal(t, model.Stat{}, adminProtected.Data)

				rootStat := decodeUsageResponse[usageLogStatResponse](
					t,
					performUserManagementRequest(t, common.RoleRootUser, http.MethodGet, path, "", nil, GetLogsStat),
				)
				assert.Equal(t, model.Stat{Quota: 325, Rpm: 4, Tpm: 92}, rootStat.Data)
			})

			t.Run("quota aggregates filter before grouping", func(t *testing.T) {
				const path = "/api/data/?start_timestamp=1000&end_timestamp=2000"
				adminAll := decodeUsageResponse[usageQuotaResponse](
					t,
					performUserManagementRequest(t, common.RoleAdminUser, http.MethodGet, path, "", nil, GetAllQuotaDates),
				)
				require.Len(t, adminAll.Data, 1)
				assert.Equal(t, 25, adminAll.Data[0].Quota)
				assert.Equal(t, 2, adminAll.Data[0].Count)
				assert.Equal(t, 7, adminAll.Data[0].TokenUsed)

				adminProtected := decodeUsageResponse[usageQuotaResponse](
					t,
					performUserManagementRequest(
						t,
						common.RoleAdminUser,
						http.MethodGet,
						path+"&username="+fixture.deletedPluginUsername,
						"",
						nil,
						GetAllQuotaDates,
					),
				)
				assert.Empty(t, adminProtected.Data)

				adminCommon := decodeUsageResponse[usageQuotaResponse](
					t,
					performUserManagementRequest(
						t,
						common.RoleAdminUser,
						http.MethodGet,
						path+"&username="+fixture.commonUsername,
						"",
						nil,
						GetAllQuotaDates,
					),
				)
				require.Len(t, adminCommon.Data, 1)
				assert.Equal(t, 25, adminCommon.Data[0].Quota)

				rootAll := decodeUsageResponse[usageQuotaResponse](
					t,
					performUserManagementRequest(t, common.RoleRootUser, http.MethodGet, path, "", nil, GetAllQuotaDates),
				)
				require.Len(t, rootAll.Data, 1)
				assert.Equal(t, 325, rootAll.Data[0].Quota)
				assert.Equal(t, 4, rootAll.Data[0].Count)
				assert.Equal(t, 87, rootAll.Data[0].TokenUsed)
			})

			t.Run("quota user grouping excludes protected owners", func(t *testing.T) {
				const path = "/api/data/users?start_timestamp=1000&end_timestamp=2000"
				adminGroups := decodeUsageResponse[usageQuotaResponse](
					t,
					performUserManagementRequest(t, common.RoleAdminUser, http.MethodGet, path, "", nil, GetQuotaDatesByUser),
				)
				require.Len(t, adminGroups.Data, 1)
				assert.Equal(t, fixture.commonUsername, adminGroups.Data[0].Username)
				assert.Equal(t, 25, adminGroups.Data[0].Quota)
				assert.Equal(t, 2, adminGroups.Data[0].Count)
				assert.Equal(t, 7, adminGroups.Data[0].TokenUsed)

				rootGroups := decodeUsageResponse[usageQuotaResponse](
					t,
					performUserManagementRequest(t, common.RoleRootUser, http.MethodGet, path, "", nil, GetQuotaDatesByUser),
				)
				require.Len(t, rootGroups.Data, 3)
				assert.ElementsMatch(
					t,
					[]string{fixture.commonUsername, fixture.pluginUsername, fixture.deletedPluginUsername},
					usageQuotaUsernames(rootGroups.Data),
				)
			})

			t.Run("flow rows exclude protected owners and exact matches", func(t *testing.T) {
				const path = "/api/data/flow?start_timestamp=1000&end_timestamp=2000"
				adminFlow := decodeUsageResponse[usageFlowResponse](
					t,
					performUserManagementRequest(t, common.RoleAdminUser, http.MethodGet, path, "", nil, GetAllFlowQuotaDates),
				)
				require.Len(t, adminFlow.Data, 1)
				assert.Equal(t, fixture.commonUsername, adminFlow.Data[0].Username)
				assert.Equal(t, 25, adminFlow.Data[0].Quota)

				adminProtected := decodeUsageResponse[usageFlowResponse](
					t,
					performUserManagementRequest(
						t,
						common.RoleAdminUser,
						http.MethodGet,
						path+"&username="+fixture.deletedPluginUsername,
						"",
						nil,
						GetAllFlowQuotaDates,
					),
				)
				assert.Empty(t, adminProtected.Data)

				rootFlow := decodeUsageResponse[usageFlowResponse](
					t,
					performUserManagementRequest(t, common.RoleRootUser, http.MethodGet, path, "", nil, GetAllFlowQuotaDates),
				)
				require.Len(t, rootFlow.Data, 3)
				assert.ElementsMatch(
					t,
					[]string{fixture.commonUsername, fixture.pluginUsername, fixture.deletedPluginUsername},
					usageFlowUsernames(rootFlow.Data),
				)
			})
		})
	}
}
