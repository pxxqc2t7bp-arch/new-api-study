package middleware

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupRealtimeTicketMiddlewareTest(t *testing.T) (*model.User, *model.Token) {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousType := common.MainDatabaseType()
	previousRedis := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:%s?mode=memory&cache=shared",
		url.QueryEscape(t.Name()),
	)), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.AuthFlow{}))
	model.DB, model.LOG_DB = db, db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	t.Cleanup(func() {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetMainDatabaseType(previousType)
		common.RedisEnabled = previousRedis
	})

	user := &model.User{
		Username: "realtime-ticket-user",
		Password: "password-placeholder",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    1000,
		AffCode:  "realtime-ticket-aff",
	}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{
		UserId:                   user.Id,
		Key:                      "realtimeticketapikey",
		Name:                     "realtime-ticket-token",
		Status:                   common.TokenStatusEnabled,
		ExpiredTime:              -1,
		RemainQuota:              100,
		ModelLimitsEnabled:       true,
		ModelLimits:              "gpt-realtime",
		DefaultRoutingStrategy:   "stable",
		AllowedRoutingStrategies: `["stable","latency"]`,
	}
	require.NoError(t, token.Insert())
	return user, token
}

func realtimeAuthTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	router := gin.New()
	router.GET("/v1/realtime", RealtimeAuth(), func(c *gin.Context) {
		routing, _ := common.GetContextKeyType[hosttypes.RoutingStrategy](
			c,
			constant.ContextKeyRoutingStrategy,
		)
		c.JSON(http.StatusOK, gin.H{
			"user_id":          c.GetInt("id"),
			"token_id":         c.GetInt("token_id"),
			"routing_strategy": routing,
			"url":              c.Request.URL.String(),
			"ticket_header":    c.GetHeader(RealtimeTicketHeader),
			"subprotocol":      c.GetHeader("Sec-WebSocket-Protocol"),
		})
	})
	return router
}

func createRealtimeTicketForMiddlewareTest(
	t *testing.T,
	user *model.User,
	token *model.Token,
) string {
	t.Helper()
	raw, _, err := model.CreateRealtimeTicket(model.RealtimeTicketCreate{
		UserId:          user.Id,
		TokenId:         token.Id,
		Model:           "gpt-realtime",
		RoutingStrategy: hosttypes.RoutingStrategyLatency,
	})
	require.NoError(t, err)
	return raw
}

func TestRealtimeAuthConsumesAndRemovesTicketCredentials(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*http.Request, string)
	}{
		{
			name: "query",
			configure: func(request *http.Request, ticket string) {
				query := request.URL.Query()
				query.Set(RealtimeTicketQuery, ticket)
				request.URL.RawQuery = query.Encode()
			},
		},
		{
			name: "header",
			configure: func(request *http.Request, ticket string) {
				request.Header.Set(RealtimeTicketHeader, ticket)
			},
		},
		{
			name: "subprotocol",
			configure: func(request *http.Request, ticket string) {
				request.Header.Set(
					"Sec-WebSocket-Protocol",
					"realtime, "+RealtimeTicketSubprotocolPrefix+ticket,
				)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user, token := setupRealtimeTicketMiddlewareTest(t)
			router := realtimeAuthTestRouter(t)
			ticket := createRealtimeTicketForMiddlewareTest(t, user, token)
			request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
			test.configure(request, ticket)
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			require.Equal(t, http.StatusOK, response.Code)
			assert.NotContains(t, response.Body.String(), ticket)
			assert.Contains(t, response.Body.String(), `"user_id":1`)
			assert.Contains(t, response.Body.String(), `"token_id":1`)
			assert.Contains(t, response.Body.String(), `"routing_strategy":"latency"`)
			assert.NotContains(t, response.Body.String(), RealtimeTicketQuery+"=")
			assert.NotContains(t, response.Body.String(), RealtimeTicketSubprotocolPrefix)
		})
	}
}

func TestRealtimeAuthRejectsReuseAndModelMismatch(t *testing.T) {
	user, token := setupRealtimeTicketMiddlewareTest(t)
	router := realtimeAuthTestRouter(t)
	ticket := createRealtimeTicketForMiddlewareTest(t, user, token)

	wrong := httptest.NewRequest(
		http.MethodGet,
		"/v1/realtime?model=other-model&ticket="+url.QueryEscape(ticket),
		nil,
	)
	wrongResponse := httptest.NewRecorder()
	router.ServeHTTP(wrongResponse, wrong)
	assert.Equal(t, http.StatusUnauthorized, wrongResponse.Code)

	validPath := "/v1/realtime?model=gpt-realtime&ticket=" + url.QueryEscape(ticket)
	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, validPath, nil))
	require.Equal(t, http.StatusOK, first.Code)

	reused := httptest.NewRecorder()
	router.ServeHTTP(reused, httptest.NewRequest(http.MethodGet, validPath, nil))
	assert.Equal(t, http.StatusUnauthorized, reused.Code)
}

