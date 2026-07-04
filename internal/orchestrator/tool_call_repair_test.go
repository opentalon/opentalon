package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// ----- repairable-result predicate -----

// Repairability is a typed pre-dispatch flag, never error-text matching: a
// dispatched tool whose error text merely LOOKS like a schema rejection
// (plugins routinely embed downstream errors emitted after partial
// mutation) must not be re-executed.
func TestIsRepairableToolResult(t *testing.T) {
	if !isRepairableToolResult(ToolResult{Error: "unknown argument(s) for inventory__update-item: user_id", ArgsInvalid: true}) {
		t.Error("args-invalid result must be repairable")
	}
	notRepairable := []ToolResult{
		{}, // no error
		{Error: "unknown argument(s) for inventory__update-item: user_id"}, // text without the typed flag
		{Error: "MCP error -32602: Invalid params"},
		{Error: "created 2 of 5 tasks; task 3 rejected: invalid arguments (unknown field)"},
		{Error: "item not found"},
		{Error: "context deadline exceeded"},
		{ArgsInvalid: true}, // flag without error text
	}
	for _, r := range notRepairable {
		if isRepairableToolResult(r) {
			t.Errorf("isRepairableToolResult(%+v) = true, want false", r)
		}
	}
}

// ----- form-only guard (the safety core) -----

// declAction builds an Action with the given declared parameter names, the
// guard's source of truth for which original keys may legitimately be
// renamed (only undeclared ones).
func declAction(names ...string) *Action {
	ps := make([]Parameter, len(names))
	for i, n := range names {
		ps[i] = Parameter{Name: n}
	}
	return &Action{Name: "update-item", Parameters: ps}
}

