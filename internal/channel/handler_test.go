package channel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/orchestrator"
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
		CreateSession: func(state.SessionParams) {},
		Runner:        &echoRunner{},
		RunAction: func(_ context.Context, _, _ string, _ map[string]string) (string, error) {
			return "", errors.New("no actions")
		},
		HasAction: func(_, _ string) bool { return false },
		Opener:    state.NewSessionStore(""),
		SendToConversation: func(context.Context, string, string, pkg.OutboundMessage) error {
			return nil
		},
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
	cfg.CreateSession = func(p state.SessionParams) { createdGroup = p.GroupID }
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

// TestHandler_StampsKindAndSystemSourceFromProfile pins the other half of the
// session-labelling wiring: a session minted on this connection carries the
// verified profile's interaction kind AND its per-feature label, so a backend
// feature's own conversation is attributable and can be kept out of the
// customer's chat list. Without this the store would faithfully write an empty
// label forever and every store-level test would still pass.
func TestHandler_StampsKindAndSystemSourceFromProfile(t *testing.T) {
	var createdKind, createdSource string
	cfg := baseHandlerConfig()
	cfg.Verifier = &stubVerifier{p: &profile.Profile{
		EntityID: "u1", Kind: profile.KindSystem, SystemSource: "csv_mapping",
	}}
	cfg.CreateSession = func(p state.SessionParams) { createdKind, createdSource = p.Kind, p.SystemSource }
	h := NewMessageHandler(cfg)

	callHandler(h, map[string]string{"profile_token": "tok"})

	if createdKind != profile.KindSystem {
		t.Errorf("CreateSession kind = %q, want %q", createdKind, profile.KindSystem)
	}
	if createdSource != "csv_mapping" {
		t.Errorf("CreateSession systemSource = %q, want csv_mapping", createdSource)
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
	return func(p state.SessionParams) {
		r.creates = append(r.creates, p.ID)
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
		CreateSession: func(state.SessionParams) {},
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
		CreateSession: func(state.SessionParams) {},
		Runner:        nil,
	})
}

// friendlyError turns a provider failure into the message the customer reads
// and the error_code the frontend translates (docs/websocket-messages.md).
// It had no coverage, and the "conversation too long" case used to match two
// phrases of its own — so wordings the routing layer correctly recognised as an
// oversized prompt still reached the user as "something went wrong".
func TestFriendlyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"vllm/ovh too long", errors.New(`openai api error (status 400): {"message":"Input length (132235) exceeds model's maximum context length (131072)."}`), "context_length_exceeded"},
		{"anthropic too long", errors.New("prompt is too long: 200000 tokens > 199999 maximum"), "context_length_exceeded"},
		{"openai too long", errors.New("This model's maximum context length is 8192 tokens. However, your messages resulted in 8500 tokens."), "context_length_exceeded"},
		{"rate limit", errors.New("openai api error (status 429): rate_limit exceeded"), "rate_limited"},
		{"timeout", errors.New("context deadline exceeded"), "timeout"},
		// The dedicated-node outage that masked a too-long prompt on
		// 2026-08-01 must NOT be dressed up as "start a new conversation".
		{"endpoint outage", errors.New(`openai api error (status 400): {"message":"app has no replica running"}`), "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, code := friendlyError(tc.err)
			if code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
			if msg == "" {
				t.Error("expected a user-facing message")
			}
		})
	}
}

// shortOpenerTiming shortens the claim timing so a stale claim and its
// scheduled recovery happen within a test. NewMessageHandler reads it, so call
// it before building the handler.
func shortOpenerTiming(t *testing.T) {
	t.Helper()
	timeout, stale, leeway := openerTimeout, openerStaleAfter, recoveryLeeway
	openerTimeout, openerStaleAfter, recoveryLeeway = time.Second, 100*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { openerTimeout, openerStaleAfter, recoveryLeeway = timeout, stale, leeway })
}

// pastRecovery outlasts any recovery the handler scheduled, several times over.
func pastRecovery() time.Duration { return 5 * (openerStaleAfter + recoveryLeeway) }

type sessionRun struct {
	sessionID string
	content   string
	ctx       context.Context
}

type sentFrame struct {
	channel string
	msg     pkg.OutboundMessage
}

// openRecorder is the open-session test double. Its Run stores the turn's user
// message as the orchestrator does (visibility from ctx, under the turn's
// claim), so the store's claims see the turn as stored; it records, in order,
// every turn, every frame sent to a session and every CreateSession. With
// holdAck set, the answer to that open_request_id waits for release before it
// is sent, as a stalled channel would.
type openRecorder struct {
	store   *state.SessionStore
	runErr  error
	holdAck string
	release chan struct{}

	mu     sync.Mutex
	events []string
	runs   []sessionRun
	frames []sentFrame
	minted []string
}

func (r *openRecorder) Run(ctx context.Context, sessionKey, content string, _ ...pkg.FileAttachment) (string, string, map[string]string, error) {
	var meta map[string]string
	if c, ok := orchestrator.TurnClaimFrom(ctx); ok {
		meta = c.Metadata()
	}
	err := r.store.AddMessageWithMetadata(sessionKey, provider.Message{
		Role: provider.RoleUser, Content: content, Visibility: actor.Visibility(ctx),
	}, meta)
	r.mu.Lock()
	r.events = append(r.events, "run")
	r.runs = append(r.runs, sessionRun{sessionID: sessionKey, content: content, ctx: ctx})
	r.mu.Unlock()
	if err != nil {
		return "", "", nil, err
	}
	if r.runErr != nil {
		return "", "", nil, r.runErr
	}
	return "reply: " + content, "", nil, nil
}

func (r *openRecorder) send(_ context.Context, channel, conversation string, msg pkg.OutboundMessage) error {
	if conversation != msg.ConversationID {
		return fmt.Errorf("sent to conversation %q a frame of %q", conversation, msg.ConversationID)
	}
	kind := msg.Metadata["type"]
	if kind == "" {
		kind = "reply"
	}
	if kind == pkg.FrameSessionOpened && r.holdAck != "" && msg.Metadata[pkg.OpenRequestIDMetadataKey] == r.holdAck {
		<-r.release
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "send "+kind)
	r.frames = append(r.frames, sentFrame{channel: channel, msg: msg})
	return nil
}

func (r *openRecorder) snapshot() (events []string, runs []sessionRun, frames []sentFrame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events), slices.Clone(r.runs), slices.Clone(r.frames)
}

// acks returns the session_opened frames sent so far, in order.
func (r *openRecorder) acks() []pkg.OutboundMessage {
	_, _, frames := r.snapshot()
	var out []pkg.OutboundMessage
	for _, f := range frames {
		if f.msg.Metadata["type"] == pkg.FrameSessionOpened {
			out = append(out, f.msg)
		}
	}
	return out
}

func (r *openRecorder) waitForRuns(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, runs, _ := r.snapshot(); len(runs) >= n {
			return
		}
	}
	t.Fatalf("waited for %d turns, the runner ran fewer", n)
}

// systemVerifier answers a fresh copy of p for every token, so concurrent calls
// never share the profile the handler stamps.
type systemVerifier struct{ p profile.Profile }

func (v systemVerifier) Verify(context.Context, string, string, map[string]string) (*profile.Profile, error) {
	p := v.p
	return &p, nil
}

// failingOpener fails every store call, as a store whose database is gone.
type failingOpener struct{}

func (failingOpener) Identity(context.Context, string) (*state.Session, error) {
	return nil, errors.New("database connection lost")
}

