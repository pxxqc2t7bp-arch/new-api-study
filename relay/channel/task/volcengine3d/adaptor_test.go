package volcengine3d

import (
	"encoding/csv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func threeDContext(t *testing.T, body string) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/3d/generations", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	return ctx, &relaycommon.RelayInfo{
		TaskRelayInfo: &relaycommon.TaskRelayInfo{},
		ChannelMeta:   &relaycommon.ChannelMeta{},
	}
}

func TestValidateThreeDRequests(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{
			name: "Seed3D one image",
			body: `{"model":"doubao-seed3d-2-0-260328","image":"https://example.com/a.png"}`,
		},
		{
			name:      "Seed3D rejects text only",
			body:      `{"model":"doubao-seed3d-2-0-260328","prompt":"crate"}`,
			wantError: "exactly one image",
		},
		{
			name: "Hitem accepts four images",
			body: `{"model":"hitem3d-2-0-251223","images":["a","b","c","d"]}`,
		},
		{
			name:      "Hitem rejects five images",
			body:      `{"model":"hitem3d-2-0-251223","images":["a","b","c","d","e"]}`,
			wantError: "1 to 4 images",
		},
		{
			name: "Hyper accepts prompt",
			body: `{"model":"hyper3d-gen2-260112","prompt":"a wooden crate"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, info := threeDContext(t, test.body)
			result := (&TaskAdaptor{}).ValidateRequestAndSetAction(ctx, info)
			if test.wantError == "" {
				require.Nil(t, result)
			} else {
				require.NotNil(t, result)
				assert.Contains(t, result.Message, test.wantError)
			}
		})
	}
}

func TestBuildThreeDRequestBody(t *testing.T) {
	ctx, info := threeDContext(t, `{
		"model":"hyper3d-gen2-260112",
		"prompt":"a crate",
		"image":"https://example.com/a.png",
		"parameters":{"fileformat":"glb","mesh_mode":"Raw"},
		"seed":42
	}`)
	adaptor := &TaskAdaptor{}
	require.Nil(t, adaptor.ValidateRequestAndSetAction(ctx, info))

	body, err := adaptor.BuildRequestBody(ctx, info)
	require.NoError(t, err)
	encoded, err := io.ReadAll(body)
	require.NoError(t, err)
	var payload requestPayload
	require.NoError(t, common.Unmarshal(encoded, &payload))
	assert.Equal(t, "hyper3d-gen2-260112", payload.Model)
	assert.Equal(t, 42, *payload.Seed)
	require.Len(t, payload.Content, 2)
	assert.Equal(t, "image_url", payload.Content[0]["type"])
	assert.Contains(t, payload.Content[1]["text"], "--fileformat glb")
	assert.Contains(t, payload.Content[1]["text"], "--mesh_mode Raw")
}

func TestParseThreeDTaskResult(t *testing.T) {
	result, err := (&TaskAdaptor{}).ParseTaskResult([]byte(`{
		"id":"cgt-1",
		"model":"hyper3d-gen2-260112",
		"status":"succeeded",
		"content":{"file_url":"https://example.com/model.zip"},
		"usage":{"completion_tokens":30000,"total_tokens":30000}
	}`))

	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusSuccess), result.Status)
	assert.Equal(t, "https://example.com/model.zip", result.Url)
	assert.Equal(t, 30000, result.TotalTokens)
}

func TestThreeDActualTokenBillingIgnoresPreconsumeMultiplier(t *testing.T) {
	task := &model.Task{
		PrivateData: model.TaskPrivateData{
			BillingContext: &model.TaskBillingContext{
				ModelRatio:  5,
				GroupRatio:  2,
				OtherRatios: map[string]float64{"preconsume_tokens": 0.12},
			},
		},
	}
	result := &relaycommon.TaskInfo{TotalTokens: 30000}

	quota := (&TaskAdaptor{}).AdjustBillingOnComplete(task, result)

	assert.Equal(t, 300000, quota)
}

func TestThreeDPreconsumeRatios(t *testing.T) {
	adaptor := &TaskAdaptor{}
	for modelName, expected := range map[string]float64{
		"doubao-seed3d-2-0-260328": 0.12,
		"hyper3d-gen2-260112":      0.12,
		"hitem3d-2-0-251223":       0.65,
	} {
		info := &relaycommon.RelayInfo{
			OriginModelName: modelName,
			PriceData:       hosttypes.PriceData{},
		}
		assert.Equal(t, expected, adaptor.EstimateBilling(nil, info)["preconsume_tokens"])
	}
}

func TestCancelThreeDTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/api/v3/contents/generations/tasks/cgt-1", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	response, err := (&TaskAdaptor{}).CancelTask(server.URL, "test-key", "cgt-1", "")

	require.NoError(t, err)
	require.NotNil(t, response)
	defer response.Body.Close()
	assert.Equal(t, http.StatusNoContent, response.StatusCode)
}

func TestThreeDConcurrentFetch1500(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"cgt-load",
			"model":"hyper3d-gen2-260112",
			"status":"succeeded",
			"content":{"file_url":"https://example.com/model.zip"},
			"usage":{"completion_tokens":30000,"total_tokens":30000}
		}`)
	}))
	t.Cleanup(server.Close)

	var failures atomic.Int64
	var wait sync.WaitGroup
	results := make([]string, 1500)
	wait.Add(1500)
	for index := range 1500 {
		go func(index int) {
			defer wait.Done()
			response, err := (&TaskAdaptor{}).FetchTask(
				server.URL,
				"test-key",
				map[string]any{"task_id": "cgt-load"},
				"",
			)
			if err != nil {
				failures.Add(1)
				results[index] = err.Error()
				return
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				failures.Add(1)
				results[index] = err.Error()
				return
			}
			result, err := (&TaskAdaptor{}).ParseTaskResult(body)
			if err != nil || result.TotalTokens != 30000 {
				failures.Add(1)
				if err != nil {
					results[index] = err.Error()
				} else {
					results[index] = "unexpected token count"
				}
				return
			}
			results[index] = "ok"
		}(index)
	}
	wait.Wait()

	writeLoadReport(t, results)
	assert.Zero(t, failures.Load())
}

func writeLoadReport(t *testing.T, results []string) {
	t.Helper()
	reportDir := os.Getenv("THREED_LOAD_REPORT_DIR")
	if reportDir == "" {
		return
	}
	require.NoError(t, os.MkdirAll(reportDir, 0o755))
	resultFile, err := os.Create(filepath.Join(reportDir, "results.csv"))
	require.NoError(t, err)
	defer resultFile.Close()
	failureFile, err := os.Create(filepath.Join(reportDir, "failures.csv"))
	require.NoError(t, err)
	defer failureFile.Close()
	resultWriter := csv.NewWriter(resultFile)
	failureWriter := csv.NewWriter(failureFile)
	require.NoError(t, resultWriter.Write([]string{"request_id", "status", "error"}))
	require.NoError(t, failureWriter.Write([]string{"request_id", "error"}))
	for index, result := range results {
		requestID := strconv.Itoa(index + 1)
		status := "passed"
		errorMessage := ""
		if result != "ok" {
			status = "failed"
			errorMessage = result
			require.NoError(t, failureWriter.Write([]string{requestID, errorMessage}))
		}
		require.NoError(t, resultWriter.Write([]string{requestID, status, errorMessage}))
	}
	resultWriter.Flush()
	failureWriter.Flush()
	require.NoError(t, resultWriter.Error())
	require.NoError(t, failureWriter.Error())
}
