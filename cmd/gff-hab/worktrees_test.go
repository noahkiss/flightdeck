package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestWorktreeRoot: the flag, then $GFF_HAB_WORKTREES, then ~/.gff/worktrees; ~/ expands; a
// relative folder is refused.
func TestWorktreeRoot(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	for _, c := range []struct {
		flag, env, want string
		bad             bool
	}{
		{"", "", "/h/.gff/worktrees", false},
		{"", "/srv/wt", "/srv/wt", false},
		{"/f", "/srv/wt", "/f", false},
		{"~/wt/", "", "/h/wt", false},
		{"rel", "", "", true},
	} {
		got, err := worktreeRoot(c.flag, env(c.env), "/h")
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("worktreeRoot(%q, %q) = %q, %v", c.flag, c.env, got, err)
		}
	}
}

// TestWorktrees is the worktree methods end to end, with the real holder, gff-hab and git:
// worktree.create copies the included files and runs the setup script as a session; a start
// point from another remote waits for worktree.setup; worktree.list marks the hab's worktrees;
// worktree.remove refuses uncommitted changes without force, runs the teardown script, keeps the
// branch, and never removes the main working tree.
func TestWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	d := shortDir(t)
	hdir, state, repo, root := filepath.Join(d, "h"), filepath.Join(d, "state"), filepath.Join(d, "repo"), filepath.Join(d, "wt")
	for _, dir := range []string{hdir, repo} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// git, for the test and for gff-hab, without the user's config.
	gitEnv := []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), gitEnv...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "remote", "add", "origin", "https://example.com/team/thing.git")
	write(filepath.Join(repo, ".gitignore"), ".env\n*.ran\n")
	write(filepath.Join(repo, ".worktreeinclude"), ".env\n")
	write(filepath.Join(repo, "flightdeck.json"), `{"worktree": {
  "setup": ["echo \"$GFF_BRANCH $GFF_SOURCE_CHECKOUT\" > setup.ran", "cat .env >> setup.ran"],
  "teardown": "echo \"$GFF_WORKTREE\" > \"$GFF_SOURCE_CHECKOUT/teardown.ran\""
}}`)
	git(repo, "add", ".")
	git(repo, "commit", "-q", "-m", "first")
	git(repo, "update-ref", "refs/remotes/origin/main", "HEAD") // as if pushed: the code is the project's own
	write(filepath.Join(repo, ".env"), "TOKEN=local\n")
	top := git(repo, "rev-parse", "--show-toplevel")

	sock := filepath.Join(d, "run", "hab.sock")
	startHolder(t, hdir, "one")
	startServeWith(t, sock, state, append(gitEnv, noRaceExitSleep), "-holders", hdir, "-worktrees", root)

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
	structureNow := func() api.StructureResult {
		var st api.StructureResult
		call("structure.get", struct{}{}, &st)
		return st
	}
	sessionOf := func(id string) api.Session {
		for _, s := range structureNow().Sessions {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("no session %s", id)
		return api.Session{}
	}
	waitSetup := func(s api.WorktreeSetup) {
		t.Helper()
		waitUntil(t, "the setup session to exit", func() bool { return sessionOf(s.Session).State == api.SessionStateExited })
		if ex := sessionOf(s.Session).Exit; ex == nil || ex.Code == nil || *ex.Code != 0 {
			t.Fatalf("setup exit %+v", ex)
		}
	}

	// Create: the included file is copied before setup, which runs in a session of the project.
	var c api.WorktreeCreated
	call("worktree.create", api.WorktreeCreateParams{WorktreeProject: api.WorktreeProject{ProjectDir: repo}, Branch: "feat/x"}, &c)
	wantPath := filepath.Join(root, "repo", "feat-x")
	if canonDir(c.Worktree.Path) != canonDir(wantPath) || c.Worktree.Branch != "feat/x" || !c.Worktree.Managed || c.Worktree.Main {
		t.Fatalf("created %+v, want path %s", c.Worktree, wantPath)
	}
	if !slices.Equal(c.Copied, []string{".env"}) || c.Setup.State != "started" || c.Setup.Session == "" || c.Setup.Pane == "" {
		t.Fatalf("created %+v", c)
	}
	wantPath = c.Worktree.Path // as git reports it, symbolic links resolved
	waitSetup(c.Setup)
	if b, err := os.ReadFile(filepath.Join(wantPath, "setup.ran")); err != nil || string(b) != "feat/x "+top+"\nTOKEN=local\n" {
		t.Fatalf("setup.ran %q %v", b, err)
	}
	if s := sessionOf(c.Setup.Session); s.Project != c.Project || !strings.HasPrefix(s.Name, "setup-feat-x") {
		t.Fatalf("setup session %+v", s)
	}

	// A start point from another remote waits for a yes.
	git(repo, "remote", "add", "fork", "https://example.com/someone/thing.git")
	git(repo, "update-ref", "refs/remotes/fork/patch", "HEAD")
	var f api.WorktreeCreated
	call("worktree.create", api.WorktreeCreateParams{WorktreeProject: api.WorktreeProject{Project: c.Project}, Branch: "try", Base: "fork/patch"}, &f)
	if f.Setup.State != "pending" || f.Setup.Session != "" || !strings.Contains(f.Setup.Reason, "fork") ||
		!strings.Contains(f.Setup.Script, "setup.ran") || !strings.Contains(f.Setup.TeardownScript, "teardown.ran") {
		t.Fatalf("foreign create %+v", f)
	}
	if _, err := os.Stat(filepath.Join(f.Worktree.Path, "setup.ran")); err == nil {
		t.Fatal("setup ran without a yes")
	}
	var s api.WorktreeSetupResult
	call("worktree.setup", api.WorktreeSetupParams{WorktreeProject: api.WorktreeProject{ProjectDir: repo}, Path: f.Worktree.Path}, &s)
	if s.Setup.State != "started" {
		t.Fatalf("setup after the yes %+v", s)
	}
	waitSetup(s.Setup)

	// List through the CLI, from the repository folder.
	var l api.WorktreeList
	if err := json.Unmarshal([]byte(run(t, "worktree", "list", "-socket", sock, "-dir", repo)), &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Worktrees) != 3 || !l.Worktrees[0].Main || l.Worktrees[0].Managed || l.Project != c.Project {
		t.Fatalf("list %+v", l)
	}
	for _, w := range l.Worktrees[1:] {
		if !w.Managed || w.Main {
			t.Fatalf("listed %+v", w)
		}
	}

	// Removal: refused with an untracked file, never for the main working tree.
	write(filepath.Join(wantPath, "work.txt"), "unsaved\n")
	p := api.WorktreeProject{ProjectDir: repo}
	if kind := callErr(t, sock, "worktree.remove", api.WorktreeRemoveParams{WorktreeProject: p, Path: wantPath}); kind != "conflict" {
		t.Errorf("dirty remove: %s, want conflict", kind)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "work.txt")); err != nil {
		t.Fatal("a refused removal lost a file")
	}
	if kind := callErr(t, sock, "worktree.remove", api.WorktreeRemoveParams{WorktreeProject: p, Path: repo, Force: true}); kind != "invalid" {
		t.Errorf("remove the main working tree: %s, want invalid", kind)
	}
	if kind := callErr(t, sock, "worktree.list", api.WorktreeListParams{WorktreeProject: api.WorktreeProject{Project: c.Project, ProjectDir: repo}}); kind != "invalid" {
		t.Errorf("project and project_dir: %s, want invalid", kind)
	}
	if kind := callErr(t, sock, "worktree.list", api.WorktreeListParams{WorktreeProject: api.WorktreeProject{Project: "p-nope"}}); kind != "not_found" {
		t.Errorf("unknown project: %s, want not_found", kind)
	}
	if kind := callErr(t, sock, "worktree.create", api.WorktreeCreateParams{WorktreeProject: p, Setup: "maybe"}); kind != "invalid" {
		t.Errorf("unknown setup mode: %s, want invalid", kind)
	}

	// The teardown gate. No teardown.ran may appear until feat/x goes below.
	teardownRan := func(what string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(repo, "teardown.ran")); err == nil {
			t.Fatalf("%s: the teardown ran", what)
		}
	}
	removeGate := func(what, path string, managed bool, why string) {
		t.Helper()
		var r api.WorktreeRemoved
		call("worktree.remove", api.WorktreeRemoveParams{WorktreeProject: p, Path: path}, &r)
		if r.Teardown != "skipped" || r.Managed != managed || !strings.Contains(r.TeardownOutput, why) {
			t.Fatalf("%s: removed %+v", what, r)
		}
		teardownRan(what)
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s: the folder is still there", what)
		}
	}
	// Foreign and never approved: no teardown.
	var g api.WorktreeCreated
	call("worktree.create", api.WorktreeCreateParams{WorktreeProject: p, Branch: "gate", Base: "fork/patch"}, &g)
	if g.Setup.State != "pending" {
		t.Fatalf("gate %+v", g)
	}
	removeGate("a worktree with no yes", g.Worktree.Path, true, "never said yes")
	// Approved, then its scripts changed (a pull from somewhere, say): no teardown.
	commitConfig := func(wt, teardown string) {
		t.Helper()
		write(filepath.Join(wt, "flightdeck.json"), `{"worktree": {"teardown": "`+teardown+`"}}`)
		git(wt, "commit", "-q", "-am", "change the scripts")
	}
	var ch api.WorktreeCreated
	call("worktree.create", api.WorktreeCreateParams{WorktreeProject: p, Branch: "changed"}, &ch)
	waitSetup(ch.Setup)
	commitConfig(ch.Worktree.Path, `echo changed > \"$GFF_SOURCE_CHECKOUT/teardown.ran\"`)
	removeGate("changed scripts", ch.Worktree.Path, true, "changed since")
	// A worktree made by hand: managed false, its scripts never approved.
	hand := filepath.Join(d, "hand")
	git(repo, "worktree", "add", "-q", "-b", "hand", hand)
	removeGate("a hand-made worktree", hand, false, "never said yes")

	// A teardown that leaves files: the removal stops with the worktree whole and says why.
	var lv api.WorktreeCreated
	call("worktree.create", api.WorktreeCreateParams{WorktreeProject: p, Branch: "leaves"}, &lv)
	waitSetup(lv.Setup)
	commitConfig(lv.Worktree.Path, "echo x > left.log")
	var yes api.WorktreeSetupResult
	call("worktree.setup", api.WorktreeSetupParams{WorktreeProject: p, Path: lv.Worktree.Path}, &yes)
	if yes.Setup.State != "none" {
		t.Fatalf("setup with no setup script %+v", yes)
	}
	out, err := exec.Command(bin, "api", "call", "-socket", sock, "worktree.remove",
		`{"project_dir":"`+repo+`","path":"`+lv.Worktree.Path+`"}`).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "(conflict)") || !strings.Contains(string(out), "teardown") ||
		!strings.Contains(string(out), "left.log") {
		t.Fatalf("a teardown that leaves files: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(lv.Worktree.Path, "flightdeck.json")); err != nil {
		t.Fatal("the stopped removal left the worktree half gone")
	}
	var lr api.WorktreeRemoved
	call("worktree.remove", api.WorktreeRemoveParams{WorktreeProject: p, Path: lv.Worktree.Path, Force: true}, &lr)
	if lr.Teardown != "ran" || !lr.Managed {
		t.Fatalf("forced removal %+v", lr)
	}

	// Clean, it goes: teardown runs, the branch stays.
	os.Remove(filepath.Join(wantPath, "work.txt"))
	var r api.WorktreeRemoved
	call("worktree.remove", api.WorktreeRemoveParams{WorktreeProject: p, Path: wantPath}, &r)
	if r.Teardown != "ran" || r.TeardownExit == nil || *r.TeardownExit != 0 {
		t.Fatalf("removed %+v", r)
	}
	if b, err := os.ReadFile(filepath.Join(repo, "teardown.ran")); err != nil || strings.TrimSpace(string(b)) != wantPath {
		t.Fatalf("teardown.ran %q %v", b, err)
	}
	if _, err := os.Stat(wantPath); err == nil {
		t.Fatal("the worktree folder is still there")
	}
	if git(repo, "branch", "--list", "feat/x") == "" {
		t.Fatal("the branch went with the worktree")
	}

	// Force removes uncommitted work, through the CLI with a relative path.
	write(filepath.Join(f.Worktree.Path, "work.txt"), "unsaved\n")
	cmd := exec.Command(bin, "worktree", "remove", "-socket", sock, "-project", c.Project, "-force", filepath.Base(f.Worktree.Path))
	cmd.Dir = filepath.Dir(f.Worktree.Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree remove -force: %v %s", err, out)
	}
	if _, err := os.Stat(f.Worktree.Path); err == nil {
		t.Fatal("the forced worktree folder is still there")
	}
}
