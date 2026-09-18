package controller

import (
	"context"
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
