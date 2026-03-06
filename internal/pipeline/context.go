package pipeline

import "sync"

// PipelineContext holds shared state across pipeline steps.
type PipelineContext struct {
	mu     sync.RWMutex
	values map[string]any
}

// NewContext creates an empty pipeline context.
func NewContext() *PipelineContext {
	return &PipelineContext{values: make(map[string]any)}
}

// Set stores a value scoped to a step ID and key.
func (c *PipelineContext) Set(stepID, key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[stepID+"."+key] = value
}

// Get retrieves a value by step ID and key.
func (c *PipelineContext) Get(stepID, key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.values[stepID+"."+key]
	return v, ok
}

// Merge stores all entries from data scoped to the given step ID.
func (c *PipelineContext) Merge(stepID string, data map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range data {
		c.values[stepID+"."+k] = v
	}
}
