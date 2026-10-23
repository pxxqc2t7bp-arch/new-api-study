package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"smart-router/api"
	"smart-router/config"
	"smart-router/internal/testutil"
	"smart-router/modeltrace"
	"smart-router/modeltraceapi"
	"smart-router/store"
)

var cliKey = []byte("synthetic-command-ingest-key-at-least-32")

func cliDir(t *testing.T) string {
	t.Helper()
	return testutil.OwnedDir(t)
}

func cliHash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func cliPut(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, data, 0600))
}

func cliJSON(t *testing.T, path string, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	cliPut(t, path, data)
	return data
}

func cliEvidence() modeltrace.Evidence {
	e := modeltrace.Evidence{SchemaVersion: 1, ProbeID: "probe-cli", BindingID: "bnd_AAAAAAAAAAAAAAAAAAAAAAAA",
		ClaimedModel: "candidate", Protocol: api.ProtocolChat, BankRevision: cliHash([]byte("{}")),
		Samples: []modeltrace.Sample{{ChallengeID: "challenge", ExpectedCount: 80, Numbers: make([]int, 80)}}}
	for i := range e.Samples[0].Numbers {
		e.Samples[0].Numbers[i] = i + 1
	}
	return e
}

func cliFixture(t *testing.T) ([]string, config.ModelTracePins, string) {
	t.Helper()
	pinnedPython := testutil.Python(t)
	root := cliDir(t)
	input := filepath.Join(root, "evidence.json")
	cliJSON(t, input, cliEvidence())
	runtime := filepath.Join(root, "runtime")
	worker := []byte("import json,sys\nr=json.load(sys.stdin)\nprint(json.dumps({'schema_version':1,'bank_revision':r['bank_revision'],'prediction':'candidate','probability':1,'used_outputs':1,'complete':False,'closed_set':True,'results':[{'model':'candidate','family':'family','probability':1,'score':0,'profile_similarity':0}],'family_probabilities':[{'family':'family','probability':1}]}))\n")
	files := map[string]string{}
	for _, name := range []string{"worker.py", "engine.py", "bank_validation.py", "__init__.py"} {
		data := []byte("")
		if name == "worker.py" {
			data = worker
		}
		cliPut(t, filepath.Join(runtime, name), data)
		files[name] = cliHash(data)
	}
	runtimeManifest := filepath.Join(runtime, "RUNTIME-MANIFEST.json")
	runtimeData := cliJSON(t, runtimeManifest, map[string]any{"schema_version": 1, "files": files})
	vendor := filepath.Join(root, "vendor")
	vendorFiles := map[string]string{}
	for _, name := range []string{"fingerprint.py", "LICENSE", "data/unified_bank.json"} {
		cliPut(t, filepath.Join(vendor, name), []byte("{}"))
		vendorFiles[name] = cliHash([]byte("{}"))
	}
	vendorManifest := filepath.Join(vendor, "PROVENANCE.json")
	vendorData := cliJSON(t, vendorManifest, map[string]any{
		"schema_version": 1, "files": vendorFiles,
		"source": map[string]string{"url": "https://github.com/xqy2006/ModelTrace.git",
			"commit": "6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2"},
	})
	python, err := os.ReadFile(pinnedPython)
	require.NoError(t, err)
	args := []string{"analyze", "-input", input, "-python", pinnedPython, "-bank", filepath.Join(vendor, "data/unified_bank.json"),
		"-runtime-manifest", runtimeManifest, "-vendor-manifest", vendorManifest}
	return args, config.ModelTracePins{PythonSHA256: cliHash(python), RuntimeManifestSHA256: cliHash(runtimeData),
		VendorManifestSHA256: cliHash(vendorData)}, root
}

func TestCLIRejectsUsageBeforeUnrelatedLookup(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"get"}, {"get", "-probe-id", "bad/id"},
		{"get", "-probe-id", "probe-cli", "-provider-key", "synthetic"}, {"submit", "-input", "-"},
		{"get", "-probe-id", "probe-cli", "-url", "https://example.invalid"},
		{"get", "-probe-id", "probe-cli", "-url", "http://127.0.0.1:18086/path"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var output, errors bytes.Buffer
			lookups := 0
			code := run(context.Background(), args, func(string) string { lookups++; return "" },
				config.ModelTracePins{}, &output, &errors)
			assert.NotZero(t, code)
			assert.Empty(t, output.String())
			assert.Equal(t, "modeltrace: invalid_request\n", errors.String())
			assert.Zero(t, lookups)
		})
	}
}

