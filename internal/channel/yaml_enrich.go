package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"
)

// enrichmentFailedKey is the metadata flag the enrich runtime sets when a
// step errors. handler.go reads it and returns errorFrame so the user sees
// a useful message instead of the bot processing their request with
// half-populated identity headers.
const enrichmentFailedKey = "__enrich_failed"

// enrichmentFailedStepKey reports which step failed; useful for surfacing
// the cause back to the user without leaking internal detail.
const enrichmentFailedStepKey = "__enrich_failed_step"

// ErrEnrichmentFailed is returned from runEnrich when any step errors.
// Returned errors are unwrapped from this sentinel so callers can branch
// on it via errors.Is.
var ErrEnrichmentFailed = errors.New("enrichment failed")

// defaultEnrichTTL applies when a cache block is configured without an
// explicit TTL. One hour balances freshness (users rarely change profile
// data) against quota burn on Slack-tier-2 APIs.
const defaultEnrichTTL = time.Hour

// runEnrich executes every enrichment step defined under
// inbound.enrich and returns a map of {step → {field → value}} suitable
// for binding into the template context under the "enrich" namespace. The
// supplied contexts already include event/self/config so templates inside
// enrich URL/headers/body can reference all of them.
//
// Fail-closed semantics: the first step that errors aborts the whole run
// and returns ErrEnrichmentFailed wrapped with the underlying cause. The
// caller (yaml_ws.go's processEvent) records the step name on the
// outbound InboundMessage's metadata so handler.go can convert it into a
// user-facing error.
//
// Cache contract: when EnrichCacheSpec.Key is set, the produced map is
// JSON-serialised under "enrich:<channel-instance>:<step>:<resolved-key>"
// for the configured TTL. Subsequent messages whose key resolves to the
// same value skip the live HTTP call entirely.
func (ch *YAMLChannel) runEnrich(ctx context.Context, contexts map[string]map[string]string) (map[string]map[string]string, error) {
	if len(ch.spec.Inbound.Enrich) == 0 {
		return nil, nil
	}
	// Iterate in a deterministic order so logs and cache writes are
	// reproducible even with Go's randomised map iteration.
	names := make([]string, 0, len(ch.spec.Inbound.Enrich))
	for name := range ch.spec.Inbound.Enrich {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]map[string]string, len(names))
	for _, name := range names {
		step := ch.spec.Inbound.Enrich[name]
		fields, err := ch.runEnrichStep(ctx, name, step, contexts)
		if err != nil {
			return out, fmt.Errorf("%w: step %q: %v", ErrEnrichmentFailed, name, err)
		}
		out[name] = fields
		// Make this step's results visible to subsequent steps via the
		// template context, so step B can reference {{enrich.stepA.field}}
		// in its URL/body. Mutates `contexts` in place — safe because we
		// own the snapshot built in processEvent.
		contexts["enrich."+name] = fields
	}
	return out, nil
}

// runEnrichStep executes one step: template the cache key, look it up,
// fall through to the HTTP call on miss, store, and return the extracted
// fields.
func (ch *YAMLChannel) runEnrichStep(ctx context.Context, name string, step EnrichSpec, contexts map[string]map[string]string) (map[string]string, error) {
	// Cache lookup first when configured.
	var cacheKey string
	if step.Cache.Key != "" && ch.enrichCache != nil {
		resolved := substituteTemplate(step.Cache.Key, contexts)
		if resolved != "" {
			cacheKey = enrichCacheKey(ch.instanceID, name, resolved)
			if raw, hit, err := ch.enrichCache.Get(ctx, cacheKey); err != nil {
				// Backend hiccup: log and fall through to live call.
				// This trades stronger cross-pod consistency for
				// availability — the live call still runs and the
				// subsequent Set retries the write. Documented in
				// EnrichCache.Get's comment.
				slog.WarnContext(ctx, "enrich cache get failed, falling through to live call",
					"channel", ch.instanceID, "step", name, "key", cacheKey, "error", err)
			} else if hit {
				fields, err := unmarshalEnrichFields(raw)
				if err == nil {
					slog.DebugContext(ctx, "enrich cache hit", "channel", ch.instanceID, "step", name, "key", cacheKey)
					return fields, nil
				}
				slog.WarnContext(ctx, "enrich cache value malformed, ignoring",
					"channel", ch.instanceID, "step", name, "key", cacheKey, "error", err)
			}
		}
	}

	// Live HTTP call.
	fields, err := ch.callEnrich(ctx, step, contexts)
	if err != nil {
		return nil, err
	}

	// Write back to cache on success.
	if cacheKey != "" {
		ttl := step.Cache.TTL
		if ttl <= 0 {
			ttl = defaultEnrichTTL
		}
		if setErr := ch.enrichCache.Set(ctx, cacheKey, jsonOrEmpty(fields), ttl); setErr != nil {
			slog.WarnContext(ctx, "enrich cache set failed; will refetch next call",
				"channel", ch.instanceID, "step", name, "key", cacheKey, "error", setErr)
		}
	}
	return fields, nil
}

// callEnrich performs the HTTP request and extracts fields. Every required
// extract key must produce a non-empty string; otherwise the step is
// considered failed (fail-closed). This rejects partial responses where the
// upstream returns success but omits the field we wanted.
func (ch *YAMLChannel) callEnrich(ctx context.Context, step EnrichSpec, contexts map[string]map[string]string) (map[string]string, error) {
	method := step.Method
	if method == "" {
		method = http.MethodPost
	}
	url := substituteTemplate(step.URL, contexts)
	if url == "" {
		return nil, errors.New("url template resolved to empty")
	}

	var body io.Reader
	if step.Body != "" {
		body = bytes.NewReader([]byte(substituteTemplate(step.Body, contexts)))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range step.Headers {
		req.Header.Set(k, substituteTemplate(v, contexts))
	}
	if step.Body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := ch.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Capture a short prefix of the response body to help operators
		// diagnose 4xx (auth/permission) vs 5xx (upstream incident)
		// without flooding logs on a high-traffic channel.
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("upstream status %d: %s", resp.StatusCode, string(preview))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB cap; users.info responses are KBs
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	fields := make(map[string]string, len(step.Extract))
	for outKey, path := range step.Extract {
		val := getStringField(parsed, path)
		if val == "" {
			return nil, fmt.Errorf("extract field %q missing or empty (path %q)", outKey, path)
		}
		fields[outKey] = val
	}
	return fields, nil
}

// enrichStepFromErr parses the step name out of an error returned by
// runEnrich. The error is wrapped as `enrichment failed: step "<name>":
// <cause>` so we look for the quoted name. Returns empty on parse failure
// — the caller still records the full error message in metadata, this is
// just for the structured step field.
func enrichStepFromErr(err error) string {
	s := err.Error()
	const marker = `step "`
	i := indexOf(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	j := indexOf(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// enrichCacheKey produces the namespaced redis/in-memory key under which a
// step's extracted fields are stored. Keying on channel instance keeps two
// bots' lookups isolated even when the inner key (e.g. a Slack user id)
// collides across them. Format is human-grep-friendly so operators can
// inspect or invalidate entries via redis-cli.
func enrichCacheKey(instanceID, step, resolved string) string {
	return fmt.Sprintf("enrich:%s:%s:%s", instanceID, step, resolved)
}

// unmarshalEnrichFields parses the JSON object that Set wrote back into
// the cache. Returns an error on malformed input so callers can fall
// through to a live call rather than serving stale or empty data.
func unmarshalEnrichFields(raw string) (map[string]string, error) {
	if raw == "" || raw == "{}" {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}
