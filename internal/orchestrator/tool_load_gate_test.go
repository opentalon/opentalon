package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// gateOrch builds an orchestrator with one always-include tool ("p__always")
// and one catalog-only tool ("p__catalog") plus a "p__foo-bar" fixture for the
// name-normalization case, sharing one counting executor so a test can assert
// whether the called tool actually ran.
func gateOrch(t *testing.T) (*Orchestrator, *countingExecutor) {
	t.Helper()
	return gateOrchWithStore(t, nil)
}

// gateOrchWithStore is gateOrch with the given injection state store wired
// (none when store is nil).
func gateOrchWithStore(t *testing.T, store *fakeInjectionStateStore) (*Orchestrator, *countingExecutor) {
	t.Helper()
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "tool-load gate fixtures",
		Actions: []Action{
			{Name: "always", Description: "Always-include tool.", AlwaysInclude: true},
			{Name: "catalog", Description: "Catalog-only tool."},
			{Name: "foo-bar", Description: "Hyphenated catalog tool."},
		},
	}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	opts := OrchestratorOpts{}
	if store != nil {
		opts.InjectionStateStore = store
	}
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, opts)
	return orch, exec
}

// storedTool returns the session's stored KnownTools entry for fqn.
func storedTool(store *fakeInjectionStateStore, fqn string) (state.KnownToolEntry, bool) {
	for _, kt := range store.store["s1"].KnownTools {
		if kt.ToolName == fqn {
			return kt, true
		}
	}
	return state.KnownToolEntry{}, false
}

// sentCtx builds a request context that carries the given FQNs as the native
// tool set sent to the model this request — the set the gate checks against.
func sentCtx(fqns ...string) context.Context {
	set := make(map[string]struct{}, len(fqns))
	for _, f := range fqns {
		set[f] = struct{}{}
	}
	return withSentNativeTools(actor.WithSessionID(context.Background(), "s1"), set)
}

// TestToolIsNative pins the single predicate the builder splits native-vs-catalog
// by: always-include core OR loaded (promoted) — nothing else.
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

// A call to a tool that was NOT in the sent native array never runs. The tool is
// loaded the way _meta__load_tools loads it — a sticky promotion in the session
// store — and recorded in the turn's in-memory loads, and the answer tells the
// model to call it again with the parameters from its description. The result
// is marked NotRun.
func TestExecuteCall_LoadsToolNotInSentArrayWithoutRunningIt(t *testing.T) {
	store := &fakeInjectionStateStore{}
	orch, exec := gateOrchWithStore(t, store)
	loads := newTurnToolLoads()
	ctx := withTurnToolLoads(sentCtx("p__always"), loads) // catalog was NOT sent

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})

	if exec.count != 0 {
		t.Errorf("a tool not in the sent array must NOT execute, ran %d time(s)", exec.count)
	}
	for _, want := range []string{"NOT run", "loaded now", "call p__catalog again directly with the parameters"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("answer must say %q, got %q", want, res.Error)
		}
	}
	if kt, ok := storedTool(store, "p__catalog"); !ok || kt.Demoted {
		t.Errorf("the tool must be promoted in the session store, got %+v (found=%v)", kt, ok)
	}
	if !loads.snapshot()["p__catalog"] || !res.NotRun {
		t.Errorf("the turn must record the load and the result must be marked not run, got loads=%v NotRun=%v",
			loads.snapshot(), res.NotRun)
	}
}

// A tool that WAS in the sent array (an always-include core tool, or a catalog
// tool the model has loaded) runs.
func TestExecuteCall_AllowsToolInSentArray(t *testing.T) {
	orch, exec := gateOrch(t)
	ctx := sentCtx("p__always", "p__catalog") // both sent (catalog was loaded)

	if res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "always", FromLLM: true}); res.Error != "" {
		t.Fatalf("always-include tool in the sent array must run, got error %q", res.Error)
	}
	if res := orch.executeCall(ctx, ToolCall{ID: "c2", Plugin: "p", Action: "catalog", FromLLM: true}); res.Error != "" {
		t.Fatalf("a loaded catalog tool in the sent array must run, got error %q", res.Error)
	}
	if exec.count != 2 {
		t.Errorf("both sent tools must execute, ran %d time(s)", exec.count)
	}
}

// The gate checks ONLY the set actually sent this request — never a fresh read
// of session state. A tool promoted in the injection store but NOT in the sent
// array (it was loaded by an earlier call in the SAME response, or evicted
// after the array was built) does not run: the model never saw its schema, so
// it is answered "loaded now, call it again". This is what makes the invariant
// hold in the same-turn load+call case and keeps a mid-turn change in the store
// from disagreeing with the array the model saw.
func TestExecuteCall_IgnoresStorePromotionNotInSentArray(t *testing.T) {
	store := &fakeInjectionStateStore{store: map[string]state.InjectionState{
		"s1": {KnownTools: []state.KnownToolEntry{{ToolName: "p__catalog", LRURank: 1}}},
	}}
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "p", Actions: []Action{{Name: "catalog", Description: "Catalog-only."}},
	}, exec)
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions,
		OrchestratorOpts{InjectionStateStore: store})

	// p__catalog IS promoted in the store, but NOT in the sent array this round.
	ctx := sentCtx() // empty sent set

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})
	if !strings.Contains(res.Error, "call p__catalog again") {
		t.Errorf("a store-promoted tool absent from the sent array must be answered with a call-again, got %q", res.Error)
	}
	if exec.count != 0 {
		t.Errorf("tool absent from the sent array must NOT execute, ran %d time(s)", exec.count)
	}
}

