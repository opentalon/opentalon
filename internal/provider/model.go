package provider

import (
	"fmt"
	"strings"
)

type ModelRef string

func NewModelRef(providerID, modelID string) ModelRef {
	return ModelRef(providerID + "/" + modelID)
}

func (r ModelRef) Provider() string {
	parts := strings.SplitN(string(r), "/", 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}

func (r ModelRef) Model() string {
	parts := strings.SplitN(string(r), "/", 2)
	if len(parts) < 2 {
		return string(r)
	}
	return parts[1]
}

func (r ModelRef) String() string {
	return string(r)
}

func (r ModelRef) Valid() bool {
	return r.Provider() != "" && r.Model() != ""
}

func ParseModelRef(s string) (ModelRef, error) {
	ref := ModelRef(s)
	if !ref.Valid() {
		return "", fmt.Errorf("invalid model ref %q: expected format provider/model", s)
	}
	return ref, nil
}

type Feature string

const (
	FeatureStreaming Feature = "streaming"
	FeatureReasoning Feature = "reasoning"
	FeatureImages    Feature = "images"
	FeatureTools     Feature = "tools"
)

type ModelCost struct {
	Input  float64 `json:"input" yaml:"input"`
	Output float64 `json:"output" yaml:"output"`
}

type ModelInfo struct {
	ID            string    `json:"id" yaml:"id"`
	Name          string    `json:"name" yaml:"name"`
	ProviderID    string    `json:"provider_id" yaml:"provider_id"`
	Reasoning     bool      `json:"reasoning" yaml:"reasoning"`
	InputTypes    []string  `json:"input" yaml:"input"`
	ContextWindow int       `json:"context_window" yaml:"context_window"`
	MaxTokens     int       `json:"max_tokens" yaml:"max_tokens"`
	Cost          ModelCost `json:"cost" yaml:"cost"`
	Features      []Feature `json:"features" yaml:"features"`
}

func (m ModelInfo) Ref() ModelRef {
	return NewModelRef(m.ProviderID, m.ID)
}

func (m ModelInfo) SupportsFeature(f Feature) bool {
	for _, feat := range m.Features {
		if feat == f {
			return true
		}
	}
	return false
}
