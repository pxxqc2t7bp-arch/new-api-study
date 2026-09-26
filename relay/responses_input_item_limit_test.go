package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const responsesInputItemSoftLimitHeaderForTest = "X-NewAPI-Responses-Input-Item-Soft-Limit"

func TestResolveResponsesInputItemLimit(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		want       int
		wantSource string
		wantErr    bool
	}{
		{name: "default", want: 1000, wantSource: "ark_default"},
		{name: "minimum client limit", header: "1", want: 1, wantSource: "client_opt_in"},
		{name: "zcode soft limit", header: "900", want: 900, wantSource: "client_opt_in"},
		{name: "maximum client limit", header: "1000", want: 1000, wantSource: "client_opt_in"},
		{name: "zero", header: "0", wantErr: true},
		{name: "negative", header: "-1", wantErr: true},
		{name: "above hard limit", header: "1001", wantErr: true},
		{name: "not an integer", header: "nine hundred", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source, err := resolveResponsesInputItemLimit(tt.header)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantSource, source)
		})
	}
}

func TestIsNativeArkResponsesURL(t *testing.T) {
	tests := []struct {
		rawURL string
		want   bool
	}{
		{rawURL: "https://ark.cn-beijing.volces.com/api/v3/responses", want: true},
		{rawURL: "https://ARK.CN-BEIJING.VOLCES.COM/api/v3/responses", want: true},
		{rawURL: "https://ark.cn-beijing.volces.com/api/coding/v3/responses", want: true},
		{rawURL: "https://ark.cn-beijing.volces.com/api/v3/chat/completions", want: false},
		{rawURL: "https://ark.cn-beijing.volces.com.example/v1/responses", want: false},
		{rawURL: "https://fallback.example/v1/responses", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.rawURL, func(t *testing.T) {
			assert.Equal(t, tt.want, isNativeArkResponsesURL(tt.rawURL))
		})
	}
}

func TestArkResponsesInputItemLimit(t *testing.T) {
	tests := []struct {
		name      string
		count     int
		input     json.RawMessage
		header    string
		wantLimit int
		wantErr   bool
	}{
		{name: "hard limit below boundary", count: 999},
		{name: "hard limit boundary", count: 1000},
		{name: "hard limit overflow", count: 1001, wantLimit: 1000, wantErr: true},
		{name: "soft limit below boundary", count: 899, header: "900"},
		{name: "soft limit boundary", count: 900, header: "900"},
		{name: "soft limit overflow", count: 901, header: "900", wantLimit: 900, wantErr: true},
		{name: "scalar input bypass", input: json.RawMessage(`"hello"`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if tt.header != "" {
				c.Request.Header.Set(responsesInputItemSoftLimitHeaderForTest, tt.header)
			}

			input := tt.input
			if input == nil {
				input = responsesInputItems(tt.count)
			}
			info := &relaycommon.RelayInfo{
				OriginModelName: "glm-5.3",
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelId: 97,
				},
			}
			apiErr := enforceArkResponsesInputItemLimit(
				c,
				info,
				"https://ark.cn-beijing.volces.com/api/v3/responses",
				&dto.OpenAIResponsesRequest{Model: "glm-5.3", Input: input},
			)

			assert.Empty(t, c.Request.Header.Get(responsesInputItemSoftLimitHeaderForTest))
			if !tt.wantErr {
				require.Nil(t, apiErr)
				return
			}
			requireResponsesInputItemLimitError(t, apiErr, tt.count, tt.wantLimit)
		})
	}
}

