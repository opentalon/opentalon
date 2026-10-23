package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

// fakeProvider implements provider.Provider for testing.
type fakeProvider struct {
	models    []provider.ModelInfo
	lastReq   *provider.CompletionRequest
	streamReq *provider.CompletionRequest
}

func (f *fakeProvider) ID() string                   { return "fake" }
func (f *fakeProvider) Models() []provider.ModelInfo { return f.models }
func (f *fakeProvider) SupportsFeature(feat provider.Feature) bool {
	for _, m := range f.models {
		if m.SupportsFeature(feat) {
			return true
		}
	}
	return false
}
func (f *fakeProvider) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	f.lastReq = req
	return &provider.CompletionResponse{Content: "ok"}, nil
}
func (f *fakeProvider) Stream(_ context.Context, req *provider.CompletionRequest) (provider.ResponseStream, error) {
	f.streamReq = req
	return nil, nil
}

func TestDefaultModelClient_SupportsFeature_Reasoning(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{
			{ID: "gpt-oss-120b", Features: []provider.Feature{provider.FeatureStreaming, provider.FeatureReasoning}},
		},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b"}

	if !c.SupportsFeature(provider.FeatureReasoning) {
		t.Error("expected SupportsFeature(FeatureReasoning) = true")
	}
	if !c.SupportsFeature(provider.FeatureStreaming) {
		t.Error("expected SupportsFeature(FeatureStreaming) = true")
	}
	if c.SupportsFeature(provider.FeatureImages) {
		t.Error("expected SupportsFeature(FeatureImages) = false")
	}
}

func TestDefaultModelClient_SupportsFeature_NoReasoning(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{
			{ID: "some-model", Features: []provider.Feature{provider.FeatureStreaming}},
		},
	}
	c := &defaultModelClient{provider: fp, model: "some-model"}

	if c.SupportsFeature(provider.FeatureReasoning) {
		t.Error("expected SupportsFeature(FeatureReasoning) = false")
	}
}

func TestDefaultModelClient_Complete_PreservesReasoningFields(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b"}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b"}

	req := &provider.CompletionRequest{
		Messages:        []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		Reasoning:       true,
		ReasoningEffort: "high",
		BudgetTokens:    5000,
	}
	_, _ = c.Complete(context.Background(), req)

	if fp.lastReq == nil {
		t.Fatal("expected provider to receive request")
	}
	if fp.lastReq.Model != "gpt-oss-120b" {
		t.Errorf("expected model gpt-oss-120b, got %s", fp.lastReq.Model)
	}
	if !fp.lastReq.Reasoning {
		t.Error("expected Reasoning=true to be preserved")
	}
	if fp.lastReq.ReasoningEffort != "high" {
		t.Errorf("expected ReasoningEffort=high, got %s", fp.lastReq.ReasoningEffort)
	}
	if fp.lastReq.BudgetTokens != 5000 {
		t.Errorf("expected BudgetTokens=5000, got %d", fp.lastReq.BudgetTokens)
	}
}

func TestDefaultModelClient_Stream_PreservesReasoningFields(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b"}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b"}

	req := &provider.CompletionRequest{
		Messages:        []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		Reasoning:       true,
		ReasoningEffort: "medium",
	}
	_, _ = c.Stream(context.Background(), req)

	if fp.streamReq == nil {
		t.Fatal("expected provider to receive stream request")
	}
	if !fp.streamReq.Reasoning {
		t.Error("expected Reasoning=true to be preserved in stream")
	}
	if fp.streamReq.ReasoningEffort != "medium" {
		t.Errorf("expected ReasoningEffort=medium, got %s", fp.streamReq.ReasoningEffort)
	}
}

// failingProvider answers every completion and stream with an error.
type failingProvider struct{ fakeProvider }

func (f *failingProvider) Complete(context.Context, *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, errors.New("endpoint down")
}

func (f *failingProvider) Stream(context.Context, *provider.CompletionRequest) (provider.ResponseStream, error) {
	return nil, errors.New("endpoint down")
}

