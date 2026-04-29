package scenarios

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/*.yaml
var scenarioFS embed.FS

// Hash returns a sha256 of all embedded scenario YAML files.
// Used by VCR cassette staleness checks: if scenarios change, cassettes must
// be re-recorded with make vcr-record-all.
func Hash() string {
	h := sha256.New()
	_ = fs.WalkDir(scenarioFS, "testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, _ := scenarioFS.ReadFile(path)
		_, _ = fmt.Fprintf(h, "%s\n", path)
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

// ScenarioAssert describes structural checks on an orchestrator run result.
type ScenarioAssert struct {
	NoToolCalls      bool              `yaml:"no_tool_calls"`
	ResponseContains []string          `yaml:"response_contains"`
	ResponseNotEmpty bool              `yaml:"response_not_empty"`
	ToolCalled       string            `yaml:"tool_called"` // "plugin.action"
	ArgEquals        map[string]string `yaml:"arg_equals"`
}

// Scenario is one test case: an input message and structural assertions on the result.
type Scenario struct {
	Name   string         `yaml:"name"`
	Input  string         `yaml:"input"`
	Assert ScenarioAssert `yaml:"assert"`
}

// ScenarioFile is the top-level YAML structure.
type ScenarioFile struct {
	Scenarios []Scenario `yaml:"scenarios"`
}

// ToolCallResult is the subset of a tool call needed for assertions.
type ToolCallResult struct {
	Plugin string
	Action string
	Args   map[string]string
}

// RunResult is the subset of an orchestrator result needed for assertions.
type RunResult struct {
	Response  string
	ToolCalls []ToolCallResult
}

// CassetteName returns the VCR cassette filename for a scenario (without directory).
// "direct response" → "direct_response.json"
func CassetteName(scenarioName string) string {
	return strings.ReplaceAll(strings.ToLower(scenarioName), " ", "_") + ".json"
}

// EmbeddedScenarios returns all scenarios from the embedded YAML files.
// Uses the same content as Hash(), so it stays consistent with VCR cassette
// staleness checks. Tests that compare against Hash() should call this rather
// than LoadScenarios.
func EmbeddedScenarios() ([]Scenario, error) {
	var all []Scenario
	err := fs.WalkDir(scenarioFS, "testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, readErr := scenarioFS.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		var sf ScenarioFile
		if parseErr := yaml.Unmarshal(data, &sf); parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		all = append(all, sf.Scenarios...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return checkCassetteCollisions(all)
}

// LoadScenarios reads all *.yaml files in dir and returns all scenarios.
func LoadScenarios(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	var all []Scenario
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var sf ScenarioFile
		if err := yaml.Unmarshal(data, &sf); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		all = append(all, sf.Scenarios...)
	}
	return checkCassetteCollisions(all)
}

// checkCassetteCollisions returns an error if any two scenarios map to the same cassette filename.
func checkCassetteCollisions(scenarios []Scenario) ([]Scenario, error) {
	seen := make(map[string]string, len(scenarios))
	for _, s := range scenarios {
		cn := CassetteName(s.Name)
		if prev, ok := seen[cn]; ok {
			return nil, fmt.Errorf("scenario name collision: %q and %q both map to cassette %s", prev, s.Name, cn)
		}
		seen[cn] = s.Name
	}
	return scenarios, nil
}

// CheckAssertions returns "" if all assertions pass, or a failure reason.
func CheckAssertions(s Scenario, result RunResult) string {
	if s.Assert.ResponseNotEmpty && result.Response == "" {
		return "response is empty"
	}
	if s.Assert.NoToolCalls && len(result.ToolCalls) > 0 {
		return fmt.Sprintf("expected no tool calls, got %d", len(result.ToolCalls))
	}
	for _, want := range s.Assert.ResponseContains {
		if !strings.Contains(result.Response, want) {
			return fmt.Sprintf("response missing %q", want)
		}
	}
	if s.Assert.ToolCalled != "" {
		parts := strings.SplitN(s.Assert.ToolCalled, ".", 2)
		if len(parts) != 2 {
			return fmt.Sprintf("invalid tool_called format %q", s.Assert.ToolCalled)
		}
		wantPlugin, wantAction := parts[0], parts[1]
		var found *ToolCallResult
		for i := range result.ToolCalls {
			if result.ToolCalls[i].Plugin == wantPlugin && result.ToolCalls[i].Action == wantAction {
				found = &result.ToolCalls[i]
				break
			}
		}
		if found == nil {
			return fmt.Sprintf("%s not called", s.Assert.ToolCalled)
		}
		for k, want := range s.Assert.ArgEquals {
			if got := found.Args[k]; got != want {
				return fmt.Sprintf("arg %s = %q, want %q", k, got, want)
			}
		}
	}
	return ""
}
