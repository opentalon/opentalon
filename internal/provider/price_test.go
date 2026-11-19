package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

func tariffProfile(tariffs map[string]profile.Tariff) context.Context {
	return profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Tariffs: tariffs})
}

func responsePayload(t *testing.T, sink *recordingEventSink) (events.LLMResponsePayload, string) {
	t.Helper()
	for _, e := range sink.snapshot() {
		if e.EventType == events.TypeLLMResponse {
			var p events.LLMResponsePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			return p, string(e.Payload)
		}
	}
	t.Fatalf("no llm_response event in %v", sink.types())
	return events.LLMResponsePayload{}, ""
}

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

func samePrice(got *float64, want *float64) bool {
	if got == nil || want == nil {
		return got == want
	}
	return math.Abs(*got-*want) < 1e-9
}

func showPrice(p *float64) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprint(*p)
}

func TestAnthropicCallPrice(t *testing.T) {
	price := func(f float64) *float64 { return &f }
	cachedInput := 0.1
	const usage = `{"input_tokens":1000000,"cache_creation_input_tokens":2000000,"cache_read_input_tokens":10000000,"output_tokens":3000000}`
	cost := ModelCost{Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.1}
	const providerCost = 1 + 2.5 + 1 + 15
	sonnet := profile.Tariff{Input: 2, CachedInput: &cachedInput, Output: 10}
	cases := []struct {
		name     string
		ctx      context.Context
		currency string
		want     *float64
		wantWarn []string
	}{
		{name: "tariff with a cached-input price", ctx: tariffProfile(map[string]profile.Tariff{"claude-x": sonnet}),
			currency: "USD", want: price((3_000_000*2 + 10_000_000*0.1 + 3_000_000*10) / 1e6)},
		{name: "tariff without a cached-input price", ctx: tariffProfile(map[string]profile.Tariff{"claude-x": {Input: 2, Output: 10}}),
			currency: "USD", want: price((13_000_000*2 + 3_000_000*10) / 1e6)},
		{name: "a tariff of 0", ctx: tariffProfile(map[string]profile.Tariff{"claude-x": {CachedInput: new(float64)}}),
			currency: "USD", want: price(0)},
		{name: "no tariff, provider in EUR", ctx: tariffProfile(map[string]profile.Tariff{"other": sonnet}),
			currency: "EUR", want: price(providerCost), wantWarn: []string{"model=claude-x"}},
		{name: "no tariff, provider in USD", ctx: tariffProfile(map[string]profile.Tariff{"other": sonnet}),
			currency: "USD", wantWarn: []string{"model=claude-x", "currency=USD"}},
		{name: "no tariff, provider without a currency", ctx: tariffProfile(map[string]profile.Tariff{}),
			wantWarn: []string{"model=claude-x", "currency=none"}},
		{name: "a profile without tariffs", ctx: tariffProfile(nil), currency: "USD"},
		{name: "no profile", ctx: context.Background(), currency: "USD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarnings(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"r1","model":"claude-x-20261101","content":[{"type":"text","text":"ok"}],"usage":`+usage+`}`)
			}))
			defer server.Close()
			sink := &recordingEventSink{}
			p := NewAnthropicProvider("anthropic", server.URL, "key",
				[]ModelInfo{{ID: "claude-x", Cost: cost}}, WithAnthropicSessionEventSink(sink), WithAnthropicCurrency(tc.currency))
			resp, err := p.Complete(tc.ctx, &CompletionRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: "Hi"}}})
			if err != nil {
				t.Fatal(err)
			}
			payload, raw := responsePayload(t, sink)
			if !samePrice(resp.Usage.Price, tc.want) || !samePrice(payload.Price, tc.want) {
				t.Errorf("price = %s in the response and %s in the event, want %s", showPrice(resp.Usage.Price), showPrice(payload.Price), showPrice(tc.want))
			}
			if tc.want == nil && strings.Contains(raw, `"price"`) {
				t.Errorf("event carries a price: %s", raw)
			}
			if payload.CostCurrency != tc.currency {
				t.Errorf("event cost_currency = %q, want %q", payload.CostCurrency, tc.currency)
			}
			assertWarnings(t, logs.String(), tc.wantWarn)
		})
	}
}

func assertWarnings(t *testing.T, logs string, want []string) {
	t.Helper()
	var warns []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "tariff") {
			warns = append(warns, line)
		}
	}
	if len(want) == 0 {
		if len(warns) > 0 {
			t.Errorf("unexpected tariff warnings: %q", warns)
		}
		return
	}
	if len(warns) != 1 {
		t.Fatalf("tariff warnings %q, want one naming %q", warns, want)
	}
	for _, w := range want {
		if !strings.Contains(warns[0], w) {
			t.Errorf("warning %q does not name %s", warns[0], w)
		}
	}
}

func TestOpenAICallPrice(t *testing.T) {
	const in, out = 1_000_000, 500_000
	tariffs := map[string]profile.Tariff{"m": {Input: 1, Output: 4}}
	want := (in*1.0 + out*4.0) / 1e6
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"model":"m-2026","choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n")
					_, _ = fmt.Fprintf(w, `data: {"model":"m-2026","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],`+
						`"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`+"\n\n", in, out)
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"c1","model":"m-2026","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
					`"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, in, out)
			}))
			defer server.Close()
			sink := &recordingEventSink{}
			p := NewOpenAIProvider("ovh", server.URL, "key",
				[]ModelInfo{{ID: "m", Cost: ModelCost{Input: 0.08, Output: 0.4}}}, WithOpenAISessionEventSink(sink), WithOpenAICurrency("EUR"))
			req := &CompletionRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "Hi"}}}
			var got *float64
			if stream {
				s, err := p.Stream(tariffProfile(tariffs), req)
				if err != nil {
					t.Fatal(err)
				}
				for {
					chunk, err := s.Recv()
					if err != nil {
						t.Fatal(err)
					}
					if chunk.Usage.Price != nil {
						got = chunk.Usage.Price
					}
					if chunk.Done {
						break
					}
				}
				_ = s.Close()
			} else {
				resp, err := p.Complete(tariffProfile(tariffs), req)
				if err != nil {
					t.Fatal(err)
				}
				got = resp.Usage.Price
			}
			payload, _ := responsePayload(t, sink)
			if !samePrice(got, &want) || !samePrice(payload.Price, &want) {
				t.Errorf("price = %s returned and %s in the event, want %v", showPrice(got), showPrice(payload.Price), want)
			}
			if payload.CostCurrency != "EUR" {
				t.Errorf("event cost_currency = %q, want EUR", payload.CostCurrency)
			}
		})
	}
}
