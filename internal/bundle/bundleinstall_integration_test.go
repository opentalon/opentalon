//go:build bundleinstall

package bundle_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/opentalon/opentalon/internal/bundle"
	"github.com/opentalon/opentalon/internal/config"
)

// TestBundleInstall_FromConfig is the "does OpenTalon install it?" proof: a
// config.yaml declares tln-plugin with a bundled asp plugin, and Core's own
// install path (EnsurePlugin → clone + write mod.tln + `make build`) compiles
// asp in — exactly what happens at startup. Nothing here calls the composer
// directly; it's driven by config, like an operator's.
//
// Needs network + git + make + go, so it's gated behind the `bundleinstall`
// build tag and run in a dedicated CI job.
func TestBundleInstall_FromConfig(t *testing.T) {
	// What an operator writes — the only thing needed to install a tln plugin.
	const configYAML = `
plugins:
  tln-plugin:
    github: opentalon/tln-plugin
    ref: v0.5.0
    bundle:
      - name: asp
        github: opentalon/tln-asp
        ref: latest
`
	var cfg struct {
		Plugins map[string]config.PluginConfig `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(configYAML), &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	p := cfg.Plugins["tln-plugin"]

	// Drive the install exactly as cmd/opentalon/main.go does.
	var bp []bundle.BundlePlugin
	for _, b := range p.Bundle {
		bp = append(bp, bundle.BundlePlugin{Name: b.Name, GitHub: b.GitHub, Ref: b.Ref, Store: b.Store})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dataDir := t.TempDir()

	path, err := bundle.EnsurePlugin(ctx, dataDir, "tln-plugin", p.GitHub, p.Ref, false, bp)
	if err != nil {
		t.Fatalf("OpenTalon install (EnsurePlugin): %v", err)
	}

	// asp was composed into the installed binary…
	gen, err := os.ReadFile(filepath.Join(dataDir, "plugins", "tln-plugin", "bundle_gen.go"))
	if err != nil {
		t.Fatalf("read installed bundle_gen.go: %v", err)
	}
	if !strings.Contains(string(gen), "opentalon/tln-asp") {
		t.Errorf("asp not composed into the install:\n%s", gen)
	}
	// …and the binary OpenTalon mounts exists.
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("installed binary missing/empty at %s: %v", path, err)
	}
	t.Logf("OpenTalon installed asp into tln-plugin from config: %s", path)
}
