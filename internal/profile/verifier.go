package profile

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	URL           string
	Method        string        // "GET" or "POST"; default "POST"
	TokenHeader   string        // default "Authorization"
	TokenPrefix   string        // default "Bearer "
	Timeout       time.Duration // default 5s
	CacheTTL      time.Duration // default 60s
	EntityIDField string        // default "entity_id"
	GroupField    string        // default "group"
	PluginsField  string        // default "plugins"
}

func (c *VerifierConfig) setDefaults() {
	if c.Method == "" {
		c.Method = "POST"
	}
	if c.TokenHeader == "" {
		c.TokenHeader = "Authorization"
	}
	if c.TokenPrefix == "" {
		c.TokenPrefix = "Bearer "
	}
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Second
	}
	if c.CacheTTL == 0 {
		c.CacheTTL = 60 * time.Second
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
}

type cacheEntry struct {
	profile   *Profile
	expiresAt time.Time
}

// Verifier verifies bearer tokens against a WhoAmI HTTP server and caches results.
type Verifier struct {
	cfg         VerifierConfig
	client      *http.Client
	groupStore  GroupPluginSaver // optional; nil disables auto-save
	entityStore EntityUpserter   // optional; nil disables entity tracking

	mu    sync.Mutex
	cache map[[32]byte]cacheEntry
}

// NewVerifier creates a Verifier. groupStore and entityStore may be nil (auto-save disabled).
func NewVerifier(cfg VerifierConfig, groupStore GroupPluginSaver, entityStore EntityUpserter) *Verifier {
	cfg.setDefaults()
	return &Verifier{
		cfg:         cfg,
		client:      &http.Client{Timeout: cfg.Timeout},
		groupStore:  groupStore,
		entityStore: entityStore,
		cache:       make(map[[32]byte]cacheEntry),
	}
}

// Verify returns the Profile for token, using the cache when valid.
// Returns ErrAuthFailed if the server rejects the token or returns incomplete data.
func (v *Verifier) Verify(ctx context.Context, token string) (*Profile, error) {
	key := sha256.Sum256([]byte(token))

	v.mu.Lock()
	if e, ok := v.cache[key]; ok && time.Now().Before(e.expiresAt) {
		v.mu.Unlock()
		return e.profile, nil
	}
	v.mu.Unlock()

	p, err := v.callServer(ctx, token)
	if err != nil {
		return nil, err
	}

	// Auto-save group→plugin assignments.
	if v.groupStore != nil && len(p.Plugins) > 0 {
		if serr := v.groupStore.UpsertGroupPlugins(ctx, p.Group, p.Plugins, "whoami"); serr != nil {
			// Non-fatal: log but don't fail the request.
			_ = serr
		}
	}
	// Track entity.
	if v.entityStore != nil {
		if serr := v.entityStore.Upsert(ctx, p.EntityID, p.Group); serr != nil {
			_ = serr
		}
	}

	v.mu.Lock()
	v.cache[key] = cacheEntry{profile: p, expiresAt: time.Now().Add(v.cfg.CacheTTL)}
	v.mu.Unlock()

	return p, nil
}

func (v *Verifier) callServer(ctx context.Context, token string) (*Profile, error) {
	req, err := http.NewRequestWithContext(ctx, v.cfg.Method, v.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrAuthFailed, err)
	}
	req.Header.Set(v.cfg.TokenHeader, v.cfg.TokenPrefix+token)

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: status %d", ErrAuthFailed, resp.StatusCode)
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
		_ = json.Unmarshal(praw, &plugins)
	}

	return &Profile{
		EntityID: entityID,
		Group:    group,
		Token:    token,
		Plugins:  plugins,
	}, nil
}

func jsonString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return strings.Trim(string(raw), `"`)
	}
	return s
}