func (failingOpener) OpenSession(context.Context, state.SessionParams) (*state.Session, bool, error) {
	return nil, false, errors.New("database connection lost")
}

func (failingOpener) ClaimOpener(context.Context, string, time.Time, time.Duration) (state.ClaimResult, error) {
	return state.ClaimHeld, errors.New("database connection lost")
}

func (failingOpener) ReserveNote(context.Context, string, string, time.Time, time.Duration) (state.ClaimResult, error) {
	return state.ClaimHeld, errors.New("database connection lost")
}

var welcomeProfile = profile.Profile{EntityID: "u1", Group: "g1", Kind: profile.KindSystem, SystemSource: "welcome_label"}

const openedSession = "u1:websocket:c1"

func newOpenHandler(p profile.Profile, override ...func(*HandlerConfig)) (*openRecorder, pkg.MessageHandler) {
	rec := &openRecorder{store: state.NewSessionStore("")}
	cfg := baseHandlerConfig()
	cfg.Verifier = systemVerifier{p: p}
	cfg.Runner = rec
	cfg.Opener = rec.store
	cfg.SendToConversation = rec.send
	cfg.ResumeSession = func(key string) error {
		_, err := rec.store.Get(key)
		return err
	}
	cfg.CreateSession = func(p state.SessionParams) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.minted = append(rec.minted, p.ID)
	}
	for _, o := range override {
		o(&cfg)
	}
	return rec, NewMessageHandler(cfg)
}

// openRequest is an open-session message on the minted conversation conv; set
// overrides metadata keys, and an empty value removes one.
func openRequest(conv string, set map[string]string) pkg.InboundMessage {
	meta := map[string]string{
		"profile_token":              "tok",
		pkg.OpenSessionMetadataKey:   "true",
		pkg.OpenedByMetadataKey:      "welcome",
		pkg.OpenKeyMetadataKey:       "event-1",
		pkg.OpenRequestIDMetadataKey: "req-" + conv,
	}
	for k, v := range set {
		if v == "" {
			delete(meta, k)
		} else {
			meta[k] = v
		}
	}
	return pkg.InboundMessage{
		ChannelID: "websocket", ConversationID: conv,
		Content: "facts and the instruction for the first turn", Metadata: meta,
	}
}

func callOpen(h pkg.MessageHandler, conv string, set map[string]string) pkg.OutboundMessage {
	out, _ := h(context.Background(), "websocket:"+conv, openRequest(conv, set))
	return out
}

// storeOpened opens session id of entity as the open path does, on the channel
// the id names after the entity.
func storeOpened(t *testing.T, store *state.SessionStore, id, entity string) {
	t.Helper()
	channel, _, _ := strings.Cut(strings.TrimPrefix(id, entity+":"), ":")
	if _, created, err := store.OpenSession(context.Background(), state.SessionParams{
		ID: id, EntityID: entity, OpenedBy: "welcome", OpenKey: "event-1", ChannelID: channel,
	}); err != nil || !created {
		t.Fatalf("OpenSession(%s) = created %v, %v", id, created, err)
	}
}

// TestHandler_OpenSession_OpensAndRunsHiddenFirstTurn: a verified system
// invocation opens the user's session through the store, never through Create
// (which would stamp the token's system kind on it), answers the caller before
// the model runs, and runs the content once as a hidden turn under the system
// profile, whose kind and label the turn's usage row records. The reply reaches
// the session's conversation without the attempt's correlation id.
func TestHandler_OpenSession_OpensAndRunsHiddenFirstTurn(t *testing.T) {
	rec, h := newOpenHandler(welcomeProfile)

	if out := callOpen(h, "c1", nil); out.Content != "" || len(out.Metadata) != 0 {
		t.Errorf("handler frame = %+v, want empty: the answer and the reply are sent to the session", out)
	}

	sess, err := rec.store.Get(openedSession)
	if err != nil {
		t.Fatalf("session not opened: %v", err)
	}
	if sess.OpenedBy != "welcome" || sess.OpenKey != "event-1" {
		t.Errorf("session origin/key = %q/%q, want welcome/event-1", sess.OpenedBy, sess.OpenKey)
	}
	if len(rec.minted) != 0 {
		t.Errorf("CreateSession called for %v; the open path must not mint a system-stamped session", rec.minted)
	}
	if len(sess.Messages) != 1 || sess.Messages[0].Visibility != provider.VisibilityHidden {
		t.Fatalf("stored messages = %+v, want the one hidden first turn", sess.Messages)
	}

	events, runs, frames := rec.snapshot()
	if want := []string{"send " + pkg.FrameSessionOpened, "run", "send reply"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v (the answer before the model runs)", events, want)
	}
	ack := frames[0]
	if ack.channel != "websocket" || ack.msg.ConversationID != "c1" ||
		ack.msg.Metadata[pkg.OpenSessionIDMetadataKey] != openedSession ||
		ack.msg.Metadata["created"] != "true" || ack.msg.Metadata[pkg.OpenRequestIDMetadataKey] != "req-c1" {
		t.Errorf("answer = %+v, want session_opened for %s, created=true, open_request_id=req-c1", ack, openedSession)
	}
	run := runs[0]
	if run.sessionID != openedSession || actor.Visibility(run.ctx) != provider.VisibilityHidden ||
		actor.ConversationID(run.ctx) != "c1" {
		t.Errorf("turn ran on %q (visibility %q, conversation %q), want a hidden turn of %s",
			run.sessionID, actor.Visibility(run.ctx), actor.ConversationID(run.ctx), openedSession)
	}
	if p := profile.FromContext(run.ctx); p == nil || p.Kind != profile.KindSystem || p.SystemSource != "welcome_label" {
		t.Errorf("turn profile = %+v, want kind system with its label", p)
	}
	reply := frames[1]
	if reply.channel != "websocket" || reply.msg.ConversationID != "c1" || reply.msg.Content != "reply: "+run.content {
		t.Errorf("reply = %+v, want the turn's reply to the session's conversation", reply)
	}
	if _, ok := reply.msg.Metadata[pkg.OpenRequestIDMetadataKey]; ok {
		t.Error("the reply carries open_request_id; only the answer to the attempt may")
	}
	// Both frames name the verified owner, which the sender keeps.
	for _, f := range frames {
		if owner := f.msg.Metadata[pkg.OwnerEntityMetadataKey]; owner != "u1" {
			t.Errorf("%s frame owner = %q, want u1", f.msg.Metadata["type"], owner)
		}
	}
}

// TestHandler_OpenSession_SameKeyAnswersFoundWithoutSecondTurn: a repeated
// request (a retried job, a lost answer) gets the stored session with
// created=false, and its first turn never runs again.
func TestHandler_OpenSession_SameKeyAnswersFoundWithoutSecondTurn(t *testing.T) {
	shortOpenerTiming(t)
	rec, h := newOpenHandler(welcomeProfile)

	callOpen(h, "c1", nil)
	callOpen(h, "c2", nil)
	time.Sleep(pastRecovery())

	acks := rec.acks()
	if len(acks) != 2 || acks[1].Metadata["created"] != "false" || acks[1].Metadata[pkg.OpenSessionIDMetadataKey] != openedSession {
		t.Fatalf("answers = %+v, want the second one created=false for %s", acks, openedSession)
	}
	if _, runs, _ := rec.snapshot(); len(runs) != 1 {
		t.Errorf("first turn ran %d times, want 1", len(runs))
	}
}

