package provider

import (
	"context"
	"slices"
	"testing"
)

func TestChain(t *testing.T) {
	dedicated := &fakeProvider{id: "dedicated", model: "m1", window: 131072, extra: []string{"m1-small"}}
	shared := &fakeProvider{id: "shared", model: "m1-shared", window: 131072}
	def := newTestHG(dedicated, shared, 1, nil)
	sel := &fakeProvider{id: "sel", model: "m2", window: 262144}
	s := NewSelectingProvider(context.Background(), def, "m1", map[string]SelectableProvider{"sel": {Prov: sel}}, HealthGateConfig{}, nil)

	for _, tc := range []struct {
		name  string
		p     Provider
		ref   ModelRef
		model string
		want  []ModelRef
	}{
		{"a selected model, then the default chain", s, "sel/m2", "m1", []ModelRef{"sel/m2", "dedicated/m1", "shared/m1-shared"}},
		{"no selection", s, "", "m1", []ModelRef{"dedicated/m1", "shared/m1-shared"}},
		{"a bare model the default provider serves", s, "m1-small", "m1-small", []ModelRef{"dedicated/m1-small", "shared/m1-shared"}},
		{"a reference no provider serves", s, "gone/m9", "m1", []ModelRef{"dedicated/m1", "shared/m1-shared"}},
		{"a bare provider", sel, "", "m2", []ModelRef{"sel/m2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []ModelRef
			for _, m := range Chain(tc.p, tc.ref, tc.model) {
				got = append(got, m.Ref())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("chain = %v, want %v", got, tc.want)
			}
		})
	}
}
