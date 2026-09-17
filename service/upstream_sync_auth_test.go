package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupUpstreamSyncAuthTest(t *testing.T) {
	t.Helper()
	originalDB := model.DB
	originalSessionSecret := common.SessionSecret
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.UpstreamSyncDevice{},
		&model.UpstreamSyncCommand{},
	))
	model.DB = db
	common.SessionSecret = "upstream-sync-test-secret"
	t.Cleanup(func() {
		model.DB = originalDB
		common.SessionSecret = originalSessionSecret
	})
}

func TestClaimPendingUpstreamSyncCommands(t *testing.T) {
	setupUpstreamSyncAuthTest(t)
	command, err := model.CreateUpstreamSyncCommand("", "sync", "", nil)
	require.NoError(t, err)

	claimed, err := model.ClaimPendingUpstreamSyncCommands("device-a", 20)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, "device-a", claimed[0].DeviceID)

	stored, err := model.GetUpstreamSyncCommand(command.CommandID)
	require.NoError(t, err)
	assert.Equal(t, "device-a", stored.DeviceID)
	assert.Equal(t, model.UpstreamSyncCommandRunning, stored.Status)
}

func TestCreateUpstreamPairingCode(t *testing.T) {
	setupUpstreamSyncAuthTest(t)

	t.Run("multiple pending devices do not collide on empty token hashes", func(t *testing.T) {
		first, firstCode, err := CreateUpstreamPairingCode("Chrome A")
		require.NoError(t, err)
		second, secondCode, err := CreateUpstreamPairingCode("Chrome B")
		require.NoError(t, err)

		assert.NotEqual(t, first.DeviceID, second.DeviceID)
		assert.NotEqual(t, firstCode, secondCode)
		assert.Empty(t, first.TokenHash)
		assert.Empty(t, second.TokenHash)
	})
}

func TestPairUpstreamSyncDevice(t *testing.T) {
	setupUpstreamSyncAuthTest(t)

	t.Run("pairing is one time and multiple active devices are supported", func(t *testing.T) {
		first, firstCode, err := CreateUpstreamPairingCode("Chrome A")
		require.NoError(t, err)
		second, secondCode, err := CreateUpstreamPairingCode("Chrome B")
		require.NoError(t, err)

		pairedFirst, firstToken, err := PairUpstreamSyncDevice(firstCode, "")
		require.NoError(t, err)
		pairedSecond, secondToken, err := PairUpstreamSyncDevice(secondCode, "")
		require.NoError(t, err)
		assert.Equal(t, model.UpstreamSyncDeviceActive, pairedFirst.Status)
		assert.Equal(t, model.UpstreamSyncDeviceActive, pairedSecond.Status)
		assert.Equal(t, second.DeviceID, pairedSecond.DeviceID)
		assert.NotEqual(t, firstToken, secondToken)

		_, _, err = PairUpstreamSyncDevice(firstCode, "")
		assert.ErrorIs(t, err, ErrUpstreamPairingInvalid)

		authenticated, err := AuthenticateUpstreamSyncDevice(firstToken)
		require.NoError(t, err)
		assert.Equal(t, first.DeviceID, authenticated.DeviceID)
	})
}
