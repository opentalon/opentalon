package orchestrator

import (
	"strings"
)

// [knowledge_context] block parsing and rendering — RFC #249 Phase 3.
//
// The block format is the only Plugin→LLM bridge for retrieved RAG
// knowledge: the orchestrator splices a `[knowledge_context]…[/knowledge_context]`
// envelope into the current-turn user message, the LLM treats it as
// part of its question, and historical-message strip removes the
// envelope (but never the user text around it) on subsequent turns.
//
// Phase 3 extends the opening tag with optional `id="..."` and
// `sha="..."` attributes so the orchestrator's lazy reconciliation
// step can scan the visible message stream and tie each block back
// to a known_knowledge entry without storing redundant copies of
// the bodies. Plugins that haven't migrated to the candidate-list
// shape keep emitting bare `[knowledge_context]` opening tags; the
// parser recognizes both forms.
//
// Helpers live in their own file so the parser/strip/render logic is
// readable in one place and easy to test in isolation.

const (
	kcOpenPrefix = "[knowledge_context"
	kcCloseTag   = "[/knowledge_context]"
)

// parsedKCBlock is one [knowledge_context]…[/knowledge_context] block
// extracted from a message string. ArticleID and ContentSHA256 are
// populated when the opening tag carried the Phase-3 id="…" and
// sha="…" attributes; both fields stay empty for legacy untagged
// blocks. Start/End are byte offsets into the source string —
// inclusive at Start, exclusive at End — so callers can splice
// around the block without re-running the parser.
type parsedKCBlock struct {
	ArticleID     string
	ContentSHA256 string
	Body          string // text between opening `]` and `[/knowledge_context]`, not trimmed
	Start         int    // offset of the opening `[`
	End           int    // offset one past the closing `]`
}

// kcInjection is one article to render into a [knowledge_context]
// block. The Phase-3 dedup logic (later commit) builds the input
// slice from KnowledgeCandidate after the dedup decision; for
// Phase 4+ extensions any additional metadata that needs to appear
// inside the envelope would be added here.
type kcInjection struct {
	ArticleID     string
	ContentSHA256 string
	Body          string
}

// parseKnowledgeContextBlocks returns every [knowledge_context] block
// found in s, in document order. Both the legacy untagged form and
// the Phase-3 tagged form are recognized; the parser stops at the
// first malformed opening (no closing `]` for the opening tag or no
// `[/knowledge_context]` afterwards). Returns nil when there's
// nothing to parse.
//
// Known limitation: a body that contains the literal string
// `[/knowledge_context]` will be truncated at that point — the parser
// has no escape mechanism for closing tags inside bodies. RAG-retrieved
// knowledge articles practically never contain this string (it's an
// orchestrator-internal wire token, not user-authored content), and
// the Phase-3 reconciliation step catches the resulting drift on the
// next turn. If a future plugin starts emitting bodies that mention
// the closing tag, switch the renderer to length-prefixed bodies.
//
// The parser is intentionally non-allocating for the common
// "no KC block here" path: the strings.Index call short-circuits and
// returns immediately, the slice header on `nil` costs nothing.
func parseKnowledgeContextBlocks(s string) []parsedKCBlock {
	if !strings.Contains(s, kcOpenPrefix) {
		return nil
	}
	var blocks []parsedKCBlock
	cursor := 0
	for cursor < len(s) {
		rel := strings.Index(s[cursor:], kcOpenPrefix)
		if rel < 0 {
			break
		}
		absStart := cursor + rel
		// The opening tag ends at the first `]` after the prefix.
		// Anything between prefix and `]` is the optional attribute
		// region; legacy bare openings are simply `[knowledge_context]`.
		tagBodyStart := absStart + len(kcOpenPrefix)
		closeBracketRel := strings.Index(s[tagBodyStart:], "]")
		if closeBracketRel < 0 {
			break // unterminated opening — give up
		}
		tagBodyEnd := tagBodyStart + closeBracketRel
		bodyStart := tagBodyEnd + 1 // skip past the `]`
		closeRel := strings.Index(s[bodyStart:], kcCloseTag)
		if closeRel < 0 {
			break // unmatched opening — give up
		}
		bodyEnd := bodyStart + closeRel
		blocks = append(blocks, parsedKCBlock{
			ArticleID:     extractKCAttr(s[tagBodyStart:tagBodyEnd], "id"),
			ContentSHA256: extractKCAttr(s[tagBodyStart:tagBodyEnd], "sha"),
			Body:          s[bodyStart:bodyEnd],
			Start:         absStart,
			End:           bodyEnd + len(kcCloseTag),
		})
		cursor = bodyEnd + len(kcCloseTag)
	}
	return blocks
}

