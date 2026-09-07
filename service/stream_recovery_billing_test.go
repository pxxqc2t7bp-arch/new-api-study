package service

import (
	"testing"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func useStreamBillingDatabase(t *testing.T) {
	t.Helper()
	previous := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.StreamExecution{}))
	model.DB = database
	t.Cleanup(func() { model.DB = previous })
}

func createStreamBillingExecution(t *testing.T, suffix string, status string) *model.StreamExecution {
	t.Helper()
	execution, created, err := model.CreateOrGetStreamExecution(&model.StreamExecution{
		StreamID:      "stream_" + suffix,
		DedupeKey:     "dedupe_" + suffix,
		UserID:        11,
		TokenID:       22,
		ModelName:     "glm-5.3",
		RelayFormat:   "openai_responses",
		RequestPath:   "/v1/responses",
		RequestDigest: "digest_" + suffix,
		BillingStatus: status,
		BillingSource: BillingSourceWallet,
		ReservedQuota: 42,
		TokenConsumed: 42,
		ExpiresAt:     2_000_000_000,
	})
	require.NoError(t, err)
	require.True(t, created)
	return execution
}

func TestBeginStreamBillingReservationIsCompareAndSwap(t *testing.T) {
	useStreamBillingDatabase(t)
	execution := createStreamBillingExecution(t, "reserve", model.StreamBillingNone)

	_, shouldReserve, err := beginStreamBillingReservation(execution.StreamID)
	require.NoError(t, err)
	assert.True(t, shouldReserve)

	_, _, err = beginStreamBillingReservation(execution.StreamID)
	require.ErrorContains(t, err, model.StreamBillingReserving)
}

func TestRestoreStreamBillingSessionSettlesOnlyOnce(t *testing.T) {
	useStreamBillingDatabase(t)
	execution := createStreamBillingExecution(t, "restore", model.StreamBillingReserved)
	relayInfo := &relaycommon.RelayInfo{
		RequestId:       execution.StreamID,
		UserId:          execution.UserID,
		TokenId:         execution.TokenID,
		TokenKey:        "token-key",
		OriginModelName: execution.ModelName,
	}

	session, err := restoreStreamBillingSession(execution, relayInfo)
	require.NoError(t, err)
	assert.Equal(t, 42, session.GetPreConsumedQuota())
	require.NoError(t, session.Settle(42))

	updated, err := model.GetStreamExecution(execution.StreamID)
	require.NoError(t, err)
	assert.Equal(t, model.StreamBillingSettled, updated.BillingStatus)
	assert.Equal(t, 42, updated.ActualQuota)

	require.NoError(t, session.Settle(42))
	updated, err = model.GetStreamExecution(execution.StreamID)
	require.NoError(t, err)
	assert.Equal(t, model.StreamBillingSettled, updated.BillingStatus)
}
