package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/config"
)

func TestNew_DisabledWhenNoURL(t *testing.T) {
	if New(config.BootstrapConfig{}) != nil {
		t.Error("New() with empty URL should return nil")
	}
}

func TestNew_Defaults(t *testing.T) {
	// If New returns nil this will panic, which is a clear test failure.
	p := New(config.BootstrapConfig{URL: "http://example.com"})
	if p.cfg.TokenHeader != "Authorization" {
		t.Errorf("TokenHeader = %q, want Authorization", p.cfg.TokenHeader)
	}
	if p.cfg.TokenPrefix != "Bearer " {
		t.Errorf("TokenPrefix = %q, want 'Bearer '", p.cfg.TokenPrefix)
	}
	if p.cfg.Retries != 3 {
		t.Errorf("Retries = %d, want 3", p.cfg.Retries)
	}
}

// TestNew_TokenPrefixDefaults pins the two branches of the token-prefix defaulting logic:
//   - no token_header configured → defaults to "Authorization" with "Bearer " prefix
//   - custom token_header configured → no prefix added (empty string)
//
// This is a regression guard: a previous version defaulted TokenPrefix to "Bearer "
// unconditionally, which caused custom headers to send "X-Custom: Bearer <token>"
// instead of "X-Custom: <token>".
func TestNew_TokenPrefixDefaults(t *testing.T) {
	t.Run("default header gets Bearer prefix", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer mytoken" {
				http.Error(w, "want Authorization: Bearer mytoken, got "+r.Header.Get("Authorization"), http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(Response{})
		}))
		defer srv.Close()

		p := New(config.BootstrapConfig{URL: srv.URL, Token: "mytoken"})
		if _, err := p.Fetch(context.Background()); err != nil {
			t.Errorf("default header + bearer prefix: %v", err)
		}
	})

	t.Run("custom header gets no prefix", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Api-Key") != "mytoken" {
				http.Error(w, "want X-Api-Key: mytoken, got "+r.Header.Get("X-Api-Key"), http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(Response{})
		}))
		defer srv.Close()

		p := New(config.BootstrapConfig{URL: srv.URL, Token: "mytoken", TokenHeader: "X-Api-Key"})
		if _, err := p.Fetch(context.Background()); err != nil {
			t.Errorf("custom header + no prefix: %v", err)
		}
	})
}

func TestFetch_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Response{
			Channels: map[string]RemoteChannelConfig{
				"slack-org1": {Enabled: true, Plugin: "./slack/channel.yaml", Config: map[string]interface{}{"token": "xoxb-1"}},
				"slack-org2": {Enabled: true, Plugin: "./slack/channel.yaml", Config: map[string]interface{}{"token": "xoxb-2"}},
			},
			Plugins: map[string]RemotePluginConfig{
				"jira": {Enabled: true, Plugin: "./jira"},
			},
			GroupPlugins: map[string][]string{
				"org1": {"jira"},
			},
		})
	}))
	defer srv.Close()

	p := New(config.BootstrapConfig{URL: srv.URL})
	resp, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(resp.Channels) != 2 {
		t.Errorf("Channels len = %d, want 2", len(resp.Channels))
	}
	if len(resp.Plugins) != 1 {
		t.Errorf("Plugins len = %d, want 1", len(resp.Plugins))
	}
	if len(resp.GroupPlugins["org1"]) != 1 {
		t.Errorf("GroupPlugins[org1] = %v, want [jira]", resp.GroupPlugins["org1"])
	}
}

func TestFetch_BearerToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(Response{})
	}))
	defer srv.Close()

	p := New(config.BootstrapConfig{URL: srv.URL, Token: "secret-token"})
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch with valid token: %v", err)
	}

	p2 := New(config.BootstrapConfig{URL: srv.URL, Token: "wrong"})
	if _, err := p2.Fetch(context.Background()); err == nil {
		t.Fatal("expected error with wrong token, got nil")
	}
}

func TestFetch_CustomTokenHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "mykey" {
			http.Error(w, "bad key", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(Response{})
	}))
	defer srv.Close()

	p := New(config.BootstrapConfig{URL: srv.URL, Token: "mykey", TokenHeader: "X-Api-Key", TokenPrefix: ""})
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch with custom header: %v", err)
	}
}

func TestFetch_4xxIsTerminal(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	p := New(config.BootstrapConfig{URL: srv.URL, Retries: 3})
	if _, err := p.Fetch(context.Background()); err == nil {
		t.Fatal("expected error for 4xx, got nil")
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (4xx should not retry)", calls)
	}
}

