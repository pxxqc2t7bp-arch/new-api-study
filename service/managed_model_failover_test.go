package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type managedModelIsolationFixture struct {
	db                 *gorm.DB
	source             model.UpstreamSource
	group              model.UpstreamGroup
	channel            model.Channel
	route              model.UpstreamManagedRoute
	initialOptionValue string
}

func setupManagedModelIsolationFixture(t *testing.T) managedModelIsolationFixture {
	t.Helper()
	db := setupChannelSelectAutoGroupsTest(t)
	require.NoError(t, db.AutoMigrate(
		&model.Option{},
		&model.UpstreamSource{},
		&model.UpstreamGroup{},
		&model.UpstreamManagedRoute{},
	))

	common.OptionMapRWMutex.Lock()
	originalOptionMap := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = originalOptionMap
		common.OptionMapRWMutex.Unlock()
	})

	originalExclusions, err := common.Marshal(
		operation_setting.GetUpstreamOrchestrationSetting().ModelExclusions,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, model.UpdateOption(
			managedModelExclusionsOption,
			string(originalExclusions),
		))
	})

	initialOptionValue := `{"source:other":["gpt-existing"]}`
	require.NoError(t, model.UpdateOption(
		managedModelExclusionsOption,
		initialOptionValue,
	))

	source := model.UpstreamSource{
		Key:        "source",
		Name:       "Source",
		ConsoleURL: "https://example.com",
		Enabled:    true,
	}
	require.NoError(t, db.Create(&source).Error)
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          "group",
		Name:                "Group",
		Platform:            "openai",
		EffectiveMultiplier: 0.1,
		Models:              `["gpt-remove","gpt-keep"]`,
	}
	require.NoError(t, db.Create(&group).Error)
	channel := model.Channel{
		Name:   "managed-isolation",
		Key:    "credential",
		Status: common.ChannelStatusEnabled,
		Models: "gpt-remove,gpt-keep",
		Group:  "default",
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	route := model.UpstreamManagedRoute{
		SourceID:        source.ID,
		ExternalGroupID: group.ExternalID,
		Platform:        group.Platform,
		Protocol:        model.UpstreamProtocolOpenAI,
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateActive,
		Rank:            7,
		UpdatedAt:       1_788_319_000,
	}
	require.NoError(t, db.Create(&route).Error)
	require.NoError(t, db.First(&route, route.ID).Error)
	model.InitChannelCache()
	managedRouteAdminCache.Store(channel.Id, managedRouteAdminCacheEntry{
		info:      map[string]any{"state": "cached"},
		expiresAt: time.Now().Add(time.Hour).Unix(),
	})
	t.Cleanup(func() {
		managedRouteAdminCache.Delete(channel.Id)
	})

	return managedModelIsolationFixture{
		db:                 db,
		source:             source,
		group:              group,
		channel:            channel,
		route:              route,
		initialOptionValue: initialOptionValue,
	}
}

func assertManagedModelIsolationUnchanged(t *testing.T, fixture managedModelIsolationFixture) {
	t.Helper()
	var storedOption model.Option
	require.NoError(t, fixture.db.First(
		&storedOption,
		"key = ?",
		managedModelExclusionsOption,
	).Error)
	assert.Equal(t, fixture.initialOptionValue, storedOption.Value)
	common.OptionMapRWMutex.RLock()
	inMemoryOption := common.OptionMap[managedModelExclusionsOption]
	common.OptionMapRWMutex.RUnlock()
	assert.Equal(t, fixture.initialOptionValue, inMemoryOption)
	assert.False(t, managedModelExcluded(
		fixture.source.Key,
		fixture.group.ExternalID,
		"gpt-remove",
		"gpt-remove",
		operation_setting.GetUpstreamOrchestrationSetting().ModelExclusions,
	))

	var storedGroup model.UpstreamGroup
	require.NoError(t, fixture.db.First(&storedGroup, fixture.group.ID).Error)
	assert.Equal(t, fixture.group.Models, storedGroup.Models)
	var storedChannel model.Channel
	require.NoError(t, fixture.db.First(&storedChannel, fixture.channel.Id).Error)
	assert.Equal(t, fixture.channel.Models, storedChannel.Models)
	var abilities []model.Ability
	require.NoError(t, fixture.db.Where("channel_id = ?", fixture.channel.Id).
		Order("model asc").
		Find(&abilities).Error)
	require.Len(t, abilities, 2)
	assert.Equal(t, "gpt-keep", abilities[0].Model)
	assert.Equal(t, "gpt-remove", abilities[1].Model)
	assert.True(t, abilities[0].Enabled)
	assert.True(t, abilities[1].Enabled)
	cached, err := model.CacheGetChannel(fixture.channel.Id)
	require.NoError(t, err)
	assert.Equal(t, fixture.channel.Models, cached.Models)
	_, adminCacheExists := managedRouteAdminCache.Load(fixture.channel.Id)
	assert.True(t, adminCacheExists)
}

