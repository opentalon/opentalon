package orchestrator

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

// systemTextSent returns the system text of a body: Anthropic's system field
// as a string or as text blocks, else the leading system message.
func systemTextSent(t *testing.T, body []byte) string {
	t.Helper()
	var sent struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content json.RawMessage
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	raw := sent.System
	if len(raw) == 0 && len(sent.Messages) > 0 && sent.Messages[0].Role == "system" {
		raw = sent.Messages[0].Content
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct{ Text string }
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		text += b.Text
	}
	return text
}

// TestRun_RequestSizeNamesTheSystemParts sends the two rounds of the segments
// fixture's turn through each provider and reads the system blocks of their
// llm_response events: each part of the system text carries its name and its
// exact size, and with system_other, which in the fixture is the subprocess
// instructions alone, they sum to the system text sent.
func TestRun_RequestSizeNamesTheSystemParts(t *testing.T) {
	anthropicAnswers := []string{
		`{"id":"r1","model":"m","content":[{"type":"tool_use","id":"t1","name":"inventory__list-items","input":{}}],` +
			`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"id":"r2","model":"m","content":[{"type":"text","text":"Two items are low on stock."}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	tools := []provider.Feature{provider.FeatureTools}
	variants := []struct {
		name     string
		answers  []string
		inline   bool
		provider func(url string, sink *recordingEventSink) provider.Provider
	}{
		{
			name: "openai",
			answers: []string{
				`{"id":"r1","model":"m","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1",` +
					`"type":"function","function":{"name":"inventory__list-items","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
				`{"id":"r2","model":"m","choices":[{"message":{"role":"assistant","content":"Two items are low on stock."},` +
					`"finish_reason":"stop"}]}`,
			},
			provider: func(url string, sink *recordingEventSink) provider.Provider {
				return provider.NewOpenAIProvider("oai", url, "key", []provider.ModelInfo{{ID: "m", Features: tools}},
					provider.WithOpenAISessionEventSink(sink))
			},
		},
		{
			name: "anthropic", answers: anthropicAnswers,
			provider: func(url string, sink *recordingEventSink) provider.Provider {
				return provider.NewAnthropicProvider("anth", url, "key", []provider.ModelInfo{{ID: "m", Features: tools}},
					provider.WithAnthropicSessionEventSink(sink))
			},
		},
		{
			name: "anthropic inline_tools prompt cache", answers: anthropicAnswers, inline: true,
			provider: func(url string, sink *recordingEventSink) provider.Provider {
				models := []provider.ModelInfo{{ID: "m", Features: []provider.Feature{provider.FeatureTools, provider.FeatureInlineTools}}}
				return provider.NewAnthropicProvider("anth", url, "key", models,
					provider.WithAnthropicSessionEventSink(sink), provider.WithAnthropicPromptCache(true))
			},
		},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			server := &bodyServer{answers: v.answers}
			ts := httptest.NewServer(server)
			defer ts.Close()
			sink := &recordingEventSink{}
			f := newSegmentsFixture(t, servedModel{Provider: v.provider(ts.URL, sink), model: "m"})
			if _, err := f.orch.Run(f.ctx, "s1", segmentsUserMessage); err != nil {
				t.Fatal(err)
			}
			var sizes []*events.RequestSize
			for _, e := range sink.snapshot() {
				if e.EventType != events.TypeLLMResponse {
					continue
				}
				var payload events.LLMResponsePayload
				if err := json.Unmarshal(e.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				sizes = append(sizes, payload.Request)
			}
			bodies := server.sent()
			if len(sizes) != 2 || len(bodies) != 2 {
				t.Fatalf("got %d llm_response events for %d requests, want 2 for 2", len(sizes), len(bodies))
			}

			s := f.sections()
			catalog := s.toolCatalog
			if v.inline {
				catalog = s.inlineCatalog
			}
			named := []events.RequestBlock{
				{Name: events.BlockGrounding, Bytes: len(s.grounding)},
				{Name: events.BlockPreamble, Bytes: len(s.preamble)},
				{Name: events.BlockRulesBuiltin, Bytes: len(s.rules) - len(s.operatorRule)},
				{Name: events.BlockRulesOperator, Bytes: len(s.operatorRule)},
				{Name: events.BlockKnowledgeCatalog, Bytes: len(s.knowledge)},
				{Name: events.BlockRuntimeInstructions, Bytes: len(s.instructions)},
			}
			for _, plugin := range s.pluginSections {
				named = append(named, events.RequestBlock{Name: events.BlockPluginPrefix + plugin[0], Bytes: len(plugin[1])})
			}
			named = append(named,
				events.RequestBlock{Name: events.BlockToolIndex, Bytes: len(catalog)},
				events.RequestBlock{Name: events.BlockConversation, Bytes: len(s.session + s.user + s.language + s.summary + "\n")})
			// Round 1 has no tool result yet, so neither the output-format
			// section nor its reminder.
			rounds := []string{s.answerOnly, s.outputFormat + s.formatReminder + s.answerOnly}
			systemNames := map[string]bool{events.BlockSystem: true, events.BlockSystemOther: true}
			for _, b := range named {
				systemNames[b.Name] = true
			}
			systemNames[events.BlockRound] = true

			for i, size := range sizes {
				if size == nil {
					t.Fatalf("round %d: llm_response carries no request", i+1)
				}
				want := append(append([]events.RequestBlock(nil), named...),
					events.RequestBlock{Name: events.BlockRound, Bytes: len(rounds[i])},
					events.RequestBlock{Name: events.BlockSystemOther, Bytes: len(s.subprocess)})
				var got []events.RequestBlock
				sum := 0
				for _, b := range size.Blocks {
					if systemNames[b.Name] {
						got = append(got, b)
						sum += b.Bytes
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("round %d system blocks:\n got %+v\nwant %+v", i+1, got, want)
				}
				if sent := len(systemTextSent(t, bodies[i])); sum != sent {
					t.Errorf("round %d: system blocks sum to %d, the system text sent has %d bytes", i+1, sum, sent)
				}
				if size.BodyBytes != len(bodies[i]) {
					t.Errorf("round %d: body_bytes %d, body sent %d", i+1, size.BodyBytes, len(bodies[i]))
				}
			}
		})
	}
}
