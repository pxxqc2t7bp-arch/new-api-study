package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	geminidto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func websocketTestURL(serverURL string, path string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + path
}

func TestGeminiLiveSetupRejectsNonSetupBeforeDownstream(t *testing.T) {
	_, token := setupRealtimeTicketMiddlewareTest(t)
	var reached atomic.Bool
	router := gin.New()
	router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {
		reached.Store(true)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	header := http.Header{"Authorization": []string{"Bearer " + token.Key}}
	conn, _, err := websocket.DefaultDialer.Dial(websocketTestURL(server.URL, GeminiLivePath), header)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.WriteJSON(map[string]any{
		"realtimeInput": map[string]any{"mediaChunks": []any{}},
	}))
	_, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(message), "setup")
	_, _, err = conn.ReadMessage()
	require.Error(t, err)
	assert.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation))
	assert.False(t, reached.Load())
}

func TestGeminiLiveSetupTimesOutBeforeDownstream(t *testing.T) {
	_, token := setupRealtimeTicketMiddlewareTest(t)
	var reached atomic.Bool
	router := gin.New()
	router.GET(
		GeminiLivePath,
		RealtimeAuth(),
		geminiLiveSetup(25*time.Millisecond, geminiLiveSetupReadLimit),
		func(c *gin.Context) {
			reached.Store(true)
		},
	)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	header := http.Header{"Authorization": []string{"Bearer " + token.Key}}
	conn, _, err := websocket.DefaultDialer.Dial(
		websocketTestURL(server.URL, GeminiLivePath),
		header,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(message), "setup")
	_, _, err = conn.ReadMessage()
	require.Error(t, err)
	assert.False(t, reached.Load())
}

func TestGeminiLiveSetupRejectsOversizedFirstFrame(t *testing.T) {
	_, token := setupRealtimeTicketMiddlewareTest(t)
	var reached atomic.Bool
	router := gin.New()
	router.GET(
		GeminiLivePath,
		RealtimeAuth(),
		geminiLiveSetup(time.Second, 64),
		func(c *gin.Context) {
			reached.Store(true)
		},
	)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	header := http.Header{"Authorization": []string{"Bearer " + token.Key}}
	conn, _, err := websocket.DefaultDialer.Dial(
		websocketTestURL(server.URL, GeminiLivePath),
		header,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.WriteJSON(map[string]any{
		"setup": map[string]any{
			"model": strings.Repeat("x", 128),
		},
	}))
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err = conn.ReadMessage()
	require.Error(t, err)
	assert.False(t, reached.Load())
}

