package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

// segmentsTestLLM records every request and answers from a script. It
// advertises native tools so the chat loop renders the tool catalog.
type segmentsTestLLM struct {
	mu       sync.Mutex
	requests []*provider.CompletionRequest
	rounds   []provider.CompletionResponse
}

func (l *segmentsTestLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, req)
	if len(l.rounds) == 0 {
		return &provider.CompletionResponse{Content: "done"}, nil
	}
	r := l.rounds[0]
	l.rounds = l.rounds[1:]
	return &r, nil
}

func (l *segmentsTestLLM) SupportsFeature(f provider.Feature) bool { return f == provider.FeatureTools }

// groundingExecutor backs the agents plugin of an agent-scoped chat and
// counts the grounding fetches per action.
type groundingExecutor struct {
	mu    sync.Mutex
	calls map[string]int
}

func (e *groundingExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	e.mu.Lock()
	e.calls[call.Action]++
	e.mu.Unlock()
	switch call.Action {
	case "show":
		return ToolResult{CallID: call.ID, StructuredContent: `{"name":"Nightly report","description":"Sum up the day","tln_source":"report daily"}`}
	case "runs":
		return ToolResult{CallID: call.ID, StructuredContent: `[{"run":1,"status":"ok"}]`}
	}
	return ToolResult{CallID: call.ID, Error: "unknown action"}
}

// segmentsFixture is an agent-scoped chat whose system prompt carries every
// section of the segment table: a session with a summary and history, a
// channel format, a user name, a reply language, a knowledge catalog,
// additional instructions, a plugin with server instructions, the tool
// catalog and the subprocess instructions.
type segmentsFixture struct {
	orch      *Orchestrator
	ctx       context.Context
	grounding *groundingExecutor
}

const segmentsUserMessage = "Please list every item that is low on stock today."

func newSegmentsFixture(t *testing.T, llm LLMClient) segmentsFixture {
	t.Helper()
	registry := NewToolRegistry()
	grounding := &groundingExecutor{calls: map[string]int{}}
	if err := registry.Register(PluginCapability{
		Name: agentGroundingPlugin, Description: "Workflow agents",
		Actions: []Action{{Name: "show", UserOnly: true}, {Name: "runs", UserOnly: true}},
	}, grounding); err != nil {
		t.Fatalf("register agents: %v", err)
	}
	if err := registry.Register(PluginCapability{
		Name: "inventory", Description: "Inventory tools",
		SystemPromptAddition: "Count items with list-items.",
		Actions: []Action{
			native(Action{Name: "list-items", Description: "List items", ReadOnly: true}),
			{Name: "add-to-stock", Description: "Add stock"},
		},
	}, &echoExecutor{}); err != nil {
		t.Fatalf("register inventory: %v", err)
	}

	instructions := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(instructions, []byte("Prefer short answers."), 0o600); err != nil {
		t.Fatal(err)
	}

	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	if err := sessions.SetSummary("s1", "The user runs a small warehouse.", []provider.Message{
		{Role: provider.RoleUser, Content: "Which items are in stock?"},
		{Role: provider.RoleAssistant, Content: "Three items are in stock."},
	}); err != nil {
		t.Fatal(err)
	}

	orch := NewWithRules(llm, DefaultParser, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		RuntimePromptPath: instructions,
		Subprocess:        SubprocessConfig{Enabled: true},
	})
	orch.knowledgeCatalog = renderKnowledgeCatalog([]knowledgeCatalogEntry{{Slug: "stock", Title: "Stock levels"}},
		prompts.OrchestratorKnowledgeInstructions)
	orch.knowledgeCatalogAt = time.Now()

	ctx := profile.WithProfile(context.Background(), &profile.Profile{ChannelID: "web", Name: "Ada"})
	ctx = actor.WithConversationID(ctx, "conv-1")
	ctx = actor.WithAgentID(ctx, "agent-7")
	ctx = pkgchannel.WithCapabilities(ctx, pkgchannel.Capabilities{ResponseFormat: pkgchannel.FormatMarkdown})
	return segmentsFixture{orch: orch, ctx: ctx, grounding: grounding}
}

// toolRounds scripts a chat turn of two rounds: a tool call, then the answer.
func toolRounds() *segmentsTestLLM {
	return &segmentsTestLLM{rounds: []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "inventory__list-items", Arguments: map[string]string{}}}},
		{Content: "Two items are low on stock."},
	}}
}

// promptSections holds the text of each system-prompt section the fixture
// renders, as the builder wrote it before the split into segments.
type promptSections struct {
	grounding, preamble, rules, knowledge, instructions, session, plugins, toolCatalog, subprocess string
	outputFormat, user, language, summary, formatReminder, answerOnly                              string
}

