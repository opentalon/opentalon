package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
	"github.com/opentalon/opentalon/pkg/plugin/contextargs"
)

// checkingExecutor runs and records every call like recordingExecutor, and
// answers a check with check, recording each call it was sent and whether the
// check ran under a deadline.
type checkingExecutor struct {
	recordingExecutor
	check     func(context.Context, ToolCall) (CheckResult, error)
	checks    []ToolCall
	deadlines []bool
}

func (e *checkingExecutor) Check(ctx context.Context, call ToolCall) (CheckResult, error) {
	_, hasDeadline := ctx.Deadline()
	e.mu.Lock()
	e.checks = append(e.checks, call)
	e.deadlines = append(e.deadlines, hasDeadline)
	e.mu.Unlock()
	return e.check(ctx, call)
}

func (e *checkingExecutor) checked() []ToolCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.checks)
}

func verdictOf(r CheckResult, err error) func(context.Context, ToolCall) (CheckResult, error) {
	return func(context.Context, ToolCall) (CheckResult, error) { return r, err }
}

const takenMessage = "name A is already taken"

var refuseTaken = verdictOf(CheckResult{Verdict: CheckRefused, Kind: "already_exists", Message: takenMessage}, nil)

// confirmOnly is a confirmation plugin that asks the user before a call to
// the one action it names, and before no other.
type confirmOnly string

func (a confirmOnly) Execute(_ context.Context, call ToolCall) ToolResult {
	need := strings.Contains(call.Args["steps"], `"action":"`+string(a)+`"`)
	return ToolResult{CallID: call.ID, Content: fmt.Sprintf(`{"requires_confirmation":%t,"confirm_before_step":0}`, need)}
}

// checkGateParser parses each line of a text-mode response: "LOAD" loads
// p__tag and p__plain, "CALL <plugin> <action> [<name>]" calls the action,
// with the id c<n> for the n-th line. A response without either is the
// model's answer.
func checkGateParser() *fakeParser {
	return &fakeParser{parseFn: func(response string) []ToolCall {
		var calls []ToolCall
		for i, line := range strings.Split(response, "\n") {
			f := strings.Fields(line)
			switch {
			case len(f) == 1 && f[0] == "LOAD":
				calls = append(calls, ToolCall{ID: fmt.Sprintf("c%d", i+1), Plugin: metaPluginName, Action: metaLoadTools,
					Args: map[string]string{"names": "p__tag,p__plain"}})
			case len(f) >= 3 && f[0] == "CALL":
				c := ToolCall{ID: fmt.Sprintf("c%d", i+1), Plugin: f[1], Action: f[2]}
				if len(f) > 3 {
					c.Args = map[string]string{"name": f[3]}
				}
				calls = append(calls, c)
			}
		}
		return calls
	}}
}

