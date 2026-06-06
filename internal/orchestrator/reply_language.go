package orchestrator

import (
	"context"
	"fmt"
	"strings"

	lingua "github.com/pemistahl/lingua-go"
)

// Reply-language pinning.
//
// Some models do not reliably honour a standing "answer in the user's
// language" rule on their own: a clearly-English message still comes back in
// another language a noticeable fraction of the time, and the failure rate
// climbs when retrieved [knowledge_context] carries foreign example tokens.
// Naming the target language explicitly, per turn ("Reply in English."),
// removes the guesswork and makes it deterministic.
//
// We detect the language of the user's OWN message — never the preparer-
// augmented content, whose injected knowledge text may be in another language
// — and inject a short, firm directive. The directive is only emitted when
// detection is confident: short or ambiguous messages ("ok", "danke", a bare
// id) fall back to the model plus the standing language rule rather than being
// pinned to a guess. Because detection runs per turn on the current message,
// this also makes mid-conversation language switches follow reliably.

// replyLanguageCandidates bounds detection to a fixed set of common languages.
// A bounded set keeps lingua's memory footprint modest and avoids pinning an
// obscure language on short text.
var replyLanguageCandidates = []lingua.Language{
	lingua.English,
	lingua.German,
	lingua.French,
	lingua.Spanish,
	lingua.Italian,
	lingua.Portuguese,
	lingua.Polish,
	lingua.Lithuanian,
}

const (
	// replyLanguageMinChars is the shortest message we trust detection on.
	// Below it the signal is too thin to pin a language, so we emit nothing
	// and let the standing rule apply.
	replyLanguageMinChars = 12
	// replyLanguageMinConfidence gates the directive on lingua's relative
	// confidence for the top language (0..1). Tuned empirically.
	replyLanguageMinConfidence = 0.65
)

func buildReplyLanguageDetector() lingua.LanguageDetector {
	// WithPreloadedLanguageModels loads the n-gram models at construction
	// (startup) rather than lazily on the first detection, so the one-time
	// model-load cost never lands on a user's request path after a restart.
	return lingua.NewLanguageDetectorBuilder().
		FromLanguages(replyLanguageCandidates...).
		WithPreloadedLanguageModels().
		Build()
}

// replyLanguageDirectiveKey carries the per-turn directive from Run (which sees
// the raw user message) to buildSystemPrompt (which may run for several cached
// prompt variants in the same turn, so detection should happen once).
type replyLanguageDirectiveKey struct{}

func withReplyLanguageDirective(ctx context.Context, directive string) context.Context {
	if directive == "" {
		return ctx
	}
	return context.WithValue(ctx, replyLanguageDirectiveKey{}, directive)
}

func replyLanguageDirectiveFromContext(ctx context.Context) string {
	v, _ := ctx.Value(replyLanguageDirectiveKey{}).(string)
	return v
}

// replyLanguageDirective detects the language of the user's own message and
// returns a firm reply-language instruction, or "" when the message is too
// short or detection is not confident enough to pin a language.
func (o *Orchestrator) replyLanguageDirective(userMessage string) string {
	if o.langDetector == nil {
		return ""
	}
	msg := strings.TrimSpace(userMessage)
	if len([]rune(msg)) < replyLanguageMinChars {
		return ""
	}
	// One pass over the n-grams: ComputeLanguageConfidenceValues returns the
	// candidates sorted by confidence descending, so values[0] is both the
	// winning language and its confidence in a single scan. (DetectLanguageOf
	// plus a separate ComputeLanguageConfidence would each rerun the full
	// scan.) The 0.65 floor already rejects close calls, so we don't need
	// DetectLanguageOf's separate ambiguity check.
	values := o.langDetector.ComputeLanguageConfidenceValues(msg)
	if len(values) == 0 || values[0].Value() < replyLanguageMinConfidence {
		return ""
	}
	name := values[0].Language().String()
	return fmt.Sprintf("## Reply language\nReply in %s. Use %s for your entire reply, regardless of the language of any "+
		"retrieved context or earlier messages. Technical terms (field, tool and status names) stay in English.\n\n", name, name)
}
