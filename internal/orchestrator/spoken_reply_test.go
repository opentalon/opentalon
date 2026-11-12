package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// metadataRecorder keeps what the orchestrator stores with each message; the in-memory store drops it.
type metadataRecorder struct {
	SessionStoreInterface
	stored []recordedMessage
}

type recordedMessage struct {
	msg  provider.Message
	meta map[string]string
}

func (r *metadataRecorder) AddMessageWithMetadata(id string, msg provider.Message, meta map[string]string) error {
	r.stored = append(r.stored, recordedMessage{msg, meta})
	return r.SessionStoreInterface.AddMessageWithMetadata(id, msg, meta)
}

func (r *metadataRecorder) last(role provider.Role) recordedMessage {
	for i := len(r.stored) - 1; i >= 0; i-- {
		if r.stored[i].msg.Role == role {
			return r.stored[i]
		}
	}
	return recordedMessage{}
}

func TestSplitSpoken(t *testing.T) {
	const call = `[tool_call]{"tool":"p__a","args":{}}[/tool_call]`
	const argWithMarkers = `[tool_call]{"tool":"p__a","args":{"text":"<spoken>x</spoken>"}}[/tool_call]`
	cases := []struct{ reply, spoken, rest, shown string }{
		{"<spoken>Zwei gefunden.</spoken>\nHier die Liste:\n- A", "Zwei gefunden.", "Hier die Liste:\n- A", "Hier die Liste:\n- A"},
		{"  <spoken> Fertig. </spoken>  Angelegt.", "Fertig.", "Angelegt.", "Angelegt."},
		{"Angelegt.", "", "Angelegt.", "Angelegt."},
		{"<spoken>Erledigt.</spoken>", "Erledigt.", "", "Erledigt."},
		{"<spoken>nie geschlossen Angelegt.", "", "", "nie geschlossen Angelegt."},
		{"Vorweg.\n<spoken>nie geschlossen.", "", "Vorweg.", "Vorweg.\nnie geschlossen."},
		{"<spoken></spoken>Angelegt.", "", "Angelegt.", "Angelegt."},
		{"<spoken>Erstens.</spoken><spoken>Zweitens.</spoken>Antwort.</spoken>", "Erstens.", "Antwort.", "Antwort."},
		{"<spoken>A<spoken>B</spoken>C</spoken>D", "ABC", "D", "D"},
		{"<spoken>A<spoken>B</spoken>C", "", "", "ABC"},
		{"<spoken>Ich sehe nach. " + call + "</spoken>Antwort.", "Ich sehe nach.", "Antwort.", "Antwort."},
		{"<spoken>A<spoken>B</spoken>" + call + "</spoken>Antwort.", "AB", "Antwort.", "Antwort."},
		{"<spoken>" + call, "", "", ""},
		{"<spoken>Erledigt. <invoke name=\"p__a\"><parameter name=\"id\">7</parameter></invoke></spoken>", "Erledigt.", "", "Erledigt."},
		{"[plugin_output]<spoken>geheim</spoken>[/plugin_output]Erledigt.", "",
			"[plugin_output]<spoken>geheim</spoken>[/plugin_output]Erledigt.", "[plugin_output]<spoken>geheim</spoken>[/plugin_output]Erledigt."},
		{argWithMarkers, "", argWithMarkers, argWithMarkers},
	}
	for _, c := range cases {
		if spoken, rest, shown := splitSpoken(c.reply); spoken != c.spoken || rest != c.rest || shown != c.shown {
			t.Errorf("splitSpoken(%q) = %q, %q, %q; want %q, %q, %q", c.reply, spoken, rest, shown, c.spoken, c.rest, c.shown)
		}
	}
}

func TestALongSpokenOnlyReplyStaysWholeAndIsNotRead(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("Das ist ein langer Satz. ", 30))
	orch, sessionID := setupOrchestrator(&fakeLLM{responses: []string{"<spoken>" + long + "</spoken>"}}, DefaultParser)
	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Erzähl")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != long {
		t.Errorf("the shown text lost words: %d of %d characters", len(result.Response), len(long))
	}
	if result.Metadata[SpokenMissingMetadataKey] != "true" {
		t.Errorf("an over-long spoken version is read: %v", result.Metadata)
	}
}

