package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
)

// TestRenderKnowledgeCatalog covers the pure rendering of the always-on
// knowledge catalog section: empty input yields nothing, invalid rows are
// skipped, valid rows render as "- Title (slug: `slug`)" sorted by title, then
// slug, and the anchor is included only when non-empty.
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

	// Sorted by title, then slug, whatever order the plugin returns them in.
	want := "\n## Available knowledge\na\n" +
		"- Orders (slug: `orders-a`)\n- Orders (slug: `orders-b`)\n- Tickets (slug: `tickets`)\n\n"
	for _, entries := range [][]knowledgeCatalogEntry{
		{{Slug: "tickets", Title: "Tickets"}, {Slug: "orders-b", Title: "Orders"}, {Slug: "orders-a", Title: "Orders"}},
		{{Slug: "orders-a", Title: "Orders"}, {Slug: "orders-b", Title: "Orders"}, {Slug: "tickets", Title: "Tickets"}},
	} {
		if got := renderKnowledgeCatalog(entries, "a"); got != want {
			t.Errorf("catalog order for %v:\n got %q\nwant %q", entries, got, want)
		}
	}

	// Anchor omitted when empty, but the section + rows still render.
	noAnchor := renderKnowledgeCatalog([]knowledgeCatalogEntry{{Slug: "s", Title: "T"}}, "")
	if !strings.Contains(noAnchor, "## Available knowledge") || !strings.Contains(noAnchor, "- T (slug: `s`)") {
		t.Errorf("no-anchor render wrong: %q", noAnchor)
	}
}

// The catalog comes from the knowledge plugin over the network, so the hot path
// only triggers a background fetch: without warming, the first prompt after
// startup lacks the section and the next one carries it.
func TestWarmKnowledgeCatalogPutsTheCatalogIntoTheFirstPrompt(t *testing.T) {
	newOrch := func(t *testing.T) *Orchestrator {
		registry := NewToolRegistry()
		if err := registry.Register(PluginCapability{
			Name: "knowledge", Description: "knowledge plugin",
			Actions: []Action{{Name: knowledgeCatalogAction, Description: "list"}},
		}, &fixedResultExecutor{content: `[{"slug":"returns","title":"Returns"}]`}); err != nil {
			t.Fatal(err)
		}
		sessions := state.NewSessionStore("")
		sessions.Create(state.SessionParams{ID: "s1"})
		return NewWithRules(&fakeLLM{}, &fakeParser{}, registry, state.NewMemoryStore(""), sessions,
			OrchestratorOpts{SyncActionsPlugin: "knowledge", SyncActionsAction: "sync"})
	}
	ctx := actor.WithSessionID(context.Background(), "s1")

	cold := newOrch(t)
	if prompt := cold.buildSystemPrompt(ctx, "q", true).Fixed; strings.Contains(prompt, "slug: `returns`") {
		t.Fatal("the first prompt of a cold orchestrator must not carry the catalog yet; the test premise is wrong")
	}

	warm := newOrch(t)
	warm.WarmKnowledgeCatalog(ctx)
	if prompt := warm.buildSystemPrompt(ctx, "q", true).Fixed; !strings.Contains(prompt, "slug: `returns`") {
		t.Fatalf("warmed orchestrator: first prompt lacks the catalog:\n%s", prompt)
	}
}
