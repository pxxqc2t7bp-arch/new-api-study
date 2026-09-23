package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRelayTaskPluginEndpointPreservesUnclaimedFallback(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	fallbackCalls := 0

	RelayTaskPluginEndpoint(c, func(c *gin.Context) {
		fallbackCalls++
		c.Status(http.StatusNoContent)
		c.Writer.WriteHeaderNow()
	})

	assert.Equal(t, 1, fallbackCalls)
	assert.Equal(t, http.StatusNoContent, recorder.Code)
}

func TestRelayTaskPluginEndpointNeverEntersOrdinaryRelayWhenClaimed(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{
		Generation: &jsplugin.RoutingGeneration{},
		Plugin:     &jsplugin.LoadedPlugin{},
		Protocol:   "openai_responses",
		Operation:  jsplugin.HostProtocolOperation{Name: "create"},
	})
	fallbackCalls := 0

	RelayTaskPluginEndpoint(c, func(c *gin.Context) {
		fallbackCalls++
		c.Status(http.StatusNoContent)
		c.Writer.WriteHeaderNow()
	})

	assert.Zero(t, fallbackCalls)
	assert.NotEqual(t, http.StatusNoContent, recorder.Code)
}

func TestRelayTaskPluginEndpointDeliversCommittedNativeResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	raw := `{"model":"registered","input":"hello","max_output_tokens":16}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	c.Set(hosttypes.AppRelaySubjectContextKey, &hosttypes.AppRelaySubject{
		ExecutionKind: model.AppExecutionModelKindNativeResponse,
	})
	fallbackCalls, executeCalls := 0, 0

	relayTaskPluginEndpointWith(c, func(*gin.Context) { fallbackCalls++ }, func(
		_ context.Context, subject hosttypes.AppRelaySubject, body []byte,
	) (service.AppResponseExecutionResult, error) {
		executeCalls++
		assert.Equal(t, model.AppExecutionModelKindNativeResponse, subject.ExecutionKind)
		assert.JSONEq(t, raw, string(body))
		return service.AppResponseExecutionResult{Result: model.AppResponseResult{
			HTTPStatus: http.StatusOK, SafeHeadersJSON: `{"Content-Type":["application/json"],"X-Request-ID":["upstream"]}`,
			Body: []byte(`{"id":"resp_committed"}`),
		}}, nil
	})

	assert.Zero(t, fallbackCalls)
	assert.Equal(t, 1, executeCalls)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
	assert.Equal(t, "upstream", recorder.Header().Get("X-Request-ID"))
	assert.Equal(t, strconv.Itoa(len(`{"id":"resp_committed"}`)), recorder.Header().Get("Content-Length"))
	assert.JSONEq(t, `{"id":"resp_committed"}`, recorder.Body.String())
}

func TestRelayTaskPluginEndpointMapsUnknownNativeOutcome(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"registered","input":"hello","max_output_tokens":16}`))
	c.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	c.Set(hosttypes.AppRelaySubjectContextKey, &hosttypes.AppRelaySubject{
		ExecutionKind: model.AppExecutionModelKindNativeResponse,
	})

	relayTaskPluginEndpointWith(c, func(*gin.Context) {
		t.Fatal("native response must not enter ordinary fallback")
	}, func(context.Context, hosttypes.AppRelaySubject, []byte) (service.AppResponseExecutionResult, error) {
		return service.AppResponseExecutionResult{}, &service.AppPluginAuthError{Code: "execution_outcome_unknown"}
	})

	assert.Equal(t, http.StatusConflict, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"code":"execution_outcome_unknown"`)
	require.NotEmpty(t, recorder.Header().Get(common.RequestIdKey))
}

func TestRelayAppNativeResponseMapsExecutionErrorRetryability(t *testing.T) {
	tests := []struct {
		name          string
		executionErr  error
		wantStatus    int
		wantCode      string
		wantRetryable bool
	}{
		{
			name:          "transient auth error",
			executionErr:  &service.AppPluginAuthError{Code: "service_unavailable"},
			wantStatus:    http.StatusServiceUnavailable,
			wantCode:      "service_unavailable",
			wantRetryable: true,
		},
		{
			name:          "disabled rollout",
			executionErr:  &service.AppPluginAuthError{Code: "app_execution_disabled"},
			wantStatus:    http.StatusForbidden,
			wantCode:      "app_execution_disabled",
			wantRetryable: false,
		},
		{
			name:          "ordinary internal error",
			executionErr:  errors.New("database unavailable"),
			wantStatus:    http.StatusServiceUnavailable,
			wantCode:      "service_unavailable",
			wantRetryable: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses",
				strings.NewReader(`{"model":"registered","input":"hello","max_output_tokens":16}`))
			t.Cleanup(func() { common.CleanupBodyStorage(c) })

			relayAppNativeResponse(c, &hosttypes.AppRelaySubject{
				ExecutionKind: model.AppExecutionModelKindNativeResponse,
			}, func(context.Context, hosttypes.AppRelaySubject, []byte) (service.AppResponseExecutionResult, error) {
				return service.AppResponseExecutionResult{}, test.executionErr
			})

			var response struct {
				Error struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Equal(t, test.wantCode, response.Error.Code)
			assert.Equal(t, test.wantRetryable, response.Error.Retryable)
			require.NotEmpty(t, recorder.Header().Get(common.RequestIdKey))
		})
	}
}

func TestRelayTaskPluginEndpointMapsInternalNativeFailureAsRetryable(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"registered","input":"hello","max_output_tokens":16}`))
	c.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	c.Set(hosttypes.AppRelaySubjectContextKey, &hosttypes.AppRelaySubject{
		ExecutionKind: model.AppExecutionModelKindNativeResponse,
	})

	relayTaskPluginEndpointWith(c, func(*gin.Context) {
		t.Fatal("native response must not enter ordinary fallback")
	}, func(context.Context, hosttypes.AppRelaySubject, []byte) (service.AppResponseExecutionResult, error) {
		return service.AppResponseExecutionResult{}, errors.New("database unavailable")
	})

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"code":"service_unavailable"`)
	assert.Contains(t, recorder.Body.String(), `"retryable":true`)
	require.NotEmpty(t, recorder.Header().Get(common.RequestIdKey))
}

func TestRelayTaskPluginEndpointMapsGrantStorageFailureAsRetryable(t *testing.T) {
	setupAppPluginControllerTest(t)
	t.Setenv("APP_PLUGIN_LAUNCH_SECRET", strings.Repeat("k", 32))
	const callbackName = "test:native-controller-grant-storage-failure"
	const privateError = "private native grant database detail"
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").
		Register(callbackName, func(tx *gorm.DB) {
			if tx.Statement.Table == "app_execution_grants" {
				tx.AddError(errors.New(privateError))
			}
		}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Callback().Query().Remove(callbackName))
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"registered","input":"hello","max_output_tokens":16}`))
	c.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	c.Set(hosttypes.AppRelaySubjectContextKey, &hosttypes.AppRelaySubject{
		TokenHash:     strings.Repeat("a", 64),
		Protocol:      "openai_responses",
		ExecutionKind: model.AppExecutionModelKindNativeResponse,
	})
	fallbackCalls := 0

	RelayTaskPluginEndpoint(c, func(*gin.Context) {
		fallbackCalls++
	})

	var response struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Zero(t, fallbackCalls)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Equal(t, "service_unavailable", response.Error.Code)
	assert.Equal(t, "App response execution failed", response.Error.Message)
	assert.True(t, response.Error.Retryable)
	assert.NotContains(t, recorder.Body.String(), privateError)
	require.NotEmpty(t, recorder.Header().Get(common.RequestIdKey))
}
