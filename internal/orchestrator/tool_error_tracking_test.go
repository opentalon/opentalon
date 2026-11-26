package orchestrator

import (
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

func TestRecordToolOutcome(t *testing.T) {
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, NewToolRegistry(), state.NewMemoryStore(""), state.NewSessionStore(""),
		OrchestratorOpts{ToolErrorHandling: ToolErrorHandlingConfig{LoopCapPerTurn: 2}})
	a, b := ToolCall{Plugin: "p", Action: "a"}, ToolCall{Plugin: "p", Action: "b"}
	failed, ok := ToolResult{Error: "boom"}, ToolResult{Content: "fine"}
	notRun := ToolResult{Error: `tool "p__a" was NOT run`, NotRun: true}
	type outcome struct {
		call   ToolCall
		result ToolResult
	}
	for _, tc := range []struct {
		name     string
		outcomes []outcome
		want     string // the nudge after the last outcome, "" for none
	}{
		{"a first error below the cap", []outcome{{a, failed}}, ""},
		{"the error that reaches the cap", []outcome{{a, failed}, {a, failed}}, "[system] Tool p__a failed 2 times"},
		{"each error past the cap, with its count", []outcome{{a, failed}, {a, failed}, {a, failed}}, "failed 3 times"},
		{"a success resets the count", []outcome{{a, failed}, {a, ok}, {a, failed}}, ""},
		{"a call that did not run leaves the count", []outcome{{a, failed}, {a, notRun}, {a, failed}}, "failed 2 times"},
		{"each tool counts on its own", []outcome{{a, failed}, {b, failed}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := turnToolErrors{}
			var nudge *provider.Message
			for _, o := range tc.outcomes {
				nudge = orch.recordToolOutcome(errs, o.call, o.result)
			}
			switch {
			case tc.want == "" && nudge != nil:
				t.Errorf("nudge = %+v, want none", nudge)
			case tc.want != "" && (nudge == nil || nudge.Role != provider.RoleUser || !strings.Contains(nudge.Content, tc.want)):
				t.Errorf("nudge = %+v, want a user message containing %q", nudge, tc.want)
			}
		})
	}
}
