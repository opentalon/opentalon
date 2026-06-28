package orchestrator

import (
	"context"
	"strings"
	"testing"
)

func TestRenderTier2Section_EmptyTierReturnsEmptyString(t *testing.T) {
	registry := NewToolRegistry()
	if got := renderTier2Section(nil, registry); got != "" {
		t.Errorf("nil decision must return empty string, got %q", got)
	}
	if got := renderTier2Section(&toolTierDecision{}, registry); got != "" {
		t.Errorf("empty Tier2 must return empty string, got %q", got)
	}
}

func TestRenderTier2Section_NameAndOneLinerPerEntry(t *testing.T) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "p", Description: "P",
		Actions: []Action{
			{Name: "a", Description: "First action description."},
			{Name: "b", Description: "Second action.\nWith a second paragraph that must be dropped."},
		},
	}, &fixedResultExecutor{content: ""})

	d := &toolTierDecision{Tier2: []string{"p__a", "p__b"}}
	got := renderTier2Section(d, registry)

	if !strings.Contains(got, "## Tool catalog — name + one-line summary") {
		t.Errorf("missing header, got: %q", got)
	}
	if !strings.Contains(got, "- p__a: First action description.") {
		t.Errorf("missing first-line summary for p__a, got: %q", got)
	}
	// Second paragraph must be dropped.
	if !strings.Contains(got, "- p__b: Second action.") {
		t.Errorf("missing first-line summary for p__b, got: %q", got)
	}
	if strings.Contains(got, "second paragraph") {
		t.Errorf("second paragraph must be stripped from Tier 2 summary, got: %q", got)
	}
	if !strings.Contains(got, "_meta__get_tool_details") {
		t.Errorf("Tier 2 section must mention _meta__get_tool_details, got: %q", got)
	}
}

func TestRenderTier2Section_FQNWithoutDescriptionStillListed(t *testing.T) {
	// A tier-2 fqn whose action isn't found in the registry (race:
	// registry change between tier decision and prompt build) should
	// still render as a bare name — no panic, no empty trailing colon.
	registry := NewToolRegistry()
	d := &toolTierDecision{Tier2: []string{"unknown__x"}}
	got := renderTier2Section(d, registry)
	if !strings.Contains(got, "- unknown__x\n") {
		t.Errorf("missing-description fqn should render as bare name, got: %q", got)
	}
	if strings.Contains(got, "- unknown__x: \n") {
		t.Errorf("must not render empty trailing colon, got: %q", got)
	}
}

func TestRenderTier3Section_EmptyTierReturnsEmptyString(t *testing.T) {
	if got := renderTier3Section(nil); got != "" {
		t.Errorf("nil decision must return empty string, got %q", got)
	}
	if got := renderTier3Section(&toolTierDecision{}); got != "" {
		t.Errorf("empty Tier3 must return empty string, got %q", got)
	}
}

func TestRenderTier3Section_GroupsByPluginSorted(t *testing.T) {
	d := &toolTierDecision{Tier3: []string{
		"timly__list-items", "timly__show-item", "weather__forecast", "timly__create-ticket",
	}}
	got := renderTier3Section(d)

	if !strings.Contains(got, "## Other available tools (request details before use)") {
		t.Errorf("missing header, got: %q", got)
	}
	// timly group has 3 entries sorted alphabetically.
	wantTimly := "- timly: create-ticket, list-items, show-item"
	if !strings.Contains(got, wantTimly) {
		t.Errorf("missing or wrong-order timly entry, got: %q want substring %q", got, wantTimly)
	}
	// weather group has 1 entry.
	if !strings.Contains(got, "- weather: forecast") {
		t.Errorf("missing weather entry, got: %q", got)
	}
	// Plugin order: timly before weather (alphabetical).
	tIdx := strings.Index(got, "- timly:")
	wIdx := strings.Index(got, "- weather:")
	if tIdx < 0 || wIdx < 0 || tIdx > wIdx {
		t.Errorf("plugin groups must be alphabetical: timly idx %d, weather idx %d", tIdx, wIdx)
	}
}

func TestRenderTier3Section_SkipsMalformedFQNs(t *testing.T) {
	// "bareName" without a plugin separator can't be parsed. Skipped
	// silently — Tier 3 is best-effort context for the LLM, not a
	// safety surface.
	d := &toolTierDecision{Tier3: []string{"bareName", "p__a"}}
	got := renderTier3Section(d)
	if !strings.Contains(got, "- p: a") {
		t.Errorf("valid entry missing, got: %q", got)
	}
	if strings.Contains(got, "bareName") {
		t.Errorf("malformed fqn must be dropped, got: %q", got)
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"single line", "single line"},
		{"first\nsecond", "first"},
		{"  trim me  \nrest", "trim me"},
		{"\nempty first line", ""},
		{"trailing newline\n", "trailing newline"},
	}
	for _, c := range cases {
		if got := firstLine(c.in); got != c.want {
			t.Errorf("firstLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestToolTierDecisionContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := toolTierDecisionFromContext(ctx); got != nil {
		t.Errorf("empty ctx must yield nil, got %+v", got)
	}
	d := &toolTierDecision{Tier1: []string{"a__x"}}
	ctx = withToolTierDecision(ctx, d)
	got := toolTierDecisionFromContext(ctx)
	if got != d {
		t.Errorf("ctx round-trip lost the pointer: got %+v want %+v", got, d)
	}
}

func TestGroupTier3ByPlugin(t *testing.T) {
	got := groupTier3ByPlugin([]string{"a__x", "a__y", "b__z", "malformed"})
	if len(got["a"]) != 2 || got["a"][0] != "x" || got["a"][1] != "y" {
		t.Errorf("plugin a should have [x, y], got %v", got["a"])
	}
	if len(got["b"]) != 1 || got["b"][0] != "z" {
		t.Errorf("plugin b should have [z], got %v", got["b"])
	}
	if _, ok := got["malformed"]; ok {
		t.Errorf("malformed fqn must not produce a plugin entry, got %v", got)
	}
}