func TestRepairedArgsPreserveSubstance(t *testing.T) {
	cases := []struct {
		name     string
		action   *Action
		original map[string]string
		repaired map[string]string
		want     bool
	}{
		{
			name:     "key rename passes",
			action:   declAction("item_id", "responsible_user_id"),
			original: map[string]string{"user_id": "131349"},
			repaired: map[string]string{"responsible_user_id": "131349"},
			want:     true,
		},
		{
			name:     "nested JSON-string key rename passes",
			action:   declAction("tasks"),
			original: map[string]string{"tasks": `[{"name":"check pressure","qty":"5"}]`},
			repaired: map[string]string{"tasks": `[{"task_name":"check pressure","quantity":5}]`},
			want:     true,
		},
		{
			name:     "restructure of unknown keys into nesting passes",
			action:   declAction("batch"),
			original: map[string]string{"item_ids": "[7,9]", "user": "131349"},
			repaired: map[string]string{"batch": `{"ids":[9,7],"responsible_user_id":"131349"}`},
			want:     true,
		},
		{
			name:     "value change fails",
			action:   declAction("item_id"),
			original: map[string]string{"item_id": "42"},
			repaired: map[string]string{"item_id": "43"},
			want:     false,
		},
		{
			name:     "value addition fails",
			action:   declAction("item_id", "status"),
			original: map[string]string{"item_id": "42"},
			repaired: map[string]string{"item_id": "42", "status": "active"},
			want:     false,
		},
		{
			name:     "value removal fails",
			action:   declAction("item_id", "note"),
			original: map[string]string{"item_id": "42", "note": "urgent"},
			repaired: map[string]string{"item_id": "42"},
			want:     false,
		},
		{
			name:     "string 5 equals number 5",
			action:   declAction("quantity"),
			original: map[string]string{"qty": "5"},
			repaired: map[string]string{"quantity": "5"}, // corrector's typed 5 arrives as wire "5"
			want:     true,
		},
		{
			name:     "float spelling of an integer passes",
			action:   declAction("quantity"),
			original: map[string]string{"qty": "5.0"},
			repaired: map[string]string{"quantity": "5"},
			want:     true,
		},
		{
			name:     "exponent spelling of the same JSON number passes",
			action:   declAction("count"),
			original: map[string]string{"n": "1e3"},
			repaired: map[string]string{"count": "1000"},
			want:     true,
		},
		{
			name:     "duplicated value fails the multiset",
			action:   declAction("a", "b"),
			original: map[string]string{"a": "5", "b": "5"},
			repaired: map[string]string{"a": "5"},
			want:     false,
		},
		{
			name:     "bool string equals bool",
			action:   declAction("is_active"),
			original: map[string]string{"active": "true"},
			repaired: map[string]string{"is_active": "true"},
			want:     true,
		},
		{
			name:     "large id stays exact",
			action:   declAction("item_id"),
			original: map[string]string{"id": "123456789012345678"},
			repaired: map[string]string{"item_id": "123456789012345678"},
			want:     true,
		},
		{
			name:     "id beyond int64 stays exact",
			action:   declAction("serial_no"),
			original: map[string]string{"serial": "12345678901234567890"},
			repaired: map[string]string{"serial_no": "12345678901234567890"},
			want:     true,
		},
		{
			name:     "id beyond int64 must not collapse via float64",
			action:   declAction("serial"),
			original: map[string]string{"serial": "12345678901234567890"},
			repaired: map[string]string{"serial": "12345678901234567891"},
			want:     false,
		},
		{
			name:     "nested value change fails",
			action:   declAction("tasks"),
			original: map[string]string{"tasks": `[{"qty":5}]`},
			repaired: map[string]string{"tasks": `[{"qty":6}]`},
			want:     false,
		},
		// Cross-field re-pairings: the multiset alone would pass all of
		// these — the key-binding check must reject them.
		{
			name:     "same-key value swap fails",
			action:   declAction("item_id", "responsible_user_id"),
			original: map[string]string{"item_id": "42", "responsible_user_id": "7"},
			repaired: map[string]string{"item_id": "7", "responsible_user_id": "42"},
			want:     false,
		},
		{
			name:     "rename with crossed values fails on the kept key",
			action:   declAction("item_id", "responsible_user_id"),
			original: map[string]string{"item_id": "42", "user_id": "7"},
			repaired: map[string]string{"item_id": "7", "responsible_user_id": "42"},
			want:     false,
		},
		{
			name:     "undeclared same-name key must still keep its value",
			action:   declAction("to_location"),
			original: map[string]string{"from_location": "A7", "to_location": "B3"},
			repaired: map[string]string{"from_location": "B3", "to_location": "A7"},
			want:     false,
		},
		{
			name:     "declared key renamed away fails",
			action:   declAction("item_id", "responsible_user_id"),
			original: map[string]string{"item_id": "42"},
			repaired: map[string]string{"id": "42"},
			want:     false,
		},
		{
			name:     "same-key array reorder fails",
			action:   declAction("range"),
			original: map[string]string{"range": "[10,20]"},
			repaired: map[string]string{"range": "[20,10]"},
			want:     false,
		},
		{
			name:     "value migrating into a kept key's array fails",
			action:   declAction("item_ids", "quantity"),
			original: map[string]string{"item_ids": "[7]", "quantity": "2"},
			repaired: map[string]string{"item_ids": "[2,7]"},
			want:     false,
		},
		// Empty containers are values: adding or dropping one is a
		// substance change (on update-style APIs an empty array means
		// "clear this collection").
		{
			name:     "added empty array fails",
			action:   declAction("item_id", "name", "assignee_ids"),
			original: map[string]string{"item_id": "5", "name": "drill"},
			repaired: map[string]string{"item_id": "5", "name": "drill", "assignee_ids": "[]"},
			want:     false,
		},
		{
			name:     "dropped empty object fails",
			action:   declAction("item_id", "filters"),
			original: map[string]string{"item_id": "5", "filters": "{}"},
			repaired: map[string]string{"item_id": "5"},
			want:     false,
		},
		// String identifiers that merely look numeric: JSON cannot spell
		// them as numbers, so a numeric respelling IS a value change.
		{
			name:     "leading-zero identifier rewrite fails",
			action:   declAction("barcode"),
			original: map[string]string{"barcode": "007"},
			repaired: map[string]string{"barcode": "7"},
			want:     false,
		},
		{
			name:     "leading-zero identifier kept verbatim passes",
			action:   declAction("barcode"),
			original: map[string]string{"code": "007"},
			repaired: map[string]string{"barcode": "007"},
			want:     true,
		},
		{
			name:     "plus-prefixed phone number rewrite fails",
			action:   declAction("phone"),
			original: map[string]string{"phone": "+4915112345678"},
			repaired: map[string]string{"phone": "4915112345678"},
			want:     false,
		},
		// Documented residual: when BOTH original keys are undeclared and
		// renamed away, their values pool into one multiset — a crossed
		// re-pairing among renamed keys is mechanically undetectable and
		// remains the corrector prompt's responsibility.
		{
			name:     "crossed values among renamed unknown keys pass (residual trust)",
			action:   declAction("source", "target"),
			original: map[string]string{"from": "5", "to": "7"},
			repaired: map[string]string{"source": "7", "target": "5"},
			want:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repairedArgsPreserveSubstance(tc.action, tc.original, tc.repaired); got != tc.want {
				t.Errorf("repairedArgsPreserveSubstance(%v, %v) = %v, want %v", tc.original, tc.repaired, got, tc.want)
			}
		})
	}
}

