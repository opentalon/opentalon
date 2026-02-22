package bundle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	githubPrefix = "https://github.com/"
	gitBin       = "git"
	goBin        = "go"
)

// ResolveRef resolves a ref (branch, tag, or partial commit) to a full commit SHA using git ls-remote.
// If ref is already a 40-char hex commit SHA, it is returned as-is.
func ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	if commitSHARegex.MatchString(ref) {
		return ref, nil
	}
	repoURL := repoURL(repo)
	cmd := exec.CommandContext(ctx, gitBin, "ls-remote", repoURL, ref)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote %s %s: %w", repoURL, ref, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", fmt.Errorf("git ls-remote: no result for ref %q", ref)
	}
	// First column is the commit SHA
	fields := strings.Fields(lines[0])
	if len(fields) < 1 {
		return "", fmt.Errorf("git ls-remote: invalid output")
	}
	sha := fields[0]
	if !commitSHARegex.MatchString(sha) {
		return "", fmt.Errorf("git ls-remote: invalid sha %q", sha)
	}
	return sha, nil
}

var commitSHARegex = regexp.MustCompile(`^[0-9a-f]{40}$`)

func repoURL(repo string) string {
	repo = strings.TrimSpace(repo)
	if strings.HasPrefix(repo, "https://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	return githubPrefix + strings.TrimPrefix(repo, "/") + ".git"
}

// CloneAndBuild clones the repo at ref into dir and runs `go build -o binaryName .`.
// resolvedSHA is the commit from ResolveRef; we checkout that commit for reproducibility.
func CloneAndBuild(ctx context.Context, repo, ref, resolvedSHA, dir, binaryName string) (binaryPath string, err error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return "", fmt.Errorf("create parent dir: %w", err)
	}
	_ = os.RemoveAll(dir)

	repoURL := repoURL(repo)
	isCommit := commitSHARegex.MatchString(ref)

	var cloneCmd *exec.Cmd
	if isCommit {
		cloneCmd = exec.CommandContext(ctx, gitBin, "clone", "--depth", "1", repoURL, dir)
	} else {
		cloneCmd = exec.CommandContext(ctx, gitBin, "clone", "--depth", "1", "--branch", ref, repoURL, dir)
	}
	if output, err := cloneCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone: %w (output: %s)", err, string(output))
	}

	checkoutTarget := resolvedSHA
	if checkoutTarget == "" {
		checkoutTarget = ref
	}
	if checkoutTarget != "" {
		checkout := exec.CommandContext(ctx, gitBin, "checkout", checkoutTarget)
		checkout.Dir = dir
		if output, err := checkout.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git checkout %s: %w (output: %s)", checkoutTarget, err, string(output))
		}
	}

	binaryPath = filepath.Join(dir, binaryName)
	buildCmd := exec.CommandContext(ctx, goBin, "build", "-o", binaryName, ".")
	buildCmd.Dir = dir
	if output, err := buildCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w (output: %s)", err, string(output))
	}
	return binaryPath, nil
}

// CloneOnly clones the repo at ref into dir and checkouts resolvedSHA (no build).
func CloneOnly(ctx context.Context, repo, ref, resolvedSHA, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	_ = os.RemoveAll(dir)

	repoURL := repoURL(repo)
	isCommit := commitSHARegex.MatchString(ref)

	var cloneCmd *exec.Cmd
	if isCommit {
		cloneCmd = exec.CommandContext(ctx, gitBin, "clone", "--depth", "1", repoURL, dir)
	} else {
		cloneCmd = exec.CommandContext(ctx, gitBin, "clone", "--depth", "1", "--branch", ref, repoURL, dir)
	}
	if output, err := cloneCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone: %w (output: %s)", err, string(output))
	}

	checkoutTarget := resolvedSHA
	if checkoutTarget == "" {
		checkoutTarget = ref
	}
	if checkoutTarget != "" {
		checkout := exec.CommandContext(ctx, gitBin, "checkout", checkoutTarget)
		checkout.Dir = dir
		if output, err := checkout.CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout %s: %w (output: %s)", checkoutTarget, err, string(output))
		}
	}
	return nil
}

