package service

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
)

func beginStreamBillingReservation(streamID string) (*model.StreamExecution, bool, error) {
	execution, err := model.GetStreamExecution(streamID)
	if err != nil {
		return nil, false, err
	}
	switch execution.BillingStatus {
	case model.StreamBillingReserved:
		return execution, false, nil
	case model.StreamBillingSettled:
		return execution, false, nil
	case model.StreamBillingNone:
		won, updateErr := model.UpdateStreamBillingState(
			streamID,
			[]string{model.StreamBillingNone},
			map[string]any{"billing_status": model.StreamBillingReserving},
		)
		if updateErr != nil || won {
			return execution, won, updateErr
		}
		current, reloadErr := model.GetStreamExecution(streamID)
		if reloadErr != nil {
			return nil, false, reloadErr
		}
		if current.BillingStatus == model.StreamBillingReserved ||
			current.BillingStatus == model.StreamBillingSettled {
			return current, false, nil
		}
		return nil, false, fmt.Errorf("stream billing state changed to %q", current.BillingStatus)
	case model.StreamBillingReserving, model.StreamBillingSettling,
		model.StreamBillingRefunding, model.StreamBillingUncertain:
		return nil, false, fmt.Errorf("stream billing state %q requires operator review", execution.BillingStatus)
	default:
		return nil, false, fmt.Errorf("unsupported stream billing state %q", execution.BillingStatus)
	}
}

func persistStreamBillingReservation(session *BillingSession) error {
	if session == nil || session.streamID == "" {
		return nil
	}
	updates := session.streamBillingSnapshot()
	updates["billing_status"] = model.StreamBillingReserved
	won, err := model.UpdateStreamBillingState(
		session.streamID,
		[]string{model.StreamBillingReserving, model.StreamBillingReserved},
		updates,
	)
	if err != nil {
		return err
	}
	if !won {
		return errors.New("stream billing reservation state changed concurrently")
	}
	return nil
}

func restoreStreamBillingSession(
	execution *model.StreamExecution,
	relayInfo *relaycommon.RelayInfo,
) (*BillingSession, error) {
	if execution == nil || relayInfo == nil {
		return nil, errors.New("stream billing restore requires execution and relay info")
	}
	var funding FundingSource
	switch execution.BillingSource {
	case BillingSourceWallet:
		funding = &WalletFunding{
			userId:   relayInfo.UserId,
			consumed: execution.ReservedQuota,
		}
	case BillingSourceSubscription:
		funding = &SubscriptionFunding{
			requestId:       relayInfo.RequestId,
			userId:          relayInfo.UserId,
			modelName:       relayInfo.OriginModelName,
			amount:          execution.SubscriptionPreConsumed,
			subscriptionId:  execution.SubscriptionID,
			preConsumed:     execution.SubscriptionPreConsumed,
			AmountTotal:     execution.SubscriptionAmountTotal,
			AmountUsedAfter: execution.SubscriptionAmountUsedAfter,
			PlanId:          execution.SubscriptionPlanID,
			PlanTitle:       execution.SubscriptionPlanTitle,
		}
	default:
		return nil, fmt.Errorf("unsupported persisted billing source %q", execution.BillingSource)
	}
	session := &BillingSession{
		relayInfo:        relayInfo,
		funding:          funding,
		preConsumedQuota: execution.ReservedQuota,
		tokenConsumed:    execution.TokenConsumed,
		extraReserved:    execution.ExtraReserved,
		trusted:          execution.Trusted,
		streamID:         execution.StreamID,
	}
	session.settled = execution.BillingStatus == model.StreamBillingSettled
	session.syncRelayInfo()
	return session, nil
}

func streamBillingError(err error) *types.NewAPIError {
	return types.NewError(
		fmt.Errorf("stream recovery billing: %w", err),
		types.ErrorCodeUpdateDataError,
		types.ErrOptionWithSkipRetry(),
	)
}
