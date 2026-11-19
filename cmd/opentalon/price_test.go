package main

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

func TestRunPricesEachCallAtItsModelsTariff(t *testing.T) {
	captureLogs(t)
	anth := newFakeLLMServer(t,
		fakeAnswer{200, `{"id":"msg_1","model":"claude-x-20261101","content":[{"type":"tool_use","id":"toolu_1","name":"inv__list","input":{}}],` +
			`"stop_reason":"tool_use","usage":{"input_tokens":1000000,"output_tokens":100000}}`},
		fakeAnswer{400, anthInvalidRequest})
	def := newFakeLLMServer(t, fakeAnswer{200, `{"id":"c1","model":"default-model-2026","choices":[{"message":{"role":"assistant",` +
		`"content":"From the default."},"finish_reason":"stop"}],"usage":{"prompt_tokens":2000000,"completion_tokens":200000}}`})
	cfg, err := config.Parse([]byte(`
models:
  providers:
    primary:
      api: openai-completions
      base_url: ` + def.URL + `
      currency: EUR
      models:
        - id: default-model
          features: [tools]
    anthropic:
      api: anthropic-messages
      base_url: ` + anth.URL + `
      api_key: sk-ant-test
      currency: USD
      retry:
        max_attempts: 1
      models:
        - id: claude-x
          features: [tools]
routing:
  primary: primary/default-model
`))
	if err != nil {
		t.Fatal(err)
	}
	prov, defaultModel, err := buildProvider(cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := orchestrator.NewToolRegistry()
	if err := registry.Register(orchestrator.PluginCapability{
		Name:    "inv",
		Actions: []orchestrator.Action{{Name: "list", Description: "List items", AlwaysInclude: true, ReadOnly: true}},
	}, listExecutor{}); err != nil {
		t.Fatal(err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	usage := &usageSpy{}
	orch := orchestrator.NewWithRules(&defaultModelClient{provider: prov, model: defaultModel},
		orchestrator.DefaultParser, registry, state.NewMemoryStore(""), sessions, orchestrator.OrchestratorOpts{UsageRecorder: usage})
	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: "anthropic/claude-x",
		Tariffs: map[string]profile.Tariff{"claude-x": {Input: 3, Output: 15}, "default-model": {Input: 0.5, Output: 1}}})

	res, err := orch.Run(ctx, "s1", "List my items")
	if err != nil {
		t.Fatal(err)
	}
	if res.Response != fromDefault || len(anth.received()) != 2 || len(def.received()) != 1 {
		t.Fatalf("answered %q after %d Anthropic and %d default calls, want the default's answer after 2 and 1",
			res.Response, len(anth.received()), len(def.received()))
	}
	const want = (1_000_000*3+100_000*15)/1e6 + (2_000_000*0.5+200_000*1)/1e6
	if len(usage.rows) != 1 || usage.rows[0].price == nil {
		t.Fatalf("usage rows = %+v, want one with a price", usage.rows)
	}
	if got := *usage.rows[0].price; math.Abs(got-want) > 1e-9 {
		t.Errorf("row price = %v, want %v", got, want)
	}
}

type llmResponseSink struct {
	mu       sync.Mutex
	payloads []events.LLMResponsePayload
}

func (s *llmResponseSink) Emit(_ context.Context, e emit.Event) {
	if e.EventType != events.TypeLLMResponse {
		return
	}
	var p events.LLMResponsePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		panic(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payloads = append(s.payloads, p)
}

// A provider's currency belongs to the provider, not to the entries of its
// models list: a model it serves without an entry (here the routing model of a
// provider that lists none) is still priced in that currency.
func TestProviderCurrencyCoversAModelOutsideItsModelsList(t *testing.T) {
	captureLogs(t)
	def := newFakeLLMServer(t, fakeAnswer{200, `{"id":"c1","model":"m-2026","choices":[{"message":{"role":"assistant",` +
		`"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`})
	cfg, err := config.Parse([]byte(`
models:
  providers:
    primary:
      api: openai-completions
      base_url: ` + def.URL + `
      currency: EUR
routing:
  primary: primary/m
`))
	if err != nil {
		t.Fatal(err)
	}
	sink := &llmResponseSink{}
	prov, model, err := buildProvider(cfg, nil, nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1",
		Tariffs: map[string]profile.Tariff{"other": {Input: 1, Output: 1}}})

	resp, err := prov.Complete(ctx, &provider.CompletionRequest{Model: model, Messages: []provider.Message{{Role: provider.RoleUser, Content: "Hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.Price == nil || *resp.Usage.Price != 0 {
		t.Errorf("price = %v, want 0: a model without a tariff is priced at its EUR provider cost", resp.Usage.Price)
	}
	if len(sink.payloads) != 1 || sink.payloads[0].CostCurrency != "EUR" {
		t.Fatalf("llm_response events = %+v, want one with cost_currency EUR", sink.payloads)
	}
}
