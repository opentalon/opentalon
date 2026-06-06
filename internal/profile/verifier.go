package profile

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"os"
	"sort"
	"sync"
	"time"
)

// ErrAuthFailed is returned when the WhoAmI server rejects the token or returns invalid data.
var ErrAuthFailed = errors.New("authentication failed")

// GroupPluginSaver is the subset of the group_plugins store used by the verifier.
type GroupPluginSaver interface {
	UpsertGroupPlugins(ctx context.Context, groupID string, pluginIDs []string, source string) error
}

// EntityUpserter is the subset of the entity store used by the verifier.
type EntityUpserter interface {
	Upsert(ctx context.Context, entityID, groupID string) error
}

// VerifierConfig holds configuration parsed from config.WhoAmIConfig.
type VerifierConfig struct {
	URL               string
	Method            string            // "GET" or "POST"; default "POST"
	TokenHeader       string            // default "Authorization"
	TokenPrefix       string            // default "Bearer "
	Timeout           time.Duration     // default 5s
	CacheTTL          time.Duration     // default 60s
	NegativeCacheTTL  time.Duration     // default 15s; caches explicit server rejections (4xx)
	EntityIDField     string            // default "entity_id"
	GroupField        string            // default "group"
	PluginsField      string            // default "plugins"
	ModelField        string            // optional JSON field for model override; default "model"
	ChannelTypeField  string            // optional JSON field for channel type in response; default "channel_type"
	ChannelTypeHeader string            // optional header name to send channel type to WhoAmI server (e.g. "X-Channel-Type")
	LimitField        string            // optional JSON field for token spend limit; default "limit"
	LimitTimeField    string            // optional JSON field for limit window duration (e.g. "1h"); default "limit_time"
	CredentialsField  string            // optional JSON field for per-MCP-server credential headers; default "credentials"
	NameField         string            // optional JSON field for user display name; default "name"
	BudgetTokensField string            // optional JSON field for reasoning budget tokens; default "budget_tokens"
	ExtraHeaders      map[string]string // static headers sent on every WhoAmI call; ${ENV_VAR} expanded once at construction
	// MetadataHeaders maps an inbound metadata key to an outbound HTTP header
	// name. For each entry, Verify reads metadata[key] from its caller and
	// sends the value under the named header. The values are also folded into
	// the cache key so distinct identities (e.g. two Slack bots sharing a user
	// token) never collide on a cached Profile.
	MetadataHeaders map[string]string
}

// orderedMetadataHeaders returns the MetadataHeaders entries sorted by
// metadata key so callers iterate in a stable order — required for the cache
// key to be deterministic regardless of map iteration order.
func (c *VerifierConfig) orderedMetadataHeaders() []struct{ Key, Header string } {
	if len(c.MetadataHeaders) == 0 {
		return nil
	}
	keys := make([]string, 0, len(c.MetadataHeaders))
	for k := range c.MetadataHeaders {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]struct{ Key, Header string }, 0, len(keys))
	for _, k := range keys {
		out = append(out, struct{ Key, Header string }{Key: k, Header: c.MetadataHeaders[k]})
	}
	return out
}

