package requestpkg

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

func TestSubstitute(t *testing.T) {
	_ = os.Setenv("TEST_ENV_VAR", "env_value")
	defer func() { _ = os.Unsetenv("TEST_ENV_VAR") }()

	tests := []struct {
		name string
		s    string
		args map[string]string
		want string
	}{
		{"env", "{{env.TEST_ENV_VAR}}", nil, "env_value"},
		{"args", "{{args.foo}}", map[string]string{"foo": "bar"}, "bar"},
		{"mixed", "{{env.TEST_ENV_VAR}}/{{args.id}}", map[string]string{"id": "123"}, "env_value/123"},
		{"missing env", "{{env.MISSING}}", nil, ""},
		{"missing args", "{{args.missing}}", nil, "{{args.missing}}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Substitute(tt.s, tt.args)
			if got != tt.want {
				t.Errorf("Substitute() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecutor_Execute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"OPS-42","self":"https://jira.example.com/browse/OPS-42"}`))
	}))
	defer srv.Close()

	_ = os.Setenv("JIRA_URL", srv.URL)
	_ = os.Setenv("JIRA_API_TOKEN", "secret")
	defer func() {
		_ = os.Unsetenv("JIRA_URL")
		_ = os.Unsetenv("JIRA_API_TOKEN")
	}()

	packages := []Package{
		{
			Action:      "create_issue",
			Description: "Create a Jira issue",
			Method:      "POST",
			URL:         "{{env.JIRA_URL}}/rest/api/3/issue",
			Body:        `{"fields":{"project":{"key":"{{args.project}}"},"summary":"{{args.summary}}","description":"{{args.description}}","issuetype":{"name":"Task"}}}`,
			Headers:     map[string]string{"Authorization": "Bearer {{env.JIRA_API_TOKEN}}"},
			RequiredEnv: []string{"JIRA_URL", "JIRA_API_TOKEN"},
		},
	}
	exec := NewExecutor("jira", packages)

	result := exec.Execute(orchestrator.ToolCall{
		ID:     "call-1",
		Plugin: "jira",
		Action: "create_issue",
		Args:   map[string]string{"project": "OPS", "summary": "X", "description": "Y"},
	})

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if result.CallID != "call-1" {
		t.Errorf("CallID = %q", result.CallID)
	}
	if result.Content == "" {
		t.Error("Content empty")
	}
	if !strings.Contains(result.Content, "OPS-42") {
		t.Errorf("Content should contain issue key OPS-42, got %q", result.Content)
	}
}

func TestExecutor_Execute_RequiredEnv(t *testing.T) {
	_ = os.Unsetenv("MISSING_VAR")
	exec := NewExecutor("test", []Package{
		{
			Action:      "do",
			RequiredEnv: []string{"MISSING_VAR"},
		},
	})
	result := exec.Execute(orchestrator.ToolCall{ID: "1", Plugin: "test", Action: "do"})
	if result.Error == "" {
		t.Error("expected error when required env missing")
	}
	if result.Error != "required env \"MISSING_VAR\" is not set" {
		t.Errorf("Error = %q", result.Error)
	}
}
