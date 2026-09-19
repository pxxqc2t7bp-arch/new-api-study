package middleware

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	RealtimeTicketQuery             = "ticket"
	RealtimeTicketHeader            = "X-NewAPI-Realtime-Ticket"
	RealtimeTicketSubprotocolPrefix = "newapi-realtime-ticket."
	openAIAPIKeySubprotocolPrefix   = "openai-insecure-api-key."
	GeminiLivePath                  = relayconstant.GeminiLivePath
)

func RealtimeAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, found, err := takeRealtimeTicket(c.Request)
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusUnauthorized, "invalid realtime ticket")
			return
		}
		if !found {
			TokenAuth()(c)
			return
		}

		modelName := strings.TrimSpace(c.Query("model"))
		deferModelBinding := c.Request.URL.Path == GeminiLivePath
		var ticket *model.RealtimeTicket
		if deferModelBinding {
			ticket, err = model.GetRealtimeTicketClaims(raw)
		} else {
			ticket, err = model.GetRealtimeTicket(raw, modelName)
		}
		if err != nil {
			abortRealtimeTicketError(c, err)
			return
		}

		token, err := model.GetTokenByIds(ticket.TokenId, ticket.UserId)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				abortWithOpenAiMessage(c, http.StatusUnauthorized, "invalid realtime ticket")
			} else {
				abortRealtimeTicketError(c, err)
			}
			return
		}
		token, err = model.ValidateUserToken(token.Key)
		if err != nil || token == nil || token.Id != ticket.TokenId || token.UserId != ticket.UserId {
			if errors.Is(err, model.ErrDatabase) {
				abortWithOpenAiMessage(
					c,
					http.StatusInternalServerError,
					common.TranslateMessage(c, i18n.MsgDatabaseError),
				)
			} else {
				abortWithOpenAiMessage(c, http.StatusUnauthorized, "invalid realtime ticket")
			}
			return
		}
		if !deferModelBinding && token.ModelLimitsEnabled &&
			!TokenModelLimitAllows(token.GetModelLimitsMap(), ticket.Model) {
			abortWithOpenAiMessage(c, http.StatusUnauthorized, "invalid realtime ticket")
			return
		}
		allowed, allowedErr := token.GetAllowedRoutingStrategies()
		if allowedErr != nil || !slices.Contains(allowed, string(ticket.RoutingStrategy)) {
			abortWithOpenAiMessage(c, http.StatusUnauthorized, "invalid realtime ticket")
			return
		}
		if !setupValidatedTokenContext(c, token) {
			return
		}
		if deferModelBinding {
			common.SetContextKey(c, constant.ContextKeyRealtimeTicket, raw)
			common.SetContextKey(c, constant.ContextKeyRealtimeModel, ticket.Model)
			common.SetContextKey(c, constant.ContextKeyRoutingStrategy, ticket.RoutingStrategy)
			c.Next()
			return
		}
		consumed, err := model.ConsumeRealtimeTicket(raw, modelName)
		if err != nil {
			abortRealtimeTicketError(c, err)
			return
		}
		common.SetContextKey(c, constant.ContextKeyRoutingStrategy, consumed.RoutingStrategy)
		c.Next()
	}
}

func abortRealtimeTicketError(c *gin.Context, err error) {
	if isRealtimeTicketClientError(err) {
		abortWithOpenAiMessage(c, http.StatusUnauthorized, "invalid realtime ticket")
		return
	}
	abortWithOpenAiMessage(
		c,
		http.StatusInternalServerError,
		common.TranslateMessage(c, i18n.MsgDatabaseError),
	)
}

func isRealtimeTicketClientError(err error) bool {
	return errors.Is(err, model.ErrAuthFlowInvalid) ||
		errors.Is(err, model.ErrAuthFlowExpired) ||
		errors.Is(err, model.ErrAuthFlowConsumed) ||
		errors.Is(err, model.ErrRealtimeTicketBinding)
}

