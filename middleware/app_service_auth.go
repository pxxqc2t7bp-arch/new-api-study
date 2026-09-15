package middleware

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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
		if !operation_setting.AppPluginV1Enabled {
			writeAppServiceAuthError(c, http.StatusForbidden, "app_plugin_disabled")
			return
		}
		allowed := false
		if c.Request.Method == http.MethodPost {
			switch c.Request.URL.Path {
			case "/internal/apps/v1/launch-codes/exchange", "/internal/apps/v1/sessions/introspect", "/internal/apps/v1/sessions/revoke":
				allowed = c.FullPath() == c.Request.URL.Path
			}
		}
		if !allowed {
			writeAppServiceAuthError(c, http.StatusForbidden, "forbidden")
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
			writeAppServiceAuthError(c, http.StatusUnauthorized, "service_identity_invalid")
			return
		}
		if !slices.Contains(identity.Scopes, "identity.read") {
			writeAppServiceAuthError(c, http.StatusForbidden, "scope_denied")
			return
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
		"retryable": false, "request_id": requestID,
	}})
}
