package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSelectingProviderLeavesTheDefaultChainAlone checks the requests the
// selecting provider hands to its default provider untouched: one without a
// reference, as a sub-agent or repair call sends its own model, and one whose
// reference names the default model at the default provider, which another
// instance of that provider must not answer outside the default chain.
func TestSelectingProviderLeavesTheDefaultChainAlone(t *testing.T) {
	cases := []struct {
		name      string
		req       CompletionRequest
		wantModel string
	}{
		{"no reference", CompletionRequest{Model: "other/m2"}, "other/m2"},
		{"the default model", CompletionRequest{Model: "m1", ModelRef: "primary/m1"}, "m1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := &fakeProvider{id: "primary", model: "m1"}
			again := &fakeProvider{id: "primary", model: "m1"}
			other := &fakeProvider{id: "other", model: "m2"}
			s := NewSelectingProvider(context.Background(), def, "m1",
				map[string]SelectableProvider{"primary": {Prov: again}, "other": {Prov: other}}, HealthGateConfig{}, nil)

			resp, err := s.Complete(context.Background(), &tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Fallback {
				t.Error("the default model's answer to a request for it is marked as a fallback")
			}
			if def.calls != 1 || def.lastModel != tc.wantModel || again.calls+other.calls != 0 {
				t.Errorf("default got %d calls with model %q, defined providers %d; want 1 with %q and none",
					def.calls, def.lastModel, again.calls+other.calls, tc.wantModel)
			}
		})
	}
}

// newSelectedFixture is a selecting provider in front of def (model m1) with
// one provider, sel, serving m2 and failing with failWith. Its probes never
// tick by themselves; a test runs them with probeOnce.
func newSelectedFixture(t *testing.T, failWith error, probe HealthProbe) (*selectingProvider, *fakeProvider, *fakeProvider) {
	def := &fakeProvider{id: "primary", model: "m1"}
	sel := &fakeProvider{id: "sel", model: "m2", failNext: failWith != nil, failWith: failWith}
	s := NewSelectingProvider(t.Context(), def, "m1", map[string]SelectableProvider{"sel": {Prov: sel, Probe: probe}},
		HealthGateConfig{Interval: time.Hour, RecoverAfter: 1}, nil)
	return s.(*selectingProvider), def, sel
}

func selectedRequestOf() *CompletionRequest {
	return &CompletionRequest{Model: "m2", ModelRef: "sel/m2"}
}

// TestSelectedContextOverflow: a prompt longer than the selected model's
// window is the caller's to handle, as before, when the default model's
// window is no larger: the chat loop trims to that window and sends again.
// When the default model's window is larger, the default model answers. The
// provider stays healthy either way.
func TestSelectedContextOverflow(t *testing.T) {
	tooLong := &StatusError{api: "anthropic", StatusCode: 400,
		Body: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 200001 tokens > 200000 maximum"}}`}
	cases := []struct {
		name          string
		defaultWindow int
		defaultUsed   bool
	}{
		{"the default window is the same", 200_000, false},
		{"the default window is smaller", 128_000, false},
		{"the default window is larger", 400_000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, def, sel := newSelectedFixture(t, tooLong, func(context.Context) error { return nil })
			def.window, sel.window = tc.defaultWindow, 200_000
			resp, err := s.Complete(context.Background(), selectedRequestOf())
			if tc.defaultUsed {
				if err != nil || resp.ID != "primary" || !resp.Fallback {
					t.Fatalf("got %+v, %v; want the default's answer, marked as a fallback", resp, err)
				}
			} else if !errors.Is(err, tooLong) || def.calls != 0 {
				t.Fatalf("error = %v, default calls %d; want the overflow and none", err, def.calls)
			}
			if !s.providers["sel"].health.isHealthy() {
				t.Error("a prompt too long for the selected model marked its provider unhealthy")
			}
		})
	}
}

// oneChunk is a stream of one final chunk.
type oneChunk struct{}

func (oneChunk) Recv() (StreamChunk, error) { return StreamChunk{Content: "ok", Done: true}, nil }
func (oneChunk) Close() error               { return nil }

// TestSelectedStreamFailure: a selected provider that cannot stream hands
// the request back to the caller's Complete without a mark; one that is
// unavailable is marked and the default model streams instead, its chunks
// marked as a fallback.
func TestSelectedStreamFailure(t *testing.T) {
	cases := []struct {
		name        string
		failWith    error
		wantErr     error
		defaultUsed bool
	}{
		{"cannot stream", ErrStreamingNotImplemented, ErrStreamingNotImplemented, false},
		{"unavailable", &StatusError{api: "anthropic", StatusCode: 529, Body: "overloaded"}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, def, _ := newSelectedFixture(t, tc.failWith, func(context.Context) error { return nil })
			def.stream = oneChunk{}
			stream, err := s.Stream(context.Background(), selectedRequestOf())
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if used := def.calls == 1 && def.lastModel == "m1"; used != tc.defaultUsed {
				t.Errorf("default streamed: %v (calls %d, model %q), want %v", used, def.calls, def.lastModel, tc.defaultUsed)
			}
			if tc.defaultUsed {
				if c, err := stream.Recv(); err != nil || !c.Fallback {
					t.Errorf("default's chunk = %+v, %v; want it marked as a fallback", c, err)
				}
			}
			if healthy := s.providers["sel"].health.isHealthy(); healthy == tc.defaultUsed {
				t.Errorf("selected healthy = %v, want %v", healthy, !tc.defaultUsed)
			}
		})
	}
}

// TestSelectedRefusedWhileProbesAnswer: when the probe answers but every call
// is refused, each recovery costs the provider one call and the default model
// answers every request.
func TestSelectedRefusedWhileProbesAnswer(t *testing.T) {
	refused := &StatusError{api: "anthropic", StatusCode: 401, Body: "invalid x-api-key"}
	s, def, sel := newSelectedFixture(t, refused, func(context.Context) error { return nil })
	for i := 0; i < 3; i++ {
		resp, err := s.Complete(context.Background(), selectedRequestOf())
		if err != nil || resp.ID != "primary" || !resp.Fallback {
			t.Fatalf("request %d: %+v, %v; want the default's answer, marked as a fallback", i, resp, err)
		}
		if s.providers["sel"].health.isHealthy() {
			t.Fatalf("request %d: a refused call left the provider healthy", i)
		}
		s.providers["sel"].health.probeOnce(context.Background())
	}
	if sel.calls != 3 || def.calls != 3 {
		t.Errorf("selected got %d calls, default %d; want 3 each", sel.calls, def.calls)
	}
}
