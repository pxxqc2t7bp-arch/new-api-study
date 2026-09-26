package controller

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/wsmanager"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type observingResponseRecorder struct {
	*httptest.ResponseRecorder
	beforeFirstWrite func()
	observed         bool
}

func (recorder *observingResponseRecorder) observe() {
	if recorder.observed {
		return
	}
	recorder.observed = true
	if recorder.beforeFirstWrite != nil {
		recorder.beforeFirstWrite()
	}
}

func (recorder *observingResponseRecorder) WriteHeader(statusCode int) {
	recorder.observe()
	recorder.ResponseRecorder.WriteHeader(statusCode)
}

func (recorder *observingResponseRecorder) Write(data []byte) (int, error) {
	recorder.observe()
	return recorder.ResponseRecorder.Write(data)
}

func createControllerPlanQuotaDomain(
	t *testing.T,
	db *gorm.DB,
	tag string,
	credential string,
) []model.Channel {
	t.Helper()

	hash, ok := model.PlanQuotaDomainHash(credential)
	require.True(t, ok)
	const generation int64 = 71
	const disabledUntil int64 = 2_000_000_000
	require.NoError(t, db.Create(&model.PlanQuotaDomain{
		CredentialHash: hash,
		Generation:     generation,
		State:          model.PlanQuotaDomainStateDisabled,
		DisabledUntil:  disabledUntil,
	}).Error)
	channels := []model.Channel{
		{
			Name: "manual-domain-source", Key: credential, Tag: &tag,
			Status: common.ChannelStatusAutoDisabled, Models: "gpt-4.1", Group: "default",
		},
		{
			Name: "manual-domain-peer", Key: credential, Tag: &tag,
			Status: common.ChannelStatusAutoDisabled, Models: "gpt-4.1", Group: "default",
		},
	}
	for index := range channels {
		channels[index].SetOtherInfo(map[string]any{
			"disabled_until":   disabledUntil,
			"quota_domain":     tag,
			"quota_domain_id":  hash,
			"quota_generation": strconv.FormatInt(generation, 10),
			"quota_type":       "plan",
		})
	}
	require.NoError(t, db.Create(&channels).Error)
	for index := range channels {
		require.NoError(t, channels[index].AddAbilities(db))
	}
	return channels
}

func assertControllerPlanQuotaDomainEnabled(
	t *testing.T,
	db *gorm.DB,
	channels []model.Channel,
) {
	t.Helper()

	for index := range channels {
		var stored model.Channel
		require.NoError(t, db.First(&stored, channels[index].Id).Error)
		assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
		assert.NotContains(t, stored.GetOtherInfo(), "quota_domain_id")
		assert.NotContains(t, stored.GetOtherInfo(), "quota_generation")
		var disabledAbilities int64
		require.NoError(t, db.Model(&model.Ability{}).
			Where("channel_id = ? AND enabled = ?", stored.Id, false).
			Count(&disabledAbilities).Error)
		assert.Zero(t, disabledAbilities)
	}
}