func TestManagedSourceDiversityWithinSamePriority(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	require.NoError(t, db.AutoMigrate(&model.UpstreamManagedRoute{}))
	const modelName = "gpt-5.4"
	createChannelSelectPriorityChannel(t, db, 62, "default", modelName, 998)
	createChannelSelectPriorityChannel(t, db, 54, "default", modelName, 0)
	createChannelSelectPriorityChannel(t, db, 60, "default", modelName, 0)
	for _, route := range []model.UpstreamManagedRoute{
		{SourceID: 1, ExternalGroupID: "25", Platform: "openai", Protocol: "openai", ChannelID: 62, State: model.UpstreamRouteStateActive},
		{SourceID: 1, ExternalGroupID: "31", Platform: "openai", Protocol: "openai", ChannelID: 54, State: model.UpstreamRouteStateActive},
		{SourceID: 2, ExternalGroupID: "55", Platform: "openai", Protocol: "openai", ChannelID: 60, State: model.UpstreamRouteStateActive},
	} {
		require.NoError(t, db.Create(&route).Error)
	}
	model.InitChannelCache()

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	param := &RetryParam{
		Ctx:        ctx,
		TokenGroup: "default",
		ModelName:  modelName,
	}

	assert.Equal(t, 2, AdaptiveRetryTimes(param))
	assert.Equal(t, []int64{998, 0, 0}, param.PriorityPath)

	first, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, 62, first.Id)

	param.IncreaseRetry()
	second, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, 60, second.Id)

	param.IncreaseRetry()
	third, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, third)
	assert.Equal(t, 54, third.Id)
}

func TestManagedModelUnsupportedDetection(t *testing.T) {
	unsupported := relaytypes.NewOpenAIError(
		errors.New(`Model "gpt-5.4" is not supported by any configured account in this group`),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusNotFound,
	)
	ordinary := relaytypes.NewOpenAIError(
		errors.New("not found"),
		relaytypes.ErrorCodeBadResponseStatusCode,
		http.StatusNotFound,
	)

	assert.True(t, IsManagedModelUnsupported(unsupported))
	assert.True(t, ShouldRecordManagedRouteFailure(unsupported))
	assert.False(t, IsManagedModelUnsupported(ordinary))
	assert.False(t, ShouldRecordManagedRouteFailure(ordinary))
}

