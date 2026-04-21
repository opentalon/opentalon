package requestpkg

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

var envVarPattern = regexp.MustCompile(`\$\{([^}]+)}`)

func expandEnv(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		varName := envVarPattern.FindStringSubmatch(match)[1]
		if val, ok := os.LookupEnv(varName); ok {
			return val
		}
		return match
	})
}

func expandEnvInSet(s *Set) {
	if s.MCP != nil {
		s.MCP.URL = expandEnv(s.MCP.URL)
		for k, v := range s.MCP.Headers {
			s.MCP.Headers[k] = expandEnv(v)
		}
	}
	for i, p := range s.Packages {
		p.URL = expandEnv(p.URL)
		for k, v := range p.Headers {
			p.Headers[k] = expandEnv(v)
		}
		s.Packages[i] = p
	}
}

// LoadDir loads all request package sets from a directory. Each .yaml file is one Set.
func LoadDir(dir string) ([]Set, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read request_packages dir: %w", err)
	}
	var sets []Set
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" && filepath.Ext(e.Name()) != ".yml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var s Set
		if err := yaml.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if s.PluginName == "" {
			return nil, fmt.Errorf("%s: missing plugin name", path)
		}
		if s.MCP != nil && s.MCP.URL == "" {
			return nil, fmt.Errorf("%s: mcp section requires a non-empty url", path)
		}
		expandEnvInSet(&s)
		sets = append(sets, s)
	}
	return sets, nil
}

// Register registers each set with the tool registry (capability + executor).
// Sets with a non-nil MCP field are skipped — their capabilities come from the
// opentalon-mcp plugin binary, not the built-in HTTP executor.
func Register(registry *orchestrator.ToolRegistry, sets []Set) error {
	for _, set := range sets {
		if set.MCP != nil {
			continue
		}
		cap := ToCapability(set)
		exec := NewExecutor(set.PluginName, set.Packages)
		if err := registry.Register(cap, exec); err != nil {
			return fmt.Errorf("register request package %q: %w", set.PluginName, err)
		}
	}
	return nil
}

// CollectMCPServers returns the MCPServerConfig from every set that has one.
// These are serialized as OPENTALON_MCP_SERVERS and injected into the MCP
// plugin binary's environment before it is launched.
func CollectMCPServers(sets []Set) []MCPServerConfig {
	var configs []MCPServerConfig
	for _, set := range sets {
		if set.MCP != nil {
			configs = append(configs, *set.MCP)
		}
	}
	return configs
}
