package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/channel"
	"github.com/opentalon/opentalon/internal/orchestrator"
)

const (
	defaultHandshakeTimeout = 60 * time.Second
	defaultDialTimeout      = 5 * time.Second
	defaultStopGrace        = 5 * time.Second
)

// PluginEntry holds the config for one plugin.
type PluginEntry struct {
	Name        string
	Plugin      string // path to binary or grpc://...
	Enabled     bool
	Config      map[string]interface{}
	Env         []string      // if non-nil, used as the subprocess env verbatim; use WithEnvOverride to build it
	DialTimeout time.Duration // overrides defaultDialTimeout for the gRPC Init call (0 = use default)
	ExposeHTTP  bool          // operator opt-in: reverse-proxy /{name}/* through the webhook server
}

// WithEnvOverride starts from the current process environment (or the entry's
// existing Env slice) and overrides key to value, replacing any prior value
// for that key. Safe to call multiple times to layer additional overrides.
func (e *PluginEntry) WithEnvOverride(key, value string) {
	base := e.Env
	if base == nil {
		base = os.Environ()
	}
	prefix := key + "="
	result := make([]string, 0, len(base)+1)
	for _, v := range base {
		if !strings.HasPrefix(v, prefix) {
			result = append(result, v)
		}
	}
	result = append(result, key+"="+value)
	e.Env = result
}

type managed struct {
	entry   PluginEntry
	process *Process
	client  *Client
}

// PluginLoadedFunc is called after a plugin is successfully loaded and registered.
// It receives the plugin name so the caller can react (e.g. sync actions).
type PluginLoadedFunc func(name string)

// Manager discovers, launches, and registers tool plugins with the
// orchestrator's ToolRegistry.
type Manager struct {
	mu             sync.Mutex
	plugins        map[string]*managed
	known          map[string]PluginEntry // all configured entries, including those that failed to load
	registry       *orchestrator.ToolRegistry
	onPluginLoaded PluginLoadedFunc
}

// NewManager creates a manager that registers plugins into the given
// tool registry.
func NewManager(registry *orchestrator.ToolRegistry) *Manager {
	return &Manager{
		plugins:  make(map[string]*managed),
		known:    make(map[string]PluginEntry),
		registry: registry,
	}
}

// OnPluginLoaded sets a callback invoked after each plugin is successfully
// loaded and registered. Intended for triggering action sync when late-loaded
// plugins come online via the retry loop.
func (m *Manager) OnPluginLoaded(fn PluginLoadedFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onPluginLoaded = fn
}

