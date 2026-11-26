package provider

import (
	"encoding/json"
	"strings"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// requestSize collects the blocks of one request body in their order (see
// events.RequestSize). A block without content is left out.
type requestSize struct {
	blocks []events.RequestBlock
}

// counted is the number and size of the items of a counted block.
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

// addSystem adds the system text sent, of size bytes: the whole as
// events.BlockSystem on a request without segments, else the named parts and
// the rest as events.BlockSystemOther. parts are nil when the segments do not
// describe the text sent.
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

// finish adds events.BlockOther, the bytes of the body no other block holds,
// so the blocks sum to the body's size.
func (r *requestSize) finish(bodyBytes int) *events.RequestSize {
	rest := bodyBytes
	for _, b := range r.blocks {
		rest -= b.Bytes
	}
	r.add(events.BlockOther, rest)
	return &events.RequestSize{BodyBytes: bodyBytes, Blocks: r.blocks}
}

// catalog returns the catalog the lead texts carry: the inline form for a
// model with FeatureInlineTools.
func (s SystemSegments) catalog(inlineTools bool) string {
	if inlineTools {
		return s.InlineCatalog
	}
	return s.Catalog
}

// leadParts returns the named parts of the lead texts the segments make up,
// one list per text: Fixed, catalog and Conversation, Round (see
// leadSegments).
func (s SystemSegments) leadParts(inlineTools bool) [][]SystemPart {
	return [][]SystemPart{
		s.FixedParts,
		{{Name: events.BlockToolIndex, Bytes: len(s.catalog(inlineTools))}, {Name: events.BlockConversation, Bytes: len(s.Conversation)}},
		{{Name: events.BlockRound, Bytes: len(s.Round)}},
	}
}

// jsonBytes returns the size of v as JSON, as it stands in a request body.
// v was part of a body that marshalled, so it marshals.
func jsonBytes(v any) int {
	raw, _ := json.Marshal(v)
	return len(raw)
}

// oaiRequestSize returns the blocks of the body of bodyBytes sent for req,
// built as oai.
func oaiRequestSize(req *CompletionRequest, oai oaiRequest, bodyBytes int) *events.RequestSize {
	var r requestSize
	var tools counted
	for _, t := range oai.Tools {
		tools.add(jsonBytes(t))
	}
	r.addCounted(events.BlockToolsSent, tools)
	msgs := oai.Messages
	if len(msgs) > 0 && msgs[0].Role == string(RoleSystem) {
		system := string(msgs[0].Content)
		var parts []SystemPart
		if req.Segments != nil && system == req.Segments.Join() {
			for _, text := range req.Segments.leadParts(false) {
				parts = append(parts, text...)
			}
		}
		r.addSystem(req.Segments, parts, len(system))
		msgs = msgs[1:]
	}
	var history, results counted
	for _, m := range msgs {
		if m.Role == string(RoleTool) {
			results.add(len(m.Content))
			continue
		}
		n := len(m.Content)
		if len(m.ToolCalls) > 0 {
			n += jsonBytes(m.ToolCalls)
		}
		history.add(n)
	}
	r.addCounted(events.BlockHistory, history)
	r.addCounted(events.BlockToolResults, results)
	return r.finish(bodyBytes)
}

// anthRequestSize returns the blocks of the body of bodyBytes sent for req,
// built as ar.
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
		n, result := anthContentBytes(m.Content)
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

// anthSystemBytes returns the size of the system text in a system field and
// whether the field holds it as text blocks.
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

// anthContentBytes returns the size of a message's content and whether the
// message is a tool result: the text of a string content or of a tool
// result, else the JSON of the content blocks.
func anthContentBytes(content json.RawMessage) (n int, result bool) {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return len(text), false
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(content, &blocks) == nil && len(blocks) == 1 && blocks[0].Type == "tool_result" &&
		json.Unmarshal(blocks[0].Content, &text) == nil {
		return len(text), true
	}
	return len(content), false
}