// A call to a tool _meta__load_tools could not load for this session is refused
// as before and loads nothing: a plugin the profile hides and a user-only action
// by their own gates, a preparer action as not available, an unknown name as
// not found. The refusals stay errors, with the status "error".
func TestExecuteCall_UnloadableToolIsRefusedAndNotLoaded(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	for _, cap := range []PluginCapability{
		{Name: "p", Actions: []Action{
			{Name: "admin", Description: "User-only action.", UserOnly: true},
			{Name: "prepare", Description: "Preparer action."},
		}},
		{Name: "secret", Actions: []Action{{Name: "peek", Description: "Hidden by the profile."}}},
	} {
		if err := registry.Register(cap, exec); err != nil {
			t.Fatalf("register %s: %v", cap.Name, err)
		}
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	store := &fakeInjectionStateStore{}
	sink := &recordingEventSink{}
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "p", Action: "prepare"}},
		InjectionStateStore: store,
		EventSink:           sink,
	})
	prof := &profile.Profile{EntityID: "u1", Plugins: []string{"p"}}

	for _, tc := range []struct{ name, plugin, action, want string }{
		{"plugin hidden by the profile", "secret", "peek", `plugin "secret" is not available`},
		{"user-only action", "p", "admin", "can only be invoked by the user"},
		{"preparer action", "p", "prepare", `tool "p__prepare" is not available`},
		{"unknown action", "p", "nope", "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loads := newTurnToolLoads()
			ctx := withTurnToolLoads(withSentNativeTools(
				actor.WithSessionID(profile.WithProfile(context.Background(), prof), "s1"), map[string]struct{}{}), loads)

			res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: tc.plugin, Action: tc.action, FromLLM: true})
			if !strings.Contains(res.Error, tc.want) || res.NotRun {
				t.Errorf("want a refusal containing %q, not marked not run, got %q (NotRun=%v)", tc.want, res.Error, res.NotRun)
			}
			if got := loads.snapshot(); len(got) != 0 {
				t.Errorf("nothing may be loaded, got %v", got)
			}
		})
	}
	if exec.count != 0 || store.updateCalls != 0 {
		t.Errorf("no refused call may run or be promoted, ran %d, store writes %d", exec.count, store.updateCalls)
	}
	// The unknown name is reported as not found, without a tool_call_result.
	results := findToolCallResultPayloads(t, sink.snapshot())
	if len(results) != 3 {
		t.Fatalf("want a tool_call_result for each of the 3 refusals, got %+v", results)
	}
	for _, r := range results {
		if r.Status != "error" {
			t.Errorf("a refusal must keep the status error, got %q (%s)", r.Status, r.ResponseExcerpt)
		}
	}
}

// A demoted tool keeps the answer it had before loading on call existed (load
// it first) and stays demoted: promoting it on call would clear the demotion.
func TestExecuteCall_DemotedToolNotInSentArrayKeepsLoadFirstAnswer(t *testing.T) {
	store := &fakeInjectionStateStore{store: map[string]state.InjectionState{
		"s1": {KnownTools: []state.KnownToolEntry{{ToolName: "p__catalog", LRURank: 1, Demoted: true}}},
	}}
	orch, exec := gateOrchWithStore(t, store)
	sink := &recordingEventSink{}
	orch.eventSink = sink
	loads := newTurnToolLoads()
	ctx := withTurnToolLoads(sentCtx("p__always"), loads)

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})
	if !strings.Contains(res.Error, "is not loaded") || !strings.Contains(res.Error, `_meta__load_tools(names="p__catalog")`) {
		t.Errorf("a demoted tool must get the load-first answer, got %q", res.Error)
	}
	if exec.count != 0 || store.updateCalls != 0 || len(loads.snapshot()) != 0 {
		t.Errorf("a demoted tool must not run or be loaded, ran %d, store writes %d, loads %v",
			exec.count, store.updateCalls, loads.snapshot())
	}
	if results := findToolCallResultPayloads(t, sink.snapshot()); res.NotRun || len(results) != 1 || results[0].Status != "error" {
		t.Errorf("a demoted tool's refusal must stay an error, got NotRun=%v results %+v", res.NotRun, results)
	}
}

// The sent-set lookup keys off the canonical registered name, so a call that
// mangles separators (hyphen<->underscore) still matches the sent FQN.
func TestExecuteCall_NameNormalizationMatchesSentArray(t *testing.T) {
	orch, exec := gateOrch(t)
	ctx := sentCtx("p__foo-bar") // canonical, hyphenated

	// Model calls it with an underscore ("foo_bar") — resolveAction canonicalizes.
	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "foo_bar", FromLLM: true})
	if res.Error != "" {
		t.Fatalf("separator-drift name of a sent tool must resolve and run, got error %q", res.Error)
	}
	if exec.count != 1 {
		t.Errorf("normalized sent tool must execute once, ran %d time(s)", exec.count)
	}
}

// When no native array was sent for this request (text mode, the sub-agent loop
// — both list every tool in full inline — or any non-agent-loop caller), the
// gate is a no-op: the tool runs regardless of loadedness. This is what keeps
// the sub-agent path working on a native-capable provider.
func TestExecuteCall_NoNativeArraySent_NoGate(t *testing.T) {
	orch, exec := gateOrch(t)
	ctx := actor.WithSessionID(context.Background(), "s1") // no sent-tools value

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})
	if res.Error != "" {
		t.Fatalf("no native array sent → gate must not fire, got error %q", res.Error)
	}
	if exec.count != 1 {
		t.Errorf("tool must execute when no native array was sent, ran %d time(s)", exec.count)
	}
}

// A scope that inherits a parent's sent set but explicitly marks itself as
// surfacing no native array (withoutSentNativeTools — what the sub-agent loop
// does) neutralizes the gate: the inherited set must NOT be enforced against
// this scope's tools. Distinct from an absent set (same no-op) and from an
// empty present set (refuses all, covered elsewhere).
func TestExecuteCall_ClearedSentSet_NoGate(t *testing.T) {
	orch, exec := gateOrch(t)
	// Parent scope sent only p__always; a nested scope then clears it.
	ctx := withoutSentNativeTools(sentCtx("p__always"))

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})
	if res.Error != "" {
		t.Fatalf("a cleared sent set must neutralize the gate, got error %q", res.Error)
	}
	if exec.count != 1 {
		t.Errorf("tool must run under a cleared sent set, ran %d time(s)", exec.count)
	}
}

// The gate targets model-originated calls only. Internal/host-constructed calls
// (preparers, guards, pipelines, formatters — FromLLM=false) are trusted and
// never gated, even when the tool is absent from the sent array.
func TestExecuteCall_InternalCall_NotGated(t *testing.T) {
	orch, exec := gateOrch(t)
	ctx := sentCtx() // empty sent set

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: false})
	if res.Error != "" {
		t.Fatalf("internal (FromLLM=false) call must not be gated, got error %q", res.Error)
	}
	if exec.count != 1 {
		t.Errorf("internal call must execute once, ran %d time(s)", exec.count)
	}
}

