package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// TestPaneCallerServeChildren covers the code gff-hab itself runs (hab-logic-api.md 4.1, 3.10):
//
//   - gff-hab's own pid is a root: a child it starts that dials the socket with a cleared
//     environment is a pane caller. A git wrapper on gff-hab's PATH stands in for a hook or a
//     lane check, any code gff-hab runs that the agent may have written.
//   - gff-hab's git calls run no hooks: a repository-local core.hooksPath with a post-checkout
//     hook does not run on worktree.create.
//   - A process inside a pane gives no yes to a pending setup script: worktree.setup and
//     setup: run on worktree.create are denied, and a rerun of a script the user approved works.
func TestPaneCallerServeChildren(t *testing.T) {
	t.Parallel()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	d := shortDir(t)
	hdir, state, repo, root, work := filepath.Join(d, "h"), filepath.Join(d, "state"), filepath.Join(d, "repo"),
		filepath.Join(d, "wt"), filepath.Join(d, "w")
	bindir := filepath.Join(d, "bin")
	for _, dir := range []string{hdir, repo, work, bindir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(work, "panecaller")
	if out, err := exec.Command("go", "build", "-o", helper, "./testdata/panecaller").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	gitEnv := []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), gitEnv...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(p, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "remote", "add", "origin", "https://example.com/team/thing.git")
	write(filepath.Join(repo, ".gitignore"), "*.ran\n", 0o644)
	write(filepath.Join(repo, "flightdeck.json"), `{"worktree": {"setup": "echo ran >> setup.ran"}}`, 0o644)
	git(repo, "add", ".")
	git(repo, "commit", "-q", "-m", "first")
	git(repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(repo, "remote", "add", "fork", "https://example.com/someone/thing.git")
	git(repo, "update-ref", "refs/remotes/fork/patch", "HEAD")

	// A repository-local hook, as an agent with file writes could add. Plain git runs it.
	hookRan := filepath.Join(d, "hook.ran")
	if err := os.MkdirAll(filepath.Join(repo, ".hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(repo, ".hooks", "post-checkout"), "#!/bin/sh\ntouch '"+hookRan+"'\n", 0o755)
	git(repo, "config", "core.hooksPath", ".hooks")
	git(repo, "worktree", "add", "-q", "-b", "plain", filepath.Join(d, "plain"))
	if _, err := os.Stat(hookRan); err != nil {
		t.Fatal("plain git did not run the repository's hook; the check below would prove nothing")
	}
	os.Remove(hookRan)

	// gff-hab's git: a wrapper that, on the first worktree add, starts the helper with a cleared
	// environment to dial the socket, then runs the real git.
	sock := filepath.Join(d, "run", "hab.sock")
	childOut := filepath.Join(work, "child.out")
	write(filepath.Join(bindir, "git"), "#!/bin/sh\n"+
		"case \"$*\" in *'worktree add'*)\n"+
		"  if mkdir '"+childOut+".lock' 2>/dev/null; then env -i '"+helper+"' dial '"+sock+"' '"+childOut+"' ''; fi;;\n"+
		"esac\n"+
		"exec '"+realGit+"' \"$@\"\n", 0o755)

	startHolder(t, hdir, "one")
	env := append(append([]string{}, gitEnv...), noRaceExitSleep, "PATH="+bindir+":"+os.Getenv("PATH"))
	startServeWith(t, sock, state, env, "-holders", hdir, "-worktrees", root)

	call := func(method string, params, res any) {
		t.Helper()
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, method, string(b))), res); err != nil {
			t.Fatal(err)
		}
	}
	p := api.WorktreeProject{ProjectDir: repo}

	// A foreign start point: the setup waits for the user's yes.
	var f api.WorktreeCreated
	call("worktree.create", api.WorktreeCreateParams{WorktreeProject: p, Branch: "try", Base: "fork/patch"}, &f)
	if f.Setup.State != "pending" {
		t.Fatalf("foreign create %+v", f)
	}
	b, err := os.ReadFile(childOut)
	if err != nil {
		t.Fatalf("the git wrapper's child wrote nothing: %v", err)
	}
	if want := "hello ok\ndevices.list denied\nstructure.get ok\nhello-hab denied\n"; string(b) != want {
		t.Errorf("a child of gff-hab got\n%s\nwant\n%s", b, want)
	}
	if _, err := os.Stat(hookRan); err == nil {
		t.Error("worktree.create ran the repository's post-checkout hook")
	}

	// From a pane: no yes to the pending script, no setup run on create.
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(60*time.Second))
	defer cancel()
	c := (&habEnv{t: t, sock: sock}).control()
	paneCalls := func(name string, pairs ...string) string {
		t.Helper()
		out := filepath.Join(work, name+".out")
		var res api.SessionCreated
		argv := append([]string{helper, "call", sock, out}, pairs...)
		if err := c.Call(ctx, "session.create", api.SessionCreateParams{Argv: argv, Cwd: work, Cols: 80, Rows: 24}, &res); err != nil {
			t.Fatal(err)
		}
		var got []byte
		waitUntil(t, name+": the pane process's answers", func() bool {
			var err error
			got, err = os.ReadFile(out)
			return err == nil
		})
		return string(got)
	}
	setupParams := func() string {
		b, _ := json.Marshal(api.WorktreeSetupParams{WorktreeProject: p, Path: f.Worktree.Path})
		return string(b)
	}
	runParams, _ := json.Marshal(api.WorktreeCreateParams{WorktreeProject: p, Branch: "run", Base: "fork/patch", Setup: "run"})
	got := paneCalls("refused", "worktree.setup", setupParams(), "worktree.create", string(runParams))
	if want := "worktree.setup denied\nworktree.create denied\n"; got != want {
		t.Errorf("from a pane before the yes, got\n%s\nwant\n%s", got, want)
	}
	if _, err := os.Stat(filepath.Join(f.Worktree.Path, "setup.ran")); err == nil {
		t.Fatal("setup ran without the user's yes")
	}

	// The user says yes; then a pane may run the approved script again.
	var s api.WorktreeSetupResult
	call("worktree.setup", api.WorktreeSetupParams{WorktreeProject: p, Path: f.Worktree.Path}, &s)
	if s.Setup.State != "started" {
		t.Fatalf("the user's yes %+v", s)
	}
	if got := paneCalls("rerun", "worktree.setup", setupParams()); got != "worktree.setup ok\n" {
		t.Errorf("a rerun from a pane after the yes: %s", got)
	}
	waitUntil(t, "two setup runs", func() bool {
		b, _ := os.ReadFile(filepath.Join(f.Worktree.Path, "setup.ran"))
		return string(b) == "ran\nran\n"
	})
}
