package lane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/internal/worktree"
)

// The lane over throwaway repositories: a bare remote in a temporary folder, the hab's
// checkout, and a second clone that plays another member or another person.

type repo struct {
	t      *testing.T
	bare   string // the remote
	src    string // the hab's checkout
	other  string // another clone
	id     string // the project's identity: the remote's URL
	laneWT string
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T, lane string) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	d := t.TempDir()
	r := &repo{t: t, bare: filepath.Join(d, "remote.git"), src: filepath.Join(d, "src"), other: filepath.Join(d, "other"), laneWT: filepath.Join(d, "lane", "wt")}
	git(t, d, "init", "-q", "--bare", "-b", "main", r.bare)
	git(t, d, "init", "-q", "-b", "main", r.src)
	r.id = r.bare
	git(t, r.src, "remote", "add", "origin", r.bare)
	if lane != "" {
		r.write(r.src, "flightdeck.json", lane)
	}
	r.write(r.src, "README", "hello\n")
	r.commit(r.src, "first")
	git(t, r.src, "push", "-q", "origin", "main")
	git(t, d, "clone", "-q", r.bare, r.other)
	return r
}

func (r *repo) write(dir, name, body string) {
	r.t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(dir, msg string) string {
	git(r.t, dir, "add", "-A")
	git(r.t, dir, "commit", "-q", "-m", msg)
	return git(r.t, dir, "rev-parse", "HEAD")
}

// item makes branch from base in src with one commit per file, and pushes it.
func (r *repo) item(branch, base string, files ...string) string {
	r.t.Helper()
	git(r.t, r.src, "fetch", "-q", "origin")
	git(r.t, r.src, "checkout", "-q", "-B", branch, base)
	var head string
	for _, f := range files {
		r.write(r.src, f, f+"\n")
		head = r.commit(r.src, "add "+f)
	}
	git(r.t, r.src, "push", "-q", "--force", "origin", branch)
	git(r.t, r.src, "checkout", "-q", "--detach", "origin/main")
	return head
}

// move pushes a commit to main from the other clone.
func (r *repo) move(file string) {
	git(r.t, r.other, "pull", "-q", "--rebase", "origin", "main")
	r.write(r.other, file, "moved "+file+"\n")
	r.commit(r.other, "move "+file)
	git(r.t, r.other, "push", "-q", "origin", "HEAD:main")
}

func (r *repo) mainHead() string { return git(r.t, r.bare, "rev-parse", "refs/heads/main") }

func (r *repo) job(branch, land string) Job {
	return Job{Source: r.src, Dir: r.laneWT, Identity: r.id, Target: "main", Branch: branch, Land: land, Tries: 3, OutputLines: 40}
}

func (r *repo) subjects() []string {
	return strings.Split(git(r.t, r.bare, "log", "--format=%s", "main"), "\n")
}

func TestLaneTwoInOrderAndConflict(t *testing.T) {
	r := newRepo(t, `{"lane":{"checks":"test -f README"}}`)
	a := r.item("group/g/a", "origin/main", "a.txt")
	b := r.item("group/g/b", "origin/main", "b.txt")
	ctx := context.Background()
	var saved []Attempt
	ja := r.job("group/g/a", a)
	ja.Saved = func(at Attempt) error { saved = append(saved, at); return nil }
	res := Run(ctx, ja)
	if res.Outcome != Landed || !res.NoCI || res.Commit != r.mainHead() {
		t.Fatalf("a: %+v, main %s", res, r.mainHead())
	}
	if len(saved) != 1 || saved[0].Rebased != res.Commit {
		t.Fatalf("saved %+v", saved)
	}
	res = Run(ctx, r.job("group/g/b", b))
	if res.Outcome != Landed {
		t.Fatalf("b: %+v", res)
	}
	if got := strings.Join(r.subjects(), ","); got != "add b.txt,add a.txt,first" {
		t.Fatalf("main log %s", got)
	}
	// The rebased item branch is on the remote too.
	if got := git(t, r.bare, "rev-parse", "refs/heads/group/g/b"); got != res.Commit {
		t.Fatalf("item branch %s, landed %s", got, res.Commit)
	}

	// A third item that conflicts goes back with the rebase's output, and main stays.
	r.move("c.txt")
	c := r.item("group/g/c", "origin/main~1", "c.txt")
	before := r.mainHead()
	res = Run(ctx, r.job("group/g/c", c))
	if res.Outcome != Back || res.Step != StepRebase || !strings.Contains(res.Output, "c.txt") {
		t.Fatalf("c: %+v", res)
	}
	if r.mainHead() != before {
		t.Fatal("main moved")
	}
	// The lane's worktree is clean for the next run.
	if st := git(t, r.laneWT, "status", "--porcelain"); st != "" {
		t.Fatalf("lane worktree: %s", st)
	}
}

func TestLaneCI(t *testing.T) {
	ci := filepath.Join(t.TempDir(), "ci.sh")
	os.WriteFile(ci, []byte("#!/bin/sh\necho \"ci $1 $2\" >> \"$CI_LOG\"\nexit \"$CI_EXIT\"\n"), 0o755)
	log := filepath.Join(t.TempDir(), "ci.log")
	t.Setenv("CI_LOG", log)
	r := newRepo(t, fmt.Sprintf(`{"lane":{"ci":%q}}`, ci+` "$1" "$2"`))
	a := r.item("group/g/a", "origin/main", "a.txt")
	ctx := context.Background()

	t.Setenv("CI_EXIT", "3")
	before := r.mainHead()
	res := Run(ctx, r.job("group/g/a", a))
	if res.Outcome != Back || res.Step != StepCI || !strings.Contains(res.Output, "exit status 3") {
		t.Fatalf("failed CI: %+v", res)
	}
	if r.mainHead() != before {
		t.Fatal("a failed CI landed")
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "ci group/g/a ") {
		t.Fatalf("CI args: %s", b)
	}

	t.Setenv("CI_EXIT", "0")
	res = Run(ctx, r.job("group/g/a", a))
	if res.Outcome != Landed || res.NoCI {
		t.Fatalf("passing CI: %+v", res)
	}
	b, _ = os.ReadFile(log)
	if !strings.Contains(string(b), "ci group/g/a "+res.Commit) {
		t.Fatalf("CI was not given the landed commit: %s", b)
	}
}

func TestLaneTargetMovesDuringCI(t *testing.T) {
	d := t.TempDir()
	count := filepath.Join(d, "n")
	// The CI moves main while it "waits" the first $MOVES times.
	ci := filepath.Join(d, "ci.sh")
	os.WriteFile(ci, []byte(`#!/bin/sh
n=$(cat "$COUNT" 2>/dev/null || echo 0)
n=$((n+1)); echo $n > "$COUNT"
if [ "$n" -le "$MOVES" ]; then
  cd "$OTHER" && git pull -q --rebase origin main && echo $n > "moved$n-$$" && git add -A && git commit -q -m "moved $n" && git push -q origin HEAD:main
fi
`), 0o755)
	r := newRepo(t, fmt.Sprintf(`{"lane":{"ci":%q}}`, ci))
	t.Setenv("COUNT", count)
	t.Setenv("OTHER", r.other)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.invalid")
	ctx := context.Background()

	// One move: the lane starts again and lands on the second try.
	t.Setenv("MOVES", "1")
	a := r.item("group/g/a", "origin/main", "a.txt")
	res := Run(ctx, r.job("group/g/a", a))
	if res.Outcome != Landed || res.Tries != 2 {
		t.Fatalf("one move: %+v", res)
	}

	// Three moves send it back after three tries.
	os.Remove(count)
	t.Setenv("MOVES", "100")
	b := r.item("group/g/b", "origin/main", "b.txt")
	res = Run(ctx, r.job("group/g/b", b))
	if res.Outcome != Back || res.Step != StepMoving || res.Tries != 3 || !strings.Contains(res.Output, "keeps moving") {
		t.Fatalf("three moves: %+v", res)
	}
	// With lane_tries 1 one move does.
	os.Remove(count)
	j := r.job("group/g/b", b)
	j.Tries = 1
	res = Run(ctx, j)
	if res.Outcome != Back || res.Step != StepMoving || res.Tries != 1 {
		t.Fatalf("lane_tries 1: %+v", res)
	}
}

func TestLaneOutputLines(t *testing.T) {
	r := newRepo(t, `{"lane":{"checks":["i=0; while [ $i -lt 100 ]; do echo line$i; i=$((i+1)); done", "exit 1"]}}`)
	a := r.item("group/g/a", "origin/main", "a.txt")
	j := r.job("group/g/a", a)
	j.OutputLines = 7
	held := 0
	j.Heavy = func(context.Context) (func(), error) { held++; return func() { held-- }, nil }
	res := Run(context.Background(), j)
	if res.Outcome != Back || res.Step != StepChecks {
		t.Fatalf("%+v", res)
	}
	lines := strings.Split(res.Output, "\n")
	if len(lines) != 7 || lines[0] != "line0" || lines[6] != "line6" {
		t.Fatalf("output %q", res.Output)
	}
	if held != 0 {
		t.Fatal("the heavy-run lock was not released")
	}
	j.OutputLines = 0
	if res := Run(context.Background(), j); res.Output != "" {
		t.Fatalf("0 lines: %q", res.Output)
	}
}

func TestLaneProtectedBranch(t *testing.T) {
	r := newRepo(t, "")
	hook := filepath.Join(r.bare, "hooks", "pre-receive")
	os.WriteFile(hook, []byte("#!/bin/sh\nwhile read old new ref; do [ \"$ref\" = refs/heads/main ] && { echo protected; exit 1; }; done\nexit 0\n"), 0o755)
	a := r.item("group/g/a", "origin/main", "a.txt")
	res := Run(context.Background(), r.job("group/g/a", a))
	if res.Outcome != Stopped || res.Step != StepLand {
		t.Fatalf("%+v", res)
	}
}

func TestLaneChecksFromTarget(t *testing.T) {
	// The target's checks fail; the item empties lane.checks on its branch, and the target's
	// checks still run.
	r := newRepo(t, `{"lane":{"checks":"echo target-checks; exit 1"}}`)
	git(t, r.src, "checkout", "-q", "-B", "group/g/a", "origin/main")
	r.write(r.src, "flightdeck.json", `{"lane":{}}`)
	a := r.commit(r.src, "empty the checks")
	git(t, r.src, "push", "-q", "origin", "group/g/a")
	git(t, r.src, "checkout", "-q", "--detach", "origin/main")
	res := Run(context.Background(), r.job("group/g/a", a))
	if res.Outcome != Back || res.Step != StepChecks || !strings.Contains(res.Output, "target-checks") {
		t.Fatalf("%+v", res)
	}
}

func TestLaneStacked(t *testing.T) {
	r := newRepo(t, "")
	a := r.item("group/g/a", "origin/main", "a.txt")
	b := r.item("group/g/b", a, "b.txt") // b stacks on a's submitted head
	r.move("m.txt")                      // main moves, so a's rebase makes new commits
	ctx := context.Background()
	if res := Run(ctx, r.job("group/g/a", a)); res.Outcome != Landed {
		t.Fatalf("a: %+v", res)
	}
	jb := r.job("group/g/b", b)
	a2 := r.item("group/g/a2", a, "a2.txt") // a later head of a that b does not hold
	jb.Upstreams = []string{a2, a}
	res := Run(ctx, jb)
	if res.Outcome != Landed {
		t.Fatalf("b: %+v", res)
	}
	if got := strings.Join(r.subjects(), ","); got != "add b.txt,add a.txt,move m.txt,first" {
		t.Fatalf("main log %s", got)
	}
}

// A stacked item that holds none of its base's heads goes back: a plain rebase would replay the
// base's old commits onto the target.
func TestLaneStackedOnAnOldHead(t *testing.T) {
	r := newRepo(t, "")
	a := r.item("group/g/a", "origin/main", "a.txt")
	b := r.item("group/g/b", a, "b.txt")
	a2 := r.item("group/g/a2", "origin/main", "a2.txt") // a submitted again from scratch
	jb := r.job("group/g/b", b)
	jb.Upstreams = []string{a2}
	if res := Run(context.Background(), jb); res.Outcome != Back || res.Step != StepRebase || !strings.Contains(res.Output, "no longer has") {
		t.Fatalf("b: %+v", res)
	}
}

func TestLaneLandedBeforeRestart(t *testing.T) {
	r := newRepo(t, "")
	a := r.item("group/g/a", "origin/main", "a.txt")
	var saved Attempt
	j := r.job("group/g/a", a)
	j.Saved = func(at Attempt) error { saved = at; return nil }
	res := Run(context.Background(), j)
	if res.Outcome != Landed {
		t.Fatal(res)
	}
	// The hab restarts after the land push, before it marked the item: the next run finds the
	// saved head on the target and pushes nothing.
	r.move("later.txt")
	before := r.mainHead()
	j.Prior, j.Saved = []string{saved.Rebased}, nil
	res = Run(context.Background(), j)
	if res.Outcome != Landed || res.Commit != saved.Rebased || r.mainHead() != before {
		t.Fatalf("%+v", res)
	}
}

func TestLaneUnavailable(t *testing.T) {
	r := newRepo(t, "")
	j := r.job("group/g/a", "0000000000000000000000000000000000000000")
	j.Identity = ""
	if res := Run(context.Background(), j); res.Outcome != Unavailable {
		t.Fatalf("no identity: %+v", res)
	}
	j.Identity = "https://example.invalid/other.git"
	if res := Run(context.Background(), j); res.Outcome != Unavailable {
		t.Fatalf("no such remote: %+v", res)
	}
	j = r.job("group/g/a", "1111111111111111111111111111111111111111")
	if res := Run(context.Background(), j); res.Outcome != Back || res.Step != StepFetch {
		t.Fatalf("missing commit: %+v", res)
	}
}

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig([]byte(`{"worktree":{"setup":"x"},"lane":{"checks":["a","b"],"ci":"c","build_folders":["out"]}}`))
	if err != nil || c.Checks != "a\nb" || c.CI != "c" || len(c.BuildFolders) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := ParseConfig([]byte(`{"lane":{"checks":1}}`)); err == nil {
		t.Fatal("a number taken as checks")
	}
	_ = worktree.ConfigFile
}

func TestPushRefusal(t *testing.T) {
	cases := map[string]refusal{
		" ! [rejected]        x -> main (non-fast-forward)":                     refusedRace,
		" ! [rejected]        x -> main (fetch first)":                          refusedRace,
		" ! [rejected]        x -> b (stale info)":                              refusedRace,
		" ! [remote rejected] x -> main (cannot lock ref 'refs/heads/main')":    refusedRace,
		" ! [remote rejected] x -> main (incorrect old value provided)":         refusedRace,
		" ! [remote rejected] x -> main (failed to update ref)":                 refusedRace,
		" ! [remote rejected] x -> main (pre-receive hook declined)":            refusedRule,
		" ! [remote rejected] x -> main (protected branch hook declined)":       refusedRule,
		"remote: Permission to o/r.git denied to u.":                            refusedRule,
		"fatal: unable to access 'https://h/r.git/': Could not resolve host: h": refusedOther,
	}
	for out, want := range cases {
		if got := pushRefusal(out); got != want {
			t.Errorf("%q: %d, want %d", out, got, want)
		}
	}
}
