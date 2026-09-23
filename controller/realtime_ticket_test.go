package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupRealtimeTicketControllerTest(t *testing.T) *gin.Engine {
	t.Helper()
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.AuthFlow{}))

	router := gin.New()
	router.POST("/api/realtime/tickets", func(c *gin.Context) {
		c.Set("id", 7)
	}, IssueRealtimeTicket)
	return router
}

func issueRealtimeTicketRequest(t *testing.T, router *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/realtime/tickets", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestIssueRealtimeTicketBindsOwnedTokenModelAndStrategy(t *testing.T) {
	router := setupRealtimeTicketControllerTest(t)
	token := model.Token{
		UserId:                   7,
		Key:                      "realtime-ticket-owned-key",
		Name:                     "realtime-ticket-owned",
		Status:                   common.TokenStatusEnabled,
		ExpiredTime:              -1,
		RemainQuota:              100,
		ModelLimitsEnabled:       true,
		ModelLimits:              "gpt-realtime",
		DefaultRoutingStrategy:   "stable",
		AllowedRoutingStrategies: `["stable","latency"]`,
	}
	require.NoError(t, token.Insert())

	response := issueRealtimeTicketRequest(t, router, fmt.Sprintf(
		`{"token_id":%d,"model":"gpt-realtime","routing_strategy":"latency"}`,
		token.Id,
	))
	require.Equal(t, http.StatusOK, response.Code)
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Ticket          string `json:"ticket"`
			ExpiresAt       int64  `json:"expires_at"`
			Model           string `json:"model"`
			RoutingStrategy string `json:"routing_strategy"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.True(t, payload.Success)
	assert.True(t, strings.HasPrefix(payload.Data.Ticket, model.RealtimeTicketPrefix))
	assert.Equal(t, "gpt-realtime", payload.Data.Model)
	assert.Equal(t, "latency", payload.Data.RoutingStrategy)
	assert.NotContains(t, response.Body.String(), token.Key)

	claims, err := model.ConsumeRealtimeTicket(payload.Data.Ticket, "gpt-realtime")
	require.NoError(t, err)
	assert.Equal(t, token.Id, claims.TokenId)
	assert.Equal(t, token.UserId, claims.UserId)
	assert.Equal(t, hosttypes.RoutingStrategyLatency, claims.RoutingStrategy)
}

func TestIssueRealtimeTicketRejectsInvalidBindings(t *testing.T) {
	tests := []struct {
		name       string
		token      model.Token
		body       func(model.Token) string
		wantStatus int
	}{
		{
			name: "foreign token",
			token: model.Token{
				UserId: 8, Status: common.TokenStatusEnabled, ExpiredTime: -1,
				RemainQuota: 100, DefaultRoutingStrategy: "stable",
				AllowedRoutingStrategies: `["stable"]`,
			},
			body:       func(token model.Token) string { return fmt.Sprintf(`{"token_id":%d,"model":"gpt-realtime"}`, token.Id) },
			wantStatus: http.StatusNotFound,
		},
		{
			name: "disabled token",
			token: model.Token{
				UserId: 7, Status: common.TokenStatusDisabled, ExpiredTime: -1,
				RemainQuota: 100, DefaultRoutingStrategy: "stable",
				AllowedRoutingStrategies: `["stable"]`,
			},
			body:       func(token model.Token) string { return fmt.Sprintf(`{"token_id":%d,"model":"gpt-realtime"}`, token.Id) },
			wantStatus: http.StatusForbidden,
		},
		{
			name: "model outside token limit",
			token: model.Token{
				UserId: 7, Status: common.TokenStatusEnabled, ExpiredTime: -1,
				RemainQuota: 100, ModelLimitsEnabled: true, ModelLimits: "gpt-realtime",
				DefaultRoutingStrategy: "stable", AllowedRoutingStrategies: `["stable"]`,
			},
			body: func(token model.Token) string {
				return fmt.Sprintf(`{"token_id":%d,"model":"other-model"}`, token.Id)
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "unauthorized strategy",
			token: model.Token{
				UserId: 7, Status: common.TokenStatusEnabled, ExpiredTime: -1,
				RemainQuota: 100, DefaultRoutingStrategy: "stable",
				AllowedRoutingStrategies: `["stable"]`,
			},
			body: func(token model.Token) string {
				return fmt.Sprintf(`{"token_id":%d,"model":"gpt-realtime","routing_strategy":"latency"}`, token.Id)
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "unknown strategy",
			token: model.Token{
				UserId: 7, Status: common.TokenStatusEnabled, ExpiredTime: -1,
				RemainQuota: 100, DefaultRoutingStrategy: "stable",
				AllowedRoutingStrategies: `["stable"]`,
			},
			body: func(token model.Token) string {
				return fmt.Sprintf(`{"token_id":%d,"model":"gpt-realtime","routing_strategy":"fastest"}`, token.Id)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "malformed token routing policy",
			token: model.Token{
				UserId: 7, Status: common.TokenStatusEnabled, ExpiredTime: -1,
				RemainQuota: 100, DefaultRoutingStrategy: "stable",
				AllowedRoutingStrategies: `["stable"`,
			},
			body: func(token model.Token) string {
				return fmt.Sprintf(`{"token_id":%d,"model":"gpt-realtime"}`, token.Id)
			},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := setupRealtimeTicketControllerTest(t)
			test.token.Key = "realtime-ticket-" + strings.ReplaceAll(test.name, " ", "-")
			test.token.Name = test.name
			require.NoError(t, test.token.Insert())

			response := issueRealtimeTicketRequest(t, router, test.body(test.token))
			assert.Equal(t, test.wantStatus, response.Code)
			assert.NotContains(t, response.Body.String(), test.token.Key)
		})
	}
}

func TestIssueRealtimeTicketReportsTokenDatabaseFailure(t *testing.T) {
	router := setupRealtimeTicketControllerTest(t)
	token := model.Token{
		UserId:                   7,
		Key:                      "realtimeticketdatabasefailure",
		Name:                     "database-failure",
		Status:                   common.TokenStatusEnabled,
		ExpiredTime:              -1,
		RemainQuota:              100,
		DefaultRoutingStrategy:   "stable",
		AllowedRoutingStrategies: `["stable"]`,
	}
	require.NoError(t, token.Insert())

	forcedErr := fmt.Errorf("forced realtime ticket token lookup failure")
	var intercepted atomic.Bool
	const callbackName = "test:fail_realtime_ticket_issue_lookup"
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" && intercepted.CompareAndSwap(false, true) {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		_ = model.DB.Callback().Query().Remove(callbackName)
	})

	response := issueRealtimeTicketRequest(t, router, fmt.Sprintf(
		`{"token_id":%d,"model":"gpt-realtime"}`,
		token.Id,
	))
	assert.Equal(t, http.StatusInternalServerError, response.Code)
}

func TestIssueRealtimeTicketReportsPersistenceFailure(t *testing.T) {
	router := setupRealtimeTicketControllerTest(t)
	token := model.Token{
		UserId:                   7,
		Key:                      "realtimeticketpersistencefailure",
		Name:                     "persistence-failure",
		Status:                   common.TokenStatusEnabled,
		ExpiredTime:              -1,
		RemainQuota:              100,
		DefaultRoutingStrategy:   "stable",
		AllowedRoutingStrategies: `["stable"]`,
	}
	require.NoError(t, token.Insert())

	forcedErr := fmt.Errorf("forced realtime ticket persistence failure")
	var intercepted atomic.Bool
	const callbackName = "test:fail_realtime_ticket_persistence"
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "auth_flows" && intercepted.CompareAndSwap(false, true) {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		_ = model.DB.Callback().Create().Remove(callbackName)
	})

	response := issueRealtimeTicketRequest(t, router, fmt.Sprintf(
		`{"token_id":%d,"model":"gpt-realtime"}`,
		token.Id,
	))
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.NotContains(t, response.Body.String(), token.Key)
}
