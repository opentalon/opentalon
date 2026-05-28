package channel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/profile"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// stubVerifier returns a fixed profile for any token.
type stubVerifier struct {
	p   *profile.Profile
	err error
}

func (s *stubVerifier) Verify(_ context.Context, _, _ string) (*profile.Profile, error) {
	return s.p, s.err
}

// stubLimitChecker returns a fixed total and error.
type stubLimitChecker struct {
	total int
	err   error
}

func (s *stubLimitChecker) TotalTokensSince(_ context.Context, _ string, _ time.Time) (int, error) {
	return s.total, s.err
}

// echoRunner returns the message content as the response.
type echoRunner struct{}

func (e *echoRunner) Run(_ context.Context, _ string, content string, _ ...pkg.FileAttachment) (string, string, map[string]string, error) {
	return "echo: " + content, "", nil, nil
}

func newTestHandler(verifier ProfileVerifier, checker LimitChecker) pkg.MessageHandler {
	loadSession := func(_ string) error { return nil }
	createSession := func(_, _, _ string) {}
	noAction := func(_ context.Context, _, _ string, _ map[string]string) (string, error) {
		return "", errors.New("no actions")
	}
	hasAction := func(_, _ string) bool { return false }
	return NewMessageHandler(HandlerConfig{
		LoadSession:   loadSession,
		CreateSession: createSession,
		Runner:        &echoRunner{},
		RunAction:     noAction,
		HasAction:     hasAction,
		Verifier:      verifier,
		LimitChecker:  checker,
	})
}

func callHandler(h pkg.MessageHandler, meta map[string]string) pkg.OutboundMessage {
	msg := pkg.InboundMessage{
		ChannelID:      "slack",
		ConversationID: "conv1",
		Content:        "hello",
		Metadata:       meta,
	}
	out, _ := h(context.Background(), "slack:conv1", msg)
	return out
}

func TestHandler_NoVerifier_Passthrough(t *testing.T) {
	h := newTestHandler(nil, nil)
	out := callHandler(h, nil)
	if out.Content != "echo: hello" {
		t.Errorf("Content = %q, want %q", out.Content, "echo: hello")
	}
}

func TestHandler_VerifierMissingToken(t *testing.T) {
	h := newTestHandler(&stubVerifier{p: &profile.Profile{EntityID: "u1"}}, nil)
	out := callHandler(h, nil) // no profile_token in metadata
	if !strings.Contains(out.Content, "profile token required") {
		t.Errorf("Content = %q, want token-required message", out.Content)
	}
}

func TestHandler_VerifierAuthFailed(t *testing.T) {
	h := newTestHandler(&stubVerifier{err: errors.New("bad token")}, nil)
	out := callHandler(h, map[string]string{"profile_token": "bad"})
	if !strings.Contains(out.Content, "authentication failed") {
		t.Errorf("Content = %q, want auth-failed message", out.Content)
	}
}

func TestHandler_VerifierSuccess(t *testing.T) {
	p := &profile.Profile{EntityID: "u1", Group: "g1"}
	h := newTestHandler(&stubVerifier{p: p}, nil)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if out.Content != "echo: hello" {
		t.Errorf("Content = %q, want %q", out.Content, "echo: hello")
	}
}

func TestHandler_LimitExceeded(t *testing.T) {
	p := &profile.Profile{EntityID: "u1", Limit: 1000, LimitWindow: time.Hour}
	checker := &stubLimitChecker{total: 1000} // at the limit
	h := newTestHandler(&stubVerifier{p: p}, checker)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if !strings.Contains(out.Content, "token limit reached") {
		t.Errorf("Content = %q, want limit-exceeded message", out.Content)
	}
}

func TestHandler_LimitNotYetExceeded(t *testing.T) {
	p := &profile.Profile{EntityID: "u1", Limit: 1000, LimitWindow: time.Hour}
	checker := &stubLimitChecker{total: 999} // one under
	h := newTestHandler(&stubVerifier{p: p}, checker)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if out.Content != "echo: hello" {
		t.Errorf("Content = %q, want echo response when under limit", out.Content)
	}
}

func TestHandler_LimitZero_Unlimited(t *testing.T) {
	// Limit=0 means no enforcement regardless of checker.
	p := &profile.Profile{EntityID: "u1", Limit: 0, LimitWindow: time.Hour}
	checker := &stubLimitChecker{total: 999999}
	h := newTestHandler(&stubVerifier{p: p}, checker)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if out.Content != "echo: hello" {
		t.Errorf("Content = %q, want passthrough when Limit=0", out.Content)
	}
}

func TestHandler_LimitWindowZero_Unlimited(t *testing.T) {
	// LimitWindow=0 means no enforcement.
	p := &profile.Profile{EntityID: "u1", Limit: 100, LimitWindow: 0}
	checker := &stubLimitChecker{total: 999999}
	h := newTestHandler(&stubVerifier{p: p}, checker)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if out.Content != "echo: hello" {
		t.Errorf("Content = %q, want passthrough when LimitWindow=0", out.Content)
	}
}

