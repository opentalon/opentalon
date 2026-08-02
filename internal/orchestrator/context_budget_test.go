package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

// Each test below pins one of the four things the previous estimate could not
// see. They are written as "this must not be zero / must not be equal" rather
// than against exact token counts: the ratios are measurements and will be
// re-measured, but a tool definition costing nothing is a bug in any calibration.

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
		t.Fatalf("tool-call arguments not counted: got %d, which is no more than the %d framing", got, perMessageOverhead)
	}
	// Sanity: ~800 characters of arguments has to land in the hundreds, not single digits.
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
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}},
	}}
	one := estimateToolTokens(tools)
	if one < 300 {
		t.Errorf("a ~2000-character tool definition estimated at %d tokens, expected several hundred", one)
	}
	if got := estimateToolTokens(append(tools, tools[0])); got != one*2 {
		t.Errorf("two identical tools = %d, want %d", got, one*2)
	}
}

func TestTrimToContextWindow_ChargesForToolDefinitions(t *testing.T) {
	// The whole point: the same conversation must trim harder once the tools
	// riding along with it are accounted for. Previously they were free, and a
	// request the trimmer believed fit at 90 631 tokens arrived at 132 235.
	mk := func(c string) string { return strings.Repeat(c, 400) }
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: mk("s")},
		{Role: provider.RoleUser, Content: mk("a")},
		{Role: provider.RoleAssistant, Content: mk("b")},
		{Role: provider.RoleUser, Content: mk("c")},
		{Role: provider.RoleAssistant, Content: mk("d")},
		{Role: provider.RoleUser, Content: mk("e")},
	}

	free := trimToContextWindow(context.Background(), msgs, 1000, 0)
	if len(free) != len(msgs) {
		t.Fatalf("without tools this must fit: kept %d of %d", len(free), len(msgs))
	}

	withTools := trimToContextWindow(withToolTokens(context.Background(), 500), msgs, 1000, 0)
	if len(withTools) >= len(msgs) {
		t.Fatalf("500 tokens of tool definitions must force a trim: kept %d of %d", len(withTools), len(msgs))
	}
	if withTools[0].Role != provider.RoleSystem {
		t.Error("system message must survive the trim")
	}
	if withTools[len(withTools)-1].Content != mk("e") {
		t.Error("the most recent message must survive the trim")
	}
}

func TestTrimToContextWindow_AppliesCalibration(t *testing.T) {
	// A correction learned from the provider makes the same conversation trim
	// earlier — this is what stops a session drifting over the wall between one
	// round and the next.
	mk := func(c string) string { return strings.Repeat(c, 400) }
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: mk("s")},
		{Role: provider.RoleUser, Content: mk("a")},
		{Role: provider.RoleAssistant, Content: mk("b")},
		{Role: provider.RoleUser, Content: mk("c")},
		{Role: provider.RoleAssistant, Content: mk("d")},
		{Role: provider.RoleUser, Content: mk("e")},
	}
	if got := trimToContextWindow(context.Background(), msgs, 1000, 0); len(got) != len(msgs) {
		t.Fatalf("uncalibrated this must fit: kept %d of %d", len(got), len(msgs))
	}

	ctx, cal := withCalibration(context.Background())
	cal.observe(1000, 2000) // provider charged twice what we estimated
	if got := trimToContextWindow(ctx, msgs, 1000, 0); len(got) >= len(msgs) {
		t.Fatalf("a 2x correction must force a trim: kept %d of %d", len(got), len(msgs))
	}
}

func TestCalibration_OnlyEverTightens(t *testing.T) {
	_, c := withCalibration(context.Background())
	if c.factor != 1.0 {
		t.Fatalf("fresh calibration = %v, want 1.0", c.factor)
	}
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
	_, c := withCalibration(context.Background())
	c.observe(0, 5000)
	c.observe(5000, 0)
	if c.factor != 1.0 {
		t.Errorf("factor = %v after unusable observations, want 1.0", c.factor)
	}
}

func TestCalibration_ObserveRejection(t *testing.T) {
	// When the provider names the size it measured, that figure is better than
	// anything a successful response gives us — use it directly.
	_, c := withCalibration(context.Background())
	c.observeRejection(90588, 132235)
	if c.factor < 1.4 {
		t.Errorf("factor = %v, expected the provider's own 132235/90588 ≈ 1.46", c.factor)
	}

	// When it names nothing, step up so the retry is meaningfully smaller.
	_, step := withCalibration(context.Background())
	step.observeRejection(90588, 0)
	if step.factor != rejectionStep {
		t.Errorf("factor = %v, want one %v step", step.factor, rejectionStep)
	}
	// maxOverflowRetries blind steps must reach the ceiling — otherwise a turn
	// runs out of attempts while still under-counting.
	for i := 1; i < maxOverflowRetries; i++ {
		step.observeRejection(90588, 0)
	}
	if step.factor != maxCalibrationFactor {
		t.Errorf("%d rejections must reach the %v cap, got %v", maxOverflowRetries, maxCalibrationFactor, step.factor)
	}
}

func TestToolTokensFromContext_DefaultsToZero(t *testing.T) {
	// Every path that is not the agent loop sends no tools, and must not be
	// charged for them.
	if got := toolTokensFromContext(context.Background()); got != 0 {
		t.Errorf("bare context = %d tool tokens, want 0", got)
	}
	if got := toolTokensFromContext(withToolTokens(context.Background(), 4200)); got != 4200 {
		t.Errorf("stamped context = %d, want 4200", got)
	}
}
