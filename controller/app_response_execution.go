package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

type appResponseExecuteFunc func(
	context.Context, hosttypes.AppRelaySubject, []byte,
) (service.AppResponseExecutionResult, error)

func relayAppNativeResponse(c *gin.Context, subject *hosttypes.AppRelaySubject,
	execute appResponseExecuteFunc) {
	if subject == nil || subject.ExecutionKind != model.AppExecutionModelKindNativeResponse {
		writeAppNativeResponseError(c, http.StatusForbidden, "model_not_supported", false)
		return
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		writeAppNativeResponseError(c, http.StatusBadRequest, "invalid_request", false)
		return
	}
	raw, err := storage.Bytes()
	if err != nil || len(raw) == 0 || len(raw) > 64*1024 {
		writeAppNativeResponseError(c, http.StatusBadRequest, "invalid_request", false)
		return
	}
	execution, err := execute(c.Request.Context(), *subject, raw)
	if err != nil {
		status, code, retryable := http.StatusServiceUnavailable, "service_unavailable", true
		var authError *service.AppPluginAuthError
		if errors.As(err, &authError) {
			status, code = service.AppRelayErrorStatus(err), authError.Code
			retryable = status >= http.StatusInternalServerError
		}
		writeAppNativeResponseError(c, status, code, retryable)
		return
	}
	var headers map[string][]string
	if common.UnmarshalJsonStr(execution.Result.SafeHeadersJSON, &headers) != nil {
		writeAppNativeResponseError(c, http.StatusServiceUnavailable, "service_unavailable", true)
		return
	}
	for name, values := range headers {
		c.Writer.Header().Del(name)
		for _, value := range values {
			c.Writer.Header().Add(name, value)
		}
	}
	c.Header("Content-Length", strconv.Itoa(len(execution.Result.Body)))
	c.Status(execution.Result.HTTPStatus)
	_, _ = c.Writer.Write(execution.Result.Body)
}

func writeAppNativeResponseError(c *gin.Context, status int, code string, retryable bool) {
	requestID := c.GetString(common.RequestIdKey)
	if requestID == "" {
		requestID = common.NewRequestId()
		c.Set(common.RequestIdKey, requestID)
		c.Header(common.RequestIdKey, requestID)
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code": code, "message": "App response execution failed",
		"field_errors": []any{}, "retryable": retryable, "request_id": requestID,
	}})
}