func (c *VerifierConfig) setDefaults() {
	if c.Method == "" {
		c.Method = "POST"
	}
	if c.TokenHeader == "" {
		// Apply defaults only when the header is unconfigured.
		// If a custom TokenHeader is set, TokenPrefix defaults to "" (no prefix).
		c.TokenHeader = "Authorization"
		if c.TokenPrefix == "" {
			c.TokenPrefix = "Bearer "
		}
	}
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Second
	}
	if c.CacheTTL == 0 {
		c.CacheTTL = 60 * time.Second
	}
	if c.NegativeCacheTTL == 0 {
		c.NegativeCacheTTL = 15 * time.Second
	}
	if c.EntityIDField == "" {
		c.EntityIDField = "entity_id"
	}
	if c.GroupField == "" {
		c.GroupField = "group"
	}
	if c.PluginsField == "" {
		c.PluginsField = "plugins"
	}
	if c.ModelField == "" {
		c.ModelField = "model"
	}
	if c.ChannelTypeField == "" {
		c.ChannelTypeField = "channel_type"
	}
	if c.LimitField == "" {
		c.LimitField = "limit"
	}
	if c.LimitTimeField == "" {
		c.LimitTimeField = "limit_time"
	}
	if c.CredentialsField == "" {
		c.CredentialsField = "credentials"
	}
	if c.NameField == "" {
		c.NameField = "name"
	}
	if c.BudgetTokensField == "" {
		c.BudgetTokensField = "budget_tokens"
	}
	// Expand env vars in ExtraHeaders once at construction time so values are
	// immutable for the verifier's lifetime and can't drift mid-run. Also guard
	// against keys that collide with TokenHeader — they would silently overwrite
	// the auth token on every request.
	if len(c.ExtraHeaders) > 0 {
		canonicalToken := textproto.CanonicalMIMEHeaderKey(c.TokenHeader)
		expanded := make(map[string]string, len(c.ExtraHeaders))
		for k, v := range c.ExtraHeaders {
			if textproto.CanonicalMIMEHeaderKey(k) == canonicalToken {
				slog.Warn("whoami extra_header collides with token_header and will be ignored", "header", k, "token_header", c.TokenHeader)
				continue
			}
			val := os.ExpandEnv(v)
			if val == "" && v != "" {
				slog.Warn("whoami extra_header resolved to empty string — check env var", "header", k, "template", v)
			}
			expanded[k] = val
		}
		c.ExtraHeaders = expanded
	}
	// Drop MetadataHeaders entries whose target header collides with TokenHeader
	// or ChannelTypeHeader, or with any ExtraHeaders entry — silently letting
	// these through would overwrite auth, channel-type, or static headers on
	// every request. Canonical header keys are compared.
	if len(c.MetadataHeaders) > 0 {
		canonicalToken := textproto.CanonicalMIMEHeaderKey(c.TokenHeader)
		var canonicalChannel string
		if c.ChannelTypeHeader != "" {
			canonicalChannel = textproto.CanonicalMIMEHeaderKey(c.ChannelTypeHeader)
		}
		reserved := map[string]string{canonicalToken: "token_header"}
		if canonicalChannel != "" {
			reserved[canonicalChannel] = "channel_type_header"
		}
		for k := range c.ExtraHeaders {
			reserved[textproto.CanonicalMIMEHeaderKey(k)] = "extra_headers"
		}
		filtered := make(map[string]string, len(c.MetadataHeaders))
		for metaKey, header := range c.MetadataHeaders {
			if header == "" {
				slog.Warn("whoami metadata_header has empty target header; ignoring", "metadata_key", metaKey)
				continue
			}
			canonical := textproto.CanonicalMIMEHeaderKey(header)
			if collides, ok := reserved[canonical]; ok {
				slog.Warn("whoami metadata_header collides with reserved header; ignoring", "metadata_key", metaKey, "header", header, "collides_with", collides)
				continue
			}
			filtered[metaKey] = header
		}
		c.MetadataHeaders = filtered
	}
}

// rejectedError wraps ErrAuthFailed for explicit server rejections (non-2xx HTTP status).
// Only this type of failure is eligible for negative caching; transient errors are not.
type rejectedError struct{ err error }

func (e rejectedError) Error() string { return e.err.Error() }
func (e rejectedError) Unwrap() error { return e.err }

type cacheEntry struct {
	profile   *Profile
	err       error // non-nil for negative cache entries
	expiresAt time.Time
}

// maxCacheEntries is the upper bound on in-memory cache size.
// When the cap is reached, the entry expiring soonest is evicted to make room.
const maxCacheEntries = 10_000

// Verifier verifies bearer tokens against a WhoAmI HTTP server and caches results.
type Verifier struct {
	cfg         VerifierConfig
	client      *http.Client
	groupStore  GroupPluginSaver // optional; nil disables auto-save
	entityStore EntityUpserter   // optional; nil disables entity tracking

	mu    sync.Mutex
	cache map[[32]byte]cacheEntry

	done chan struct{}
}

// NewVerifier creates a Verifier. groupStore and entityStore may be nil (auto-save disabled).
// Call Close when the Verifier is no longer needed to stop the background cleanup goroutine.
func NewVerifier(cfg VerifierConfig, groupStore GroupPluginSaver, entityStore EntityUpserter) *Verifier {
	cfg.setDefaults()
	v := &Verifier{
		cfg:         cfg,
		client:      &http.Client{Timeout: cfg.Timeout},
		groupStore:  groupStore,
		entityStore: entityStore,
		cache:       make(map[[32]byte]cacheEntry),
		done:        make(chan struct{}),
	}
	go v.cleanupLoop()
	return v
}

// Close stops the background cache cleanup goroutine.
func (v *Verifier) Close() {
	close(v.done)
}

