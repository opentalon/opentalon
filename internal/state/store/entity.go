package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// EntityStore tracks known entities (profile identities verified by WhoAmI).
type EntityStore struct {
	db *sql.DB
}

// NewEntityStore returns an EntityStore backed by db.
func NewEntityStore(db *DB) *EntityStore {
	return &EntityStore{db: db.db}
}

// Upsert records the entity, setting first_seen on insert and last_seen on every call.
func (s *EntityStore) Upsert(ctx context.Context, entityID, groupID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO entities (id, group_id, first_seen, last_seen) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET group_id = excluded.group_id, last_seen = excluded.last_seen`,
		entityID, groupID, now, now)
	if err != nil {
		return fmt.Errorf("entity store: upsert %s: %w", entityID, err)
	}
	return nil
}
