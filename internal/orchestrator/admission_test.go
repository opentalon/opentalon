package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
	"github.com/opentalon/opentalon/pkg/plugin/contextargs"
)

// stubPermissionChecker denies one plugin, cannot decide for another and
// allows every other.
type stubPermissionChecker struct{ denied, failing string }

func (c stubPermissionChecker) Allowed(_ context.Context, _, plugin string) (bool, error) {
	if plugin == c.failing {
		return false, errors.New("permission store unreachable")
	}
	return plugin != c.denied, nil
}

// toolEventLine renders a tool or confirmation event as its type and the
// payload fields a refusal is told apart by.
func toolEventLine(t *testing.T, e emit.Event) string {
	t.Helper()
	decode := func(v any) {
		if err := json.Unmarshal(e.Payload, v); err != nil {
			t.Fatalf("unmarshal %s: %v", e.EventType, err)
		}
	}
	switch e.EventType {
	case events.TypeToolCallExtracted:
		var p events.ToolCallExtractedPayload
		decode(&p)
		return e.EventType + " " + p.CallID
	case events.TypeToolCallNotFound:
		var p events.ToolCallNotFoundPayload
		decode(&p)
		return e.EventType + " " + p.RequestedName
	case events.TypeToolCallResult:
		var p events.ToolCallResultPayload
		decode(&p)
		return e.EventType + " " + p.Status + " " + p.ResponseExcerpt
	case events.TypeToolCallArgsInvalid:
		var p events.ToolCallArgsInvalidPayload
		decode(&p)
		return e.EventType + " " + p.ValidationError
	case events.TypeConfirmationRequested:
		var p events.ConfirmationRequestedPayload
		decode(&p)
		return e.EventType + " " + p.ToolCallID
	}
	return e.EventType
}

