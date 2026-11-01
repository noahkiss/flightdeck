package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	if ok, err := copyOne(from, to); ok || err != nil {
		t.Fatalf("copyOne %v %v", ok, err)
	}
	if b, _ := os.ReadFile(to); string(b) != "old" {
		t.Fatalf("contents %q", b)
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

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"feature/Auth_2": "feature-auth-2", "--": "worktree", "a..b": "a-b", "wt/x-y": "wt-x-y"} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
