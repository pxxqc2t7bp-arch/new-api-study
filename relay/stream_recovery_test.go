package relay

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRelayStreamRecoveryWriter(t *testing.T) *service.StreamRecoveryWriter {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	keyPath := filepath.Join(t.TempDir(), "recovery.keys")
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	require.NoError(t, os.WriteFile(keyPath, []byte("current:"+key+"\n"), 0o600))
	keyring, err := service.LoadStreamRecoveryKeyring(keyPath)
	require.NoError(t, err)
	store, err := service.NewStreamRecoveryStore(
		client,
		keyring,
		service.StreamRecoveryStoreConfig{},
	)
	require.NoError(t, err)
	return service.NewStreamRecoveryWriter(
		context.Background(),
		store,
		"stream-relay-test",
		1,
		nil,
	)
}

func TestStreamRecoveryTerminalErrorRequiresProtocolTerminal(t *testing.T) {
	context, _ := gin.CreateTestContext(nil)
	writer := newRelayStreamRecoveryWriter(t)
	context.Writer = writer
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryWorker, true)
	common.SetContextKey(context, constant.ContextKeyStreamRecoveryBroker, writer)
	info := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)

	apiError := streamRecoveryTerminalError(context, info)
	require.NotNil(t, apiError)
	assert.Equal(t, 502, apiError.StatusCode)

	_, err := writer.WriteString(
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
	)
	require.NoError(t, err)
	writer.Flush()
	assert.Nil(t, streamRecoveryTerminalError(context, info))
}

func TestStreamRecoveryTerminalErrorDoesNotAffectOrdinaryStreams(t *testing.T) {
	context, _ := gin.CreateTestContext(nil)
	info := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, os.ErrClosed)

	assert.Nil(t, streamRecoveryTerminalError(context, info))
}