// checkGateOrch builds an orchestrator over the plugin p, run by exec: tag
// declares a check and has the session id injected, plain is a write without
// a check declaration and look a read that declares one. The plugin locked
// has one action that declares a check, and the permission checker denies it.
// conf answers whether a call needs the user's approval (nil: every call
// does); always puts p's actions into the native tools array.
func checkGateOrch(t *testing.T, exec, conf PluginExecutor, always bool, llm LLMClient, opts OrchestratorOpts) (
	*Orchestrator, *state.SessionStore, *recordingEventSink,
) {
	t.Helper()
	if conf == nil {
		conf = confirmingExecutor{}
	}
	name := []Parameter{{Name: "name", Description: "The name."}}
	registry := NewToolRegistry()
	for _, reg := range []struct {
		cap  PluginCapability
		exec PluginExecutor
	}{
		{PluginCapability{Name: "p", Description: "check fixtures", Actions: []Action{
			{Name: "tag", Description: "Tag something.", Parameters: name, Check: true, AlwaysInclude: always,
				InjectContextArgs: []string{contextargs.SessionID}},
			{Name: "plain", Description: "A write without a check.", Parameters: name, AlwaysInclude: always},
			{Name: "look", Description: "A read.", ReadOnly: true, Check: true, AlwaysInclude: always},
		}}, exec},
		{PluginCapability{Name: "locked", Actions: []Action{{Name: "open", Description: "Open it.", Check: true}}}, exec},
		{PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, conf},
	} {
		if err := registry.Register(reg.cap, reg.exec); err != nil {
			t.Fatalf("register %s: %v", reg.cap.Name, err)
		}
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	sink := &recordingEventSink{}
	opts.EventSink = sink
	opts.ConfirmationPlugin, opts.ConfirmationAction = "conf", "check"
	opts.PermissionChecker = stubPermissionChecker{denied: "locked"}
	orch := NewWithRules(llm, checkGateParser(), registry, state.NewMemoryStore(""), sessions, opts)
	return orch, sessions, sink
}

// toolEventsOf renders the sink's tool and confirmation events in order
// (toolEventLine) and returns them with the events.
func toolEventsOf(t *testing.T, sink *recordingEventSink) ([]string, []emit.Event) {
	t.Helper()
	var lines []string
	var evs []emit.Event
	for _, e := range sink.snapshot() {
		switch e.EventType {
		case events.TypeToolCallExtracted, events.TypeToolCallResult, events.TypeToolCallArgsInvalid,
			events.TypeToolCallNotFound, events.TypeConfirmationRequested, events.TypeToolCallRepairInvoked:
			lines = append(lines, toolEventLine(t, e))
			evs = append(evs, e)
		}
	}
	return lines, evs
}

// exchangeRows returns the session's text-mode call and result rows.
func exchangeRows(t *testing.T, sessions *state.SessionStore) []string {
	t.Helper()
	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	var rows []string
	for _, m := range s.Messages {
		if strings.HasPrefix(m.Content, "[tool_call]") || strings.HasPrefix(m.Content, "[plugin_output]") {
			rows = append(rows, m.Content)
		}
	}
	return rows
}

// assertRefusedEvents checks the events of a model's call answered without a
// question: the extracted call with its arguments as issued, then one outcome
// parented to it, and nothing else.
func assertRefusedEvents(t *testing.T, sink *recordingEventSink, outcome string, issued map[string]string) {
	t.Helper()
	lines, evs := toolEventsOf(t, sink)
	if want := []string{"tool_call_extracted c1", outcome}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("tool events = %q, want %q", lines, want)
	}
	if evs[1].ParentID != evs[0].ID {
		t.Errorf("the outcome must be parented to the extracted event %q, got %q", evs[0].ID, evs[1].ParentID)
	}
	if got := findToolCallExtractedPayloads(t, evs[:1]); !reflect.DeepEqual(got[0].Arguments, issued) {
		t.Errorf("the extracted event must carry the arguments as issued %v, got %v", issued, got[0].Arguments)
	}
}

