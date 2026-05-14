package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// payloadJSON marshals a typed event payload for the test inserts. Real
// producers do the same — typed struct → json.Marshal → Insert.
func payloadJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

func TestSessionEventStore_InsertAssignsMonotonicSeq(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionEventStore(db)
	ctx := context.Background()

	// Insert three events on session A and one on session B; A's seq must
	// be 1, 2, 3 and B's must be 1 — independent counters per session.
	for i := 0; i < 3; i++ {
		err := store.Insert(ctx, SessionEvent{
			SessionID: "sess-A",
			EventType: events.TypeUserMessage,
			Payload:   payloadJSON(t, events.UserMessagePayload{Header: events.Header{V: 1}, Content: "hi", ContentLength: 2}),
		})
		if err != nil {
			t.Fatalf("Insert A[%d]: %v", i, err)
		}
	}
	err := store.Insert(ctx, SessionEvent{
		SessionID: "sess-B",
		EventType: events.TypeUserMessage,
		Payload:   payloadJSON(t, events.UserMessagePayload{Header: events.Header{V: 1}, Content: "hi", ContentLength: 2}),
	})
	if err != nil {
		t.Fatalf("Insert B: %v", err)
	}

	listA, err := store.ListForSession(ctx, "sess-A", 0, 0)
	if err != nil {
		t.Fatalf("ListForSession A: %v", err)
	}
	if len(listA) != 3 {
		t.Fatalf("len(listA) = %d, want 3", len(listA))
	}
	for i, ev := range listA {
		wantSeq := int64(i + 1)
		if ev.Seq != wantSeq {
			t.Errorf("listA[%d].Seq = %d, want %d", i, ev.Seq, wantSeq)
		}
	}

	listB, _ := store.ListForSession(ctx, "sess-B", 0, 0)
	if len(listB) != 1 || listB[0].Seq != 1 {
		t.Errorf("listB seq = %+v, want one event with seq=1", listB)
	}
}

func TestSessionEventStore_InsertValidatesInputs(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionEventStore(db)
	ctx := context.Background()

	cases := []struct {
		name string
		in   SessionEvent
	}{
		{"empty session_id", SessionEvent{EventType: "x", Payload: json.RawMessage(`{}`)}},
		{"empty event_type", SessionEvent{SessionID: "s", Payload: json.RawMessage(`{}`)}},
		{"empty payload", SessionEvent{SessionID: "s", EventType: "x"}},
		{"invalid json payload", SessionEvent{SessionID: "s", EventType: "x", Payload: json.RawMessage(`not json`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := store.Insert(ctx, c.in); err == nil {
				t.Errorf("Insert(%s): want error, got nil", c.name)
			}
		})
	}
}

func TestSessionEventStore_ParentIDAndDurationRoundTrip(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionEventStore(db)
	ctx := context.Background()

	parent := SessionEvent{
		ID:        "deadbeefdeadbeefdeadbeefdeadbeef",
		SessionID: "sess-link",
		EventType: events.TypeToolCallExtracted,
		Payload: payloadJSON(t, events.ToolCallExtractedPayload{
			Header: events.Header{V: 1}, CallID: "call_1", Plugin: "p", Action: "a", Mode: "native",
		}),
	}
	if err := store.Insert(ctx, parent); err != nil {
		t.Fatalf("Insert parent: %v", err)
	}
	child := SessionEvent{
		SessionID:  "sess-link",
		EventType:  events.TypeToolCallResult,
		ParentID:   parent.ID,
		DurationMS: 137,
		Payload: payloadJSON(t, events.ToolCallResultPayload{
			Header: events.Header{V: 1}, CallID: "call_1", Status: "ok", ResponseExcerpt: "{}",
		}),
	}
	if err := store.Insert(ctx, child); err != nil {
		t.Fatalf("Insert child: %v", err)
	}

	list, err := store.ListForSession(ctx, "sess-link", 0, 0)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2", len(list))
	}
	if list[0].ParentID != "" {
		t.Errorf("root event ParentID = %q, want empty", list[0].ParentID)
	}
	if list[1].ParentID != parent.ID {
		t.Errorf("child ParentID = %q, want %q", list[1].ParentID, parent.ID)
	}
	if list[1].DurationMS != 137 {
		t.Errorf("child DurationMS = %d, want 137", list[1].DurationMS)
	}
}

func TestSessionEventStore_ListForSessionSinceSeqAndLimit(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionEventStore(db)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		err := store.Insert(ctx, SessionEvent{
			SessionID: "sess-page",
			EventType: events.TypeUserMessage,
			Payload:   payloadJSON(t, events.UserMessagePayload{Header: events.Header{V: 1}, Content: "x"}),
		})
		if err != nil {
			t.Fatalf("Insert[%d]: %v", i, err)
		}
	}

	// since_seq = 2 → events with seq > 2 (i.e. 3, 4, 5).
	list, err := store.ListForSession(ctx, "sess-page", 2, 0)
	if err != nil {
		t.Fatalf("ListForSession sinceSeq=2: %v", err)
	}
	if len(list) != 3 || list[0].Seq != 3 {
		t.Errorf("sinceSeq=2: got len=%d first.Seq=%d, want len=3 first.Seq=3", len(list), list[0].Seq)
	}

	// limit = 2 → first two only.
	list, _ = store.ListForSession(ctx, "sess-page", 0, 2)
	if len(list) != 2 || list[0].Seq != 1 || list[1].Seq != 2 {
		t.Errorf("limit=2: got %+v, want seq=[1,2]", list)
	}
}

