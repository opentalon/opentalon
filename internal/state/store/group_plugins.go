package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// GroupPluginStore persists group → plugin assignments to SQLite.
type GroupPluginStore struct {
	db *sql.DB
}

// NewGroupPluginStore returns a GroupPluginStore backed by db.
func NewGroupPluginStore(db *DB) *GroupPluginStore {
	return &GroupPluginStore{db: db.db}
}

// PluginsForGroup returns all plugin IDs assigned to groupID.
func (s *GroupPluginStore) PluginsForGroup(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT plugin_id FROM group_plugins WHERE group_id = ?`, groupID)
	if err != nil {
		return nil, fmt.Errorf("group_plugins: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("group_plugins: scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UpsertGroupPlugins upserts the given plugin IDs for groupID with the given source.
// It will not downgrade an existing row to a lower-priority source (config < whoami < admin).
// All writes are batched in a single transaction using a prepared statement.
func (s *GroupPluginStore) UpsertGroupPlugins(ctx context.Context, groupID string, pluginIDs []string, source string) error {
	if len(pluginIDs) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)

	// Priority is encoded directly in SQL so no per-row SELECT is needed.
	// The ON CONFLICT WHERE clause only performs the UPDATE when the incoming
	// source has equal or higher priority than the stored one.
	const upsertSQL = `
INSERT INTO group_plugins (group_id, plugin_id, source, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (group_id, plugin_id) DO UPDATE SET
    source     = excluded.source,
    updated_at = excluded.updated_at
WHERE CASE excluded.source WHEN 'config' THEN 1 WHEN 'bootstrap' THEN 1 WHEN 'whoami' THEN 2 WHEN 'admin' THEN 3 ELSE 0 END
   >= CASE source           WHEN 'config' THEN 1 WHEN 'bootstrap' THEN 1 WHEN 'whoami' THEN 2 WHEN 'admin' THEN 3 ELSE 0 END`

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("group_plugins: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, upsertSQL)
	if err != nil {
		return fmt.Errorf("group_plugins: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, pid := range pluginIDs {
		if _, err := stmt.ExecContext(ctx, groupID, pid, source, now, now); err != nil {
			return fmt.Errorf("group_plugins: upsert %s/%s: %w", groupID, pid, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("group_plugins: commit: %w", err)
	}
	return nil
}

// RevokePlugin removes a specific plugin from a group.
func (s *GroupPluginStore) RevokePlugin(ctx context.Context, groupID, pluginID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM group_plugins WHERE group_id = ? AND plugin_id = ?`, groupID, pluginID)
	if err != nil {
		return fmt.Errorf("group_plugins: revoke %s/%s: %w", groupID, pluginID, err)
	}
	return nil
}
