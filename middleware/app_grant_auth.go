package middleware

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	kittypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func GetAppGrantTaskRetrieval(c *gin.Context) (*service.AppTaskRetrieval, bool) {
	if c == nil {
		return nil, false
	}
	value, exists := c.Get(types.AppTaskRetrievalContextKey)
	retrieval, ok := value.(*service.AppTaskRetrieval)
	return retrieval, exists && ok && retrieval != nil
}

func IsAppGrantTaskRetrieval(c *gin.Context) bool {
	_, ok := GetAppGrantTaskRetrieval(c)
	return ok
}

func AppGrantOrTokenAuth() gin.HandlerFunc {
	tokenAuth := TokenAuth()
	return func(c *gin.Context) {
		headers := c.Request.Header.Values("Authorization")
		if len(headers) <= 1 && !strings.HasPrefix(c.GetHeader("Authorization"), "AppGrant ") &&
			!strings.HasPrefix(c.GetHeader("Authorization"), "AppService ") {
			tokenAuth(c)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		if c.Request.TLS == nil || len(headers) != 1 || !strings.HasPrefix(headers[0], "AppGrant ") {
			writeAppServiceAuthError(c, http.StatusUnauthorized, "invalid_grant")
			return
		}
		protocol := ""
		if c.Request.Method == http.MethodPost && c.FullPath() == c.Request.URL.Path && c.Request.URL.RawQuery == "" {
			switch c.Request.URL.Path {
			case "/v1/videos":
				protocol = "openai_video"
			case "/v1/responses":
				protocol = "openai_responses"
			}
		}
		if protocol == "" || c.ContentType() != "application/json" {
			writeAppServiceAuthError(c, http.StatusForbidden, "scope_denied")
			return
		}
		if c.GetHeader(HeaderRoutingStrategy) != "" || c.GetHeader(HeaderConversionPolicy) != "" {
			writeAppServiceAuthError(c, http.StatusForbidden, "scope_denied")
			return
		}
		// Bound the body before either the host decoder or plugin sees it.
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 64*1024+1))
		if err != nil || len(raw) > 64*1024 {
			writeAppServiceAuthError(c, http.StatusBadRequest, "invalid_request")
			return
		}
		subject, user, err := service.NewAppExecutionService(model.DB, service.ConfiguredAppPluginAuthOptions()).
			AuthenticateAppRelay(c.Request.Context(), strings.TrimPrefix(headers[0], "AppGrant "), protocol, raw)
		if err != nil {
			writeAppServiceAuthError(c, service.AppRelayErrorStatus(err), "app_execution_denied")
			return
		}
		c.Request.Body = io.NopCloser(strings.NewReader(string(raw)))
		c.Request.Header.Del("Authorization")
		user.ToBaseUser().WriteContext(c)
		common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
		common.SetContextKey(c, constant.ContextKeyRoutingStrategy, types.RoutingStrategy(subject.Strategy))
		common.SetContextKey(c, constant.ContextKeyConversionPolicy, kittypes.ConversionLossPolicy(subject.ConversionPolicy))
		c.Set(types.AppRelaySubjectContextKey, &subject)
		c.Next()
	}
}

func AppGrantOrTokenTaskRetrievalAuth(protocol, resourceParam string) gin.HandlerFunc {
	tokenAuth := TokenAuth()
	return func(c *gin.Context) {
		if !strings.HasPrefix(c.GetHeader("Authorization"), "AppGrant ") {
			tokenAuth(c)
			return
		}
		if !appGrantTaskRetrievalRouteAllowed(c, protocol) {
			writeAppServiceAuthError(c, http.StatusForbidden, "scope_denied")
			return
		}
		authenticateAppGrantTaskRetrieval(c, protocol, c.Param(resourceParam))
	}
}

