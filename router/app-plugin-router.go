package router

import (
	"bytes"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func registerAppPluginRoutes(apiRouter *gin.RouterGroup) {
	apiRouter.GET("/app_plugins", normalizeAppPluginAuthErrors(), middleware.UserAuth(), controller.ListAppPlugins)
	apiRouter.POST("/app_plugins/:key/authorize", appPluginNoStore(), normalizeAppPluginAuthErrors(),
		middleware.CriticalRateLimit(), middleware.UserAuth(), middleware.UserCriticalRateLimit("app-plugin-authorize"), controller.AuthorizeAppPlugin)

	installations := apiRouter.Group("/app_plugins/installations")
	installations.Use(appPluginNoStore(), normalizeAppPluginAuthErrors(), middleware.UserAuth(), requireAppPluginManage())
	{
		installations.GET("", controller.ListAppPluginInstallations)
		installations.POST("", middleware.AppPluginOperationAudit(), controller.CreateAppPluginInstallation)
		installations.PATCH("", middleware.AppPluginOperationAudit(), controller.PatchAppPluginInstallation)
		installations.POST("/:id/service-credentials", middleware.CriticalRateLimit(),
			middleware.UserCriticalRateLimit("app-plugin-credential"), middleware.AppPluginOperationAudit(), controller.CreateAppPluginServiceCredential)
		installations.POST("/:id/service-credentials/rotate", middleware.CriticalRateLimit(),
			middleware.UserCriticalRateLimit("app-plugin-credential"), middleware.AppPluginOperationAudit(), controller.RotateAppPluginServiceCredential)
		installations.DELETE("/:id/service-credentials/:credential_id",
			middleware.AppPluginOperationAudit(), controller.RevokeAppPluginServiceCredential)
	}
	// Gin cleans the sibling path, retaining common API middleware without
	// placing the service-only endpoints under the dashboard /api namespace.
	internal := apiRouter.Group("../internal/apps/v1", appPluginNoStore(), normalizeAppPluginAuthErrors(),
		middleware.CriticalRateLimit(), middleware.AppServiceAuth())
	internal.POST("/launch-codes/exchange", controller.ExchangeAppPluginLaunchCode)
	internal.POST("/sessions/introspect", controller.IntrospectAppPluginSession)
	internal.POST("/sessions/revoke", controller.RevokeAppPluginSession)
}

func appPluginNoStore() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		c.Next()
	}
}

func appPluginAPIErrorBoundary() gin.HandlerFunc {
	normalize := normalizeAppPluginAuthErrors()
	return func(c *gin.Context) {
		// Match registered routes, not prefixes that could include other APIs.
		switch c.Request.Method + " " + c.FullPath() {
		case "GET /api/app_plugins",
			"GET /api/app_plugins/installations",
			"POST /api/app_plugins/installations",
			"PATCH /api/app_plugins/installations",
			"POST /api/app_plugins/:key/authorize",
			"POST /api/app_plugins/installations/:id/service-credentials",
			"POST /api/app_plugins/installations/:id/service-credentials/rotate",
			"DELETE /api/app_plugins/installations/:id/service-credentials/:credential_id",
			"POST /internal/apps/v1/launch-codes/exchange",
			"POST /internal/apps/v1/sessions/introspect",
			"POST /internal/apps/v1/sessions/revoke":
			normalize(c)
		default:
			c.Next()
		}
	}
}

type appPluginBufferedResponseWriter struct {
	gin.ResponseWriter
	header http.Header
	body   bytes.Buffer
	status int
	size   int
}

