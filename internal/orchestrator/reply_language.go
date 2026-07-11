package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/opentalon/opentalon/internal/provider"
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

// replyLanguageDirectiveWithHistory returns the reply-language directive for the
// current message, falling back to recent history when the current message is
// too short or ambiguous to detect on its own.
//
// A bare approval ("y", a tapped Approve button), an "ok", or a numeric reply
// carries no language signal, so replyLanguageDirective returns "" for it and
// the turn loses its language pin — the model then answers in whatever it
// defaults to. This is the confirm→approve→summarise path: the summary turn is
// driven by the approval reply, not by the request it fulfils, so an approved
// mutation would be summarised in the model's default language instead of the
// one the user asked in. Fall back to the most recent earlier user message that
// IS detectable so the reply stays in the user's language.
//
// priorHistory must EXCLUDE the current turn's own rows (pass
// sess.Messages[:msgCountAtStart]); otherwise a just-added tool result or the
// approval reply itself could be mistaken for the request. A detectable current
// message always wins, so a genuine mid-conversation language switch is honoured.
func (o *Orchestrator) replyLanguageDirectiveWithHistory(current string, priorHistory []provider.Message) string {
	if directive := o.replyLanguageDirective(current); directive != "" {
		return directive
	}
	if prev := lastUserMessage(priorHistory); prev != "" {
		return o.replyLanguageDirective(prev)
	}
	return ""
}

// replyLanguageDirectiveForHidden returns the reply-language directive for a
// hidden (system-injected) turn — a background-job status note delivered via
// the inject path. Its text is not the user's own words (it is often an English
// facts line), so detecting on it would answer a German conversation in English.
// Instead walk back to the most recent VISIBLE user message that is long enough
// to detect confidently, skipping bare approvals and any earlier hidden turns.
// Returns "" when no visible user message is confidently detectable, so the
// caller leaves the turn unpinned and the standing language rule (plus any
// locale hint the injected note itself carries) applies.
func (o *Orchestrator) replyLanguageDirectiveForHidden(priorHistory []provider.Message) string {
	for i := len(priorHistory) - 1; i >= 0; i-- {
		m := priorHistory[i]
		if !isVisibleUserMessage(m) { // skip assistant/tool rows and hidden system notes
			continue
		}
		if directive := o.replyLanguageDirective(stripKnowledgeContext(m.Content)); directive != "" {
			return directive
		}
	}
	return ""
}
