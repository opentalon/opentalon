package orchestrator

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

type pluginUsageLLM struct{}

func (pluginUsageLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return &provider.CompletionResponse{Content: "ok", Model: "m-1", Usage: provider.Usage{InputTokens: 120, OutputTokens: 30}}, nil
}

type recordedPluginUsage struct {
	plugin, action, model string
	in, out               int
}

type pluginUsageSpy struct{ got []recordedPluginUsage }

func (s *pluginUsageSpy) ObservePluginUsage(_ context.Context, plugin, action, model string, in, out int) {
	s.got = append(s.got, recordedPluginUsage{plugin, action, model, in, out})
}

func runNoToolsSubprocess(t *testing.T, o *Orchestrator, ctx context.Context) {
	t.Helper()
	res := (&subprocessExecutor{orch: o}).Execute(ctx, ToolCall{Plugin: "_subprocess", Action: "run", Args: map[string]string{"task": "review this", "tools": noToolsSentinel}})
	if res.Error != "" {
		t.Fatalf("subprocess: %s", res.Error)
	}
}

func TestSubprocessReportsUsageForTheCallingPlugin(t *testing.T) {
	spy := &pluginUsageSpy{}
	o := setupSubprocessOrchestrator(pluginUsageLLM{})
	o.pluginUsageObserver = spy

	runNoToolsSubprocess(t, o, WithPluginCaller(context.Background(), "talooner", "evaluate_pr"))
	if len(spy.got) != 1 || spy.got[0] != (recordedPluginUsage{"talooner", "evaluate_pr", "m-1", 120, 30}) {
		t.Fatalf("got %+v", spy.got)
	}
}

func TestSubprocessWithoutPluginCallerIsNotAttributed(t *testing.T) {
	spy := &pluginUsageSpy{}
	o := setupSubprocessOrchestrator(pluginUsageLLM{})
	o.pluginUsageObserver = spy

	runNoToolsSubprocess(t, o, context.Background())
	if len(spy.got) != 0 {
		t.Fatalf("a run with no plugin caller must not be attributed: %+v", spy.got)
	}
}

func TestSubprocessWithoutObserverStillRuns(t *testing.T) {
	o := setupSubprocessOrchestrator(pluginUsageLLM{})
	runNoToolsSubprocess(t, o, WithPluginCaller(context.Background(), "talooner", "evaluate_pr"))
}

func TestPluginCallerFrom(t *testing.T) {
	if _, ok := PluginCallerFrom(context.Background()); ok {
		t.Error("empty context has no caller")
	}
	if _, ok := PluginCallerFrom(WithPluginCaller(context.Background(), "", "x")); ok {
		t.Error("a caller needs a plugin name")
	}
	c, ok := PluginCallerFrom(WithPluginCaller(context.Background(), "p", "a"))
	if !ok || c != (PluginCaller{Plugin: "p", Action: "a"}) {
		t.Errorf("got %+v %v", c, ok)
	}
}