// In the agent loop: a passed check asks once and the approved call executes
// without a second check; a refused check answers the call with its refusal,
// without a question; every check that cannot be answered asks as without a
// check. The check gets the call its execution would get (the injected
// session id), under the plugin-call deadline.
func TestCheckGate_AgentLoopOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   func(context.Context, ToolCall) (CheckResult, error) // nil: the plugin does not check calls
		refused bool
	}{
		{"passed", verdictOf(CheckResult{Verdict: CheckPassed}, nil), false},
		{"refused", refuseTaken, true},
		{"refused without a message", verdictOf(CheckResult{Verdict: CheckRefused, Kind: "invalid_argument"}, nil), false},
		{"refused with a blank message", verdictOf(CheckResult{Verdict: CheckRefused, Message: " \n"}, nil), false},
		{"no verdict", verdictOf(CheckResult{}, nil), false},
		{"plugin does not implement the check", verdictOf(CheckResult{}, status.Error(codes.Unimplemented, "unknown method Check")), false},
		{"transport error", verdictOf(CheckResult{}, status.Error(codes.Unavailable, "connection refused")), false},
		{"deadline", func(ctx context.Context, _ ToolCall) (CheckResult, error) {
			select {
			case <-ctx.Done():
				return CheckResult{}, ctx.Err()
			case <-time.After(10 * time.Second):
				return CheckResult{}, errors.New("the check got no deadline")
			}
		}, false},
		{"executor without the check", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var exec interface {
				PluginExecutor
				snapshot() []ToolCall
			} = &recordingExecutor{}
			checker := &checkingExecutor{check: tc.check}
			if tc.check != nil {
				exec = checker
			}
			responses := []string{"CALL p tag A", "Shall I tag A?", "Tagged A."}
			if tc.refused {
				responses = []string{"CALL p tag A", "I cannot tag A: the name is taken."}
			}
			orch, sessions, sink := checkGateOrch(t, exec, nil, false, &fakeLLM{responses: responses}, OrchestratorOpts{})
			if tc.name == "deadline" {
				orch.guard.Timeout = 20 * time.Millisecond
			}
			issued := map[string]string{"name": "A"}
			admitted := map[string]string{"name": "A", contextargs.SessionID: "s1"}

			res, err := orch.Run(context.Background(), "s1", "tag A")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.check != nil {
				checks := checker.checked()
				if len(checks) != 1 || checks[0].Action != "tag" || !reflect.DeepEqual(checks[0].Args, admitted) {
					t.Fatalf("the check must get the admitted call once, got %+v", checks)
				}
				if !checker.deadlines[0] {
					t.Error("the check must run under the plugin-call deadline")
				}
			}
			pending, _, _, _ := loadPendingToolCall(sessions, "s1")

			if tc.refused {
				if res.Metadata["type"] == "confirmation" || res.Response != "I cannot tag A: the name is taken." {
					t.Errorf("a refused call must be answered by the model without a question, got %+v", res)
				}
				if pending != nil {
					t.Errorf("a refused call must not be pending, got %+v", pending)
				}
				if ran := exec.snapshot(); len(ran) != 0 {
					t.Errorf("a refused call must not be executed, ran %+v", ran)
				}
				want := []string{
					formatToolCallMessage(ToolCall{Plugin: "p", Action: "tag", Args: issued}),
					orch.guard.WrapContent(ToolResult{Error: takenMessage}),
				}
				if rows := exchangeRows(t, sessions); !reflect.DeepEqual(rows, want) {
					t.Errorf("the history must pair the call with its refusal, got %q, want %q", rows, want)
				}
				assertRefusedEvents(t, sink, "tool_call_result refused "+takenMessage, issued)
				return
			}

			if res.Metadata["type"] != "confirmation" || res.Response != "Shall I tag A?" {
				t.Fatalf("the user must be asked as without a check, got %+v", res)
			}
			if pending == nil || pending.ID != "c1" || !reflect.DeepEqual(pending.Args, issued) {
				t.Fatalf("the call as issued must be pending, got %+v", pending)
			}
			if lines, _ := toolEventsOf(t, sink); !reflect.DeepEqual(lines, []string{"confirmation_requested c1"}) {
				t.Errorf("a call waiting for approval has only its question, got %q", lines)
			}

			if _, err := orch.Run(actor.WithConfirmationDecision(context.Background(), "approve"), "s1", "yes"); err != nil {
				t.Fatalf("Run (approve): %v", err)
			}
			if ran := exec.snapshot(); len(ran) != 1 || !reflect.DeepEqual(ran[0].Args, admitted) {
				t.Errorf("the approved call must execute once with the injected arg, ran %+v", ran)
			}
			if tc.check != nil && len(checker.checked()) != 1 {
				t.Errorf("an approved call must not be checked again, checks %+v", checker.checked())
			}
		})
	}
}

