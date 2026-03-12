package channel

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/requestpkg"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// ChannelEntry holds the config for one channel.
type ChannelEntry struct {
	Name    string
	Plugin  string // path to binary, grpc://..., or path to .yaml
	Enabled bool
	Config  map[string]interface{}
}

// Manager discovers, connects, and registers channel plugins with
// the channel Registry.
type Manager struct {
	connector    *Connector
	registry     *Registry
	toolRegistry *orchestrator.ToolRegistry
}

// NewManager creates a channel manager.
// toolRegistry may be nil if channel tool registration is not needed.
func NewManager(registry *Registry, toolRegistry *orchestrator.ToolRegistry) *Manager {
	return &Manager{
		connector:    NewConnector(),
		registry:     registry,
		toolRegistry: toolRegistry,
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
	ch, err := m.connector.Connect(ctx, entry)
	if err != nil {
		return err
	}

	// If channel supports configuration, pass the config map
	if cc, ok := ch.(pkg.ConfigurableChannel); ok && len(entry.Config) > 0 {
		if err := cc.Configure(entry.Config); err != nil {
			_ = ch.Stop()
			_ = m.connector.StopProcess(entry.Name)
			return fmt.Errorf("configure channel %s: %w", entry.Name, err)
		}
	}

	// If channel provides tools and we have a tool registry, register them
	if tp, ok := ch.(pkg.ToolProvider); ok && m.toolRegistry != nil {
		if tools := tp.Tools(); len(tools) > 0 {
			if err := m.registerChannelTools(ch.ID(), tools); err != nil {
				log.Printf("channel-manager: %s: register tools: %v", entry.Name, err)
			}
		}
	}

	if err := m.registry.Register(ch); err != nil {
		if pc, ok := ch.(*PluginClient); ok {
			_ = pc.Stop()
		}
		_ = m.connector.StopProcess(entry.Name)
		return fmt.Errorf("register channel %s: %w", entry.Name, err)
	}

	modeStr := pkg.DetectMode(entry.Plugin).String()
	log.Printf("channel-manager: loaded %s via %s", entry.Name, modeStr)
	return nil
}

// registerChannelTools converts channel tool definitions to request packages
// and registers them with the tool registry.
func (m *Manager) registerChannelTools(channelID string, tools []pkg.ToolDefinition) error {
	// Group tools by plugin name
	grouped := make(map[string][]requestpkg.Package)
	descs := make(map[string]string)
	for _, t := range tools {
		pluginName := t.Plugin
		if pluginName == "" {
			pluginName = channelID
		}
		params := make([]requestpkg.ParamDefinition, len(t.Parameters))
		for i, p := range t.Parameters {
			params[i] = requestpkg.ParamDefinition{
				Name:        p.Name,
				Description: p.Description,
				Required:    p.Required,
			}
		}
		grouped[pluginName] = append(grouped[pluginName], requestpkg.Package{
			Action:      t.Action,
			Description: t.ActionDesc,
			Method:      t.Method,
			URL:         t.URL,
			Body:        t.Body,
			Headers:     t.Headers,
			RequiredEnv: t.RequiredEnv,
			Parameters:  params,
		})
		if t.Description != "" {
			descs[pluginName] = t.Description
		}
	}

	var sets []requestpkg.Set
	for name, pkgs := range grouped {
		sets = append(sets, requestpkg.Set{
			PluginName:  name,
			Description: descs[name],
			Packages:    pkgs,
		})
	}

	return requestpkg.Register(m.toolRegistry, sets)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = globalWebhookServer.Shutdown(ctx)
}