func TestPrepareResponsesRequestItemLimit(t *testing.T) {
	t.Run("hard limit rejects before reading body", func(t *testing.T) {
		reader := &responsesInputFailReader{}
		c, info, request := newResponsesInputItemLimitContext(
			t,
			responsesInputItems(1001),
			reader,
			constant.ChannelTypeVolcEngine,
			"https://ark.cn-beijing.volces.com/api/v3",
			dto.ChannelOtherSettings{},
		)

		adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)

		assert.Nil(t, adaptor)
		assert.Nil(t, body)
		assert.Nil(t, closer)
		requireResponsesInputItemLimitError(t, apiErr, 1001, 1000)
		assert.False(t, reader.read, "oversized input must be rejected before body storage is read")
	})

	t.Run("default conversion hard limit rejects", func(t *testing.T) {
		settings := model_setting.GetGlobalSettings()
		originalPassThrough := settings.PassThroughRequestEnabled
		settings.PassThroughRequestEnabled = false
		t.Cleanup(func() {
			settings.PassThroughRequestEnabled = originalPassThrough
		})

		reader := &responsesInputFailReader{}
		c, info, request := newResponsesInputItemLimitContext(
			t,
			responsesInputItems(1001),
			reader,
			constant.ChannelTypeVolcEngine,
			"https://ark.cn-beijing.volces.com/api/v3",
			dto.ChannelOtherSettings{},
		)
		common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{})
		t.Cleanup(func() {
			common.CleanupBodyStorage(c)
		})

		adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
		if closer != nil {
			t.Cleanup(func() {
				assert.NoError(t, closer.Close())
			})
		}

		assert.Nil(t, adaptor)
		assert.Nil(t, body)
		assert.Nil(t, closer)
		requireResponsesInputItemLimitError(t, apiErr, 1001, 1000)
		assert.False(t, reader.read, "oversized input must be rejected before default conversion")
	})

	t.Run("soft limit rejects and deletes header before reading body", func(t *testing.T) {
		reader := &responsesInputFailReader{}
		c, info, request := newResponsesInputItemLimitContext(
			t,
			responsesInputItems(901),
			reader,
			constant.ChannelTypeVolcEngine,
			"https://ark.cn-beijing.volces.com/api/v3",
			dto.ChannelOtherSettings{},
		)
		c.Request.Header.Set(responsesInputItemSoftLimitHeaderForTest, "900")

		adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)

		assert.Nil(t, adaptor)
		assert.Nil(t, body)
		assert.Nil(t, closer)
		requireResponsesInputItemLimitError(t, apiErr, 901, 900)
		assert.False(t, reader.read, "oversized input must be rejected before body storage is read")
		assert.Empty(t, c.Request.Header.Get(responsesInputItemSoftLimitHeaderForTest))
	})

	t.Run("empty soft limit rejects and deletes header before reading body", func(t *testing.T) {
		reader := &responsesInputFailReader{}
		c, info, request := newResponsesInputItemLimitContext(
			t,
			responsesInputItems(1),
			reader,
			constant.ChannelTypeVolcEngine,
			"https://ark.cn-beijing.volces.com/api/v3",
			dto.ChannelOtherSettings{},
		)
		canonicalHeader := http.CanonicalHeaderKey(responsesInputItemSoftLimitHeaderForTest)
		c.Request.Header[canonicalHeader] = []string{""}
		_, present := c.Request.Header[canonicalHeader]
		require.True(t, present)

		adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)

		assert.Nil(t, adaptor)
		assert.Nil(t, body)
		assert.Nil(t, closer)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
		assert.Equal(t, types.ErrorTypeOpenAIError, apiErr.GetErrorType())
		assert.Equal(t, types.ErrorCodeInvalidRequest, apiErr.GetErrorCode())
		assert.True(t, types.IsSkipRetryError(apiErr))
		assert.Equal(t, types.OpenAIError{
			Message: "X-NewAPI-Responses-Input-Item-Soft-Limit must be an integer from 1 through 1000",
			Type:    "invalid_request_error",
			Param:   responsesInputItemSoftLimitHeaderForTest,
			Code:    string(types.ErrorCodeInvalidRequest),
		}, apiErr.ToOpenAIError())
		assert.False(t, reader.read, "empty header must be rejected before body storage is read")
		assert.NotContains(t, c.Request.Header, canonicalHeader)
	})

	t.Run("invalid soft limit rejects and deletes header before reading body", func(t *testing.T) {
		reader := &responsesInputFailReader{}
		c, info, request := newResponsesInputItemLimitContext(
			t,
			responsesInputItems(1),
			reader,
			constant.ChannelTypeVolcEngine,
			"https://ark.cn-beijing.volces.com/api/v3",
			dto.ChannelOtherSettings{},
		)
		c.Request.Header.Set(responsesInputItemSoftLimitHeaderForTest, "0")
		_, _, wantErr := resolveResponsesInputItemLimit("0")
		require.Error(t, wantErr)

		adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)

		assert.Nil(t, adaptor)
		assert.Nil(t, body)
		assert.Nil(t, closer)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
		assert.Equal(t, types.ErrorTypeOpenAIError, apiErr.GetErrorType())
		assert.Equal(t, types.ErrorCodeInvalidRequest, apiErr.GetErrorCode())
		assert.True(t, types.IsSkipRetryError(apiErr))
		openAIError := apiErr.ToOpenAIError()
		assert.Equal(t, "invalid_request_error", openAIError.Type)
		assert.Equal(t, wantErr.Error(), openAIError.Message)
		assert.False(t, reader.read, "invalid header must be rejected before body storage is read")
		assert.Empty(t, c.Request.Header.Get(responsesInputItemSoftLimitHeaderForTest))
	})

	t.Run("advanced custom native Ark route is guarded", func(t *testing.T) {
		reader := &responsesInputFailReader{}
		c, info, request := newResponsesInputItemLimitContext(
			t,
			responsesInputItems(1001),
			reader,
			constant.ChannelTypeAdvancedCustom,
			"https://ark.cn-beijing.volces.com/api/v3",
			dto.ChannelOtherSettings{
				AdvancedCustom: &dto.AdvancedCustomConfig{
					Routes: []dto.AdvancedCustomRoute{{
						IncomingPath: "/v1/responses",
						UpstreamPath: "/responses",
						Converter:    relayconvert.ConverterNone,
					}},
				},
			},
		)

		adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)

		assert.Nil(t, adaptor)
		assert.Nil(t, body)
		assert.Nil(t, closer)
		requireResponsesInputItemLimitError(t, apiErr, 1001, 1000)
		assert.False(t, reader.read, "oversized input must be rejected before body storage is read")
	})

	t.Run("soft limit persists across channel attempts", func(t *testing.T) {
		input := responsesInputItems(901)
		rawBody := responsesInputRequestBody(input)
		c, info, request := newResponsesInputItemLimitContext(
			t,
			input,
			bytes.NewReader(rawBody),
			constant.ChannelTypeVolcEngine,
			"https://fallback.example",
			dto.ChannelOtherSettings{},
		)
		c.Request.Header.Set(responsesInputItemSoftLimitHeaderForTest, "900")
		t.Cleanup(func() {
			common.CleanupBodyStorage(c)
		})

		_, firstBody, firstCloser, apiErr := PrepareResponsesRequest(c, info, request)

		require.Nil(t, apiErr)
		require.NotNil(t, firstBody)
		require.NotNil(t, firstCloser)
		got, err := io.ReadAll(firstBody)
		require.NoError(t, err)
		require.NoError(t, firstCloser.Close())
		assert.Equal(t, rawBody, got)
		assert.Empty(t, c.Request.Header.Get(responsesInputItemSoftLimitHeaderForTest))

		common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeVolcEngine)
		common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://ark.cn-beijing.volces.com/api/v3")
		adaptor, secondBody, secondCloser, apiErr := PrepareResponsesRequest(c, info, request)
		if secondCloser != nil {
			t.Cleanup(func() {
				assert.NoError(t, secondCloser.Close())
			})
		}

		assert.Nil(t, adaptor)
		assert.Nil(t, secondBody)
		assert.Nil(t, secondCloser)
		requireResponsesInputItemLimitError(t, apiErr, 901, 900)
		assert.Empty(t, c.Request.Header.Get(responsesInputItemSoftLimitHeaderForTest))
	})

	for _, tt := range []struct {
		name        string
		channelType int
		baseURL     string
		other       dto.ChannelOtherSettings
	}{
		{
			name:        "non-Ark Responses URL is not guarded",
			channelType: constant.ChannelTypeVolcEngine,
			baseURL:     "https://fallback.example",
		},
		{
			name:        "Ark chat completions route is not guarded",
			channelType: constant.ChannelTypeAdvancedCustom,
			baseURL:     "https://ark.cn-beijing.volces.com/api/v3",
			other: dto.ChannelOtherSettings{
				AdvancedCustom: &dto.AdvancedCustomConfig{
					Routes: []dto.AdvancedCustomRoute{{
						IncomingPath: "/v1/responses",
						UpstreamPath: "/chat/completions",
						Converter:    relayconvert.ConverterOpenAIResponsesToOpenAIChat,
					}},
				},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := responsesInputItems(1001)
			rawBody := responsesInputRequestBody(input)
			c, info, request := newResponsesInputItemLimitContext(
				t,
				input,
				bytes.NewReader(rawBody),
				tt.channelType,
				tt.baseURL,
				tt.other,
			)
			c.Request.Header.Set(responsesInputItemSoftLimitHeaderForTest, "900")
			t.Cleanup(func() {
				common.CleanupBodyStorage(c)
			})

			_, body, closer, apiErr := PrepareResponsesRequest(c, info, request)

			require.Nil(t, apiErr)
			require.NotNil(t, body)
			require.NotNil(t, closer)
			defer closer.Close()
			got, err := io.ReadAll(body)
			require.NoError(t, err)
			assert.Equal(t, rawBody, got)
			assert.Empty(t, c.Request.Header.Get(responsesInputItemSoftLimitHeaderForTest))
		})
	}
}

