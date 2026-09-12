package gemini

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type geminiLiveTestResult struct {
	usage *dto.RealtimeUsage
	err   *types.NewAPIError
}

type geminiLiveTestBilling struct {
	mu      sync.Mutex
	reserve int
	targets []int
	err     error
}

func (*geminiLiveTestBilling) Settle(int) error {
	return nil
}

func (*geminiLiveTestBilling) Refund(*gin.Context) {}

func (*geminiLiveTestBilling) NeedsRefund() bool {
	return false
}

func (b *geminiLiveTestBilling) GetPreConsumedQuota() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reserve
}

func (b *geminiLiveTestBilling) Reserve(target int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.targets = append(b.targets, target)
	if b.err != nil {
		return b.err
	}
	b.reserve = max(b.reserve, target)
	return nil
}

func geminiLiveWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{
		CheckOrigin: func(_ *http.Request) bool {
			return true
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		accepted <- conn
	}))
	t.Cleanup(server.Close)

	client, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http"),
		nil,
	)
	require.NoError(t, err)
	serverConn := <-accepted
	t.Cleanup(func() {
		_ = client.Close()
		_ = serverConn.Close()
	})
	return client, serverConn
}

func TestGeminiLiveWebSocketURL(t *testing.T) {
	got, err := GeminiLiveWebSocketURL("https://generativelanguage.googleapis.com")
	require.NoError(t, err)
	assert.Equal(t, "wss://generativelanguage.googleapis.com"+relayconstant.GeminiLivePath, got)

	got, err = GeminiLiveWebSocketURL("http://127.0.0.1:8080/proxy/")
	require.NoError(t, err)
	assert.Equal(t, "ws://127.0.0.1:8080/proxy"+relayconstant.GeminiLivePath, got)

	_, err = GeminiLiveWebSocketURL("ftp://example.com")
	require.Error(t, err)
}

func TestGeminiAdaptorBuildsLiveRequest(t *testing.T) {
	info := &relaycommon.RelayInfo{
		RelayMode: relayconstant.RelayModeGeminiLive,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    "https://generativelanguage.googleapis.com",
			ApiKey:            "upstream-secret",
			UpstreamModelName: "gemini-live-test",
		},
	}
	adaptor := &Adaptor{}

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(
		t,
		"wss://generativelanguage.googleapis.com"+relayconstant.GeminiLivePath,
		requestURL,
	)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, relayconstant.GeminiLivePath, nil)
	header := http.Header{}
	require.NoError(t, adaptor.SetupRequestHeader(c, &header, info))
	assert.Equal(t, "upstream-secret", header.Get("x-goog-api-key"))
	assert.Empty(t, header.Get("Authorization"))
}

func TestGeminiLiveHandlerForwardsSetupFirstAndUsesAuthoritativeUsage(t *testing.T) {
	downstreamClient, relayClient := geminiLiveWebSocketPair(t)
	relayTarget, upstreamServer := geminiLiveWebSocketPair(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, relayconstant.GeminiLivePath, nil)
	common.SetContextKey(
		c,
		constant.ContextKeyRealtimeSetup,
		[]byte(`{"setup":{"model":"models/gemini-live-test"}}`),
	)
	common.SetContextKey(c, constant.ContextKeyRealtimeMessageType, websocket.TextMessage)
	info := &relaycommon.RelayInfo{
		ClientWs: relayClient,
		TargetWs: relayTarget,
	}

	result := make(chan geminiLiveTestResult, 1)
	go func() {
		usage, apiErr := GeminiLiveHandler(c, info)
		result <- geminiLiveTestResult{usage: usage, err: apiErr}
	}()

	messageType, setup, err := upstreamServer.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, messageType)
	assert.JSONEq(t, `{"setup":{"model":"models/gemini-live-test"}}`, string(setup))

	require.NoError(t, downstreamClient.WriteJSON(map[string]any{
		"clientContent": map[string]any{"turnComplete": true},
	}))
	_, clientMessage, err := upstreamServer.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(clientMessage), "clientContent")

	require.NoError(t, upstreamServer.WriteMessage(websocket.TextMessage, []byte(`{
		"serverContent":{"turnComplete":true},
		"usageMetadata":{
			"promptTokenCount":8,
			"toolUsePromptTokenCount":2,
			"responseTokenCount":8,
			"thoughtsTokenCount":2,
			"totalTokenCount":20,
			"cachedContentTokenCount":1,
			"promptTokensDetails":[
				{"modality":"TEXT","tokenCount":6},
				{"modality":"AUDIO","tokenCount":2}
			],
			"toolUsePromptTokensDetails":[
				{"modality":"TEXT","tokenCount":2}
			],
			"responseTokensDetails":[
				{"modality":"AUDIO","tokenCount":8}
			]
		}
	}`)))
	_, response, err := downstreamClient.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(response), "usageMetadata")
	require.NoError(t, upstreamServer.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	))
	_, _, err = downstreamClient.ReadMessage()
	require.Error(t, err)
	assert.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure))

	select {
	case got := <-result:
		require.Nil(t, got.err)
		require.NotNil(t, got.usage)
		assert.Equal(t, 20, got.usage.TotalTokens)
		assert.Equal(t, 10, got.usage.InputTokens)
		assert.Equal(t, 10, got.usage.OutputTokens)
		assert.Equal(t, 8, got.usage.InputTokenDetails.TextTokens)
		assert.Equal(t, 2, got.usage.InputTokenDetails.AudioTokens)
		assert.Equal(t, 1, got.usage.InputTokenDetails.CachedTokens)
		assert.Equal(t, 2, got.usage.OutputTokenDetails.TextTokens)
		assert.Equal(t, 8, got.usage.OutputTokenDetails.AudioTokens)
		assert.False(t, c.GetBool("gemini_live_usage_estimated"))
	case <-time.After(3 * time.Second):
		t.Fatal("Gemini Live handler did not stop after downstream close")
	}
}

