package router

import (
	"fmt"
	"sort"

	"github.com/opentalon/opentalon/internal/provider"
)

type CatalogModel struct {
	Ref    provider.ModelRef
	Alias  string
	Weight int // 0-100, higher = preferred (cheaper)
}

type Override struct {
	Model provider.ModelRef
	Scope string // "request", "session", "pin"
}

type WeightedRouter struct {
	catalog   []CatalogModel
	pins      map[TaskType]provider.ModelRef
	affinity  *AffinityStore
	threshold float64 // minimum affinity score to use learned model
}

func NewWeightedRouter(catalog []CatalogModel, pins map[TaskType]provider.ModelRef, affinity *AffinityStore) *WeightedRouter {
	sorted := make([]CatalogModel, len(catalog))
	copy(sorted, catalog)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Weight > sorted[j].Weight
	})

	return &WeightedRouter{
		catalog:   sorted,
		pins:      pins,
		affinity:  affinity,
		threshold: 0.3,
	}
}

func (r *WeightedRouter) Route(taskType TaskType, override *Override) (provider.ModelRef, error) {
	if override != nil {
		return override.Model, nil
	}

	if pin, ok := r.pins[taskType]; ok {
		return pin, nil
	}

	if r.affinity != nil {
		scores := r.affinity.Get(taskType)
		for _, ms := range scores {
			if ms.Score >= r.threshold {
				return ms.Model, nil
			}
		}
	}

	if len(r.catalog) == 0 {
		return "", fmt.Errorf("no models in catalog")
	}
	return r.catalog[0].Ref, nil
}

func (r *WeightedRouter) NextModel(current provider.ModelRef) (provider.ModelRef, error) {
	for i, m := range r.catalog {
		if m.Ref == current && i+1 < len(r.catalog) {
			return r.catalog[i+1].Ref, nil
		}
	}
	return "", fmt.Errorf("no next model available after %s", current)
}

func (r *WeightedRouter) RecordSignal(taskType TaskType, model provider.ModelRef, signal Signal) {
	if r.affinity != nil {
		r.affinity.Record(taskType, model, signal)
	}
}

func (r *WeightedRouter) Models() []CatalogModel {
	return r.catalog
}
