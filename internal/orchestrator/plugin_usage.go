package orchestrator

import (
	"context"

	"github.com/opentalon/opentalon/internal/provider"
)

type PluginUsageObserver interface {
	ObservePluginUsage(ctx context.Context, plugin, action, model string, inputTokens, outputTokens int)
}

type PluginCaller struct {
	Plugin string
	Action string
}

type pluginCallerKey struct{}

func WithPluginCaller(ctx context.Context, plugin, action string) context.Context {
	return context.WithValue(ctx, pluginCallerKey{}, PluginCaller{Plugin: plugin, Action: action})
}

func PluginCallerFrom(ctx context.Context) (PluginCaller, bool) {
	c, ok := ctx.Value(pluginCallerKey{}).(PluginCaller)
	return c, ok && c.Plugin != ""
}

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
