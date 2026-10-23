package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"smart-router/api"
	"smart-router/internal/catalog"
	"smart-router/metrics"
	"smart-router/routing"
	"smart-router/signing"
	"smart-router/store"
	"smart-router/tests/integration/mocks"
)

func TestMockHostProviderFlowUsesOpaqueBindingsAndPersistsCooldown(t *testing.T) {
	const (
		firstBindingID  = "bnd_AAAAAAAAAAAAAAAAAAAAAAAA"
		secondBindingID = "bnd_BBBBBBBBBBBBBBBBBBBBBBBB"
	)
	ctx := context.Background()
	database, err := store.OpenSQLite(ctx, t.TempDir()+"/router.db")
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.UpsertSource(ctx, store.Source{
		ID: "fixture-source", Connector: "ebond", Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertSource() error = %v", err)
	}
	for _, binding := range []store.BindingRecord{
		{
			ID: firstBindingID, SourceID: "fixture-source", Group: "default",
			Model: "model-a", Protocol: api.ProtocolChat,
			ProtocolPlugin: "chat-v1", Enabled: true, CostMicrounits: 10,
		},
		{
			ID: secondBindingID, SourceID: "fixture-source", Group: "default",
			Model: "model-a", Protocol: api.ProtocolChat,
			ProtocolPlugin: "chat-v1", Enabled: true, CostMicrounits: 20,
		},
	} {
		if err := database.UpsertBinding(ctx, binding); err != nil {
			t.Fatalf("UpsertBinding(%q) error = %v", binding.ID, err)
		}
		if err := database.SetObservation(ctx, binding.ID, store.Observation{
			Health: routing.HealthHealthy, Fingerprint: strings.Repeat("f", 64),
			ObservedAt: time.Unix(1, 0),
		}); err != nil {
			t.Fatalf("SetObservation(%q) error = %v", binding.ID, err)
		}
	}

	key := []byte(strings.Repeat("k", 32))
	now := time.Unix(1700000000, 0).UTC()
	runtime := catalog.NewRuntime(catalog.RuntimeConfig{
		Store: database, SigningKey: key, Now: func() time.Time { return now },
		SnapshotTTL: time.Minute, ConfigSHA256: strings.Repeat("a", 64),
		PluginManifestSHA256: strings.Repeat("b", 64),
	})
	registry := metrics.New()
	router := httptest.NewServer(api.NewServer(runtime, registry))
	t.Cleanup(router.Close)

	firstProvider := mocks.NewProvider(http.StatusTooManyRequests, 3000)
	t.Cleanup(firstProvider.Close)
	secondProvider := mocks.NewProvider(http.StatusOK, 0)
	t.Cleanup(secondProvider.Close)
	host := mocks.Host{
		RouterURL: router.URL, SigningKey: key,
		Providers: map[string]*mocks.Provider{
			firstBindingID: firstProvider, secondBindingID: secondProvider,
		},
		Clock: &fakeClock{now: now},
	}
	request := api.RouteRequest{
		Kind: "route_request", SchemaVersion: 1, RequestID: "req-1",
		Group: "default", Model: "model-a", Protocol: api.ProtocolChat,
		AttemptLimit: 2,
	}
	execution, err := host.Execute(ctx, request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Join(execution.Attempts, ",") != firstBindingID+","+secondBindingID {
		t.Fatalf("attempts = %#v, want price order then retry", execution.Attempts)
	}
	if firstProvider.Calls() != 1 || secondProvider.Calls() != 1 {
		t.Fatalf("provider calls = %d/%d, want 1/1", firstProvider.Calls(), secondProvider.Calls())
	}

	request.RequestID = "req-2"
	execution, err = host.Execute(ctx, request)
	if err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if len(execution.Attempts) != 1 || execution.Attempts[0] != secondBindingID {
		t.Fatalf("attempts during cooldown = %#v, want second binding only", execution.Attempts)
	}

	response, err := http.Get(router.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics error = %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if !strings.Contains(string(body), `route="route",status="200"} 2`) ||
		!strings.Contains(string(body), `route="outcome",status="204"} 3`) {
		t.Fatalf("metrics = %q", body)
	}
}

func TestSubscriptionExpiredOutcomeEjectsBindingAndPersistsAcrossReopen(t *testing.T) {
	const (
		firstBindingID   = "bnd_AAAAAAAAAAAAAAAAAAAAAAAA"
		secondBindingID  = "bnd_BBBBBBBBBBBBBBBBBBBBBBBB"
		siblingBindingID = "bnd_CCCCCCCCCCCCCCCCCCCCCCCC"
	)
	ctx := context.Background()
	fixture := &sqliteRouterHTTPFixture{
		path: t.TempDir() + "/router.db",
		key:  []byte(strings.Repeat("k", 32)),
		now:  time.Unix(1700000000, 0).UTC(),
	}
	fixture.open(t)
	t.Cleanup(func() { _ = fixture.database.Close() })
	require.NoError(t, fixture.database.UpsertSource(ctx, store.Source{
		ID: "fixture-source", Connector: "ebond", Enabled: true,
	}))
	for _, binding := range []store.BindingRecord{
		{
			ID: firstBindingID, SourceID: "fixture-source", Group: "default",
			Model: "model-a", Protocol: api.ProtocolChat,
			ProtocolPlugin: "chat-v1", Enabled: true, CostMicrounits: 10,
		},
		{
			ID: secondBindingID, SourceID: "fixture-source", Group: "default",
			Model: "model-a", Protocol: api.ProtocolChat,
			ProtocolPlugin: "chat-v1", Enabled: true, CostMicrounits: 20,
		},
		{
			ID: siblingBindingID, SourceID: "fixture-source", Group: "default",
			Model: "model-c", Protocol: api.ProtocolChat,
			ProtocolPlugin: "chat-v1", Enabled: true, CostMicrounits: 5,
		},
	} {
		require.NoError(t, fixture.database.UpsertBinding(ctx, binding))
		require.NoError(t, fixture.database.SetObservation(ctx, binding.ID, store.Observation{
			Health: routing.HealthHealthy, Fingerprint: strings.Repeat("f", 64),
			ObservedAt: fixture.now,
		}))
	}
	want := fixture.candidates(t)
	require.Len(t, want, 3)
	for _, candidate := range want {
		require.Equal(t, routing.HealthHealthy, candidate.Health)
		require.True(t, candidate.CooldownUntil.IsZero())
	}
	initial := fixture.route(t, "req-initial", "model-a", firstBindingID, secondBindingID)
	sibling := fixture.route(t, "req-sibling", "model-c", siblingBindingID)
	initialSnapshot := fixture.snapshot(t)
	assert.Equal(t, []api.SnapshotRoute{
		{Group: "default", Model: "model-a", Protocol: api.ProtocolChat, Bindings: initial.Bindings},
		{Group: "default", Model: "model-c", Protocol: api.ProtocolChat, Bindings: sibling.Bindings},
	}, initialSnapshot.Routes)

	outcome := api.AttemptOutcome{
		Kind: "attempt_outcome", SchemaVersion: 1, EventID: "evt-expired",
		RequestID: initial.RequestID, BindingID: firstBindingID,
		Protocol: api.ProtocolChat, Outcome: api.Outcome("subscription_expired"),
	}
	deadline := fixture.now.Add(15 * time.Minute)
	fixture.request(t, http.MethodPost, "/v1/outcomes", outcome, http.StatusNoContent, nil)
	first := want[firstBindingID]
	first.Health = routing.HealthDegraded
	first.CooldownUntil = deadline
	want[firstBindingID] = first
	assert.Equal(t, want, fixture.candidates(t))
	fresh := fixture.route(t, "req-fresh", "model-a", secondBindingID)
	fixture.route(t, "req-sibling-fresh", "model-c", siblingBindingID)
	freshSnapshot := fixture.snapshot(t)
	assert.NotEqual(t, initialSnapshot.Revision, freshSnapshot.Revision)
	assert.Equal(t, fresh.SnapshotRevision, freshSnapshot.Revision)
	assert.Equal(t, []api.SnapshotRoute{
		{Group: "default", Model: "model-a", Protocol: api.ProtocolChat, Bindings: fresh.Bindings},
		{Group: "default", Model: "model-c", Protocol: api.ProtocolChat, Bindings: sibling.Bindings},
	}, freshSnapshot.Routes)

	var contractError api.ContractError
	fixture.request(t, http.MethodPost, "/v1/route", api.RouteRequest{
		Kind: "route_request", SchemaVersion: 1, RequestID: "req-stale",
		Group: "default", Model: "model-a", Protocol: api.ProtocolChat,
		AttemptLimit: 2, SnapshotRevision: initialSnapshot.Revision,
	}, http.StatusConflict, &contractError)
	assert.Equal(t, api.ErrorContractMismatch, contractError.Code)

	fixture.now = fixture.now.Add(time.Minute)
	fixture.request(t, http.MethodPost, "/v1/outcomes", outcome, http.StatusNoContent, nil)
	assert.Equal(t, want, fixture.candidates(t), "duplicate must not extend cooldown")
	conflict := outcome
	conflict.LatencyMS = 1
	fixture.request(t, http.MethodPost, "/v1/outcomes", conflict, http.StatusConflict, &contractError)
	assert.Equal(t, api.ErrorOutcomeConflict, contractError.Code)
	assert.Equal(t, want, fixture.candidates(t), "conflict must not mutate state")

	for _, invalid := range []api.AttemptOutcome{
		{
			Kind: "attempt_outcome", SchemaVersion: 1, EventID: "evt-wrong-protocol",
			RequestID: initial.RequestID, BindingID: secondBindingID,
			Protocol: api.ProtocolMessages, Outcome: api.Outcome("subscription_expired"),
		},
		{
			Kind: "attempt_outcome", SchemaVersion: 1, EventID: "evt-missing-binding",
			RequestID: initial.RequestID, BindingID: "bnd_DDDDDDDDDDDDDDDDDDDDDDDD",
			Protocol: api.ProtocolChat, Outcome: api.Outcome("subscription_expired"),
		},
	} {
		fixture.request(t, http.MethodPost, "/v1/outcomes", invalid, http.StatusServiceUnavailable, &contractError)
		assert.Equal(t, api.ErrorBindingUnavailable, contractError.Code)
		assert.Equal(t, want, fixture.candidates(t), "invalid binding/protocol must not affect siblings")
	}
	for _, terminal := range []api.Outcome{
		api.OutcomeClient4xx, api.OutcomeWriteUnknown, api.OutcomeResponseCommitted,
	} {
		control := outcome
		control.EventID = "evt-control-" + string(terminal)
		control.BindingID = secondBindingID
		control.Outcome = terminal
		fixture.request(t, http.MethodPost, "/v1/outcomes", control, http.StatusNoContent, nil)
		assert.Equal(t, want, fixture.candidates(t), "terminal outcome %s must not eject", terminal)
	}
	for _, unsupported := range []api.Outcome{
		api.Outcome("unsupported"), api.Outcome("subscription_expired_future"),
	} {
		control := outcome
		control.EventID = "evt-unsupported-" + string(unsupported)
		control.BindingID = secondBindingID
		control.Outcome = unsupported
		fixture.request(t, http.MethodPost, "/v1/outcomes", control, http.StatusBadRequest, &contractError)
		assert.Equal(t, api.ErrorInvalidRequest, contractError.Code)
		assert.Equal(t, want, fixture.candidates(t))
	}

	require.NoError(t, fixture.database.Close())
	fixture.open(t)
	assert.Equal(t, want, fixture.candidates(t))
	fixture.route(t, "req-reopened", "model-a", secondBindingID)
	fixture.now = deadline.Add(-time.Millisecond)
	fixture.route(t, "req-before-deadline", "model-a", secondBindingID)
	fixture.now = deadline
	fixture.route(t, "req-at-deadline", "model-a", firstBindingID, secondBindingID)
	assert.Equal(t, want, fixture.candidates(t), "expiry permits degraded bindings without resetting health")

	outcome.EventID = "evt-expired-again"
	fixture.request(t, http.MethodPost, "/v1/outcomes", outcome, http.StatusNoContent, nil)
	first.CooldownUntil = fixture.now.Add(15 * time.Minute)
	want[firstBindingID] = first
	assert.Equal(t, want, fixture.candidates(t))
	fixture.route(t, "req-ejected-again", "model-a", secondBindingID)
	outcome.EventID = "evt-distinct-success"
	outcome.Outcome = api.OutcomeSuccess
	fixture.request(t, http.MethodPost, "/v1/outcomes", outcome, http.StatusNoContent, nil)
	first.Health = routing.HealthHealthy
	first.CooldownUntil = time.Time{}
	want[firstBindingID] = first
	assert.Equal(t, want, fixture.candidates(t))
	fixture.route(t, "req-recovered", "model-a", firstBindingID, secondBindingID)

	auth := outcome
	auth.EventID = "evt-auth-control"
	auth.BindingID = siblingBindingID
	auth.Outcome = api.OutcomeAuth401
	fixture.request(t, http.MethodPost, "/v1/outcomes", auth, http.StatusNoContent, nil)
	authDeadline := fixture.now.Add(15 * time.Minute)
	candidate := want[siblingBindingID]
	candidate.Health = routing.HealthUnhealthy
	candidate.CooldownUntil = authDeadline
	want[siblingBindingID] = candidate
	assert.Equal(t, want, fixture.candidates(t))
	fixture.now = authDeadline
	fixture.request(t, http.MethodPost, "/v1/route", api.RouteRequest{
		Kind: "route_request", SchemaVersion: 1, RequestID: "req-auth-at-deadline",
		Group: "default", Model: "model-c", Protocol: api.ProtocolChat, AttemptLimit: 2,
	}, http.StatusServiceUnavailable, &contractError)
	assert.Equal(t, api.ErrorBindingUnavailable, contractError.Code)
	assert.Equal(t, want, fixture.candidates(t), "auth_401 must remain unhealthy after its deadline")
}

func TestMockHostWaitsForRetryAfterBeforeNextProviderAttempt(t *testing.T) {
	const (
		firstBindingID  = "bnd_AAAAAAAAAAAAAAAAAAAAAAAA"
		secondBindingID = "bnd_BBBBBBBBBBBBBBBBBBBBBBBB"
	)
	key := []byte(strings.Repeat("k", 32))
	start := time.Unix(1700000000, 0).UTC()
	routeResponse := api.RouteResponse{
		Kind: "route_response", SchemaVersion: 1, RequestID: "req-delay",
		SnapshotRevision: "rev-1", ExpiresAt: start.Add(time.Minute).UnixMilli(),
		Bindings: []api.Binding{
			{BindingID: firstBindingID, ProtocolPlugin: "chat-v1", Rank: 1},
			{BindingID: secondBindingID, ProtocolPlugin: "chat-v1", Rank: 2},
		},
	}
	signature, err := signing.Sign(routeResponse, key)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	routeResponse.Signature = signature
	router := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/v1/route":
				writer.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(writer).Encode(routeResponse); err != nil {
					t.Errorf("encode route response: %v", err)
				}
			case "/v1/outcomes":
				writer.WriteHeader(http.StatusNoContent)
			default:
				writer.WriteHeader(http.StatusNotFound)
			}
		},
	))
	t.Cleanup(router.Close)

	firstProvider := mocks.NewProvider(http.StatusTooManyRequests, 3000)
	t.Cleanup(firstProvider.Close)
	secondProvider := mocks.NewProvider(http.StatusOK, 0)
	t.Cleanup(secondProvider.Close)
	clock := &fakeClock{now: start}
	host := mocks.Host{
		RouterURL: router.URL, SigningKey: key,
		Providers: map[string]*mocks.Provider{
			firstBindingID: firstProvider, secondBindingID: secondProvider,
		},
		Clock: clock,
	}
	execution, err := host.Execute(context.Background(), api.RouteRequest{
		Kind: "route_request", SchemaVersion: 1, RequestID: "req-delay",
		Group: "default", Model: "model-a", Protocol: api.ProtocolChat,
		AttemptLimit: 2,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != 3*time.Second {
		t.Fatalf("clock sleeps = %v, want [3s]", clock.sleeps)
	}
	if len(execution.AttemptedAt) != 2 {
		t.Fatalf("attempt timestamps = %v, want two", execution.AttemptedAt)
	}
	if delay := execution.AttemptedAt[1].Sub(execution.AttemptedAt[0]); delay != 3*time.Second {
		t.Fatalf("second provider attempted after %v, want 3s", delay)
	}
}

func TestMockHostRejectsRouteAtInjectedClockExpiryBeforeProviderAttempt(t *testing.T) {
	const bindingID = "bnd_AAAAAAAAAAAAAAAAAAAAAAAA"
	key := []byte(strings.Repeat("k", 32))
	expiresAt := time.Unix(1700000000, 0).UTC()
	routeResponse := api.RouteResponse{
		Kind: "route_response", SchemaVersion: 1, RequestID: "req-expired",
		SnapshotRevision: "rev-1", ExpiresAt: expiresAt.UnixMilli(),
		Bindings: []api.Binding{{
			BindingID: bindingID, ProtocolPlugin: "chat-v1", Rank: 1,
		}},
	}
	signature, err := signing.Sign(routeResponse, key)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	routeResponse.Signature = signature
	router := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/v1/route":
				writer.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(writer).Encode(routeResponse); err != nil {
					t.Errorf("encode route response: %v", err)
				}
			default:
				writer.WriteHeader(http.StatusNotFound)
			}
		},
	))
	t.Cleanup(router.Close)

	provider := mocks.NewProvider(http.StatusOK, 0)
	t.Cleanup(provider.Close)
	host := mocks.Host{
		RouterURL: router.URL, SigningKey: key,
		Providers: map[string]*mocks.Provider{bindingID: provider},
		Clock:     &fakeClock{now: expiresAt},
	}
	_, err = host.Execute(context.Background(), api.RouteRequest{
		Kind: "route_request", SchemaVersion: 1, RequestID: "req-expired",
		Group: "default", Model: "model-a", Protocol: api.ProtocolChat,
		AttemptLimit: 1,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want expired route rejection")
	}
	if provider.Calls() != 0 {
		t.Fatalf("provider calls = %d, want zero for expired route", provider.Calls())
	}
}

