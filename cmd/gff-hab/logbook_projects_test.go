package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// projectPane is a session's program: it writes its GFF_HAB to $ADDR and runs until $DIR/end.
const projectPane = `
printf '%s' "$GFF_HAB" > "$ADDR.tmp" && mv "$ADDR.tmp" "$ADDR"
while [ ! -e "$DIR/end" ]; do sleep 0.05; done
`

// gitRepos makes git repositories for a test, without the user's git config.
type gitRepos struct {
	t   *testing.T
	env []string
}

func newGitRepos(t *testing.T) *gitRepos {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	return &gitRepos{t: t, env: []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}}
}

func (g *gitRepos) run(dir string, args ...string) {
	g.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		g.t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// repo makes a repository at dir with remote as origin and one commit.
func (g *gitRepos) repo(dir, remote string) string {
	g.t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		g.t.Fatal(err)
	}
	g.run(dir, "init", "-q", "-b", "main")
	g.run(dir, "remote", "add", "origin", remote)
	g.run(dir, "commit", "-q", "--allow-empty", "-m", "start")
	return dir
}

// startIn starts a session named name in the project folder dir on e and returns the pane's
// GFF_HAB.
func startIn(t *testing.T, e *habEnv, name, dir string) string {
	t.Helper()
	addr := filepath.Join(e.work, name+".addr")
	createSession(t, e.sock, api.SessionCreateParams{Name: name, Argv: []string{"/bin/sh", "-c", projectPane}, ProjectDir: dir,
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work, "ADDR=" + addr}})
	waitUntil(t, name+"'s GFF_HAB", func() bool { b, err := os.ReadFile(addr); return err == nil && len(b) > 0 })
	b, _ := os.ReadFile(addr)
	return string(b)
}

func lbProjects(t *testing.T, c *control.Client) []api.LogbookProject {
	t.Helper()
	var r api.LogbookProjects
	call(t, c, "logbook.projects", api.LogbookProjectsParams{}, &r)
	return r.Projects
}

// waitProject waits until the hab lists a project that holds identity, and returns it.
func waitProject(t *testing.T, c *control.Client, identity string, ok func(api.LogbookProject) bool) api.LogbookProject {
	t.Helper()
	var got api.LogbookProject
	waitFor(t, "a project with "+identity, scaleTimeout(20*time.Second), func() bool {
		for _, p := range lbProjects(t, c) {
			if slices.Contains(p.Identities, identity) && (ok == nil || ok(p)) {
				got = p
				return true
			}
		}
		return false
	})
	return got
}

func lbTask(t *testing.T, c *control.Client, project, title string) string {
	t.Helper()
	var w api.LogbookWritten
	call(t, c, "logbook.write", api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: "create", Type: "task", Title: title, Project: project}}}, &w)
	return w.IDs[0]
}