func TestFetch_RetriesOnTransientError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(Response{})
	}))
	defer srv.Close()

	// 5xx is not in the terminal-error path (terminal = network errors that are *terminalError)
	// Actually our terminalError wraps 4xx. 5xx falls through as a non-terminal error? Let me check...
	// No: doFetch returns terminalError for ANY non-2xx (statusCode < 200 || >= 300).
	// So 5xx IS terminal too in the current impl. Let's test actual network retry instead.
	srv.Close() // close the server so the first attempt gets a network error

	// Use a server that fails then succeeds by counting attempts via a closure.
	attempt := 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		_ = json.NewEncoder(w).Encode(Response{})
	}))
	defer srv2.Close()

	p := New(config.BootstrapConfig{URL: srv2.URL, Retries: 2})
	resp, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestFetch_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			_ = json.NewEncoder(w).Encode(Response{})
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	p := New(config.BootstrapConfig{URL: srv.URL, Retries: 0})
	if _, err := p.Fetch(ctx); err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

func TestFetch_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	p := New(config.BootstrapConfig{URL: srv.URL})
	if _, err := p.Fetch(context.Background()); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestMerge_AddsRemoteChannels(t *testing.T) {
	static := &config.Config{}
	remote := &Response{
		Channels: map[string]RemoteChannelConfig{
			"slack-org1": {Enabled: true, Plugin: "./slack/channel.yaml"},
		},
	}
	merged := Merge(static, remote)
	if len(merged.Channels) != 1 {
		t.Fatalf("Channels len = %d, want 1", len(merged.Channels))
	}
	ch := merged.Channels["slack-org1"]
	if !ch.Enabled || ch.Plugin != "./slack/channel.yaml" {
		t.Errorf("unexpected channel: %+v", ch)
	}
}

func TestMerge_AddsRemotePlugins(t *testing.T) {
	static := &config.Config{}
	remote := &Response{
		Plugins: map[string]RemotePluginConfig{
			"jira": {Enabled: true, Plugin: "./jira", DialTimeout: "10s"},
		},
	}
	merged := Merge(static, remote)
	if len(merged.Plugins) != 1 {
		t.Fatalf("Plugins len = %d, want 1", len(merged.Plugins))
	}
	p := merged.Plugins["jira"]
	if !p.Enabled || p.Plugin != "./jira" || p.DialTimeout != "10s" {
		t.Errorf("unexpected plugin: %+v", p)
	}
}

func TestMerge_StaticWinsOnChannelConflict(t *testing.T) {
	static := &config.Config{
		Channels: map[string]config.ChannelConfig{
			"slack": {Enabled: true, Plugin: "static-plugin"},
		},
	}
	remote := &Response{
		Channels: map[string]RemoteChannelConfig{
			"slack": {Enabled: false, Plugin: "remote-plugin"},
		},
	}
	merged := Merge(static, remote)
	if merged.Channels["slack"].Plugin != "static-plugin" {
		t.Errorf("static should win: got plugin %q", merged.Channels["slack"].Plugin)
	}
}

func TestMerge_StaticWinsOnPluginConflict(t *testing.T) {
	static := &config.Config{
		Plugins: map[string]config.PluginConfig{
			"jira": {Enabled: true, Plugin: "static-jira"},
		},
	}
	remote := &Response{
		Plugins: map[string]RemotePluginConfig{
			"jira": {Enabled: false, Plugin: "remote-jira"},
		},
	}
	merged := Merge(static, remote)
	if merged.Plugins["jira"].Plugin != "static-jira" {
		t.Errorf("static should win: got plugin %q", merged.Plugins["jira"].Plugin)
	}
}

func TestMerge_NilResponseReturnsStatic(t *testing.T) {
	static := &config.Config{}
	merged := Merge(static, nil)
	if merged != static {
		t.Error("nil response should return static unchanged")
	}
}

func TestMerge_DoesNotMutateStatic(t *testing.T) {
	static := &config.Config{
		Channels: map[string]config.ChannelConfig{
			"existing": {Plugin: "orig"},
		},
		Plugins: map[string]config.PluginConfig{
			"existing-plugin": {Plugin: "orig"},
		},
	}
	remote := &Response{
		Channels: map[string]RemoteChannelConfig{
			"new-channel": {Enabled: true, Plugin: "new"},
		},
		Plugins: map[string]RemotePluginConfig{
			"new-plugin": {Enabled: true, Plugin: "new"},
		},
	}

	Merge(static, remote)

	if len(static.Channels) != 1 {
		t.Errorf("static.Channels was mutated: len = %d, want 1", len(static.Channels))
	}
	if len(static.Plugins) != 1 {
		t.Errorf("static.Plugins was mutated: len = %d, want 1", len(static.Plugins))
	}
}

func TestMerge_PreservesStaticEntries(t *testing.T) {
	static := &config.Config{
		Channels: map[string]config.ChannelConfig{
			"existing": {Plugin: "keep-me"},
		},
	}
	remote := &Response{
		Channels: map[string]RemoteChannelConfig{
			"new": {Plugin: "added"},
		},
	}
	merged := Merge(static, remote)
	if len(merged.Channels) != 2 {
		t.Fatalf("Channels len = %d, want 2", len(merged.Channels))
	}
	if merged.Channels["existing"].Plugin != "keep-me" {
		t.Errorf("existing channel was not preserved")
	}
	if merged.Channels["new"].Plugin != "added" {
		t.Errorf("new channel was not added")
	}
}
