package orchestrator

import (
	"context"

	"github.com/opentalon/opentalon/internal/provider"
)

// PluginUsageObserver is told about every LLM call the host makes on behalf
// of a plugin — a _subprocess run or a decide call that the plugin requested
// through a callback while executing one of its actions. Unlike
// PluginCallObserver (the LLM turn that *called* a plugin tool), this is the
// plugin's own model spend, so it can be priced per plugin.
type PluginUsageObserver interface {
	ObservePluginUsage(ctx context.Context, plugin, action, model string, inputTokens, outputTokens int)
}

// PluginCaller is the plugin, and the action it was executing, on whose behalf
// a host callback runs.
type PluginCaller struct {
	Plugin string
	Action string
}

type pluginCallerKey struct{}

// WithPluginCaller marks ctx as running a callback for plugin's action. The
// plugin client sets it on every callback a plugin makes while executing.
func WithPluginCaller(ctx context.Context, plugin, action string) context.Context {
	return context.WithValue(ctx, pluginCallerKey{}, PluginCaller{Plugin: plugin, Action: action})
}

// PluginCallerFrom returns the plugin a callback runs for, if any.
func PluginCallerFrom(ctx context.Context) (PluginCaller, bool) {
	c, ok := ctx.Value(pluginCallerKey{}).(PluginCaller)
	return c, ok && c.Plugin != ""
}

// observePluginUsage reports one LLM call made for the calling plugin, if the
// call came from a plugin callback and an observer is configured.
func (o *Orchestrator) observePluginUsage(ctx context.Context, model string, u provider.Usage) {
	if o.pluginUsageObserver == nil {
		return
	}
	c, ok := PluginCallerFrom(ctx)
	if !ok {
		return
	}
	o.pluginUsageObserver.ObservePluginUsage(ctx, c.Plugin, c.Action, model, u.InputTokens, u.OutputTokens)
}
