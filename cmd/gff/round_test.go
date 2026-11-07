package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestFindHab: gff round finds gff-hab beside gff, then in GFF Hab.app beside it, then beside
// the file a link to gff names, then on PATH; a file that does not run is skipped.
func TestFindHab(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(p string, mode os.FileMode) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	onPath := func(string) (string, error) { return "/path/gff-hab", nil }
	noPath := func(string) (string, error) { return "", errors.New("not found") }

	libexec := filepath.Join(dir, "libexec")
	gff := mk(filepath.Join(libexec, "gff"), 0o755)
	if got, _ := findHab(gff, onPath); got != "/path/gff-hab" {
		t.Errorf("nothing beside gff: %s", got)
	}
	if _, err := findHab(gff, noPath); err == nil {
		t.Error("no gff-hab anywhere: no error")
	}
	bundled := mk(filepath.Join(libexec, "GFF Hab.app", "Contents", "MacOS", "gff-hab"), 0o755)
	if got, _ := findHab(gff, noPath); got != bundled {
		t.Errorf("bundle: %s", got)
	}
	plain := mk(filepath.Join(libexec, "gff-hab"), 0o644)
	if got, _ := findHab(gff, noPath); got != bundled {
		t.Errorf("a gff-hab that does not run was taken: %s", got)
	}
	os.Chmod(plain, 0o755)
	if got, _ := findHab(gff, noPath); got != plain {
		t.Errorf("beside gff: %s", got)
	}
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	link := filepath.Join(bin, "gff")
	if err := os.Symlink(gff, link); err != nil {
		t.Fatal(err)
	}
	if got, _ := findHab(link, noPath); got != plain {
		t.Errorf("through a link: %s", got)
	}
}
