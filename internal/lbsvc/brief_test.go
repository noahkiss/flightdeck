package lbsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/logbook"
)

// briefRig is a rig whose store and service share one fake clock, so a record's age is known.
func briefRig(t *testing.T) (*rig, func(time.Duration)) {
	t.Helper()
	ctx := context.Background()
	var mu sync.Mutex
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r := &rig{path: filepath.Join(t.TempDir(), "logbook.db")}
	st, err := logbook.Open(ctx, r.path, logbook.Options{Origin: "SHA256:habA", Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if r.svc, err = New(ctx, Config{Store: st, Now: clock}); err != nil {
		t.Fatal(err)
	}
	return r, func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
}

func write(t *testing.T, r *rig, chs ...api.LogbookChange) []string {
	t.Helper()
	w, err := r.svc.Write(context.Background(), person, api.LogbookWriteParams{Changes: chs})
	if err != nil {
		t.Fatal(err)
	}
	return w.IDs
}

func rec(typ, id, project, title string, f func(*api.LogbookChange)) api.LogbookChange {
	c := api.LogbookChange{Op: api.LogbookChangeOpCreate, Type: typ, ID: id, Project: project, Title: title}
	if f != nil {
		f(&c)
	}
	return c
}

func briefOf(t *testing.T, r *rig, pn *Pane, project string) (api.LogbookBrief, error) {
	t.Helper()
	b, _ := json.Marshal(api.LogbookBriefParams{Project: project})
	a := person
	if pn != nil {
		a = PaneActor(*pn)
	}
	res, err := r.svc.Dispatch(context.Background(), a, pn, "logbook.brief", b)
	if err != nil {
		return api.LogbookBrief{}, err
	}
	return res.(api.LogbookBrief), nil
}

// TestBriefContent: the brief of 11.2, section by section in rank order, each record once, with
// what does not belong left out: closed tasks, a retired or unpinned guide, an old or superseded
// finding, a plan with no open stage, another project's records, and general ones not tagged
// everywhere.
func TestBriefContent(t *testing.T) {
	r, advance := briefRig(t)
	app := sight(t, r, "/work/app", "git@example.com:o/app.git")
	other := sight(t, r, "/work/other", "git@example.com:o/other.git")
	write(t, r, rec("finding", "finding-old00001", app, "an old finding", nil))
	advance(8 * 24 * time.Hour)
	write(t, r,
		rec("task", "task-doing001", app, "doing it", func(c *api.LogbookChange) { c.Status = "doing" }),
		rec("issue", "issue-block001", app, "blocked one", func(c *api.LogbookChange) { c.Status = "blocked" }),
		rec("task", "task-ready003", app, "low priority", func(c *api.LogbookChange) { c.Priority = 3 }),
		rec("task", "task-readynone", app, "no priority", nil),
		rec("task", "task-ready001", app, "top priority", func(c *api.LogbookChange) { c.Priority = 1 }),
		rec("task", "task-done0001", app, "finished", func(c *api.LogbookChange) { c.Status = "done" }),
		rec("task", "task-from0001", other, "source", nil),
		rec("task", "task-hand0001", app, "handed over", func(c *api.LogbookChange) {
			c.Triage = "new"
			c.Links = []api.LogbookLink{{Kind: "from", To: "task-from0001"}}
		}),
		rec("guide", "guide-pin00001", app, "how to build", func(c *api.LogbookChange) { c.Pinned = true }),
		rec("guide", "guide-nopin001", app, "not pinned", nil),
		rec("guide", "guide-retired1", app, "retired", func(c *api.LogbookChange) { c.Pinned, c.Status = true, "retired" }),
		rec("finding", "finding-new00001", app, "a new finding", nil),
		rec("finding", "finding-gone0001", app, "replaced", nil),
		rec("finding", "finding-repl0001", app, "the replacement", func(c *api.LogbookChange) {
			c.Links = []api.LogbookLink{{Kind: "supersedes", To: "finding-gone0001"}}
		}),
		rec("plan", "plan-open0001", app, "the plan", func(c *api.LogbookChange) {
			c.Links = []api.LogbookLink{{Kind: "stage", To: "task-readynone", Pos: 1}}
		}),
		rec("plan", "plan-closed001", app, "a plan with no open stage", func(c *api.LogbookChange) {
			c.Links = []api.LogbookLink{{Kind: "stage", To: "task-done0001", Pos: 1}}
		}),
		rec("finding", "finding-every001", "", "true everywhere", func(c *api.LogbookChange) { c.Tags = []string{"everywhere"} }),
		rec("finding", "finding-gen00001", "", "general, not tagged", nil),
		rec("task", "task-other001", other, "elsewhere", func(c *api.LogbookChange) { c.Status = "doing" }),
	)
	got, err := briefOf(t, r, nil, app)
	if err != nil {
		t.Fatal(err)
	}
	want := `Logbook brief for app (` + app + `): 1 untriaged, 0 flagged.
One line per record: id, status, title. Read one with ` + "`gff lb show <id>`" + `.

## Doing and blocked
task-doing001  doing  doing it
issue-block001  blocked  blocked one

## Handed over from other projects
task-hand0001  open  handed over

## Ready
task-ready001  open  top priority
task-ready003  open  low priority
task-readynone  open  no priority

## Pinned guides
guide-pin00001  current  how to build

## Findings from the last 7 days
finding-new00001  a new finding
finding-repl0001  the replacement

## Open plans
plan-open0001  open  the plan

## For every project
finding-every001  true everywhere
`
	if got.Text != want {
		t.Fatalf("brief:\n%s\nwant:\n%s", got.Text, want)
	}
	if got.Project != app || got.Name != "app" || got.Records != 11 || got.Dropped != 0 {
		t.Fatalf("brief fields %+v", got)
	}
	// With no project only the records for every project show.
	none, err := briefOf(t, r, &Pane{ID: "pane-x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(none.Text, "Logbook brief: no project") || !strings.Contains(none.Text, "finding-every001") || none.Records != 1 {
		t.Fatalf("no-project brief:\n%s", none.Text)
	}
	// Not a project: not found.
	if _, err := briefOf(t, r, nil, "task-doing001"); kindOf(err) != api.ErrorKindNotFound {
		t.Fatalf("brief of a task: %v", err)
	}
}

// TestBriefCap: with 1,000 open tasks the brief stays under 8 KB, keeps the higher ranks, drops
// the rest by rank and ends with the more line.
func TestBriefCap(t *testing.T) {
	r, _ := briefRig(t)
	app := sight(t, r, "/work/app", "git@example.com:o/app.git")
	var chs []api.LogbookChange
	for i := range 1000 {
		chs = append(chs, rec("task", "", app, fmt.Sprintf("open task number %04d with a title of some length", i), nil))
		if len(chs) == MaxChanges {
			write(t, r, chs...)
			chs = nil
		}
	}
	write(t, r, rec("task", "task-doing001", app, "doing it", func(c *api.LogbookChange) { c.Status = "doing" }),
		rec("finding", "finding-new00001", app, "a finding", nil))
	got, err := briefOf(t, r, nil, app)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Text) > BriefCap {
		t.Fatalf("brief is %d bytes", len(got.Text))
	}
	if !strings.HasSuffix(got.Text, BriefMore) || got.Dropped == 0 || got.Records == 0 {
		t.Fatalf("records %d dropped %d, ends %q", got.Records, got.Dropped, got.Text[len(got.Text)-40:])
	}
	if !strings.Contains(got.Text, "task-doing001") || strings.Contains(got.Text, "finding-new00001") {
		t.Fatalf("the cap dropped by something other than rank:\n%s", got.Text)
	}
	if n := strings.Count(got.Text, "\ntask-"); n != got.Records {
		t.Fatalf("%d task lines, %d records", n, got.Records)
	}
	// The doing task, the finding, and the ready section's first logbook.MaxLimit tasks: it
	// reads no more, since the rest are past the cap anyway.
	if got.Records+got.Dropped != 2+logbook.MaxLimit {
		t.Fatalf("records %d + dropped %d", got.Records, got.Dropped)
	}
}

// TestBriefPrivate: an agent's brief never shows a private project outside its own, and its own
// private project's brief works.
func TestBriefPrivate(t *testing.T) {
	r, _ := briefRig(t)
	ctx := context.Background()
	app := sight(t, r, "/work/app", "git@example.com:o/app.git")
	secret := sight(t, r, "/work/diary", "git@example.com:me/diary.git")
	write(t, r, rec("task", "task-secret01", secret, "secret work", nil), rec("task", "task-public01", app, "public work", nil))
	if _, err := r.svc.Write(ctx, person, api.LogbookWriteParams{Changes: []api.LogbookChange{set(secret, "private", true)}}); err != nil {
		t.Fatal(err)
	}
	outside := &Pane{ID: "pane-1", Session: "s1", Project: app}
	inside := &Pane{ID: "pane-2", Session: "s2", Project: secret}
	if _, err := briefOf(t, r, outside, secret); kindOf(err) != api.ErrorKindNotFound {
		t.Fatalf("an agent outside reads the private brief: %v", err)
	}
	b, err := briefOf(t, r, outside, "")
	if err != nil || b.Project != app || strings.Contains(b.Text, "secret") {
		t.Fatalf("outside brief %v:\n%s", err, b.Text)
	}
	b, err = briefOf(t, r, inside, "")
	if err != nil || b.Project != secret || !strings.Contains(b.Text, "task-secret01") || strings.Contains(b.Text, "public") {
		t.Fatalf("inside brief %v:\n%s", err, b.Text)
	}
	if b, err := briefOf(t, r, nil, secret); err != nil || !strings.Contains(b.Text, "task-secret01") {
		t.Fatalf("a person's brief of the private project %v", err)
	}
}

// TestDefaultView: a pane's ls and ready cover its project unless the call names a project,
// general or all (12.1); a person's cover every project; find searches every project.
func TestDefaultView(t *testing.T) {
	r, _ := briefRig(t)
	ctx := context.Background()
	app := sight(t, r, "/work/app", "git@example.com:o/app.git")
	other := sight(t, r, "/work/other", "git@example.com:o/other.git")
	write(t, r, rec("task", "task-app00001", app, "here", nil), rec("task", "task-other001", other, "there", nil),
		rec("task", "task-general1", "", "nowhere", nil))
	pn := &Pane{ID: "pane-1", Session: "s1", Project: app}
	ids := func(pn *Pane, method string, params any) []string {
		t.Helper()
		b, _ := json.Marshal(params)
		a := person
		if pn != nil {
			a = PaneActor(*pn)
		}
		res, err := r.svc.Dispatch(ctx, a, pn, method, b)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range res.(api.LogbookRecords).Records {
			if x.Type == "task" {
				out = append(out, x.ID)
			}
		}
		slices.Sort(out)
		return out
	}
	all := []string{"task-app00001", "task-general1", "task-other001"}
	for _, c := range []struct {
		pn     *Pane
		method string
		params any
		want   []string
	}{
		{pn, "logbook.list", map[string]any{}, []string{"task-app00001"}},
		{pn, "logbook.ready", map[string]any{}, []string{"task-app00001"}},
		{pn, "logbook.list", map[string]any{"all": true}, all},
		{pn, "logbook.ready", map[string]any{"all": true}, all},
		{pn, "logbook.list", map[string]any{"general": true}, []string{"task-general1"}},
		{pn, "logbook.list", map[string]any{"project": other}, []string{"task-other001"}},
		{pn, "logbook.ready", map[string]any{"project": other}, []string{"task-other001"}},
		{pn, "logbook.find", map[string]any{}, all},
		{&Pane{ID: "pane-2"}, "logbook.list", map[string]any{}, all},
		{nil, "logbook.list", map[string]any{}, all},
		{nil, "logbook.ready", map[string]any{}, all},
	} {
		if got := ids(c.pn, c.method, c.params); !slices.Equal(got, c.want) {
			t.Errorf("%v %s %v: %v, want %v", c.pn, c.method, c.params, got, c.want)
		}
	}
}
