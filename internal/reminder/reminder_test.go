package reminder

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

func TestReminderSayEchoes(t *testing.T) {
	tool := NewTool()
	res := tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "say",
		Args: map[string]string{"message": "hello there"},
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.Content != "hello there" {
		t.Errorf("content = %q, want %q", res.Content, "hello there")
	}
}

func TestReminderSayRejectsEmpty(t *testing.T) {
	tool := NewTool()
	res := tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "say",
	})
	if res.Error == "" {
		t.Error("expected error for empty message")
	}
}

func TestReminderUnknownAction(t *testing.T) {
	tool := NewTool()
	res := tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "yell",
		Args: map[string]string{"message": "x"},
	})
	if res.Error == "" {
		t.Error("expected error for unknown action")
	}
}