// TestHandler_OpenSession_RecoversAnInterruptedFirstTurnOnce: an attempt that
// claimed the first turn and died before it ran leaves a session without a
// message. The next identical request recovers it exactly once, on the stored
// session: after the claim's expiry when the claim is still fresh, answered
// created=false, at once when it is already stale, answered created=true since
// that request takes the claim and runs the turn.
func TestHandler_OpenSession_RecoversAnInterruptedFirstTurnOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{"retry while the claim is fresh", false},
		{"retry after the claim went stale", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortOpenerTiming(t)
			rec, h := newOpenHandler(welcomeProfile)
			storeOpened(t, rec.store, openedSession, "u1")
			if result, err := rec.store.ClaimOpener(context.Background(), openedSession, time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
				t.Fatalf("crashed attempt's claim = %v, %v", result, err)
			}
			if tc.stale {
				time.Sleep(2 * openerStaleAfter)
			}

			callOpen(h, "c2", nil)
			if acks := rec.acks(); len(acks) != 1 || acks[0].Metadata["created"] != strconv.FormatBool(tc.stale) ||
				acks[0].Metadata[pkg.OpenSessionIDMetadataKey] != openedSession {
				t.Fatalf("answers = %+v, want created=%v for %s", acks, tc.stale, openedSession)
			}
			if _, runs, _ := rec.snapshot(); (len(runs) == 1) != tc.stale {
				t.Fatalf("turns right after the retry = %d, want %d", len(runs), map[bool]int{true: 1}[tc.stale])
			}
			rec.waitForRuns(t, 1)
			time.Sleep(pastRecovery())

			_, runs, frames := rec.snapshot()
			if len(runs) != 1 || runs[0].sessionID != openedSession {
				t.Fatalf("turns = %+v, want exactly one on %s", runs, openedSession)
			}
			if last := frames[len(frames)-1]; last.channel != "websocket" || last.msg.ConversationID != "c1" {
				t.Errorf("reply went to %q conversation %q, want the stored session's conversation c1",
					last.channel, last.msg.ConversationID)
			}
		})
	}
}

// TestHandler_OpenSession_ConcurrentDuplicatesRunOneFirstTurn: simultaneous
// requests for one key get one created and the rest found, all for the same
// session, and one first turn runs.
func TestHandler_OpenSession_ConcurrentDuplicatesRunOneFirstTurn(t *testing.T) {
	shortOpenerTiming(t)
	rec, h := newOpenHandler(welcomeProfile)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { callOpen(h, fmt.Sprintf("c%d", i), nil) })
	}
	wg.Wait()
	time.Sleep(pastRecovery())

	acks := rec.acks()
	created := 0
	for _, a := range acks {
		if a.Metadata["created"] == "true" {
			created++
		}
		if a.Metadata[pkg.OpenSessionIDMetadataKey] != acks[0].Metadata[pkg.OpenSessionIDMetadataKey] {
			t.Errorf("answers name different sessions: %+v", acks)
		}
	}
	if len(acks) != 8 || created != 1 {
		t.Errorf("answers = %d with %d created, want 8 with 1 created", len(acks), created)
	}
	if _, runs, _ := rec.snapshot(); len(runs) != 1 {
		t.Errorf("first turn ran %d times, want 1", len(runs))
	}
}

