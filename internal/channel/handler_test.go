package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// stubVerifier returns a fixed profile for any token.
type stubVerifier struct {
	p   *profile.Profile
	err error
}

func (s *stubVerifier) Verify(_ context.Context, _, _ string, _ map[string]string) (*profile.Profile, error) {
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

// baseHandlerConfig returns the shared no-op session/action stubs every
// handler test needs. Individual tests override fields they care about
// (ResumeSession/CreateSession for routing tests, Verifier/LimitChecker
// for auth tests) and re-pass the config to NewMessageHandler.
func baseHandlerConfig() HandlerConfig {
	return HandlerConfig{
		ResumeSession: func(_ string) error { return nil },
		CreateSession: func(_, _, _, _ string) {},
		Runner:        &echoRunner{},
		RunAction: func(_ context.Context, _, _ string, _ map[string]string) (string, error) {
			return "", errors.New("no actions")
		},
		HasAction: func(_, _ string) bool { return false },
	}
}

func newTestHandler(verifier ProfileVerifier, checker LimitChecker) pkg.MessageHandler {
	cfg := baseHandlerConfig()
	cfg.Verifier = verifier
	cfg.LimitChecker = checker
	return NewMessageHandler(cfg)
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

// TestHandler_StampsOwnerEntityOnReply pins the seam a cross-pod channel
// fan-out gates delivery on: a profile-verified turn's reply metadata must
// carry the owning entity (an internal underscore key, stripped by channels
// before the client frame), while profile_token stays stripped and an
// anonymous turn carries no owner at all.
func TestHandler_StampsOwnerEntityOnReply(t *testing.T) {
	h := newTestHandler(&stubVerifier{p: &profile.Profile{EntityID: "u1", Group: "g1"}}, nil)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if got := out.Metadata[pkg.OwnerEntityMetadataKey]; got != "u1" {
		t.Errorf("reply Metadata[%s] = %q, want u1", pkg.OwnerEntityMetadataKey, got)
	}
	if _, ok := out.Metadata["profile_token"]; ok {
		t.Error("reply must not echo profile_token")
	}
}

func TestHandler_NoOwnerEntityWithoutProfile(t *testing.T) {
	h := newTestHandler(nil, nil)
	out := callHandler(h, nil)
	if _, ok := out.Metadata[pkg.OwnerEntityMetadataKey]; ok {
		t.Error("anonymous reply must not carry the owner-entity stamp")
	}
}

// TestHandler_StampsOwnerEntityOnTokenLimitFrame pins the stamp-before-limit
// ordering: the token-limit error frame is built from the inbound metadata via
// safeMetadata, so the owner must already be stamped when the limit check
// short-circuits — otherwise a cross-pod fan-out could not gate the frame to
// the conversation owner.
func TestHandler_StampsOwnerEntityOnTokenLimitFrame(t *testing.T) {
	p := &profile.Profile{EntityID: "u1", Limit: 1000, LimitWindow: time.Hour}
	checker := &stubLimitChecker{total: 1000} // at the limit
	h := newTestHandler(&stubVerifier{p: p}, checker)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if got := out.Metadata["error_code"]; got != "token_limit_exceeded" {
		t.Fatalf("error_code = %q, want token_limit_exceeded", got)
	}
	if got := out.Metadata[pkg.OwnerEntityMetadataKey]; got != "u1" {
		t.Errorf("limit frame Metadata[%s] = %q, want u1", pkg.OwnerEntityMetadataKey, got)
	}
}

// captureRunner records the context it is handed so a test can assert what
// the handler stamped onto it before dispatch.
type captureRunner struct{ ctx context.Context }

func (c *captureRunner) Run(ctx context.Context, _ string, content string, _ ...pkg.FileAttachment) (string, string, map[string]string, error) {
	c.ctx = ctx
	return "echo: " + content, "", nil, nil
}

// TestHandler_StampsGroupIDFromProfile pins the load-bearing seam that the
// per-account event tagging depends on: a profile-verified turn must carry the
// profile's Group both on the context handed to the Runner (the exact ctx the
// emit helpers read actor.GroupID from to stamp group_id onto every session
// event and the event-webhook envelope) AND on the CreateSession call. Without
// this, dropping the WithGroupID wiring in the handler would leave the
// per-hop tests (context/emit/sink) green while every real event shipped an
// empty group_id.
func TestHandler_StampsGroupIDFromProfile(t *testing.T) {
	runner := &captureRunner{}
	var createdGroup string
	cfg := baseHandlerConfig()
	cfg.Runner = runner
	cfg.Verifier = &stubVerifier{p: &profile.Profile{EntityID: "u1", Group: "g1"}}
	cfg.CreateSession = func(_, _, group, _ string) { createdGroup = group }
	h := NewMessageHandler(cfg)

	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if out.Content != "echo: hello" {
		t.Fatalf("Content = %q, want %q", out.Content, "echo: hello")
	}
	if got := actor.GroupID(runner.ctx); got != "g1" {
		t.Errorf("actor.GroupID(dispatch ctx) = %q, want g1 (profile group must reach the emit choke point)", got)
	}
	if createdGroup != "g1" {
		t.Errorf("CreateSession group = %q, want g1", createdGroup)
	}
}

// TestHandler_HonorsHiddenVisibilityForSystemProfile: a WhoAmI-verified system
// invocation (e.g. a job-completion inject) may mark its turn hidden — fed to
// the model but kept out of the audited transcript — so the visibility must
// reach the dispatch ctx the orchestrator stamps from.
func TestHandler_HonorsHiddenVisibilityForSystemProfile(t *testing.T) {
	runner := &captureRunner{}
	cfg := baseHandlerConfig()
	cfg.Runner = runner
	cfg.Verifier = &stubVerifier{p: &profile.Profile{EntityID: "u1", Kind: profile.KindSystem}}
	h := NewMessageHandler(cfg)

	callHandler(h, map[string]string{"profile_token": "tok", "visibility": provider.VisibilityHidden})

	if got := actor.Visibility(runner.ctx); got != provider.VisibilityHidden {
		t.Errorf("actor.Visibility(dispatch ctx) = %q, want %q for a system turn", got, provider.VisibilityHidden)
	}
}

// TestHandler_IgnoresClientVisibilityForChatProfile is the security regression
// guard: an ordinary chat client that sets visibility=hidden must NOT get its
// turn hidden — otherwise it could feed model-directed content while keeping it
// out of the audited transcript. Hiding is gated on the verified system kind.
func TestHandler_IgnoresClientVisibilityForChatProfile(t *testing.T) {
	runner := &captureRunner{}
	cfg := baseHandlerConfig()
	cfg.Runner = runner
	cfg.Verifier = &stubVerifier{p: &profile.Profile{EntityID: "u1", Kind: profile.KindChat}}
	h := NewMessageHandler(cfg)

	callHandler(h, map[string]string{"profile_token": "tok", "visibility": provider.VisibilityHidden})

	if got := actor.Visibility(runner.ctx); got != "" {
		t.Errorf("actor.Visibility(dispatch ctx) = %q, want empty (a chat client must not hide its own turn)", got)
	}
}

// TestHandler_EnrichmentFailedRejects asserts the fail-closed contract:
// when yaml_ws.go marks a message with __enrich_failed metadata, the
// handler short-circuits before even consulting the verifier and returns
// a typed error frame the channel can render to the user.
func TestHandler_EnrichmentFailedRejects(t *testing.T) {
	// Verifier is wired but should never be called: enrich failure runs
	// before profile verification.
	h := newTestHandler(&stubVerifier{p: &profile.Profile{EntityID: "u1"}}, nil)
	out := callHandler(h, map[string]string{
		"profile_token":         "tok",
		enrichmentFailedKey:     "step \"user\": upstream status 500",
		enrichmentFailedStepKey: "user",
	})
	if !strings.Contains(out.Content, "verify your account info") {
		t.Errorf("Content = %q, want user-visible enrichment-failed message", out.Content)
	}
	if got := out.Metadata["error_code"]; got != "enrichment_failed" {
		t.Errorf("error_code = %q, want enrichment_failed", got)
	}
	// Internal flags must not leak back to the channel.
	if _, ok := out.Metadata[enrichmentFailedKey]; ok {
		t.Errorf("internal enrich-failed key leaked to outbound metadata")
	}
	if _, ok := out.Metadata[enrichmentFailedStepKey]; ok {
		t.Errorf("internal enrich-failed-step key leaked to outbound metadata")
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

// TestHandler_LimitNotAppliedToSystemRun: a system invocation is outside the
// chat budget on BOTH sides — it is excluded from TotalTokensSince and must not
// be blocked by an exhausted budget either, so a job-completion note still lands.
func TestHandler_LimitNotAppliedToSystemRun(t *testing.T) {
	p := &profile.Profile{EntityID: "u1", Kind: profile.KindSystem, Limit: 1000, LimitWindow: time.Hour}
	checker := &stubLimitChecker{total: 5000} // way over the limit
	h := newTestHandler(&stubVerifier{p: p}, checker)
	out := callHandler(h, map[string]string{"profile_token": "tok"})
	if strings.Contains(out.Content, "token limit reached") {
		t.Errorf("system run was blocked by the chat budget: %q", out.Content)
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

// instrumented session pair: records every Resume/Create call so a test can
// assert which branch the handler took for a given resume_intent value.
type sessionRecorder struct {
	resumes     []string
	resumeError map[string]error
	creates     []string
}

func (r *sessionRecorder) resumeFunc() pkg.ResumeSessionFunc {
	return func(key string) error {
		r.resumes = append(r.resumes, key)
		if err, ok := r.resumeError[key]; ok {
			return err
		}
		return nil
	}
}

func (r *sessionRecorder) createFunc() pkg.CreateSessionFunc {
	return func(key, _, _, _ string) {
		r.creates = append(r.creates, key)
	}
}

func newRecordingHandler(rec *sessionRecorder) pkg.MessageHandler {
	cfg := baseHandlerConfig()
	cfg.ResumeSession = rec.resumeFunc()
	cfg.CreateSession = rec.createFunc()
	return NewMessageHandler(cfg)
}

func TestHandler_ResumeIntentTrue_CallsResumeNotCreate(t *testing.T) {
	// Client-supplied conv-id must take the strict-resume path. Auto-create
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
	if len(rec.resumes) != 1 || rec.resumes[0] != "websocket:abc" {
		t.Errorf("expected one Resume for websocket:abc, got %v", rec.resumes)
	}
	if len(rec.creates) != 0 {
		t.Errorf("Create must not be called on resume_intent=true (creates=%v)", rec.creates)
	}
}

func TestHandler_ResumeIntentMissing_CallsCreateNotResume(t *testing.T) {
	// Channel-minted conv-id (no resume_intent) must take the idempotent
	// create path. Resume on a fresh id would always error and emit a
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
	if len(rec.resumes) != 0 {
		t.Errorf("Resume must not be called when resume_intent is absent (resumes=%v)", rec.resumes)
	}
}

func TestHandler_ResumeIntentTrue_ResumeFails_NotFound_EmitsSessionExpired(t *testing.T) {
	// The whole point of the refactor: stale conv-id must surface as a
	// typed error frame so the client can clear its storage and reconnect
	// fresh. error_code is the contract — UI translates it. The recorder
	// returns a wrapped state.ErrSessionNotFound so the handler discriminates
	// this case from an infrastructure failure.
	rec := &sessionRecorder{
		resumeError: map[string]error{
			"websocket:gone": fmt.Errorf("session %q: %w", "websocket:gone", state.ErrSessionNotFound),
		},
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
	if len(rec.creates) != 0 {
		t.Errorf("Create must not be called after Resume failure (creates=%v)", rec.creates)
	}
}

func TestHandler_ResumeIntentTrue_ResumeFails_InfraError_EmitsInternalError(t *testing.T) {
	// A transient infrastructure failure (dropped DB connection, context
	// cancellation, etc.) must NOT masquerade as session_expired — telling
	// the user "start a new chat" would cost them a valid conversation_id
	// on every brief DB hiccup. The contract: only errors.Is(err,
	// state.ErrSessionNotFound) maps to session_expired; everything else
	// maps to internal_error so the client can retry without resetting.
	rec := &sessionRecorder{
		resumeError: map[string]error{"websocket:live": errors.New("database connection lost")},
	}
	h := newRecordingHandler(rec)
	msg := pkg.InboundMessage{
		ChannelID:      "websocket",
		ConversationID: "live",
		Content:        "ping",
		Metadata:       map[string]string{pkg.ResumeIntentMetadataKey: "true"},
	}
	out, _ := h(context.Background(), "websocket:live", msg)
	if got := out.Metadata["error_code"]; got != "internal_error" {
		t.Errorf("error_code = %q, want internal_error (not session_expired)", got)
	}
	if got := out.Metadata["type"]; got != "error" {
		t.Errorf("type = %q, want error", got)
	}
	if strings.Contains(out.Content, "no longer available") || strings.Contains(out.Content, "start a new chat") {
		t.Errorf("Content = %q must not say session is gone on infra failure", out.Content)
	}
	if len(rec.creates) != 0 {
		t.Errorf("Create must not be called after Resume failure (creates=%v)", rec.creates)
	}
}

func TestHandler_ResumeHello_PendingConfirmation_ReEmitsPromptFrame(t *testing.T) {
	// A resume handshake with a still-pending confirmation must re-emit the
	// prompt frame (content + confirmation metadata) so the reconnected client
	// redraws its Approve/Reject buttons. The Runner must NOT run.
	cfg := baseHandlerConfig()
	cfg.Runner = &failRunner{t}
	cfg.PendingConfirmation = func(_ string) (string, map[string]string, bool) {
		return "Proceed with deleting 3 items?",
			map[string]string{"type": "confirmation", "prompt_type": "tool_confirmation", "tool_call_id": "call_9"}, true
	}
	h := NewMessageHandler(cfg)
	msg := pkg.InboundMessage{
		ChannelID:      "websocket",
		ConversationID: "abc",
		Metadata:       map[string]string{pkg.ControlMetadataKey: pkg.ControlResumeHello, pkg.ResumeIntentMetadataKey: "true"},
	}
	out, err := h(context.Background(), "websocket:abc", msg)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if out.Content != "Proceed with deleting 3 items?" {
		t.Errorf("Content = %q, want the re-emitted prompt", out.Content)
	}
	if out.Metadata["prompt_type"] != "tool_confirmation" || out.Metadata["tool_call_id"] != "call_9" {
		t.Errorf("re-emit metadata mismatch: %+v", out.Metadata)
	}
	// The hello's own control/resume flags must not leak back to the client.
	if _, ok := out.Metadata[pkg.ControlMetadataKey]; ok {
		t.Errorf("control key leaked into re-emit frame: %+v", out.Metadata)
	}
}

// TestHandler_ResumeHello_ReEmitCarriesOwnerEntity: the re-emitted confirmation
// frame's metadata comes from the orchestrator, not from safeMetadata, so the
// handler must stamp the owner explicitly — otherwise a cross-pod fan-out could
// not deliver the redrawn Approve/Reject prompt to the owner's other tabs.
func TestHandler_ResumeHello_ReEmitCarriesOwnerEntity(t *testing.T) {
	cfg := baseHandlerConfig()
	cfg.Runner = &failRunner{t}
	cfg.Verifier = &stubVerifier{p: &profile.Profile{EntityID: "e1", Group: "g1"}}
	cfg.PendingConfirmation = func(_ string) (string, map[string]string, bool) {
		return "Proceed?", map[string]string{"type": "confirmation", "prompt_type": "tool_confirmation"}, true
	}
	h := NewMessageHandler(cfg)
	msg := pkg.InboundMessage{
		ChannelID:      "websocket",
		ConversationID: "abc",
		Metadata: map[string]string{
			"profile_token": "tok", pkg.ControlMetadataKey: pkg.ControlResumeHello, pkg.ResumeIntentMetadataKey: "true",
		},
	}
	out, err := h(context.Background(), "websocket:abc", msg)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if out.Content != "Proceed?" {
		t.Fatalf("Content = %q, want the re-emitted prompt", out.Content)
	}
	if got := out.Metadata[pkg.OwnerEntityMetadataKey]; got != "e1" {
		t.Errorf("re-emit Metadata[%s] = %q, want e1", pkg.OwnerEntityMetadataKey, got)
	}
}

func TestHandler_ResumeHello_NothingPending_ReturnsEmptyFrame(t *testing.T) {
	// A resume handshake with nothing pending must return a zero-value frame
	// (which the registry drops) — never a Runner turn, never a stray bubble.
	cfg := baseHandlerConfig()
	cfg.Runner = &failRunner{t}
	cfg.PendingConfirmation = func(_ string) (string, map[string]string, bool) { return "", nil, false }
	h := NewMessageHandler(cfg)
	msg := pkg.InboundMessage{
		ChannelID:      "websocket",
		ConversationID: "abc",
		Metadata:       map[string]string{pkg.ControlMetadataKey: pkg.ControlResumeHello, pkg.ResumeIntentMetadataKey: "true"},
	}
	out, err := h(context.Background(), "websocket:abc", msg)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if out.Content != "" || len(out.Metadata) != 0 || len(out.Files) != 0 {
		t.Errorf("expected zero-value frame for nothing-pending hello, got %+v", out)
	}
}

func TestHandler_ResumeHello_ResumeFails_NotFound_EmitsSessionExpired(t *testing.T) {
	// A hello for a session that no longer exists still takes the strict resume
	// path, so a dead conversation is reported at reconnect — not on the first
	// keystroke. PendingConfirmation must not even be consulted.
	pendingConsulted := false
	cfg := baseHandlerConfig()
	cfg.Runner = &failRunner{t}
	cfg.ResumeSession = func(_ string) error {
		return fmt.Errorf("session gone: %w", state.ErrSessionNotFound)
	}
	cfg.PendingConfirmation = func(_ string) (string, map[string]string, bool) {
		pendingConsulted = true
		return "", nil, false
	}
	h := NewMessageHandler(cfg)
	msg := pkg.InboundMessage{
		ChannelID:      "websocket",
		ConversationID: "gone",
		Metadata:       map[string]string{pkg.ControlMetadataKey: pkg.ControlResumeHello, pkg.ResumeIntentMetadataKey: "true"},
	}
	out, _ := h(context.Background(), "websocket:gone", msg)
	if got := out.Metadata["error_code"]; got != "session_expired" {
		t.Errorf("error_code = %q, want session_expired", got)
	}
	if pendingConsulted {
		t.Error("PendingConfirmation must not be consulted when resume fails")
	}
}

func TestHandler_ResumeHello_SkipsTokenLimit(t *testing.T) {
	// A control message does no LLM work, so an exhausted token budget must not
	// block the confirmation re-emit — otherwise a user who hit their limit
	// mid-approval could never resolve the pending write on reconnect.
	cfg := baseHandlerConfig()
	cfg.Runner = &failRunner{t}
	cfg.Verifier = &stubVerifier{p: &profile.Profile{EntityID: "e1", Limit: 100, LimitWindow: time.Hour}}
	cfg.LimitChecker = &stubLimitChecker{total: 1_000} // way over the limit
	cfg.PendingConfirmation = func(_ string) (string, map[string]string, bool) {
		return "Proceed?", map[string]string{"prompt_type": "tool_confirmation"}, true
	}
	h := NewMessageHandler(cfg)
	msg := pkg.InboundMessage{
		ChannelID:      "websocket",
		ConversationID: "abc",
		Metadata: map[string]string{
			"profile_token": "tok", pkg.ControlMetadataKey: pkg.ControlResumeHello, pkg.ResumeIntentMetadataKey: "true",
		},
	}
	out, _ := h(context.Background(), "websocket:abc", msg)
	if got := out.Metadata["error_code"]; got == "token_limit_exceeded" {
		t.Fatal("resume hello was blocked by the token limit; control messages must skip it")
	}
	if out.Metadata["prompt_type"] != "tool_confirmation" {
		t.Errorf("expected the confirmation re-emit despite over-limit, got %+v", out.Metadata)
	}
}

// failRunner fails the test if the LLM Runner is invoked. Used by control-path
// tests to prove no LLM turn runs.
type failRunner struct{ t *testing.T }

func (r *failRunner) Run(_ context.Context, _ string, _ string, _ ...pkg.FileAttachment) (string, string, map[string]string, error) {
	r.t.Fatal("Runner.Run must not be called on a control message")
	return "", "", nil, nil
}

func TestHandler_NewMessageHandler_PanicsOnNilResumeSession(t *testing.T) {
	// Boot-time misconfiguration must surface immediately at construction,
	// not as a nil-deref under the first inbound message. The strict-session
	// contract requires both callbacks; making one optional was the legacy
	// "right default for console" wart that hid silent failures.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil ResumeSession, got nothing")
		}
	}()
	NewMessageHandler(HandlerConfig{
		ResumeSession: nil,
		CreateSession: func(_, _, _, _ string) {},
		Runner:        &echoRunner{},
	})
}

func TestHandler_NewMessageHandler_PanicsOnNilCreateSession(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil CreateSession, got nothing")
		}
	}()
	NewMessageHandler(HandlerConfig{
		ResumeSession: func(_ string) error { return nil },
		CreateSession: nil,
		Runner:        &echoRunner{},
	})
}

func TestHandler_NewMessageHandler_PanicsOnNilRunner(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil Runner, got nothing")
		}
	}()
	NewMessageHandler(HandlerConfig{
		ResumeSession: func(_ string) error { return nil },
		CreateSession: func(_, _, _, _ string) {},
		Runner:        nil,
	})
}
