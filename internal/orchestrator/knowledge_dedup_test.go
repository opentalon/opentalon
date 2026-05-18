package orchestrator

import (
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

// defaultDedupCfg returns the RFC-default config so each test reads
// like the runtime would behave with an empty `knowledge_dedup:`
// YAML block (sans master-switch — every test enables the flag
// explicitly via its own decision).
func defaultDedupCfg() KnowledgeDedupConfig {
	return KnowledgeDedupConfig{
		Enabled:                true,
		ReinjectScoreThreshold: 0.85,
		ReinjectTopKForce:      3,
		CapPerTurn:             5,
	}
}

func TestApplyKnowledgeDedup_FirstTurnAllNew(t *testing.T) {
	candidates := []KnowledgeCandidate{
		{ArticleID: "kb_a", ContentSHA256: "aaa", Score: 0.91, Content: "first body"},
		{ArticleID: "kb_b", ContentSHA256: "bbb", Score: 0.55, Content: "second body"},
	}
	d := applyKnowledgeDedup(candidates, state.InjectionState{}, defaultDedupCfg(), 1)
	if len(d.Injected) != 2 {
		t.Fatalf("first turn must inject all candidates, got %d", len(d.Injected))
	}
	for _, r := range d.InjectedReasons {
		if r != events.PreparerDecisionReasonNew {
			t.Errorf("first-turn reason must be %q, got %q", events.PreparerDecisionReasonNew, r)
		}
	}
	if len(d.Skipped) != 0 {
		t.Errorf("first turn must skip nothing, got %d entries", len(d.Skipped))
	}
	if len(d.UpdatedState.KnownKnowledge) != 2 {
		t.Errorf("UpdatedState must record all candidates, got %d", len(d.UpdatedState.KnownKnowledge))
	}
	if d.UpdatedState.KnownKnowledge[0].FirstInjectedTurn != 1 {
		t.Errorf("FirstInjectedTurn = %d, want 1", d.UpdatedState.KnownKnowledge[0].FirstInjectedTurn)
	}
}

func TestApplyKnowledgeDedup_KnownSHAsSkipped(t *testing.T) {
	existing := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
		},
	}
	// Place candidate AFTER ReinjectTopKForce so top_k_force doesn't fire.
	// Score below ReinjectScoreThreshold so score_override doesn't fire.
	candidates := []KnowledgeCandidate{
		{ArticleID: "kb_x", ContentSHA256: "xxx", Score: 0.6, Content: "x"},
		{ArticleID: "kb_y", ContentSHA256: "yyy", Score: 0.6, Content: "y"},
		{ArticleID: "kb_z", ContentSHA256: "zzz", Score: 0.6, Content: "z"},
		{ArticleID: "kb_a", ContentSHA256: "aaa", Score: 0.6, Content: "a"}, // index 3 — outside top-K-force=3, score below threshold
	}
	d := applyKnowledgeDedup(candidates, existing, defaultDedupCfg(), 2)
	// Expect: kb_x/y/z injected as new, kb_a skipped as already_known.
	if len(d.Injected) != 3 {
		t.Fatalf("got %d injected, want 3", len(d.Injected))
	}
	if len(d.Skipped) != 1 || d.Skipped[0].ArticleID != "kb_a" {
		t.Fatalf("expected kb_a skipped, got %+v", d.Skipped)
	}
	if d.SkippedReasons[0] != events.PreparerDecisionReasonAlreadyKnown {
		t.Errorf("skip reason = %q, want %q", d.SkippedReasons[0], events.PreparerDecisionReasonAlreadyKnown)
	}
}