func TestUpdateChannelStatusManuallyRecoversPlanQuotaDomain(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})
	channels := createControllerPlanQuotaDomain(
		t,
		db,
		"plan:test:controller-single-enable",
		"controller-single-credential",
	)

	body := []byte(fmt.Sprintf(`{"status":%d}`, common.ChannelStatusEnabled))
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "id", Value: strconv.Itoa(channels[0].Id)}}
	context.Request = httptest.NewRequest(http.MethodPost, "/api/channel/status", bytes.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")

	UpdateChannelStatus(context)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool `json:"success"`
		Data    bool `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.True(t, response.Data)
	assertControllerPlanQuotaDomainEnabled(t, db, channels)
}

func TestUpdateChannelStatusRecoversPlanQuotaDomainFromOldGenerationManualSource(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})
	tag := "plan:test:controller-manual-source"
	const credential = "controller-manual-source-credential"
	channels := createControllerPlanQuotaDomain(t, db, tag, credential)
	require.True(t, model.UpdateChannelStatus(
		channels[0].Id,
		"",
		common.ChannelStatusManuallyDisabled,
		"operator pause",
	))
	require.NoError(t, db.Model(&model.Ability{}).
		Where("channel_id = ?", channels[0].Id).
		Update("enabled", true).Error)
	var manualBefore model.Channel
	require.NoError(t, db.First(&manualBefore, channels[0].Id).Error)
	manualInfoBefore := manualBefore.GetOtherInfo()
	oldGeneration := manualInfoBefore["quota_generation"]

	disabled, err := model.DisablePlanQuotaDomain(model.PlanQuotaDomainDisableRequest{
		FailingChannelID:   channels[1].Id,
		ObservedCredential: credential,
		ObservedTag:        tag,
		Reason:             "fresh quota event",
		ResetAt:            2_000_000_100,
	})
	require.NoError(t, err)
	require.Zero(t, disabled.NewlyDisabled)
	var peerBefore model.Channel
	require.NoError(t, db.First(&peerBefore, channels[1].Id).Error)
	require.NotEqual(t, oldGeneration, peerBefore.GetOtherInfo()["quota_generation"])

	body := []byte(fmt.Sprintf(`{"status":%d}`, common.ChannelStatusEnabled))
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "id", Value: strconv.Itoa(channels[0].Id)}}
	context.Request = httptest.NewRequest(http.MethodPost, "/api/channel/status", bytes.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")

	UpdateChannelStatus(context)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool `json:"success"`
		Data    bool `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.True(t, response.Data)
	var source model.Channel
	require.NoError(t, db.First(&source, channels[0].Id).Error)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, source.Status)
	sourceInfo := source.GetOtherInfo()
	assert.Equal(t, manualInfoBefore["status_reason"], sourceInfo["status_reason"])
	assert.Equal(t, manualInfoBefore["status_time"], sourceInfo["status_time"])
	assert.NotContains(t, sourceInfo, "quota_domain_id")
	assert.NotContains(t, sourceInfo, "quota_generation")
	assert.NotContains(t, sourceInfo, "quota_type")
	var peer model.Channel
	require.NoError(t, db.First(&peer, channels[1].Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, peer.Status)
	assert.NotContains(t, peer.GetOtherInfo(), "quota_domain_id")
	var sourceAbility model.Ability
	require.NoError(t, db.First(&sourceAbility, "channel_id = ?", source.Id).Error)
	assert.True(t, sourceAbility.Enabled)
}

