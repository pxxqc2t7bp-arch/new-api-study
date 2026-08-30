package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestThreeDRoutesAreRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetThreeDRouter(engine)

	tests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/3d/generations", `{"model":"hyper3d-gen2-260112","prompt":"crate"}`},
		{http.MethodGet, "/v1/3d/generations/task_test", ""},
		{http.MethodDelete, "/v1/3d/generations/task_test", ""},
		{http.MethodPost, "/api/v3/contents/generations/tasks", `{"model":"hyper3d-gen2-260112","content":[{"type":"text","text":"crate"}]}`},
		{http.MethodGet, "/api/v3/contents/generations/tasks/task_test", ""},
		{http.MethodDelete, "/api/v3/contents/generations/tasks/task_test", ""},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()

		engine.ServeHTTP(recorder, request)

		assert.NotEqual(t, http.StatusNotFound, recorder.Code, "%s %s", test.method, test.path)
	}
}
