package channel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// --- getStringField tests ---

func TestGetStringField_SimpleKey(t *testing.T) {
	m := map[string]interface{}{
		"name": "Alice",
	}
	if got := getStringField(m, "name"); got != "Alice" {
		t.Errorf("got %q, want %q", got, "Alice")
	}
}

func TestGetStringField_MissingKey(t *testing.T) {
	m := map[string]interface{}{}
	if got := getStringField(m, "missing"); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestGetStringField_DottedPath(t *testing.T) {
	m := map[string]interface{}{
		"from": map[string]interface{}{
			"id": "user123",
		},
	}
	if got := getStringField(m, "from.id"); got != "user123" {
		t.Errorf("got %q, want %q", got, "user123")
	}
}

func TestGetStringField_DottedPathDeep(t *testing.T) {
	m := map[string]interface{}{
		"channelData": map[string]interface{}{
			"tenant": map[string]interface{}{
				"id": "tenant-abc",
			},
		},
	}
	if got := getStringField(m, "channelData.tenant.id"); got != "tenant-abc" {
		t.Errorf("got %q, want %q", got, "tenant-abc")
	}
}

func TestGetStringField_LiteralDotKeyTakesPriority(t *testing.T) {
	// A key that literally contains a dot should be returned before navigating
	m := map[string]interface{}{
		"from.id": "literal-dot-value",
		"from": map[string]interface{}{
			"id": "nested-value",
		},
	}
	if got := getStringField(m, "from.id"); got != "literal-dot-value" {
		t.Errorf("got %q, want %q (literal dot key should take priority)", got, "literal-dot-value")
	}
}

func TestGetStringField_DottedPathMissingIntermediate(t *testing.T) {
	m := map[string]interface{}{
		"a": "not-a-map",
	}
	if got := getStringField(m, "a.b"); got != "" {
		t.Errorf("got %q, want empty string for non-map intermediate", got)
	}
}

func TestGetStringField_Float64(t *testing.T) {
	m := map[string]interface{}{
		"count": float64(42),
	}
	if got := getStringField(m, "count"); got != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

func TestGetStringField_NilValue(t *testing.T) {
	m := map[string]interface{}{
		"key": nil,
	}
	if got := getStringField(m, "key"); got != "" {
		t.Errorf("got %q, want empty string for nil value", got)
	}
}

// --- WebhookServer tests ---

func newTestWebhookServer() *WebhookServer {
	return &WebhookServer{
		mux: http.NewServeMux(),
	}
}

func reserveFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type: %T", ln.Addr())
	}
	return addr.Port
}

func TestWebhookServer_SingleRegistration(t *testing.T) {
	s := newTestWebhookServer()
	port := reserveFreePort(t)
	t.Cleanup(func() {
		if s.server != nil {
			_ = s.server.Close()
		}
	})

	called := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	if err := s.register(port, "/api/messages", handler); err != nil {
		t.Fatalf("unexpected error on first registration: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if !called {
		t.Error("handler was not called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusOK)
	}

}

func TestWebhookServer_MultipleRegistrationsSamePort(t *testing.T) {
	s := newTestWebhookServer()
	port := reserveFreePort(t)
	t.Cleanup(func() {
		if s.server != nil {
			_ = s.server.Close()
		}
	})

	handler1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "handler1")
	})
	handler2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "handler2")
	})

	if err := s.register(port, "/api/messages", handler1); err != nil {
		t.Fatalf("unexpected error on first registration: %v", err)
	}
	if err := s.register(port, "/api/other", handler2); err != nil {
		t.Fatalf("unexpected error on second registration with same port: %v", err)
	}

	req1 := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	rec1 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec1, req1)
	if rec1.Body.String() != "handler1" {
		t.Errorf("route /api/messages: got %q, want %q", rec1.Body.String(), "handler1")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/other", nil)
	rec2 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec2, req2)
	if rec2.Body.String() != "handler2" {
		t.Errorf("route /api/other: got %q, want %q", rec2.Body.String(), "handler2")
	}

}

