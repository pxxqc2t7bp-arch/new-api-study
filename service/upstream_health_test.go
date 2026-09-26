package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestGetManagedRouteAdminInfoUsesNamingStrategy(t *testing.T) {
	originalDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: "prefixed_"},
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.UpstreamSource{},
		&model.UpstreamGroup{},
		&model.UpstreamManagedRoute{},
	))
	model.DB = db

	const channelID = 240923
	managedRouteAdminCache.Delete(channelID)
	t.Cleanup(func() {
		managedRouteAdminCache.Delete(channelID)
		model.DB = originalDB
	})

	source := model.UpstreamSource{
		Key:              "prefixed-source",
		Name:             "Prefixed Source",
		ConsoleURL:       "https://console.example.com",
		SelectedEndpoint: "https://api.example.com",
		Status:           model.UpstreamHealthOperational,
		Enabled:          true,
	}
	require.NoError(t, db.Create(&source).Error)

	availability := 99.75
	observedAt := time.Now().Unix() - 15
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "prefixed-group",
		Name:                "Prefixed Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.25,
		HealthStatus:        model.UpstreamHealthOperational,
		Availability:        &availability,
		ObservedAt:          observedAt,
	}
	require.NoError(t, db.Create(&group).Error)

	route := model.UpstreamManagedRoute{
		SourceID:            source.ID,
		ExternalGroupID:     group.ExternalID,
		Platform:            group.Platform,
		Protocol:            model.UpstreamProtocolOpenAI,
		ChannelID:           channelID,
		State:               model.UpstreamRouteStateActive,
		EffectiveMultiplier: group.EffectiveMultiplier,
	}
	require.NoError(t, db.Create(&route).Error)

	before := time.Now().Unix()
	info := GetManagedRouteAdminInfo(channelID)
	after := time.Now().Unix()

	require.NotNil(t, info)
	healthSampleAge, ok := info["health_sample_age"].(int64)
	require.True(t, ok)
	assert.GreaterOrEqual(t, healthSampleAge, before-observedAt)
	assert.LessOrEqual(t, healthSampleAge, after-observedAt)
	assert.Equal(t, map[string]any{
		"source":               source.Key,
		"group":                group.Name,
		"external_group_id":    group.ExternalID,
		"protocol":             route.Protocol,
		"state":                route.State,
		"effective_multiplier": route.EffectiveMultiplier,
		"selected_endpoint":    source.SelectedEndpoint,
		"health_status":        group.HealthStatus,
		"health_sample_age":    healthSampleAge,
		"availability":         availability,
	}, info)
}
