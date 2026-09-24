package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nilResponseTaskAdaptor struct {
	channel.TaskAdaptor
	attempts int
	events   *[]string
}

func (a *nilResponseTaskAdaptor) DoRequest(*gin.Context, *relaycommon.RelayInfo, io.Reader) (*http.Response, error) {
	a.attempts++
	*a.events = append(*a.events, "provider")
	return nil, nil
}

func TestRC40TaskSubmitPreservesSuccessfulAndFailedUpstreamStatus(t *testing.T) {
	service.InitHttpClient()
	const source = `
export const meta = {apiVersion:1,key:"rc40-status",name:"RC40 Status",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/submit",method:"POST",body:{model:ctx.model},action:"text_to_video"}}
export function parseSubmitResponse(ctx,response){return {taskId:response.body.id,taskData:{status:response.statusCode}}}
export function buildQueryRequest(ctx){return {url:ctx.baseUrl+"/query"}}
export function parseTaskResult(){return {status:"SUCCESS"}}
`
	for _, testCase := range []struct {
		status  int
		wantErr bool
	}{
		{status: http.StatusOK},
		{status: http.StatusCreated},
		{status: http.StatusAccepted},
		{status: http.StatusBadRequest, wantErr: true},
		{status: http.StatusBadGateway, wantErr: true},
	} {
		t.Run(strconv.Itoa(testCase.status), func(t *testing.T) {
			saveBillingConfig(t)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				"billing_setting.billing_mode": `{"declared-model":"tiered_expr"}`,
				"billing_setting.billing_expr": `{"declared-model":"tier(\"flat\", 3)"}`,
			}))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(`{"id":"job-42","message":"upstream-body"}`))
			}))
			defer server.Close()

			context, info := newTaskSubmitContext(t, "declared-model", "")
			common.SetContextKey(context, constant.ContextKeyChannelBaseUrl, server.URL)
			context.Set("group", "default")
			info.UserGroup, info.UsingGroup = "default", "default"
			info.OriginModelName = "declared-model"
			info.Billing = &imageReservation{limit: 1 << 30}
			pinMappingOrderPlugin(t, context, source)

			result, taskErr := RelayTaskSubmit(context, info)

			assert.EqualValues(t, 1, requests.Load())
			if testCase.wantErr {
				require.NotNil(t, taskErr)
				assert.Equal(t, "fail_to_fetch_task", taskErr.Code)
				assert.Equal(t, testCase.status, taskErr.StatusCode)
				assert.Equal(t, `{"id":"job-42","message":"upstream-body"}`, taskErr.Message)
				assert.False(t, taskErr.ProviderAccepted)
				assert.Nil(t, result)
				return
			}
			require.Nil(t, taskErr, "submission error: %+v", taskErr)
			require.NotNil(t, result)
			assert.Equal(t, "job-42", result.UpstreamTaskID)
			assert.JSONEq(t, `{"status":`+strconv.Itoa(testCase.status)+`}`, string(result.TaskData))
			assert.True(t, result.ProviderAccepted)
		})
	}
}

func TestRC40TaskSubmitAcceptedLocalFailuresAreNonRetryable(t *testing.T) {
	service.InitHttpClient()
	const source = `
export const meta = {apiVersion:1,key:"rc40-local-failure",name:"RC40 Local Failure",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/submit",method:"POST",body:{model:ctx.model},action:"text_to_video"}}
export function parseSubmitResponse(){throw new Error("rc40 parse failure")}
export function buildQueryRequest(ctx){return {url:ctx.baseUrl+"/query"}}
export function parseTaskResult(){return {status:"SUCCESS"}}
`
	for _, status := range []int{
		http.StatusOK,
		http.StatusCreated,
		http.StatusAccepted,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			saveBillingConfig(t)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				"billing_setting.billing_mode": `{"declared-model":"tiered_expr"}`,
				"billing_setting.billing_expr": `{"declared-model":"tier(\"flat\", 3)"}`,
			}))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"id":"job-42"}`))
			}))
			defer server.Close()

			context, info := newTaskSubmitContext(t, "declared-model", "")
			common.SetContextKey(context, constant.ContextKeyChannelBaseUrl, server.URL)
			context.Set("group", "default")
			info.UserGroup, info.UsingGroup = "default", "default"
			info.OriginModelName = "declared-model"
			info.Billing = &imageReservation{limit: 1 << 30}
			pinMappingOrderPlugin(t, context, source)

			result, taskErr := RelayTaskSubmit(context, info)

			assert.EqualValues(t, 1, requests.Load())
			assert.Nil(t, result)
			require.NotNil(t, taskErr)
			assert.Equal(t, "plugin_submit_response_failed", taskErr.Code)
			assert.Contains(t, taskErr.Message, "rc40 parse failure")
			assert.True(t, taskErr.NoRetry, "an accepted provider operation must not be submitted again")
			assert.True(t, taskErr.ProviderAccepted)
		})
	}
}

func TestRC40DeferredEmptyResponseAfterFenceIsNonRetryable(t *testing.T) {
	c, info := newTaskSubmitContext(t, "declared-model", "")
	events := make([]string, 0, 2)
	adaptor := &nilResponseTaskAdaptor{events: &events}

	result, taskErr := submitDeferredTaskUpstream(
		c,
		info,
		adaptor,
		constant.TaskPlatform("test"),
		nil,
		123,
		func() error {
			events = append(events, "fence")
			return nil
		},
	)

	assert.Nil(t, result)
	require.NotNil(t, taskErr)
	assert.Equal(t, []string{"fence", "provider"}, events)
	assert.Equal(t, 1, adaptor.attempts)
	assert.Equal(t, "fail_to_fetch_task", taskErr.Code)
	assert.Equal(t, http.StatusBadGateway, taskErr.StatusCode)
	assert.True(t, taskErr.NoRetry)
	assert.False(t, taskErr.ProviderAccepted)
	assert.ErrorIs(t, taskErr.Error, errTaskUpstreamEmptyResponse)

	foregroundEvents := make([]string, 0, 1)
	foregroundAdaptor := &nilResponseTaskAdaptor{events: &foregroundEvents}
	result, taskErr = submitTaskUpstream(c, info, foregroundAdaptor, constant.TaskPlatform("test"), nil, 123)

	assert.Nil(t, result)
	require.NotNil(t, taskErr)
	assert.Equal(t, []string{"provider"}, foregroundEvents)
	assert.Equal(t, 1, foregroundAdaptor.attempts)
	assert.False(t, taskErr.NoRetry)
	assert.False(t, taskErr.ProviderAccepted)
	assert.ErrorIs(t, taskErr.Error, errTaskUpstreamEmptyResponse)
}
