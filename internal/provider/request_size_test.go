package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// sizeTestSegments names two parts of Fixed and leaves "Other.\n" unnamed.
var sizeTestSegments = SystemSegments{
	Fixed:         "Preamble.\nRules.\nOther.\n",
	FixedParts:    []SystemPart{{Name: events.BlockPreamble, Bytes: 10}, {Name: events.BlockRulesBuiltin, Bytes: 7}},
	Catalog:       "Catalog.\n",
	InlineCatalog: "Inline catalog.\n",
	Conversation:  "Conversation.\n",
	Round:         "Round.",
}

// sizeTestRequest is a chat-loop request with a tool round in its history:
// a fixed and a loaded on-demand tool, one more loadable tool, a tool call
// and its result.
func sizeTestRequest(segs *SystemSegments) *CompletionRequest {
	system := sizeTestSegments.Join()
	return &CompletionRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleSystem, Content: system},
			{Role: RoleUser, Content: "How many items?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "p__count", Arguments: map[string]string{"q": "<all>"}}}},
			{Role: RoleTool, ToolCallID: "c1", Content: "3 items"},
			{Role: RoleUser, Content: "And now?"},
		},
		Tools: []ToolDefinition{
			inlineTestTool("p__count", "Counts items.", false),
			inlineTestTool("p__loaded", "Loaded tool.", true),
		},
		LoadableTools: []ToolDefinition{inlineTestTool("p__hidden", "Loadable tool.", true)},
		Segments:      segs,
	}
}