func normalizeAppPluginAuthErrors() gin.HandlerFunc {
	return func(c *gin.Context) {
		writer := &appPluginBufferedResponseWriter{
			ResponseWriter: c.Writer,
			header:         c.Writer.Header().Clone(),
			status:         http.StatusOK,
			size:           -1,
		}
		c.Writer = writer
		c.Next()
		c.Writer = writer.ResponseWriter

		var legacy struct {
			Success *bool  `json:"success"`
			Code    string `json:"code"`
		}
		_, authenticated := c.Get("id")
		rateLimited := writer.status == http.StatusTooManyRequests && writer.body.Len() == 0
		if !rateLimited && (authenticated || !c.IsAborted() || common.Unmarshal(writer.body.Bytes(), &legacy) != nil || legacy.Success == nil || *legacy.Success) {
			writer.commit()
			return
		}

		status := 0
		code := ""
		message := ""
		retryable := false
		switch {
		case rateLimited:
			status = http.StatusTooManyRequests
			code = "rate_limited"
			message = "Too many app plugin requests"
			retryable = true
			header := c.Writer.Header()
			clear(header)
			for key, values := range writer.header {
				header[key] = append([]string(nil), values...)
			}
			c.Header("Cache-Control", "no-store")
			c.Header("Referrer-Policy", "no-referrer")
		case legacy.Code == "AUTH_UNAUTHORIZED" || legacy.Code == "AUTH_TOKEN_EXPIRED" || legacy.Code == "AUTH_SESSION_REVOKED":
			status = http.StatusUnauthorized
			code = "unauthenticated"
			message = "Authentication required"
		case legacy.Code == "AUTH_USER_DISABLED" || legacy.Code == "AUTH_USER_INVALID":
			status = http.StatusForbidden
			code = "identity_inactive"
			message = "Identity is inactive"
		case legacy.Code == "AUTH_INSUFFICIENT_PRIVILEGE":
			status = http.StatusForbidden
			code = "forbidden"
			message = "App plugin request is forbidden"
		case legacy.Code == "AUTH_INTERNAL_ERROR":
			status = http.StatusServiceUnavailable
			code = "service_unavailable"
			message = "App plugin request failed"
			retryable = true
		default:
			writer.commit()
			return
		}

		requestID := c.GetString(common.RequestIdKey)
		if requestID == "" {
			requestID = common.NewRequestId()
			c.Set(common.RequestIdKey, requestID)
			c.Header(common.RequestIdKey, requestID)
		}
		c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
			"code":         code,
			"message":      message,
			"field_errors": []any{},
			"retryable":    retryable,
			"request_id":   requestID,
		}})
	}
}

func (w *appPluginBufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *appPluginBufferedResponseWriter) WriteHeader(statusCode int) {
	if !w.Written() {
		w.status = statusCode
	}
}

func (w *appPluginBufferedResponseWriter) WriteHeaderNow() {
	if !w.Written() {
		w.size = 0
	}
}

func (w *appPluginBufferedResponseWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	n, err := w.body.Write(data)
	w.size += n
	return n, err
}

func (w *appPluginBufferedResponseWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *appPluginBufferedResponseWriter) Status() int {
	return w.status
}

func (w *appPluginBufferedResponseWriter) Size() int {
	return w.size
}

func (w *appPluginBufferedResponseWriter) Written() bool {
	return w.size >= 0
}

func (w *appPluginBufferedResponseWriter) Flush() {
	w.WriteHeaderNow()
}

func (w *appPluginBufferedResponseWriter) commit() {
	header := w.ResponseWriter.Header()
	clear(header)
	for key, values := range w.header {
		header[key] = append([]string(nil), values...)
	}
	w.ResponseWriter.WriteHeader(w.status)
	if w.body.Len() != 0 {
		_, _ = w.ResponseWriter.Write(w.body.Bytes())
		return
	}
	if w.Written() {
		w.ResponseWriter.WriteHeaderNow()
	}
}

func requireAppPluginManage() gin.HandlerFunc {
	return func(c *gin.Context) {
		if authz.Can(c.GetInt("id"), c.GetInt("role"), authz.AppPluginManage) {
			c.Next()
			return
		}
		envelope := authz.NewAppPluginError(
			authz.AppPluginErrorCodePermissionDenied,
			c.GetString(common.RequestIdKey),
			nil,
		)
		c.AbortWithStatusJSON(envelope.StatusCode, envelope)
	}
}
