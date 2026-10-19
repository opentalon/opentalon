package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

// fakeMessagesAPI stands in for Anthropic's Messages API. It keeps each
// request body as sent and answers a request whose last message is a tool
// result with text, any other with a call to inventory__list-items, so every
// turn takes two rounds and loads no tool.
type fakeMessagesAPI struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (f *fakeMessagesAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	n := len(f.bodies)
	f.mu.Unlock()
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	w.Header().Set("Content-Type", "application/json")
	if len(req.Messages) > 0 && bytes.Contains(req.Messages[len(req.Messages)-1].Content, []byte(`"tool_result"`)) {
		_, _ = fmt.Fprintf(w, `{"id":"msg_%d","model":"claude-x","content":[{"type":"text","text":"Two items."}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, n)
		return
	}
	_, _ = fmt.Fprintf(w, `{"id":"msg_%d","model":"claude-x","content":[{"type":"tool_use","id":"toolu_%d",`+
		`"name":"inventory__list-items","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`, n, n)
}

// cachedPrefix holds the parts of a request body Anthropic reads up to the
// first cache marker (tools, model, thinking, first system block), plus the
// messages and the automatic marker.
type cachedPrefix struct {
	Tools        json.RawMessage   `json:"tools"`
	Model        json.RawMessage   `json:"model"`
	Thinking     json.RawMessage   `json:"thinking"`
	System       []json.RawMessage `json:"system"`
	Messages     []json.RawMessage `json:"messages"`
	CacheControl json.RawMessage   `json:"cache_control"`
}

// TestAnthropicCachedPrefixIsStable renders the chat loop's requests on the
// wire through the Anthropic provider with prompt caching on: two turns of
// two rounds in one conversation, then a turn in a second conversation with
// the same plugins and tools. Everything up to the first cache marker is the
// same bytes in every request, and within a turn each request's messages
// begin with the previous request's messages. No round adds a one-round
// message (nudge, retry, warning).
func TestAnthropicCachedPrefixIsStable(t *testing.T) {
	api := &fakeMessagesAPI{}
	server := httptest.NewServer(api)
	defer server.Close()
	llm, err := provider.FromConfig(provider.ProviderConfig{
		ID: "anthropic", API: provider.APIAnthropic, BaseURL: server.URL, APIKey: "key", PromptCache: true,
		Models: []provider.ModelInfo{{ID: "claude-x", Features: []provider.Feature{provider.FeatureTools, provider.FeatureReasoning}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "inventory", Description: "Inventory tools",
		SystemPromptAddition: "Count items with list-items.",
		Actions: []Action{
			native(Action{Name: "list-items", Description: "List items", ReadOnly: true}),
			{Name: "add-to-stock", Description: "Add stock"},
		},
	}, &echoExecutor{}); err != nil {
		t.Fatal(err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	sessions.Create(state.SessionParams{ID: "s2"})
	orch := NewWithRules(llm, DefaultParser, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		Subprocess: SubprocessConfig{Enabled: true},
	})
	conversation := func(id string) context.Context {
		ctx := profile.WithProfile(context.Background(), &profile.Profile{ChannelID: "web", Name: "Ada", Model: "anthropic/claude-x"})
		ctx = actor.WithConversationID(ctx, id)
		return pkgchannel.WithCapabilities(ctx, pkgchannel.Capabilities{ResponseFormat: pkgchannel.FormatMarkdown})
	}

	turns := []struct {
		ctx       context.Context
		sessionID string
		message   string
	}{
		{conversation("conv-1"), "s1", "How many items are low on stock?"},
		{conversation("conv-1"), "s1", "And how many are out of stock?"},
		{conversation("conv-2"), "s2", "How many items are low on stock?"},
	}
	var labels []string
	for i, turn := range turns {
		if _, err := orch.Run(turn.ctx, turn.sessionID, turn.message); err != nil {
			t.Fatal(err)
		}
		labels = append(labels, fmt.Sprintf("turn %d round 1", i+1), fmt.Sprintf("turn %d round 2", i+1))
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.bodies) != len(labels) {
		t.Fatalf("model calls = %d, want %d (two rounds per turn)", len(api.bodies), len(labels))
	}
	requests := make([]cachedPrefix, len(api.bodies))
	for i, body := range api.bodies {
		if err := json.Unmarshal(body, &requests[i]); err != nil {
			t.Fatalf("%s: system is not cache-marked blocks: %v\n%s", labels[i], err, body)
		}
		r := requests[i]
		if len(r.Tools) == 0 || len(r.Thinking) == 0 || len(r.System) == 0 {
			t.Fatalf("%s: want tools, thinking and system blocks:\n%s", labels[i], body)
		}
		var first struct {
			CacheControl *anthropicCacheControl `json:"cache_control"`
		}
		if err := json.Unmarshal(r.System[0], &first); err != nil || first.CacheControl == nil ||
			first.CacheControl.Type != "ephemeral" || string(r.CacheControl) != `{"type":"ephemeral"}` {
			t.Fatalf("%s: want a cache marker on the first system block and the automatic marker:\n%s", labels[i], body)
		}
	}

	want := requests[0]
	for i, r := range requests[1:] {
		label := labels[i+1]
		for _, part := range []struct {
			name      string
			got, want []byte
		}{
			{"tools", r.Tools, want.Tools},
			{"model", r.Model, want.Model},
			{"thinking", r.Thinking, want.Thinking},
			{"first system block", r.System[0], want.System[0]},
		} {
			if !bytes.Equal(part.got, part.want) {
				t.Errorf("%s: %s differs from turn 1 round 1:\n got %s\nwant %s", label, part.name, part.got, part.want)
			}
		}
	}
	for i := 1; i < len(requests); i += 2 {
		prev, cur := requests[i-1].Messages, requests[i].Messages
		if len(cur) < len(prev) {
			t.Errorf("%s: %d messages, fewer than the round before", labels[i], len(cur))
			continue
		}
		for j := range prev {
			if !bytes.Equal(cur[j], prev[j]) {
				t.Errorf("%s: message %d differs from the round before:\n got %s\nwant %s", labels[i], j, cur[j], prev[j])
			}
		}
	}
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}
