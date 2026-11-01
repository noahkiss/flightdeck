package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestRecentDir: a session filed by project with no cwd starts in the project's most recently
// used folder that still exists, skipping one that is gone; a project with no folders, or none
// that exist, leaves the choice to the caller (the hab's home).
func TestRecentDir(t *testing.T) {
	d := t.TempDir()
	st, err := structure.Open(structure.Config{Dir: filepath.Join(d, "structure"), Holders: structure.NewFakeHolder()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &actions{store: st}
	older, newer := filepath.Join(d, "older"), filepath.Join(d, "newer")
	for _, dir := range []string{older, newer} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p, err := st.CreateProject("p", "example.com/p", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := a.recentDir(p.ID); got != "" {
		t.Errorf("a project with no folders: %q, want none", got)
	}
	for _, dir := range []string{older, newer} {
		if err := st.AddProjectDir(p.ID, dir); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.recentDir(p.ID); got != newer {
		t.Errorf("got %q, want the newest folder %q", got, newer)
	}
	if err := os.Remove(newer); err != nil {
		t.Fatal(err)
	}
	if got := a.recentDir(p.ID); got != older {
		t.Errorf("newest folder gone: got %q, want %q", got, older)
	}
	if err := os.Remove(older); err != nil {
		t.Fatal(err)
	}
	if got := a.recentDir(p.ID); got != "" {
		t.Errorf("every folder gone: got %q, want none", got)
	}
	if got := a.recentDir("no-such-project"); got != "" {
		t.Errorf("an unknown project: %q", got)
	}
}
