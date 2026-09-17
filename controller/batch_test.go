package controller

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestParseBatchJSONLValidatesEndpointAndCustomIDs(t *testing.T) {
	input := strings.Join([]string{
		`{"custom_id":"one","method":"POST","url":"/v1/responses","body":{"model":"gpt-test","input":"hello"}}`,
		`{"custom_id":"two","method":"POST","url":"/v1/responses","body":{"model":"gpt-test","input":"world","stream":false}}`,
	}, "\n")
	items, err := parseBatchJSONL(strings.NewReader(input), "/v1/responses")
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, 1, items[0].LineNumber)
	assert.Equal(t, "two", items[1].CustomID)

	_, err = parseBatchJSONL(strings.NewReader(
		`{"custom_id":"one","method":"GET","url":"/v1/responses","body":{"model":"gpt-test"}}`,
	), "/v1/responses")
	assert.ErrorContains(t, err, "method must be POST")

	_, err = parseBatchJSONL(strings.NewReader(strings.Join([]string{
		`{"custom_id":"same","method":"POST","url":"/v1/responses","body":{"model":"gpt-test"}}`,
		`{"custom_id":"same","method":"POST","url":"/v1/responses","body":{"model":"gpt-test"}}`,
	}, "\n")), "/v1/responses")
	assert.ErrorContains(t, err, "duplicate custom_id")

	_, err = parseBatchJSONL(strings.NewReader(
		`{"custom_id":"one","method":"POST","url":"/v1/batches","body":{"model":"gpt-test"}}`,
	), "/v1/responses")
	assert.ErrorContains(t, err, "must match batch endpoint")
}

func TestCreateBatchIsTokenScopedAndIdempotent(t *testing.T) {
	previousDB := model.DB
	previousStorageDir := constant.BatchStorageDir
	previousMaxLines := constant.BatchMaxLines
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(
		&model.APIFile{},
		&model.Batch{},
		&model.BatchItem{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
	))
	model.DB = database
	constant.BatchStorageDir = t.TempDir()
	constant.BatchMaxLines = 100
	t.Cleanup(func() {
		model.DB = previousDB
		constant.BatchStorageDir = previousStorageDir
		constant.BatchMaxLines = previousMaxLines
	})

	input := `{"custom_id":"one","method":"POST","url":"/v1/responses","body":{"model":"gpt-test","input":"hello"}}` + "\n"
	file, err := service.CreateBatchAPIFile(
		context.Background(),
		7,
		11,
		model.APIFilePurposeBatch,
		"input.jsonl",
		strings.NewReader(input),
	)
	require.NoError(t, err)

	callCreate := func(metadata string) *httptest.ResponseRecorder {
		body := `{"input_file_id":"` + file.FileID + `","endpoint":"/v1/responses","completion_window":"24h","metadata":` + metadata + `}`
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/batches", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("Idempotency-Key", "same-request")
		common.SetContextKey(c, constant.ContextKeyUserId, 7)
		common.SetContextKey(c, constant.ContextKeyTokenId, 11)
		CreateBatch(c)
		return recorder
	}

	first := callCreate(`{"team":"media"}`)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var firstBatch model.Batch
	require.NoError(t, common.Unmarshal(first.Body.Bytes(), &firstBatch))
	assert.Equal(t, 1, firstBatch.RequestCounts.Total)

	second := callCreate(`{"team":"media"}`)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	var secondBatch model.Batch
	require.NoError(t, common.Unmarshal(second.Body.Bytes(), &secondBatch))
	assert.Equal(t, firstBatch.BatchID, secondBatch.BatchID)

	conflict := callCreate(`{"team":"other"}`)
	assert.Equal(t, http.StatusConflict, conflict.Code)

	_, exists, err := model.GetOwnedBatch(8, 11, firstBatch.BatchID)
	require.NoError(t, err)
	assert.False(t, exists)
}
