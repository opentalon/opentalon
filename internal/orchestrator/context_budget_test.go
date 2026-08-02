package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

// block is one conversation message's worth of filler — 400 characters, which
// the prose ratio prices at ~97 tokens including framing.
func block(c string) string { return strings.Repeat(c, 400) }

// sixMessages is a system prompt plus five conversation turns: ~500 estimated
// tokens, sized so a 1000-token window fits it with the flat 10 % reserve and
// trims it once anything else (tools, a correction, an output reserve) is
// charged against the same window.
func sixMessages() []provider.Message {
	return []provider.Message{
		{Role: provider.RoleSystem, Content: block("s")},
		{Role: provider.RoleUser, Content: block("a")},
		{Role: provider.RoleAssistant, Content: block("b")},
		{Role: provider.RoleUser, Content: block("c")},
		{Role: provider.RoleAssistant, Content: block("d")},
		{Role: provider.RoleUser, Content: block("e")},
	}
}

func fit(t *testing.T, req *provider.CompletionRequest, window, maxOut int, correction float64) int {
	t.Helper()
	return fitRequestToWindow(context.Background(), req, window, maxOut, correction)
}

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

func TestFitRequestToWindow_LeavesAFittingRequestAlone(t *testing.T) {
	req := &provider.CompletionRequest{Messages: sixMessages()}
	if got := fit(t, req, 100000, 0, 1.0); got == 0 {
		t.Error("expected a non-zero estimate")
	}
	if len(req.Messages) != 6 {
		t.Fatalf("nothing should have been dropped, got %d of 6", len(req.Messages))
	}
}

func TestFitRequestToWindow_DropsOldestFirstAndKeepsTheEnds(t *testing.T) {
	req := &provider.CompletionRequest{Messages: sixMessages()}
	fit(t, req, 400, 0, 1.0)

	if len(req.Messages) >= 6 {
		t.Fatalf("expected a trim, kept %d of 6", len(req.Messages))
	}
	if req.Messages[0].Role != provider.RoleSystem {
		t.Errorf("system message must survive, got %s", req.Messages[0].Role)
	}
	if req.Messages[len(req.Messages)-1].Content != block("e") {
		t.Error("the most recent message must survive")
	}
}

func TestFitRequestToWindow_KeepsEverySystemMessage(t *testing.T) {
	req := &provider.CompletionRequest{Messages: []provider.Message{
		{Role: provider.RoleSystem, Content: block("s")},
		{Role: provider.RoleSystem, Content: "Previous conversation summary: stuff"},
		{Role: provider.RoleUser, Content: strings.Repeat("a", 4000)},
		{Role: provider.RoleAssistant, Content: strings.Repeat("b", 4000)},
		{Role: provider.RoleUser, Content: block("c")},
	}}
	fit(t, req, 2000, 0, 1.0)

	systems := 0
	for _, m := range req.Messages {
		if m.Role == provider.RoleSystem {
			systems++
		}
	}
	if systems != 2 {
		t.Errorf("expected both system messages kept, got %d", systems)
	}
}

func TestFitRequestToWindow_DoesNotOrphanAToolResult(t *testing.T) {
	// The budget forces dropping the oldest user turn AND the assistant
	// tool-call — which would leave the tool RESULT as the new first message: a
	// RoleTool with no preceding call, which the LLM API rejects.
	req := &provider.CompletionRequest{Messages: []provider.Message{
		{Role: provider.RoleSystem, Content: block("s")},
		{Role: provider.RoleUser, Content: block("a")},
		{Role: provider.RoleAssistant, Content: block("b"), ToolCalls: []provider.ToolCall{{ID: "c1", Name: "inv.list"}}},
		{Role: provider.RoleTool, Content: block("r"), ToolCallID: "c1"},
		{Role: provider.RoleAssistant, Content: block("d")},
		{Role: provider.RoleUser, Content: block("e")},
	}}
	fit(t, req, 400, 0, 1.0)

	for i, m := range req.Messages {
		if m.Role == provider.RoleTool {
			t.Errorf("orphaned tool result survived at index %d", i)
		}
	}
	if req.Messages[0].Role != provider.RoleSystem {
		t.Errorf("system message must survive, got %s", req.Messages[0].Role)
	}
	if req.Messages[len(req.Messages)-1].Content != block("e") {
		t.Error("the most recent message must survive")
	}
}