// End-to-end through Run: an unloaded write never reaches a confirmation
// prompt. The unrun call asks nothing and loads the tool; only the model's call
// of the loaded tool raises the confirmation, so the user is asked about a call
// made with the tool's description in view, and nothing ran before that.
func TestRun_UnloadedWriteAsksNoConfirmationUntilCalledAgain(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "confirmation gate fixtures",
		Actions: []Action{{Name: "catalog_write", Description: "A write tool not yet loaded.",
			Parameters: []Parameter{{Name: "name", Description: "Name."}}}},
	}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(PluginCapability{
		Name: "conf", Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{}); err != nil {
		t.Fatalf("register conf: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	write := func(id string) provider.CompletionResponse {
		return provider.CompletionResponse{ToolCalls: []provider.ToolCall{
			{ID: id, Name: "p__catalog_write", Arguments: map[string]string{"name": "A"}},
		}}
	}
	llm := &scriptedNativeLLM{rounds: []provider.CompletionResponse{write("w1"), write("w2")}}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, OrchestratorOpts{
			ConfirmationPlugin:  "conf",
			ConfirmationAction:  "check",
			InjectionStateStore: &fakeInjectionStateStore{},
		})

	if _, err := orch.Run(actor.WithSessionID(context.Background(), "s1"), "s1", "create A"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exec.count != 0 {
		t.Errorf("no write may execute before the approval, ran %d time(s)", exec.count)
	}
	if pending, _, _, _ := loadPendingToolCall(sessions, "s1"); pending == nil || pending.ID != "w2" {
		t.Errorf("the confirmation must be for the call of the loaded tool (w2), not the unrun one, got %+v", pending)
	}
}

// An unresolvable call — hallucinated action, mangled name, unknown plugin —
// must never reach a confirmation prompt: executeCall's not-found path owns
// that error, and asking the user to approve a call that cannot run costs a
// wasted approval plus a SECOND prompt for the model's corrected retry
// (observed as the double-confirm in ai_eval
// checkout/put_in_container_by_barcode, where the model called action
// "timly__load_tools" under plugin "timly"). The gate must also stay free of
// side effects on the skip: no messages, no pending call.
func TestMaybeRequireConfirmation_UnresolvableCallNeverPrompts(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "confirmation gate fixtures",
		Actions: []Action{{Name: "sent_write", Description: "A write tool that was sent."}},
	}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(PluginCapability{
		Name: "conf", Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{}); err != nil {
		t.Fatalf("register conf: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})
	ctx := sentCtx("p__sent_write")

	for _, call := range []ToolCall{
		{ID: "c1", Plugin: "p", Action: "does_not_exist", FromLLM: true}, // known plugin, garbage action
		{ID: "c2", Plugin: "ghost", Action: "sent_write", FromLLM: true}, // unknown plugin
		{ID: "c3", Plugin: "p", Action: "p__load_tools", FromLLM: true},  // meta action mangled under a plugin prefix
	} {
		rr, raised := orch.maybeRequireConfirmation(ctx, sessions, "s1", call, "do it", nil, false, nil)
		if raised || rr != nil {
			t.Fatalf("unresolvable call %s/%s must not raise a confirmation, got raised=%v rr=%v",
				call.Plugin, call.Action, raised, rr)
		}
	}
	if s, err := sessions.Get("s1"); err != nil || len(s.Messages) != 0 {
		t.Fatalf("the skip must leave the session untouched, got %d message(s), err=%v", len(s.Messages), err)
	}
}

// A confirmation raised mid-batch abandons every later call of the same model
// response. Each abandoned call must leave an explicit "NOT EXECUTED" exchange
// in history — BEFORE the prompt row, so the resolved prompt+reply pair stays
// adjacent for stripApprovedConfirmationExchanges — or the model later treats
// its own dropped call as done and invents its result (observed in ai_eval
// structure/create_category_and_container_type: the second create of a
// two-call batch vanished silently and the model reported both as created).
func TestMaybeRequireConfirmation_InterruptedBatchRecordedBeforePrompt(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "confirmation gate fixtures",
		Actions: []Action{
			{Name: "write_a", Description: "First write of the batch."},
			{Name: "write_b", Description: "Second write of the batch."},
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
	sessions.Create(state.SessionParams{ID: "s1"})
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})
	ctx := sentCtx("p__write_a", "p__write_b")

	gated := ToolCall{ID: "c-a", Plugin: "p", Action: "write_a", FromLLM: true, Args: map[string]string{"name": "A"}}
	dropped := ToolCall{ID: "c-b", Plugin: "p", Action: "write_b", FromLLM: true, Args: map[string]string{"name": "B"}}
	rr, raised := orch.maybeRequireConfirmation(ctx, sessions, "s1", gated, "create both", []ToolCall{dropped}, true, nil)
	if !raised || rr == nil {
		t.Fatalf("gated write must raise a confirmation, got raised=%v rr=%v", raised, rr)
	}

	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(s.Messages) != 3 {
		t.Fatalf("want 3 messages (dropped call, its result, prompt row), got %d: %+v", len(s.Messages), s.Messages)
	}
	callRow, resultRow, promptRow := s.Messages[0], s.Messages[1], s.Messages[2]
	if len(callRow.ToolCalls) != 1 || callRow.ToolCalls[0].ID != "c-b" || callRow.ToolCalls[0].Name != "p__write_b" {
		t.Errorf("first row must carry the dropped call c-b, got %+v", callRow)
	}
	if resultRow.Role != provider.RoleTool || resultRow.ToolCallID != "c-b" {
		t.Errorf("second row must be the dropped call's tool result, got %+v", resultRow)
	}
	if !strings.Contains(resultRow.Content, "NOT EXECUTED") || !strings.Contains(resultRow.Content, "p__write_a") {
		t.Errorf("dropped-call result must say NOT EXECUTED and name the gated call, got %q", resultRow.Content)
	}
	if len(promptRow.ToolCalls) != 0 || promptRow.Role != provider.RoleAssistant || promptRow.Content != rr.Response {
		t.Errorf("last row must be the confirmation prompt %q, got %+v", rr.Response, promptRow)
	}
}

// Text-mode counterpart: a stack whose responses carry [tool_call] blocks
// instead of native tool_calls must get the trace in ITS transcript shape
// (assistant text + [plugin_output] user row), or the pair reads as a
// malformed turn to the very model it is meant to inform.
func TestMaybeRequireConfirmation_InterruptedBatchRecordedInTextMode(t *testing.T) {
	orch, sessions := interruptedTraceFixture(t)

	gated := ToolCall{ID: "c-a", Plugin: "p", Action: "write_a", FromLLM: true, Args: map[string]string{"name": "A"}}
	dropped := ToolCall{ID: "c-b", Plugin: "p", Action: "write_b", FromLLM: true, Args: map[string]string{"name": "B"}}
	if _, raised := orch.maybeRequireConfirmation(sentCtx("p__write_a", "p__write_b"), sessions, "s1",
		gated, "create both", []ToolCall{dropped}, false, nil); !raised {
		t.Fatal("gated write must raise a confirmation")
	}

	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(s.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(s.Messages), s.Messages)
	}
	callRow, resultRow := s.Messages[0], s.Messages[1]
	if callRow.Role != provider.RoleAssistant || len(callRow.ToolCalls) != 0 ||
		callRow.Content != formatToolCallMessage(dropped) {
		t.Errorf("text mode must record the call as an assistant [tool_call] line, got %+v", callRow)
	}
	if resultRow.Role != provider.RoleUser || !strings.Contains(resultRow.Content, "[plugin_output]") ||
		!strings.Contains(resultRow.Content, "NOT EXECUTED") {
		t.Errorf("text mode must record the result as a wrapped user row, got %+v", resultRow)
	}
}

// The trace must be tied to the confirmation, not to the mere presence of
// later calls: when the gated call needs no approval the loop keeps executing
// them itself, so recording "NOT EXECUTED" for calls that are about to run
// would plant a lie in history. Recording unconditionally passes every other
// test in this file, so this negative case is what pins the coupling.
func TestMaybeRequireConfirmation_NoConfirmationRecordsNoTrace(t *testing.T) {
	orch, sessions := interruptedTraceFixture(t)

	readOnly := ToolCall{ID: "c-r", Plugin: "p", Action: "read_x", FromLLM: true}
	dropped := ToolCall{ID: "c-b", Plugin: "p", Action: "write_b", FromLLM: true}
	rr, raised := orch.maybeRequireConfirmation(sentCtx("p__read_x", "p__write_b"), sessions, "s1",
		readOnly, "just look", []ToolCall{dropped}, true, nil)
	if raised || rr != nil {
		t.Fatalf("a read-only call must not raise a confirmation, got raised=%v rr=%v", raised, rr)
	}

	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(s.Messages) != 0 {
		t.Fatalf("no confirmation means no interrupted calls, so nothing may be recorded, got %d: %+v",
			len(s.Messages), s.Messages)
	}
}

// Shared fixture for the interrupted-trace tests: two writes and one read-only
// action behind a confirmation plugin that gates every write, plus an empty
// session so a message-count assertion means what it says.
func interruptedTraceFixture(t *testing.T) (*Orchestrator, *state.SessionStore) {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "confirmation gate fixtures",
		Actions: []Action{
			{Name: "write_a", Description: "First write of the batch."},
			{Name: "write_b", Description: "Second write of the batch."},
			{Name: "read_x", Description: "A read-only query.", ReadOnly: true},
		},
	}, &countingExecutor{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(PluginCapability{
		Name: "conf", Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{}); err != nil {
		t.Fatalf("register conf: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})
	return orch, sessions
}

// End-to-end through Run: the model answers ONE response with TWO write calls;
// the first needs confirmation, so the loop pauses there. The second call must
// not vanish — the real loop hands calls[i+1:] to the gate, which records the
// explicit NOT EXECUTED exchange before the prompt row, and neither write may
// have executed. Pins the exact wiring the unit test above cannot see (the
// call-site slice and the per-response native flag). The dropped call's
// message is the first stored for the response, so it takes the response's
// thinking blocks and the pending call keeps none.
func TestRun_ConfirmationMidBatchLeavesNotExecutedTrace(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "confirmation gate fixtures",
		Actions: []Action{
			{Name: "write_a", Description: "First write of the batch.", AlwaysInclude: true},
			{Name: "write_b", Description: "Second write of the batch.", AlwaysInclude: true},
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
	sessions.Create(state.SessionParams{ID: "s1"})
	llm := &scriptedNativeLLM{rounds: []provider.CompletionResponse{
		{Thinking: replayThinking, ThinkingFingerprint: "fp-1", ToolCalls: []provider.ToolCall{
			{ID: "c-a", Name: "p__write_a", Arguments: map[string]string{"name": "A"}},
			{ID: "c-b", Name: "p__write_b", Arguments: map[string]string{"name": "B"}},
		}},
	}}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, OrchestratorOpts{
			ConfirmationPlugin:  "conf",
			ConfirmationAction:  "check",
			InjectionStateStore: &fakeInjectionStateStore{},
		})

	res, err := orch.Run(actor.WithSessionID(context.Background(), "s1"), "s1", "create both")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exec.count != 0 {
		t.Errorf("no write may execute before the approval, ran %d time(s)", exec.count)
	}

	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	droppedCallIdx, droppedResultIdx, promptIdx := -1, -1, -1
	for i, m := range s.Messages {
		if len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "c-b" {
			droppedCallIdx = i
		}
		if m.Role == provider.RoleTool && m.ToolCallID == "c-b" {
			droppedResultIdx = i
			if !strings.Contains(m.Content, "NOT EXECUTED") || !strings.Contains(m.Content, "p__write_a") {
				t.Errorf("dropped-call result must say NOT EXECUTED and name the gated call, got %q", m.Content)
			}
		}
		if m.Role == provider.RoleAssistant && m.Content != "" && m.Content == res.Response {
			promptIdx = i
		}
	}
	if droppedCallIdx == -1 || droppedResultIdx == -1 || promptIdx == -1 {
		t.Fatalf("missing rows: droppedCall=%d droppedResult=%d prompt=%d in %+v",
			droppedCallIdx, droppedResultIdx, promptIdx, s.Messages)
	}
	if droppedCallIdx >= droppedResultIdx || droppedResultIdx >= promptIdx {
		t.Errorf("dropped exchange must precede the prompt row, got call=%d result=%d prompt=%d",
			droppedCallIdx, droppedResultIdx, promptIdx)
	}
	if pending, _, _, thinking := loadPendingToolCall(sessions, "s1"); pending == nil || pending.ID != "c-a" ||
		!reflect.DeepEqual(thinking, responseThinking{}) {
		t.Errorf("pending call must be the gated write_a (c-a) without thinking, got %+v with %+v", pending, thinking)
	}
	if m := s.Messages[droppedCallIdx]; !reflect.DeepEqual(m.Thinking, replayThinking) || m.ThinkingFingerprint != "fp-1" {
		t.Errorf("dropped call message thinking = %q (fingerprint %q), want the response's blocks and fingerprint", m.Thinking, m.ThinkingFingerprint)
	}
}

// scriptedNativeLLM returns a pre-scripted CompletionResponse per round so a
// test can drive a full multi-round agent loop, and records every request; it
// advertises FeatureTools so the orchestrator takes the native path.
type scriptedNativeLLM struct {
	rounds   []provider.CompletionResponse
	calls    int
	requests []*provider.CompletionRequest
}

func (l *scriptedNativeLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.requests = append(l.requests, req)
	i := l.calls
	l.calls++
	if i < len(l.rounds) {
		r := l.rounds[i]
		return &r, nil
	}
	return &provider.CompletionResponse{Content: "done"}, nil
}

func (l *scriptedNativeLLM) SupportsFeature(f provider.Feature) bool {
	return f == provider.FeatureTools
}

// loadOnCallOrch builds an orchestrator over an always-include tool, two
// catalog tools with a parameter and the first one's two companions, driven by
// a scripted native model; store is wired when not nil.
func loadOnCallOrch(t *testing.T, store *fakeInjectionStateStore, rounds ...provider.CompletionResponse) (
	*Orchestrator, *scriptedNativeLLM, *countingExecutor,
) {
	t.Helper()
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	query := []Parameter{{Name: "q", Description: "Query."}}
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "load-on-call fixtures",
		Actions: []Action{
			{Name: "always", Description: "Always-include tool.", AlwaysInclude: true, ReadOnly: true},
			{Name: "catalog", Description: "Catalog tool.", ReadOnly: true, Parameters: query,
				Companions: []string{"lookup", "list"}},
			{Name: "other", Description: "Another catalog tool.", ReadOnly: true, Parameters: query},
			{Name: "lookup", Description: "The catalog tool's first companion.", ReadOnly: true, Parameters: query},
			{Name: "list", Description: "The catalog tool's second companion.", ReadOnly: true, Parameters: query},
		},
	}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	llm := &scriptedNativeLLM{rounds: rounds}
	opts := OrchestratorOpts{}
	if store != nil {
		opts.InjectionStateStore = store
	}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, opts)
	return orch, llm, exec
}

func runTurn(t *testing.T, orch *Orchestrator) *RunResult {
	t.Helper()
	res, err := orch.Run(actor.WithSessionID(context.Background(), "s1"), "s1", "look it up")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func sentToolsJSON(t *testing.T, req *provider.CompletionRequest) string {
	t.Helper()
	raw, err := json.Marshal(req.Tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	return string(raw)
}

func callOf(id, name string, args map[string]string) provider.CompletionResponse {
	return provider.CompletionResponse{ToolCalls: []provider.ToolCall{{ID: id, Name: name, Arguments: args}}}
}

// End-to-end through Run: the model calls a catalog tool it never loaded. That
// call does not run, the tool is loaded, and the model sends the same response
// again: the call that did not run now runs exactly once, while a call of that
// response which already ran does not run again. A third identical response
// repeats only calls that ran and is stopped.
func TestRun_IdenticalRetryRunsOnlyTheUnrunCallsOnce(t *testing.T) {
	catalog := provider.ToolCall{Name: "p__catalog", Arguments: map[string]string{"q": "x"}}
	always := provider.ToolCall{Name: "p__always", Arguments: map[string]string{}}
	for _, tc := range []struct {
		name     string
		response []provider.ToolCall
		want     []string // executed and answered calls, in order: "<tool> ran" or "<tool> loaded"
	}{
		{"one unloaded call", []provider.ToolCall{catalog},
			[]string{"p__catalog loaded", "p__catalog ran"}},
		{"a loaded call and an unloaded one", []provider.ToolCall{always, catalog},
			[]string{"p__always ran", "p__catalog loaded", "p__catalog ran"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			round := provider.CompletionResponse{ToolCalls: tc.response}
			orch, llm, exec := loadOnCallOrch(t, &fakeInjectionStateStore{}, round, round, round)

			res := runTurn(t, orch)
			var got []string
			for i, call := range res.ToolCalls {
				outcome := " ran"
				if res.Results[i].Error != "" {
					outcome = " loaded"
					if !strings.Contains(res.Results[i].Error, "call p__catalog again") {
						t.Errorf("unexpected error for %s: %q", call.Action, res.Results[i].Error)
					}
				}
				got = append(got, toolFQN(call.Plugin, call.Action)+outcome)
			}
			if !reflect.DeepEqual(got, tc.want) || exec.count != len(tc.want)-1 {
				t.Errorf("calls = %v (executor ran %d), want %v", got, exec.count, tc.want)
			}
			if len(llm.requests) != 4 {
				t.Fatalf("want 4 requests (call, retry, repeat, answer), got %d", len(llm.requests))
			}
			afterRepeat := llm.requests[3].Messages
			if last := afterRepeat[len(afterRepeat)-1].Content; !strings.Contains(last, "You are repeating the same tool call") {
				t.Errorf("the identical repeat of calls that ran must be stopped, last message %q", last)
			}
		})
	}
}

// After _meta__load_tools or a call to an unloaded tool, the next request of the
// turn carries the tool and its companions with their full schemas, in the same
// array on both paths — also when the session store cannot save the promotion,
// when there is no store, when the stored sticky set is full with tools ranked
// above them, and when a companion that was already loaded sits at the bottom
// of a full sticky set and the other companion's promotion pushes it out.
func TestRun_LoadedToolAndCompanionsAreInTheNextRequest(t *testing.T) {
	loadTools := callOf("l1", "_meta__load_tools", map[string]string{"names": "p__catalog"})
	refOrch, ref, _ := loadOnCallOrch(t, &fakeInjectionStateStore{}, loadTools)
	runTurn(t, refOrch)
	want := sentToolsJSON(t, ref.requests[1])
	for _, tool := range []string{"p__catalog", "p__lookup", "p__list"} {
		if !strings.Contains(want, `"name":"`+tool+`","description":"`) {
			t.Fatalf("reference array after load_tools must carry %s, got %s", tool, want)
		}
	}
	if strings.Count(want, `"q":`) != 3 {
		t.Fatalf("reference array must carry all three schemas, got %s", want)
	}

	full := make([]state.KnownToolEntry, maxStickyTools)
	for i := range full {
		full[i] = state.KnownToolEntry{ToolName: fmt.Sprintf("p__filler%02d", i), LRURank: 1000}
	}
	// 38 tools ranked above everything this turn promotes, then p__lookup ranked
	// below it: the tool's promotion fills the 40th place, p__list's pushes
	// p__lookup out of the stored set.
	lookupLast := append(append([]state.KnownToolEntry(nil), full[:maxStickyTools-2]...),
		state.KnownToolEntry{ToolName: "p__lookup", LRURank: 0})
	for _, path := range []struct {
		name string
		call provider.CompletionResponse
	}{
		{"load_tools", loadTools},
		{"on call", callOf("r1", "p__catalog", map[string]string{"q": "x"})},
	} {
		for _, tc := range []struct {
			name  string
			store func() *fakeInjectionStateStore
		}{
			{"store saves the promotion", func() *fakeInjectionStateStore { return &fakeInjectionStateStore{} }},
			{"store write fails", func() *fakeInjectionStateStore {
				return &fakeInjectionStateStore{failUpdateErr: errors.New("simulated db write failure")}
			}},
			{"store read fails", func() *fakeInjectionStateStore {
				return &fakeInjectionStateStore{failGetErr: errors.New("simulated db read failure")}
			}},
			{"no store", func() *fakeInjectionStateStore { return nil }},
			{"sticky set at its cap", func() *fakeInjectionStateStore {
				return &fakeInjectionStateStore{store: map[string]state.InjectionState{"s1": {KnownTools: full}}}
			}},
			{"loaded companion pushed out of the sticky set", func() *fakeInjectionStateStore {
				return &fakeInjectionStateStore{store: map[string]state.InjectionState{"s1": {KnownTools: lookupLast}}}
			}},
		} {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				orch, llm, exec := loadOnCallOrch(t, tc.store(), path.call)
				runTurn(t, orch)
				if exec.count != 0 {
					t.Errorf("no business tool may run, ran %d time(s)", exec.count)
				}
				if len(llm.requests) < 2 {
					t.Fatalf("want a request after the load, got %d", len(llm.requests))
				}
				if got := sentToolsJSON(t, llm.requests[1]); got != want {
					t.Errorf("next request's tools differ from the array after load_tools:\n got %s\nwant %s", got, want)
				}
			})
		}
	}
}

func TestRun_LoadedToolsLeaveTheNextRequestsCatalog(t *testing.T) {
	descriptions := map[string]string{
		"p__catalog": "Catalog tool.",
		"p__other":   "Another catalog tool.",
		"p__lookup":  "The catalog tool's first companion.",
		"p__list":    "The catalog tool's second companion.",
	}
	always := callOf("a1", "p__always", map[string]string{})
	for _, tc := range []struct {
		name   string
		load   provider.CompletionResponse
		loaded []string
	}{
		{"load_tools", callOf("l1", "_meta__load_tools", map[string]string{"names": "p__other"}), []string{"p__other"}},
		{"on call", callOf("r1", "p__other", map[string]string{"q": "x"}), []string{"p__other"}},
		{"as a companion", callOf("r1", "p__catalog", map[string]string{"q": "x"}),
			[]string{"p__catalog", "p__lookup", "p__list"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, llm, _ := loadOnCallOrch(t, &fakeInjectionStateStore{}, tc.load, always)
			runTurn(t, orch)
			if len(llm.requests) != 3 {
				t.Fatalf("want 3 requests (load, call, answer), got %d", len(llm.requests))
			}
			before, after, next := *llm.requests[0].Segments, *llm.requests[1].Segments, *llm.requests[2].Segments
			tools := sentToolsJSON(t, llm.requests[1])
			want := before.Conversation
			for _, fqn := range tc.loaded {
				line := "- " + fqn + ": " + descriptions[fqn] + "\n"
				if !strings.Contains(want, line) {
					t.Fatalf("the first request's catalog must list %s, got %q", fqn, before.Conversation)
				}
				want = strings.Replace(want, line, "", 1)
				if !strings.Contains(tools, `"name":"`+fqn+`"`) {
					t.Errorf("the next request's tools must carry %s, got %s", fqn, tools)
				}
			}
			if after.Fixed != before.Fixed {
				t.Errorf("a load changed the Fixed segment:\n got %q\nwant %q", after.Fixed, before.Fixed)
			}
			if after.Conversation != want {
				t.Errorf("after the load, the Conversation segment must only lose the loaded tools' catalog lines:\n got %q\nwant %q",
					after.Conversation, want)
			}
			if next.Fixed != after.Fixed || next.Conversation != after.Conversation {
				t.Errorf("a request without a load changed the segments before the per-round one:\n got %#v\nwant %#v", next, after)
			}
		})
	}
}

// Several unloaded calls in one response, or _meta__load_tools(X) together with
// X(...): every unloaded call gets the call-again answer, none runs, and the
// next request carries every tool so loaded.
func TestRun_SeveralUnloadedCallsInOneResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		calls    []provider.ToolCall
		answered []string
	}{
		{"two unloaded tools", []provider.ToolCall{
			{ID: "a1", Name: "p__catalog", Arguments: map[string]string{"q": "1"}},
			{ID: "a2", Name: "p__other", Arguments: map[string]string{"q": "2"}},
		}, []string{"p__catalog", "p__other"}},
		{"load_tools and the call together", []provider.ToolCall{
			{ID: "b1", Name: "_meta__load_tools", Arguments: map[string]string{"names": "p__catalog"}},
			{ID: "b2", Name: "p__catalog", Arguments: map[string]string{"q": "1"}},
		}, []string{"p__catalog"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, llm, exec := loadOnCallOrch(t, &fakeInjectionStateStore{}, provider.CompletionResponse{ToolCalls: tc.calls})
			res := runTurn(t, orch)
			if exec.count != 0 {
				t.Errorf("no unloaded call may run, ran %d time(s)", exec.count)
			}
			next := sentToolsJSON(t, llm.requests[1])
			for _, fqn := range tc.answered {
				answered := false
				for i, call := range res.ToolCalls {
					if toolFQN(call.Plugin, call.Action) == fqn && strings.Contains(res.Results[i].Error, "call "+fqn+" again") {
						answered = true
					}
				}
				if !answered {
					t.Errorf("%s must get the call-again answer, got %+v", fqn, res.Results)
				}
				if !strings.Contains(next, `"name":"`+fqn+`"`) {
					t.Errorf("next request must carry %s, got %s", fqn, next)
				}
			}
		})
	}
}

