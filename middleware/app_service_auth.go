package middleware

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const appServiceIdentityKey = "app_plugin_service_identity"

func GetAppServiceIdentity(c *gin.Context) (model.AppServiceIdentity, bool) {
	value, exists := c.Get(appServiceIdentityKey)
	identity, ok := value.(model.AppServiceIdentity)
	return identity, exists && ok && identity.InstallationID != "" && identity.CredentialID != ""
}

func AppServiceAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		var required []string
		if c.Request.Method == http.MethodPost {
			switch c.Request.URL.Path {
			case "/internal/apps/v1/launch-codes/exchange", "/internal/apps/v1/sessions/introspect", "/internal/apps/v1/sessions/revoke":
				required = []string{"identity.read"}
			case "/internal/apps/v1/execution-grants":
				required = []string{"model.invoke"}
			case "/internal/apps/v1/tasks/lookup":
				required = []string{"task.read"}
			case "/internal/apps/v1/tasks/cancel":
				required = []string{"task.read", "model.invoke"}
			case "/internal/apps/v1/imports/ark-task-lookup":
				required = []string{"task.import"}
			}
		}
		if len(required) == 0 || c.FullPath() != c.Request.URL.Path {
			writeAppServiceAuthError(c, http.StatusForbidden, "forbidden")
			return
		}
		// A rollout stop must retain authenticated Task facts and allow session
		// invalidation. These exact routes still perform all current-authority
		// checks below and in their services.
		retained := c.Request.URL.Path == "/internal/apps/v1/tasks/lookup" ||
			c.Request.URL.Path == "/internal/apps/v1/sessions/introspect" ||
			c.Request.URL.Path == "/internal/apps/v1/sessions/revoke"
		if !model.AppPluginRolloutAllowsCached("", "") && !retained {
			writeAppServiceAuthError(c, http.StatusForbidden, "app_plugin_disabled")
			return
		}
		headers := c.Request.Header.Values("Authorization")
		if c.Request.TLS == nil || len(headers) != 1 {
			writeAppServiceAuthError(c, http.StatusUnauthorized, "service_identity_invalid")
			return
		}
		raw, ok := strings.CutPrefix(headers[0], "AppService ")
		if !ok || len(raw) != 43 {
			writeAppServiceAuthError(c, http.StatusUnauthorized, "service_identity_invalid")
			return
		}
		identity, err := model.AuthenticateAppServiceCredential(c.Request.Context(), model.DB, raw, time.Now())
		if err != nil {
			if errors.Is(err, model.ErrAppServiceIdentityInvalid) {
				writeAppServiceAuthError(c, http.StatusUnauthorized, "service_identity_invalid")
			} else {
				writeAppServiceAuthError(c, http.StatusServiceUnavailable, "service_unavailable")
			}
			return
		}
		for _, scope := range required {
			if !slices.Contains(identity.Scopes, scope) {
				writeAppServiceAuthError(c, http.StatusForbidden, "scope_denied")
				return
			}
		}
		c.Set(appServiceIdentityKey, identity)
		c.Next()
	}
}

func writeAppServiceAuthError(c *gin.Context, status int, code string) {
	requestID := c.GetString(common.RequestIdKey)
	if requestID == "" {
		requestID = common.NewRequestId()
		c.Set(common.RequestIdKey, requestID)
		c.Header(common.RequestIdKey, requestID)
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code": code, "message": "App service request denied", "field_errors": []any{},
		"retryable": status >= http.StatusInternalServerError, "request_id": requestID,
	}})
}
