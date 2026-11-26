package orchestrator

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

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

func responseSizes(t *testing.T, sink *recordingEventSink) []*events.RequestSize {
	t.Helper()
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
	return sizes
}

func TestRun_RequestSizeNamesTheSystemParts(t *testing.T) {
	tools := []provider.Feature{provider.FeatureTools}
	variants := []struct {
		name     string
		answers  []string
		inline   bool
		provider func(url string, sink *recordingEventSink) provider.Provider
	}{
		{
			name: "openai", answers: segmentsOpenAIAnswers,
			provider: func(url string, sink *recordingEventSink) provider.Provider {
				return provider.NewOpenAIProvider("oai", url, "key", []provider.ModelInfo{{ID: "m", Features: tools}},
					provider.WithOpenAISessionEventSink(sink))
			},
		},
		{
			name: "anthropic", answers: segmentsAnthropicAnswers,
			provider: func(url string, sink *recordingEventSink) provider.Provider {
				return provider.NewAnthropicProvider("anth", url, "key", []provider.ModelInfo{{ID: "m", Features: tools}},
					provider.WithAnthropicSessionEventSink(sink))
			},
		},
		{
			name: "anthropic inline_tools prompt cache", answers: segmentsAnthropicAnswers, inline: true,
			provider: func(url string, sink *recordingEventSink) provider.Provider {
				models := []provider.ModelInfo{{ID: "m", Features: []provider.Feature{provider.FeatureTools, provider.FeatureInlineTools}}}
				return provider.NewAnthropicProvider("anth", url, "key", models,
					provider.WithAnthropicSessionEventSink(sink), provider.WithAnthropicPromptCache(true))
			},
		},
	}
	instructions := map[string]string{"valid": "Prefer short answers.", "invalid byte": "Prefer short answers.\xff"}
	for _, v := range variants {
		for label, prompt := range instructions {
			t.Run(v.name+"/"+label, func(t *testing.T) {
				server := &bodyServer{answers: v.answers}
				ts := httptest.NewServer(server)
				defer ts.Close()
				sink := &recordingEventSink{}
				f := newSegmentsFixture(t, servedModel{Provider: v.provider(ts.URL, sink), model: "m"})
				f.orch.runtimePromptPath = filepath.Join(t.TempDir(), "prompt.txt")
				if err := os.WriteFile(f.orch.runtimePromptPath, []byte(prompt), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := f.orch.Run(f.ctx, "s1", segmentsUserMessage); err != nil {
					t.Fatal(err)
				}
				sizes := responseSizes(t, sink)
				bodies := server.sent()
				if len(sizes) != 2 || len(bodies) != 2 {
					t.Fatalf("got %d llm_response events for %d requests, want 2 for 2", len(sizes), len(bodies))
				}

				s := f.sections()
				s.instructions = "\n## Additional instructions (editable from chat)\n" + strings.ToValidUTF8(prompt, "\uFFFD") + "\n\n"
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
					events.RequestBlock{Name: events.BlockConversation, Bytes: len(s.session + s.user + s.language)})
				// The output-format section and its reminder follow a tool result only.
				rounds := []string{s.answerOnly, s.outputFormat + s.formatReminder + s.answerOnly}
				systemNames := map[string]bool{events.BlockSystem: true, events.BlockSystemOther: true, events.BlockRound: true}
				for _, b := range named {
					systemNames[b.Name] = true
				}

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
}

func TestRun_RequestSizeCountsTextFormatToolResults(t *testing.T) {
	text := func(t *testing.T, s string) string {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	call := "[tool_call]\n{\"tool\": \"inventory__list-items\", \"args\": {}}\n[/tool_call]"
	answer := "Two items are low on stock."
	models := []provider.ModelInfo{{ID: "m"}}
	variants := []struct {
		name     string
		answers  func(t *testing.T) []string
		provider func(url string, sink *recordingEventSink) provider.Provider
	}{
		{
			name: "openai",
			answers: func(t *testing.T) []string {
				reply := func(s string) string {
					return `{"id":"r","model":"m","choices":[{"message":{"role":"assistant","content":` + text(t, s) +
						`},"finish_reason":"stop"}]}`
				}
				return []string{reply(call), reply(answer)}
			},
			provider: func(url string, sink *recordingEventSink) provider.Provider {
				return provider.NewOpenAIProvider("oai", url, "key", models, provider.WithOpenAISessionEventSink(sink))
			},
		},
		{
			name: "anthropic",
			answers: func(t *testing.T) []string {
				reply := func(s string) string {
					return `{"id":"r","model":"m","content":[{"type":"text","text":` + text(t, s) +
						`}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				}
				return []string{reply(call), reply(answer)}
			},
			provider: func(url string, sink *recordingEventSink) provider.Provider {
				return provider.NewAnthropicProvider("anth", url, "key", models, provider.WithAnthropicSessionEventSink(sink))
			},
		},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			ts := httptest.NewServer(&bodyServer{answers: v.answers(t)})
			defer ts.Close()
			sink := &recordingEventSink{}
			llm := &recordingProvider{Provider: servedModel{Provider: v.provider(ts.URL, sink), model: "m"}}
			f := newSegmentsFixture(t, llm)
			if _, err := f.orch.Run(f.ctx, "s1", segmentsUserMessage); err != nil {
				t.Fatal(err)
			}
			sizes := responseSizes(t, sink)
			if len(sizes) != 2 || len(llm.requests) != 2 || sizes[1] == nil {
				t.Fatalf("got %d llm_response events for %d requests, want 2 with a request each", len(sizes), len(llm.requests))
			}
			msgs := llm.requests[1].Messages
			result := msgs[len(msgs)-1]
			if len(llm.requests[1].Tools) != 0 || result.Role != provider.RoleUser {
				t.Fatalf("round 2 is no text-format tool round: %d tools, last message %s", len(llm.requests[1].Tools), result.Role)
			}
			want := events.RequestBlock{Name: events.BlockToolResults, Count: 1, Bytes: len(result.Content)}
			var got events.RequestBlock
			for _, b := range sizes[1].Blocks {
				if b.Name == events.BlockToolResults {
					got = b
				}
			}
			if got != want {
				t.Errorf("tool_results = %+v, want %+v; blocks %+v", got, want, sizes[1].Blocks)
			}
		})
	}
}