// End-to-end through Run: a call answered without running is not a failed tool
// call. Its tool_call_result has the status not_run; it is not observed as a
// plugin call, not logged as a tool error, and with both protections at their
// lowest threshold it gets no repeated-failure hint and no demotion. The call
// of the same response that ran is observed as before.
func TestRun_UnrunCallIsNotAFailedToolCall(t *testing.T) {
	store := &fakeInjectionStateStore{}
	orch, llm, exec := loadOnCallOrch(t, store, provider.CompletionResponse{ToolCalls: []provider.ToolCall{
		{ID: "a1", Name: "p__always", Arguments: map[string]string{}},
		{ID: "c1", Name: "p__catalog", Arguments: map[string]string{"q": "x"}},
	}})
	sink, obs := &recordingEventSink{}, &fakeObserver{}
	orch.eventSink, orch.pluginCallObserver = sink, obs
	orch.toolErrorHandling = ToolErrorHandlingConfig{LoopCapPerTurn: 1, StickyDemotionThreshold: 1}
	warnings := captureWarnings(t)

	runTurn(t, orch)
	if exec.count != 1 {
		t.Fatalf("only p__always may run, ran %d time(s)", exec.count)
	}
	var statuses []string
	for _, r := range findToolCallResultPayloads(t, sink.snapshot()) {
		statuses = append(statuses, r.Status)
	}
	if !reflect.DeepEqual(statuses, []string{"ok", "not_run"}) {
		t.Errorf("tool_call_result statuses = %v, want [ok not_run]", statuses)
	}
	if len(obs.calls) != 1 || obs.calls[0].action != "always" || obs.calls[0].failed {
		t.Errorf("only the call that ran may be observed, as a success, got %+v", obs.calls)
	}
	if strings.Contains(warnings.String(), "tool call error") {
		t.Errorf("the unrun call must not be logged as a tool error, got %s", warnings.String())
	}
	for _, m := range llm.requests[1].Messages {
		if strings.Contains(m.Content, "failed 1 times") {
			t.Errorf("the unrun call must not trigger the repeated-failure hint, got %q", m.Content)
		}
	}
	if kt, ok := storedTool(store, "p__catalog"); !ok || kt.Demoted {
		t.Errorf("the loaded tool must stay promoted and not demoted, got %+v (found=%v)", kt, ok)
	}
}

