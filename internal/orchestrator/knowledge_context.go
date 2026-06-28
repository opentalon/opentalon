package orchestrator

import "strings"

// [knowledge_context] block parsing and stripping.
//
// Knowledge is pull-only: the model discovers articles from the
// system-prompt catalog (titles + slugs) and fetches bodies on demand
// via the ask_knowledge tool. The orchestrator never auto-injects
// retrieved article bodies into the user turn. stripKnowledgeContext
// remains as a fail-safe: it removes any stray
// `[knowledge_context]…[/knowledge_context]` envelope (legacy or tagged
// form) from messages so nothing leaks into the LLM turn or the stored
// history.
//
// Some opening tags carry optional `id="..."` and `sha="..."`
// attributes; the parser recognizes both the tagged and the bare
// `[knowledge_context]` forms.
//
// Helpers live in their own file so the parser/strip logic is
// readable in one place and easy to test in isolation.

const (
	kcOpenPrefix = "[knowledge_context"
	kcCloseTag   = "[/knowledge_context]"
)

// parsedKCBlock is one [knowledge_context]…[/knowledge_context] block
// extracted from a message string. ArticleID and ContentSHA256 are
// populated when the opening tag carried the id="…" and sha="…"
// attributes; both fields stay empty for legacy untagged blocks.
// Start/End are byte offsets into the source string — inclusive at
// Start, exclusive at End — so callers can splice around the block
// without re-running the parser.
type parsedKCBlock struct {
	ArticleID     string
	ContentSHA256 string
	Body          string // text between opening `]` and `[/knowledge_context]`, not trimmed
	Start         int    // offset of the opening `[`
	End           int    // offset one past the closing `]`
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
// orchestrator-internal wire token, not user-authored content).
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
