package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
)

var inlineTestSegments = SystemSegments{Fixed: "Fixed.\n", Conversation: "Conversation.\n", Round: "Round."}

func inlineTestTool(name, description string, onDemand bool) ToolDefinition {
	return ToolDefinition{Name: name, Description: description, Parameters: map[string]interface{}{"type": "object"}, OnDemand: onDemand}
}

func inlineTestRequest(chatLoop bool) *CompletionRequest {
	req := &CompletionRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: RoleSystem, Content: inlineTestSegments.Join()},
			{Role: RoleUser, Content: "How many items?", Announcement: &ToolAnnouncement{Added: []string{"p__loaded"}}},
		},
		Tools: []ToolDefinition{
			inlineTestTool("p__loaded", "Loaded tool.", true),
			inlineTestTool("p__fixed", "Fixed tool.", false),
		},
		LoadableTools: []ToolDefinition{
			inlineTestTool("p__a", "Loadable tool a.", true),
			inlineTestTool("p__b", "Loadable tool b.", true),
		},
	}
	if chatLoop {
		segs := inlineTestSegments
		req.Segments = &segs
	}
	return req
}

// Bodies 8ba8891 (origin/master) sent for inlineTestRequest without OnDemand and LoadableTools.
const (
	inlineTestTodaysTools = `"tools":[{"name":"p__loaded","description":"Loaded tool.","input_schema":{"type":"object"}},` +
		`{"name":"p__fixed","description":"Fixed tool.","input_schema":{"type":"object"}}]`
	inlineTestChatLoopHead = `{"model":"claude-x","system":[{"type":"text","text":"Fixed.\n","cache_control":{"type":"ephemeral"}},` +
		`{"type":"text","text":"Conversation.\n"},{"type":"text","text":"Round."}],` +
		`"messages":[{"role":"user","content":"How many items?"}],"max_tokens":4096,`
	inlineTestChatLoopBody = inlineTestChatLoopHead + inlineTestTodaysTools + `,"cache_control":{"type":"ephemeral"}}`
	inlineTestOtherBody    = `{"model":"claude-x","system":"Fixed.\nConversation.\nRound.",` +
		`"messages":[{"role":"user","content":"How many items?"}],"max_tokens":4096,` + inlineTestTodaysTools + `}`
	inlineTestOpenAIBody = `{"model":"claude-x","messages":[{"role":"system","content":"Fixed.\nConversation.\nRound."},` +
		`{"role":"user","content":"How many items?"}],"tools":[` +
		`{"type":"function","function":{"name":"p__loaded","description":"Loaded tool.","parameters":{"type":"object"}}},` +
		`{"type":"function","function":{"name":"p__fixed","description":"Fixed tool.","parameters":{"type":"object"}}}]}`
)

