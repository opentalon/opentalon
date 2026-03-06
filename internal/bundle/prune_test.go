package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanAll(t *testing.T) {
	dir := t.TempDir()

	// Create cached dirs and lock files
	for _, sub := range []string{"plugins/foo", "channels/bar", "skills/baz", "lua_plugins/qux"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := SavePluginsLock(dir, &PluginsLock{Plugins: map[string]LockEntry{"foo": {GitHub: "a/b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveChannelsLock(dir, &ChannelsLock{Channels: map[string]LockEntry{"bar": {GitHub: "a/c"}}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSkillsLock(dir, &SkillsLock{Skills: map[string]LockEntry{"baz": {GitHub: "a/d"}}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveLuaPluginsLock(dir, &LuaPluginsLock{Plugins: map[string]LockEntry{"qux": {GitHub: "a/e"}}}); err != nil {
		t.Fatal(err)
	}

	if err := CleanAll(dir); err != nil {
		t.Fatal(err)
	}

	// Verify dirs are gone
	for _, sub := range []string{"plugins", "channels", "skills", "lua_plugins"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed", sub)
		}
	}
	// Verify lock files are gone
	for _, lock := range []string{"plugins.lock", "channels.lock", "skills.lock", "lua_plugins.lock"} {
		if _, err := os.Stat(filepath.Join(dir, lock)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed", lock)
		}
	}
}

func TestCleanPluginsOnly(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "plugins/foo"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "channels/bar"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := SavePluginsLock(dir, &PluginsLock{Plugins: map[string]LockEntry{"foo": {}}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveChannelsLock(dir, &ChannelsLock{Channels: map[string]LockEntry{"bar": {}}}); err != nil {
		t.Fatal(err)
	}

	if err := CleanPlugins(dir); err != nil {
		t.Fatal(err)
	}

	// plugins gone
	if _, err := os.Stat(filepath.Join(dir, "plugins")); !os.IsNotExist(err) {
		t.Error("expected plugins dir to be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins.lock")); !os.IsNotExist(err) {
		t.Error("expected plugins.lock to be removed")
	}

	// channels untouched
	if _, err := os.Stat(filepath.Join(dir, "channels")); os.IsNotExist(err) {
		t.Error("expected channels dir to still exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "channels.lock")); os.IsNotExist(err) {
		t.Error("expected channels.lock to still exist")
	}
}

func TestCleanNonexistent(t *testing.T) {
	dir := t.TempDir()
	// Should not error when there's nothing to clean
	if err := CleanAll(dir); err != nil {
		t.Fatal(err)
	}
}