// The planner's single step passes the same gate: a passed check and one
// that cannot be answered ask; a refused check seeds the history with the
// call and its refusal like a result, and the model answers. The step is not
// a model's call, so the refusal emits no tool event.
func TestCheckGate_SingleStepOutcomes(t *testing.T) {
	plan := `{"type":"pipeline","steps":[{"id":"1","name":"Tag","plugin":"p","action":"tag","args":{"name":"A"}}]}`
	for _, tc := range []struct {
		name    string
		check   func(context.Context, ToolCall) (CheckResult, error)
		refused bool
	}{
		{"passed", verdictOf(CheckResult{Verdict: CheckPassed}, nil), false},
		{"refused", refuseTaken, true},
		{"transport error", verdictOf(CheckResult{}, status.Error(codes.Unavailable, "connection refused")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &checkingExecutor{check: tc.check}
			responses := []string{plan, "Shall I tag A?"}
			if tc.refused {
				responses = []string{plan, "I cannot tag A: the name is taken."}
			}
			orch, sessions, sink := checkGateOrch(t, exec, nil, false, &fakeLLM{responses: responses},
				OrchestratorOpts{PipelineEnabled: true})

			res, err := orch.Run(context.Background(), "s1", "tag A")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if checks := exec.checked(); len(checks) != 1 {
				t.Fatalf("the step must be checked once, got %+v", checks)
			}
			if ran := exec.snapshot(); len(ran) != 0 {
				t.Errorf("the step must not be executed, ran %+v", ran)
			}
			pending, _, _, _ := loadPendingToolCall(sessions, "s1")
			lines, _ := toolEventsOf(t, sink)

			if tc.refused {
				if res.Metadata["type"] == "confirmation" || res.Response != "I cannot tag A: the name is taken." || pending != nil {
					t.Errorf("a refused step must be answered by the model without a question, got %+v, pending %+v", res, pending)
				}
				want := []string{
					formatToolCallMessage(ToolCall{Plugin: "p", Action: "tag", Args: map[string]string{"name": "A"}}),
					orch.guard.WrapContent(ToolResult{Error: takenMessage}),
				}
				if rows := exchangeRows(t, sessions); !reflect.DeepEqual(rows, want) {
					t.Errorf("the refusal must seed the history, got %q, want %q", rows, want)
				}
				if len(lines) != 0 {
					t.Errorf("a refused step emits no tool event, got %q", lines)
				}
				return
			}
			if res.Metadata["type"] != "confirmation" || pending == nil || pending.ID != "planner-p-tag" {
				t.Errorf("the user must be asked for the step, got %+v, pending %+v", res, pending)
			}
			if !reflect.DeepEqual(lines, []string{"confirmation_requested planner-p-tag"}) {
				t.Errorf("a step waiting for approval has only its question, got %q", lines)
			}
		})
	}
}

// A turn that ends while its call is checked ends there, at both sites: no
// question, no pending call, no history row and no event for the call.
func TestCheckGate_TurnCancelledDuringCheck(t *testing.T) {
	plan := `{"type":"pipeline","steps":[{"id":"1","name":"Tag","plugin":"p","action":"tag","args":{"name":"A"}}]}`
	for _, tc := range []struct {
		name     string
		pipeline bool
		response string
	}{
		{"agent loop", false, "CALL p tag A"},
		{"single step", true, plan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exec := &checkingExecutor{check: func(ctx context.Context, _ ToolCall) (CheckResult, error) {
				cancel()
				<-ctx.Done()
				return CheckResult{}, ctx.Err()
			}}
			orch, sessions, sink := checkGateOrch(t, exec, nil, false,
				&fakeLLM{responses: []string{tc.response, "Shall I tag A?"}}, OrchestratorOpts{PipelineEnabled: tc.pipeline})

			res, err := orch.Run(ctx, "s1", "tag A")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the turn must end as cancelled, got %+v, %v", res, err)
			}
			if pending, _, _, _ := loadPendingToolCall(sessions, "s1"); pending != nil {
				t.Errorf("no call may be pending, got %+v", pending)
			}
			if rows := exchangeRows(t, sessions); len(rows) != 0 {
				t.Errorf("no call may be recorded, got %q", rows)
			}
			if lines, _ := toolEventsOf(t, sink); len(lines) != 0 {
				t.Errorf("no event may be emitted for the call, got %q", lines)
			}
			if ran := exec.snapshot(); len(ran) != 0 {
				t.Errorf("the call must not be executed, ran %+v", ran)
			}
		})
	}
}

