package testgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHermeticIgnoresGlobalConfig: a global config with a hooks path whose post-checkout hook
// fails, and a default branch, has no effect on git once Hermetic ran.
func TestHermeticIgnoresGlobalConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home := t.TempDir()
	hooks := filepath.Join(home, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, "gitconfig")
	cfg := "[core]\n\thooksPath = " + hooks + "\n[init]\n\tdefaultBranch = trunk\n[user]\n\tname = Someone\n"
	if err := os.WriteFile(global, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	git := func(env []string, args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	// Without Hermetic the global config applies: the hook fails the worktree add.
	plain := append(os.Environ(), "GIT_CONFIG_GLOBAL="+global, "GIT_AUTHOR_NAME=x", "GIT_AUTHOR_EMAIL=x@example.invalid",
		"GIT_COMMITTER_NAME=x", "GIT_COMMITTER_EMAIL=x@example.invalid")
	if out, err := git(plain, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := git(plain, "commit", "-q", "--allow-empty", "-m", "s"); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	if _, err := git(plain, "worktree", "add", "-q", filepath.Join(t.TempDir(), "a"), "HEAD"); err == nil {
		t.Fatal("the failing global hook did not fail the worktree add; the test proves nothing")
	}

	t.Setenv("GIT_CONFIG_GLOBAL", global) // what a developer's environment could hold
	for k, v := range Env() {
		t.Setenv(k, v) // Hermetic, undone when the test ends
	}
	env := os.Environ()
	if out, err := git(env, "worktree", "add", "-q", filepath.Join(t.TempDir(), "b"), "HEAD"); err != nil {
		t.Fatalf("worktree add under Hermetic: %v\n%s", err, out)
	}
	if out, _ := git(env, "config", "--get", "user.name"); out != "" {
		t.Errorf("user.name %q reached git through the global config", out)
	}
	if out, err := git(env, "commit", "-q", "--allow-empty", "-m", "t"); err != nil {
		t.Fatalf("commit under Hermetic: %v\n%s", err, out)
	}
	if out, _ := git(env, "log", "-1", "--format=%an <%ae>"); out != Name+" <"+Email+">" {
		t.Errorf("author %q, want %s <%s>", out, Name, Email)
	}
}
