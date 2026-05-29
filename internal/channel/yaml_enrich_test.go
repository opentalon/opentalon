package channel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newEnrichTestChannel builds a YAMLChannel with no spec wired up beyond
// the inbound.enrich section the test cares about. selfVars/config start
// empty; tests assign them directly when a step's template needs them.
func newEnrichTestChannel(enrich map[string]EnrichSpec, cache EnrichCache) *YAMLChannel {
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			ID: "slack",
			Inbound: InboundSpec{
				Enrich: enrich,
			},
		},
		instanceID:  "slack-test",
		selfVars:    make(map[string]string),
		config:      make(map[string]string),
		client:      &http.Client{Timeout: 5 * time.Second},
		enrichCache: cache,
	}
	ch.ctx = context.Background()
	return ch
}

// TestRunEnrich_Success: a single step that resolves a template URL,
// receives a JSON response, and extracts the configured fields ends up in
// the per-message context under enrich.<step>.<field>.
func TestRunEnrich_Success(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U1","profile":{"email":"alex@example.com"},"real_name":"Alex"}}`))
	}))
	defer srv.Close()

	ch := newEnrichTestChannel(map[string]EnrichSpec{
		"user": {
			Method: http.MethodPost,
			URL:    srv.URL,
			Extract: map[string]string{
				"email":     "user.profile.email",
				"real_name": "user.real_name",
			},
		},
	}, nil)

	contexts := map[string]map[string]string{"event": {"user": "U1"}}
	got, err := ch.runEnrich(context.Background(), contexts)
	if err != nil {
		t.Fatalf("runEnrich: %v", err)
	}
	if got["user"]["email"] != "alex@example.com" {
		t.Errorf("email = %q, want alex@example.com", got["user"]["email"])
	}
	if got["user"]["real_name"] != "Alex" {
		t.Errorf("real_name = %q, want Alex", got["user"]["real_name"])
	}
	// Verify the step's results are also visible under enrich.<step> in
	// the contexts map so chained metadata templates can reference them.
	if v := contexts["enrich.user"]["email"]; v != "alex@example.com" {
		t.Errorf("contexts[enrich.user][email] = %q, want propagated", v)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("server calls = %d, want 1", atomic.LoadInt32(&calls))
	}
}

// TestRunEnrich_CacheHit: a second call with a configured cache key reuses
// the cached payload instead of hitting the HTTP endpoint again. This is
// the load-bearing optimisation that keeps users.info quota usable.
func TestRunEnrich_CacheHit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"user":{"profile":{"email":"a@b"}}}`))
	}))
	defer srv.Close()

	cache := newMemoryEnrichCache()
	ch := newEnrichTestChannel(map[string]EnrichSpec{
		"user": {
			URL:     srv.URL,
			Extract: map[string]string{"email": "user.profile.email"},
			Cache:   EnrichCacheSpec{Key: "{{event.user}}", TTL: time.Minute},
		},
	}, cache)

	contexts := map[string]map[string]string{"event": {"user": "U1"}}
	if _, err := ch.runEnrich(context.Background(), contexts); err != nil {
		t.Fatalf("first runEnrich: %v", err)
	}
	if _, err := ch.runEnrich(context.Background(), contexts); err != nil {
		t.Fatalf("second runEnrich: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("server calls = %d, want 1 (second call should be cached)", atomic.LoadInt32(&calls))
	}
}

// TestRunEnrich_CacheIsolatedPerInstance: two YAMLChannel instances with the
// same kind but distinct instanceIDs do not share cached entries even when
// the resolved cache key (the user id) collides. This is the multi-tenancy
// safety property: an admin bot's enrichment result must never bleed into
// the customer bot's view of the same user.
func TestRunEnrich_CacheIsolatedPerInstance(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"user":{"profile":{"email":"x@y"}}}`))
	}))
	defer srv.Close()

	cache := newMemoryEnrichCache()
	enrich := map[string]EnrichSpec{
		"user": {
			URL:     srv.URL,
			Extract: map[string]string{"email": "user.profile.email"},
			Cache:   EnrichCacheSpec{Key: "{{event.user}}", TTL: time.Minute},
		},
	}

	admin := newEnrichTestChannel(enrich, cache)
	admin.instanceID = "slack-admin"
	customer := newEnrichTestChannel(enrich, cache)
	customer.instanceID = "slack-customer"

	contexts := map[string]map[string]string{"event": {"user": "U1"}}
	if _, err := admin.runEnrich(context.Background(), contexts); err != nil {
		t.Fatalf("admin: %v", err)
	}
	// Re-clone the contexts because runEnrich mutates it by adding enrich.user.
	contexts2 := map[string]map[string]string{"event": {"user": "U1"}}
	if _, err := customer.runEnrich(context.Background(), contexts2); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("server calls = %d, want 2 (cache must not cross instances)", atomic.LoadInt32(&calls))
	}
}

// TestRunEnrich_FailClosedOnHTTPError: a 5xx response from the enrichment
// endpoint surfaces as ErrEnrichmentFailed. This is the contract handler.go
// keys off to emit a user-visible error frame instead of processing the
// message with empty identity data.
func TestRunEnrich_FailClosedOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream is down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ch := newEnrichTestChannel(map[string]EnrichSpec{
		"user": {URL: srv.URL, Extract: map[string]string{"email": "user.profile.email"}},
	}, nil)

	_, err := ch.runEnrich(context.Background(), map[string]map[string]string{"event": {}})
	if !errors.Is(err, ErrEnrichmentFailed) {
		t.Fatalf("err = %v, want ErrEnrichmentFailed", err)
	}
	if step := enrichStepFromErr(err); step != "user" {
		t.Errorf("enrichStepFromErr = %q, want user", step)
	}
}

// TestRunEnrich_FailClosedOnMissingField: a 2xx response that's missing the
// requested extract field is treated as failure. Half-populated identity is
// worse than no identity for the WhoAmI server.
func TestRunEnrich_FailClosedOnMissingField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Valid JSON but no email present — common when a workspace doesn't
		// grant users:read.email.
		_, _ = w.Write([]byte(`{"user":{"real_name":"Alex"}}`))
	}))
	defer srv.Close()

	ch := newEnrichTestChannel(map[string]EnrichSpec{
		"user": {URL: srv.URL, Extract: map[string]string{"email": "user.profile.email"}},
	}, nil)
	_, err := ch.runEnrich(context.Background(), map[string]map[string]string{"event": {}})
	if !errors.Is(err, ErrEnrichmentFailed) {
		t.Fatalf("err = %v, want ErrEnrichmentFailed", err)
	}
}
