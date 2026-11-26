package orchestrator

import (
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

// --- what the estimate counts -------------------------------------------
//
// One test per blind spot the previous estimate had. Written as "must not be
// zero / must not be equal" rather than against exact counts: the ratios are
// measurements and will be re-measured, but a tool definition costing nothing
// is a bug under any calibration.

func TestEstimateMessageTokens_CountsToolCallArguments(t *testing.T) {
	// The message that broke the accounting: an assistant tool call is stored
	// with its arguments in ToolCalls and Content left empty, so an estimate
	// that reads Content alone priced it at zero. One production session
	// carried 52 of these.
	call := provider.Message{
		Role: provider.RoleAssistant,
		ToolCalls: []provider.ToolCall{{
			ID:        "call_1",
			Name:      "timly__list-items",
			Arguments: map[string]string{"query": strings.Repeat("org_unit_id:961 AND ", 40)},
		}},
	}
	got := estimateMessageTokens(call)
	if got <= perMessageOverhead {
		t.Fatalf("tool-call arguments not counted: got %d, no more than the %d framing", got, perMessageOverhead)
	}
	if got < 100 {
		t.Errorf("tool-call arguments under-counted: got %d tokens for ~800 characters", got)
	}
}

func TestEstimateMessageTokens_PricesToolResultsAsJSON(t *testing.T) {
	// A tool result is serialised JSON and tokenises far denser than prose
	// (measured 2.78 characters per token against 4.77 for German). Pricing it
	// as prose is what let long tool sessions drift past the window.
	body := strings.Repeat("x", 4000)
	asProse := estimateMessageTokens(provider.Message{Role: provider.RoleAssistant, Content: body})
	asJSON := estimateMessageTokens(provider.Message{Role: provider.RoleTool, Content: body})
	if asJSON <= asProse {
		t.Errorf("tool result priced no higher than prose: json=%d prose=%d", asJSON, asProse)
	}
}

func TestEstimateMessageTokens_ChargesPerMessageFraming(t *testing.T) {
	// The chat template costs ~6 tokens per message regardless of content;
	// across a hundred-message session that is real.
	if got := estimateMessageTokens(provider.Message{Role: provider.RoleUser}); got != perMessageOverhead {
		t.Errorf("empty message = %d tokens, want the %d framing", got, perMessageOverhead)
	}
}

func TestEstimateToolTokens(t *testing.T) {
	// Tool definitions ride on every request but are not messages. Measured at
	// 552 tokens each for the real Timly catalogue — 22 081 for the 40-tool cap.
	if got := estimateToolTokens(nil); got != 0 {
		t.Errorf("no tools should cost nothing, got %d", got)
	}
	tools := []provider.ToolDefinition{{
		Name:        "timly__list-persons",
		Description: strings.Repeat("List persons via a Lucene query. ", 60),
		Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"query": map[string]interface{}{"type": "string"},
		}},
	}}
	one := estimateToolTokens(tools)
	if one < 300 {
		t.Errorf("a ~2000-character tool definition estimated at %d tokens, expected several hundred", one)
	}
	if got := estimateToolTokens(append(tools, tools[0])); got != one*2 {
		t.Errorf("two identical tools = %d, want %d", got, one*2)
	}
}

// --- what the fit does ----------------------------------------------------

func TestInputTokenBudget(t *testing.T) {
	// Unknown output budget (0) → historical flat 10% reserve.
	if got := inputTokenBudget(100000, 0); got != 90000 {
		t.Errorf("fallback budget = %d, want 90000", got)
	}
	// Known output budget → window - max_tokens - 5% margin.
	// 131072 - 32768 - 6553 = 91751.
	if got := inputTokenBudget(131072, 32768); got != 91751 {
		t.Errorf("budget = %d, want 91751", got)
	}
	// The core invariant: reserving the output budget must leave room for it —
	// trimmed input + full output can never exceed the window. The old flat-10%
	// reserve (117964) violated this for a 32768 output budget.
	if got := inputTokenBudget(131072, 32768); got+32768 > 131072 {
		t.Errorf("budget %d + output 32768 exceeds window 131072", got)
	}
	// Zero / negative window → no budget.
	if got := inputTokenBudget(0, 32768); got != 0 {
		t.Errorf("zero window budget = %d, want 0", got)
	}
	// A large-but-valid output budget (>70% of window) must still satisfy the
	// invariant: budget + output <= window.
	if got := inputTokenBudget(1000, 800); got != 150 {
		t.Errorf("budget(1000,800) = %d, want 150", got)
	}
	if got := inputTokenBudget(1000, 800); got+800 > 1000 {
		t.Errorf("budget %d + output 800 exceeds window 1000", got)
	}
	// Misconfig (max_tokens >= window): budget clamps to 0, never negative.
	if got := inputTokenBudget(1000, 2000); got != 0 {
		t.Errorf("misconfig budget = %d, want 0", got)
	}
}

