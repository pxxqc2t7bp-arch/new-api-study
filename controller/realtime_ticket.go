package controller

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type realtimeTicketRequest struct {
	TokenId         int    `json:"token_id"`
	Model           string `json:"model"`
	RoutingStrategy string `json:"routing_strategy"`
}

func IssueRealtimeTicket(c *gin.Context) {
	var request realtimeTicketRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeRealtimeTicketError(c, http.StatusBadRequest, "invalid realtime ticket request")
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.TokenId <= 0 || request.Model == "" || len(request.Model) > 255 {
		writeRealtimeTicketError(c, http.StatusBadRequest, "token_id and model are required")
		return
	}

	userId := c.GetInt("id")
	token, err := model.GetTokenByIds(request.TokenId, userId)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeRealtimeTicketError(c, http.StatusNotFound, "token not found")
		} else {
			writeRealtimeTicketError(c, http.StatusInternalServerError, "failed to load token")
		}
		return
	}
	validated, err := model.ValidateUserToken(token.Key)
	if err != nil || validated == nil || validated.Id != token.Id || validated.UserId != userId {
		if errors.Is(err, model.ErrDatabase) {
			writeRealtimeTicketError(c, http.StatusInternalServerError, "failed to validate token")
		} else {
			writeRealtimeTicketError(c, http.StatusForbidden, "token is not valid for realtime access")
		}
		return
	}
	token = validated
	if token.ModelLimitsEnabled &&
		!middleware.TokenModelLimitAllows(token.GetModelLimitsMap(), request.Model) {
		writeRealtimeTicketError(c, http.StatusForbidden, "token cannot access the requested model")
		return
	}

	strategy, status, err := realtimeTicketRoutingStrategy(token, request.RoutingStrategy)
	if err != nil {
		writeRealtimeTicketError(c, status, err.Error())
		return
	}
	raw, ticket, err := model.CreateRealtimeTicket(model.RealtimeTicketCreate{
		UserId:          userId,
		TokenId:         token.Id,
		Model:           request.Model,
		RoutingStrategy: strategy,
	})
	if err != nil {
		writeRealtimeTicketError(c, http.StatusInternalServerError, "failed to issue realtime ticket")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"ticket":           raw,
			"expires_at":       ticket.ExpiresAt,
			"model":            ticket.Model,
			"routing_strategy": ticket.RoutingStrategy,
		},
	})
}

func realtimeTicketRoutingStrategy(token *model.Token, requested string) (hosttypes.RoutingStrategy, int, error) {
	allowed, err := token.GetAllowedRoutingStrategies()
	if err != nil {
		return "", http.StatusInternalServerError, errors.New("token routing policy is invalid")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		strategy, ok := hosttypes.ParseRoutingStrategy(requested)
		if !ok {
			return "", http.StatusBadRequest, errors.New("unsupported routing strategy")
		}
		if !slices.Contains(allowed, string(strategy)) {
			return "", http.StatusForbidden, errors.New("routing strategy is not authorized for this token")
		}
		return strategy, 0, nil
	}

	defaultStrategy := strings.TrimSpace(token.DefaultRoutingStrategy)
	if defaultStrategy == "" {
		defaultStrategy = string(hosttypes.RoutingStrategyStable)
	}
	strategy, ok := hosttypes.ParseRoutingStrategy(defaultStrategy)
	if !ok || !slices.Contains(allowed, string(strategy)) {
		return "", http.StatusInternalServerError, errors.New("token routing policy is invalid")
	}
	return strategy, 0, nil
}

func writeRealtimeTicketError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{
		"success": false,
		"error": gin.H{
			"message": message,
		},
	})
}
