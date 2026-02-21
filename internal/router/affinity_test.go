package router

import (
	"testing"
)

func TestAffinityRecordAndGet(t *testing.T) {
	store := NewAffinityStore("", 30)

	store.Record(TaskCode, "anthropic/claude-sonnet-4", SignalAccepted)
	store.Record(TaskCode, "anthropic/claude-sonnet-4", SignalAccepted)
	store.Record(TaskCode, "anthropic/claude-haiku-4", SignalRejected)

	scores := store.Get(TaskCode)
	if len(scores) < 2 {
		t.Fatalf("expected at least 2 scores, got %d", len(scores))
	}

	if scores[0].Model != "anthropic/claude-sonnet-4" {
		t.Errorf("top model should be sonnet, got %s", scores[0].Model)
	}
	if scores[0].Score <= 0 {
		t.Errorf("sonnet score should be positive, got %f", scores[0].Score)
	}
}

func TestAffinityEmptyTaskType(t *testing.T) {
	store := NewAffinityStore("", 30)
	scores := store.Get(TaskCode)
	if scores != nil {
		t.Errorf("expected nil for empty task type, got %v", scores)
	}
}

func TestAffinityReset(t *testing.T) {
	store := NewAffinityStore("", 30)
	store.Record(TaskCode, "anthropic/claude-sonnet-4", SignalAccepted)
	store.Reset()

	scores := store.Get(TaskCode)
	if scores != nil {
		t.Error("expected nil after reset")
	}
}

func TestAffinityMultipleTaskTypes(t *testing.T) {
	store := NewAffinityStore("", 30)
	store.Record(TaskCode, "anthropic/claude-sonnet-4", SignalAccepted)
	store.Record(TaskChat, "anthropic/claude-haiku-4", SignalAccepted)

	codeScores := store.Get(TaskCode)
	chatScores := store.Get(TaskChat)

	if len(codeScores) == 0 || codeScores[0].Model != "anthropic/claude-sonnet-4" {
		t.Error("code should have sonnet")
	}
	if len(chatScores) == 0 || chatScores[0].Model != "anthropic/claude-haiku-4" {
		t.Error("chat should have haiku")
	}
}

func TestAffinityNegativeScoresRankedLower(t *testing.T) {
	store := NewAffinityStore("", 30)

	for i := 0; i < 5; i++ {
		store.Record(TaskCode, "model-a", SignalAccepted)
	}
	for i := 0; i < 5; i++ {
		store.Record(TaskCode, "model-b", SignalRejected)
	}

	scores := store.Get(TaskCode)
	if len(scores) < 2 {
		t.Fatalf("expected 2 scores, got %d", len(scores))
	}
	if scores[0].Model != "model-a" {
		t.Errorf("accepted model should rank first, got %s", scores[0].Model)
	}
	if scores[1].Score >= 0 {
		t.Errorf("rejected model should have negative score, got %f", scores[1].Score)
	}
}

func TestAffinityDefaultDecayDays(t *testing.T) {
	store := NewAffinityStore("", 0)
	if store.decayDays != 30 {
		t.Errorf("decayDays = %d, want 30 (default)", store.decayDays)
	}

	store2 := NewAffinityStore("", -5)
	if store2.decayDays != 30 {
		t.Errorf("decayDays = %d, want 30 (default for negative)", store2.decayDays)
	}
}

func TestAffinitySaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/affinity.json"

	store := NewAffinityStore(path, 30)
	store.Record(TaskCode, "anthropic/claude-sonnet-4", SignalAccepted)
	store.Record(TaskChat, "anthropic/claude-haiku-4", SignalAccepted)

	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	loaded := NewAffinityStore(path, 30)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}

	scores := loaded.Get(TaskCode)
	if len(scores) == 0 {
		t.Fatal("expected code scores after load")
	}
	if scores[0].Model != "anthropic/claude-sonnet-4" {
		t.Errorf("loaded model = %s, want sonnet", scores[0].Model)
	}

	chatScores := loaded.Get(TaskChat)
	if len(chatScores) == 0 {
		t.Fatal("expected chat scores after load")
	}
}

func TestAffinitySaveNoPath(t *testing.T) {
	store := NewAffinityStore("", 30)
	store.Record(TaskCode, "m", SignalAccepted)
	if err := store.Save(); err != nil {
		t.Errorf("Save with no path should not error, got %v", err)
	}
}

func TestAffinityLoadNoPath(t *testing.T) {
	store := NewAffinityStore("", 30)
	if err := store.Load(); err != nil {
		t.Errorf("Load with no path should not error, got %v", err)
	}
}

func TestAffinityLoadNonexistentFile(t *testing.T) {
	store := NewAffinityStore("/nonexistent/affinity.json", 30)
	if err := store.Load(); err != nil {
		t.Errorf("Load of nonexistent file should not error, got %v", err)
	}
}

func TestAffinityRegeneratedSignal(t *testing.T) {
	store := NewAffinityStore("", 30)
	store.Record(TaskCode, "model-a", SignalRegenerated)

	scores := store.Get(TaskCode)
	if len(scores) == 0 {
		t.Fatal("expected score")
	}
	if scores[0].Score >= 0 {
		t.Errorf("regenerated signal should produce negative score, got %f", scores[0].Score)
	}
}
