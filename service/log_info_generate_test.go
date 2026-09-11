package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendRelayLogAdminInfoIncludesResolvedRequestPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	other := model.NewLogOther()
	info := &relaycommon.RelayInfo{
		RoutingStrategy:  hosttypes.RoutingStrategyLatency,
		ConversionPolicy: relaytypes.ConversionLossPolicySafe,
		TokenKey:         "sk-secret",
	}

	AppendRelayLogAdminInfo(ctx, info, other)

	adminInfo, ok := other.Snapshot()["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "latency", adminInfo["routing_strategy"])
	assert.Equal(t, "safe", adminInfo["conversion_policy"])
	assert.NotContains(t, adminInfo, "token_key")
	assert.NotContains(t, adminInfo, "service_tier")
}

func TestAppendChannelFailoverAdminInfoSingleAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("use_channel", []string{"101"})
	ctx.Set("channel_priority_path", []int64{30})
	adminInfo := map[string]any{"existing": "preserved"}

	AppendChannelFailoverAdminInfo(ctx, adminInfo)

	assert.Equal(t, "preserved", adminInfo["existing"])
	assert.Equal(t, 1, adminInfo["attempt_count"])
	assert.Equal(t, []string{"101"}, adminInfo["attempted_channels"])
	assert.Equal(t, []int64{30}, adminInfo["priority_path"])
	assert.Equal(t, int64(30), adminInfo["final_tier"])
	assert.NotContains(t, adminInfo, "fallback_reason")
}

func TestAppendChannelFailoverAdminInfoMultipleAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("use_channel", []string{"201", "202"})
	ctx.Set("channel_priority_path", []int64{30, 20})
	ctx.Set("channel_fallback_reason", "status_500:ha_test")
	adminInfo := map[string]any{}

	AppendChannelFailoverAdminInfo(ctx, adminInfo)

	assert.Equal(t, 2, adminInfo["attempt_count"])
	assert.Equal(t, []string{"201", "202"}, adminInfo["attempted_channels"])
	assert.Equal(t, []int64{30, 20}, adminInfo["priority_path"])
	assert.Equal(t, int64(20), adminInfo["final_tier"])
	assert.Equal(t, "status_500:ha_test", adminInfo["fallback_reason"])
}

func TestAppendChannelFailoverAdminInfoWithoutAttemptsIsNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	adminInfo := map[string]any{"existing": "preserved"}

	AppendChannelFailoverAdminInfo(ctx, adminInfo)

	assert.Equal(t, map[string]any{"existing": "preserved"}, adminInfo)
}