func TestCLIAnalyzeExecutesPinnedChildWithoutConfigOrNetwork(t *testing.T) {
	args, pins, root := cliFixture(t)
	var output, errors bytes.Buffer
	lookups := 0
	code := run(context.Background(), args, func(string) string { lookups++; return "forbidden" }, pins, &output, &errors)
	require.Equal(t, 0, code, errors.String())
	assert.Empty(t, errors.String())
	assert.Zero(t, lookups)
	var result modeltrace.Result
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	require.NoError(t, result.Validate())
	assert.Equal(t, "candidate", result.Prediction)
	assert.NotContains(t, output.String(), "numbers")
	assert.NotContains(t, output.String(), root)
	assert.NotContains(t, output.String(), "diagnostics")
}

func TestCLIAnalyzeRejectsMalformedInputAndPrivatePins(t *testing.T) {
	for _, mode := range []string{"empty_pin", "invalid_input", "trailing", "oversize", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			args, pins, _ := cliFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "empty_pin" {
				pins.PythonSHA256 = ""
			} else if mode == "cancelled" {
				cancel()
			} else {
				data := []byte(`{"provider_key":"synthetic"}`)
				if mode == "trailing" {
					valid, err := json.Marshal(cliEvidence())
					require.NoError(t, err)
					data = append(valid, []byte("{}")...)
				} else if mode == "oversize" {
					data = bytes.Repeat([]byte(" "), modeltraceapi.MaxBodyBytes+1)
				}
				cliPut(t, args[2], data)
			}
			var output, errors bytes.Buffer
			code := run(ctx, args, func(string) string { t.Fatal("unrelated lookup"); return "" }, pins, &output, &errors)
			assert.NotZero(t, code)
			assert.Empty(t, output.String())
			assert.Less(t, errors.Len(), 80)
			assert.NotContains(t, errors.String(), "synthetic")
		})
	}
}

type cliBackend struct {
	record modeltrace.Record
	calls  int
}

func (b *cliBackend) Submit(_ context.Context, e modeltrace.Evidence) (modeltrace.Record, error) {
	b.calls++
	return b.record, nil
}
func (b *cliBackend) GetProbe(_ context.Context, id string) (modeltrace.Record, error) {
	b.calls++
	return b.record, nil
}
func (b *cliBackend) LatestAttribution(context.Context, string, string, api.Protocol) (modeltrace.Record, error) {
	return modeltrace.Record{}, modeltrace.ErrProbeNotFound
}

func cliRecord(t *testing.T) modeltrace.Record {
	t.Helper()
	e := cliEvidence()
	digest, err := e.Digest()
	require.NoError(t, err)
	return modeltrace.Record{ProbeID: e.ProbeID, BindingID: e.BindingID, ClaimedModel: e.ClaimedModel,
		Protocol: e.Protocol, EvidenceSHA256: digest, State: modeltrace.StateRunning, ObservedAt: time.Now().UTC()}
}

func TestCLISubmitAndGetUseSignedTransportOnly(t *testing.T) {
	for _, command := range []string{"submit", "get"} {
		t.Run(command, func(t *testing.T) {
			backend := &cliBackend{record: cliRecord(t)}
			handler, err := modeltraceapi.NewServer(modeltraceapi.Options{Backend: backend, Key: cliKey})
			require.NoError(t, err)
			server := httptest.NewServer(handler)
			defer server.Close()
			args := []string{"get", "-probe-id", "probe-cli", "-url", server.URL}
			if command == "submit" {
				input := filepath.Join(cliDir(t), "evidence.json")
				cliJSON(t, input, cliEvidence())
				args = []string{"submit", "-input", input, "-url", server.URL}
			}
			var output, errors bytes.Buffer
			names := []string{}
			code := run(context.Background(), args, func(name string) string {
				names = append(names, name)
				return string(cliKey)
			}, config.ModelTracePins{}, &output, &errors)
			require.Equal(t, 0, code, errors.String())
			assert.Equal(t, []string{"SMART_ROUTER_MODELTRACE_INGEST_KEY"}, names)
			assert.Equal(t, 1, backend.calls)
			record, err := modeltraceapi.DecodeRecord(output.Bytes())
			require.NoError(t, err)
			assert.Equal(t, "probe-cli", record.ProbeID)
			assert.Empty(t, errors.String())
		})
	}
}

type cliCountingEngine struct {
	modeltrace.Engine
	calls int
}

func (engine *cliCountingEngine) Analyze(ctx context.Context, evidence modeltrace.Evidence) (modeltrace.Result, error) {
	engine.calls++
	return engine.Engine.Analyze(ctx, evidence)
}