// wireBlocks returns the blocks of a body read from the body itself, by the
// definition of events.RequestBlock, with the given system parts: the
// tools, the system text, then every other message by its content as sent.
func wireBlocks(t *testing.T, body []byte, system func(t *testing.T, text int) []events.RequestBlock) []events.RequestBlock {
	t.Helper()
	var sent struct {
		Tools    []json.RawMessage `json:"tools"`
		System   json.RawMessage   `json:"system"`
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	var blocks []events.RequestBlock
	block := func(name string, count, bytes int) {
		if bytes != 0 || count != 0 {
			blocks = append(blocks, events.RequestBlock{Name: name, Count: count, Bytes: bytes})
		}
	}
	var tools, held counted
	for _, tool := range sent.Tools {
		if strings.Contains(string(tool), `"defer_loading":true`) {
			held.add(len(tool))
		} else {
			tools.add(len(tool))
		}
	}
	block(events.BlockToolsSent, tools.count, tools.bytes)
	block(events.BlockToolsHeldBack, held.count, held.bytes)
	text := func(raw json.RawMessage) (string, bool) {
		var s string
		return s, json.Unmarshal(raw, &s) == nil
	}
	messages := sent.Messages
	systemText := 0
	if s, ok := text(sent.System); ok {
		systemText = len(s)
	} else if len(sent.System) > 0 {
		var parts []struct{ Text string }
		if err := json.Unmarshal(sent.System, &parts); err != nil {
			t.Fatal(err)
		}
		for _, p := range parts {
			systemText += len(p.Text)
		}
	} else if s, _ := text(messages[0].Content); messages[0].Role == "system" {
		systemText = len(s)
		messages = messages[1:]
	}
	blocks = append(blocks, system(t, systemText)...)
	var history, results counted
	for _, m := range messages {
		s, isText := text(m.Content)
		var parts []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		switch {
		case m.Role == "tool":
			results.add(len(s))
		case isText:
			history.add(len(s) + len(m.ToolCalls))
		case json.Unmarshal(m.Content, &parts) == nil && len(parts) == 1 && parts[0].Type == "tool_result":
			s, _ = text(parts[0].Content)
			results.add(len(s))
		default:
			history.add(len(m.Content))
		}
	}
	block(events.BlockHistory, history.count, history.bytes)
	block(events.BlockToolResults, results.count, results.bytes)
	rest := len(body)
	for _, b := range blocks {
		rest -= b.Bytes
	}
	block(events.BlockOther, 0, rest)
	return blocks
}

// namedSystem returns the system blocks of sizeTestSegments with catalog as
// the tool index: the named parts, then the rest of the text sent, which is
// the one unnamed line of Fixed.
func namedSystem(catalog string) func(t *testing.T, text int) []events.RequestBlock {
	return func(t *testing.T, text int) []events.RequestBlock {
		named := []events.RequestBlock{
			{Name: events.BlockPreamble, Bytes: len("Preamble.\n")},
			{Name: events.BlockRulesBuiltin, Bytes: len("Rules.\n")},
			{Name: events.BlockToolIndex, Bytes: len(catalog)},
			{Name: events.BlockConversation, Bytes: len("Conversation.\n")},
			{Name: events.BlockRound, Bytes: len("Round.")},
		}
		rest := text
		for _, b := range named {
			rest -= b.Bytes
		}
		if rest != len("Other.\n") {
			t.Errorf("the system text sent has %d bytes beside the named parts, want %d", rest, len("Other.\n"))
		}
		return append(named, events.RequestBlock{Name: events.BlockSystemOther, Bytes: rest})
	}
}

func wholeSystem(name string) func(t *testing.T, text int) []events.RequestBlock {
	return func(_ *testing.T, text int) []events.RequestBlock {
		return []events.RequestBlock{{Name: name, Bytes: text}}
	}
}

// sizeTestPath is one way a provider sends a request; the tool index of
// sizeTestSegments it sends is catalog.
type sizeTestPath struct {
	name    string
	catalog string
	send    func(url string, sink *recordingEventSink, req *CompletionRequest) error
}

func sizeTestPaths() []sizeTestPath {
	anthropic := func(features []Feature, opts ...AnthropicOption) func(string, *recordingEventSink, *CompletionRequest) error {
		return func(url string, sink *recordingEventSink, req *CompletionRequest) error {
			opts := append(opts, WithAnthropicSessionEventSink(sink))
			_, err := NewAnthropicProvider("anthropic", url, "key", []ModelInfo{{ID: "m", Features: features}}, opts...).
				Complete(context.Background(), req)
			return err
		}
	}
	return []sizeTestPath{
		{"openai", "Catalog.\n", func(url string, sink *recordingEventSink, req *CompletionRequest) error {
			_, err := NewOpenAIProvider("openai", url, "key", nil, WithOpenAISessionEventSink(sink)).Complete(context.Background(), req)
			return err
		}},
		{"openai stream", "Catalog.\n", func(url string, sink *recordingEventSink, req *CompletionRequest) error {
			stream, err := NewOpenAIProvider("openai", url, "key", nil, WithOpenAISessionEventSink(sink)).
				Stream(context.Background(), req)
			if err != nil {
				return err
			}
			for {
				chunk, err := stream.Recv()
				if err != nil || chunk.Done {
					_ = stream.Close()
					return err
				}
			}
		}},
		{"anthropic", "Catalog.\n", anthropic(nil)},
		{"anthropic inline_tools prompt cache", "Inline catalog.\n",
			anthropic([]Feature{FeatureTools, FeatureInlineTools}, WithAnthropicPromptCache(true))},
	}
}

// sizeTestServer answers every request of each path with "ok".
func sizeTestServer(t *testing.T, bodies *[][]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, body)
		switch {
		case r.URL.Path == anthropicMessagesPath:
			_, _ = io.WriteString(w, `{"id":"r1","model":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		case strings.Contains(string(body), `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, streamResponse([]string{"ok"}, "stop", true))
		default:
			_, _ = io.WriteString(w, `{"id":"c1","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestLLMResponseCarriesTheRequestSize checks the request blocks on the
// llm_response event of every provider path against the body sent: on a
// chat-loop request the named parts of the system text and the rest, on a
// request whose system message the segments no longer describe the whole
// text as system_other, on any other request the whole text as system.
func TestLLMResponseCarriesTheRequestSize(t *testing.T) {
	changed := func() *CompletionRequest {
		req := sizeTestRequest(&sizeTestSegments)
		req.Messages[0].Content += "Guarded.\n"
		return req
	}
	for _, path := range sizeTestPaths() {
		for _, tc := range []struct {
			name   string
			req    *CompletionRequest
			system func(t *testing.T, text int) []events.RequestBlock
		}{
			{"chat loop", sizeTestRequest(&sizeTestSegments), namedSystem(path.catalog)},
			{"chat loop, system message changed", changed(), wholeSystem(events.BlockSystemOther)},
			{"outside the chat loop", sizeTestRequest(nil), wholeSystem(events.BlockSystem)},
		} {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				var bodies [][]byte
				server := sizeTestServer(t, &bodies)
				sink := &recordingEventSink{}
				if err := path.send(server.URL, sink, tc.req); err != nil {
					t.Fatal(err)
				}
				got := sink.snapshot()
				if len(got) != 2 || got[1].EventType != events.TypeLLMResponse || len(bodies) != 1 {
					t.Fatalf("events = %v and %d bodies, want llm_request then llm_response for one body", sink.types(), len(bodies))
				}
				var payload events.LLMResponsePayload
				if err := json.Unmarshal(got[1].Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.V != 4 || payload.Request == nil {
					t.Fatalf("payload v%d carries no request: %s", payload.V, got[1].Payload)
				}
				want := &events.RequestSize{BodyBytes: len(bodies[0]), Blocks: wireBlocks(t, bodies[0], tc.system)}
				if !reflect.DeepEqual(payload.Request, want) {
					t.Errorf("request:\n got %+v\nwant %+v\nbody %s", *payload.Request, *want, bodies[0])
				}
				// Every path sends the history and the tool result; inline_tools
				// holds the loaded and the loadable on-demand tool back.
				counts := map[string]int{}
				for _, b := range want.Blocks {
					counts[b.Name] = b.Count
				}
				if counts[events.BlockToolResults] != 1 || counts[events.BlockHistory] < 3 ||
					(path.catalog == "Inline catalog.\n" && tc.req.Segments != nil) != (counts[events.BlockToolsHeldBack] == 2) {
					t.Errorf("the body sent lacks a block the test is about: %+v", want.Blocks)
				}
				sum := 0
				for _, b := range payload.Request.Blocks {
					if b.Bytes < 0 {
						t.Errorf("block %s has %d bytes", b.Name, b.Bytes)
					}
					sum += b.Bytes
				}
				if sum != payload.Request.BodyBytes {
					t.Errorf("blocks sum to %d, body has %d bytes", sum, payload.Request.BodyBytes)
				}
			})
		}
	}
}

// TestFailedCallCarriesNoRequestSize: a call the provider refuses writes its
// error event, as before, and no response event with a request size.
func TestFailedCallCarriesNoRequestSize(t *testing.T) {
	for _, path := range sizeTestPaths() {
		t.Run(path.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad"}}`)
			}))
			defer server.Close()
			sink := &recordingEventSink{}
			if err := path.send(server.URL, sink, sizeTestRequest(&sizeTestSegments)); err == nil {
				t.Fatal("the call succeeded, want the provider's refusal")
			}
			if got := strings.Join(sink.types(), ","); got != "llm_request,llm_error" {
				t.Errorf("events = %s, want llm_request,llm_error", got)
			}
			for _, e := range sink.snapshot() {
				if strings.Contains(string(e.Payload), `"body_bytes"`) {
					t.Errorf("%s carries a request size: %s", e.EventType, e.Payload)
				}
			}
		})
	}
}
