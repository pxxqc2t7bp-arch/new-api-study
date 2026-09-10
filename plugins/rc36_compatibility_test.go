package plugins_test

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/router"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRC36Compatibility(t *testing.T) {
	t.Run("custom Doubao golden preserves deferred aliases and artifacts", func(t *testing.T) {
		source, err := builtinplugins.Source("doubao")
		require.NoError(t, err)
		fixture, err := os.ReadFile("testdata/rc36-custom-golden.json")
		require.NoError(t, err)

		report, err := jsplugin.ReplayFixture(t.Context(), source, fixture)

		require.NoError(t, err)
		assert.Equal(t, jsplugin.FixtureReport{Total: 5, Passed: 5}, report)
	})

	t.Run("Files Batch 3D and artifact routes remain registered", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		engine := gin.New()
		require.NotPanics(t, func() {
			router.SetBatchRouter(engine)
			router.SetThreeDRouter(engine)
			router.SetTaskRouter(engine)
		})

		actual := make(map[string]struct{})
		for _, route := range engine.Routes() {
			actual[route.Method+" "+route.Path] = struct{}{}
		}
		for _, expected := range []string{
			http.MethodPost + " /v1/files",
			http.MethodGet + " /v1/files",
			http.MethodGet + " /v1/files/:id",
			http.MethodDelete + " /v1/files/:id",
			http.MethodGet + " /v1/files/:id/content",
			http.MethodHead + " /v1/files/:id/content",
			http.MethodPost + " /v1/batches",
			http.MethodGet + " /v1/batches",
			http.MethodGet + " /v1/batches/:id",
			http.MethodPost + " /v1/batches/:id/cancel",
			http.MethodPost + " /v1/3d/generations",
			http.MethodGet + " /v1/3d/generations/:task_id",
			http.MethodDelete + " /v1/3d/generations/:task_id",
			http.MethodPost + " /api/v3/contents/generations/tasks",
			http.MethodGet + " /api/v3/contents/generations/tasks/:task_id",
			http.MethodDelete + " /api/v3/contents/generations/tasks/:task_id",
			http.MethodGet + " /v1/tasks/:key/artifacts",
			http.MethodGet + " /v1/tasks/:key/artifacts/:artifact_key/content",
			http.MethodHead + " /v1/tasks/:key/artifacts/:artifact_key/content",
		} {
			assert.Contains(t, actual, expected)
		}
	})

	t.Run("custom channel type assignments stay stable", func(t *testing.T) {
		assert.Equal(t, 61, constant.ChannelTypeTaskPlugin)
		assert.Equal(t, 62, constant.ChannelTypeVolcEngine3D)
		assert.Equal(t, 63, constant.ChannelTypeDummy)
		assert.Equal(t, "VolcEngine3D", constant.GetChannelTypeName(constant.ChannelTypeVolcEngine3D))
	})

	t.Run("model aliases retain plugin ownership and protocol capabilities", func(t *testing.T) {
		originalDB := model.DB
		t.Cleanup(func() { model.DB = originalDB })
		database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		require.NoError(t, err)
		model.DB = database
		require.NoError(t, model.DB.AutoMigrate(&model.Channel{}))

		source := strings.ReplaceAll(rc36RuntimePluginSource, "compat-version", "1.0.0")
		registry := jsplugin.NewRegistry()
		plugin, err := registry.RegisterFactory(source, jsplugin.Options{})
		require.NoError(t, err)
		mapping := `{"customer-video":"compat-video"}`
		require.NoError(t, model.DB.Create(&model.Channel{
			Id:           93601,
			Type:         constant.ChannelTypeTaskPlugin,
			Status:       common.ChannelStatusEnabled,
			Name:         "rc36 compatibility",
			Models:       "customer-video,compat-video",
			ModelMapping: &mapping,
		}).Error)

		target, found := model.ResolveTaskModelAlias(registry.Generation(), "CUSTOMER-VIDEO")
		require.True(t, found)
		assert.Equal(t, "customer-video", target.Alias)
		assert.Equal(t, "compat-video", target.Declared)
		assert.Equal(t, plugin.Meta.Key, target.PluginKey)

		responses, found := registry.Generation().LookupEndpoint(http.MethodPost, "/v1/responses", "compat-image")
		require.True(t, found)
		assert.Equal(t, plugin.Meta.Key, responses.Plugin.Meta.Key)
		_, found = registry.Generation().LookupEndpoint(http.MethodPost, "/v1/responses", "compat-video")
		assert.False(t, found)
		video, found := registry.Generation().LookupEndpoint(http.MethodPost, "/v1/videos", "compat-video")
		require.True(t, found)
		assert.Equal(t, plugin.Meta.Key, video.Plugin.Meta.Key)
		_, found = registry.Generation().LookupEndpoint(http.MethodPost, "/v1/videos", "compat-image")
		assert.False(t, found)
		assert.True(t, plugin.Meta.ProtocolSupports("openai_responses", "sync"))
		assert.False(t, plugin.Meta.ProtocolSupports("openai_responses", "stream"))

		native, found := registry.Generation().LookupDeclaredRoute(http.MethodPost, "/compat/native/tasks")
		require.True(t, found)
		assert.Equal(t, []string{"compat-video"}, native.Route.Models)
	})

	t.Run("runtime revision changes without mutating a pinned generation", func(t *testing.T) {
		registry := jsplugin.NewRegistry()
		v1Source := strings.ReplaceAll(rc36RuntimePluginSource, "compat-version", "1.0.0")
		v1, err := registry.RegisterFactory(v1Source, jsplugin.Options{})
		require.NoError(t, err)
		pinned := registry.Generation()

		v2Source := strings.ReplaceAll(rc36RuntimePluginSource, "compat-version", "2.0.0")
		v2, err := registry.Register(v2Source, jsplugin.Options{})
		require.NoError(t, err)
		current := registry.Generation()

		assert.Greater(t, current.Number, pinned.Number)
		pinnedPlugin, found := pinned.Get(v1.Meta.Key)
		require.True(t, found)
		assert.Same(t, v1, pinnedPlugin)
		currentPlugin, found := current.Get(v2.Meta.Key)
		require.True(t, found)
		assert.Same(t, v2, currentPlugin)
		assert.Equal(t, "1.0.0", pinnedPlugin.Meta.Version)
		assert.Equal(t, "2.0.0", currentPlugin.Meta.Version)
	})

	t.Run("rc36 schema metadata and usage examples compile", func(t *testing.T) {
		registry := jsplugin.NewRegistry()
		plugin, err := registry.Register(rc36SchemaPluginSource, jsplugin.Options{})
		require.NoError(t, err)

		encoded, err := common.Marshal(plugin.Meta)
		require.NoError(t, err)
		var meta map[string]any
		require.NoError(t, common.Unmarshal(encoded, &meta))
		assert.Equal(t, float64(25), meta["sortPriority"])
		assert.Equal(t, "https://plugins.example.test/compat", meta["website"])
		assert.Equal(t, "https://api.example.test/v1", meta["baseUrl"])
		assert.Equal(t, []any{"cdn.example.test:8443"}, meta["allowedHosts"])

		usageSchema, ok := meta["usageSchema"].(map[string]any)
		require.True(t, ok)
		resolution, ok := usageSchema["resolution"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, map[string]any{
			"720p": map[string]any{"en": "720p HD", "zh": "720p"},
		}, resolution["enumLabels"])
		assert.Equal(t, []any{
			map[string]any{
				"label": "720p sample",
				"facts": map[string]any{"resolution": "720p"},
			},
		}, meta["usageExamples"])
	})
}