// TestHandler_OpenSession_ModelErrorReachesTheConversation: a first turn that
// fails at the model answers the channel's error frame to the session's
// conversation, without the correlation id the answer before it carried.
func TestHandler_OpenSession_ModelErrorReachesTheConversation(t *testing.T) {
	rec, h := newOpenHandler(welcomeProfile)
	rec.runErr = errors.New("openai api error (status 429): rate_limit exceeded")

	callOpen(h, "c1", nil)

	events, _, frames := rec.snapshot()
	if want := []string{"send " + pkg.FrameSessionOpened, "run", "send error"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	errFrame := frames[1]
	if errFrame.channel != "websocket" || errFrame.msg.ConversationID != "c1" ||
		errFrame.msg.Metadata["error_code"] != "rate_limited" {
		t.Errorf("error frame = %+v, want rate_limited to the session's conversation", errFrame)
	}
	if _, ok := errFrame.msg.Metadata[pkg.OpenRequestIDMetadataKey]; ok {
		t.Error("the error frame carries open_request_id; only the answer to the attempt may")
	}
}

// TestHandler_OpenSession_NoteRunsOncePerKey: the continuation form posts the
// content as a hidden note into the user's session, keyed so the store keeps
// one note per key; a repeated key answers created=false and runs nothing.
func TestHandler_OpenSession_NoteRunsOncePerKey(t *testing.T) {
	rec, h := newOpenHandler(welcomeProfile)
	storeOpened(t, rec.store, openedSession, "u1")
	note := map[string]string{
		pkg.OpenSessionIDMetadataKey: openedSession, pkg.NoteKeyMetadataKey: "chapter-2",
		pkg.OpenedByMetadataKey: "", pkg.OpenKeyMetadataKey: "",
	}

	callOpen(h, "c8", note)
	callOpen(h, "c9", note)

	acks := rec.acks()
	if len(acks) != 2 || acks[0].Metadata["created"] != "true" || acks[1].Metadata["created"] != "false" ||
		acks[1].Metadata[pkg.OpenSessionIDMetadataKey] != openedSession {
		t.Fatalf("answers = %+v, want created=true then created=false for %s", acks, openedSession)
	}
	_, runs, frames := rec.snapshot()
	if len(runs) != 1 {
		t.Fatalf("note turns = %d, want 1", len(runs))
	}
	claim, _ := orchestrator.TurnClaimFrom(runs[0].ctx)
	if r := runs[0]; r.sessionID != openedSession || claim.NoteKey != "chapter-2" ||
		actor.Visibility(r.ctx) != provider.VisibilityHidden || actor.ConversationID(r.ctx) != "c1" {
		t.Errorf("note ran on %q (key %q, visibility %q, conversation %q), want a hidden keyed turn of %s",
			r.sessionID, claim.NoteKey, actor.Visibility(r.ctx), actor.ConversationID(r.ctx), openedSession)
	}
	if reply := frames[1]; reply.channel != "websocket" || reply.msg.ConversationID != "c1" {
		t.Errorf("note reply = %+v, want it in the session's conversation c1", reply)
	}
}

// countStored is how many of session id's stored messages carry content.
func countStored(t *testing.T, store *state.SessionStore, id, content string) int {
	t.Helper()
	sess, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	n := 0
	for _, m := range sess.Messages {
		if m.Content == content {
			n++
		}
	}
	return n
}

// replies counts the turn replies (frames without a type) sent so far.
func (r *openRecorder) replies() int {
	_, _, frames := r.snapshot()
	n := 0
	for _, f := range frames {
		if f.msg.Metadata["type"] == "" {
			n++
		}
	}
	return n
}

// lateClaimOpener grants the first ClaimOpener call late: the call's now ages
// while the write waits (a row lock, an exhausted connection pool), so the
// claim it writes is stale at once; the call then returns only on release.
type lateClaimOpener struct {
	*state.SessionStore
	once    sync.Once
	claimed chan struct{}
	release chan struct{}
}

func (o *lateClaimOpener) ClaimOpener(ctx context.Context, id string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error) {
	first := false
	o.once.Do(func() { first = true })
	if !first {
		return o.SessionStore.ClaimOpener(ctx, id, now, staleAfter)
	}
	time.Sleep(2 * staleAfter)
	result, err := o.SessionStore.ClaimOpener(ctx, id, now, staleAfter)
	close(o.claimed)
	<-o.release
	return result, err
}

// syncBuffer collects log output written from the handler's goroutines and
// timers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs routes the default logger, Info and above, into a buffer for the
// test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// TestHandler_OpenSession_FirstTurnClaimTakenOverRunsOnce: a claim granted
// after it went stale is taken over by the next request, which runs the first
// turn; when the late claimant gets to its turn, the store refuses its claim,
// so it stores nothing and answers nothing, and the takeover is logged at
// Info, not as a failed run.
func TestHandler_OpenSession_FirstTurnClaimTakenOverRunsOnce(t *testing.T) {
	shortOpenerTiming(t)
	logs := captureLogs(t)
	op := &lateClaimOpener{claimed: make(chan struct{}), release: make(chan struct{})}
	rec, h := newOpenHandler(welcomeProfile, func(cfg *HandlerConfig) {
		op.SessionStore = cfg.Opener.(*state.SessionStore)
		cfg.Opener = op
	})

	late := make(chan struct{})
	go func() { callOpen(h, "c1", nil); close(late) }()
	<-op.claimed
	callOpen(h, "c2", nil)
	close(op.release)
	<-late
	time.Sleep(pastRecovery())

	content := openRequest("c1", nil).Content
	if n := countStored(t, rec.store, openedSession, content); n != 1 {
		t.Errorf("first turn stored %d times, want once", n)
	}
	if n := rec.replies(); n != 1 {
		t.Errorf("replies = %d, want 1", n)
	}
	if out := logs.String(); !strings.Contains(out, "turn taken over by another run") || strings.Contains(out, "level=ERROR") {
		t.Errorf("log:\n%s\nwant the takeover at Info and no error", out)
	}
}

// TestHandler_OpenSession_NoteReservationTakenOverRunsOnce: a note whose
// answer stalls past its reservation's expiry is taken over by a retry, which
// stores and answers the note; when the stalled request gets to its turn, the
// store refuses its reservation, so the note is stored and answered once.
func TestHandler_OpenSession_NoteReservationTakenOverRunsOnce(t *testing.T) {
	shortOpenerTiming(t)
	rec, h := newOpenHandler(welcomeProfile)
	rec.holdAck, rec.release = "req-c8", make(chan struct{})
	storeOpened(t, rec.store, openedSession, "u1")
	note := map[string]string{
		pkg.OpenSessionIDMetadataKey: openedSession, pkg.NoteKeyMetadataKey: "chapter-2",
		pkg.OpenedByMetadataKey: "", pkg.OpenKeyMetadataKey: "",
	}

	stalled := make(chan struct{})
	go func() { callOpen(h, "c8", note); close(stalled) }()
	time.Sleep(2 * openerStaleAfter)
	callOpen(h, "c9", note)
	close(rec.release)
	<-stalled
	time.Sleep(pastRecovery())

	content := openRequest("c8", note).Content
	if n := countStored(t, rec.store, openedSession, content); n != 1 {
		t.Errorf("note stored %d times, want once", n)
	}
	if n := rec.replies(); n != 1 {
		t.Errorf("replies = %d, want 1", n)
	}
}

// TestHandler_OpenSession_RecoversAnInterruptedNoteOnce: an attempt that
// reserved a note and died before it stored the note leaves the reservation
// held. A retry while it is fresh is answered created=false and recovers the
// note once the reservation is stale: the note is stored and answered once.
func TestHandler_OpenSession_RecoversAnInterruptedNoteOnce(t *testing.T) {
	shortOpenerTiming(t)
	rec, h := newOpenHandler(welcomeProfile)
	storeOpened(t, rec.store, openedSession, "u1")
	if result, err := rec.store.ReserveNote(context.Background(), openedSession, "chapter-2", time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
		t.Fatalf("crashed attempt's reservation = %v, %v", result, err)
	}
	note := map[string]string{
		pkg.OpenSessionIDMetadataKey: openedSession, pkg.NoteKeyMetadataKey: "chapter-2",
		pkg.OpenedByMetadataKey: "", pkg.OpenKeyMetadataKey: "",
	}

	callOpen(h, "c8", note)
	if acks := rec.acks(); len(acks) != 1 || acks[0].Metadata["created"] != "false" {
		t.Fatalf("answers = %+v, want created=false", acks)
	}
	if _, runs, _ := rec.snapshot(); len(runs) != 0 {
		t.Fatalf("the note ran while the reservation was fresh: %+v", runs)
	}
	rec.waitForRuns(t, 1)
	time.Sleep(pastRecovery())

	_, runs, _ := rec.snapshot()
	if claim, _ := orchestrator.TurnClaimFrom(runs[0].ctx); len(runs) != 1 || runs[0].sessionID != openedSession || claim.NoteKey != "chapter-2" {
		t.Fatalf("turns = %+v, want one recovered note chapter-2 on %s", runs, openedSession)
	}
	if n := countStored(t, rec.store, openedSession, openRequest("c8", note).Content); n != 1 {
		t.Errorf("note stored %d times, want once", n)
	}
	if n := rec.replies(); n != 1 {
		t.Errorf("replies = %d, want 1", n)
	}
}

// flakyOpener fails claim call number failAt (ClaimOpener and ReserveNote
// counted together), as a database that is briefly unavailable.
type flakyOpener struct {
	*state.SessionStore
	calls  atomic.Int32
	failAt int32
}

func (o *flakyOpener) ClaimOpener(ctx context.Context, id string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error) {
	if o.calls.Add(1) == o.failAt {
		return state.ClaimHeld, errors.New("database unavailable")
	}
	return o.SessionStore.ClaimOpener(ctx, id, now, staleAfter)
}

func (o *flakyOpener) ReserveNote(ctx context.Context, id, key string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error) {
	if o.calls.Add(1) == o.failAt {
		return state.ClaimHeld, errors.New("database unavailable")
	}
	return o.SessionStore.ReserveNote(ctx, id, key, now, staleAfter)
}

// TestHandler_OpenSession_FailedClaimIsRefusedThenRetried: an open whose
// first-turn claim the store cannot answer is refused with internal_error
// before any answer or run; the session it opened stays, and a retry with the
// same key takes the claim, is answered created=true and runs the turn once.
func TestHandler_OpenSession_FailedClaimIsRefusedThenRetried(t *testing.T) {
	shortOpenerTiming(t)
	op := &flakyOpener{failAt: 1}
	rec, h := newOpenHandler(welcomeProfile, func(cfg *HandlerConfig) {
		op.SessionStore = cfg.Opener.(*state.SessionStore)
		cfg.Opener = op
	})

	out := callOpen(h, "c1", nil)
	if out.Metadata["type"] != pkg.FrameOpenRejected || out.Metadata["error_code"] != "internal_error" ||
		out.Metadata[pkg.OpenRequestIDMetadataKey] != "req-c1" {
		t.Fatalf("answer = %+v, want open_rejected internal_error with req-c1", out.Metadata)
	}
	if events, _, _ := rec.snapshot(); len(events) != 0 {
		t.Fatalf("events = %v, want nothing sent or run", events)
	}

	callOpen(h, "c2", nil)
	time.Sleep(pastRecovery())
	if acks := rec.acks(); len(acks) != 1 || acks[0].Metadata["created"] != "true" ||
		acks[0].Metadata[pkg.OpenSessionIDMetadataKey] != openedSession {
		t.Errorf("answers = %+v, want the retry's created=true for %s", acks, openedSession)
	}
	if _, runs, _ := rec.snapshot(); len(runs) != 1 || runs[0].sessionID != openedSession {
		t.Errorf("turns = %+v, want one on %s", runs, openedSession)
	}
}

// stuckOpener's open (stuck "open"), a note's lookup (stuck "lookup") or the
// first-turn claim (stuck "claim") waits until its context ends, as an open
// waits for another transaction's insert of its key, a lookup for a table lock
// or a free connection, and a claim for a session row another transaction
// holds, and records whether that context carried a deadline.
type stuckOpener struct {
	*state.SessionStore
	stuck    string
	deadline atomic.Bool
}

func (o *stuckOpener) wait(ctx context.Context) error {
	_, bounded := ctx.Deadline()
	o.deadline.Store(bounded)
	<-ctx.Done()
	return ctx.Err()
}

func (o *stuckOpener) OpenSession(ctx context.Context, p state.SessionParams) (*state.Session, bool, error) {
	if o.stuck == "open" {
		return nil, false, o.wait(ctx)
	}
	return o.SessionStore.OpenSession(ctx, p)
}

func (o *stuckOpener) Identity(ctx context.Context, id string) (*state.Session, error) {
	if o.stuck == "lookup" {
		return nil, o.wait(ctx)
	}
	return o.SessionStore.Identity(ctx, id)
}

func (o *stuckOpener) ClaimOpener(ctx context.Context, id string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error) {
	if o.stuck == "claim" {
		return state.ClaimHeld, o.wait(ctx)
	}
	return o.SessionStore.ClaimOpener(ctx, id, now, staleAfter)
}

// TestHandler_OpenSession_StoreWaitIsBounded: an open, a note's lookup or a
// claim whose store call waits gives up once the turn timeout passed, or as
// soon as the request's context ends; the attempt is refused with
// internal_error and nothing is sent or run.
func TestHandler_OpenSession_StoreWaitIsBounded(t *testing.T) {
	shortOpenerTiming(t)
	for _, tc := range []struct {
		name, stuck string
		cancel      bool
	}{
		{"open, turn timeout", "open", false},
		{"open, request ends", "open", true},
		{"note lookup, turn timeout", "lookup", false},
		{"note lookup, request ends", "lookup", true},
		{"claim, turn timeout", "claim", false},
		{"claim, request ends", "claim", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := &stuckOpener{stuck: tc.stuck}
			rec, h := newOpenHandler(welcomeProfile, func(cfg *HandlerConfig) {
				op.SessionStore = cfg.Opener.(*state.SessionStore)
				cfg.Opener = op
			})
			req := openRequest("c1", nil)
			if tc.stuck == "lookup" {
				storeOpened(t, rec.store, openedSession, "u1")
				req = openRequest("c1", map[string]string{
					pkg.OpenSessionIDMetadataKey: openedSession, pkg.NoteKeyMetadataKey: "chapter-2",
					pkg.OpenedByMetadataKey: "", pkg.OpenKeyMetadataKey: "",
				})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				time.AfterFunc(100*time.Millisecond, cancel)
			}

			start := time.Now()
			out, _ := h(ctx, "websocket:c1", req)
			waited := time.Since(start)

			if out.Metadata["type"] != pkg.FrameOpenRejected || out.Metadata["error_code"] != "internal_error" {
				t.Errorf("answer = %+v, want open_rejected internal_error", out.Metadata)
			}
			if !op.deadline.Load() {
				t.Errorf("the %s's context carries no deadline", tc.stuck)
			}
			if limit := openerTimeout + time.Second; tc.cancel && waited >= openerTimeout || waited > limit {
				t.Errorf("the attempt returned after %v", waited)
			}
			if events, _, _ := rec.snapshot(); len(events) != 0 {
				t.Errorf("events = %v, want nothing sent or run", events)
			}
		})
	}
}

