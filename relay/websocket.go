package relay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/wsmanager"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	relayhelper "github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type realtimeWSConnection struct {
	mu     sync.Mutex
	client *websocket.Conn
	target *websocket.Conn
	closed bool
}

func (s *realtimeWSConnection) setTarget(target *websocket.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = target.Close()
		return false
	}
	s.target = target
	return true
}

func (s *realtimeWSConnection) closeTarget() {
	s.mu.Lock()
	target := s.target
	s.target = nil
	s.mu.Unlock()
	if target != nil {
		_ = target.Close()
	}
}

func (s *realtimeWSConnection) closeForPolicy(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	client, target := s.client, s.target
	s.mu.Unlock()

	deadline := time.Now().Add(time.Second)
	closeMessage := websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason)
	_ = client.WriteControl(websocket.CloseMessage, closeMessage, deadline)
	if target != nil {
		_ = target.WriteControl(websocket.CloseMessage, closeMessage, deadline)
	}
	_ = client.Close()
	if target != nil {
		_ = target.Close()
	}
}

func WssHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)
	connection := &realtimeWSConnection{client: info.ClientWs}
	defer connection.closeTarget()
	unregister, err := service.RegisterActiveWebSocketForChannel(
		info.ChannelId,
		wsmanager.KindRealtime,
		connection.closeForPolicy,
	)
	if err != nil {
		return types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	defer unregister()
	//var requestBody io.Reader
	//firstWssRequest, _ := c.Get("first_wss_request")
	//requestBody = bytes.NewBuffer(firstWssRequest.([]byte))

	statusCodeMappingStr := c.GetString("status_code_mapping")
	resp, err := adaptor.DoRequest(c, info, nil)
	if err != nil {
		return types.NewError(err, types.ErrorCodeDoRequestFailed)
	}

	if resp != nil {
		target := resp.(*websocket.Conn)
		if !connection.setTarget(target) {
			return types.NewError(context.Canceled, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
		}
		info.TargetWs = target
	}

	usage, newAPIError := adaptor.DoResponse(c, nil, info)
	if newAPIError != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return newAPIError
	}
	if err := service.PostWssConsumeQuota(
		c,
		info,
		info.UpstreamModelName,
		usage.(*dto.RealtimeUsage),
		"",
	); err != nil {
		rootcommon.SetContextKey(
			c,
			constant.ContextKeyRealtimeSettlementUncertain,
			true,
		)
		return types.NewError(
			err,
			types.ErrorCodeUpdateDataError,
			types.ErrOptionWithSkipRetry(),
		)
	}
	return nil
}

func GeminiLiveHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)
	if info.RelayMode != relayconstant.RelayModeGeminiLive {
		return types.NewError(
			fmt.Errorf("invalid Gemini Live relay mode: %d", info.RelayMode),
			types.ErrorCodeInvalidApiType,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if err := relayhelper.ModelMappedHelper(c, info, nil); err != nil {
		return types.NewError(
			err,
			types.ErrorCodeInvalidRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	setup, ok := rootcommon.GetContextKeyType[[]byte](
		c,
		constant.ContextKeyRealtimeSetup,
	)
	if !ok {
		return types.NewError(
			fmt.Errorf("missing Gemini Live setup"),
			types.ErrorCodeInvalidRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	setup, err := dto.RewriteGeminiLiveSetupModel(setup, info.UpstreamModelName)
	if err != nil {
		return types.NewError(
			err,
			types.ErrorCodeInvalidRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	rootcommon.SetContextKey(c, constant.ContextKeyRealtimeSetup, setup)

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(
			fmt.Errorf("invalid api type: %d", info.ApiType),
			types.ErrorCodeInvalidApiType,
			types.ErrOptionWithSkipRetry(),
		)
	}
	adaptor.Init(info)
	statusCodeMappingStr := c.GetString("status_code_mapping")
	resp, err := adaptor.DoRequest(c, info, nil)
	if err != nil {
		return types.NewError(err, types.ErrorCodeDoRequestFailed)
	}
	target, ok := resp.(*websocket.Conn)
	if !ok || target == nil {
		return types.NewError(
			fmt.Errorf("invalid Gemini Live upstream websocket"),
			types.ErrorCodeBadResponse,
			types.ErrOptionWithSkipRetry(),
		)
	}
	info.TargetWs = target

	usage := &dto.RealtimeUsage{}
	defer func() {
		_ = target.Close()
		extra := "gemini_live_usage=authoritative"
		if rootcommon.GetContextKeyBool(c, constant.ContextKeyGeminiLiveUsageEstimated) {
			extra = "gemini_live_usage=estimated"
		}
		if err := service.PostWssConsumeQuota(
			c,
			info,
			info.OriginModelName,
			usage,
			extra,
		); err != nil {
			rootcommon.SetContextKey(
				c,
				constant.ContextKeyRealtimeSettlementUncertain,
				true,
			)
			if newAPIError != nil {
				err = errors.Join(newAPIError, err)
			}
			newAPIError = types.NewError(
				err,
				types.ErrorCodeUpdateDataError,
				types.ErrOptionWithSkipRetry(),
			)
		}
	}()

	result, apiErr := adaptor.DoResponse(c, nil, info)
	if result != nil {
		if liveUsage, valid := result.(*dto.RealtimeUsage); valid && liveUsage != nil {
			usage = liveUsage
		}
	}
	if apiErr != nil {
		service.ResetStatusCode(apiErr, statusCodeMappingStr)
		return apiErr
	}
	return nil
}
