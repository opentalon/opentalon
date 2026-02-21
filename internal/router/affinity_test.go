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
