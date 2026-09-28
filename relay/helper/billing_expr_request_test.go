package helper

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func imageRequestScalar(t *testing.T, input billingexpr.RequestInput, fieldName string) float64 {
	t.Helper()
	switch fieldName {
	case "ImagesUpTo1_5K":
		require.NotNil(t, input.ImagesUpTo1_5K)
		return *input.ImagesUpTo1_5K
	case "ImagesAbove1_5K":
		require.NotNil(t, input.ImagesAbove1_5K)
		return *input.ImagesAbove1_5K
	case "InputImages":
		require.NotNil(t, input.InputImages)
		return *input.InputImages
	default:
		t.Fatalf("unknown request scalar field %s", fieldName)
		return 0
	}
}

func TestResolveIncomingBillingExprRequestInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("Content-Type", "application/json")

	body := []byte(`{"service_tier":"fast"}`)
	ctx.Request.Body = io.NopCloser(bytes.NewReader(body))
	ctx.Set(common.KeyRequestBody, body)

	info := &relaycommon.RelayInfo{
		RequestHeaders: map[string]string{"Content-Type": "application/json"},
	}

	input, err := ResolveIncomingBillingExprRequestInput(ctx, info)
	require.NoError(t, err)
	require.Equal(t, body, input.Body)
	require.Equal(t, "application/json", input.Headers["Content-Type"])
}

func TestBuildBillingExprRequestInputFromRequest(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model:  "gemini-3.1-pro-preview",
		Stream: lo.ToPtr(true),
		Messages: []dto.Message{
			{
				Role:    "user",
				Content: "hi",
			},
		},
		MaxTokens: lo.ToPtr(uint(3000)),
	}

	input, err := BuildBillingExprRequestInputFromRequest(request, map[string]string{
		"Content-Type": "application/json",
		"X-Test":       "1",
	})
	require.NoError(t, err)
	require.Equal(t, "application/json", input.Headers["Content-Type"])
	require.Equal(t, "1", input.Headers["X-Test"])
	require.True(t, gjson.GetBytes(input.Body, "stream").Bool())
	require.Equal(t, "user", gjson.GetBytes(input.Body, "messages.0.role").String())
	require.Equal(t, float64(3000), gjson.GetBytes(input.Body, "max_tokens").Float())
}

func TestBuildBillingExprRequestInputFromRequestSanitizesSeedreamImages(t *testing.T) {
	request := &dto.ImageRequest{}
	require.NoError(t, common.Unmarshal([]byte(`{
		"model":"doubao-seedream-5-0-pro-260628",
		"prompt":"secret prompt",
		"size":"1K",
		"image":"data:image/png;base64,c2VjcmV0"
	}`), request))

	input, err := BuildBillingExprRequestInputFromRequest(request, map[string]string{"X-Test": "1"})
	require.NoError(t, err)
	require.Empty(t, input.Body)
	assert.Equal(t, "1", input.Headers["X-Test"])
	assert.Equal(t, float64(1), imageRequestScalar(t, input, "ImagesUpTo1_5K"))
	assert.Equal(t, float64(0), imageRequestScalar(t, input, "ImagesAbove1_5K"))
	assert.Equal(t, float64(1), imageRequestScalar(t, input, "InputImages"))
}