// TestHandler_OpenSession_RecoveryOutlivesAFailedClaim: a recovery whose claim
// call fails looks again later instead of giving up, so a note acknowledged
// while a dead attempt held its reservation still runs once.
func TestHandler_OpenSession_RecoveryOutlivesAFailedClaim(t *testing.T) {
	shortOpenerTiming(t)
	op := &flakyOpener{failAt: 2} // 1: the request's own reservation, 2: the first recovery
	rec, h := newOpenHandler(welcomeProfile, func(cfg *HandlerConfig) {
		op.SessionStore = cfg.Opener.(*state.SessionStore)
		cfg.Opener = op
	})
	storeOpened(t, rec.store, openedSession, "u1")
	if result, err := rec.store.ReserveNote(context.Background(), openedSession, "chapter-2", time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
		t.Fatalf("crashed attempt's reservation = %v, %v", result, err)
	}
	note := map[string]string{
		pkg.OpenSessionIDMetadataKey: openedSession, pkg.NoteKeyMetadataKey: "chapter-2",
		pkg.OpenedByMetadataKey: "", pkg.OpenKeyMetadataKey: "",
	}

	callOpen(h, "c8", note)
	rec.waitForRuns(t, 1)
	time.Sleep(pastRecovery())

	if _, runs, _ := rec.snapshot(); len(runs) != 1 {
		t.Errorf("note ran %d times, want 1", len(runs))
	}
	if n := countStored(t, rec.store, openedSession, openRequest("c8", note).Content); n != 1 {
		t.Errorf("note stored %d times, want once", n)
	}
}

// countingOpener counts the first-turn claims that reach the store.
type countingOpener struct {
	*state.SessionStore
	claims atomic.Int32
}

func (o *countingOpener) ClaimOpener(ctx context.Context, id string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error) {
	o.claims.Add(1)
	return o.SessionStore.ClaimOpener(ctx, id, now, staleAfter)
}

// TestHandler_OpenSession_RecoveryOnlyForAHeldClaimAndOnce: duplicates of a
// request whose first turn ran schedule no recovery; duplicates that find the
// claim held by a dead attempt schedule one recovery between them, which runs
// the first turn once.
func TestHandler_OpenSession_RecoveryOnlyForAHeldClaimAndOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ran        bool
		recoveries int32
	}{
		{"first turn ran", true, 0},
		{"claim held by a dead attempt", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortOpenerTiming(t)
			op := &countingOpener{}
			rec, h := newOpenHandler(welcomeProfile, func(cfg *HandlerConfig) {
				op.SessionStore = cfg.Opener.(*state.SessionStore)
				cfg.Opener = op
			})
			if tc.ran {
				callOpen(h, "c1", nil)
			} else {
				storeOpened(t, rec.store, openedSession, "u1")
				if result, err := rec.store.ClaimOpener(context.Background(), openedSession, time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
					t.Fatalf("dead attempt's claim = %v, %v", result, err)
				}
			}
			before := op.claims.Load()

			const duplicates = 6
			var wg sync.WaitGroup
			for i := range duplicates {
				wg.Go(func() { callOpen(h, fmt.Sprintf("d%d", i), nil) })
			}
			wg.Wait()
			time.Sleep(pastRecovery())

			if got := op.claims.Load() - before - duplicates; got != tc.recoveries {
				t.Errorf("recovery claims = %d, want %d", got, tc.recoveries)
			}
			if _, runs, _ := rec.snapshot(); len(runs) != 1 {
				t.Errorf("first turn ran %d times, want 1", len(runs))
			}
		})
	}
}