func TestIsolateManagedRouteModelOnlyRemovesFailedModel(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	require.NoError(t, db.AutoMigrate(
		&model.Option{},
		&model.UpstreamSource{},
		&model.UpstreamGroup{},
		&model.UpstreamManagedRoute{},
	))
	common.OptionMapRWMutex.Lock()
	originalOptionMap := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = originalOptionMap
		common.OptionMapRWMutex.Unlock()
	})
	originalExclusions, err := json.Marshal(
		operation_setting.GetUpstreamOrchestrationSetting().ModelExclusions,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, model.UpdateOption(
			managedModelExclusionsOption,
			string(originalExclusions),
		))
	})
	require.NoError(t, model.UpdateOption(
		managedModelExclusionsOption,
		`{"ebond:31":["gpt-existing"]}`,
	))
	priority := int64(998)
	weight := uint(100)
	channel := model.Channel{
		Id:       62,
		Status:   common.ChannelStatusEnabled,
		Name:     "ebond",
		Models:   "gpt-5.4,gpt-5.5",
		Group:    "default",
		Priority: &priority,
		Weight:   &weight,
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	require.NoError(t, db.Create(&model.UpstreamSource{
		ID:         1,
		Key:        "ebond",
		Name:       "eBond",
		ConsoleURL: "https://example.com",
	}).Error)
	group := model.UpstreamGroup{
		SourceID:            1,
		ExternalID:          "25",
		Name:                "GPT Pro",
		Platform:            "openai",
		EffectiveMultiplier: 0.2,
		HealthStatus:        model.UpstreamHealthUnknown,
		Models:              `["gpt-5.4","gpt-5.5"]`,
	}
	require.NoError(t, db.Create(&group).Error)
	require.NoError(t, db.Create(&model.UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "25",
		Platform:        "openai",
		Protocol:        "openai",
		ChannelID:       channel.Id,
		State:           model.UpstreamRouteStateActive,
	}).Error)
	model.InitChannelCache()
	managedRouteAdminCache.Store(channel.Id, managedRouteAdminCacheEntry{
		info:      map[string]any{"state": "stale"},
		expiresAt: time.Now().Add(time.Hour).Unix(),
	})
	t.Cleanup(func() {
		managedRouteAdminCache.Delete(channel.Id)
	})

	isolated, err := IsolateManagedRouteModel(
		channel.Id,
		"gpt-5.4",
		"status_code=404, unsupported",
	)
	require.NoError(t, err)
	assert.True(t, isolated)

	var storedChannel model.Channel
	require.NoError(t, db.First(&storedChannel, channel.Id).Error)
	assert.Equal(t, "gpt-5.5", storedChannel.Models)
	cachedChannel, err := model.CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.5", cachedChannel.Models)
	_, adminCacheExists := managedRouteAdminCache.Load(channel.Id)
	assert.False(t, adminCacheExists)
	var failedAbilityCount int64
	require.NoError(t, db.Model(&model.Ability{}).
		Where("channel_id = ? AND model = ?", channel.Id, "gpt-5.4").
		Count(&failedAbilityCount).Error)
	assert.Zero(t, failedAbilityCount)
	var retainedAbilityCount int64
	require.NoError(t, db.Model(&model.Ability{}).
		Where("channel_id = ? AND model = ?", channel.Id, "gpt-5.5").
		Count(&retainedAbilityCount).Error)
	assert.Equal(t, int64(1), retainedAbilityCount)

	var storedGroup model.UpstreamGroup
	require.NoError(t, db.First(&storedGroup, group.ID).Error)
	assert.Equal(t, `["gpt-5.5"]`, storedGroup.Models)

	var option model.Option
	require.NoError(t, db.First(
		&option,
		"key = ?",
		managedModelExclusionsOption,
	).Error)
	var exclusions map[string][]string
	require.NoError(t, json.Unmarshal([]byte(option.Value), &exclusions))
	assert.Equal(t, []string{"gpt-existing"}, exclusions["ebond:31"])
	assert.Equal(t, []string{"gpt-5.4"}, exclusions["ebond:25"])
	assert.True(t, managedModelExcluded(
		"ebond",
		"25",
		"gpt-5.4",
		"gpt-5.4",
		operation_setting.GetUpstreamOrchestrationSetting().ModelExclusions,
	))
	common.OptionMapRWMutex.RLock()
	inMemoryOption := common.OptionMap[managedModelExclusionsOption]
	common.OptionMapRWMutex.RUnlock()
	assert.JSONEq(t, option.Value, inMemoryOption)
}

func TestIsolateManagedRouteModelRejectsStaleRouteWithoutExclusion(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		detached bool
	}{
		{name: "paused", state: model.UpstreamRouteStatePaused},
		{name: "detached", state: model.UpstreamRouteStateDetached, detached: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := setupManagedModelIsolationFixture(t)
			const pauseUntil = int64(1_788_330_000)
			injected := false
			callbackName := "test:inject_stale_model_isolation_" + testCase.name
			require.NoError(t, fixture.db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if injected || tx.Statement == nil || tx.Statement.Table != "upstream_managed_routes" {
					return
				}
				if _, inTransaction := tx.Statement.ConnPool.(*sql.Tx); inTransaction {
					return
				}
				injected = true
				require.NoError(t, fixture.db.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
					Model(&model.UpstreamManagedRoute{}).
					Where("id = ?", fixture.route.ID).
					Updates(map[string]any{
						"state":              testCase.state,
						"detached":           testCase.detached,
						"manual_pause_until": pauseUntil,
						"last_reason":        "operator transition",
						"updated_at":         fixture.route.UpdatedAt + 1,
					}).Error)
			}))
			t.Cleanup(func() {
				require.NoError(t, fixture.db.Callback().Query().Remove(callbackName))
			})

			isolated, err := IsolateManagedRouteModel(
				fixture.channel.Id,
				"gpt-remove",
				"status_code=404",
			)

			require.NoError(t, err)
			assert.False(t, isolated)
			require.True(t, injected)
			assertManagedModelIsolationUnchanged(t, fixture)
			var storedRoute model.UpstreamManagedRoute
			require.NoError(t, fixture.db.First(&storedRoute, fixture.route.ID).Error)
			assert.Equal(t, testCase.state, storedRoute.State)
			assert.Equal(t, testCase.detached, storedRoute.Detached)
			assert.Equal(t, pauseUntil, storedRoute.ManualPauseUntil)
			assert.Equal(t, "operator transition", storedRoute.LastReason)
			assert.Equal(t, fixture.route.UpdatedAt+1, storedRoute.UpdatedAt)
			assert.Zero(t, storedRoute.LastFailureAt)
		})
	}
}

