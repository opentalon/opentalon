package orchestrator

import (
	"context"
	"encoding/json"

	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/provider"
)

// Context-window accounting: what a request will really cost the provider, and
// how much of that the conversation may occupy.
//
// The previous estimate counted `Message.Content` and nothing else. Measured
// against the production gpt-oss endpoint on 2026-08-02, that missed 41 647 of
// 132 235 tokens — 46 % — on a real customer session, which then died and kept
// dying on every later message. Three things were invisible to it and one was
// mis-priced:
//
//   - the tool definitions, which ride on EVERY request but are not messages
//     (22 081 tokens at the 40-tool sticky cap);
//   - the arguments of an assistant tool call, which live in Message.ToolCalls
//     and leave Content empty, so each such message counted as zero;
//   - the chat template's per-message framing (6.1 tokens per message);
//   - JSON, priced at the same 4 characters per token as prose when it really
//     runs 2.78 — and tool results, the content a long session accumulates,
//     are all JSON.

// Characters per token, measured against gpt-oss-120b (OVH) on 2026-08-02 by
// sending known text and reading back usage.prompt_tokens:
//
//	German prose  4.77    English prose  5.36    JSON  2.78
//
// Prose is priced at 4.4 rather than its measured 4.77 so the estimate errs
// high; JSON takes its measured value, which is already the pessimistic end.
// Both are ratios, not truths — the calibration below corrects whatever the
// real mix turns out to be. They only have to be close enough that the FIRST
// request of a turn does not overflow before any correction exists.
const (
	charsPerTokenProse = 4.4
	charsPerTokenJSON  = 2.78
)

// perMessageOverhead is what the chat template costs per message on top of its
// content. Measured at 6.1 (the same 8 000 characters as one message: 1 678
// tokens; as fifty messages: 1 979). Rounded up, because a hundred-message
// session should over-reserve by 90 tokens rather than under-reserve.
const perMessageOverhead = 7

func estimateProseTokens(s string) int { return int(float64(len(s)) / charsPerTokenProse) }

func estimateJSONTokens(s string) int { return int(float64(len(s)) / charsPerTokenJSON) }

// estimateMessageTokens returns what one message costs: its text, its tool-call
// arguments, and the per-message framing.
//
// Role decides the price of the text rather than any sniffing of the content: a
// role=tool message IS a serialised tool result and a role=user/assistant one is
// prose. The tool calls are marshalled rather than measured field by field so
// the braces and quotes the provider is charging for are actually in the count.
func estimateMessageTokens(m provider.Message) int {
	n := perMessageOverhead
	if m.Role == provider.RoleTool {
		n += estimateJSONTokens(m.Content)
	} else {
		n += estimateProseTokens(m.Content)
	}
	if len(m.ToolCalls) > 0 {
		if b, err := json.Marshal(m.ToolCalls); err == nil {
			n += estimateJSONTokens(string(b))
		}
	}
	return n
}

func estimateMessagesTokens(messages []provider.Message) int {
	total := 0
	for _, m := range messages {
		total += estimateMessageTokens(m)
	}
	return total
}

// estimateToolTokens returns what the tool definitions cost. Name and
// description are prose; the JSON Schema is JSON.
func estimateToolTokens(tools []provider.ToolDefinition) int {
	total := 0
	for _, t := range tools {
		total += estimateProseTokens(t.Name) + estimateProseTokens(t.Description)
		if b, err := json.Marshal(t.Parameters); err == nil {
			total += estimateJSONTokens(string(b))
		}
	}
	return total
}

// toolTokensKey carries the cost of the tool definitions attached to this
// round's request into the message assembly.
//
// The two run in the wrong order to pass it as an argument: appendConversation
// trims while assembling, and req.Tools is set afterwards in the agent loop. So
// the loop stamps the already-computed figure onto the context whenever it
// (re)builds the tool set, and the trimmer reads it from there. Absent — every
// non-agent-loop caller — it reads 0, which is correct for those paths: they
// send no tools.
type toolTokensKey struct{}

func withToolTokens(ctx context.Context, tokens int) context.Context {
	return context.WithValue(ctx, toolTokensKey{}, tokens)
}