func TestBatchUpdateChannelStatusManuallyRecoversPlanQuotaDomain(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})
	channels := createControllerPlanQuotaDomain(
		t,
		db,
		"plan:test:controller-batch-enable",
		"controller-batch-credential",
	)

	body := []byte(fmt.Sprintf(
		`{"ids":[%d],"status":%d}`,
		channels[0].Id,
		common.ChannelStatusEnabled,
	))
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/channel/status/batch", bytes.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")

	BatchUpdateChannelStatus(context)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool `json:"success"`
		Data    int  `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, 1, response.Data)
	assertControllerPlanQuotaDomainEnabled(t, db, channels)
}

func TestChannelStatusEndpointsReportPlanQuotaRecoveryFailure(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		body      func(int) []byte
		setParams func(*gin.Context, int)
		handle    func(*gin.Context)
	}{
		{
			name: "single",
			path: "/api/channel/1/status",
			body: func(_ int) []byte {
				return []byte(fmt.Sprintf(`{"status":%d}`, common.ChannelStatusEnabled))
			},
			setParams: func(context *gin.Context, id int) {
				context.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
			},
			handle: UpdateChannelStatus,
		},
		{
			name: "batch",
			path: "/api/channel/status/batch",
			body: func(id int) []byte {
				return []byte(fmt.Sprintf(
					`{"ids":[%d],"status":%d}`,
					id,
					common.ChannelStatusEnabled,
				))
			},
			setParams: func(_ *gin.Context, _ int) {},
			handle:    BatchUpdateChannelStatus,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Log{}))
			channels := createControllerPlanQuotaDomain(
				t,
				db,
				"plan:test:controller-recovery-failure-"+testCase.name,
				"controller-recovery-failure-credential-"+testCase.name,
			)

			forcedErr := errors.New("forced Plan quota recovery ability failure")
			callbackName := "test:controller_plan_recovery_failure_" + testCase.name
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Table == "abilities" {
					tx.AddError(forcedErr)
				}
			}))
			t.Cleanup(func() {
				require.NoError(t, db.Callback().Update().Remove(callbackName))
			})

			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			testCase.setParams(context, channels[0].Id)
			context.Request = httptest.NewRequest(
				http.MethodPost,
				testCase.path,
				bytes.NewReader(testCase.body(channels[0].Id)),
			)
			context.Request.Header.Set("Content-Type", "application/json")

			testCase.handle(context)

			require.Equal(t, http.StatusOK, recorder.Code)
			var response struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.False(t, response.Success)
			assert.Contains(t, response.Message, forcedErr.Error())

			var authority model.PlanQuotaDomain
			require.NoError(t, db.First(&authority).Error)
			assert.Equal(t, model.PlanQuotaDomainStateDisabled, authority.State)
			var stored model.Channel
			require.NoError(t, db.First(&stored, channels[0].Id).Error)
			assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
		})
	}
}

func TestChannelStatusEndpointsReportManualDisableFailure(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		action    string
		body      func(int) []byte
		params    func(int) string
		setParams func(*gin.Context, int)
		handle    func(*gin.Context)
	}{
		{
			name:   "single",
			path:   "/api/channel/1/status",
			action: "channel.status_update",
			body: func(_ int) []byte {
				return []byte(fmt.Sprintf(
					`{"status":%d}`,
					common.ChannelStatusManuallyDisabled,
				))
			},
			params: func(id int) string {
				return fmt.Sprintf(
					`{"changed":false,"id":%d,"status":%d}`,
					id,
					common.ChannelStatusManuallyDisabled,
				)
			},
			setParams: func(context *gin.Context, id int) {
				context.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
			},
			handle: UpdateChannelStatus,
		},
		{
			name:   "batch",
			path:   "/api/channel/status/batch",
			action: "channel.status_update_batch",
			body: func(id int) []byte {
				return []byte(fmt.Sprintf(
					`{"ids":[%d],"status":%d}`,
					id,
					common.ChannelStatusManuallyDisabled,
				))
			},
			params: func(id int) string {
				return fmt.Sprintf(
					`{"changed_ids":null,"count":0,"failed_id":%d,"status":%d,"total":1}`,
					id,
					common.ChannelStatusManuallyDisabled,
				)
			},
			setParams: func(_ *gin.Context, _ int) {},
			handle:    BatchUpdateChannelStatus,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.AuditLog{}))
			channel := model.Channel{
				Name:   "manual-disable-failure-" + testCase.name,
				Key:    "manual-disable-failure-key-" + testCase.name,
				Status: common.ChannelStatusEnabled,
				Models: "gpt-4.1",
				Group:  "default",
			}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(db))
			closed := make(chan string, 1)
			unregister := wsmanager.Register(channel.Id, "responses", func(reason string) {
				closed <- reason
			})
			t.Cleanup(unregister)

			forcedErr := errors.New("forced manual disable ability failure")
			callbackName := "test:controller_manual_disable_failure_" + testCase.name
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Table == "abilities" {
					tx.AddError(forcedErr)
				}
			}))
			t.Cleanup(func() {
				require.NoError(t, db.Callback().Update().Remove(callbackName))
			})

			recorder := &observingResponseRecorder{
				ResponseRecorder: httptest.NewRecorder(),
			}
			recorder.beforeFirstWrite = func() {
				var failureAudits int64
				assert.NoError(t, model.LOG_DB.Model(&model.AuditLog{}).
					Where("action = ? AND success = ?", testCase.action, false).
					Count(&failureAudits).Error)
				assert.Equal(t, int64(1), failureAudits)
			}
			context, _ := gin.CreateTestContext(recorder)
			context.Set("id", 900)
			context.Set("role", common.RoleRootUser)
			context.Set("username", "manual-disable-root")
			testCase.setParams(context, channel.Id)
			context.Request = httptest.NewRequest(
				http.MethodPost,
				testCase.path,
				bytes.NewReader(testCase.body(channel.Id)),
			)
			context.Request.Header.Set("Content-Type", "application/json")

			testCase.handle(context)

			require.Equal(t, http.StatusOK, recorder.Code)
			var response struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.False(t, response.Success)
			assert.Contains(t, response.Message, forcedErr.Error())

			var stored model.Channel
			require.NoError(t, db.First(&stored, channel.Id).Error)
			assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
			var ability model.Ability
			require.NoError(t, db.First(&ability, "channel_id = ?", channel.Id).Error)
			assert.True(t, ability.Enabled)
			select {
			case reason := <-closed:
				t.Fatalf("failed disable closed websocket: %s", reason)
			default:
			}
			var successfulAudits int64
			require.NoError(t, model.LOG_DB.Model(&model.AuditLog{}).
				Where("action = ? AND success = ?", testCase.action, true).
				Count(&successfulAudits).Error)
			assert.Zero(t, successfulAudits)
			var audit model.AuditLog
			require.NoError(t, model.LOG_DB.
				Where("action = ?", testCase.action).
				Last(&audit).Error)
			assert.False(t, audit.Success)
			require.NotNil(t, audit.Other.AuditInfo)
			assert.False(t, audit.Other.AuditInfo.Success)
			require.NotNil(t, audit.Other.Op)
			paramsJSON, err := common.Marshal(audit.Other.Op.Params)
			require.NoError(t, err)
			assert.JSONEq(t, testCase.params(channel.Id), string(paramsJSON))
		})
	}
}

func TestBatchUpdateChannelStatusCleansUpPartialManualDisableBeforeReportingFailure(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.AuditLog{}))
	channels := []model.Channel{
		{
			Name: "partial-disable-committed", Key: "partial-disable-key-1",
			Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
		},
		{
			Name: "partial-disable-failed", Key: "partial-disable-key-2",
			Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
		},
		{
			Name: "partial-disable-unprocessed", Key: "partial-disable-key-3",
			Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
		},
	}
	require.NoError(t, db.Create(&channels).Error)
	for index := range channels {
		require.NoError(t, channels[index].AddAbilities(db))
	}

	committedClosed := make(chan string, 1)
	unregisterCommitted := wsmanager.Register(channels[0].Id, "responses", func(reason string) {
		committedClosed <- reason
	})
	t.Cleanup(unregisterCommitted)
	failedClosed := make(chan string, 1)
	unregisterFailed := wsmanager.Register(channels[1].Id, "responses", func(reason string) {
		failedClosed <- reason
	})
	t.Cleanup(unregisterFailed)

	forcedErr := errors.New("forced second manual disable ability failure")
	var abilityUpdates atomic.Int32
	callbackName := "test:controller_partial_manual_disable_failure"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil &&
			tx.Statement.Table == "abilities" &&
			abilityUpdates.Add(1) == 2 {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Update().Remove(callbackName))
	})

	body := []byte(fmt.Sprintf(
		`{"ids":[%d,%d,%d],"status":%d}`,
		channels[0].Id,
		channels[1].Id,
		channels[2].Id,
		common.ChannelStatusManuallyDisabled,
	))
	committedClosedBeforeResponse := false
	recorder := &observingResponseRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}
	recorder.beforeFirstWrite = func() {
		select {
		case reason := <-committedClosed:
			committedClosedBeforeResponse = true
			assert.Equal(t, service.ChannelDisabledCloseReason, reason)
		default:
			assert.Fail(t, "committed disable left websocket open before response")
		}
		var failureAudits int64
		assert.NoError(t, model.LOG_DB.Model(&model.AuditLog{}).
			Where("action = ? AND success = ?", "channel.status_update_batch", false).
			Count(&failureAudits).Error)
		assert.Equal(t, int64(1), failureAudits)
	}
	context, _ := gin.CreateTestContext(recorder)
	context.Set("id", 901)
	context.Set("role", common.RoleRootUser)
	context.Set("username", "partial-disable-root")
	context.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/channel/status/batch",
		bytes.NewReader(body),
	)
	context.Request.Header.Set("Content-Type", "application/json")

	BatchUpdateChannelStatus(context)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
	assert.Contains(t, response.Message, forcedErr.Error())
	assert.Equal(t, int32(2), abilityUpdates.Load())

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 3)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored[0].Status)
	assert.Equal(t, common.ChannelStatusEnabled, stored[1].Status)
	assert.Equal(t, common.ChannelStatusEnabled, stored[2].Status)
	assert.True(t, committedClosedBeforeResponse)
	select {
	case reason := <-failedClosed:
		t.Fatalf("failed disable closed websocket: %s", reason)
	default:
	}

	var audit model.AuditLog
	require.NoError(t, model.LOG_DB.
		Where("action = ?", "channel.status_update_batch").
		Last(&audit).Error)
	assert.False(t, audit.Success)
	require.NotNil(t, audit.Other.AuditInfo)
	assert.False(t, audit.Other.AuditInfo.Success)
	require.NotNil(t, audit.Other.Op)
	paramsJSON, err := common.Marshal(audit.Other.Op.Params)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(
		`{"changed_ids":[%d],"count":1,"failed_id":%d,"status":%d,"total":3}`,
		channels[0].Id,
		channels[1].Id,
		common.ChannelStatusManuallyDisabled,
	), string(paramsJSON))
}

func TestBatchUpdateChannelStatusAuditsPartialManualEnableBeforeReportingFailure(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.AuditLog{}))
	channels := []model.Channel{
		{
			Name: "partial-enable-committed", Key: "partial-enable-key-1",
			Status: common.ChannelStatusManuallyDisabled, Models: "gpt-4.1", Group: "default",
		},
		{
			Name: "partial-enable-failed", Key: "partial-enable-key-2",
			Status: common.ChannelStatusManuallyDisabled, Models: "gpt-4.1", Group: "default",
		},
		{
			Name: "partial-enable-unprocessed", Key: "partial-enable-key-3",
			Status: common.ChannelStatusManuallyDisabled, Models: "gpt-4.1", Group: "default",
		},
	}
	require.NoError(t, db.Create(&channels).Error)
	for index := range channels {
		require.NoError(t, channels[index].AddAbilities(db))
	}

	forcedErr := errors.New("forced second manual enable ability failure")
	var abilityUpdates atomic.Int32
	callbackName := "test:controller_partial_manual_enable_failure"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil &&
			tx.Statement.Table == "abilities" &&
			abilityUpdates.Add(1) == 2 {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Update().Remove(callbackName))
	})

	body := []byte(fmt.Sprintf(
		`{"ids":[%d,%d,%d],"status":%d}`,
		channels[0].Id,
		channels[1].Id,
		channels[2].Id,
		common.ChannelStatusEnabled,
	))
	recorder := &observingResponseRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}
	recorder.beforeFirstWrite = func() {
		var failureAudits int64
		assert.NoError(t, model.LOG_DB.Model(&model.AuditLog{}).
			Where("action = ? AND success = ?", "channel.status_update_batch", false).
			Count(&failureAudits).Error)
		assert.Equal(t, int64(1), failureAudits)
	}
	context, _ := gin.CreateTestContext(recorder)
	context.Set("id", 902)
	context.Set("role", common.RoleRootUser)
	context.Set("username", "partial-enable-root")
	context.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/channel/status/batch",
		bytes.NewReader(body),
	)
	context.Request.Header.Set("Content-Type", "application/json")

	BatchUpdateChannelStatus(context)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
	assert.Contains(t, response.Message, forcedErr.Error())
	assert.Equal(t, int32(2), abilityUpdates.Load())

	var stored []model.Channel
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 3)
	assert.Equal(t, common.ChannelStatusEnabled, stored[0].Status)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored[1].Status)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored[2].Status)

	var audit model.AuditLog
	require.NoError(t, model.LOG_DB.
		Where("action = ?", "channel.status_update_batch").
		Last(&audit).Error)
	assert.False(t, audit.Success)
	require.NotNil(t, audit.Other.AuditInfo)
	assert.False(t, audit.Other.AuditInfo.Success)
	require.NotNil(t, audit.Other.Op)
	paramsJSON, err := common.Marshal(audit.Other.Op.Params)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(
		`{"changed_ids":[%d],"count":1,"failed_id":%d,"status":%d,"total":3}`,
		channels[0].Id,
		channels[1].Id,
		common.ChannelStatusEnabled,
	), string(paramsJSON))
}

func createControllerOwnerlessPlanQuotaDomain(
	t *testing.T,
	db *gorm.DB,
	name string,
	status int,
) ([]model.Channel, string) {
	t.Helper()

	tag := "plan:test:controller-ownerless-" + name
	credential := "controller-ownerless-credential-" + name
	hash, ok := model.PlanQuotaDomainHash(credential)
	require.True(t, ok)
	require.NoError(t, db.Create(&model.PlanQuotaDomain{
		CredentialHash: hash,
		Generation:     171,
		State:          model.PlanQuotaDomainStateDisabled,
		DisabledUntil:  2_000_000_000,
	}).Error)
	channels := []model.Channel{
		{
			Name: "ownerless-selected", Key: credential, Tag: &tag,
			Status: status, Models: "gpt-4.1", Group: "default",
		},
		{
			Name: "ownerless-unselected", Key: credential, Tag: &tag,
			Status: status, Models: "gpt-4.1", Group: "default",
		},
	}
	for index := range channels {
		channels[index].SetOtherInfo(map[string]any{
			"status_reason": fmt.Sprintf("independent owner %d", index),
			"failure_owner": fmt.Sprintf("owner-%d", index),
		})
	}
	require.NoError(t, db.Create(&channels).Error)
	for index := range channels {
		require.NoError(t, channels[index].AddAbilities(db))
	}
	return channels, hash
}

func assertControllerOwnerlessRecovery(
	t *testing.T,
	db *gorm.DB,
	channels []model.Channel,
	hash string,
) {
	t.Helper()

	var authority model.PlanQuotaDomain
	require.NoError(t, db.First(&authority, "credential_hash = ?", hash).Error)
	assert.Equal(t, model.PlanQuotaDomainStateActive, authority.State)

	var selected model.Channel
	require.NoError(t, db.First(&selected, channels[0].Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, selected.Status)
	assert.Equal(t, "owner-0", selected.GetOtherInfo()["failure_owner"])
	var selectedAbility model.Ability
	require.NoError(t, db.First(&selectedAbility, "channel_id = ?", selected.Id).Error)
	assert.True(t, selectedAbility.Enabled)

	var unselected model.Channel
	require.NoError(t, db.First(&unselected, channels[1].Id).Error)
	assert.Equal(t, channels[1].Status, unselected.Status)
	assert.Equal(t, channels[1].OtherInfo, unselected.OtherInfo)
	var unselectedAbility model.Ability
	require.NoError(t, db.First(&unselectedAbility, "channel_id = ?", unselected.Id).Error)
	assert.False(t, unselectedAbility.Enabled)
}

func TestUpdateChannelStatusManuallyRecoversOwnerlessDisabledAuthority(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	channels, hash := createControllerOwnerlessPlanQuotaDomain(
		t,
		db,
		"single",
		common.ChannelStatusManuallyDisabled,
	)

	body := []byte(fmt.Sprintf(`{"status":%d}`, common.ChannelStatusEnabled))
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "id", Value: strconv.Itoa(channels[0].Id)}}
	context.Request = httptest.NewRequest(http.MethodPost, "/api/channel/status", bytes.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")

	UpdateChannelStatus(context)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool `json:"success"`
		Data    bool `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.True(t, response.Data)
	assertControllerOwnerlessRecovery(t, db, channels, hash)
}

func TestBatchUpdateChannelStatusManuallyRecoversOwnerlessDisabledAuthority(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	channels, hash := createControllerOwnerlessPlanQuotaDomain(
		t,
		db,
		"batch",
		common.ChannelStatusAutoDisabled,
	)

	body := []byte(fmt.Sprintf(
		`{"ids":[%d],"status":%d}`,
		channels[0].Id,
		common.ChannelStatusEnabled,
	))
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/channel/status/batch", bytes.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")

	BatchUpdateChannelStatus(context)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool `json:"success"`
		Data    int  `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, 1, response.Data)
	assertControllerOwnerlessRecovery(t, db, channels, hash)
}
