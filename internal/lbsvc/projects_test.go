package lbsvc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

func sight(t *testing.T, r *rig, dir, remote string) string {
	t.Helper()
	id, err := r.svc.Sight(context.Background(), Sighting{Dir: dir, Identity: Identity(remote, ""), Host: "host-a"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func projectsOf(t *testing.T, r *rig) []api.LogbookProject {
	t.Helper()
	ps, err := r.svc.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ps.Projects
}

func listIn(t *testing.T, r *rig, project string) []string {
	t.Helper()
	l, err := r.svc.List(context.Background(), api.LogbookListParams{LogbookFilter: api.LogbookFilter{Project: project}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range l.Records {
		ids = append(ids, x.ID)
	}
	slices.Sort(ids)
	return ids
}

func fileTask(t *testing.T, r *rig, project, title string) string {
	t.Helper()
	c := create("task", title)
	c.Project = project
	w, err := r.svc.Write(context.Background(), person, api.LogbookWriteParams{Changes: []api.LogbookChange{c}})
	if err != nil {
		t.Fatal(err)
	}
	return w.IDs[0]
}

// TestIdentityForms: the SSH and HTTPS remotes of one repository are one identity, the catalog's
// form (mesh.md 5.1); a repository with no remote is its root commit; a fork is another.
func TestIdentityForms(t *testing.T) {
	want := "example.com/o/app"
	for _, remote := range []string{"git@example.com:o/app.git", "https://example.com/o/app.git", "ssh://git@Example.com:22/o/app",
		"https://user@example.com/o/app/", "https://EXAMPLE.com/o/app"} {
		if got := Identity(remote, ""); got != want {
			t.Errorf("Identity(%q) = %q, want %q", remote, got, want)
		}
	}
	if got := Identity("", "abc123"); got != "root:abc123" {
		t.Errorf("root commit: %q", got)
	}
	if got := Identity("git@example.com:fork/app.git", ""); got == want {
		t.Error("a fork has the same identity")
	}
	if Identity("", "") != "" {
		t.Error("no identity from nothing")
	}
	for identity, want := range map[string]string{"example.com/o/app": "app", "root:abc": "dir", "": "dir"} {
		if got := ProjectName(identity, "/src/dir"); got != want {
			t.Errorf("ProjectName(%q) = %q, want %q", identity, got, want)
		}
	}
}

// TestFirstSight: the first session in a folder files a project with the identity and the
// repository's name; a second sight of it, an HTTPS clone of the SSH one, and a second worktree
// all land in that one project.
func TestFirstSight(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	p := sight(t, r, "/src/app", "git@example.com:o/app.git")
	if again := sight(t, r, "/src/app", "git@example.com:o/app.git"); again != p {
		t.Fatalf("a second sight filed %s, want %s", again, p)
	}
	if clone := sight(t, r, "/other/app-https", "https://example.com/o/app"); clone != p {
		t.Fatalf("the HTTPS clone filed %s, want %s", clone, p)
	}
	if wt := sight(t, r, "/src/app-feature", "git@example.com:o/app.git"); wt != p {
		t.Fatalf("a second worktree filed %s, want %s", wt, p)
	}
	ps := projectsOf(t, r)
	if len(ps) != 1 || ps[0].ID != p || ps[0].Name != "app" || !slices.Equal(ps[0].Identities, []string{"example.com/o/app"}) {
		t.Fatalf("projects %+v", ps)
	}
	g, err := r.svc.Get(ctx, api.LogbookGetParams{ID: p})
	if err != nil {
		t.Fatal(err)
	}
	if g.Record.CreatedBy == nil || g.Record.CreatedBy.Kind != "hab" || g.Record.CreatedBy.Host != "host-a" {
		t.Fatalf("created by %+v, want the hab", g.Record.CreatedBy)
	}
	if other := sight(t, r, "/src/fork", "git@example.com:fork/app.git"); other == p {
		t.Fatal("a fork filed under the same project")
	}
	if _, err := r.svc.Sight(ctx, Sighting{Dir: "/x"}); err == nil {
		t.Fatal("a sighting with no identity filed something")
	}
}

// TestRemoteChange: a folder filed under P whose remote is renamed keeps P: the new identity
// joins P's set, P's records stay, and a new clone with the new remote lands in P. A folder that
// now names a project that already exists is filed there, and nothing is joined.
func TestRemoteChange(t *testing.T) {
	r := newRig(t)
	p := sight(t, r, "/src/app", "git@example.com:o/app.git")
	task := fileTask(t, r, p, "keep me")
	if got := sight(t, r, "/src/app", "git@example.com:o/renamed.git"); got != p {
		t.Fatalf("after the rename %s, want %s", got, p)
	}
	ps := projectsOf(t, r)
	if len(ps) != 1 || !slices.Equal(ps[0].Identities, []string{"example.com/o/app", "example.com/o/renamed"}) {
		t.Fatalf("projects %+v", ps)
	}
	if got := sight(t, r, "/new/clone", "https://example.com/o/renamed"); got != p {
		t.Fatalf("a clone of the new remote filed %s", got)
	}
	if got := listIn(t, r, p); !slices.Equal(got, []string{task}) {
		t.Fatalf("P lists %v", got)
	}
	q := sight(t, r, "/src/lib", "git@example.com:o/lib.git")
	if got := sight(t, r, "/src/app", "git@example.com:o/lib.git"); got != q {
		t.Fatalf("a folder that now names lib filed %s, want %s", got, q)
	}
	if ps := projectsOf(t, r); len(ps) != 2 || ps[0].JoinedInto != "" || ps[1].JoinedInto != "" {
		t.Fatalf("app and lib joined: %+v", ps)
	}
}

// joinChanges are what `gff lb project join a b` writes: b's identities on a, and one shared
// identity, so a project with none joins too.
func joinChanges(t *testing.T, r *rig, a, b string) []api.LogbookChange {
	t.Helper()
	g, err := r.svc.Get(context.Background(), api.LogbookGetParams{ID: b})
	if err != nil {
		t.Fatal(err)
	}
	var out []api.LogbookChange
	for _, id := range append(g.Record.Identities, "project:"+b) {
		out = append(out, api.LogbookChange{Op: api.LogbookChangeOpAdd, ID: a, Identity: id})
	}
	return append(out, api.LogbookChange{Op: api.LogbookChangeOpAdd, ID: b, Identity: "project:" + b})
}

// TestJoin: a person joins two projects by identity; the older keeps its id, the other shows
// joined into it, and records of both list under either id.
func TestJoin(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := sight(t, r, "/src/app", "git@example.com:o/app.git")
	r.advance(time.Second)
	b := sight(t, r, "/src/fork", "git@example.com:fork/app.git")
	ta, tb := fileTask(t, r, a, "on a"), fileTask(t, r, b, "on b")
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: joinChanges(t, r, b, a)}); err != nil {
		t.Fatal(err)
	}
	want := []string{ta, tb}
	slices.Sort(want)
	for _, id := range []string{a, b} {
		if got := listIn(t, r, id); !slices.Equal(got, want) {
			t.Fatalf("list by %s: %v, want %v", id, got, want)
		}
	}
	gb, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: b})
	if gb.Record.JoinedInto != a {
		t.Fatalf("b joined into %q, want the older %s", gb.Record.JoinedInto, a)
	}
	if got := sight(t, r, "/x/fork", "https://example.com/fork/app"); got != a {
		t.Fatalf("the fork's identity now files under %s, want %s", got, a)
	}
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: tb})
	if g.Record.Project != a {
		t.Fatalf("b's task shows under %q", g.Record.Project)
	}
}

