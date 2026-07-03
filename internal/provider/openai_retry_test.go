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
	// Retry-After (delta-seconds) is honoured verbatim (within the cap).
	h := http.Header{}
	h.Set("Retry-After", "5")
	if got := retryDelay(&http.Response{Header: h}, 1); got != 5*time.Second {
		t.Errorf("Retry-After: got %v, want 5s", got)
	}
	// A Retry-After above the cap is clamped.
	h2 := http.Header{}
	h2.Set("Retry-After", "600")
	if got := retryDelay(&http.Response{Header: h2}, 1); got != maxRetryDelay {
		t.Errorf("Retry-After cap: got %v, want %v", got, maxRetryDelay)
	}
	// No response -> exponential backoff, attempt 1 ~= baseRetryDelay (+jitter).
	d1 := retryDelay(nil, 1)
	if d1 < baseRetryDelay || d1 >= baseRetryDelay+100*time.Millisecond {
		t.Errorf("attempt 1 backoff = %v, want ~%v", d1, baseRetryDelay)
	}
	// Large attempt -> capped at maxRetryDelay (+ small jitter).
	dBig := retryDelay(nil, 20)
	if dBig < maxRetryDelay || dBig > maxRetryDelay+2*time.Second {
		t.Errorf("capped backoff = %v, want ~%v", dBig, maxRetryDelay)
	}
}

// shrinkDelays makes the backoff negligible for fast tests.
func shrinkDelays(t *testing.T) {
	t.Helper()
	ob, om := baseRetryDelay, maxRetryDelay
	baseRetryDelay, maxRetryDelay = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { baseRetryDelay, maxRetryDelay = ob, om })
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
	shrinkDelays(t)
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

	resp, err := doComplete(NewOpenAIProvider("openai", srv.URL, "k", nil))
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
	shrinkDelays(t)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad request"}`))
	}))
	defer srv.Close()

	if _, err := doComplete(NewOpenAIProvider("openai", srv.URL, "k", nil)); err == nil {
		t.Fatal("expected error on 400")
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("request count = %d, want 1 (no retry on 4xx)", got)
	}
}

// Persistent 429 exhausts the attempt budget and then surfaces the error.
func TestComplete_ExhaustsRetries(t *testing.T) {
	shrinkDelays(t)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	if _, err := doComplete(NewOpenAIProvider("openai", srv.URL, "k", nil)); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := atomic.LoadInt32(&n); got != maxLLMAttempts {
		t.Errorf("request count = %d, want %d", got, maxLLMAttempts)
	}
}
