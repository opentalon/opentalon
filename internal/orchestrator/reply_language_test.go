package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
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

// One request can cost several approvals — a turn with two gated writes asks
// twice — and then the newest user rows are a RUN of bare "y"s with the real
// request behind them. Testing only the newest one leaves every turn after the
// second approval unpinned, which is how an English request came back answered
// in German (ai_eval structure/create_category_and_container_type, once the
// batch actually produced its second confirmation instead of dropping it).
func TestReplyLanguageDirectiveWithHistory_SecondApprovalStillInheritsRequest(t *testing.T) {
	o := newDetectorOrchestrator()
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "Please create the category 'Measuring Instruments' and a new container type 'Shelf'."},
		{Role: provider.RoleAssistant, Content: "This will create the category. Shall I proceed?"},
		{Role: provider.RoleUser, Content: "y", PromptType: provider.PromptTypeConfirmationResponse},
		{Role: provider.RoleAssistant, Content: "This will create the container type. Shall I proceed?"},
	}
	if got := o.replyLanguageDirectiveWithHistory("y", history); !strings.Contains(got, "Reply in English.") {
		t.Errorf("a second approval must still inherit English from the request behind the approvals, got %q", got)
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

// TestReplyLanguageDirectiveWithHistory_SkipsHiddenNote is the regression guard
// for the VISIBLE path (the gap that let the lastUserMessage bug through): a
// hidden [system] note is stored as a role=user row, so the fallback for a
// signal-less visible reply must skip it and follow the earlier visible German
// request — otherwise the user's own turn is answered in the note's language.
func TestReplyLanguageDirectiveWithHistory_SkipsHiddenNote(t *testing.T) {
	o := newDetectorOrchestrator()
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "Bitte lege ein neues Ticket für die defekte Bohrmaschine an."},
		{Role: provider.RoleAssistant, Content: "Der Batch-Job läuft, ich melde mich."},
		{Role: provider.RoleUser, Visibility: provider.VisibilityHidden,
			Content: "[system] Automated update on the background job. Outcome: completed, 42 items updated."},
		{Role: provider.RoleAssistant, Content: "Fertig, 42 Artikel aktualisiert."},
	}
	// "ok danke" (< replyLanguageMinChars) carries no signal → fallback must land
	// on the German request, skipping the English hidden note between them.
	if got := o.replyLanguageDirectiveWithHistory("ok danke", history); !strings.Contains(got, "Reply in German.") {
		t.Errorf("visible fallback must skip the hidden English note and inherit German, got %q", got)
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

// TestReplyLanguageDirectiveWithProfile covers the profile confirmation. When
// neither the current message nor the visible history pins a language, the
// profile's language pins the reply only if it is lingua's leading candidate
// by a clear lead (replyLanguageProfileLeadRatio) for a message of at least
// replyLanguageMinChars runes. The
// messages detect below replyLanguageMinConfidence on their own: "How do I
// create a new item?" English 0.577 (Polish 0.165), "Zeig mir die Tickets."
// German 0.544 (English 0.233), "Tour starten" German 0.431 (French 0.209).
func TestReplyLanguageDirectiveWithProfile(t *testing.T) {
	o := newDetectorOrchestrator()
	// The confirmation emits the same directive as a confident detection.
	reference := map[string]string{
		"English": o.replyLanguageDirective("Can you show me how to create a ticket in my account?"),
		"German":  o.replyLanguageDirective("Kannst du mir zeigen, wie ich ein Ticket in meinem Konto anlege?"),
	}
	germanRequest := []provider.Message{
		{Role: provider.RoleUser, Content: "Bitte lege ein neues Ticket für die defekte Bohrmaschine an."},
	}
	cases := []struct {
		name     string
		msg      string
		language string
		history  []provider.Message
		want     string // language of the expected directive; "" = no directive
	}{
		{"english confirmed by en", "How do I create a new item?", "en", nil, "English"},
		{"upper-case code", "How do I create a new item?", "EN", nil, "English"},
		{"region subtag after a hyphen", "How do I create a new item?", "en-US", nil, "English"},
		{"german confirmed by de", "Zeig mir die Tickets.", "de", nil, "German"},
		{"region subtag after an underscore", "Zeig mir die Tickets.", "de_CH", nil, "German"},
		{"shortest detectable message confirmed", "Tour starten", "de", nil, "German"},
		{"german lead not confirmed by en", "Zeig mir die Tickets.", "en", nil, ""},
		{"shortest message not confirmed by en", "Tour starten", "en", nil, ""},
		{"english lead not confirmed by de", "How do I create a new item?", "de", nil, ""},
		{"no profile language", "How do I create a new item?", "", nil, ""},
		{"code outside the candidates", "How do I create a new item?", "hr", nil, ""},
		{"confident detection wins over the profile", "Erklär mir die Organisationseinheiten.", "en", nil, "German"},
		{"history wins over the profile", "How do I create a new item?", "en", germanRequest, "German"},
		{"no letters to detect on", "1234567890123456", "en", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := o.replyLanguageDirectiveWithProfile(tc.msg, tc.history, tc.language)
			if tc.want == "" {
				if got != "" {
					t.Errorf("replyLanguageDirectiveWithProfile(%q, profile %q) = %q, want no directive", tc.msg, tc.language, got)
				}
				return
			}
			want := reference[tc.want]
			if !strings.Contains(want, "Reply in "+tc.want+".") {
				t.Fatalf("reference directive for %s is %q", tc.want, want)
			}
			if got != want {
				t.Errorf("replyLanguageDirectiveWithProfile(%q, profile %q) = %q, want the %s directive %q",
					tc.msg, tc.language, got, tc.want, want)
			}
		})
	}
}

