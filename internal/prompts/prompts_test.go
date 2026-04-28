package prompts

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestHashFormat(t *testing.T) {
	h := Hash()
	if len(h) != 64 {
		t.Fatalf("expected 64-char hex digest, got %d chars: %q", len(h), h)
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Fatalf("Hash() not valid hex: %v", err)
	}
}

func TestHashDeterministic(t *testing.T) {
	a, b := Hash(), Hash()
	if a != b {
		t.Fatalf("Hash() not deterministic: %q != %q", a, b)
	}
}

func TestHashCoversAllFiles(t *testing.T) {
	entries, err := promptFS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var txtCount int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			txtCount++
		}
	}
	if txtCount == 0 {
		t.Fatal("no .txt files found in embedded FS")
	}
}