// ----- scaffolding -----

// recordingExecutor succeeds on every call and records what it ran with, so
// tests can assert that a rejected repair never reaches execution and a
// successful repair runs with the corrected args.
type recordingExecutor struct {
	mu    sync.Mutex
	calls []ToolCall
}

func (e *recordingExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call)
	return ToolResult{CallID: call.ID, Content: "ok"}
}

func (e *recordingExecutor) snapshot() []ToolCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ToolCall, len(e.calls))
	copy(out, e.calls)
	return out
}

// setupRepairOrchestrator registers one write action with declared parameters
// (item_id, responsible_user_id) so executeCall's rejectUnknownArgs produces
// the repairable argument-name rejection the repair phase targets.
func setupRepairOrchestrator(llm LLMClient, parser ToolCallParser, sink emit.Sink, exec PluginExecutor, opts OrchestratorOpts) (*Orchestrator, string) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:        "inventory",
		Description: "Inventory integration",
		Actions: []Action{{
			Name:        "update-item",
			Description: "Update one item",
			Parameters: []Parameter{
				{Name: "item_id", Description: "Item id", Required: true},
				{Name: "responsible_user_id", Description: "Responsible user id"},
			},
		}},
	}, exec)
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("test-session", "", "")
	opts.EventSink = sink
	orch := NewWithRules(llm, parser, registry, memory, sessions, opts)
	return orch, "test-session"
}

// misnamedArgParser returns the update-item call with a misnamed argument on
// the first tool-call-marker response and nothing afterwards.
func misnamedArgParser(args map[string]string) *fakeParser {
	return &fakeParser{parseFn: func(response string) []ToolCall {
		if !strings.Contains(response, "[tool_call]") {
			return nil
		}
		return []ToolCall{{ID: "tc-1", Plugin: "inventory", Action: "update-item", Args: args}}
	}}
}

// ----- Part 1: pre-confirmation argument validation -----

// A privileged call with unknown argument names must NOT cost the user an
// approval: the gate skips, executeCall rejects with the usual
// tool_call_args_invalid (exactly once — no double emit), and the error flows
// back so the model corrects silently.
func TestConfirmation_InvalidArgsSkipGate(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		"I corrected myself and answered.",
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})
	_ = orch.registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{})

	result, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42")
	if err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	if n := countEventType(evs, events.TypeConfirmationRequested); n != 0 {
		t.Errorf("confirmation_requested count = %d, want 0 — invalid args must never raise a confirmation", n)
	}
	if n := countEventType(evs, events.TypeToolCallArgsInvalid); n != 1 {
		t.Errorf("tool_call_args_invalid count = %d, want exactly 1 (no double emit)", n)
	}
	if len(exec.snapshot()) != 0 {
		t.Error("the invalid call must not reach the executor")
	}
	if result.Response != "I corrected myself and answered." {
		t.Errorf("Response = %q — the validation error must flow back into the loop", result.Response)
	}
}