func TestGeminiLiveHandlerMarksFallbackUsageAsEstimated(t *testing.T) {
	downstreamClient, relayClient := geminiLiveWebSocketPair(t)
	relayTarget, upstreamServer := geminiLiveWebSocketPair(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, relayconstant.GeminiLivePath, nil)
	common.SetContextKey(
		c,
		constant.ContextKeyRealtimeSetup,
		[]byte(`{"setup":{"model":"models/gemini-live-test"}}`),
	)
	common.SetContextKey(c, constant.ContextKeyRealtimeMessageType, websocket.TextMessage)
	info := &relaycommon.RelayInfo{
		ClientWs: relayClient,
		TargetWs: relayTarget,
	}

	result := make(chan geminiLiveTestResult, 1)
	go func() {
		usage, apiErr := GeminiLiveHandler(c, info)
		result <- geminiLiveTestResult{usage: usage, err: apiErr}
	}()

	_, _, err := upstreamServer.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, downstreamClient.WriteJSON(map[string]any{
		"clientContent": map[string]any{"turnComplete": true},
	}))
	_, _, err = upstreamServer.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, upstreamServer.WriteJSON(map[string]any{
		"serverContent": map[string]any{"turnComplete": true},
	}))
	_, _, err = downstreamClient.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, downstreamClient.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	))

	select {
	case got := <-result:
		require.Nil(t, got.err)
		require.NotNil(t, got.usage)
		assert.Positive(t, got.usage.InputTokens)
		assert.Positive(t, got.usage.OutputTokens)
		assert.True(t, c.GetBool("gemini_live_usage_estimated"))
	case <-time.After(3 * time.Second):
		t.Fatal("Gemini Live handler did not stop after downstream close")
	}
}

func TestGeminiLiveHandlerTreatsClientDisconnectAsSessionEnd(t *testing.T) {
	downstreamClient, relayClient := geminiLiveWebSocketPair(t)
	relayTarget, upstreamServer := geminiLiveWebSocketPair(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, relayconstant.GeminiLivePath, nil)
	common.SetContextKey(
		c,
		constant.ContextKeyRealtimeSetup,
		[]byte(`{"setup":{"model":"models/gemini-live-test"}}`),
	)
	common.SetContextKey(c, constant.ContextKeyRealtimeMessageType, websocket.TextMessage)
	info := &relaycommon.RelayInfo{
		ClientWs: relayClient,
		TargetWs: relayTarget,
	}

	result := make(chan geminiLiveTestResult, 1)
	go func() {
		usage, apiErr := GeminiLiveHandler(c, info)
		result <- geminiLiveTestResult{usage: usage, err: apiErr}
	}()

	_, _, err := upstreamServer.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, downstreamClient.Close())

	select {
	case got := <-result:
		assert.Nil(t, got.err)
		require.NotNil(t, got.usage)
		assert.Positive(t, got.usage.TotalTokens)
	case <-time.After(3 * time.Second):
		t.Fatal("Gemini Live handler did not stop after client disconnect")
	}
}

