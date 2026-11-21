package orchestrator

import (
	"context"
	"reflect"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

func toolMarks(defs []provider.ToolDefinition) []string {
	out := []string{}
	for _, d := range defs {
		if d.OnDemand {
			out = append(out, d.Name+"*")
			continue
		}
		out = append(out, d.Name)
	}
	return out
}

func TestBuildToolDefinitions_CallableAndLoadable(t *testing.T) {
	registry := NewToolRegistry()
	for _, c := range []PluginCapability{
		{Name: "p", Description: "allowed plugin", Actions: []Action{
			{Name: "zeta", Description: "Registered first."},
			{Name: "always", Description: "Fixed tool.", AlwaysInclude: true},
			{Name: "alpha", Description: "Registered after zeta."},
			{Name: "user", Description: "User-only action.", UserOnly: true},
			{Name: "prepare", Description: "Preparer action."},
		}},
		{Name: "q", Description: "plugin the profile does not allow", Actions: []Action{
			{Name: "hidden", Description: "Not allowed."},
		}},
	} {
		if err := registry.Register(c, &fixedResultExecutor{content: "ok"}); err != nil {
			t.Fatalf("register %s: %v", c.Name, err)
		}
	}
	store := &fakeInjectionStateStore{}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	orch := NewWithRules(nativeToolsLLM{&fakeLLM{}}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions,
		OrchestratorOpts{
			ContentPreparers:    []ContentPreparerEntry{{Plugin: "p", Action: "prepare"}},
			InjectionStateStore: store,
		})
	ctx := withAllowedPlugins(actor.WithSessionID(context.Background(), "s1"),
		cachedAllowedPlugins{m: map[string]bool{"p": true}, strict: true})

	for _, tc := range []struct {
		name               string
		loaded             []state.KnownToolEntry
		callable, loadable []string
	}{
		{"nothing loaded", nil,
			[]string{"_meta__load_tools", "p__always"}, []string{"p__alpha*", "p__zeta*"}},
		{"one tool loaded", []state.KnownToolEntry{{ToolName: "p__zeta"}},
			[]string{"_meta__load_tools", "p__zeta*", "p__always"}, []string{"p__alpha*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.store = map[string]state.InjectionState{"s1": {KnownTools: tc.loaded}}
			callable, loadable := orch.buildToolDefinitions(ctx)
			if got := toolMarks(callable); !reflect.DeepEqual(got, tc.callable) {
				t.Errorf("callable = %v, want %v", got, tc.callable)
			}
			if got := toolMarks(loadable); !reflect.DeepEqual(got, tc.loadable) {
				t.Errorf("loadable = %v, want %v", got, tc.loadable)
			}
			againCallable, againLoadable := orch.buildToolDefinitions(ctx)
			if !reflect.DeepEqual(againCallable, callable) || !reflect.DeepEqual(againLoadable, loadable) {
				t.Errorf("a second build differs: %+v %+v, first %+v %+v", againCallable, againLoadable, callable, loadable)
			}
		})
	}
}

func TestRun_LoadableToolsStayOutOfTheToolsArray(t *testing.T) {
	orch, llm, exec := loadOnCallOrch(t, &fakeInjectionStateStore{}, callOf("r1", "p__catalog", map[string]string{"q": "x"}))
	res := runTurn(t, orch)
	if len(llm.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (the call, the answer)", len(llm.requests))
	}
	if len(res.Results) != 1 || !res.Results[0].NotRun || exec.count != 0 {
		t.Fatalf("the call to the loadable tool must be answered without running, got %+v (executor ran %d)", res.Results, exec.count)
	}
	for i, want := range []struct{ tools, loadable []string }{
		{[]string{"_meta__load_tools", "p__always"}, []string{"p__catalog*", "p__list*", "p__lookup*", "p__other*"}},
		{[]string{"_meta__load_tools", "p__always", "p__catalog*", "p__lookup*", "p__list*"}, []string{"p__other*"}},
	} {
		req := llm.requests[i]
		if got := toolMarks(req.Tools); !reflect.DeepEqual(got, want.tools) {
			t.Errorf("request %d tools = %v, want %v", i+1, got, want.tools)
		}
		if got := toolMarks(req.LoadableTools); !reflect.DeepEqual(got, want.loadable) {
			t.Errorf("request %d loadable tools = %v, want %v", i+1, got, want.loadable)
		}
	}
	with, without := *llm.requests[0], *llm.requests[0]
	without.LoadableTools = nil
	if a, b := fitRequestToWindow(context.Background(), &with, 0, 0, 1), fitRequestToWindow(context.Background(), &without, 0, 0, 1); a != b {
		t.Errorf("estimate with loadable tools = %d, without = %d, want them equal", a, b)
	}
}