// The valid-args path is unchanged: the gate still raises a confirmation.
func TestConfirmation_ValidArgsStillRaiseGate(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		"Shall I update item 42?", // confirmation narration
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "responsible_user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})
	_ = orch.registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{})

	result, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42")
	if err != nil {
		t.Fatal(err)
	}
	if n := countEventType(sink.snapshot(), events.TypeConfirmationRequested); n != 1 {
		t.Errorf("confirmation_requested count = %d, want 1", n)
	}
	if result.Metadata["type"] != "confirmation" {
		t.Errorf("Metadata type = %q, want confirmation", result.Metadata["type"])
	}
	if len(exec.snapshot()) != 0 {
		t.Error("the call must pause at the gate, not execute")
	}
}

// ----- Part 2: repair loop -----

// Happy path (no confirmation gate configured): the LLM misnames a
// parameter, executeCall rejects, the corrector renames it, the
// re-execution succeeds — tool_call_repaired fires and the corrected call
// is what the caller records.
func TestRepair_AgentLoop_HappyPath(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		`{"repaired_args": {"item_id": "42", "responsible_user_id": "131349"}}`, // corrector
		"Done — user 131349 is now responsible.",
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		Repair: RepairConfig{Enabled: true},
	})

	result, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42")
	if err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	if n := countEventType(evs, events.TypeToolCallRepairInvoked); n != 1 {
		t.Errorf("tool_call_repair_invoked count = %d, want 1", n)
	}
	if n := countEventType(evs, events.TypeToolCallRepaired); n != 1 {
		t.Errorf("tool_call_repaired count = %d, want 1", n)
	}
	calls := exec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d, want 1 (only the corrected call executes)", len(calls))
	}
	if calls[0].Args["responsible_user_id"] != "131349" || calls[0].Args["item_id"] != "42" {
		t.Errorf("executed args = %v, want the corrected args", calls[0].Args)
	}
	if calls[0].ConfirmationBypass {
		t.Error("agent-loop repair carries no user approval — the corrected call must not claim ConfirmationBypass")
	}
	if len(result.Results) != 1 || result.Results[0].Error != "" {
		t.Fatalf("recorded result = %+v, want the repaired success", result.Results)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Args["responsible_user_id"] != "131349" {
		t.Errorf("recorded call args = %v, want the corrected args", result.ToolCalls)
	}
	if result.Response != "Done — user 131349 is now responsible." {
		t.Errorf("Response = %q", result.Response)
	}
}

// Consent boundary regression: a privileged write whose unknown argument
// names skipped the confirmation gate must NOT be repaired on the agent
// loop — no approval exists. The validation error flows back to the model,
// and its corrected re-plan raises the confirmation the user never granted.
// Without the approval check in maybeRepairToolCall this exact setup
// executed the write with zero confirmations.
func TestRepair_AgentLoop_UnapprovedWriteFallsBackToConfirmation(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call misnamed] update the item",
		"[tool_call corrected] update the item",
		"Shall I set user 131349 as responsible for item 42?", // confirmation narration
	}}
	parser := &fakeParser{parseFn: func(response string) []ToolCall {
		switch {
		case strings.Contains(response, "[tool_call misnamed]"):
			return []ToolCall{{ID: "tc-1", Plugin: "inventory", Action: "update-item",
				Args: map[string]string{"item_id": "42", "user_id": "131349"}}}
		case strings.Contains(response, "[tool_call corrected]"):
			return []ToolCall{{ID: "tc-2", Plugin: "inventory", Action: "update-item",
				Args: map[string]string{"item_id": "42", "responsible_user_id": "131349"}}}
		}
		return nil
	}}
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
		Repair:             RepairConfig{Enabled: true},
	})
	_ = orch.registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{})

	result, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42")
	if err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	if n := countEventType(evs, events.TypeToolCallRepairInvoked); n != 0 {
		t.Errorf("tool_call_repair_invoked count = %d, want 0 — no approval, no corrector", n)
	}
	if n := countEventType(evs, events.TypeToolCallRepaired); n != 0 {
		t.Errorf("tool_call_repaired count = %d, want 0", n)
	}
	if n := countEventType(evs, events.TypeConfirmationRequested); n != 1 {
		t.Errorf("confirmation_requested count = %d, want 1 — the corrected re-plan must re-enter the gate", n)
	}
	if len(exec.snapshot()) != 0 {
		t.Fatalf("executor calls = %+v, want none — the write must pause at the gate", exec.snapshot())
	}
	if result.Metadata["type"] != "confirmation" {
		t.Errorf("Metadata type = %q, want confirmation", result.Metadata["type"])
	}
}

