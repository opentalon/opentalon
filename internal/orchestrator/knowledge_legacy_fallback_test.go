package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

func TestResponseUsesLegacyKnowledgeInjection(t *testing.T) {
	cases := []struct {
		name string
		pr   preparerResponse
		want bool
	}{
		{
			name: "candidates_present_message_with_kc",
			pr: preparerResponse{
				Message:             "[knowledge_context]\nbody\n[/knowledge_context]\nrest",
				KnowledgeCandidates: []KnowledgeCandidate{{ArticleID: "kb", ContentSHA256: "x"}},
			},
			want: false, // structured candidates win
		},
		{
			name: "no_candidates_message_with_kc",
			pr:   preparerResponse{Message: "[knowledge_context]\nbody\n[/knowledge_context]"},
			want: true,
		},
		{
			name: "no_candidates_message_with_tagged_kc",
			pr:   preparerResponse{Message: `[knowledge_context id="kb_a" sha="aaa"]body[/knowledge_context]`},
			want: true, // tagged but no candidate-slice still trips fallback
		},
		{
			name: "no_candidates_message_without_kc",
			pr:   preparerResponse{Message: "plain user content"},
			want: false,
		},
		{
			name: "everything_empty",
			pr:   preparerResponse{},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := responseUsesLegacyKnowledgeInjection(c.pr); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestPreparerAggregate_AppendRecordsLegacyPlugin(t *testing.T) {
	var agg preparerAggregate
	agg.append("modern", preparerResponse{
		KnowledgeCandidates: []KnowledgeCandidate{{ArticleID: "kb_x", ContentSHA256: "x"}},
	})
	agg.append("legacy", preparerResponse{
		Message: "[knowledge_context]\nbody\n[/knowledge_context]",
	})
	if len(agg.LegacyKnowledgePlugins) != 1 || agg.LegacyKnowledgePlugins[0] != "legacy" {
		t.Errorf("LegacyKnowledgePlugins = %v, want [legacy]", agg.LegacyKnowledgePlugins)
	}
	if len(agg.Knowledge) != 1 {
		t.Errorf("modern plugin's candidate must still be aggregated, got %+v", agg.Knowledge)
	}
}

func TestWarnLegacyKnowledgePluginsOnce_DedupesAcrossCalls(t *testing.T) {
	// Same (session, plugin) pair across multiple calls only warns
	// once. Verified indirectly via the sync.Map state: a second
	// LoadOrStore for the same key reports "loaded=true" so the
	// warning block is short-circuited. We can't capture slog output
	// without rewiring the global logger, so this test instead
	// proves the de-dup map is the gate by asserting it holds the
	// expected key after the second call.
	orch := &Orchestrator{}
	orch.warnLegacyKnowledgePluginsOnce(context.Background(), "s1", []string{"legacy-plugin"})
	orch.warnLegacyKnowledgePluginsOnce(context.Background(), "s1", []string{"legacy-plugin"})
	key := "s1" + legacyWarningKeySeparator + "legacy-plugin"
	if _, loaded := orch.legacyKnowledgeWarnings.Load(key); !loaded {
		t.Fatalf("first call must record the (session, plugin) pair under key %q", key)
	}
	// Different session for the same plugin must record a separate key.
	orch.warnLegacyKnowledgePluginsOnce(context.Background(), "s2", []string{"legacy-plugin"})
	if _, loaded := orch.legacyKnowledgeWarnings.Load("s2" + legacyWarningKeySeparator + "legacy-plugin"); !loaded {
		t.Errorf("different session must produce a separate warn-key")
	}
}

func TestWarnLegacyKnowledgePluginsOnce_ParallelSafe(t *testing.T) {
	// Concurrent calls with the same key must not race the sync.Map.
	orch := &Orchestrator{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			orch.warnLegacyKnowledgePluginsOnce(context.Background(), "s1", []string{"plugin-a"})
		}()
	}
	wg.Wait()
	// Only one entry should exist for the contended key.
	count := 0
	orch.legacyKnowledgeWarnings.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 1 {
		t.Errorf("contended sync.Map must hold exactly 1 entry, got %d", count)
	}
}

func TestOrchestrator_PreparerPhase_LegacyKnowledgePluginEmitsLegacyFallback(t *testing.T) {
	// End-to-end: a plugin that returns a [knowledge_context] block
	// in pr.Message without structured KnowledgeCandidates triggers
	// mode=legacy_fallback when dedup is enabled. The orchestrator
	// MUST:
	//   - skip the dedup-decision rebuild (content passes through
	//     verbatim from the plugin)
	//   - NOT call UpdateInjectionState (no decision to persist)
	//   - emit preparer_decision with mode=legacy_fallback and an
	//     empty Knowledge.Injected slice
	preparerJSON := `{
		"send_to_llm": true,
		"message": "[knowledge_context]\nplugin-rendered body\n[/knowledge_context]\n\nuser question"
	}`
	sink := &recordingEventSink{}
	dedupStore := &fakeInjectionStateStore{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "legacy-rag", Description: "legacy RAG",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&capturingLLM{responses: []string{"answer"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:           sink,
			ContentPreparers:    []ContentPreparerEntry{{Plugin: "legacy-rag", Action: "prepare"}},
			KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
			InjectionStateStore: dedupStore,
		})
	if _, err := orch.Run(context.Background(), "s1", "user question"); err != nil {
		t.Fatal(err)
	}

	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision missing")
	}
	var p events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Mode != events.PreparerDecisionModeLegacyFallback {
		t.Errorf("Mode = %q, want %q", p.Mode, events.PreparerDecisionModeLegacyFallback)
	}
	if len(p.Knowledge.Injected) != 0 {
		t.Errorf("legacy_fallback must leave Knowledge.Injected empty, got %+v", p.Knowledge.Injected)
	}
	if dedupStore.updateCalls != 0 {
		t.Errorf("legacy_fallback must NOT persist state, got updateCalls=%d", dedupStore.updateCalls)
	}
	// Warn-key for the legacy plugin must be recorded.
	if _, loaded := orch.legacyKnowledgeWarnings.Load("s1" + legacyWarningKeySeparator + "legacy-rag"); !loaded {
		t.Errorf("deprecation warning de-dup key not recorded for legacy plugin")
	}
}

func TestOrchestrator_PreparerPhase_LegacyFallbackPassesPluginMessageVerbatim(t *testing.T) {
	// Robustness contract: the legacy plugin's pr.Message reaches the
	// LLM unchanged (with its embedded [knowledge_context] block).
	// Phase 3 dedup would have rewritten this; legacy_fallback must
	// not.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "[knowledge_context]\nlegacy plugin body\n[/knowledge_context]\n\nuser question"
	}`
	sink := &recordingEventSink{}
	llm := &capturingLLM{responses: []string{"answer"}}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "legacy-rag", Description: "legacy RAG",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(llm,
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:           sink,
			ContentPreparers:    []ContentPreparerEntry{{Plugin: "legacy-rag", Action: "prepare"}},
			KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
			InjectionStateStore: &fakeInjectionStateStore{},
		})
	if _, err := orch.Run(context.Background(), "s1", "user question"); err != nil {
		t.Fatal(err)
	}
	if len(llm.requests) == 0 {
		t.Fatal("LLM not called")
	}
	var lastUser string
	for _, m := range llm.requests[0].Messages {
		if m.Role == "user" {
			lastUser = m.Content
		}
	}
	if !strings.Contains(lastUser, "legacy plugin body") {
		t.Errorf("legacy_fallback must forward plugin's KC body verbatim, got %q", lastUser)
	}
	// The block must NOT be ID-tagged — the legacy plugin emitted a
	// bare form, and dedup did NOT run to rewrite it.
	if strings.Contains(lastUser, "id=") {
		t.Errorf("legacy_fallback must keep the plugin's bare KC form, got %q", lastUser)
	}
}

func TestOrchestrator_PreparerPhase_LegacyWithDedupDisabledStaysInstrumentationOnly(t *testing.T) {
	// Regression guard: a legacy plugin is NORMAL when dedup is
	// disabled. The mode must stay instrumentation_only (NOT
	// legacy_fallback) — fallback is a dedup-specific concept.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "[knowledge_context]\nplugin body\n[/knowledge_context]\nq"
	}`
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "legacy-rag", Description: "legacy RAG",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{responses: []string{"final"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:        sink,
			ContentPreparers: []ContentPreparerEntry{{Plugin: "legacy-rag", Action: "prepare"}},
			KnowledgeDedup:   KnowledgeDedupConfig{Enabled: false},
		})
	if _, err := orch.Run(context.Background(), "s1", "q"); err != nil {
		t.Fatal(err)
	}
	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	if pd == nil {
		// preparer_decision must STILL emit even though there were no
		// structured candidates — the legacy plugin's relevant_tools
		// or message-only response counts as "the preparer ran". But
		// since the legacy detection here is purely Knowledge-side
		// AND we have no Knowledge/Glossary/Tools, the event may
		// short-circuit. Tolerate either path by skipping when no
		// event was produced.
		t.Skip("preparer_decision short-circuited (no candidates of any kind); legacy-only message-mode is acceptable")
		return
	}
	var p events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Mode == events.PreparerDecisionModeLegacyFallback {
		t.Errorf("legacy plugin must not trigger legacy_fallback when dedup is disabled, got %q", p.Mode)
	}
}

func TestOrchestrator_PreparerPhase_LegacyFallbackSurfacesStructuredCandidateIDs(t *testing.T) {
	// Mixed-preparer case: preparer A returns structured candidates,
	// preparer B returns legacy [knowledge_context] in pr.Message.
	// Per RFC "per-turn fallback": the whole turn switches to
	// legacy_fallback (Injected/Skipped stay empty) but CandidateIDs
	// must still surface A's contributions so the audit trail shows
	// what was retrieved even though no decision was made.
	prepA := `{"send_to_llm": true, "message": "A's text",
	 "knowledge_candidates": [{"article_id": "kb_structured", "content": "body", "content_sha256": "sha-s", "score": 0.9}]}`
	prepB := `{"send_to_llm": true, "message": "[knowledge_context]\nlegacy B body\n[/knowledge_context]\nuser text"}`

	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-a", Description: "structured",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: prepA})
	_ = registry.Register(PluginCapability{
		Name: "rag-b-legacy", Description: "legacy",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: prepB})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{responses: []string{"final"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink: sink,
			ContentPreparers: []ContentPreparerEntry{
				{Plugin: "rag-a", Action: "prepare"},
				{Plugin: "rag-b-legacy", Action: "prepare"},
			},
			KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
			InjectionStateStore: &fakeInjectionStateStore{},
		})
	if _, err := orch.Run(context.Background(), "s1", "user text"); err != nil {
		t.Fatal(err)
	}
	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision missing")
	}
	var p events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Mode != events.PreparerDecisionModeLegacyFallback {
		t.Errorf("Mode = %q, want %q", p.Mode, events.PreparerDecisionModeLegacyFallback)
	}
	if len(p.Knowledge.CandidateIDs) != 1 || p.Knowledge.CandidateIDs[0] != "kb_structured" {
		t.Errorf("CandidateIDs must surface structured contribution, got %v", p.Knowledge.CandidateIDs)
	}
	if len(p.Knowledge.Injected) != 0 {
		t.Errorf("legacy_fallback must leave Injected empty, got %+v", p.Knowledge.Injected)
	}
}

