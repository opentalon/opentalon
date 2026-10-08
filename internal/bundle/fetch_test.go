package bundle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func repoWithTwoCommits(t *testing.T) (url, first, second string) {
	t.Helper()
	return repoWithTwoCommitsOf(t, "version.txt")
}

func repoWithTwoCommitsOf(t *testing.T, file string) (url, first, second string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "master")
	if err := os.WriteFile(filepath.Join(dir, file), []byte("one"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "one")
	first = gitRun(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, file), []byte("two"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "commit", "-q", "-am", "two")
	second = gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "config", "uploadpack.allowReachableSHA1InWant", "true")
	return "file://" + dir, first, second
}

func readVersion(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "version.txt"))
	if err != nil {
		t.Fatalf("read checkout: %v", err)
	}
	return string(b)
}

func TestCloneOnlyPinnedCommitBehindDefaultBranchHead(t *testing.T) {
	url, first, _ := repoWithTwoCommits(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := CloneOnly(context.Background(), url, first, first, dir); err != nil {
		t.Fatalf("CloneOnly at non-HEAD pinned commit %s: %v", first, err)
	}
	if got := readVersion(t, dir); got != "one" {
		t.Errorf("checked out %q, want %q", got, "one")
	}
}

func TestCloneOnlyPinnedCommitAtHead(t *testing.T) {
	url, _, second := repoWithTwoCommits(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := CloneOnly(context.Background(), url, second, second, dir); err != nil {
		t.Fatalf("CloneOnly at HEAD commit: %v", err)
	}
	if got := readVersion(t, dir); got != "two" {
		t.Errorf("checked out %q, want %q", got, "two")
	}
}

func TestCloneOnlyBranchRefWithStaleResolvedSHA(t *testing.T) {
	url, first, _ := repoWithTwoCommits(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := CloneOnly(context.Background(), url, "master", first, dir); err != nil {
		t.Fatalf("CloneOnly branch ref with lockfile SHA behind branch head: %v", err)
	}
	if got := readVersion(t, dir); got != "one" {
		t.Errorf("checked out %q, want %q", got, "one")
	}
}

func TestCloneOnlyBranchRefWithoutResolvedSHA(t *testing.T) {
	url, _, _ := repoWithTwoCommits(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := CloneOnly(context.Background(), url, "master", "", dir); err != nil {
		t.Fatalf("CloneOnly branch ref: %v", err)
	}
	if got := readVersion(t, dir); got != "two" {
		t.Errorf("checked out %q, want %q", got, "two")
	}
}

func TestCloneOnlyUnknownCommitFails(t *testing.T) {
	url, _, _ := repoWithTwoCommits(t)
	missing := strings.Repeat("a", 40)
	dir := filepath.Join(t.TempDir(), "checkout")
	err := CloneOnly(context.Background(), url, missing, missing, dir)
	if err == nil {
		t.Fatal("expected error for a commit that does not exist upstream")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the missing commit, got: %v", err)
	}
}

func TestCloneOnlyUnknownBranchFails(t *testing.T) {
	url, _, _ := repoWithTwoCommits(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := CloneOnly(context.Background(), url, "no-such-branch", "", dir); err == nil {
		t.Fatal("expected error for a branch that does not exist upstream")
	}
}

func TestCloneAndBuildYAMLRepoPinnedCommitBehindDefaultBranchHead(t *testing.T) {
	url, first, _ := repoWithTwoCommitsOf(t, "channel.yaml")
	dir := filepath.Join(t.TempDir(), "checkout")
	path, err := CloneAndBuild(context.Background(), url, first, first, dir, "unused", nil)
	if err != nil {
		t.Fatalf("CloneAndBuild at non-HEAD pinned commit %s: %v", first, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != "one" {
		t.Errorf("checked out %q, want %q", b, "one")
	}
}

func TestCloneAndBuildUnknownCommitFails(t *testing.T) {
	url, _, _ := repoWithTwoCommitsOf(t, "channel.yaml")
	missing := strings.Repeat("b", 40)
	dir := filepath.Join(t.TempDir(), "checkout")
	_, err := CloneAndBuild(context.Background(), url, missing, missing, dir, "unused", nil)
	if err == nil {
		t.Fatal("expected error for a commit that does not exist upstream")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error should name the missing commit, got: %v", err)
	}
}

func TestRepoURL(t *testing.T) {
	cases := map[string]string{
		"opentalon/example-plugin":   "https://github.com/opentalon/example-plugin.git",
		"/opentalon/example-plugin":  "https://github.com/opentalon/example-plugin.git",
		"https://gitlab.com/a/b.git": "https://gitlab.com/a/b.git",
		"git@github.com:a/b.git":     "git@github.com:a/b.git",
		"file:///tmp/some/repo":      "file:///tmp/some/repo",
	}
	for in, want := range cases {
		if got := repoURL(in); got != want {
			t.Errorf("repoURL(%q) = %q, want %q", in, got, want)
		}
	}
}
