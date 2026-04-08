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
	EntityID     string
	GroupID      string
	ChannelID    string
	SessionID    string
	InputTokens  int
	OutputTokens int
	ToolCalls    int
}

// UsageStore records LLM usage statistics per profile.
type UsageStore struct {
	db *sql.DB
}

// NewUsageStore returns a UsageStore backed by db.
func NewUsageStore(db *DB) *UsageStore {
	return &UsageStore{db: db.db}
}

// Record inserts a usage record.
func (s *UsageStore) Record(ctx context.Context, r UsageRecord) error {
	id := "usg_" + uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO profile_usage
		  (id, entity_id, group_id, channel_id, session_id, input_tokens, output_tokens, tool_calls, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, r.EntityID, r.GroupID, r.ChannelID, r.SessionID,
		r.InputTokens, r.OutputTokens, r.ToolCalls, now)
	if err != nil {
		return fmt.Errorf("usage store: record: %w", err)
	}
	return nil
}