func TestHandler_LimitCheckerError_Passthrough(t *testing.T) {
	// A limit-check failure must not block the request — fail open.
	p := &profile.Profile{EntityID: "u1", Limit: 1000, LimitWindow: time.Hour}
	checker := &stubLimitChecker{err: errors.New("db unavailable")}
	h := newTestHandler(&stubVerifier{p: p}, checker)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if out.Content != "echo: hello" {
		t.Errorf("Content = %q, want passthrough on checker error", out.Content)
	}
}

func TestHandler_NilLimitChecker_WithLimit(t *testing.T) {
	// Profile has a limit but no checker is wired — must not panic.
	p := &profile.Profile{EntityID: "u1", Limit: 100, LimitWindow: time.Hour}
	h := newTestHandler(&stubVerifier{p: p}, nil)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if out.Content != "echo: hello" {
		t.Errorf("Content = %q, want passthrough when limitChecker is nil", out.Content)
	}
}

// instrumented session pair: records every Load/Create call so a test can
// assert which branch the handler took for a given resume_intent value.
type sessionRecorder struct {
	loads        []string
	loadErrors   map[string]error
	creates      []string
	createCalled bool
}

func (r *sessionRecorder) loadFunc() pkg.LoadSessionFunc {
	return func(key string) error {
		r.loads = append(r.loads, key)
		if err, ok := r.loadErrors[key]; ok {
			return err
		}
		return nil
	}
}

func (r *sessionRecorder) createFunc() pkg.CreateSessionFunc {
	return func(key, _, _ string) {
		r.creates = append(r.creates, key)
		r.createCalled = true
	}
}

func newRecordingHandler(rec *sessionRecorder) pkg.MessageHandler {
	noAction := func(_ context.Context, _, _ string, _ map[string]string) (string, error) {
		return "", errors.New("no actions")
	}
	hasAction := func(_, _ string) bool { return false }
	return NewMessageHandler(HandlerConfig{
		LoadSession:   rec.loadFunc(),
		CreateSession: rec.createFunc(),
		Runner:        &echoRunner{},
		RunAction:     noAction,
		HasAction:     hasAction,
	})
}

func TestHandler_ResumeIntentTrue_CallsLoadNotCreate(t *testing.T) {
	// Client-supplied conv-id must take the strict-load path. Auto-create
	// would let the UI keep its history while talking to a fresh empty
	// server session — the original silent-drift bug.
	rec := &sessionRecorder{}
	h := newRecordingHandler(rec)
	msg := pkg.InboundMessage{
		ChannelID:      "websocket",
		ConversationID: "abc",
		Content:        "hi",
		Metadata:       map[string]string{pkg.ResumeIntentMetadataKey: "true"},
	}
	_, _ = h(context.Background(), "websocket:abc", msg)
	if len(rec.loads) != 1 || rec.loads[0] != "websocket:abc" {
		t.Errorf("expected one Load for websocket:abc, got %v", rec.loads)
	}
	if rec.createCalled {
		t.Errorf("Create must not be called on resume_intent=true (creates=%v)", rec.creates)
	}
}

func TestHandler_ResumeIntentMissing_CallsCreateNotLoad(t *testing.T) {
	// Channel-minted conv-id (no resume_intent) must take the idempotent
	// create path. Load on a fresh id would always error and emit a
	// false-positive session_expired.
	rec := &sessionRecorder{}
	h := newRecordingHandler(rec)
	msg := pkg.InboundMessage{
		ChannelID:      "websocket",
		ConversationID: "new",
		Content:        "hi",
		// No resume_intent key at all.
	}
	_, _ = h(context.Background(), "websocket:new", msg)
	if len(rec.creates) != 1 || rec.creates[0] != "websocket:new" {
		t.Errorf("expected one Create for websocket:new, got %v", rec.creates)
	}
	if len(rec.loads) != 0 {
		t.Errorf("Load must not be called when resume_intent is absent (loads=%v)", rec.loads)
	}
}

func TestHandler_ResumeIntentTrue_LoadFails_EmitsSessionExpired(t *testing.T) {
	// The whole point of the refactor: stale conv-id must surface as a
	// typed error frame so the client can clear its storage and reconnect
	// fresh. error_code is the contract — UI translates it.
	rec := &sessionRecorder{
		loadErrors: map[string]error{"websocket:gone": errors.New("session \"websocket:gone\" not found")},
	}
	h := newRecordingHandler(rec)
	msg := pkg.InboundMessage{
		ChannelID:      "websocket",
		ConversationID: "gone",
		Content:        "are you there?",
		Metadata:       map[string]string{pkg.ResumeIntentMetadataKey: "true"},
	}
	out, _ := h(context.Background(), "websocket:gone", msg)
	if got := out.Metadata["error_code"]; got != "session_expired" {
		t.Errorf("error_code = %q, want session_expired", got)
	}
	if got := out.Metadata["type"]; got != "error" {
		t.Errorf("type = %q, want error", got)
	}
	if rec.createCalled {
		t.Errorf("Create must not be called after Load failure (creates=%v)", rec.creates)
	}
}