// Budget exhaustion: the corrector keeps producing renames that still fail
// validation; after MaxAttempts the ORIGINAL error falls back into the
// normal flow and the tool never executes.
func TestRepair_BudgetExhausted(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		`{"repaired_args": {"item_id": "42", "still_wrong": "131349"}}`, // corrector attempt 1
		`{"repaired_args": {"item_id": "42", "also_wrong": "131349"}}`,  // corrector attempt 2
		"I could not run the update.",
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		Repair: RepairConfig{Enabled: true, MaxAttempts: 2},
	})

	result, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42")
	if err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	if n := countEventType(evs, events.TypeToolCallRepairInvoked); n != 2 {
		t.Errorf("tool_call_repair_invoked count = %d, want 2 (the budget)", n)
	}
	if n := countEventType(evs, events.TypeToolCallRepaired); n != 0 {
		t.Errorf("tool_call_repaired count = %d, want 0", n)
	}
	if len(exec.snapshot()) != 0 {
		t.Error("no repair attempt passed validation — the executor must never run")
	}
	// The ORIGINAL error is what falls back into history, not attempt 2's.
	if len(result.Results) != 1 || !strings.Contains(result.Results[0].Error, "user_id") {
		t.Errorf("recorded error = %+v, want the original unknown-args error", result.Results)
	}
}

// Corrector abort: repair steps aside after one invocation and the normal
// error flow continues.
func TestRepair_CorrectorAbort(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		`{"abort": true, "reason": "the required item_id is missing from the failed arguments"}`,
		"I need the item id to proceed.",
	}}
	parser := misnamedArgParser(map[string]string{"user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		Repair: RepairConfig{Enabled: true},
	})

	result, err := orch.Run(context.Background(), sessID, "set user 131349 as responsible")
	if err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	if n := countEventType(evs, events.TypeToolCallRepairInvoked); n != 1 {
		t.Errorf("tool_call_repair_invoked count = %d, want 1", n)
	}
	if n := countEventType(evs, events.TypeToolCallRepaired); n != 0 {
		t.Errorf("tool_call_repaired count = %d, want 0", n)
	}
	if len(exec.snapshot()) != 0 {
		t.Error("abort must not re-execute anything")
	}
	if len(result.Results) != 1 || result.Results[0].Error == "" {
		t.Errorf("recorded result = %+v, want the original error", result.Results)
	}
}

// Corrector failure: an unparseable corrector reply (prose instead of the
// JSON contract) aborts the repair after ONE invocation, the executor never
// runs, and the ORIGINAL error is what lands in history.
func TestRepair_CorrectorFailureFallsBack(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		"I am not sure I can fix this call for you.", // corrector emits prose — parse failure
		"I could not run the update.",
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		Repair: RepairConfig{Enabled: true},
	})

	result, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42")
	if err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	if n := countEventType(evs, events.TypeToolCallRepairInvoked); n != 1 {
		t.Errorf("tool_call_repair_invoked count = %d, want 1 (no retry on corrector failure)", n)
	}
	if n := countEventType(evs, events.TypeToolCallRepaired); n != 0 {
		t.Errorf("tool_call_repaired count = %d, want 0", n)
	}
	if len(exec.snapshot()) != 0 {
		t.Error("a failed corrector must not re-execute anything")
	}
	if llm.callCount != 3 {
		t.Errorf("LLM calls = %d, want 3 (turn, one corrector attempt, summary)", llm.callCount)
	}
	if len(result.Results) != 1 || !strings.Contains(result.Results[0].Error, "user_id") {
		t.Errorf("recorded error = %+v, want the original unknown-args error", result.Results)
	}
}