func TestIsolateManagedRouteModelPersistsExclusionWhenModelIsTemporarilyAbsent(t *testing.T) {
	fixture := setupManagedModelIsolationFixture(t)
	require.NoError(t, fixture.db.Model(&model.Channel{}).
		Where("id = ?", fixture.channel.Id).
		Update("models", "gpt-keep").Error)
	fixture.channel.Models = "gpt-keep"
	require.NoError(t, fixture.channel.UpdateAbilities(fixture.db))
	require.NoError(t, fixture.db.Model(&model.UpstreamGroup{}).
		Where("id = ?", fixture.group.ID).
		Update("models", `["gpt-keep"]`).Error)
	model.InitChannelCache()

	isolated, err := IsolateManagedRouteModel(
		fixture.channel.Id,
		"gpt-remove",
		"status_code=404",
	)

	require.NoError(t, err)
	assert.True(t, isolated)
	var option model.Option
	require.NoError(t, fixture.db.First(
		&option,
		"key = ?",
		managedModelExclusionsOption,
	).Error)
	var exclusions map[string][]string
	require.NoError(t, common.UnmarshalJsonStr(option.Value, &exclusions))
	assert.Equal(t, []string{"gpt-remove"}, exclusions["source:group"])
	assert.Equal(t, []string{"gpt-existing"}, exclusions["source:other"])
	common.OptionMapRWMutex.RLock()
	inMemoryOption := common.OptionMap[managedModelExclusionsOption]
	common.OptionMapRWMutex.RUnlock()
	assert.JSONEq(t, option.Value, inMemoryOption)
	assert.True(t, managedModelExcluded(
		fixture.source.Key,
		fixture.group.ExternalID,
		"gpt-remove",
		"gpt-remove",
		operation_setting.GetUpstreamOrchestrationSetting().ModelExclusions,
	))
	var storedChannel model.Channel
	require.NoError(t, fixture.db.First(&storedChannel, fixture.channel.Id).Error)
	assert.Equal(t, "gpt-keep", storedChannel.Models)
	var storedGroup model.UpstreamGroup
	require.NoError(t, fixture.db.First(&storedGroup, fixture.group.ID).Error)
	assert.Equal(t, `["gpt-keep"]`, storedGroup.Models)
	var abilities []model.Ability
	require.NoError(t, fixture.db.Where("channel_id = ?", fixture.channel.Id).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	assert.Equal(t, "gpt-keep", abilities[0].Model)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, fixture.db.First(&storedRoute, fixture.route.ID).Error)
	assert.Equal(t, "status_code=404", storedRoute.LastReason)
	assert.NotZero(t, storedRoute.LastFailureAt)
	cached, cacheErr := model.CacheGetChannel(fixture.channel.Id)
	require.NoError(t, cacheErr)
	assert.Equal(t, "gpt-keep", cached.Models)
	_, adminCacheExists := managedRouteAdminCache.Load(fixture.channel.Id)
	assert.False(t, adminCacheExists)
}

func TestIsolateManagedRouteModelRollsBackOnAbilityFailure(t *testing.T) {
	fixture := setupManagedModelIsolationFixture(t)
	forcedErr := errors.New("forced managed model ability failure")
	const callbackName = "test:fail_managed_model_ability"
	require.NoError(t, fixture.db.Callback().Delete().Before("gorm:delete").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "abilities" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, fixture.db.Callback().Delete().Remove(callbackName))
	})

	isolated, err := IsolateManagedRouteModel(
		fixture.channel.Id,
		"gpt-remove",
		"status_code=404",
	)

	require.ErrorIs(t, err, forcedErr)
	assert.False(t, isolated)
	assertManagedModelIsolationUnchanged(t, fixture)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, fixture.db.First(&storedRoute, fixture.route.ID).Error)
	assert.Equal(t, fixture.route, storedRoute)
}

func TestIsolateManagedRouteModelRollsBackOnOptionFailure(t *testing.T) {
	fixture := setupManagedModelIsolationFixture(t)
	forcedErr := errors.New("forced managed model option failure")
	const callbackName = "test:fail_managed_model_option"
	require.NoError(t, fixture.db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "options" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, fixture.db.Callback().Update().Remove(callbackName))
	})

	isolated, err := IsolateManagedRouteModel(
		fixture.channel.Id,
		"gpt-remove",
		"status_code=404",
	)

	require.ErrorIs(t, err, forcedErr)
	assert.False(t, isolated)
	assertManagedModelIsolationUnchanged(t, fixture)
	var storedRoute model.UpstreamManagedRoute
	require.NoError(t, fixture.db.First(&storedRoute, fixture.route.ID).Error)
	assert.Equal(t, fixture.route, storedRoute)
}
