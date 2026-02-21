# Workflow: Jira + GitHub

This example demonstrates how OpenTalon orchestrates a multi-step workflow across two plugins (Jira and GitHub) to turn a Jira ticket into a GitHub pull request — fully automated by the LLM.

## What happens

A user says:

> "Create a PR on GitHub for JIRA-5678"

The LLM orchestrator:

1. Reads the Jira ticket for context (title, description, acceptance criteria)
2. Forks or clones the GitHub repo
3. Creates a feature branch named after the ticket
4. Commits the changes
5. Opens a pull request linking back to the ticket
6. Adds a comment on the Jira ticket with the PR link

```
User ──▶ LLM Orchestrator
              │
              ├──▶ jira.get_issue(key="JIRA-5678")
              │       └── returns: title, description, criteria
              │
              ├──▶ github.clone_repo(owner="myorg", repo="backend")
              │       └── returns: repo cloned
              │
              ├──▶ github.create_branch(name="feature/JIRA-5678")
              │       └── returns: branch created
              │
              ├──▶ github.commit_changes(branch="feature/JIRA-5678", files=[...])
              │       └── returns: commit def456
              │
              ├──▶ github.create_pr(head="feature/JIRA-5678", base="main")
              │       └── returns: PR #123
              │
              └──▶ jira.add_comment(key="JIRA-5678", body="PR: .../pull/123")
                      └── returns: comment added

LLM ──▶ User: "Done! Created PR #123 for JIRA-5678"
```

## Plugins required

### 1. Jira plugin

Same Jira plugin as in the [Jira + GitLab example](../workflow-jira-gitlab/). One plugin works across all workflows.

**Capabilities:**

```yaml
name: jira
description: "Manage Jira issues, comments, and transitions"
actions:
  - name: get_issue
    description: "Get issue details by key"
    parameters:
      - name: key
        description: "Jira issue key (e.g., JIRA-5678)"
        required: true
  - name: create_issue
    description: "Create a new issue"
    parameters:
      - name: project
        required: true
      - name: type
        required: true
      - name: title
        required: true
      - name: description
        required: false
  - name: add_comment
    description: "Add a comment to an issue"
    parameters:
      - name: key
        required: true
      - name: body
        required: true
  - name: transition
    description: "Move issue to a new status"
    parameters:
      - name: key
        required: true
      - name: status
        required: true
```

### 2. GitHub plugin

A gRPC tool plugin (any language) that wraps the GitHub API, GitHub CLI, or GitHub MCP server.

**Capabilities:**

```yaml
name: github
description: "Manage GitHub repositories, branches, and pull requests"
actions:
  - name: clone_repo
    description: "Clone a GitHub repository"
    parameters:
      - name: owner
        description: "Repository owner or organization"
        required: true
      - name: repo
        description: "Repository name"
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
  - name: create_pr
    description: "Create a pull request"
    parameters:
      - name: head
        description: "Source branch"
        required: true
      - name: base
        description: "Target branch (default: main)"
        required: false
      - name: title
        description: "PR title"
        required: true
      - name: body
        description: "PR description"
        required: false
  - name: get_file
    description: "Read a file from the repository"
    parameters:
      - name: owner
        required: true
      - name: repo
        required: true
      - name: path
        description: "File path"
        required: true
      - name: ref
        description: "Branch or commit (default: main)"
        required: false
  - name: list_prs
    description: "List open pull requests"
    parameters:
      - name: owner
        required: true
      - name: repo
        required: true
      - name: state
        description: "Filter by state: open, closed, all (default: open)"
        required: false
  - name: review_pr
    description: "Get PR diff and review comments"
    parameters:
      - name: owner
        required: true
      - name: repo
        required: true
      - name: number
        description: "PR number"
        required: true
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
#   GITHUB_TOKEN=ghp_...
```

## Workflow memory

After the first successful run, the orchestrator saves the pattern:

```yaml
trigger: "create pull request on github for jira ticket"
steps:
  - plugin: jira
    action: get_issue
    order: 1
  - plugin: github
    action: clone_repo
    order: 2
  - plugin: github
    action: create_branch
    order: 3
  - plugin: github
    action: commit_changes
    order: 4
  - plugin: github
    action: create_pr
    order: 5
  - plugin: jira
    action: add_comment
    order: 6
outcome: success
```

## Other workflows this enables

With the same two plugins, the LLM can handle many variations:

- "Review PR #123 and create Jira sub-tasks for each finding" — reads the diff, creates structured feedback
- "What PRs are open for the backend repo?" — lists and summarizes open PRs
- "Close JIRA-5678, it's done — PR #123 was merged" — transitions the ticket
- "Create a hotfix PR for JIRA-9999 based on the v2.1 tag" — branches from a tag
- "Summarize all changes in PR #123 and update JIRA-5678 description" — cross-plugin data flow

## GitHub plugin internals

The GitHub plugin is a black box to the core. Internally it can use:

- **GitHub REST API** — direct HTTP calls to `api.github.com`
- **GitHub GraphQL API** — for complex queries (e.g., PR reviews with threads)
- **GitHub CLI (`gh`)** — shell out to the `gh` command
- **GitHub MCP server** — connect as an MCP client for standardized tool access
- **Any combination** — different actions can use different backends
