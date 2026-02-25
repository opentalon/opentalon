package lua

import (
	"os"
	"path/filepath"
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