const rc36RuntimePluginSource = `
export const meta = {
  apiVersion: 1,
  key: "rc36-runtime-compat",
  name: "RC36 Runtime Compatibility",
  version: "compat-version",
  author: {name: "Compatibility Test"},
  channelTypes: [9361],
  models: ["compat-video", "compat-image"],
  fetchMode: "per_task",
  routes: [
    {method: "POST", path: "/compat/native/tasks", type: "submit", decode: "createTask", render: "taskCreated", models: ["compat-video"]}
  ],
  protocols: [
    {name: "openai_responses", models: ["compat-image"], supports: ["sync"]},
    {name: "openai_video", models: ["compat-video"]}
  ]
};
export function buildSubmitRequest(ctx) { return {url: ctx.baseUrl + "/tasks", method: "POST", body: ctx.requestBody}; }
export function parseSubmitResponse(ctx, response) { return {taskId: response.body.id}; }
export function buildQueryRequest(ctx) { return {url: ctx.baseUrl + "/tasks/" + ctx.taskId, method: "GET"}; }
export function parseTaskResult(ctx, body) { return body; }
export function listArtifacts(task) { return task.status === "SUCCESS" ? [{key: "video", type: "video"}] : []; }
export function buildContentRequest(ctx) { return {url: ctx.data.url, method: ctx.clientRequest.method, credentialless: true}; }
export const native = {
  createTask: function(ctx) { return {kind: "submit", model: ctx.body.value.model, requestBody: ctx.body.value}; },
  taskCreated: function(ctx, task) { return {id: task.task_id}; }
};
export const protocols = {
  openai_responses: {
    decodeRequest: function(ctx) { return {kind: "submit", model: ctx.model, requestBody: {model: ctx.upstreamModel || ctx.model}}; },
    renderFinal: function(ctx, task) { return {output: [], metadata: {task_id: task.task_id}}; }
  },
  openai_video: {
    decodeRequest: function(ctx) { return {kind: "submit", model: ctx.model, requestBody: ctx.body.value}; },
    render: function(ctx, task) { return {id: task.task_id, status: task.status}; }
  }
};
`

const rc36SchemaPluginSource = `
export const meta = {
  sortPriority: 25,
  website: "https://plugins.example.test/compat",
  apiVersion: 1,
  key: "rc36-schema-compat",
  name: "RC36 Schema Compatibility",
  version: "1.0.0",
  author: {name: "Compatibility Test"},
  baseUrl: "HTTPS://API.Example.test/v1/",
  channelTypes: [9362],
  models: ["rc36-video"],
  fetchMode: "per_task",
  allowedHosts: ["CDN.Example.test:8443"],
  usageSchema: {
    resolution: {
      enum: ["720p"],
      enumLabels: {"720p": {en: "720p HD", zh: "720p"}},
      description: {en: "Output resolution."}
    }
  },
  usageExamples: [{label: "720p sample", facts: {resolution: "720p"}}]
};
export function buildSubmitRequest(ctx) { return {url: ctx.baseUrl + "/tasks", method: "POST", body: ctx.requestBody}; }
export function parseSubmitResponse(ctx, response) { return {taskId: response.body.id}; }
export function buildQueryRequest(ctx) { return {url: ctx.baseUrl + "/tasks/" + ctx.taskId, method: "GET"}; }
export function parseTaskResult(ctx, body) { return body; }
`
