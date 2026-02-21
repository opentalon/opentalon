package provider

import (
	"fmt"
	"sync"
)

type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
	}
}

func (r *Registry) Register(p Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := p.ID()
	if _, exists := r.providers[id]; exists {
		return fmt.Errorf("provider %q already registered", id)
	}
	r.providers[id] = p
	return nil
}

func (r *Registry) Get(id string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	if !ok {
		return nil, fmt.Errorf("provider %q not found", id)
	}
	return p, nil
}

func (r *Registry) GetForModel(ref ModelRef) (Provider, error) {
	return r.Get(ref.Provider())
}

func (r *Registry) List() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		result = append(result, p)
	}
	return result
}

func (r *Registry) Deregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, id)
}
