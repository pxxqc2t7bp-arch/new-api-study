package service

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/wsmanager"
)

const ChannelDisabledCloseReason = "channel disabled or deleted"

var ErrChannelWebSocketUnavailable = errors.New("channel is unavailable for websocket connection")

func CloseActiveWebSocketsForChannel(channelID int, reason string) int {
	return wsmanager.CloseChannelsAndBroadcast([]int{channelID}, reason)
}

func CloseActiveWebSocketsForChannels(channelIDs []int, reason string) int {
	return wsmanager.CloseChannelsAndBroadcast(channelIDs, reason)
}

func RegisterActiveWebSocketForChannel(
	channelID int,
	kind string,
	closeConnection func(reason string),
) (func(), error) {
	var closed atomic.Bool
	var closeOnce sync.Once
	closeRegistered := func(reason string) {
		closed.Store(true)
		closeOnce.Do(func() {
			closeConnection(reason)
		})
	}
	unregister := wsmanager.Register(channelID, kind, closeRegistered)

	channel, err := model.CacheGetChannel(channelID)
	if err != nil || channel == nil || channel.Status != common.ChannelStatusEnabled {
		unregister()
		closeRegistered(ChannelDisabledCloseReason)
		if err != nil {
			return func() {}, fmt.Errorf("%w: %v", ErrChannelWebSocketUnavailable, err)
		}
		return func() {}, ErrChannelWebSocketUnavailable
	}
	if closed.Load() {
		unregister()
		return func() {}, ErrChannelWebSocketUnavailable
	}
	return unregister, nil
}

func updateManagedChannelStatus(channelID int, usingKey string, status int, reason string) bool {
	if !model.UpdateChannelStatus(channelID, usingKey, status, reason) {
		return false
	}
	if status != common.ChannelStatusEnabled {
		CloseActiveWebSocketsForChannel(channelID, ChannelDisabledCloseReason)
	}
	return true
}