// TestHandler_OpenSession_ReplyReachesItsConversation: the answer goes to the
// request's own conversation and the turn, run on the session's whole key, to
// the session's conversation, the part after "<entity>:<channel>:", on the
// request's channel. The turn runs with that channel and conversation, the
// address work it defers (a reminder) is delivered to.
func TestHandler_OpenSession_ReplyReachesItsConversation(t *testing.T) {
	note := func(target string) map[string]string {
		return map[string]string{
			pkg.OpenSessionIDMetadataKey: target, pkg.NoteKeyMetadataKey: "chapter-2",
			pkg.OpenedByMetadataKey: "", pkg.OpenKeyMetadataKey: "",
		}
	}
	for _, tc := range []struct {
		name          string
		channel, conv string
		target        string // a session the note goes to; empty for a first turn
		wantConv      string
	}{
		{name: "first turn", channel: "websocket", conv: "room-42", wantConv: "room-42"},
		{name: "first turn on another channel", channel: "ws-a", conv: "room-42", wantConv: "room-42"},
		{name: "note", channel: "websocket", conv: "c9", target: "u1:websocket:room-42", wantConv: "room-42"},
		{name: "note on another channel", channel: "ws-a", conv: "c9", target: "u1:ws-a:room-42", wantConv: "room-42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, h := newOpenHandler(welcomeProfile)
			session, set := "u1:"+pkg.SessionKey(tc.channel, tc.conv, ""), map[string]string(nil)
			if tc.target != "" {
				storeOpened(t, rec.store, tc.target, "u1")
				session, set = tc.target, note(tc.target)
			}
			msg := openRequest(tc.conv, set)
			msg.ChannelID = tc.channel
			_, _ = h(context.Background(), pkg.SessionKey(tc.channel, tc.conv, ""), msg)

			_, runs, frames := rec.snapshot()
			if len(runs) != 1 || runs[0].sessionID != session || actor.ConversationID(runs[0].ctx) != tc.wantConv ||
				profile.FromContext(runs[0].ctx).ChannelID != tc.channel {
				t.Fatalf("turns = %+v, want one on %s in %s/%q", runs, session, tc.channel, tc.wantConv)
			}
			if len(frames) != 2 {
				t.Fatalf("frames = %+v, want the answer and the reply", frames)
			}
			if ack := frames[0]; ack.channel != tc.channel || ack.msg.ConversationID != tc.conv {
				t.Errorf("answer to %s/%q, want the request's %s/%q", ack.channel, ack.msg.ConversationID, tc.channel, tc.conv)
			}
			reply := frames[1]
			if reply.channel != tc.channel || reply.msg.ConversationID != tc.wantConv || reply.msg.ThreadID != "" {
				t.Errorf("reply to %s/%q thread %q, want %s/%q", reply.channel, reply.msg.ConversationID,
					reply.msg.ThreadID, tc.channel, tc.wantConv)
			}
			for _, key := range []string{pkg.OpenSessionMetadataKey, pkg.OpenedByMetadataKey, pkg.OpenKeyMetadataKey,
				pkg.OpenSessionIDMetadataKey, pkg.NoteKeyMetadataKey} {
				if v, ok := reply.msg.Metadata[key]; ok {
					t.Errorf("reply carries the request's %s=%q", key, v)
				}
			}
		})
	}
}

// TestHandler_OpenSession_NoteForASessionOutOfReachIsNotFound: a note reaches
// a session the open path opened and the store records for the user and the
// request's channel only. A session that is gone, another user's, the user's
// on another channel, or an ordinary chat, also one whose channel the store
// backfilled, is refused with not_found, so the caller may open a new one, and
// nothing is stored, reserved, run or sent.
func TestHandler_OpenSession_NoteForASessionOutOfReachIsNotFound(t *testing.T) {
	rec, h := newOpenHandler(welcomeProfile)
	var foreign []string
	for _, s := range []struct{ id, owner, channel string }{
		{"u2:websocket:c1", "u2", "websocket"}, // another user's
		{"u1:slack:c1", "u1", "slack"},         // u1's on another channel
	} {
		if _, created, err := rec.store.OpenSession(context.Background(), state.SessionParams{
			ID: s.id, EntityID: s.owner, OpenedBy: "welcome", OpenKey: s.id, ChannelID: s.channel,
		}); err != nil || !created {
			t.Fatalf("OpenSession(%s) = created %v, %v", s.id, created, err)
		}
		foreign = append(foreign, s.id)
	}
	rec.store.Create(state.SessionParams{ID: "u1:websocket:c2", EntityID: "u1"}) // u1's chat, no recorded channel
	backfilled := rec.store.Create(state.SessionParams{ID: "u1:websocket:c3", EntityID: "u1"})
	backfilled.ChannelID = "websocket" // u1's chat on this channel, recorded by the store's backfill
	foreign = append(foreign, "u1:websocket:c2", "u1:websocket:c3")

	for _, target := range append([]string{"u1:websocket:gone"}, foreign...) {
		out := callOpen(h, "c8", map[string]string{pkg.OpenSessionIDMetadataKey: target, pkg.NoteKeyMetadataKey: "chapter-2"})
		if out.Metadata["type"] != pkg.FrameOpenRejected || out.Metadata["error_code"] != "not_found" ||
			out.Metadata[pkg.OpenRequestIDMetadataKey] != "req-c8" {
			t.Errorf("note for %s answered %+v, want open_rejected not_found with req-c8", target, out.Metadata)
		}
	}
	if events, _, _ := rec.snapshot(); len(events) != 0 {
		t.Errorf("events = %v, want nothing sent or run", events)
	}
	for _, id := range foreign {
		if sess, _ := rec.store.Get(id); len(sess.Messages) != 0 {
			t.Errorf("%s holds %+v, want nothing stored", id, sess.Messages)
		}
		if result, err := rec.store.ReserveNote(context.Background(), id, "chapter-2", time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
			t.Errorf("%s note reservation = %v, %v; want it still free", id, result, err)
		}
	}
}

