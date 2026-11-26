package orchestrator

import (
	"encoding/json"
	"sync"

	"github.com/opentalon/opentalon/internal/provider"
)

// Context-window accounting: what a request will really cost the provider, and
// how much of that the conversation may occupy.
//
// The previous estimate counted `Message.Content` and nothing else. Measured
// against the production gpt-oss endpoint on 2026-08-02, that missed 41 647 of
// 132 235 tokens on a real customer session, which then died and kept dying on
// every later message. Three things were invisible to it and one was mis-priced:
//
//   - the tool definitions, which ride on EVERY request but are not messages;
//   - the arguments of an assistant tool call, which live in Message.ToolCalls
//     and leave Content empty, so each such message counted as zero;
//   - the chat template's per-message framing;
//   - JSON, priced at the same characters per token as prose when it is far
//     denser — and tool results, the content a long session accumulates, are
//     all JSON.

// Characters per token, measured against gpt-oss-120b (OVH) on 2026-08-02 by
// sending known text and reading back usage.prompt_tokens:
//
//	German prose  4.77    English prose  5.36    JSON  2.78
//
// Prose is priced at 4.4 rather than its measured 4.77 so the estimate errs
// high; JSON takes its measured value, which is already the pessimistic end.
// These are a cold-start seed, not a truth: the first response from any model
// replaces them with that model's own ratio (see calibrators). They only have
// to be close enough that the FIRST request against a model does not overflow.
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
// arguments, the thinking blocks it replays, and the per-message framing.
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
	// A block's signature carries its full thinking, which the provider
	// counts as input.
	for _, t := range m.Thinking {
		n += estimateJSONTokens(string(t))
	}
	return n
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

// calibration is the ratio between what a provider actually charges for a
// request and what this file estimates for it, for one model.
type calibration struct {
	factor float64
}

// observe folds "the provider charged `actual` for what we estimated at
// `estimated`" into the correction.
//
// Only ever raises the factor, never lowers it below 1.0: an estimate that came
// out too high costs some unused window, an estimate that came out too low
// kills the session. Capped so one anomalous response — a provider that reports
// cached-prompt tokens differently, say — cannot collapse the usable window.
func (c *calibration) observe(estimated, actual int) {
	if estimated <= 0 || actual <= 0 {
		return
	}
	c.raiseTo(float64(actual) / float64(estimated))
}

// observeRejection folds a "prompt too long" refusal into the correction.
//
// When the provider names the size it measured, that is a better observation
// than any successful response gives us — it is the exact figure the estimate
// should have produced. When it names nothing, the factor steps up so the retry
// is at least meaningfully smaller than the attempt that was just refused.
func (c *calibration) observeRejection(estimated, measured int) {
	if measured > 0 {
		c.observe(estimated, measured)
		return
	}
	c.raiseTo(c.factor * rejectionStep)
}

func (c *calibration) raiseTo(f float64) {
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

// rejectionStep is how far the correction moves when the provider refuses the
// prompt without saying how big it found it: far enough that the compaction the
// refusal triggers (compaction.go) aims meaningfully lower than the request
// that was refused. Providers that DO name the size skip this path and land on
// the exact factor in one go.
const rejectionStep = 1.5

// calibrators holds one correction per model for the lifetime of the process.
//
// Per model, because the ratio is a property of a tokenizer, not of a
// conversation. Per process rather than per turn, because a turn-scoped factor
// makes every turn of a long session re-learn the same correction — and the
// only way to learn it is to overflow once, which costs a wasted request the
// size of the whole window. Not persisted: losing it on restart costs one
// re-learn, which docs/concurrency.md licenses for exactly this class of
// ephemeral counter, and persisting it would buy a schema change and a
// cross-pod agreement problem for nothing.
//
// Guarded, and this matters: subprocessRunner.executeParallel fans one turn's
// context out to several goroutines, so anything reachable from a turn is
// reachable concurrently.
type calibrators struct {
	mu      sync.Mutex
	byModel map[string]*calibration
}

func newCalibrators() *calibrators {
	return &calibrators{byModel: map[string]*calibration{}}
}

// get returns the correction for a model, creating it at 1.0 on first sight.
// The empty model id is a legitimate key: it means "whatever the provider's
// default is", which is what the request asked for and what the response will
// be measured against.
func (c *calibrators) get(model string) *calibration {
	cal, ok := c.byModel[model]
	if !ok {
		cal = &calibration{factor: 1.0}
		c.byModel[model] = cal
	}
	return cal
}

func (c *calibrators) factor(model string) float64 {
	if c == nil {
		return 1.0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.get(model).factor
}

func (c *calibrators) observe(model string, estimated, actual int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.get(model).observe(estimated, actual)
}

func (c *calibrators) observeRejection(model string, estimated, measured int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.get(model).observeRejection(estimated, measured)
}

// inputTokenBudget returns the maximum estimated input tokens a request may
// occupy, given the model's context window and its per-call output budget
// (max_tokens).
//
// The output budget is reserved in full rather than as a flat percentage.
// Measured on 2026-08-02, THIS endpoint does not enforce it — 126 983 input
// tokens went through with max_tokens=32768, a sum of 159 751 against a 131 072
// window — but other OpenAI-compatible endpoints do reject on the sum, and
// reasoning models such as gpt-oss can genuinely spend the whole output budget
// on chain-of-thought. Reserving it is the portable choice; the deployment
// controls the cost by setting max_tokens to what completions actually need.
//
// When maxOutputTokens is unknown (0) we fall back to a 10% reserve. When
// max_tokens (plus margin) alone fills the window — a misconfiguration, since
// max_tokens should be a fraction of the window — the budget clamps to 0
// rather than a positive floor: any positive floor plus that output budget
// would exceed the window and re-admit the very overflow this reserve exists
// to prevent.
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
