package requestpkg

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

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
		sets = append(sets, s)
	}
	return sets, nil
}

// Register registers each set with the tool registry (capability + executor).
func Register(registry *orchestrator.ToolRegistry, sets []Set) error {
	for _, set := range sets {
		cap := ToCapability(set)
		exec := NewExecutor(set.PluginName, set.Packages)
		if err := registry.Register(cap, exec); err != nil {
			return fmt.Errorf("register request package %q: %w", set.PluginName, err)
		}
	}
	return nil
}
