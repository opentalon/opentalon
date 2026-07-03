package provider

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// RetryPolicy tunes transient-failure retry (429 rate limits, transient 5xx,
// transport errors) for a provider's LLM HTTP calls. Zero-valued fields fall
// back to DefaultRetryPolicy, so a partial config is safe.
type RetryPolicy struct {
	MaxAttempts  int           // total tries: 1 initial + retries
	BaseDelay    time.Duration // first backoff; doubled each subsequent retry
	MaxDelay     time.Duration // ceiling for a SINGLE wait (also caps a large Retry-After)
	MaxTotalWait time.Duration // ceiling for the SUM of all waits in one call
}

// DefaultRetryPolicy is the built-in policy: 4 tries with 1s→2s→4s backoff
// (never reaching the 20s single-wait cap at 4 tries), bounded to 30s total.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:  4,
		BaseDelay:    1 * time.Second,
		MaxDelay:     20 * time.Second,
		MaxTotalWait: 30 * time.Second,
	}
}

// withDefaults fills any zero/negative field from DefaultRetryPolicy.
func (r RetryPolicy) withDefaults() RetryPolicy {
	d := DefaultRetryPolicy()
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = d.MaxAttempts
	}
	if r.BaseDelay <= 0 {
		r.BaseDelay = d.BaseDelay
	}
	if r.MaxDelay <= 0 {
		r.MaxDelay = d.MaxDelay
	}
	if r.MaxTotalWait <= 0 {
		r.MaxTotalWait = d.MaxTotalWait
	}
	return r
}

// isRetryableStatus reports whether an HTTP status is worth retrying: 429
// (rate limit — the Mistral Scale-tier beta case) and transient 5xx. Other
// 4xx (bad request, auth, unsupported param) are permanent and must fail fast.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// retryDelay is the wait before the next attempt: honour a Retry-After header
// (delta-seconds) when the server sends one, else exponential backoff (base *
// 2^(attempt-1)) with a small deterministic jitter — both capped at maxDelay.
func retryDelay(resp *http.Response, attempt int, base, maxDelay time.Duration) time.Duration {
	if resp != nil {
		if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
				return capDelay(time.Duration(secs)*time.Second, maxDelay)
			}
		}
	}
	backoff := base << (attempt - 1)
	return capDelay(backoff, maxDelay) + time.Duration(attempt*50)*time.Millisecond
}

func capDelay(d, maxDelay time.Duration) time.Duration {
	if d > maxDelay || d <= 0 { // d<=0 guards the shift overflowing to negative at huge attempts
		return maxDelay
	}
	return d
}

// drainSnippet reads a short prefix of an error body (for the retry log) and
// closes it, so the connection can be reused for the next attempt.
func drainSnippet(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	_ = resp.Body.Close()
	return strings.TrimSpace(string(b))
}

// retryTransport is an http.RoundTripper that retries transient failures
// (429 / 5xx / transport errors) per a RetryPolicy. It sits BELOW the
// http.Client, so retry is transparent to every caller — no provider needs
// to know about it. This is the single, provider-agnostic home for retry:
// any Provider (OpenAI-compatible, Anthropic, or a future one) gets it for
// free just by using a client wrapped via withRetry.
//
// Retry is safe for both non-streaming and streaming requests: a 2xx is
// returned immediately with its body unread (so an SSE stream flows normally),
// and only a retryable *status* or a transport error triggers a re-attempt,
// before any streaming has begun.
type retryTransport struct {
	base http.RoundTripper
	pol  RetryPolicy
	sink emit.Sink // optional; nil disables retry-event emission
}

func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	pol := rt.pol
	var lastErr error
	var waited time.Duration

	for attempt := 1; attempt <= pol.MaxAttempts; attempt++ {
		// The body reader is single-use; rebuild it from GetBody on every
		// re-attempt (http.NewRequest populates GetBody for in-memory bodies,
		// which is what every provider sends).
		attemptReq := req
		if attempt > 1 {
			if req.GetBody == nil {
				return nil, fmt.Errorf("retry: request body is not replayable")
			}
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("retry: rebuild body: %w", err)
			}
			attemptReq = req.Clone(ctx)
			attemptReq.Body = body
		}

		resp, err := rt.base.RoundTrip(attemptReq)
		if err == nil && !isRetryableStatus(resp.StatusCode) {
			return resp, nil // success or a permanent error — hand back untouched
		}

		delay := retryDelay(resp, attempt, pol.BaseDelay, pol.MaxDelay)
		if attempt == pol.MaxAttempts || ctx.Err() != nil ||
			(pol.MaxTotalWait > 0 && waited+delay > pol.MaxTotalWait) {
			switch {
			case err == nil:
				return resp, nil // give up but return the response for a rich error
			case ctx.Err() != nil:
				return nil, ctx.Err()
			default:
				return nil, err
			}
		}

		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, drainSnippet(resp))
		}
		waited += delay
		if rt.sink != nil {
			emit.EmitRetry(ctx, rt.sink, emit.RetryArgs{Phase: phaseLLMHTTP, Attempt: attempt, LastError: lastErr.Error()})
		}
		slog.WarnContext(ctx, "llm call retry",
			"attempt", attempt, "delay", delay.String(), "err", lastErr.Error())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("llm call failed after %d attempts", pol.MaxAttempts)
	}
	return nil, lastErr
}

const phaseLLMHTTP = "llm.http"

// withRetry wraps a client's transport with retry so every request it makes
// is retried per pol. The client is mutated in place and returned for
// convenience. Passing the provider's event sink surfaces each re-attempt as
// a retry session-event.
func withRetry(c *http.Client, pol RetryPolicy, sink emit.Sink) *http.Client {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = &retryTransport{base: base, pol: pol.withDefaults(), sink: sink}
	return c
}
