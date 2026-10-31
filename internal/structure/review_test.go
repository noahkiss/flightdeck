package structure

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// A holder that drops out and comes back must not leave its sessions exited for good, and its
// panes must keep rising revisions.
func TestHolderDropoutRecovers(t *testing.T) {
	h := NewFakeHolder(pane("p1"))
	s, rec := open(t, t.TempDir(), h)
	a, _ := s.CreateSession(SessionSpec{Name: "a", Pane: "p1"}) // pane rev 2
	rec.take()

	h.Remove("p1")
	evs := rec.take()
	mustEvents(t, evs, "session.updated:"+a.ID, "pane.deleted:p1")
	if evs[1].Revision != 3 {
		t.Fatalf("final pane revision %d", evs[1].Revision)
	}
	h.Add(pane("p1"))
	evs = rec.take()
	mustEvents(t, evs, "session.updated:"+a.ID, "pane.updated:p1")
	if ss := evs[0].Data.(api.Session); ss.State != "running" || ss.Exit != nil || ss.Revision != 3 { // 1 create, 2 lost, 3 back
		t.Fatalf("revived session %+v", ss)
	}
	if pa := evs[1].Data.(api.Pane); pa.Revision != 4 || pa.Session != a.ID {
		t.Fatalf("returned pane %+v", pa)
	}

	// A real exit is final: the session does not revive.
	code := 1
	h.Exit("p1", api.ExitStatus{Code: &code})
	h.Remove("p1")
	h.Add(pane("p1"))
	for _, ss := range s.Snapshot(api.HostInfo{}).Sessions {
		if ss.State != "exited" || ss.Exit == nil {
			t.Fatalf("session after a real exit %+v", ss)
		}
	}
}

func TestHolderDropoutAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder(pane("p1"))
	s, _ := open(t, dir, h)
	a, _ := s.CreateSession(SessionSpec{Name: "a", Pane: "p1"})
	s.Close()

	h.RemoveQuietly("p1")
	s2, _ := open(t, dir, h)
	if got := s2.Snapshot(api.HostInfo{}).Sessions[0]; got.State != "exited" || got.Exit != nil {
		t.Fatalf("session while its holder is gone %+v", got)
	}
	s2.Close()

	h.Add(pane("p1"))
	s3, rec := open(t, dir, h)
	evs := rec.take()
	mustEvents(t, evs, "session.updated:"+a.ID, "pane.updated:p1")
	if ss := evs[0].Data.(api.Session); ss.State != "running" {
		t.Fatalf("session after the holder came back %+v", ss)
	}
	if pa := evs[1].Data.(api.Pane); pa.Revision != 4 { // 1 seen, 2 linked, 3 final, 4 back
		t.Fatalf("pane revision %d, want one past its tombstone", pa.Revision)
	}
	if st := s3.Snapshot(api.HostInfo{}); len(st.Panes) != 1 || st.Panes[0].Session != a.ID {
		t.Fatalf("snapshot %+v", st)
	}
}

type failingHolder struct {
	*FakeHolder
	err error
}

func (f failingHolder) Panes() ([]HolderPane, error) { return nil, f.err }

func TestPanesFailureChangesNothing(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder(pane("p1"))
	s, _ := open(t, dir, h)
	s.CreateSession(SessionSpec{Name: "a", Pane: "p1"})
	s.Close()
	before, _ := os.ReadFile(filepath.Join(dir, StateFile))

	if _, err := Open(Config{Dir: dir, Holders: failingHolder{h, errors.New("holder unreachable")}}); err == nil {
		t.Fatal("Open succeeded with no pane list")
	}
	after, _ := os.ReadFile(filepath.Join(dir, StateFile))
	if string(before) != string(after) {
		t.Fatal("a failed pane list changed the state file")
	}
	s2, _ := open(t, dir, h)
	if got := s2.Snapshot(api.HostInfo{}).Sessions[0].State; got != "running" {
		t.Fatalf("state %s", got)
	}
}

// asyncHolder delivers a notice from another goroutine inside Panes and cancel, and waits for
// it. A store that held its lock across those calls would deadlock.
type asyncHolder struct {
	*FakeHolder
	fn func(Notice)
}

func (a *asyncHolder) Subscribe(fn func(Notice)) func() {
	a.fn = fn
	cancel := a.FakeHolder.Subscribe(fn)
	return func() {
		a.deliver(Notice{Kind: PaneResized, Pane: HolderPane{ID: "p2", Cols: 1, Rows: 1}})
		cancel()
	}
}

