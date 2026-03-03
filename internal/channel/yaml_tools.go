package channel

import (
	"fmt"
	"os"
	"path/filepath"

	pkg "github.com/opentalon/opentalon/pkg/channel"
	"gopkg.in/yaml.v3"
)

// toolDef mirrors pkg.ToolDefinition with yaml tags.
type toolDef struct {
	Plugin      string            `yaml:"plugin"`
	Description string            `yaml:"description"`
	Action      string            `yaml:"action"`
	ActionDesc  string            `yaml:"action_description"`
	Method      string            `yaml:"method"`
	URL         string            `yaml:"url"`
	Body        string            `yaml:"body"`
	Headers     map[string]string `yaml:"headers"`
	RequiredEnv []string          `yaml:"required_env"`
	Parameters  []toolParam       `yaml:"parameters"`
}

type toolParam struct {
	Plugin      string `yaml:"plugin"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// LoadToolDefs parses tool definitions from YAML bytes.
func LoadToolDefs(data []byte) ([]pkg.ToolDefinition, error) {
	var defs []toolDef
	if err := yaml.Unmarshal(data, &defs); err != nil {
		return nil, fmt.Errorf("parse tools yaml: %w", err)
	}
	tools := make([]pkg.ToolDefinition, len(defs))
	for i, d := range defs {
		params := make([]pkg.ToolParam, len(d.Parameters))
		for j, p := range d.Parameters {
			params[j] = pkg.ToolParam{
				Name:        p.Name,
				Description: p.Description,
				Required:    p.Required,
			}
		}
		tools[i] = pkg.ToolDefinition{
			Plugin:      d.Plugin,
			Description: d.Description,
			Action:      d.Action,
			ActionDesc:  d.ActionDesc,
			Method:      d.Method,
			URL:         d.URL,
			Body:        d.Body,
			Headers:     d.Headers,
			RequiredEnv: d.RequiredEnv,
			Parameters:  params,
		}
	}
	return tools, nil
}

// LoadToolDefsFromFile loads tool definitions from a YAML file path.
// If path is relative, it is resolved against baseDir.
func LoadToolDefsFromFile(path, baseDir string) ([]pkg.ToolDefinition, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tools file %s: %w", path, err)
	}
	return LoadToolDefs(data)
}