func TestApplyKnowledgeDedup_ScoreOverrideReinjectsKnown(t *testing.T) {
	existing := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
		},
	}
	// Place kb_a outside top-K-force (index 3) so only score_override can rescue it.
	candidates := []KnowledgeCandidate{
		{ArticleID: "kb_x", ContentSHA256: "xxx", Score: 0.5},
		{ArticleID: "kb_y", ContentSHA256: "yyy", Score: 0.5},
		{ArticleID: "kb_z", ContentSHA256: "zzz", Score: 0.5},
		{ArticleID: "kb_a", ContentSHA256: "aaa", Score: 0.92, Content: "high-scoring re-inject"},
	}
	d := applyKnowledgeDedup(candidates, existing, defaultDedupCfg(), 2)
	if len(d.Injected) != 4 {
		t.Fatalf("got %d injected, want 4 (3 new + 1 score_override)", len(d.Injected))
	}
	if d.InjectedReasons[3] != events.PreparerDecisionReasonScoreOverride {
		t.Errorf("last reason = %q, want %q", d.InjectedReasons[3], events.PreparerDecisionReasonScoreOverride)
	}
	if len(d.ScoreOverrides) != 1 {
		t.Fatalf("expected 1 score-override record, got %d", len(d.ScoreOverrides))
	}
	if d.ScoreOverrides[0].ArticleID != "kb_a" || d.ScoreOverrides[0].CurrentScore != 0.92 || d.ScoreOverrides[0].Threshold != 0.85 {
		t.Errorf("score-override payload mismatch: %+v", d.ScoreOverrides[0])
	}
}

func TestApplyKnowledgeDedup_TopKForceReinjectsKnown(t *testing.T) {
	existing := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
			{ArticleID: "kb_b", ContentSHA256: "bbb", FirstInjectedTurn: 1},
		},
	}
	// Known candidate at index 0 with score below threshold — only
	// top_k_force can carry it across.
	candidates := []KnowledgeCandidate{
		{ArticleID: "kb_a", ContentSHA256: "aaa", Score: 0.4, Content: "carried by top-K"},
		{ArticleID: "kb_b", ContentSHA256: "bbb", Score: 0.4, Content: "carried by top-K"},
		{ArticleID: "kb_b_other", ContentSHA256: "bb2", Score: 0.4},
		{ArticleID: "kb_c", ContentSHA256: "ccc", Score: 0.4},
	}
	d := applyKnowledgeDedup(candidates, existing, defaultDedupCfg(), 2)
	// Indexes 0,1 should be top_k_force; index 2 new; index 3 new.
	if len(d.Injected) != 4 {
		t.Fatalf("got %d injected, want 4", len(d.Injected))
	}
	if d.InjectedReasons[0] != events.PreparerDecisionReasonTopKForce || d.InjectedReasons[1] != events.PreparerDecisionReasonTopKForce {
		t.Errorf("top-K reasons mismatch: %v", d.InjectedReasons)
	}
	if d.InjectedReasons[2] != events.PreparerDecisionReasonNew || d.InjectedReasons[3] != events.PreparerDecisionReasonNew {
		t.Errorf("trailing reasons should be 'new': %v", d.InjectedReasons)
	}
	if len(d.ScoreOverrides) != 0 {
		t.Errorf("top-K-force must not record as score-override, got %+v", d.ScoreOverrides)
	}
}

func TestApplyKnowledgeDedup_CapPerTurnExceeded(t *testing.T) {
	cfg := defaultDedupCfg()
	cfg.CapPerTurn = 2
	candidates := []KnowledgeCandidate{
		{ArticleID: "kb_a", ContentSHA256: "aaa", Score: 0.9},
		{ArticleID: "kb_b", ContentSHA256: "bbb", Score: 0.9},
		{ArticleID: "kb_c", ContentSHA256: "ccc", Score: 0.9},
		{ArticleID: "kb_d", ContentSHA256: "ddd", Score: 0.9},
	}
	d := applyKnowledgeDedup(candidates, state.InjectionState{}, cfg, 1)
	if len(d.Injected) != 2 {
		t.Fatalf("cap=2 must produce 2 injections, got %d", len(d.Injected))
	}
	if len(d.Skipped) != 2 {
		t.Fatalf("cap=2 with 4 candidates must skip 2, got %d", len(d.Skipped))
	}
	for _, r := range d.SkippedReasons {
		if r != events.PreparerDecisionReasonCapExceeded {
			t.Errorf("cap-exceeded reason expected, got %q", r)
		}
	}
	// All four still recorded in UpdatedState — RFC: the LLM "knows
	// about" rejected-because-capped ones too.
	if len(d.UpdatedState.KnownKnowledge) != 4 {
		t.Errorf("UpdatedState must record all 4 candidates, got %d", len(d.UpdatedState.KnownKnowledge))
	}
}

