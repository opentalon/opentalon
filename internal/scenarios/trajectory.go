package scenarios

import (
	"fmt"
	"strings"

	"github.com/opentalon/opentalon/pkg/toolfqn"
)

// Trajectory is an optional, AssetOpsBench-style expected tool-call plan for a
// scenario. Where ScenarioAssert.ToolCalled asserts a single call, a Trajectory
// asserts a whole multi-step plan: which tools run, with which args, and in what
// partial order. It is deterministic and structural — the LLM-judge leg (see
// Rubric) is scored separately in the eval package, not here.
//
// Design mirrors AssetOpsBench's ground-truth schema
// (docs/guideline/ground_truth_design_guideline.md): Steps ≈ execution_steps,
// Links ≈ execution_links, and per-arg "*" values ≈ their per-field
// `deterministic:false` flags (key must be produced, value not checked).
type Trajectory struct {
	// Steps are the expected tool calls, in declared (planning) order.
	Steps []TrajStep `yaml:"steps"`
	// Links are ordering edges "from must occur before to" (a DAG over step IDs).
	// Only consulted in Match == "dag".
	Links []TrajLink `yaml:"links"`
	// Match controls how the expected steps are matched against the actual,
	// ordered tool-call trace:
	//   "dag"     (default) — steps may appear in any order except where a Link
	//                         constrains them; enforces the execution_links DAG.
	//   "ordered"           — steps must appear as an in-order subsequence.
	//   "set"               — order is ignored entirely; presence only.
	Match string `yaml:"match"`
	// AllowExtra tolerates actual tool calls that bind to no expected step.
	// Defaults to true (nil == true) — real runs often make incidental calls.
	AllowExtra *bool `yaml:"allow_extra"`
}

// TrajStep is one expected tool call in a Trajectory.
type TrajStep struct {
	// ID uniquely names the step so Links can reference it. Required.
	ID string `yaml:"id"`
	// Tool is the expected call in canonical "plugin__action" form (the legacy
	// "plugin.action" form is also accepted, via toolfqn.Split).
	Tool string `yaml:"tool"`
	// Args are expected argument values. A value of "*" asserts only that the
	// key is present (any non-empty value) — the AssetOpsBench "non-deterministic
	// argument" case. Keys not listed here are never checked.
	Args map[string]string `yaml:"args"`
	// ArgsMatch is "subset" (default: listed keys must match, extras allowed) or
	// "exact" (the actual call must carry exactly the listed keys, no more).
	ArgsMatch string `yaml:"args_match"`
	// Optional steps that fail to match are skipped rather than failing the
	// scenario — for branches a correct run may legitimately not take.
	Optional bool `yaml:"optional"`
	// Outputs names the variables this step is expected to produce. Currently
	// documentation only (and a hook for future "{{step.output}}" arg refs); not
	// enforced.
	Outputs []string `yaml:"outputs"`
}

// TrajLink is a "from before to" ordering edge between two step IDs.
type TrajLink struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// Rubric is the optional LLM-judge leg for non-deterministic scenarios
// (AssetOpsBench's characteristic_form / acceptance-criteria case). The
// structural Trajectory checker ignores it; the eval package's Judge scores it.
// Kept here so a scenario file carries both the deterministic plan and the
// judge rubric in one place.
type Rubric struct {
	// CharacteristicForm describes what a valid response looks like in prose.
	CharacteristicForm string `yaml:"characteristic_form"`
	// Dimensions are the judge axes to score, e.g. "reasoning", "tool_selection",
	// "arg_accuracy", "data_handling", "completion". Empty == judge's default set.
	Dimensions []string `yaml:"dimensions"`
	// MinScore is the pass threshold on the judge's aggregate (0..1). Zero means
	// "record the score but never fail on it".
	MinScore float64 `yaml:"min_score"`
}

// JudgeResult is the eval package's judge output for a Rubric; defined here so
// the Judge seam and the rubric type live together.
type JudgeResult struct {
	Aggregate  float64            // 0..1
	Dimensions map[string]float64 // per-dimension score, keys == Rubric.Dimensions
	Rationale  string
}

// Judge scores a run against a Rubric. Implemented in the eval package (an LLM
// call); nil-able so the structural path never depends on a model client.
type Judge interface {
	Score(input string, r Rubric, result RunResult) (JudgeResult, error)
}

func (t *Trajectory) allowExtra() bool { return t.AllowExtra == nil || *t.AllowExtra }

func (t *Trajectory) matchMode() string {
	if t.Match == "" {
		return "dag"
	}
	return t.Match
}