// A refused check is not repaired, an identical repeat of it is neither run
// nor checked again, it counts as a failed call within the turn but not
// toward the tool's demotion, and the plugin-call observer leaves it out.
// Native mode: there a result row does not count as a new turn.
func TestCheckGate_RefusedCheckMarks(t *testing.T) {
	exec := &checkingExecutor{check: refuseTaken}
	observer := &fakeObserver{}
	tagCall := func(id, name string) provider.CompletionResponse {
		return provider.CompletionResponse{ToolCalls: []provider.ToolCall{
			{ID: id, Name: "p__tag", Arguments: map[string]string{"name": name}},
		}}
	}
	llm := &scriptedNativeLLM{rounds: []provider.CompletionResponse{
		tagCall("n1", "A"), tagCall("n2", "A"), tagCall("n3", "B"), {Content: "Both names are taken."},
	}}
	orch, _, sink := checkGateOrch(t, exec, nil, true, llm, OrchestratorOpts{
		Repair:             RepairConfig{Enabled: true},
		ToolErrorHandling:  ToolErrorHandlingConfig{LoopCapPerTurn: 2},
		PluginCallObserver: observer,
	})

	res, err := orch.Run(context.Background(), "s1", "tag A, else B")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Response != "Both names are taken." || llm.calls != 4 {
		t.Fatalf("the model must answer after three call rounds, got %q after %d rounds", res.Response, llm.calls)
	}
	var names []string
	for _, c := range exec.checked() {
		names = append(names, c.Args["name"])
	}
	if !reflect.DeepEqual(names, []string{"A", "B"}) {
		t.Errorf("the identical repeat must not be checked again, checked %q", names)
	}
	if ran := exec.snapshot(); len(ran) != 0 {
		t.Errorf("a refused call must not be executed, ran %+v", ran)
	}
	sent := func(content string) bool {
		for _, req := range llm.requests {
			for _, m := range req.Messages {
				if strings.Contains(m.Content, content) {
					return true
				}
			}
		}
		return false
	}
	if !sent("You are repeating the same tool call") {
		t.Error("the identical repeat must get the repeat nudge")
	}
	if !sent("[system] Tool p__tag failed 2 times in this turn") {
		t.Error("two refused checks must count as two failed calls in the turn")
	}
	if n := countEventType(sink.snapshot(), events.TypeToolCallRepairInvoked); n != 0 {
		t.Errorf("a refused check must not be repaired, got %d repair(s)", n)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	for _, c := range observer.calls {
		if c.plugin == "p" && c.action == "tag" {
			t.Errorf("the plugin-call observer must leave a refused check out, got %+v", c)
		}
	}
}

// A refused check is the model's attempt at the write: after loading the
// write tool, a refusal followed by the model's question ends the turn without
// the completion guard's nudge, as does a refusal after an executed write.
func TestCheckGate_RefusedCheckLeavesTheCompletionGuardQuiet(t *testing.T) {
	question := "Which name should I use instead?"
	for _, tc := range []struct {
		name      string
		responses []string
		ran       int
	}{
		{"refused after loading the tool", []string{"LOAD", "CALL p tag A", question}, 0},
		{"refused after an executed write", []string{"LOAD", "CALL p plain X", "CALL p tag A", question}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &checkingExecutor{check: refuseTaken}
			llm := &capturingLLM{responses: tc.responses}
			orch, _, _ := checkGateOrch(t, exec, confirmOnly("tag"), false, llm, OrchestratorOpts{})

			res, err := orch.Run(context.Background(), "s1", "tag A")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Response != question || llm.callCount != len(tc.responses) {
				t.Errorf("the question must end the turn without a nudge round, got %q after %d rounds", res.Response, llm.callCount)
			}
			for _, req := range llm.requests {
				for _, m := range req.Messages {
					if m.Content == phantomCompletionNudge {
						t.Fatal("the completion guard must not nudge after a refused check")
					}
				}
			}
			if ran := exec.snapshot(); len(ran) != tc.ran || len(exec.checked()) != 1 {
				t.Errorf("want %d execution(s) and one check, ran %+v, checked %+v", tc.ran, ran, exec.checked())
			}
		})
	}
}

