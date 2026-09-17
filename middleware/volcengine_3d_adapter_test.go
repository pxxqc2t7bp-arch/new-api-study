package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolcEngine3DRequestConvertStripsCallbackAndPreservesContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(VolcEngine3DRequestConvert())
	engine.POST("/api/v3/contents/generations/tasks", func(c *gin.Context) {
		var request relaycommon.TaskSubmitReq
		require.NoError(t, common.UnmarshalBodyReusable(c, &request))
		assert.Empty(t, request.CallbackURL)
		assert.Len(t, request.Content, 2)
		assert.True(t, c.GetBool("three_d_native_response"))
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v3/contents/generations/tasks",
		strings.NewReader(`{
			"model":"hyper3d-gen2-260112",
			"callback_url":"https://callback.example",
			"content":[
				{"type":"text","text":"crate"},
				{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}
			]
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusNoContent, recorder.Code)
}