func TestMockHostRechecksExpiryAfterRetryDelay(t *testing.T) {
	const (
		firstBindingID  = "bnd_AAAAAAAAAAAAAAAAAAAAAAAA"
		secondBindingID = "bnd_BBBBBBBBBBBBBBBBBBBBBBBB"
	)
	key := []byte(strings.Repeat("k", 32))
	start := time.Unix(1700000000, 0).UTC()
	routeResponse := api.RouteResponse{
		Kind: "route_response", SchemaVersion: 1, RequestID: "req-expiry-delay",
		SnapshotRevision: "rev-1", ExpiresAt: start.Add(2 * time.Second).UnixMilli(),
		Bindings: []api.Binding{
			{BindingID: firstBindingID, ProtocolPlugin: "chat-v1", Rank: 1},
			{BindingID: secondBindingID, ProtocolPlugin: "chat-v1", Rank: 2},
		},
	}
	signature, err := signing.Sign(routeResponse, key)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	routeResponse.Signature = signature
	router := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/v1/route":
				writer.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(writer).Encode(routeResponse); err != nil {
					t.Errorf("encode route response: %v", err)
				}
			case "/v1/outcomes":
				writer.WriteHeader(http.StatusNoContent)
			default:
				writer.WriteHeader(http.StatusNotFound)
			}
		},
	))
	t.Cleanup(router.Close)

	firstProvider := mocks.NewProvider(http.StatusTooManyRequests, 3000)
	t.Cleanup(firstProvider.Close)
	secondProvider := mocks.NewProvider(http.StatusOK, 0)
	t.Cleanup(secondProvider.Close)
	clock := &fakeClock{now: start}
	host := mocks.Host{
		RouterURL: router.URL, SigningKey: key,
		Providers: map[string]*mocks.Provider{
			firstBindingID: firstProvider, secondBindingID: secondProvider,
		},
		Clock: clock,
	}
	_, err = host.Execute(context.Background(), api.RouteRequest{
		Kind: "route_request", SchemaVersion: 1, RequestID: "req-expiry-delay",
		Group: "default", Model: "model-a", Protocol: api.ProtocolChat,
		AttemptLimit: 2,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want expiry after Retry-After rejection")
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != 3*time.Second {
		t.Fatalf("clock sleeps = %v, want [3s]", clock.sleeps)
	}
	if firstProvider.Calls() != 1 || secondProvider.Calls() != 0 {
		t.Fatalf(
			"provider calls = %d/%d, want first only before expiry",
			firstProvider.Calls(),
			secondProvider.Calls(),
		)
	}
}

func TestMockHostFailsClosedOnRetryAfterOver24Hours(t *testing.T) {
	const (
		firstBindingID  = "bnd_AAAAAAAAAAAAAAAAAAAAAAAA"
		secondBindingID = "bnd_BBBBBBBBBBBBBBBBBBBBBBBB"
	)
	key := []byte(strings.Repeat("k", 32))
	start := time.Unix(1700000000, 0).UTC()
	routeResponse := api.RouteResponse{
		Kind: "route_response", SchemaVersion: 1, RequestID: "req-retry-invalid",
		SnapshotRevision: "rev-1", ExpiresAt: start.Add(time.Minute).UnixMilli(),
		Bindings: []api.Binding{
			{BindingID: firstBindingID, ProtocolPlugin: "chat-v1", Rank: 1},
			{BindingID: secondBindingID, ProtocolPlugin: "chat-v1", Rank: 2},
		},
	}
	signature, err := signing.Sign(routeResponse, key)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	routeResponse.Signature = signature
	router := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/v1/route":
				writer.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(writer).Encode(routeResponse); err != nil {
					t.Errorf("encode route response: %v", err)
				}
			case "/v1/outcomes":
				writer.WriteHeader(http.StatusNoContent)
			default:
				writer.WriteHeader(http.StatusNotFound)
			}
		},
	))
	t.Cleanup(router.Close)

	firstProvider := mocks.NewProvider(
		http.StatusTooManyRequests,
		(24*time.Hour).Milliseconds()+1,
	)
	t.Cleanup(firstProvider.Close)
	secondProvider := mocks.NewProvider(http.StatusOK, 0)
	t.Cleanup(secondProvider.Close)
	clock := &fakeClock{now: start}
	host := mocks.Host{
		RouterURL: router.URL, SigningKey: key,
		Providers: map[string]*mocks.Provider{
			firstBindingID: firstProvider, secondBindingID: secondProvider,
		},
		Clock: clock,
	}
	_, err = host.Execute(context.Background(), api.RouteRequest{
		Kind: "route_request", SchemaVersion: 1, RequestID: "req-retry-invalid",
		Group: "default", Model: "model-a", Protocol: api.ProtocolChat,
		AttemptLimit: 2,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid Retry-After fail-closed error")
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("clock sleeps = %v, want none for invalid Retry-After", clock.sleeps)
	}
	if firstProvider.Calls() != 1 || secondProvider.Calls() != 0 {
		t.Fatalf(
			"provider calls = %d/%d, want first only",
			firstProvider.Calls(),
			secondProvider.Calls(),
		)
	}
}

