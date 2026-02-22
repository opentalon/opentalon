package channel

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// ChannelEntry holds the config for one channel.
type ChannelEntry struct {
	Name    string
	Plugin  string
	Enabled bool
	Config  map[string]interface{}
}

// Manager discovers, connects, and registers channel plugins with
// the channel Registry.
type Manager struct {
	connector *Connector
	registry  *Registry
}

// NewManager creates a channel manager.
func NewManager(registry *Registry) *Manager {
	return &Manager{
		connector: NewConnector(),
		registry:  registry,
	}
}

// LoadAll connects all enabled channels and registers them.
func (m *Manager) LoadAll(ctx context.Context, entries []ChannelEntry) error {
	var errs []string
	for _, e := range entries {
		if !e.Enabled {
			log.Printf("channel-manager: %s disabled, skipping", e.Name)
			continue
		}
		if err := m.Load(ctx, e); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to load channels: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Load connects a single channel and registers it.
func (m *Manager) Load(ctx context.Context, entry ChannelEntry) error {
	ch, err := m.connector.Connect(ctx, entry.Name, entry.Plugin)
	if err != nil {
		return err
	}

	if err := m.registry.Register(ch); err != nil {
		if pc, ok := ch.(*PluginClient); ok {
			pc.Stop()
		}
		m.connector.StopProcess(entry.Name)
		return fmt.Errorf("register channel %s: %w", entry.Name, err)
	}

	log.Printf("channel-manager: loaded %s via %s", entry.Name, DetectMode(entry.Plugin))
	return nil
}

// Unload deregisters a channel and stops its process.
func (m *Manager) Unload(name string) error {
	if err := m.registry.Deregister(name); err != nil {
		return err
	}
	return m.connector.StopProcess(name)
}

// StopAll shuts down all channels and their subprocesses.
func (m *Manager) StopAll() {
	m.registry.StopAll()
	m.connector.StopAll()
}
