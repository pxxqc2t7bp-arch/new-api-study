package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type taskHTTPErrorAdaptor struct {
	channel.TaskAdaptor
	client   *http.Client
	endpoint string
}

func (a *taskHTTPErrorAdaptor) DoRequest(
	c *gin.Context,
	_ *relaycommon.RelayInfo,
	requestBody io.Reader,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, a.endpoint, requestBody)
	if err != nil {
		return nil, err
	}
	return a.client.Do(req)
}

func submitTaskToErrorServer(t *testing.T, statusCode int, responseBody string) *taskdto.TaskError {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(server.Close)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	adaptor := &taskHTTPErrorAdaptor{
		client:   server.Client(),
		endpoint: server.URL,
	}

	result, taskErr := submitTaskUpstream(
		c,
		&relaycommon.RelayInfo{},
		adaptor,
		constant.TaskPlatform("test"),
		nil,
		0,
	)

	assert.Nil(t, result)
	require.NotNil(t, taskErr)
	return taskErr
}

func TestSubmitTaskUpstreamPreservesStructuredQuotaErrorCode(t *testing.T) {
	const quotaMessage = "You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC."
	taskErr := submitTaskToErrorServer(
		t,
		http.StatusTooManyRequests,
		`{"error":{"message":"`+quotaMessage+`","type":"AccountQuotaExceeded","code":"AccountQuotaExceeded"}}`,
	)

	assert.Equal(t, http.StatusTooManyRequests, taskErr.StatusCode)
	assert.Equal(t, "AccountQuotaExceeded", taskErr.Code)
	assert.Equal(t, quotaMessage, taskErr.Message)
	assert.False(t, taskErr.LocalError)

	apiErr := relaytypes.NewOpenAIError(
		taskErr.Error,
		relaytypes.ErrorCode(taskErr.Code),
		taskErr.StatusCode,
	)
	_, classified := service.ClassifyPlanQuotaError(apiErr)
	assert.True(t, classified)
}

func TestSubmitTaskUpstreamPreservesDistinctStructuredQuotaType(t *testing.T) {
	const quotaMessage = "You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC."
	taskErr := submitTaskToErrorServer(
		t,
		http.StatusTooManyRequests,
		`{"error":{"message":"`+quotaMessage+`","type":"AccountQuotaExceeded","code":"other_error"}}`,
	)

	assert.Equal(t, http.StatusTooManyRequests, taskErr.StatusCode)
	assert.Equal(t, "other_error", taskErr.Code)
	assert.Equal(t, quotaMessage, taskErr.Message)
	assert.False(t, taskErr.LocalError)

	encoded, err := common.Marshal(taskErr)
	require.NoError(t, err)
	var response map[string]any
	require.NoError(t, common.Unmarshal(encoded, &response))
	errType, ok := response["type"].(string)
	require.True(t, ok)
	assert.Equal(t, "AccountQuotaExceeded", errType)

	apiErr := relaytypes.WithOpenAIError(relaytypes.OpenAIError{
		Message: taskErr.Message,
		Type:    errType,
		Code:    taskErr.Code,
	}, taskErr.StatusCode)
	_, classified := service.ClassifyPlanQuotaError(apiErr)
	assert.True(t, classified)
}

func TestSubmitTaskUpstreamPreservesStructuredQuotaTypeWithoutCode(t *testing.T) {
	const quotaMessage = "You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC."
	taskErr := submitTaskToErrorServer(
		t,
		http.StatusTooManyRequests,
		`{"error":{"message":"`+quotaMessage+`","type":"AccountQuotaExceeded"}}`,
	)

	assert.Equal(t, http.StatusTooManyRequests, taskErr.StatusCode)
	assert.Equal(t, "unknown_error", taskErr.Code)
	assert.Equal(t, "AccountQuotaExceeded", taskErr.Type)
	assert.Equal(t, quotaMessage, taskErr.Message)
	assert.False(t, taskErr.LocalError)

	apiErr := relaytypes.WithOpenAIError(relaytypes.OpenAIError{
		Message: taskErr.Message,
		Type:    taskErr.Type,
		Code:    taskErr.Code,
	}, taskErr.StatusCode)
	_, classified := service.ClassifyPlanQuotaError(apiErr)
	assert.True(t, classified)
}

