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

func (e *echoRunner) Run(_ context.Context, _ string, content string, _ ...pkg.FileAttachment) (pkg.RunResponse, error) {
	return pkg.RunResponse{Response: "echo: " + content}, nil
}

func newTestHandler(verifier ProfileVerifier, checker LimitChecker) pkg.MessageHandler {
	ensureSession := func(_, _, _ string) {}
	noAction := func(_ context.Context, _, _ string, _ map[string]string) (string, error) {
		return "", errors.New("no actions")
	}
	hasAction := func(_, _ string) bool { return false }
	return NewMessageHandler(HandlerConfig{
		EnsureSession: ensureSession,
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