// TestDefaultModelClient_FallbackSendsItsOwnEffortAndMaxTokens checks that
// when the primary endpoint fails, an adaptive fallback model is sent its own
// configured effort and max_tokens, not the primary model's, while a turn's
// effort and a request's max_tokens are kept.
func TestDefaultModelClient_FallbackSendsItsOwnEffortAndMaxTokens(t *testing.T) {
	cases := []struct {
		name          string
		primaryEffort string
		turnEffort    string
		maxTokens     int
		want          string
		wantMaxTokens int
	}{
		{"primary with an effort", "high", "", 0, `{"effort":"low"}`, 2048},
		{"primary without effort", "", "", 0, `{"effort":"low"}`, 2048},
		{"the turn's effort", "high", "medium", 0, `{"effort":"medium"}`, 2048},
		{"the request's max_tokens", "high", "", 1000, `{"effort":"low"}`, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(body))
				mu.Unlock()
				_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"Hello."}],`+
					`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer server.Close()
			primary := &failingProvider{fakeProvider{models: []provider.ModelInfo{
				{ID: "primary-model", ReasoningEffort: tc.primaryEffort, MaxTokens: 8192}}}}
			fallback := provider.NewAnthropicProvider("anthropic", server.URL, "key",
				[]provider.ModelInfo{{ID: "claude-x", Thinking: "adaptive", ReasoningEffort: "low", MaxTokens: 2048}})
			hg := provider.NewHealthGatedProvider(context.Background(), []provider.ProviderEntry{
				{Prov: primary, Model: "primary-model"},
				{Prov: fallback, Model: "claude-x"},
			}, nil, provider.HealthGateConfig{}, nil)
			c := &defaultModelClient{provider: hg, model: "primary-model"}

			_, err := c.Complete(context.Background(), &provider.CompletionRequest{
				Messages:        []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
				ReasoningEffort: tc.turnEffort,
				MaxTokens:       tc.maxTokens,
			})
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 1 {
				t.Fatalf("fallback received %d requests, want 1", len(bodies))
			}
			var sent struct {
				OutputConfig json.RawMessage `json:"output_config"`
				MaxTokens    int             `json:"max_tokens"`
			}
			if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
				t.Fatal(err)
			}
			if string(sent.OutputConfig) != tc.want {
				t.Errorf("output_config = %s, want %s", sent.OutputConfig, tc.want)
			}
			if sent.MaxTokens != tc.wantMaxTokens {
				t.Errorf("max_tokens = %d, want %d", sent.MaxTokens, tc.wantMaxTokens)
			}
		})
	}
}

// TestDefaultModelClient_StreamFallbackSendsItsOwnMaxTokens checks the same
// for a stream: when the primary endpoint fails, an OpenAI-compatible fallback
// model is sent its own max_tokens, not the primary model's, while a
// request's max_tokens is kept.
func TestDefaultModelClient_StreamFallbackSendsItsOwnMaxTokens(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens int
		want      int
	}{
		{"the fallback model's max_tokens", 0, 2048},
		{"the request's max_tokens", 1000, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(body))
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			primary := &failingProvider{fakeProvider{models: []provider.ModelInfo{{ID: "primary-model", MaxTokens: 8192}}}}
			fallback := provider.NewOpenAIProvider("openai", server.URL, "key",
				[]provider.ModelInfo{{ID: "gpt-oss-120b", MaxTokens: 2048}})
			hg := provider.NewHealthGatedProvider(context.Background(), []provider.ProviderEntry{
				{Prov: primary, Model: "primary-model"},
				{Prov: fallback, Model: "gpt-oss-120b"},
			}, nil, provider.HealthGateConfig{}, nil)
			c := &defaultModelClient{provider: hg, model: "primary-model"}

			stream, err := c.Stream(context.Background(), &provider.CompletionRequest{
				Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
				MaxTokens: tc.maxTokens,
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = stream.Close()
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 1 {
				t.Fatalf("fallback received %d requests, want 1", len(bodies))
			}
			var sent struct {
				MaxTokens int `json:"max_tokens"`
			}
			if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
				t.Fatal(err)
			}
			if sent.MaxTokens != tc.want {
				t.Errorf("max_tokens = %d, want %d", sent.MaxTokens, tc.want)
			}
		})
	}
}

func TestDefaultModelClient_ReasoningEffortNotOverridden(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b", ReasoningEffort: "high"}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b"}

	req := &provider.CompletionRequest{
		Messages:        []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		ReasoningEffort: "low",
	}
	_, _ = c.Complete(context.Background(), req)

	if fp.lastReq.ReasoningEffort != "low" {
		t.Errorf("expected ReasoningEffort=low (explicit), got %s", fp.lastReq.ReasoningEffort)
	}
}
