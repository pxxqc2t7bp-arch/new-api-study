package middleware

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	geminidto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

const (
	geminiLiveSetupReadLimit   = 1 << 20
	geminiLiveSetupReadTimeout = 10 * time.Second
)

var geminiLiveUpgrader = websocket.Upgrader{
	CheckOrigin: func(_ *http.Request) bool {
		return true
	},
}

func GeminiLiveSetup() gin.HandlerFunc {
	return geminiLiveSetup(geminiLiveSetupReadTimeout, geminiLiveSetupReadLimit)
}

func geminiLiveSetup(readTimeout time.Duration, readLimit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		clientWS, err := geminiLiveUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			c.Abort()
			return
		}
		defer clientWS.Close()
		common.SetContextKey(c, constant.ContextKeyRealtimeClientWS, clientWS)
		common.SetContextKey(c, constant.ContextKeyRealtimeWSOwned, true)

		clientWS.SetReadLimit(readLimit)
		if err = clientWS.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			writeGeminiLiveError(clientWS, http.StatusInternalServerError, "failed to initialize setup deadline")
			c.Abort()
			return
		}
		messageType, firstFrame, err := clientWS.ReadMessage()
		if err != nil {
			writeGeminiLiveError(clientWS, http.StatusBadRequest, "failed to read Gemini Live setup")
			c.Abort()
			return
		}
		if err = clientWS.SetReadDeadline(time.Time{}); err != nil {
			writeGeminiLiveError(clientWS, http.StatusInternalServerError, "failed to clear setup deadline")
			c.Abort()
			return
		}
		if messageType != websocket.TextMessage {
			writeGeminiLiveError(clientWS, http.StatusBadRequest, "first Gemini Live message must be a text setup frame")
			c.Abort()
			return
		}

		setup, canonical, err := geminidto.ParseGeminiLiveSetup(firstFrame)
		if err != nil {
			writeGeminiLiveError(clientWS, http.StatusBadRequest, err.Error())
			c.Abort()
			return
		}
		if !geminiLiveTicketMatchesSetup(c, setup.Model) {
			writeGeminiLiveError(clientWS, http.StatusUnauthorized, "invalid realtime ticket model binding")
			c.Abort()
			return
		}
		if !revalidateGeminiLiveToken(c, setup.Model, clientWS) {
			return
		}
		if !consumeGeminiLiveTicket(c, setup.Model, clientWS) {
			return
		}

		query := c.Request.URL.Query()
		query.Set("model", setup.Model)
		c.Request.URL.RawQuery = query.Encode()
		c.Request.RequestURI = c.Request.URL.RequestURI()

		service.GetChannelConstraints(c).AddFilter(taskdto.ChannelFilter{
			Kind:                taskdto.FilterChannelTypes,
			AllowedChannelTypes: []int{constant.ChannelTypeGemini},
		})
		service.GetChannelConstraints(c).AddFilter(taskdto.ChannelFilter{
			Kind: taskdto.FilterGeminiLive,
		})
		common.SetContextKey(c, constant.ContextKeyRealtimeSetup, canonical)
		common.SetContextKey(c, constant.ContextKeyRealtimeMessageType, messageType)
		c.Next()
	}
}

func geminiLiveTicketMatchesSetup(c *gin.Context, modelName string) bool {
	raw := common.GetContextKeyString(c, constant.ContextKeyRealtimeTicket)
	if raw == "" {
		return true
	}
	boundModel := common.GetContextKeyString(c, constant.ContextKeyRealtimeModel)
	return geminidto.NormalizeGeminiLiveModel(boundModel) == modelName
}

