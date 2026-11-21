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
//
// A visible turn tries three sources in order: (a) a confident detection on
// the current message, (b) the most recent confidently detectable visible user
// message in the history, (c) the profile's language as a confirmation of an
// unsure detection. Short plain questions often detect below the confidence
// floor — "How do I create a new item?" detects as English at about 0.58 —
// and as the first message of a conversation they have no history to fall
// back on, so the model picks the language and sometimes picks the wrong one.
// Step (c) pins such a message only when lingua's strictly leading candidate
// for it is the language the profile names. It never overrides a detection:
// when lingua leans elsewhere, or the profile names no language or one outside
// the candidate set, the turn stays unpinned. History comes before the profile
// so a running conversation does not switch language because one short
// message, for example one built around a product name, happens to lean
// toward the profile's language.

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
	return replyLanguageDirectiveFor(values[0].Language())
}

// replyLanguageDirectiveFor renders the reply-language directive for lang.
func replyLanguageDirectiveFor(lang lingua.Language) string {
	name := lang.String()
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
// The fallback WALKS BACK rather than testing only the newest user row: one
// request can cost several approvals (a turn whose writes are gated one by
// one), and then the newest user rows are a run of bare "y"s with the real
// request behind them. Stopping at the first of those leaves every turn after
// the second approval unpinned — observed as an English request answered in
// German once a two-write turn started producing its second confirmation.
//
// priorHistory must EXCLUDE the current turn's own rows (pass
// sess.Messages[:msgCountAtStart]); otherwise a just-added tool result or the
// approval reply itself could be mistaken for the request. A detectable current
// message always wins, so a genuine mid-conversation language switch is honoured.
func (o *Orchestrator) replyLanguageDirectiveWithHistory(current string, priorHistory []provider.Message) string {
	if directive := o.replyLanguageDirective(current); directive != "" {
		return directive
	}
	return o.replyLanguageDirectiveFromHistory(priorHistory)
}

// replyLanguageDirectiveWithProfile returns the reply-language directive for a
// visible turn: replyLanguageDirectiveWithHistory, and when that pins nothing,
// the profile's confirmation of an unsure detection on the current message
// (step (c) in the file header). profileLanguage is the profile's ISO 639-1
// code; "" skips the confirmation.
func (o *Orchestrator) replyLanguageDirectiveWithProfile(current string, priorHistory []provider.Message, profileLanguage string) string {
	if directive := o.replyLanguageDirectiveWithHistory(current, priorHistory); directive != "" {
		return directive
	}
	return o.replyLanguageConfirmedByProfile(current, profileLanguage)
}

// replyLanguageConfirmedByProfile returns the reply-language directive for the
// profile's language when lingua's leading candidate for the user's message is
// that language and leads strictly (above 0 and above the runner-up), with no
// confidence floor. It returns "" for a message shorter than
// replyLanguageMinChars, a lead that is another language or a tie, and a
// profile language that is empty or outside replyLanguageCandidates.
func (o *Orchestrator) replyLanguageConfirmedByProfile(userMessage, profileLanguage string) string {
	if o.langDetector == nil {
		return ""
	}
	want, ok := replyLanguageFromISOCode(profileLanguage)
	if !ok {
		return ""
	}
	msg := strings.TrimSpace(userMessage)
	if len([]rune(msg)) < replyLanguageMinChars {
		return ""
	}
	values := o.langDetector.ComputeLanguageConfidenceValues(msg)
	if len(values) == 0 || values[0].Language() != want || values[0].Value() <= 0 {
		return ""
	}
	if len(values) > 1 && values[0].Value() <= values[1].Value() {
		return ""
	}
	return replyLanguageDirectiveFor(want)
}

// replyLanguageFromISOCode maps a profile language to a candidate language. It
// takes an ISO 639-1 code in any case, optionally followed by a region subtag
// after "-" or "_" ("en", "EN", "en-US", "de_CH", "pt-BR"); the subtag is
// ignored. ok is false for an empty value, a code outside
// replyLanguageCandidates and anything that is not such a code.
func replyLanguageFromISOCode(code string) (lingua.Language, bool) {
	if i := strings.IndexAny(code, "-_"); i >= 0 {
		code = code[:i]
	}
	iso := lingua.GetIsoCode639_1FromValue(code)
	for _, lang := range replyLanguageCandidates {
		if lang.IsoCode639_1() == iso {
			return lang, true
		}
	}
	return lingua.Unknown, false
}

// replyLanguageDirectiveForHidden returns the reply-language directive for a
// hidden (system-injected) turn — a background-job status note delivered via
// the inject path. Its text is not the user's own words (it is often an English
// facts line), so detecting on it would answer a German conversation in English.
// The visible history is the only trustworthy signal here, so this path has
// nothing to try first and goes straight to the walk-back.
func (o *Orchestrator) replyLanguageDirectiveForHidden(priorHistory []provider.Message) string {
	return o.replyLanguageDirectiveFromHistory(priorHistory)
}

// replyLanguageDirectiveFromHistory walks back to the most recent VISIBLE user
// message that is long enough to detect confidently, skipping assistant and
// tool rows, hidden system notes, and bare approvals. Returns "" when no
// visible user message is confidently detectable, so the caller leaves the turn
// unpinned and the standing language rule applies.
func (o *Orchestrator) replyLanguageDirectiveFromHistory(priorHistory []provider.Message) string {
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