func takeRealtimeTicket(request *http.Request) (string, bool, error) {
	if request == nil || request.URL == nil {
		return "", false, nil
	}

	tickets := make([]string, 0, 3)
	query := request.URL.Query()
	for _, value := range query[RealtimeTicketQuery] {
		if value = strings.TrimSpace(value); value != "" {
			tickets = append(tickets, value)
		}
	}
	query.Del(RealtimeTicketQuery)
	request.URL.RawQuery = query.Encode()
	request.RequestURI = request.URL.RequestURI()
	hasNativeCredential := strings.TrimSpace(request.Header.Get("Authorization")) != "" ||
		strings.TrimSpace(request.Header.Get("x-goog-api-key")) != ""
	for _, value := range query["key"] {
		if strings.TrimSpace(value) != "" {
			hasNativeCredential = true
			break
		}
	}

	if value := strings.TrimSpace(request.Header.Get(RealtimeTicketHeader)); value != "" {
		tickets = append(tickets, value)
	}
	request.Header.Del(RealtimeTicketHeader)

	protocols := websocketSubprotocols(request)
	cleanProtocols := make([]string, 0, len(protocols))
	for _, protocol := range protocols {
		switch {
		case strings.HasPrefix(protocol, RealtimeTicketSubprotocolPrefix):
			if raw := strings.TrimPrefix(protocol, RealtimeTicketSubprotocolPrefix); raw != "" {
				tickets = append(tickets, raw)
			}
		case strings.HasPrefix(protocol, openAIAPIKeySubprotocolPrefix):
			hasNativeCredential = true
			cleanProtocols = append(cleanProtocols, protocol)
		default:
			cleanProtocols = append(cleanProtocols, protocol)
		}
	}
	setWebsocketSubprotocols(request, cleanProtocols)

	if len(tickets) == 0 {
		return "", false, nil
	}
	if len(tickets) != 1 || hasNativeCredential {
		request.Header.Del("Authorization")
		request.Header.Del("x-goog-api-key")
		query := request.URL.Query()
		query.Del("key")
		request.URL.RawQuery = query.Encode()
		request.RequestURI = request.URL.RequestURI()
		setWebsocketSubprotocols(request, removeCredentialSubprotocols(cleanProtocols))
		return "", false, errors.New("ambiguous realtime authentication")
	}
	request.Header.Del("Authorization")
	setWebsocketSubprotocols(request, removeCredentialSubprotocols(cleanProtocols))
	return tickets[0], true, nil
}

func splitWebsocketSubprotocols(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func websocketSubprotocols(request *http.Request) []string {
	if request == nil {
		return nil
	}
	return splitWebsocketSubprotocols(
		strings.Join(request.Header.Values("Sec-WebSocket-Protocol"), ","),
	)
}

func removeCredentialSubprotocols(protocols []string) []string {
	result := make([]string, 0, len(protocols))
	for _, protocol := range protocols {
		if strings.HasPrefix(protocol, openAIAPIKeySubprotocolPrefix) ||
			strings.HasPrefix(protocol, RealtimeTicketSubprotocolPrefix) {
			continue
		}
		result = append(result, protocol)
	}
	return result
}

func setWebsocketSubprotocols(request *http.Request, protocols []string) {
	if len(protocols) == 0 {
		request.Header.Del("Sec-WebSocket-Protocol")
		return
	}
	request.Header.Set("Sec-WebSocket-Protocol", strings.Join(protocols, ", "))
}

func takeOpenAIRealtimeAPIKey(request *http.Request) (string, bool) {
	if request == nil {
		return "", false
	}
	protocols := websocketSubprotocols(request)
	cleanProtocols := make([]string, 0, len(protocols))
	var key string
	for _, protocol := range protocols {
		if strings.HasPrefix(protocol, openAIAPIKeySubprotocolPrefix) {
			if key == "" {
				key = strings.TrimPrefix(protocol, openAIAPIKeySubprotocolPrefix)
			}
			continue
		}
		cleanProtocols = append(cleanProtocols, protocol)
	}
	setWebsocketSubprotocols(request, cleanProtocols)
	return key, key != ""
}

func takeGeminiLiveAPIKey(request *http.Request) (string, bool, error) {
	if request == nil || request.URL == nil {
		return "", false, nil
	}
	keys := make([]string, 0, 2)
	query := request.URL.Query()
	for _, value := range query["key"] {
		if value = strings.TrimSpace(value); value != "" {
			keys = append(keys, value)
		}
	}
	query.Del("key")
	request.URL.RawQuery = query.Encode()
	request.RequestURI = request.URL.RequestURI()

	if value := strings.TrimSpace(request.Header.Get("x-goog-api-key")); value != "" {
		keys = append(keys, value)
	}
	request.Header.Del("x-goog-api-key")
	if len(keys) == 0 {
		return "", false, nil
	}
	if len(keys) != 1 {
		return "", false, errors.New("ambiguous Gemini Live authentication")
	}
	return keys[0], true, nil
}