func (a *asyncHolder) Panes() ([]HolderPane, error) {
	a.deliver(Notice{Kind: PaneAdded, Pane: pane("p2")})
	return a.FakeHolder.Panes()
}

func (a *asyncHolder) deliver(n Notice) {
	done := make(chan struct{})
	go func() { a.fn(n); close(done) }()
	<-done
}

func TestNoHolderCallsUnderLock(t *testing.T) {
	h := &asyncHolder{FakeHolder: NewFakeHolder(pane("p1"))}
	finished := make(chan []string)
	go func() {
		s, err := Open(Config{Dir: t.TempDir(), Holders: h})
		if err != nil {
			t.Error(err)
			close(finished)
			return
		}
		u := s.Unfiled()
		s.Close()
		finished <- u
	}()
	select {
	case got := <-finished:
		// p2 arrived by notice before reconcile; it is applied after it, not lost.
		if !reflect.DeepEqual(got, []string{"p1", "p2"}) {
			t.Fatalf("unfiled %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: the store called into the holder under its lock")
	}
}

func TestNewerSchemaReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StateFile)
	content := `{"schema": 99, "projects": [], "sessions": [], "panes": [], "future": true}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := open(t, dir, NewFakeHolder(pane("p1")))
	hl := s.Health()
	if !hl.ReadOnly || hl.Problem == "" || hl.KeptAs != "" {
		t.Fatalf("health %+v", hl)
	}
	if got := s.Unfiled(); !reflect.DeepEqual(got, []string{"p1"}) {
		t.Fatalf("unfiled %v", got)
	}
	if _, err := s.CreateSession(SessionSpec{Name: "a", Pane: "p1"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Fatal("the newer state file was overwritten")
	}
	matches, _ := filepath.Glob(path + ".bad-*")
	if len(matches) != 0 {
		t.Fatalf("newer file renamed aside: %v", matches)
	}
}

func TestStateDirLocked(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder()
	s, _ := open(t, dir, h)
	if _, err := Open(Config{Dir: dir, Holders: h}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: %v", err)
	}
	s.Close()
	s2, err := Open(Config{Dir: dir, Holders: h})
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	s2.Close()
}

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.com/o/app.git":      "example.com/o/app",
		"https://user@Example.COM/o/app/":    "example.com/o/app",
		"http://example.com:8080/o/app":      "example.com/o/app",
		"ssh://git@example.com:22/o/app.git": "example.com/o/app",
		"git@example.com:o/app.git":          "example.com/o/app",
		"example.com:o/App":                  "example.com/o/App",
		"git://example.com/o/app":            "example.com/o/app",
		"file:///srv/git/app.git":            "/srv/git/app",
		"/srv/git/app.git/":                  "/srv/git/app",
		"../app":                             "../app",
		"":                                   "",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}

	s, _ := open(t, t.TempDir(), NewFakeHolder())
	p, err := s.CreateProject("app", "https://example.com/o/app.git", "")
	if err != nil || p.Remote != "example.com/o/app" {
		t.Fatalf("create: %+v %v", p, err)
	}
	if _, err := s.CreateProject("dup", "git@EXAMPLE.com:o/app", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("same repo, other form: %v", err)
	}
	if got, ok := s.ProjectByIdentity("ssh://git@example.com/o/app.git", ""); !ok || got.ID != p.ID {
		t.Fatalf("lookup: %+v %v", got, ok)
	}
}

func TestDirSyncEINVAL(t *testing.T) {
	old := syncFile
	t.Cleanup(func() { syncFile = old })
	path := filepath.Join(t.TempDir(), StateFile)
	st := &stateFile{Schema: SchemaVersion}

	syncFile = func(f *os.File) error {
		if fi, _ := f.Stat(); fi != nil && fi.IsDir() {
			return &os.PathError{Op: "sync", Path: f.Name(), Err: syscall.EINVAL}
		}
		return f.Sync()
	}
	if err := writeState(path, st); err != nil {
		t.Fatalf("EINVAL from a directory fsync must count as success: %v", err)
	}
	syncFile = func(f *os.File) error { return &os.PathError{Op: "sync", Path: f.Name(), Err: syscall.EIO} }
	if err := writeState(path, st); err == nil {
		t.Fatal("EIO must fail the save")
	}
}
