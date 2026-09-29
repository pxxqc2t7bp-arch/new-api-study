package model

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openModelPricingInstanceDatabases(t *testing.T) (*gorm.DB, *gorm.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model-pricing.sqlite")
	dsn := "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL"
	first, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	second, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, first.AutoMigrate(
		&Option{},
		&UpstreamPriceEvidence{},
		&Vendor{},
		&Model{},
		&Channel{},
		&Ability{},
	))
	for _, database := range []*gorm.DB{first, second} {
		sqlDB, err := database.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	}
	return first, second
}

func modelPricingCASEvidence(modelName, hashCharacter string) UpstreamPriceEvidence {
	return UpstreamPriceEvidence{
		Vendor:          "test",
		ModelName:       modelName,
		Currency:        "USD",
		Unit:            "per_1m_tokens",
		NormalizedPrice: `{"input_per_m":1}`,
		SourceURL:       "https://example.invalid/pricing",
		EvidenceHash:    strings.Repeat(hashCharacter, 64),
		Status:          UpstreamPriceStatusApplied,
	}
}

func TestUpdateModelPricingDatabaseUsesDurableCrossInstanceCAS(t *testing.T) {
	first, second := openModelPricingInstanceDatabases(t)
	emptyVersion := ModelPricingVersion(PricingValues{})

	t.Run("same model stale version commits once with evidence", func(t *testing.T) {
		const modelName = "cross-instance-model"
		results := make(chan error, 2)
		var ready sync.WaitGroup
		ready.Add(2)
		start := make(chan struct{})
		for index, instance := range []*gorm.DB{first, second} {
			go func(index int, instance *gorm.DB) {
				ready.Done()
				<-start
				evidence := modelPricingCASEvidence(modelName, string(rune('a'+index)))
				_, err := updateModelPricingDatabase(instance, []ModelPricingChange{{
					ModelName:       modelName,
					ExpectedVersion: emptyVersion,
					Pricing:         PricingValues{"ModelRatio": float64(index + 1)},
				}}, func(tx *gorm.DB) error {
					return tx.Create(&evidence).Error
				})
				results <- err
			}(index, instance)
		}
		ready.Wait()
		close(start)

		successes, conflicts := 0, 0
		for range 2 {
			err := <-results
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrModelPricingConflict):
				conflicts++
			default:
				require.NoError(t, err)
			}
		}
		assert.Equal(t, 1, successes)
		assert.Equal(t, 1, conflicts)

		var evidenceCount int64
		require.NoError(t, first.Model(&UpstreamPriceEvidence{}).Where("model_name = ?", modelName).Count(&evidenceCount).Error)
		assert.Equal(t, int64(1), evidenceCount)
		var revision Option
		require.NoError(t, first.Where("key = ?", modelPricingRevisionOptionKey).First(&revision).Error)
		assert.NotEmpty(t, revision.Value)
	})

	t.Run("unrelated models both survive a CAS retry", func(t *testing.T) {
		results := make(chan error, 2)
		var ready sync.WaitGroup
		ready.Add(2)
		start := make(chan struct{})
		for index, instance := range []*gorm.DB{first, second} {
			go func(index int, instance *gorm.DB) {
				ready.Done()
				<-start
				_, err := updateModelPricingDatabase(instance, []ModelPricingChange{{
					ModelName:       "unrelated-" + string(rune('a'+index)),
					ExpectedVersion: emptyVersion,
					Pricing:         PricingValues{"ModelRatio": float64(index + 3)},
				}}, nil)
				results <- err
			}(index, instance)
		}
		ready.Wait()
		close(start)
		for range 2 {
			require.NoError(t, <-results)
		}

		var option Option
		require.NoError(t, first.Where("key = ?", "ModelRatio").First(&option).Error)
		var ratios map[string]float64
		require.NoError(t, common.UnmarshalJsonStr(option.Value, &ratios))
		assert.Equal(t, float64(3), ratios["unrelated-a"])
		assert.Equal(t, float64(4), ratios["unrelated-b"])
	})
}

