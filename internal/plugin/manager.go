package plugin

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

const (
	defaultHandshakeTimeout = 10 * time.Second
	defaultDialTimeout      = 5 * time.Second
	defaultStopGrace        = 5 * time.Second
)

// PluginEntry holds the config for one plugin.
type PluginEntry struct {
	Name    string
	Path    string
	Enabled bool
	Config  map[string]interface{}
}

type managed struct {
	entry   PluginEntry
	process *Process
	client  *Client
}

// Manager discovers, launches, and registers tool plugins with the
// orchestrator's ToolRegistry.
type Manager struct {
	mu       sync.Mutex
	plugins  map[string]*managed
	registry *orchestrator.ToolRegistry
}

// NewManager creates a manager that registers plugins into the given
// tool registry.
func NewManager(registry *orchestrator.ToolRegistry) *Manager {
	return &Manager{
		plugins:  make(map[string]*managed),
		registry: registry,
	}
}

// LoadAll launches all enabled plugins and registers them.
func (m *Manager) LoadAll(ctx context.Context, entries []PluginEntry) error {
	var errs []string
	for _, e := range entries {
		if !e.Enabled {
			log.Printf("plugin-manager: %s disabled, skipping", e.Name)
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
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.plugins[entry.Name]; exists {
		return fmt.Errorf("plugin %q already loaded", entry.Name)
	}

	mode := detectPluginMode(entry.Path)

	var client *Client
	var proc *Process
	var err error

	switch mode {
	case modeBinary:
		proc, client, err = m.launchBinary(ctx, entry)
	case modeRemoteGRPC:
		client, err = m.connectRemote(entry)
	default:
		return fmt.Errorf("unsupported plugin mode %q for %s", mode, entry.Name)
	}
	if err != nil {
		return err
	}

	cap := client.Capability()
	if cap.Name == "" {
		cap.Name = entry.Name
	}

	if err := m.registry.Register(cap, client); err != nil {
		client.Close()
		if proc != nil {
			_ = proc.Stop(defaultStopGrace)
		}
		return fmt.Errorf("register %s: %w", entry.Name, err)
	}

	m.plugins[entry.Name] = &managed{
		entry:   entry,
		process: proc,
		client:  client,
	}

	log.Printf("plugin-manager: loaded %s (%s, %d actions)", entry.Name, mode, len(cap.Actions))
	return nil
}

func (m *Manager) launchBinary(ctx context.Context, entry PluginEntry) (*Process, *Client, error) {
	proc := NewProcess(entry.Path)
	hs, err := proc.Start(ctx, defaultHandshakeTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("start %s: %w", entry.Name, err)
	}

	client, err := DialFromHandshake(hs, defaultDialTimeout)
	if err != nil {
		_ = proc.Stop(defaultStopGrace)
		return nil, nil, fmt.Errorf("dial %s: %w", entry.Name, err)
	}

	return proc, client, nil
}

func (m *Manager) connectRemote(entry PluginEntry) (*Client, error) {
	addr := strings.TrimPrefix(entry.Path, "grpc://")
	client, err := Dial("tcp", addr, defaultDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("connect remote %s at %s: %w", entry.Name, addr, err)
	}
	return client, nil
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
			log.Printf("plugin-manager: unload %s: %v", name, err)
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
