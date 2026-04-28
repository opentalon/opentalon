package orchestrator

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/vcr"
)

// mustPlayer loads a cassette and fails the test immediately if the cassette is
// missing or stale. A stale error means prompts changed; re-record with:
//
//	make vcr-record-all
func mustPlayer(t *testing.T, path string) *vcr.Player {
	t.Helper()
	p, err := vcr.NewPlayer(path)
	if err != nil {
		t.Fatalf("vcr: %v", err)
	}
	return p
}

// TestVCRDirectResponse verifies the orchestrator returns an LLM response
// directly when no tool call is emitted.
func TestVCRDirectResponse(t *testing.T) {
	player := mustPlayer(t, "testdata/vcr/direct_response.json")
	orch, sessID := setupOrchestrator(player, DefaultParser)

	result, err := orch.Run(context.Background(), sessID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Hello! How can I help you today?" {
		t.Errorf("response = %q", result.Response)
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(result.ToolCalls))
	}
}

// TestVCRSingleToolCall verifies the orchestrator executes one tool call and
// returns the final LLM response that follows.
func TestVCRSingleToolCall(t *testing.T) {
	player := mustPlayer(t, "testdata/vcr/single_tool_call.json")
	orch, sessID := setupOrchestrator(player, DefaultParser)

	result, err := orch.Run(context.Background(), sessID, "analyze code in myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "The code in myrepo has been analyzed. No critical issues found." {
		t.Errorf("response = %q", result.Response)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result.ToolCalls))
	}
	tc := result.ToolCalls[0]
	if tc.Plugin != "gitlab" || tc.Action != "analyze_code" {
		t.Errorf("tool call = %s.%s, want gitlab.analyze_code", tc.Plugin, tc.Action)
	}
	if tc.Args["repo"] != "myrepo" {
		t.Errorf("args[repo] = %q, want %q", tc.Args["repo"], "myrepo")
	}
}

// TestVCRMultiToolCall verifies the orchestrator executes multiple tool calls
// from a single LLM response and returns the final answer.
func TestVCRMultiToolCall(t *testing.T) {
	player := mustPlayer(t, "testdata/vcr/multi_tool_call.json")
	orch, sessID := setupOrchestrator(player, DefaultParser)

	result, err := orch.Run(context.Background(), sessID, "analyze code and create a jira issue")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Done! I analyzed the code in myrepo and created a Jira issue for the review." {
		t.Errorf("response = %q", result.Response)
	}
	if len(result.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(result.ToolCalls))
	}
}