func TestEveryWayASpokenTurnEndsSaysWhatToRead(t *testing.T) {
	spokenCtx := actor.WithSpokenReply(context.Background())
	short := &RunResult{Response: "Abgebrochen."}
	markUnspoken(spokenCtx, short)
	long := &RunResult{Response: strings.Repeat("x", spokenLimit+1)}
	markUnspoken(spokenCtx, long)
	kept := &RunResult{Response: "Liste", Metadata: map[string]string{SpokenMetadataKey: "Zwei gefunden."}}
	markUnspoken(spokenCtx, kept)
	typed := &RunResult{Response: "Abgebrochen."}
	markUnspoken(context.Background(), typed)
	failed := &RunResult{Response: "Request blocked: guard g failed.", Metadata: map[string]string{"type": "error", "error_code": "guard_blocked"}}
	markUnspoken(spokenCtx, failed)

	if short.Metadata[SpokenMetadataKey] != "Abgebrochen." || long.Metadata[SpokenMissingMetadataKey] != "true" ||
		kept.Metadata[SpokenMetadataKey] != "Zwei gefunden." || typed.Metadata != nil || len(failed.Metadata) != 2 {
		t.Errorf("short %v, long %v, kept %v, typed %v, failed %v", short.Metadata, long.Metadata, kept.Metadata, typed.Metadata, failed.Metadata)
	}
}

func TestASpokenTurnsErrorIsNotRead(t *testing.T) {
	orch, sessionID := setupOrchestrator(&fakeLLM{}, DefaultParser)
	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	_, spoken := result.Metadata[SpokenMetadataKey]
	_, missing := result.Metadata[SpokenMissingMetadataKey]
	if result.Metadata["error_code"] != "empty_content" || spoken || missing {
		t.Errorf("error result metadata = %v, want the error without spoken or spoken_missing", result.Metadata)
	}
}

func TestARejectedPlanIsStoredWithWhatIsRead(t *testing.T) {
	orch, sessionID := setupOrchestrator(&fakeLLM{}, DefaultParser)
	rec := &metadataRecorder{SessionStoreInterface: orch.sessions}
	orch.sessions = rec
	orch.pendingPipelines[sessionID] = pendingPipeline{plan: pipeline.NewPipeline(nil, pipeline.PipelineConfig{}), spoken: true}

	ctx := actor.WithConfirmationDecision(actor.WithSpokenReply(context.Background()), pipeline.DecisionReject)
	result, err := orch.Run(ctx, sessionID, "Nein")
	if err != nil {
		t.Fatal(err)
	}
	stored := rec.last(provider.RoleAssistant)
	if result.Metadata[SpokenMetadataKey] == "" || stored.msg.Content != result.Response ||
		stored.meta[SpokenMetadataKey] != result.Metadata[SpokenMetadataKey] {
		t.Errorf("result %q %v, stored %q %v; want the stored reply read as the live one", result.Response, result.Metadata, stored.msg.Content, stored.meta)
	}
}

func TestAGivenUpSpokenTurnIsStoredWithWhatIsRead(t *testing.T) {
	t.Parallel() // the empty-response retries back off for 15 seconds
	responses := make([]string, 6)
	for i := range responses {
		responses[i] = unparseableToolBlock
	}
	orch, sessionID := setupOrchestrator(&fakeLLM{responses: responses}, &fakeParser{parseFn: func(string) []ToolCall { return nil }})
	rec := &metadataRecorder{SessionStoreInterface: orch.sessions}
	orch.sessions = rec

	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Hallo")
	if err != nil {
		t.Fatal(err)
	}
	stored := rec.last(provider.RoleAssistant)
	if result.Response != "(no response)" || result.Metadata[SpokenMetadataKey] == "" ||
		stored.meta[SpokenMetadataKey] != result.Metadata[SpokenMetadataKey] {
		t.Errorf("result %q %v, stored %q %v; want the stored reply read as the live one", result.Response, result.Metadata, stored.msg.Content, stored.meta)
	}
}

