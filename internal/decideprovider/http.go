package decideprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// defaultTimeout bounds a single decision call. Decision models are meant to be
// fast (a BERT forward pass or a single-token completion); a generous ceiling
// still protects the orchestrator's callback path from a hung backend.
const defaultTimeout = 30 * time.Second

// maxAttempts bounds how often postJSON sends one request: a 429 or 5xx is
// retried, with backoff, up to this many attempts in total. Hosted decision
// APIs return both transiently, and a review sends several requests at once.
const maxAttempts = 3

// retryWait is how long postJSON waits before attempt n+1 (n starts at 1): a
// server's Retry-After when it sends one (capped at 5s), else 250ms, 500ms, ….
// A variable so tests can shorten it.
var retryWait = func(n int, retryAfter string) time.Duration {
	if secs, err := strconv.Atoi(retryAfter); err == nil && secs >= 0 {
		return min(time.Duration(secs)*time.Second, 5*time.Second)
	}
	return 250 * time.Millisecond << (n - 1)
}

// newHTTPClient returns the shared client configuration used by every backend.
func newHTTPClient() *http.Client { return &http.Client{Timeout: defaultTimeout} }

// postJSON POSTs reqBody as JSON to url and decodes the JSON response into out.
// bearer, when non-empty, is sent as an Authorization header. A 429 or 5xx is
// retried up to maxAttempts in total; any other non-2xx status, or the last
// failed attempt, is returned as an error carrying a bounded snippet of the body.
func postJSON(ctx context.Context, client *http.Client, url, bearer string, reqBody, out any) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	for attempt := 1; ; attempt++ {
		retryAfter, retry, err := postOnce(ctx, client, url, bearer, body, out)
		if !retry || attempt == maxAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(retryWait(attempt, retryAfter)):
		}
	}
}

// postOnce sends one attempt. retry reports a 429 or 5xx, with the server's
// Retry-After header.
func postOnce(ctx context.Context, client *http.Client, url, bearer string, body []byte, out any) (retryAfter string, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("call %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return resp.Header.Get("Retry-After"), retry, fmt.Errorf("%s: unexpected status %s: %s", url, resp.Status, bytes.TrimSpace(snippet))
	}
	if out == nil {
		return "", false, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", false, fmt.Errorf("decode response from %s: %w", url, err)
	}
	return "", false, nil
}

// approxTokens is a cheap, provider-agnostic token estimate (~4 chars/token)
// used to attribute input cost for backends that don't report usage themselves
// (laya's classifier server, a bare local-logits endpoint). It is intentionally
// rough: usage rows are for cost visibility and soft caps, not billing.
func approxTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}
