package failover

import (
	"context"
	"time"

	"github.com/opentalon/opentalon/internal/auth"
	"github.com/opentalon/opentalon/internal/provider"
)

type ProviderFunc func(ctx context.Context, p provider.Provider, profile *auth.Profile, req *provider.CompletionRequest) (*provider.CompletionResponse, error)

type Controller struct {
	registry  *provider.Registry
	rotator   *auth.Rotator
	cooldowns *auth.CooldownTracker
	fallbacks []provider.ModelRef
}

func NewController(
	registry *provider.Registry,
	rotator *auth.Rotator,
	cooldowns *auth.CooldownTracker,
	fallbacks []provider.ModelRef,
) *Controller {
	return &Controller{
		registry:  registry,
		rotator:   rotator,
		cooldowns: cooldowns,
		fallbacks: fallbacks,
	}
}

func (c *Controller) Execute(
	ctx context.Context,
	model provider.ModelRef,
	sessionID string,
	req *provider.CompletionRequest,
	fn ProviderFunc,
) (*provider.CompletionResponse, error) {
	models := append([]provider.ModelRef{model}, c.fallbacks...)
	attempted := make([]string, 0)

	for _, m := range models {
		if containsRef(attempted, m.String()) {
			continue
		}
		attempted = append(attempted, m.String())

		if c.rotator.AllInCooldown(m.Provider(), time.Now()) {
			continue
		}

		resp, err := c.tryWithRotation(ctx, m, sessionID, req, fn)
		if err == nil {
			return resp, nil
		}

		if !IsRetryable(err) {
			return nil, err
		}
	}

	return nil, &AllExhaustedError{Attempted: attempted}
}

func (c *Controller) tryWithRotation(
	ctx context.Context,
	model provider.ModelRef,
	sessionID string,
	req *provider.CompletionRequest,
	fn ProviderFunc,
) (*provider.CompletionResponse, error) {
	p, err := c.registry.GetForModel(model)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	maxAttempts := 3
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		profile, err := c.rotator.Select(model.Provider(), sessionID, now)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}

		req.Model = model.Model()
		resp, err := fn(ctx, p, profile, req)
		if err == nil {
			c.cooldowns.Reset(profile)
			return resp, nil
		}

		lastErr = err
		if IsRateLimitError(err) || IsAuthError(err) {
			c.cooldowns.PutInCooldown(profile, now)
			continue
		}

		return nil, err
	}

	return nil, lastErr
}

func containsRef(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