func TestResolveImageBillingRequestInputDerivesOnlyBoundedSeedreamScalars(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantLower  float64
		wantHigher float64
		wantInputs float64
		wantError  string
	}{
		{name: "default size is 2K", body: `{"model":"doubao-seedream-5-0-pro-260628","prompt":"secret prompt"}`, wantHigher: 1},
		{name: "1K is lower tier", body: `{"model":"doubao-seedream-5-0-pro-260628","size":"1K"}`, wantLower: 1},
		{name: "1.5K is lower tier", body: `{"model":"doubao-seedream-5-0-pro-260628","size":"1.5k"}`, wantLower: 1},
		{name: "2K is higher tier", body: `{"model":"doubao-seedream-5-0-pro-260628","size":"2K"}`, wantHigher: 1},
		{name: "pixel boundary is lower tier", body: `{"model":"doubao-seedream-5-0-pro-260628","size":"1500x1740"}`, wantLower: 1},
		{name: "pixel above boundary is higher tier", body: `{"model":"doubao-seedream-5-0-pro-260628","size":"1501*1740"}`, wantHigher: 1},
		{name: "multiple outputs stay in one tier", body: `{"model":"doubao-seedream-5-0-pro-260628","size":"1K","n":3}`, wantLower: 3},
		{name: "reference array stores only its count", body: `{"model":"doubao-seedream-5-0-pro-260628","size":"1K","image":["https://secret.example/a.png","data:image/png;base64,c2VjcmV0"]}`, wantLower: 1, wantInputs: 2},
		{name: "layer decomposition reserves seventeen lower outputs", body: `{"model":"doubao-seedream-5-0-pro-260628","size":"1K","image":"https://secret.example/a.png","layer_decomposition":true}`, wantLower: 17, wantInputs: 1},
		{name: "layer decomposition default is conservative", body: `{"model":"doubao-seedream-5-0-pro-260628","image":"https://secret.example/a.png","layer_decomposition":true}`, wantHigher: 17, wantInputs: 1},
		{name: "unknown size is conservative", body: `{"model":"doubao-seedream-5-0-pro-260628","size":"provider-auto"}`, wantHigher: 1},
		{name: "pro output count follows profile limit", body: `{"model":"doubao-seedream-5-0-pro-260628","n":16}`, wantError: "between 1 and 15"},
		{name: "pro references follow profile limit", body: `{"model":"doubao-seedream-5-0-pro-260628","image":["1","2","3","4","5","6","7","8","9","10","11"]}`, wantError: "at most 10"},
		{name: "ordinary references follow provider ceiling", body: `{"model":"doubao-seedream-4-0-20260415","image":["1","2","3","4","5","6","7","8","9","10","11","12","13","14","15"]}`, wantError: "at most 14"},
		{name: "layer decomposition requires one reference", body: `{"model":"doubao-seedream-5-0-pro-260628","layer_decomposition":true}`, wantError: "exactly one"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request := &dto.ImageRequest{}
			require.NoError(t, common.Unmarshal([]byte(testCase.body), request))
			input, err := ResolveImageBillingRequestInput(
				nil,
				&relaycommon.RelayInfo{Request: request},
				billingexpr.RequestInput{Body: []byte(testCase.body)},
			)
			if testCase.wantError != "" {
				require.ErrorContains(t, err, testCase.wantError)
				return
			}
			require.NoError(t, err)
			require.Empty(t, input.Body, "image billing snapshots retain only scalar counts")
			assert.Equal(t, testCase.wantLower, imageRequestScalar(t, input, "ImagesUpTo1_5K"))
			assert.Equal(t, testCase.wantHigher, imageRequestScalar(t, input, "ImagesAbove1_5K"))
			assert.Equal(t, testCase.wantInputs, imageRequestScalar(t, input, "InputImages"))
			encoded, marshalErr := common.Marshal(input)
			require.NoError(t, marshalErr)
			assert.NotContains(t, string(encoded), "secret")
			assert.NotContains(t, string(encoded), "base64")
			assert.NotContains(t, string(encoded), "prompt")
		})
	}
}

func TestResolveIncomingBillingExprRequestInputClonesSeedreamScalars(t *testing.T) {
	request := &dto.ImageRequest{}
	require.NoError(t, common.Unmarshal([]byte(`{"model":"doubao-seedream-5-0-pro-260628","size":"1K"}`), request))
	input, err := ResolveImageBillingRequestInput(nil, &relaycommon.RelayInfo{Request: request}, billingexpr.RequestInput{})
	require.NoError(t, err)

	cloned, err := ResolveIncomingBillingExprRequestInput(nil, &relaycommon.RelayInfo{BillingRequestInput: &input})
	require.NoError(t, err)
	require.NotNil(t, input.ImagesUpTo1_5K)
	*input.ImagesUpTo1_5K = 9
	assert.Equal(t, float64(1), imageRequestScalar(t, cloned, "ImagesUpTo1_5K"))
}

