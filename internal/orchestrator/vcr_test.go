package orchestrator

import (
	"context"
	"os"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/vcr"
)

// recordModel is the model used for VCR recording. Haiku is the cheapest option.
const recordModel = "claude-haiku-4-5-20251001"

// isRecording returns true when VCR_RECORD=1 is set.
func isRecording() bool { return os.Getenv("VCR_RECORD") == "1" }

// mustPlayer loads a cassette and fails the test immediately if missing or stale.
// A stale error means prompts changed since recording; re-record with:
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

// vcrLLM returns either a Player (normal test run) or a Recorder wrapping the
// real Anthropic API (when VCR_RECORD=1). The cleanup fn saves the cassette on
// record; it is a no-op on replay.
func vcrLLM(t *testing.T, cassettePath string) (LLMClient, func()) {
	t.Helper()
	if isRecording() {
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			t.Skip("VCR_RECORD=1 requires ANTHROPIC_API_KEY")
		}
		real := provider.NewAnthropicProvider("anthropic", "", apiKey, nil)
		rec := vcr.NewRecorder(real, cassettePath, recordModel)
		return rec, func() {
			if err := rec.Save(); err != nil {
				t.Errorf("vcr save: %v", err)
			}
		}
	}
	return mustPlayer(t, cassettePath), func() {}
}

// vcrCtx returns a context with the record model injected so the Anthropic
// provider gets a valid model field. On replay the Player ignores the model.
func vcrCtx() context.Context {
	return profile.WithProfile(context.Background(), &profile.Profile{Model: recordModel})
}

// TestVCRDirectResponse verifies the orchestrator returns a direct LLM reply
// with no tool calls when the user asks a general question.
func TestVCRDirectResponse(t *testing.T) {
	const cassette = "testdata/vcr/direct_response.json"
	llm, save := vcrLLM(t, cassette)
	defer save()

	orch, sessID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(vcrCtx(), sessID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(result.ToolCalls))
	}
}

// TestVCRSingleToolCall verifies the orchestrator executes one tool call and
// returns the final LLM response that follows.
func TestVCRSingleToolCall(t *testing.T) {
	const cassette = "testdata/vcr/single_tool_call.json"
	llm, save := vcrLLM(t, cassette)
	defer save()

	orch, sessID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(vcrCtx(), sessID, "analyze code in myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}
	if !isRecording() {
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
}

// TestVCRMultiToolCall verifies multiple tool calls from one or more LLM turns.
func TestVCRMultiToolCall(t *testing.T) {
	const cassette = "testdata/vcr/multi_tool_call.json"
	llm, save := vcrLLM(t, cassette)
	defer save()

	orch, sessID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(vcrCtx(), sessID, "analyze code in myrepo and create a jira issue titled 'Code review'")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}
	if !isRecording() && len(result.ToolCalls) < 2 {
		t.Fatalf("expected at least 2 tool calls, got %d", len(result.ToolCalls))
	}
}