// --- the correction -------------------------------------------------------

func TestCalibration_OnlyEverTightens(t *testing.T) {
	c := &calibration{factor: 1.0}
	// An estimate that came out too high wastes window; an estimate that came
	// out too low kills the session. Only the second is worth reacting to.
	c.observe(1000, 500)
	if c.factor != 1.0 {
		t.Errorf("an over-estimate must not loosen the budget: factor = %v", c.factor)
	}
	c.observe(1000, 1500)
	if c.factor != 1.5 {
		t.Errorf("under-estimate factor = %v, want 1.5", c.factor)
	}
	// A later, milder observation must not undo a correction already learned.
	c.observe(1000, 1100)
	if c.factor != 1.5 {
		t.Errorf("factor slipped back to %v", c.factor)
	}
	// One anomalous response cannot squeeze the conversation to nothing.
	c.observe(1000, 99999)
	if c.factor != maxCalibrationFactor {
		t.Errorf("factor = %v, want the %v cap", c.factor, maxCalibrationFactor)
	}
}

func TestCalibration_IgnoresUnusableObservations(t *testing.T) {
	// Providers that report no usage must leave the correction alone rather
	// than driving it to zero and stranding the budget.
	c := &calibration{factor: 1.0}
	c.observe(0, 5000)
	c.observe(5000, 0)
	if c.factor != 1.0 {
		t.Errorf("factor = %v after unusable observations, want 1.0", c.factor)
	}
}

func TestCalibration_ObserveRejection(t *testing.T) {
	// When the provider names the size it measured, that figure is better than
	// anything a successful response gives us — use it directly.
	measured := &calibration{factor: 1.0}
	measured.observeRejection(90588, 132235)
	if measured.factor < 1.4 {
		t.Errorf("factor = %v, expected the provider's own 132235/90588 ≈ 1.46", measured.factor)
	}

	// When it names nothing, step up so the compaction the refusal triggers
	// aims meaningfully lower, never past the cap.
	blind := &calibration{factor: 1.0}
	blind.observeRejection(90588, 0)
	if blind.factor != rejectionStep {
		t.Errorf("factor = %v after one blind rejection, want %v", blind.factor, rejectionStep)
	}
	blind.observeRejection(90588, 0)
	if blind.factor != maxCalibrationFactor {
		t.Errorf("factor = %v after two blind rejections, want the %v cap", blind.factor, maxCalibrationFactor)
	}
}

func TestCalibrators_PerModelAndSurvivesTheTurn(t *testing.T) {
	// The correction is a property of a tokenizer, not of a conversation. A
	// turn-scoped factor made every turn of a long session re-learn the same
	// number, and the only way to learn it is to overflow once.
	c := newCalibrators()
	if got := c.factor("gpt-oss-120b"); got != 1.0 {
		t.Errorf("an unseen model starts at %v, want 1.0", got)
	}
	c.observe("gpt-oss-120b", 1000, 1600)
	if got := c.factor("gpt-oss-120b"); got != 1.6 {
		t.Errorf("factor = %v, want 1.6", got)
	}
	if got := c.factor("claude-opus-4-8"); got != 1.0 {
		t.Errorf("a different model must not inherit it: %v", got)
	}
	// The empty model id is a real key — it means "the provider's default".
	c.observeRejection("", 1000, 1800)
	if got := c.factor(""); got != 1.8 {
		t.Errorf("default-model factor = %v, want 1.8", got)
	}
}

func TestCalibrators_ConcurrentUseIsSafe(t *testing.T) {
	// executeParallel fans one turn's context out to several goroutines, so
	// anything reachable from a turn is reachable concurrently. Run under -race.
	c := newCalibrators()
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				c.observe("gpt-oss-120b", 1000, 1200)
				_ = c.factor("gpt-oss-120b")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
