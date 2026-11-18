package provider

import (
	"cmp"
	"context"
	"log/slog"

	"github.com/opentalon/opentalon/internal/profile"
)

const eur = "EUR"

func Price(ctx context.Context, model, currency string, u Usage) *float64 {
	p := profile.FromContext(ctx)
	if p == nil || p.Tariffs == nil {
		return nil
	}
	t, ok := p.Tariffs[model]
	if !ok {
		if currency == eur {
			slog.WarnContext(ctx, "model has no tariff; the call is priced at its provider cost", "model", model)
			price := u.InputCost + u.OutputCost
			return &price
		}
		slog.WarnContext(ctx, "model has no tariff and its provider cost is not in EUR; the call has no price",
			"model", model, "currency", cmp.Or(currency, "none"))
		return nil
	}
	cachedInput := t.Input
	if t.CachedInput != nil {
		cachedInput = *t.CachedInput
	}
	price := (float64(u.InputTokens-u.CacheReadTokens)*t.Input +
		float64(u.CacheReadTokens)*cachedInput +
		float64(u.OutputTokens)*t.Output) / 1_000_000
	return &price
}
