package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrdinaryRequestPolicyResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name                 string
		defaultRouting       string
		allowedRouting       []string
		defaultConversion    string
		allowLossyConversion bool
		routingHeader        string
		conversionHeader     string
		wantStatus           int
		wantRouting          hosttypes.RoutingStrategy
		wantConversion       types.ConversionLossPolicy
		wantCode             types.ErrorCode
		wantNext             bool
	}{
		{
			name:           "old token defaults",
			wantStatus:     http.StatusNoContent,
			wantRouting:    hosttypes.RoutingStrategyStable,
			wantConversion: types.ConversionLossPolicyStrict,
			wantNext:       true,
		},
		{
			name:                 "recognized token defaults",
			defaultRouting:       string(hosttypes.RoutingStrategyEconomy),
			allowedRouting:       []string{string(hosttypes.RoutingStrategyStable), string(hosttypes.RoutingStrategyEconomy)},
			defaultConversion:    string(types.ConversionLossPolicySafe),
			allowLossyConversion: true,
			wantStatus:           http.StatusNoContent,
			wantRouting:          hosttypes.RoutingStrategyEconomy,
			wantConversion:       types.ConversionLossPolicySafe,
			wantNext:             true,
		},
		{
			name:                 "headers take precedence",
			defaultRouting:       string(hosttypes.RoutingStrategyEconomy),
			allowedRouting:       []string{string(hosttypes.RoutingStrategyEconomy), string(hosttypes.RoutingStrategyLatency)},
			defaultConversion:    string(types.ConversionLossPolicySafe),
			allowLossyConversion: true,
			routingHeader:        string(hosttypes.RoutingStrategyLatency),
			conversionHeader:     string(types.ConversionLossPolicyAllow),
			wantStatus:           http.StatusNoContent,
			wantRouting:          hosttypes.RoutingStrategyLatency,
			wantConversion:       types.ConversionLossPolicyAllow,
			wantNext:             true,
		},
		{
			name:          "unknown routing strategy",
			routingHeader: "fastest",
			wantStatus:    http.StatusBadRequest,
			wantCode:      types.ErrorCodeInvalidRequest,
		},
		{
			name:             "unknown conversion policy",
			conversionHeader: "lossy",
			wantStatus:       http.StatusBadRequest,
			wantCode:         types.ErrorCodeInvalidRequest,
		},
		{
			name:           "known disallowed routing strategy",
			allowedRouting: []string{string(hosttypes.RoutingStrategyStable)},
			routingHeader:  string(hosttypes.RoutingStrategyEconomy),
			wantStatus:     http.StatusForbidden,
			wantCode:       types.ErrorCodeAccessDenied,
		},
		{
			name:             "safe requires token authorization",
			conversionHeader: string(types.ConversionLossPolicySafe),
			wantStatus:       http.StatusForbidden,
			wantCode:         types.ErrorCodeAccessDenied,
		},
		{
			name:             "allow requires token authorization",
			conversionHeader: string(types.ConversionLossPolicyAllow),
			wantStatus:       http.StatusForbidden,
			wantCode:         types.ErrorCodeAccessDenied,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nextCalled := false
			router := gin.New()
			router.POST(
				"/v1/chat/completions",
				func(c *gin.Context) {
					if test.defaultRouting != "" {
						common.SetContextKey(c, constant.ContextKeyTokenDefaultRoutingStrategy, test.defaultRouting)
					}
					if test.allowedRouting != nil {
						common.SetContextKey(c, constant.ContextKeyTokenAllowedRoutingStrategies, test.allowedRouting)
					}
					if test.defaultConversion != "" {
						common.SetContextKey(c, constant.ContextKeyTokenDefaultConversionPolicy, test.defaultConversion)
					}
					common.SetContextKey(c, constant.ContextKeyTokenAllowLossyConversion, test.allowLossyConversion)
				},
				OrdinaryRequestPolicy(),
				func(c *gin.Context) {
					nextCalled = true
					routing, ok := common.GetContextKeyType[hosttypes.RoutingStrategy](c, constant.ContextKeyRoutingStrategy)
					require.True(t, ok)
					conversion, ok := common.GetContextKeyType[types.ConversionLossPolicy](c, constant.ContextKeyConversionPolicy)
					require.True(t, ok)
					assert.Equal(t, test.wantRouting, routing)
					assert.Equal(t, test.wantConversion, conversion)
					c.Status(http.StatusNoContent)
				},
			)

			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			request.Header.Set(HeaderRoutingStrategy, test.routingHeader)
			request.Header.Set(HeaderConversionPolicy, test.conversionHeader)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Equal(t, test.wantNext, nextCalled)
			if test.wantCode != "" {
				var response struct {
					Error struct {
						Code types.ErrorCode `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				assert.Equal(t, test.wantCode, response.Error.Code)
			}
		})
	}
}

func TestOrdinaryRequestPolicyFailsClosedForInvalidStoredTokenPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token := &model.Token{
		Id:                       1,
		UserId:                   2,
		DefaultRoutingStrategy:   "economy",
		AllowedRoutingStrategies: `["economy","invalid"]`,
		DefaultConversionPolicy:  "allow",
		AllowLossyConversion:     false,
	}

	t.Run("invalid defaults fall back to stable strict", func(t *testing.T) {
		router := gin.New()
		router.POST(
			"/v1/chat/completions",
			func(c *gin.Context) {
				require.NoError(t, SetupContextForToken(c, token))
			},
			OrdinaryRequestPolicy(),
			func(c *gin.Context) {
				routing, ok := common.GetContextKeyType[hosttypes.RoutingStrategy](c, constant.ContextKeyRoutingStrategy)
				require.True(t, ok)
				conversion, ok := common.GetContextKeyType[types.ConversionLossPolicy](c, constant.ContextKeyConversionPolicy)
				require.True(t, ok)
				assert.Equal(t, hosttypes.RoutingStrategyStable, routing)
				assert.Equal(t, types.ConversionLossPolicyStrict, conversion)
				c.Status(http.StatusNoContent)
			},
		)

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		router.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusNoContent, recorder.Code)
	})

	t.Run("invalid allowed list cannot authorize explicit strategy", func(t *testing.T) {
		router := gin.New()
		router.POST(
			"/v1/chat/completions",
			func(c *gin.Context) {
				require.NoError(t, SetupContextForToken(c, token))
			},
			OrdinaryRequestPolicy(),
			func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			},
		)

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		request.Header.Set(HeaderRoutingStrategy, string(hosttypes.RoutingStrategyEconomy))
		router.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusForbidden, recorder.Code)
	})
}
