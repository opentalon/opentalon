package requestpkg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

// Package defines a single request package (skill-style): an HTTP request
// with URL/body/headers templated with {{env.X}} and {{args.Y}}.
type Package struct {
	Action      string            `yaml:"action"`       // action name, e.g. create_issue
	Description string            `yaml:"description"`  // for capability
	Method      string            `yaml:"method"`       // GET, POST, etc.
	URL         string            `yaml:"url"`          // template: {{env.JIRA_URL}}/rest/api/3/issue
	Body        string            `yaml:"body"`         // optional JSON/body template
	Headers     map[string]string `yaml:"headers"`      // optional, values are templates
	RequiredEnv []string          `yaml:"required_env"` // e.g. ["JIRA_URL", "JIRA_API_TOKEN"]
	Parameters  []ParamDefinition `yaml:"parameters"`   // for capability; name, description, required
}

// ParamDefinition describes one argument (for capability and docs).
type ParamDefinition struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// Set groups request packages by plugin name. Each plugin has a list of actions (packages).
type Set struct {
	PluginName  string    `yaml:"plugin"` // e.g. jira
	Description string    `yaml:"description"`
	Packages    []Package `yaml:"packages"`
}

var (
	envRe  = regexp.MustCompile(`\{\{env\.(\w+)\}\}`)
	argsRe = regexp.MustCompile(`\{\{args\.(\w+)\}\}`)
)

// Substitute replaces {{env.X}} and {{args.Y}} in s. Missing env vars are empty; missing args are left as literal.
func Substitute(s string, args map[string]string) string {
	s = envRe.ReplaceAllStringFunc(s, func(match string) string {
		name := envRe.FindStringSubmatch(match)[1]
		return os.Getenv(name)
	})
	s = argsRe.ReplaceAllStringFunc(s, func(match string) string {
		name := argsRe.FindStringSubmatch(match)[1]
		if v, ok := args[name]; ok {
			return v
		}
		return match
	})
	return s
}

// Executor runs request packages for a single plugin. It implements orchestrator.PluginExecutor.
type Executor struct {
	pluginName string
	packages   map[string]Package
	client     *http.Client
}

// NewExecutor builds an executor for the given plugin and packages.
func NewExecutor(pluginName string, packages []Package) *Executor {
	pm := make(map[string]Package)
	for _, p := range packages {
		pm[p.Action] = p
	}
	return &Executor{
		pluginName: pluginName,
		packages:   pm,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
}

// Execute runs the request package for call.Action and returns a ToolResult.
func (e *Executor) Execute(call orchestrator.ToolCall) orchestrator.ToolResult {
	pkg, ok := e.packages[call.Action]
	if !ok {
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("action %q not found in request package %q", call.Action, e.pluginName),
		}
	}

	for _, name := range pkg.RequiredEnv {
		if os.Getenv(name) == "" {
			return orchestrator.ToolResult{
				CallID: call.ID,
				Error:  fmt.Sprintf("required env %q is not set", name),
			}
		}
	}

	url := Substitute(pkg.URL, call.Args)
	if url == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "URL is empty after substitution"}
	}

	req, err := http.NewRequest(strings.ToUpper(pkg.Method), url, nil)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("build request: %v", err)}
	}

	for k, v := range pkg.Headers {
		req.Header.Set(k, Substitute(v, call.Args))
	}

	if pkg.Body != "" {
		body := Substitute(pkg.Body, call.Args)
		req.Body = io.NopCloser(strings.NewReader(body))
		req.ContentLength = int64(len(body))
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("request failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body)),
		}
	}

	// Try to extract issue key/link from JSON for friendlier output
	content := string(body)
	if resp.Header.Get("Content-Type") == "application/json" && len(body) > 0 {
		var m map[string]interface{}
		if json.Unmarshal(body, &m) == nil {
			if key, _ := m["key"].(string); key != "" {
				self, _ := m["self"].(string)
				if self != "" {
					content = fmt.Sprintf("Issue %s: %s", key, self)
				} else {
					content = fmt.Sprintf("Issue %s", key)
				}
			}
		}
	}

	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: content,
	}
}

// ToCapability converts the package set into an orchestrator.PluginCapability for registration.
func ToCapability(set Set) orchestrator.PluginCapability {
	actions := make([]orchestrator.Action, 0, len(set.Packages))
	for _, p := range set.Packages {
		params := make([]orchestrator.Parameter, 0, len(p.Parameters))
		for _, q := range p.Parameters {
			params = append(params, orchestrator.Parameter{
				Name:        q.Name,
				Description: q.Description,
				Required:    q.Required,
			})
		}
		actions = append(actions, orchestrator.Action{
			Name:        p.Action,
			Description: p.Description,
			Parameters:  params,
		})
	}
	return orchestrator.PluginCapability{
		Name:        set.PluginName,
		Description: set.Description,
		Actions:     actions,
	}
}