// TestPrivateHidden: a project marked private is hidden from agents' reads outside it, in every
// read, while a person and an agent inside it see it; a project joined with a private one is
// private too.
func TestPrivateHidden(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	open := sight(t, r, "/src/app", "git@example.com:o/app.git")
	secret := sight(t, r, "/src/diary", "git@example.com:me/diary.git")
	openTask, secretTask := fileTask(t, r, open, "public work"), fileTask(t, r, secret, "secret work")
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{set(secret, "private", true)}}); err != nil {
		t.Fatal(err)
	}
	outside := &Pane{ID: "pane-1", Session: "s1", Project: open}
	inside := &Pane{ID: "pane-2", Session: "s2", Project: secret}
	nowhere := &Pane{ID: "pane-3"}
	call := func(pn *Pane, method string, params any) (any, error) {
		b, _ := json.Marshal(params)
		a := person
		if pn != nil {
			a = PaneActor(*pn)
		}
		return r.svc.Dispatch(ctx, a, pn, method, b)
	}
	ids := func(res any) []string {
		var out []string
		switch v := res.(type) {
		case api.LogbookRecords:
			for _, x := range v.Records {
				out = append(out, x.ID)
			}
		case api.LogbookProjects:
			for _, x := range v.Projects {
				out = append(out, x.ID)
			}
		}
		slices.Sort(out)
		return out
	}
	sees := func(pn *Pane, id string) bool {
		_, err := call(pn, "logbook.get", api.LogbookGetParams{ID: id})
		if err != nil && kindOf(err) != api.ErrorKindNotFound {
			t.Fatal(err)
		}
		_, herr := call(pn, "logbook.history", api.LogbookHistoryParams{ID: id})
		if (err == nil) != (herr == nil) {
			t.Fatalf("get and history disagree on %s: %v, %v", id, err, herr)
		}
		return err == nil
	}
	for name, pn := range map[string]*Pane{"outside": outside, "nowhere": nowhere} {
		for _, m := range []string{"logbook.find", "logbook.list"} {
			res, err := call(pn, m, map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(res); slices.Contains(got, secret) || slices.Contains(got, secretTask) || !slices.Contains(got, openTask) {
				t.Fatalf("%s %s: %v", name, m, got)
			}
		}
		res, _ := call(pn, "logbook.find", api.LogbookFindParams{Words: "secret"})
		if got := ids(res); len(got) != 0 {
			t.Fatalf("%s finds %v by words", name, got)
		}
		res, _ = call(pn, "logbook.ready", api.LogbookReadyParams{})
		if got := ids(res); !slices.Equal(got, []string{openTask}) {
			t.Fatalf("%s ready %v", name, got)
		}
		res, _ = call(pn, "logbook.projects", api.LogbookProjectsParams{})
		if got := ids(res); !slices.Equal(got, []string{open}) {
			t.Fatalf("%s projects %v", name, got)
		}
		if sees(pn, secret) || sees(pn, secretTask) || !sees(pn, openTask) {
			t.Fatalf("%s reads the private project", name)
		}
		_, err := call(pn, "logbook.write", api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpNote, ID: secretTask, Text: "x"}}})
		if kindOf(err) != api.ErrorKindNotFound {
			t.Fatalf("%s notes a hidden task: %v", name, err)
		}
		c := create("task", "smuggled")
		c.Project = secret
		if _, err := call(pn, "logbook.write", api.LogbookWriteParams{Changes: []api.LogbookChange{c}}); kindOf(err) != api.ErrorKindNotFound {
			t.Fatalf("%s files into a hidden project: %v", name, err)
		}
	}
	for name, pn := range map[string]*Pane{"inside": inside, "person": nil} {
		res, _ := call(pn, "logbook.list", map[string]any{"all": true})
		if got := ids(res); !slices.Contains(got, secretTask) || !slices.Contains(got, openTask) {
			t.Fatalf("%s list %v", name, got)
		}
		if !sees(pn, secret) || !sees(pn, secretTask) {
			t.Fatalf("%s does not read the private project", name)
		}
	}
	ps := projectsOf(t, r)
	for _, p := range ps {
		if (p.ID == secret) != (p.Private && p.Marked) {
			t.Fatalf("project %+v", p)
		}
	}

	// A project joined with the private one is private too, and an agent inside either sees both.
	r.advance(time.Second)
	side := sight(t, r, "/src/diary-2", "git@example.com:me/diary-mirror.git")
	sideTask := fileTask(t, r, side, "mirror work")
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: joinChanges(t, r, secret, side)}); err != nil {
		t.Fatal(err)
	}
	if sees(outside, side) || sees(outside, sideTask) {
		t.Fatal("outside reads a project joined with a private one")
	}
	if !sees(&Pane{ID: "pane-4", Project: side}, secretTask) {
		t.Fatal("an agent in the joined project does not read the private one's records")
	}
	g, _ := r.svc.Get(ctx, api.LogbookGetParams{ID: side})
	if !g.Record.Private || g.Record.Marked || g.Record.JoinedInto != secret {
		t.Fatalf("joined project %+v", g.Record)
	}
}

