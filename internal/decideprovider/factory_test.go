package decideprovider

import (
	"math"
	"strings"
	"testing"
)

func TestFromConfig_SelectsBackend(t *testing.T) {
	cases := []struct {
		backend string
		want    string
	}{
		{BackendLaya, "*decideprovider.LayaProvider"},
		{BackendJev, "*decideprovider.SystemOneProvider"},
		{BackendSubQ, "*decideprovider.SystemOneProvider"},
		{BackendLocalLogits, "*decideprovider.LocalLogitsProvider"},
	}
	for _, tc := range cases {
		p, err := FromConfig(DeciderConfig{Name: "d", Backend: tc.backend, BaseURL: "http://x"})
		if err != nil {
			t.Fatalf("%s: %v", tc.backend, err)
		}
		if got := typeName(p); got != tc.want {
			t.Errorf("%s -> %s, want %s", tc.backend, got, tc.want)
		}
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *LayaProvider:
		return "*decideprovider.LayaProvider"
	case *SystemOneProvider:
		return "*decideprovider.SystemOneProvider"
	case *LocalLogitsProvider:
		return "*decideprovider.LocalLogitsProvider"
	default:
		return "unknown"
	}
}

func TestFromConfig_Errors(t *testing.T) {
	if _, err := FromConfig(DeciderConfig{Backend: BackendLaya, BaseURL: "http://x"}); err == nil {
		t.Error("expected error for missing name")
	}
	if _, err := FromConfig(DeciderConfig{Name: "d", Backend: BackendLaya}); err == nil {
		t.Error("expected error for missing base_url")
	}
	if _, err := FromConfig(DeciderConfig{Name: "d", BaseURL: "http://x"}); err == nil {
		t.Error("expected error for missing backend")
	}
	if _, err := FromConfig(DeciderConfig{Name: "d", Backend: "bogus", BaseURL: "http://x"}); err == nil {
		t.Error("expected error for unknown backend")
	}
	for _, cost := range []Cost{{Input: -1}, {Output: -0.5}, {Input: math.NaN()}, {Output: math.Inf(1)}} {
		if _, err := FromConfig(DeciderConfig{Name: "d", Backend: BackendSubQ, BaseURL: "http://x", Cost: cost}); err == nil {
			t.Errorf("expected error for cost %+v", cost)
		}
	}
	if _, err := FromConfig(DeciderConfig{Name: "d", Backend: BackendSubQ, BaseURL: "http://x", Cost: Cost{Input: 0.05}}); err != nil {
		t.Errorf("a valid cost is rejected: %v", err)
	}
}

func TestRegistryFromConfigs(t *testing.T) {
	reg, err := RegistryFromConfigs(map[string]DeciderConfig{
		"decider": {Backend: BackendLaya, BaseURL: "http://laya"},
		"jev":     {Backend: BackendJev, BaseURL: "http://jev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Has("decider") || !reg.Has("jev") {
		t.Error("registry missing configured deciders")
	}
	if reg.Has("nope") {
		t.Error("registry reports an unconfigured decider")
	}
	if _, ok := reg.Get("decider"); !ok {
		t.Error("Get failed for configured decider")
	}
	if got := reg.Names(); len(got) != 2 || got[0] != "decider" || got[1] != "jev" {
		t.Errorf("Names = %v, want [decider jev]", got)
	}
	var nilReg *Registry
	if nilReg.Names() != nil {
		t.Error("nil registry should have no names")
	}
	if nilReg.Cost("jev") != (Cost{}) {
		t.Error("nil registry should have no costs")
	}
}

func TestRegistryFromConfigsKeepsCosts(t *testing.T) {
	reg, err := RegistryFromConfigs(map[string]DeciderConfig{
		"snap": {Backend: BackendSubQ, BaseURL: "http://subq", Cost: Cost{Input: 0.05, Output: 1}},
		"jev":  {Backend: BackendJev, BaseURL: "http://jev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Cost("snap"); got != (Cost{Input: 0.05, Output: 1}) {
		t.Errorf("snap cost = %+v", got)
	}
	if got := reg.Cost("jev"); got != (Cost{}) {
		t.Errorf("an unpriced decider costs %+v, want zero", got)
	}
	if _, err := RegistryFromConfigs(map[string]DeciderConfig{"bad": {Backend: BackendSubQ, BaseURL: "http://subq", Cost: Cost{Input: -1}}}); err == nil {
		t.Error("a negative cost should fail the registry")
	}
}

func TestCostPrice(t *testing.T) {
	u := Cost{Input: 0.5, Output: 4}.Price(Usage{InputTokens: 2_000_000, OutputTokens: 250_000})
	if u.InputCost != 1 || u.OutputCost != 1 || u.InputTokens != 2_000_000 || u.OutputTokens != 250_000 {
		t.Errorf("priced usage = %+v", u)
	}
	if u := (Cost{}).Price(Usage{InputTokens: 10, OutputTokens: 10}); u.InputCost != 0 || u.OutputCost != 0 {
		t.Errorf("zero cost priced usage = %+v", u)
	}
}

func TestValidateName(t *testing.T) {
	for name, ok := range map[string]bool{
		"jev": true, "jev-small": true, "laya_v2": true,
		"": false, "jev.small": false, "_notify": false, "has space": false,
		strings.Repeat("a", 56): true, strings.Repeat("a", 57): false,
	} {
		if err := ValidateName(name); (err == nil) != ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
	if _, err := FromConfig(DeciderConfig{Name: "jev.small", Backend: BackendJev, BaseURL: "http://x"}); err == nil {
		t.Error("FromConfig must reject an invalid name")
	}
}
