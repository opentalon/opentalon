package profile

import "context"

type contextKey struct{}

// WithProfile returns a context that carries the given Profile.
func WithProfile(ctx context.Context, p *Profile) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

// FromContext returns the Profile stored in ctx, or nil if not set.
func FromContext(ctx context.Context) *Profile {
	if ctx == nil {
		return nil
	}
	v := ctx.Value(contextKey{})
	if v == nil {
		return nil
	}
	p, _ := v.(*Profile)
	return p
}
