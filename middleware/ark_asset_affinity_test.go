package middleware

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	channelconstraint "github.com/QuantumNous/new-api/dto"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainsArkAssetReference(t *testing.T) {
	tests := []struct {
		name string
		body any
		want bool
	}{
		{name: "top level image", body: map[string]any{"image": "asset://image-1"}, want: true},
		{name: "image list", body: map[string]any{"images": []any{"https://example.com/a.png", " Asset://image-2 "}}, want: true},
		{
			name: "nested image url object",
			body: map[string]any{"content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "asset://image-3"}},
			}},
			want: true,
		},
		{
			name: "metadata video url",
			body: map[string]any{"metadata": map[string]any{"content": []any{
				map[string]any{"type": "video_url", "video_url": map[string]any{"url": "asset://video-1"}},
			}}},
			want: true,
		},
		{
			name: "responses input image",
			body: map[string]any{"input": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_image", "image_url": "asset://image-4"},
				}},
			}},
			want: true,
		},
		{name: "prompt mention is not a reference", body: map[string]any{"prompt": "Explain asset://image-5 without loading it."}},
		{
			name: "text content mention is not a reference",
			body: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "asset://image-6"},
			}},
		},
		{
			name: "ordinary references",
			body: map[string]any{
				"image":           "data:image/png;base64,AAAA",
				"input_reference": "https://example.com/video.mp4",
			},
		},
		{
			name: "draft task stays on origin pin path",
			body: map[string]any{"content": []any{
				map[string]any{"type": "draft_task", "draft_task": map[string]any{"id": "task-public"}},
			}},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, containsArkAssetReference(testCase.body))
		})
	}
}

func TestAppendArkAssetAffinityFilterKeepsBodyReusable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"doubao-seedance-2-5-260628","images":["asset://image-1"]}`)
	request := httptest.NewRequest("POST", "/v1/videos", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = request
	t.Cleanup(func() { common.CleanupBodyStorage(context) })

	constraints := &channelconstraint.ChannelConstraints{}
	require.NoError(t, appendArkAssetAffinityFilter(context, constraints))
	require.Len(t, constraints.Filters, 1)
	assert.Equal(t, channelconstraint.FilterRoutingAccount, constraints.Filters[0].Kind)
	assert.Equal(t, kitdto.RoutingAccountCXY, constraints.Filters[0].RoutingAccount)

	var replayed map[string]any
	require.NoError(t, common.UnmarshalBodyReusable(context, &replayed))
	assert.Equal(t, "doubao-seedance-2-5-260628", replayed["model"])
}

func TestArkAssetAffinityErrorCode(t *testing.T) {
	constraints := &channelconstraint.ChannelConstraints{}
	assert.Equal(t, types.ErrorCodeModelNotFound, noAvailableChannelErrorCode(constraints))

	constraints.AddFilter(channelconstraint.ChannelFilter{
		Kind:           channelconstraint.FilterRoutingAccount,
		RoutingAccount: kitdto.RoutingAccountCXY,
	})
	assert.Equal(t, types.ErrorCodeArkAssetAccountUnavailable, noAvailableChannelErrorCode(constraints))
	assert.Equal(t, types.ErrorCodeArkAssetAccountUnavailable, channelFilterErrorCode(channelconstraint.FilterRoutingAccount))
}
