package orchestrator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/decideprovider"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

func TestRunRecordsTheSumOfTheCallPrices(t *testing.T) {
	price := func(f float64) *float64 { return &f }
	cases := []struct {
		name        string
		first, last *float64
		want        float64
		wantPriced  bool
	}{
		{"both calls priced", price(0.5), price(0.25), 0.75, true},
		{"one call priced", nil, price(0.25), 0.25, true},
		{"priced at 0", price(0), price(0), 0, true},
		{"no call priced", nil, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			llm := &scriptedUsageLLM{responses: []provider.CompletionResponse{
				{Content: "call", Model: "m", Usage: provider.Usage{InputTokens: 1000, OutputTokens: 20, Price: tc.first}},
				{Content: "done", Model: "m", Usage: provider.Usage{InputTokens: 1100, OutputTokens: 30, Price: tc.last}},
			}}
			orch, sessID := setupOrchestrator(llm, callOn("call"))
			usage := &usageSpy{}
			orch.usageRecorder = usage

			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1"})
			if _, err := orch.Run(ctx, sessID, "Analyze my code"); err != nil {
				t.Fatal(err)
			}
			if len(usage.calls) != 1 {
				t.Fatalf("usage = %+v, want one row", usage.calls)
			}
			if got := usage.calls[0]; got.priced != tc.wantPriced || !near(got.price, tc.want) {
				t.Errorf("row price = %v (priced %t), want %v (priced %t)", got.price, got.priced, tc.want, tc.wantPriced)
			}
		})
	}
}

func TestSubprocessRowCarriesThePriceOfItsModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","model":"sub-model-2026","choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":2000000,"completion_tokens":500000}}`)
	}))
	defer server.Close()
	llm := provider.NewOpenAIProvider("p", server.URL, "key", []provider.ModelInfo{{ID: "default-model"}, {ID: "sub-model"}})
	usage := &usageSpy{}
	orch := deadlineTestOrch(llm, OrchestratorOpts{UsageRecorder: usage})
	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1", Tariffs: map[string]profile.Tariff{
		"default-model": {Input: 9, Output: 9},
		"sub-model":     {Input: 0.5, Output: 2},
	}})
	ctx = actor.WithSessionID(ctx, "sess1")

	if _, err := orch.RunAction(ctx, "_subprocess", "run", map[string]string{"task": "x", "model": "sub-model"}); err != nil {
		t.Fatal(err)
	}
	if len(usage.calls) != 1 || !usage.calls[0].priced || !near(usage.calls[0].price, 2*0.5+0.5*2) {
		t.Fatalf("usage = %+v, want one row priced %v at sub-model's tariff", usage.calls, 2*0.5+0.5*2)
	}
}

func TestDecideRowCarriesTheDecidersPrice(t *testing.T) {
	d := &fakeDecider{name: "snap", dec: &decideprovider.Decision{Chosen: "match", Confidence: 1,
		Probabilities: map[string]float64{"match": 1, "mismatch": 0},
		Usage:         decideprovider.Usage{InputTokens: 2_000_000, OutputTokens: 500_000}}}
	usage := &usageSpy{}
	orch := NewWithRules(&fakeLLM{}, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), state.NewSessionStore(""),
		OrchestratorOpts{UsageRecorder: usage, Deciders: decideprovider.NewRegistry(d)})
	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1", Tariffs: map[string]profile.Tariff{
		"snap": {Input: 0.5, Output: 2},
	}})

	if _, _, err := orch.RunActionResult(ctx, "snap", "decide", map[string]string{"state": "s", "choices": `["match","mismatch"]`}); err != nil {
		t.Fatal(err)
	}
	if len(usage.calls) != 1 || usage.calls[0].modelID != "decide/snap" || !usage.calls[0].priced || !near(usage.calls[0].price, 2*0.5+0.5*2) {
		t.Fatalf("usage = %+v, want one decide/snap row priced %v", usage.calls, 2*0.5+0.5*2)
	}
}

func TestDecideRowIsPricedByTheModelItsDeciderServes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		const answer = `{"type":"choice","choice":"match","probabilities":{"match":1,"mismatch":0}}`
		_, _ = io.WriteString(w, `{"model":"subq-snap-2026","answers":{"decision":`+answer+`,"q":`+answer+`},`+
			`"usage":{"input_tokens":2000000,"output_tokens":500000}}`)
	}))
	defer server.Close()
	const want = 2*0.5 + 0.5*2
	cases := []struct {
		name, model, tariffed string
		args                  map[string]string
	}{
		{"the backend's default model", "", "subq-snap-preview", map[string]string{"state": "s", "choices": `["match","mismatch"]`}},
		{"the configured model", "subq-snap-2", "subq-snap-2", map[string]string{"state": "s", "choices": `["match","mismatch"]`}},
		{"several questions", "subq-snap-2", "subq-snap-2",
			map[string]string{"state": "s", "questions": `{"q":{"instructions":"i","choices":["match","mismatch"]}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deciders, err := decideprovider.RegistryFromConfigs(map[string]decideprovider.DeciderConfig{
				"snap": {Backend: decideprovider.BackendSubQ, BaseURL: server.URL, Model: tc.model},
			})
			if err != nil {
				t.Fatal(err)
			}
			usage := &usageSpy{}
			orch := NewWithRules(&fakeLLM{}, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), state.NewSessionStore(""),
				OrchestratorOpts{UsageRecorder: usage, Deciders: deciders})
			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1", Tariffs: map[string]profile.Tariff{
				"snap":      {Input: 9, Output: 9},
				tc.tariffed: {Input: 0.5, Output: 2},
			}})

			if _, _, err := orch.RunActionResult(ctx, "snap", "decide", tc.args); err != nil {
				t.Fatal(err)
			}
			if len(usage.calls) != 1 || usage.calls[0].modelID != "decide/snap" || !usage.calls[0].priced || !near(usage.calls[0].price, want) {
				t.Fatalf("usage = %+v, want one decide/snap row priced %v at %s's tariff", usage.calls, want, tc.tariffed)
			}
		})
	}
}
