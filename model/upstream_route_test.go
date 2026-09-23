package model

import (
	"database/sql"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupUpstreamRouteTest(t *testing.T) {
	t.Helper()
	originalDB := DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&UpstreamManagedRoute{}))
	DB = db
	t.Cleanup(func() {
		DB = originalDB
	})
}

func TestRecordUpstreamRouteFailure(t *testing.T) {
	setupUpstreamRouteTest(t)
	route := UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "group",
		Platform:        "openai",
		Protocol:        UpstreamProtocolOpenAI,
		ChannelID:       101,
		State:           UpstreamRouteStateActive,
	}
	require.NoError(t, DB.Create(&route).Error)

	t.Run("quarantines only after two failures inside five minutes", func(t *testing.T) {
		first, quarantined, err := RecordUpstreamRouteFailure(101, 1000, 300, 2, "first")
		require.NoError(t, err)
		assert.False(t, quarantined)
		assert.Equal(t, 1, first.ConsecutiveFailures)
		assert.Equal(t, UpstreamRouteStateActive, first.State)

		second, quarantined, err := RecordUpstreamRouteFailure(101, 1299, 300, 2, "second")
		require.NoError(t, err)
		assert.True(t, quarantined)
		assert.Equal(t, 2, second.ConsecutiveFailures)
		assert.Equal(t, UpstreamRouteStateQuarantined, second.State)
	})
}

func TestRecordUpstreamRouteSuccess(t *testing.T) {
	setupUpstreamRouteTest(t)
	route := UpstreamManagedRoute{
		SourceID:            1,
		ExternalGroupID:     "group",
		Platform:            "openai",
		Protocol:            UpstreamProtocolOpenAI,
		ChannelID:           102,
		State:               UpstreamRouteStateActive,
		ConsecutiveFailures: 1,
		FailureWindowStart:  1000,
	}
	require.NoError(t, DB.Create(&route).Error)

	updated, err := RecordUpstreamRouteSuccess(102, 1100, 1250)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Zero(t, updated.ConsecutiveFailures)
	assert.Zero(t, updated.FailureWindowStart)
	assert.Equal(t, int64(1250), updated.LastLatencyMS)
}

func TestRecordUpstreamRouteFailureRestartsOutsideWindow(t *testing.T) {
	setupUpstreamRouteTest(t)
	route := UpstreamManagedRoute{
		SourceID:        2,
		ExternalGroupID: "window-reset",
		Platform:        "openai",
		Protocol:        UpstreamProtocolOpenAI,
		ChannelID:       103,
		State:           UpstreamRouteStateActive,
	}
	require.NoError(t, DB.Create(&route).Error)

	_, quarantined, err := RecordUpstreamRouteFailure(103, 1000, 300, 2, "first")
	require.NoError(t, err)
	assert.False(t, quarantined)

	updated, quarantined, err := RecordUpstreamRouteFailure(103, 1301, 300, 2, "outside window")
	require.NoError(t, err)
	assert.False(t, quarantined)
	assert.Equal(t, 1, updated.ConsecutiveFailures)
	assert.Equal(t, int64(1301), updated.FailureWindowStart)
	assert.Equal(t, UpstreamRouteStateActive, updated.State)
}

func TestIsolateManagedRouteModelLocksSourceGroupRouteChannel(t *testing.T) {
	setupUpstreamRouteTest(t)
	require.NoError(t, DB.AutoMigrate(
		&UpstreamSource{},
		&UpstreamGroup{},
		&Channel{},
		&Ability{},
		&Option{},
	))
	source := UpstreamSource{
		Key:        "source",
		Name:       "Source",
		ConsoleURL: "https://example.com",
		Enabled:    true,
	}
	require.NoError(t, DB.Create(&source).Error)
	group := UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group",
		Name:                "Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		Models:              `["gpt-a","gpt-b"]`,
	}
	require.NoError(t, DB.Create(&group).Error)
	channel := Channel{
		Name:   "managed",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-a,gpt-b",
		Group:  "default",
	}
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := UpstreamManagedRoute{
		SourceID:        source.ID,
		ExternalGroupID: group.ExternalID,
		Platform:        group.Platform,
		Protocol:        UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           UpstreamRouteStateActive,
	}
	require.NoError(t, DB.Create(&route).Error)
	const optionKey = "test.managed_model_exclusions"
	require.NoError(t, DB.Create(&Option{
		Key:   optionKey,
		Value: `{"source:other":["gpt-existing"]}`,
	}).Error)

	var transactionalReads []string
	const callbackName = "test:capture_managed_model_isolation_lock_order"
	require.NoError(t, DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil {
			return
		}
		if _, inTransaction := tx.Statement.ConnPool.(*sql.Tx); !inTransaction {
			return
		}
		switch tx.Statement.Table {
		case "upstream_sources", "upstream_groups", "upstream_managed_routes", "channels", "options":
			transactionalReads = append(transactionalReads, tx.Statement.Table)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, DB.Callback().Query().Remove(callbackName))
	})

	isolated, optionValue, err := IsolateManagedRouteModel(
		&route,
		"gpt-a",
		"status_code=404",
		1_788_320_000,
		optionKey,
	)

	require.NoError(t, err)
	require.True(t, isolated)
	assert.Equal(t, []string{
		"upstream_sources",
		"upstream_groups",
		"upstream_managed_routes",
		"channels",
		"options",
	}, transactionalReads)

	var storedChannel Channel
	require.NoError(t, DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, "gpt-b", storedChannel.Models)
	var abilities []Ability
	require.NoError(t, DB.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	assert.Equal(t, "gpt-b", abilities[0].Model)
	assert.True(t, abilities[0].Enabled)
	var storedGroup UpstreamGroup
	require.NoError(t, DB.First(&storedGroup, group.ID).Error)
	assert.Equal(t, `["gpt-b"]`, storedGroup.Models)
	var storedRoute UpstreamManagedRoute
	require.NoError(t, DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, int64(1_788_320_000), storedRoute.LastFailureAt)
	assert.Equal(t, "status_code=404", storedRoute.LastReason)
	assert.Equal(t, int64(1_788_320_000), storedRoute.UpdatedAt)
	var exclusions map[string][]string
	require.NoError(t, common.UnmarshalJsonStr(optionValue, &exclusions))
	assert.Equal(t, []string{"gpt-a"}, exclusions["source:group"])
	assert.Equal(t, []string{"gpt-existing"}, exclusions["source:other"])
}