func TestAProtocolBlockIsNoMoreReadThanShown(t *testing.T) {
	llm := &fakeLLM{responses: []string{"<spoken>Erledigt. [plugin_output]geheim[/plugin_output]</spoken>Erledigt."}}
	orch, sessionID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Leg an")
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Metadata[SpokenMetadataKey]; got != "Erledigt." {
		t.Errorf("spoken = %q, want it without the protocol block", got)
	}
	if meta := spokenMetadata(actor.WithSpokenReply(context.Background()), "[tool_call]x[/tool_call]"); meta[SpokenMissingMetadataKey] != "true" {
		t.Errorf("a spoken version that is only a protocol block: %v; want spoken_missing", meta)
	}
}

func TestSpokenTurnSendsAndStoresTheSpokenVersionApart(t *testing.T) {
	llm := &fakeLLM{responses: []string{"<spoken>Ich habe zwei Akkuschrauber gefunden, die Liste steht im Chat.</spoken>\nHier die Liste:\n- A\n- B"}}
	orch, sessionID := setupOrchestrator(llm, DefaultParser)
	rec := &metadataRecorder{SessionStoreInterface: orch.sessions}
	orch.sessions = rec

	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Welche Akkuschrauber haben wir?")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Hier die Liste:\n- A\n- B" {
		t.Errorf("shown reply = %q, want it without the spoken version", result.Response)
	}
	if got := result.Metadata[SpokenMetadataKey]; got != "Ich habe zwei Akkuschrauber gefunden, die Liste steht im Chat." {
		t.Errorf("result spoken = %q", got)
	}
	reply := rec.last(provider.RoleAssistant)
	if strings.Contains(reply.msg.Content, spokenOpen) || reply.meta[SpokenMetadataKey] == "" {
		t.Errorf("stored reply = %q with %v, want it without markers and with the spoken version", reply.msg.Content, reply.meta)
	}
	if got := rec.last(provider.RoleUser).meta[InputMetadataKey]; got != InputVoice {
		t.Errorf("stored user turn input = %q, want voice", got)
	}
}

func TestASpokenVersionAloneIsAlsoTheShownReply(t *testing.T) {
	orch, sessionID := setupOrchestrator(&fakeLLM{responses: []string{"<spoken>Erledigt, der Akkuschrauber ist angelegt.</spoken>"}}, DefaultParser)
	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Leg ihn an")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Erledigt, der Akkuschrauber ist angelegt." || result.Metadata[SpokenMetadataKey] != result.Response {
		t.Errorf("reply = %q, spoken = %q", result.Response, result.Metadata[SpokenMetadataKey])
	}
}

func TestSpokenTurnWithoutASpokenVersionIsMarkedAndStaysSilent(t *testing.T) {
	orch, sessionID := setupOrchestrator(&fakeLLM{responses: []string{"Hier die Liste:\n- A"}}, DefaultParser)
	rec := &metadataRecorder{SessionStoreInterface: orch.sessions}
	orch.sessions = rec

	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Liste?")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Metadata[SpokenMetadataKey]; ok {
		t.Errorf("result carries a spoken version it never had: %v", result.Metadata)
	}
	if rec.last(provider.RoleAssistant).meta[SpokenMissingMetadataKey] != "true" {
		t.Errorf("stored reply not marked spoken_missing: %v", rec.last(provider.RoleAssistant).meta)
	}
}

func TestTypedTurnIsLeftAsItWas(t *testing.T) {
	orch, sessionID := setupOrchestrator(&fakeLLM{responses: []string{"Angelegt."}}, DefaultParser)
	rec := &metadataRecorder{SessionStoreInterface: orch.sessions}
	orch.sessions = rec

	result, err := orch.Run(context.Background(), sessionID, "Leg an")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Angelegt." || len(result.Metadata) != 0 {
		t.Errorf("typed turn changed: %q %v", result.Response, result.Metadata)
	}
	if rec.last(provider.RoleUser).meta != nil || rec.last(provider.RoleAssistant).meta != nil {
		t.Errorf("typed turn stored metadata: %v / %v", rec.last(provider.RoleUser).meta, rec.last(provider.RoleAssistant).meta)
	}
}