func TestSubmitTaskUpstreamKeepsGenericSemanticsForOtherErrors(t *testing.T) {
	const quotaMessage = "You have exceeded the monthly usage quota. It will reset at 2033-05-18 03:33:20 +0000 UTC."
	const untrustedBody = "not-json upstream-secret"
	testCases := []struct {
		name         string
		statusCode   int
		responseBody string
		wantCode     string
		wantMessage  string
	}{
		{
			name:         "quota message without structured semantics",
			statusCode:   http.StatusTooManyRequests,
			responseBody: `{"message":"` + quotaMessage + `"}`,
			wantCode:     string(relaytypes.ErrorCodeBadResponseStatusCode),
			wantMessage:  quotaMessage,
		},
		{
			name:         "ordinary structured rate limit",
			statusCode:   http.StatusTooManyRequests,
			responseBody: `{"error":{"message":"rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			wantCode:     "rate_limit_exceeded",
			wantMessage:  "rate limit exceeded",
		},
		{
			name:         "malformed upstream response",
			statusCode:   http.StatusBadGateway,
			responseBody: untrustedBody,
			wantCode:     string(relaytypes.ErrorCodeBadResponseStatusCode),
			wantMessage:  "bad response status code 502",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			taskErr := submitTaskToErrorServer(t, testCase.statusCode, testCase.responseBody)

			assert.Equal(t, testCase.statusCode, taskErr.StatusCode)
			assert.Equal(t, testCase.wantCode, taskErr.Code)
			assert.Equal(t, testCase.wantMessage, taskErr.Message)
			assert.False(t, taskErr.LocalError)
			assert.NotContains(t, taskErr.Message, untrustedBody)
			require.Error(t, taskErr.Error)
			assert.NotContains(t, taskErr.Error.Error(), untrustedBody)

			apiErr := relaytypes.NewOpenAIError(
				taskErr.Error,
				relaytypes.ErrorCode(taskErr.Code),
				taskErr.StatusCode,
			)
			_, classified := service.ClassifyPlanQuotaError(apiErr)
			assert.False(t, classified)
		})
	}
}

func TestTaskModel2DtoNormalizesLegacyAction(t *testing.T) {
	task := &model.Task{Action: "firstTailGenerate"}

	dtoTask := TaskModel2Dto(task)

	assert.Equal(t, constant.TaskActionFirstTailToVideo, dtoTask.Action)
	assert.Equal(t, "firstTailGenerate", task.Action)
}

const mappingOrderSubmitPlugin = `
export const meta = {apiVersion:1,key:"maporder",name:"Map Order",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel, model: ctx.model}, action:"text_to_video"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

const mappingOrderRewritePlugin = `
export const meta = {apiVersion:1,key:"maporder-rw",name:"Map Order RW",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel}, rewriteModel:"rewritten"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

func pinMappingOrderPlugin(t *testing.T, c *gin.Context, source string) {
	t.Helper()
	plugin, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
}

func newTaskSubmitContext(t *testing.T, originalModel, mapping string) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, originalModel)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://provider.example")
	if mapping != "" {
		c.Set("model_mapping", mapping)
	}
	c.Set("task_request", map[string]any{"prompt": "p"})
	return c, &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
}

func TestRelayTaskSubmitMapsBeforeValidateWhenOriginSet(t *testing.T) {
	const mapping = `{"alias-model":"mid-model","mid-model":"declared-model"}`

	c, info := newTaskSubmitContext(t, "alias-model", mapping)
	pinMappingOrderPlugin(t, c, mappingOrderSubmitPlugin)
	info.OriginModelName = "alias-model"

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, "alias-model", info.OriginModelName)
	assert.Equal(t, "declared-model", info.UpstreamModelName)
	assert.True(t, info.IsModelMapped)
}

func TestRelayTaskSubmitDeclaredNameWithoutMappingIsUnchanged(t *testing.T) {
	c, info := newTaskSubmitContext(t, "declared-model", "")
	pinMappingOrderPlugin(t, c, mappingOrderSubmitPlugin)
	info.OriginModelName = "declared-model"

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, "declared-model", info.OriginModelName)
	assert.Equal(t, "declared-model", info.UpstreamModelName)
	assert.False(t, info.IsModelMapped)
}

func TestRelayTaskSubmitDoesNotApplyMappingTwice(t *testing.T) {
	c, info := newTaskSubmitContext(t, "alias-model", `{"alias-model":"declared-model"}`)
	pinMappingOrderPlugin(t, c, mappingOrderRewritePlugin)
	info.OriginModelName = "alias-model"

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, "rewritten", info.UpstreamModelName, "late mapping would overwrite rewriteModel with the chain tail")
	assert.Equal(t, "alias-model", info.OriginModelName)
}

func TestRelayTaskSubmitEmptyOriginKeepsLateMapping(t *testing.T) {
	plugin, err := pluginruntime.NewRegistry().Register(mappingOrderSubmitPlugin, pluginruntime.Options{})
	require.NoError(t, err)
	synthesized := service.CoverTaskActionToModelName(constant.TaskPlatform(plugin.Meta.Key), "text_to_video")
	c, info := newTaskSubmitContext(t, "pre-validate-upstream",
		`{"pre-validate-upstream":"should-not-apply-early","`+synthesized+`":"legacy-tail"}`)
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
	info.OriginModelName = ""

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, synthesized, info.OriginModelName)
	assert.Equal(t, "legacy-tail", info.UpstreamModelName)
	assert.True(t, info.IsModelMapped)
}

const billingFallbackPlugin = `
export const meta = {apiVersion:1,key:"bill-fallback",name:"Bill Fallback",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel, model: ctx.model}, action:"text_to_video"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

func saveBillingConfig(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})
}

func TestRelayTaskSubmitAliasBillingIdentityAndExprFallback(t *testing.T) {
	const mapping = `{"alias-model":"declared-model"}`
	const aliasExpr = `tier("alias", 2)`
	const tailExpr = `tier("tail", 3)`

	tests := []struct {
		name       string
		modes      map[string]string
		exprs      map[string]string
		wantTiered bool
		wantExpr   string
	}{
		{
			name:       "alias own tiered wins",
			modes:      map[string]string{"alias-model": "tiered_expr", "declared-model": "tiered_expr"},
			exprs:      map[string]string{"alias-model": aliasExpr, "declared-model": tailExpr},
			wantTiered: true,
			wantExpr:   aliasExpr,
		},
		{
			name:       "fallback uses tail expr",
			modes:      map[string]string{"declared-model": "tiered_expr"},
			exprs:      map[string]string{"declared-model": tailExpr},
			wantTiered: true,
			wantExpr:   tailExpr,
		},
		{
			name:       "neither tiered uses ordinary pricing",
			wantTiered: false,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			saveBillingConfig(t)
			if len(testCase.modes) > 0 {
				modeJSON, marshalErr := common.Marshal(testCase.modes)
				require.NoError(t, marshalErr)
				exprJSON, marshalErr := common.Marshal(testCase.exprs)
				require.NoError(t, marshalErr)
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
					"billing_setting.billing_mode": string(modeJSON),
					"billing_setting.billing_expr": string(exprJSON),
				}))
				if testCase.wantExpr == aliasExpr {
					require.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("alias-model"))
				} else {
					require.Equal(t, billing_setting.BillingModeRatio, billing_setting.GetBillingMode("alias-model"))
					require.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("declared-model"))
				}
			}

			c, info := newTaskSubmitContext(t, "alias-model", mapping)
			c.Set("group", "default")
			info.UserGroup = "default"
			info.UsingGroup = "default"
			pinMappingOrderPlugin(t, c, billingFallbackPlugin)
			info.OriginModelName = "alias-model"

			_, taskErr := RelayTaskSubmit(c, info)
			require.NotNil(t, taskErr)
			assert.Equal(t, "alias-model", info.OriginModelName)
			assert.Equal(t, "declared-model", info.UpstreamModelName)
			assert.True(t, info.IsModelMapped)

			task := model.InitTask(constant.TaskPlatform("bill-fallback"), info)
			assert.Equal(t, "alias-model", task.Properties.OriginModelName)
			assert.Equal(t, "declared-model", task.Properties.UpstreamModelName)

			if testCase.wantTiered {
				require.NotNil(t, info.TieredBillingSnapshot)
				assert.Equal(t, "alias-model", info.TieredBillingSnapshot.ModelName)
				assert.Equal(t, testCase.wantExpr, info.TieredBillingSnapshot.ExprString)
				assert.Equal(t, billingexpr.ExprHashString(testCase.wantExpr), info.TieredBillingSnapshot.ExprHash)
				assert.NotEqual(t, "model_price_error", taskErr.Code)
			} else {
				assert.Nil(t, info.TieredBillingSnapshot)
				assert.Equal(t, "model_price_error", taskErr.Code)
			}
		})
	}
}
