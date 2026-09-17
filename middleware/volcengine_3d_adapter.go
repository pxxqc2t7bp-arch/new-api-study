package middleware

import (
	"bytes"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

func VolcEngine3DRequestConvert() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("three_d_native_response", true)
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		var request relaycommon.TaskSubmitReq
		if err := common.UnmarshalBodyReusable(c, &request); err != nil {
			abortWithOpenAiMessage(c, http.StatusBadRequest, "invalid 3D generation request")
			return
		}
		// Upstream callbacks expose the provider task ID and bypass NewAPI
		// ownership checks. NewAPI polling is the authoritative status path.
		request.CallbackURL = ""
		body, err := common.Marshal(request)
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusInternalServerError, "failed to normalize 3D generation request")
			return
		}
		common.CleanupBodyStorage(c)
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		c.Set(common.KeyRequestBody, body)
		c.Next()
	}
}
