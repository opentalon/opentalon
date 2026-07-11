package orchestrator

import (
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

func newDetectorOrchestrator() *Orchestrator {
	return &Orchestrator{langDetector: buildReplyLanguageDetector()}
}

func TestReplyLanguageDirective_ConfidentLanguages(t *testing.T) {
	o := newDetectorOrchestrator()
	// One case per shipped candidate language (replyLanguageCandidates), so a
	// regression in the candidate set or the confidence gate is caught.
	cases := []struct {
		name string
		msg  string
		want string // expected "Reply in <X>." language name
	}{
		{"english", "Can you show me how to create a ticket in my account?", "English"},
		{"german", "Kannst du mir zeigen, wie ich ein Ticket in meinem Konto anlege?", "German"},
		{"french", "Peux-tu me montrer comment créer un ticket dans mon compte ?", "French"},
		{"spanish", "¿Puedes mostrarme cómo crear un ticket en mi cuenta?", "Spanish"},
		{"italian", "Puoi mostrarmi come creare un ticket nel mio account?", "Italian"},
		{"portuguese", "Você pode me mostrar como criar um ticket na minha conta?", "Portuguese"},
		{"polish", "Czy możesz mi pokazać, jak utworzyć zgłoszenie na moim koncie?", "Polish"},
		{"lithuanian", "Ar galite parodyti, kaip sukurti bilietą mano paskyroje?", "Lithuanian"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := o.replyLanguageDirective(tc.msg)
			if !strings.Contains(got, "Reply in "+tc.want+".") {
				t.Errorf("replyLanguageDirective(%q) = %q, want it to pin %q", tc.msg, got, tc.want)
			}
		})
	}
}

// TestReplyLanguageDirective_Wording guards the two clauses that are the whole
// point of the directive: ignore the language of retrieved context, and keep
// technical terms in English. A refactor that drops them must fail here.
func TestReplyLanguageDirective_Wording(t *testing.T) {
	o := newDetectorOrchestrator()
	got := o.replyLanguageDirective("Can you show me how to create a ticket in my account?")
	for _, want := range []string{
		"## Reply language",
		"Reply in English.",
		"regardless of the language of any retrieved context",
		"Technical terms (field, tool and status names) stay in English.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("directive missing %q\ngot: %q", want, got)
		}
	}
}

// TestReplyLanguageDirective_LowConfidenceSkipped covers the confidence gate:
// a long-enough message with no clear single language (here "Lorem ipsum …",
// which detects at ~0.19) must yield no directive rather than pin a guess.
func TestReplyLanguageDirective_LowConfidenceSkipped(t *testing.T) {
	o := newDetectorOrchestrator()
	if got := o.replyLanguageDirective("Lorem ipsum dolor sit amet"); got != "" {
		t.Errorf("low-confidence message should yield empty directive, got %q", got)
	}
}

// TestReplyLanguageDirective_ShortMessageSkipped covers the length gate,
// including an 11-rune message (just under replyLanguageMinChars=12) that is
// otherwise clearly German — it must still be skipped as too short.
func TestReplyLanguageDirective_ShortMessageSkipped(t *testing.T) {
	o := newDetectorOrchestrator()
	for _, msg := range []string{"ok", "Danke", "Hi", "👍", "  ", "42", "Hallo Welt!"} {
		if got := o.replyLanguageDirective(msg); got != "" {
			t.Errorf("replyLanguageDirective(%q) = %q, want empty (too short to pin)", msg, got)
		}
	}
}

func TestReplyLanguageDirective_NilDetectorSafe(t *testing.T) {
	o := &Orchestrator{} // no detector wired
	if got := o.replyLanguageDirective("Can you show me how to create a ticket?"); got != "" {
		t.Errorf("nil detector should yield empty directive, got %q", got)
	}
}

// TestReplyLanguageDirectiveWithHistory_ApprovalInheritsRequest is the
// regression guard for the confirm→approve→summarise drift: a bare "y" approval
// carries no language signal, so the summary must inherit the language of the
// request it fulfils (here English) instead of dropping the pin and letting the
// model default to another language.
func TestReplyLanguageDirectiveWithHistory_ApprovalInheritsRequest(t *testing.T) {
	o := newDetectorOrchestrator()
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "Please create a ticket to repair the drill at the Hamburg warehouse."},
		{Role: provider.RoleAssistant, Content: "I'm about to create that ticket. Shall I proceed?"},
	}
	if got := o.replyLanguageDirectiveWithHistory("y", history); !strings.Contains(got, "Reply in English.") {
		t.Errorf("bare approval should inherit English from the request, got %q", got)
	}
}

