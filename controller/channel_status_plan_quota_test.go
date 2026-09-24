package controller

import (
	"bytes"
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
