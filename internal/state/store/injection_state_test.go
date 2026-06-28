package store

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/state"
)

// freshSessionStore opens a temp-dir DB, creates a session, and returns
// both the SessionStore and the session id. Used by every injection-
// state test so the table-and-row plumbing stays out of the case
// bodies.
func freshSessionStore(t *testing.T) (*SessionStore, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewSessionStore(db, 0, 0)
	sess := s.Create("sess-injection-test", "ent-1", "grp-1")
	if sess == nil {
		t.Fatal("Create returned nil session")
	}
	return s, sess.ID
}

func TestInjectionState_DefaultsToZeroValue(t *testing.T) {
	s, id := freshSessionStore(t)
	ctx := context.Background()
	got, err := s.GetInjectionState(ctx, id)
	if err != nil {
		t.Fatalf("GetInjectionState: %v", err)
	}
	if len(got.KnownTools) != 0 {
		t.Errorf("fresh session must yield zero-valued state, got %+v", got)
	}
}

func TestInjectionState_RoundTrip(t *testing.T) {
	s, id := freshSessionStore(t)
	ctx := context.Background()
	want := state.InjectionState{
		KnownTools: []state.KnownToolEntry{
			{ToolName: "weaviate__list-items", Tier: state.KnownToolTier1, LRURank: 3},
			{ToolName: "weaviate__get-item", Tier: state.KnownToolTier1, LRURank: 1},
		},
	}
	if err := s.UpdateInjectionState(ctx, id, want); err != nil {
		t.Fatalf("UpdateInjectionState: %v", err)
	}
	got, err := s.GetInjectionState(ctx, id)
	if err != nil {
		t.Fatalf("GetInjectionState: %v", err)
	}
	if len(got.KnownTools) != 2 {
		t.Fatalf("got %d known_tools entries, want 2", len(got.KnownTools))
	}
	if got.KnownTools[0].ToolName != "weaviate__list-items" || got.KnownTools[1].LRURank != 1 {
		t.Errorf("round-trip mismatch: %+v", got.KnownTools)
	}
}

func TestInjectionState_OverwritesPreviousState(t *testing.T) {
	s, id := freshSessionStore(t)
	ctx := context.Background()
	first := state.InjectionState{KnownTools: []state.KnownToolEntry{{ToolName: "tool_a", Tier: state.KnownToolTier1}}}
	if err := s.UpdateInjectionState(ctx, id, first); err != nil {
		t.Fatalf("UpdateInjectionState first: %v", err)
	}
	second := state.InjectionState{KnownTools: []state.KnownToolEntry{{ToolName: "tool_b", Tier: state.KnownToolTier1}}}
	if err := s.UpdateInjectionState(ctx, id, second); err != nil {
		t.Fatalf("UpdateInjectionState second: %v", err)
	}
	got, err := s.GetInjectionState(ctx, id)
	if err != nil {
		t.Fatalf("GetInjectionState: %v", err)
	}
	if len(got.KnownTools) != 1 || got.KnownTools[0].ToolName != "tool_b" {
		t.Errorf("overwrite expected only tool_b, got %+v", got.KnownTools)
	}
}

func TestInjectionState_PreservesToolFields(t *testing.T) {
	// Round-trip guarantee: a row written with KnownTools entries
	// preserves every field (tier, lru_rank, demoted) through
	// GetInjectionState.
	s, id := freshSessionStore(t)
	ctx := context.Background()
	want := state.InjectionState{
		KnownTools: []state.KnownToolEntry{
			{ToolName: "timly__list-items", Tier: state.KnownToolTier1, LRURank: 5, Demoted: false},
			{ToolName: "timly__broken-action", Tier: state.KnownToolTier3, LRURank: 2, Demoted: true},
		},
	}
	if err := s.UpdateInjectionState(ctx, id, want); err != nil {
		t.Fatalf("UpdateInjectionState: %v", err)
	}
	got, err := s.GetInjectionState(ctx, id)
	if err != nil {
		t.Fatalf("GetInjectionState: %v", err)
	}
	if len(got.KnownTools) != 2 || got.KnownTools[1].Demoted != true {
		t.Errorf("Phase-4 KnownTools round-trip mismatch: %+v", got.KnownTools)
	}
}

func TestInjectionState_InvalidJSONIsReportedNotSwallowed(t *testing.T) {
	s, id := freshSessionStore(t)
	ctx := context.Background()
	d := s.db.Dialect()
	if _, err := s.db.SQLDB().ExecContext(ctx,
		d.Rebind(`UPDATE sessions SET injection_state = ? WHERE id = ?`),
		"{not valid json", id,
	); err != nil {
		t.Fatalf("seed invalid json: %v", err)
	}
	_, err := s.GetInjectionState(ctx, id)
	if err == nil {
		t.Fatal("expected parse error for malformed injection_state, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error must surface parse failure, got %v", err)
	}
}

