package decideprovider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// statusServer answers with the given statuses in turn (the last repeats),
// returning `{}` on a 2xx, and counts the attempts.
func statusServer(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		status := statuses[min(i, len(statuses)-1)]
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestPostJSON_RetriesTransientStatuses(t *testing.T) {
	noRetryWait(t)
	for name, tc := range map[string]struct {
		statuses []int
		attempts int32
		ok       bool
	}{
		"503 then ok":        {[]int{503, 200}, 2, true},
		"429 twice then ok":  {[]int{429, 429, 200}, 3, true},
		"429 every time":     {[]int{429}, maxAttempts, false},
		"400 is not retried": {[]int{400}, 1, false},
		"ok at once":         {[]int{200}, 1, true},
	} {
		srv, n := statusServer(t, tc.statuses...)
		var out struct{}
		err := postJSON(context.Background(), newHTTPClient(), srv.URL, "", map[string]string{}, &out)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
		if got := n.Load(); got != tc.attempts {
			t.Errorf("%s: attempts = %d, want %d", name, got, tc.attempts)
		}
	}
}

// A cancelled context stops the retries and returns the last error.
func TestPostJSON_StopsRetryingWhenCancelled(t *testing.T) {
	old := retryWait
	retryWait = func(int, string) time.Duration { return time.Hour }
	t.Cleanup(func() { retryWait = old })
	srv, n := statusServer(t, 503)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := postJSON(ctx, newHTTPClient(), srv.URL, "", map[string]string{}, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want the 503", err)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestRetryWait(t *testing.T) {
	for _, tc := range []struct {
		n          int
		retryAfter string
		want       time.Duration
	}{
		{1, "", 250 * time.Millisecond},
		{2, "", 500 * time.Millisecond},
		{1, "2", 2 * time.Second},
		{1, "60", 5 * time.Second},
		{1, "soon", 250 * time.Millisecond},
	} {
		if got := retryWait(tc.n, tc.retryAfter); got != tc.want {
			t.Errorf("retryWait(%d, %q) = %v, want %v", tc.n, tc.retryAfter, got, tc.want)
		}
	}
}

func TestNewHTTPClientLeavesTheDeadlineToTheContext(t *testing.T) {
	if got := newHTTPClient().Timeout; got != 0 {
		t.Errorf("client timeout = %s, want 0 so orchestrator.callback_timeout is the only cut", got)
	}
}