// TestHandler_OpenSession_KeyPartsWithAColonAreRefused: a session key names its
// user, channel and conversation only while none of them is empty or holds a
// colon: "u1:websocket:websocket:c1" is u1's conversation "websocket:c1" and
// u1:websocket's conversation "c1". An open-session request whose entity,
// channel or conversation id is not such a part, and a note whose target is
// not a key of three such parts, is refused with bad_request naming the field
// before the store is asked: nothing is opened, stored, claimed, reserved, run
// or sent, also in the sessions those keys name.
func TestHandler_OpenSession_KeyPartsWithAColonAreRefused(t *testing.T) {
	note := func(target string) map[string]string {
		return map[string]string{pkg.OpenSessionIDMetadataKey: target, pkg.NoteKeyMetadataKey: "chapter-2"}
	}
	for _, tc := range []struct {
		name, entity, channel, conv string
		set                         map[string]string
		field                       string
	}{
		{"entity id with a colon", "u1:websocket", "websocket", "c1", nil, "entity id"},
		{"empty entity id", "", "websocket", "c1", nil, "entity id"},
		{"channel id with a colon", "u1", "websocket:admin", "c1", nil, "channel id"},
		{"conversation id with a colon", "u1", "websocket", "websocket:c1", nil, "conversation id"},
		{"note into a conversation id with a colon", "u1", "websocket", "c8", note("u1:websocket:websocket:c1"), "session id"},
		{"note on a channel id with a colon", "u1", "websocket", "c8", note("u1:websocket:admin:c1"), "session id"},
		{"note for a user id with a colon", "u1", "websocket", "c8", note("u1:other:websocket:c1"), "session id"},
		{"note for a key of no conversation", "u1", "websocket", "c8", note("u1-job-7"), "session id"},
		{"note for a key with an empty conversation", "u1", "websocket", "c8", note("u1:websocket:"), "session id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := welcomeProfile
			p.EntityID = tc.entity
			rec, h := newOpenHandler(p)
			// The sessions those keys name: u1:websocket's chat "c1", which is
			// also the key of u1's conversation "websocket:c1", u1's chat
			// "admin:c1", u1:other's chat "c1" and u1's session of no
			// conversation.
			existing := []state.SessionParams{
				{ID: "u1:websocket:websocket:c1", EntityID: "u1:websocket"},
				{ID: "u1:websocket:admin:c1", EntityID: "u1"},
				{ID: "u1:other:websocket:c1", EntityID: "u1:other"},
				{ID: "u1-job-7", EntityID: "u1"},
			}
			for _, e := range existing {
				rec.store.Create(e)
			}
			msg := openRequest(tc.conv, tc.set)
			msg.ChannelID = tc.channel

			out, _ := h(context.Background(), pkg.SessionKey(tc.channel, tc.conv, ""), msg)

			if out.Metadata["type"] != pkg.FrameOpenRejected || out.Metadata["error_code"] != "bad_request" ||
				out.Metadata[pkg.OpenRequestIDMetadataKey] != "req-"+tc.conv || !strings.Contains(out.Content, tc.field) {
				t.Errorf("answer = %q %+v, want open_rejected bad_request naming the %s", out.Content, out.Metadata, tc.field)
			}
			if events, _, _ := rec.snapshot(); len(events) != 0 || len(rec.minted) != 0 {
				t.Errorf("events = %v, minted = %v, want nothing", events, rec.minted)
			}
			if n := len(rec.store.List()); n != len(existing) {
				t.Errorf("store holds %d sessions, want the %d it had", n, len(existing))
			}
			for _, e := range existing {
				if sess, err := rec.store.Get(e.ID); err != nil || sess.OpenKey != "" || len(sess.Messages) != 0 {
					t.Errorf("%s = %+v, %v; want it unopened and empty", e.ID, sess, err)
				}
				if result, err := rec.store.ClaimOpener(context.Background(), e.ID, time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
					t.Errorf("%s first-turn claim = %v, %v; want it still free", e.ID, result, err)
				}
				if result, err := rec.store.ReserveNote(context.Background(), e.ID, "chapter-2", time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
					t.Errorf("%s note reservation = %v, %v; want it still free", e.ID, result, err)
				}
			}
		})
	}
}

// TestHandler_OpenSession_KeyWithoutAnOwnerIsNeitherReachedNorTaken: the
// handler keys a message without a verified profile "<channel>:<conversation>"
// with any thread after it, and such a key may read like a user's:
// "u1:websocket:c4" is thread c4 of conversation "websocket" on a channel u1.
// That session has no open key, so a note for it is not_found, and an open
// whose own key it is finds the id taken and answers internal_error; neither
// stores, claims, runs or sends anything in it, also when the store backfilled
// the key's first part as its owner and a channel for it.
func TestHandler_OpenSession_KeyWithoutAnOwnerIsNeitherReachedNorTaken(t *testing.T) {
	const id = "u1:websocket:c4"
	for _, tc := range []struct{ name, owner, channel string }{
		{"as keyed", "", ""},
		{"owner and channel backfilled", "u1", "websocket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, h := newOpenHandler(welcomeProfile)
			unowned := rec.store.Create(state.SessionParams{ID: id, EntityID: tc.owner})
			unowned.ChannelID = tc.channel

			for _, attempt := range []struct {
				conv string
				set  map[string]string
				code string
			}{
				{"c8", map[string]string{pkg.OpenSessionIDMetadataKey: id, pkg.NoteKeyMetadataKey: "chapter-2"}, "not_found"},
				{"c4", nil, "internal_error"}, // the open's own key is u1:websocket:c4
			} {
				out := callOpen(h, attempt.conv, attempt.set)
				if out.Metadata["type"] != pkg.FrameOpenRejected || out.Metadata["error_code"] != attempt.code {
					t.Errorf("attempt on %s answered %+v, want open_rejected %s", attempt.conv, out.Metadata, attempt.code)
				}
			}
			if events, _, _ := rec.snapshot(); len(events) != 0 || len(rec.minted) != 0 {
				t.Errorf("events = %v, minted = %v, want nothing", events, rec.minted)
			}
			if sess, err := rec.store.Get(id); err != nil || sess.OpenKey != "" || sess.EntityID != tc.owner || len(sess.Messages) != 0 {
				t.Errorf("%s = %+v, %v; want it as it was", id, sess, err)
			}
			if result, err := rec.store.ClaimOpener(context.Background(), id, time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
				t.Errorf("first-turn claim = %v, %v; want it still free", result, err)
			}
			if result, err := rec.store.ReserveNote(context.Background(), id, "chapter-2", time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
				t.Errorf("note reservation = %v, %v; want it still free", result, err)
			}
		})
	}
}

// TestHandler_OrdinaryChatTakesAConversationIdWithAColon: the open path's
// refusal leaves ordinary chats as they are: a chat's conversation id may hold
// a colon, and its session, keyed with it, is created, resumed and answered
// in that conversation.
func TestHandler_OrdinaryChatTakesAConversationIdWithAColon(t *testing.T) {
	chat := welcomeProfile
	chat.Kind, chat.SystemSource = profile.KindChat, ""
	var rec *openRecorder
	var h pkg.MessageHandler
	rec, h = newOpenHandler(chat, func(cfg *HandlerConfig) {
		mint := cfg.CreateSession
		cfg.CreateSession = func(p state.SessionParams) {
			mint(p)
			rec.store.Create(p)
		}
	})
	const conv, session = "websocket:c1", "u1:websocket:websocket:c1"
	for _, resume := range []bool{false, true} {
		meta := map[string]string{"profile_token": "tok"}
		if resume {
			meta[pkg.ResumeIntentMetadataKey] = "true"
		}
		msg := pkg.InboundMessage{ChannelID: "websocket", ConversationID: conv, Content: "hello", Metadata: meta}

		out, err := h(context.Background(), pkg.SessionKey("websocket", conv, ""), msg)

		if err != nil || out.Content != "reply: hello" || out.ConversationID != conv {
			t.Errorf("resume %v: answer = %q to %q, %v; want the reply in %q", resume, out.Content, out.ConversationID, err, conv)
		}
	}
	_, runs, _ := rec.snapshot()
	if len(runs) != 2 || runs[0].sessionID != session || runs[1].sessionID != session {
		t.Errorf("turns = %+v, want two in %s", runs, session)
	}
	if len(rec.minted) != 1 || rec.minted[0] != session {
		t.Errorf("minted = %v, want %s once", rec.minted, session)
	}
}