func TestGeminiLiveHandlerReservesQuotaBySegment(t *testing.T) {
	downstreamClient, relayClient := geminiLiveWebSocketPair(t)
	relayTarget, upstreamServer := geminiLiveWebSocketPair(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, relayconstant.GeminiLivePath, nil)
	common.SetContextKey(
		c,
		constant.ContextKeyRealtimeSetup,
		[]byte(`{"setup":{"model":"models/gemini-live-test"}}`),
	)
	common.SetContextKey(c, constant.ContextKeyRealtimeMessageType, websocket.TextMessage)
	billing := &geminiLiveTestBilling{reserve: 500}
	info := &relaycommon.RelayInfo{
		ClientWs:        relayClient,
		TargetWs:        relayTarget,
		Billing:         billing,
		OriginModelName: "gemini-live-test",
		PriceData: hosttypes.PriceData{
			ModelRatio: 1,
			GroupRatioInfo: hosttypes.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
	}

	result := make(chan geminiLiveTestResult, 1)
	go func() {
		usage, apiErr := GeminiLiveHandler(c, info)
		result <- geminiLiveTestResult{usage: usage, err: apiErr}
	}()

	_, _, err := upstreamServer.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, downstreamClient.WriteJSON(map[string]any{
		"clientContent": map[string]any{
			"parts": []any{map[string]any{"text": strings.Repeat("x", 2400)}},
		},
	}))
	_, _, err = upstreamServer.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, downstreamClient.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	))

	select {
	case got := <-result:
		require.Nil(t, got.err)
	case <-time.After(3 * time.Second):
		t.Fatal("Gemini Live handler did not stop after downstream close")
	}
	assert.Equal(t, []int{1000}, billing.targets)
}

func TestGeminiLiveHandlerStopsBeforeForwardingUnreservedSegment(t *testing.T) {
	downstreamClient, relayClient := geminiLiveWebSocketPair(t)
	relayTarget, upstreamServer := geminiLiveWebSocketPair(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, relayconstant.GeminiLivePath, nil)
	common.SetContextKey(
		c,
		constant.ContextKeyRealtimeSetup,
		[]byte(`{"setup":{"model":"models/gemini-live-test"}}`),
	)
	common.SetContextKey(c, constant.ContextKeyRealtimeMessageType, websocket.TextMessage)
	billing := &geminiLiveTestBilling{
		reserve: 500,
		err:     errors.New("quota exhausted"),
	}
	info := &relaycommon.RelayInfo{
		ClientWs:        relayClient,
		TargetWs:        relayTarget,
		Billing:         billing,
		OriginModelName: "gemini-live-test",
		PriceData: hosttypes.PriceData{
			ModelRatio: 1,
			GroupRatioInfo: hosttypes.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
	}

	result := make(chan geminiLiveTestResult, 1)
	go func() {
		usage, apiErr := GeminiLiveHandler(c, info)
		result <- geminiLiveTestResult{usage: usage, err: apiErr}
	}()

	_, _, err := upstreamServer.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, downstreamClient.WriteJSON(map[string]any{
		"clientContent": map[string]any{
			"parts": []any{map[string]any{"text": strings.Repeat("x", 2400)}},
		},
	}))

	select {
	case got := <-result:
		require.NotNil(t, got.err)
		assert.Contains(t, got.err.Error(), "quota exhausted")
		require.NotNil(t, got.usage)
		setupText, setupAudio := estimateGeminiLiveUsage(
			[]byte(`{"setup":{"model":"models/gemini-live-test"}}`),
			false,
		)
		assert.Equal(
			t,
			setupText+setupAudio,
			got.usage.InputTokens,
		)
		assert.Equal(
			t,
			500,
			common.GetContextKeyInt(c, constant.ContextKeyRealtimeQuotaLimit),
		)
	case <-time.After(3 * time.Second):
		t.Fatal("Gemini Live handler did not stop after quota reservation failure")
	}

	require.NoError(t, upstreamServer.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, _, err = upstreamServer.ReadMessage()
	require.Error(t, err)
}

func TestGeminiLiveReservationErrorPreservesFailureClass(t *testing.T) {
	quotaErr := types.NewErrorWithStatusCode(
		errors.New("quota exhausted"),
		types.ErrorCodeInsufficientUserQuota,
		http.StatusForbidden,
		types.ErrOptionWithSkipRetry(),
	)
	assert.Same(t, quotaErr, geminiLiveReservationError(quotaErr))

	storageErr := geminiLiveReservationError(errors.New("database unavailable"))
	require.NotNil(t, storageErr)
	assert.Equal(t, http.StatusInternalServerError, storageErr.StatusCode)
	assert.Equal(t, types.ErrorCodeUpdateDataError, storageErr.GetErrorCode())
}

func TestGeminiLiveUsageAccumulatorSupportsConcurrentPumps(t *testing.T) {
	const iterations = 1000
	clientMessage := []byte(`{"clientContent":{"turnComplete":true}}`)
	serverMessage := []byte(`{"serverContent":{"turnComplete":true}}`)
	accumulator := &geminiLiveUsageAccumulator{}

	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		for range iterations {
			accumulator.ObserveClient(clientMessage)
		}
	}()
	go func() {
		defer wait.Done()
		for range iterations {
			accumulator.ObserveServer(serverMessage)
		}
	}()
	wait.Wait()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	usage := accumulator.Usage(c)
	clientText, clientAudio := estimateGeminiLiveUsage(clientMessage, false)
	serverText, serverAudio := estimateGeminiLiveUsage(serverMessage, true)
	assert.Equal(t, iterations*(clientText+clientAudio), usage.InputTokens)
	assert.Equal(t, iterations*(serverText+serverAudio), usage.OutputTokens)
	assert.Equal(t, usage.InputTokens+usage.OutputTokens, usage.TotalTokens)
}