// companionOrch builds an orchestrator whose catalog tool p__write names the
// given companions, next to one tool of each kind a companion can be: a lookup
// (which names p__deep as its own companion), p__deep, an always-included tool,
// a user-only action and a preparer action.
func companionOrch(t *testing.T, store *fakeInjectionStateStore, companions ...string) *Orchestrator {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "companion fixtures",
		Actions: []Action{
			{Name: "write", Description: "A write tool.", Companions: companions},
			{Name: "lookup", Description: "A lookup.", ReadOnly: true, Companions: []string{"deep"}},
			{Name: "deep", Description: "The lookup's companion.", ReadOnly: true},
			{Name: "always", Description: "Always-include tool.", AlwaysInclude: true, ReadOnly: true},
			{Name: "admin", Description: "User-only action.", UserOnly: true},
			{Name: "prepare", Description: "Preparer action."},
		},
	}, &countingExecutor{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	return NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "p", Action: "prepare"}},
		InjectionStateStore: store,
	})
}

// Loading a tool loads its companions with it, through _meta__load_tools and on
// call alike: promoted in the session store, recorded in the turn's loads and
// named in the answer. A companion the session may not see is skipped and the
// tool still loads; a demoted one is skipped and stays demoted; one already
// loaded (stored, or earlier this turn) or always included is left as it is,
// though a stored one is kept in the turn's loads; a companion's own companions
// are not loaded; a tool without companions loads alone. (A companion is an
// action of the tool's own plugin, so the profile cannot hide it while the tool
// stays visible.)
func TestLoad_CompanionsLoadWithTheTool(t *testing.T) {
	demoted := state.KnownToolEntry{ToolName: "p__lookup", LRURank: 1, Demoted: true}
	stored := state.KnownToolEntry{ToolName: "p__lookup"}
	for _, tc := range []struct {
		name       string
		companions []string
		stored     []state.KnownToolEntry // the session's tools before the load
		earlier    string                 // a tool loaded earlier this turn
		want       []string               // the companions loaded with p__write
		kept       string                 // a companion kept in the turn's loads without loading it
	}{
		{"companion, not its own", []string{"lookup"}, nil, "", []string{"p__lookup"}, ""},
		{"companions the session may not see", []string{"admin", "prepare", "nope", "lookup"}, nil, "", []string{"p__lookup"}, ""},
		{"demoted companion", []string{"lookup"}, []state.KnownToolEntry{demoted}, "", nil, ""},
		{"stored companion", []string{"lookup"}, []state.KnownToolEntry{stored}, "", nil, "p__lookup"},
		{"companion loaded earlier this turn", []string{"lookup"}, nil, "p__lookup", nil, ""},
		{"always-included companion", []string{"always"}, nil, "", nil, ""},
		{"no companions", nil, nil, "", nil, ""},
	} {
		for _, path := range []string{"load_tools", "on call"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				store := &fakeInjectionStateStore{store: map[string]state.InjectionState{
					"s1": {KnownTools: append([]state.KnownToolEntry(nil), tc.stored...)},
				}}
				orch := companionOrch(t, store, tc.companions...)
				loads := newTurnToolLoads()
				if tc.earlier != "" {
					loads.record(tc.earlier)
				}
				ctx := withTurnToolLoads(sentCtx("p__always"), loads)

				var named []string
				if path == "load_tools" {
					exec, _ := orch.registry.GetExecutor(metaPluginName)
					res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "p__write"}})
					named = decodeLoadResult(t, res).Loaded
				} else {
					res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "write", FromLLM: true})
					if !res.NotRun {
						t.Fatalf("the call must be answered without running, got %+v", res)
					}
					named = []string{"p__write"}
					for _, fqn := range []string{"p__lookup", "p__deep", "p__always", "p__admin", "p__prepare", "p__nope"} {
						if strings.Contains(res.Error, fqn) {
							named = append(named, fqn)
						}
					}
				}
				want := append([]string{"p__write"}, tc.want...)
				if !reflect.DeepEqual(named, want) {
					t.Errorf("the answer names %v as loaded, want %v", named, want)
				}

				wantLoads := map[string]bool{}
				for _, fqn := range want {
					wantLoads[fqn] = true
				}
				for _, fqn := range []string{tc.earlier, tc.kept} {
					if fqn != "" {
						wantLoads[fqn] = true
					}
				}
				if got := loads.snapshot(); !reflect.DeepEqual(got, wantLoads) {
					t.Errorf("the turn's loads = %v, want %v", got, wantLoads)
				}

				if got := len(store.store["s1"].KnownTools); got != len(tc.stored)+len(want) {
					t.Errorf("the store holds %d tools, want %d: %+v", got, len(tc.stored)+len(want), store.store["s1"].KnownTools)
				}
				for _, before := range tc.stored {
					if after, _ := storedTool(store, before.ToolName); after != before {
						t.Errorf("%s must be left as it was, got %+v, want %+v", before.ToolName, after, before)
					}
				}
				for _, fqn := range want {
					if kt, ok := storedTool(store, fqn); !ok || kt.Demoted {
						t.Errorf("%s must be promoted, got %+v (found=%v)", fqn, kt, ok)
					}
				}
			})
		}
	}
}

