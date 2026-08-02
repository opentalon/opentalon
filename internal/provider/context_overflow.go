package provider

import (
	"regexp"
	"strconv"
	"strings"
)

// A "prompt too long" rejection is the one provider error that is entirely our
// own doing. It carries two consequences the generic error path gets wrong:
//
//   - Retrying it on the next endpoint cannot help. Same model, same window —
//     it fails identically, and the turn then reports whatever THAT endpoint
//     said. On 2026-08-01 a customer session overflowed against OVH and the
//     user was shown "app has no replica running" from the dedicated node
//     probed afterwards, which sent the diagnosis in the wrong direction.
//   - It says nothing about the endpoint's health, so tripping the health gate
//     on it takes a working endpoint out of rotation for our mistake.
//
// Providers word it differently but all of them name the size they measured,
// which is worth more than the classification: it is a free, exact reading of
// how far the local estimate was off.

var contextOverflowPatterns = []*regexp.Regexp{
	// vLLM / OVH: "Input length (132235) exceeds model's maximum context length (131072)."
	regexp.MustCompile(`(?i)input length \((\d+)\) exceeds`),
	// OpenAI: "... maximum context length is 8192 tokens. However, your messages resulted in 8500 tokens ..."
	regexp.MustCompile(`(?i)resulted in (\d+) tokens`),
	// Anthropic: "prompt is too long: 200000 tokens > 199999 maximum"
	regexp.MustCompile(`(?i)prompt is too long: (\d+) tokens`),
}

// contextOverflowPhrases catch the same condition when the provider reports no
// number. Kept deliberately narrow: a false positive here would silence a real
// endpoint failure by treating it as our oversized prompt.
var contextOverflowPhrases = []string{
	"maximum context length",
	"context_length_exceeded",
	"prompt is too long",
	"reduce the length of the messages",
	"too many tokens",
}

// ContextOverflow reports whether err is the provider refusing a prompt for
// being longer than the model's window, and how many input tokens it measured.
// The count is 0 when the provider did not name one.
func ContextOverflow(err error) (measured int, ok bool) {
	if err == nil {
		return 0, false
	}
	msg := err.Error()
	for _, re := range contextOverflowPatterns {
		if m := re.FindStringSubmatch(msg); m != nil {
			n, convErr := strconv.Atoi(m[1])
			if convErr != nil {
				n = 0
			}
			return n, true
		}
	}
	lower := strings.ToLower(msg)
	for _, phrase := range contextOverflowPhrases {
		if strings.Contains(lower, phrase) {
			return 0, true
		}
	}
	return 0, false
}

// IsContextOverflow is ContextOverflow without the measured size, for callers
// that only need the classification.
func IsContextOverflow(err error) bool {
	_, ok := ContextOverflow(err)
	return ok
}
