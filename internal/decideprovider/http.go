package decideprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultTimeout bounds a single decision call. Decision models are meant to be
// fast (a BERT forward pass or a single-token completion); a generous ceiling
// still protects the orchestrator's callback path from a hung backend.
const defaultTimeout = 30 * time.Second

// newHTTPClient returns the shared client configuration used by every backend.
func newHTTPClient() *http.Client { return &http.Client{Timeout: defaultTimeout} }

// postJSON POSTs reqBody as JSON to url and decodes the JSON response into out.
// bearer, when non-empty, is sent as an Authorization header. A non-2xx status
// is returned as an error carrying a bounded snippet of the body.
func postJSON(ctx context.Context, client *http.Client, url, bearer string, reqBody, out any) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: unexpected status %s: %s", url, resp.Status, bytes.TrimSpace(snippet))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s: %w", url, err)
	}
	return nil
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