func TestMockHostFailsClosedOnMalformedRetryAfter(t *testing.T) {
	const (
		firstBindingID  = "bnd_AAAAAAAAAAAAAAAAAAAAAAAA"
		secondBindingID = "bnd_BBBBBBBBBBBBBBBBBBBBBBBB"
	)
	key := []byte(strings.Repeat("k", 32))
	start := time.Unix(1700000000, 0).UTC()
	routeResponse := api.RouteResponse{
		Kind: "route_response", SchemaVersion: 1, RequestID: "req-retry-malformed",
		SnapshotRevision: "rev-1", ExpiresAt: start.Add(time.Minute).UnixMilli(),
		Bindings: []api.Binding{
			{BindingID: firstBindingID, ProtocolPlugin: "chat-v1", Rank: 1},
			{BindingID: secondBindingID, ProtocolPlugin: "chat-v1", Rank: 2},
		},
	}
	signature, err := signing.Sign(routeResponse, key)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	routeResponse.Signature = signature
	router := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/v1/route":
				writer.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(writer).Encode(routeResponse); err != nil {
					t.Errorf("encode route response: %v", err)
				}
			case "/v1/outcomes":
				writer.WriteHeader(http.StatusNoContent)
			default:
				writer.WriteHeader(http.StatusNotFound)
			}
		},
	))
	t.Cleanup(router.Close)

	firstProvider := mocks.NewProviderWithRetryAfterHeader(
		http.StatusTooManyRequests,
		"not-a-duration",
	)
	t.Cleanup(firstProvider.Close)
	secondProvider := mocks.NewProvider(http.StatusOK, 0)
	t.Cleanup(secondProvider.Close)
	host := mocks.Host{
		RouterURL: router.URL, SigningKey: key,
		Providers: map[string]*mocks.Provider{
			firstBindingID: firstProvider, secondBindingID: secondProvider,
		},
		Clock: &fakeClock{now: start},
	}
	_, err = host.Execute(context.Background(), api.RouteRequest{
		Kind: "route_request", SchemaVersion: 1, RequestID: "req-retry-malformed",
		Group: "default", Model: "model-a", Protocol: api.ProtocolChat,
		AttemptLimit: 2,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want malformed Retry-After rejection")
	}
	if firstProvider.Calls() != 1 || secondProvider.Calls() != 0 {
		t.Fatalf(
			"provider calls = %d/%d, want first only",
			firstProvider.Calls(),
			secondProvider.Calls(),
		)
	}
}

