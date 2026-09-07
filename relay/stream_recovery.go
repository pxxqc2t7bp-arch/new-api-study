package relay

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

func streamRecoveryTerminalError(
	c *gin.Context,
	info *relaycommon.RelayInfo,
) *types.NewAPIError {
	if !common.GetContextKeyBool(c, constant.ContextKeyStreamRecoveryWorker) {
		return nil
	}
	value, exists := common.GetContextKey(c, constant.ContextKeyStreamRecoveryBroker)
	if !exists {
		return types.NewErrorWithStatusCode(
			errors.New("stream recovery broker is missing"),
			types.ErrorCodeBadResponse,
			http.StatusBadGateway,
		)
	}
	writer, ok := value.(*service.StreamRecoveryWriter)
	if !ok {
		return types.NewErrorWithStatusCode(
			errors.New("stream recovery broker has an invalid type"),
			types.ErrorCodeBadResponse,
			http.StatusBadGateway,
		)
	}
	if writer.Terminal() {
		return nil
	}
	message := "stream ended without a protocol terminal event"
	if info != nil && info.StreamStatus != nil {
		message = info.StreamStatus.Summary()
	}
	return types.NewErrorWithStatusCode(
		errors.New(message),
		types.ErrorCodeBadResponse,
		http.StatusBadGateway,
	)
}
