package requestpkg

import (
	"strings"
	"testing"
)

const jiraSkillMD = `# Jira create issue

Create a Jira issue in a project.

## Request

Create a Jira issue in OPS with summary X and description Y.

OpenClaw should:

1. Make an HTTP POST to:

{{env.JIRA_URL}}/rest/api/3/issue

2. With body:
` + "```json" + `
{
  "fields": {
    "project": {"key": "{{args.project}}"},
    "summary": "{{args.summary}}",
    "description": "{{args.description}}",
    "issuetype": {"name":"Task"}
  }
}
` + "```" + `

Return the issue key and link.

## Guardrails

Validate that JIRA_API_TOKEN is provided.
Validate that JIRA_URL is provided.

If request fails, return error message.
`

func TestParseSkillMD(t *testing.T) {
	pkg, pluginName, desc, err := ParseSkillMD(jiraSkillMD)
	if err != nil {
		t.Fatalf("ParseSkillMD: %v", err)
	}
	if pluginName == "" {
		t.Error("plugin name empty")
	}
	if desc == "" {
		t.Error("description empty")
	}
	if pkg.Method != "POST" {
		t.Errorf("method = %q", pkg.Method)
	}
	if !strings.Contains(pkg.URL, "JIRA_URL") || !strings.Contains(pkg.URL, "rest/api/3/issue") {
		t.Errorf("url = %q", pkg.URL)
	}
	if !strings.Contains(pkg.Body, "{{args.project}}") {
		t.Errorf("body missing args: %q", pkg.Body)
	}
	hasToken := false
	hasURL := false
	for _, e := range pkg.RequiredEnv {
		if e == "JIRA_API_TOKEN" {
			hasToken = true
		}
		if e == "JIRA_URL" {
			hasURL = true
		}
	}
	if !hasToken || !hasURL {
		t.Errorf("required_env = %v", pkg.RequiredEnv)
	}
}
