package orchestrator

import (
	"strings"
	"testing"
)

func TestParseKnowledgeContextBlocks_LegacyBareOpening(t *testing.T) {
	const msg = "before\n[knowledge_context]\nbody one\n[/knowledge_context]\nafter"
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	got := blocks[0]
	if got.ArticleID != "" || got.ContentSHA256 != "" {
		t.Errorf("legacy form must yield empty id/sha, got %+v", got)
	}
	if strings.TrimSpace(got.Body) != "body one" {
		t.Errorf("body mismatch: %q", got.Body)
	}
	if msg[got.Start:got.End] != "[knowledge_context]\nbody one\n[/knowledge_context]" {
		t.Errorf("Start/End slice mismatch: %q", msg[got.Start:got.End])
	}
}

func TestParseKnowledgeContextBlocks_TaggedOpening(t *testing.T) {
	const msg = `[knowledge_context id="kb_recurring-tickets" sha="9f3a"]
recurring tickets body
[/knowledge_context]`
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	got := blocks[0]
	if got.ArticleID != "kb_recurring-tickets" {
		t.Errorf("ArticleID = %q, want kb_recurring-tickets", got.ArticleID)
	}
	if got.ContentSHA256 != "9f3a" {
		t.Errorf("ContentSHA256 = %q, want 9f3a", got.ContentSHA256)
	}
	if strings.TrimSpace(got.Body) != "recurring tickets body" {
		t.Errorf("body mismatch: %q", got.Body)
	}
}

func TestParseKnowledgeContextBlocks_MultipleBlocks(t *testing.T) {
	const msg = `intro
[knowledge_context id="kb_a" sha="aaa"]
first body
[/knowledge_context]
middle
[knowledge_context]
second legacy body
[/knowledge_context]
trailer`
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[0].ArticleID != "kb_a" || blocks[0].ContentSHA256 != "aaa" {
		t.Errorf("first block: %+v", blocks[0])
	}
	if blocks[1].ArticleID != "" || blocks[1].ContentSHA256 != "" {
		t.Errorf("second block (legacy) must have empty id/sha, got %+v", blocks[1])
	}
	if strings.TrimSpace(blocks[1].Body) != "second legacy body" {
		t.Errorf("second body: %q", blocks[1].Body)
	}
}

func TestParseKnowledgeContextBlocks_AttributeOrderIndependent(t *testing.T) {
	const msg = `[knowledge_context sha="abc" id="kb_x"]
body
[/knowledge_context]`
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].ArticleID != "kb_x" || blocks[0].ContentSHA256 != "abc" {
		t.Errorf("attribute extraction failed: %+v", blocks[0])
	}
}

func TestParseKnowledgeContextBlocks_MissingAttribute(t *testing.T) {
	const msg = `[knowledge_context id="kb_only"]
body without sha
[/knowledge_context]`
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].ArticleID != "kb_only" {
		t.Errorf("ArticleID = %q, want kb_only", blocks[0].ArticleID)
	}
	if blocks[0].ContentSHA256 != "" {
		t.Errorf("ContentSHA256 must stay empty when attribute absent, got %q", blocks[0].ContentSHA256)
	}
}

func TestParseKnowledgeContextBlocks_MalformedReturnsParsedPrefix(t *testing.T) {
	// Unmatched opening at the end shouldn't lose earlier well-formed blocks.
	const msg = `[knowledge_context]
first
[/knowledge_context]
[knowledge_context id="kb_dangling"
truncated body without close`
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1 (the dangling block is dropped), have %+v", len(blocks), blocks)
	}
	if strings.TrimSpace(blocks[0].Body) != "first" {
		t.Errorf("first block body mismatch: %q", blocks[0].Body)
	}
}

func TestParseKnowledgeContextBlocks_NoOpeningReturnsNil(t *testing.T) {
	if blocks := parseKnowledgeContextBlocks("plain text"); blocks != nil {
		t.Errorf("expected nil for KC-free string, got %+v", blocks)
	}
	if blocks := parseKnowledgeContextBlocks(""); blocks != nil {
		t.Errorf("expected nil for empty string, got %+v", blocks)
	}
}

