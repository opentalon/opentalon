package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/decideprovider"
	"github.com/opentalon/opentalon/internal/provider"
)

type pluginUsageLLM struct{}

func (pluginUsageLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return &provider.CompletionResponse{Content: "ok", Model: "m-1",
		Usage: provider.Usage{InputTokens: 120, OutputTokens: 30, InputCost: 0.25, OutputCost: 0.5}}, nil
}

type recordedPluginUsage struct {
	plugin, action, stage, model string
	in, out                      int
	cost                         float64
}

type pluginUsageSpy struct {
	mu  sync.Mutex
	got []recordedPluginUsage
}

func (s *pluginUsageSpy) ObservePluginUsage(_ context.Context, plugin, action, stage, model string, in, out int, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, recordedPluginUsage{plugin, action, stage, model, in, out, cost})
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
	if len(spy.got) != 1 || spy.got[0] != (recordedPluginUsage{"talooner", "evaluate_pr", "", "m-1", 120, 30, 0.75}) {
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

func TestDecideReportsUsageForTheCallingPlugin(t *testing.T) {
	d := &fakeDecider{name: "jev", dec: &decideprovider.Decision{Chosen: "match", Confidence: 1,
		Usage: decideprovider.Usage{InputTokens: 40, OutputTokens: 2}}}
	spy := &pluginUsageSpy{}
	o := decideTestOrch(t, d, nil)
	o.pluginUsageObserver = spy

	ctx := WithPluginCaller(context.Background(), "talooner", "evaluate_pr")
	if _, _, err := o.RunActionResult(ctx, "jev", "decide", map[string]string{"state": "s", "choices": `["match"]`}); err != nil {
		t.Fatalf("RunActionResult: %v", err)
	}
	want := recordedPluginUsage{"talooner", "evaluate_pr", "", "decide/jev", 40, 2, 0}
	if len(spy.got) != 1 || spy.got[0] != want {
		t.Fatalf("got %+v, want %+v", spy.got, want)
	}

	if _, _, err := o.RunActionResult(context.Background(), "jev", "decide", map[string]string{"state": "s", "choices": `["match"]`}); err != nil {
		t.Fatalf("RunActionResult: %v", err)
	}
	if len(spy.got) != 1 {
		t.Fatalf("a decide call with no plugin caller must not be attributed: %+v", spy.got)
	}
}

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestPluginCallbacksCarryStage(t *testing.T) {
	ctx := WithPluginCaller(context.Background(), "talooner", "evaluate_pr")

	t.Run("run", func(t *testing.T) {
		spy := &pluginUsageSpy{}
		o := setupSubprocessOrchestrator(pluginUsageLLM{})
		o.pluginUsageObserver = spy
		if _, _, err := o.RunActionResult(ctx, "_subprocess", "run", map[string]string{"task": "t", "tools": noToolsSentinel, "stage": "verify"}); err != nil {
			t.Fatal(err)
		}
		if len(spy.got) != 1 || spy.got[0] != (recordedPluginUsage{"talooner", "evaluate_pr", "verify", "m-1", 120, 30, 0.75}) {
			t.Fatalf("got %+v", spy.got)
		}
	})

	t.Run("parallel", func(t *testing.T) {
		spy := &pluginUsageSpy{}
		o := setupSubprocessOrchestrator(pluginUsageLLM{})
		o.pluginUsageObserver = spy
		tasks := `[{"task":"a","tools":"none"},{"task":"b","tools":"none"},{"task":"c","tools":"none"}]`
		if _, _, err := o.RunActionResult(ctx, "_subprocess", "parallel", map[string]string{"tasks": tasks, "stage": "verify"}); err != nil {
			t.Fatal(err)
		}
		if len(spy.got) != 3 {
			t.Fatalf("want one observation per child, got %+v", spy.got)
		}
		for _, g := range spy.got {
			if g.stage != "verify" {
				t.Errorf("child observation %+v lost the stage", g)
			}
		}
	})

	t.Run("decide", func(t *testing.T) {
		d := &fakeDecider{name: "jev", dec: &decideprovider.Decision{Chosen: "match", Confidence: 1, Usage: decideprovider.Usage{InputTokens: 40, OutputTokens: 2}}}
		spy := &pluginUsageSpy{}
		o := decideTestOrch(t, d, nil)
		o.pluginUsageObserver = spy
		if _, _, err := o.RunActionResult(ctx, "jev", "decide", map[string]string{"state": "s", "choices": `["match"]`, "stage": "verify"}); err != nil {
			t.Fatal(err)
		}
		questions := `{"q":{"instructions":"i","choices":["match","mismatch"]}}`
		if _, _, err := o.RunActionResult(ctx, "jev", "decide", map[string]string{"state": "s", "questions": questions, "stage": "decide"}); err != nil {
			t.Fatal(err)
		}
		if len(spy.got) != 2 || spy.got[0].stage != "verify" || spy.got[1].stage != "decide" {
			t.Fatalf("got %+v", spy.got)
		}
	})
}

func TestPluginStageMissingIsEmptyWithoutWarning(t *testing.T) {
	logs := captureWarnings(t)
	spy := &pluginUsageSpy{}
	o := setupSubprocessOrchestrator(pluginUsageLLM{})
	o.pluginUsageObserver = spy
	runNoToolsSubprocess(t, o, WithPluginCaller(context.Background(), "talooner", "evaluate_pr"))
	if len(spy.got) != 1 || spy.got[0].stage != "" {
		t.Fatalf("got %+v", spy.got)
	}
	if logs.Len() != 0 {
		t.Fatalf("a missing stage must not warn: %s", logs)
	}
}

func TestPluginStageInvalidIsEmptyAndWarnsOncePerPlugin(t *testing.T) {
	logs := captureWarnings(t)
	spy := &pluginUsageSpy{}
	o := setupSubprocessOrchestrator(pluginUsageLLM{})
	o.pluginUsageObserver = spy
	bad := []string{"Verify", "a-b", "", strings.Repeat("a", 33), "x\n", "1x", "_x"}
	for _, plugin := range []string{"talooner", "other"} {
		ctx := WithPluginCaller(context.Background(), plugin, "evaluate_pr")
		for _, stage := range bad {
			if _, _, err := o.RunActionResult(ctx, "_subprocess", "run", map[string]string{"task": "t", "tools": noToolsSentinel, "stage": stage}); err != nil {
				t.Fatalf("stage %q: an invalid stage must not fail the callback: %v", stage, err)
			}
		}
	}
	if len(spy.got) != 2*len(bad) {
		t.Fatalf("got %d observations, want %d", len(spy.got), 2*len(bad))
	}
	for _, g := range spy.got {
		if g.stage != "" {
			t.Errorf("invalid stage leaked into %+v", g)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Fatalf("want one warning per plugin, got %d: %s", n, logs)
	}
	if !strings.Contains(logs.String(), "plugin=talooner") || !strings.Contains(logs.String(), "plugin=other") {
		t.Fatalf("warnings must name the plugin: %s", logs)
	}
}

func TestPluginStageBoundaryLengths(t *testing.T) {
	spy := &pluginUsageSpy{}
	o := setupSubprocessOrchestrator(pluginUsageLLM{})
	o.pluginUsageObserver = spy
	ctx := WithPluginCaller(context.Background(), "talooner", "evaluate_pr")
	for _, stage := range []string{"a", "re_verify2", "a" + strings.Repeat("b", 31)} {
		if _, _, err := o.RunActionResult(ctx, "_subprocess", "run", map[string]string{"task": "t", "tools": noToolsSentinel, "stage": stage}); err != nil {
			t.Fatal(err)
		}
		if got := spy.got[len(spy.got)-1].stage; got != stage {
			t.Errorf("stage %q recorded as %q", stage, got)
		}
	}
}

func TestLLMSubprocessWithStageIsNotAttributed(t *testing.T) {
	spy := &pluginUsageSpy{}
	o := setupSubprocessOrchestrator(pluginUsageLLM{})
	o.pluginUsageObserver = spy
	res := (&subprocessExecutor{orch: o}).Execute(context.Background(), ToolCall{Plugin: "_subprocess", Action: "run", FromLLM: true,
		Args: map[string]string{"task": "t", "tools": noToolsSentinel, "stage": "verify"}})
	if res.Error != "" {
		t.Fatalf("subprocess: %s", res.Error)
	}
	if len(spy.got) != 0 {
		t.Fatalf("an LLM-sourced call must produce no plugin series: %+v", spy.got)
	}
}
