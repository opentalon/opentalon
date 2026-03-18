package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

func TestPollingLoop(t *testing.T) {
	// Simulate Telegram getUpdates response
	callCount := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		call := callCount
		mu.Unlock()

		offset := r.URL.Query().Get("offset")

		var resp interface{}
		switch {
		case call == 1 && offset == "":
			// First call: return two updates
			resp = map[string]interface{}{
				"ok": true,
				"result": []interface{}{
					map[string]interface{}{
						"update_id": 100,
						"message": map[string]interface{}{
							"message_id": 1,
							"text":       "hello",
							"chat":       map[string]interface{}{"id": 42},
							"from":       map[string]interface{}{"id": 7},
						},
					},
					map[string]interface{}{
						"update_id": 101,
						"message": map[string]interface{}{
							"message_id": 2,
							"text":       "world",
							"chat":       map[string]interface{}{"id": 42},
							"from":       map[string]interface{}{"id": 7},
						},
					},
				},
			}
		case offset == "102":
			// Second call with correct offset: return one more update
			resp = map[string]interface{}{
				"ok": true,
				"result": []interface{}{
					map[string]interface{}{
						"update_id": 102,
						"message": map[string]interface{}{
							"message_id": 3,
							"text":       "again",
							"chat":       map[string]interface{}{"id": 42},
							"from":       map[string]interface{}{"id": 7},
						},
					},
				},
			}
		default:
			// No updates
			resp = map[string]interface{}{"ok": true, "result": []interface{}{}}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	spec := &YAMLChannelSpec{
		ID:   "test-poll",
		Name: "Test Polling",
		Inbound: InboundSpec{
			Polling: &PollingInboundSpec{
				Method:      "GET",
				URL:         server.URL + "?offset={{self.poll_offset}}",
				Interval:    100 * time.Millisecond,
				ResultPath:  "result",
				CursorField: "update_id",
			},
			EventPath: "message",
			Mapping: MappingSpec{
				ConversationID: MappingField{Field: "chat.id"},
				SenderID:       MappingField{Field: "from.id"},
				Content:        MappingField{Field: "text"},
			},
		},
	}

	ch := NewYAMLChannel(spec, ".")
	inbox := make(chan pkg.InboundMessage, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ch.Start(ctx, inbox); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for messages to arrive
	var msgs []pkg.InboundMessage
	timeout := time.After(2 * time.Second)
	for len(msgs) < 3 {
		select {
		case msg := <-inbox:
			msgs = append(msgs, msg)
		case <-timeout:
			t.Fatalf("timed out waiting for messages, got %d", len(msgs))
		}
	}

	cancel()
	_ = ch.Stop()

	// Verify messages
	if msgs[0].Content != "hello" {
		t.Errorf("msg[0].Content = %q, want %q", msgs[0].Content, "hello")
	}
	if msgs[1].Content != "world" {
		t.Errorf("msg[1].Content = %q, want %q", msgs[1].Content, "world")
	}
	if msgs[2].Content != "again" {
		t.Errorf("msg[2].Content = %q, want %q", msgs[2].Content, "again")
	}
	if msgs[0].ConversationID != "42" {
		t.Errorf("msg[0].ConversationID = %q, want %q", msgs[0].ConversationID, "42")
	}

	// Verify offset was tracked
	if ch.selfVars["poll_offset"] != "103" {
		t.Errorf("poll_offset = %q, want %q", ch.selfVars["poll_offset"], "103")
	}
}

func TestExtractEvents(t *testing.T) {
	ch := &YAMLChannel{}

	t.Run("empty path returns raw as single event", func(t *testing.T) {
		raw := map[string]interface{}{"foo": "bar"}
		events, err := ch.extractEvents(raw, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
	})

	t.Run("navigates dotted path", func(t *testing.T) {
		raw := map[string]interface{}{
			"data": map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{"id": 1},
					map[string]interface{}{"id": 2},
				},
			},
		}
		events, err := ch.extractEvents(raw, "data.items")
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 {
			t.Fatalf("got %d events, want 2", len(events))
		}
	})

	t.Run("returns nil for non-array", func(t *testing.T) {
		raw := map[string]interface{}{"result": "not-an-array"}
		events, err := ch.extractEvents(raw, "result")
		if err != nil {
			t.Fatal(err)
		}
		if events != nil {
			t.Fatalf("expected nil, got %v", events)
		}
	})
}
