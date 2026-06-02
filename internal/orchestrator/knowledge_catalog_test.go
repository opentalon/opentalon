package orchestrator

import (
	"strings"
	"testing"
)

// TestRenderKnowledgeCatalog covers the pure rendering of the always-on
// knowledge catalog section: empty input yields nothing, invalid rows are
// skipped, valid rows render as "- Title (slug: `slug`)" in input order, and
// the anchor is included only when non-empty.
func TestRenderKnowledgeCatalog(t *testing.T) {
	// No entries → empty section (so buildSystemPrompt emits nothing).
	if got := renderKnowledgeCatalog(nil, "anchor"); got != "" {
		t.Errorf("empty entries: want \"\", got %q", got)
	}

	// All-invalid entries (missing slug or title) → empty section.
	invalid := []knowledgeCatalogEntry{{Slug: "", Title: "x"}, {Slug: "y", Title: ""}}
	if got := renderKnowledgeCatalog(invalid, "a"); got != "" {
		t.Errorf("all-invalid entries: want \"\", got %q", got)
	}

	out := renderKnowledgeCatalog([]knowledgeCatalogEntry{
		{Slug: "categories", Title: "Categories"},
		{Slug: "tickets", Title: "Tickets"},
	}, "Consult the knowledge base first.")

	for _, want := range []string{
		"## Available knowledge",
		"Consult the knowledge base first.",
		"- Categories (slug: `categories`)",
		"- Tickets (slug: `tickets`)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered catalog missing %q\n--- got ---\n%s", want, out)
		}
	}

	// Input order is preserved (the plugin already sorts by title).
	if strings.Index(out, "categories") > strings.Index(out, "tickets") {
		t.Errorf("entry order not preserved:\n%s", out)
	}

	// Anchor omitted when empty, but the section + rows still render.
	noAnchor := renderKnowledgeCatalog([]knowledgeCatalogEntry{{Slug: "s", Title: "T"}}, "")
	if !strings.Contains(noAnchor, "## Available knowledge") || !strings.Contains(noAnchor, "- T (slug: `s`)") {
		t.Errorf("no-anchor render wrong: %q", noAnchor)
	}
}