func revalidateGeminiLiveToken(
	c *gin.Context,
	modelName string,
	clientWS *websocket.Conn,
) bool {
	tokenID := common.GetContextKeyInt(c, constant.ContextKeyTokenId)
	userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	token, err := model.GetTokenByIds(tokenID, userID)
	if err != nil {
		status := http.StatusInternalServerError
		message := "failed to revalidate realtime token"
		if errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusUnauthorized
			message = "invalid realtime token"
		}
		writeGeminiLiveError(clientWS, status, message)
		c.Abort()
		return false
	}
	token, err = model.ValidateUserToken(token.Key)
	if err != nil || token == nil || token.Id != tokenID || token.UserId != userID {
		status := http.StatusUnauthorized
		message := "invalid realtime token"
		if errors.Is(err, model.ErrDatabase) {
			status = http.StatusInternalServerError
			message = "failed to revalidate realtime token"
		}
		writeGeminiLiveError(clientWS, status, message)
		c.Abort()
		return false
	}
	if token.ModelLimitsEnabled &&
		!TokenModelLimitAllows(token.GetModelLimitsMap(), modelName) {
		writeGeminiLiveError(clientWS, http.StatusForbidden, "token does not allow the Gemini Live model")
		c.Abort()
		return false
	}
	if raw := common.GetContextKeyString(c, constant.ContextKeyRealtimeTicket); raw != "" {
		strategy, ok := common.GetContextKeyType[hosttypes.RoutingStrategy](
			c,
			constant.ContextKeyRoutingStrategy,
		)
		allowed, allowedErr := token.GetAllowedRoutingStrategies()
		if !ok || allowedErr != nil ||
			!slices.Contains(allowed, string(strategy)) {
			writeGeminiLiveError(clientWS, http.StatusUnauthorized, "invalid realtime ticket")
			c.Abort()
			return false
		}
	}
	return setupValidatedTokenContext(c, token)
}

func consumeGeminiLiveTicket(c *gin.Context, modelName string, clientWS *websocket.Conn) bool {
	raw := common.GetContextKeyString(c, constant.ContextKeyRealtimeTicket)
	if raw == "" {
		return true
	}
	boundModel := strings.TrimSpace(common.GetContextKeyString(c, constant.ContextKeyRealtimeModel))
	if boundModel == "" {
		writeGeminiLiveError(clientWS, http.StatusUnauthorized, "invalid realtime ticket")
		c.Abort()
		return false
	}
	if _, err := model.ConsumeRealtimeTicket(raw, boundModel); err != nil {
		if isRealtimeTicketClientError(err) {
			writeGeminiLiveError(clientWS, http.StatusUnauthorized, "invalid realtime ticket")
		} else {
			writeGeminiLiveError(clientWS, http.StatusInternalServerError, "failed to validate realtime ticket")
		}
		c.Abort()
		return false
	}
	common.SetContextKey(c, constant.ContextKeyRealtimeTicket, "")
	common.SetContextKey(c, constant.ContextKeyRealtimeModel, modelName)
	return true
}

func writeGeminiLiveError(conn *websocket.Conn, code int, message string) {
	if conn == nil {
		return
	}
	_ = conn.WriteJSON(gin.H{
		"error": gin.H{
			"code":    code,
			"message": message,
		},
	})
	closeCode := websocket.ClosePolicyViolation
	closeReason := "request rejected"
	if code == http.StatusTooManyRequests {
		closeCode = websocket.CloseTryAgainLater
		closeReason = "rate limited"
	} else if code >= http.StatusInternalServerError {
		closeCode = websocket.CloseInternalServerErr
		closeReason = "internal error"
	}
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(closeCode, closeReason),
		time.Now().Add(time.Second),
	)
}

func WriteGeminiLiveError(conn *websocket.Conn, code int, message string) {
	writeGeminiLiveError(conn, code, message)
}

func abortGeminiLiveAfterUpgrade(c *gin.Context, statusCode int, message string) bool {
	conn, ok := common.GetContextKeyType[*websocket.Conn](
		c,
		constant.ContextKeyRealtimeClientWS,
	)
	if !ok || conn == nil {
		return false
	}
	common.SetContextKey(c, constant.ContextKeyRealtimeFailed, true)
	writeGeminiLiveError(conn, statusCode, message)
	c.Abort()
	return true
}
