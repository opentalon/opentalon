package toolfqn

import "testing"

func TestJoin(t *testing.T) {
	if got := Join("depot", "list-items"); got != "depot__list-items" {
		t.Errorf("Join: got %q, want %q", got, "depot__list-items")
	}
	// An action that itself contains "__" (the MCP-bridged form) round-trips:
	// Join nests it and Split recovers it via the first-"__" boundary.
	fqn := Join("depot", "depot__create-item")
	if fqn != "depot__depot__create-item" {
		t.Fatalf("Join nested: got %q, want %q", fqn, "depot__depot__create-item")
	}
	plugin, action, err := Split(fqn)
	if err != nil || plugin != "depot" || action != "depot__create-item" {
		t.Errorf("round-trip Split(%q) = (%q, %q, %v); want (depot, depot__create-item, nil)", fqn, plugin, action, err)
	}
}

func TestSplit(t *testing.T) {
	tests := []struct {
		name           string
		in             string
		plugin, action string
		wantErr        bool
	}{
		// Canonical "__" form.
		{"canonical", "depot__list-items", "depot", "list-items", false},
		{"canonical leading-underscore plugin", "_meta__get_tool_details", "_meta", "get_tool_details", false},
		{"canonical action contains __", "depot__depot__create-item", "depot", "depot__create-item", false},
		{"canonical hyphenated parts", "weaviate__ask_knowledge", "weaviate", "ask_knowledge", false},

		// Legacy dot form, still tolerated on decode.
		{"legacy dot", "depot.list-items", "depot", "list-items", false},
		{"legacy dot, action contains __", "depot.depot__create-item", "depot", "depot__create-item", false},
		// LastIndex('.') contract: a multi-dot legacy name splits on the LAST dot.
		{"legacy multi-dot", "a.b.c", "a.b", "c", false},

		// Rejected: degenerate / malformed.
		{"empty", "", "", "", true},
		{"separator only", "__", "", "", true},
		{"dot only", ".", "", "", true},
		{"leading separator", "__action", "", "", true},
		{"trailing separator", "plugin__", "", "", true},
		{"leading dot", ".action", "", "", true},
		{"trailing dot", "plugin.", "", "", true},
		{"no separator", "plainname", "", "", true},
		{"natural language", "call the list-items tool", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin, action, err := Split(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Split(%q) = (%q, %q, nil); want error", tt.in, plugin, action)
				}
				return
			}
			if err != nil {
				t.Fatalf("Split(%q) unexpected error: %v", tt.in, err)
			}
			if plugin != tt.plugin || action != tt.action {
				t.Errorf("Split(%q) = (%q, %q); want (%q, %q)", tt.in, plugin, action, tt.plugin, tt.action)
			}
		})
	}
}