func (f segmentsFixture) sections() promptSections {
	return promptSections{
		grounding:    f.orch.buildAgentGroundingPrompt(f.ctx, "agent-7"),
		preamble:     prompts.OrchestratorPreambleNative,
		rules:        f.orch.rules.BuildPromptSection(),
		knowledge:    f.orch.knowledgeCatalog,
		instructions: "\n## Additional instructions (editable from chat)\nPrefer short answers.\n\n",
		session: "## Current session\nYou are currently on channel `web`, conversation `conv-1`.\n\n" +
			"When a tool parameter's description says it defaults to the current channel or conversation, OMIT it — " +
			"the host injects these values automatically. Do not try to invent or ask for an id.\n\n",
		plugins: "## _meta\nOrchestrator-owned meta-tools.\n\n" +
			"## _subprocess\nFork a focused sub-agent process to handle a sub-task\n\n" +
			"## inventory\nInventory tools\n--- plugin: inventory ---\nCount items with list-items.\n--- end plugin: inventory ---\n\n",
		toolCatalog:    f.orch.renderToolCatalog(f.orch.promotedToolSet(f.ctx), f.orch.resolveAllowedPlugins(f.ctx)),
		subprocess:     prompts.OrchestratorSubprocess,
		outputFormat:   "## OUTPUT FORMAT\n" + prompts.FormatMarkdown + "\n",
		user:           "## User\nThe user's name is Ada. Address them by name where it feels natural; do not force it into every message.\n\n",
		language:       f.orch.replyLanguageDirective(segmentsUserMessage),
		summary:        "\n\nPrevious conversation summary: The user runs a small warehouse.",
		formatReminder: "\n\n[IMPORTANT — output format reminder] " + prompts.FormatMarkdown,
		answerOnly: "\n\n[IMPORTANT] Answer ONLY the user's most recent message. " +
			"Do NOT repeat or summarize any earlier answers from this conversation. Be concise.",
	}
}

// TestRun_SystemPromptSectionsKeepTheirText compares the chat loop's system
// prompt with the sections the builder wrote before the split: each keeps its
// text and appears exactly once, with nothing but whitespace between them.
func TestRun_SystemPromptSectionsKeepTheirText(t *testing.T) {
	llm := toolRounds()
	f := newSegmentsFixture(t, llm)
	if _, err := f.orch.Run(f.ctx, "s1", segmentsUserMessage); err != nil {
		t.Fatal(err)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("chat loop made %d model calls, want 2", len(llm.requests))
	}
	// The second round carries every section, the per-round ones included.
	system := llm.requests[1].Messages[0].Content
	s := f.sections()
	rest := system
	for i, text := range []string{
		s.grounding, s.preamble, s.rules, s.knowledge, s.instructions, s.session, s.plugins, s.toolCatalog,
		s.subprocess, s.outputFormat, s.user, s.language, s.summary, s.formatReminder, s.answerOnly,
	} {
		if text == "" {
			t.Fatalf("section %d renders empty in the fixture", i)
		}
		if n := strings.Count(system, text); n != 1 {
			t.Errorf("section %d appears %d times, want once:\n%s", i, n, text)
		}
		rest = strings.Replace(rest, text, "", 1)
	}
	if strings.TrimSpace(rest) != "" {
		t.Errorf("system prompt holds text outside its sections: %q", rest)
	}
}

// TestRun_SystemPromptSegments checks the segments of each chat-loop request
// section by section, agent grounding first, and that the leading system
// message is their join.
func TestRun_SystemPromptSegments(t *testing.T) {
	llm := toolRounds()
	f := newSegmentsFixture(t, llm)
	if _, err := f.orch.Run(f.ctx, "s1", segmentsUserMessage); err != nil {
		t.Fatal(err)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("chat loop made %d model calls, want 2", len(llm.requests))
	}
	s := f.sections()
	fixed := s.grounding + s.preamble + s.rules + s.knowledge + s.instructions + s.plugins + s.subprocess
	conversation := s.toolCatalog + s.session + s.user + s.language + s.summary + "\n"
	want := []provider.SystemSegments{
		// Round 1: no tool result yet, so neither the output-format section
		// nor its reminder.
		{Fixed: fixed, Conversation: conversation, Round: s.answerOnly},
		{Fixed: fixed, Conversation: conversation, Round: s.outputFormat + s.formatReminder + s.answerOnly},
	}
	for i, req := range llm.requests {
		if req.Segments == nil {
			t.Fatalf("round %d: chat-loop request carries no segments", i+1)
		}
		if *req.Segments != want[i] {
			t.Errorf("round %d segments:\n got %#v\nwant %#v", i+1, *req.Segments, want[i])
		}
		if req.Messages[0].Role != provider.RoleSystem || req.Messages[0].Content != req.Segments.Join() {
			t.Errorf("round %d: leading system message is not the join of the segments:\n%q", i+1, req.Messages[0].Content)
		}
	}
}

func TestRun_FetchesAgentGroundingOnce(t *testing.T) {
	f := newSegmentsFixture(t, toolRounds())
	if _, err := f.orch.Run(f.ctx, "s1", segmentsUserMessage); err != nil {
		t.Fatal(err)
	}
	f.grounding.mu.Lock()
	defer f.grounding.mu.Unlock()
	if f.grounding.calls["show"] != 1 || f.grounding.calls["runs"] != 1 {
		t.Errorf("agent grounding fetches = %v, want show and runs once each", f.grounding.calls)
	}
}

// recordingProvider keeps each request it hands to a real provider.
type recordingProvider struct {
	provider.Provider
	requests []*provider.CompletionRequest
}