func TestWebhookServer_ConflictingPort(t *testing.T) {
	s := newTestWebhookServer()
	port1 := reserveFreePort(t)
	port2 := reserveFreePort(t)
	if port1 == port2 {
		port2 = reserveFreePort(t)
	}
	t.Cleanup(func() {
		if s.server != nil {
			_ = s.server.Close()
		}
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	if err := s.register(port1, "/api/messages", handler); err != nil {
		t.Fatalf("unexpected error on first registration: %v", err)
	}

	err := s.register(port2, "/api/other", handler)
	if err == nil {
		t.Error("expected error when registering on a different port, got nil")
	}
}

func TestRegisterWebhookRoute_DefaultPort(t *testing.T) {
	if got := webhookPortOrDefault(0); got != 3978 {
		t.Fatalf("webhookPortOrDefault(0)=%d, want 3978", got)
	}
	if got := webhookPortOrDefault(-1); got != 3978 {
		t.Fatalf("webhookPortOrDefault(-1)=%d, want 3978", got)
	}
	if got := webhookPortOrDefault(4123); got != 4123 {
		t.Fatalf("webhookPortOrDefault(4123)=%d, want 4123", got)
	}
}

func TestWebhookServer_ShutdownResetsState(t *testing.T) {
	s := newTestWebhookServer()
	port := reserveFreePort(t)
	t.Cleanup(func() {
		if s.server != nil {
			_ = s.server.Close()
		}
	})
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	if err := s.register(port, "/api/messages", handler); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !s.started || s.server == nil {
		t.Fatalf("expected started server after register")
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if s.started {
		t.Fatalf("expected started=false after shutdown")
	}
	if s.server != nil {
		t.Fatalf("expected server=nil after shutdown")
	}
	if s.port != 0 {
		t.Fatalf("expected port reset to 0, got %d", s.port)
	}

	if err := s.register(port, "/api/messages2", handler); err != nil {
		t.Fatalf("register after shutdown: %v", err)
	}
}

// --- buildWebhookHandler tests ---

// newTestChannel builds a minimal YAMLChannel suitable for handler tests.
func newTestChannel(eventTypes []string, inbox chan<- pkg.InboundMessage) *YAMLChannel {
	ctx, cancel := context.WithCancel(context.Background())
	return &YAMLChannel{
		spec: &YAMLChannelSpec{
			ID: "test-webhook",
			Inbound: InboundSpec{
				EventTypes: eventTypes,
				Mapping: MappingSpec{
					ConversationID: MappingField{Field: "conversation.id"},
					SenderID:       MappingField{Field: "from.id"},
					Content:        MappingField{Field: "text"},
					ThreadID:       MappingField{Field: "replyToId", Fallback: "id"},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
		inbox:    inbox,
		ctx:      ctx,
		cancel:   cancel,
	}
}

func TestWebhookHandler_RejectsNonPOST(t *testing.T) {
	ch := newTestChannel(nil, make(chan pkg.InboundMessage, 1))
	wh := &WebhookInboundSpec{ResponseCode: 200}
	handler := ch.buildWebhookHandler(wh)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/messages", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got status %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestWebhookHandler_Returns200OnPost(t *testing.T) {
	ch := newTestChannel(nil, make(chan pkg.InboundMessage, 1))
	wh := &WebhookInboundSpec{ResponseCode: 200}
	handler := ch.buildWebhookHandler(wh)

	req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestWebhookHandler_CustomResponseCode(t *testing.T) {
	ch := newTestChannel(nil, make(chan pkg.InboundMessage, 1))
	wh := &WebhookInboundSpec{ResponseCode: 202}
	handler := ch.buildWebhookHandler(wh)

	req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusAccepted)
	}
}

func TestWebhookHandler_JWTValidationFailure(t *testing.T) {
	ch := newTestChannel(nil, make(chan pkg.InboundMessage, 1))
	// Validator that always rejects
	ch.jwtValidator = NewJWTValidator("http://invalid", "aud", "iss")
	wh := &WebhookInboundSpec{ValidateJWT: true, ResponseCode: 200}
	handler := ch.buildWebhookHandler(wh)

	req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer not.a.jwt")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWebhookHandler_NoJWTHeaderReturns401(t *testing.T) {
	ch := newTestChannel(nil, make(chan pkg.InboundMessage, 1))
	ch.jwtValidator = NewJWTValidator("http://invalid", "aud", "iss")
	wh := &WebhookInboundSpec{ValidateJWT: true, ResponseCode: 200}
	handler := ch.buildWebhookHandler(wh)

	req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(`{}`))
	// No Authorization header
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWebhookHandler_ProcessesActivityBody(t *testing.T) {
	inbox := make(chan pkg.InboundMessage, 1)
	ch := newTestChannel([]string{"message"}, inbox)
	wh := &WebhookInboundSpec{ResponseCode: 200}
	handler := ch.buildWebhookHandler(wh)

	body := `{
		"type": "message",
		"id": "act-1",
		"text": "hello bot",
		"from": {"id": "user-1", "role": "user"},
		"conversation": {"id": "conv-1"},
		"replyToId": "reply-1"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}

	select {
	case msg := <-inbox:
		if msg.Content != "hello bot" {
			t.Errorf("Content = %q, want %q", msg.Content, "hello bot")
		}
		if msg.ConversationID != "conv-1" {
			t.Errorf("ConversationID = %q, want %q", msg.ConversationID, "conv-1")
		}
		if msg.SenderID != "user-1" {
			t.Errorf("SenderID = %q, want %q", msg.SenderID, "user-1")
		}
		if msg.ThreadID != "reply-1" {
			t.Errorf("ThreadID = %q, want %q", msg.ThreadID, "reply-1")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("timeout: no message delivered to inbox")
	}
}

func TestWebhookHandler_SkipsBotMessages(t *testing.T) {
	inbox := make(chan pkg.InboundMessage, 1)
	ch := newTestChannel([]string{"message"}, inbox)
	ch.spec.Inbound.Skip = []SkipRule{{Field: "from.role", Equals: "bot"}}
	wh := &WebhookInboundSpec{ResponseCode: 200}
	handler := ch.buildWebhookHandler(wh)

	body := `{"type":"message","id":"act-2","text":"I am bot","from":{"id":"bot-1","role":"bot"},"conversation":{"id":"conv-1"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}

	select {
	case msg := <-inbox:
		t.Errorf("expected no message, got %+v", msg)
	case <-time.After(100 * time.Millisecond):
		// correct: bot message was skipped
	}
}

func TestWebhookHandler_StripsMention(t *testing.T) {
	inbox := make(chan pkg.InboundMessage, 1)
	ch := newTestChannel([]string{"message"}, inbox)
	ch.spec.Inbound.Transforms = []Transform{
		{Type: "replace", Pattern: `<at>[^<]*</at>\s*`, Replacement: "", Regex: true},
		{Type: "trim"},
	}
	wh := &WebhookInboundSpec{ResponseCode: 200}
	handler := ch.buildWebhookHandler(wh)

	body := `{"type":"message","id":"act-3","text":"<at>MyBot</at> hello there","from":{"id":"u1"},"conversation":{"id":"conv-1"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	select {
	case msg := <-inbox:
		if msg.Content != "hello there" {
			t.Errorf("Content = %q, want %q", msg.Content, "hello there")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("timeout: no message delivered to inbox")
	}
}