// recordingLLM keeps the system message of every request it answers.
type recordingLLM struct {
	fakeLLM
	systems []string
}

func (r *recordingLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Messages) > 0 && req.Messages[0].Role == provider.RoleSystem {
		r.systems = append(r.systems, req.Messages[0].Content)
	}
	return r.fakeLLM.Complete(ctx, req)
}

func TestTheSpokenInstructionReachesTheModelInTheFirstRoundBehindTheCachedPrefix(t *testing.T) {
	typedLLM := &recordingLLM{fakeLLM: fakeLLM{responses: []string{"Hallo."}}}
	typed, typedSession := setupOrchestrator(typedLLM, DefaultParser)
	if _, err := typed.Run(context.Background(), typedSession, "Hallo"); err != nil {
		t.Fatal(err)
	}
	spokenLLM := &recordingLLM{fakeLLM: fakeLLM{responses: []string{"<spoken>Hallo.</spoken>Hallo."}}}
	spoken, spokenSession := setupOrchestrator(spokenLLM, DefaultParser)
	if _, err := spoken.Run(actor.WithSpokenReply(context.Background()), spokenSession, "Hallo"); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(typedLLM.systems[0], "## SPOKEN REPLY") {
		t.Error("a typed turn gets the spoken-reply instruction")
	}
	if !strings.Contains(spokenLLM.systems[0], "## SPOKEN REPLY") {
		t.Fatal("the first round of a spoken turn lacks the spoken-reply instruction")
	}
	if !strings.HasPrefix(spokenLLM.systems[0], strings.TrimRight(typedLLM.systems[0], "\n")) {
		t.Error("the spoken-reply instruction is not behind the typed turn's system prompt, so the cached prefix changes")
	}
}

func TestAToolCallingResponseHandsNoSpokenVersionOn(t *testing.T) {
	var parsed []string
	parser := &fakeParser{parseFn: func(response string) []ToolCall {
		parsed = append(parsed, response)
		if len(parsed) == 1 {
			return []ToolCall{{ID: "1", Plugin: "gitlab", Action: "analyze_code", Args: map[string]string{"repo": "myrepo"}, FromLLM: true}}
		}
		return nil
	}}
	llm := &fakeLLM{responses: []string{
		"<spoken>Ich sehe nach.</spoken>[tool_call] gitlab__analyze_code",
		"<spoken>Keine Fehler gefunden.</spoken>Keine Fehler in myrepo.",
	}}
	orch, sessionID := setupOrchestrator(llm, parser)

	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Prüf myrepo")
	if err != nil {
		t.Fatal(err)
	}
	for _, response := range parsed {
		if strings.Contains(response, spokenOpen) {
			t.Errorf("the parser saw a spoken version: %q", response)
		}
	}
	sess, _ := orch.sessions.Get(sessionID)
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, spokenOpen) || strings.Contains(m.Content, "Ich sehe nach.") {
			t.Errorf("stored %s message carries a spoken version: %q", m.Role, m.Content)
		}
	}
	if result.Response != "Keine Fehler in myrepo." || result.Metadata[SpokenMetadataKey] != "Keine Fehler gefunden." {
		t.Errorf("reply = %q, spoken = %q", result.Response, result.Metadata[SpokenMetadataKey])
	}
}

func TestAnEmptySpokenVersionIsAnEmptyResponse(t *testing.T) {
	orch, sessionID := setupOrchestrator(&fakeLLM{responses: []string{"<spoken></spoken>", "Angelegt."}}, DefaultParser)

	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Leg an")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Angelegt." || result.Metadata[SpokenMissingMetadataKey] != "true" {
		t.Errorf("reply = %q, metadata = %v; want the retried answer, marked spoken_missing", result.Response, result.Metadata)
	}
}

