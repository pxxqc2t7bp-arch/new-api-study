package controller

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAPIFileUploadDownloadOwnershipAndActiveBatchGuard(t *testing.T) {
	previousDB := model.DB
	previousStorageDir := constant.BatchStorageDir
	previousMaxFileMB := constant.BatchMaxFileMB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.APIFile{}, &model.Batch{}))
	model.DB = database
	constant.BatchStorageDir = t.TempDir()
	constant.BatchMaxFileMB = 1
	t.Cleanup(func() {
		model.DB = previousDB
		constant.BatchStorageDir = previousStorageDir
		constant.BatchMaxFileMB = previousMaxFileMB
	})

	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	require.NoError(t, writer.WriteField("purpose", model.APIFilePurposeBatch))
	part, err := writer.CreateFormFile("file", "requests.jsonl")
	require.NoError(t, err)
	content := []byte(`{"custom_id":"one"}` + "\n")
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	createRecorder := httptest.NewRecorder()
	createContext, _ := gin.CreateTestContext(createRecorder)
	createContext.Request = httptest.NewRequest(http.MethodPost, "/v1/files", &upload)
	createContext.Request.Header.Set("Content-Type", writer.FormDataContentType())
	common.SetContextKey(createContext, constant.ContextKeyUserId, 7)
	common.SetContextKey(createContext, constant.ContextKeyTokenId, 11)
	CreateAPIFile(createContext)
	require.Equal(t, http.StatusOK, createRecorder.Code, createRecorder.Body.String())
	var file model.APIFile
	require.NoError(t, common.Unmarshal(createRecorder.Body.Bytes(), &file))
	assert.Equal(t, int64(len(content)), file.Bytes)
	assert.Equal(t, model.APIFilePurposeBatch, file.Purpose)

	downloadRecorder := httptest.NewRecorder()
	downloadContext, _ := gin.CreateTestContext(downloadRecorder)
	downloadContext.Request = httptest.NewRequest(http.MethodGet, "/v1/files/"+file.FileID+"/content", nil)
	downloadContext.Params = gin.Params{{Key: "id", Value: file.FileID}}
	common.SetContextKey(downloadContext, constant.ContextKeyUserId, 7)
	common.SetContextKey(downloadContext, constant.ContextKeyTokenId, 11)
	DownloadAPIFile(downloadContext)
	assert.Equal(t, http.StatusOK, downloadRecorder.Code)
	assert.Equal(t, content, downloadRecorder.Body.Bytes())

	foreignRecorder := httptest.NewRecorder()
	foreignContext, _ := gin.CreateTestContext(foreignRecorder)
	foreignContext.Request = httptest.NewRequest(http.MethodGet, "/v1/files/"+file.FileID, nil)
	foreignContext.Params = gin.Params{{Key: "id", Value: file.FileID}}
	common.SetContextKey(foreignContext, constant.ContextKeyUserId, 7)
	common.SetContextKey(foreignContext, constant.ContextKeyTokenId, 12)
	RetrieveAPIFile(foreignContext)
	assert.Equal(t, http.StatusNotFound, foreignRecorder.Code)

	require.NoError(t, database.Create(&model.Batch{
		BatchID:     "batch-active",
		UserID:      7,
		TokenID:     11,
		InputFileID: file.FileID,
		Endpoint:    "/v1/responses",
		Status:      model.BatchStatusInProgress,
	}).Error)
	deleteRecorder := httptest.NewRecorder()
	deleteContext, _ := gin.CreateTestContext(deleteRecorder)
	deleteContext.Request = httptest.NewRequest(http.MethodDelete, "/v1/files/"+file.FileID, nil)
	deleteContext.Params = gin.Params{{Key: "id", Value: file.FileID}}
	common.SetContextKey(deleteContext, constant.ContextKeyUserId, 7)
	common.SetContextKey(deleteContext, constant.ContextKeyTokenId, 11)
	DeleteAPIFile(deleteContext)
	assert.Equal(t, http.StatusConflict, deleteRecorder.Code)
}