type sqliteRouterHTTPFixture struct {
	path     string
	key      []byte
	now      time.Time
	database *store.DB
	handler  http.Handler
}

func (fixture *sqliteRouterHTTPFixture) open(t *testing.T) {
	t.Helper()
	database, err := store.OpenSQLite(context.Background(), fixture.path)
	require.NoError(t, err)
	fixture.database = database
	runtime := catalog.NewRuntime(catalog.RuntimeConfig{
		Store: database, SigningKey: fixture.key, Now: func() time.Time { return fixture.now },
		SnapshotTTL: time.Minute, HysteresisMicrounits: 0,
		ConfigSHA256: strings.Repeat("a", 64), PluginManifestSHA256: strings.Repeat("b", 64),
	})
	fixture.handler = api.NewServer(runtime, metrics.New())
}

func (fixture *sqliteRouterHTTPFixture) request(
	t *testing.T, method, path string, value any, status int, response api.Validatable,
) {
	t.Helper()
	var body []byte
	if value != nil {
		var err error
		body, err = json.Marshal(value)
		require.NoError(t, err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	require.Equal(t, status, recorder.Code, "%s %s: %s", method, path, recorder.Body.String())
	if response != nil {
		require.NoError(t, api.DecodeStrict(recorder.Body, response))
	} else {
		assert.Empty(t, recorder.Body.String())
	}
}

func (fixture *sqliteRouterHTTPFixture) route(
	t *testing.T, requestID, model string, bindingIDs ...string,
) api.RouteResponse {
	t.Helper()
	var response api.RouteResponse
	fixture.request(t, http.MethodPost, "/v1/route", api.RouteRequest{
		Kind: "route_request", SchemaVersion: 1, RequestID: requestID,
		Group: "default", Model: model, Protocol: api.ProtocolChat, AttemptLimit: 2,
	}, http.StatusOK, &response)
	require.NoError(t, signing.Verify(response, fixture.key, response.Signature))
	assert.Equal(t, requestID, response.RequestID)
	require.Len(t, response.Bindings, len(bindingIDs))
	for index, bindingID := range bindingIDs {
		assert.Equal(t, bindingID, response.Bindings[index].BindingID)
		assert.Equal(t, index+1, response.Bindings[index].Rank)
	}
	return response
}

func (fixture *sqliteRouterHTTPFixture) snapshot(t *testing.T) api.SignedSnapshot {
	t.Helper()
	var snapshot api.SignedSnapshot
	fixture.request(t, http.MethodGet, "/v1/snapshot", nil, http.StatusOK, &snapshot)
	require.NoError(t, signing.Verify(snapshot, fixture.key, snapshot.Signature))
	return snapshot
}

func (fixture *sqliteRouterHTTPFixture) candidates(t *testing.T) map[string]routing.Candidate {
	t.Helper()
	result := make(map[string]routing.Candidate)
	for _, model := range []string{"model-a", "model-c"} {
		candidates, err := fixture.database.ListCandidates(context.Background(), "default", model, api.ProtocolChat)
		require.NoError(t, err)
		for _, candidate := range candidates {
			result[candidate.BindingID] = candidate
		}
	}
	return result
}

type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (clock *fakeClock) Now() time.Time {
	return clock.now
}

func (clock *fakeClock) Sleep(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		clock.sleeps = append(clock.sleeps, delay)
		clock.now = clock.now.Add(delay)
		return nil
	}
}