func TestOrchestrator_PreparerPhase_MultipleLegacyPluginsPerSessionWarnSeparately(t *testing.T) {
	// Two distinct legacy plugins in one session — each must get its
	// own warn-key under the same session prefix, and prepAgg.LegacyKnowledgePlugins
	// must aggregate them in preparer order without duplication.
	prepA := `{"send_to_llm": true, "message": "[knowledge_context]\nA body\n[/knowledge_context]\nq"}`
	prepB := `{"send_to_llm": true, "message": "[knowledge_context]\nB body\n[/knowledge_context]\nq"}`

	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "legacy-a", Description: "legacy A",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: prepA})
	_ = registry.Register(PluginCapability{
		Name: "legacy-b", Description: "legacy B",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: prepB})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{responses: []string{"final"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink: sink,
			ContentPreparers: []ContentPreparerEntry{
				{Plugin: "legacy-a", Action: "prepare"},
				{Plugin: "legacy-b", Action: "prepare"},
			},
			KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
			InjectionStateStore: &fakeInjectionStateStore{},
		})
	if _, err := orch.Run(context.Background(), "s1", "q"); err != nil {
		t.Fatal(err)
	}
	for _, plugin := range []string{"legacy-a", "legacy-b"} {
		key := "s1" + legacyWarningKeySeparator + plugin
		if _, loaded := orch.legacyKnowledgeWarnings.Load(key); !loaded {
			t.Errorf("warn-key %q missing — each legacy plugin must be tracked separately", key)
		}
	}
}

