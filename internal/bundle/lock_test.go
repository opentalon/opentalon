package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPluginsLockMissing(t *testing.T) {
	dir := t.TempDir()
	lock, err := LoadPluginsLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if lock == nil || lock.Plugins == nil {
		t.Fatal("expected non-nil lock with empty Plugins map")
	}
	if len(lock.Plugins) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(lock.Plugins))
	}
}

func TestSaveAndLoadPluginsLock(t *testing.T) {
	dir := t.TempDir()
	lock := &PluginsLock{
		Plugins: map[string]LockEntry{
			"example-plugin": {
				GitHub:   "owner/example-repo",
				Ref:      "main",
				Resolved: "abc123def456789012345678901234567890abcd",
				Path:     "plugins/example-plugin/example-plugin",
			},
		},
	}
	if err := SavePluginsLock(dir, lock); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPluginsLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(loaded.Plugins))
	}
	e := loaded.Plugins["example-plugin"]
	if e.GitHub != "owner/example-repo" || e.Ref != "main" || e.Resolved != "abc123def456789012345678901234567890abcd" || e.Path != "plugins/example-plugin/example-plugin" {
		t.Errorf("got %+v", e)
	}
}

func TestChannelsLockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	lock := &ChannelsLock{
		Channels: map[string]LockEntry{
			"slack": {
				GitHub:   "opentalon/slack-channel",
				Ref:      "v0.1.0",
				Resolved: "deadbeef00000000000000000000000000000000",
				Path:     filepath.Join("channels", "slack", "slack-channel"),
			},
		},
	}
	if err := SaveChannelsLock(dir, lock); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadChannelsLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Channels) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(loaded.Channels))
	}
}

func TestPluginsLockPath(t *testing.T) {
	p := pluginsLockPath("/var/lib/opentalon")
	if p != "/var/lib/opentalon/plugins.lock" && p != filepath.Join("/var/lib/opentalon", "plugins.lock") {
		t.Errorf("unexpected path: %s", p)
	}
}

func TestLoadPluginsLockExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.lock")
	if err := os.WriteFile(path, []byte("plugins:\n  x:\n    github: a/b\n    ref: main\n    resolved: abc\n    path: p\n"), 0644); err != nil {
		t.Fatal(err)
	}
	lock, err := LoadPluginsLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(lock.Plugins))
	}
}