// EnsureSkillDir ensures a single-skill repo is present under stateDir/skills/<name>/,
// clones only (no build), updates skills.lock, and returns the path to the skill directory.
func EnsureSkillDir(ctx context.Context, stateDir, name, github, ref string) (string, error) {
	if github == "" || ref == "" {
		return "", fmt.Errorf("github and ref are required for skill %q", name)
	}

	lock, err := LoadSkillsLock(stateDir)
	if err != nil {
		return "", err
	}

	entry, locked := lock.Skills[name]
	if locked && entry.GitHub == github && entry.Ref == ref && entry.Resolved != "" && entry.Path != "" {
		absPath := entry.Path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(stateDir, entry.Path)
		}
		if _, err := os.Stat(absPath); err == nil {
			return absPath, nil
		}
	}

	resolved, err := ResolveRef(ctx, github, ref)
	if err != nil {
		return "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}

	skillDir := filepath.Join(stateDir, "skills", name)
	if err := CloneOnly(ctx, github, ref, resolved, skillDir); err != nil {
		return "", err
	}

	relPath, _ := filepath.Rel(stateDir, skillDir)
	if relPath == "" || strings.HasPrefix(relPath, "..") {
		relPath = skillDir
	}
	lock.Skills[name] = LockEntry{
		GitHub:   github,
		Ref:      ref,
		Resolved: resolved,
		Path:     relPath,
	}
	if err := SaveSkillsLock(stateDir, lock); err != nil {
		return "", err
	}
	return skillDir, nil
}

// sanitizeRepoName returns a dir-safe name for the repo (e.g. "owner/repo" -> "owner-repo").
func sanitizeRepoName(repo string) string {
	return strings.ReplaceAll(strings.TrimSpace(repo), "/", "-")
}

// EnsureSkillsRepo ensures the default monorepo (one repo with many skill subdirs) is present
// under stateDir/skills/<repo-name>/, clones only, updates skills.lock Repo, and returns the repo root path.
func EnsureSkillsRepo(ctx context.Context, stateDir, github, ref string) (string, error) {
	if github == "" || ref == "" {
		return "", fmt.Errorf("github and ref are required for skills repo")
	}

	lock, err := LoadSkillsLock(stateDir)
	if err != nil {
		return "", err
	}

	entry := lock.Repo
	if entry != nil && entry.GitHub == github && entry.Ref == ref && entry.Resolved != "" && entry.Path != "" {
		absPath := entry.Path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(stateDir, entry.Path)
		}
		if _, err := os.Stat(absPath); err == nil {
			return absPath, nil
		}
	}

	resolved, err := ResolveRef(ctx, github, ref)
	if err != nil {
		return "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}

	repoDir := filepath.Join(stateDir, "skills", sanitizeRepoName(github))
	if err := CloneOnly(ctx, github, ref, resolved, repoDir); err != nil {
		return "", err
	}

	relPath, _ := filepath.Rel(stateDir, repoDir)
	if relPath == "" || strings.HasPrefix(relPath, "..") {
		relPath = repoDir
	}
	lock.Repo = &LockEntry{
		GitHub:   github,
		Ref:      ref,
		Resolved: resolved,
		Path:     relPath,
	}
	if err := SaveSkillsLock(stateDir, lock); err != nil {
		return "", err
	}
	return repoDir, nil
}

// EnsureLuaPluginsRepo ensures the default Lua plugins repo is present under stateDir/lua_plugins/<repo-name>/,
// clones only, updates lua_plugins.lock Repo, and returns the repo root path.
func EnsureLuaPluginsRepo(ctx context.Context, stateDir, github, ref string) (string, error) {
	if github == "" || ref == "" {
		return "", fmt.Errorf("github and ref are required for Lua plugins repo")
	}
	lock, err := LoadLuaPluginsLock(stateDir)
	if err != nil {
		return "", err
	}
	entry := lock.Repo
	if entry != nil && entry.GitHub == github && entry.Ref == ref && entry.Resolved != "" && entry.Path != "" {
		absPath := entry.Path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(stateDir, entry.Path)
		}
		if _, err := os.Stat(absPath); err == nil {
			return absPath, nil
		}
	}
	resolved, err := ResolveRef(ctx, github, ref)
	if err != nil {
		return "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}
	repoDir := filepath.Join(stateDir, "lua_plugins", sanitizeRepoName(github))
	if err := CloneOnly(ctx, github, ref, resolved, repoDir); err != nil {
		return "", err
	}
	relPath, _ := filepath.Rel(stateDir, repoDir)
	if relPath == "" || strings.HasPrefix(relPath, "..") {
		relPath = repoDir
	}
	lock.Repo = &LockEntry{
		GitHub:   github,
		Ref:      ref,
		Resolved: resolved,
		Path:     relPath,
	}
	if err := SaveLuaPluginsLock(stateDir, lock); err != nil {
		return "", err
	}
	return repoDir, nil
}