// Every refusal of a model's call before its dispatch: the extracted event
// under the turn's parent, then exactly one outcome event parented to it, and
// the result the model reads. Nothing reaches the plugin.
func TestExecuteCall_AdmissionRefusalEvents(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	for _, cap := range []PluginCapability{
		{Name: "p", Actions: []Action{
			{Name: "write", Description: "A write tool.", Parameters: []Parameter{{Name: "name"}}},
			{Name: "admin", Description: "User-only action.", UserOnly: true},
			{Name: "catalog", Description: "Catalog tool."},
		}},
		{Name: "locked", Actions: []Action{{Name: "open"}}},
		{Name: "broken", Actions: []Action{{Name: "open"}}},
		{Name: "hidden", Actions: []Action{{Name: "peek"}}},
	} {
		if err := registry.Register(cap, exec); err != nil {
			t.Fatalf("register %s: %v", cap.Name, err)
		}
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	sink := &recordingEventSink{}
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		EventSink:           sink,
		InjectionStateStore: &fakeInjectionStateStore{},
		PermissionChecker:   stubPermissionChecker{denied: "locked", failing: "broken"},
	})
	base := emit.WithParent(actor.WithActor(actor.WithSessionID(context.Background(), "s1"), "user:alice"), "turn-parent")
	unknownArgs := "unknown argument(s) for p__write: title; allowed: name"

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		call    ToolCall
		outcome string
		result  ToolResult
	}{
		{"no plugin", base, ToolCall{Action: "write"},
			"tool_call_not_found __write", ToolResult{Error: textWrongNameFormError}},
		{"unknown plugin", base, ToolCall{Plugin: "ghost", Action: "write"},
			"tool_call_not_found ghost__write", ToolResult{Error: `plugin "ghost" not found`}},
		{"unknown action", base, ToolCall{Plugin: "p", Action: "nope"},
			"tool_call_not_found p__nope", ToolResult{Error: `action "nope" not found in plugin "p"`}},
		{"permission denied", base, ToolCall{Plugin: "locked", Action: "open"},
			"tool_call_result error permission denied", ToolResult{Error: "permission denied"}},
		{"permission check failed", base, ToolCall{Plugin: "broken", Action: "open"},
			"tool_call_result error permission denied", ToolResult{Error: "permission denied"}},
		{"plugin outside the profile", profile.WithProfile(base, &profile.Profile{Plugins: []string{"p"}}),
			ToolCall{Plugin: "hidden", Action: "peek"},
			`tool_call_result error plugin "hidden" is not available for this profile`,
			ToolResult{Error: `plugin "hidden" is not available for this profile`}},
		{"user-only action", base, ToolCall{Plugin: "p", Action: "admin"},
			`tool_call_result error action "admin" can only be invoked by the user, not the LLM`,
			ToolResult{Error: `action "admin" can only be invoked by the user, not the LLM`}},
		{"tool not loaded", withSentNativeTools(base, map[string]struct{}{"p__write": {}}),
			ToolCall{Plugin: "p", Action: "catalog"},
			"tool_call_result not_run " + loadOnCallAnswer("p__catalog", nil),
			ToolResult{Error: loadOnCallAnswer("p__catalog", nil), NotRun: true}},
		{"unknown argument names", base, ToolCall{Plugin: "p", Action: "write", Args: map[string]string{"title": "A"}},
			"tool_call_args_invalid " + unknownArgs, ToolResult{Error: unknownArgs, ArgsInvalid: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink.mu.Lock()
			sink.events = nil
			sink.mu.Unlock()
			tc.call.ID, tc.call.FromLLM = "c1", true

			res := orch.executeCall(tc.ctx, tc.call)

			evs := sink.snapshot()
			if len(evs) != 2 {
				t.Fatalf("want the extracted event and one outcome, got %d event(s): %+v", len(evs), evs)
			}
			if got := toolEventLine(t, evs[0]); got != "tool_call_extracted c1" || evs[0].ParentID != "turn-parent" {
				t.Errorf("first event = %q under %q, want the extracted call under the turn's parent", got, evs[0].ParentID)
			}
			if got := toolEventLine(t, evs[1]); got != tc.outcome || evs[1].ParentID != evs[0].ID {
				t.Errorf("outcome = %q under %q, want %q under the extracted event %q", got, evs[1].ParentID, tc.outcome, evs[0].ID)
			}
			want := tc.result
			want.CallID = "c1"
			if want.ArgsInvalid {
				want.EventID = evs[1].ID
			}
			if !reflect.DeepEqual(res, want) {
				t.Errorf("result = %+v, want %+v", res, want)
			}
		})
	}
	if exec.count != 0 {
		t.Errorf("no refused call may reach the plugin, ran %d time(s)", exec.count)
	}
}

// executeCall dispatches the call under the action's registered name with the
// host's context args, and leaves the caller's call as the model issued it: the
// agent loop records that call in the history after the execution, and the
// extracted event records the same.
func TestExecuteCall_LeavesTheIssuedCallUnchanged(t *testing.T) {
	exec := &recordingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{Name: "p", Actions: []Action{{
		Name: "tag-item", Description: "Tag an item.",
		Parameters:        []Parameter{{Name: "name"}},
		InjectContextArgs: []string{contextargs.SessionID},
	}}}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	sink := &recordingEventSink{}
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions,
		OrchestratorOpts{EventSink: sink})

	call := ToolCall{ID: "c1", Plugin: "p", Action: "tag_item", Args: map[string]string{"name": "A"}, FromLLM: true}
	issued := call
	issued.Args = maps.Clone(call.Args)
	if res := orch.executeCall(actor.WithSessionID(context.Background(), "s1"), call); res.Error != "" {
		t.Fatalf("executeCall: %s", res.Error)
	}

	if !reflect.DeepEqual(call, issued) {
		t.Errorf("the caller's call changed: %+v, issued %+v", call, issued)
	}
	ran := exec.snapshot()
	if len(ran) != 1 || ran[0].Action != "tag-item" ||
		!reflect.DeepEqual(ran[0].Args, map[string]string{"name": "A", contextargs.SessionID: "s1"}) {
		t.Errorf("the plugin must get the registered name and the injected arg, got %+v", ran)
	}
	extracted := findToolCallExtractedPayloads(t, sink.snapshot())
	if len(extracted) != 1 || extracted[0].Action != "tag_item" ||
		!reflect.DeepEqual(extracted[0].Arguments, map[string]string{"name": "A"}) {
		t.Errorf("the extracted event must record the call as issued, got %+v", extracted)
	}
}

