package orchestrator

import (
	"context"
	"fmt"
	"sync"
)

type PluginExecutor interface {
	Execute(ctx context.Context, call ToolCall) ToolResult
}

type ToolRegistry struct {
	mu        sync.RWMutex
	plugins   map[string]PluginCapability
	executors map[string]PluginExecutor
	// aliases maps a virtual plugin name (e.g. "jira") to the real plugin name
	// (e.g. "mcp"). Used for MCP servers so each server appears as its own plugin.
	aliases map[string]string // alias → target
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		plugins:   make(map[string]PluginCapability),
		executors: make(map[string]PluginExecutor),
		aliases:   make(map[string]string),
	}
}

func (r *ToolRegistry) Register(cap PluginCapability, exec PluginExecutor) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.plugins[cap.Name]; exists {
		return fmt.Errorf("plugin %q already registered", cap.Name)
	}
	r.plugins[cap.Name] = cap
	r.executors[cap.Name] = exec
	return nil
}

func (r *ToolRegistry) Deregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.plugins, name)
	delete(r.executors, name)
	// Also remove any aliases pointing to this plugin.
	for alias, target := range r.aliases {
		if target == name {
			delete(r.aliases, alias)
		}
	}
}

// RegisterAlias adds a virtual plugin name that resolves to an existing plugin.
// The alias shares the target's executor and capability (with the name rewritten).
// Useful for MCP servers: each server name (e.g. "jira") aliases "mcp".
func (r *ToolRegistry) RegisterAlias(alias, target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.plugins[alias]; exists {
		return fmt.Errorf("cannot alias %q: a plugin with that name is already registered", alias)
	}
	if _, exists := r.aliases[alias]; exists {
		return fmt.Errorf("alias %q already registered", alias)
	}
	if _, exists := r.plugins[target]; !exists {
		return fmt.Errorf("alias target %q not registered", target)
	}
	r.aliases[alias] = target
	return nil
}

// DeregisterAlias removes a single alias.
func (r *ToolRegistry) DeregisterAlias(alias string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.aliases, alias)
}

// AliasesFor returns the alias names pointing to the given plugin.
func (r *ToolRegistry) AliasesFor(target string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for alias, t := range r.aliases {
		if t == target {
			out = append(out, alias)
		}
	}
	return out
}

// resolveAlias returns the target plugin name if name is an alias, or name itself.
func (r *ToolRegistry) resolveAlias(name string) string {
	if target, ok := r.aliases[name]; ok {
		return target
	}
	return name
}

func (r *ToolRegistry) GetCapability(name string) (PluginCapability, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cap, ok := r.plugins[name]
	if ok {
		return cap, true
	}
	// Resolve alias: return target capability with the alias name.
	if target, isAlias := r.aliases[name]; isAlias {
		cap, ok = r.plugins[target]
		if ok {
			aliased := cap
			aliased.Name = name
			return aliased, true
		}
	}
	return PluginCapability{}, false
}

func (r *ToolRegistry) GetExecutor(name string) (PluginExecutor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	exec, ok := r.executors[name]
	if ok {
		return exec, true
	}
	// Resolve alias.
	if target, isAlias := r.aliases[name]; isAlias {
		exec, ok = r.executors[target]
		return exec, ok
	}
	return nil, false
}

// ListCapabilities returns all registered capabilities. Plugins that have
// aliases (e.g. MCP servers) are replaced by one entry per alias, each with
// the alias name but the parent's actions, so the LLM sees "jira" and
// "appsignal" instead of "mcp".
func (r *ToolRegistry) ListCapabilities() []PluginCapability {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Collect which plugins are alias targets so we can skip them.
	aliasTargets := make(map[string]struct{})
	for _, target := range r.aliases {
		aliasTargets[target] = struct{}{}
	}

	caps := make([]PluginCapability, 0, len(r.plugins)+len(r.aliases))
	for _, cap := range r.plugins {
		if _, isTarget := aliasTargets[cap.Name]; isTarget {
			// Skip parent — aliases will represent it.
			continue
		}
		caps = append(caps, cap)
	}
	// Add one entry per alias, inheriting the parent's capability.
	for alias, target := range r.aliases {
		if parent, ok := r.plugins[target]; ok {
			aliased := parent
			aliased.Name = alias
			caps = append(caps, aliased)
		}
	}
	return caps
}

func (r *ToolRegistry) HasAction(plugin, action string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	resolved := r.resolveAlias(plugin)
	cap, ok := r.plugins[resolved]
	if !ok {
		return false
	}
	for _, a := range cap.Actions {
		if a.Name == action {
			return true
		}
	}
	return false
}