func (r *recordingProvider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	r.requests = append(r.requests, req)
	return r.Provider.Complete(ctx, req)
}

// TestRun_ProvidersSendJoinedSegments renders the chat loop's request on the
// wire and checks that the system prompt goes out as the concatenation of the
// segments.
func TestRun_ProvidersSendJoinedSegments(t *testing.T) {
	models := []provider.ModelInfo{{ID: "m", Features: []provider.Feature{provider.FeatureTools}}}
	cases := []struct {
		name     string
		reply    string
		provider func(url string) provider.Provider
		system   func(t *testing.T, body []byte) string
	}{
		{
			name:  "openai-compatible",
			reply: `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
			provider: func(url string) provider.Provider {
				return provider.NewOpenAIProvider("oai", url, "key", models)
			},
			system: func(t *testing.T, body []byte) string {
				var req struct {
					Messages []struct{ Role, Content string } `json:"messages"`
				}
				if err := json.Unmarshal(body, &req); err != nil {
					t.Fatal(err)
				}
				var system []string
				for _, m := range req.Messages {
					if m.Role == "system" {
						system = append(system, m.Content)
					}
				}
				if len(system) != 1 || req.Messages[0].Role != "system" {
					t.Fatalf("want one leading system message, got %d in %+v", len(system), req.Messages)
				}
				return system[0]
			},
		},
		{
			name:  "anthropic",
			reply: `{"id":"r1","model":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`,
			provider: func(url string) provider.Provider {
				return provider.NewAnthropicProvider("anth", url, "key", models)
			},
			system: func(t *testing.T, body []byte) string {
				var req struct {
					System string `json:"system"`
				}
				if err := json.Unmarshal(body, &req); err != nil {
					t.Fatal(err)
				}
				return req.System
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodies := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				bodies <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer server.Close()

			llm := &recordingProvider{Provider: tc.provider(server.URL)}
			f := newSegmentsFixture(t, llm)
			if _, err := f.orch.Run(f.ctx, "s1", segmentsUserMessage); err != nil {
				t.Fatal(err)
			}
			if len(llm.requests) != 1 || llm.requests[0].Segments == nil {
				t.Fatalf("want one chat-loop request with segments, got %d", len(llm.requests))
			}
			segs := llm.requests[0].Segments
			if got, want := tc.system(t, <-bodies), segs.Fixed+segs.Conversation+segs.Round; got != want {
				t.Errorf("system prompt on the wire is not the concatenation of the segments:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// TestNonChatLoopRequestsCarryNoSegments covers every model call outside the
// chat loop: none of them sets segments.
func TestNonChatLoopRequestsCarryNoSegments(t *testing.T) {
	newOrch := func(llm LLMClient, opts OrchestratorOpts, messages int) *Orchestrator {
		sessions := state.NewSessionStore("")
		sessions.Create(state.SessionParams{ID: "s1"})
		for i := 0; i < messages; i++ {
			role := provider.RoleUser
			if i%2 == 1 {
				role = provider.RoleAssistant
			}
			_ = sessions.AddMessage("s1", provider.Message{Role: role, Content: "message " + strconv.Itoa(i)})
		}
		return NewWithRules(llm, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), sessions, opts)
	}
	cases := []struct {
		name string
		call func(t *testing.T, llm *segmentsTestLLM)
	}{
		{"session title", func(_ *testing.T, llm *segmentsTestLLM) {
			newOrch(llm, OrchestratorOpts{SessionTitlesEnabled: true}, 2).maybeGenerateTitle(context.Background(), "s1")
		}},
		{"summary", func(_ *testing.T, llm *segmentsTestLLM) {
			opts := OrchestratorOpts{SummarizeAfterMessages: 5, MaxMessagesAfterSummary: 2}
			newOrch(llm, opts, 10).maybeSummarizeSession(context.Background(), "s1")
		}},
		{"confirmation narration", func(_ *testing.T, llm *segmentsTestLLM) {
			newOrch(llm, OrchestratorOpts{}, 0).narrateConfirmation(context.Background(), nil,
				ToolCall{Action: "delete-item"}, "delete the item")
		}},
		{"subprocess", func(t *testing.T, llm *segmentsTestLLM) {
			if _, err := setupSubprocessOrchestrator(llm).runSubprocess(context.Background(),
				subprocessRequest{Task: "Find the capital of France."}, 1); err != nil {
				t.Fatal(err)
			}
		}},
		{"planner", func(t *testing.T, llm *segmentsTestLLM) {
			if _, err := (&plannerLLMAdapter{llm: llm}).Complete(context.Background(), &pipeline.CompletionRequest{
				Messages: []pipeline.Message{{Role: "system", Content: "plan"}, {Role: "user", Content: "list items"}},
			}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			llm := &segmentsTestLLM{}
			tc.call(t, llm)
			if len(llm.requests) == 0 {
				t.Fatal("no model call was made")
			}
			for i, req := range llm.requests {
				if req.Segments != nil {
					t.Errorf("request %d carries segments: %+v", i, *req.Segments)
				}
			}
		})
	}
}
