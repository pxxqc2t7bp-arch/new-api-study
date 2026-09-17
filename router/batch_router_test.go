package router

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetBatchRouterRegistersOpenAICompatibleRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NotPanics(t, func() { SetBatchRouter(engine) })

	actual := make(map[string]struct{})
	for _, route := range engine.Routes() {
		actual[route.Method+" "+route.Path] = struct{}{}
	}
	for _, route := range []string{
		http.MethodPost + " /v1/files",
		http.MethodGet + " /v1/files",
		http.MethodGet + " /v1/files/:id",
		http.MethodDelete + " /v1/files/:id",
		http.MethodGet + " /v1/files/:id/content",
		http.MethodPost + " /v1/batches",
		http.MethodGet + " /v1/batches",
		http.MethodGet + " /v1/batches/:id",
		http.MethodPost + " /v1/batches/:id/cancel",
	} {
		assert.Contains(t, actual, route)
	}
}