// A short non-approval follow-up ("ok") after a German request must also inherit
// German — the fallback is general, not approval-specific.
func TestReplyLanguageDirectiveWithHistory_ShortReplyInheritsGerman(t *testing.T) {
	o := newDetectorOrchestrator()
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "Bitte lege ein neues Ticket für die defekte Bohrmaschine an."},
	}
	if got := o.replyLanguageDirectiveWithHistory("ok", history); !strings.Contains(got, "Reply in German.") {
		t.Errorf("short reply should inherit German from the request, got %q", got)
	}
}

// A detectable current message wins over history: a genuine mid-conversation
// switch must NOT be overridden by the earlier language.
func TestReplyLanguageDirectiveWithHistory_CurrentWins(t *testing.T) {
	o := newDetectorOrchestrator()
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "How many items do we have in total?"},
	}
	got := o.replyLanguageDirectiveWithHistory("Kannst du mir bitte alle defekten Geräte im Lager auflisten?", history)
	if !strings.Contains(got, "Reply in German.") {
		t.Errorf("a detectable current message must win over history, got %q", got)
	}
}

// No detectable signal anywhere (short current message, no usable history) must
// still yield an empty directive — the standing rule then applies, as before.
func TestReplyLanguageDirectiveWithHistory_NoSignal(t *testing.T) {
	o := newDetectorOrchestrator()
	history := []provider.Message{{Role: provider.RoleUser, Content: "ok"}}
	if got := o.replyLanguageDirectiveWithHistory("y", history); got != "" {
		t.Errorf("no detectable signal should yield empty directive, got %q", got)
	}
}

// TestReplyLanguageDirectiveForHidden_FollowsVisibleConversation is the guard
// for the system-inject case: a hidden background-job status note (often an
// English facts line) must NOT pin the reply language to itself. The reply must
// follow the visible conversation — here German — walking back past the bare
// approval to the German request.
func TestReplyLanguageDirectiveForHidden_FollowsVisibleConversation(t *testing.T) {
	o := newDetectorOrchestrator()
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "Bitte nummeriere alle Türen der Reihe nach durch."},
		{Role: provider.RoleAssistant, Content: "Das würde 20 Türen umbenennen. Fortfahren?"},
		{Role: provider.RoleUser, Content: "Genehmigen"}, // short approval, carries no signal
		{Role: provider.RoleAssistant, Content: "Der Batch-Job wurde erstellt."},
	}
	if got := o.replyLanguageDirectiveForHidden(history); !strings.Contains(got, "Reply in German.") {
		t.Errorf("hidden turn should follow the visible German conversation, got %q", got)
	}
}

// An earlier hidden turn (a previous notify, in English) must itself be skipped
// as a language signal, so a later hidden turn still resolves to the visible
// user's language.
func TestReplyLanguageDirectiveForHidden_SkipsEarlierHiddenTurn(t *testing.T) {
	o := newDetectorOrchestrator()
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "Bitte lege ein Ticket für die defekte Bohrmaschine an."},
		{Role: provider.RoleAssistant, Content: "Erledigt."},
		{Role: provider.RoleUser, Visibility: provider.VisibilityHidden,
			Content: "[system] Automated update on the background job. Outcome: completed."},
		{Role: provider.RoleAssistant, Content: "Ihr Ticket wurde erstellt."},
	}
	if got := o.replyLanguageDirectiveForHidden(history); !strings.Contains(got, "Reply in German.") {
		t.Errorf("earlier hidden English turn must be skipped, got %q", got)
	}
}

// No confidently detectable visible user message → no pin, so the caller leaves
// the turn unpinned and the standing rule (plus the note's own locale hint)
// applies. This is the "fall back to the Timly language setting" path.
func TestReplyLanguageDirectiveForHidden_NoDetectableHistory(t *testing.T) {
	o := newDetectorOrchestrator()
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "ok"},
		{Role: provider.RoleAssistant, Content: "..."},
	}
	if got := o.replyLanguageDirectiveForHidden(history); got != "" {
		t.Errorf("undetectable history should leave the turn unpinned, got %q", got)
	}
}