// A change request answers the pending call (amend); the corrected call the
// model issues next passes the gate again, and its refused check answers it:
// one question in all, nothing executed, nothing left pending.
func TestCheckGate_AmendedCallRefusedByItsCheck(t *testing.T) {
	exec := &checkingExecutor{check: func(ctx context.Context, call ToolCall) (CheckResult, error) {
		if call.Args["name"] == "B" {
			return CheckResult{Verdict: CheckRefused, Kind: "already_exists", Message: "name B is already taken"}, nil
		}
		return CheckResult{Verdict: CheckPassed}, nil
	}}
	llm := &fakeLLM{responses: []string{
		"CALL p tag A", "Shall I tag A?",
		`{"decision":"amend","requests_change":true,"reason":"use B"}`, "CALL p tag B", "B is taken too. Which name should I use?",
	}}
	orch, sessions, sink := checkGateOrch(t, exec, nil, false, llm, OrchestratorOpts{ConfirmationClassifierEnabled: true})

	if first, err := orch.Run(context.Background(), "s1", "tag A"); err != nil || first.Metadata["type"] != "confirmation" {
		t.Fatalf("the first call must be confirmed, got %+v, %v", first, err)
	}
	res, err := orch.Run(context.Background(), "s1", "no, call it B")
	if err != nil {
		t.Fatalf("Run (amend): %v", err)
	}
	if res.Metadata["type"] == "confirmation" || res.Response != "B is taken too. Which name should I use?" {
		t.Errorf("the corrected call's refusal must reach the model, which answers, got %+v", res)
	}
	var names []string
	for _, c := range exec.checked() {
		names = append(names, c.Args["name"])
	}
	if !reflect.DeepEqual(names, []string{"A", "B"}) {
		t.Errorf("both calls must be checked once, checked %q", names)
	}
	if n := countEventType(sink.snapshot(), events.TypeConfirmationRequested); n != 1 {
		t.Errorf("want one question in all, got %d", n)
	}
	if ran := exec.snapshot(); len(ran) != 0 {
		t.Errorf("nothing may be executed, ran %+v", ran)
	}
	if pending, _, _, _ := loadPendingToolCall(sessions, "s1"); pending != nil {
		t.Errorf("nothing may be pending, got %+v", pending)
	}
}