func TestFitRequestToWindow_ChargesForToolDefinitions(t *testing.T) {
	// The whole point of moving the fit onto the assembled request: the tools
	// are half of what it costs, and they used to be free. A request the
	// trimmer believed fit at 90 631 tokens arrived at 132 235.
	free := &provider.CompletionRequest{Messages: sixMessages()}
	fit(t, free, 1000, 0, 1.0)
	if len(free.Messages) != 6 {
		t.Fatalf("without tools this must fit: kept %d of 6", len(free.Messages))
	}

	// ~500 tokens of definitions, the same order as a handful of real tools.
	withTools := &provider.CompletionRequest{
		Messages: sixMessages(),
		Tools: []provider.ToolDefinition{{
			Name:        "timly__list-items",
			Description: strings.Repeat("List items via a Lucene query. ", 70),
		}},
	}
	fit(t, withTools, 1000, 0, 1.0)
	if len(withTools.Messages) >= 6 {
		t.Fatalf("tool definitions must be charged for: kept %d of 6", len(withTools.Messages))
	}
}

func TestFitRequestToWindow_AppliesTheCorrection(t *testing.T) {
	// A correction learned from the provider makes the same conversation trim
	// earlier — this is what stops a session drifting over the wall.
	plain := &provider.CompletionRequest{Messages: sixMessages()}
	fit(t, plain, 1000, 0, 1.0)
	if len(plain.Messages) != 6 {
		t.Fatalf("uncorrected this must fit: kept %d of 6", len(plain.Messages))
	}

	corrected := &provider.CompletionRequest{Messages: sixMessages()}
	fit(t, corrected, 1000, 0, 2.0)
	if len(corrected.Messages) >= 6 {
		t.Fatalf("a 2x correction must force a trim: kept %d of 6", len(corrected.Messages))
	}
}

func TestFitRequestToWindow_ReservingOutputTrimsEarlier(t *testing.T) {
	// Proves the output reserve tightens the usable input as intended:
	// 1000-token window, ~500 estimated. Flat 10 % reserve (900) fits;
	// reserving 400 for output (1000-400-50 = 550) does not.
	fits := &provider.CompletionRequest{Messages: sixMessages()}
	fit(t, fits, 1000, 0, 1.0)
	if len(fits.Messages) != 6 {
		t.Fatalf("flat reserve: expected no trim, kept %d of 6", len(fits.Messages))
	}

	reserved := &provider.CompletionRequest{Messages: sixMessages()}
	fit(t, reserved, 1000, 400, 1.0)
	if len(reserved.Messages) >= 6 {
		t.Fatalf("output reserve: expected a trim, kept %d of 6", len(reserved.Messages))
	}
}

func TestFitRequestToWindow_WindowDisabledStillEstimates(t *testing.T) {
	// contextWindow 0 means "no trimming configured"; the caller still needs
	// the estimate, because that is what the calibration is judged against.
	req := &provider.CompletionRequest{Messages: sixMessages()}
	got := fit(t, req, 0, 0, 1.0)
	if len(req.Messages) != 6 {
		t.Errorf("no window configured must not trim, kept %d of 6", len(req.Messages))
	}
	if got == 0 {
		t.Error("expected an estimate even with trimming disabled")
	}
}

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

	// When it names nothing, step up so the retry is meaningfully smaller —
	// and maxOverflowRetries blind steps must reach the ceiling, or a turn runs
	// out of attempts while still under-counting.
	blind := &calibration{factor: 1.0}
	for i := 0; i < maxOverflowRetries; i++ {
		before := blind.factor
		blind.observeRejection(90588, 0)
		if blind.factor <= before {
			t.Fatalf("rejection %d did not tighten: still %v", i+1, blind.factor)
		}
	}
	if blind.factor != maxCalibrationFactor {
		t.Errorf("%d rejections must reach the %v cap, got %v", maxOverflowRetries, maxCalibrationFactor, blind.factor)
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