func TestApplyKnowledgeDedup_NoCandidatesReturnsEmptyDecision(t *testing.T) {
	d := applyKnowledgeDedup(nil, state.InjectionState{}, defaultDedupCfg(), 1)
	if len(d.Injected) != 0 || len(d.Skipped) != 0 || len(d.UpdatedState.KnownKnowledge) != 0 {
		t.Errorf("empty input must yield empty decision, got %+v", d)
	}
}

func TestApplyKnowledgeDedup_SameSHAInInputOnlyOneStateEntry(t *testing.T) {
	// A plugin returning the same SHA twice within a turn (e.g. the
	// same article hit by two RAG queries) must produce a single
	// known_knowledge entry — otherwise the state grows unboundedly.
	candidates := []KnowledgeCandidate{
		{ArticleID: "kb_dup", ContentSHA256: "dup", Score: 0.7, Content: "body"},
		{ArticleID: "kb_dup", ContentSHA256: "dup", Score: 0.7, Content: "body"},
	}
	d := applyKnowledgeDedup(candidates, state.InjectionState{}, defaultDedupCfg(), 1)
	// First instance is "new"; second is treated as already-known
	// because the in-turn map was updated. Skipped because below
	// the score_override threshold and at index 1 (top_k_force
	// covers indices 0..2 so index 1 IS top_k_force) — actually
	// at top_k_force=3 the second candidate hits the top_k_force
	// path because seen==true at index 1.
	if len(d.UpdatedState.KnownKnowledge) != 1 {
		t.Errorf("same SHA twice must produce 1 known entry, got %d", len(d.UpdatedState.KnownKnowledge))
	}
}

func TestApplyKnowledgeDedup_PreservesPhase4KnownTools(t *testing.T) {
	// Phase 3 must not clobber Phase-4 KnownTools entries when it
	// rewrites the state.
	existing := state.InjectionState{
		KnownTools: []state.KnownToolEntry{
			{ToolName: "timly__keep", Tier: state.KnownToolTier1, LRURank: 4},
		},
	}
	candidates := []KnowledgeCandidate{
		{ArticleID: "kb_a", ContentSHA256: "aaa", Content: "x"},
	}
	d := applyKnowledgeDedup(candidates, existing, defaultDedupCfg(), 1)
	if len(d.UpdatedState.KnownTools) != 1 || d.UpdatedState.KnownTools[0].ToolName != "timly__keep" {
		t.Errorf("Phase-4 KnownTools must survive Phase-3 update, got %+v", d.UpdatedState.KnownTools)
	}
}

func TestApplyDedupToContent_EmptyInjectionsStripsPluginKC(t *testing.T) {
	content := "[knowledge_context]\nold body\n[/knowledge_context]\n\nuser question"
	out := applyDedupToContent(content, nil)
	if out != "user question" {
		t.Errorf("empty injections must strip and leave user text, got %q", out)
	}
}

func TestApplyDedupToContent_NoKCPassthrough(t *testing.T) {
	const content = "plain user question with no plugin KC"
	if out := applyDedupToContent(content, nil); out != content {
		t.Errorf("no-KC passthrough mutated: got %q, want %q", out, content)
	}
}

func TestApplyDedupToContent_RebuildsKCFromDecision(t *testing.T) {
	content := "[knowledge_context]\nold plugin KC\n[/knowledge_context]\n\nuser asks something"
	injections := []kcInjection{
		{ArticleID: "kb_new", ContentSHA256: "abc", Body: "fresh deduped body"},
	}
	out := applyDedupToContent(content, injections)
	if strings.Contains(out, "old plugin KC") {
		t.Errorf("dedup must drop plugin's KC, got %q", out)
	}
	if !strings.Contains(out, "fresh deduped body") || !strings.Contains(out, `id="kb_new"`) {
		t.Errorf("dedup must render new KC with attributes, got %q", out)
	}
	if !strings.Contains(out, "user asks something") {
		t.Errorf("user text must be preserved, got %q", out)
	}
	// Verify ordering: KC block precedes user text.
	if strings.Index(out, "fresh deduped body") > strings.Index(out, "user asks something") {
		t.Errorf("KC block must come before user text, got %q", out)
	}
}

