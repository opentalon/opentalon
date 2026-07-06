package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// UsageRecord captures LLM usage statistics for one orchestrator run.
type UsageRecord struct {
	EntityID        string
	GroupID         string
	ChannelID       string
	SessionID       string
	ModelID         string
	InteractionKind string // "chat" | "system"; empty is stored as "chat"
	SystemSource    string // per-feature label for system runs; empty stored as NULL
	InputTokens     int
	OutputTokens    int
	ToolCalls       int
	InputCost       float64
	OutputCost      float64
}

// UsageStore records LLM usage statistics per profile.
type UsageStore struct {
	db *DB
}

// NewUsageStore returns a UsageStore backed by db.
func NewUsageStore(db *DB) *UsageStore {
	return &UsageStore{db: db}
}

// TotalTokensSince returns the sum of input + output tokens for entityID
// recorded on or after since. Used to enforce per-profile token limits.
//
// Only interaction_kind='chat' runs count: a programmatic system run
// (interaction_kind='system') is attributed to the same entity for cost
// visibility but must not consume the interactive chat budget. The
// (entity_id, interaction_kind, created_at) index covers this predicate.
func (s *UsageStore) TotalTokensSince(ctx context.Context, entityID string, since time.Time) (int, error) {
	sinceStr := since.UTC().Format(time.RFC3339)
	row := s.db.SQLDB().QueryRowContext(ctx, s.db.Dialect().Rebind(`
		SELECT COALESCE(SUM(input_tokens + output_tokens), 0)
		FROM profile_usage
		WHERE entity_id = ? AND created_at >= ? AND interaction_kind = 'chat'`),
		entityID, sinceStr)
	var total int
	if err := row.Scan(&total); err != nil {
		return 0, fmt.Errorf("usage store: total tokens since: %w", err)
	}
	return total, nil
}

// Record inserts a usage record.
func (s *UsageStore) Record(ctx context.Context, r UsageRecord) error {
	id := "usg_" + uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	kind := r.InteractionKind
	if kind == "" {
		kind = "chat"
	}
	source := sql.NullString{String: r.SystemSource, Valid: r.SystemSource != ""}
	_, err := s.db.SQLDB().ExecContext(ctx, s.db.Dialect().Rebind(`
		INSERT INTO profile_usage
		  (id, entity_id, group_id, channel_id, session_id, model_id,
		   interaction_kind, system_source,
		   input_tokens, output_tokens, tool_calls,
		   input_cost, output_cost, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, r.EntityID, r.GroupID, r.ChannelID, r.SessionID, r.ModelID,
		kind, source,
		r.InputTokens, r.OutputTokens, r.ToolCalls,
		r.InputCost, r.OutputCost, now)
	if err != nil {
		return fmt.Errorf("usage store: record: %w", err)
	}
	return nil
}
