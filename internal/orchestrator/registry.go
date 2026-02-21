package orchestrator

import (
	"fmt"
	"sync"
)

type PluginExecutor interface {
	Execute(call ToolCall) ToolResult
}

type ToolRegistry struct {
	mu        sync.RWMutex
	plugins   map[string]PluginCapability
	executors map[string]PluginExecutor
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		plugins:   make(map[string]PluginCapability),
		executors: make(map[string]PluginExecutor),
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
}

func (r *ToolRegistry) GetCapability(name string) (PluginCapability, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cap, ok := r.plugins[name]
	return cap, ok
}

func (r *ToolRegistry) GetExecutor(name string) (PluginExecutor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	exec, ok := r.executors[name]
	return exec, ok
}

func (r *ToolRegistry) ListCapabilities() []PluginCapability {
	r.mu.RLock()
	defer r.mu.RUnlock()

	caps := make([]PluginCapability, 0, len(r.plugins))
	for _, cap := range r.plugins {
		caps = append(caps, cap)
	}
	return caps
}

func (r *ToolRegistry) HasAction(plugin, action string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cap, ok := r.plugins[plugin]
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