func TestCLISignedPartialSubmitPreservesDurableDigest(t *testing.T) {
	ctx := context.Background()
	args, pins, root := cliFixture(t)
	e := cliEvidence()
	e.Samples = append(e.Samples, modeltrace.Sample{ChallengeID: "empty", Numbers: []int{}})
	data := cliJSON(t, args[2], e)
	digest, err := e.Digest()
	require.NoError(t, err)
	process, err := config.ModelTraceProcess(args[4], filepath.Join(root, "runtime", "worker.py"),
		args[8], args[10], args[6], cliDir(t), pins)
	require.NoError(t, err)
	child, err := modeltrace.NewProcessEngine(process)
	require.NoError(t, err)
	engine := &cliCountingEngine{Engine: child}
	db, err := store.OpenSQLite(ctx, filepath.Join(root, "probes.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.UpsertSource(ctx, store.Source{ID: "cli-source", Connector: "fixture", Enabled: true}))
	require.NoError(t, db.UpsertBinding(ctx, store.BindingRecord{
		ID: e.BindingID, SourceID: "cli-source", Group: "default", Model: e.ClaimedModel,
		Protocol: e.Protocol, ProtocolPlugin: "chat-v1", Enabled: true,
	}))
	backend := struct {
		modeltrace.Service
		modeltrace.Repository
	}{modeltrace.Service{Repository: db, Engine: engine}, db}
	handler, err := modeltraceapi.NewServer(modeltraceapi.Options{Backend: backend, Key: cliKey})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	defer server.Close()
	submit := []string{"submit", "-input", args[2], "-url", server.URL}
	var first modeltraceapi.Record
	for attempt := range 2 {
		var output, errors bytes.Buffer
		code := run(ctx, submit, func(string) string { return string(cliKey) }, config.ModelTracePins{}, &output, &errors)
		require.Equal(t, 0, code, errors.String())
		assert.Empty(t, errors.String())
		record, err := modeltraceapi.DecodeRecord(output.Bytes())
		require.NoError(t, err)
		assert.Equal(t, digest, record.EvidenceSHA256)
		assert.Equal(t, modeltrace.StateSucceeded, record.State)
		require.NotNil(t, record.Result)
		assert.Equal(t, 1, record.Result.UsedOutputs)
		if attempt == 0 {
			first = record
		} else {
			assert.Equal(t, first, record)
		}
	}
	assert.Equal(t, 1, engine.calls)
	durable, err := db.GetProbe(ctx, e.ProbeID)
	require.NoError(t, err)
	assert.Equal(t, first, modeltraceapi.Record(durable))

	cliPut(t, args[2], bytes.Replace(data, []byte(`"numbers":[]`), []byte(`"numbers":null`), 1))
	var output, errors bytes.Buffer
	code := run(ctx, submit, func(string) string { return string(cliKey) }, config.ModelTracePins{}, &output, &errors)
	assert.NotZero(t, code)
	assert.Empty(t, output.String())
	assert.Equal(t, "modeltrace: invalid_request\n", errors.String())
	assert.Equal(t, 1, engine.calls)
}

func TestCLITransportRejectsUntrustedResponsesWithoutDisclosure(t *testing.T) {
	for _, mode := range []string{"status", "invalid", "oversize", "redirect", "wrong_id", "wrong_digest"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch mode {
				case "status":
					w.WriteHeader(500)
					_, _ = io.WriteString(w, "raw-synthetic-backend-secret")
				case "invalid":
					_, _ = io.WriteString(w, `{"raw":"raw-synthetic-backend-secret"}`)
				case "oversize":
					_, _ = w.Write(bytes.Repeat([]byte(" "), modeltraceapi.MaxBodyBytes+1))
				case "redirect":
					http.Redirect(w, r, "/other", http.StatusFound)
				case "wrong_id", "wrong_digest":
					record := cliRecord(t)
					if mode == "wrong_id" {
						record.ProbeID = "different-id"
					} else {
						record.EvidenceSHA256 = strings.Repeat("c", 64)
					}
					data, err := modeltraceapi.EncodeRecord(record)
					require.NoError(t, err)
					_, _ = w.Write(data)
				}
			}))
			defer server.Close()
			var output, errors bytes.Buffer
			args := []string{"get", "-probe-id", "probe-cli", "-url", server.URL}
			if mode == "wrong_digest" {
				input := filepath.Join(cliDir(t), "evidence.json")
				cliJSON(t, input, cliEvidence())
				args = []string{"submit", "-input", input, "-url", server.URL}
			}
			code := run(context.Background(), args,
				func(string) string { return string(cliKey) }, config.ModelTracePins{}, &output, &errors)
			assert.NotZero(t, code)
			assert.Empty(t, output.String())
			assert.Less(t, errors.Len(), 80)
			assert.NotContains(t, errors.String(), "raw-synthetic")
			assert.Equal(t, 1, calls)
			if mode == "wrong_digest" {
				assert.Equal(t, "modeltrace: invalid_record\n", errors.String())
			}
		})
	}
}