// LoadAll launches all enabled plugins and registers them. Plugins that fail
// to load are recorded in the known map so they can be retried via Reload.
func (m *Manager) LoadAll(ctx context.Context, entries []PluginEntry) error {
	// Record all enabled entries upfront so Reload can retry failures.
	m.mu.Lock()
	for _, e := range entries {
		if e.Enabled {
			m.known[e.Name] = e
		}
	}
	m.mu.Unlock()

	var errs []string
	for _, e := range entries {
		if !e.Enabled {
			slog.Info("plugin disabled, skipping", "component", "plugin-manager", "plugin", e.Name)
			continue
		}
		if err := m.Load(ctx, e); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to load plugins: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Load launches a single plugin and registers it.
func (m *Manager) Load(ctx context.Context, entry PluginEntry) error {
	name, err := m.loadLocked(ctx, entry)
	if err != nil {
		return err
	}

	// Fire callback outside the lock to avoid deadlocks if the callback
	// calls back into the manager.
	m.mu.Lock()
	fn := m.onPluginLoaded
	m.mu.Unlock()
	if fn != nil {
		fn(name)
	}

	return nil
}

func (m *Manager) loadLocked(ctx context.Context, entry PluginEntry) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.plugins[entry.Name]; exists {
		return "", fmt.Errorf("plugin %q already loaded", entry.Name)
	}

	mode := detectPluginMode(entry.Plugin)

	var client *Client
	var proc *Process
	var err error

	switch mode {
	case modeBinary:
		proc, client, err = m.launchBinary(ctx, entry)
	case modeRemoteGRPC:
		client, err = m.connectRemote(entry)
	default:
		return "", fmt.Errorf("unsupported plugin mode %q for %s", mode, entry.Name)
	}
	if err != nil {
		return "", err
	}

	cap := client.Capability()
	if cap.Name == "" {
		cap.Name = entry.Name
	}

	if err := m.registry.Register(cap, client); err != nil {
		_ = client.Close()
		if proc != nil {
			_ = proc.Stop(defaultStopGrace)
		}
		return "", fmt.Errorf("register %s: %w", entry.Name, err)
	}

	// Register per-server aliases for MCP plugins so that each MCP server
	// (e.g. "jira", "appsignal") appears as its own plugin in the registry.
	// This allows WhoAmI plugin lists to use server names instead of "mcp".
	for _, name := range mcpServerNames(entry) {
		if err := m.registry.RegisterAlias(name, cap.Name); err != nil {
			slog.Warn("mcp alias registration failed", "component", "plugin-manager",
				"alias", name, "target", cap.Name, "error", err)
		} else {
			slog.Info("mcp server alias registered", "component", "plugin-manager",
				"alias", name, "target", cap.Name)
		}
	}

	mg := &managed{
		entry:   entry,
		process: proc,
		client:  client,
	}
	m.plugins[entry.Name] = mg

	if proc != nil {
		m.watchProcess(ctx, entry.Name, proc)
	}

	// Reverse-proxy /{plugin-name}/* through the shared webhook server only when the
	// operator explicitly opts in via expose_http: true. The plugin's declared HTTPAddr
	// alone is not sufficient — the operator must have the final say since the webhook
	// server is typically internet-facing.
	httpAddr := client.HTTPAddr()
	switch {
	case httpAddr != "" && entry.ExposeHTTP:
		if err := channel.RegisterReverseProxy(0, entry.Name, httpAddr); err != nil {
			slog.Warn("plugin http proxy registration failed", "component", "plugin-manager", "plugin", entry.Name, "error", err)
		} else {
			slog.Info("plugin http proxy registered", "component", "plugin-manager", "plugin", entry.Name, "target", httpAddr)
		}
	case httpAddr != "" && !entry.ExposeHTTP:
		// Plugin is running an HTTP server but expose_http: true is not set, so
		// it will never receive traffic. Set expose_http: true to proxy it, or
		// remove OPENTALON_HTTP_PORT from the plugin's environment.
		slog.Warn("plugin declares an HTTP server but expose_http is not enabled; HTTP server is unused",
			"component", "plugin-manager", "plugin", entry.Name, "addr", httpAddr)
	case httpAddr == "" && entry.ExposeHTTP:
		// Operator set expose_http: true but the plugin did not advertise an HTTP
		// address (OPENTALON_HTTP_PORT not set or plugin doesn't use it).
		// No proxy will be registered; the setting has no effect.
		slog.Warn("expose_http is enabled but plugin did not advertise an HTTP address; no proxy registered",
			"component", "plugin-manager", "plugin", entry.Name)
	}

	slog.Info("loaded plugin", "component", "plugin-manager", "plugin", entry.Name, "mode", mode, "actions", len(cap.Actions))

	return entry.Name, nil
}

// watchProcess monitors a plugin process and cleans up if it exits unexpectedly.
// It compares the stored process pointer to guard against races with Reload.
func (m *Manager) watchProcess(ctx context.Context, name string, proc *Process) {
	go func() {
		select {
		case <-proc.Exited():
			exitErr := proc.ExitErr()
			slog.Warn("plugin exited unexpectedly, will retry", "component", "plugin-manager", "plugin", name, "exit_error", exitErr)
			m.mu.Lock()
			current, ok := m.plugins[name]
			if ok && current.process == proc {
				delete(m.plugins, name)
			} else {
				ok = false
			}
			m.mu.Unlock()
			if ok {
				if current.client != nil {
					_ = current.client.Close()
				}
				m.registry.Deregister(name)
			}
		case <-ctx.Done():
		}
	}()
}

func (m *Manager) dialTimeout(entry PluginEntry) time.Duration {
	if entry.DialTimeout > 0 {
		return entry.DialTimeout
	}
	return defaultDialTimeout
}

func (m *Manager) launchBinary(ctx context.Context, entry PluginEntry) (*Process, *Client, error) {
	proc := NewProcess(entry.Plugin)
	if len(entry.Env) > 0 {
		proc.SetEnv(entry.Env)
	}
	hs, err := proc.Start(ctx, defaultHandshakeTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("start %s: %w", entry.Name, err)
	}

	client, err := DialFromHandshake(hs, m.dialTimeout(entry), configJSON(entry))
	if err != nil {
		_ = proc.Stop(defaultStopGrace)
		return nil, nil, fmt.Errorf("dial %s: %w", entry.Name, err)
	}

	return proc, client, nil
}

func (m *Manager) connectRemote(entry PluginEntry) (*Client, error) {
	addr := strings.TrimPrefix(entry.Plugin, "grpc://")
	client, err := Dial("tcp", addr, m.dialTimeout(entry), configJSON(entry))
	if err != nil {
		return nil, fmt.Errorf("connect remote %s at %s: %w", entry.Name, addr, err)
	}
	return client, nil
}

// configJSON serializes the plugin's Config map to JSON, returning "{}"
// when there is no config or serialization fails.
func configJSON(entry PluginEntry) string {
	if len(entry.Config) == 0 {
		return "{}"
	}
	b, err := json.Marshal(entry.Config)
	if err != nil {
		slog.Warn("failed to marshal config", "component", "plugin-manager", "plugin", entry.Name, "error", err)
		return "{}"
	}
	return string(b)
}

// Reload stops the named plugin and relaunches it with the same entry config.
// The plugin's capabilities are re-fetched from the subprocess on startup.
// If the plugin was never loaded (e.g. failed at startup), Reload attempts a
// fresh load using the entry recorded in the known map.
func (m *Manager) Reload(ctx context.Context, name string) error {
	m.mu.Lock()
	mg, loaded := m.plugins[name]
	entry, known := m.known[name]
	if loaded {
		entry = mg.entry
	}
	m.mu.Unlock()

	if !loaded && !known {
		return fmt.Errorf("plugin %q not loaded", name)
	}

	if loaded {
		if err := m.Unload(name); err != nil {
			slog.Warn("reload unload failed", "component", "plugin-manager", "plugin", name, "error", err)
		}
	}
	return m.Load(ctx, entry)
}

// Unload stops a plugin and removes it from the registry.
func (m *Manager) Unload(name string) error {
	m.mu.Lock()
	mg, ok := m.plugins[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("plugin %q not loaded", name)
	}
	delete(m.plugins, name)
	m.mu.Unlock()

	m.registry.Deregister(name)

	if mg.client != nil {
		_ = mg.client.Close()
	}
	if mg.process != nil {
		return mg.process.Stop(defaultStopGrace)
	}
	return nil
}

// StartRetryLoop starts a background goroutine that periodically retries
// loading any plugin in the known map that failed to load.
//
// MCP plugins handle their own sidecar reconnection internally (background
// retry for offline servers, on-demand reconnect at Execute time), so the
// host no longer kills and restarts the MCP plugin process to retry sidecars.
//
// The loop stops when ctx is cancelled.
func (m *Manager) StartRetryLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.mu.Lock()
				var pending []PluginEntry
				for name, entry := range m.known {
					if _, loaded := m.plugins[name]; !loaded {
						pending = append(pending, entry)
					}
				}
				m.mu.Unlock()

				for _, entry := range pending {
					slog.Info("retrying failed plugin load", "component", "plugin-manager", "plugin", entry.Name)
					if err := m.Load(ctx, entry); err != nil {
						slog.Warn("plugin retry failed", "component", "plugin-manager", "plugin", entry.Name, "error", err)
					}
				}
			}
		}
	}()
}