// cleanupLoop periodically evicts expired cache entries off the hot path.
// It ticks at NegativeCacheTTL so entries never linger more than 2× their TTL.
func (v *Verifier) cleanupLoop() {
	ticker := time.NewTicker(v.cfg.NegativeCacheTTL)
	defer ticker.Stop()
	for {
		select {
		case <-v.done:
			return
		case <-ticker.C:
			now := time.Now()
			v.mu.Lock()
			for k, e := range v.cache {
				if now.After(e.expiresAt) {
					delete(v.cache, k)
				}
			}
			v.mu.Unlock()
		}
	}
}

// cacheKey hashes the inputs that uniquely identify a WhoAmI request: the
// bearer token, the channel type, and every metadata value that is configured
// to be forwarded as a header. Folding metadata values in is what keeps two
// bots that share a user token from colliding on a single cached Profile.
func (v *Verifier) cacheKey(token, channelType string, metadata map[string]string) [32]byte {
	h := sha256.New()
	h.Write([]byte(token))
	h.Write([]byte{0})
	h.Write([]byte(channelType))
	for _, mh := range v.cfg.orderedMetadataHeaders() {
		h.Write([]byte{0})
		h.Write([]byte(mh.Key))
		h.Write([]byte{'='})
		h.Write([]byte(metadata[mh.Key]))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Verify returns the Profile for token, using the cache when valid.
// channelType is the originating channel ID (e.g. "slack", "telegram") and is
// forwarded to the WhoAmI server if ChannelTypeHeader is configured.
// metadata is the inbound-message metadata map; values for keys configured in
// MetadataHeaders are forwarded as HTTP headers and folded into the cache key
// so distinct identities (e.g. two Slack bots) never share cached profiles.
// Returns ErrAuthFailed if the server rejects the token or returns incomplete data.
func (v *Verifier) Verify(ctx context.Context, token, channelType string, metadata map[string]string) (*Profile, error) {
	key := v.cacheKey(token, channelType, metadata)

	v.mu.Lock()
	if e, ok := v.cache[key]; ok && time.Now().Before(e.expiresAt) {
		v.mu.Unlock()
		return e.profile, e.err
	}
	v.mu.Unlock()

	p, err := v.callServer(ctx, token, channelType, metadata)
	if err != nil {
		var rejected rejectedError
		if errors.As(err, &rejected) {
			v.mu.Lock()
			v.evictLocked()
			v.cache[key] = cacheEntry{err: err, expiresAt: time.Now().Add(v.cfg.NegativeCacheTTL)}
			v.mu.Unlock()
		}
		return nil, err
	}

	// Auto-save group→plugin assignments.
	if v.groupStore != nil && len(p.Plugins) > 0 {
		if serr := v.groupStore.UpsertGroupPlugins(ctx, p.Group, p.Plugins, "whoami"); serr != nil {
			slog.Warn("auto-save group plugins failed", "group", p.Group, "error", serr)
		}
	}
	// Track entity.
	if v.entityStore != nil {
		if serr := v.entityStore.Upsert(ctx, p.EntityID, p.Group); serr != nil {
			slog.Warn("auto-save entity failed", "entity_id", p.EntityID, "group", p.Group, "error", serr)
		}
	}

	v.mu.Lock()
	v.evictLocked()
	v.cache[key] = cacheEntry{profile: p, expiresAt: time.Now().Add(v.cfg.CacheTTL)}
	v.mu.Unlock()

	return p, nil
}

// evictLocked enforces the hard cap by evicting the soonest-expiring entry when
// the cache is full. Expired-entry sweeps happen in cleanupLoop instead.
// Must be called with v.mu held.
func (v *Verifier) evictLocked() {
	if len(v.cache) < maxCacheEntries {
		return
	}
	var victim [32]byte
	var victimExp time.Time
	first := true
	for k, e := range v.cache {
		if first || e.expiresAt.Before(victimExp) {
			victim = k
			victimExp = e.expiresAt
			first = false
		}
	}
	if !first {
		delete(v.cache, victim)
	}
}

func (v *Verifier) callServer(ctx context.Context, token, channelType string, metadata map[string]string) (*Profile, error) {
	req, err := http.NewRequestWithContext(ctx, v.cfg.Method, v.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrAuthFailed, err)
	}
	req.Header.Set(v.cfg.TokenHeader, v.cfg.TokenPrefix+token)
	if v.cfg.ChannelTypeHeader != "" && channelType != "" {
		req.Header.Set(v.cfg.ChannelTypeHeader, channelType)
	}
	for k, val := range v.cfg.ExtraHeaders {
		req.Header.Set(k, val)
	}
	for _, mh := range v.cfg.orderedMetadataHeaders() {
		if val := metadata[mh.Key]; val != "" {
			req.Header.Set(mh.Header, val)
		}
	}

	// Log every outgoing WhoAmI call at INFO so operators can confirm
	// which identity headers reach the server without enabling DEBUG. Only
	// fires on cache miss, so traffic-per-line is bounded by CacheTTL.
	// Credential-bearing headers (token + every static ExtraHeaders entry)
	// are replaced with a "[REDACTED:<len>]" marker so the log proves the
	// header was sent without leaking the secret. Metadata-derived headers
	// (user id/email, channel id) and the channel-type header are emitted
	// in full — they are the ones operators usually need to diagnose
	// misrouted profiles.
	logHeaders := make(map[string]string, len(req.Header))
	redacted := map[string]struct{}{
		textproto.CanonicalMIMEHeaderKey(v.cfg.TokenHeader): {},
	}
	for k := range v.cfg.ExtraHeaders {
		redacted[textproto.CanonicalMIMEHeaderKey(k)] = struct{}{}
	}
	for k := range req.Header {
		val := req.Header.Get(k)
		if _, hide := redacted[textproto.CanonicalMIMEHeaderKey(k)]; hide {
			logHeaders[k] = fmt.Sprintf("[REDACTED:%d]", len(val))
		} else {
			logHeaders[k] = val
		}
	}
	slog.InfoContext(ctx, "whoami request", "method", req.Method, "url", req.URL.String(), "channel_type", channelType, "headers", logHeaders)

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		slog.Warn("whoami server rejected token", "method", req.Method, "url", req.URL.String(), "status", resp.StatusCode, "channel", channelType, "response_body", string(respBody))
		return nil, rejectedError{fmt.Errorf("%w: status %d", ErrAuthFailed, resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrAuthFailed, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: parse body: %v", ErrAuthFailed, err)
	}

	entityID := jsonString(raw[v.cfg.EntityIDField])
	if entityID == "" {
		return nil, fmt.Errorf("%w: missing %q field", ErrAuthFailed, v.cfg.EntityIDField)
	}
	group := jsonString(raw[v.cfg.GroupField])

	var plugins []string
	if praw, ok := raw[v.cfg.PluginsField]; ok {
		if err := json.Unmarshal(praw, &plugins); err != nil {
			slog.WarnContext(ctx, "whoami: malformed plugins field, ignoring", "field", v.cfg.PluginsField, "error", err)
		}
	}

	model := jsonString(raw[v.cfg.ModelField])
	channelTypeResp := jsonString(raw[v.cfg.ChannelTypeField])
	name := jsonString(raw[v.cfg.NameField])

	var limit int
	if lraw, ok := raw[v.cfg.LimitField]; ok {
		if err := json.Unmarshal(lraw, &limit); err != nil {
			return nil, fmt.Errorf("%w: malformed %q field: %v", ErrAuthFailed, v.cfg.LimitField, err)
		}
	}
	var limitWindow time.Duration
	if ltraw, ok := raw[v.cfg.LimitTimeField]; ok {
		if s := jsonString(ltraw); s != "" {
			var err error
			limitWindow, err = time.ParseDuration(s)
			if err != nil {
				return nil, fmt.Errorf("%w: malformed %q field %q: %v", ErrAuthFailed, v.cfg.LimitTimeField, s, err)
			}
		}
	}

	var budgetTokens int
	if braw, ok := raw[v.cfg.BudgetTokensField]; ok {
		_ = json.Unmarshal(braw, &budgetTokens) // best-effort; 0 if missing or malformed
	}

	var credentials map[string]CredentialHeader
	if craw, ok := raw[v.cfg.CredentialsField]; ok {
		if err := json.Unmarshal(craw, &credentials); err != nil {
			credentials = nil
			slog.WarnContext(ctx, "whoami: malformed credentials field, ignoring", "field", v.cfg.CredentialsField, "error", err)
		}
	}

	p := &Profile{
		EntityID:     entityID,
		Group:        group,
		Token:        token,
		Plugins:      plugins,
		Model:        model,
		ChannelType:  channelTypeResp,
		Name:         name,
		Limit:        limit,
		LimitWindow:  limitWindow,
		BudgetTokens: budgetTokens,
		Credentials:  credentials,
	}
	slog.DebugContext(ctx, "whoami profile resolved",
		"entity_id", p.EntityID,
		"group", p.Group,
		"plugins", p.Plugins,
		"model", p.Model,
		"channel_type", p.ChannelType,
		"name", p.Name,
	)
	return p, nil
}

func jsonString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