// TestReplyLanguageDirectiveWithProfile_FromHistory covers the profile
// confirmation on a turn whose current message is too short to detect, as the
// summary turn after an approval ("y", a tapped button): the newest user
// message of at least replyLanguageMinChars runes decides, the current message
// first, then the visible user messages of the history from newest to oldest.
// Older messages are not consulted once one decides. The two requests detect
// below replyLanguageMinConfidence: "How do I create a new item?" English
// 0.577, "Zeig mir die Tickets." German 0.544; "Erklär mir die
// Organisationseinheiten." detects as German at 0.999.
func TestReplyLanguageDirectiveWithProfile_FromHistory(t *testing.T) {
	o := newDetectorOrchestrator()
	reference := map[string]string{
		"English": o.replyLanguageDirective("Can you show me how to create a ticket in my account?"),
		"German":  o.replyLanguageDirective("Kannst du mir zeigen, wie ich ein Ticket in meinem Konto anlege?"),
	}
	english := provider.Message{Role: provider.RoleUser, Content: "How do I create a new item?"}
	german := provider.Message{Role: provider.RoleUser, Content: "Zeig mir die Tickets."}
	confidentGerman := provider.Message{Role: provider.RoleUser, Content: "Erklär mir die Organisationseinheiten."}
	// Each skipped row would lean German if it counted.
	skipped := []provider.Message{
		{Role: provider.RoleAssistant, Content: "Ich lege den Artikel gleich an. Soll ich fortfahren?"},
		{Role: provider.RoleTool, ToolCallID: "c1", Content: "Der Artikel wurde erfolgreich angelegt."},
		{Role: provider.RoleUser, Visibility: provider.VisibilityHidden,
			Content: "[system] Automatische Meldung zum Hintergrundauftrag: abgeschlossen."},
		{Role: provider.RoleUser, Content: "ok"},
	}
	cases := []struct {
		name     string
		msg      string
		language string
		history  []provider.Message
		want     string // language of the expected directive; "" = no directive
	}{
		{"request behind the approval confirmed", "y", "en", []provider.Message{english}, "English"},
		{"non-user, hidden and short rows skipped", "y", "en", append([]provider.Message{english}, skipped...), "English"},
		{"request leaning german not confirmed by en", "y", "en", []provider.Message{german}, ""},
		{"newest request decides, the older one is not reached", "y", "en", []provider.Message{english, german}, ""},
		{"newest request confirmed over an older one", "y", "en", []provider.Message{german, english}, "English"},
		{"request not confirmed by de", "y", "de", []provider.Message{english}, ""},
		{"no profile language", "y", "", []provider.Message{english}, ""},
		{"long enough current message decides", "Zeig mir die Tickets.", "en", []provider.Message{english}, ""},
		{"confident history message wins over the profile", "y", "en", []provider.Message{confidentGerman, english}, "German"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := o.replyLanguageDirectiveWithProfile(tc.msg, tc.history, tc.language)
			if tc.want == "" {
				if got != "" {
					t.Errorf("replyLanguageDirectiveWithProfile(%q, profile %q) = %q, want no directive", tc.msg, tc.language, got)
				}
				return
			}
			want := reference[tc.want]
			if !strings.Contains(want, "Reply in "+tc.want+".") {
				t.Fatalf("reference directive for %s is %q", tc.want, want)
			}
			if got != want {
				t.Errorf("replyLanguageDirectiveWithProfile(%q, profile %q) = %q, want the %s directive %q",
					tc.msg, tc.language, got, tc.want, want)
			}
		})
	}
}

