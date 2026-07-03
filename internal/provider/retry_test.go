package provider

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxTotalWait: time.Second}
}

// The retry transport is provider-agnostic: it retries a 429 on any client and
// replays the request body byte-for-byte on every attempt (via GetBody). This
// is exercised here with a bare http.Client — no Provider involved.
func TestRetryTransport_RetriesAndReplaysBody(t *testing.T) {
	var n int32
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if atomic.AddInt32(&n, 1) < 3 { // fail the first two attempts
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := withRetry(&http.Client{}, testPolicy(), nil)
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte("PAYLOAD")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&n); got != 3 {
		t.Errorf("request count = %d, want 3 (2 x 429 + 1 x 200)", got)
	}
	for i, b := range bodies {
		if b != "PAYLOAD" {
			t.Errorf("attempt %d body = %q, want %q (body must be replayed)", i+1, b, "PAYLOAD")
		}
	}
}

// A permanent 4xx is returned on the first try — no retry, and the body is
// left intact for the caller to read.
func TestRetryTransport_PermanentStatusNotRetried(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	client := withRetry(&http.Client{}, testPolicy(), nil)
	resp, err := client.Do(mustReq(t, srv.URL))
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || string(body) != "nope" {
		t.Errorf("status/body = %d/%q, want 400/\"nope\" (returned untouched)", resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("request count = %d, want 1 (no retry on 4xx)", got)
	}
}

func mustReq(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	return req
}
