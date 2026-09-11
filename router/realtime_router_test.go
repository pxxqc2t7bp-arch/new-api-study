package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupRealtimeRouterTest(t *testing.T) (*model.User, *model.Token) {
	t.Helper()
	require.NoError(t, i18n.Init())
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.AuthFlow{}, &model.AuditLog{}))
	pat := "realtimeticketpat"
	user := &model.User{
		Username:    "realtime-router-user",
		Password:    "password-placeholder",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		Quota:       1000,
		AffCode:     "realtime-router-aff",
		AccessToken: &pat,
	}
	require.NoError(t, model.DB.Create(user).Error)
	token := &model.Token{
		UserId:                   user.Id,
		Key:                      "realtimerouterkey",
		Name:                     "realtime-router-token",
		Status:                   common.TokenStatusEnabled,
		ExpiredTime:              -1,
		RemainQuota:              100,
		DefaultRoutingStrategy:   "stable",
		AllowedRoutingStrategies: `["stable","latency"]`,
	}
	require.NoError(t, token.Insert())
	return user, token
}

func TestRealtimeTicketIssueRouteRequiresDashboardAuthentication(t *testing.T) {
	_, token := setupRealtimeRouterTest(t)
	engine := gin.New()
	SetApiRouter(engine)
	body := fmt.Sprintf(`{"token_id":%d,"model":"gpt-realtime"}`, token.Id)

	unauthorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/realtime/tickets", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(unauthorized, request)
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	authorized := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/realtime/tickets", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer realtimeticketpat")
	engine.ServeHTTP(authorized, request)
	require.Equal(t, http.StatusOK, authorized.Code)
	assert.Contains(t, authorized.Body.String(), model.RealtimeTicketPrefix)
	assert.NotContains(t, authorized.Body.String(), token.Key)
}

func TestRealtimeRelayRouteConsumesTicketBeforeDistribution(t *testing.T) {
	user, token := setupRealtimeRouterTest(t)
	raw, _, err := model.CreateRealtimeTicket(model.RealtimeTicketCreate{
		UserId:          user.Id,
		TokenId:         token.Id,
		Model:           "gpt-realtime",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)

	engine := gin.New()
	SetRelayRouter(engine)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/realtime?model=gpt-realtime&ticket="+raw,
		nil,
	)
	engine.ServeHTTP(response, request)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	_, err = model.ConsumeRealtimeTicket(raw, "gpt-realtime")
	assert.ErrorIs(t, err, model.ErrAuthFlowConsumed)
}

func TestRealtimeRelayRoutePreservesNativeBearerAuthentication(t *testing.T) {
	_, token := setupRealtimeRouterTest(t)
	engine := gin.New()
	SetRelayRouter(engine)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	request.Header.Set("Authorization", "Bearer "+token.Key)
	engine.ServeHTTP(response, request)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
}