// Admission works on a copy: the admitted call carries the action's registered
// name and the injected context arg, and the call passed in, the one a caller
// stores for approval, is unchanged.
func TestAdmitCall_LeavesTheCallPassedInUnchanged(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{Name: "p", Actions: []Action{{
		Name: "tag-item", Description: "Tag an item.",
		Parameters:        []Parameter{{Name: "name"}},
		InjectContextArgs: []string{contextargs.SessionID},
	}}}, &recordingExecutor{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), state.NewSessionStore(""),
		OrchestratorOpts{})

	call := ToolCall{ID: "c1", Plugin: "p", Action: "tag_item", Args: map[string]string{"name": "A"}, FromLLM: true}
	issued := call
	issued.Args = maps.Clone(call.Args)
	admitted, refusal, ok := orch.admitCall(actor.WithSessionID(context.Background(), "s1"), call, time.Now())
	if !ok {
		t.Fatalf("the call must be admitted, got %+v", refusal)
	}

	if !reflect.DeepEqual(call, issued) {
		t.Errorf("the call passed in changed: %+v, issued %+v", call, issued)
	}
	if admitted.call.Action != "tag-item" || admitted.action == nil || admitted.action.Name != "tag-item" ||
		!reflect.DeepEqual(admitted.call.Args, map[string]string{"name": "A", contextargs.SessionID: "s1"}) {
		t.Errorf("the admitted call must carry the registered name and the injected arg, got %+v (action %+v)",
			admitted.call, admitted.action)
	}
}

