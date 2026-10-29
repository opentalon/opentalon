package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
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
	cases := []struct{ reply, spoken, rest string }{
		{"<spoken>Zwei gefunden.</spoken>\nHier die Liste:\n- A", "Zwei gefunden.", "Hier die Liste:\n- A"},
		{"  <spoken> Fertig. </spoken>  Angelegt.", "Fertig.", "Angelegt."},
		{"Angelegt.", "", "Angelegt."},
		{"<spoken>nie geschlossen Angelegt.", "", "nie geschlossen Angelegt."},
		{"<spoken></spoken>Angelegt.", "", "Angelegt."},
	}
	for _, c := range cases {
		spoken, rest, found := splitSpoken(c.reply)
		if spoken != c.spoken || rest != c.rest || found != (c.spoken != "") {
			t.Errorf("splitSpoken(%q) = %q, %q, %v; want %q, %q", c.reply, spoken, rest, found, c.spoken, c.rest)
		}
	}
	repeated := "<spoken>Erstens.</spoken><spoken>Zweitens.</spoken>Antwort.</spoken>"
	if spoken, rest, _ := splitSpoken(repeated); spoken != "Erstens." || rest != "Antwort." {
		t.Errorf("repeated blocks: %q, %q; want the first spoken and every marker gone", spoken, rest)
	}
	for _, nested := range []string{"<spoken>A<spoken>B</spoken>C", "<spoken>A<spoken>B"} {
		spoken, rest, _ := splitSpoken(nested)
		if strings.Contains(spoken+rest, "<spoken") || strings.Contains(spoken+rest, "</spoken") {
			t.Errorf("nested %q left a marker: %q, %q", nested, spoken, rest)
		}
	}
}

func TestALongSpokenOnlyReplyStaysWholeAndIsNotRead(t *testing.T) {
	long := strings.Repeat("Das ist ein langer Satz. ", 30)
	spokenCtx := actor.WithSpokenReply(context.Background())
	spoken, rest := takeSpoken(spokenCtx, "<spoken>"+long+"</spoken>")
	if rest != strings.TrimSpace(long) {
		t.Errorf("the shown text lost words: %d of %d characters", len(rest), len(strings.TrimSpace(long)))
	}
	if meta := spokenMetadata(spokenCtx, spoken); meta[SpokenMissingMetadataKey] != "true" {
		t.Errorf("an over-long spoken version is read: %v", meta)
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

	if short.Metadata[SpokenMetadataKey] != "Abgebrochen." || long.Metadata[SpokenMissingMetadataKey] != "true" ||
		kept.Metadata[SpokenMetadataKey] != "Zwei gefunden." || typed.Metadata != nil {
		t.Errorf("short %v, long %v, kept %v, typed %v", short.Metadata, long.Metadata, kept.Metadata, typed.Metadata)
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
			return []ToolCall{{ID: "1", Plugin: "gitlab", Action: "analyze_code", Args: map[string]string{"repo": "timly"}, FromLLM: true}}
		}
		return nil
	}}
	llm := &fakeLLM{responses: []string{
		"<spoken>Ich sehe nach.</spoken>[tool_call] gitlab__analyze_code",
		"<spoken>Keine Fehler gefunden.</spoken>Keine Fehler in timly.",
	}}
	orch, sessionID := setupOrchestrator(llm, parser)

	result, err := orch.Run(actor.WithSpokenReply(context.Background()), sessionID, "Prüf timly")
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
	if result.Response != "Keine Fehler in timly." || result.Metadata[SpokenMetadataKey] != "Keine Fehler gefunden." {
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
