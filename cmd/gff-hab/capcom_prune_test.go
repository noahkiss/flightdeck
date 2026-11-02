package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// TestCapcomPrune: at start, capcom.json drops the records whose session is gone, and the
// standing pointer with them. When the structure state did not load (a file from a newer build),
// the store is empty, so nothing is pruned and capcom.json is left as it was.
func TestCapcomPrune(t *testing.T) {
	saved := capcom.State{Assignment: 2, Home: "dev-a", Standing: "s-gone",
		Sessions: []capcom.Session{{ID: "s-gone", Dir: "/nowhere", Harness: capcom.Claude}}}
	open := func(t *testing.T, dir string) *structure.Store {
		t.Helper()
		st, err := structure.Open(structure.Config{Dir: dir, Holders: structure.NewFakeHolder()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		return st
	}
	newSvc := func(dir string, store *structure.Store) *capcomService {
		return newCapcomService(&capcomService{self: "dev-a", state: dir, exe: "/opt/gff-hab", store: store})
	}

	t.Run("good store", func(t *testing.T) {
		dir := t.TempDir()
		if err := capcom.SaveState(dir, saved); err != nil {
			t.Fatal(err)
		}
		c := newSvc(dir, open(t, dir))
		got, err := capcom.LoadState(dir)
		if err != nil || len(got.Sessions) != 0 || got.Standing != "" || got.Assignment != 2 || len(c.st.Sessions) != 0 {
			t.Fatalf("after the prune: %+v, %v", got, err)
		}
	})

	t.Run("store from a newer build", func(t *testing.T) {
		dir := t.TempDir()
		if err := capcom.SaveState(dir, saved); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, structure.StateFile), []byte(`{"schema":99,"projects":[],"sessions":[],"panes":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		store := open(t, dir)
		if store.Health().Problem == "" {
			t.Fatal("the newer state file loaded")
		}
		newSvc(dir, store)
		got, err := capcom.LoadState(dir)
		if err != nil || len(got.Sessions) != 1 || got.Standing != "s-gone" {
			t.Fatalf("capcom.json after a start with an empty store: %+v, %v", got, err)
		}
	})
}
