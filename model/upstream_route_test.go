package model

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func setupUpstreamRouteTest(t *testing.T) {
	t.Helper()
	originalDB := DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}, &UpstreamManagedRoute{}))
	DB = db
	t.Cleanup(func() {
		DB = originalDB
	})
}

func TestRecordUpstreamRouteFailure(t *testing.T) {
	setupUpstreamRouteTest(t)
	channel := Channel{
		Id:     101,
		Name:   "managed-route-failure",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-a",
		Group:  "default",
	}
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "group",
		Platform:        "openai",
		Protocol:        UpstreamProtocolOpenAI,
		ChannelID:       101,
		State:           UpstreamRouteStateActive,
		Rank:            7,
	}
	require.NoError(t, DB.Create(&route).Error)

	t.Run("quarantines only after two failures inside five minutes", func(t *testing.T) {
		first, quarantined, err := RecordUpstreamRouteFailure(101, 1000, 300, 2, "first")
		require.NoError(t, err)
		assert.False(t, quarantined)
		assert.Equal(t, 1, first.ConsecutiveFailures)
		assert.Equal(t, UpstreamRouteStateActive, first.State)
		assert.Equal(t, 7, first.Rank)

		second, quarantined, err := RecordUpstreamRouteFailure(101, 1299, 300, 2, "second")
		require.NoError(t, err)
		assert.True(t, quarantined)
		assert.Equal(t, 2, second.ConsecutiveFailures)
		assert.Equal(t, UpstreamRouteStateQuarantined, second.State)
		assert.Zero(t, second.Rank)

		var stored UpstreamManagedRoute
		require.NoError(t, DB.First(&stored, route.ID).Error)
		assert.Zero(t, stored.Rank)
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

func TestRecordUpstreamRouteFailureQuarantineDisablesWholeMultiKeyChannel(t *testing.T) {
	setupUpstreamRouteTest(t)
	require.NoError(t, DB.AutoMigrate(&Channel{}, &Ability{}))
	channel := Channel{
		Name:   "managed-multi-key-failure",
		Key:    "credential-a\ncredential-b",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-a,gpt-b",
		Group:  "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       2,
			MultiKeyStatusList: map[int]int{},
			MultiKeyMode:       constant.MultiKeyModePolling,
		},
	}
	channel.SetOtherInfo(map[string]any{"owner": "managed"})
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := UpstreamManagedRoute{
		SourceID:        3,
		ExternalGroupID: "multi-key-failure",
		Platform:        "openai",
		Protocol:        UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           UpstreamRouteStateActive,
		Rank:            7,
	}
	require.NoError(t, DB.Create(&route).Error)

	updated, quarantined, err := RecordUpstreamRouteFailure(
		channel.Id,
		1_788_320_000,
		300,
		1,
		"upstream failed",
	)

	require.NoError(t, err)
	require.True(t, quarantined)
	assert.Equal(t, UpstreamRouteStateQuarantined, updated.State)
	assert.Zero(t, updated.Rank)
	var storedChannel Channel
	require.NoError(t, DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedChannel.Status)
	assert.Empty(t, storedChannel.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, "upstream failed", storedChannel.GetOtherInfo()["status_reason"])
	var abilities []Ability
	require.NoError(t, DB.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
	require.Len(t, abilities, 2)
	for _, ability := range abilities {
		assert.False(t, ability.Enabled)
	}
}

func TestRecordUpstreamRouteFailureQuarantineRollsBackOnAbilityFailure(t *testing.T) {
	setupUpstreamRouteTest(t)
	require.NoError(t, DB.AutoMigrate(&Channel{}, &Ability{}))
	channel := Channel{
		Name:   "managed-failure-rollback",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-a",
		Group:  "default",
	}
	channel.SetOtherInfo(map[string]any{"owner": "before"})
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := UpstreamManagedRoute{
		SourceID:            4,
		ExternalGroupID:     "failure-rollback",
		Platform:            "openai",
		Protocol:            UpstreamProtocolOpenAI,
		ChannelID:           channel.Id,
		State:               UpstreamRouteStateActive,
		Rank:                7,
		ConsecutiveFailures: 1,
		FailureWindowStart:  1_788_319_900,
		UpdatedAt:           1_788_319_900,
	}
	require.NoError(t, DB.Create(&route).Error)
	var expectedRoute UpstreamManagedRoute
	require.NoError(t, DB.First(&expectedRoute, route.ID).Error)

	forcedErr := errors.New("forced request failure ability error")
	const callbackName = "test:fail_request_failure_ability"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "abilities" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, DB.Callback().Update().Remove(callbackName))
	})

	_, _, err := RecordUpstreamRouteFailure(
		channel.Id,
		1_788_320_000,
		300,
		2,
		"upstream failed",
	)

	require.ErrorIs(t, err, forcedErr)
	var storedRoute UpstreamManagedRoute
	require.NoError(t, DB.First(&storedRoute, route.ID).Error)
	assert.Equal(t, expectedRoute, storedRoute)
	var storedChannel Channel
	require.NoError(t, DB.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, channel.Status, storedChannel.Status)
	assert.Equal(t, channel.OtherInfo, storedChannel.OtherInfo)
	var ability Ability
	require.NoError(t, DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.True(t, ability.Enabled)
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
		"options",
		"upstream_sources",
		"upstream_groups",
		"upstream_managed_routes",
		"channels",
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

func TestIsolateManagedRouteModelCreatesMissingOptionWithConflictSafeInsert(t *testing.T) {
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
		Models:              `["gpt-a"]`,
	}
	require.NoError(t, DB.Create(&group).Error)
	channel := Channel{
		Name:   "managed",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-a",
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

	conflictSafeInsert := false
	const callbackName = "test:capture_managed_model_option_create"
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "options" {
			return
		}
		onConflict, ok := tx.Statement.Clauses["ON CONFLICT"]
		if !ok {
			return
		}
		expression, ok := onConflict.Expression.(clause.OnConflict)
		conflictSafeInsert = ok && expression.DoNothing
	}))
	t.Cleanup(func() {
		require.NoError(t, DB.Callback().Create().Remove(callbackName))
	})

	isolated, optionValue, err := IsolateManagedRouteModel(
		&route,
		"gpt-a",
		"status_code=404",
		1_788_320_000,
		"test.missing_managed_model_exclusions",
	)

	require.NoError(t, err)
	assert.True(t, isolated)
	assert.True(t, conflictSafeInsert)
	assert.JSONEq(t, `{"source:group":["gpt-a"]}`, optionValue)
}
