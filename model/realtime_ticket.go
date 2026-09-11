package model

import (
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
)

const (
	AuthFlowPurposeRealtimeTicket = "realtime_ticket"
	RealtimeTicketPrefix          = "rt_"
	RealtimeTicketTTL             = 60 * time.Second
)

var ErrRealtimeTicketBinding = errors.New("realtime ticket binding does not match")

type RealtimeTicketCreate struct {
	UserId          int
	TokenId         int
	Model           string
	RoutingStrategy hosttypes.RoutingStrategy
}

type RealtimeTicket struct {
	UserId          int                       `json:"user_id"`
	TokenId         int                       `json:"token_id"`
	Model           string                    `json:"model"`
	RoutingStrategy hosttypes.RoutingStrategy `json:"routing_strategy"`
	ExpiresAt       int64                     `json:"expires_at"`
}

type realtimeTicketPayload struct {
	TokenId         int                       `json:"token_id"`
	Model           string                    `json:"model"`
	RoutingStrategy hosttypes.RoutingStrategy `json:"routing_strategy"`
}

func CreateRealtimeTicket(input RealtimeTicketCreate) (string, *RealtimeTicket, error) {
	modelName := strings.TrimSpace(input.Model)
	strategy, validStrategy := hosttypes.ParseRoutingStrategy(string(input.RoutingStrategy))
	if input.UserId <= 0 || input.TokenId <= 0 || modelName == "" || len(modelName) > 255 || !validStrategy {
		return "", nil, ErrAuthFlowInvalid
	}

	payload, err := common.Marshal(realtimeTicketPayload{
		TokenId:         input.TokenId,
		Model:           modelName,
		RoutingStrategy: strategy,
	})
	if err != nil {
		return "", nil, err
	}
	expiresAt := time.Now().Add(RealtimeTicketTTL)
	raw, _, err := CreateAuthFlow(AuthFlowCreate{
		Purpose:   AuthFlowPurposeRealtimeTicket,
		UserId:    input.UserId,
		Payload:   string(payload),
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return "", nil, err
	}
	return RealtimeTicketPrefix + raw, &RealtimeTicket{
		UserId:          input.UserId,
		TokenId:         input.TokenId,
		Model:           modelName,
		RoutingStrategy: strategy,
		ExpiresAt:       expiresAt.Unix(),
	}, nil
}

func ConsumeRealtimeTicket(raw string, modelName string) (*RealtimeTicket, error) {
	if !strings.HasPrefix(raw, RealtimeTicketPrefix) {
		return nil, ErrAuthFlowInvalid
	}
	raw = strings.TrimPrefix(raw, RealtimeTicketPrefix)
	modelName = strings.TrimSpace(modelName)
	if raw == "" || modelName == "" {
		return nil, ErrAuthFlowInvalid
	}

	var (
		payload  realtimeTicketPayload
		strategy hosttypes.RoutingStrategy
	)
	flow, err := ConsumeAuthFlowWithAction(
		raw,
		AuthFlowMatch{Purpose: AuthFlowPurposeRealtimeTicket},
		func(_ *gorm.DB, flow *AuthFlow) error {
			if err := common.UnmarshalJsonStr(flow.Payload, &payload); err != nil {
				return ErrAuthFlowInvalid
			}
			if flow.UserId <= 0 || payload.TokenId <= 0 || payload.Model != modelName {
				return ErrRealtimeTicketBinding
			}
			var ok bool
			strategy, ok = hosttypes.ParseRoutingStrategy(string(payload.RoutingStrategy))
			if !ok {
				return ErrAuthFlowInvalid
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return &RealtimeTicket{
		UserId:          flow.UserId,
		TokenId:         payload.TokenId,
		Model:           payload.Model,
		RoutingStrategy: strategy,
		ExpiresAt:       flow.ExpiresAt.Unix(),
	}, nil
}
