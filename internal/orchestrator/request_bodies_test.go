package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

// requestBodiesFile holds the sha256 of every request body
// TestRun_RequestBodiesStayTheSame sends, keyed by variant and request number.
const requestBodiesFile = "testdata/request_bodies.json"

// bodyServer answers the requests of one variant in order, the last answer
// for every later request, and keeps each body it was sent.
type bodyServer struct {
	mu      sync.Mutex
	answers []string
	bodies  [][]byte
}

func (s *bodyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	answer := s.answers[min(len(s.bodies), len(s.answers)-1)]
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()
	if strings.HasPrefix(answer, "data: ") {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	_, _ = io.WriteString(w, answer)
}

func (s *bodyServer) sent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

// TestRun_RequestBodiesStayTheSame sends the two rounds of the segments
// fixture's turn (a tool call, then the answer) through each provider, the
// second round once more as a stream, and compares every body with the digest
// recorded for it. A deliberate change to a prompt or to how a request is put
// together records them again:
//
//	UPDATE_REQUEST_BODIES=1 go test -run TestRun_RequestBodiesStayTheSame ./internal/orchestrator/
func TestRun_RequestBodiesStayTheSame(t *testing.T) {
	tools := []provider.Feature{provider.FeatureTools}
	inline := []provider.Feature{provider.FeatureTools, provider.FeatureInlineTools}
	anthropicAnswers := []string{
		`{"id":"r1","model":"m","content":[{"type":"tool_use","id":"t1","name":"inventory__list-items","input":{}}],` +
			`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"id":"r2","model":"m","content":[{"type":"text","text":"Two items are low on stock."}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	anthropic := func(features []provider.Feature, opts ...provider.AnthropicOption) func(string) provider.Provider {
		return func(url string) provider.Provider {
			return provider.NewAnthropicProvider("anth", url, "key", []provider.ModelInfo{{ID: "m", Features: features}}, opts...)
		}
	}
	variants := []struct {
		name     string
		answers  []string
		provider func(url string) provider.Provider
		stream   bool
	}{
		{
			name: "openai",
			answers: []string{
				`{"id":"r1","model":"m","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1",` +
					`"type":"function","function":{"name":"inventory__list-items","arguments":"{}"}}]},"finish_reason":"tool_calls"}],` +
					`"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				`{"id":"r2","model":"m","choices":[{"message":{"role":"assistant","content":"Two items are low on stock."},` +
					`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				"data: {\"id\":\"r3\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Two items.\"}}]}\n\n" +
					"data: [DONE]\n\n",
			},
			provider: func(url string) provider.Provider {
				return provider.NewOpenAIProvider("oai", url, "key", []provider.ModelInfo{{ID: "m", Features: tools}})
			},
			stream: true,
		},
		{name: "anthropic", answers: anthropicAnswers, provider: anthropic(tools)},
		{name: "anthropic prompt cache", answers: anthropicAnswers, provider: anthropic(tools, provider.WithAnthropicPromptCache(true))},
		{
			name: "anthropic inline_tools prompt cache", answers: anthropicAnswers,
			provider: anthropic(inline, provider.WithAnthropicPromptCache(true)),
		},
	}

	got := map[string]string{}
	for _, v := range variants {
		server := &bodyServer{answers: v.answers}
		ts := httptest.NewServer(server)
		p := v.provider(ts.URL)
		llm := &recordingProvider{Provider: servedModel{Provider: p, model: "m"}}
		f := newSegmentsFixture(t, llm)
		if _, err := f.orch.Run(f.ctx, "s1", segmentsUserMessage); err != nil {
			ts.Close()
			t.Fatalf("%s: %v", v.name, err)
		}
		if len(llm.requests) != 2 {
			ts.Close()
			t.Fatalf("%s: chat loop made %d model calls, want 2", v.name, len(llm.requests))
		}
		if v.stream {
			streamed := *llm.requests[1]
			streamed.Model = "m"
			stream, err := p.Stream(context.Background(), &streamed)
			if err != nil {
				ts.Close()
				t.Fatalf("%s stream: %v", v.name, err)
			}
			for {
				chunk, err := stream.Recv()
				if err != nil {
					t.Fatalf("%s stream: %v", v.name, err)
				}
				if chunk.Done {
					break
				}
			}
			_ = stream.Close()
		}
		ts.Close()
		for i, body := range server.sent() {
			sum := sha256.Sum256(body)
			got[v.name+" "+strconv.Itoa(i+1)] = strconv.Itoa(len(body)) + " " + hex.EncodeToString(sum[:])
		}
	}

	if os.Getenv("UPDATE_REQUEST_BODIES") == "1" {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(requestBodiesFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(requestBodiesFile, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(requestBodiesFile)
	if err != nil {
		t.Fatalf("read the recorded digests: %v", err)
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if got[k] != want[k] {
			t.Errorf("request %s: body is %s (bytes, sha256), recorded %q", k, got[k], want[k])
		}
	}
	if len(got) != len(want) {
		t.Errorf("sent %d requests, %d are recorded", len(got), len(want))
	}
}
