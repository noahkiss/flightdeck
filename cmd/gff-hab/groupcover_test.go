package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/worktree"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// groupWorktree makes a repository with one commit and a linked worktree on branch, marked as
// made by gff-hab when marked; it returns the repository and the worktree.
func groupWorktree(t *testing.T, branch string, marked bool) (repo, wt string) {
	t.Helper()
	dir := shortDir(t)
	repo = gitRepo(t, dir, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	wt = filepath.Join(dir, "wt-"+strings.ReplaceAll(branch, "/", "-"))
	gitRun(t, repo, "worktree", "add", "-q", "-b", branch, wt)
	if marked {
		if err := (worktree.Git{}).WriteMark(context.Background(), wt, worktree.Mark{Source: repo, Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	return realPath(repo), realPath(wt)
}

// TestCoverCheck (groups.md 4.2): the cover answers only a worktree this hab made, on a branch
// under the group's prefix, for the cover's harness; a folder below the worktree's top counts.
// Every other folder gets a reason, so the card shows.
func TestCoverCheck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	check := coverCheck(worktree.Git{})
	ctx := context.Background()
	cov := api.TrustCover{Group: "g1", Confirmation: "none", Harness: "claude", BranchPrefix: "group/g1/"}
	repo, wt := groupWorktree(t, "group/g1/cache", true)
	if why := check(ctx, "claude", paneCover{cover: cov, folder: wt}); why != "" {
		t.Fatalf("a covered worktree: %s", why)
	}
	sub := filepath.Join(wt, "pkg")
	gitRun(t, wt, "status") // the worktree works
	if err := exec.Command("mkdir", "-p", sub).Run(); err != nil {
		t.Fatal(err)
	}
	if why := check(ctx, "claude", paneCover{cover: cov, folder: sub}); why != "" {
		t.Errorf("a folder inside the worktree: %s", why)
	}
	_, unmarked := groupWorktree(t, "group/g1/other", false)
	_, otherGroup := groupWorktree(t, "group/g2/cache", true)
	_, bare := groupWorktree(t, "group/g1x/cache", true)
	_, foreign := groupWorktree(t, "group/g1/pr", true)
	if err := (worktree.Git{}).WriteMark(ctx, foreign, worktree.Mark{Source: repo, Created: time.Now(), Foreign: "the start point is pull/7/head"}); err != nil {
		t.Fatal(err)
	}
	_, approved := groupWorktree(t, "group/g1/pr2", true)
	if err := (worktree.Git{}).WriteMark(ctx, approved, worktree.Mark{Source: repo, Created: time.Now(), Foreign: "the start point is pull/8/head", Approved: true}); err != nil {
		t.Fatal(err)
	}
	if why := check(ctx, "claude", paneCover{cover: cov, folder: approved}); why != "" {
		t.Errorf("a foreign worktree the owner approved: %s", why)
	}
	for _, c := range []struct {
		name, harness, folder, want string
	}{
		{"another harness", "codex", wt, "the cover is for claude"},
		{"the main checkout", "claude", repo, "main checkout"},
		{"a worktree gff-hab did not make", "claude", unmarked, "not a worktree this hab made"},
		{"another group's branch", "claude", otherGroup, "not under group/g1/"},
		{"a group id that starts the same", "claude", bare, "not under group/g1/"},
		{"someone else's code, not approved", "claude", foreign, "not yet approved"},
		{"no git", "claude", shortDir(t), "not in a git worktree"},
	} {
		if why := check(ctx, c.harness, paneCover{cover: cov, folder: c.folder}); !strings.Contains(why, c.want) {
			t.Errorf("%s: %q, want %q", c.name, why, c.want)
		}
	}
	// Codex records trust on the repository's main checkout, so its cover never answers.
	codex := cov
	codex.Harness = "codex"
	if why := check(ctx, "codex", paneCover{cover: codex, folder: wt}); !strings.Contains(why, "whole repository") {
		t.Errorf("a Codex cover: %q", why)
	}
}

// TestCoverParams: a cover comes from this hab's own confirmations or a listed peer's link,
// never from any other client, and its fields must be well formed and name the create's
// harness.
func TestCoverParams(t *testing.T) {
	good := api.SessionCreateParams{Harness: "codex", TrustCover: &api.TrustCover{Group: "g1", Confirmation: "none", Harness: "codex", BranchPrefix: "group/g1/"}}
	listed := func(dev string) bool { return dev == "dev-b" }
	peer := func(dev string) context.Context {
		return control.WithCallerPeer(context.Background(), api.PeerHello{DeviceID: dev, Host: "bee"})
	}
	if err := checkCoverParams(withOwnCover(context.Background()), good, nil); err != nil {
		t.Errorf("own: %v", err)
	}
	if err := checkCoverParams(peer("dev-b"), good, listed); err != nil {
		t.Errorf("a listed peer: %v", err)
	}
	if err := checkCoverParams(context.Background(), api.SessionCreateParams{Harness: "codex"}, nil); err != nil {
		t.Errorf("no cover: %v", err)
	}
	if err := checkCoverParams(context.Background(), good, listed); errKind(err) != api.ErrorKindInvalid || !strings.Contains(err.Error(), "only from a hab") {
		t.Errorf("a client: %v", err)
	}
	if err := checkCoverParams(peer("dev-x"), good, listed); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("an unlisted peer: %v", err)
	}
	bad := func(edit func(p *api.SessionCreateParams)) api.SessionCreateParams {
		p := good
		c := *good.TrustCover
		p.TrustCover = &c
		edit(&p)
		return p
	}
	for name, p := range map[string]api.SessionCreateParams{
		"no harness":       bad(func(p *api.SessionCreateParams) { p.Harness = "" }),
		"another harness":  bad(func(p *api.SessionCreateParams) { p.TrustCover.Harness = "claude" }),
		"a bad group":      bad(func(p *api.SessionCreateParams) { p.TrustCover.Group = "G/1" }),
		"a wide prefix":    bad(func(p *api.SessionCreateParams) { p.TrustCover.BranchPrefix = "group/" }),
		"a bad confirm id": bad(func(p *api.SessionCreateParams) { p.TrustCover.Confirmation = "yes" }),
	} {
		if err := checkCoverParams(withOwnCover(context.Background()), p, nil); errKind(err) != api.ErrorKindInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	if c := groupCover(api.ConfirmStart{Group: "g1", Harness: "codex"}, strings.Repeat("a", 32), true); c.Confirmation != strings.Repeat("a", 32) || c.BranchPrefix != "group/g1/" || c.Harness != "codex" {
		t.Errorf("confirmed cover %+v", c)
	}
	if c := groupCover(api.ConfirmStart{Group: "g1", Harness: "codex"}, strings.Repeat("a", 32), false); c.Confirmation != "none" {
		t.Errorf("record cover %+v", c)
	}
}

// TestTrustHoldConfig: [groups] trust_hold takes a duration from 1 s to 60 s; anything else is
// logged and 10 s runs.
func TestTrustHoldConfig(t *testing.T) {
	dir := shortDir(t)
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }
	if d := loadTrustHold(filepath.Join(dir, "none.toml"), logf); d != 10*time.Second || len(logs) != 0 {
		t.Errorf("no file: %s %q", d, logs)
	}
	for v, want := range map[string]time.Duration{`"2s"`: 2 * time.Second, `"60s"`: time.Minute, `"0s"`: 10 * time.Second, `"61s"`: 10 * time.Second, `5`: 10 * time.Second} {
		p := filepath.Join(dir, "hab.toml")
		writeFile(t, p, "[groups]\ntrust_hold = "+v+"\n")
		logs = nil
		if d := loadTrustHold(p, logf); d != want {
			t.Errorf("%s: %s, want %s", v, d, want)
		}
		if (want == 10*time.Second) != (len(logs) == 1) {
			t.Errorf("%s: logs %q", v, logs)
		}
	}
}