func TestExtractKCAttr_RejectsSubstringMatches(t *testing.T) {
	// Without a word-boundary check, "sha" would falsely match the
	// trailing characters of e.g. `extrasha="..."`. Guard against that
	// regression — the boundary check requires whitespace before the
	// attribute name.
	body := `extrasha="bogus" sha="real"`
	if got := extractKCAttr(body, "sha"); got != "real" {
		t.Errorf("extractKCAttr(sha) = %q, want real", got)
	}
}

func TestRenderKnowledgeContextBlock_RoundTripsThroughParser(t *testing.T) {
	in := []kcInjection{
		{ArticleID: "kb_a", ContentSHA256: "aaa", Body: "first body"},
		{ArticleID: "kb_b", ContentSHA256: "bbb", Body: "second body"},
	}
	rendered := renderKnowledgeContextBlock(in)
	if rendered == "" {
		t.Fatal("renderKnowledgeContextBlock produced empty string for non-empty input")
	}
	blocks := parseKnowledgeContextBlocks(rendered)
	if len(blocks) != 2 {
		t.Fatalf("round-trip produced %d blocks, want 2", len(blocks))
	}
	for i, want := range in {
		got := blocks[i]
		if got.ArticleID != want.ArticleID || got.ContentSHA256 != want.ContentSHA256 {
			t.Errorf("block %d: id/sha mismatch — got %+v, want %+v", i, got, want)
		}
		if strings.TrimSpace(got.Body) != want.Body {
			t.Errorf("block %d: body = %q, want %q", i, got.Body, want.Body)
		}
	}
}

func TestRenderKnowledgeContextBlock_EmptyInput(t *testing.T) {
	if got := renderKnowledgeContextBlock(nil); got != "" {
		t.Errorf("nil input must render to empty string, got %q", got)
	}
	if got := renderKnowledgeContextBlock([]kcInjection{}); got != "" {
		t.Errorf("empty slice must render to empty string, got %q", got)
	}
}

func TestRenderKnowledgeContextBlock_SkipsEmptyAttributes(t *testing.T) {
	// A legacy-style injection (no id/sha) must render as the bare
	// `[knowledge_context]` form so downstream tooling unaware of the
	// Phase-3 attributes still sees a recognizable envelope.
	rendered := renderKnowledgeContextBlock([]kcInjection{{Body: "legacy"}})
	if !strings.HasPrefix(rendered, "[knowledge_context]\n") {
		t.Errorf("missing-attribute injection must render bare opening, got %q", rendered)
	}
}

func TestRenderKnowledgeContextBlock_SanitizesEmbeddedQuotes(t *testing.T) {
	rendered := renderKnowledgeContextBlock([]kcInjection{{
		ArticleID:     `kb_"injected"`,
		ContentSHA256: `sha"with"quote`,
		Body:          "body",
	}})
	// Embedded quotes must be dropped so the parser's name="value"
	// matching still works. Confirm by round-tripping.
	blocks := parseKnowledgeContextBlocks(rendered)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1 — render: %q", len(blocks), rendered)
	}
	if strings.Contains(blocks[0].ArticleID, `"`) || strings.Contains(blocks[0].ContentSHA256, `"`) {
		t.Errorf("sanitization left embedded quotes: %+v", blocks[0])
	}
}

func TestStripKnowledgeContext_LegacyBare(t *testing.T) {
	const msg = "[knowledge_context]\nbody\n[/knowledge_context]\nuser question"
	if got := stripKnowledgeContext(msg); got != "user question" {
		t.Errorf("strip(legacy) = %q, want %q", got, "user question")
	}
}

func TestStripKnowledgeContext_Tagged(t *testing.T) {
	const msg = `[knowledge_context id="kb_x" sha="abc"]
tagged body
[/knowledge_context]
user question`
	if got := stripKnowledgeContext(msg); got != "user question" {
		t.Errorf("strip(tagged) = %q, want %q", got, "user question")
	}
}

