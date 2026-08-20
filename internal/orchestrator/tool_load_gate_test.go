package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// gateOrch builds an orchestrator with one always-include tool ("p__always")
// and one catalog-only tool ("p__catalog") plus a "p__foo-bar" fixture for the
// name-normalization case, sharing one counting executor so a test can assert
// whether the called tool actually ran.
func gateOrch(t *testing.T) (*Orchestrator, *countingExecutor) {
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
	sessions.Create("s1", "", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{})
	return orch, exec
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

// A call to a tool that was NOT in the sent native array is refused — the tool
// never runs, and the error names both the problem and the fix.
func TestExecuteCall_RefusesToolNotInSentArray(t *testing.T) {
	orch, exec := gateOrch(t)
	ctx := sentCtx("p__always") // catalog was NOT sent

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})

	if res.Error == "" {
		t.Fatal("expected a refusal for a tool not in the sent array")
	}
	if !strings.Contains(res.Error, "not loaded") || !strings.Contains(res.Error, toolFQN(metaPluginName, metaLoadTools)) {
		t.Errorf("refusal must name the problem and point at load_tools, got %q", res.Error)
	}
	if exec.count != 0 {
		t.Errorf("a tool not in the sent array must NOT execute, ran %d time(s)", exec.count)
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
// array (it was loaded by an earlier call in the SAME response, or demoted /
// evicted after the array was built) is refused. This is what makes the
// invariant hold in the same-turn load+call case and keeps a mid-turn demotion
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
	sessions.Create("s1", "", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions,
		OrchestratorOpts{InjectionStateStore: store})

	// p__catalog IS promoted in the store, but NOT in the sent array this round.
	ctx := sentCtx() // empty sent set

	res := orch.executeCall(ctx, ToolCall{ID: "c1", Plugin: "p", Action: "catalog", FromLLM: true})
	if res.Error == "" {
		t.Fatal("a store-promoted tool absent from the sent array must still be refused")
	}
	if exec.count != 0 {
		t.Errorf("tool absent from the sent array must NOT execute, ran %d time(s)", exec.count)
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

// An unloaded write tool must never reach a confirmation prompt: the model has
// to load the tool and retry, not be asked to approve a call it guessed from a
// one-line summary. The contrast case (a sent write tool DOES prompt) proves the
// skip is the load-gate, not a mis-wired confirmation plugin.
func TestMaybeRequireConfirmation_ToolNotInSentArrayNeverPrompts(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "confirmation gate fixtures",
		Actions: []Action{
			{Name: "sent_write", Description: "A write tool that was sent."},
			{Name: "catalog_write", Description: "A write tool not yet loaded."},
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
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})
	ctx := sentCtx("p__sent_write") // only sent_write is in the array

	// Not in the sent array → confirmation SKIPPED (falls through to executeCall's refusal).
	rr, raised := orch.maybeRequireConfirmation(ctx, sessions, "s1",
		ToolCall{ID: "c1", Plugin: "p", Action: "catalog_write", FromLLM: true}, "do it", nil, false)
	if raised || rr != nil {
		t.Fatalf("a tool not in the sent array must not raise a confirmation, got raised=%v rr=%v", raised, rr)
	}

	// In the sent array → confirmation IS raised.
	rr2, raised2 := orch.maybeRequireConfirmation(ctx, sessions, "s1",
		ToolCall{ID: "c2", Plugin: "p", Action: "sent_write", FromLLM: true}, "do it", nil, false)
	if !raised2 || rr2 == nil {
		t.Fatalf("a sent write tool must raise a confirmation, got raised=%v rr=%v", raised2, rr2)
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
	sessions.Create("s1", "", "", "")
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
		rr, raised := orch.maybeRequireConfirmation(ctx, sessions, "s1", call, "do it", nil, false)
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
	sessions.Create("s1", "", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})
	ctx := sentCtx("p__write_a", "p__write_b")

	gated := ToolCall{ID: "c-a", Plugin: "p", Action: "write_a", FromLLM: true, Args: map[string]string{"name": "A"}}
	dropped := ToolCall{ID: "c-b", Plugin: "p", Action: "write_b", FromLLM: true, Args: map[string]string{"name": "B"}}
	rr, raised := orch.maybeRequireConfirmation(ctx, sessions, "s1", gated, "create both", []ToolCall{dropped}, true)
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
		gated, "create both", []ToolCall{dropped}, false); !raised {
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
		readOnly, "just look", []ToolCall{dropped}, true)
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
	sessions.Create("s1", "", "", "")
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
// call-site slice and the per-response native flag).
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
	sessions.Create("s1", "", "", "")
	llm := &scriptedNativeLLM{rounds: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{
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
	if pending, _, _ := loadPendingToolCall(sessions, "s1"); pending == nil || pending.ID != "c-a" {
		t.Errorf("pending call must be the gated write_a (c-a), got %+v", pending)
	}
}

// scriptedNativeLLM returns a pre-scripted CompletionResponse per round so a
// test can drive a full multi-round agent loop; it advertises FeatureTools so
// the orchestrator takes the native path.
type scriptedNativeLLM struct {
	rounds []provider.CompletionResponse
	calls  int
}

func (l *scriptedNativeLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
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

// End-to-end through Run: the model calls a catalog tool it never loaded → the
// gate refuses it in the real agent loop → the model reads the refusal, calls
// _meta__load_tools → the rebuilt array now carries the tool → the model calls
// it and it executes. Proves the refusal reaches history and the model recovers
// on a subsequent round, and that the tool runs exactly once (on the retry, not
// the blind first call).
func TestRun_UnloadedToolRefusedThenLoadedAndRun(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Actions: []Action{{Name: "catalog", Description: "Catalog-only tool."}},
	}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "", "")
	llm := &scriptedNativeLLM{rounds: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{{ID: "r1", Name: "p__catalog", Arguments: map[string]string{}}}},                             // blind call → refused
		{ToolCalls: []provider.ToolCall{{ID: "r2", Name: "_meta__load_tools", Arguments: map[string]string{"names": "p__catalog"}}}}, // load it
		{ToolCalls: []provider.ToolCall{{ID: "r3", Name: "p__catalog", Arguments: map[string]string{}}}},                             // retry → runs
		{Content: "all done"},
	}}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, OrchestratorOpts{InjectionStateStore: &fakeInjectionStateStore{}})

	res, err := orch.Run(actor.WithSessionID(context.Background(), "s1"), "s1", "call the catalog tool")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exec.count != 1 {
		t.Errorf("catalog tool must run exactly once (the retry), ran %d time(s)", exec.count)
	}
	var refusals int
	for _, r := range res.Results {
		if strings.Contains(r.Error, "not loaded") {
			refusals++
		}
	}
	if refusals != 1 {
		t.Errorf("expected exactly one 'not loaded' refusal in history, got %d (results=%d)", refusals, len(res.Results))
	}
}
