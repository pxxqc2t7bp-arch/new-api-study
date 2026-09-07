package controller

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func CreateAPIFile(c *gin.Context) {
	maxBytes := service.BatchMaxFileBytes()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+(1<<20))
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "invalid multipart file upload")
		return
	}
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	purpose := strings.TrimSpace(c.PostForm("purpose"))
	if purpose != model.APIFilePurposeBatch {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", `purpose must be "batch"`)
		return
	}
	header, err := c.FormFile("file")
	if err != nil {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "file is required")
		return
	}
	if header.Size <= 0 {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "file must not be empty")
		return
	}
	if header.Size > maxBytes {
		writeOpenAIResourceError(c, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds the configured size limit")
		return
	}
	source, err := header.Open()
	if err != nil {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", "failed to open uploaded file")
		return
	}
	defer source.Close()
	file, err := service.CreateBatchAPIFile(
		c.Request.Context(),
		common.GetContextKeyInt(c, constant.ContextKeyUserId),
		common.GetContextKeyInt(c, constant.ContextKeyTokenId),
		purpose,
		filepath.Base(header.Filename),
		source,
	)
	if err != nil {
		status := http.StatusInternalServerError
		code := "file_storage_error"
		if errors.Is(err, service.ErrBatchFileTooLarge) {
			status = http.StatusRequestEntityTooLarge
			code = "file_too_large"
		}
		writeOpenAIResourceError(c, status, code, "failed to store file")
		return
	}
	c.JSON(http.StatusOK, file)
}

func ListAPIFiles(c *gin.Context) {
	limit, err := parseResourceListLimit(c.Query("limit"))
	if err != nil {
		writeOpenAIResourceError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	files, hasMore, err := model.ListOwnedAPIFiles(
		common.GetContextKeyInt(c, constant.ContextKeyUserId),
		common.GetContextKeyInt(c, constant.ContextKeyTokenId),
		strings.TrimSpace(c.Query("purpose")),
		strings.TrimSpace(c.Query("after")),
		limit,
	)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to list files")
		return
	}
	firstID, lastID := "", ""
	if len(files) > 0 {
		firstID = files[0].FileID
		lastID = files[len(files)-1].FileID
	}
	c.JSON(http.StatusOK, gin.H{
		"object":   "list",
		"data":     files,
		"has_more": hasMore,
		"first_id": firstID,
		"last_id":  lastID,
	})
}

func RetrieveAPIFile(c *gin.Context) {
	file, exists, err := ownedAPIFile(c)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to retrieve file")
		return
	}
	if !exists {
		writeOpenAIResourceError(c, http.StatusNotFound, "not_found", "file not found")
		return
	}
	c.JSON(http.StatusOK, file)
}

func DeleteAPIFile(c *gin.Context) {
	userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	tokenID := common.GetContextKeyInt(c, constant.ContextKeyTokenId)
	file, exists, err := model.DeleteOwnedAPIFile(userID, tokenID, c.Param("id"))
	if err != nil {
		if strings.Contains(err.Error(), "active batch") {
			writeOpenAIResourceError(c, http.StatusConflict, "file_in_use", err.Error())
			return
		}
		writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to delete file")
		return
	}
	if !exists {
		writeOpenAIResourceError(c, http.StatusNotFound, "not_found", "file not found")
		return
	}
	if err = service.DeleteBatchFile(file.StorageKey); err != nil {
		common.SysError("delete batch file content failed: " + err.Error())
	}
	c.JSON(http.StatusOK, model.APIFileDeleted{ID: file.FileID, Object: "file", Deleted: true})
}

func DownloadAPIFile(c *gin.Context) {
	file, exists, err := ownedAPIFile(c)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "internal_error", "failed to retrieve file")
		return
	}
	if !exists {
		writeOpenAIResourceError(c, http.StatusNotFound, "not_found", "file not found")
		return
	}
	content, err := service.OpenBatchFile(file.StorageKey)
	if err != nil {
		writeOpenAIResourceError(c, http.StatusInternalServerError, "file_storage_error", "failed to open file")
		return
	}
	defer content.Close()
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": file.Filename})
	c.Header("Content-Type", "application/jsonl")
	c.Header("Content-Disposition", disposition)
	c.Header("Content-Length", strconv.FormatInt(file.Bytes, 10))
	c.Status(http.StatusOK)
	if c.Request.Method != http.MethodHead {
		_, _ = io.Copy(c.Writer, content)
	}
}

func ownedAPIFile(c *gin.Context) (*model.APIFile, bool, error) {
	return model.GetOwnedAPIFile(
		common.GetContextKeyInt(c, constant.ContextKeyUserId),
		common.GetContextKeyInt(c, constant.ContextKeyTokenId),
		c.Param("id"),
	)
}

func parseResourceListLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 20, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > 100 {
		return 0, fmt.Errorf("limit must be between 1 and 100")
	}
	return limit, nil
}

func writeOpenAIResourceError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{
		"message": message,
		"type":    code,
		"code":    code,
	}})
}