func TestStripKnowledgeContext_MultipleBlocks(t *testing.T) {
	const msg = `[knowledge_context]
first
[/knowledge_context]
middle text
[knowledge_context id="kb_b" sha="bbb"]
second
[/knowledge_context]
trailing question`
	got := stripKnowledgeContext(msg)
	want := "middle text\n\ntrailing question"
	if got != want {
		t.Errorf("strip(multi) = %q, want %q", got, want)
	}
}

func TestStripKnowledgeContext_NoBlockIsPassthrough(t *testing.T) {
	const msg = "plain user message"
	if got := stripKnowledgeContext(msg); got != msg {
		t.Errorf("passthrough mutated: got %q, want %q", got, msg)
	}
}

func TestStripKnowledgeContext_BlockOnlyTrimsToEmpty(t *testing.T) {
	const msg = "[knowledge_context]\nbody\n[/knowledge_context]"
	if got := stripKnowledgeContext(msg); got != "" {
		t.Errorf("KC-only message must strip to empty, got %q", got)
	}
}

func TestParseKnowledgeContextBlocks_ToleratesExtraSpacingInOpeningTag(t *testing.T) {
	// The wire format the renderer produces is single-spaced, but a
	// future plugin might emit `[knowledge_context  id="x"  sha="y"  ]`
	// with extra whitespace. The parser must accept it so backwards-compat
	// with non-canonical encodings doesn't break reconciliation.
	const msg = `[knowledge_context  id="kb_loose"   sha="abc"   ]
body
[/knowledge_context]`
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].ArticleID != "kb_loose" || blocks[0].ContentSHA256 != "abc" {
		t.Errorf("extra-spacing extraction failed: %+v", blocks[0])
	}
}

func TestParseKnowledgeContextBlocks_EmptyBody(t *testing.T) {
	// `[knowledge_context]…[/knowledge_context]` with a zero-byte body
	// is a legitimate construction (e.g. the renderer caller passed
	// an empty article body by mistake). The parser must accept it
	// rather than treat it as malformed.
	const msg = "[knowledge_context id=\"kb_empty\" sha=\"e\"][/knowledge_context]"
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].Body != "" {
		t.Errorf("empty body expected, got %q", blocks[0].Body)
	}
	if blocks[0].ArticleID != "kb_empty" {
		t.Errorf("ArticleID = %q, want kb_empty", blocks[0].ArticleID)
	}
}

func TestParseKnowledgeContextBlocks_AdjacentBlocksWithoutSeparator(t *testing.T) {
	// Two blocks back-to-back with zero whitespace between the closing
	// of the first and the opening of the second. Production code emits
	// "\n\n" between them, but the parser shouldn't require it.
	const msg = `[knowledge_context id="kb_a" sha="aaa"]first[/knowledge_context][knowledge_context id="kb_b" sha="bbb"]second[/knowledge_context]`
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[0].ArticleID != "kb_a" || blocks[1].ArticleID != "kb_b" {
		t.Errorf("adjacent-block ids: got %q / %q, want kb_a / kb_b", blocks[0].ArticleID, blocks[1].ArticleID)
	}
	if blocks[0].Body != "first" || blocks[1].Body != "second" {
		t.Errorf("adjacent-block bodies: got %q / %q, want first / second", blocks[0].Body, blocks[1].Body)
	}
}

func TestParseKnowledgeContextBlocks_BodyContainingLiteralCloseTag(t *testing.T) {
	// Documents the known parser limitation: a body that mentions
	// `[/knowledge_context]` literally truncates at that point.
	// RAG-retrieved articles practically never do this, and the
	// reconciliation step catches the resulting drift next turn — but
	// the test pins the behaviour so a future "escape closing tag"
	// fix doesn't silently change observable semantics.
	const msg = "[knowledge_context id=\"kb_meta\" sha=\"abc\"]body before [/knowledge_context] trailing"
	blocks := parseKnowledgeContextBlocks(msg)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].Body != "body before " {
		t.Errorf("body should truncate at first close tag, got %q", blocks[0].Body)
	}
}