// TestReplyLanguageDirectiveWithProfile_ClearLead covers the size of the lead
// the profile confirmation requires: lingua's leading candidate must be the
// profile's language with at least replyLanguageProfileLeadRatio times the
// runner-up's value. A sentence in another language that names products or
// companies leans only weakly toward the profile's language and stays
// unpinned. Values (leading, runner-up, ratio): "Gib Dell XPS 15 an Sample
// Parts Ltd aus." English 0.384, German 0.306, 1.25; "It's about the Bosch GSR
// 18V." English 0.398, German 0.317, 1.25; "Delete the order for the Munich
// branch spare parts." English 0.481, German 0.282, 1.70; "Tour starten"
// German 0.431, French 0.209, 2.06; "Zeig mir die Tickets." German 0.544,
// English 0.233, 2.33; "How do I create a new item?" English 0.577, Polish
// 0.165, 3.50. "Please create a ticket for the broken drill in the workshop."
// detects as English with confidence.
func TestReplyLanguageDirectiveWithProfile_ClearLead(t *testing.T) {
	o := newDetectorOrchestrator()
	reference := map[string]string{
		"English": o.replyLanguageDirective("Can you show me how to create a ticket in my account?"),
		"German":  o.replyLanguageDirective("Kannst du mir zeigen, wie ich ein Ticket in meinem Konto anlege?"),
	}
	englishRequest := []provider.Message{
		{Role: provider.RoleUser, Content: "Please create a ticket for the broken drill in the workshop."},
	}
	germanWithNames := []provider.Message{
		{Role: provider.RoleUser, Content: "Gib Dell XPS 15 an Sample Parts Ltd aus."},
	}
	cases := []struct {
		name     string
		msg      string
		language string
		history  []provider.Message
		want     string // language of the expected directive; "" = no directive
	}{
		{"german sentence with english names not confirmed by en", "Gib Dell XPS 15 an Sample Parts Ltd aus.", "en", nil, ""},
		{"ambiguous lead not confirmed by en", "It's about the Bosch GSR 18V.", "en", nil, ""},
		{"ambiguous message follows a confident history message", "It's about the Bosch GSR 18V.", "en", englishRequest, "English"},
		{"lead of 1.70 confirmed by en", "Delete the order for the Munich branch spare parts.", "en", nil, "English"},
		{"lead of 2.06 confirmed by de", "Tour starten", "de", nil, "German"},
		{"lead of 2.33 confirmed by de", "Zeig mir die Tickets.", "de", nil, "German"},
		{"lead of 3.50 confirmed by en", "How do I create a new item?", "en", nil, "English"},
		{"history request without a clear lead not confirmed by en", "y", "en", germanWithNames, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := o.replyLanguageDirectiveWithProfile(tc.msg, tc.history, tc.language)
			if tc.want == "" {
				if got != "" {
					t.Errorf("replyLanguageDirectiveWithProfile(%q, profile %q) = %q, want no directive", tc.msg, tc.language, got)
				}
				return
			}
			want := reference[tc.want]
			if !strings.Contains(want, "Reply in "+tc.want+".") {
				t.Fatalf("reference directive for %s is %q", tc.want, want)
			}
			if got != want {
				t.Errorf("replyLanguageDirectiveWithProfile(%q, profile %q) = %q, want the %s directive %q",
					tc.msg, tc.language, got, tc.want, want)
			}
		})
	}
}