// TestPaneProjectRules: an agent never files a project, changes an identity or marks a project
// private; a person does.
func TestPaneProjectRules(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	p := sight(t, r, "/src/app", "git@example.com:o/app.git")
	pane := Pane{ID: "pane-1", Session: "s1", Project: p}
	for _, ch := range []api.LogbookChange{
		create("project", "mine"),
		{Op: api.LogbookChangeOpAdd, ID: p, Identity: "example.com/o/other"},
		{Op: api.LogbookChangeOpRemove, ID: p, Identity: "example.com/o/app"},
		set(p, "private", true),
	} {
		if _, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: []api.LogbookChange{ch}}); kindOf(err) != api.ErrorKindInvalid {
			t.Fatalf("pane %+v: %v", ch, err)
		}
		if _, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Propose: true, Changes: []api.LogbookChange{ch}}); kindOf(err) != api.ErrorKindInvalid {
			t.Fatalf("pane proposal %+v: %v", ch, err)
		}
	}
	b, _ := json.Marshal(api.LogbookResolveParams{ID: p, Field: "private", Value: api.RawJSON("true")})
	if _, err := r.svc.Dispatch(ctx, PaneActor(pane), &pane, "logbook.resolve", b); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("pane resolve of private: %v", err)
	}
	if _, err := r.svc.WritePane(ctx, pane, api.LogbookWriteParams{Changes: []api.LogbookChange{set(p, "title", "renamed")}}); err != nil {
		t.Fatalf("a pane renames its project: %v", err)
	}
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{
		{Op: api.LogbookChangeOpAdd, ID: p, Identity: "example.com/o/other"}, set(p, "private", true)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{{Op: api.LogbookChangeOpAdd, ID: p, Identity: "x", Tag: "y"}}}); kindOf(err) != api.ErrorKindInvalid {
		t.Fatalf("an add of an identity and a tag: %v", err)
	}
}