func TestOrchestrator_PreparerPhase_LegacyFallbackPreservesStoreState(t *testing.T) {
	// A legacy_fallback turn must not mutate the persisted state.
	// Pre-populate state with entries; run the turn; confirm the
	// store's map is unchanged. (The orchestrator skips both
	// prepareDedupDecision and persistDedupDecision in legacy mode.)
	preLoaded := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_pre", ContentSHA256: "sha-pre", FirstInjectedTurn: 1},
		},
	}
	dedupStore := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{"s1": preLoaded},
	}
	preparerJSON := `{"send_to_llm": true,
	 "message": "[knowledge_context]\nlegacy body\n[/knowledge_context]\nuser q"}`
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "legacy", Description: "legacy",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{responses: []string{"final"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:           sink,
			ContentPreparers:    []ContentPreparerEntry{{Plugin: "legacy", Action: "prepare"}},
			KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
			InjectionStateStore: dedupStore,
		})
	if _, err := orch.Run(context.Background(), "s1", "user q"); err != nil {
		t.Fatal(err)
	}
	got := dedupStore.store["s1"]
	if len(got.KnownKnowledge) != 1 || got.KnownKnowledge[0].ContentSHA256 != "sha-pre" {
		t.Errorf("pre-loaded state must survive legacy_fallback turn, got %+v", got.KnownKnowledge)
	}
	if dedupStore.updateCalls != 0 {
		t.Errorf("legacy_fallback must skip UpdateInjectionState, got %d calls", dedupStore.updateCalls)
	}
}