func TestGeminiLiveSetupStoresCanonicalFirstFrame(t *testing.T) {
	_, token := setupRealtimeTicketMiddlewareTest(t)
	token.ModelLimits = "gemini-live-test"
	require.NoError(t, model.DB.Model(token).Update("model_limits", token.ModelLimits).Error)
	router := gin.New()
	router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {
		assert.Empty(t, c.GetHeader("Authorization"))
		ws, ok := common.GetContextKeyType[*websocket.Conn](c, constant.ContextKeyRealtimeClientWS)
		require.True(t, ok)
		messageType := common.GetContextKeyInt(c, constant.ContextKeyRealtimeMessageType)
		frame, ok := common.GetContextKeyType[[]byte](c, constant.ContextKeyRealtimeSetup)
		require.True(t, ok)
		setup, _, parseErr := geminidto.ParseGeminiLiveSetup(frame)
		require.NoError(t, parseErr)
		assert.Equal(t, websocket.TextMessage, messageType)
		assert.Equal(t, "gemini-live-test", c.Query("model"))
		assert.Equal(t, "gemini-live-test", setup.Model)
		constraints := service.GetChannelConstraints(c)
		require.Len(t, constraints.Filters, 2)
		assert.True(t, constraints.HasFilter(taskdto.FilterChannelTypes))
		assert.True(t, constraints.HasFilter(taskdto.FilterGeminiLive))
		assert.Equal(t, []int{constant.ChannelTypeGemini}, constraints.Filters[0].AllowedChannelTypes)
		require.NoError(t, ws.WriteJSON(map[string]any{"setupComplete": map[string]any{}}))
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	header := http.Header{"Authorization": []string{"Bearer " + token.Key}}
	conn, _, err := websocket.DefaultDialer.Dial(websocketTestURL(server.URL, GeminiLivePath), header)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{
		"setup":{"model":"models/gemini-live-test","generationConfig":{"responseModalities":["TEXT"]}}
	}`)))
	_, response, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.JSONEq(t, `{"setupComplete":{}}`, string(response))
}

func TestGeminiLiveTicketModelMismatchDoesNotConsumeTicket(t *testing.T) {
	user, token := setupRealtimeTicketMiddlewareTest(t)
	raw, _, err := model.CreateRealtimeTicket(model.RealtimeTicketCreate{
		UserId:          user.Id,
		TokenId:         token.Id,
		Model:           "gemini-live-test",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)

	router := gin.New()
	router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {
		t.Fatal("mismatched setup reached downstream handler")
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial(
		websocketTestURL(server.URL, GeminiLivePath)+"?ticket="+url.QueryEscape(raw),
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.WriteJSON(map[string]any{
		"setup": map[string]any{"model": "models/other-live-model"},
	}))
	_, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(message), "ticket")

	_, err = model.ConsumeRealtimeTicket(raw, "gemini-live-test")
	require.NoError(t, err)
}

func TestGeminiLiveTicketQueryModelCannotTriggerEarlyConsumption(t *testing.T) {
	user, token := setupRealtimeTicketMiddlewareTest(t)
	token.ModelLimits = "gemini-live-test"
	require.NoError(t, model.DB.Model(token).Update("model_limits", token.ModelLimits).Error)
	raw, _, err := model.CreateRealtimeTicket(model.RealtimeTicketCreate{
		UserId:          user.Id,
		TokenId:         token.Id,
		Model:           "gemini-live-test",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)

	router := gin.New()
	router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {
		t.Fatal("mismatched setup reached downstream handler")
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial(
		websocketTestURL(server.URL, GeminiLivePath)+
			"?model=gemini-live-test&ticket="+url.QueryEscape(raw),
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.WriteJSON(map[string]any{
		"setup": map[string]any{"model": "models/other-live-model"},
	}))
	_, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(message), "ticket")

	_, err = model.ConsumeRealtimeTicket(raw, "gemini-live-test")
	require.NoError(t, err)
}

func TestGeminiLiveTicketConsumesAfterMatchingSetup(t *testing.T) {
	user, token := setupRealtimeTicketMiddlewareTest(t)
	token.ModelLimits = "gemini-live-test"
	require.NoError(t, model.DB.Model(token).Update("model_limits", token.ModelLimits).Error)
	raw, _, err := model.CreateRealtimeTicket(model.RealtimeTicketCreate{
		UserId:          user.Id,
		TokenId:         token.Id,
		Model:           "gemini-live-test",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)

	router := gin.New()
	router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {
		ws, _ := common.GetContextKeyType[*websocket.Conn](c, constant.ContextKeyRealtimeClientWS)
		require.NoError(t, ws.WriteJSON(map[string]any{"setupComplete": map[string]any{}}))
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial(
		websocketTestURL(server.URL, GeminiLivePath)+"?ticket="+url.QueryEscape(raw),
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.WriteJSON(map[string]any{
		"setup": map[string]any{"model": "models/gemini-live-test"},
	}))
	_, _, err = conn.ReadMessage()
	require.NoError(t, err)

	_, err = model.ConsumeRealtimeTicket(raw, "gemini-live-test")
	assert.ErrorIs(t, err, model.ErrAuthFlowConsumed)
}

func TestGeminiLiveTokenModelRejectionDoesNotConsumeTicket(t *testing.T) {
	user, token := setupRealtimeTicketMiddlewareTest(t)
	raw, _, err := model.CreateRealtimeTicket(model.RealtimeTicketCreate{
		UserId:          user.Id,
		TokenId:         token.Id,
		Model:           "gemini-live-test",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)

	var reached atomic.Bool
	router := gin.New()
	router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {
		reached.Store(true)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial(
		websocketTestURL(server.URL, GeminiLivePath)+"?ticket="+url.QueryEscape(raw),
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.WriteJSON(map[string]any{
		"setup": map[string]any{"model": "models/gemini-live-test"},
	}))
	_, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(message), "model")
	assert.False(t, reached.Load())

	_, err = model.ConsumeRealtimeTicket(raw, "gemini-live-test")
	require.NoError(t, err)
}

func TestGeminiLiveRevalidatesTicketPolicyAfterUpgrade(t *testing.T) {
	user, token := setupRealtimeTicketMiddlewareTest(t)
	token.ModelLimits = "gemini-live-test"
	require.NoError(t, model.DB.Model(token).Update("model_limits", token.ModelLimits).Error)
	raw, _, err := model.CreateRealtimeTicket(model.RealtimeTicketCreate{
		UserId:          user.Id,
		TokenId:         token.Id,
		Model:           "gemini-live-test",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)

	router := gin.New()
	router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {
		t.Fatal("revoked model policy reached downstream handler")
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial(
		websocketTestURL(server.URL, GeminiLivePath)+"?ticket="+url.QueryEscape(raw),
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, model.DB.Model(token).Update("model_limits", "other-model").Error)
	require.NoError(t, conn.WriteJSON(map[string]any{
		"setup": map[string]any{"model": "models/gemini-live-test"},
	}))
	_, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(message), "model")

	_, err = model.ConsumeRealtimeTicket(raw, "gemini-live-test")
	require.NoError(t, err)
}

func TestGeminiLiveEndpointRejectsUnauthenticatedBeforeUpgrade(t *testing.T) {
	setupRealtimeTicketMiddlewareTest(t)
	router := gin.New()
	router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, response, err := websocket.DefaultDialer.Dial(websocketTestURL(server.URL, GeminiLivePath), nil)
	require.Error(t, err)
	assert.Nil(t, conn)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	_ = response.Body.Close()
}

func TestRequestSucceededRejectsPostUpgradeGeminiLiveError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.True(t, requestSucceeded(c))

	common.SetContextKey(c, constant.ContextKeyRealtimeFailed, true)
	assert.False(t, requestSucceeded(c))
}

func TestGeminiLiveNativeAPIKeyTransportsAreScrubbed(t *testing.T) {
	tests := []struct {
		name   string
		path   func(string) string
		header func(string) http.Header
	}{
		{
			name: "x-goog-api-key header",
			path: func(string) string {
				return GeminiLivePath
			},
			header: func(key string) http.Header {
				return http.Header{"x-goog-api-key": []string{key}}
			},
		},
		{
			name: "key query",
			path: func(key string) string {
				return GeminiLivePath + "?key=" + url.QueryEscape(key)
			},
			header: func(string) http.Header {
				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, token := setupRealtimeTicketMiddlewareTest(t)
			token.ModelLimits = "gemini-live-test"
			require.NoError(t, model.DB.Model(token).Update("model_limits", token.ModelLimits).Error)

			router := gin.New()
			router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {
				assert.Empty(t, c.GetHeader("Authorization"))
				assert.Empty(t, c.GetHeader("x-goog-api-key"))
				assert.Empty(t, c.Query("key"))
				assert.NotContains(t, c.Request.RequestURI, token.Key)
				ws, _ := common.GetContextKeyType[*websocket.Conn](
					c,
					constant.ContextKeyRealtimeClientWS,
				)
				require.NoError(t, ws.WriteJSON(map[string]any{
					"setupComplete": map[string]any{},
				}))
			})
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)

			conn, _, err := websocket.DefaultDialer.Dial(
				websocketTestURL(server.URL, test.path(token.Key)),
				test.header(token.Key),
			)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			require.NoError(t, conn.WriteJSON(map[string]any{
				"setup": map[string]any{"model": "models/gemini-live-test"},
			}))
			_, _, err = conn.ReadMessage()
			require.NoError(t, err)
		})
	}
}

func TestGeminiLiveRejectsTicketMixedWithNativeCredential(t *testing.T) {
	tests := []struct {
		name   string
		path   func(string, string) string
		header func(string) http.Header
	}{
		{
			name: "x-goog-api-key header",
			path: func(ticket string, _ string) string {
				return GeminiLivePath + "?ticket=" + url.QueryEscape(ticket)
			},
			header: func(key string) http.Header {
				return http.Header{"x-goog-api-key": []string{key}}
			},
		},
		{
			name: "key query",
			path: func(ticket string, key string) string {
				return GeminiLivePath +
					"?ticket=" + url.QueryEscape(ticket) +
					"&key=" + url.QueryEscape(key)
			},
			header: func(string) http.Header {
				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user, token := setupRealtimeTicketMiddlewareTest(t)
			raw, _, err := model.CreateRealtimeTicket(model.RealtimeTicketCreate{
				UserId:          user.Id,
				TokenId:         token.Id,
				Model:           "gemini-live-test",
				RoutingStrategy: hosttypes.RoutingStrategyStable,
			})
			require.NoError(t, err)

			router := gin.New()
			router.GET(GeminiLivePath, RealtimeAuth(), GeminiLiveSetup(), func(c *gin.Context) {
				t.Fatal("ambiguous credentials reached downstream handler")
			})
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)

			conn, response, err := websocket.DefaultDialer.Dial(
				websocketTestURL(server.URL, test.path(raw, token.Key)),
				test.header(token.Key),
			)
			require.Error(t, err)
			assert.Nil(t, conn)
			require.NotNil(t, response)
			assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
			_ = response.Body.Close()

			_, err = model.ConsumeRealtimeTicket(raw, "gemini-live-test")
			require.NoError(t, err)
		})
	}
}