func TestValidateModelPricingRequiresUsableMainExpressionWithPluginVariants(t *testing.T) {
	const modelName = "main-expression-source-of-truth"
	for _, spec := range []struct {
		key, field, unit string
	}{
		{"main-expression-alpha", "seconds", "second"},
		{"main-expression-beta", "credits", "credit"},
	} {
		source := fmt.Sprintf(`
export const meta = {
  apiVersion: 1, key: %q, name: %q, version: "1.0.0", author: {name: "Test"},
  models: [%q], fetchMode: "per_task",
  usageSchema: {%s: {type: "number", unit: %q}}
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`, spec.key, spec.key, modelName, spec.field, spec.unit)
		_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
		require.NoError(t, err)
		t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(spec.key) })
	}
	variants := map[string]any{
		"main-expression-alpha": `tier("alpha", u("seconds") * 1)`,
		"main-expression-beta":  `tier("beta", u("credits") * 1)`,
	}
	for _, test := range []struct {
		name, expression string
	}{
		{"empty", ""},
		{"undeclared usage key", `tier("main", u("undeclared") * 1)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateModelPricing(modelName, PricingValues{
				"billing_setting.billing_mode":          billing_setting.BillingModeTieredExpr,
				"billing_setting.billing_expr":          test.expression,
				billing_setting.PluginBillingExprOption: variants,
			})
			require.Error(t, err)
		})
	}
}

func TestValidateModelPricingRejectsUnusableUnchangedMainExpressionAfterProviderChanges(t *testing.T) {
	const (
		modelName  = "main-expression-provider-change"
		expression = `tier("main", u("seconds") * 1)`
	)
	previous := PricingValues{
		"billing_setting.billing_mode": billing_setting.BillingModeTieredExpr,
		"billing_setting.billing_expr": expression,
	}

	t.Run("all providers disappear", func(t *testing.T) {
		err := validateModelPricing(modelName, previous, previous, nil)
		require.ErrorContains(t, err, "no task plugin usage schema")
	})

	t.Run("provider returns with incompatible schema", func(t *testing.T) {
		const pluginKey = "main-return-provider"
		source := fmt.Sprintf(`
export const meta = {
  apiVersion: 1, key: %q, name: %q, version: "1.0.0", author: {name: "Test"},
  models: [%q], fetchMode: "per_task",
  usageSchema: {credits: {type: "number", unit: "credit"}}
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`, pluginKey, pluginKey, modelName)
		_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
		require.NoError(t, err)
		t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(pluginKey) })

		err = validateModelPricing(modelName, previous, previous, nil)
		require.ErrorContains(t, err, `usage key "seconds" is not declared`)
	})
}

func TestUpdateModelPricingOptionsRejectsRemovingActiveAliasVariant(t *testing.T) {
	database, _ := openModelPricingInstanceDatabases(t)
	previousDB := DB
	previousOptions := common.OptionMap
	previousConfig := config.GlobalConfig.ExportAllConfigs()
	previousAliasView := taskAliasViewPtr.Load()
	restoreRatios := []struct {
		value   string
		restore func(string) error
	}{
		{ratio_setting.ModelPrice2JSONString(), ratio_setting.UpdateModelPriceByJSONString},
		{ratio_setting.ModelRatio2JSONString(), ratio_setting.UpdateModelRatioByJSONString},
		{ratio_setting.CompletionRatio2JSONString(), ratio_setting.UpdateCompletionRatioByJSONString},
		{ratio_setting.CacheRatio2JSONString(), ratio_setting.UpdateCacheRatioByJSONString},
		{ratio_setting.CreateCacheRatio2JSONString(), ratio_setting.UpdateCreateCacheRatioByJSONString},
		{ratio_setting.ImageRatio2JSONString(), ratio_setting.UpdateImageRatioByJSONString},
		{ratio_setting.AudioRatio2JSONString(), ratio_setting.UpdateAudioRatioByJSONString},
		{ratio_setting.AudioCompletionRatio2JSONString(), ratio_setting.UpdateAudioCompletionRatioByJSONString},
	}
	DB = database
	common.OptionMap = maps.Clone(previousOptions)
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	t.Cleanup(func() {
		for _, ratio := range restoreRatios {
			require.NoError(t, ratio.restore(ratio.value))
		}
		require.NoError(t, config.GlobalConfig.LoadFromDB(previousConfig))
		taskAliasViewPtr.Store(previousAliasView)
		common.OptionMap = previousOptions
		DB = previousDB
	})

	const (
		alias       = "seedream-alias-variant-test"
		canonical   = "seedream-canonical-variant-test"
		pluginKey   = "seedream-alias-variant-plugin"
		mainExpr    = `tier("request", fixed(0.01))`
		variantExpr = `tier("task", u("seconds") * 1)`
	)
	source := fmt.Sprintf(`
export const meta = {
  apiVersion: 1, key: %q, name: %q, version: "1.0.0", author: {name: "Test"},
  models: [%q], fetchMode: "per_task",
  usageSchema: {credits: {type: "number", unit: "credit"}},
  usageExamples: [{label: "default credit", facts: {credits: 1}}],
  usageProfiles: [{
    models: [%q],
    schema: {seconds: {type: "number", unit: "second"}},
    examples: [{label: "canonical second", facts: {seconds: 1}}]
  }]
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`, pluginKey, pluginKey, canonical, canonical)
	_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(pluginKey) })
	plugin, exists := jsplugin.DefaultRegistry.Generation().Get(pluginKey)
	require.True(t, exists)
	schema, examples := plugin.Meta.UsageForModel(canonical)
	require.NoError(t, billing_setting.SmokeTestExpr(mainExpr))
	require.ErrorContains(t, billing_setting.SmokeTestTaskExpr(mainExpr, schema), "fixed pricing is not supported")
	require.NoError(t, billing_setting.SmokeTestTaskExpr(variantExpr, schema))

	mapping := fmt.Sprintf(`{%q:%q}`, alias, canonical)
	channel := Channel{
		Id: 940, Status: common.ChannelStatusEnabled, Name: "seedream alias variant",
		Models: alias, ModelMapping: &mapping,
	}
	require.NoError(t, database.Create(&channel).Error)
	rebuildTaskAliasView()
	target, resolved := ResolveTaskModelAlias(jsplugin.DefaultRegistry.Generation(), alias)
	require.True(t, resolved)
	assert.Equal(t, TaskAliasTarget{Alias: alias, Declared: canonical, PluginKey: pluginKey}, target)

	initial, err := GetModelPricingSnapshot([]string{alias})
	require.NoError(t, err)
	require.NoError(t, UpdateModelPricing([]ModelPricingChange{{
		ModelName:       alias,
		ExpectedVersion: initial.Entries[0].Version,
		Pricing: PricingValues{
			"billing_setting.billing_mode":          billing_setting.BillingModeTieredExpr,
			"billing_setting.billing_expr":          mainExpr,
			billing_setting.PluginBillingExprOption: map[string]any{pluginKey: variantExpr},
		},
		PluginValidationModels: map[string]string{pluginKey: canonical},
	}}))

	err = UpdateModelPricingOptions(map[string]string{billing_setting.PluginBillingExprOption: `{}`})
	require.ErrorContains(t, err, "plugin "+pluginKey)
	active, err := GetModelPricingSnapshot([]string{alias})
	require.NoError(t, err)
	require.Equal(t, map[string]any{pluginKey: variantExpr}, active.Entries[0].Configured[billing_setting.PluginBillingExprOption])
	require.Len(t, active.Entries[0].PluginVariants, 1)
	activeVariant := active.Entries[0].PluginVariants[0]
	assert.False(t, activeVariant.Stale)
	assert.Equal(t, schema, activeVariant.UsageSchema)
	assert.Equal(t, examples, activeVariant.UsageExamples)
	assert.Equal(t, variantExpr, activeVariant.Configured)
	assert.Equal(t, variantExpr, activeVariant.Effective)
	assert.True(t, activeVariant.Compatible)

	require.NoError(t, database.Model(&Channel{}).Where("id = ?", channel.Id).Update("status", common.ChannelStatusManuallyDisabled).Error)
	rebuildTaskAliasView()
	stale, err := GetModelPricingSnapshot([]string{alias})
	require.NoError(t, err)
	require.Len(t, stale.Entries[0].PluginVariants, 1)
	staleVariant := stale.Entries[0].PluginVariants[0]
	assert.True(t, staleVariant.Stale)
	assert.Equal(t, variantExpr, staleVariant.Configured)

	require.NoError(t, UpdateModelPricingOptions(map[string]string{billing_setting.PluginBillingExprOption: `{}`}))
	staleRemoved, err := GetModelPricingSnapshot([]string{alias})
	require.NoError(t, err)
	assert.NotContains(t, staleRemoved.Entries[0].Configured, billing_setting.PluginBillingExprOption)
	assert.Equal(t, mainExpr, staleRemoved.Entries[0].Configured["billing_setting.billing_expr"])
}

func TestLegacyWildcardAliasUsesCanonicalConfiguredStateAndRuntimePublication(t *testing.T) {
	database, _ := openModelPricingInstanceDatabases(t)
	previousDB := DB
	previousOptions := common.OptionMap
	previousConfig := config.GlobalConfig.ExportAllConfigs()
	restoreRatios := []struct {
		value   string
		restore func(string) error
	}{
		{ratio_setting.ModelPrice2JSONString(), ratio_setting.UpdateModelPriceByJSONString},
		{ratio_setting.ModelRatio2JSONString(), ratio_setting.UpdateModelRatioByJSONString},
		{ratio_setting.CompletionRatio2JSONString(), ratio_setting.UpdateCompletionRatioByJSONString},
		{ratio_setting.CacheRatio2JSONString(), ratio_setting.UpdateCacheRatioByJSONString},
		{ratio_setting.CreateCacheRatio2JSONString(), ratio_setting.UpdateCreateCacheRatioByJSONString},
		{ratio_setting.ImageRatio2JSONString(), ratio_setting.UpdateImageRatioByJSONString},
		{ratio_setting.AudioRatio2JSONString(), ratio_setting.UpdateAudioRatioByJSONString},
		{ratio_setting.AudioCompletionRatio2JSONString(), ratio_setting.UpdateAudioCompletionRatioByJSONString},
	}
	DB = database
	common.OptionMap = maps.Clone(previousOptions)
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	t.Cleanup(func() {
		for _, ratio := range restoreRatios {
			require.NoError(t, ratio.restore(ratio.value))
		}
		require.NoError(t, config.GlobalConfig.LoadFromDB(previousConfig))
		common.OptionMap = previousOptions
		DB = previousDB
	})

	const (
		canonical  = "gpt-4-gizmo-*"
		firstName  = "gpt-4-gizmo-alpha"
		secondName = "gpt-4-gizmo-beta"
	)
	initial, err := GetModelPricingSnapshot([]string{firstName, secondName})
	require.NoError(t, err)
	require.Len(t, initial.Entries, 2)
	assert.Equal(t, initial.Entries[0].Version, initial.Entries[1].Version)
	assert.Equal(t, float64(15), initial.Entries[0].Configured["ModelRatio"])
	assert.Equal(t, float64(15), initial.Entries[1].Configured["ModelRatio"])

	staleVersion := initial.Entries[1].Version
	require.NoError(t, UpdateModelPricing([]ModelPricingChange{{
		ModelName:       firstName,
		ExpectedVersion: initial.Entries[0].Version,
		Pricing:         PricingValues{"ModelRatio": float64(21)},
	}}))
	ratio, configured, resolvedName := ratio_setting.GetModelRatio(firstName)
	assert.True(t, configured)
	assert.Equal(t, float64(21), ratio)
	assert.Equal(t, canonical, resolvedName)

	err = UpdateModelPricing([]ModelPricingChange{{
		ModelName:       secondName,
		ExpectedVersion: staleVersion,
		Pricing:         PricingValues{"ModelRatio": float64(22)},
	}})
	require.ErrorIs(t, err, ErrModelPricingConflict)

	updated, err := GetModelPricingSnapshot([]string{firstName})
	require.NoError(t, err)
	require.NoError(t, UpdateModelPricing([]ModelPricingChange{{
		ModelName:       firstName,
		ExpectedVersion: updated.Entries[0].Version,
		Reset:           true,
	}}))

	var option Option
	require.NoError(t, database.Where("key = ?", "ModelRatio").First(&option).Error)
	var ratios map[string]float64
	require.NoError(t, common.UnmarshalJsonStr(option.Value, &ratios))
	assert.Equal(t, float64(15), ratios[canonical])
	assert.NotContains(t, ratios, firstName)
	assert.NotContains(t, ratios, secondName)
	ratio, configured, resolvedName = ratio_setting.GetModelRatio(secondName)
	assert.True(t, configured)
	assert.Equal(t, float64(15), ratio)
	assert.Equal(t, canonical, resolvedName)
}