func TestRealtimeAuthRevalidatesTokenAndRoutingPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *model.Token)
	}{
		{
			name: "token disabled",
			mutate: func(t *testing.T, token *model.Token) {
				require.NoError(t, model.DB.Model(token).Update("status", common.TokenStatusDisabled).Error)
			},
		},
		{
			name: "strategy revoked",
			mutate: func(t *testing.T, token *model.Token) {
				require.NoError(t, model.DB.Model(token).Updates(map[string]any{
					"default_routing_strategy":   "stable",
					"allowed_routing_strategies": `["stable"]`,
				}).Error)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user, token := setupRealtimeTicketMiddlewareTest(t)
			router := realtimeAuthTestRouter(t)
			ticket := createRealtimeTicketForMiddlewareTest(t, user, token)
			test.mutate(t, token)

			request := httptest.NewRequest(
				http.MethodGet,
				"/v1/realtime?model=gpt-realtime&ticket="+url.QueryEscape(ticket),
				nil,
			)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assert.Equal(t, http.StatusUnauthorized, response.Code)
		})
	}
}

func TestRealtimeAuthDoesNotConsumeTicketOnTransientTokenLookupFailure(t *testing.T) {
	user, token := setupRealtimeTicketMiddlewareTest(t)
	router := realtimeAuthTestRouter(t)
	ticket := createRealtimeTicketForMiddlewareTest(t, user, token)

	transientErr := fmt.Errorf("temporary token database failure")
	var intercepted atomic.Bool
	const callbackName = "test:fail_realtime_ticket_token_lookup"
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" && intercepted.CompareAndSwap(false, true) {
			tx.AddError(transientErr)
		}
	}))
	failed := httptest.NewRecorder()
	requestPath := "/v1/realtime?model=gpt-realtime&ticket=" + url.QueryEscape(ticket)
	router.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, requestPath, nil))
	require.Equal(t, http.StatusInternalServerError, failed.Code)
	require.NoError(t, model.DB.Callback().Query().Remove(callbackName))

	retried := httptest.NewRecorder()
	router.ServeHTTP(retried, httptest.NewRequest(http.MethodGet, requestPath, nil))
	assert.Equal(t, http.StatusOK, retried.Code)
}

func TestRealtimeAuthPreservesNativeSDKAuthenticationAndScrubsSubprotocolKey(t *testing.T) {
	_, token := setupRealtimeTicketMiddlewareTest(t)
	router := realtimeAuthTestRouter(t)

	bearer := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	bearer.Header.Set("Authorization", "Bearer "+token.Key)
	bearerResponse := httptest.NewRecorder()
	router.ServeHTTP(bearerResponse, bearer)
	require.Equal(t, http.StatusOK, bearerResponse.Code)

	subprotocol := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	subprotocol.Header.Set(
		"Sec-WebSocket-Protocol",
		"realtime, openai-insecure-api-key."+token.Key,
	)
	subprotocolResponse := httptest.NewRecorder()
	router.ServeHTTP(subprotocolResponse, subprotocol)
	require.Equal(t, http.StatusOK, subprotocolResponse.Code)
	assert.NotContains(t, subprotocolResponse.Body.String(), token.Key)
	assert.Contains(t, subprotocolResponse.Body.String(), `"subprotocol":"realtime"`)
}

func TestRealtimeAuthRejectsAmbiguousTicketSourcesWithoutConsuming(t *testing.T) {
	user, token := setupRealtimeTicketMiddlewareTest(t)
	router := realtimeAuthTestRouter(t)
	ticket := createRealtimeTicketForMiddlewareTest(t, user, token)
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/realtime?model=gpt-realtime&ticket="+url.QueryEscape(ticket),
		nil,
	)
	request.Header.Set(RealtimeTicketHeader, ticket)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)

	valid := httptest.NewRequest(
		http.MethodGet,
		"/v1/realtime?model=gpt-realtime&ticket="+url.QueryEscape(ticket),
		nil,
	)
	validResponse := httptest.NewRecorder()
	router.ServeHTTP(validResponse, valid)
	assert.Equal(t, http.StatusOK, validResponse.Code)
	assert.False(t, strings.Contains(validResponse.Body.String(), ticket))
}

func TestLoggerRedactsRealtimeTicketQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousWriter := gin.DefaultWriter
	var output bytes.Buffer
	gin.DefaultWriter = &output
	t.Cleanup(func() { gin.DefaultWriter = previousWriter })

	router := gin.New()
	SetUpLogger(router)
	router.GET("/v1/realtime", func(c *gin.Context) {
		c.Status(http.StatusUnauthorized)
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/realtime?model=gpt-realtime&ticket=never-log-this-ticket",
		nil,
	)
	router.ServeHTTP(httptest.NewRecorder(), request)

	assert.NotContains(t, output.String(), "never-log-this-ticket")
	assert.Contains(t, output.String(), "model=gpt-realtime")
}