func toolTokensFromContext(ctx context.Context) int {
	n, _ := ctx.Value(toolTokensKey{}).(int)
	return n
}

// calibrationKey carries the correction factor learned within the current turn.
type calibrationKey struct{}

// calibration is the ratio between what the provider actually charged for a
// request and what this file estimated for it. One pointer is stamped onto the
// turn's context and updated in place after every response, so later rounds of
// the same agent loop — the rounds where tool results pile up and the window
// gets tight — size themselves against what this exact model charged for this
// exact conversation, not against a constant.
//
// Deliberately turn-scoped and in-memory: a session's mix of prose and JSON is
// stable within a turn, and keeping it out of the session row avoids a schema
// change and any cross-pod agreement problem. A fresh turn simply starts from
// the constants again.
//
// Unguarded on purpose: the turn context never leaves its goroutine — every
// background job Run starts (summarization, title generation, catalogue
// refresh) is handed context.Background() precisely so it cannot inherit turn
// state. Hand this context to a goroutine and this field needs a mutex.
type calibration struct {
	factor float64
}

func withCalibration(ctx context.Context) (context.Context, *calibration) {
	c := &calibration{factor: 1.0}
	return context.WithValue(ctx, calibrationKey{}, c), c
}

func calibrationFromContext(ctx context.Context) float64 {
	c, _ := ctx.Value(calibrationKey{}).(*calibration)
	if c == nil || c.factor <= 0 {
		return 1.0
	}
	return c.factor
}

// observe records what the provider charged for a request we had estimated,
// and folds it into the running correction.
//
// Only ever raises the factor, never lowers it below 1.0: an estimate that came
// out too high costs some unused window, an estimate that came out too low
// kills the session. The factor is capped so one anomalous response (a provider
// that reports cached-prompt tokens differently, say) cannot collapse the usable
// window to nothing.
func (c *calibration) observe(estimated, actual int) {
	if c == nil || estimated <= 0 || actual <= 0 {
		return
	}
	f := float64(actual) / float64(estimated)
	if f < 1.0 {
		f = 1.0
	}
	if f > maxCalibrationFactor {
		f = maxCalibrationFactor
	}
	if f > c.factor {
		c.factor = f
	}
}

// maxCalibrationFactor bounds the correction. The worst mis-pricing this file
// can produce is prose-vs-JSON, a factor of 4.4/2.78 ≈ 1.6; 2.0 leaves room for
// framing the measurement did not cover without letting a single odd response
// squeeze the conversation to nothing.
const maxCalibrationFactor = 2.0

// maxOverflowRetries bounds how often one turn may re-run a round after a
// "prompt too long" refusal. Two is enough for the correction to reach its
// ceiling from any starting point; beyond that the conversation genuinely does
// not fit and the turn should say so rather than spin.
const maxOverflowRetries = 2

// rejectionStep is how far the correction moves when the provider refuses the
// prompt without saying how big it found it. Sized so that maxOverflowRetries
// blind steps traverse the whole range up to maxCalibrationFactor: with only
// two attempts to spend, a timid step would run out of retries still under-
// counting. Providers that DO name the size skip this path entirely and land on
// the exact factor in one go.
const rejectionStep = 1.5

// observeRejection folds a "prompt too long" refusal into the correction.
//
// When the provider names the size it measured, that is a better observation
// than any successful response gives us — it is the exact figure the estimate
// should have produced. When it names nothing, the factor steps up so the retry
// is at least meaningfully smaller than the attempt that was just refused.
func (c *calibration) observeRejection(estimated, measured int) {
	if c == nil {
		return
	}
	if measured > 0 {
		c.observe(estimated, measured)
		return
	}
	f := c.factor * rejectionStep
	if f > maxCalibrationFactor {
		f = maxCalibrationFactor
	}
	c.factor = f
}

