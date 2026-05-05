package channel

import (
	"testing"
)

func TestReady_NoChannels(t *testing.T) {
	reg := NewRegistry(nil)
	m := NewManager(reg, nil)

	// No channels expected → vacuously ready.
	if !m.Ready() {
		t.Error("expected Ready() = true when no channels expected")
	}
}

func TestReady_AllLoaded(t *testing.T) {
	reg := NewRegistry(nil)
	m := NewManager(reg, nil)

	// Simulate expectedCount set by LoadAll.
	m.mu.Lock()
	m.expectedCount = 2
	m.mu.Unlock()

	if m.Ready() {
		t.Error("expected Ready() = false when 0/2 loaded")
	}

	// Register mock channels directly into the registry.
	ch1 := newMockChannel("ch1")
	ch2 := newMockChannel("ch2")
	if err := reg.Register(ch1); err != nil {
		t.Fatal(err)
	}

	if m.Ready() {
		t.Error("expected Ready() = false when 1/2 loaded")
	}

	if err := reg.Register(ch2); err != nil {
		t.Fatal(err)
	}

	if !m.Ready() {
		t.Error("expected Ready() = true when 2/2 loaded")
	}
}
