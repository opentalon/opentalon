package channel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadToolDefs(t *testing.T) {
	toolsYAML := `
- plugin: test
  description: "Test plugin"
  action: do_thing
  action_description: "Does a thing"
  method: POST
  url: "https://example.com/api"
  body: '{"key":"{{args.value}}"}'
  headers:
    Authorization: "Bearer {{env.TOKEN}}"
  required_env:
    - TOKEN
  parameters:
    - name: value
      description: "The value"
      required: true
`
	tools, err := LoadToolDefs([]byte(toolsYAML))
	if err != nil {
		t.Fatalf("LoadToolDefs: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}

	tool := tools[0]
	if tool.Plugin != "test" {
		t.Errorf("Plugin = %q, want %q", tool.Plugin, "test")
	}
	if tool.Action != "do_thing" {
		t.Errorf("Action = %q, want %q", tool.Action, "do_thing")
	}
	if tool.Method != "POST" {
		t.Errorf("Method = %q, want %q", tool.Method, "POST")
	}
	if len(tool.Parameters) != 1 {
		t.Fatalf("Parameters = %d, want 1", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "value" {
		t.Errorf("Parameters[0].Name = %q, want %q", tool.Parameters[0].Name, "value")
	}
	if !tool.Parameters[0].Required {
		t.Error("Parameters[0].Required should be true")
	}
}

func TestLoadToolDefsFromFile(t *testing.T) {
	toolsYAML := `
- plugin: file-test
  action: ping
  action_description: "Ping"
  method: GET
  url: "https://example.com/ping"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	if err := os.WriteFile(path, []byte(toolsYAML), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tools, err := LoadToolDefsFromFile("tools.yaml", dir)
	if err != nil {
		t.Fatalf("LoadToolDefsFromFile: %v", err)
	}
	if len(tools) != 1 || tools[0].Plugin != "file-test" {
		t.Errorf("unexpected tools: %v", tools)
	}
}

func TestLoadToolDefsInvalidYAML(t *testing.T) {
	_, err := LoadToolDefs([]byte("not valid yaml: ["))
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}