// inputTokenBudget returns the maximum estimated input tokens the assembled
// message list may occupy, given the model's context window and its per-call
// output budget (max_tokens).
//
// The output budget is reserved in full rather than as a flat percentage.
// Measured on 2026-08-02, THIS endpoint does not enforce it — 126 983 input
// tokens went through with max_tokens=32768, a sum of 159 751 against a 131 072
// window — but other OpenAI-compatible endpoints do reject on the sum, and
// reasoning models such as gpt-oss can genuinely spend the whole output budget
// on chain-of-thought. Reserving it is the portable choice; the deployment
// controls the cost by setting max_tokens to what completions actually need.
//
// When maxOutputTokens is unknown (0) we fall back to the historical 10%
// reserve. When max_tokens (plus margin) alone fills the window — a
// misconfiguration, since max_tokens should be a fraction of the window — the
// budget clamps to 0 rather than a positive floor: any positive floor plus
// that output budget would exceed the window and re-admit the very overflow
// this reserve exists to prevent. The trim loop still keeps the system prompt
// and the most recent message, so a 0 budget cannot strand it.
func inputTokenBudget(contextWindow, maxOutputTokens int) int {
	if contextWindow <= 0 {
		return 0
	}
	if maxOutputTokens <= 0 {
		return contextWindow * 9 / 10
	}
	margin := contextWindow / 20 // 5% headroom for framing the estimate does not model
	budget := contextWindow - maxOutputTokens - margin
	if budget < 0 {
		return 0
	}
	return budget
}

// trimToContextWindow drops the oldest conversation messages (preserving system
// messages at the front) until the whole request — messages AND the tool
// definitions that ride along with them — fits the usable input budget.
//
// The tool definitions are subtracted off the top rather than trimmed: the model
// cannot call what it cannot see, so the conversation is what gives way.
func trimToContextWindow(ctx context.Context, messages []provider.Message, contextWindow, maxOutputTokens int) []provider.Message {
	log := logger.FromContext(ctx)
	correction := calibrationFromContext(ctx)
	budget := inputTokenBudget(contextWindow, maxOutputTokens)

	// What the tools cost is not negotiable, so it comes off the budget before
	// the conversation is measured against it.
	toolTokens := int(float64(toolTokensFromContext(ctx)) * correction)
	maxInputTokens := budget - toolTokens
	if maxInputTokens < 0 {
		maxInputTokens = 0
	}

	corrected := func(msgs []provider.Message) int {
		return int(float64(estimateMessagesTokens(msgs)) * correction)
	}

	total := corrected(messages)
	if total <= maxInputTokens {
		return messages
	}

	// Find where conversation messages start (skip leading system messages).
	convStart := 0
	for convStart < len(messages) && messages[convStart].Role == provider.RoleSystem {
		convStart++
	}

	// Drop oldest conversation messages (pairs of assistant+user typically)
	// until we fit. Always keep at least the last conversation message.
	for total > maxInputTokens && convStart < len(messages)-1 {
		total -= int(float64(estimateMessageTokens(messages[convStart])) * correction)
		convStart++
	}
	// Don't leave a tool result orphaned by dropping its tool-call — advance the
	// boundary past any leading orphaned results (but never to an empty slice).
	if i := firstNonOrphanIndex(messages, convStart); i < len(messages) {
		convStart = i
	}

	trimmed := make([]provider.Message, 0, len(messages))
	// Keep system messages.
	for i := 0; i < len(messages); i++ {
		if messages[i].Role == provider.RoleSystem {
			trimmed = append(trimmed, messages[i])
		} else {
			break
		}
	}
	// Append remaining conversation messages.
	trimmed = append(trimmed, messages[convStart:]...)

	if len(trimmed) < len(messages) {
		log.Info("context trimming",
			"dropped", len(messages)-len(trimmed),
			"tokens", total, "limit", maxInputTokens,
			"tool_tokens", toolTokens, "calibration", correction)
	}
	// Running out of things to drop is not the same as fitting. Saying so is
	// the difference between a diagnosable warning and the old log line, which
	// reported the leftover estimate as though the trim had succeeded.
	if total > maxInputTokens {
		log.Warn("context trimming could not free enough room; request will likely be rejected",
			"tokens", total, "limit", maxInputTokens,
			"tool_tokens", toolTokens, "kept_messages", len(trimmed))
	}

	return trimmed
}
