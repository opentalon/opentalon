package lua

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunPrepareReturnsInvokeSingleStep(t *testing.T) {
	// Script returns table with send_to_llm = false and invoke = single step table
	script := `
function prepare(text)
  return {
    send_to_llm = false,
    invoke = {
      plugin = "gitlab",
      action = "deploy",
      args = { branch = "one", env = "staging" }
    }
  }
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "prep.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := RunPrepare(path, "deploy branch one to staging")
	if err != nil {
		t.Fatal(err)
	}
	if result.SendToLLM {
		t.Error("SendToLLM should be false")
	}
	if len(result.InvokeSteps) != 1 {
		t.Fatalf("expected 1 invoke step, got %d", len(result.InvokeSteps))
	}
	step := result.InvokeSteps[0]
	if step.Plugin != "gitlab" || step.Action != "deploy" {
		t.Errorf("step = plugin %q action %q", step.Plugin, step.Action)
	}
	if step.Args["branch"] != "one" || step.Args["env"] != "staging" {
		t.Errorf("args = %v", step.Args)
	}
}

func TestRunPrepareReturnsInvokeArray(t *testing.T) {
	// Script returns invoke as array of steps
	script := `
function prepare(text)
  return {
    send_to_llm = false,
    invoke = {
      { plugin = "gitlab", action = "analyze_code", args = {} },
      { plugin = "jira", action = "create_issue", args = { project = "X" } }
    }
  }
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "prep.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := RunPrepare(path, "analyze and create issue")
	if err != nil {
		t.Fatal(err)
	}
	if result.SendToLLM {
		t.Error("SendToLLM should be false")
	}
	if len(result.InvokeSteps) != 2 {
		t.Fatalf("expected 2 invoke steps, got %d", len(result.InvokeSteps))
	}
	if result.InvokeSteps[0].Plugin != "gitlab" || result.InvokeSteps[0].Action != "analyze_code" {
		t.Errorf("first step = %+v", result.InvokeSteps[0])
	}
	if result.InvokeSteps[1].Plugin != "jira" || result.InvokeSteps[1].Action != "create_issue" {
		t.Errorf("second step = %+v", result.InvokeSteps[1])
	}
	if result.InvokeSteps[1].Args["project"] != "X" {
		t.Errorf("second step args = %v", result.InvokeSteps[1].Args)
	}
}

func TestRunPrepareReturnsString(t *testing.T) {
	script := `function prepare(text) return "transformed: " .. text end`
	dir := t.TempDir()
	path := filepath.Join(dir, "prep.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := RunPrepare(path, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !result.SendToLLM {
		t.Error("SendToLLM should be true for string return")
	}
	if result.Content != "transformed: hello" {
		t.Errorf("Content = %q", result.Content)
	}
	if len(result.InvokeSteps) != 0 {
		t.Errorf("expected no invoke steps, got %d", len(result.InvokeSteps))
	}
}

func TestRunPrepareReturnsBlockMessage(t *testing.T) {
	script := `
function prepare(text)
  return { send_to_llm = false, message = "Request blocked." }
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "prep.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := RunPrepare(path, "sensitive")
	if err != nil {
		t.Fatal(err)
	}
	if result.SendToLLM {
		t.Error("SendToLLM should be false")
	}
	if result.Content != "Request blocked." {
		t.Errorf("Content = %q", result.Content)
	}
	if len(result.InvokeSteps) != 0 {
		t.Errorf("expected no invoke steps, got %d", len(result.InvokeSteps))
	}
}

func TestRunFormatReturnsString(t *testing.T) {
	script := `
function format(text, response_format)
  return "formatted(" .. response_format .. "): " .. text
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "fmt.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := RunFormat(path, "hello **world**", "slack")
	if err != nil {
		t.Fatal(err)
	}
	if result != "formatted(slack): hello **world**" {
		t.Errorf("result = %q", result)
	}
}

func TestRunFormatReceivesResponseFormat(t *testing.T) {
	script := `
function format(text, response_format)
  return response_format
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "fmt.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := RunFormat(path, "anything", "teams")
	if err != nil {
		t.Fatal(err)
	}
	if result != "teams" {
		t.Errorf("expected response_format 'teams', got %q", result)
	}
}

func TestRunFormatMissingFunction(t *testing.T) {
	script := `function prepare(text) return text end`
	dir := t.TempDir()
	path := filepath.Join(dir, "fmt.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := RunFormat(path, "hello", "slack")
	if err == nil {
		t.Fatal("expected error for missing format function")
	}
}

func TestRunFormatNonStringReturn(t *testing.T) {
	script := `
function format(text, response_format)
  return { message = "oops" }
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "fmt.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := RunFormat(path, "hello", "slack")
	if err == nil {
		t.Fatal("expected error for non-string return")
	}
}

