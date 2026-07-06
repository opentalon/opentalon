package provider

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// fakeProvider is a minimal Provider for exercising the routing/health logic.
type fakeProvider struct {
	id        string
	model     string
	failNext  bool   // when true, Complete returns an error
	lastModel string // model id it was last called with
	calls     int
}

func (f *fakeProvider) ID() string                   { return f.id }
func (f *fakeProvider) SupportsFeature(Feature) bool { return true }
func (f *fakeProvider) Models() []ModelInfo          { return []ModelInfo{{ID: f.model, ProviderID: f.id}} }

func (f *fakeProvider) Complete(_ context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	f.calls++
	f.lastModel = req.Model
	if f.failNext {
		return nil, errors.New("boom")
	}
	return &CompletionResponse{ID: f.id, Model: req.Model, Content: "ok-" + f.id}, nil
}

func (f *fakeProvider) Stream(_ context.Context, req *CompletionRequest) (ResponseStream, error) {
	f.calls++
	f.lastModel = req.Model
	if f.failNext {
		return nil, errors.New("boom")
	}
	return nil, errors.New("stream not exercised in these tests")
}

// newTestHG builds a health-gated provider directly (no background goroutine)
// so the health state can be driven deterministically via probeOnce/trip.
func newTestHG(preferred, fallback *fakeProvider, recoverAfter int, probe HealthProbe) *healthGatedProvider {
	h := &endpointHealth{
		probe:        probe,
		recoverAfter: recoverAfter,
		interval:     time.Hour,
		timeout:      time.Second,
		log:          slog.Default(),
	}
	h.healthy.Store(true)
	h.consecutiveOK = recoverAfter
	return &healthGatedProvider{
		entries: []ProviderEntry{
			{Prov: preferred, Model: preferred.model},
			{Prov: fallback, Model: fallback.model},
		},
		health: h,
		log:    slog.Default(),
	}
}

func TestHealthGatedRoutingAndHysteresis(t *testing.T) {
	ctx := context.Background()
	req := &CompletionRequest{Model: "ignored-by-wrapper"}

	preferred := &fakeProvider{id: "dedicated", model: "gpt-oss-120b"}
	fallback := &fakeProvider{id: "shared", model: "gpt-oss-120b-ovh"}
	var probeErr error
	hg := newTestHG(preferred, fallback, 2, func(context.Context) error { return probeErr })

	// 1. Healthy -> preferred, and the wrapper overrides the model id.
	resp, err := hg.Complete(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "ok-dedicated" {
		t.Fatalf("expected preferred provider, got %q", resp.Content)
	}
	if preferred.lastModel != "gpt-oss-120b" {
		t.Fatalf("expected model overridden to preferred id, got %q", preferred.lastModel)
	}

	// 2. Preferred fails live -> falls back to shared AND trips health.
	preferred.failNext = true
	resp, err = hg.Complete(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "ok-shared" {
		t.Fatalf("expected fallback after live failure, got %q", resp.Content)
	}
	if fallback.lastModel != "gpt-oss-120b-ovh" {
		t.Fatalf("expected fallback's own model id, got %q", fallback.lastModel)
	}
	if hg.health.isHealthy() {
		t.Fatal("expected health tripped after a live failure")
	}

	// 3. While unhealthy, requests go straight to the fallback (preferred not tried first).
	preferred.failNext = false
	preferred.calls, fallback.calls = 0, 0
	resp, _ = hg.Complete(ctx, req)
	if resp.Content != "ok-shared" {
		t.Fatalf("expected fallback while unhealthy, got %q", resp.Content)
	}
	if preferred.calls != 0 {
		t.Fatalf("preferred should not be tried first while unhealthy, calls=%d", preferred.calls)
	}

	// 4. Recovery needs RecoverAfter (2) consecutive healthy probes — one is not enough.
	probeErr = nil
	hg.health.probeOnce(ctx)
	if hg.health.isHealthy() {
		t.Fatal("one healthy probe must not recover when recover_after=2")
	}
	hg.health.probeOnce(ctx)
	if !hg.health.isHealthy() {
		t.Fatal("two healthy probes should recover")
	}

	// 5. Recovered -> routes back to preferred.
	preferred.calls = 0
	resp, _ = hg.Complete(ctx, req)
	if resp.Content != "ok-dedicated" {
		t.Fatalf("expected preferred after recovery, got %q", resp.Content)
	}
}

func TestHealthGatedProbeFailureTripsAndRecovers(t *testing.T) {
	ctx := context.Background()
	preferred := &fakeProvider{id: "dedicated", model: "m1"}
	fallback := &fakeProvider{id: "shared", model: "m2"}
	probeErr := error(errors.New("down"))
	hg := newTestHG(preferred, fallback, 1, func(context.Context) error { return probeErr })

	// A failed probe trips health even without any live traffic.
	hg.health.probeOnce(ctx)
	if hg.health.isHealthy() {
		t.Fatal("a failed probe should trip health")
	}
	resp, _ := hg.Complete(ctx, &CompletionRequest{})
	if resp.Content != "ok-shared" {
		t.Fatalf("expected fallback after probe failure, got %q", resp.Content)
	}

	// With recover_after=1, a single healthy probe restores the preferred endpoint.
	probeErr = nil
	hg.health.probeOnce(ctx)
	if !hg.health.isHealthy() {
		t.Fatal("healthy probe should recover with recover_after=1")
	}
}

func TestHealthGatedModelsUnion(t *testing.T) {
	preferred := &fakeProvider{id: "dedicated", model: "m1"}
	fallback := &fakeProvider{id: "shared", model: "m2"}
	hg := newTestHG(preferred, fallback, 1, nil)
	if got := len(hg.Models()); got != 2 {
		t.Fatalf("expected union of 2 models, got %d", got)
	}
	if hg.ID() != "dedicated" {
		t.Fatalf("expected wrapper ID to be preferred's, got %q", hg.ID())
	}
}