// A refused check next to a sibling call that needs the user's approval: the
// refused call is recorded with its refusal and takes the response's text,
// and the sibling asks and is pending.
func TestCheckGate_RefusedCheckNextToASiblingQuestion(t *testing.T) {
	exec := &checkingExecutor{check: refuseTaken}
	llm := &scriptedNativeLLM{rounds: []provider.CompletionResponse{
		{Content: "Doing both.", ToolCalls: []provider.ToolCall{
			{ID: "n1", Name: "p__tag", Arguments: map[string]string{"name": "A"}},
			{ID: "n2", Name: "p__plain", Arguments: map[string]string{"name": "X"}},
		}},
		{Content: "Shall I write X?"},
	}}
	orch, sessions, sink := checkGateOrch(t, exec, nil, true, llm, OrchestratorOpts{})

	res, err := orch.Run(context.Background(), "s1", "tag A and write X")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Metadata["type"] != "confirmation" || res.Response != "Shall I write X?" {
		t.Fatalf("the sibling must ask, got %+v", res)
	}
	if pending, _, _, _ := loadPendingToolCall(sessions, "s1"); pending == nil || pending.ID != "n2" {
		t.Errorf("the sibling must be pending, got %+v", pending)
	}
	if ran := exec.snapshot(); len(ran) != 0 || len(exec.checked()) != 1 {
		t.Errorf("want no execution and one check, ran %+v, checked %+v", ran, exec.checked())
	}
	s, _ := sessions.Get("s1")
	var rows []string
	for _, m := range s.Messages {
		switch {
		case len(m.ToolCalls) == 1:
			rows = append(rows, fmt.Sprintf("call %s %q", m.ToolCalls[0].ID, m.Content))
		case m.Role == provider.RoleTool:
			rows = append(rows, fmt.Sprintf("result %s %q", m.ToolCallID, m.Content))
		case m.Role == provider.RoleAssistant:
			rows = append(rows, fmt.Sprintf("question %q", m.Content))
		}
	}
	want := []string{`call n1 "Doing both."`, `result n1 "error: ` + takenMessage + `"`, `question "Shall I write X?"`}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("history = %q, want %q", rows, want)
	}
	lines, _ := toolEventsOf(t, sink)
	if want := []string{"tool_call_extracted n1", "tool_call_result refused " + takenMessage, "confirmation_requested n2"}; !reflect.DeepEqual(lines, want) {
		t.Errorf("tool events = %q, want %q", lines, want)
	}
}

// A call to a declared action that OpenTalon refuses itself (here the
// permission check) is answered with that refusal: no check, no question.
func TestCheckGate_PermissionDenialAnsweredWithoutCheck(t *testing.T) {
	exec := &checkingExecutor{check: verdictOf(CheckResult{Verdict: CheckPassed}, nil)}
	orch, sessions, sink := checkGateOrch(t, exec, nil, false,
		&fakeLLM{responses: []string{"CALL locked open", "You may not open it."}}, OrchestratorOpts{})

	res, err := orch.Run(actor.WithActor(context.Background(), "user:alice"), "s1", "open it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Metadata["type"] == "confirmation" || res.Response != "You may not open it." {
		t.Errorf("the denial must reach the model without a question, got %+v", res)
	}
	if len(exec.checked()) != 0 || len(exec.snapshot()) != 0 {
		t.Errorf("a denied call is neither checked nor executed, checked %+v, ran %+v", exec.checked(), exec.snapshot())
	}
	if pending, _, _, _ := loadPendingToolCall(sessions, "s1"); pending != nil {
		t.Errorf("nothing may be pending, got %+v", pending)
	}
	assertRefusedEvents(t, sink, "tool_call_result error permission denied", nil)
}

// An action without a check declaration and a read-only action are never
// checked: the write asks as before, the read runs without a question.
func TestCheckGate_ActionsNeverChecked(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []string
		asks      bool
	}{
		{"write without a declaration", []string{"CALL p plain X", "Shall I write X?"}, true},
		{"read-only action with a declaration", []string{"CALL p look", "Here it is."}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &checkingExecutor{check: refuseTaken}
			orch, _, _ := checkGateOrch(t, exec, nil, false, &fakeLLM{responses: tc.responses}, OrchestratorOpts{})

			res, err := orch.Run(context.Background(), "s1", "go")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if asked := res.Metadata["type"] == "confirmation"; asked != tc.asks {
				t.Errorf("asked = %v, want %v (%+v)", asked, tc.asks, res)
			}
			if len(exec.checked()) != 0 {
				t.Errorf("the call must not be checked, got %+v", exec.checked())
			}
			if ran := len(exec.snapshot()); (ran == 1) == tc.asks {
				t.Errorf("ran %d time(s); a read runs, a write waits for approval", ran)
			}
		})
	}
}