func TestApplyDedupToContent_EmptyContentWithInjections(t *testing.T) {
	// Pathological but defined: plugin returned only a KC block as
	// pr.Message — after strip the user text is empty. The rendered
	// KC becomes the entire content.
	const content = "[knowledge_context]\nold\n[/knowledge_context]"
	injections := []kcInjection{{ArticleID: "kb_x", ContentSHA256: "x", Body: "deduped"}}
	out := applyDedupToContent(content, injections)
	if !strings.HasPrefix(out, "[knowledge_context") || !strings.Contains(out, "deduped") {
		t.Errorf("KC-only content must result in pure rendered KC, got %q", out)
	}
	if strings.Contains(out, "\n\n") {
		t.Errorf("KC-only content must not have trailing blank line, got %q", out)
	}
}

func TestDedupInjectedItems_BuildsEventPayload(t *testing.T) {
	d := &knowledgeDedupDecision{
		Injected: []KnowledgeCandidate{
			{ArticleID: "kb_a", ContentSHA256: "aaa"},
			{ArticleID: "kb_b", ContentSHA256: "bbb"},
		},
		InjectedReasons: []string{events.PreparerDecisionReasonNew, events.PreparerDecisionReasonScoreOverride},
	}
	got := dedupInjectedItems(d)
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	if got[0].ArticleID != "kb_a" || got[0].Reason != events.PreparerDecisionReasonNew {
		t.Errorf("first item mismatch: %+v", got[0])
	}
	if got[1].Reason != events.PreparerDecisionReasonScoreOverride {
		t.Errorf("second item reason = %q, want %q", got[1].Reason, events.PreparerDecisionReasonScoreOverride)
	}
}

func TestKnowledgeDedupDecision_InjectedBytesSumsContentLengths(t *testing.T) {
	d := &knowledgeDedupDecision{
		Injected: []KnowledgeCandidate{
			{Content: "12345"},         // 5 bytes
			{Content: "Hello, world!"}, // 13 bytes
			{Content: ""},              // 0 bytes
		},
	}
	if got := d.InjectedBytes(); got != 18 {
		t.Errorf("InjectedBytes = %d, want 18", got)
	}
}

func TestTurnNumberForDedup_EmptySessionStartsAtOne(t *testing.T) {
	sessions := state.NewSessionStore("")
	sessions.Create("s", "", "")
	orch := &Orchestrator{sessions: sessions}
	if got := orch.turnNumberForDedup("s"); got != 1 {
		t.Errorf("empty session turn = %d, want 1", got)
	}
}

func TestTurnNumberForDedup_CountsUserMessagesOnly(t *testing.T) {
	sessions := state.NewSessionStore("")
	sessions.Create("s", "", "")
	// 2 user messages + 1 assistant + 1 tool → turn 3 (1 base + 2 user).
	_ = sessions.AddMessage("s", provider.Message{Role: provider.RoleUser, Content: "q1"})
	_ = sessions.AddMessage("s", provider.Message{Role: provider.RoleAssistant, Content: "a1"})
	_ = sessions.AddMessage("s", provider.Message{Role: provider.RoleUser, Content: "q2"})
	_ = sessions.AddMessage("s", provider.Message{Role: provider.RoleTool, Content: "tool-result"})
	orch := &Orchestrator{sessions: sessions}
	if got := orch.turnNumberForDedup("s"); got != 3 {
		t.Errorf("turn = %d, want 3 (1 base + 2 user messages)", got)
	}
}

func TestTurnNumberForDedup_MissingSessionFallsBackToOne(t *testing.T) {
	sessions := state.NewSessionStore("")
	orch := &Orchestrator{sessions: sessions}
	if got := orch.turnNumberForDedup("does-not-exist"); got != 1 {
		t.Errorf("missing session must fall back to 1, got %d", got)
	}
}