// For an action that is not read-only the user is asked first, as for any
// write; the refusal of OpenTalon's own gates comes at the execution after the
// approval, is recorded in the history as the call's result, and the plugin
// never runs. With the repair step off, unknown argument names take the same
// order.
func TestRun_ConfirmationComesBeforeAdmissionRefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile *profile.Profile
		call    ToolCall
		want    string
	}{
		{"unknown argument names, repair off", nil,
			ToolCall{Plugin: "inv", Action: "update-item", Args: map[string]string{"item_id": "42", "user_id": "7"}},
			"unknown argument(s) for inv__update-item: user_id; allowed: item_id"},
		{"permission denied", nil, ToolCall{Plugin: "locked", Action: "open"}, "permission denied"},
		{"plugin outside the profile", &profile.Profile{Plugins: []string{"inv"}},
			ToolCall{Plugin: "hidden", Action: "peek"}, `plugin "hidden" is not available for this profile`},
		{"user-only action", nil, ToolCall{Plugin: "inv", Action: "wipe"},
			`action "wipe" can only be invoked by the user, not the LLM`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &countingExecutor{}
			registry := NewToolRegistry()
			for _, cap := range []PluginCapability{
				{Name: "inv", Actions: []Action{
					{Name: "update-item", Description: "Update an item.", Parameters: []Parameter{{Name: "item_id"}}},
					{Name: "wipe", Description: "User-only action.", UserOnly: true},
				}},
				{Name: "locked", Actions: []Action{{Name: "open"}}},
				{Name: "hidden", Actions: []Action{{Name: "peek"}}},
			} {
				if err := registry.Register(cap, exec); err != nil {
					t.Fatalf("register %s: %v", cap.Name, err)
				}
			}
			if err := registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{}); err != nil {
				t.Fatalf("register conf: %v", err)
			}
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "s1"})
			call := tc.call
			call.ID = "tc-1"
			parser := &fakeParser{parseFn: func(response string) []ToolCall {
				if strings.Contains(response, "[tool_call]") {
					return []ToolCall{call}
				}
				return nil
			}}
			llm := &fakeLLM{responses: []string{"[tool_call] do it", "Shall I do it?", "It was refused."}}
			orch := NewWithRules(llm, parser, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
				ConfirmationPlugin: "conf",
				ConfirmationAction: "check",
				PermissionChecker:  stubPermissionChecker{denied: "locked"},
			})
			ctx := actor.WithActor(context.Background(), "user:alice")
			if tc.profile != nil {
				ctx = profile.WithProfile(ctx, tc.profile)
			}

			first, err := orch.Run(ctx, "s1", "do it")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if first.Metadata["type"] != "confirmation" {
				t.Fatalf("the call must be confirmed first, got %+v", first)
			}
			if pending, _, _, _ := loadPendingToolCall(sessions, "s1"); pending == nil || pending.ID != "tc-1" {
				t.Fatalf("the pending call must be tc-1, got %+v", pending)
			}

			if _, err := orch.Run(actor.WithConfirmationDecision(ctx, "approve"), "s1", "yes"); err != nil {
				t.Fatalf("Run (approve): %v", err)
			}
			if exec.count != 0 {
				t.Errorf("the refused call must not reach the plugin, ran %d time(s)", exec.count)
			}
			s, _ := sessions.Get("s1")
			recorded := false
			for i, m := range s.Messages {
				if m.Role == provider.RoleAssistant && strings.HasPrefix(m.Content, "[tool_call] "+toolFQN(call.Plugin, call.Action)) &&
					i+1 < len(s.Messages) && strings.Contains(s.Messages[i+1].Content, tc.want) {
					recorded = true
				}
			}
			if !recorded {
				t.Errorf("the history must pair the approved call with its refusal %q, got %+v", tc.want, s.Messages)
			}
		})
	}
}

// The planner's single step passes the same gate: a read-only step runs
// without asking. Its result seeds the history when it succeeded; a failed
// step is only logged, and the agent loop answers the turn.
func TestRun_SingleStepReadOnlyRunsWithoutAsking(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result ToolResult
		seeded bool
	}{
		{"succeeds", ToolResult{Content: "3 items"}, true},
		{"fails", ToolResult{Error: "index unavailable"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := 0
			registry := NewToolRegistry()
			if err := registry.Register(PluginCapability{Name: "p", Actions: []Action{
				{Name: "look", Description: "Look items up.", ReadOnly: true},
			}}, &capturingExecutor{fn: func(c ToolCall) ToolResult {
				ran++
				r := tc.result
				r.CallID = c.ID
				return r
			}}); err != nil {
				t.Fatalf("register: %v", err)
			}
			if err := registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{}); err != nil {
				t.Fatalf("register conf: %v", err)
			}
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "s1"})
			sink := &recordingEventSink{}
			llm := &fakeLLM{responses: []string{
				`{"type":"pipeline","steps":[{"id":"1","name":"Look","plugin":"p","action":"look"}]}`,
				"Here is what I found.",
			}}
			orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
				state.NewMemoryStore(""), sessions, OrchestratorOpts{
					EventSink:          sink,
					PipelineEnabled:    true,
					ConfirmationPlugin: "conf",
					ConfirmationAction: "check",
				})

			res, err := orch.Run(context.Background(), "s1", "how many items?")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ran != 1 || res.Response != "Here is what I found." ||
				countEventType(sink.snapshot(), events.TypeConfirmationRequested) != 0 {
				t.Errorf("the step must run once without a question and the agent loop answer, ran %d, response %q",
					ran, res.Response)
			}
			s, _ := sessions.Get("s1")
			var exchange []string
			for _, m := range s.Messages {
				if strings.Contains(m.Content, "[tool_call]") || strings.Contains(m.Content, "[plugin_output]") {
					exchange = append(exchange, m.Content)
				}
			}
			if tc.seeded && (len(exchange) != 2 || !strings.Contains(exchange[1], "3 items")) {
				t.Errorf("the history must hold the step's call and result, got %q", exchange)
			}
			if !tc.seeded && len(exchange) != 0 {
				t.Errorf("a failed step must leave no exchange in the history, got %q", exchange)
			}
		})
	}
}