func TestRunFormatEmptyFormat(t *testing.T) {
	// Verify empty response_format is passed correctly
	script := `
function format(text, response_format)
  if response_format == "" then
    return text
  end
  return "changed"
end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "fmt.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := RunFormat(path, "keep me", "")
	if err != nil {
		t.Fatal(err)
	}
	if result != "keep me" {
		t.Errorf("expected no-op for empty format, got %q", result)
	}
}

// formatResponseScriptPath returns the path to the bundled format-response.lua script.
func formatResponseScriptPath(t *testing.T) string {
	t.Helper()
	// Resolve relative to this test file's package: internal/lua -> ../../scripts
	path := filepath.Join("..", "..", "scripts", "format-response.lua")
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve format-response.lua path: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("format-response.lua not found at %s, skipping", abs)
	}
	return abs
}

func TestFormatResponseSlackToolDebug(t *testing.T) {
	path := formatResponseScriptPath(t)

	input := "[tool_call] timly.list-containers(page=1, per_page=1)\n[tool_result] {\"total\": 7}\n\n---\n\nYou have **7** containers."
	result, err := RunFormat(path, input, "slack")
	if err != nil {
		t.Fatal(err)
	}
	// Debug section should use Slack blockquote format
	if !strings.Contains(result, "> :wrench:") {
		t.Errorf("expected Slack wrench emoji in blockquote, got:\n%s", result)
	}
	if !strings.Contains(result, "`timly.list-containers(page=1, per_page=1)`") {
		t.Errorf("expected tool call in code block, got:\n%s", result)
	}
	// Response should be Slack-formatted (bold: *text* not **text**)
	if !strings.Contains(result, "*7*") {
		t.Errorf("expected Slack bold formatting, got:\n%s", result)
	}
	// Should not contain raw [tool_call] tags
	if strings.Contains(result, "[tool_call]") {
		t.Errorf("raw [tool_call] tag should be removed, got:\n%s", result)
	}
}

func TestFormatResponseSlackToolDebugError(t *testing.T) {
	path := formatResponseScriptPath(t)

	input := "[tool_call] jira.get-issue(id=123)\n[tool_result] error: not found\n\n---\n\nI could not find that issue."
	result, err := RunFormat(path, input, "slack")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "> :x:") {
		t.Errorf("expected error indicator in debug, got:\n%s", result)
	}
}

func TestFormatResponseMarkdownToolDebug(t *testing.T) {
	path := formatResponseScriptPath(t)

	input := "[tool_call] timly.list-containers(page=1)\n[tool_result] {\"total\": 7}\n\n---\n\nYou have **7** containers."
	result, err := RunFormat(path, input, "markdown")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "> **Tool:**") {
		t.Errorf("expected markdown blockquote with bold, got:\n%s", result)
	}
	// Response body should pass through unchanged for markdown
	if !strings.Contains(result, "**7**") {
		t.Errorf("expected markdown bold preserved, got:\n%s", result)
	}
}

func TestFormatResponseHTMLToolDebug(t *testing.T) {
	path := formatResponseScriptPath(t)

	input := "[tool_call] timly.list-containers(page=1)\n[tool_result] ok\n\n---\n\nDone."
	result, err := RunFormat(path, input, "html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "<blockquote>") {
		t.Errorf("expected HTML blockquote, got:\n%s", result)
	}
	if !strings.Contains(result, "<code>timly.list-containers(page=1)</code>") {
		t.Errorf("expected HTML code tag, got:\n%s", result)
	}
}

func TestFormatResponseNoDebugUnchanged(t *testing.T) {
	path := formatResponseScriptPath(t)

	// Normal response without tool debug blocks
	input := "You have **7** containers."
	result, err := RunFormat(path, input, "slack")
	if err != nil {
		t.Fatal(err)
	}
	// Should just do normal Slack formatting
	if !strings.Contains(result, "*7*") {
		t.Errorf("expected normal Slack formatting, got:\n%s", result)
	}
}