func TestGeminiLiveUsageAccumulatorEstimatesOnlyTailAfterMetadata(t *testing.T) {
	accumulator := &geminiLiveUsageAccumulator{}
	accumulator.ObserveClient([]byte(`{"clientContent":{"turnComplete":true}}`))
	accumulator.ObserveServer([]byte(`{
		"usageMetadata":{
			"promptTokenCount":2,
			"responseTokenCount":3,
			"totalTokenCount":5
		}
	}`))
	trailing := []byte(`{"realtimeInput":{"audio":"trailing"}}`)
	accumulator.ObserveClient(trailing)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	usage := accumulator.Usage(c)

	beforeText, beforeAudio := estimateGeminiLiveUsage(
		[]byte(`{"clientContent":{"turnComplete":true}}`),
		false,
	)
	trailingText, trailingAudio := estimateGeminiLiveUsage(trailing, false)
	estimatedInput := beforeText + beforeAudio + trailingText + trailingAudio
	assert.Equal(t, max(2, estimatedInput), usage.InputTokens)
	assert.Equal(t, 3, usage.OutputTokens)
	assert.Equal(t, usage.InputTokens+usage.OutputTokens, usage.TotalTokens)
	assert.True(t, c.GetBool("gemini_live_usage_estimated"))
}

func TestGeminiLiveUsageAccumulatorUsesLatestCumulativeMetadata(t *testing.T) {
	accumulator := &geminiLiveUsageAccumulator{}
	accumulator.ObserveServer([]byte(`{
		"usageMetadata":{
			"promptTokenCount":4,
			"responseTokenCount":6,
			"totalTokenCount":10
		}
	}`))
	accumulator.ObserveServer([]byte(`{
		"usageMetadata":{
			"promptTokenCount":6,
			"responseTokenCount":9,
			"totalTokenCount":15
		}
	}`))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	usage := accumulator.Usage(c)

	assert.Equal(t, 6, usage.InputTokens)
	assert.Equal(t, 9, usage.OutputTokens)
	assert.Equal(t, 15, usage.TotalTokens)
	assert.False(t, c.GetBool("gemini_live_usage_estimated"))
}

func TestGeminiLiveFallbackSeparatesAudioAndTextUsage(t *testing.T) {
	audio := base64.StdEncoding.EncodeToString(make([]byte, 48_000))
	accumulator := &geminiLiveUsageAccumulator{}
	accumulator.ObserveClient([]byte(`{
		"realtimeInput":{
			"audio":{"mimeType":"audio/pcm;rate=24000","data":"` + audio + `"},
			"text":"hello"
		}
	}`))
	accumulator.ObserveServer([]byte(`{
		"serverContent":{
			"modelTurn":{"parts":[
				{"inlineData":{"mimeType":"audio/pcm;rate=24000","data":"` + audio + `"}},
				{"text":"world"}
			]}
		}
	}`))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	usage := accumulator.Usage(c)

	assert.Positive(t, usage.InputTokenDetails.AudioTokens)
	assert.Positive(t, usage.OutputTokenDetails.AudioTokens)
	assert.Positive(t, usage.InputTokenDetails.TextTokens)
	assert.Positive(t, usage.OutputTokenDetails.TextTokens)
	assert.Equal(
		t,
		usage.InputTokenDetails.TextTokens+usage.InputTokenDetails.AudioTokens,
		usage.InputTokens,
	)
	assert.Equal(
		t,
		usage.OutputTokenDetails.TextTokens+usage.OutputTokenDetails.AudioTokens,
		usage.OutputTokens,
	)
	assert.True(t, c.GetBool("gemini_live_usage_estimated"))
}