// requestRecordingLUMLLM routes requests like lumLLM and records each one.
type requestRecordingLUMLLM struct {
	*lumLLM
	reqMu    sync.Mutex
	requests []*provider.CompletionRequest
}

func (l *requestRecordingLUMLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.reqMu.Lock()
	l.requests = append(l.requests, req)
	l.reqMu.Unlock()
	return l.lumLLM.Complete(ctx, req)
}

func (l *requestRecordingLUMLLM) snapshot() []*provider.CompletionRequest {
	l.reqMu.Lock()
	defer l.reqMu.Unlock()
	return append([]*provider.CompletionRequest(nil), l.requests...)
}

// TestRun_ApprovalSummaryKeepsTheProfileConfirmedLanguage runs a confirmation
// and its approval with the profile language en. The request "How do I create
// a new item?" detects below the confidence floor and is pinned to English by
// the profile; the approval turn's own message is a tapped button, so the
// model call that summarises the approved call must still be pinned to
// English through the request in the history.
func TestRun_ApprovalSummaryKeepsTheProfileConfirmedLanguage(t *testing.T) {
	llm := &requestRecordingLUMLLM{lumLLM: &lumLLM{}}
	exec := &recordingExecutor{}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "items",
		Actions: []Action{native(Action{Name: "create", Description: "Create an item"})},
	}, exec)
	_ = registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{})
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, OrchestratorOpts{ConfirmationPlugin: "conf", ConfirmationAction: "check"})
	ctx := profile.WithProfile(context.Background(), &profile.Profile{ChannelID: "web", Language: "en"})
	const pinned = "## Reply language\nReply in English."

	llm.script([]provider.CompletionResponse{lumToolCall("c-create", "items__create")}, "Shall I create the item?")
	res, err := orch.Run(ctx, "s1", "How do I create a new item?")
	if err != nil {
		t.Fatal(err)
	}
	if res.Metadata["type"] != "confirmation" {
		t.Fatalf("the request turn did not pause for confirmation: %+v", res)
	}
	requestTurn := llm.snapshot()
	if len(requestTurn) == 0 || !strings.Contains(requestTurn[0].Messages[0].Content, pinned) {
		t.Fatalf("the request turn is not pinned to English, so the approval turn has nothing to follow")
	}

	llm.script([]provider.CompletionResponse{{Content: "The item was created."}})
	if _, err := orch.Run(actor.WithConfirmationDecision(ctx, "approve"), "s1", "Approve"); err != nil {
		t.Fatal(err)
	}
	if calls := exec.snapshot(); len(calls) != 1 || calls[0].Action != "create" {
		t.Fatalf("executed calls = %+v, want the approved create", calls)
	}
	approvalTurn := llm.snapshot()[len(requestTurn):]
	if len(approvalTurn) == 0 {
		t.Fatal("no model call followed the approval")
	}
	if len(approvalTurn[0].Tools) == 0 {
		t.Fatalf("the first model call after the approval is not the summarising agent-loop call: %+v", approvalTurn[0].Messages)
	}
	if system := approvalTurn[0].Messages[0].Content; !strings.Contains(system, pinned) {
		t.Errorf("the model call after the approval does not pin English:\n%s", system)
	}
}