// The mechanical guard: a corrector that changes a VALUE is rejected even
// though its JSON parsed fine — the call is never re-executed.
func TestRepair_GuardRejectsValueChange(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		`{"repaired_args": {"item_id": "42", "responsible_user_id": "999"}}`, // 131349 → 999: substance change
		"I could not run the update.",
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		Repair: RepairConfig{Enabled: true},
	})

	if _, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42"); err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	if n := countEventType(evs, events.TypeToolCallRepaired); n != 0 {
		t.Errorf("tool_call_repaired count = %d, want 0", n)
	}
	if len(exec.snapshot()) != 0 {
		t.Error("a guard-rejected repair must never reach the executor")
	}
}

// The mechanical guard: a corrector that swaps values between the two kept
// keys is rejected — the value multiset is identical, but the substance
// (which item, which user) is inverted.
func TestRepair_GuardRejectsValueSwap(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		`{"repaired_args": {"item_id": "131349", "responsible_user_id": "42"}}`, // crossed
		"I could not run the update.",
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		Repair: RepairConfig{Enabled: true},
	})

	if _, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42"); err != nil {
		t.Fatal(err)
	}
	if n := countEventType(sink.snapshot(), events.TypeToolCallRepaired); n != 0 {
		t.Errorf("tool_call_repaired count = %d, want 0", n)
	}
	if len(exec.snapshot()) != 0 {
		t.Error("a swap-rejected repair must never reach the executor")
	}
}

// Disabled: with repair off the corrector never fires and the error flows
// back exactly as before the feature existed.
func TestRepair_DisabledIsNoOp(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		"I corrected myself and answered.",
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{})

	result, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42")
	if err != nil {
		t.Fatal(err)
	}
	if llm.callCount != 2 {
		t.Errorf("LLM calls = %d, want 2 (no corrector call)", llm.callCount)
	}
	evs := sink.snapshot()
	if countEventType(evs, events.TypeToolCallRepairInvoked)+countEventType(evs, events.TypeToolCallRepaired) != 0 {
		t.Error("repair events must not fire when disabled")
	}
	if result.Response != "I corrected myself and answered." {
		t.Errorf("Response = %q", result.Response)
	}
}

// Non-repairable errors (possible partial execution) never enter the repair
// loop even when repair is enabled.
type businessErrorExecutor struct{}

func (businessErrorExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	return ToolResult{CallID: call.ID, Error: "item 42 not found"}
}

func TestRepair_NonRepairableErrorSkipsCorrector(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		"That item does not exist.",
	}}
	// Valid arg names so the call reaches the executor, which fails
	// with a business error.
	parser := misnamedArgParser(map[string]string{"item_id": "42"})
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, businessErrorExecutor{}, OrchestratorOpts{
		Repair: RepairConfig{Enabled: true},
	})

	if _, err := orch.Run(context.Background(), sessID, "update item 42"); err != nil {
		t.Fatal(err)
	}
	if n := countEventType(sink.snapshot(), events.TypeToolCallRepairInvoked); n != 0 {
		t.Errorf("tool_call_repair_invoked count = %d, want 0 for a non-repairable error", n)
	}
	if llm.callCount != 2 {
		t.Errorf("LLM calls = %d, want 2 (no corrector call)", llm.callCount)
	}
}

// schemaLookalikeErrorExecutor returns a dispatched-tool error whose TEXT
// resembles a schema rejection — the classic partial-execution trap ("2 of
// 5 created" followed by a validation phrase from a downstream system).
type schemaLookalikeErrorExecutor struct{}

func (schemaLookalikeErrorExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	return ToolResult{CallID: call.ID, Error: "created 2 of 5 tasks; task 3 rejected: invalid arguments (unknown field)"}
}