// direct is a sync link straight to another store, as the peer link's logbook.pull and
// logbook.push carry it.
type direct struct{ s *logbook.Store }

func (d direct) Pull(ctx context.Context, have logbook.Vector, skip []string, lim logbook.Limits) (logbook.PullAnswer, error) {
	return d.s.ServePull(ctx, have, skip, lim)
}

func (d direct) Push(ctx context.Context, ops [][]byte) (logbook.PushAnswer, error) {
	a, _, err := d.s.ServePush(ctx, ops)
	return a, err
}

func (direct) Page() logbook.Limits { return logbook.Limits{} }

// TestE9TwoReplicas: E9 (logbook.md 6) over sync. Apart, a session on A files P1 for a clone of
// R by its SSH remote, and one on B files P2 for a clone by HTTPS, with a task in each. After
// one exchange both replicas show one project: P1, the older, keeps its id, P2 is joined into it,
// both tasks list under either id, and the next sight on B files under P1.
func TestE9TwoReplicas(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	ra := newRig2(t, "SHA256:habA", t0)
	rb := newRig2(t, "SHA256:habB", t0.Add(time.Minute))
	ctx := context.Background()
	p1 := sight(t, ra, "/src/r", "git@example.com:o/r.git")
	p2 := sight(t, rb, "/work/r", "https://example.com/o/r.git")
	t1, t2 := fileTask(t, ra, p1, "on A"), fileTask(t, rb, p2, "on B")
	if _, err := ra.svc.Store().Sync(ctx, direct{rb.svc.Store()}); err != nil {
		t.Fatal(err)
	}
	want := []string{t1, t2}
	slices.Sort(want)
	for name, r := range map[string]*rig{"a": ra, "b": rb} {
		for _, id := range []string{p1, p2} {
			if got := listIn(t, r, id); !slices.Equal(got, want) {
				t.Fatalf("%s: list by %s %v, want %v", name, id, got, want)
			}
		}
		g, err := r.svc.Get(ctx, api.LogbookGetParams{ID: p2})
		if err != nil || g.Record.JoinedInto != p1 {
			t.Fatalf("%s: P2 %+v %v, want joined into P1", name, g.Record, err)
		}
		g, _ = r.svc.Get(ctx, api.LogbookGetParams{ID: t2})
		if g.Record.Project != p1 {
			t.Fatalf("%s: B's task shows under %s", name, g.Record.Project)
		}
	}
	if got := sight(t, rb, "/work/r", "https://example.com/o/r.git"); got != p1 {
		t.Fatalf("b files the folder under %s after the join, want %s", got, p1)
	}
}

// newRig2 is newRig with another origin and clock: a second hab.
func newRig2(t *testing.T, origin string, now time.Time) *rig {
	t.Helper()
	ctx := context.Background()
	r := &rig{path: filepath.Join(t.TempDir(), "logbook.db"), now: now}
	st, err := logbook.Open(ctx, r.path, logbook.Options{Origin: origin, Now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r.svc, err = New(ctx, Config{Store: st, Now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
