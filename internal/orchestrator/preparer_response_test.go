package orchestrator

import (
	"encoding/json"
	"testing"
)

// preparerResponse is the JSON contract between RAG plugins (weaviate,
// future per-tenant corpora, …) and Core's preparer-loop. RFC #249 Phase
// 2 extends it with optional `knowledge_candidates` / `glossary_candidates`
// / `tool_candidates` slices plus a `retrieval_metrics` summary so Phase
// 3+ can apply session-aware dedup + tier-promotion logic. The contract
// must stay strictly backwards-compatible: legacy plugins emitting only
// the old fields keep working as if nothing changed.

func TestPreparerResponse_LegacyShapeStillUnmarshals(t *testing.T) {
	// A pre-RFC plugin response: only message + send_to_llm +
	// relevant_tools. The orchestrator must continue to parse this
	// without errors and leave the new candidate slices as nil so
	// existing call sites (which check `len(pr.KnowledgeCandidates) > 0`)
	// fall through to the legacy path.
	raw := `{
		"send_to_llm": true,
		"message": "user content with [knowledge_context]...[/knowledge_context]",
		"relevant_tools": ["plugin.show", "plugin.list"]
	}`

	var pr preparerResponse
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		t.Fatalf("legacy shape failed to unmarshal: %v", err)
	}
	if pr.SendToLLM == nil || !*pr.SendToLLM {
		t.Errorf("SendToLLM = %v, want pointer-to-true", pr.SendToLLM)
	}
	if pr.Message == "" {
		t.Error("Message empty after unmarshal")
	}
	if len(pr.RelevantTools) != 2 {
		t.Errorf("RelevantTools len = %d, want 2", len(pr.RelevantTools))
	}
	if pr.KnowledgeCandidates != nil || pr.GlossaryCandidates != nil || pr.ToolCandidates != nil {
		t.Errorf("legacy plugin must leave candidate slices nil; got %d/%d/%d",
			len(pr.KnowledgeCandidates), len(pr.GlossaryCandidates), len(pr.ToolCandidates))
	}
	if pr.RetrievalMetrics != nil {
		t.Errorf("legacy plugin must leave RetrievalMetrics nil; got %+v", pr.RetrievalMetrics)
	}
}

func TestPreparerResponse_FullCandidatesShape(t *testing.T) {
	// A new RFC-aware plugin response: structured candidates plus
	// retrieval_metrics. The orchestrator must round-trip every field
	// so Phase 3's dedup logic sees what the plugin sent.
	raw := `{
		"send_to_llm": true,
		"message": "user content",
		"knowledge_candidates": [
			{
				"article_id": "kb_recurring",
				"title": "Recurring Tickets",
				"content": "Body of the article...",
				"content_sha256": "9f3a000000000000000000000000000000000000000000000000000000000000",
				"score": 0.91,
				"source": "knowledge_base",
				"position_in_results": 0
			}
		],
		"glossary_candidates": [
			{
				"term": "ticket",
				"content": "A ticket is...",
				"score": 0.71
			}
		],
		"tool_candidates": [
			{"tool_name": "timly.list-tickets", "score": 0.89, "position_in_results": 0},
			{"tool_name": "timly.show-ticket", "score": 0.71, "position_in_results": 1}
		],
		"retrieval_metrics": {
			"knowledge": {"search_text_source": "enriched", "top_k": 5, "min_score": 0.45, "latency_ms": 142},
			"tools":     {"search_text_source": "user_input", "top_k": 8, "min_score": 0.50, "latency_ms": 98}
		}
	}`

	var pr preparerResponse
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		t.Fatalf("RFC shape failed to unmarshal: %v", err)
	}

	if len(pr.KnowledgeCandidates) != 1 {
		t.Fatalf("KnowledgeCandidates len = %d, want 1", len(pr.KnowledgeCandidates))
	}
	k := pr.KnowledgeCandidates[0]
	if k.ArticleID != "kb_recurring" || k.Title != "Recurring Tickets" || k.Score != 0.91 {
		t.Errorf("KnowledgeCandidate scalar fields mismatch: %+v", k)
	}
	if k.ContentSHA256 == "" || k.Content == "" {
		t.Error("KnowledgeCandidate.Content / ContentSHA256 must round-trip; got empty values")
	}
	if k.PositionInResults != 0 || k.Source != "knowledge_base" {
		t.Errorf("KnowledgeCandidate Position/Source mismatch: pos=%d source=%q", k.PositionInResults, k.Source)
	}

	if len(pr.GlossaryCandidates) != 1 || pr.GlossaryCandidates[0].Term != "ticket" {
		t.Errorf("GlossaryCandidates mismatch: %+v", pr.GlossaryCandidates)
	}

	if len(pr.ToolCandidates) != 2 {
		t.Fatalf("ToolCandidates len = %d, want 2", len(pr.ToolCandidates))
	}
	if pr.ToolCandidates[1].PositionInResults != 1 {
		t.Errorf("ToolCandidates[1].PositionInResults = %d, want 1", pr.ToolCandidates[1].PositionInResults)
	}

	if pr.RetrievalMetrics == nil {
		t.Fatal("RetrievalMetrics nil; want populated")
	}
	if pr.RetrievalMetrics.Knowledge == nil || pr.RetrievalMetrics.Knowledge.LatencyMS != 142 {
		t.Errorf("RetrievalMetrics.Knowledge mismatch: %+v", pr.RetrievalMetrics.Knowledge)
	}
	if pr.RetrievalMetrics.Glossary != nil {
		t.Errorf("RetrievalMetrics.Glossary must stay nil when plugin omits it; got %+v", pr.RetrievalMetrics.Glossary)
	}
	if pr.RetrievalMetrics.Tools == nil || pr.RetrievalMetrics.Tools.TopK != 8 {
		t.Errorf("RetrievalMetrics.Tools mismatch: %+v", pr.RetrievalMetrics.Tools)
	}
}

func TestPreparerResponse_PartialCandidatesOmitted(t *testing.T) {
	// A plugin can opt into the new contract per-corpus — e.g. ship
	// knowledge candidates structured while leaving tools on the legacy
	// `relevant_tools` path. The orchestrator must accept that and not
	// confuse "absent" with "empty".
	raw := `{
		"message": "...",
		"knowledge_candidates": [{"article_id": "kb_a", "content": "body", "score": 0.7}],
		"relevant_tools": ["plugin.show"]
	}`

	var pr preparerResponse
	if err := json.Unmarshal([]byte(raw), &pr); err != nil {
		t.Fatalf("partial shape failed to unmarshal: %v", err)
	}
	if len(pr.KnowledgeCandidates) != 1 {
		t.Errorf("KnowledgeCandidates len = %d, want 1", len(pr.KnowledgeCandidates))
	}
	if pr.GlossaryCandidates != nil {
		t.Errorf("GlossaryCandidates must stay nil when omitted, got len=%d", len(pr.GlossaryCandidates))
	}
	if pr.ToolCandidates != nil {
		t.Errorf("ToolCandidates must stay nil when omitted, got len=%d", len(pr.ToolCandidates))
	}
	if len(pr.RelevantTools) != 1 || pr.RelevantTools[0] != "plugin.show" {
		t.Errorf("legacy relevant_tools must still parse alongside structured candidates; got %v", pr.RelevantTools)
	}
}