func appGrantTaskRetrievalRouteAllowed(c *gin.Context, protocol string) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil || c.Request.URL.RawQuery != "" || c.Request.URL.ForceQuery ||
		c.Request.ContentLength > 0 || len(c.Request.TransferEncoding) != 0 ||
		c.GetBool(taskArtifactAccessPresentContextKey) || c.GetBool(taskArtifactAccessInvalidContextKey) ||
		c.GetHeader(HeaderRoutingStrategy) != "" || c.GetHeader(HeaderConversionPolicy) != "" {
		return false
	}
	switch protocol {
	case "":
		return c.Request.Method == http.MethodGet &&
			(c.FullPath() == "/v1/tasks/:key" || c.FullPath() == "/v1/tasks/:key/artifacts") ||
			(c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) &&
				c.FullPath() == "/v1/tasks/:key/artifacts/:artifact_key/content"
	case "openai_video":
		return (c.Request.Method == http.MethodGet && c.FullPath() == "/v1/videos/:task_id") ||
			((c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) &&
				c.FullPath() == "/v1/videos/:task_id/content")
	case "openai_responses":
		return c.Request.Method == http.MethodGet && c.FullPath() == "/v1/responses/:response_id"
	default:
		return false
	}
}

func authenticateAppGrantTaskRetrieval(c *gin.Context, protocol, resourceID string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	headers := c.Request.Header.Values("Authorization")
	if c.Request.TLS == nil || len(headers) != 1 {
		writeAppServiceAuthError(c, http.StatusUnauthorized, "invalid_grant")
		return
	}
	raw, ok := strings.CutPrefix(headers[0], "AppGrant ")
	if !ok {
		writeAppServiceAuthError(c, http.StatusUnauthorized, "invalid_grant")
		return
	}
	retrieval, user, err := service.NewAppExecutionService(
		model.DB, service.ConfiguredAppPluginAuthOptions(),
	).AuthenticateAppTaskRetrieval(c.Request.Context(), raw, protocol, resourceID)
	if err != nil {
		status := service.AppRelayErrorStatus(err)
		code := "app_execution_denied"
		var authErr *service.AppPluginAuthError
		if errors.As(err, &authErr) {
			code = authErr.Code
		}
		writeAppServiceAuthError(c, status, code)
		return
	}
	c.Request.Header.Del("Authorization")
	user.ToBaseUser().WriteContext(c)
	common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
	c.Set(types.AppTaskRetrievalContextKey, &retrieval)
	c.Next()
}

func distributeAppRelay(c *gin.Context, subject *types.AppRelaySubject) {
	value, exists := c.Get(jsplugin.ContextKeyPinnedEndpoint)
	pinned, ok := value.(jsplugin.PinnedEndpoint)
	switch subject.ExecutionKind {
	case model.AppExecutionModelKindTaskBacked:
		if !exists || !ok || pinned.Plugin == nil || pinned.Protocol != subject.Protocol ||
			c.GetString("resolved_task_model") != subject.PublicModel {
			writeAppServiceAuthError(c, http.StatusForbidden, "model_not_supported")
			return
		}
	case model.AppExecutionModelKindNativeResponse:
		if exists {
			writeAppServiceAuthError(c, http.StatusForbidden, "model_not_supported")
			return
		}
		common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
		c.Next()
		return
	default:
		writeAppServiceAuthError(c, http.StatusForbidden, "model_not_supported")
		return
	}
	constraints := service.GetChannelConstraints(c)
	constraints.AddFilter(taskdto.ChannelFilter{Kind: taskdto.FilterRequestPath, RequestPath: c.Request.URL.Path})
	if subject.ExecutionKind == model.AppExecutionModelKindTaskBacked {
		service.AppendTaskPluginIdentityFilter(c, pinned.Plugin.Meta.Key)
		if err := appendArkAssetAffinityFilter(c, constraints); err != nil {
			writeAppServiceAuthError(c, http.StatusForbidden, "scope_denied")
			return
		}
	}
	channel, selected, err := service.NewAppExecutionService(model.DB, service.ConfiguredAppPluginAuthOptions()).
		SelectAppRelayChannel(c.Request.Context(), *subject, constraints)
	if err != nil {
		writeAppServiceAuthError(c, service.AppRelayErrorStatus(err), "app_execution_denied")
		return
	}
	subject.ChannelID, subject.Group = selected.ChannelID, selected.Group
	subject.PluginSHA256 = selected.PluginSHA256
	common.SetContextKey(c, constant.ContextKeyUsingGroup, selected.Group)
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	if err := SetupContextForSelectedChannel(c, channel, subject.PublicModel); err != nil {
		writeAppServiceAuthError(c, http.StatusForbidden, "channel_unavailable")
		return
	}
	c.Next()
}
