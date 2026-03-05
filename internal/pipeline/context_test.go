package pipeline

import "testing"

func TestContextSetGet(t *testing.T) {
	ctx := NewContext()
	ctx.Set("step1", "output", "hello")

	val, ok := ctx.Get("step1", "output")
	if !ok {
		t.Fatal("expected value to exist")
	}
	if val != "hello" {
		t.Errorf("Get = %v, want hello", val)
	}
}

func TestContextGetMissing(t *testing.T) {
	ctx := NewContext()
	_, ok := ctx.Get("nonexistent", "key")
	if ok {
		t.Error("expected ok=false for missing key")
	}
}

func TestContextMerge(t *testing.T) {
	ctx := NewContext()
	ctx.Merge("step1", map[string]any{
		"output": "data1",
		"status": "ok",
	})

	val, ok := ctx.Get("step1", "output")
	if !ok || val != "data1" {
		t.Errorf("Get output = %v, %v", val, ok)
	}
	val, ok = ctx.Get("step1", "status")
	if !ok || val != "ok" {
		t.Errorf("Get status = %v, %v", val, ok)
	}
}

func TestContextOverwrite(t *testing.T) {
	ctx := NewContext()
	ctx.Set("s1", "k", "v1")
	ctx.Set("s1", "k", "v2")

	val, _ := ctx.Get("s1", "k")
	if val != "v2" {
		t.Errorf("expected overwritten value v2, got %v", val)
	}
}