// One response with a read, a call to an action that does not exist, a write
// that needs the user's approval and a write after it: the read runs, the
// unknown action is refused without a question, the first write asks and the
// second is recorded as not executed. Each call that ran or was refused has
// its extracted event and one outcome; the two writes have none.
func TestRun_MixedBatchStopsAtTheConfirmation(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{Name: "p", Actions: []Action{
		{Name: "read", Description: "A read.", ReadOnly: true, AlwaysInclude: true},
		{Name: "write_a", Description: "First write.", AlwaysInclude: true},
		{Name: "write_b", Description: "Second write.", AlwaysInclude: true},
	}}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{}); err != nil {
		t.Fatalf("register conf: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	sink := &recordingEventSink{}
	llm := &scriptedNativeLLM{rounds: []provider.CompletionResponse{{ToolCalls: []provider.ToolCall{
		{ID: "r1", Name: "p__read", Arguments: map[string]string{}},
		{ID: "x1", Name: "p__nope", Arguments: map[string]string{}},
		{ID: "a1", Name: "p__write_a", Arguments: map[string]string{}},
		{ID: "b1", Name: "p__write_b", Arguments: map[string]string{}},
	}}}}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:           sink,
			ConfirmationPlugin:  "conf",
			ConfirmationAction:  "check",
			InjectionStateStore: &fakeInjectionStateStore{},
		})

	res, err := orch.Run(actor.WithSessionID(context.Background(), "s1"), "s1", "read, then write both")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exec.count != 1 || res.Metadata["type"] != "confirmation" {
		t.Fatalf("only the read may run before the question, ran %d, result %+v", exec.count, res)
	}
	if pending, _, _, _ := loadPendingToolCall(sessions, "s1"); pending == nil || pending.ID != "a1" {
		t.Errorf("the pending call must be a1, got %+v", pending)
	}

	var got []string
	for _, e := range sink.snapshot() {
		switch e.EventType {
		case events.TypeToolCallExtracted, events.TypeToolCallResult, events.TypeToolCallNotFound,
			events.TypeToolCallArgsInvalid, events.TypeConfirmationRequested:
			line := toolEventLine(t, e)
			if e.EventType == events.TypeToolCallResult {
				line = strings.Fields(line)[0] + " " + strings.Fields(line)[1]
			}
			got = append(got, line)
		}
	}
	wantEvents := []string{
		"tool_call_extracted r1", "tool_call_result ok",
		"tool_call_extracted x1", "tool_call_not_found p__nope",
		"confirmation_requested a1",
	}
	if !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("events = %q, want %q", got, wantEvents)
	}

	s, _ := sessions.Get("s1")
	var rows []string
	for _, m := range s.Messages {
		switch {
		case len(m.ToolCalls) == 1:
			rows = append(rows, "call "+m.ToolCalls[0].ID)
		case m.Role == provider.RoleTool:
			rows = append(rows, "result "+m.ToolCallID)
		case m.Role == provider.RoleAssistant && m.Content == res.Response:
			rows = append(rows, "question")
		}
	}
	wantRows := []string{"call r1", "result r1", "call x1", "result x1", "call b1", "result b1", "question"}
	if !reflect.DeepEqual(rows, wantRows) {
		t.Errorf("history = %q, want %q", rows, wantRows)
	}
}
