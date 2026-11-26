package provider

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

type requestSize struct {
	blocks []events.RequestBlock
}

type counted struct {
	count, bytes int
}

func (c *counted) add(bytes int) {
	c.count++
	c.bytes += bytes
}

func (r *requestSize) add(name string, bytes int) {
	if bytes != 0 {
		r.blocks = append(r.blocks, events.RequestBlock{Name: name, Bytes: bytes})
	}
}

func (r *requestSize) addCounted(name string, c counted) {
	if c.count > 0 {
		r.blocks = append(r.blocks, events.RequestBlock{Name: name, Count: c.count, Bytes: c.bytes})
	}
}

func (r *requestSize) addSystem(segs *SystemSegments, parts []SystemPart, bytes int) {
	if segs == nil {
		r.add(events.BlockSystem, bytes)
		return
	}
	for _, p := range parts {
		r.add(p.Name, p.Bytes)
		bytes -= p.Bytes
	}
	r.add(events.BlockSystemOther, bytes)
}

func (r *requestSize) finish(bodyBytes int) *events.RequestSize {
	rest := bodyBytes
	for _, b := range r.blocks {
		rest -= b.Bytes
	}
	r.add(events.BlockOther, rest)
	return &events.RequestSize{BodyBytes: bodyBytes, Blocks: r.blocks}
}

// TextBytes returns the size of s as sent: encoding/json replaces each invalid UTF-8 byte with U+FFFD.
func TextBytes(s string) int {
	if utf8.ValidString(s) {
		return len(s)
	}
	n := 0
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		i += w
		if r == utf8.RuneError && w == 1 {
			w = utf8.RuneLen(utf8.RuneError)
		}
		n += w
	}
	return n
}

func (s SystemSegments) catalog(inlineTools bool) string {
	if inlineTools {
		return s.InlineCatalog
	}
	return s.Catalog
}

// leadParts follows the order of the texts leadSegments returns.
func (s SystemSegments) leadParts(inlineTools bool) [][]SystemPart {
	return [][]SystemPart{
		s.FixedParts,
		{
			{Name: events.BlockToolIndex, Bytes: TextBytes(s.catalog(inlineTools))},
			{Name: events.BlockConversation, Bytes: TextBytes(s.Conversation)},
		},
		{{Name: events.BlockRound, Bytes: TextBytes(s.Round)}},
	}
}

// jsonBytes ignores the error: v is part of a body that already marshalled.
func jsonBytes(v any) int {
	raw, _ := json.Marshal(v)
	return len(raw)
}

func oaiRequestSize(req *CompletionRequest, oai oaiRequest, bodyBytes int) *events.RequestSize {
	var r requestSize
	var tools counted
	for _, t := range oai.Tools {
		tools.add(jsonBytes(t))
	}
	r.addCounted(events.BlockToolsSent, tools)
	var parts []SystemPart
	var system int
	var history, results counted
	for i, m := range oai.Messages {
		text := string(m.Content)
		switch {
		case m.Role == string(RoleSystem):
			system += TextBytes(text)
			if i == 0 && req.Segments != nil && text == req.Segments.Join() {
				for _, lead := range req.Segments.leadParts(false) {
					parts = append(parts, lead...)
				}
			}
		case IsToolResult(Message{Role: Role(m.Role), Content: text}):
			results.add(TextBytes(text))
		default:
			n := TextBytes(text)
			if len(m.ToolCalls) > 0 {
				n += jsonBytes(m.ToolCalls)
			}
			history.add(n)
		}
	}
	r.addSystem(req.Segments, parts, system)
	r.addCounted(events.BlockHistory, history)
	r.addCounted(events.BlockToolResults, results)
	return r.finish(bodyBytes)
}

func anthRequestSize(req *CompletionRequest, ar anthRequest, bodyBytes int) *events.RequestSize {
	var r requestSize
	var sent, held counted
	for _, t := range ar.Tools {
		if t.DeferLoading {
			held.add(jsonBytes(t))
		} else {
			sent.add(jsonBytes(t))
		}
	}
	r.addCounted(events.BlockToolsSent, sent)
	r.addCounted(events.BlockToolsHeldBack, held)
	systemBytes, inBlocks := anthSystemBytes(ar.System)
	var parts []SystemPart
	if lead, segmented := leadSegments(req, ar.inlineTools); segmented {
		for i, text := range req.Segments.leadParts(ar.inlineTools) {
			// cachedSystemBlocks leaves a blank text out.
			if !inBlocks || strings.TrimSpace(lead[i]) != "" {
				parts = append(parts, text...)
			}
		}
	}
	r.addSystem(req.Segments, parts, systemBytes)
	var history, results counted
	for _, m := range ar.Messages {
		n, result := anthMessageBytes(m)
		if result {
			results.add(n)
		} else {
			history.add(n)
		}
	}
	r.addCounted(events.BlockHistory, history)
	r.addCounted(events.BlockToolResults, results)
	return r.finish(bodyBytes)
}

func anthSystemBytes(system json.RawMessage) (n int, inBlocks bool) {
	var text string
	if json.Unmarshal(system, &text) == nil {
		return len(text), false
	}
	var blocks []anthTextBlock
	if json.Unmarshal(system, &blocks) != nil {
		return 0, false
	}
	for _, b := range blocks {
		n += len(b.Text)
	}
	return n, true
}

func anthMessageBytes(m anthMessage) (n int, result bool) {
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		return len(text), IsToolResult(Message{Role: Role(m.Role), Content: text})
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(m.Content, &blocks) == nil && len(blocks) == 1 && blocks[0].Type == "tool_result" &&
		json.Unmarshal(blocks[0].Content, &text) == nil {
		return len(text), true
	}
	return len(m.Content), false
}