// TestHandler_OpenSession_Rejections: an attempt that may not or cannot open a
// session is answered with open_rejected and its correlation id, also when the
// refusal comes before the open path (the token check), and opens, claims,
// reserves, runs and sends nothing. A chat token is refused like any
// privileged option. A request on a channel with threads, even for the user's
// session there, in a thread, or with a channel id its key does not carry, is
// refused before the store is asked; the user's session for the origin and
// key on another channel is out of reach, also where that channel's id
// extends this one's.
func TestHandler_OpenSession_Rejections(t *testing.T) {
	chat := welcomeProfile
	chat.Kind, chat.SystemSource = profile.KindChat, ""
	unverifiable := func(cfg *HandlerConfig) { cfg.Verifier = &stubVerifier{err: errors.New("bad token")} }
	failingStore := func(cfg *HandlerConfig) { cfg.Opener = failingOpener{} }
	keep := func(*HandlerConfig) {}
	inThread := func(m *pkg.InboundMessage) { m.ThreadID = "t1" }
	misnamed := func(m *pkg.InboundMessage) { m.ChannelID = "websocket-2" }
	// The user's sessions for the origin and keys event-1 and event-2: on a
	// channel with threads, and on a channel whose id extends "websocket".
	elsewhere := map[string]state.SessionParams{
		"u1:slack:c0":           {OpenKey: "event-1", ChannelID: "slack"},
		"u1:websocket:admin:c0": {OpenKey: "event-2", ChannelID: "websocket:admin"},
	}
	note := map[string]string{pkg.OpenSessionIDMetadataKey: "u1:slack:c0", pkg.NoteKeyMetadataKey: "chapter-2"}
	for _, tc := range []struct {
		name            string
		p               profile.Profile
		channel         string
		threads         bool
		mutate          func(*pkg.InboundMessage)
		set             map[string]string
		override        func(*HandlerConfig)
		code, requestID string
	}{
		{"unverifiable token", welcomeProfile, "websocket", false, nil, nil, unverifiable, "auth_failed", "req-c1"},
		{"chat token", chat, "websocket", false, nil, nil, keep, "forbidden", "req-c1"},
		{"missing key", welcomeProfile, "websocket", false, nil, map[string]string{pkg.OpenKeyMetadataKey: ""}, keep, "bad_request", "req-c1"},
		{"missing request id", welcomeProfile, "websocket", false, nil, map[string]string{pkg.OpenRequestIDMetadataKey: ""}, keep, "bad_request", ""},
		{"store failure", welcomeProfile, "websocket", false, nil, nil, failingStore, "internal_error", "req-c1"},
		{"session on another channel", welcomeProfile, "websocket", false, nil, nil, keep, "not_found", "req-c1"},
		{"session on a channel whose id extends this one's", welcomeProfile, "websocket", false, nil,
			map[string]string{pkg.OpenKeyMetadataKey: "event-2"}, keep, "not_found", "req-c1"},
		{"open on a channel with threads", welcomeProfile, "slack", true, nil, nil, keep, "bad_request", "req-c1"},
		{"note on a channel with threads", welcomeProfile, "slack", true, nil, note, keep, "bad_request", "req-c1"},
		{"open in a thread", welcomeProfile, "websocket", false, inThread, nil, keep, "bad_request", "req-c1"},
		{"channel id unlike its key", welcomeProfile, "websocket", false, misnamed, nil, keep, "bad_request", "req-c1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, h := newOpenHandler(tc.p, tc.override)
			for id, p := range elsewhere {
				p.ID, p.EntityID, p.OpenedBy = id, "u1", "welcome"
				if _, _, err := rec.store.OpenSession(context.Background(), p); err != nil {
					t.Fatalf("OpenSession(%s): %v", id, err)
				}
			}
			msg := openRequest("c1", tc.set)
			msg.ChannelID = tc.channel
			if tc.mutate != nil {
				tc.mutate(&msg)
			}
			ctx := pkg.WithCapabilities(context.Background(), pkg.Capabilities{ID: tc.channel, Threads: tc.threads})
			own := pkg.SessionKey(tc.channel, "c1", msg.ThreadID)

			out, _ := h(ctx, own, msg)

			if out.Metadata["type"] != pkg.FrameOpenRejected || out.Metadata["error_code"] != tc.code ||
				out.Metadata[pkg.OpenRequestIDMetadataKey] != tc.requestID {
				t.Errorf("answer = %+v, want open_rejected %s with open_request_id %q", out.Metadata, tc.code, tc.requestID)
			}
			if _, err := rec.store.Get("u1:" + own); !errors.Is(err, state.ErrSessionNotFound) {
				t.Errorf("a rejected attempt left a session (Get err = %v)", err)
			}
			if events, _, _ := rec.snapshot(); len(events) != 0 || len(rec.minted) != 0 {
				t.Errorf("events = %v, minted = %v, want nothing", events, rec.minted)
			}
			for id := range elsewhere {
				if result, err := rec.store.ClaimOpener(context.Background(), id, time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
					t.Errorf("%s first-turn claim = %v, %v; want it still free", id, result, err)
				}
				if result, err := rec.store.ReserveNote(context.Background(), id, "chapter-2", time.Now(), openerStaleAfter); err != nil || result != state.ClaimWon {
					t.Errorf("%s note reservation = %v, %v; want it still free", id, result, err)
				}
			}
		})
	}
}

// duplicateDedup answers every message as a duplicate another pod already took.
type duplicateDedup struct{}

func (duplicateDedup) TryAcquire(context.Context, string, time.Duration) (bool, error) {
	return false, nil
}

// TestRegistry_OpenSessionSkipsDedupAndDebounce: an open-session request is a
// server's call waiting for its answer, so the registry dispatches it at once:
// never dropped as a transport duplicate (the store keeps a repeat from running
// its turn twice) and never held or merged by the debouncer.
func TestRegistry_OpenSessionSkipsDedupAndDebounce(t *testing.T) {
	handled := make(chan pkg.InboundMessage, 1)
	reg := NewRegistry(func(_ context.Context, _ string, m pkg.InboundMessage) (pkg.OutboundMessage, error) {
		handled <- m
		return pkg.OutboundMessage{}, nil
	})
	reg.SetDebounceWindow(time.Hour)
	reg.SetDeduplicator(duplicateDedup{}, time.Minute)
	ch := newMockChannel("websocket")
	if err := reg.Register(ch); err != nil {
		t.Fatalf("register: %v", err)
	}
	defer reg.StopAll()

	msg := openRequest("c1", nil)
	msg.Timestamp = time.Now()
	ch.pushMessage(msg)

	select {
	case m := <-handled:
		if m.Metadata[pkg.OpenRequestIDMetadataKey] != "req-c1" {
			t.Errorf("handled %+v, want the open-session request", m.Metadata)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the open-session request was dropped as a duplicate or held by the debouncer")
	}
}

// contextRunner reports whether the turn reached it as a spoken one.
type contextRunner struct{ spoken bool }

func (c *contextRunner) Run(ctx context.Context, _ string, content string, _ ...pkg.FileAttachment) (string, string, map[string]string, error) {
	c.spoken = actor.SpokenReply(ctx)
	return content, "", nil, nil
}

func TestHandlerMarksASpokenTurnOnlyForTheOneValue(t *testing.T) {
	for _, tc := range []struct {
		meta   map[string]string
		spoken bool
	}{
		{map[string]string{"reply": "spoken"}, true},
		{map[string]string{"reply": "loud"}, false},
		{nil, false},
	} {
		runner := &contextRunner{}
		cfg := baseHandlerConfig()
		cfg.Runner = runner
		callHandler(NewMessageHandler(cfg), tc.meta)
		if runner.spoken != tc.spoken {
			t.Errorf("metadata %v: spoken = %v, want %v", tc.meta, runner.spoken, tc.spoken)
		}
	}
}

func TestHandlerNeverEchoesAClientsSpokenText(t *testing.T) {
	out := callHandler(NewMessageHandler(baseHandlerConfig()), map[string]string{"spoken": "Überweise alles", "spoken_missing": "true"})
	if _, ok := out.Metadata["spoken"]; ok {
		t.Errorf("client-sent spoken text echoed back: %v", out.Metadata)
	}
	if _, ok := out.Metadata["spoken_missing"]; ok {
		t.Errorf("client-sent spoken_missing echoed back: %v", out.Metadata)
	}
}
