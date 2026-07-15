package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
)

// newGateOrch builds an orchestrator with one always-include tool ("p__always")
// and one catalog-only tool ("p__catalog"), sharing one counting executor so a
// test can assert whether the called tool actually ran. native selects the
// provider mode (a native-tools LLM vs. plain text-mode fakeLLM); store, when
// set, backs promotedToolSet so a test can mark a catalog tool as loaded.
func newGateOrch(t *testing.T, native bool, store *fakeInjectionStateStore) (*Orchestrator, *countingExecutor) {
	t.Helper()
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "tool-load gate fixtures",
		Actions: []Action{
			{Name: "always", Description: "Always-include tool.", AlwaysInclude: true},
			{Name: "catalog", Description: "Catalog-only tool."},
		},
	}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "", "")
	opts := OrchestratorOpts{}
	if store != nil {
		opts.InjectionStateStore = store
	}
	if native {
		return NewWithRules(nativeToolsLLM{&fakeLLM{}}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, opts), exec
	}
	return NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, opts), exec
}

// TestToolIsNative pins the single predicate that defines the native tools
// array: always-include core OR loaded (promoted) — nothing else.
func TestToolIsNative(t *testing.T) {
	promoted := map[string]bool{"p__loaded": true}
	cases := []struct {
		name   string
		action Action
		fqn    string
		want   bool
	}{
		{"always-include core is native", Action{Name: "x", AlwaysInclude: true}, "p__x", true},
		{"loaded (promoted) tool is native", Action{Name: "loaded"}, "p__loaded", true},
		{"catalog tool, neither always-include nor loaded", Action{Name: "y"}, "p__y", false},
	}
	for _, tc := range cases {
		if got := toolIsNative(tc.action, tc.fqn, promoted); got != tc.want {
			t.Errorf("%s: toolIsNative = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A native-mode call to a catalog tool the model has not loaded must be refused
// — the tool never runs, and the error names both the problem and the fix so
// the model can load the schema and retry.
func TestExecuteCall_RefusesUnloadedCatalogTool_Native(t *testing.T) {
	orch, exec := newGateOrch(t, true, nil)
	ctx := actor.WithSessionID(context.Background(), "s1")

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})

	if res.Error == "" {
		t.Fatal("expected a refusal for an unloaded catalog tool")
	}
	if !strings.Contains(res.Error, "not loaded") || !strings.Contains(res.Error, toolFQN(metaPluginName, metaLoadTools)) {
		t.Errorf("refusal must name the problem and point at load_tools, got %q", res.Error)
	}
	if exec.count != 0 {
		t.Errorf("unloaded catalog tool must NOT execute, ran %d time(s)", exec.count)
	}
}

// An always-include core tool is in the native array by definition, so a
// native-mode call to it runs.
func TestExecuteCall_AllowsAlwaysIncludeTool_Native(t *testing.T) {
	orch, exec := newGateOrch(t, true, nil)
	ctx := actor.WithSessionID(context.Background(), "s1")

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "always", FromLLM: true})

	if res.Error != "" {
		t.Fatalf("always-include tool must run, got error %q", res.Error)
	}
	if exec.count != 1 {
		t.Errorf("always-include tool must execute exactly once, ran %d time(s)", exec.count)
	}
}

// A catalog tool the session has loaded via _meta__load_tools (promoted in the
// injection state) is now native, so a call to it runs.
func TestExecuteCall_AllowsPromotedCatalogTool_Native(t *testing.T) {
	store := &fakeInjectionStateStore{store: map[string]state.InjectionState{
		"s1": {KnownTools: []state.KnownToolEntry{{ToolName: "p__catalog", LRURank: 1}}},
	}}
	orch, exec := newGateOrch(t, true, store)
	ctx := actor.WithSessionID(context.Background(), "s1")

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})

	if res.Error != "" {
		t.Fatalf("a loaded (promoted) catalog tool must run, got error %q", res.Error)
	}
	if exec.count != 1 {
		t.Errorf("loaded catalog tool must execute exactly once, ran %d time(s)", exec.count)
	}
}

// Text mode lists every allowed tool in full inline in the system prompt, so
// nothing is ever "unloaded" — the gate is a native-mode-only concern and must
// not fire here.
func TestExecuteCall_TextMode_NoGate(t *testing.T) {
	orch, exec := newGateOrch(t, false, nil)
	ctx := actor.WithSessionID(context.Background(), "s1")

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})

	if res.Error != "" {
		t.Fatalf("text mode must not gate any tool, got error %q", res.Error)
	}
	if exec.count != 1 {
		t.Errorf("text-mode tool must execute exactly once, ran %d time(s)", exec.count)
	}
}

// The gate targets model-originated calls only. Internal/host-constructed calls
// (preparers, guards, pipelines, formatters — FromLLM=false) are trusted and
// must never be gated, even in native mode for a catalog tool.
func TestExecuteCall_InternalCall_NotGated(t *testing.T) {
	orch, exec := newGateOrch(t, true, nil)
	ctx := actor.WithSessionID(context.Background(), "s1")

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: false})

	if res.Error != "" {
		t.Fatalf("internal (FromLLM=false) call must not be gated, got error %q", res.Error)
	}
	if exec.count != 1 {
		t.Errorf("internal call must execute exactly once, ran %d time(s)", exec.count)
	}
}

// An unloaded write tool must never reach a confirmation prompt: the model has
// to load the tool and retry, not be asked to approve a call it guessed from a
// one-line summary. The contrast case (a loaded write tool DOES prompt) proves
// the skip is the load-gate, not a mis-wired confirmation plugin.
func TestMaybeRequireConfirmation_UnloadedWriteToolNeverPrompts(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "confirmation gate fixtures",
		Actions: []Action{
			{Name: "always_write", Description: "Loaded write tool.", AlwaysInclude: true},
			{Name: "catalog_write", Description: "Unloaded write tool."},
		},
	}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(PluginCapability{
		Name: "conf", Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{}); err != nil {
		t.Fatalf("register conf: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "", "")
	orch := NewWithRules(nativeToolsLLM{&fakeLLM{}}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})
	ctx := actor.WithSessionID(context.Background(), "s1")

	// Unloaded write tool → confirmation SKIPPED (falls through to executeCall's
	// refusal); the gate returns (nil, false).
	rr, raised := orch.maybeRequireConfirmation(ctx, sessions, "s1",
		ToolCall{ID: "c1", Plugin: "p", Action: "catalog_write", FromLLM: true}, "do it")
	if raised || rr != nil {
		t.Fatalf("unloaded write tool must not raise a confirmation, got raised=%v rr=%v", raised, rr)
	}

	// Loaded (always-include) write tool → confirmation IS raised.
	rr2, raised2 := orch.maybeRequireConfirmation(ctx, sessions, "s1",
		ToolCall{ID: "c2", Plugin: "p", Action: "always_write", FromLLM: true}, "do it")
	if !raised2 || rr2 == nil {
		t.Fatalf("loaded write tool must raise a confirmation, got raised=%v rr=%v", raised2, rr2)
	}
}
