package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestExecuteBatchItemUsesExistingRelayAuthenticationAndRouting(t *testing.T) {
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousRedisEnabled := common.RedisEnabled
	previousBatchUpdate := common.BatchUpdateEnabled
	previousLogConsume := common.LogConsumeEnabled
	previousIsMasterNode := common.IsMasterNode
	previousSQLitePath := common.SQLitePath
	previousMainDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	previousSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")
	previousRatios := ratio_setting.ModelRatio2JSONString()
	previousGroupRatios := ratio_setting.GroupRatio2JSONString()
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.IsMasterNode = true
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	database := model.DB
	model.LOG_DB = database
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = false
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-batch-test":1}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	t.Cleanup(func() {
		if sqlDB, dbErr := database.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.MemoryCacheEnabled = previousMemoryCache
		common.RedisEnabled = previousRedisEnabled
		common.BatchUpdateEnabled = previousBatchUpdate
		common.LogConsumeEnabled = previousLogConsume
		common.IsMasterNode = previousIsMasterNode
		common.SQLitePath = previousSQLitePath
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", previousSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousGroupRatios))
	})
	service.InitHttpClient()

	user := &model.User{
		Username: "batch-relay-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1_000_000,
	}
	require.NoError(t, database.Create(user).Error)
	token := &model.Token{
		UserId:         user.Id,
		Key:            "batchrelaytoken",
		Name:           "batch-relay-token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, database.Create(token).Error)

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer upstream-key", r.Header.Get("Authorization"))
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)
		assert.Contains(t, string(body), `"model":"gpt-batch-test"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-batch",
			"object":"chat.completion",
			"created":1,
			"model":"gpt-batch-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer upstream.Close()
	channel := &model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Name:    "batch-upstream",
		Key:     "upstream-key",
		BaseURL: &upstream.URL,
		Status:  common.ChannelStatusEnabled,
		Models:  "gpt-batch-test",
		Group:   "default",
	}
	require.NoError(t, database.Create(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))

	batch := &model.Batch{
		BatchID:  "batch_test",
		UserID:   user.Id,
		TokenID:  token.Id,
		Endpoint: "/v1/chat/completions",
	}
	item := &model.BatchItem{
		CustomID: "custom-one",
		Method:   http.MethodPost,
		URL:      "/v1/chat/completions",
		Body:     json.RawMessage(`{"model":"gpt-batch-test","messages":[{"role":"user","content":"ping"}]}`),
	}

	response, itemError, success := executeBatchItem(context.Background(), batch, item)
	require.True(t, success, string(itemError))
	assert.Empty(t, itemError)
	assert.Contains(t, string(response), `"custom_id":"custom-one"`)
	assert.Contains(t, string(response), `"content":"OK"`)
	assert.Equal(t, 1, upstreamCalls)
}

func TestExecuteBatchItemRejectsUnavailableOriginalToken(t *testing.T) {
	previousDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.Token{}))
	model.DB = database
	t.Cleanup(func() { model.DB = previousDB })

	_, itemError, success := executeBatchItem(
		context.Background(),
		&model.Batch{UserID: 7, TokenID: 999},
		&model.BatchItem{CustomID: "missing-token", Method: http.MethodPost, URL: "/v1/responses"},
	)
	assert.False(t, success)
	assert.Contains(t, string(itemError), "authentication_error")
}

func TestRunClaimedBatchFinalizesOutputFile(t *testing.T) {
	database := setupBatchDispatchDatabase(t)
	batch, created, err := model.CreateBatchWithItems(&model.Batch{
		UserID:      7,
		TokenID:     11,
		InputFileID: "file-input",
		Endpoint:    "/v1/responses",
	}, []model.BatchItem{{
		LineNumber: 1,
		CustomID:   "one",
		Method:     http.MethodPost,
		URL:        "/v1/responses",
		Body:       json.RawMessage(`{"model":"gpt-test","input":"hello"}`),
	}})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, database.Model(&model.BatchItem{}).
		Where("batch_record_id = ?", batch.ID).
		Updates(map[string]any{
			"status":   model.BatchItemStatusCompleted,
			"response": json.RawMessage(`{"custom_id":"one","response":{"status_code":200}}`),
		}).Error)

	claimed, won, err := model.ClaimBatch(batch.ID, "runner", 100, 1000)
	require.NoError(t, err)
	require.True(t, won)
	status, err := runClaimedBatch(context.Background(), claimed, "runner")
	require.NoError(t, err)
	assert.Equal(t, model.BatchStatusCompleted, status)

	stored, exists, err := model.GetOwnedBatch(7, 11, batch.BatchID)
	require.NoError(t, err)
	require.True(t, exists)
	require.NotNil(t, stored.OutputFileID)
	output, exists, err := model.GetOwnedAPIFile(7, 11, *stored.OutputFileID)
	require.NoError(t, err)
	require.True(t, exists)
	content, err := service.OpenBatchFile(output.StorageKey)
	require.NoError(t, err)
	defer content.Close()
	body, err := io.ReadAll(content)
	require.NoError(t, err)
	assert.JSONEq(t, `{"custom_id":"one","response":{"status_code":200}}`, strings.TrimSpace(string(body)))
}

func TestRunClaimedBatchCancellationStopsPendingItems(t *testing.T) {
	setupBatchDispatchDatabase(t)
	batch, created, err := model.CreateBatchWithItems(&model.Batch{
		UserID:      7,
		TokenID:     11,
		InputFileID: "file-input",
		Endpoint:    "/v1/responses",
	}, []model.BatchItem{{
		LineNumber: 1,
		CustomID:   "one",
		Method:     http.MethodPost,
		URL:        "/v1/responses",
		Body:       json.RawMessage(`{"model":"gpt-test","input":"hello"}`),
	}})
	require.NoError(t, err)
	require.True(t, created)
	claimed, won, err := model.ClaimBatch(batch.ID, "runner", 100, 1000)
	require.NoError(t, err)
	require.True(t, won)
	_, exists, err := model.RequestBatchCancellation(7, 11, batch.BatchID, 110)
	require.NoError(t, err)
	require.True(t, exists)

	claimed.Status = model.BatchStatusCancelling
	status, err := runClaimedBatch(context.Background(), claimed, "runner")
	require.NoError(t, err)
	assert.Equal(t, model.BatchStatusCancelled, status)
	stored, exists, err := model.GetOwnedBatch(7, 11, batch.BatchID)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, model.BatchStatusCancelled, stored.Status)
	assert.Equal(t, 1, stored.RequestCounts.Failed)
	require.NotNil(t, stored.ErrorFileID)
}

func setupBatchDispatchDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousStorageDir := constant.BatchStorageDir
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.APIFile{}, &model.Batch{}, &model.BatchItem{}))
	model.DB = database
	constant.BatchStorageDir = t.TempDir()
	t.Cleanup(func() {
		model.DB = previousDB
		constant.BatchStorageDir = previousStorageDir
	})
	return database
}