func TestSessionEventStore_PruneByTimestamp(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionEventStore(db)
	ctx := context.Background()

	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now().Add(-1 * time.Minute)

	if err := store.Insert(ctx, SessionEvent{
		SessionID: "s", EventType: events.TypeUserMessage, Timestamp: old,
		Payload: payloadJSON(t, events.UserMessagePayload{Header: events.Header{V: 1}, Content: "old"}),
	}); err != nil {
		t.Fatalf("Insert old: %v", err)
	}
	if err := store.Insert(ctx, SessionEvent{
		SessionID: "s", EventType: events.TypeUserMessage, Timestamp: fresh,
		Payload: payloadJSON(t, events.UserMessagePayload{Header: events.Header{V: 1}, Content: "fresh"}),
	}); err != nil {
		t.Fatalf("Insert fresh: %v", err)
	}

	deleted, err := store.Prune(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted != 1 {
		t.Errorf("Prune deleted %d, want 1", deleted)
	}

	n, _ := store.CountForSession(ctx, "s")
	if n != 1 {
		t.Errorf("post-prune count = %d, want 1", n)
	}

	// Prune with retention=0 is a no-op (caller signals "disabled").
	deleted, err = store.Prune(ctx, 0)
	if err != nil {
		t.Fatalf("Prune(0): %v", err)
	}
	if deleted != 0 {
		t.Errorf("Prune(0) deleted %d, want 0", deleted)
	}
}

func TestSessionEventStore_PromptSnapshotUpsertIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionEventStore(db)
	ctx := context.Background()

	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := store.UpsertPromptSnapshot(ctx, sha, events.PromptKindSystemPrompt, "first content"); err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	// Second upsert with different content must be a no-op (content-addressed
	// table — same sha means same content by construction).
	if err := store.UpsertPromptSnapshot(ctx, sha, events.PromptKindSystemPrompt, "second content"); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}
	content, kind, err := store.GetPromptSnapshot(ctx, sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if content != "first content" {
		t.Errorf("content = %q, want %q (idempotent upsert must not overwrite)", content, "first content")
	}
	if kind != events.PromptKindSystemPrompt {
		t.Errorf("kind = %q, want %q", kind, events.PromptKindSystemPrompt)
	}

	// Unknown sha resolves to empty (not an error).
	content, kind, err = store.GetPromptSnapshot(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("Get unknown: %v", err)
	}
	if content != "" || kind != "" {
		t.Errorf("unknown sha returned content=%q kind=%q, want both empty", content, kind)
	}
}

func TestEvents_ExcerptCapAndUTF8(t *testing.T) {
	// Excerpt with content under the cap returns untruncated.
	s := strings.Repeat("a", 100)
	got, truncated := events.Excerpt(s)
	if truncated {
		t.Errorf("Excerpt(100): truncated = true, want false")
	}
	if got != s {
		t.Errorf("Excerpt(100): content changed unexpectedly")
	}

	// Excerpt over the cap returns truncated at a rune boundary.
	big := strings.Repeat("z", events.ExcerptCap+512)
	got, truncated = events.Excerpt(big)
	if !truncated {
		t.Errorf("Excerpt(cap+512): truncated = false, want true")
	}
	if len(got) > events.ExcerptCap {
		t.Errorf("Excerpt: len = %d, want <= %d", len(got), events.ExcerptCap)
	}

	// Invalid UTF-8 (only continuation bytes, no rune starts): falls back
	// to a raw-byte cut at the cap rather than returning empty.
	invalid := strings.Repeat("\x80", events.ExcerptCap+10)
	got, truncated = events.Excerpt(invalid)
	if !truncated {
		t.Errorf("Excerpt(invalid UTF-8): truncated = false, want true")
	}
	if len(got) != events.ExcerptCap {
		t.Errorf("Excerpt(invalid UTF-8): len = %d, want %d (raw-byte fallback)", len(got), events.ExcerptCap)
	}

	// SanitizeUTF8 leaves valid input untouched and replaces invalid bytes.
	if got := events.SanitizeUTF8("hello"); got != "hello" {
		t.Errorf("SanitizeUTF8 valid input modified to %q", got)
	}
	bad := "hello\xc3\x28world"
	clean := events.SanitizeUTF8(bad)
	if clean == bad {
		t.Errorf("SanitizeUTF8 did not replace invalid bytes")
	}
	if !strings.Contains(clean, "hello") || !strings.Contains(clean, "world") {
		t.Errorf("SanitizeUTF8 corrupted valid surrounding text: %q", clean)
	}
}

// TestLLMResponsePayload_NativeToolCallsRawInlinesAsJSON pins the wire
// format: NativeToolCallsRaw must embed as inline JSON, not as an escaped
// string. This is the contract the api-plugin (TIM-868x follow-up) and
// psql inspection rely on.
func TestLLMResponsePayload_NativeToolCallsRawInlinesAsJSON(t *testing.T) {
	raw := json.RawMessage(`[{"id":"call_1","name":"tickets.show","arguments":{"id":"42"}}]`)
	p := events.LLMResponsePayload{
		Header:             events.Header{V: events.LLMResponseVersion},
		RawContentExcerpt:  "ok",
		NativeToolCallsRaw: raw,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := string(b)
	if !strings.Contains(out, `"native_tool_calls_raw":[{"id":"call_1"`) {
		t.Errorf("native_tool_calls_raw not embedded inline; got: %s", out)
	}
	if strings.Contains(out, `"native_tool_calls_raw":"`) {
		t.Errorf("native_tool_calls_raw marshalled as escaped string (regression); got: %s", out)
	}

	// Round-trip survives a re-decode with the same struct.
	var decoded events.LLMResponsePayload
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if string(decoded.NativeToolCallsRaw) != string(raw) {
		t.Errorf("round-trip mismatch: got %s, want %s", decoded.NativeToolCallsRaw, raw)
	}
}
