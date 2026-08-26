package plugin

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

func TestDetectPluginMode(t *testing.T) {
	tests := []struct {
		path string
		want pluginMode
	}{
		{"grpc://localhost:50051", modeRemoteGRPC},
		{"GRPC://localhost:50051", modeRemoteGRPC},
		{"/usr/local/bin/myplugin", modeBinary},
		{"./myplugin", modeBinary},
		{"myplugin", modeBinary},
	}
	for _, tt := range tests {
		if got := detectPluginMode(tt.path); got != tt.want {
			t.Errorf("detectPluginMode(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// TestHTTPPortEnvOverride locks the contract cmd/opentalon relies on: a
// configured http_port is applied as the plugin's own OPENTALON_HTTP_PORT, so
// multiple expose_http plugins bind distinct listeners instead of colliding on
// the single inherited port (opentalon#331).
func TestHTTPPortEnvOverride(t *testing.T) {
	e := PluginEntry{Name: "agents"}
	e.WithEnvOverride("OPENTALON_HTTP_PORT", "8084")

	found := false
	for _, v := range e.Env {
		if v == "OPENTALON_HTTP_PORT=8084" {
			found = true
		}
	}
	if !found {
		t.Error("expected OPENTALON_HTTP_PORT=8084 in the plugin env")
	}
}

func TestWithEnvOverride(t *testing.T) {
	e := PluginEntry{Name: "test"}

	// Sets a new key when env is nil (starts from os.Environ).
	e.WithEnvOverride("MY_TEST_VAR_UNIQUE", "hello")
	found := false
	for _, v := range e.Env {
		if v == "MY_TEST_VAR_UNIQUE=hello" {
			found = true
		}
	}
	if !found {
		t.Error("expected MY_TEST_VAR_UNIQUE=hello in env")
	}

	// Overrides an existing key without duplicating it.
	e.WithEnvOverride("MY_TEST_VAR_UNIQUE", "world")
	count := 0
	for _, v := range e.Env {
		if len(v) >= len("MY_TEST_VAR_UNIQUE=") && v[:len("MY_TEST_VAR_UNIQUE=")] == "MY_TEST_VAR_UNIQUE=" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one MY_TEST_VAR_UNIQUE entry, got %d", count)
	}

	// Value is updated.
	found = false
	for _, v := range e.Env {
		if v == "MY_TEST_VAR_UNIQUE=world" {
			found = true
		}
	}
	if !found {
		t.Error("expected MY_TEST_VAR_UNIQUE=world after override")
	}
}

func TestWatchProcessCleansUpOnExit(t *testing.T) {
	registry := orchestrator.NewToolRegistry()
	m := NewManager(registry)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := newTestPluginClient(t)
	cap := client.Capability()
	if err := registry.Register(cap, client); err != nil {
		t.Fatal(err)
	}

	proc := &Process{exited: make(chan struct{})}

	m.mu.Lock()
	m.plugins["echo"] = &managed{
		entry:   PluginEntry{Name: "echo"},
		process: proc,
		client:  client,
	}
	m.mu.Unlock()

	m.watchProcess(ctx, "echo", proc)

	if _, ok := registry.GetExecutor("echo"); !ok {
		t.Fatal("echo should be in registry before exit")
	}

	close(proc.exited)

	// Deregister is the last cleanup step (runs after m.plugins delete, outside
	// the lock), so poll on it — polling m.plugins races the registry deregister.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := registry.GetExecutor("echo"); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	m.mu.Lock()
	_, inPlugins := m.plugins["echo"]
	m.mu.Unlock()
	if inPlugins {
		t.Error("plugin should be removed from m.plugins after process exit")
	}
	if _, ok := registry.GetExecutor("echo"); ok {
		t.Error("plugin should be deregistered from registry after process exit")
	}
}

// A crashed plugin must free its gateway port — otherwise the retry loop's
// reload can never rebind it (net.Listen: address already in use), and the
// stale gateway keeps accepting external calls and forwarding them to a
// client that's already closed.
func TestWatchProcessStopsGatewayOnExit(t *testing.T) {
	registry := orchestrator.NewToolRegistry()
	m := NewManager(registry)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := newTestPluginClient(t)
	cap := client.Capability()
	if err := registry.Register(cap, client); err != nil {
		t.Fatal(err)
	}

	gw, err := startGateway("echo", client, 0)
	if err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	addr := gw.addr()

	proc := &Process{exited: make(chan struct{})}

	m.mu.Lock()
	m.plugins["echo"] = &managed{
		entry:   PluginEntry{Name: "echo"},
		process: proc,
		client:  client,
		gw:      gw,
	}
	m.mu.Unlock()

	m.watchProcess(ctx, "echo", proc)
	close(proc.exited)

	// The port must be free to rebind soon after the crash — poll instead of
	// asserting once, since cleanup runs in a goroutine.
	deadline := time.Now().Add(time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lis, err := net.Listen("tcp", addr)
		if err == nil {
			_ = lis.Close()
			return
		}
		lastErr = err
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("gateway port %s never freed after plugin crash: %v", addr, lastErr)
}

func TestWatchProcessContextCancelDoesNotCleanUp(t *testing.T) {
	registry := orchestrator.NewToolRegistry()
	m := NewManager(registry)
	ctx, cancel := context.WithCancel(context.Background())

	proc := &Process{exited: make(chan struct{})}

	m.mu.Lock()
	m.plugins["echo"] = &managed{
		entry:   PluginEntry{Name: "echo"},
		process: proc,
	}
	m.mu.Unlock()

	m.watchProcess(ctx, "echo", proc)
	cancel()
	time.Sleep(50 * time.Millisecond)

	m.mu.Lock()
	_, ok := m.plugins["echo"]
	m.mu.Unlock()
	if !ok {
		t.Error("plugin should remain in m.plugins when ctx is cancelled (not an unexpected exit)")
	}
}

func TestWatchProcessIgnoresStaleProcess(t *testing.T) {
	// Simulate: process exits, but by the time the goroutine fires, the plugin
	// has already been reloaded with a different process. The old watcher
	// should not remove the new process from m.plugins.
	registry := orchestrator.NewToolRegistry()
	m := NewManager(registry)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldProc := &Process{exited: make(chan struct{})}
	newProc := &Process{exited: make(chan struct{})}

	client := newTestPluginClient(t)
	cap := client.Capability()
	if err := registry.Register(cap, client); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.plugins["echo"] = &managed{
		entry:   PluginEntry{Name: "echo"},
		process: oldProc,
		client:  client,
	}
	m.mu.Unlock()

	m.watchProcess(ctx, "echo", oldProc)

	// Simulate reload: replace plugin with new process before the watcher fires.
	m.mu.Lock()
	m.plugins["echo"] = &managed{
		entry:   PluginEntry{Name: "echo"},
		process: newProc,
		client:  client,
	}
	m.mu.Unlock()

	// Now the old process exits.
	close(oldProc.exited)
	time.Sleep(50 * time.Millisecond)

	m.mu.Lock()
	mg, ok := m.plugins["echo"]
	m.mu.Unlock()
	if !ok {
		t.Fatal("plugin should still be in m.plugins (new process)")
	}
	if mg.process != newProc {
		t.Error("expected new process to still be registered")
	}
}

func TestReady_AllLoaded(t *testing.T) {
	registry := orchestrator.NewToolRegistry()
	m := NewManager(registry)

	// No plugins configured → vacuously ready.
	if !m.Ready() {
		t.Error("expected Ready() = true when no plugins are known")
	}

	// Add known entries.
	m.mu.Lock()
	m.known["a"] = PluginEntry{Name: "a"}
	m.known["b"] = PluginEntry{Name: "b"}
	m.mu.Unlock()

	if m.Ready() {
		t.Error("expected Ready() = false when known > loaded")
	}

	// Simulate one loaded.
	m.mu.Lock()
	m.plugins["a"] = &managed{entry: PluginEntry{Name: "a"}}
	m.mu.Unlock()

	if m.Ready() {
		t.Error("expected Ready() = false when 1/2 loaded")
	}

	// Simulate both loaded.
	m.mu.Lock()
	m.plugins["b"] = &managed{entry: PluginEntry{Name: "b"}}
	m.mu.Unlock()

	if !m.Ready() {
		t.Error("expected Ready() = true when all plugins loaded")
	}
}

func TestMCPServerNames(t *testing.T) {
	tests := []struct {
		name  string
		entry PluginEntry
		want  []string
	}{
		{
			name:  "no config",
			entry: PluginEntry{Name: "mcp"},
			want:  nil,
		},
		{
			name:  "no servers key",
			entry: PluginEntry{Name: "mcp", Config: map[string]interface{}{"foo": "bar"}},
			want:  nil,
		},
		{
			name: "inline servers with server key",
			entry: PluginEntry{
				Name: "mcp",
				Config: map[string]interface{}{
					"servers": []interface{}{
						map[string]interface{}{"server": "jira", "url": "http://localhost:8001"},
						map[string]interface{}{"server": "appsignal", "url": "http://localhost:8002"},
					},
				},
			},
			want: []string{"jira", "appsignal"},
		},
		{
			name: "static servers with name key",
			entry: PluginEntry{
				Name: "mcp",
				Config: map[string]interface{}{
					"servers": []interface{}{
						map[string]interface{}{"name": "gitlab", "url": "http://localhost:9000"},
					},
				},
			},
			want: []string{"gitlab"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mcpServerNames(tt.entry)
			if len(got) != len(tt.want) {
				t.Fatalf("mcpServerNames() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("mcpServerNames()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestConfigJSON(t *testing.T) {
	tests := []struct {
		name  string
		entry PluginEntry
		want  string
	}{
		{
			name:  "nil config returns empty JSON object",
			entry: PluginEntry{Name: "test"},
			want:  "{}",
		},
		{
			name:  "empty config map returns empty JSON object",
			entry: PluginEntry{Name: "test", Config: map[string]interface{}{}},
			want:  "{}",
		},
		{
			name: "config with values returns JSON",
			entry: PluginEntry{
				Name: "mcp",
				Config: map[string]interface{}{
					"servers": []interface{}{
						map[string]interface{}{
							"name": "myserver",
							"url":  "https://example.com/mcp",
						},
					},
				},
			},
			want: `{"servers":[{"name":"myserver","url":"https://example.com/mcp"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := configJSON(tt.entry)
			if got != tt.want {
				t.Errorf("configJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}
