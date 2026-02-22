// Package requestpkg can load OpenClaw-style skills: parse SKILL.md (and optional
// request.yaml) into request packages so the core can run them without a compiled plugin.
//
// OpenClaw skills are typically a folder with SKILL.md. Many skills describe an
// HTTP request (method, URL, body) and guardrails (required env). We support:
//
//  1. request.yaml (or openclaw.yaml) in the skill dir — same format as our
//     request_packages inline YAML (plugin name, packages with url/body/required_env).
//     If present, we use it and ignore SKILL.md for the request definition.
//
//  2. SKILL.md parsing — heuristics to extract:
//     - Method + URL from lines like "Make an HTTP POST to:\n{{env.URL}}/path"
//     - Body from a fenced code block (```json ... ```) after "With body:" or similar
//     - Required env from "Guardrails" / "Validate that X is provided"
//
// One skill folder = one plugin (one Set). Action name from first package or "run".

package requestpkg

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadSkillDir loads one OpenClaw-style skill directory into a Set.
// It looks for request.yaml (or openclaw.yaml) first; otherwise parses SKILL.md.
func LoadSkillDir(skillDir string) (Set, error) {
	// Prefer explicit request YAML (OpenClaw-compat or our format)
	for _, name := range []string{"request.yaml", "request.yml", "openclaw.yaml"} {
		path := filepath.Join(skillDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Set{}, fmt.Errorf("read %s: %w", path, err)
		}
		var s Set
		if err := yaml.Unmarshal(data, &s); err != nil {
			return Set{}, fmt.Errorf("parse %s: %w", path, err)
		}
		if s.PluginName == "" {
			s.PluginName = filepath.Base(skillDir)
		}
		return s, nil
	}

	// Fall back: parse SKILL.md
	mdPath := filepath.Join(skillDir, "SKILL.md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Set{}, fmt.Errorf("no request.yaml or SKILL.md in %s", skillDir)
		}
		return Set{}, fmt.Errorf("read %s: %w", mdPath, err)
	}
	pkg, pluginName, description, err := ParseSkillMD(string(data))
	if err != nil {
		return Set{}, fmt.Errorf("parse SKILL.md: %w", err)
	}
	return Set{
		PluginName:  pluginName,
		Description: description,
		Packages:    []Package{pkg},
	}, nil
}

// LoadSkillsDir loads all skill subdirectories under dir (each subdir with SKILL.md
// or request.yaml becomes one Set).
func LoadSkillsDir(dir string) ([]Set, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills dir: %w", err)
	}
	var sets []Set
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, e.Name())
		set, err := LoadSkillDir(skillDir)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", e.Name(), err)
		}
		sets = append(sets, set)
	}
	return sets, nil
}

var (
	// "Make an HTTP POST to:" or "POST {{env.X}}/path" or "1. Make an HTTP POST to:\n{{env.JIRA_URL}}/rest/..."
	httpMethodURLRe = regexp.MustCompile(`(?i)(?:make\s+an?\s+)?HTTP\s+(GET|POST|PUT|PATCH|DELETE)\s+(?:to\s*:\s*)?\s*(\S+|(?:\{\{env\.\w+\}\}[^\s]*))`)
	bodyBlockRe     = regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)```")
	guardrailsRe    = regexp.MustCompile(`(?i)validate\s+that\s+(\w+)`)
	requiredEnvRe   = regexp.MustCompile(`(?i)(?:required|guardrails?|validate).*?(\w+_API_TOKEN|\w+_URL|\w+_KEY)`)
)

// ParseSkillMD extracts one request package from OpenClaw-style SKILL.md content.
// Returns (package, pluginName, description, error). Plugin name is from first # title or "skill".
func ParseSkillMD(content string) (Package, string, string, error) {
	lines := strings.Split(content, "\n")
	var pkg Package
	pkg.Action = "run"
	pluginName := "skill"
	description := ""

	// First # title -> plugin name and description
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "# ") {
			pluginName = strings.ToLower(s[2:])
			pluginName = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(pluginName, "_")
			if pluginName == "" {
				pluginName = "skill"
			}
			description = s[2:]
			break
		}
	}

	// Method + URL
	contentLower := strings.ToLower(content)
	switch {
	case strings.Contains(contentLower, "post"):
		pkg.Method = "POST"
	case strings.Contains(contentLower, "get"):
		pkg.Method = "GET"
	case strings.Contains(contentLower, "put"):
		pkg.Method = "PUT"
	case strings.Contains(contentLower, "patch"):
		pkg.Method = "PATCH"
	case strings.Contains(contentLower, "delete"):
		pkg.Method = "DELETE"
	default:
		pkg.Method = "POST"
	}

	if m := httpMethodURLRe.FindStringSubmatch(content); len(m) >= 3 {
		pkg.Method = strings.ToUpper(m[1])
		pkg.URL = strings.TrimSpace(m[2])
	} else {
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "{{env.") && (strings.Contains(trimmed, "/") || strings.Contains(trimmed, "}}")) {
				pkg.URL = trimmed
				break
			}
		}
	}

	// Body from ```...``` block (prefer one that looks like JSON with {{)
	codeBlocks := bodyBlockRe.FindAllStringSubmatch(content, -1)
	for _, m := range codeBlocks {
		if len(m) < 2 {
			continue
		}
		block := strings.TrimSpace(m[1])
		if strings.Contains(block, "{{") && (strings.Contains(block, "fields") || strings.Contains(block, "project")) {
			pkg.Body = block
			break
		}
		if pkg.Body == "" && (strings.HasPrefix(block, "{") || strings.Contains(block, "{{")) {
			pkg.Body = block
		}
	}

	// Required env from "Validate that X is provided" / "JIRA_API_TOKEN"
	seen := make(map[string]bool)
	for _, m := range guardrailsRe.FindAllStringSubmatch(content, -1) {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			pkg.RequiredEnv = append(pkg.RequiredEnv, name)
		}
	}
	for _, m := range requiredEnvRe.FindAllStringSubmatch(content, -1) {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			pkg.RequiredEnv = append(pkg.RequiredEnv, name)
		}
	}
	// Also collect any {{env.X}} from URL and body
	for _, name := range envRe.FindAllStringSubmatch(pkg.URL+" "+pkg.Body, -1) {
		if len(name) >= 2 && !seen[name[1]] {
			seen[name[1]] = true
			pkg.RequiredEnv = append(pkg.RequiredEnv, name[1])
		}
	}

	if pkg.URL == "" {
		return Package{}, "", "", fmt.Errorf("could not extract URL from SKILL.md")
	}
	return pkg, pluginName, description, nil
}
