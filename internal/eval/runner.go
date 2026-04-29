package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ScenarioResult is the outcome of running one scenario.
type ScenarioResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}

// EvalResult aggregates all scenario outcomes for one run.
type EvalResult struct {
	Tag      string           `json:"tag"`
	PassRate float64          `json:"pass_rate"`
	Passed   int              `json:"passed"`
	Total    int              `json:"total"`
	Results  []ScenarioResult `json:"results"`
}

// LoadBaseline reads a previously saved EvalResult from path.
// Returns nil if the file does not exist.
func LoadBaseline(path string) (*EvalResult, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read baseline %s: %w", path, err)
	}
	var r EvalResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	return &r, nil
}

// SaveBaseline writes an EvalResult to path, creating parent directories as needed.
func SaveBaseline(path string, r EvalResult) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