// A tool named in a _meta__load_tools batch loads its own companions also when
// an earlier name of the batch already loaded it as a companion: the order of
// the names does not change what loads, and each tool is promoted once.
func TestLoadTools_BatchOrderDoesNotChangeWhatLoads(t *testing.T) {
	for _, tc := range []struct {
		names string
		want  []string
	}{
		{"p__write,p__lookup", []string{"p__write", "p__lookup", "p__deep"}},
		{"p__lookup,p__write", []string{"p__lookup", "p__deep", "p__write"}},
	} {
		t.Run(tc.names, func(t *testing.T) {
			store := &fakeInjectionStateStore{}
			orch := companionOrch(t, store, "lookup")
			exec, _ := orch.registry.GetExecutor(metaPluginName)
			res := exec.Execute(withTurnToolLoads(sentCtx("p__always"), newTurnToolLoads()),
				ToolCall{ID: "c1", Args: map[string]string{"names": tc.names}})
			if got := decodeLoadResult(t, res).Loaded; !reflect.DeepEqual(got, tc.want) || store.updateCalls != 3 {
				t.Errorf("loaded = %v with %d store writes, want %v with 3", got, store.updateCalls, tc.want)
			}
		})
	}
}

// The answer to a call of an unloaded tool keeps its first sentence, with the
// tool's name as the first quoted string, names the companions loaded with it,
// and tells the model to call the tools directly instead of loading them again.
// Its tool_call_result keeps the status not_run.
func TestExecuteCall_LoadOnCallAnswer(t *testing.T) {
	const first = `tool "p__write" was NOT run: it was not loaded, so its parameters were not in your context. `
	for _, tc := range []struct {
		name       string
		companions []string
		want       string
	}{
		{"no companions", nil, first +
			"It is loaded now: its description and parameters are in your tools. " +
			"Read its description, then call p__write again directly with the parameters it defines; " +
			"do not load it again with _meta__load_tools."},
		{"two companions", []string{"lookup", "deep"}, first +
			"It is loaded now, together with p__lookup, p__deep: their descriptions and parameters are in your tools. " +
			"Read its description, then call p__write again directly with the parameters it defines, " +
			"and call p__lookup, p__deep directly when you need them; " +
			"do not load any of these tools again with _meta__load_tools."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch := companionOrch(t, &fakeInjectionStateStore{}, tc.companions...)
			sink := &recordingEventSink{}
			orch.eventSink = sink
			res := orch.executeCall(withTurnToolLoads(sentCtx("p__always"), newTurnToolLoads()),
				ToolCall{ID: "c1", Plugin: "p", Action: "write", FromLLM: true})
			if res.Error != tc.want || !res.NotRun {
				t.Errorf("answer (NotRun=%v):\n got %q\nwant %q", res.NotRun, res.Error, tc.want)
			}
			if results := findToolCallResultPayloads(t, sink.snapshot()); len(results) != 1 || results[0].Status != "not_run" {
				t.Errorf("want one tool_call_result with the status not_run, got %+v", results)
			}
		})
	}
}

// The load_tools executor records on its own goroutine, which can outlive its
// deadline while the agent loop reads the turn's loads; run with -race.
func TestTurnToolLoads_RecordWhileRead(t *testing.T) {
	loads := newTurnToolLoads()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			loads.record(fmt.Sprintf("p__t%d", i))
		}
	}()
	for i := 0; i < 100; i++ {
		_ = loads.snapshot()
	}
	<-done
	if got := len(loads.snapshot()); got != 100 {
		t.Errorf("want 100 recorded tools, got %d", got)
	}
}
