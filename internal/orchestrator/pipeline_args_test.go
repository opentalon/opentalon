package orchestrator

import (
	"encoding/json"
	"math"
	"testing"
)

// TestArgToWireString covers the type-aware stringification used at the
// pipeline → ToolCall.Args boundary. Every case where the previous default
// (fmt.Sprintf("%v", v)) lost information OR introduced surprises is
// captured here as a regression test.
func TestArgToWireString(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		// Plain scalars
		{"string", "hello", "hello"},
		{"empty string", "", ""},
		{"true", true, "true"},
		{"false", false, "false"},
		{"int", 42, "42"},
		{"int64 large", int64(2_000_000_000_000), "2000000000000"},
		{"nil", nil, "null"},

		// Floats — the regression heart. JSON unmarshal into interface{}
		// produces float64 for every numeric. Default %v on a 7-digit
		// integer-valued float renders as "2.037838e+06"; the wire layer
		// must keep the plain decimal so the downstream MCP server's
		// integer-typed schema still accepts it.
		{"float64 integer-valued", float64(2037838), "2037838"},
		{"float64 zero", float64(0), "0"},
		{"float64 negative integer", float64(-7), "-7"},
		{"float64 fractional", 1.5, "1.5"},
		{"float64 small", 0.001, "0.001"},

		// JSON-flavoured numerics (when planner uses json.Number, e.g. via
		// UseNumber()). Pass-through unchanged.
		{"json.Number int", json.Number("99"), "99"},
		{"json.Number float", json.Number("3.14"), "3.14"},

		// Compound values round-trip via JSON so the downstream plugin
		// can re-coerce per its declared input schema (array/object).
		{"[]any of strings", []any{"a", "b"}, `["a","b"]`},
		{"[]any of numbers", []any{float64(1), float64(2)}, `[1,2]`},
		{"map[string]any", map[string]any{"k": "v"}, `{"k":"v"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := argToWireString(c.in)
			if got != c.want {
				t.Errorf("argToWireString(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestArgToWireStringSpecialFloats(t *testing.T) {
	// NaN / ±Inf must not crash and must not produce "1.797e+308"-style
	// surprises that would slip through to the wire.
	if got := argToWireString(math.NaN()); got != "NaN" {
		t.Errorf("NaN → %q, want NaN", got)
	}
	if got := argToWireString(math.Inf(+1)); got != "+Inf" {
		t.Errorf("+Inf → %q, want +Inf", got)
	}
	if got := argToWireString(math.Inf(-1)); got != "-Inf" {
		t.Errorf("-Inf → %q, want -Inf", got)
	}
}

func TestPipelineArgsToWireMixed(t *testing.T) {
	// Mirrors a realistic post-substitution pipeline arg set: integer id
	// (from a list-* lookup), string query, array, optional bool flag.
	in := map[string]any{
		"item_id":        float64(2037838),
		"query":          "name:Tesla",
		"include_fields": []any{"id", "name", "type"},
		"published":      true,
	}
	out := pipelineArgsToWire(in)
	if out["item_id"] != "2037838" {
		t.Errorf("item_id = %q, want 2037838", out["item_id"])
	}
	if out["query"] != "name:Tesla" {
		t.Errorf("query = %q", out["query"])
	}
	if out["include_fields"] != `["id","name","type"]` {
		t.Errorf("include_fields = %q", out["include_fields"])
	}
	if out["published"] != "true" {
		t.Errorf("published = %q", out["published"])
	}
}
