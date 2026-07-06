package provider

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ProviderEntry pairs a Provider with the model id used to address it. The
// health-gated wrapper overrides CompletionRequest.Model with this id before
// delegating, so each endpoint may serve the same logical model under its own
// id and record usage against its own configured cost (e.g. a self-hosted
// endpoint at cost 0 vs. a metered public endpoint).
type ProviderEntry struct {
	Prov  Provider
	Model string
}

// HealthProbe reports whether the preferred endpoint is reachable: nil means
// healthy, a non-nil error means unhealthy. It is a function so tests can fake
// it without real network I/O.
type HealthProbe func(context.Context) error

// HealthGateConfig tunes the health-gated fallback. Zero values fall back to
// the defaults below.
type HealthGateConfig struct {
	Interval     time.Duration // how often to probe the preferred endpoint
	Timeout      time.Duration // per-probe timeout
	RecoverAfter int           // consecutive healthy probes required to switch back to the preferred endpoint
}

const (
	defaultHealthInterval     = 10 * time.Second
	defaultHealthTimeout      = 3 * time.Second
	defaultHealthRecoverAfter = 3
)

// healthGatedProvider routes completions to a preferred provider while its
// endpoint is healthy, otherwise to ordered fallbacks. It applies hysteresis:
// it trips to the fallbacks immediately on a failed health probe or a live
// request error, but only switches back after RecoverAfter consecutive healthy
// probes. This prevents flapping when the preferred endpoint is intermittently
// available — e.g. a self-hosted GPU node that is warming up, being cycled on a
// schedule, or briefly unreachable.
type healthGatedProvider struct {
	entries []ProviderEntry // [0] preferred, [1:] fallbacks in priority order
	health  *endpointHealth
	log     *slog.Logger
}

// NewHealthGatedProvider wraps entries[0] (preferred) with entries[1:] as
// ordered fallbacks. When probe is non-nil it starts a background goroutine
// that probes the preferred endpoint every cfg.Interval; the goroutine stops
// when ctx is done. It returns the Provider interface so the wrapper is a
// drop-in replacement for a bare provider.
func NewHealthGatedProvider(ctx context.Context, entries []ProviderEntry, probe HealthProbe, cfg HealthGateConfig, log *slog.Logger) Provider {
	if log == nil {
		log = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultHealthInterval
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	recoverAfter := cfg.RecoverAfter
	if recoverAfter <= 0 {
		recoverAfter = defaultHealthRecoverAfter
	}
	h := &endpointHealth{
		probe:        probe,
		interval:     interval,
		timeout:      timeout,
		recoverAfter: recoverAfter,
		log:          log,
	}
	// Optimistic start: prefer the preferred endpoint from the first request.
	// A failed probe or live error trips it; recovery then needs RecoverAfter
	// consecutive healthy probes.
	h.healthy.Store(true)
	h.consecutiveOK = recoverAfter
	hg := &healthGatedProvider{entries: entries, health: h, log: log}
	if probe != nil {
		go h.run(ctx)
	}
	return hg
}

func (h *healthGatedProvider) ID() string { return h.entries[0].Prov.ID() }

func (h *healthGatedProvider) SupportsFeature(f Feature) bool {
	return h.entries[0].Prov.SupportsFeature(f)
}

// Models returns the union of every backing provider's models so cost and
// usage lookups resolve each endpoint's own model id (and its own cost).
func (h *healthGatedProvider) Models() []ModelInfo {
	out := make([]ModelInfo, 0, len(h.entries))
	for _, e := range h.entries {
		out = append(out, e.Prov.Models()...)
	}
	return out
}

// order returns the indices of entries to try, in priority order: preferred
// first when healthy; otherwise fallbacks first with the preferred endpoint
// kept as a last resort (in case it recovered between probes).
func (h *healthGatedProvider) order() []int {
	n := len(h.entries)
	idx := make([]int, 0, n)
	if h.health.isHealthy() {
		for i := 0; i < n; i++ {
			idx = append(idx, i)
		}
		return idx
	}
	for i := 1; i < n; i++ {
		idx = append(idx, i)
	}
	idx = append(idx, 0)
	return idx
}

func (h *healthGatedProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	var lastErr error
	for _, idx := range h.order() {
		e := h.entries[idx]
		cp := *req
		cp.Model = e.Model
		resp, err := e.Prov.Complete(ctx, &cp)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if idx == 0 {
			h.health.trip()
		}
		if len(h.entries) > 1 {
			h.log.Warn("llm provider failed; trying next endpoint",
				"provider", e.Prov.ID(), "model", e.Model, "error", err)
		}
	}
	return nil, lastErr
}

func (h *healthGatedProvider) Stream(ctx context.Context, req *CompletionRequest) (ResponseStream, error) {
	var lastErr error
	for _, idx := range h.order() {
		e := h.entries[idx]
		cp := *req
		cp.Model = e.Model
		cp.Stream = true
		stream, err := e.Prov.Stream(ctx, &cp)
		if err == nil {
			return stream, nil
		}
		lastErr = err
		if idx == 0 {
			h.health.trip()
		}
		if len(h.entries) > 1 {
			h.log.Warn("llm provider stream failed; trying next endpoint",
				"provider", e.Prov.ID(), "model", e.Model, "error", err)
		}
	}
	return nil, lastErr
}

// endpointHealth tracks the reachability of the preferred endpoint with
// hysteresis on recovery.
type endpointHealth struct {
	healthy      atomic.Bool
	probe        HealthProbe
	interval     time.Duration
	timeout      time.Duration
	recoverAfter int

	mu            sync.Mutex
	consecutiveOK int

	log *slog.Logger
}

func (h *endpointHealth) isHealthy() bool { return h.healthy.Load() }

// trip marks the endpoint unhealthy immediately, called on a live request
// failure against the preferred endpoint. Recovery then requires recoverAfter
// consecutive healthy probes.
func (h *endpointHealth) trip() {
	h.mu.Lock()
	h.consecutiveOK = 0
	h.mu.Unlock()
	if h.healthy.Swap(false) {
		h.log.Warn("preferred llm endpoint tripped to unhealthy after a live failure")
	}
}

func (h *endpointHealth) run(ctx context.Context) {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.probeOnce(ctx)
		}
	}
}

func (h *endpointHealth) probeOnce(ctx context.Context) {
	pctx := ctx
	if h.timeout > 0 {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(ctx, h.timeout)
		defer cancel()
	}
	err := h.probe(pctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.consecutiveOK = 0
		if h.healthy.Swap(false) {
			h.log.Warn("preferred llm endpoint probe failed; falling back", "error", err)
		}
		return
	}
	h.consecutiveOK++
	if !h.healthy.Load() && h.consecutiveOK >= h.recoverAfter {
		h.healthy.Store(true)
		h.log.Info("preferred llm endpoint healthy again; switching back", "consecutive_ok", h.consecutiveOK)
	}
}

// NewHTTPHealthProbe returns a HealthProbe that GETs probeURL with an optional
// bearer token and treats any 2xx response as healthy. probeURL is typically
// the provider base_url joined with "/models" (liveness + auth check) or
// "/health" (liveness only).
func NewHTTPHealthProbe(probeURL, apiKey string, client *http.Client) HealthProbe {
	if client == nil {
		client = &http.Client{Timeout: defaultHealthTimeout}
	}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return err
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("health probe %s returned status %d", probeURL, resp.StatusCode)
		}
		return nil
	}
}
