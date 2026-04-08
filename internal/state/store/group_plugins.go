package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// sourcePriority maps source name to numeric priority. Higher = harder to overwrite.
var sourcePriority = map[string]int{
	"config": 1,
	"whoami": 2,
	"admin":  3,
}

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
// It will not downgrade an existing row to a lower-priority source.
func (s *GroupPluginStore) UpsertGroupPlugins(ctx context.Context, groupID string, pluginIDs []string, source string) error {
	newPri := sourcePriority[source]
	now := time.Now().UTC().Format(time.RFC3339)
	for _, pid := range pluginIDs {
		// Check existing priority.
		var existingSource string
		err := s.db.QueryRowContext(ctx,
			`SELECT source FROM group_plugins WHERE group_id = ? AND plugin_id = ?`, groupID, pid,
		).Scan(&existingSource)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("group_plugins: check existing: %w", err)
		}
		if err == nil && sourcePriority[existingSource] > newPri {
			// Existing row has higher priority — do not downgrade.
			continue
		}
		if err == sql.ErrNoRows {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO group_plugins (group_id, plugin_id, source, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
				groupID, pid, source, now, now)
		} else {
			_, err = s.db.ExecContext(ctx,
				`UPDATE group_plugins SET source = ?, updated_at = ? WHERE group_id = ? AND plugin_id = ?`,
				source, now, groupID, pid)
		}
		if err != nil {
			return fmt.Errorf("group_plugins: upsert %s/%s: %w", groupID, pid, err)
		}
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

// ListGroups returns all group IDs that have at least one plugin assigned.
func (s *GroupPluginStore) ListGroups(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT group_id FROM group_plugins ORDER BY group_id`)
	if err != nil {
		return nil, fmt.Errorf("group_plugins: list groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
