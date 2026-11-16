package provider

import (
	"context"
	"testing"
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
			s := NewSelectingProvider(def, "m1", map[string]Provider{"primary": again, "other": other}, nil)

			if _, err := s.Complete(context.Background(), &tc.req); err != nil {
				t.Fatal(err)
			}
			if def.calls != 1 || def.lastModel != tc.wantModel || again.calls+other.calls != 0 {
				t.Errorf("default got %d calls with model %q, defined providers %d; want 1 with %q and none",
					def.calls, def.lastModel, again.calls+other.calls, tc.wantModel)
			}
		})
	}
}