// Repairability is the typed pre-dispatch flag, not error-text matching: an
// error that LOOKS like a schema rejection but came from a dispatched tool
// (which may have partially mutated) must never be re-executed.
func TestRepair_DispatchedSchemaLookalikeErrorIsNotRepaired(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		"Two of the five tasks were created; the rest failed.",
	}}
	// Valid arg names so the call reaches the executor.
	parser := misnamedArgParser(map[string]string{"item_id": "42"})
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, schemaLookalikeErrorExecutor{}, OrchestratorOpts{
		Repair: RepairConfig{Enabled: true},
	})

	if _, err := orch.Run(context.Background(), sessID, "update item 42"); err != nil {
		t.Fatal(err)
	}
	if n := countEventType(sink.snapshot(), events.TypeToolCallRepairInvoked); n != 0 {
		t.Errorf("tool_call_repair_invoked count = %d, want 0 — dispatched errors are never repairable", n)
	}
	if llm.callCount != 2 {
		t.Errorf("LLM calls = %d, want 2 (no corrector call)", llm.callCount)
	}
}

// recordingErrorExecutor records calls and fails each with a business error,
// so tests can drive "corrected call was dispatched and failed".
type recordingErrorExecutor struct {
	mu    sync.Mutex
	calls []ToolCall
}

func (e *recordingErrorExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call)
	return ToolResult{CallID: call.ID, Error: "item 42 not found"}
}

func (e *recordingErrorExecutor) snapshot() []ToolCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ToolCall, len(e.calls))
	copy(out, e.calls)
	return out
}

// Re-execution fails with a non-repairable (dispatched) error: the loop
// stops immediately — no second corrector call inside the same approval —
// and history records the CORRECTED call with ITS real error, because that
// is the attempt that actually reached the tool. Masking it behind the
// original shape error would let the model re-plan a write that already
// ran.
func TestRepair_ReexecutionFailsNonRepairably(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		`{"repaired_args": {"item_id": "42", "responsible_user_id": "131349"}}`, // corrector
		"That item does not exist.",
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
	exec := &recordingErrorExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		Repair: RepairConfig{Enabled: true, MaxAttempts: 2},
	})

	result, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42")
	if err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	if n := countEventType(evs, events.TypeToolCallRepairInvoked); n != 1 {
		t.Errorf("tool_call_repair_invoked count = %d, want 1 (no second corrector after a dispatched failure)", n)
	}
	if llm.callCount != 3 {
		t.Errorf("LLM calls = %d, want 3 (turn, one corrector, summary)", llm.callCount)
	}
	calls := exec.snapshot()
	if len(calls) != 1 || calls[0].Args["responsible_user_id"] != "131349" {
		t.Fatalf("executor calls = %+v, want exactly the corrected call", calls)
	}
	// tool_call_repaired carries the re-execution outcome: status "error".
	repairedCount, errorStatus := 0, false
	for _, e := range evs {
		if e.EventType != events.TypeToolCallRepaired {
			continue
		}
		repairedCount++
		var p events.ToolCallRepairedPayload
		if jerr := json.Unmarshal(e.Payload, &p); jerr == nil && p.Status == "error" {
			errorStatus = true
		}
	}
	if repairedCount != 1 || !errorStatus {
		t.Errorf("tool_call_repaired count = %d (error status: %v), want 1 with status error", repairedCount, errorStatus)
	}
	// History carries the corrected call + the real dispatched error.
	if len(result.Results) != 1 || result.Results[0].Error != "item 42 not found" {
		t.Errorf("recorded result = %+v, want the re-execution's business error", result.Results)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Args["responsible_user_id"] != "131349" {
		t.Errorf("recorded call = %+v, want the corrected call that actually ran", result.ToolCalls)
	}
}

