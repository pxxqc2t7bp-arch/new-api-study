package volcengine3d

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

var ModelList = []string{
	"doubao-seed3d-2-0-260328",
	"hitem3d-2-0-251223",
	"hyper3d-gen2-260112",
}

const ChannelName = "volcengine-3d"

type requestPayload struct {
	Model   string           `json:"model"`
	Content []map[string]any `json:"content"`
	Seed    *int             `json:"seed,omitempty"`
}

type responsePayload struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	Content   struct {
		FileURL string `json:"file_url"`
	} `json:"content"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type TaskAdaptor struct {
	taskcommon.BaseBilling
	apiKey  string
	baseURL string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.apiKey = info.ApiKey
	a.baseURL = info.ChannelBaseUrl
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *taskdto.TaskError {
	var request relaycommon.TaskSubmitReq
	if err := common.UnmarshalBodyReusable(c, &request); err != nil {
		return taskError(err, "invalid_request", http.StatusBadRequest)
	}
	if request.Model == "" {
		return taskError(fmt.Errorf("model is required"), "missing_model", http.StatusBadRequest)
	}
	images := collectImages(&request)
	prompt := collectPrompt(&request)
	switch request.Model {
	case "doubao-seed3d-2-0-260328":
		if len(images) != 1 {
			return taskError(fmt.Errorf("Seed3D requires exactly one image"), "invalid_images", http.StatusBadRequest)
		}
	case "hitem3d-2-0-251223":
		if len(images) < 1 || len(images) > 4 {
			return taskError(fmt.Errorf("Hitem3D requires 1 to 4 images"), "invalid_images", http.StatusBadRequest)
		}
	case "hyper3d-gen2-260112":
		if len(images) > 5 {
			return taskError(fmt.Errorf("Hyper3D accepts at most 5 images"), "invalid_images", http.StatusBadRequest)
		}
		if len(images) == 0 && strings.TrimSpace(prompt) == "" {
			return taskError(fmt.Errorf("Hyper3D requires a prompt or at least one image"), "invalid_input", http.StatusBadRequest)
		}
	default:
		return taskError(fmt.Errorf("unsupported 3D model: %s", request.Model), "unsupported_model", http.StatusBadRequest)
	}
	if err := validateParameters(request.Model, request.Parameters); err != nil {
		return taskError(err, "invalid_parameters", http.StatusBadRequest)
	}
	if request.Seed != nil && (*request.Seed < 0 || *request.Seed > 65535) {
		return taskError(fmt.Errorf("seed must be between 0 and 65535"), "invalid_seed", http.StatusBadRequest)
	}
	if request.Seed != nil && request.Model != "hyper3d-gen2-260112" {
		return taskError(fmt.Errorf("seed is only supported by Hyper3D"), "invalid_seed", http.StatusBadRequest)
	}
	request.Images = images
	request.Prompt = prompt
	request.CallbackURL = ""
	relaycommon.StoreTaskRequest(c, info, constant.TaskActionThreeDGenerate, request)
	return nil
}

func (a *TaskAdaptor) BuildRequestURL(_ *relaycommon.RelayInfo) (string, error) {
	return strings.TrimRight(a.baseURL, "/") + "/api/v3/contents/generations/tasks", nil
}

func (a *TaskAdaptor) BuildRequestHeader(_ *gin.Context, req *http.Request, _ *relaycommon.RelayInfo) error {
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	request, err := relaycommon.GetTaskRequest(c)
	if err != nil {
		return nil, err
	}
	modelName := request.Model
	if info.IsModelMapped {
		modelName = info.UpstreamModelName
	} else {
		info.UpstreamModelName = modelName
	}
	content := append([]map[string]any(nil), request.Content...)
	if len(content) == 0 {
		for _, image := range request.Images {
			content = append(content, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": image},
			})
		}
		if text := buildTextContent(request.Prompt, request.Parameters); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
	}
	body, err := common.Marshal(requestPayload{
		Model:   modelName,
		Content: content,
		Seed:    request.Seed,
	})
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(body), nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, body)
}

func (a *TaskAdaptor) ParseResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*channel.TaskSubmitResponse, *taskdto.TaskError) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, upstreamTaskError(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	var upstream responsePayload
	if err := common.Unmarshal(body, &upstream); err != nil {
		return nil, upstreamTaskError(errors.Wrapf(err, "body: %s", body), "unmarshal_response_body_failed", http.StatusInternalServerError)
	}
	if upstream.ID == "" {
		return nil, upstreamTaskError(fmt.Errorf("task_id is empty"), "invalid_response", http.StatusInternalServerError)
	}
	var clientResponse any
	if c.GetBool("three_d_native_response") {
		clientResponse = gin.H{"id": info.PublicTaskID}
	} else {
		clientResponse = taskdto.ThreeDGeneration{
			ID:        info.PublicTaskID,
			Object:    "3d.generation",
			Model:     info.OriginModelName,
			Status:    "queued",
			CreatedAt: time.Now().Unix(),
		}
	}
	return &channel.TaskSubmitResponse{
		UpstreamTaskID: upstream.ID,
		TaskData:       body,
		ClientResponse: clientResponse,
	}, nil
}

func (a *TaskAdaptor) EstimateBilling(_ *gin.Context, info *relaycommon.RelayInfo) map[string]float64 {
	switch info.OriginModelName {
	case "hitem3d-2-0-251223":
		return map[string]float64{"preconsume_tokens": 0.65}
	case "doubao-seed3d-2-0-260328", "hyper3d-gen2-260112":
		return map[string]float64{"preconsume_tokens": 0.12}
	default:
		return nil
	}
}

func (a *TaskAdaptor) AdjustBillingOnComplete(task *model.Task, result *relaycommon.TaskInfo) int {
	if task == nil || result == nil || result.TotalTokens <= 0 ||
		task.PrivateData.BillingContext == nil {
		return 0
	}
	billing := task.PrivateData.BillingContext
	quota, _ := common.QuotaFromFloatChecked(
		float64(result.TotalTokens) * billing.ModelRatio * billing.GroupRatio,
	)
	return quota
}

func (a *TaskAdaptor) FetchTask(baseURL, key string, body map[string]any, proxy string) (*http.Response, error) {
	taskID, ok := body["task_id"].(string)
	if !ok || taskID == "" {
		return nil, fmt.Errorf("invalid task_id")
	}
	return doTaskRequest(http.MethodGet, baseURL, key, taskID, proxy)
}

func (a *TaskAdaptor) CancelTask(baseURL, key, taskID, proxy string) (*http.Response, error) {
	if taskID == "" {
		return nil, fmt.Errorf("invalid task_id")
	}
	return doTaskRequest(http.MethodDelete, baseURL, key, taskID, proxy)
}

func doTaskRequest(method, baseURL, key, taskID, proxy string) (*http.Response, error) {
	url := fmt.Sprintf("%s/api/v3/contents/generations/tasks/%s", strings.TrimRight(baseURL, "/"), taskID)
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

func (a *TaskAdaptor) ParseTaskResult(body []byte) (*relaycommon.TaskInfo, error) {
	var response responsePayload
	if err := common.Unmarshal(body, &response); err != nil {
		return nil, errors.Wrap(err, "unmarshal 3D task result failed")
	}
	result := &relaycommon.TaskInfo{Code: 0, TaskID: response.ID}
	switch response.Status {
	case "pending", "queued":
		result.Status = model.TaskStatusQueued
		result.Progress = taskcommon.ProgressQueued
	case "processing", "running":
		result.Status = model.TaskStatusInProgress
		result.Progress = taskcommon.ProgressInProgress
	case "succeeded":
		result.Status = model.TaskStatusSuccess
		result.Progress = taskcommon.ProgressComplete
		result.Url = response.Content.FileURL
		result.CompletionTokens = response.Usage.CompletionTokens
		result.TotalTokens = response.Usage.TotalTokens
	case "cancelled", "canceled":
		result.Status = model.TaskStatusCancelled
		result.Progress = taskcommon.ProgressComplete
		result.Reason = "cancelled"
	case "failed":
		result.Status = model.TaskStatusFailure
		result.Progress = taskcommon.ProgressComplete
		result.Reason = response.Error.Message
	default:
		result.Status = model.TaskStatusInProgress
		result.Progress = "30%"
	}
	return result, nil
}

func (a *TaskAdaptor) GetModelList() []string {
	return ModelList
}

func (a *TaskAdaptor) GetChannelName() string {
	return ChannelName
}

func taskError(err error, code string, status int) *taskdto.TaskError {
	return &taskdto.TaskError{
		Code: code, Message: err.Error(), StatusCode: status, LocalError: true, Error: err,
	}
}

func upstreamTaskError(err error, code string, status int) *taskdto.TaskError {
	return &taskdto.TaskError{
		Code: code, Message: err.Error(), StatusCode: status, LocalError: false, Error: err,
	}
}

func collectImages(request *relaycommon.TaskSubmitReq) []string {
	images := append([]string(nil), request.Images...)
	if image := strings.TrimSpace(request.Image); image != "" {
		images = append(images, image)
	}
	for _, item := range request.Content {
		if item["type"] != "image_url" {
			continue
		}
		image, _ := item["image_url"].(map[string]any)
		if url, _ := image["url"].(string); strings.TrimSpace(url) != "" {
			images = append(images, strings.TrimSpace(url))
		}
	}
	return images
}

func collectPrompt(request *relaycommon.TaskSubmitReq) string {
	if strings.TrimSpace(request.Prompt) != "" {
		return strings.TrimSpace(request.Prompt)
	}
	for _, item := range request.Content {
		if item["type"] == "text" {
			if text, _ := item["text"].(string); strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func buildTextContent(prompt string, parameters map[string]any) string {
	parts := []string{}
	if strings.TrimSpace(prompt) != "" {
		parts = append(parts, strings.TrimSpace(prompt))
	}
	keys := make([]string, 0, len(parameters))
	for key := range parameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := fmt.Sprint(parameters[key])
		switch parameters[key].(type) {
		case []any, map[string]any:
			if encoded, err := common.Marshal(parameters[key]); err == nil {
				value = string(encoded)
			}
		}
		parts = append(parts, fmt.Sprintf("--%s %s", key, value))
	}
	return strings.Join(parts, " ")
}

func validateParameters(modelName string, parameters map[string]any) error {
	if len(parameters) == 0 {
		return nil
	}
	allowedByModel := map[string]map[string]struct{}{
		"doubao-seed3d-2-0-260328": {
			"fileformat": {}, "ff": {}, "subdivisionlevel": {}, "sl": {},
		},
		"hitem3d-2-0-251223": {
			"fileformat": {}, "ff": {}, "resolution": {}, "request_type": {},
			"face": {}, "multi_images_bit": {},
		},
		"hyper3d-gen2-260112": {
			"fileformat": {}, "ff": {}, "subdivisionlevel": {}, "sl": {},
			"material": {}, "mesh_mode": {}, "hd_texture": {}, "addons": {},
			"quality_override": {}, "use_original_alpha": {}, "bbox_condition": {},
			"TAPose": {},
		},
	}
	allowed := allowedByModel[modelName]
	for key := range parameters {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("parameter %s is not supported by %s", key, modelName)
		}
	}
	if value, ok := stringParameter(parameters, "subdivisionlevel", "sl"); ok &&
		!oneOf(value, "low", "medium", "high") {
		return fmt.Errorf("subdivisionlevel must be low, medium, or high")
	}
	if value, ok := stringParameter(parameters, "material"); ok &&
		!oneOf(value, "PBR", "Shaded", "All", "None") {
		return fmt.Errorf("material is invalid")
	}
	if value, ok := stringParameter(parameters, "mesh_mode"); ok &&
		!oneOf(value, "Raw", "Quad") {
		return fmt.Errorf("mesh_mode must be Raw or Quad")
	}
	if value, ok := intParameter(parameters, "face"); ok && (value < 100000 || value > 2000000) {
		return fmt.Errorf("face must be between 100000 and 2000000")
	}
	if value, ok := intParameter(parameters, "request_type"); ok && value != 1 && value != 3 {
		return fmt.Errorf("request_type must be 1 or 3")
	}
	if value, ok := stringParameter(parameters, "resolution"); ok &&
		!oneOf(value, "1536", "1536pro") {
		return fmt.Errorf("resolution must be 1536 or 1536pro")
	}
	if value, ok := stringParameter(parameters, "fileformat", "ff"); ok {
		switch modelName {
		case "doubao-seed3d-2-0-260328":
			if !oneOf(value, "glb", "obj", "usd", "usdz") {
				return fmt.Errorf("fileformat is invalid for Seed3D")
			}
		case "hyper3d-gen2-260112":
			if !oneOf(value, "glb", "obj", "usdz", "fbx", "stl") {
				return fmt.Errorf("fileformat is invalid for Hyper3D")
			}
		case "hitem3d-2-0-251223":
			return fmt.Errorf("Hitem3D fileformat must be an integer between 1 and 5")
		}
	}
	if modelName == "hitem3d-2-0-251223" {
		if value, ok := intParameter(parameters, "fileformat"); ok && (value < 1 || value > 5) {
			return fmt.Errorf("fileformat must be between 1 and 5")
		}
		if value, ok := intParameter(parameters, "ff"); ok && (value < 1 || value > 5) {
			return fmt.Errorf("ff must be between 1 and 5")
		}
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func stringParameter(parameters map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := parameters[key]; ok {
			text, ok := value.(string)
			return text, ok
		}
	}
	return "", false
}

func intParameter(parameters map[string]any, key string) (int, bool) {
	switch value := parameters[key].(type) {
	case int:
		return value, true
	case float64:
		return int(value), true
	default:
		return 0, false
	}
}