func sentRequests(t *testing.T, complete func(baseURL string, req *CompletionRequest) error, reqs ...*CompletionRequest) []capturedRequest {
	t.Helper()
	var mu sync.Mutex
	var got []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, capturedRequest{header: r.Header.Clone(), body: body})
		mu.Unlock()
		if r.URL.Path == anthropicMessagesPath {
			_, _ = io.WriteString(w, `{"id":"r1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"r1","model":"claude-x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()
	for _, req := range reqs {
		if err := complete(server.URL, req); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return got
}

func anthropicComplete(models []ModelInfo) func(string, *CompletionRequest) error {
	return func(baseURL string, req *CompletionRequest) error {
		_, err := NewAnthropicProvider("anthropic", baseURL, "key", models, WithAnthropicPromptCache(true)).
			Complete(actor.WithSessionID(context.Background(), "s1"), req)
		return err
	}
}

func inlineModels(features ...Feature) []ModelInfo {
	return []ModelInfo{{ID: "claude-x", Features: features}}
}

func TestAnthropicInlineTools(t *testing.T) {
	const want = `{"model":"claude-x","system":[{"type":"text","text":"Fixed.\n","cache_control":{"type":"ephemeral"}},` +
		`{"type":"text","text":"Conversation.\n"},{"type":"text","text":"Round."}],` +
		`"messages":[{"role":"user","content":"How many items?"},` +
		`{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"p__loaded"}}]}],"max_tokens":4096,` +
		`"tools":[{"name":"p__fixed","description":"Fixed tool.","input_schema":{"type":"object"}},` +
		`{"name":"p__a","description":"Loadable tool a.","input_schema":{"type":"object"},"defer_loading":true},` +
		`{"name":"p__b","description":"Loadable tool b.","input_schema":{"type":"object"},"defer_loading":true},` +
		`{"name":"p__loaded","description":"Loaded tool.","input_schema":{"type":"object"},"defer_loading":true}]` +
		`,"cache_control":{"type":"ephemeral"}}`
	req := inlineTestRequest(true)
	sent := sentRequests(t, anthropicComplete(inlineModels(FeatureTools, FeatureInlineTools)), req, req)
	for i, r := range sent {
		if string(r.body) != want {
			t.Errorf("request %d body:\n got %s\nwant %s", i+1, r.body, want)
		}
		if got := r.header.Get("anthropic-beta"); got != anthropicInlineToolsBeta {
			t.Errorf("request %d anthropic-beta = %q, want %q", i+1, got, anthropicInlineToolsBeta)
		}
	}
	if req.Tools[0].Name != "p__loaded" || len(req.LoadableTools) != 2 {
		t.Errorf("the request was changed: tools %+v, loadable %+v", req.Tools, req.LoadableTools)
	}
}

func TestAnthropicInlineToolsBetaJoins(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		thinking   string
		chatLoop   bool
		want       string
	}{
		{"with configured and existing betas", "other-beta-2025-01-01", "adaptive", true,
			"other-beta-2025-01-01," + anthropicCacheDiagnosisBeta + "," + anthropicThinkingBindingBeta + "," + anthropicInlineToolsBeta},
		{"already configured", anthropicInlineToolsBeta, "", true, anthropicInlineToolsBeta + "," + anthropicCacheDiagnosisBeta},
		{"not the chat loop", "other-beta-2025-01-01", "", false, "other-beta-2025-01-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, sent := diagnosticsServer(t, "")
			models := []ModelInfo{{ID: "claude-x", Thinking: tc.thinking, Features: []Feature{FeatureReasoning, FeatureInlineTools}}}
			p := NewAnthropicProvider("anthropic", server.URL, "key", models,
				WithAnthropicHeaders(map[string]string{"anthropic-beta": tc.configured}),
				WithAnthropicPromptCache(true), WithAnthropicCacheDiagnostics(true))
			completeInSession(t, p, "s1", tc.chatLoop)
			if got := sent()[0].header.Get("anthropic-beta"); got != tc.want {
				t.Errorf("anthropic-beta = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInlineToolsOffSendsTodaysBody(t *testing.T) {
	openAI := func(baseURL string, req *CompletionRequest) error {
		_, err := NewOpenAIProvider("openai", baseURL, "key", inlineModels(FeatureTools, FeatureInlineTools)).
			Complete(context.Background(), req)
		return err
	}
	cases := []struct {
		name     string
		complete func(string, *CompletionRequest) error
		req      *CompletionRequest
		wantBody string
		wantBeta string
	}{
		{"features absent or null", anthropicComplete(inlineModels()), inlineTestRequest(true), inlineTestChatLoopBody, ""},
		{"features empty", anthropicComplete([]ModelInfo{{ID: "claude-x", Features: []Feature{}}}), inlineTestRequest(true), inlineTestChatLoopBody, ""},
		{"an unknown feature name", anthropicComplete(inlineModels(FeatureTools, "inline-tools")), inlineTestRequest(true), inlineTestChatLoopBody, ""},
		{"the feature on another model of the provider", anthropicComplete([]ModelInfo{{ID: "claude-x"}, {ID: "claude-y", Features: []Feature{FeatureInlineTools}}}),
			inlineTestRequest(true), inlineTestChatLoopBody, ""},
		{"not the chat loop", anthropicComplete(inlineModels(FeatureInlineTools)), inlineTestRequest(false), inlineTestOtherBody, ""},
		{"an OpenAI-compatible provider", openAI, inlineTestRequest(true), inlineTestOpenAIBody, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sent := sentRequests(t, tc.complete, tc.req)
			if len(sent) != 1 || string(sent[0].body) != tc.wantBody {
				t.Fatalf("body:\n got %s\nwant %s", sent[0].body, tc.wantBody)
			}
			if got := sent[0].header.Get("anthropic-beta"); got != tc.wantBeta {
				t.Errorf("anthropic-beta = %q, want %q", got, tc.wantBeta)
			}
		})
	}
}

func TestCatalogSegment(t *testing.T) {
	catalog := "## Catalog\n"
	split := SystemSegments{Fixed: "Fixed.\n", Catalog: catalog, InlineCatalog: "## Every tool\n",
		Conversation: "Conversation.\n", Round: "Round."}
	if got, want := split.Join(), "Fixed.\n"+catalog+"Conversation.\nRound."; got != want {
		t.Fatalf("Join() = %q, want %q", got, want)
	}
	merged := SystemSegments{Fixed: split.Fixed, Conversation: catalog + split.Conversation, Round: split.Round}
	request := func(segs SystemSegments) *CompletionRequest {
		req := inlineTestRequest(true)
		req.Messages[0].Content = segs.Join()
		req.Segments = &segs
		return req
	}
	sent := sentRequests(t, anthropicComplete(inlineModels()), request(split), request(merged))
	if string(sent[0].body) != string(sent[1].body) {
		t.Errorf("body with the catalog in Catalog:\n%s\nwith it in Conversation:\n%s", sent[0].body, sent[1].body)
	}
}
