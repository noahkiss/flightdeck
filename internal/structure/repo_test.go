package structure

import (
	"testing"
)

// A project's working directories and repository state survive a restart. SetProjectRepo
// raises the revision and emits only when a value changes.
func TestProjectRepo(t *testing.T) {
	dir := t.TempDir()
	h := NewFakeHolder()
	s, rec := open(t, dir, h)
	p, err := s.CreateProject("app", "example.com/o/app", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"/w/a", "/w/b", "/w/a/"} {
		if err := s.AddProjectDir(p.ID, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddProjectDir(p.ID, "rel"); err == nil {
		t.Error("a relative directory was accepted")
	}
	rec.take()
	one, zero := 1, 0
	st := RepoState{Branch: "main", Head: "abc", Ahead: &one, Behind: &zero}
	if ch, err := s.SetProjectRepo(p.ID, st); !ch || err != nil {
		t.Fatalf("first set: %v %v", ch, err)
	}
	if ch, _ := s.SetProjectRepo(p.ID, st); ch {
		t.Error("an unchanged state reported a change")
	}
	ev := rec.take()
	if len(ev) != 1 || ev[0].Type != ProjectUpdated || ev[0].Revision != p.Revision+1 {
		t.Fatalf("events: %+v", ev)
	}
	if got, _ := s.Project(p.ID); got.Branch != "main" || got.Head != "abc" || *got.Ahead != 1 || *got.Behind != 0 {
		t.Fatalf("project: %+v", got)
	}
	s.Close()

	s2, _ := open(t, dir, h)
	pd := s2.ProjectDirs()
	if len(pd) != 1 || len(pd[0].Dirs) != 2 || pd[0].Dirs[0] != "/w/a" || pd[0].Dirs[1] != "/w/b" {
		t.Fatalf("dirs after restart: %+v", pd)
	}
	got, _ := s2.Project(p.ID)
	if got.Branch != "main" || got.Ahead == nil || *got.Ahead != 1 {
		t.Fatalf("repo state after restart: %+v", got)
	}
	if ch, _ := s2.SetProjectRepo(p.ID, RepoState{}); !ch {
		t.Error("clearing the state reported no change")
	}
	if got, _ := s2.Project(p.ID); got.Branch != "" || got.Ahead != nil {
		t.Fatalf("cleared: %+v", got)
	}
}
