package store

import (
	"context"
	"testing"
)

func TestGroupPluginStore_UpsertAndQuery(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	s := NewGroupPluginStore(db)
	ctx := context.Background()

	// Insert config-level entries.
	if err := s.UpsertGroupPlugins(ctx, "team-a", []string{"jira", "github"}, "config"); err != nil {
		t.Fatal(err)
	}
	plugins, err := s.PluginsForGroup(ctx, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 2 {
		t.Errorf("len = %d, want 2", len(plugins))
	}
}

func TestGroupPluginStore_PriorityNotDowngraded(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	s := NewGroupPluginStore(db)
	ctx := context.Background()

	// Insert with admin (high priority).
	if err := s.UpsertGroupPlugins(ctx, "g", []string{"jira"}, "admin"); err != nil {
		t.Fatal(err)
	}
	// Try to overwrite with whoami (lower priority) — should be a no-op.
	if err := s.UpsertGroupPlugins(ctx, "g", []string{"jira"}, "whoami"); err != nil {
		t.Fatal(err)
	}
	// Verify source is still "admin".
	var src string
	if err := db.db.QueryRow(`SELECT source FROM group_plugins WHERE group_id='g' AND plugin_id='jira'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "admin" {
		t.Errorf("source = %q, want admin (should not be downgraded)", src)
	}
}

func TestGroupPluginStore_Revoke(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	s := NewGroupPluginStore(db)
	ctx := context.Background()

	if err := s.UpsertGroupPlugins(ctx, "g", []string{"jira", "github"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokePlugin(ctx, "g", "jira"); err != nil {
		t.Fatal(err)
	}
	plugins, _ := s.PluginsForGroup(ctx, "g")
	if len(plugins) != 1 || plugins[0] != "github" {
		t.Errorf("after revoke: %v, want [github]", plugins)
	}
}

func TestGroupPluginStore_ConfigWinsOverBootstrap(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	s := NewGroupPluginStore(db)
	ctx := context.Background()

	// Bootstrap seeds first.
	if err := s.UpsertGroupPlugins(ctx, "g", []string{"jira"}, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	// Config should overwrite bootstrap.
	if err := s.UpsertGroupPlugins(ctx, "g", []string{"jira"}, "config"); err != nil {
		t.Fatal(err)
	}
	var src string
	if err := db.db.QueryRow(`SELECT source FROM group_plugins WHERE group_id='g' AND plugin_id='jira'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "config" {
		t.Errorf("source = %q, want config (config must win over bootstrap)", src)
	}

	// Now verify bootstrap cannot overwrite config.
	if err := s.UpsertGroupPlugins(ctx, "g", []string{"jira"}, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT source FROM group_plugins WHERE group_id='g' AND plugin_id='jira'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "config" {
		t.Errorf("source = %q, want config (bootstrap must not overwrite config)", src)
	}
}

func TestGroupPluginStore_WhoAmIUpgradesConfig(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	s := NewGroupPluginStore(db)
	ctx := context.Background()

	// Config-level first.
	if err := s.UpsertGroupPlugins(ctx, "g", []string{"jira"}, "config"); err != nil {
		t.Fatal(err)
	}
	// WhoAmI should upgrade.
	if err := s.UpsertGroupPlugins(ctx, "g", []string{"jira"}, "whoami"); err != nil {
		t.Fatal(err)
	}
	var src string
	if err := db.db.QueryRow(`SELECT source FROM group_plugins WHERE group_id='g' AND plugin_id='jira'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "whoami" {
		t.Errorf("source = %q, want whoami (should upgrade from config)", src)
	}
}