// lookupOrchestrator answers with llm in a session where p__lookup runs on executor.
func lookupOrchestrator(t *testing.T, llm LLMClient, parser ToolCallParser, executor *countingExecutor) (*Orchestrator, string) {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{Name: "p", Actions: []Action{{Name: "lookup", Description: "Looks up."}}}, executor); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	return NewWithRules(llm, parser, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{}), "s1"
}

func TestWhatASpokenBlockHoldsIsNeverRunAsACall(t *testing.T) {
	const call = `[tool_call]{"tool":"p__lookup","args":{}}[/tool_call]`
	for _, c := range []struct {
		name      string
		responses []string
		shown     string
	}{
		{"closed", []string{"<spoken>" + call + "</spoken>", "Fertig."}, "Fertig."},
		{"unclosed", []string{"<spoken>" + call, "Fertig."}, "Fertig."},
		{"nested", []string{"<spoken>A<spoken>B</spoken>" + call + "</spoken>Fertig.", "Fertig."}, "Fertig."},
		{"bare invoke", []string{`<spoken>Fertig. <invoke name="p__lookup"><parameter name="id">geheim</parameter></invoke></spoken>`, "Fertig."}, "Fertig."},
		{"bare JSON", []string{`<spoken>{"tool":"p__lookup","args":{}}</spoken>`, "Fertig."}, ""},
		{"unclosed bare JSON", []string{`<spoken>{"tool":"p__lookup","args":{}}`, "Fertig."}, ""},
	} {
		// A trailing answer lets a regression that runs the call end its turn.
		executor := &countingExecutor{}
		orch, sessionID := lookupOrchestrator(t, &fakeLLM{responses: c.responses}, DefaultParser, executor)
		result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Schau nach")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if executor.count != 0 || (c.shown != "" && result.Response != c.shown) || strings.Contains(result.Metadata[SpokenMetadataKey], "geheim") {
			t.Errorf("%s: calls run %d, reply %q, metadata %v; want no call", c.name, executor.count, result.Response, result.Metadata)
		}
	}
}

func TestASpokenBlockInsideAProtocolBlockIsNotRead(t *testing.T) {
	llm := &fakeLLM{responses: []string{"[plugin_output]<spoken>geheim</spoken>[/plugin_output]Erledigt."}}
	orch, sessionID := setupOrchestrator(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }})
	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Leg an")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Erledigt." || result.Metadata[SpokenMissingMetadataKey] != "true" || strings.Contains(result.Metadata[SpokenMetadataKey], "geheim") {
		t.Errorf("reply %q, metadata %v; want the block neither shown nor read", result.Response, result.Metadata)
	}
}

func TestTextWrittenWithoutMarkersIsItsOwnSpokenVersionWhenShort(t *testing.T) {
	spoken := actor.WithSpokenReply(context.Background())
	short := speakAsIs(spoken, "Soll ich den Akkuschrauber anlegen?", map[string]string{"prompt_type": "tool_confirmation"})
	if short[SpokenMetadataKey] != "Soll ich den Akkuschrauber anlegen?" || short["prompt_type"] != "tool_confirmation" {
		t.Errorf("short prompt: %v", short)
	}
	long := speakAsIs(spoken, strings.Repeat("Schritt. ", 60), nil)
	if long[SpokenMissingMetadataKey] != "true" || long[SpokenMetadataKey] != "" {
		t.Errorf("long plan: %v", long)
	}
	typed := map[string]string{"prompt_type": "tool_confirmation"}
	if got := speakAsIs(context.Background(), "Soll ich?", typed); len(got) != 1 {
		t.Errorf("a typed turn's prompt gained metadata: %v", got)
	}
}

