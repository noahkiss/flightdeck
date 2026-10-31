package gitstatus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// waitLong bounds every wait; GFF_TEST_TIMEOUT_SCALE raises it on a slow machine.
func waitLong() time.Duration {
	d := 10 * time.Second
	if s := os.Getenv("GFF_TEST_TIMEOUT_SCALE"); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			d = time.Duration(float64(d) * f)
		}
	}
	return d
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
}

// gitEnv isolates the test's git runs from the user's config.
func gitEnv(home string) []string {
	return []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}
}

type repoT struct {
	t    *testing.T
	home string
}

func newRepoT(t *testing.T) *repoT {
	needGit(t)
	return &repoT{t: t, home: t.TempDir()}
}

func (r *repoT) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv(r.home)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repoT) commit(dir, msg string) {
	r.t.Helper()
	r.git(dir, "commit", "-q", "--allow-empty", "-m", msg)
}

func (r *repoT) reader() Reader { return Reader{Env: gitEnv(r.home)} }

// upstreamPair makes a bare upstream with one commit, and a clone of it.
func (r *repoT) upstreamPair() (bare, clone string) {
	base := r.t.TempDir()
	bare = filepath.Join(base, "up.git")
	seed := filepath.Join(base, "seed")
	clone = filepath.Join(base, "work")
	r.git(base, "init", "-q", "--bare", bare)
	r.git(base, "clone", "-q", bare, seed)
	r.commit(seed, "first")
	r.git(seed, "push", "-q", "origin", "main")
	r.git(base, "clone", "-q", bare, clone)
	return bare, seed
}

// fixture is a structure store with one project, a Watcher on it, and a record of every
// project event and every store write.
type fixture struct {
	store  *structure.Store
	w      *Watcher
	id     string
	mu     sync.Mutex
	events []structure.Event
	sets   int
	logs   []string
}

type countingStore struct {
	*structure.Store
	f *fixture
}

func (c countingStore) SetProjectRepo(id string, r structure.RepoState) (bool, error) {
	ch, err := c.Store.SetProjectRepo(id, r)
	c.f.mu.Lock()
	c.f.sets++
	c.f.mu.Unlock()
	return ch, err
}

