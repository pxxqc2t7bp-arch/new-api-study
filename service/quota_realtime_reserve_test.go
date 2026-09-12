package service

import (
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failingWssBilling struct {
	err error
}

func (b *failingWssBilling) Settle(int) error {
	return b.err
}

func (*failingWssBilling) Refund(*gin.Context) {}

func (*failingWssBilling) NeedsRefund() bool {
	return false
}

func (*failingWssBilling) GetPreConsumedQuota() int {
	return 0
}

func (*failingWssBilling) Reserve(int) error {
	return nil
}

func TestReserveWssConsumeQuotaReservesNextSegment(t *testing.T) {
	billing := &recordingBillingSettler{preConsumedQuota: 500}
	info := &relaycommon.RelayInfo{
		Billing: billing,
		PriceData: hosttypes.PriceData{
			ModelRatio: 1,
			GroupRatioInfo: hosttypes.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
	}
	usage := &dto.RealtimeUsage{
		TotalTokens: 501,
		InputTokens: 501,
		InputTokenDetails: dto.InputTokenDetails{
			TextTokens: 501,
		},
	}

	err := ReserveWssConsumeQuota(
		&gin.Context{},
		info,
		"gemini-live-test",
		usage,
	)

	require.NoError(t, err)
	assert.Equal(t, []int{1000}, billing.reserveTargets)
	assert.Equal(t, 1000, info.FinalPreConsumedQuota)
}

func TestReserveWssConsumeQuotaDoesNotWriteInsideCurrentSegment(t *testing.T) {
	billing := &recordingBillingSettler{preConsumedQuota: 500}
	info := &relaycommon.RelayInfo{
		Billing: billing,
		PriceData: hosttypes.PriceData{
			ModelRatio: 1,
			GroupRatioInfo: hosttypes.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
	}
	usage := &dto.RealtimeUsage{
		TotalTokens: 100,
		InputTokens: 100,
		InputTokenDetails: dto.InputTokenDetails{
			TextTokens: 100,
		},
	}

	err := ReserveWssConsumeQuota(
		&gin.Context{},
		info,
		"gemini-live-test",
		usage,
	)

	require.NoError(t, err)
	assert.Empty(t, billing.reserveTargets)
	assert.Equal(t, 500, billing.preConsumedQuota)
}

func TestApplyWssQuotaLimitCapsOnlyExplicitFailedReservation(t *testing.T) {
	c := &gin.Context{}
	quota, capped := applyWssQuotaLimit(c, 750)
	assert.Equal(t, 750, quota)
	assert.False(t, capped)

	common.SetContextKey(c, constant.ContextKeyRealtimeQuotaLimit, 500)
	quota, capped = applyWssQuotaLimit(c, 750)
	assert.Equal(t, 500, quota)
	assert.True(t, capped)

	quota, capped = applyWssQuotaLimit(c, 400)
	assert.Equal(t, 400, quota)
	assert.False(t, capped)
}

func TestCalculateWssUsageQuotaIncludesFixedModelPrice(t *testing.T) {
	previousQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500_000
	t.Cleanup(func() {
		common.QuotaPerUnit = previousQuotaPerUnit
	})
	info := &relaycommon.RelayInfo{
		PriceData: hosttypes.PriceData{
			UsePrice:   true,
			ModelPrice: 0.02,
			GroupRatioInfo: hosttypes.GroupRatioInfo{
				GroupRatio: 1.5,
			},
		},
	}

	quota, _ := calculateWssUsageQuota(
		info,
		"gemini-live-test",
		&dto.RealtimeUsage{TotalTokens: 1},
	)

	assert.Equal(t, 15_000, quota)
}

func TestPostWssConsumeQuotaReturnsSettlementError(t *testing.T) {
	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() {
		common.LogConsumeEnabled = previousLogConsumeEnabled
	})
	settleErr := errors.New("settlement failed")
	info := &relaycommon.RelayInfo{
		Billing:     &failingWssBilling{err: settleErr},
		ChannelMeta: &relaycommon.ChannelMeta{},
		StartTime:   time.Now(),
	}

	err := PostWssConsumeQuota(
		&gin.Context{},
		info,
		"gemini-live-test",
		&dto.RealtimeUsage{},
		"",
	)

	require.ErrorIs(t, err, settleErr)
}
