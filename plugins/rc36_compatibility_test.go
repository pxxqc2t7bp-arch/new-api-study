package plugins_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
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
		assert.Equal(t, jsplugin.FixtureReport{Total: 6, Passed: 6}, report)
	})

	t.Run("current built-in sources load and replay through the fixture API", func(t *testing.T) {
		fixture := []byte(`{
			"cases": [{
				"name": "non-success tasks expose no artifacts",
				"hook": "listArtifacts",
				"args": [{"status": "FAILURE", "data": {}}],
				"expected": []
			}]
		}`)
		for _, key := range []string{"alibaba", "doubao", "google", "hailuo", "jimeng", "kling", "sora", "sunoapi", "vertex-ai", "vidu"} {
			t.Run(key, func(t *testing.T) {
				source, err := builtinplugins.Source(key)
				require.NoError(t, err)

				report, err := jsplugin.ReplayFixture(t.Context(), source, fixture)

				require.NoError(t, err)
				assert.Equal(t, jsplugin.FixtureReport{Total: 1, Passed: 1}, report)
			})
		}
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

	t.Run("native route decodes and renders a task", func(t *testing.T) {
		registry := jsplugin.NewRegistry()
		source := strings.ReplaceAll(rc36RuntimePluginSource, "compat-version", "1.0.0")
		plugin, err := registry.RegisterFactory(source, jsplugin.Options{})
		require.NoError(t, err)
		binding, found := registry.Generation().LookupDeclaredRoute(http.MethodPost, "/compat/native/tasks")
		require.True(t, found)
		route := binding.Route

		decodedValue, err := plugin.Engine.CallMember(
			context.Background(),
			"native",
			route.Decode,
			jsplugin.RouteRequestContext{
				Path:   "/compat/native/tasks",
				Method: http.MethodPost,
				Body: map[string]any{"kind": "json", "value": map[string]any{
					"model":  "compat-video",
					"prompt": "render this",
				}},
			}.JSValue(),
		)
		require.NoError(t, err)
		decoded := jsonObject(t, decodedValue)
		assert.Equal(t, "submit", decoded["kind"])
		assert.Equal(t, "compat-video", decoded["model"])
		assert.Equal(t, map[string]any{
			"model":  "compat-video",
			"prompt": "render this",
		}, decoded["requestBody"])

		renderedValue, err := plugin.Engine.CallMember(
			context.Background(),
			"native",
			route.Render,
			map[string]any{},
			map[string]any{"task_id": "task_rc36_native", "status": model.TaskStatusSuccess},
		)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"id": "task_rc36_native"}, jsonObject(t, renderedValue))
	})

	t.Run("artifact routes proxy GET and HEAD", func(t *testing.T) {
		database := setupRC36CompatibilityDatabase(t)
		source := strings.ReplaceAll(rc36RuntimePluginSource, "compat-version", "1.0.0")
		plugin, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, jsplugin.DefaultRegistry.Unregister(plugin.Meta.Key)) })

		var requestMethodsMu sync.Mutex
		requestMethods := make([]string, 0, 2)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			requestMethodsMu.Lock()
			requestMethods = append(requestMethods, request.Method)
			requestMethodsMu.Unlock()
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", "14")
			if request.Method == http.MethodGet {
				_, _ = w.Write([]byte("artifact-bytes"))
			}
		}))
		defer upstream.Close()
		allowPrivateArtifactProxy(t)

		channel := model.Channel{
			Type:    constant.ChannelTypeTaskPlugin,
			Status:  common.ChannelStatusEnabled,
			Name:    "rc36 artifact",
			Key:     plugin.Meta.Key,
			BaseURL: &upstream.URL,
			Models:  "compat-video",
		}
		require.NoError(t, database.Create(&channel).Error)
		task := model.Task{
			TaskID:    "task_rc36_artifact",
			Platform:  constant.TaskPlatform(plugin.Meta.Key),
			UserId:    936,
			ChannelId: channel.Id,
			Status:    model.TaskStatusSuccess,
			PrivateData: model.TaskPrivateData{
				Execution: &model.TaskExecutionSnapshot{
					TaskPlugin: &model.TaskPluginSnapshot{Key: plugin.Meta.Key},
				},
			},
		}
		task.SetData(map[string]any{"url": upstream.URL})
		require.NoError(t, database.Create(&model.User{
			Id:       task.UserId,
			Username: "rc36-artifact-owner",
			Password: "not-used-in-test",
			Status:   common.UserStatusEnabled,
		}).Error)
		require.NoError(t, database.Create(&task).Error)

		originalCryptoSecret := common.CryptoSecret
		common.CryptoSecret = "rc36-artifact-access-secret"
		t.Cleanup(func() { common.CryptoSecret = originalCryptoSecret })
		access, err := service.IssueTaskArtifactAccess(task.TaskID, "video")
		require.NoError(t, err)

		engine := gin.New()
		path := "/v1/tasks/:key/artifacts/:artifact_key/content"
		handlerReached := make(map[string]bool, 2)
		handler := func(c *gin.Context) {
			handlerReached[c.Request.Method] = true
			assert.True(t, middleware.IsTaskArtifactAccess(c))
			controller.TaskArtifactContent(c)
		}
		engine.GET(path, middleware.TokenOrTaskArtifactAccessAuth("key", "artifact_key"), handler)
		engine.HEAD(path, middleware.TokenOrTaskArtifactAccessAuth("key", "artifact_key"), handler)

		for _, testCase := range []struct {
			method string
			body   string
		}{
			{method: http.MethodGet, body: "artifact-bytes"},
			{method: http.MethodHead},
		} {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(
				testCase.method,
				"/v1/tasks/"+task.TaskID+"/artifacts/video/content?access="+access,
				nil,
			)
			request.RemoteAddr = "192.0.2.1:1234"

			engine.ServeHTTP(recorder, request)

			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.True(t, handlerReached[testCase.method])
			requestMethodsMu.Lock()
			require.NotEmpty(t, requestMethods)
			actualMethod := requestMethods[len(requestMethods)-1]
			requestMethodsMu.Unlock()
			assert.Equal(t, testCase.method, actualMethod)
			assert.Equal(t, testCase.body, recorder.Body.String())
			assert.Equal(t, "video/mp4", recorder.Header().Get("Content-Type"))
			assert.Equal(t, "14", recorder.Header().Get("Content-Length"))
			assert.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
		}
	})

	t.Run("custom channel type assignments stay stable", func(t *testing.T) {
		assert.Equal(t, 61, constant.ChannelTypeTaskPlugin)
		assert.Equal(t, 62, constant.ChannelTypeVolcEngine3D)
		assert.Equal(t, 63, constant.ChannelTypeDummy)
		assert.Equal(t, "VolcEngine3D", constant.GetChannelTypeName(constant.ChannelTypeVolcEngine3D))
	})

	t.Run("model aliases retain plugin ownership and protocol capabilities", func(t *testing.T) {
		database := setupRC36CompatibilityDatabase(t)
		registry := jsplugin.NewRegistry()
		v1Source := strings.ReplaceAll(rc36RuntimePluginSource, "compat-version", "1.0.0")
		_, err := registry.RegisterFactory(v1Source, jsplugin.Options{})
		require.NoError(t, err)
		generationN := registry.Generation()
		_, _ = model.ResolveTaskModelAlias(generationN, "CUSTOMER-VIDEO")

		mapping := `{"customer-video":"compat-video"}`
		require.NoError(t, database.Create(&model.Channel{
			Id:           93601,
			Type:         constant.ChannelTypeTaskPlugin,
			Status:       common.ChannelStatusEnabled,
			Name:         "rc36 compatibility",
			Models:       "customer-video,compat-video",
			ModelMapping: &mapping,
		}).Error)

		v2Source := strings.ReplaceAll(rc36RuntimePluginSource, "compat-version", "2.0.0")
		plugin, err := registry.Register(v2Source, jsplugin.Options{})
		require.NoError(t, err)
		generation := registry.Generation()
		require.Equal(t, generationN.Number+1, generation.Number)

		target, found := model.ResolveTaskModelAlias(generation, "CUSTOMER-VIDEO")
		require.True(t, found)
		assert.Equal(t, "customer-video", target.Alias)
		assert.Equal(t, "compat-video", target.Declared)
		assert.Equal(t, plugin.Meta.Key, target.PluginKey)

		responses, found := generation.LookupEndpoint(http.MethodPost, "/v1/responses", "compat-image")
		require.True(t, found)
		assert.Equal(t, plugin.Meta.Key, responses.Plugin.Meta.Key)
		_, found = generation.LookupEndpoint(http.MethodPost, "/v1/responses", "compat-video")
		assert.False(t, found)
		video, found := generation.LookupEndpoint(http.MethodPost, "/v1/videos", "compat-video")
		require.True(t, found)
		assert.Equal(t, plugin.Meta.Key, video.Plugin.Meta.Key)
		_, found = generation.LookupEndpoint(http.MethodPost, "/v1/videos", "compat-image")
		assert.False(t, found)
		assert.True(t, plugin.Meta.ProtocolSupports("openai_responses", "sync"))
		assert.False(t, plugin.Meta.ProtocolSupports("openai_responses", "stream"))

		native, found := generation.LookupDeclaredRoute(http.MethodPost, "/compat/native/tasks")
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

	t.Run("old and legacy in-flight tasks complete with the active newer plugin", func(t *testing.T) {
		database := setupRC36CompatibilityDatabase(t)
		require.NoError(t, database.AutoMigrate(&model.Log{}))
		previousLogDB := model.LOG_DB
		previousLogConsume := common.LogConsumeEnabled
		model.LOG_DB = database
		common.LogConsumeEnabled = false
		t.Cleanup(func() {
			model.LOG_DB = previousLogDB
			common.LogConsumeEnabled = previousLogConsume
		})

		v1Source := strings.ReplaceAll(rc36PollingCompatibilityPluginSource, "compat-version", "1.0.0")
		v1, err := jsplugin.DefaultRegistry.Register(v1Source, jsplugin.Options{})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, jsplugin.DefaultRegistry.Unregister(v1.Meta.Key)) })
		oldGeneration := jsplugin.DefaultRegistry.Generation()
		require.NotNil(t, oldGeneration)

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			assert.Equal(t, "2.0.0", request.Header.Get("X-Plugin-Version"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"state":"complete","units":3}`)
		}))
		defer upstream.Close()

		require.NoError(t, database.Create(&model.User{
			Id:       937,
			Username: "rc36-in-flight-owner",
			Group:    "default",
			Quota:    10_000,
			Status:   common.UserStatusEnabled,
		}).Error)
		channel := model.Channel{
			Type:      constant.ChannelTypeTaskPlugin,
			Name:      "rc36 in-flight",
			Key:       "current-credential",
			BaseURL:   &upstream.URL,
			Status:    common.ChannelStatusEnabled,
			Models:    "rc36-poll-model",
			Group:     "default",
			UsedQuota: 10_000,
		}
		require.NoError(t, database.Create(&channel).Error)

		newTask := func(taskID, upstreamTaskID string) model.Task {
			expression := `tier("actual", u("units"))`
			return model.Task{
				TaskID:    taskID,
				Platform:  constant.TaskPlatform(v1.Meta.Key),
				UserId:    937,
				Group:     "default",
				ChannelId: channel.Id,
				Quota:     5_000,
				Status:    model.TaskStatusInProgress,
				Progress:  "30%",
				Properties: model.Properties{
					OriginModelName:   "rc36-poll-model",
					UpstreamModelName: "rc36-poll-model",
				},
				PrivateData: model.TaskPrivateData{
					UpstreamTaskID: upstreamTaskID,
					BillingContext: &model.TaskBillingContext{
						OriginModelName: "rc36-poll-model",
						GroupRatio:      1,
						TieredSnapshot: &billingexpr.BillingSnapshot{
							ExprString:       expression,
							ExprHash:         billingexpr.ExprHashString(expression),
							GroupRatio:       1,
							QuotaPerUnit:     1_000,
							ExprVersion:      1,
							TaskUsageBilling: true,
						},
					},
				},
			}
		}

		snapshotted := newTask("task_rc36_snapshotted", "upstream-snapshotted")
		snapshotted.PrivateData.Execution = &model.TaskExecutionSnapshot{
			TaskPlugin: &model.TaskPluginSnapshot{
				Key:        v1.Meta.Key,
				Name:       v1.Meta.Name,
				Version:    v1.Meta.Version,
				APIVersion: v1.Meta.APIVersion,
				Generation: oldGeneration.Number,
			},
		}
		require.NoError(t, database.Create(&snapshotted).Error)

		legacy := newTask("task_rc36_legacy", "upstream-legacy")
		require.NoError(t, database.Create(&legacy).Error)
		var legacyPrivateData string
		require.NoError(t, database.Model(&model.Task{}).
			Select("private_data").
			Where("id = ?", legacy.ID).
			Scan(&legacyPrivateData).Error)
		assert.NotContains(t, legacyPrivateData, `"execution"`)
		assert.NotContains(t, legacyPrivateData, `"plugin_state"`)
		assert.NotContains(t, legacyPrivateData, `"poll_failures"`)

		var persistedSnapshot model.Task
		require.NoError(t, database.First(&persistedSnapshot, snapshotted.ID).Error)
		require.NotNil(t, persistedSnapshot.PrivateData.Execution)
		require.NotNil(t, persistedSnapshot.PrivateData.Execution.TaskPlugin)
		assert.Equal(t, "1.0.0", persistedSnapshot.PrivateData.Execution.TaskPlugin.Version)
		assert.Equal(t, oldGeneration.Number, persistedSnapshot.PrivateData.Execution.TaskPlugin.Generation)

		v2Source := strings.ReplaceAll(rc36PollingCompatibilityPluginSource, "compat-version", "2.0.0")
		v2, err := jsplugin.DefaultRegistry.Register(v2Source, jsplugin.Options{})
		require.NoError(t, err)
		currentGeneration := jsplugin.DefaultRegistry.Generation()
		require.Greater(t, currentGeneration.Number, oldGeneration.Number)
		currentPlugin, found := currentGeneration.Get(v2.Meta.Key)
		require.True(t, found)
		require.Same(t, v2, currentPlugin)
		assert.Equal(t, "2.0.0", currentPlugin.Meta.Version)

		previousAdaptorFactory := service.GetTaskAdaptorFunc
		service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
			return relay.GetTaskAdaptor(platform)
		}
		t.Cleanup(func() { service.GetTaskAdaptorFunc = previousAdaptorFactory })

		for _, task := range []*model.Task{&persistedSnapshot, &legacy} {
			service.DispatchPlatformUpdate(
				t.Context(),
				task.Platform,
				map[int][]string{channel.Id: {task.GetUpstreamTaskID()}},
				map[string]*model.Task{task.GetUpstreamTaskID(): task},
			)
		}

		var settledSnapshot model.Task
		require.NoError(t, database.First(&settledSnapshot, snapshotted.ID).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), settledSnapshot.Status)
		assert.Equal(t, 3_000, settledSnapshot.Quota)
		require.NotNil(t, settledSnapshot.PrivateData.Execution)
		require.NotNil(t, settledSnapshot.PrivateData.Execution.TaskPlugin)
		assert.Equal(t, "1.0.0", settledSnapshot.PrivateData.Execution.TaskPlugin.Version)
		assert.Equal(t, oldGeneration.Number, settledSnapshot.PrivateData.Execution.TaskPlugin.Generation)

		var settledLegacy model.Task
		require.NoError(t, database.First(&settledLegacy, legacy.ID).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), settledLegacy.Status)
		assert.Equal(t, 3_000, settledLegacy.Quota)
		assert.Nil(t, settledLegacy.PrivateData.Execution)

		var settledUser model.User
		require.NoError(t, database.First(&settledUser, 937).Error)
		assert.Equal(t, 14_000, settledUser.Quota)
		var settledChannel model.Channel
		require.NoError(t, database.First(&settledChannel, channel.Id).Error)
		assert.EqualValues(t, 6_000, settledChannel.UsedQuota)
	})

	t.Run("rc36 schema metadata and usage examples compile", func(t *testing.T) {
		registry := jsplugin.NewRegistry()
		_, err := registry.Register(rc36SortPriorityPluginSource, jsplugin.Options{})
		require.NoError(t, err)

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

func setupRC36CompatibilityDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	originalDB := model.DB
	originalMemoryCache := common.MemoryCacheEnabled
	originalRedisEnabled := common.RedisEnabled
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.Task{}, &model.User{}))
	model.DB = database
	t.Cleanup(func() {
		model.DB = originalDB
		model.InitChannelCache()
		common.MemoryCacheEnabled = originalMemoryCache
		common.RedisEnabled = originalRedisEnabled
	})
	return database
}

func allowPrivateArtifactProxy(t *testing.T) {
	t.Helper()
	originalFetchSetting := *system_setting.GetFetchSetting()
	system_setting.GetFetchSetting().EnableSSRFProtection = true
	system_setting.GetFetchSetting().AllowPrivateIp = true
	system_setting.GetFetchSetting().AllowedPorts = []string{"1-65535"}
	service.InitHttpClient()
	t.Cleanup(func() {
		*system_setting.GetFetchSetting() = originalFetchSetting
		service.InitHttpClient()
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

const rc36PollingCompatibilityPluginSource = `
export const meta = {
  apiVersion: 1,
  key: "rc36-poll-compat",
  name: "RC36 Poll Compatibility",
  version: "compat-version",
  author: {name: "Compatibility Test"},
  models: ["rc36-poll-model"],
  fetchMode: "per_task",
  usageSchema: {
    units: {type: "number", unit: "count", description: {en: "Completed units"}}
  }
};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl + "/tasks", method: "POST", body: ctx.requestBody};
}
export function parseSubmitResponse(ctx, response) {
  return {taskId: response.body.id};
}
export function buildQueryRequest(ctx) {
  return {
    url: ctx.baseUrl + "/tasks/" + ctx.taskId,
    method: "GET",
    headers: {"X-Plugin-Version": "compat-version"}
  };
}
export function parseTaskResult(ctx, body) {
  if ("compat-version" !== "2.0.0") {
    return {status: "UNKNOWN", reason: "old parser must not handle this response"};
  }
  return {status: body.state === "complete" ? "SUCCESS" : "IN_PROGRESS"};
}
export function extractUsageOnComplete(ctx, result, body) {
  return {units: body.units};
}
`

const rc36SortPriorityPluginSource = `
export const meta = {
  sortPriority: 25,
  apiVersion: 1,
  key: "rc36-sort-priority-compat",
  name: "RC36 Sort Priority Compatibility",
  version: "1.0.0",
  author: {name: "Compatibility Test"},
  channelTypes: [9363],
  models: ["rc36-sort-priority"],
  fetchMode: "per_task"
};
export function buildSubmitRequest(ctx) { return {url: ctx.baseUrl + "/tasks", method: "POST", body: ctx.requestBody}; }
export function parseSubmitResponse(ctx, response) { return {taskId: response.body.id}; }
export function buildQueryRequest(ctx) { return {url: ctx.baseUrl + "/tasks/" + ctx.taskId, method: "GET"}; }
export function parseTaskResult(ctx, body) { return body; }
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
