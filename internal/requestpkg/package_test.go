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

func TestCleanURLParams(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"no templates", "https://api.example.com/search?q=cats", "https://api.example.com/search?q=cats"},
		{"all resolved", "https://api.example.com/search?q=cats&count=5", "https://api.example.com/search?q=cats&count=5"},
		{"one unresolved", "https://api.example.com/search?q=cats&count={{args.count}}", "https://api.example.com/search?q=cats"},
		{"multiple unresolved", "https://api.example.com/search?q=cats&count={{args.count}}&lang={{args.lang}}", "https://api.example.com/search?q=cats"},
		{"all unresolved", "https://api.example.com/search?q={{args.query}}&count={{args.count}}", "https://api.example.com/search"},
		{"no query string", "https://api.example.com/search", "https://api.example.com/search"},
		{"mixed resolved and unresolved", "https://api.example.com/search?q=cats&count=5&lang={{args.lang}}&country=US", "https://api.example.com/search?q=cats&count=5&country=US"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanURLParams(tt.url)
			if got != tt.want {
				t.Errorf("cleanURLParams() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecutor_Execute_OptionalParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("q") != "test" {
			t.Errorf("q = %q, want %q", q.Get("q"), "test")
		}
		// count should be stripped (not provided by caller)
		if q.Get("count") != "" {
			t.Errorf("count should be empty, got %q", q.Get("count"))
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	exec := NewExecutor("search", []Package{
		{
			Action: "search",
			Method: "GET",
			URL:    srv.URL + "/search?q={{args.query}}&count={{args.count}}",
		},
	})

	result := exec.Execute(orchestrator.ToolCall{
		ID:     "call-opt",
		Plugin: "search",
		Action: "search",
		Args:   map[string]string{"query": "test"},
	})

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
}

func TestEncodeURLParams(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"no encoding needed", "https://api.example.com/search?q=cats", "https://api.example.com/search?q=cats"},
		{"spaces in value", "https://api.example.com/search?q=SpaceX launch news", "https://api.example.com/search?q=SpaceX+launch+news"},
		{"multiple params with spaces", "https://api.example.com/search?q=hello world&lang=en", "https://api.example.com/search?lang=en&q=hello+world"},
		{"no query string", "https://api.example.com/search", "https://api.example.com/search"},
		{"already encoded", "https://api.example.com/search?q=already%20encoded", "https://api.example.com/search?q=already+encoded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := encodeURLParams(tt.url)
			if got != tt.want {
				t.Errorf("encodeURLParams() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecutor_Execute_URLEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("q") != "SpaceX launch news" {
			t.Errorf("q = %q, want %q", q.Get("q"), "SpaceX launch news")
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	exec := NewExecutor("search", []Package{
		{
			Action: "search",
			Method: "GET",
			URL:    srv.URL + "/search?q={{args.query}}",
		},
	})

	result := exec.Execute(orchestrator.ToolCall{
		ID:     "call-enc",
		Plugin: "search",
		Action: "search",
		Args:   map[string]string{"query": "SpaceX launch news"},
	})

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
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
