package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetry keeps the retry behaviour but with negligible waits so retry
// tests run in milliseconds.
func fastRetry() OpenAIOption {
	return WithOpenAIRetryPolicy(RetryPolicy{
		MaxAttempts:  4,
		BaseDelay:    time.Millisecond,
		MaxDelay:     5 * time.Millisecond,
		MaxTotalWait: time.Second,
	})
}

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	permanent := []int{200, 201, 400, 401, 403, 404, 422}
	for _, c := range retryable {
		if !isRetryableStatus(c) {
			t.Errorf("status %d should be retryable", c)
		}
	}
	for _, c := range permanent {
		if isRetryableStatus(c) {
			t.Errorf("status %d should NOT be retryable", c)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	base, maxD := time.Second, 20*time.Second
	// Retry-After (delta-seconds) is honoured verbatim (within the cap).
	h := http.Header{}
	h.Set("Retry-After", "5")
	if got := retryDelay(&http.Response{Header: h}, 1, base, maxD); got != 5*time.Second {
		t.Errorf("Retry-After: got %v, want 5s", got)
	}
	// A Retry-After above the cap is clamped.
	h2 := http.Header{}
	h2.Set("Retry-After", "600")
	if got := retryDelay(&http.Response{Header: h2}, 1, base, maxD); got != maxD {
		t.Errorf("Retry-After cap: got %v, want %v", got, maxD)
	}
	// No response -> exponential backoff, attempt 1 ~= base (+jitter).
	d1 := retryDelay(nil, 1, base, maxD)
	if d1 < base || d1 >= base+100*time.Millisecond {
		t.Errorf("attempt 1 backoff = %v, want ~%v", d1, base)
	}
	// Doubling: attempt 3 ~= 4s (+jitter).
	d3 := retryDelay(nil, 3, base, maxD)
	if d3 < 4*time.Second || d3 >= 4*time.Second+200*time.Millisecond {
		t.Errorf("attempt 3 backoff = %v, want ~4s", d3)
	}
	// Large attempt -> capped at maxD (+ small jitter).
	dBig := retryDelay(nil, 20, base, maxD)
	if dBig < maxD || dBig > maxD+2*time.Second {
		t.Errorf("capped backoff = %v, want ~%v", dBig, maxD)
	}
}

func okBody() []byte {
	b, _ := json.Marshal(oaiResponse{
		ID: "id-1", Model: "m",
		Choices: []oaiChoice{{Index: 0, Message: oaiMessage{Role: "assistant", Content: "hi"}}},
		Usage:   oaiUsage{PromptTokens: 1, CompletionTokens: 1},
	})
	return b
}

func doComplete(p *OpenAIProvider) (*CompletionResponse, error) {
	return p.Complete(context.Background(), &CompletionRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
}

// A 429 must not fail the turn: it retries and succeeds once the window clears.
func TestComplete_RetriesThenSucceeds(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(okBody())
	}))
	defer srv.Close()

	resp, err := doComplete(NewOpenAIProvider("openai", srv.URL, "k", nil, fastRetry()))
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("content = %q", resp.Content)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Errorf("request count = %d, want 2 (1 x 429 + 1 x 200)", got)
	}
}

// A permanent 4xx (e.g. a bad-schema 400) must fail fast — no retries.
func TestComplete_NoRetryOn400(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad request"}`))
	}))
	defer srv.Close()

	if _, err := doComplete(NewOpenAIProvider("openai", srv.URL, "k", nil, fastRetry())); err == nil {
		t.Fatal("expected error on 400")
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("request count = %d, want 1 (no retry on 4xx)", got)
	}
}

// Persistent 429 exhausts the attempt budget and then surfaces the error.
func TestComplete_ExhaustsRetries(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	if _, err := doComplete(NewOpenAIProvider("openai", srv.URL, "k", nil, fastRetry())); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := atomic.LoadInt32(&n); got != 4 { // fastRetry MaxAttempts
		t.Errorf("request count = %d, want 4", got)
	}
}

// A tiny total-wait budget stops retrying early even with attempts remaining.
func TestComplete_TotalWaitBudget(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"unavailable"}`))
	}))
	defer srv.Close()

	// base 10ms, but total budget 5ms -> the first wait already exceeds it,
	// so it gives up after the initial attempt (no retry sleep fits).
	p := NewOpenAIProvider("openai", srv.URL, "k", nil, WithOpenAIRetryPolicy(RetryPolicy{
		MaxAttempts: 4, BaseDelay: 10 * time.Millisecond, MaxDelay: time.Second, MaxTotalWait: 5 * time.Millisecond,
	}))
	if _, err := doComplete(p); err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("request count = %d, want 1 (total-wait budget blocks any retry)", got)
	}
}

func TestRetryPolicyWithDefaults(t *testing.T) {
	got := RetryPolicy{MaxAttempts: 7}.withDefaults() // only attempts set
	d := DefaultRetryPolicy()
	if got.MaxAttempts != 7 {
		t.Errorf("MaxAttempts = %d, want 7 (explicit kept)", got.MaxAttempts)
	}
	if got.BaseDelay != d.BaseDelay || got.MaxDelay != d.MaxDelay || got.MaxTotalWait != d.MaxTotalWait {
		t.Errorf("zero fields not defaulted: %+v", got)
	}
}
