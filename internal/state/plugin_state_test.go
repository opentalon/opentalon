package state

import (
	"sort"
	"testing"
)

func TestPluginStateSetGet(t *testing.T) {
	store := NewPluginStateStore("")
	_ = store.Set("gitlab", "token", "abc123")

	val, err := store.Get("gitlab", "token")
	if err != nil {
		t.Fatal(err)
	}
	if val != "abc123" {
		t.Errorf("value = %q, want abc123", val)
	}
}

func TestPluginStateGetNotFound(t *testing.T) {
	store := NewPluginStateStore("")

	_, err := store.Get("unknown", "key")
	if err == nil {
		t.Error("expected error for unknown plugin")
	}

	_ = store.Set("gitlab", "token", "val")
	_, err = store.Get("gitlab", "missing")
	if err == nil {
		t.Error("expected error for missing key")
	}
}

func TestPluginStateIsolation(t *testing.T) {
	store := NewPluginStateStore("")
	_ = store.Set("gitlab", "url", "https://gitlab.com")
	_ = store.Set("jira", "url", "https://jira.example.com")

	val, _ := store.Get("gitlab", "url")
	if val != "https://gitlab.com" {
		t.Errorf("gitlab url = %q", val)
	}

	val, _ = store.Get("jira", "url")
	if val != "https://jira.example.com" {
		t.Errorf("jira url = %q", val)
	}
}

func TestPluginStateDelete(t *testing.T) {
	store := NewPluginStateStore("")
	_ = store.Set("gitlab", "token", "val")

	if err := store.Delete("gitlab", "token"); err != nil {
		t.Fatal(err)
	}

	_, err := store.Get("gitlab", "token")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestPluginStateDeleteNotFound(t *testing.T) {
	store := NewPluginStateStore("")
	err := store.Delete("unknown", "key")
	if err == nil {
		t.Error("expected error for unknown plugin")
	}

	_ = store.Set("gitlab", "token", "val")
	err = store.Delete("gitlab", "missing")
	if err == nil {
		t.Error("expected error for missing key")
	}
}

func TestPluginStateDeleteCleansNamespace(t *testing.T) {
	store := NewPluginStateStore("")
	_ = store.Set("gitlab", "only_key", "val")
	_ = store.Delete("gitlab", "only_key")

	keys, _ := store.Keys("gitlab")
	if keys != nil {
		t.Errorf("expected nil keys after last key deleted, got %v", keys)
	}
}

func TestPluginStateKeys(t *testing.T) {
	store := NewPluginStateStore("")
	_ = store.Set("gitlab", "url", "a")
	_ = store.Set("gitlab", "token", "b")
	_ = store.Set("gitlab", "project", "c")

	keys, err := store.Keys("gitlab")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
	expected := []string{"project", "token", "url"}
	for i, k := range keys {
		if k != expected[i] {
			t.Errorf("keys[%d] = %q, want %q", i, k, expected[i])
		}
	}
}

func TestPluginStateKeysEmpty(t *testing.T) {
	store := NewPluginStateStore("")
	keys, err := store.Keys("unknown")
	if err != nil {
		t.Fatal(err)
	}
	if keys != nil {
		t.Errorf("expected nil, got %v", keys)
	}
}

func TestPluginStatePersistence(t *testing.T) {
	dir := t.TempDir()
	store := NewPluginStateStore(dir)
	_ = store.Set("gitlab", "url", "https://gitlab.com")
	_ = store.Set("gitlab", "token", "secret")

	if err := store.Save("gitlab"); err != nil {
		t.Fatal(err)
	}

	loaded := NewPluginStateStore(dir)
	if err := loaded.Load("gitlab"); err != nil {
		t.Fatal(err)
	}

	val, err := loaded.Get("gitlab", "url")
	if err != nil {
		t.Fatal(err)
	}
	if val != "https://gitlab.com" {
		t.Errorf("url = %q", val)
	}

	val, err = loaded.Get("gitlab", "token")
	if err != nil {
		t.Fatal(err)
	}
	if val != "secret" {
		t.Errorf("token = %q", val)
	}
}

func TestPluginStateSaveNoDir(t *testing.T) {
	store := NewPluginStateStore("")
	_ = store.Set("gitlab", "key", "val")
	err := store.Save("gitlab")
	if err != nil {
		t.Error("expected nil error for empty dir")
	}
}

func TestPluginStateLoadNoFile(t *testing.T) {
	dir := t.TempDir()
	store := NewPluginStateStore(dir)
	err := store.Load("nonexistent")
	if err != nil {
		t.Error("expected nil error for nonexistent file")
	}
}
