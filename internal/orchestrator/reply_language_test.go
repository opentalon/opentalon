package orchestrator

import (
	"strings"
	"testing"
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