func TestInjectionState_MissingSessionReturnsError(t *testing.T) {
	s, _ := freshSessionStore(t)
	ctx := context.Background()
	_, err := s.GetInjectionState(ctx, "does-not-exist")
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error must signal not-found, got %v", err)
	}
	err = s.UpdateInjectionState(ctx, "does-not-exist", state.InjectionState{})
	if err == nil {
		t.Fatal("update on missing session must error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("update error must signal not-found, got %v", err)
	}
}

func TestInjectionState_EmptyAndNilSlicesAreOmittedOnMarshal(t *testing.T) {
	// Defensive guard against an `omitempty` tag-drift regression: both
	// nil and non-nil-empty slices must serialize to "{}" so on-disk
	// rows stay compact for sessions that have never run dedup.
	s, id := freshSessionStore(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		state state.InjectionState
	}{
		{"nil-slices", state.InjectionState{}},
		{"empty-non-nil-slices", state.InjectionState{
			KnownTools: []state.KnownToolEntry{},
		}},
	}
	d := s.db.Dialect()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.UpdateInjectionState(ctx, id, tc.state); err != nil {
				t.Fatalf("UpdateInjectionState: %v", err)
			}
			var raw string
			if err := s.db.SQLDB().QueryRowContext(ctx,
				d.Rebind(`SELECT injection_state FROM sessions WHERE id = ?`), id,
			).Scan(&raw); err != nil {
				t.Fatalf("read raw: %v", err)
			}
			if raw != "{}" {
				t.Errorf("on-disk row must collapse to %q, got %q", "{}", raw)
			}
		})
	}
}

func TestInjectionState_DefaultObjectUnmarshalsToNilSlices(t *testing.T) {
	// On a fresh session, the "{}" default produced by migration 010
	// must yield nil (not zero-length) slices so producers can rely on
	// `len(state.KnownTools) == 0` as the "first turn" sentinel
	// without worrying about nil-vs-empty semantics.
	s, id := freshSessionStore(t)
	ctx := context.Background()
	got, err := s.GetInjectionState(ctx, id)
	if err != nil {
		t.Fatalf("GetInjectionState: %v", err)
	}
	if got.KnownTools != nil {
		t.Errorf("KnownTools must be nil for a fresh row, got %#v", got.KnownTools)
	}
}

func TestInjectionState_IgnoresLegacyKnownKnowledgeKey(t *testing.T) {
	// Forward-compat: rows written before knowledge went pull-only carry
	// a `known_knowledge` JSON key. It is now an unknown field and must
	// be dropped on read without error, leaving KnownTools intact.
	s, id := freshSessionStore(t)
	ctx := context.Background()
	d := s.db.Dialect()
	if _, err := s.db.SQLDB().ExecContext(ctx,
		d.Rebind(`UPDATE sessions SET injection_state = ? WHERE id = ?`),
		`{"known_knowledge":[{"article_id":"kb_a","content_sha256":"aaa"}],"known_tools":[{"tool_name":"t","tier":"tier1"}]}`,
		id,
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	got, err := s.GetInjectionState(ctx, id)
	if err != nil {
		t.Fatalf("GetInjectionState: %v", err)
	}
	if len(got.KnownTools) != 1 || got.KnownTools[0].ToolName != "t" {
		t.Errorf("legacy row must round-trip KnownTools, got %+v", got.KnownTools)
	}
}

func TestInjectionState_RejectsEmptySessionID(t *testing.T) {
	s, _ := freshSessionStore(t)
	ctx := context.Background()
	if _, err := s.GetInjectionState(ctx, ""); err == nil || !strings.Contains(err.Error(), "session_id required") {
		t.Errorf("Get with empty session_id must error explicitly, got %v", err)
	}
	if err := s.UpdateInjectionState(ctx, "", state.InjectionState{}); err == nil || !strings.Contains(err.Error(), "session_id required") {
		t.Errorf("Update with empty session_id must error explicitly, got %v", err)
	}
}

func TestInjectionState_EmptyStateMarshalsAsObject(t *testing.T) {
	// Confirms json.Marshal of zero-value InjectionState produces "{}"
	// — the omitempty tags keep the on-disk row compact, which
	// matters because every session gets one of these rows from
	// migration 010 onward.
	s, id := freshSessionStore(t)
	ctx := context.Background()
	if err := s.UpdateInjectionState(ctx, id, state.InjectionState{}); err != nil {
		t.Fatalf("UpdateInjectionState: %v", err)
	}
	d := s.db.Dialect()
	var raw string
	if err := s.db.SQLDB().QueryRowContext(ctx,
		d.Rebind(`SELECT injection_state FROM sessions WHERE id = ?`), id,
	).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if raw != "{}" {
		t.Errorf("zero-value must serialize to %q, got %q", "{}", raw)
	}
}
