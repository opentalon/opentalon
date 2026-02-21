package state

import "testing"

func TestMemoryAdd(t *testing.T) {
	store := NewMemoryStore("")
	m := store.Add("user prefers dark mode", "preference")

	if m.ID != "mem_1" {
		t.Errorf("ID = %q, want mem_1", m.ID)
	}
	if m.Content != "user prefers dark mode" {
		t.Errorf("Content = %q", m.Content)
	}
	if !m.HasTag("preference") {
		t.Error("expected tag 'preference'")
	}
}

func TestMemoryAutoIncrementID(t *testing.T) {
	store := NewMemoryStore("")
	m1 := store.Add("first")
	m2 := store.Add("second")

	if m1.ID == m2.ID {
		t.Error("IDs should be unique")
	}
}

func TestMemoryGet(t *testing.T) {
	store := NewMemoryStore("")
	added := store.Add("test content")

	got, err := store.Get(added.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "test content" {
		t.Errorf("Content = %q, want test content", got.Content)
	}
}

func TestMemoryGetNotFound(t *testing.T) {
	store := NewMemoryStore("")
	_, err := store.Get("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestMemorySearch(t *testing.T) {
	store := NewMemoryStore("")
	store.Add("user likes golang")
	store.Add("user prefers dark mode")
	store.Add("golang is fast")

	results := store.Search("golang")
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
}

func TestMemorySearchCaseInsensitive(t *testing.T) {
	store := NewMemoryStore("")
	store.Add("User likes GoLang")

	results := store.Search("golang")
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}
}

func TestMemorySearchByTag(t *testing.T) {
	store := NewMemoryStore("")
	store.Add("workflow: gitlab -> jira -> pr", "workflow")
	store.Add("user prefers haiku", "preference")
	store.Add("workflow: analyze -> report", "workflow")

	results := store.SearchByTag("workflow")
	if len(results) != 2 {
		t.Errorf("expected 2 workflows, got %d", len(results))
	}
}

func TestMemoryDelete(t *testing.T) {
	store := NewMemoryStore("")
	m := store.Add("to delete")

	if err := store.Delete(m.ID); err != nil {
		t.Fatal(err)
	}

	_, err := store.Get(m.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestMemoryDeleteNotFound(t *testing.T) {
	store := NewMemoryStore("")
	err := store.Delete("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestMemoryList(t *testing.T) {
	store := NewMemoryStore("")
	store.Add("one")
	store.Add("two")

	list := store.List()
	if len(list) != 2 {
		t.Errorf("expected 2, got %d", len(list))
	}
}

func TestMemoryPersistence(t *testing.T) {
	dir := t.TempDir()
	store := NewMemoryStore(dir)
	store.Add("fact one", "fact")
	store.Add("workflow: a -> b", "workflow")

	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	loaded := NewMemoryStore(dir)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}

	list := loaded.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(list))
	}
	if list[0].Content != "fact one" {
		t.Errorf("first content = %q", list[0].Content)
	}
}

func TestMemoryIDContinuesAfterLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewMemoryStore(dir)
	store.Add("one")
	store.Add("two")
	_ = store.Save()

	loaded := NewMemoryStore(dir)
	_ = loaded.Load()
	m := loaded.Add("three")

	if m.ID != "mem_3" {
		t.Errorf("ID after reload = %q, want mem_3", m.ID)
	}
}

func TestMemoryHasTag(t *testing.T) {
	m := &Memory{Tags: []string{"workflow", "important"}}
	if !m.HasTag("workflow") {
		t.Error("expected HasTag(workflow) = true")
	}
	if m.HasTag("missing") {
		t.Error("expected HasTag(missing) = false")
	}
}