// checkTrajectory returns "" if the actual tool-call trace satisfies the
// expected trajectory, or a human-readable failure reason (same convention as
// CheckAssertions). It is deterministic and makes no model calls.
func checkTrajectory(t *Trajectory, result RunResult) string {
	if reason := t.validate(); reason != "" {
		return reason
	}

	// pos[i] is the index in result.ToolCalls that step i bound to, or -1 if the
	// (optional) step went unmatched. bound[j] marks an actual call as consumed.
	pos := make([]int, len(t.Steps))
	bound := make([]bool, len(result.ToolCalls))
	ordered := t.matchMode() == "ordered"
	cursor := 0 // ordered mode: actual calls before cursor are already spoken for

	for i := range t.Steps {
		step := &t.Steps[i]
		wantPlugin, wantAction, _ := toolfqn.Split(step.Tool) // validated already
		idx := -1
		for j := range result.ToolCalls {
			if bound[j] {
				continue
			}
			if ordered && j < cursor {
				continue
			}
			c := &result.ToolCalls[j]
			if c.Plugin == wantPlugin && c.Action == wantAction && argsMatch(step, c) {
				idx = j
				break
			}
		}
		if idx == -1 {
			if step.Optional {
				pos[i] = -1
				continue
			}
			return fmt.Sprintf("trajectory: step %q (%s) not found in tool-call trace", step.ID, step.Tool)
		}
		bound[idx] = true
		pos[i] = idx
		if ordered {
			cursor = idx + 1
		}
	}

	if !t.allowExtra() {
		for j := range result.ToolCalls {
			if !bound[j] {
				c := &result.ToolCalls[j]
				return fmt.Sprintf("trajectory: unexpected tool call %s__%s (allow_extra: false)", c.Plugin, c.Action)
			}
		}
	}

	if t.matchMode() == "dag" {
		if reason := t.checkLinks(pos); reason != "" {
			return reason
		}
	}
	return ""
}

// checkLinks enforces every Link's "from before to" ordering on bound steps.
// A link touching an unmatched optional step is vacuously satisfied.
func (t *Trajectory) checkLinks(pos []int) string {
	idOf := make(map[string]int, len(t.Steps))
	for i := range t.Steps {
		idOf[t.Steps[i].ID] = i
	}
	for _, ln := range t.Links {
		fi, ti := idOf[ln.From], idOf[ln.To]
		if pos[fi] == -1 || pos[ti] == -1 {
			continue
		}
		if pos[fi] >= pos[ti] {
			return fmt.Sprintf("trajectory: link %s→%s violated (%s ran at #%d, not before %s at #%d)",
				ln.From, ln.To, ln.From, pos[fi], ln.To, pos[ti])
		}
	}
	return ""
}

// argsMatch reports whether an actual call's args satisfy a step's expected
// args, honoring "*" (present, any value) and ArgsMatch subset/exact.
func argsMatch(step *TrajStep, c *ToolCallResult) bool {
	for k, want := range step.Args {
		got, ok := c.Args[k]
		if !ok {
			return false
		}
		if want == "*" {
			if got == "" {
				return false
			}
			continue
		}
		if got != want {
			return false
		}
	}
	if strings.EqualFold(step.ArgsMatch, "exact") && len(c.Args) != len(step.Args) {
		return false
	}
	return true
}

// validate checks the trajectory is internally well-formed (unique/non-empty
// step IDs, parseable tools, links referencing real steps). Returns "" if OK.
func (t *Trajectory) validate() string {
	if len(t.Steps) == 0 {
		return "trajectory: no steps defined"
	}
	ids := make(map[string]bool, len(t.Steps))
	for i := range t.Steps {
		s := &t.Steps[i]
		if s.ID == "" {
			return fmt.Sprintf("trajectory: step %d has empty id", i)
		}
		if ids[s.ID] {
			return fmt.Sprintf("trajectory: duplicate step id %q", s.ID)
		}
		ids[s.ID] = true
		if _, _, err := toolfqn.Split(s.Tool); err != nil {
			return fmt.Sprintf("trajectory: step %q has invalid tool %q", s.ID, s.Tool)
		}
	}
	for _, ln := range t.Links {
		if !ids[ln.From] {
			return fmt.Sprintf("trajectory: link references unknown step %q", ln.From)
		}
		if !ids[ln.To] {
			return fmt.Sprintf("trajectory: link references unknown step %q", ln.To)
		}
	}
	switch t.matchMode() {
	case "dag", "ordered", "set":
	default:
		return fmt.Sprintf("trajectory: unknown match mode %q", t.Match)
	}
	return ""
}
