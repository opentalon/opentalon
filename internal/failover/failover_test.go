package failover

import (
	"context"
	"fmt"
	"testing"

	"github.com/opentalon/opentalon/internal/auth"
	"github.com/opentalon/opentalon/internal/provider"
)

type mockProvider struct {
	id        string
	callCount int
	err       error
}

func (m *mockProvider) ID() string { return m.id }
func (m *mockProvider) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	m.callCount++
	if m.err != nil {
		return nil, m.err
	}
	return &provider.CompletionResponse{Content: "ok from " + m.id}, nil
}
func (m *mockProvider) Stream(_ context.Context, _ *provider.CompletionRequest) (provider.ResponseStream, error) {
	return nil, nil
}
func (m *mockProvider) Models() []provider.ModelInfo            { return nil }
func (m *mockProvider) SupportsFeature(_ provider.Feature) bool { return false }

func setupTest() (*provider.Registry, *auth.Rotator, *auth.CooldownTracker) {
	store := auth.NewStore("")
	store.Add(&auth.Profile{ID: "anthropic:key1", ProviderID: "anthropic", Type: auth.AuthTypeAPIKey})
	store.Add(&auth.Profile{ID: "anthropic:key2", ProviderID: "anthropic", Type: auth.AuthTypeAPIKey})
	store.Add(&auth.Profile{ID: "openai:key1", ProviderID: "openai", Type: auth.AuthTypeAPIKey})

	rotator := auth.NewRotator(store)
	cooldowns := auth.NewCooldownTracker(auth.DefaultCooldownConfig())
	reg := provider.NewRegistry()

	return reg, rotator, cooldowns
}

func TestExecuteSuccess(t *testing.T) {
	reg, rotator, cooldowns := setupTest()
	p := &mockProvider{id: "anthropic"}
	_ = reg.Register(p)

	ctrl := NewController(reg, rotator, cooldowns, nil)

	fn := func(ctx context.Context, p provider.Provider, profile *auth.Profile, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
		return p.Complete(ctx, req)
	}

	resp, err := ctrl.Execute(context.Background(), "anthropic/claude-haiku-4", "sess1", &provider.CompletionRequest{}, fn)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok from anthropic" {
		t.Errorf("unexpected content: %s", resp.Content)
	}
}

func TestExecuteRetryOnRateLimit(t *testing.T) {
	reg, rotator, cooldowns := setupTest()

	callCount := 0
	_ = reg.Register(&mockProvider{id: "anthropic"})

	fn := func(ctx context.Context, p provider.Provider, profile *auth.Profile, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
		callCount++
		if callCount == 1 {
			return nil, &ProviderError{StatusCode: 429, Message: "rate limited", Retryable: true}
		}
		return &provider.CompletionResponse{Content: "ok"}, nil
	}

	ctrl := NewController(reg, rotator, cooldowns, nil)
	resp, err := ctrl.Execute(context.Background(), "anthropic/claude-haiku-4", "sess1", &provider.CompletionRequest{}, fn)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Errorf("unexpected content: %s", resp.Content)
	}
	if callCount != 2 {
		t.Errorf("expected 2 calls, got %d", callCount)
	}
}

func TestExecuteFallbackToNextModel(t *testing.T) {
	reg, rotator, cooldowns := setupTest()
	_ = reg.Register(&mockProvider{id: "anthropic", err: &ProviderError{StatusCode: 429, Retryable: true}})
	_ = reg.Register(&mockProvider{id: "openai"})

	fn := func(ctx context.Context, p provider.Provider, profile *auth.Profile, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
		return p.Complete(ctx, req)
	}

	fallbacks := []provider.ModelRef{"openai/gpt-5.2"}
	ctrl := NewController(reg, rotator, cooldowns, fallbacks)

	resp, err := ctrl.Execute(context.Background(), "anthropic/claude-haiku-4", "sess1", &provider.CompletionRequest{}, fn)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok from openai" {
		t.Errorf("expected fallback to openai, got %s", resp.Content)
	}
}

func TestExecuteAllExhausted(t *testing.T) {
	reg, rotator, cooldowns := setupTest()
	rateLimitErr := &ProviderError{StatusCode: 429, Retryable: true}
	_ = reg.Register(&mockProvider{id: "anthropic", err: rateLimitErr})
	_ = reg.Register(&mockProvider{id: "openai", err: rateLimitErr})

	fn := func(ctx context.Context, p provider.Provider, profile *auth.Profile, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
		return p.Complete(ctx, req)
	}

	fallbacks := []provider.ModelRef{"openai/gpt-5.2"}
	ctrl := NewController(reg, rotator, cooldowns, fallbacks)

	_, err := ctrl.Execute(context.Background(), "anthropic/claude-haiku-4", "sess1", &provider.CompletionRequest{}, fn)
	if err == nil {
		t.Fatal("expected error when all exhausted")
	}
	if _, ok := err.(*AllExhaustedError); !ok {
		t.Errorf("expected AllExhaustedError, got %T", err)
	}
}