func TestOnlyTheSentencesOfATextAreRead(t *testing.T) {
	cases := []struct{ text, read string }{
		{"Soll ich in Ihrem Konto die neue Hauptkategorie **Werkzeuge** anlegen?\n" +
			"- name: neu -> Werkzeuge (eine Kategorie mit diesem Namen existiert bisher nicht)\n" +
			"Danach würde ich die Objekte Bohrmaschine und Hammer darin erstellen. Möchten Sie die Kategorie jetzt anlegen (ja/nein)?",
			"Soll ich in Ihrem Konto die neue Hauptkategorie Werkzeuge anlegen? Danach würde ich die Objekte " +
				"Bohrmaschine und Hammer darin erstellen. Möchten Sie die Kategorie jetzt anlegen (ja/nein)?"},
		{"Soll ich [Maschine 7](/items/42) ändern?\nSeriennummer: A-1 → X-9", "Soll ich Maschine 7 ändern?"},
		{"## Ergebnis\n| Name | Anzahl |\n|---|---|\nA | 2\n```\nlist-items\n```\n---\n1. Erstens\n2. Zweitens\n* Drittens\n" +
			"> - name: Werkzeuge\n> ## Ergebnis\n> Zwei Treffer, siehe `serial_number` und _Notizen_.",
			"Zwei Treffer, siehe serial number und Notizen."},
		{"1. Januar ist der nächste freie Termin, er kostet ~10 € und dauert 2*3 Stunden.",
			"1. Januar ist der nächste freie Termin, er kostet ~10 € und dauert 2*3 Stunden."},
		{"Der Wert 5 * 3 ist **sehr** wichtig (*geschätzt*), ~~alt~~ neu.", "Der Wert 5 * 3 ist sehr wichtig (geschätzt), alt neu."},
		{"Créer «**Outils**» ? ¿**Confirmar**?", "Créer «Outils» ? ¿Confirmar?"},
		{"Hier der Befehl:\n    rake db:migrate\n\tlist-items\n```\n1. a\n2. b\n```\n1. Januar ist frei.",
			"Hier der Befehl: 1. Januar ist frei."},
		{"    rake db:migrate\n>     rake db:seed\n> > Erledigt.\n````md\n```\nlist-items\n```\n````\nFertig.\n" +
			"```\n    ```\nlist-items\n```\nGut.", "Erledigt. Fertig. Gut."},
		{"Fertig. ![Bild](/a.png) Mehr unter https://example.com/a", "Fertig. Mehr unter"},
		{"- A\n- B", ""},
	}
	for _, c := range cases {
		if got := speakable(c.text); got != c.read {
			t.Errorf("speakable(%q)\n = %q\nwant %q", c.text, got, c.read)
		}
	}
}

func TestASpokenVersionIsReadWithoutMarkdownAndATextOfOnlyAListStaysSilent(t *testing.T) {
	spokenCtx := actor.WithSpokenReply(context.Background())
	if got := spokenMetadata(spokenCtx, "Ich habe **zwei** Treffer, die Details stehen im Chat.")[SpokenMetadataKey]; got !=
		"Ich habe zwei Treffer, die Details stehen im Chat." {
		t.Errorf("spoken version read as %q", got)
	}
	if meta := speakAsIs(spokenCtx, "- A\n- B", nil); meta[SpokenMissingMetadataKey] != "true" || meta[SpokenMetadataKey] != "" {
		t.Errorf("a list alone: %v; want spoken_missing", meta)
	}
}

// raiseConfirmation asks for approval of one write in ctx with a narrator that answers narration; it returns the
// result, the narrator's system prompt and the session store.
func raiseConfirmation(t *testing.T, ctx context.Context, narration string) (*RunResult, string, *state.SessionStore, *Orchestrator) {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Actions: []Action{{Name: "create_category", Description: "Creates a category."}},
	}, &countingExecutor{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{}); err != nil {
		t.Fatalf("register conf: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	llm := &capturingLLM{responses: []string{narration}}
	orch := NewWithRules(llm, &fakeParser{}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		ConfirmationPlugin: "conf", ConfirmationAction: "check",
	})
	call := ToolCall{ID: "c1", Plugin: "p", Action: "create_category", Args: map[string]string{"name": "Werkzeuge"}, FromLLM: true}
	rr, raised := orch.maybeRequireConfirmation(ctx, sessions, "s1", call, "Lege Werkzeuge an", nil, false, nil)
	if !raised || rr == nil || len(llm.requests) != 1 {
		t.Fatalf("no confirmation raised: raised=%v rr=%v narrator calls=%d", raised, rr, len(llm.requests))
	}
	return rr, llm.requests[0].Messages[0].Content, sessions, orch
}

