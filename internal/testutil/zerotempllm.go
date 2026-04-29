package testutil

import (
	"context"

	"github.com/opentalon/opentalon/internal/provider"
)

// Completer is the minimal interface needed to wrap an LLM for temperature control.
type Completer interface {
	Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error)
}

// ZeroTempLLM wraps any Completer and forces temperature=0 for deterministic output.
type ZeroTempLLM struct {
	Inner Completer
}

func (z *ZeroTempLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	zero := 0.0
	req.Temperature = &zero
	return z.Inner.Complete(ctx, req)
}