func TestExecuteNonRetryableError(t *testing.T) {
	reg, rotator, cooldowns := setupTest()
	_ = reg.Register(&mockProvider{id: "anthropic"})

	fn := func(ctx context.Context, p provider.Provider, profile *auth.Profile, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
		return nil, &ProviderError{StatusCode: 400, Message: "bad request", Retryable: false}
	}

	ctrl := NewController(reg, rotator, cooldowns, nil)
	_, err := ctrl.Execute(context.Background(), "anthropic/claude-haiku-4", "sess1", &provider.CompletionRequest{}, fn)
	if err == nil {
		t.Fatal("expected error for non-retryable")
	}
	pe, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("expected ProviderError, got %T", err)
	}
	if pe.StatusCode != 400 {
		t.Errorf("expected status 400, got %d", pe.StatusCode)
	}
}

func TestIsRateLimitError(t *testing.T) {
	if !IsRateLimitError(&ProviderError{StatusCode: 429}) {
		t.Error("429 should be rate limit error")
	}
	if IsRateLimitError(&ProviderError{StatusCode: 500}) {
		t.Error("500 should not be rate limit error")
	}
}

func TestIsAuthError(t *testing.T) {
	if !IsAuthError(&ProviderError{StatusCode: 401}) {
		t.Error("401 should be auth error")
	}
	if !IsAuthError(&ProviderError{StatusCode: 403}) {
		t.Error("403 should be auth error")
	}
	if IsAuthError(&ProviderError{StatusCode: 429}) {
		t.Error("429 should not be auth error")
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		code      int
		retryable bool
		want      bool
	}{
		{429, false, true},
		{500, false, true},
		{503, false, true},
		{400, false, false},
		{200, true, true},
	}
	for _, tt := range tests {
		err := &ProviderError{StatusCode: tt.code, Retryable: tt.retryable}
		if got := IsRetryable(err); got != tt.want {
			t.Errorf("IsRetryable(%d, retryable=%v) = %v, want %v", tt.code, tt.retryable, got, tt.want)
		}
	}
}

func TestProviderErrorString(t *testing.T) {
	err := &ProviderError{StatusCode: 429, Message: "rate limited"}
	want := "provider error 429: rate limited"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestAllExhaustedErrorString(t *testing.T) {
	err := &AllExhaustedError{Attempted: []string{"anthropic/haiku", "openai/gpt-5"}}
	got := err.Error()
	if got == "" {
		t.Error("expected non-empty error string")
	}
}

func TestNonProviderErrorClassification(t *testing.T) {
	plainErr := fmt.Errorf("connection refused")
	if IsRateLimitError(plainErr) {
		t.Error("plain error should not be rate limit")
	}
	if IsAuthError(plainErr) {
		t.Error("plain error should not be auth error")
	}
	if IsRetryable(plainErr) {
		t.Error("plain error should not be retryable")
	}
}

func TestExecuteRetryOnAuthError(t *testing.T) {
	reg, rotator, cooldowns := setupTest()
	_ = reg.Register(&mockProvider{id: "anthropic"})

	callCount := 0
	fn := func(ctx context.Context, p provider.Provider, profile *auth.Profile, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
		callCount++
		if callCount == 1 {
			return nil, &ProviderError{StatusCode: 401, Message: "unauthorized", Retryable: true}
		}
		return &provider.CompletionResponse{Content: "ok"}, nil
	}

	ctrl := NewController(reg, rotator, cooldowns, nil)
	resp, err := ctrl.Execute(context.Background(), "anthropic/claude-haiku-4", "sess1", &provider.CompletionRequest{}, fn)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q, want ok", resp.Content)
	}
	if callCount != 2 {
		t.Errorf("expected 2 calls, got %d", callCount)
	}
}

func TestExecuteSkipsDuplicateModels(t *testing.T) {
	reg, rotator, cooldowns := setupTest()
	_ = reg.Register(&mockProvider{id: "anthropic"})

	callCount := 0
	fn := func(ctx context.Context, p provider.Provider, profile *auth.Profile, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
		callCount++
		return nil, &ProviderError{StatusCode: 429, Retryable: true}
	}

	fallbacks := []provider.ModelRef{"anthropic/claude-haiku-4"}
	ctrl := NewController(reg, rotator, cooldowns, fallbacks)

	_, err := ctrl.Execute(context.Background(), "anthropic/claude-haiku-4", "sess1", &provider.CompletionRequest{}, fn)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*AllExhaustedError); !ok {
		t.Errorf("expected AllExhaustedError, got %T: %v", err, err)
	}
}

func TestExecuteUnregisteredProvider(t *testing.T) {
	reg, rotator, cooldowns := setupTest()

	fn := func(ctx context.Context, p provider.Provider, profile *auth.Profile, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
		return p.Complete(ctx, req)
	}

	ctrl := NewController(reg, rotator, cooldowns, nil)
	_, err := ctrl.Execute(context.Background(), "unknown/model-x", "sess1", &provider.CompletionRequest{}, fn)
	if err == nil {
		t.Fatal("expected error for unregistered provider")
	}
}