// Approval path: an approved call that fails validation is repaired inside
// the SAME approval — no fresh confirmation_requested — and the corrector
// sees the confirmation prompt the user approved.
func TestRepair_ApprovedCall_RepairedWithoutReconfirmation(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &capturingLLM{responses: []string{
		`{"decision": "approve"}`, // confirmation classifier
		`{"repaired_args": {"item_id": "7", "responsible_user_id": "131349"}}`, // corrector
		"Done — user 131349 is now responsible for item 7.",
	}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		ConfirmationClassifierEnabled: true,
		ConfirmationPlugin:            "conf",
		ConfirmationAction:            "check",
		Repair:                        RepairConfig{Enabled: true},
	})

	approvedPrompt := "Set user 131349 as responsible for item 7 — proceed?"
	orch.pendingMu.Lock()
	orch.pendingToolCalls[sessID] = &ToolCall{
		ID: "pending-1", Plugin: "inventory", Action: "update-item",
		Args:    map[string]string{"item_id": "7", "user_id": "131349"},
		FromLLM: true,
	}
	orch.pendingConfirmationPrompts[sessID] = approvedPrompt
	orch.pendingMu.Unlock()

	result, err := orch.Run(context.Background(), sessID, "yes, go ahead")
	if err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	if n := countEventType(evs, events.TypeConfirmationRequested); n != 0 {
		t.Errorf("confirmation_requested count = %d, want 0 — the repair must reuse the approval", n)
	}
	if n := countEventType(evs, events.TypeToolCallRepaired); n != 1 {
		t.Errorf("tool_call_repaired count = %d, want 1", n)
	}
	calls := exec.snapshot()
	if len(calls) != 1 || calls[0].Args["responsible_user_id"] != "131349" {
		t.Fatalf("executor calls = %+v, want exactly the corrected call", calls)
	}
	if !calls[0].ConfirmationBypass {
		t.Error("approved-path repair must carry ConfirmationBypass — a real approval exists")
	}
	// The corrector's context must include the approved confirmation prompt
	// (requests[1] is the corrector call: classifier, corrector, summary).
	if len(llm.requests) < 3 {
		t.Fatalf("LLM requests = %d, want 3", len(llm.requests))
	}
	var correctorInput string
	for _, m := range llm.requests[1].Messages {
		correctorInput += m.Content + "\n"
	}
	if !strings.Contains(correctorInput, approvedPrompt) {
		t.Errorf("corrector input must carry the approved prompt, got: %q", correctorInput)
	}
	if result.Response != "Done — user 131349 is now responsible for item 7." {
		t.Errorf("Response = %q", result.Response)
	}
	// The corrected args (not the failed ones) must be what landed in
	// session history for the summarizing LLM round.
	var summaryInput string
	for _, m := range llm.requests[2].Messages {
		summaryInput += m.Content + "\n"
	}
	if !strings.Contains(summaryInput, "responsible_user_id") {
		t.Errorf("session history must carry the corrected call, got: %q", summaryInput)
	}
}

// A repaired success resets the consecutive-error counters exactly like a
// normal success — the loop-cap nudge must NOT fire after a repaired call —
// but the corrector invocation is counted in the SEPARATE repair counter,
// so a repair loop can never mask a tool whose schema chronically misleads
// the planner.
func TestRepair_SuccessResetsToolErrorCounterButCountsRepair(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] update the item",
		`{"repaired_args": {"item_id": "42", "responsible_user_id": "131349"}}`,
		"Done.",
	}}
	parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
	exec := &recordingExecutor{}
	orch, sessID := setupRepairOrchestrator(llm, parser, sink, exec, OrchestratorOpts{
		Repair:            RepairConfig{Enabled: true},
		ToolErrorHandling: ToolErrorHandlingConfig{LoopCapPerTurn: 1},
	})

	if _, err := orch.Run(context.Background(), sessID, "set user 131349 on item 42"); err != nil {
		t.Fatal(err)
	}
	// recordToolOutcome saw the repaired SUCCESS, so the per-tool error
	// counter is clean — a fresh error would be the first, not a loop-cap
	// trip. The repair counter, by contrast, keeps the corrector invocation.
	st := orch.toolErrorTracker.stateFor(sessID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if n := st.sessionErrors["inventory__update-item"]; n != 0 {
		t.Errorf("consecutive-error count after repaired success = %d, want 0", n)
	}
	if n := st.sessionRepairs["inventory__update-item"]; n != 1 {
		t.Errorf("repair-attempt count after repaired success = %d, want 1 (counted separately)", n)
	}
}