// EnsureLuaPluginDir ensures a single Lua plugin repo is present under stateDir/lua_plugins/<name>/,
// clones only, updates lua_plugins.lock, and returns the plugin directory path.
func EnsureLuaPluginDir(ctx context.Context, stateDir, name, github, ref string) (string, error) {
	if github == "" || ref == "" {
		return "", fmt.Errorf("github and ref are required for Lua plugin %q", name)
	}
	lock, err := LoadLuaPluginsLock(stateDir)
	if err != nil {
		return "", err
	}
	entry, locked := lock.Plugins[name]
	if locked && entry.GitHub == github && entry.Ref == ref && entry.Resolved != "" && entry.Path != "" {
		absPath := entry.Path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(stateDir, entry.Path)
		}
		if _, err := os.Stat(absPath); err == nil {
			return absPath, nil
		}
	}
	resolved, err := ResolveRef(ctx, github, ref)
	if err != nil {
		return "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}
	pluginDir := filepath.Join(stateDir, "lua_plugins", name)
	if err := CloneOnly(ctx, github, ref, resolved, pluginDir); err != nil {
		return "", err
	}
	relPath, _ := filepath.Rel(stateDir, pluginDir)
	if relPath == "" || strings.HasPrefix(relPath, "..") {
		relPath = pluginDir
	}
	lock.Plugins[name] = LockEntry{
		GitHub:   github,
		Ref:      ref,
		Resolved: resolved,
		Path:     relPath,
	}
	if err := SaveLuaPluginsLock(stateDir, lock); err != nil {
		return "", err
	}
	return pluginDir, nil
}

// EnsurePlugin ensures the plugin is present under stateDir/plugins/<name>/,
// resolves ref to a commit, clones and builds if needed, updates plugins.lock, and returns the path to the binary.
func EnsurePlugin(ctx context.Context, stateDir, name, github, ref string) (path string, err error) {
	if github == "" || ref == "" {
		return "", fmt.Errorf("github and ref are required")
	}

	lock, err := LoadPluginsLock(stateDir)
	if err != nil {
		return "", err
	}

	entry, locked := lock.Plugins[name]
	if locked && entry.GitHub == github && entry.Ref == ref && entry.Resolved != "" && entry.Path != "" {
		absPath := entry.Path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(stateDir, entry.Path)
		}
		if _, err := os.Stat(absPath); err == nil {
			return absPath, nil
		}
	}

	resolved, err := ResolveRef(ctx, github, ref)
	if err != nil {
		return "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}

	pluginDir := filepath.Join(stateDir, "plugins", name)
	binaryName := name
	if !strings.Contains(binaryName, "-") {
		binaryName = name + "-plugin"
	}

	builtPath, err := CloneAndBuild(ctx, github, ref, resolved, pluginDir, binaryName)
	if err != nil {
		return "", err
	}

	relPath, _ := filepath.Rel(stateDir, builtPath)
	if relPath == "" || strings.HasPrefix(relPath, "..") {
		relPath = builtPath
	}
	lock.Plugins[name] = LockEntry{
		GitHub:   github,
		Ref:      ref,
		Resolved: resolved,
		Path:     relPath,
	}
	if err := SavePluginsLock(stateDir, lock); err != nil {
		return "", err
	}
	return builtPath, nil
}

// EnsureChannel ensures the channel is present under stateDir/channels/<name>/,
// resolves ref, clones and builds, updates channels.lock, and returns the path to the binary.
func EnsureChannel(ctx context.Context, stateDir, name, github, ref string) (path string, err error) {
	if github == "" || ref == "" {
		return "", fmt.Errorf("github and ref are required")
	}

	lock, err := LoadChannelsLock(stateDir)
	if err != nil {
		return "", err
	}

	entry, locked := lock.Channels[name]
	if locked && entry.GitHub == github && entry.Ref == ref && entry.Resolved != "" && entry.Path != "" {
		absPath := entry.Path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(stateDir, entry.Path)
		}
		if _, err := os.Stat(absPath); err == nil {
			return absPath, nil
		}
	}

	resolved, err := ResolveRef(ctx, github, ref)
	if err != nil {
		return "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}

	channelDir := filepath.Join(stateDir, "channels", name)
	binaryName := name
	if !strings.Contains(binaryName, "-") {
		binaryName = name + "-channel"
	}

	builtPath, err := CloneAndBuild(ctx, github, ref, resolved, channelDir, binaryName)
	if err != nil {
		return "", err
	}

	relPath, _ := filepath.Rel(stateDir, builtPath)
	if relPath == "" || strings.HasPrefix(relPath, "..") {
		relPath = builtPath
	}
	lock.Channels[name] = LockEntry{
		GitHub:   github,
		Ref:      ref,
		Resolved: resolved,
		Path:     relPath,
	}
	if err := SaveChannelsLock(stateDir, lock); err != nil {
		return "", err
	}
	return builtPath, nil
}
