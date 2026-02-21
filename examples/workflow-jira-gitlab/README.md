# Workflow: Jira + GitLab

This example demonstrates how OpenTalon orchestrates a multi-step workflow across two plugins (Jira and GitLab) to turn a Jira ticket into a GitLab merge request — fully automated by the LLM.

## What happens

A user says:

> "Make a pull request to GitLab for JIRA-1234"

The LLM orchestrator:

1. Reads the Jira ticket for context (title, description, acceptance criteria)
2. Clones the GitLab repo
3. Creates a feature branch named after the ticket
4. Commits the changes
5. Opens a merge request linking back to the ticket
6. Adds a comment on the Jira ticket with the MR link

```
User ──▶ LLM Orchestrator
              │
              ├──▶ jira.get_issue(key="JIRA-1234")
              │       └── returns: title, description, criteria
              │
              ├──▶ gitlab.clone_repo(project="myteam/backend")
              │       └── returns: repo cloned
              │
              ├──▶ gitlab.create_branch(name="feature/JIRA-1234")
              │       └── returns: branch created
              │
              ├──▶ gitlab.commit_changes(branch="feature/JIRA-1234", files=[...])
              │       └── returns: commit abc123
              │
              ├──▶ gitlab.create_mr(source="feature/JIRA-1234", target="main")
              │       └── returns: MR !42
              │
              └──▶ jira.add_comment(key="JIRA-1234", body="MR: .../merge_requests/42")
                      └── returns: comment added

LLM ──▶ User: "Done! Created MR !42 for JIRA-1234"
```

## Plugins required

### 1. Jira plugin

A gRPC tool plugin (any language) that wraps the Jira REST API.

**Capabilities:**

```yaml
name: jira
description: "Manage Jira issues, comments, and transitions"
actions:
  - name: get_issue
    description: "Get issue details by key"
    parameters:
      - name: key
        description: "Jira issue key (e.g., JIRA-1234)"
        required: true
  - name: create_issue
    description: "Create a new issue"
    parameters:
      - name: project
        description: "Project key"
        required: true
      - name: type
        description: "Issue type (Bug, Story, Task)"
        required: true
      - name: title
        description: "Issue summary"
        required: true
      - name: description
        description: "Issue description"
        required: false
  - name: add_comment
    description: "Add a comment to an issue"
    parameters:
      - name: key
        description: "Jira issue key"
        required: true
      - name: body
        description: "Comment text"
        required: true
  - name: transition
    description: "Move issue to a new status"
    parameters:
      - name: key
        description: "Jira issue key"
        required: true
      - name: status
        description: "Target status (e.g., In Progress, Done)"
        required: true
```

### 2. GitLab plugin

A gRPC tool plugin (any language) that wraps the GitLab API or MCP server.

**Capabilities:**

```yaml
name: gitlab
description: "Manage GitLab repositories, branches, and merge requests"
actions:
  - name: clone_repo
    description: "Clone a GitLab repository"
    parameters:
      - name: project
        description: "Project path (e.g., myteam/backend)"
        required: true
  - name: create_branch
    description: "Create a new branch"
    parameters:
      - name: name
        description: "Branch name"
        required: true
      - name: from
        description: "Base branch (default: main)"
        required: false
  - name: commit_changes
    description: "Commit file changes to a branch"
    parameters:
      - name: branch
        description: "Target branch"
        required: true
      - name: message
        description: "Commit message"
        required: true
  - name: create_mr
    description: "Create a merge request"
    parameters:
      - name: source
        description: "Source branch"
        required: true
      - name: target
        description: "Target branch (default: main)"
        required: false
      - name: title
        description: "MR title"
        required: true
      - name: description
        description: "MR description"
        required: false
  - name: get_file
    description: "Read a file from the repository"
    parameters:
      - name: project
        description: "Project path"
        required: true
      - name: path
        description: "File path"
        required: true
      - name: ref
        description: "Branch or commit (default: main)"
        required: false
```

## Configuration

```yaml
# config.yaml
plugins:
  tools:
    plugin_dir: "./plugins"

# Environment variables (never in config):
#   JIRA_BASE_URL=https://mycompany.atlassian.net
#   JIRA_API_TOKEN=...
#   GITLAB_URL=https://gitlab.com
#   GITLAB_TOKEN=glpat-...
```

## Workflow memory

After the first successful run, the orchestrator saves the workflow pattern. Future requests like "create MR for JIRA-5678" skip the planning phase — the LLM already knows the exact sequence.

```yaml
trigger: "make pull request for jira ticket"
steps:
  - plugin: jira
    action: get_issue
    order: 1
  - plugin: gitlab
    action: clone_repo
    order: 2
  - plugin: gitlab
    action: create_branch
    order: 3
  - plugin: gitlab
    action: commit_changes
    order: 4
  - plugin: gitlab
    action: create_mr
    order: 5
  - plugin: jira
    action: add_comment
    order: 6
outcome: success
```

## Other workflows this enables

With the same two plugins, the LLM can handle many variations:

- "Close JIRA-1234 and link the MR" — transitions the ticket to Done
- "What's the status of JIRA-1234?" — reads the ticket and summarizes
- "Review the code in MR !42 and add findings to JIRA-1234" — reads the MR diff, creates Jira sub-tasks for each finding
- "Create a hotfix branch for JIRA-9999 from the release tag" — branches from a tag instead of main
