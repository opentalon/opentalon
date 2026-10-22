package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
)

type scriptedUsageLLM struct {
	responses []provider.CompletionResponse
	calls     int
}

func (l *scriptedUsageLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if l.calls >= len(l.responses) {
		return nil, fmt.Errorf("no more responses")
	}
	resp := l.responses[l.calls]
	l.calls++
	return &resp, nil
}

// callOn returns a parser that reads a response equal to content as a call to
// gitlab.analyze_code and any other response as the final answer.
func callOn(content string) *fakeParser {
	return &fakeParser{parseFn: func(response string) []ToolCall {
		if response == content {
			return []ToolCall{{ID: "call_1", Plugin: "gitlab", Action: "analyze_code"}}
		}
		return nil
	}}
}

func TestRunRecordsTheSumOfTheProviderCosts(t *testing.T) {
	llm := &scriptedUsageLLM{responses: []provider.CompletionResponse{
		{Content: "call", Model: "m", Usage: provider.Usage{InputTokens: 1000, OutputTokens: 20, CacheReadTokens: 800, InputCost: 0.25, OutputCost: 0.125}},
		{Content: "done", Model: "m", Usage: provider.Usage{InputTokens: 1100, OutputTokens: 30, CacheReadTokens: 900, InputCost: 0.5, OutputCost: 1}},
	}}
	orch, sessID := setupOrchestrator(llm, callOn("call"))
	usage := &usageSpy{}
	orch.usageRecorder = usage

	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1"})
	if _, err := orch.Run(ctx, sessID, "Analyze my code"); err != nil {
		t.Fatal(err)
	}
	want := usageCall{entityID: "ent1", sessionID: sessID, modelID: "m", in: 2100, out: 50, inCost: 0.75, outCost: 1.125}
	if llm.calls != 2 || len(usage.calls) != 1 || usage.calls[0] != want {
		t.Fatalf("after %d model calls usage = %+v, want %+v", llm.calls, usage.calls, want)
	}
}

// TestRunOnOpenAICompatibleRecordsCostAsBefore checks a two-round turn on the
// OpenAI-compatible provider, plain and streaming, against the figure the
// usage recorder computed before the provider returned costs: the turn's
// tokens times the model's price.
func TestRunOnOpenAICompatibleRecordsCostAsBefore(t *testing.T) {
	price := provider.ModelCost{Input: 2, Output: 8}
	// Multiples of 15625 (1e6/64) keep every cost exact in floating point.
	rounds := []struct {
		content string
		in, out int
	}{{"call", 31250, 15625}, {"done", 46875, 15625}}

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var mu sync.Mutex
			n, streamed := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream bool `json:"stream"`
				}
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &req)
				mu.Lock()
				round := rounds[min(n, len(rounds)-1)]
				n++
				if req.Stream {
					streamed++
				}
				mu.Unlock()
				if req.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", round.content)
					_, _ = io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
					_, _ = fmt.Fprintf(w, "data: {\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d}}\n\n", round.in, round.out)
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
					`"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, round.content, round.in, round.out)
			}))
			defer server.Close()

			llm := provider.NewOpenAIProvider("oai", server.URL, "key", []provider.ModelInfo{{ID: "m", Cost: price}})
			orch, sessID := setupOrchestrator(llm, callOn("call"))
			usage := &usageSpy{}
			orch.usageRecorder = usage
			if stream {
				orch.onStreamChunk = func(context.Context, string, bool) {}
			}

			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1", Model: "oai/m"})
			if _, err := orch.Run(ctx, sessID, "Analyze my code"); err != nil {
				t.Fatal(err)
			}
			in, out := rounds[0].in+rounds[1].in, rounds[0].out+rounds[1].out
			want := usageCall{entityID: "ent1", sessionID: sessID, modelID: "m", in: in, out: out,
				inCost: float64(in) * price.Input / 1_000_000, outCost: float64(out) * price.Output / 1_000_000}
			mu.Lock()
			calls, streamedCalls := n, streamed
			mu.Unlock()
			if stream && streamedCalls != len(rounds) || !stream && streamedCalls != 0 {
				t.Fatalf("%d of %d model calls streamed", streamedCalls, calls)
			}
			if calls != len(rounds) || len(usage.calls) != 1 || usage.calls[0] != want {
				t.Fatalf("after %d model calls usage = %+v, want %+v", calls, usage.calls, want)
			}
		})
	}
}
