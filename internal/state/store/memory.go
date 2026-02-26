package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
)

// MemoryStore is the SQLite-backed memory store with general (actor_id NULL) and per-actor scope.
type MemoryStore struct {
	db *DB
}

// NewMemoryStore returns a memory store that uses the given DB.
func NewMemoryStore(db *DB) *MemoryStore {
	return &MemoryStore{db: db}
}

// AddScoped inserts a memory. If actorID is empty, it is stored as general (actor_id NULL).
// Tags are stored as JSON. Persisted immediately.
func (s *MemoryStore) AddScoped(ctx context.Context, actorID string, content string, tags ...string) (*state.Memory, error) {
	id := "mem_" + uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("memory add: marshal tags: %w", err)
	}
	var aid *string
	if actorID != "" {
		aid = &actorID
	}
	_, err = s.db.SQLDB().ExecContext(ctx,
		`INSERT INTO memories (id, actor_id, content, tags, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, aid, content, string(tagsJSON), now)
	if err != nil {
		return nil, fmt.Errorf("memory add: %w", err)
	}
	return &state.Memory{
		ID:        id,
		Content:   content,
		Tags:      tags,
		CreatedAt: mustParseTime(now),
	}, nil
}

// MemoriesForContext returns memories visible to the current actor: all general (actor_id IS NULL)
// plus all for actor.Actor(ctx), optionally filtered by tag. Empty tag means no filter.
func (s *MemoryStore) MemoriesForContext(ctx context.Context, tag string) ([]*state.Memory, error) {
	actorID := actor.Actor(ctx)
	query := `SELECT id, actor_id, content, tags, created_at FROM memories WHERE (actor_id IS NULL OR actor_id = ?)`
	args := []interface{}{actorID}
	if tag != "" {
		query += ` AND (tags LIKE ? OR tags LIKE ? OR tags LIKE ?)`
		// JSON array: "tag", "tag" at start, or in middle
		args = append(args, `%"`+tag+`"%`, `"`+tag+`"%`, `%,"`+tag+`"%`)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.SQLDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("memories for context: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*state.Memory
	for rows.Next() {
		var id, content, tagsJSON, createdAt string
		var actorIDNull *string
		if err := rows.Scan(&id, &actorIDNull, &content, &tagsJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("memories scan: %w", err)
		}
		var tags []string
		if tagsJSON != "" {
			_ = json.Unmarshal([]byte(tagsJSON), &tags)
		}
		t, _ := time.Parse(time.RFC3339, createdAt)
		out = append(out, &state.Memory{
			ID:        id,
			Content:   content,
			Tags:      tags,
			CreatedAt: t,
		})
	}
	return out, rows.Err()
}

// Search returns memories whose content contains the query (case-insensitive). No actor scope; global only.
// For backward compatibility when needed.
func (s *MemoryStore) Search(query string) []*state.Memory {
	lower := strings.ToLower(query)
	rows, err := s.db.SQLDB().Query(
		`SELECT id, actor_id, content, tags, created_at FROM memories WHERE LOWER(content) LIKE ? ORDER BY created_at DESC`,
		"%"+lower+"%")
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	return scanMemories(rows)
}

// SearchByTag returns all memories that have the given tag. No actor scope; global only.
func (s *MemoryStore) SearchByTag(tag string) []*state.Memory {
	rows, err := s.db.SQLDB().Query(
		`SELECT id, actor_id, content, tags, created_at FROM memories WHERE tags LIKE ? OR tags LIKE ? OR tags LIKE ? ORDER BY created_at DESC`,
		`%"`+tag+`"%`, `"`+tag+`"%`, `%,"`+tag+`"%`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	return scanMemories(rows)
}

func scanMemories(rows *sql.Rows) []*state.Memory {
	var out []*state.Memory
	for rows.Next() {
		var id, content, tagsJSON, createdAt string
		var actorIDNull *string
		if err := rows.Scan(&id, &actorIDNull, &content, &tagsJSON, &createdAt); err != nil {
			return nil
		}
		var tags []string
		if tagsJSON != "" {
			_ = json.Unmarshal([]byte(tagsJSON), &tags)
		}
		t, _ := time.Parse(time.RFC3339, createdAt)
		out = append(out, &state.Memory{ID: id, Content: content, Tags: tags, CreatedAt: t})
	}
	return out
}

func mustParseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