// TestRun_FirstTurnProfileLanguageConfirmsUnsureDetection runs the first turn
// of a conversation whose message detects below the confidence floor and
// checks the system prompt the model receives: the profile language en pins
// English, and without a profile language the turn stays unpinned.
func TestRun_FirstTurnProfileLanguageConfirmsUnsureDetection(t *testing.T) {
	cases := []struct {
		name     string
		language string
		pinned   bool
	}{
		{"profile language en", "en", true},
		{"no profile language", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			llm := &segmentsTestLLM{}
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "s1"})
			orch := NewWithRules(llm, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{})
			ctx := profile.WithProfile(context.Background(), &profile.Profile{ChannelID: "web", Language: tc.language})
			if _, err := orch.Run(ctx, "s1", "How do I create a new item?"); err != nil {
				t.Fatal(err)
			}
			if len(llm.requests) == 0 {
				t.Fatal("no model call was made")
			}
			system := llm.requests[0].Messages[0].Content
			if tc.pinned && !strings.Contains(system, "## Reply language\nReply in English.") {
				t.Errorf("system prompt does not pin English:\n%s", system)
			}
			if !tc.pinned && strings.Contains(system, "## Reply language") {
				t.Errorf("system prompt pins a language without a profile language:\n%s", system)
			}
		})
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
	if got := o.replyLanguageDirectiveForHidden(history, ""); !strings.Contains(got, "Reply in German.") {
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
	if got := o.replyLanguageDirectiveForHidden(history, ""); !strings.Contains(got, "Reply in German.") {
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
	if got := o.replyLanguageDirectiveForHidden(history, ""); got != "" {
		t.Errorf("undetectable history should leave the turn unpinned, got %q", got)
	}
}

// TestRun_HiddenTurnProfileLanguageConfirmsHistory runs a hidden turn (a
// background job's note, English text) with the profile language en after a
// history that detects below the confidence floor, and checks the system
// prompt the model receives. Only the history's visible user messages are
// candidates for the profile confirmation, never the note's own text and never
// an assistant row.
func TestRun_HiddenTurnProfileLanguageConfirmsHistory(t *testing.T) {
	cases := []struct {
		name    string
		history []provider.Message
		pinned  bool
	}{
		{"request confirmed by en", []provider.Message{
			{Role: provider.RoleUser, Content: "How do I create a new item?"},
			{Role: provider.RoleAssistant, Content: "Open the item list and choose New."},
		}, true},
		{"request leaning german not confirmed by en", []provider.Message{
			{Role: provider.RoleUser, Content: "Zeig mir die Tickets."},
			{Role: provider.RoleAssistant, Content: "Hier sind die offenen Tickets."},
		}, false},
		{"no user message", []provider.Message{
			{Role: provider.RoleAssistant, Content: "Welcome! How can I help you today?"},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			llm := &segmentsTestLLM{}
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "s1"})
			for _, m := range tc.history {
				if err := sessions.AddMessage("s1", m); err != nil {
					t.Fatal(err)
				}
			}
			orch := NewWithRules(llm, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{})
			ctx := profile.WithProfile(context.Background(), &profile.Profile{ChannelID: "web", Language: "en"})
			ctx = actor.WithVisibility(ctx, provider.VisibilityHidden)
			if _, err := orch.Run(ctx, "s1", "[system] Automated update on the background job. Outcome: completed."); err != nil {
				t.Fatal(err)
			}
			if len(llm.requests) == 0 {
				t.Fatal("no model call was made")
			}
			system := llm.requests[0].Messages[0].Content
			if tc.pinned && !strings.Contains(system, "## Reply language\nReply in English.") {
				t.Errorf("system prompt does not pin English:\n%s", system)
			}
			if !tc.pinned && strings.Contains(system, "## Reply language") {
				t.Errorf("system prompt pins a language:\n%s", system)
			}
		})
	}
}