func newFixture(t *testing.T, dir string, cfg Config) *fixture {
	t.Helper()
	f := &fixture{}
	st, err := structure.Open(structure.Config{
		Dir:     t.TempDir(),
		Holders: structure.NewFakeHolder(),
		OnEvent: func(e structure.Event) {
			if e.Type == structure.ProjectUpdated {
				f.mu.Lock()
				f.events = append(f.events, e)
				f.mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	p, err := st.CreateProject("app", "example.com/o/app", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectDir(p.ID, dir); err != nil {
		t.Fatal(err)
	}
	f.store, f.id = st, p.ID
	f.mu.Lock()
	f.events = nil // the create event is not the watcher's
	f.mu.Unlock()
	cfg.Store = countingStore{st, f}
	if cfg.Poll == 0 {
		cfg.Poll = -1
	}
	if cfg.Debounce == 0 {
		cfg.Debounce = 10 * time.Millisecond
	}
	cfg.Logf = func(format string, args ...any) {
		f.mu.Lock()
		f.logs = append(f.logs, format)
		f.mu.Unlock()
	}
	f.w = New(cfg)
	t.Cleanup(f.w.Close)
	return f
}

// waitFor polls cond until it holds or waitLong passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLong())
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fixture) project() structureProject {
	p, _ := f.store.Project(f.id)
	return structureProject{p.Branch, p.Head, p.Ahead, p.Behind, p.Revision}
}

func (f *fixture) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sets
}

func (f *fixture) eventCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

type structureProject struct {
	Branch, Head  string
	Ahead, Behind *int
	Revision      uint64
}

func is(p *int, v int) bool { return p != nil && *p == v }

// A clone one commit ahead shows ahead=1, behind=0. After a commit on the upstream and a
// fetch by the test, it shows behind=1.
func TestAheadBehind(t *testing.T) {
	r := newRepoT(t)
	_, seed := r.upstreamPair()
	work := filepath.Join(filepath.Dir(seed), "work")
	r.commit(work, "local")
	f := newFixture(t, work, Config{Reader: r.reader()})
	waitFor(t, "ahead=1 behind=0", func() bool {
		p := f.project()
		return p.Branch == "main" && is(p.Ahead, 1) && is(p.Behind, 0) && p.Head == r.git(work, "rev-parse", "HEAD")
	})

	r.commit(seed, "upstream")
	r.git(seed, "push", "-q", "origin", "main")
	r.git(work, "fetch", "-q")
	f.w.Refresh(f.id)
	waitFor(t, "behind=1", func() bool {
		p := f.project()
		return is(p.Ahead, 1) && is(p.Behind, 1)
	})
}

// Switching branch shows up through the poll of .git/HEAD, with no explicit refresh.
func TestBranchSwitch(t *testing.T) {
	r := newRepoT(t)
	_, seed := r.upstreamPair()
	work := filepath.Join(filepath.Dir(seed), "work")
	f := newFixture(t, work, Config{Reader: r.reader(), Poll: 50 * time.Millisecond})
	waitFor(t, "branch main", func() bool { return f.project().Branch == "main" })
	if p := f.project(); !is(p.Ahead, 0) || !is(p.Behind, 0) {
		t.Fatalf("ahead/behind = %v/%v, want 0/0", p.Ahead, p.Behind)
	}

	r.git(work, "switch", "-q", "-c", "feature")
	waitFor(t, "branch feature", func() bool {
		p := f.project()
		return p.Branch == "feature" && p.Ahead == nil && p.Behind == nil
	})

	r.git(work, "switch", "-q", "--detach")
	waitFor(t, "detached HEAD", func() bool {
		p := f.project()
		return p.Branch == "" && p.Head != ""
	})
}

// A folder that is not a repository leaves the fields empty.
func TestNotARepo(t *testing.T) {
	r := newRepoT(t)
	dir := t.TempDir()
	res, err := r.reader().Read(context.Background(), dir)
	if err != nil || !res.State.Equal(structure.RepoState{}) || len(res.Watch) != 0 {
		t.Fatalf("Read = %+v, %v; want the zero state", res, err)
	}
	f := newFixture(t, dir, Config{Reader: r.reader()})
	waitFor(t, "a read", func() bool { return f.setCount() >= 1 })
	if p := f.project(); p.Branch != "" || p.Head != "" || p.Ahead != nil || p.Behind != nil {
		t.Fatalf("project = %+v, want empty fields", p)
	}
	if n := f.eventCount(); n != 0 {
		t.Fatalf("%d project events, want 0", n)
	}
}

// An empty repository has a branch and no head; a repository with no upstream has no counts.
func TestReadEdges(t *testing.T) {
	r := newRepoT(t)
	dir := t.TempDir()
	r.git(dir, "init", "-q")
	res, err := r.reader().Read(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := res.State; s.Branch != "main" || s.Head != "" || s.Ahead != nil || s.Behind != nil {
		t.Fatalf("empty repo: %+v", s)
	}
	r.commit(dir, "one")
	res, err = r.reader().Read(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := res.State; s.Branch != "main" || len(s.Head) < 40 || s.Ahead != nil || s.Behind != nil {
		t.Fatalf("no upstream: %+v", s)
	}
	// A subfolder reads the same repository.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	res2, err := r.reader().Read(context.Background(), sub)
	if err != nil || !res2.State.Equal(res.State) {
		t.Fatalf("subfolder: %+v, %v", res2.State, err)
	}
	// An upstream whose ref was never fetched gives no counts and no error.
	r.git(dir, "config", "branch.main.remote", "origin")
	r.git(dir, "config", "branch.main.merge", "refs/heads/main")
	r.git(dir, "remote", "add", "origin", filepath.Join(dir, "missing.git"))
	res, err = r.reader().Read(context.Background(), dir)
	if err != nil || res.State.Ahead != nil || res.State.Branch != "main" {
		t.Fatalf("gone upstream: %+v, %v", res.State, err)
	}
}

// A hung git times out, keeps the last values, logs once, and never blocks the store.
func TestHungGit(t *testing.T) {
	needGit(t)
	bin := t.TempDir()
	script := "#!/bin/sh\necho x >> \"$0.calls\"\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	f := newFixture(t, dir, Config{Timeout: 200 * time.Millisecond, Interval: time.Hour})
	ahead := 3
	if _, err := f.store.SetProjectRepo(f.id, structure.RepoState{Branch: "main", Head: "abc", Ahead: &ahead, Behind: &ahead}); err != nil {
		t.Fatal(err)
	}
	rev := f.project().Revision

	// While git hangs, the store answers at once.
	start := time.Now()
	f.w.RefreshNow(f.id)
	for i := 0; i < 20; i++ {
		if _, err := f.store.RenameProject(f.id, "app"); err != nil {
			t.Fatal(err)
		}
		f.store.Snapshot(api.HostInfo{})
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("store calls took %v while git hung", d)
	}

	waitFor(t, "the timeout log", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.logs) == 1
	})
	f.w.RefreshNow(f.id)
	calls := func() int {
		b, _ := os.ReadFile(filepath.Join(bin, "git.calls"))
		return strings.Count(string(b), "x")
	}
	// The first read, the one RefreshNow queued behind it, and this one: all time out.
	waitFor(t, "three timed-out reads", func() bool { return calls() >= 3 && f.w.idle() })
	f.mu.Lock()
	logs := len(f.logs)
	f.mu.Unlock()
	if logs != 1 {
		t.Fatalf("%d log lines, want 1", logs)
	}
	p := f.project()
	if p.Branch != "main" || p.Head != "abc" || !is(p.Ahead, 3) || p.Revision != rev {
		t.Fatalf("project = %+v, want the last values at revision %d", p, rev)
	}
	if f.setCount() != 0 {
		t.Fatalf("the watcher wrote %d times after a timeout", f.setCount())
	}
}

// No change, no event: repeated reads of an unchanged repository emit nothing new.
func TestNoChangeNoEvent(t *testing.T) {
	r := newRepoT(t)
	_, seed := r.upstreamPair()
	work := filepath.Join(filepath.Dir(seed), "work")
	f := newFixture(t, work, Config{Reader: r.reader()})
	waitFor(t, "first read", func() bool { return f.project().Branch == "main" })
	events, rev := f.eventCount(), f.project().Revision
	if events != 1 {
		t.Fatalf("%d events after the first read, want 1", events)
	}
	for i := 0; i < 3; i++ {
		n := f.setCount()
		f.w.RefreshNow(f.id)
		waitFor(t, "another read", func() bool { return f.setCount() > n })
	}
	if got := f.eventCount(); got != events {
		t.Fatalf("%d events after unchanged reads, want %d", got, events)
	}
	if got := f.project().Revision; got != rev {
		t.Fatalf("revision %d, want %d", got, rev)
	}
}
