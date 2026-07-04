package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRepairToolCallSuccess(t *testing.T) {
	llm := &capturingPlannerLLM{response: `{"repaired_args": {"responsible_user_id": "131349", "item_id": 42}}`}
	planner := NewPlanner(llm, 0)
	got, err := planner.RepairToolCall(context.Background(), RepairToolCallRequest{
		Model:          "strong-model",
		ToolDefinition: "Tool: inventory__update-item\nParameters:\n- item_id (required): Item id\n- responsible_user_id: User id\n",
		FailedArgs:     map[string]string{"item_id": "42", "user_id": "131349"},
		ErrorText:      "unknown argument(s) for inventory__update-item: user_id; allowed: item_id, responsible_user_id",
		ApprovedPrompt: "Set user 131349 as responsible for item 42?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Aborted {
		t.Fatal("expected a repair, got abort")
	}
	if got.RepairedArgs["responsible_user_id"] != "131349" {
		t.Errorf("RepairedArgs = %v, want responsible_user_id carried through", got.RepairedArgs)
	}
	// The corrector's model routing must reach the LLM request.
	if llm.captured.Model != "strong-model" {
		t.Errorf("request Model = %q, want strong-model", llm.captured.Model)
	}
	// The corrector's whole world view: definition, failed args, error, and
	// approved prompt must all be in the request — and nothing else is passed.
	var user string
	for _, m := range llm.captured.Messages {
		if m.Role == "user" {
			user = m.Content
		}
	}
	for _, want := range []string{"inventory__update-item", "user_id: 131349", "unknown argument(s)", "Set user 131349 as responsible"} {
		if !strings.Contains(user, want) {
			t.Errorf("corrector input missing %q, got: %q", want, user)
		}
	}
}

func TestRepairToolCallAbort(t *testing.T) {
	llm := &fakePlannerLLM{response: `{"abort": true, "reason": "required value missing from the failed arguments"}`}
	planner := NewPlanner(llm, 0)
	got, err := planner.RepairToolCall(context.Background(), RepairToolCallRequest{ErrorText: "Invalid params"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Aborted {
		t.Fatal("expected abort")
	}
	if got.AbortReason == "" {
		t.Error("abort should carry the reason")
	}
}

func TestRepairToolCallMarkdownWrapped(t *testing.T) {
	llm := &fakePlannerLLM{response: "```json\n{\"repaired_args\": {\"a\": \"1\"}}\n```"}
	planner := NewPlanner(llm, 0)
	got, err := planner.RepairToolCall(context.Background(), RepairToolCallRequest{ErrorText: "Invalid params"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Aborted || got.RepairedArgs["a"] != "1" {
		t.Errorf("got %+v, want repaired_args a=1", got)
	}
}

// Any parse failure or empty verdict is an error — the caller falls back to
// the normal error flow; the corrector never half-repairs.
func TestRepairToolCallStrictParsing(t *testing.T) {
	for name, response := range map[string]string{
		"prose":           "I would rename user_id to responsible_user_id.",
		"empty_object":    `{}`,
		"empty_args":      `{"repaired_args": {}}`,
		"broken_json":     `{"repaired_args": {"a": `,
		"abort_not_bool":  `{"abort": "yes"}`,
		"missing_verdict": `{"reason": "no idea"}`,
	} {
		t.Run(name, func(t *testing.T) {
			llm := &fakePlannerLLM{response: response}
			planner := NewPlanner(llm, 0)
			if _, err := planner.RepairToolCall(context.Background(), RepairToolCallRequest{ErrorText: "Invalid params"}); err == nil {
				t.Errorf("response %q must fail strict parsing", response)
			}
		})
	}
}

// The corrector call is bounded by the planner's own timeout so a hung LLM
// falls back to the normal error flow instead of stalling the turn.
func TestRepairToolCallTimeout(t *testing.T) {
	planner := NewPlanner(&blockingPlannerLLM{}, 50*time.Millisecond)
	if _, err := planner.RepairToolCall(context.Background(), RepairToolCallRequest{ErrorText: "Invalid params"}); err == nil {
		t.Fatal("expected timeout error")
	}
}
