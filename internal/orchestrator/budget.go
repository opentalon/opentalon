package orchestrator

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
)

// BudgetReader sums the customer price of the usage rows a budget counts:
// groupID's rows of kinds since since, or entityID's rows in groupID when
// entityID is set. *store.UsageStore satisfies it structurally.
type BudgetReader interface {
	PriceSince(ctx context.Context, groupID, entityID string, kinds []string, since time.Time) (float64, error)
}

// BudgetMetadataKey marks the reply of a turn that a used-up budget moved to
// the default model; its value is the budget's scope (profile.ScopeGroup or
// profile.ScopeEntity).
const BudgetMetadataKey = "budget_used_up"

// BudgetUsedUpError refuses a turn whose budget is used up while the default
// model is priced above 0 as well.
type BudgetUsedUpError struct {
	Scope string // profile.ScopeGroup or profile.ScopeEntity
}

func (e *BudgetUsedUpError) Error() string { return "budget used up: " + e.Scope }

// checkBudget decides once per turn what the profile's used-up budgets do to
// it: run as today (""), run on the default model (the used-up budget's
// scope), or not run (*BudgetUsedUpError). Rows are summed only when the
// answer depends on them.
func (o *Orchestrator) checkBudget(ctx context.Context) (string, error) {
	p := profile.FromContext(ctx)
	if o.budget == nil || p == nil || len(p.Limits) == 0 {
		return "", nil
	}
	model := cmp.Or(provider.ModelRef(p.Model).Model(), o.defaultModel)
	def, ok := p.Tariffs[o.defaultModel]
	refuse := ok && pricedAboveZero(def)
	if model == o.defaultModel {
		if !refuse {
			return "", nil
		}
	} else if t, ok := p.Tariffs[model]; ok && !pricedAboveZero(t) {
		return "", nil
	}
	scope := o.usedUpBudget(ctx, p)
	switch {
	case scope == "":
		return "", nil
	case refuse:
		return "", &BudgetUsedUpError{Scope: scope}
	default:
		return scope, nil
	}
}

// usedUpBudget returns the scope of a budget of p that covers this run's kind
// and is used up, ScopeGroup when both are. A budget whose sum cannot be read
// counts as not used up.
func (o *Orchestrator) usedUpBudget(ctx context.Context, p *profile.Profile) string {
	kind := cmp.Or(p.Kind, profile.KindChat)
	for _, scope := range []string{profile.ScopeGroup, profile.ScopeEntity} {
		entity := ""
		if scope == profile.ScopeEntity {
			entity = p.EntityID
		}
		for _, l := range p.Limits {
			if l.Scope != scope || !slices.Contains(l.Kinds, kind) {
				continue
			}
			used, err := o.budget.PriceSince(ctx, p.Group, entity, l.Kinds, l.Since)
			if err != nil {
				logger.FromContext(ctx).Warn("budget check failed; the turn runs on its model",
					"scope", scope, "group", p.Group, "entity", p.EntityID, "error", err)
				continue
			}
			if used >= l.Max {
				return scope
			}
		}
	}
	return ""
}

func pricedAboveZero(t profile.Tariff) bool {
	return t.Input > 0 || t.Output > 0 || t.CachedInput != nil && *t.CachedInput > 0
}

// markBudgetUsedUp adds BudgetMetadataKey to the result of a turn a used-up
// budget moved to the default model. An error result gets no mark: its
// client words it by error_code.
func markBudgetUsedUp(result *RunResult, scope string) {
	if scope == "" || result == nil || result.Metadata["type"] == "error" {
		return
	}
	meta := make(map[string]string, len(result.Metadata)+1)
	maps.Copy(meta, result.Metadata)
	meta[BudgetMetadataKey] = scope
	result.Metadata = meta
}