func lbListIDs(t *testing.T, c *control.Client, project string) []string {
	t.Helper()
	var l api.LogbookRecords
	call(t, c, "logbook.list", api.LogbookListParams{LogbookFilter: api.LogbookFilter{Project: project}}, &l)
	var ids []string
	for _, r := range l.Records {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	return ids
}

// TestLogbookProjects: projects in a real gff-hab with its holder (logbook.md 3.3, 18 row 5).
// Sessions in a repository with an SSH remote, in a second worktree of it, and in a clone by
// HTTPS all land in one Logbook project, filed at first sight. A project the user marks private
// is left out of a pane's reads, and the user still sees it. The remote is renamed and gff-hab
// restarted: the folder's new identity joins the project, and its records stay.
func TestLogbookProjects(t *testing.T) {
	g := newGitRepos(t)
	e := newHolderEnv(t, 0)
	e.env = g.env
	e.serve()
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	app := g.repo(filepath.Join(e.work, "app"), "git@example.com:o/app.git")
	feature := filepath.Join(e.work, "app-feature")
	g.run(app, "worktree", "add", "-q", "-b", "feature", feature)
	clone := g.repo(filepath.Join(e.work, "clone"), "https://example.com/o/app")
	diary := g.repo(filepath.Join(e.work, "diary"), "git@example.com:me/diary.git")
	c := e.control()

	pane := startIn(t, e, "main", app)
	p := waitProject(t, c, "example.com/o/app", nil)
	startIn(t, e, "feature", feature)
	startIn(t, e, "clone", clone)
	startIn(t, e, "diary", diary)
	d := waitProject(t, c, "example.com/me/diary", nil)
	lb := openReplicaFile(t, e.state)
	waitUntil(t, "every folder filed", func() bool {
		for _, dir := range []string{app, feature, clone, diary} {
			real, _ := filepath.EvalSymlinks(dir)
			if _, ok, _ := lb.FolderProject(context.Background(), real); !ok {
				return false
			}
		}
		return true
	})
	ps := lbProjects(t, c)
	if len(ps) != 2 || p.Name != "app" || !slices.Equal(p.Identities, []string{"example.com/o/app"}) {
		t.Fatalf("projects %+v, want app once and diary", ps)
	}
	task, secret := lbTask(t, c, p.ID, "Fix the cart"), lbTask(t, c, d.ID, "Write the entry")

	// Private: the pane in app reads no diary record; the user does.
	var w api.LogbookWritten
	call(t, c, "logbook.write", api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: "set", ID: d.ID, Field: "private", Value: api.RawJSON("true")}}}, &w)
	inPane := []string{"GFF_HAB=" + pane, "GFF_HAB_SOCKET=" + filepath.Join(e.work, "none.sock")}
	out, _ := lbOK(t, inPane, "ls")
	if !strings.Contains(out, task) || strings.Contains(out, secret) {
		t.Fatalf("the pane lists:\n%s", out)
	}
	if _, errs, code := lbRun(t, inPane, "show", secret); code != 3 || !strings.Contains(errs, "no such record") {
		t.Fatalf("the pane shows the private task: %d %s", code, errs)
	}
	if _, errs, code := lbRun(t, inPane, "project", "private", p.ID); code != 3 || !strings.Contains(errs, "only the user") {
		t.Fatalf("a pane marks a project private: %d %s", code, errs)
	}
	if out, _ := lbOK(t, []string{"GFF_HAB_SOCKET=" + e.sock}, "ls"); !strings.Contains(out, secret) {
		t.Fatalf("the user lists:\n%s", out)
	}

	// A remote rename, seen by the catalog's scan at start: the identity joins app's set.
	g.run(app, "remote", "set-url", "origin", "git@example.com:o/shop.git")
	e.restart(false)
	c = e.control()
	got := waitProject(t, c, "example.com/o/shop", nil)
	if got.ID != p.ID || !slices.Contains(got.Identities, "example.com/o/app") {
		t.Fatalf("after the rename %+v, want %s with both identities", got, p.ID)
	}
	if ids := lbListIDs(t, c, p.ID); !slices.Equal(ids, []string{task}) {
		t.Fatalf("app's records after the rename: %v", ids)
	}
	if n := len(lbProjects(t, c)); n != 2 {
		t.Fatalf("%d projects after the rename", n)
	}
}

// TestLogbookProjectsTwoHabs: E9 (logbook.md 6) across two real gff-hab processes over the ssh
// stand-in. Apart, a session on A in a clone of R by SSH files P1, and one on B in a clone by
// HTTPS files P2, each with a task. Once both run, both habs show one project: P1, the older,
// keeps its id, P2 is joined into it, and both tasks list under either id.
func TestLogbookProjectsTwoHabs(t *testing.T) {
	g := newGitRepos(t)
	m := newMeshPair(t)
	m.b.stopHab(false)
	ra := g.repo(filepath.Join(m.a.work, "r"), "git@example.com:o/r.git")
	rb := g.repo(filepath.Join(m.b.work, "r"), "https://example.com/o/r.git")

	startIn(t, m.a, "on-a", ra)
	ca := m.a.control()
	p1 := waitProject(t, ca, "example.com/o/r", nil)
	t1 := lbTask(t, ca, p1.ID, "on A")
	ca.Close()
	m.a.stopHab(false)

	m.b.serve()
	startIn(t, m.b, "on-b", rb)
	cb := m.b.control()
	p2 := waitProject(t, cb, "example.com/o/r", nil)
	if p2.ID == p1.ID {
		t.Fatal("b filed a's project while apart")
	}
	t2 := lbTask(t, cb, p2.ID, "on B")

	m.a.serve()
	ca = m.a.control()
	want := []string{t1, t2}
	slices.Sort(want)
	for name, c := range map[string]*control.Client{"a": ca, "b": cb} {
		waitProject(t, c, "example.com/o/r", func(p api.LogbookProject) bool { return p.ID == p2.ID && p.JoinedInto == p1.ID })
		for _, id := range []string{p1.ID, p2.ID} {
			if got := lbListIDs(t, c, id); !slices.Equal(got, want) {
				t.Fatalf("%s: list by %s %v, want %v", name, id, got, want)
			}
		}
		r, _ := lbGet(t, c, t2)
		if r.Project != p1.ID {
			t.Fatalf("%s: B's task shows under %s, want %s", name, r.Project, p1.ID)
		}
	}
}
