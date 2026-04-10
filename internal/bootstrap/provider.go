package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/opentalon/opentalon/internal/config"
)

// RemoteChannelConfig is the JSON representation of a channel entry returned by the bootstrap endpoint.
type RemoteChannelConfig struct {
	Enabled bool                   `json:"enabled"`
	Plugin  string                 `json:"plugin"`
	GitHub  string                 `json:"github"`
	Ref     string                 `json:"ref"`
	Config  map[string]interface{} `json:"config"`
}

// RemotePluginConfig is the JSON representation of a plugin entry returned by the bootstrap endpoint.
type RemotePluginConfig struct {
	Enabled     bool                   `json:"enabled"`
	Plugin      string                 `json:"plugin"`
	GitHub      string                 `json:"github"`
	Ref         string                 `json:"ref"`
	Config      map[string]interface{} `json:"config"`
	DialTimeout string                 `json:"dial_timeout,omitempty"`
}

// Response is the top-level payload returned by the bootstrap endpoint.
type Response struct {
	Channels     map[string]RemoteChannelConfig `json:"channels"`
	Plugins      map[string]RemotePluginConfig  `json:"plugins"`
	GroupPlugins map[string][]string            `json:"group_plugins"`
}

// Provider fetches bootstrap configuration from a remote HTTP endpoint.
type Provider struct {
	cfg    config.BootstrapConfig
	client *http.Client
}

// New returns a Provider for cfg. Returns nil when cfg.URL is empty (bootstrap disabled).
func New(cfg config.BootstrapConfig) *Provider {
	if cfg.URL == "" {
		return nil
	}
	timeout := 30 * time.Second
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		timeout = d
	}
	if cfg.TokenHeader == "" {
		cfg.TokenHeader = "Authorization"
		if cfg.TokenPrefix == "" {
			cfg.TokenPrefix = "Bearer "
		}
	}
	if cfg.Retries == 0 {
		cfg.Retries = 3
	}
	return &Provider{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

// Fetch calls the remote endpoint and returns the parsed Response.
// On transient errors it retries up to cfg.Retries times with linear backoff.
// 4xx HTTP responses are treated as terminal (no retry); 5xx responses are retried.
func (p *Provider) Fetch(ctx context.Context) (*Response, error) {
	var lastErr error
	for attempt := 0; attempt <= p.cfg.Retries; attempt++ {
		if attempt > 0 {
			slog.Info("bootstrap retry", "attempt", attempt, "url", p.cfg.URL)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		resp, err := p.doFetch(ctx)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if isTerminalError(err) {
			break
		}
	}
	return nil, lastErr
}

func (p *Provider) doFetch(ctx context.Context) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: build request: %w", err)
	}
	if p.cfg.Token != "" {
		req.Header.Set(p.cfg.TokenHeader, p.cfg.TokenPrefix+p.cfg.Token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: fetch %s: %w", p.cfg.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, &terminalError{fmt.Errorf("bootstrap: HTTP %d from %s", resp.StatusCode, p.cfg.URL)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bootstrap: HTTP %d from %s", resp.StatusCode, p.cfg.URL)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("bootstrap: read body: %w", err)
	}

	var out Response
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &terminalError{fmt.Errorf("bootstrap: parse body: %w", err)}
	}
	return &out, nil
}

// Merge merges the remote Response into the static config and returns a new Config.
// Static config entries always win on key conflicts — remote entries are purely additive.
// The static config is never mutated. GroupPlugins from the response is not merged into
// the returned Config; callers seed it to the DB separately.
//
// Only Channels and Plugins are deep-copied; all other fields (Models, Routing, Lua, etc.)
// are shallow-copied from static. Do not mutate those fields on the returned Config.
func Merge(static *config.Config, remote *Response) *config.Config {
	if remote == nil {
		return static
	}
	merged := *static

	if len(remote.Channels) > 0 {
		merged.Channels = make(map[string]config.ChannelConfig, len(static.Channels)+len(remote.Channels))
		for name, ch := range static.Channels {
			merged.Channels[name] = ch
		}
		for name, rc := range remote.Channels {
			if _, exists := merged.Channels[name]; exists {
				slog.Debug("bootstrap: channel already in static config, skipping", "channel", name)
				continue
			}
			merged.Channels[name] = config.ChannelConfig{
				Enabled: rc.Enabled,
				Plugin:  rc.Plugin,
				GitHub:  rc.GitHub,
				Ref:     rc.Ref,
				Config:  rc.Config,
			}
		}
	}

	if len(remote.Plugins) > 0 {
		merged.Plugins = make(map[string]config.PluginConfig, len(static.Plugins)+len(remote.Plugins))
		for name, p := range static.Plugins {
			merged.Plugins[name] = p
		}
		for name, rp := range remote.Plugins {
			if _, exists := merged.Plugins[name]; exists {
				slog.Debug("bootstrap: plugin already in static config, skipping", "plugin", name)
				continue
			}
			merged.Plugins[name] = config.PluginConfig{
				Enabled:     rp.Enabled,
				Plugin:      rp.Plugin,
				GitHub:      rp.GitHub,
				Ref:         rp.Ref,
				Config:      rp.Config,
				DialTimeout: rp.DialTimeout,
			}
		}
	}

	return &merged
}

type terminalError struct{ err error }

func (e *terminalError) Error() string { return e.err.Error() }
func (e *terminalError) Unwrap() error { return e.err }

func isTerminalError(err error) bool {
	var t *terminalError
	return errors.As(err, &t)
}