// StopAll gracefully shuts down all managed plugins.
func (m *Manager) StopAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.plugins))
	for name := range m.plugins {
		names = append(names, name)
	}
	m.mu.Unlock()

	for _, name := range names {
		if err := m.Unload(name); err != nil {
			slog.Warn("unload failed", "component", "plugin-manager", "plugin", name, "error", err)
		}
	}
}

// List returns the names of all loaded plugins.
func (m *Manager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(m.plugins))
	for name := range m.plugins {
		names = append(names, name)
	}
	return names
}

type pluginMode string

const (
	modeBinary     pluginMode = "binary"
	modeRemoteGRPC pluginMode = "grpc"
)

func detectPluginMode(path string) pluginMode {
	lower := strings.ToLower(path)
	if strings.HasPrefix(lower, "grpc://") {
		return modeRemoteGRPC
	}
	return modeBinary
}

// mcpServerNames extracts MCP server names from a plugin entry's Config["servers"].
// Returns nil when the entry is not an MCP plugin or has no configured servers.
func mcpServerNames(entry PluginEntry) []string {
	servers, _ := entry.Config["servers"].([]interface{})
	if len(servers) == 0 {
		return nil
	}
	var names []string
	for _, raw := range servers {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		// Try "server" key (inline request packages) then "name" key (static config).
		name, _ := m["server"].(string)
		if name == "" {
			name, _ = m["name"].(string)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}
