package orchestrator

import (
	"context"
	"log/slog"
	"regexp"

	"github.com/opentalon/opentalon/internal/provider"
)

type PluginUsageObserver interface {
	ObservePluginUsage(ctx context.Context, plugin, action, stage, model string, inputTokens, outputTokens int, cost float64)
}

type PluginCaller struct {
	Plugin string
	Action string
	Stage  string
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
	o.pluginUsageObserver.ObservePluginUsage(ctx, c.Plugin, c.Action, c.Stage, model, u.InputTokens, u.OutputTokens, u.InputCost+u.OutputCost)
}

var pluginStage = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func (o *Orchestrator) withPluginStage(ctx context.Context, call ToolCall) context.Context {
	c, ok := PluginCallerFrom(ctx)
	if !ok || call.FromLLM {
		return ctx
	}
	stage, set := call.Args["stage"]
	if set && !pluginStage.MatchString(stage) {
		if _, warned := o.pluginStageWarned.LoadOrStore(c.Plugin, true); !warned {
			slog.Warn("plugin callback stage is not ^[a-z][a-z0-9_]{0,31}$; recording stage as empty", "plugin", c.Plugin, "action", c.Action, "stage", stage)
		}
		stage = ""
	}
	c.Stage = stage
	return context.WithValue(ctx, pluginCallerKey{}, c)
}
