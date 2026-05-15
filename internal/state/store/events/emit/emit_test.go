package emit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

// fakeSink records every emitted event in arrival order. Concurrent
// safe so tests can fan out across goroutines if needed later.
type fakeSink struct {
	mu     sync.Mutex
	events []Event
}

func (f *fakeSink) Emit(_ context.Context, e Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeSink) snapshot() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, len(f.events))
	copy(out, f.events)
	return out
}

// ----- foundation -----

func TestNoOpSinkNeverPanics(t *testing.T) {
	// Belt-and-braces: the NoOpSink is the documented fallback for code
	// paths with no state DB; it must accept any well-formed event
	// without observable side effects.
	NoOpSink{}.Emit(context.Background(), Event{
		SessionID: "sess",
		EventType: events.TypeTurnStart,
		Payload:   json.RawMessage(`{}`),
	})
}

func TestNilSinkIsSilentNoop(t *testing.T) {
	// send() must short-circuit on nil so producers don't have to
	// nil-check at every emission site. Treat this as a contract test.
	EmitUserMessage(context.Background(), nil, "hi") // must not panic
}

func TestSendPopulatesContextFields(t *testing.T) {
	sink := &fakeSink{}
	ctx := actor.WithSessionID(context.Background(), "sess-abc")
	ctx = WithParent(ctx, "parent-evt-1")

	EmitUserMessage(ctx, sink, "hello")

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q (must be sourced from actor.SessionID)", got[0].SessionID, "sess-abc")
	}
	if got[0].ParentID != "parent-evt-1" {
		t.Errorf("ParentID = %q, want %q (must be sourced from emit.ParentID)", got[0].ParentID, "parent-evt-1")
	}
	if got[0].EventType != events.TypeUserMessage {
		t.Errorf("EventType = %q, want %q", got[0].EventType, events.TypeUserMessage)
	}
}

func TestWithParentEmptyIsNoop(t *testing.T) {
	base := context.Background()
	if WithParent(base, "") != base {
		t.Error("WithParent with empty parentID must return the original ctx unchanged")
	}
}

func TestParentIDNilCtxIsEmpty(t *testing.T) {
	//nolint:staticcheck // intentional nil-ctx defensive path
	if got := ParentID(nil); got != "" {
		t.Errorf("ParentID(nil) = %q, want empty", got)
	}
}

// ----- turn & user_message -----

