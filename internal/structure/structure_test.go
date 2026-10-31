package structure

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
