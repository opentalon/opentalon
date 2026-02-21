package router

import (
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

func testCatalog() []CatalogModel {
	return []CatalogModel{
		{Ref: "anthropic/claude-haiku-4", Alias: "haiku", Weight: 90},
		{Ref: "ovh/gpt-oss-120b", Alias: "ovh", Weight: 80},
		{Ref: "anthropic/claude-sonnet-4", Alias: "sonnet", Weight: 50},
		{Ref: "openai/gpt-5.2", Alias: "gpt52", Weight: 40},
		{Ref: "anthropic/claude-opus-4-6", Alias: "opus", Weight: 10},
	}
}

func TestRouteByWeight(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)

	ref, err := r.Route(TaskChat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "anthropic/claude-haiku-4" {
		t.Errorf("expected highest weight model, got %s", ref)
	}
}

func TestRouteSortedByWeight(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)

	models := r.Models()
	for i := 1; i < len(models); i++ {
		if models[i].Weight > models[i-1].Weight {
			t.Errorf("models not sorted by weight: %d > %d", models[i].Weight, models[i-1].Weight)
		}
	}
}

func TestRouteWithOverride(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)

	override := &Override{Model: "anthropic/claude-opus-4-6", Scope: "request"}
	ref, err := r.Route(TaskChat, override)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "anthropic/claude-opus-4-6" {
		t.Errorf("override should win, got %s", ref)
	}
}

func TestRouteWithTaskPin(t *testing.T) {
	pins := map[TaskType]provider.ModelRef{
		TaskCode: "anthropic/claude-sonnet-4",
	}
	r := NewWeightedRouter(testCatalog(), pins, nil)

	ref, err := r.Route(TaskCode, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "anthropic/claude-sonnet-4" {
		t.Errorf("pin should win for code tasks, got %s", ref)
	}

	ref, err = r.Route(TaskChat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "anthropic/claude-haiku-4" {
		t.Errorf("chat should fall through to weight, got %s", ref)
	}
}

func TestRouteWithAffinity(t *testing.T) {
	affinity := NewAffinityStore("", 30)

	for i := 0; i < 10; i++ {
		affinity.Record(TaskCode, "anthropic/claude-sonnet-4", SignalAccepted)
	}

	r := NewWeightedRouter(testCatalog(), nil, affinity)

	ref, err := r.Route(TaskCode, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "anthropic/claude-sonnet-4" {
		t.Errorf("should use learned model, got %s", ref)
	}
}

func TestRouteAffinityBelowThreshold(t *testing.T) {
	affinity := NewAffinityStore("", 30)

	affinity.Record(TaskCode, "anthropic/claude-sonnet-4", SignalAccepted)
	affinity.Record(TaskCode, "anthropic/claude-sonnet-4", SignalRejected)
	affinity.Record(TaskCode, "anthropic/claude-sonnet-4", SignalRejected)

	r := NewWeightedRouter(testCatalog(), nil, affinity)

	ref, err := r.Route(TaskCode, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "anthropic/claude-haiku-4" {
		t.Errorf("low affinity should fall through to weight, got %s", ref)
	}
}

func TestNextModel(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)

	next, err := r.NextModel("anthropic/claude-haiku-4")
	if err != nil {
		t.Fatal(err)
	}
	if next != "ovh/gpt-oss-120b" {
		t.Errorf("next after haiku should be ovh, got %s", next)
	}
}

func TestNextModelLast(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)

	_, err := r.NextModel("anthropic/claude-opus-4-6")
	if err == nil {
		t.Error("expected error for last model")
	}
}

func TestRouteEmptyCatalog(t *testing.T) {
	r := NewWeightedRouter(nil, nil, nil)
	_, err := r.Route(TaskChat, nil)
	if err == nil {
		t.Error("expected error for empty catalog")
	}
}

func TestRecordSignal(t *testing.T) {
	affinity := NewAffinityStore("", 30)
	r := NewWeightedRouter(testCatalog(), nil, affinity)

	r.RecordSignal(TaskCode, "anthropic/claude-sonnet-4", SignalAccepted)

	scores := affinity.Get(TaskCode)
	if len(scores) == 0 {
		t.Fatal("expected at least one score")
	}
	if scores[0].Model != "anthropic/claude-sonnet-4" {
		t.Errorf("expected sonnet in scores, got %s", scores[0].Model)
	}
}

func TestOverrideTakesPriorityOverPin(t *testing.T) {
	pins := map[TaskType]provider.ModelRef{
		TaskCode: "anthropic/claude-sonnet-4",
	}
	r := NewWeightedRouter(testCatalog(), pins, nil)

	override := &Override{Model: "anthropic/claude-opus-4-6", Scope: "request"}
	ref, err := r.Route(TaskCode, override)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "anthropic/claude-opus-4-6" {
		t.Errorf("override should beat pin, got %s", ref)
	}
}

func TestRecordSignalNilAffinity(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)
	r.RecordSignal(TaskCode, "anthropic/claude-sonnet-4", SignalAccepted)
}

func TestNextModelNotInCatalog(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)
	_, err := r.NextModel("unknown/model")
	if err == nil {
		t.Error("expected error for model not in catalog")
	}
}

func TestRouteMultiplePins(t *testing.T) {
	pins := map[TaskType]provider.ModelRef{
		TaskCode:     "anthropic/claude-sonnet-4",
		TaskAnalysis: "anthropic/claude-opus-4-6",
	}
	r := NewWeightedRouter(testCatalog(), pins, nil)

	ref, _ := r.Route(TaskCode, nil)
	if ref != "anthropic/claude-sonnet-4" {
		t.Errorf("code pin = %s, want sonnet", ref)
	}

	ref, _ = r.Route(TaskAnalysis, nil)
	if ref != "anthropic/claude-opus-4-6" {
		t.Errorf("analysis pin = %s, want opus", ref)
	}

	ref, _ = r.Route(TaskChat, nil)
	if ref != "anthropic/claude-haiku-4" {
		t.Errorf("unpinned chat = %s, want haiku (by weight)", ref)
	}
}

func TestNextModelChaining(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)

	first, _ := r.Route(TaskChat, nil)
	second, err := r.NextModel(first)
	if err != nil {
		t.Fatal(err)
	}
	third, err := r.NextModel(second)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second == third {
		t.Error("chained models should be different")
	}
}

func TestModelsCatalogLength(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)
	if len(r.Models()) != 5 {
		t.Errorf("expected 5 models, got %d", len(r.Models()))
	}
}

func TestRouteSessionOverrideScope(t *testing.T) {
	r := NewWeightedRouter(testCatalog(), nil, nil)
	override := &Override{Model: "openai/gpt-5.2", Scope: "session"}
	ref, err := r.Route(TaskCode, override)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "openai/gpt-5.2" {
		t.Errorf("session override = %s, want openai/gpt-5.2", ref)
	}
}
