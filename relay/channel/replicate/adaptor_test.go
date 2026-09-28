package replicate

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func replicateEditRequestContext(t *testing.T) (*gin.Context, dto.ImageRequest) {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("prompt", "edit prompt"))
	image, err := writer.CreateFormFile("image", "fixture.png")
	require.NoError(t, err)
	_, err = image.Write([]byte("fixture image"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	require.NoError(t, c.Request.ParseMultipartForm(1<<20))

	request := dto.ImageRequest{}
	require.NoError(t, common.Unmarshal([]byte(`{
		"model":"billing-model",
		"prompt":"edit prompt",
		"n":1,
		"input":{"negative_prompt":"merged negative prompt","image_prompt":"https://ignored.invalid/input.png"}
	}`), &request))
	return c, request
}

func replicateEditInfo(baseURL string) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeReplicate,
			ApiType:           constant.APITypeReplicate,
			ChannelBaseUrl:    baseURL,
			ApiKey:            "local-test-key",
			UpstreamModelName: ModelFlux11Pro,
		},
		RelayMode: relayconstant.RelayModeImagesEdits,
	}
}

func TestPrepareAndFinalizeImageRequestDefersReplicateEditUpload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	var uploads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/v1/files", request.URL.Path)
		uploads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"urls":{"get":"https://files.invalid/input.png"}}`)
	}))
	t.Cleanup(upstream.Close)

	c, request := replicateEditRequestContext(t)
	adaptor := &Adaptor{}
	info := replicateEditInfo(upstream.URL)

	prepared, err := adaptor.PrepareImageRequest(c, info, request)
	require.NoError(t, err)
	assert.Zero(t, uploads.Load(), "prepare must not perform network I/O")

	preparedJSON, err := common.Marshal(prepared)
	require.NoError(t, err)
	assert.Equal(t, "edit prompt", gjson.GetBytes(preparedJSON, "input.prompt").String())
	assert.Equal(t, "merged negative prompt", gjson.GetBytes(preparedJSON, "input.negative_prompt").String())
	assert.Equal(t, int64(1), gjson.GetBytes(preparedJSON, "input.num_outputs").Int())
	assert.False(t, gjson.GetBytes(preparedJSON, "input.image_prompt").Exists())

	finalized, err := adaptor.FinalizeImageRequest(c, info, preparedJSON)
	require.NoError(t, err)
	assert.Equal(t, int32(1), uploads.Load())

	finalizedJSON, err := common.Marshal(finalized)
	require.NoError(t, err)
	assert.Equal(t, "https://files.invalid/input.png", gjson.GetBytes(finalizedJSON, "input.image_prompt").String())
	assert.Equal(t, gjson.GetBytes(preparedJSON, "input.prompt").String(), gjson.GetBytes(finalizedJSON, "input.prompt").String())
	assert.Equal(t, gjson.GetBytes(preparedJSON, "input.negative_prompt").String(), gjson.GetBytes(finalizedJSON, "input.negative_prompt").String())
	assert.Equal(t, gjson.GetBytes(preparedJSON, "input.num_outputs").Int(), gjson.GetBytes(finalizedJSON, "input.num_outputs").Int())
}

func TestConvertImageRequestRetainsDirectCallerCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	var uploads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		uploads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"urls":{"get":"https://files.invalid/direct.png"}}`)
	}))
	t.Cleanup(upstream.Close)

	c, request := replicateEditRequestContext(t)
	converted, err := (&Adaptor{}).ConvertImageRequest(c, replicateEditInfo(upstream.URL), request)
	require.NoError(t, err)
	assert.Equal(t, int32(1), uploads.Load())

	convertedJSON, err := common.Marshal(converted)
	require.NoError(t, err)
	assert.Equal(t, "https://files.invalid/direct.png", gjson.GetBytes(convertedJSON, "input.image_prompt").String())
}

func TestFinalizeImageRequestMarksUploadTransportFailureSkipRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	var uploads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		uploads.Add(1)
		hijacker, ok := w.(http.Hijacker)
		require.True(t, ok)
		connection, _, err := hijacker.Hijack()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	}))
	t.Cleanup(upstream.Close)

	c, request := replicateEditRequestContext(t)
	adaptor := &Adaptor{}
	info := replicateEditInfo(upstream.URL)
	prepared, err := adaptor.PrepareImageRequest(c, info, request)
	require.NoError(t, err)
	preparedJSON, err := common.Marshal(prepared)
	require.NoError(t, err)

	_, err = adaptor.FinalizeImageRequest(c, info, preparedJSON)
	require.Error(t, err)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	assert.True(t, types.IsSkipRetryError(apiErr))
	assert.Equal(t, int32(1), uploads.Load())
}