const shownQuestion = "Soll ich in Ihrem Konto die neue Hauptkategorie **Werkzeuge** anlegen?\n- name: neu -> Werkzeuge\n" +
	"Möchten Sie die Kategorie jetzt anlegen (ja/nein)?"

func TestASpokenConfirmationReadsTheQuestionAsTheNarratorAskedItAloud(t *testing.T) {
	aloud := "Soll ich die Hauptkategorie Werkzeuge anlegen?"
	rr, system, sessions, orch := raiseConfirmation(t, actor.WithSpokenReply(sentCtx("p__create_category")),
		spokenOpen+aloud+spokenClose+"\n"+shownQuestion)

	if !strings.Contains(system, "## SPOKEN QUESTION") {
		t.Error("the narrator of a spoken turn is not asked for the question as it is asked aloud")
	}
	if rr.Response != shownQuestion || rr.Metadata[SpokenMetadataKey] != aloud || rr.Metadata["prompt_type"] != "tool_confirmation" {
		t.Errorf("shown %q, metadata %v", rr.Response, rr.Metadata)
	}
	sess, _ := sessions.Get("s1")
	stored := sess.Messages[len(sess.Messages)-1]
	if stored.Content != shownQuestion {
		t.Errorf("stored prompt = %q, want the question without the spoken version", stored.Content)
	}
	if prompt, _, _ := orch.PendingConfirmationFrame("s1"); prompt != shownQuestion {
		t.Errorf("pending prompt = %q, want the question without the spoken version", prompt)
	}
}

func TestANarratorThatWroteOnlyTheQuestionAloudShowsItToo(t *testing.T) {
	aloud := "Soll ich die Hauptkategorie Werkzeuge anlegen?"
	rr, _, _, _ := raiseConfirmation(t, actor.WithSpokenReply(sentCtx("p__create_category")), spokenOpen+aloud+spokenClose)
	if rr.Response != aloud || rr.Metadata[SpokenMetadataKey] != aloud {
		t.Errorf("shown %q, read %q; want the question aloud as both", rr.Response, rr.Metadata[SpokenMetadataKey])
	}
}

func TestASpokenConfirmationWithoutTheQuestionAloudReadsOnlyItsSentences(t *testing.T) {
	rr, _, _, _ := raiseConfirmation(t, actor.WithSpokenReply(sentCtx("p__create_category")), shownQuestion)
	want := "Soll ich in Ihrem Konto die neue Hauptkategorie Werkzeuge anlegen? Möchten Sie die Kategorie jetzt anlegen (ja/nein)?"
	if rr.Response != shownQuestion || rr.Metadata[SpokenMetadataKey] != want {
		t.Errorf("shown %q, read %q", rr.Response, rr.Metadata[SpokenMetadataKey])
	}
}

func TestAConfirmationComesBackAfterAReconnectAsSpokenAsItWasAsked(t *testing.T) {
	for _, spoken := range []bool{true, false} {
		ctx := sentCtx("p__create_category")
		if spoken {
			ctx = actor.WithSpokenReply(ctx)
		}
		_, _, _, orch := raiseConfirmation(t, ctx, shownQuestion)
		_, meta, ok := orch.PendingConfirmationFrame("s1")
		if !ok || (meta[ReplyMetadataKey] == ReplySpoken) != spoken {
			t.Errorf("asked in a spoken turn %v: re-emitted with %v", spoken, meta)
		}
	}
}

func TestATypedConfirmationIsNarratedAsBefore(t *testing.T) {
	rr, system, _, _ := raiseConfirmation(t, sentCtx("p__create_category"), shownQuestion)
	if strings.Contains(system, "## SPOKEN QUESTION") {
		t.Error("the narrator of a typed turn is asked for a spoken question")
	}
	if _, ok := rr.Metadata[SpokenMetadataKey]; ok || rr.Response != shownQuestion {
		t.Errorf("typed confirmation changed: %q %v", rr.Response, rr.Metadata)
	}
}
