package structure

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) add(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) take() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

func pane(id string) HolderPane {
	return HolderPane{ID: id, HolderID: "h1", HolderBuild: "b1", Cols: 80, Rows: 24}
}

func open(t *testing.T, dir string, h Holders) (*Store, *recorder) {
	t.Helper()
	rec := &recorder{}
	s, err := Open(Config{Dir: dir, Holders: h, OnEvent: rec.add})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s, rec
}

func types(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, string(e.Type)+":"+e.ID)
	}
	return out
}

func mustEvents(t *testing.T, got []Event, want ...string) {
	t.Helper()
	if g := types(got); !reflect.DeepEqual(g, want) {
		t.Fatalf("events\n got %v\nwant %v", g, want)
	}
}

func TestProjectLifecycle(t *testing.T) {
	h := NewFakeHolder(pane("pa"))
	s, rec := open(t, t.TempDir(), h)
	rec.take()

	p, err := s.CreateProject("app", "git@example.com:o/app.git", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Revision != 1 || p.Sessions == nil || len(p.Sessions) != 0 {
		t.Fatalf("new project %+v", p)
	}
	mustEvents(t, rec.take(), "project.updated:"+p.ID)

	if _, err := s.CreateProject("again", "git@example.com:o/app.git", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("same remote: %v", err)
	}
	if _, err := s.CreateProject("x", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no identity: %v", err)
	}
	q, err := s.CreateProject("local", "", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject("local2", "", "abc123"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same root commit: %v", err)
	}
	if got, ok := s.ProjectByIdentity("", "abc123"); !ok || got.ID != q.ID {
		t.Fatalf("ProjectByIdentity: %+v %v", got, ok)
	}
	rec.take()

	p, err = s.RenameProject(p.ID, "app2")
	if err != nil || p.Name != "app2" || p.Revision != 2 {
		t.Fatalf("rename: %+v %v", p, err)
	}
	mustEvents(t, rec.take(), "project.updated:"+p.ID)
	if _, err := s.RenameProject("p-none", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename missing: %v", err)
	}

	ss, err := s.CreateSession(SessionSpec{Name: "one", Project: p.ID, Pane: "pa", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	rec.take()
	if err := s.DeleteProject(p.ID); err != nil {
		t.Fatal(err)
	}
	evs := rec.take()
	mustEvents(t, evs, "session.updated:"+ss.ID, "project.deleted:"+p.ID)
	if evs[1].Revision != 4 { // 1 create, 2 rename, 3 session added, 4 final
		t.Fatalf("final project revision %d", evs[1].Revision)
	}
	snap := s.Snapshot(api.HostInfo{ID: "host"})
	if len(snap.Projects) != 1 || snap.Sessions[0].Project != "" {
		t.Fatalf("after delete: %+v", snap)
	}
	if err := s.DeleteProject(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	h := NewFakeHolder(pane("pa"), pane("pb"), pane("pc"))
	s, rec := open(t, t.TempDir(), h)
	p, _ := s.CreateProject("app", "r", "")
	rec.take()

	a, err := s.CreateSession(SessionSpec{Name: "a", Project: p.ID, Pane: "pa", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "running" || a.Pane != "pa" || a.Revision != 1 {
		t.Fatalf("new session %+v", a)
	}
	mustEvents(t, rec.take(), "session.updated:"+a.ID, "project.updated:"+p.ID, "pane.updated:pa")
	b, _ := s.CreateSession(SessionSpec{Name: "b", Project: p.ID, Pane: "pb", Index: -1})
	c, _ := s.CreateSession(SessionSpec{Name: "c", Project: p.ID, Pane: "pc", Index: 0})
	rec.take()

	if _, err := s.CreateSession(SessionSpec{Name: "a", Pane: "pa"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("name taken: %v", err)
	}
	if _, err := s.CreateSession(SessionSpec{Name: "z", Pane: "pa"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("pane taken: %v", err)
	}
	if _, err := s.CreateSession(SessionSpec{Name: "z", Pane: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing pane: %v", err)
	}
	if _, err := s.RenameSession(a.ID, "b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename onto taken name: %v", err)
	}

	a, err = s.RenameSession(a.ID, "alpha")
	if err != nil || a.Name != "alpha" || a.Revision != 2 {
		t.Fatalf("rename: %+v %v", a, err)
	}
	mustEvents(t, rec.take(), "session.updated:"+a.ID)

	order := func() []string { pr, _ := s.ProjectByIdentity("r", ""); return pr.Sessions }
	if got := order(); !reflect.DeepEqual(got, []string{c.ID, a.ID, b.ID}) {
		t.Fatalf("order %v", got)
	}
	if _, err := s.ReorderSessions(p.ID, []string{a.ID, b.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial reorder: %v", err)
	}
	if _, err := s.ReorderSessions(p.ID, []string{a.ID, a.ID, b.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("repeated reorder: %v", err)
	}
	if _, err := s.ReorderSessions(p.ID, []string{a.ID, b.ID, c.ID}); err != nil {
		t.Fatal(err)
	}
	mustEvents(t, rec.take(), "project.updated:"+p.ID)
	if got := order(); !reflect.DeepEqual(got, []string{a.ID, b.ID, c.ID}) {
		t.Fatalf("order %v", got)
	}
	if _, err := s.MoveSession(c.ID, p.ID, 1); err != nil {
		t.Fatal(err)
	}
	mustEvents(t, rec.take(), "project.updated:"+p.ID)
	if got := order(); !reflect.DeepEqual(got, []string{a.ID, c.ID, b.ID}) {
		t.Fatalf("order after move %v", got)
	}
	q, _ := s.CreateProject("other", "r2", "")
	rec.take()
	moved, err := s.MoveSession(b.ID, q.ID, -1)
	if err != nil || moved.Project != q.ID {
		t.Fatalf("move to other project: %+v %v", moved, err)
	}
	mustEvents(t, rec.take(), "session.updated:"+b.ID, "project.updated:"+p.ID, "project.updated:"+q.ID)

	if err := s.CloseSession(a.ID); err != nil {
		t.Fatal(err)
	}
	evs := rec.take()
	mustEvents(t, evs, "session.deleted:"+a.ID, "project.updated:"+p.ID, "pane.updated:pa")
	if evs[0].Revision != 3 {
		t.Fatalf("final session revision %d", evs[0].Revision)
	}
	if got := s.Unfiled(); !reflect.DeepEqual(got, []string{"pa"}) {
		t.Fatalf("unfiled after close %v", got)
	}
	if err := s.CloseSession(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("close twice: %v", err)
	}
	// The name is free again once the record is closed.
	if _, err := s.CreateSession(SessionSpec{Name: "alpha", Pane: "pa"}); err != nil {
		t.Fatal(err)
	}
}

// TestSessionForNewPane: session.create files the session before it spawns (NewPane), so the
// record names a pane the holder has not reported yet. No pane event goes with the record; the
// pane's first pane.updated already names the session. A failed spawn closes the record:
// session.deleted follows, and the name is free again.
func TestSessionForNewPane(t *testing.T) {
	h := NewFakeHolder(pane("live"))
	s, rec := open(t, t.TempDir(), h)
	p, _ := s.CreateProject("app", "r", "")
	rec.take()

	a, err := s.CreateSession(SessionSpec{Name: "a", Project: p.ID, Pane: "new-a", Index: -1, NewPane: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "running" || a.Pane != "new-a" {
		t.Fatalf("new session %+v", a)
	}
	mustEvents(t, rec.take(), "session.updated:"+a.ID, "project.updated:"+p.ID)
	if got, ok := s.SessionOfPane("new-a"); !ok || got.ID != a.ID {
		t.Fatalf("session of the new pane before the holder reports it: %+v %v", got, ok)
	}
	h.Add(pane("new-a"))
	evs := rec.take()
	mustEvents(t, evs, "pane.updated:new-a")
	if pa := evs[0].Data.(api.Pane); pa.Session != a.ID || pa.Revision != 1 {
		t.Fatalf("the new pane's first event %+v", pa)
	}
	if got := s.Unfiled(); !reflect.DeepEqual(got, []string{"live"}) {
		t.Fatalf("unfiled %v", got)
	}

	for _, bad := range []struct {
		spec SessionSpec
		want error
	}{
		{SessionSpec{Name: "z", Pane: "", NewPane: true}, ErrInvalid},
		{SessionSpec{Name: "z", Pane: "live", NewPane: true}, ErrConflict},  // live already
		{SessionSpec{Name: "z", Pane: "new-a", NewPane: true}, ErrConflict}, // live and filed
		{SessionSpec{Name: "a", Pane: "new-z", NewPane: true}, ErrConflict}, // name taken
		{SessionSpec{Name: "z", Project: "nope", Pane: "new-z", NewPane: true}, ErrNotFound},
	} {
		if _, err := s.CreateSession(bad.spec); !errors.Is(err, bad.want) {
			t.Errorf("%+v: %v, want %v", bad.spec, err, bad.want)
		}
	}
	h.Remove("live")
	rec.take()
	if _, err := s.CreateSession(SessionSpec{Name: "z", Pane: "live", NewPane: true}); !errors.Is(err, ErrConflict) {
		t.Errorf("a gone pane as a new one: %v", err)
	}
	if evs := rec.take(); len(evs) != 0 {
		t.Fatalf("refused creates sent %v", types(evs))
	}

	// The spawn failed: the caller closes the record, and its removal follows its creation.
	b, err := s.CreateSession(SessionSpec{Name: "b", Project: p.ID, Pane: "new-b", Index: -1, NewPane: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CloseSession(b.ID); err != nil {
		t.Fatal(err)
	}
	mustEvents(t, rec.take(), "session.updated:"+b.ID, "project.updated:"+p.ID, "session.deleted:"+b.ID, "project.updated:"+p.ID)
	if _, ok := s.SessionOfPane("new-b"); ok {
		t.Fatal("the failed spawn's pane still names a session")
	}
	if _, err := s.CreateSession(SessionSpec{Name: "b", Pane: "new-c", NewPane: true}); err != nil {
		t.Fatalf("the name after a failed spawn: %v", err)
	}
}

// TestRestartDropsNeverSpawned: gff-hab stopped between filing a session (NewPane) and its
// spawn. At the next start the pane is not live, was never saved and has no tombstone, so the
// session goes and its name is free. A session whose pane was live once and is gone now stays,
// exited.
func TestRestartDropsNeverSpawned(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder(pane("pa"))
	s, _ := open(t, dir, h)
	p, _ := s.CreateProject("app", "r", "")
	kept, _ := s.CreateSession(SessionSpec{Name: "kept", Project: p.ID, Pane: "pa", Index: -1})
	ghost, err := s.CreateSession(SessionSpec{Name: "ghost", Project: p.ID, Pane: "never", Index: -1, NewPane: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	h.RemoveQuietly("pa")
	s2, rec := open(t, dir, h)
	evs := rec.take()
	want := []string{"session.deleted:" + ghost.ID, "project.updated:" + p.ID, "session.updated:" + kept.ID, "pane.deleted:pa"}
	got := types(evs)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Fatalf("events %v lack %s", got, w)
		}
	}
	if _, ok := s2.Session(ghost.ID); ok {
		t.Fatal("the never-spawned session is still there")
	}
	if k, ok := s2.Session(kept.ID); !ok || k.State != "exited" {
		t.Fatalf("the session of a gone pane: %+v %v", k, ok)
	}
	if pr, _ := s2.Project(p.ID); !reflect.DeepEqual(pr.Sessions, []string{kept.ID}) {
		t.Fatalf("project sessions %v", pr.Sessions)
	}
	if _, err := s2.CreateSession(SessionSpec{Name: "ghost", Pane: "new", NewPane: true}); err != nil {
		t.Fatalf("the name after the restart: %v", err)
	}
}

func TestPaneExitAndGone(t *testing.T) {
	h := NewFakeHolder(pane("pa"), pane("pb"))
	s, rec := open(t, t.TempDir(), h)
	a, _ := s.CreateSession(SessionSpec{Name: "a", Pane: "pa"})
	b, _ := s.CreateSession(SessionSpec{Name: "b", Pane: "pb"})
	rec.take()

	code := 2
	h.Exit("pa", api.ExitStatus{Code: &code})
	evs := rec.take()
	mustEvents(t, evs, "session.updated:"+a.ID)
	got := evs[0].Data.(api.Session)
	if got.State != "exited" || got.Exit == nil || *got.Exit.Code != 2 || got.Revision != 2 {
		t.Fatalf("exited session %+v", got)
	}

	h.Remove("pb")
	evs = rec.take()
	mustEvents(t, evs, "session.updated:"+b.ID, "pane.deleted:pb")
	if evs[0].Data.(api.Session).State != "exited" {
		t.Fatalf("session of a gone pane: %+v", evs[0].Data)
	}
	snap := s.Snapshot(api.HostInfo{})
	if len(snap.Sessions) != 2 || len(snap.Panes) != 1 {
		t.Fatalf("records must stay: %+v", snap)
	}

	h.Add(pane("pc"))
	mustEvents(t, rec.take(), "pane.updated:pc")
	h.Resize("pc", 100, 30)
	evs = rec.take()
	mustEvents(t, evs, "pane.updated:pc")
	if pa := evs[0].Data.(api.Pane); pa.Cols != 100 || pa.Revision != 2 {
		t.Fatalf("resized pane %+v", pa)
	}
	h.Write("pc", 4096)
	for _, pa := range s.Snapshot(api.HostInfo{}).Panes {
		if pa.ID == "pc" && pa.Offset != 4096 {
			t.Fatalf("offset %d, want the holder's absolute offset", pa.Offset)
		}
	}
}

func TestRestartKeepsStructure(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder(pane("p1"), pane("p2"), pane("p3"))
	s, _ := open(t, dir, h)
	pr, _ := s.CreateProject("app", "r", "")
	var ids []string
	for i, pn := range []string{"p1", "p2", "p3"} {
		ss, err := s.CreateSession(SessionSpec{Name: "s" + pn, Project: pr.ID, Pane: pn, Index: 0})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ss.ID)
		if i == 1 {
			s.RenameSession(ss.ID, "renamed")
		}
	}
	h.Resize("p1", 120, 40)
	before := s.Snapshot(api.HostInfo{ID: "h"})
	s.Close()

	s2, rec2 := open(t, dir, h)
	after := s2.Snapshot(api.HostInfo{ID: "h"})
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("structure changed across restart\nbefore %+v\nafter  %+v", before, after)
	}
	if evs := rec2.take(); len(evs) != 0 {
		t.Fatalf("a clean restart emitted %v", types(evs))
	}
	if after.Projects[0].Revision != 4 {
		t.Fatalf("project revision %d, want 4", after.Projects[0].Revision)
	}
	if h := s2.Health(); h.Problem != "" || h.SaveError != "" {
		t.Fatalf("health %+v", h)
	}

	// Revisions keep rising from the stored value.
	ss, _ := s2.RenameSession(ids[0], "again")
	if ss.Revision != 2 {
		t.Fatalf("session revision after restart %d", ss.Revision)
	}
}

func TestRestartUnfiledAndGone(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder(pane("p1"), pane("p2"))
	s, _ := open(t, dir, h)
	a, _ := s.CreateSession(SessionSpec{Name: "a", Pane: "p1"})
	b, _ := s.CreateSession(SessionSpec{Name: "b", Pane: "p2"})
	s.Close()

	// While gff-hab is down: p2 vanishes, p1's child exits, a new pane appears.
	h.RemoveQuietly("p2")
	code := 0
	h.Exit("p1", api.ExitStatus{Code: &code})
	h.Add(pane("p9"))

	s2, rec := open(t, dir, h)
	evs := rec.take()
	mustEvents(t, evs, "pane.deleted:p2", "session.updated:"+a.ID, "session.updated:"+b.ID, "pane.updated:p9")
	if evs[0].Revision != 3 { // 1 seen, 2 linked to b, 3 final
		t.Fatalf("pane.deleted revision %d", evs[0].Revision)
	}
	if got := s2.Unfiled(); !reflect.DeepEqual(got, []string{"p9"}) {
		t.Fatalf("unfiled %v", got)
	}
	snap := s2.Snapshot(api.HostInfo{})
	for _, ss := range snap.Sessions {
		if ss.State != "exited" {
			t.Fatalf("session %s state %s", ss.Name, ss.State)
		}
	}
	if snap.Sessions[0].Exit == nil || *snap.Sessions[0].Exit.Code != 0 {
		t.Fatalf("exit status lost: %+v", snap.Sessions[0])
	}
}

func TestCorruptStateFile(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":      "{not json",
		"old schema":   `{"schema": 0, "projects": [], "sessions": [], "panes": []}`,
		"inconsistent": `{"schema": 1, "projects": [], "sessions": [{"id":"s-1","revision":1,"name":"x","project":"p-gone","state":"running"}], "panes": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, StateFile)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			h := NewFakeHolder(pane("p1"), pane("p2"))
			s, _ := open(t, dir, h)
			hl := s.Health()
			if hl.Problem == "" || hl.KeptAs == "" {
				t.Fatalf("problem not reported: %+v", hl)
			}
			kept, err := os.ReadFile(hl.KeptAs)
			if err != nil || string(kept) != content {
				t.Fatalf("bad file not kept: %v", err)
			}
			if !strings.HasPrefix(filepath.Base(hl.KeptAs), StateFile+".bad-") {
				t.Fatalf("kept as %s", hl.KeptAs)
			}
			if got := s.Unfiled(); !reflect.DeepEqual(got, []string{"p1", "p2"}) {
				t.Fatalf("panes not listed as unfiled: %v", got)
			}
			if ps, _ := h.Panes(); len(ps) != 2 {
				t.Fatalf("panes touched: %v", ps)
			}
			// The store works and writes a good file again.
			if _, err := s.CreateSession(SessionSpec{Name: "x", Pane: "p1"}); err != nil {
				t.Fatal(err)
			}
			if _, err := loadState(path); err != nil {
				t.Fatalf("new state file unreadable: %v", err)
			}
		})
	}
}

func TestUnreadableStateFile(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: reading it fails.
	if err := os.Mkdir(filepath.Join(dir, StateFile), 0o700); err != nil {
		t.Fatal(err)
	}
	s, _ := open(t, dir, NewFakeHolder(pane("p1")))
	if hl := s.Health(); hl.Problem == "" || hl.KeptAs == "" {
		t.Fatalf("health %+v", hl)
	}
	if got := s.Unfiled(); len(got) != 1 {
		t.Fatalf("unfiled %v", got)
	}
}

func TestRequiredArraysNeverNull(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder(pane("p1"))
	rec := &recorder{}
	s, err := Open(Config{Dir: dir, Holders: h, OnEvent: rec.add})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	check := func(what string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "null") {
			t.Fatalf("%s has null: %s", what, b)
		}
	}
	empty := NewFakeHolder()
	s0, _ := open(t, t.TempDir(), empty)
	check("empty snapshot", s0.Snapshot(api.HostInfo{ID: "h"}))

	p, _ := s.CreateProject("app", "r", "")
	check("project", p)
	check("snapshot", s.Snapshot(api.HostInfo{ID: "h"}))
	s.CreateSession(SessionSpec{Name: "a", Pane: "p1"})
	s.DeleteProject(p.ID)
	for i, e := range rec.take() {
		ev, err := e.API(uint64(i + 1))
		if err != nil {
			t.Fatal(err)
		}
		check(string(e.Type), ev)
		if ev.Revision == nil || *ev.Revision == 0 {
			t.Fatalf("event %s has no revision", e.Type)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, StateFile))
	check("state file", json.RawMessage(b))
}

func TestConcurrentUse(t *testing.T) {
	h := NewFakeHolder()
	s, _ := open(t, t.TempDir(), h)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + i))
			h.Add(pane(id))
			s.CreateSession(SessionSpec{Name: id, Pane: id})
			s.Snapshot(api.HostInfo{})
			h.Resize(id, 90, 30)
		}()
	}
	wg.Wait()
	if snap := s.Snapshot(api.HostInfo{}); len(snap.Sessions) != 8 || len(snap.Panes) != 8 {
		t.Fatalf("snapshot %d sessions, %d panes", len(snap.Sessions), len(snap.Panes))
	}
}

// TestSetOwnerDoesNotSave: an owner change emits its event and raises the pane's revision, but
// never writes the state file, so a caller holding a lock never waits on the disk (spec 6.8).
func TestSetOwnerDoesNotSave(t *testing.T) {
	s, rec := open(t, t.TempDir(), NewFakeHolder(pane("pa")))
	rec.take()
	path := s.Health().StatePath
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file after open: %v", err)
	}
	rev := s.Snapshot(api.HostInfo{ID: "host"}).Panes[0].Revision
	if !s.SetOwner("pa", api.PaneOwner{Owner: "client-a", Reason: api.OwnerReasonTake, Cols: 80, Rows: 24}) {
		t.Fatal("SetOwner of a known pane reported false")
	}
	evs := rec.take()
	mustEvents(t, evs, "pane.owner:pa")
	if evs[0].Revision != rev+1 {
		t.Fatalf("pane.owner revision %d, want %d", evs[0].Revision, rev+1)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("an owner change wrote the state file:\nbefore %s\nafter  %s", before, after)
	}
	if p := s.Snapshot(api.HostInfo{ID: "host"}).Panes[0]; p.Owner != "client-a" || p.Revision != rev+1 {
		t.Fatalf("pane after SetOwner: owner %q revision %d", p.Owner, p.Revision)
	}
}