func responsesInputItems(count int) json.RawMessage {
	items := make([]map[string]any, count)
	for index := range items {
		items[index] = map[string]any{
			"type":    "message",
			"role":    "user",
			"content": fmt.Sprintf("item-%d", index),
		}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	return raw
}

func responsesInputRequestBody(input json.RawMessage) []byte {
	return []byte(fmt.Sprintf("{\n  \"model\": \"glm-5.3\",\n  \"input\": %s\n}\n", input))
}

func requireResponsesInputItemLimitError(t *testing.T, apiErr *types.NewAPIError, count int, limit int) {
	t.Helper()
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal(t, types.ErrorTypeOpenAIError, apiErr.GetErrorType())
	assert.True(t, types.IsSkipRetryError(apiErr))
	assert.Equal(t, types.OpenAIError{
		Message: fmt.Sprintf(
			"Responses input contains %d items; the configured maximum is %d. Compact the conversation and retry.",
			count,
			limit,
		),
		Type:  "invalid_request_error",
		Param: "input",
		Code:  "context_length_exceeded",
	}, apiErr.ToOpenAIError())
}

func newResponsesInputItemLimitContext(
	t *testing.T,
	input json.RawMessage,
	body io.Reader,
	channelType int,
	baseURL string,
	other dto.ChannelOtherSettings,
) (*gin.Context, *relaycommon.RelayInfo, *dto.OpenAIResponsesRequest) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "glm-5.3")
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, baseURL)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{
		PassThroughBodyEnabled: true,
	})
	if other.AdvancedCustom != nil {
		common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, other)
	}
	request := &dto.OpenAIResponsesRequest{
		Model: "glm-5.3",
		Input: input,
	}
	return c, relaycommon.GenRelayInfoResponses(c, request), request
}

type responsesInputFailReader struct {
	read bool
}

func (r *responsesInputFailReader) Read([]byte) (int, error) {
	r.read = true
	return 0, errors.New("request body must not be read")
}
