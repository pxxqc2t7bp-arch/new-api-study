package controller

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Only bounded control JSON uses the App error envelope. Artifact streams use
// their dedicated data-plane handlers.
func appExecutionControl[T, R any](c *gin.Context, decode func([]byte, *T) error,
	invoke func(*service.AppExecutionService, model.AppServiceIdentity, T) (R, error)) {
	appPluginSensitiveResponse(c)
	identity, ok := middleware.GetAppServiceIdentity(c)
	if !ok {
		writeAppPluginError(c, http.StatusUnauthorized, "service_identity_invalid", "")
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			writeAppPluginError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "")
		} else {
			writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		}
		return
	}
	var request T
	if err := decode(raw, &request); err != nil {
		writeAppExecutionError(c, err)
		return
	}
	result, err := invoke(service.NewAppExecutionService(model.DB, service.ConfiguredAppPluginAuthOptions()), identity, request)
	if err != nil {
		writeAppExecutionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func IssueAppExecutionGrant(c *gin.Context) {
	if !appExecutionWriteEnabled(c) {
		return
	}
	appExecutionControl(c, service.DecodeAppExecutionGrantRequest,
		func(s *service.AppExecutionService, identity model.AppServiceIdentity, request service.AppExecutionGrantRequest) (service.AppExecutionGrantResult, error) {
			return s.IssueExecutionGrant(c.Request.Context(), identity, request)
		})
}

func LookupAppTask(c *gin.Context) {
	appExecutionControl(c, service.DecodeAppTaskLookupRequest,
		func(s *service.AppExecutionService, identity model.AppServiceIdentity, request service.AppTaskLookupRequest) (service.AppTaskLookupResult, error) {
			return s.LookupScopedTask(c.Request.Context(), identity, request)
		})
}

func CancelAppTask(c *gin.Context) {
	if !appExecutionWriteEnabled(c) {
		return
	}
	if len(c.Request.Header.Values("Idempotency-Key")) != 1 {
		appPluginSensitiveResponse(c)
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "")
		return
	}
	appExecutionControl(c, service.DecodeAppTaskCancelRequest,
		func(s *service.AppExecutionService, identity model.AppServiceIdentity, request service.AppTaskCancelRequest) (service.AppTaskCancelResult, error) {
			return s.CancelTask(c.Request.Context(), identity, c.GetHeader("Idempotency-Key"), request)
		})
}

func appExecutionWriteEnabled(c *gin.Context) bool {
	appPluginSensitiveResponse(c)
	identity, ok := middleware.GetAppServiceIdentity(c)
	if !ok {
		writeAppPluginError(c, http.StatusUnauthorized, "service_identity_invalid", "")
		return false
	}
	if !operation_setting.AppExecutionGrantsEnabled || !appPluginEntryAllowed(identity.AppKey, "") {
		writeAppPluginError(c, http.StatusForbidden, "app_plugin_disabled", "")
		return false
	}
	return true
}

func LookupAppArkImport(c *gin.Context) {
	appExecutionControl(c, service.DecodeAppArkImportLookupRequest,
		func(s *service.AppExecutionService, identity model.AppServiceIdentity, request service.AppArkImportLookupRequest) (service.AppArkImportLookupResult, error) {
			return s.LookupArkImport(c.Request.Context(), identity, request)
		})
}

func writeAppExecutionError(c *gin.Context, err error) {
	var auth *service.AppPluginAuthError
	if errors.As(err, &auth) {
		switch auth.Code {
		case "model_policy_version_conflict":
			writeAppPluginError(c, http.StatusConflict, "version_conflict", "")
		case "model_not_supported":
			writeAppPluginError(c, http.StatusForbidden, "model_policy_denied", "")
		case "pricing_not_supported":
			writeAppPluginError(c, http.StatusServiceUnavailable, "price_version_unavailable", "")
		default:
			writeAppPluginError(c, appPluginAuthStatus(auth.Code), auth.Code, "")
		}
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeAppPluginError(c, http.StatusNotFound, "not_found", "")
		return
	}
	writeAppPluginError(c, http.StatusServiceUnavailable, "service_unavailable", "")
}

func publishAppExecutionPolicyOption(c *gin.Context, option OptionUpdateRequest) {
	appPluginSensitiveResponse(c)
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok {
		writeAppPluginError(c, http.StatusUnauthorized, "unauthenticated", "")
		return
	}
	raw, ok := option.Value.(string)
	if !ok {
		writeAppPluginError(c, http.StatusBadRequest, "invalid_request", "/value")
		return
	}
	publish := service.PublishAppModelInvokePolicy
	if option.Key == model.AppArkImportDelegationsKey {
		publish = service.PublishAppArkImportDelegations
	}
	version, err := publish(c.Request.Context(), model.DB, identity, []byte(raw), time.Now())
	if err != nil {
		// Invalid input is distinct from corrupt stored policy on read paths.
		if err.Error() == "invalid_policy" {
			writeAppPluginError(c, http.StatusUnprocessableEntity, "validation_error", "/value")
		} else if err.Error() == "immutable_rule" {
			writeAppPluginError(c, http.StatusConflict, "version_conflict", "/value")
		} else {
			writeAppExecutionError(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"version": version}})
}
