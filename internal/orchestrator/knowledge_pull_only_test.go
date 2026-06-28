package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

// With knowledge PUSH disabled (orchestrator.knowledge.push_enabled:
// false) the orchestrator runs pull-only: a preparer that returns
// structured candidates AND an embedded [knowledge_context] block must
// have its block stripped from the content the LLM sees, the dedup
// decision skipped (no state persist), and preparer_decision must
// report mode=pull_only — the retrieved candidates listed (what a push
// WOULD have surfaced) but nothing injected. This guards both the
// pull-only branch and the honest telemetry that keeps push-vs-pull
// A/B readable in the session log.
func TestOrchestrator_PreparerPhase_PushDisabledEmitsPullOnly(t *testing.T) {
	preparerJSON := `{
		"send_to_llm": true,
		"message": "[knowledge_context]\nplugin-rendered body\n[/knowledge_context]\n\nuser question",
		"knowledge_candidates": [{"article_id": "kb_a", "content_sha256": "aaa", "content": "plugin-rendered body", "score": 0.7}]
	}`
	sink := &recordingEventSink{}
	dedupStore := &fakeInjectionStateStore{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "modern-rag", Description: "modern RAG",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	llm := &capturingLLM{responses: []string{"answer"}}
	pushOff := false
	orch := NewWithRules(llm,
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:            sink,
			ContentPreparers:     []ContentPreparerEntry{{Plugin: "modern-rag", Action: "prepare"}},
			KnowledgeDedup:       KnowledgeDedupConfig{Enabled: true},
			InjectionStateStore:  dedupStore,
			KnowledgePushEnabled: &pushOff,
		})
	if _, err := orch.Run(context.Background(), "s1", "user question"); err != nil {
		t.Fatal(err)
	}

	// preparer_decision: mode=pull_only, candidates listed, nothing injected.
	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision missing")
	}
	var p events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Mode != events.PreparerDecisionModePullOnly {
		t.Errorf("Mode = %q, want %q", p.Mode, events.PreparerDecisionModePullOnly)
	}
	if len(p.Knowledge.Injected) != 0 || p.Knowledge.InjectedBytes != 0 {
		t.Errorf("pull_only must inject nothing, got Injected=%+v bytes=%d", p.Knowledge.Injected, p.Knowledge.InjectedBytes)
	}
	if len(p.Knowledge.CandidateIDs) != 1 || p.Knowledge.CandidateIDs[0] != "kb_a" {
		t.Errorf("pull_only must still list retrieved candidates, got CandidateIDs=%+v", p.Knowledge.CandidateIDs)
	}

	// No dedup state persisted — the decision was skipped entirely.
	if dedupStore.updateCalls != 0 {
		t.Errorf("pull_only must NOT persist dedup state, got updateCalls=%d", dedupStore.updateCalls)
	}

	// The content the LLM saw must have the [knowledge_context] stripped
	// while the user's actual question survives.
	if len(llm.requests) == 0 {
		t.Fatal("LLM received no request")
	}
	var seen strings.Builder
	for _, m := range llm.requests[0].Messages {
		seen.WriteString(m.Content)
		seen.WriteString("\n")
	}
	body := seen.String()
	if strings.Contains(body, "[knowledge_context]") {
		t.Errorf("pull_only must strip [knowledge_context] from the LLM content, but it was present:\n%s", body)
	}
	if !strings.Contains(body, "user question") {
		t.Errorf("the user's question must survive stripping, got:\n%s", body)
	}
}
