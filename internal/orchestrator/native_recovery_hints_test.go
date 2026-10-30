package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// recoveryLLM records every request and answers from a script, then "done".
// native decides whether it advertises native tool calling, so one fake drives
// both tool-calling modes.
type recoveryLLM struct {
	native   bool
	rounds   []provider.CompletionResponse
	requests []*provider.CompletionRequest
}

func (l *recoveryLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.requests = append(l.requests, req)
	if len(l.rounds) == 0 {
		return &provider.CompletionResponse{Content: "done"}, nil
	}
	r := l.rounds[0]
	l.rounds = l.rounds[1:]
	return &r, nil
}

func (l *recoveryLLM) SupportsFeature(f provider.Feature) bool {
	return l.native && f == provider.FeatureTools
}

// recoveryParser stands in for the text parser: "NARRATED" is a narrated call,
// "BROKEN" a [tool_call] block it could not decode, "CALL" a valid call of
// p__lookup; anything else carries no call.
func recoveryParser() ToolCallParser {
	return &fakeParser{parseFn: func(s string) []ToolCall {
		switch {
		case strings.HasPrefix(s, "NARRATED"):
			return narratedToolCallPlaceholder
		case s == "BROKEN":
			return []ToolCall{{ID: "c1"}}
		case s == "CALL":
			return []ToolCall{{ID: "c2", Plugin: "p", Action: "lookup", Args: map[string]string{"q": "x"}}}
		}
		return nil
	}}
}

// recoveryMessage runs one turn whose first reply is the given one and returns
// the last message of the request that follows it — where the recovery hint, or
// the tool result carrying the wrong-name error, reaches the model.
func recoveryMessage(t *testing.T, nativeMode bool, steps []*pipeline.Step, rounds ...provider.CompletionResponse) string {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "Test plugin",
		Actions: []Action{native(Action{Name: "lookup", Description: "Look something up.", ReadOnly: true,
			Parameters: []Parameter{{Name: "q", Description: "Query."}}})},
	}, &echoExecutor{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	llm := &recoveryLLM{native: nativeMode, rounds: rounds}
	orch := New(llm, recoveryParser(), registry, state.NewMemoryStore(""), sessions)

	ctx := context.Background()
	if steps != nil {
		ctx = withExpectedTools(ctx, steps)
	}
	if _, err := orch.Run(ctx, "s1", "how many items are there?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(llm.requests) < 2 {
		t.Fatalf("expected a request after the first reply, got %d request(s)", len(llm.requests))
	}
	msgs := llm.requests[1].Messages
	if len(msgs) == 0 {
		t.Fatal("request after the first reply carries no messages")
	}
	return msgs[len(msgs)-1].Content
}

var (
	invented       = provider.CompletionResponse{Content: "You have {{plugin_output.pagination.total}} items."}
	plainAnswer    = provider.CompletionResponse{Content: "There are 3 items."}
	concreteSteps  = []*pipeline.Step{{ID: "1", Command: &pipeline.PluginCommand{Plugin: "p", Action: "lookup", Args: map[string]any{"q": "x"}}}}
	sentinelSteps  = []*pipeline.Step{{ID: "direct"}}
	callAfterNudge = provider.CompletionResponse{Content: "CALL"}
)

func TestRecoveryHints_NativeMode(t *testing.T) {
	cases := []struct {
		name   string
		steps  []*pipeline.Step
		rounds []provider.CompletionResponse
		want   []string
	}{
		{"empty reply", nil, []provider.CompletionResponse{{}},
			[]string{"native tool call", "exact name", "plain language"}},
		{"invented result", nil, []provider.CompletionResponse{invented},
			[]string{"native tool call", "exact name"}},
		{"narrated call", nil, []provider.CompletionResponse{{Content: "NARRATED: let me look that up"}},
			[]string{"native tool call", "exact name"}},
		{"planner, concrete step", concreteSteps, []provider.CompletionResponse{plainAnswer, callAfterNudge},
			[]string{"native tool call", "p__lookup", `{"q":"x"}`}},
		{"planner, no concrete step", sentinelSteps, []provider.CompletionResponse{plainAnswer},
			[]string{"native tool call", "exact name"}},
		{"tool name in the wrong form", nil,
			[]provider.CompletionResponse{{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "lookup", Arguments: map[string]string{"q": "x"}}}}},
			[]string{"native tool call", "exact name", "plugin__action"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := recoveryMessage(t, true, tc.steps, tc.rounds...)
			if strings.Contains(got, "[tool_call]") {
				t.Errorf("native-mode message names the text tool-call format:\n%s", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("native-mode message lacks %q:\n%s", w, got)
				}
			}
		})
	}
}

func TestRecoveryHints_TextModeUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		steps  []*pipeline.Step
		rounds []provider.CompletionResponse
		want   string
	}{
		{"empty reply", nil, []provider.CompletionResponse{{}},
			"[system] Your previous response was empty or contained only unparseable tool call blocks. Please respond to the user in natural language. If you need to call a tool, use the [tool_call] format shown in your instructions."},
		{"invented result", nil, []provider.CompletionResponse{invented},
			"[system] Your previous response contained placeholder variables instead of real data. You MUST call the tool first using [tool_call] format, then answer with the actual results."},
		{"narrated call", nil, []provider.CompletionResponse{{Content: "NARRATED: let me look that up"}},
			"[system] Do not describe what you will do. Execute the tool call NOW using the [tool_call] format shown in your instructions."},
		{"planner, concrete step", concreteSteps, []provider.CompletionResponse{plainAnswer, callAfterNudge},
			"[system] You answered without calling a tool — your answer may be fabricated. You MUST call the tool first, then answer with real data. " +
				"Output ONLY a [tool_call] block like this:\n\n[tool_call]\n{\"tool\": \"p__lookup\", \"args\": {\"q\":\"x\"}}\n[/tool_call]"},
		{"planner, no concrete step", sentinelSteps, []provider.CompletionResponse{plainAnswer},
			"[system] Do not describe what you will do. Execute the tool call NOW using the [tool_call] format shown in your instructions."},
		{"tool name in the wrong form", nil, []provider.CompletionResponse{{Content: "BROKEN"}},
			`tool call format not recognized. You MUST use this exact format: [tool_call] {"tool": "plugin__action", "args": {"key": "value"}} [/tool_call]. Do NOT use XML or any other format. Retry the same action now using the correct format.`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := recoveryMessage(t, false, tc.steps, tc.rounds...)
			if !strings.Contains(got, tc.want) {
				t.Errorf("text-mode message changed:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}
