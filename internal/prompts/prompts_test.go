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

func TestApplyOverrides_AppliesWithTransforms(t *testing.T) {
	origNative, origSched, origSuffix, origCatalog := OrchestratorPreambleNative, SchedulingRules, PlannerSuffix, OrchestratorToolCatalogInline
	origSubagent := ConfirmationSubagentTask
	t.Cleanup(func() {
		OrchestratorPreambleNative, SchedulingRules, PlannerSuffix, OrchestratorToolCatalogInline = origNative, origSched, origSuffix, origCatalog
		ConfirmationSubagentTask = origSubagent
	})

	applied, unknown := ApplyOverrides(map[string]string{
		"orchestrator_preamble_native":     "RAW PREAMBLE",         // raw, no transform
		"rules_scheduling":                 "rule one\nrule two\n", // splitLines
		"planner_suffix":                   "suffix text\n",        // trailing newline trimmed
		"orchestrator_tool_catalog_inline": "catalog header\n",
		"confirmation_subagent_task":       "task {{task}}\n",
	})

	if len(unknown) != 0 {
		t.Fatalf("unexpected unknown keys: %v", unknown)
	}
	if got, want := strings.Join(applied, ","), "confirmation_subagent_task,orchestrator_preamble_native,orchestrator_tool_catalog_inline,planner_suffix,rules_scheduling"; got != want {
		t.Errorf("applied = %q, want sorted %q", got, want)
	}
	if OrchestratorPreambleNative != "RAW PREAMBLE" {
		t.Errorf("raw preamble not applied: %q", OrchestratorPreambleNative)
	}
	if len(SchedulingRules) != 2 || SchedulingRules[0] != "rule one" || SchedulingRules[1] != "rule two" {
		t.Errorf("scheduling rules not line-split: %v", SchedulingRules)
	}
	if PlannerSuffix != "suffix text" {
		t.Errorf("planner suffix not trimmed: %q", PlannerSuffix)
	}
	if OrchestratorToolCatalogInline != "catalog header" {
		t.Errorf("inline tool catalog header not trimmed: %q", OrchestratorToolCatalogInline)
	}
	if ConfirmationSubagentTask != "task {{task}}" {
		t.Errorf("sub-agent confirmation text not trimmed: %q", ConfirmationSubagentTask)
	}
}

func TestApplyOverrides_EmptyValueBlanks(t *testing.T) {
	orig := SchedulingRules
	t.Cleanup(func() { SchedulingRules = orig })

	ApplyOverrides(map[string]string{"rules_scheduling": ""})
	if len(SchedulingRules) != 0 {
		t.Errorf("empty value should blank the prompt, got %v", SchedulingRules)
	}
}

func TestApplyOverrides_UnknownKeysReportedSorted(t *testing.T) {
	applied, unknown := ApplyOverrides(map[string]string{"zzz_typo": "x", "aaa_typo": "y"})
	if len(applied) != 0 {
		t.Errorf("no known keys; applied should be empty, got %v", applied)
	}
	if got, want := strings.Join(unknown, ","), "aaa_typo,zzz_typo"; got != want {
		t.Errorf("unknown = %q, want sorted %q", got, want)
	}
}

// TestPromptSettersCoverAllEmbedded is the guardrail: every embedded *.txt must
// be overridable, and every setter must have a backing file. A new prompt added
// without a setter (or a renamed file) fails here.
func TestPromptSettersCoverAllEmbedded(t *testing.T) {
	entries, err := promptFS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".txt")
		if _, ok := promptSetters[name]; !ok {
			t.Errorf("embedded prompt %q has no override setter — add it to promptSetters", e.Name())
		}
	}
	for name := range promptSetters {
		if _, err := promptFS.ReadFile(name + ".txt"); err != nil {
			t.Errorf("promptSetters has %q but no %s.txt is embedded", name, name)
		}
	}
}