// renderKnowledgeContextBlock builds the orchestrator-side string the
// current-turn user message will carry. One envelope per article, in
// input order, separated by a blank line so the LLM sees the blocks
// as distinct. Empty slice returns the empty string so callers can
// concatenate the output without conditionals.
//
// Article IDs and SHAs are sanitized to drop any embedded `"` so the
// parser can rely on simple name="value" matching. In practice IDs
// are RAG-source slugs and SHAs are hex — neither carries quotes —
// but the sanitization keeps the contract robust against future
// plugins that might forward less constrained identifiers.
func renderKnowledgeContextBlock(articles []kcInjection) string {
	if len(articles) == 0 {
		return ""
	}
	var b strings.Builder
	for i, a := range articles {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(kcOpenPrefix)
		if id := sanitizeKCAttr(a.ArticleID); id != "" {
			b.WriteString(` id="`)
			b.WriteString(id)
			b.WriteString(`"`)
		}
		if sha := sanitizeKCAttr(a.ContentSHA256); sha != "" {
			b.WriteString(` sha="`)
			b.WriteString(sha)
			b.WriteString(`"`)
		}
		b.WriteString("]\n")
		b.WriteString(a.Body)
		b.WriteString("\n")
		b.WriteString(kcCloseTag)
	}
	return b.String()
}

// stripKnowledgeContext removes every [knowledge_context]…[/knowledge_context]
// block (legacy or tagged form, single or multiple) from s and trims
// surrounding whitespace. Used by the historical-strip path and the
// planner's pre-prompt sanitization to keep KC bodies from
// accumulating across turns.
func stripKnowledgeContext(s string) string {
	blocks := parseKnowledgeContextBlocks(s)
	if len(blocks) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, blk := range blocks {
		b.WriteString(s[last:blk.Start])
		last = blk.End
	}
	b.WriteString(s[last:])
	return strings.TrimSpace(b.String())
}

// extractKCAttr pulls the value of name="value" out of the opening-tag
// body. Tolerant of attribute order and surrounding whitespace.
// Empty string when the attribute is absent.
//
// The boundary check (whitespace before the attribute name) prevents
// `sha=` from matching the tail of a longer attribute like `extrasha=`;
// when an apparent match fails the boundary check the scan resumes
// past it so a legitimate later occurrence is still found.
func extractKCAttr(tagBody, name string) string {
	needle := name + `="`
	cursor := 0
	for cursor < len(tagBody) {
		rel := strings.Index(tagBody[cursor:], needle)
		if rel < 0 {
			return ""
		}
		idx := cursor + rel
		if idx > 0 {
			prev := tagBody[idx-1]
			if prev != ' ' && prev != '\t' {
				cursor = idx + 1
				continue
			}
		}
		valStart := idx + len(needle)
		end := strings.Index(tagBody[valStart:], `"`)
		if end < 0 {
			return ""
		}
		return tagBody[valStart : valStart+end]
	}
	return ""
}

// sanitizeKCAttr drops embedded `"` so the rendered attribute string
// stays parseable. Other whitespace and special characters survive —
// the parser only treats `"` as significant.
func sanitizeKCAttr(s string) string {
	if !strings.ContainsRune(s, '"') {
		return s
	}
	return strings.ReplaceAll(s, `"`, "")
}
