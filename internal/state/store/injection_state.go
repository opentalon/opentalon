package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// InjectionState is the per-session knowledge / tool dedup bookkeeping
// persisted in sessions.injection_state (migration 010). Phase 3
// populates KnownKnowledge; KnownTools is reserved for Phase 4 and
// stays empty on the Phase-3 write path.
//
// The on-disk format mirrors the RFC #249 shape — JSON-encoded TEXT,
// not JSONB, so SQLite and PostgreSQL share one code path. New rows
// (created before this column existed) default to '{}' so the zero-
// valued struct represents "first turn, nothing known yet".
type InjectionState struct {
	KnownKnowledge []KnownKnowledgeEntry `json:"known_knowledge,omitempty"`
	KnownTools     []KnownToolEntry      `json:"known_tools,omitempty"`
}

// KnownKnowledgeEntry is one knowledge-article chunk the orchestrator
// has already seen this session. ContentSHA256 is the dedup key —
// different chunks of the same article have different SHAs, so chunk-
// level disjoint information correctly triggers re-injection.
// ArticleID is auxiliary: O(1) lookup for truncation/summarization
// release-paths and human-meaningful event-log strings.
type KnownKnowledgeEntry struct {
	ArticleID         string `json:"article_id"`
	ContentSHA256     string `json:"content_sha256"`
	FirstInjectedTurn int    `json:"first_injected_turn,omitempty"`
}

// KnownToolEntry is the Phase-4 tool-tier bookkeeping shape. Phase 3
// readers unmarshal existing entries to preserve forward-compatible
// rows, but the Phase-3 writer never produces a non-empty slice.
type KnownToolEntry struct {
	ToolName string `json:"tool_name"`
	Tier     string `json:"tier"`
	LRURank  int    `json:"lru_rank"`
	Demoted  bool   `json:"demoted"`
}

// GetInjectionState returns the deserialized dedup state for sessionID.
// An empty / NULL / "{}" column yields a zero-valued state without
// error — the caller treats that as "first turn, nothing known yet".
//
// A row that exists but contains invalid JSON returns the parse error
// so callers can decide between aborting the turn and dropping state
// to start fresh. Phase 3 always picks "start fresh" to keep service
// availability ahead of dedup correctness (see Orchestrator.applyKnowledgeDedup
// once that lands).
//
// Missing sessions return a `session %q not found` error, matching the
// shape SessionStore.Get returns. The underlying sql.ErrNoRows is not
// chained — consistent with the existing package-level convention.
func (s *SessionStore) GetInjectionState(ctx context.Context, sessionID string) (InjectionState, error) {
	if sessionID == "" {
		return InjectionState{}, fmt.Errorf("get injection state: session_id required")
	}
	d := s.db.Dialect()
	var raw string
	err := s.db.SQLDB().QueryRowContext(ctx,
		d.Rebind(`SELECT COALESCE(injection_state, '{}') FROM sessions WHERE id = ?`),
		sessionID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return InjectionState{}, fmt.Errorf("session %q not found", sessionID)
	}
	if err != nil {
		return InjectionState{}, fmt.Errorf("get injection state: %w", err)
	}
	if raw == "" || raw == "{}" {
		return InjectionState{}, nil
	}
	var state InjectionState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return InjectionState{}, fmt.Errorf("get injection state: parse: %w", err)
	}
	return state, nil
}

// UpdateInjectionState overwrites the persisted state for sessionID.
// The whole struct is rewritten — callers are expected to merge before
// calling, mirroring the read-modify-write pattern SetMetadata uses
// for the metadata column.
//
// The serialized bytes go into the TEXT column added by migration 010.
// updated_at is intentionally NOT bumped here: injection_state churns
// on every preparer pass and bumping the session-level timestamp would
// fight with the legitimate per-message touches in AddMessage.
func (s *SessionStore) UpdateInjectionState(ctx context.Context, sessionID string, state InjectionState) error {
	if sessionID == "" {
		return fmt.Errorf("update injection state: session_id required")
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("update injection state: marshal: %w", err)
	}
	d := s.db.Dialect()
	res, err := s.db.SQLDB().ExecContext(ctx,
		d.Rebind(`UPDATE sessions SET injection_state = ? WHERE id = ?`),
		string(payload), sessionID,
	)
	if err != nil {
		return fmt.Errorf("update injection state: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session %q not found", sessionID)
	}
	return nil
}
