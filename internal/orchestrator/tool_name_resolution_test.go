package orchestrator

import "testing"

// A bridged MCP server registers its tools under the "<server>__<tool>" action
// name (e.g. "timly__list-persons"), and the orchestrator prefixes the plugin,
// so the canonical FQN offered to the model is "timly__timly__list-persons".
// gpt-oss-class models routinely mangle that name two ways at once — dropping a
// prefix and/or typing "_" where the canonical carries "-":
//
//	timly__timly__list-persons  (canonical)
//	timly__timly__list_persons  (underscore — the variant proven to trigger the bug)
//	timly__list-persons         (one prefix dropped)
//	timly__list_persons
//	list-persons                (both prefixes dropped)
//	list_persons
//
// toolfqn.Split turns each into (plugin="timly", action=<remainder>). Every one
// must resolve back to the registered "timly__list-persons". The regression:
// the pre-fix candidate generator ran ReplaceAll(action, "_", "-") across the
// whole string, corrupting the "__" separator ("timly__list_persons" ->
// "timly--list-persons") so the read-only flag was missed.

const (
	readServer  = "timly"
	readAction  = "timly__list-persons" // registered read-only action
	writeAction = "timly__assign-item"  // registered mutating action
)

func toolNameRegistry(t *testing.T) *ToolRegistry {
	t.Helper()
	reg := NewToolRegistry()
	cap := PluginCapability{
		Name: readServer,
		Actions: []Action{
			{Name: readAction, ReadOnly: true},
			{Name: writeAction, ReadOnly: false},
		},
	}
	if err := reg.Register(cap, &mockExecutor{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// contains reports whether want is one of the generated candidates.
func contains(candidates []string, want string) bool {
	for _, c := range candidates {
		if c == want {
			return true
		}
	}
	return false
}

// TestActionNameCandidates_SeparatorSafe pins the pure candidate generator:
// every mangled spelling must yield the registered "timly__list-persons", and
// the "__" separator must never be corrupted into "--".
func TestActionNameCandidates_SeparatorSafe(t *testing.T) {
	// (plugin, action) as toolfqn.Split would hand them over.
	mangled := []string{
		"timly__list-persons",
		"timly__list_persons", // the proven bug trigger
		"list-persons",
		"list_persons",
	}
	for _, action := range mangled {
		got := actionNameCandidates(readServer, action)
		if !contains(got, readAction) {
			t.Errorf("actionNameCandidates(%q, %q) = %v; missing canonical %q",
				readServer, action, got, readAction)
		}
		if contains(got, "timly--list-persons") {
			t.Errorf("actionNameCandidates(%q, %q) = %v; leaked separator-corrupted form",
				readServer, action, got)
		}
		for _, c := range got {
			if c == "" {
				t.Errorf("actionNameCandidates(%q, %q) produced an empty candidate", readServer, action)
			}
		}
	}
}

// TestActionNameCandidates_NoDuplicates keeps the candidate set tidy (the fix
// dedups; a caller iterating them should not re-check the same name).
func TestActionNameCandidates_NoDuplicates(t *testing.T) {
	got := actionNameCandidates(readServer, "timly__list_persons")
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c] {
			t.Errorf("duplicate candidate %q in %v", c, got)
		}
		seen[c] = true
	}
}

// TestIsActionReadOnly_ResolvesMangledReadTool is the end-to-end regression for
// the confirmation defect: a pure read must be recognised as read-only under
// EVERY mangled spelling, so the confirmation gate short-circuits and never
// raises a spurious Approve/Reject prompt.
func TestIsActionReadOnly_ResolvesMangledReadTool(t *testing.T) {
	reg := toolNameRegistry(t)
	for _, action := range []string{
		"timly__list-persons",
		"timly__list_persons", // proven bug trigger — pre-fix returned false here
		"list-persons",
		"list_persons",
	} {
		if !reg.IsActionReadOnly(readServer, action) {
			t.Errorf("IsActionReadOnly(%q, %q) = false; want true (read tool mis-flagged as write)",
				readServer, action)
		}
	}
}

// TestIsActionReadOnly_WriteToolNeverReadOnly is the safety property: no
// mangling of a MUTATING tool may ever resolve to read-only, which would let a
// write skip the confirmation gate. Toggling "_"<->"-" can only rewrite a name
// into the SAME tool's other spelling, never into a different tool.
func TestIsActionReadOnly_WriteToolNeverReadOnly(t *testing.T) {
	reg := toolNameRegistry(t)
	for _, action := range []string{
		"timly__assign-item",
		"timly__assign_item",
		"assign-item",
		"assign_item",
	} {
		if reg.IsActionReadOnly(readServer, action) {
			t.Errorf("IsActionReadOnly(%q, %q) = true; want false (write tool must not be read-only)",
				readServer, action)
		}
	}
}

// TestIsActionReadOnly_UnknownFailsClosed keeps the fail-safe: an unknown action
// or plugin is treated as a potential write (false), never optimistically
// read-only.
func TestIsActionReadOnly_UnknownFailsClosed(t *testing.T) {
	reg := toolNameRegistry(t)
	cases := []struct{ plugin, action string }{
		{readServer, "totally-unknown"},
		{readServer, "timly__does-not-exist"},
		{"no-such-plugin", "list-persons"},
	}
	for _, tc := range cases {
		if reg.IsActionReadOnly(tc.plugin, tc.action) {
			t.Errorf("IsActionReadOnly(%q, %q) = true; want false (unknown must fail closed)",
				tc.plugin, tc.action)
		}
	}
}