func TestEmitTurnStart_PayloadShape(t *testing.T) {
	sink := &fakeSink{}
	ctx := actor.WithSessionID(context.Background(), "s1")
	temp := 0.7
	EmitTurnStart(ctx, sink, TurnStartArgs{
		SystemPromptSHA256: "abc123",
		ServerInstructions: []events.ServerInstructionRef{{Name: "instr", SHA256: "def456"}},
		AvailableTools:     []events.ToolRef{{Name: "tickets.show", DescSHA256: "ghi789"}},
		ModelID:            "gpt-x",
		Temperature:        &temp,
		ReasoningEffort:    "medium",
	})

	got := sink.snapshot()
	if len(got) != 1 || got[0].EventType != events.TypeTurnStart {
		t.Fatalf("unexpected events: %+v", got)
	}
	var p events.TurnStartPayload
	if err := json.Unmarshal(got[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.V != events.TurnStartVersion {
		t.Errorf("Header.V = %d, want %d", p.V, events.TurnStartVersion)
	}
	if p.SystemPromptSHA256 != "abc123" || p.ModelID != "gpt-x" || p.ReasoningEffort != "medium" {
		t.Errorf("scalar fields mismatch: %+v", p)
	}
	if p.Temperature == nil || *p.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", p.Temperature)
	}
	if len(p.AvailableTools) != 1 || p.AvailableTools[0].Name != "tickets.show" {
		t.Errorf("AvailableTools mismatch: %+v", p.AvailableTools)
	}
}

func TestEmitUserMessage_ContentLengthMatchesStoredContent(t *testing.T) {
	// The contract: ContentLength matches the BYTES of the stored Content
	// field (post-sanitization), not the input. ASCII inputs are
	// invariant under SanitizeUTF8 so the simple case still passes — but
	// drift in the helper (e.g. computing length of the pre-sanitize
	// input) would silently produce inconsistent rows on non-UTF-8 input.
	sink := &fakeSink{}
	content := "hello, world"
	EmitUserMessage(context.Background(), sink, content)

	got := sink.snapshot()
	var p events.UserMessagePayload
	if err := json.Unmarshal(got[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Content != content {
		t.Errorf("Content = %q, want %q", p.Content, content)
	}
	if p.ContentLength != len(p.Content) {
		t.Errorf("ContentLength = %d, want %d (must match stored Content byte length)", p.ContentLength, len(p.Content))
	}
}

// ----- llm cluster (raw-capture invariants) -----

// TestEmitLLMResponse_CostFieldsPassThrough verifies the cost fields on
// LLMResponseArgs land on LLMResponsePayload unchanged. The helper does
// no math of its own — pricing is the caller's responsibility (the
// provider wrapper) so historical events stay frozen at their call-time
// rates even after the model catalogue is reconfigured.
func TestEmitLLMResponse_CostFieldsPassThrough(t *testing.T) {
	sink := &fakeSink{}
	EmitLLMResponse(context.Background(), sink, LLMResponseArgs{
		RawContent: "ok",
		TokensIn:   1000,
		TokensOut:  500,
		CostInput:  0.0025,
		CostOutput: 0.005,
	})

	var p events.LLMResponsePayload
	if err := json.Unmarshal(sink.snapshot()[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.CostInput != 0.0025 {
		t.Errorf("CostInput = %v, want 0.0025", p.CostInput)
	}
	if p.CostOutput != 0.005 {
		t.Errorf("CostOutput = %v, want 0.005", p.CostOutput)
	}
}

// TestEmitLLMResponse_ZeroCostOmitted verifies that a zero-cost emit (the
// common case for unconfigured models) leaves the cost fields out of the
// JSON entirely — so consumers can distinguish "unpriced" (absent) from
// "priced at zero" (present and 0) without ambiguity.
func TestEmitLLMResponse_ZeroCostOmitted(t *testing.T) {
	sink := &fakeSink{}
	EmitLLMResponse(context.Background(), sink, LLMResponseArgs{
		RawContent: "ok",
		TokensIn:   10,
		TokensOut:  5,
	})

	raw := string(sink.snapshot()[0].Payload)
	if strings.Contains(raw, `"cost_input"`) || strings.Contains(raw, `"cost_output"`) {
		t.Errorf("zero cost must be omitted from payload, got: %s", raw)
	}
}

func TestEmitLLMResponse_ExcerptAndSHA256(t *testing.T) {
	sink := &fakeSink{}
	long := strings.Repeat("A", events.ExcerptCap+128) // overshoot the cap
	EmitLLMResponse(context.Background(), sink, LLMResponseArgs{
		RawContent:         long,
		NativeToolCallsRaw: json.RawMessage(`[{"id":"c1","name":"ticket.show","arguments":{"id":"42"}}]`),
		FinishReason:       "stop",
		LatencyMS:          1234,
	})

	got := sink.snapshot()
	var p events.LLMResponsePayload
	if err := json.Unmarshal(got[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !p.RawContentTruncated {
		t.Error("truncated flag must be true when content exceeds ExcerptCap")
	}
	if len(p.RawContentExcerpt) != events.ExcerptCap {
		t.Errorf("excerpt len = %d, want %d", len(p.RawContentExcerpt), events.ExcerptCap)
	}
	if p.RawContentSHA256 == "" {
		t.Error("SHA256 must be populated by helper; callers must not compute")
	}
	// Inline embedding (json.RawMessage), not escaped-string form.
	if !json.Valid(p.NativeToolCallsRaw) {
		t.Errorf("NativeToolCallsRaw = %q, want valid inline JSON", string(p.NativeToolCallsRaw))
	}
	if p.LatencyMS != 1234 {
		t.Errorf("LatencyMS = %d, want 1234", p.LatencyMS)
	}
	if got[0].DurationMS != 1234 {
		t.Errorf("row DurationMS = %d, want 1234 (row column must mirror payload latency)", got[0].DurationMS)
	}
	if p.V != events.LLMResponseVersion {
		t.Errorf("Header.V = %d, want %d", p.V, events.LLMResponseVersion)
	}
}

func TestEmitLLMError_BodyExcerpted(t *testing.T) {
	sink := &fakeSink{}
	body := strings.Repeat("E", events.ExcerptCap+16)
	EmitLLMError(context.Background(), sink, LLMErrorArgs{
		Phase: "chat", StatusCode: 503, ResponseBodyText: body,
	})
	var p events.LLMErrorPayload
	_ = json.Unmarshal(sink.snapshot()[0].Payload, &p)
	if !p.ResponseBodyTruncated {
		t.Error("ResponseBodyTruncated must be set for over-cap body")
	}
	if len(p.ResponseBodyExcerpt) != events.ExcerptCap {
		t.Errorf("excerpt len = %d, want %d", len(p.ResponseBodyExcerpt), events.ExcerptCap)
	}
}

func TestEmitLLMRefused_NotExcerptCapped(t *testing.T) {
	// Refusals are deliberately not excerpt-capped per the payload doc —
	// they are short by construction and we want the full refusal text
	// for moderation analysis.
	sink := &fakeSink{}
	long := strings.Repeat("R", events.ExcerptCap+16)
	EmitLLMRefused(context.Background(), sink, LLMRefusedArgs{
		RefusalText: long, ContentSafetyHit: "hate",
	})
	var p events.LLMRefusedPayload
	_ = json.Unmarshal(sink.snapshot()[0].Payload, &p)
	if len(p.RefusalText) != events.ExcerptCap+16 {
		t.Errorf("RefusalText length changed (helper should NOT excerpt refusals): got %d, want %d",
			len(p.RefusalText), events.ExcerptCap+16)
	}
}

// ----- tool cluster (parent linkage) -----

func TestEmitToolCallExtractedThenResult_ParentLinkage(t *testing.T) {
	sink := &fakeSink{}
	ctx := actor.WithSessionID(context.Background(), "sess")

	EmitToolCallExtracted(ctx, sink, ToolCallExtractedArgs{
		CallID: "c1", Plugin: "tickets", Action: "show",
		Arguments: map[string]string{"id": "42"}, Mode: ToolCallModeNative,
	})

	// The dispatcher would capture the extracted-event id and stamp it
	// onto the child span via WithParent. Simulate that linkage.
	extractedEvtID := "evt-extracted-1"
	childCtx := WithParent(ctx, extractedEvtID)
	EmitToolCallResult(childCtx, sink, ToolCallResultArgs{
		CallID: "c1", Status: "ok", Response: `{"id":42}`, LatencyMS: 12,
	})

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].ParentID != "" {
		t.Errorf("extracted ParentID = %q, want empty (root of span)", got[0].ParentID)
	}
	if got[1].ParentID != extractedEvtID {
		t.Errorf("result ParentID = %q, want %q", got[1].ParentID, extractedEvtID)
	}

	var resPayload events.ToolCallResultPayload
	_ = json.Unmarshal(got[1].Payload, &resPayload)
	if resPayload.LatencyMS != 12 || got[1].DurationMS != 12 {
		t.Errorf("latency/duration mismatch — payload=%d row=%d", resPayload.LatencyMS, got[1].DurationMS)
	}
}

func TestEmitToolCallNotFound_Minimal(t *testing.T) {
	sink := &fakeSink{}
	EmitToolCallNotFound(context.Background(), sink, "nonexistent.action")
	var p events.ToolCallNotFoundPayload
	_ = json.Unmarshal(sink.snapshot()[0].Payload, &p)
	if p.RequestedName != "nonexistent.action" {
		t.Errorf("RequestedName = %q", p.RequestedName)
	}
}

// ----- header.V coverage for every helper -----

// Asserts the Header.V stamp for every Emit<Type> is the matching
// <Type>Version constant. Catches a class of bug where a helper is
// copy-pasted from another event type and the version field silently
// stays wrong (consumers downstream depend on v to switch decoder).
func TestAllEmitHelpers_StampMatchingVersion(t *testing.T) {
	temp := 0.0
	cases := []struct {
		name     string
		emit     func(ctx context.Context, s Sink)
		wantType string
		wantVer  int
	}{
		{"TurnStart", func(c context.Context, s Sink) { EmitTurnStart(c, s, TurnStartArgs{ModelID: "m", Temperature: &temp}) }, events.TypeTurnStart, events.TurnStartVersion},
		{"UserMessage", func(c context.Context, s Sink) { EmitUserMessage(c, s, "u") }, events.TypeUserMessage, events.UserMessageVersion},
		{"LLMRequest", func(c context.Context, s Sink) { EmitLLMRequest(c, s, LLMRequestArgs{ModelID: "m"}) }, events.TypeLLMRequest, events.LLMRequestVersion},
		{"LLMResponse", func(c context.Context, s Sink) { EmitLLMResponse(c, s, LLMResponseArgs{RawContent: "r"}) }, events.TypeLLMResponse, events.LLMResponseVersion},
		{"LLMError", func(c context.Context, s Sink) { EmitLLMError(c, s, LLMErrorArgs{Phase: "p"}) }, events.TypeLLMError, events.LLMErrorVersion},
		{"LLMRefused", func(c context.Context, s Sink) { EmitLLMRefused(c, s, LLMRefusedArgs{RefusalText: "r"}) }, events.TypeLLMRefused, events.LLMRefusedVersion},
		{"PlannerInvoked", func(c context.Context, s Sink) { EmitPlannerInvoked(c, s, "r") }, events.TypePlannerInvoked, events.PlannerInvokedVersion},
		{"PlannerRequest", func(c context.Context, s Sink) { EmitPlannerRequest(c, s, PlannerRequestArgs{}) }, events.TypePlannerRequest, events.PlannerRequestVersion},
		{"PlannerResponse", func(c context.Context, s Sink) { EmitPlannerResponse(c, s, PlannerResponseArgs{}) }, events.TypePlannerResponse, events.PlannerResponseVersion},
		{"PlannerStep", func(c context.Context, s Sink) { EmitPlannerStep(c, s, PlannerStepArgs{}) }, events.TypePlannerStep, events.PlannerStepVersion},
		{"ToolRetrieval", func(c context.Context, s Sink) { EmitToolRetrieval(c, s, ToolRetrievalArgs{}) }, events.TypeToolRetrieval, events.ToolRetrievalVersion},
		{"SummarizationTriggered", func(c context.Context, s Sink) { EmitSummarizationTriggered(c, s, SummarizationTriggeredArgs{}) }, events.TypeSummarizationTriggered, events.SummarizationTriggeredVersion},
		{"SummarizationCompleted", func(c context.Context, s Sink) { EmitSummarizationCompleted(c, s, SummarizationCompletedArgs{}) }, events.TypeSummarizationCompleted, events.SummarizationCompletedVersion},
		{"ModelSwitch", func(c context.Context, s Sink) { EmitModelSwitch(c, s, ModelSwitchArgs{}) }, events.TypeModelSwitch, events.ModelSwitchVersion},
		{"ConfirmationRequested", func(c context.Context, s Sink) { EmitConfirmationRequested(c, s, ConfirmationRequestedArgs{}) }, events.TypeConfirmationRequested, events.ConfirmationRequestedVersion},
		{"ConfirmationResolved", func(c context.Context, s Sink) { EmitConfirmationResolved(c, s, ConfirmationResolvedArgs{}) }, events.TypeConfirmationResolved, events.ConfirmationResolvedVersion},
		{"Retry", func(c context.Context, s Sink) { EmitRetry(c, s, RetryArgs{Phase: "p"}) }, events.TypeRetry, events.RetryVersion},
		{"ToolCallExtracted", func(c context.Context, s Sink) {
			EmitToolCallExtracted(c, s, ToolCallExtractedArgs{CallID: "c", Mode: ToolCallModeNative})
		}, events.TypeToolCallExtracted, events.ToolCallExtractedVersion},
		{"ToolCallResult", func(c context.Context, s Sink) { EmitToolCallResult(c, s, ToolCallResultArgs{CallID: "c"}) }, events.TypeToolCallResult, events.ToolCallResultVersion},
		{"ToolCallParseFailed", func(c context.Context, s Sink) { EmitToolCallParseFailed(c, s, ToolCallParseFailedArgs{}) }, events.TypeToolCallParseFailed, events.ToolCallParseFailedVersion},
		{"ToolCallArgsInvalid", func(c context.Context, s Sink) { EmitToolCallArgsInvalid(c, s, ToolCallArgsInvalidArgs{}) }, events.TypeToolCallArgsInvalid, events.ToolCallArgsInvalidVersion},
		{"ToolCallNotFound", func(c context.Context, s Sink) { EmitToolCallNotFound(c, s, "x") }, events.TypeToolCallNotFound, events.ToolCallNotFoundVersion},
		{"ScoreComputed", func(c context.Context, s Sink) { EmitScoreComputed(c, s, ScoreComputedArgs{}) }, events.TypeScoreComputed, events.ScoreComputedVersion},
		{"Error", func(c context.Context, s Sink) { EmitError(c, s, "where", "msg") }, events.TypeError, events.ErrorVersion},
	}

	// All 24 event types must be exercised — keep this in lockstep with
	// the constants in event_types.go. If you add a new event type and
	// this count drops below it, add a row above.
	const wantCases = 24
	if len(cases) != wantCases {
		t.Fatalf("len(cases) = %d, want %d — keep TestAllEmitHelpers in sync with event_types.go", len(cases), wantCases)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			tc.emit(context.Background(), sink)
			got := sink.snapshot()
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			if got[0].EventType != tc.wantType {
				t.Errorf("EventType = %q, want %q", got[0].EventType, tc.wantType)
			}
			// Decode just the Header field to verify the version stamp.
			var hdr struct {
				V int `json:"v"`
			}
			if err := json.Unmarshal(got[0].Payload, &hdr); err != nil {
				t.Fatalf("unmarshal header: %v", err)
			}
			if hdr.V != tc.wantVer {
				t.Errorf("payload.v = %d, want %d", hdr.V, tc.wantVer)
			}
		})
	}
}

// ----- mini-turn integration sequence -----

func TestMiniTurn_EmitsCanonicalSequence(t *testing.T) {
	// Walks through a representative orchestrator turn and asserts the
	// event sequence shape that consumers (api-plugin, score worker,
	// Rails UI) will rely on.
	sink := &fakeSink{}
	ctx := actor.WithSessionID(context.Background(), "sess-mini")

	EmitTurnStart(ctx, sink, TurnStartArgs{ModelID: "gpt-x"})
	EmitUserMessage(ctx, sink, "show me ticket 42")
	EmitLLMRequest(ctx, sink, LLMRequestArgs{ModelID: "gpt-x", MessageCount: 2, HasTools: true})
	EmitLLMResponse(ctx, sink, LLMResponseArgs{
		RawContent: "calling tickets.show",
		NativeToolCallsRaw: json.RawMessage(
			`[{"id":"c1","name":"tickets.show","arguments":{"id":"42"}}]`),
		FinishReason: "tool_calls", LatencyMS: 850,
	})
	EmitToolCallExtracted(ctx, sink, ToolCallExtractedArgs{
		CallID: "c1", Plugin: "tickets", Action: "show",
		Arguments: map[string]string{"id": "42"}, Mode: ToolCallModeNative,
	})
	dispatchCtx := WithParent(ctx, "evt-tool-extracted")
	EmitToolCallResult(dispatchCtx, sink, ToolCallResultArgs{
		CallID: "c1", Status: "ok", Response: `{"id":42,"status":"open"}`, LatencyMS: 67,
	})

	got := sink.snapshot()
	wantTypes := []string{
		events.TypeTurnStart,
		events.TypeUserMessage,
		events.TypeLLMRequest,
		events.TypeLLMResponse,
		events.TypeToolCallExtracted,
		events.TypeToolCallResult,
	}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d events, want %d", len(got), len(wantTypes))
	}
	for i, want := range wantTypes {
		if got[i].EventType != want {
			t.Errorf("event[%d].Type = %q, want %q", i, got[i].EventType, want)
		}
		if got[i].SessionID != "sess-mini" {
			t.Errorf("event[%d].SessionID = %q, want %q", i, got[i].SessionID, "sess-mini")
		}
	}
	// Last event is the tool result — must carry the parent linkage we
	// stamped above. Everything else is root.
	for i := 0; i < len(got)-1; i++ {
		if got[i].ParentID != "" {
			t.Errorf("event[%d] (%s) ParentID = %q, want empty", i, got[i].EventType, got[i].ParentID)
		}
	}
	if got[len(got)-1].ParentID != "evt-tool-extracted" {
		t.Errorf("tool_call_result ParentID = %q, want %q", got[len(got)-1].ParentID, "evt-tool-extracted")
	}
}

// ----- raw-capture invariants (invalid-UTF-8 input) -----

// invalidUTF8 carries two raw bytes (0xff, 0xfe) that are not valid
// UTF-8 lead bytes. Postgres' UTF-8 column refuses these bytes verbatim,
// so the helper must sanitize them to U+FFFD (UTF-8: 0xEF 0xBF 0xBD)
// before the payload reaches the writer. Producers in the wild ARE the
// realistic source (LLM streaming chunks truncated mid-multibyte, tool
// outputs from non-UTF-8 sources) so this isn't a theoretical concern.
const invalidUTF8Raw = "ok\xff\xfetail"
const invalidUTF8Sanitized = "ok��tail"

// captures the free-text field that each sanitization-bearing helper
// commits to scrubbing. If a future change skips SanitizeUTF8 for any
// of these, the test fails with a clear pointer to which helper drifted.
func TestEmit_SanitizesAllFreeTextFields(t *testing.T) {
	cases := []struct {
		name    string
		emit    func(ctx context.Context, s Sink)
		extract func(t *testing.T, payload []byte) string
	}{
		{
			"UserMessage.Content",
			func(c context.Context, s Sink) { EmitUserMessage(c, s, invalidUTF8Raw) },
			func(t *testing.T, p []byte) string {
				var v events.UserMessagePayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.Content
			},
		},
		{
			"LLMResponse.RawContentExcerpt",
			func(c context.Context, s Sink) {
				EmitLLMResponse(c, s, LLMResponseArgs{RawContent: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.LLMResponsePayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.RawContentExcerpt
			},
		},
		{
			"LLMError.ResponseBodyExcerpt",
			func(c context.Context, s Sink) {
				EmitLLMError(c, s, LLMErrorArgs{Phase: "p", ResponseBodyText: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.LLMErrorPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.ResponseBodyExcerpt
			},
		},
		{
			"LLMRefused.RefusalText",
			func(c context.Context, s Sink) {
				EmitLLMRefused(c, s, LLMRefusedArgs{RefusalText: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.LLMRefusedPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.RefusalText
			},
		},
		{
			"ToolCallResult.ResponseExcerpt",
			func(c context.Context, s Sink) {
				EmitToolCallResult(c, s, ToolCallResultArgs{CallID: "c", Response: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.ToolCallResultPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.ResponseExcerpt
			},
		},
		{
			"ToolCallParseFailed.RawSnippet",
			func(c context.Context, s Sink) {
				EmitToolCallParseFailed(c, s, ToolCallParseFailedArgs{RawSnippet: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.ToolCallParseFailedPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.RawSnippet
			},
		},
		{
			"PlannerResponse.RawContentExcerpt",
			func(c context.Context, s Sink) {
				EmitPlannerResponse(c, s, PlannerResponseArgs{RawContent: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.PlannerResponsePayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.RawContentExcerpt
			},
		},
		{
			"ToolRetrieval.Query",
			func(c context.Context, s Sink) {
				EmitToolRetrieval(c, s, ToolRetrievalArgs{Query: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.ToolRetrievalPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.Query
			},
		},
		{
			"SummarizationCompleted.SummaryExcerpt",
			func(c context.Context, s Sink) {
				EmitSummarizationCompleted(c, s, SummarizationCompletedArgs{Summary: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.SummarizationCompletedPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.SummaryExcerpt
			},
		},
		{
			"ConfirmationRequested.Prompt",
			func(c context.Context, s Sink) {
				EmitConfirmationRequested(c, s, ConfirmationRequestedArgs{Prompt: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.ConfirmationRequestedPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.Prompt
			},
		},
		{
			"Retry.LastError",
			func(c context.Context, s Sink) {
				EmitRetry(c, s, RetryArgs{Phase: "p", LastError: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.RetryPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.LastError
			},
		},
		{
			"ScoreComputed.Reasoning",
			func(c context.Context, s Sink) {
				EmitScoreComputed(c, s, ScoreComputedArgs{Reasoning: invalidUTF8Raw})
			},
			func(t *testing.T, p []byte) string {
				var v events.ScoreComputedPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.Reasoning
			},
		},
		{
			"Error.Message",
			func(c context.Context, s Sink) { EmitError(c, s, "where", invalidUTF8Raw) },
			func(t *testing.T, p []byte) string {
				var v events.ErrorPayload
				if err := json.Unmarshal(p, &v); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return v.Message
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			tc.emit(context.Background(), sink)
			got := tc.extract(t, sink.snapshot()[0].Payload)
			if got != invalidUTF8Sanitized {
				t.Errorf("field = %q, want %q (raw bytes 0xff/0xfe must become U+FFFD via SanitizeUTF8 before storage)",
					got, invalidUTF8Sanitized)
			}
		})
	}
}

// ----- excerpt + truncated flag parity -----

// Helpers that excerpt MUST set the truncated flag on overflow. Symmetric
// to TestEmitLLMResponse_ExcerptAndSHA256 / TestEmitLLMError_BodyExcerpted
// but covering every helper that takes a sanitized + excerpted free-text
// field — drift on any one would mean a forensic field silently loses its
// "this was truncated" signal.
func TestEmit_ExcerptAndTruncatedFlag_AllHelpers(t *testing.T) {
	long := strings.Repeat("X", events.ExcerptCap+128)

	cases := []struct {
		name           string
		emit           func(ctx context.Context, s Sink)
		excerpt        func(payload []byte) (string, bool)
		expectTruncate bool
	}{
		{
			"ToolCallResult.Response",
			func(c context.Context, s Sink) {
				EmitToolCallResult(c, s, ToolCallResultArgs{CallID: "c", Response: long})
			},
			func(p []byte) (string, bool) {
				var v events.ToolCallResultPayload
				_ = json.Unmarshal(p, &v)
				return v.ResponseExcerpt, v.ResponseTruncated
			},
			true,
		},
		{
			"PlannerResponse.RawContent",
			func(c context.Context, s Sink) {
				EmitPlannerResponse(c, s, PlannerResponseArgs{RawContent: long})
			},
			func(p []byte) (string, bool) {
				var v events.PlannerResponsePayload
				_ = json.Unmarshal(p, &v)
				return v.RawContentExcerpt, v.RawContentTruncated
			},
			true,
		},
		{
			"SummarizationCompleted.Summary",
			func(c context.Context, s Sink) {
				EmitSummarizationCompleted(c, s, SummarizationCompletedArgs{Summary: long})
			},
			func(p []byte) (string, bool) {
				var v events.SummarizationCompletedPayload
				_ = json.Unmarshal(p, &v)
				return v.SummaryExcerpt, v.SummaryTruncated
			},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			tc.emit(context.Background(), sink)
			excerpt, truncated := tc.excerpt(sink.snapshot()[0].Payload)
			if len(excerpt) != events.ExcerptCap {
				t.Errorf("excerpt len = %d, want %d", len(excerpt), events.ExcerptCap)
			}
			if truncated != tc.expectTruncate {
				t.Errorf("truncated = %v, want %v", truncated, tc.expectTruncate)
			}
		})
	}

	// ToolCallParseFailed is special-cased — the helper discards the
	// truncated flag because parse failures are short by construction.
	// We still verify the excerpt is capped so an outlier giant snippet
	// can't blow up the row size.
	t.Run("ToolCallParseFailed.RawSnippet_capped_no_truncate_flag", func(t *testing.T) {
		sink := &fakeSink{}
		EmitToolCallParseFailed(context.Background(), sink, ToolCallParseFailedArgs{RawSnippet: long})
		var p events.ToolCallParseFailedPayload
		_ = json.Unmarshal(sink.snapshot()[0].Payload, &p)
		if len(p.RawSnippet) != events.ExcerptCap {
			t.Errorf("RawSnippet len = %d, want %d (cap must still apply even though the truncated flag is dropped)",
				len(p.RawSnippet), events.ExcerptCap)
		}
	})
}

// ----- SHA256 invariant: digest over SANITIZED full content -----

// The contract: RawContentSHA256 = sha256(SanitizeUTF8(RawContent)).
// Not over the raw input (Postgres can't store invalid UTF-8), and not
// over the excerpt (truncation would make the digest forensically
// useless). The mid-tier test below pins this contract so a future
// "simplification" that switches to one of the other two inputs fails
// the build.
func TestEmitLLMResponse_SHA256_OverSanitizedFullContent(t *testing.T) {
	sink := &fakeSink{}
	EmitLLMResponse(context.Background(), sink, LLMResponseArgs{RawContent: invalidUTF8Raw})

	var p events.LLMResponsePayload
	if err := json.Unmarshal(sink.snapshot()[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantSum := sha256.Sum256([]byte(invalidUTF8Sanitized))
	wantHex := hex.EncodeToString(wantSum[:])
	if p.RawContentSHA256 != wantHex {
		t.Errorf("RawContentSHA256 = %q, want %q (digest must be over sanitized full content, not raw input nor excerpt)",
			p.RawContentSHA256, wantHex)
	}
}

// ----- DurationMS row-vs-payload mirroring (remaining latency helpers) -----

// Symmetric to the LLMResponse / ToolCallResult mirror checks already in
// place. If a future change forgets to pass args.LatencyMS as the
// durationMS arg to send(), row-level analytics on these event types
// silently lose the duration column.
func TestEmit_RowDurationMirrorsPayloadLatency(t *testing.T) {
	cases := []struct {
		name    string
		emit    func(ctx context.Context, s Sink)
		latency int64
		extract func(payload []byte) int64
	}{
		{
			"PlannerResponse",
			func(c context.Context, s Sink) {
				EmitPlannerResponse(c, s, PlannerResponseArgs{RawContent: "r", LatencyMS: 432})
			},
			432,
			func(p []byte) int64 {
				var v events.PlannerResponsePayload
				_ = json.Unmarshal(p, &v)
				return v.LatencyMS
			},
		},
		{
			"SummarizationCompleted",
			func(c context.Context, s Sink) {
				EmitSummarizationCompleted(c, s, SummarizationCompletedArgs{Summary: "s", LatencyMS: 789})
			},
			789,
			func(p []byte) int64 {
				var v events.SummarizationCompletedPayload
				_ = json.Unmarshal(p, &v)
				return v.LatencyMS
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			tc.emit(context.Background(), sink)
			got := sink.snapshot()[0]
			if got.DurationMS != tc.latency {
				t.Errorf("row DurationMS = %d, want %d", got.DurationMS, tc.latency)
			}
			if pl := tc.extract(got.Payload); pl != tc.latency {
				t.Errorf("payload LatencyMS = %d, want %d", pl, tc.latency)
			}
		})
	}
}

// ----- distinctive-field check (helper-body cross-contamination) -----

// TestAllEmitHelpers_StampMatchingVersion catches the case where a
// helper's Header.V drifts to a wrong constant — but it would NOT catch
// the case where a helper's body got swapped wholesale with another's
// (different event_type stamp, same V constant by coincidence) IF only
// the Header.V invariant is asserted. This test asserts at least one
// distinctive payload field per helper, so cross-contamination shows up
// as an unmarshalled field that doesn't equal the value we just sent in.
func TestAllEmitHelpers_PopulateDistinctiveField(t *testing.T) {
	cases := []struct {
		name   string
		emit   func(ctx context.Context, s Sink)
		assert func(t *testing.T, payload []byte)
	}{
		{
			"TurnStart.ModelID",
			func(c context.Context, s Sink) { EmitTurnStart(c, s, TurnStartArgs{ModelID: "distinctive-model"}) },
			func(t *testing.T, p []byte) {
				var v events.TurnStartPayload
				_ = json.Unmarshal(p, &v)
				if v.ModelID != "distinctive-model" {
					t.Errorf("ModelID = %q", v.ModelID)
				}
			},
		},
		{
			"LLMRequest.MessageCount",
			func(c context.Context, s Sink) { EmitLLMRequest(c, s, LLMRequestArgs{MessageCount: 17}) },
			func(t *testing.T, p []byte) {
				var v events.LLMRequestPayload
				_ = json.Unmarshal(p, &v)
				if v.MessageCount != 17 {
					t.Errorf("MessageCount = %d", v.MessageCount)
				}
			},
		},
		{
			"LLMError.StatusCode",
			func(c context.Context, s Sink) { EmitLLMError(c, s, LLMErrorArgs{Phase: "chat", StatusCode: 418}) },
			func(t *testing.T, p []byte) {
				var v events.LLMErrorPayload
				_ = json.Unmarshal(p, &v)
				if v.StatusCode != 418 {
					t.Errorf("StatusCode = %d", v.StatusCode)
				}
			},
		},
		{
			"LLMRefused.ContentSafetyHit",
			func(c context.Context, s Sink) {
				EmitLLMRefused(c, s, LLMRefusedArgs{RefusalText: "r", ContentSafetyHit: "self_harm"})
			},
			func(t *testing.T, p []byte) {
				var v events.LLMRefusedPayload
				_ = json.Unmarshal(p, &v)
				if v.ContentSafetyHit != "self_harm" {
					t.Errorf("ContentSafetyHit = %q", v.ContentSafetyHit)
				}
			},
		},
		{
			"PlannerInvoked.Reason",
			func(c context.Context, s Sink) { EmitPlannerInvoked(c, s, "user_request") },
			func(t *testing.T, p []byte) {
				var v events.PlannerInvokedPayload
				_ = json.Unmarshal(p, &v)
				if v.Reason != "user_request" {
					t.Errorf("Reason = %q", v.Reason)
				}
			},
		},
		{
			"PlannerStep.StepKind",
			func(c context.Context, s Sink) {
				EmitPlannerStep(c, s, PlannerStepArgs{StepIndex: 3, StepKind: "decide_tool"})
			},
			func(t *testing.T, p []byte) {
				var v events.PlannerStepPayload
				_ = json.Unmarshal(p, &v)
				if v.StepIndex != 3 || v.StepKind != "decide_tool" {
					t.Errorf("StepIndex/Kind = %d/%q", v.StepIndex, v.StepKind)
				}
			},
		},
		{
			"ToolRetrieval.TopK",
			func(c context.Context, s Sink) { EmitToolRetrieval(c, s, ToolRetrievalArgs{Query: "q", TopK: 9}) },
			func(t *testing.T, p []byte) {
				var v events.ToolRetrievalPayload
				_ = json.Unmarshal(p, &v)
				if v.TopK != 9 {
					t.Errorf("TopK = %d", v.TopK)
				}
			},
		},
		{
			"SummarizationTriggered.MessageCount",
			func(c context.Context, s Sink) {
				EmitSummarizationTriggered(c, s, SummarizationTriggeredArgs{MessageCount: 42, Reason: "ctx_pressure"})
			},
			func(t *testing.T, p []byte) {
				var v events.SummarizationTriggeredPayload
				_ = json.Unmarshal(p, &v)
				if v.MessageCount != 42 {
					t.Errorf("MessageCount = %d", v.MessageCount)
				}
			},
		},
		{
			"ModelSwitch.From_To",
			func(c context.Context, s Sink) { EmitModelSwitch(c, s, ModelSwitchArgs{From: "a", To: "b"}) },
			func(t *testing.T, p []byte) {
				var v events.ModelSwitchPayload
				_ = json.Unmarshal(p, &v)
				if v.From != "a" || v.To != "b" {
					t.Errorf("From/To = %q/%q", v.From, v.To)
				}
			},
		},
		{
			"ConfirmationResolved.Choice",
			func(c context.Context, s Sink) {
				EmitConfirmationResolved(c, s, ConfirmationResolvedArgs{Choice: "approve", ToolCallID: "c1"})
			},
			func(t *testing.T, p []byte) {
				var v events.ConfirmationResolvedPayload
				_ = json.Unmarshal(p, &v)
				if v.Choice != "approve" || v.ToolCallID != "c1" {
					t.Errorf("Choice/ToolCallID = %q/%q", v.Choice, v.ToolCallID)
				}
			},
		},
		{
			"Retry.Attempt",
			func(c context.Context, s Sink) { EmitRetry(c, s, RetryArgs{Phase: "llm", Attempt: 5}) },
			func(t *testing.T, p []byte) {
				var v events.RetryPayload
				_ = json.Unmarshal(p, &v)
				if v.Attempt != 5 || v.Phase != "llm" {
					t.Errorf("Attempt/Phase = %d/%q", v.Attempt, v.Phase)
				}
			},
		},
		{
			"ToolCallArgsInvalid.ValidationError",
			func(c context.Context, s Sink) {
				EmitToolCallArgsInvalid(c, s, ToolCallArgsInvalidArgs{
					CallID: "c", Plugin: "p", Action: "a", ValidationError: "missing id",
				})
			},
			func(t *testing.T, p []byte) {
				var v events.ToolCallArgsInvalidPayload
				_ = json.Unmarshal(p, &v)
				if v.ValidationError != "missing id" {
					t.Errorf("ValidationError = %q", v.ValidationError)
				}
			},
		},
		{
			"ScoreComputed.Score",
			func(c context.Context, s Sink) {
				EmitScoreComputed(c, s, ScoreComputedArgs{Score: 0.85, RubricVersion: "v3"})
			},
			func(t *testing.T, p []byte) {
				var v events.ScoreComputedPayload
				_ = json.Unmarshal(p, &v)
				if v.Score != 0.85 || v.RubricVersion != "v3" {
					t.Errorf("Score/Rubric = %v/%q", v.Score, v.RubricVersion)
				}
			},
		},
		{
			"Error.Where",
			func(c context.Context, s Sink) { EmitError(c, s, "orchestrator.turn", "msg") },
			func(t *testing.T, p []byte) {
				var v events.ErrorPayload
				_ = json.Unmarshal(p, &v)
				if v.Where != "orchestrator.turn" {
					t.Errorf("Where = %q", v.Where)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			tc.emit(context.Background(), sink)
			tc.assert(t, sink.snapshot()[0].Payload)
		})
	}
}