func TestResolveIncomingBillingExprRequestInputFreezesSeedreamScalars(t *testing.T) {
	request := &dto.ImageRequest{}
	require.NoError(t, common.Unmarshal([]byte(`{
		"model":"doubao-seedream-5-0-pro-260628",
		"prompt":"secret prompt",
		"size":"2K",
		"image":["https://secret.example/a.png","https://secret.example/b.png"]
	}`), request))
	input, err := ResolveIncomingBillingExprRequestInput(nil, &relaycommon.RelayInfo{Request: request})
	require.NoError(t, err)
	require.Empty(t, input.Body)

	request.Size = "1K"
	request.Image = nil
	cost, trace, err := billingexpr.RunExprWithRequest(
		`tier("seedream", fixed(images_up_to_1_5k * 0.1 + images_above_1_5k * 0.2 + max(input_images - 1, 0) * 0.01))`,
		billingexpr.TokenParams{},
		input,
	)
	require.NoError(t, err)
	assert.Equal(t, billingexpr.BillingUnitRequest, trace.BillingUnit)
	assert.InDelta(t, 210000, cost, 1e-9)
}

func TestResolveOutboundImageBillingJSONPreservesOriginIdentity(t *testing.T) {
	info := &relaycommon.RelayInfo{
		Request: &dto.ImageRequest{Model: "billing-origin-image"},
	}
	resolved, err := ResolveOutboundImageBillingJSON(info, []byte(`{
		"model":"provider-image-alias",
		"prompt":"secret prompt",
		"n":2,
		"size":"1024x1024",
		"quality":"hd",
		"parameters":{"n":3}
	}`))
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, "billing-origin-image", resolved.Request.Model)
	assert.Equal(t, 2, resolved.Count)
	assert.Equal(t, "billing-origin-image", gjson.GetBytes(resolved.Input.Body, "model").String())
	assert.Equal(t, int64(2), gjson.GetBytes(resolved.Input.Body, "n").Int())
	assert.Equal(t, int64(3), gjson.GetBytes(resolved.Input.Body, "parameters.n").Int())
	assert.NotContains(t, string(resolved.Input.Body), "secret")
}

func TestResolveOutboundImageBillingMultipartKeepsOnlySeedreamScalars(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "provider-seedream-alias"))
	require.NoError(t, writer.WriteField("prompt", "secret multipart prompt"))
	require.NoError(t, writer.WriteField("n", "2"))
	require.NoError(t, writer.WriteField("size", "1K"))
	image, err := writer.CreateFormFile("image", "secret.png")
	require.NoError(t, err)
	_, err = image.Write([]byte("secret image bytes"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	info := &relaycommon.RelayInfo{
		Request: &dto.ImageRequest{Model: "doubao-seedream-5-0-pro-260628"},
	}
	resolved, err := ResolveOutboundImageBillingMultipart(info, writer.FormDataContentType(), bytes.NewReader(body.Bytes()))
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, "doubao-seedream-5-0-pro-260628", resolved.Request.Model)
	assert.Equal(t, 2, resolved.Count)
	assert.Equal(t, float64(2), imageRequestScalar(t, resolved.Input, "ImagesUpTo1_5K"))
	assert.Equal(t, float64(0), imageRequestScalar(t, resolved.Input, "ImagesAbove1_5K"))
	assert.Equal(t, float64(1), imageRequestScalar(t, resolved.Input, "InputImages"))
	encoded, err := common.Marshal(resolved)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "secret")
	assert.NotContains(t, string(encoded), "image bytes")
}

func TestResolveOutboundImageBillingMultipartRejectsExplicitZero(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "provider-seedream-alias"))
	require.NoError(t, writer.WriteField("n", "0"))
	require.NoError(t, writer.Close())

	info := &relaycommon.RelayInfo{
		Request: &dto.ImageRequest{Model: "doubao-seedream-5-0-pro-260628"},
	}
	_, err := ResolveOutboundImageBillingMultipart(info, writer.FormDataContentType(), bytes.NewReader(body.Bytes()))
	require.ErrorContains(t, err, "n must be an integer between 1 and 15")
}
