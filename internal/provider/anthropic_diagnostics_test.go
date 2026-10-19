package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

// diagnosticsServer answers the n-th request with response id msg_<n> and the
// given raw diagnostics field, and records every request.
func diagnosticsServer(t *testing.T, diagnostics string) (*httptest.Server, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, capturedRequest{header: r.Header.Clone(), body: body})
		n := len(reqs)
		mu.Unlock()
		reply := fmt.Sprintf(`{"id":"msg_%d","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}`, n)
		if diagnostics != "" {
			reply += `,"diagnostics":` + diagnostics
		}
		_, _ = io.WriteString(w, reply+"}")
	}))
	t.Cleanup(server.Close)
	return server, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), reqs...)
	}
}

func completeInSession(t *testing.T, p *AnthropicProvider, session string, chatLoop bool) *CompletionResponse {
	t.Helper()
	segs := cacheTestSegments
	req := cacheTestRequest(segs, &segs)
	if !chatLoop {
		req.Segments = nil
	}
	resp, err := p.Complete(actor.WithSessionID(context.Background(), session), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func bodyDiagnostics(t *testing.T, body []byte) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	return string(fields["diagnostics"])
}

// TestAnthropicCacheDiagnostics covers R8's request side: with the flag on,
// every chat-loop request carries the beta header and the previous response
// id of its session, null on the session's first; other calls carry neither
// and leave the stored id alone. With the flag off, no request carries either.
func TestAnthropicCacheDiagnostics(t *testing.T) {
	steps := []struct {
		session  string
		chatLoop bool
		want     string // diagnostics sent with the flag on; "" = none
	}{
		{"s1", true, `{"previous_message_id":null}`},
		{"s1", true, `{"previous_message_id":"msg_1"}`},
		{"s1", false, ""},
		{"s2", true, `{"previous_message_id":null}`},
		{"s1", true, `{"previous_message_id":"msg_2"}`},
		{"", true, `{"previous_message_id":null}`},
		{"", true, `{"previous_message_id":null}`},
	}
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("cache_diagnostics=%t", on), func(t *testing.T) {
			server, sent := diagnosticsServer(t, "")
			p := NewAnthropicProvider("anthropic", server.URL, "key", nil,
				WithAnthropicPromptCache(true), WithAnthropicCacheDiagnostics(on))
			for _, step := range steps {
				completeInSession(t, p, step.session, step.chatLoop)
			}
			reqs := sent()
			for i, step := range steps {
				want := step.want
				if !on {
					want = ""
				}
				if got := bodyDiagnostics(t, reqs[i].body); got != want {
					t.Errorf("request %d (session %q): diagnostics = %s, want %s", i+1, step.session, got, want)
				}
				var wantBeta []string
				if want != "" {
					wantBeta = []string{anthropicCacheDiagnosisBeta}
				}
				if got := reqs[i].header.Values("Anthropic-Beta"); !reflect.DeepEqual(got, wantBeta) {
					t.Errorf("request %d (session %q): anthropic-beta = %q, want %q", i+1, step.session, got, wantBeta)
				}
			}
		})
	}
}

// TestAnthropicCacheMissReason covers R8's response side: the miss reason the
// API reports lands in the llm_response event, an unknown previous id
// included, and the request is sent once and answered normally.
func TestAnthropicCacheMissReason(t *testing.T) {
	cases := []struct {
		name        string
		diagnostics string
		want        string // "" = no cache_miss_reason in the event
	}{
		{"reported miss", `{"cache_miss_reason":{"type":"messages_changed","cache_missed_input_tokens":41850}}`, "messages_changed"},
		{"unknown previous id", `{"cache_miss_reason":{"type":"previous_message_not_found"}}`, "previous_message_not_found"},
		{"no divergence", `null`, ""},
		{"comparison still running", `{"cache_miss_reason":null}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, sent := diagnosticsServer(t, tc.diagnostics)
			sink := &recordingEventSink{}
			p := NewAnthropicProvider("anthropic", server.URL, "key", nil, WithAnthropicSessionEventSink(sink),
				WithAnthropicPromptCache(true), WithAnthropicCacheDiagnostics(true))
			if resp := completeInSession(t, p, "s1", true); resp.Content != "ok" {
				t.Errorf("content = %q, want the reply", resp.Content)
			}
			if n := len(sent()); n != 1 {
				t.Errorf("requests sent = %d, want 1", n)
			}
			got := sink.snapshot()
			if len(got) != 2 || got[1].EventType != events.TypeLLMResponse {
				t.Fatalf("events = %v, want llm_request then llm_response", sink.types())
			}
			var payload events.LLMResponsePayload
			if err := json.Unmarshal(got[1].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.CacheMissReason != tc.want {
				t.Errorf("cache_miss_reason = %q, want %q (payload %s)", payload.CacheMissReason, tc.want, got[1].Payload)
			}
		})
	}
}

// TestAnthropicDiagnosticsBetaJoinsConfiguredBeta checks that a configured
// anthropic-beta header and the cache-diagnosis beta are sent together.
func TestAnthropicDiagnosticsBetaJoinsConfiguredBeta(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		chatLoop   bool
		want       string
	}{
		{"chat loop", "other-beta-2025-01-01", true, "other-beta-2025-01-01," + anthropicCacheDiagnosisBeta},
		{"already configured", "other-beta-2025-01-01, " + anthropicCacheDiagnosisBeta, true, "other-beta-2025-01-01, " + anthropicCacheDiagnosisBeta},
		{"not the chat loop", "other-beta-2025-01-01", false, "other-beta-2025-01-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, sent := diagnosticsServer(t, "")
			p := NewAnthropicProvider("anthropic", server.URL, "key", nil,
				WithAnthropicHeaders(map[string]string{"anthropic-beta": tc.configured}),
				WithAnthropicPromptCache(true), WithAnthropicCacheDiagnostics(true))
			completeInSession(t, p, "s1", tc.chatLoop)
			if got := sent()[0].header.Values("Anthropic-Beta"); !reflect.DeepEqual(got, []string{tc.want}) {
				t.Errorf("anthropic-beta = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLastResponseIDsBounded checks that the per-session id store keeps at
// most its limit of sessions and drops the one updated longest ago.
func TestLastResponseIDsBounded(t *testing.T) {
	ids := newLastResponseIDs(2)
	ids.put("a", "1")
	ids.put("b", "2")
	ids.put("a", "3")
	ids.put("c", "4")
	for session, want := range map[string]string{"a": "3", "b": "", "c": "4"} {
		got := ids.get(session)
		switch {
		case want == "" && got != nil:
			t.Errorf("session %s: id %q kept, want it dropped", session, *got)
		case want != "" && (got == nil || *got != want):
			t.Errorf("session %s: id = %v, want %q", session, got, want)
		}
	}
	if len(ids.ids) != 2 || ids.order.Len() != 2 {
		t.Errorf("store holds %d sessions (%d in order), want 2", len(ids.ids), ids.order.Len())
	}
}
