package channel

import (
	"testing"
	"time"
)

func TestDeduplicator(t *testing.T) {
	d := NewDeduplicator(time.Minute)

	if d.IsDuplicate("a") {
		t.Error("first occurrence of 'a' should not be duplicate")
	}
	if !d.IsDuplicate("a") {
		t.Error("second occurrence of 'a' should be duplicate")
	}
	if d.IsDuplicate("b") {
		t.Error("first occurrence of 'b' should not be duplicate")
	}
}

func TestDeduplicatorEmpty(t *testing.T) {
	d := NewDeduplicator(time.Minute)
	if d.IsDuplicate("") {
		t.Error("empty string should never be duplicate")
	}
	if d.IsDuplicate("") {
		t.Error("empty string should never be duplicate even on second call")
	}
}

func TestDeduplicatorDefaultTTL(t *testing.T) {
	d := NewDeduplicator(0)
	if d.ttl != 10*time.Minute {
		t.Errorf("default TTL = %v, want 10m", d.ttl)
	}
}
