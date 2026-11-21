package orchestrator

import (
	"context"

	"github.com/opentalon/opentalon/internal/provider"
)

type sessionModelKey struct{}

func withSessionModel(ctx context.Context, ref provider.ModelRef) context.Context {
	return context.WithValue(ctx, sessionModelKey{}, ref)
}

func sessionModel(ctx context.Context) provider.ModelRef {
	ref, _ := ctx.Value(sessionModelKey{}).(provider.ModelRef)
	return ref
}

// onSessionModel leaves a request that names its own Model alone: a set
// repair.model is a bare id of the default provider, which a ModelRef would
// replace.
func onSessionModel(ctx context.Context, req *provider.CompletionRequest) *provider.CompletionRequest {
	if ref := sessionModel(ctx); ref != "" && req.Model == "" {
		req.ModelRef = ref
		req.Model = ref.Model()
	}
	return req
}
