package store

import (
	"context"
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
	ModelID      string
	InputTokens  int
	OutputTokens int
	ToolCalls    int
	InputCost    float64
	OutputCost   float64
}

// UsageStore records LLM usage statistics per profile.
type UsageStore struct {
	db *DB
}

// NewUsageStore returns a UsageStore backed by db.
func NewUsageStore(db *DB) *UsageStore {
	return &UsageStore{db: db}
}

// Record inserts a usage record.
func (s *UsageStore) Record(ctx context.Context, r UsageRecord) error {
	id := "usg_" + uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.SQLDB().ExecContext(ctx, s.db.Dialect().Rebind(`
		INSERT INTO profile_usage
		  (id, entity_id, group_id, channel_id, session_id, model_id,
		   input_tokens, output_tokens, tool_calls,
		   input_cost, output_cost, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, r.EntityID, r.GroupID, r.ChannelID, r.SessionID, r.ModelID,
		r.InputTokens, r.OutputTokens, r.ToolCalls,
		r.InputCost, r.OutputCost, now)
	if err != nil {
		return fmt.Errorf("usage store: record: %w", err)
	}
	return nil
}
