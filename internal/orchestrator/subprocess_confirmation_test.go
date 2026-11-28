package orchestrator

import (
	"context"
	"reflect"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

type executorFunc func(context.Context, ToolCall) ToolResult

func (f executorFunc) Execute(ctx context.Context, call ToolCall) ToolResult { return f(ctx, call) }

const writeTask = "write it"

func writeChild() map[string][]provider.CompletionResponse {
	return map[string][]provider.CompletionResponse{writeTask: {
		callReply("c1", "store__write", map[string]string{}),
		{Content: "could not write"},
	}}
}

type registeredPlugin struct {
	cap  PluginCapability
	exec PluginExecutor
}

func gatedSubprocessOrch(t *testing.T, llm LLMClient, opts OrchestratorOpts, extra ...registeredPlugin) (*Orchestrator, *countingExecutor, *recordingEventSink) {
	t.Helper()
	write := &countingExecutor{}
	registry := NewToolRegistry()
	for _, p := range append([]registeredPlugin{
		{PluginCapability{Name: "store", Actions: []Action{{Name: "write", Description: "Write a record."}}}, write},
		{PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{}},
	}, extra...) {
		if err := registry.Register(p.cap, p.exec); err != nil {
			t.Fatalf("register %s: %v", p.cap.Name, err)
		}
	}
	sink := &recordingEventSink{}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	opts.EventSink = sink
	opts.ConfirmationPlugin, opts.ConfirmationAction = "conf", "check"
	opts.Subprocess = SubprocessConfig{Enabled: true, MaxDepth: 2}
	return NewWithRules(llm, DefaultParser, registry, state.NewMemoryStore(""), sessions, opts), write, sink
}

func assertRefusedWithoutAsking(t *testing.T, orch *Orchestrator, llm *subagentLLM, write *countingExecutor, sink *recordingEventSink, tools []string) {
	t.Helper()
	if n := countEventType(sink.snapshot(), events.TypeConfirmationRequested); n != 0 {
		t.Errorf("confirmation_requested events = %d, want none", n)
	}
	s, err := orch.sessions.Get("s1")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if p := s.Metadata["pending_tool_call"]; p != "" {
		t.Errorf("pending call stored: %s", p)
	}
	for _, m := range s.Messages {
		if m.PromptType == provider.PromptTypeToolConfirmation {
			t.Errorf("question stored in the session: %q", m.Content)
		}
	}
	if write.count != 0 {
		t.Errorf("store__write ran %d time(s), want never", write.count)
	}
	llm.assertTools(t, 2, tools...)
	want := provider.Message{Role: provider.RoleTool, ToolCallID: "c1",
		Content: "error: tool store__write needs the user's approval, which this sub-agent cannot ask for; the call did not run"}
	if got := llm.requests[1].Messages[3]; !reflect.DeepEqual(got, want) {
		t.Errorf("result of the gated call = %+v, want %+v", got, want)
	}
}

func TestSubprocessGatedCallOutsideTheAgentLoop(t *testing.T) {
	const singleStep = `{"type":"pipeline","steps":[{"id":"1","name":"Delegate","plugin":"_subprocess","action":"run","args":{"task":"` + writeTask + `"}}]}`
	const invoke = `{"send_to_llm": false, "invoke": {"plugin": "_subprocess", "action": "run", "args": {"task": "` + writeTask + `"}}}`
	for _, tc := range []struct {
		name      string
		parent    []provider.CompletionResponse
		opts      OrchestratorOpts
		untouched bool
		tools     []string
		run       func(ctx context.Context, orch *Orchestrator) error
	}{
		{
			name:      "direct action entry with a chat session id",
			untouched: true,
			tools:     []string{"conf__check", "hook__review", "invoker__prepare", "store__write"},
			run: func(ctx context.Context, orch *Orchestrator) error {
				_, _, err := orch.RunActionResult(actor.WithSessionID(ctx, "s1"), "_subprocess", "run", map[string]string{"task": writeTask})
				return err
			},
		},
		{
			name:   "callback of a plugin the agent loop called",
			parent: []provider.CompletionResponse{callReply("p1", "hook__review", map[string]string{}), {Content: "reviewed"}},
			tools:  []string{"conf__check", "hook__review", "invoker__prepare", "store__write"},
			run: func(ctx context.Context, orch *Orchestrator) error {
				_, err := orch.Run(ctx, "s1", "review it")
				return err
			},
		},
		{
			name:   "single-step path",
			parent: []provider.CompletionResponse{{Content: singleStep}, {Content: "delegated"}},
			opts:   OrchestratorOpts{PipelineEnabled: true},
			tools:  []string{"conf__check", "hook__review", "invoker__prepare", "store__write"},
			run: func(ctx context.Context, orch *Orchestrator) error {
				_, err := orch.Run(ctx, "s1", "delegate it")
				return err
			},
		},
		{
			name:  "preparer step",
			opts:  OrchestratorOpts{ContentPreparers: []ContentPreparerEntry{{Plugin: "invoker", Action: "prepare"}}},
			tools: []string{"conf__check", "hook__review", "store__write"},
			run: func(ctx context.Context, orch *Orchestrator) error {
				_, err := orch.Run(ctx, "s1", "anything")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			llm := &subagentLLM{parent: tc.parent, children: writeChild()}
			var orch *Orchestrator
			// A plugin callback runs on the context of the plugin call that sent it (plugin.Client.handleCallback).
			hook := executorFunc(func(ctx context.Context, call ToolCall) ToolResult {
				content, _, err := orch.RunActionResult(WithPluginCaller(ctx, "hook", "review"), "_subprocess", "run", map[string]string{"task": writeTask})
				if err != nil {
					return ToolResult{CallID: call.ID, Error: err.Error()}
				}
				return ToolResult{CallID: call.ID, Content: content}
			})
			orch, write, sink := gatedSubprocessOrch(t, llm, tc.opts,
				registeredPlugin{PluginCapability{Name: "hook", Actions: []Action{{Name: "review", Description: "Review.", ReadOnly: true, AlwaysInclude: true}}}, hook},
				registeredPlugin{PluginCapability{Name: "invoker", Actions: []Action{{Name: "prepare", Description: "Prepare."}}}, &fixedResultExecutor{content: invoke}},
			)

			if err := tc.run(context.Background(), orch); err != nil {
				t.Fatalf("entry: %v", err)
			}
			assertRefusedWithoutAsking(t, orch, llm, write, sink, tc.tools)
			if tc.untouched {
				s, _ := orch.sessions.Get("s1")
				if len(s.Messages) != 0 || len(s.Metadata) != 0 {
					t.Errorf("session written: messages %+v, metadata %v", s.Messages, s.Metadata)
				}
			}
		})
	}
}

func TestInteractiveTurnMarksOnlyTheAgentLoopsCall(t *testing.T) {
	seen := map[string]*interactiveTurn{}
	probe := executorFunc(func(ctx context.Context, call ToolCall) ToolResult {
		turn, _ := ctx.Value(interactiveTurnKey{}).(*interactiveTurn)
		seen[call.ID] = turn
		return ToolResult{CallID: call.ID, Content: "looked"}
	})
	var orch *Orchestrator
	hook := executorFunc(func(ctx context.Context, call ToolCall) ToolResult {
		content, _, err := orch.RunActionResult(ctx, "probe", "look", map[string]string{})
		if err != nil {
			return ToolResult{CallID: call.ID, Error: err.Error()}
		}
		return ToolResult{CallID: call.ID, Content: content}
	})
	readOnly := []Action{{Name: "look", Description: "Look.", ReadOnly: true, AlwaysInclude: true}}
	llm := &subagentLLM{
		parent: []provider.CompletionResponse{
			{ToolCalls: []provider.ToolCall{
				{ID: "p-load", Name: toolFQN(metaPluginName, metaLoadTools), Arguments: map[string]string{"names": "_subprocess__run"}},
				{ID: "p-probe", Name: "probe__look", Arguments: map[string]string{}},
			}},
			callReply("p-sub", "_subprocess__run", map[string]string{"task": "look", "tools": "probe__look"}),
			callReply("p-hook", "hook__look", map[string]string{}),
			{Content: "done"},
		},
		children: map[string][]provider.CompletionResponse{"look": {callReply("c1", "probe__look", map[string]string{}), {Content: "seen"}}},
	}
	orch, _, _ = gatedSubprocessOrch(t, llm, OrchestratorOpts{},
		registeredPlugin{PluginCapability{Name: "probe", Actions: readOnly}, probe},
		registeredPlugin{PluginCapability{Name: "hook", Actions: readOnly}, hook},
	)

	if _, err := orch.Run(context.Background(), "s1", "look around"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if turn := seen["p-probe"]; turn == nil || turn.sessions == nil || turn.userMessage != "look around" {
		t.Errorf("the agent loop's call carries %+v, want the turn's session store and user message", turn)
	}
	for _, id := range []string{"c1", "direct-probe-look"} {
		if turn, ok := seen[id]; !ok || turn != nil {
			t.Errorf("call %s: ran %v, marker %+v; want it run without the marker", id, ok, turn)
		}
	}
	llm.assertTools(t, 2, "probe__look")
}
