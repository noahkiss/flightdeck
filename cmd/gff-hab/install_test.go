package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/noahkiss/flightdeck/internal/service"
)

// TestInstallDefaultSocket: install may tighten the socket folder only when the socket is
// gff-hab's default, so DefaultSocket is true with no -socket and no $GFF_HAB_SOCKET, and false
// with either.
func TestInstallDefaultSocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "run")) // the default socket's home off macOS
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want bool
	}{
		{"default", "", nil, true},
		{"flag", "", []string{"-socket", filepath.Join(dir, "s", "hab.sock")}, false},
		{"environment", filepath.Join(dir, "e", "hab.sock"), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GFF_HAB_SOCKET", tc.env)
			fs := flag.NewFlagSet("install", flag.ContinueOnError)
			sf := addServiceFlags(fs, "")
			args := append([]string{"-state", filepath.Join(dir, "state"), "-holders", filepath.Join(dir, "holders"),
				"-unit-dir", filepath.Join(dir, "units")}, tc.args...)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			c, err := sf.config(service.Systemd)
			if err != nil {
				t.Fatal(err)
			}
			if c.DefaultSocket != tc.want {
				t.Errorf("DefaultSocket = %v, want %v (socket %s)", c.DefaultSocket, tc.want, c.Socket)
			}
		})
	}
}

// TestClientSource: the gff a macOS install copies is -gff-bin (a file, not a folder), else the
// gff beside GFF Hab.app, else none.
func TestClientSource(t *testing.T) {
	dir := t.TempDir()
	macos := filepath.Join(dir, "gff-macos", service.HabBundle, "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o755); err != nil {
		t.Fatal(err)
	}
	hab := filepath.Join(macos, service.HabBinary)
	os.WriteFile(hab, []byte("#!/bin/sh\n"), 0o755)

	if got, err := clientSource("", hab); err != nil || got != "" {
		t.Errorf("no gff: %q %v", got, err)
	}
	beside := filepath.Join(dir, "gff-macos", service.ClientBinary)
	os.WriteFile(beside, []byte("#!/bin/sh\n"), 0o755)
	if got, err := clientSource("", hab); err != nil || got != beside {
		t.Errorf("beside the bundle: %q %v, want %q", got, err, beside)
	}
	if got, err := clientSource("", filepath.Join(dir, "gff-macos", service.HabBundle)); err != nil || got != beside {
		t.Errorf("from the bundle path: %q %v", got, err)
	}
	named := filepath.Join(dir, "other", "gff")
	os.MkdirAll(filepath.Dir(named), 0o755)
	os.WriteFile(named, []byte("#!/bin/sh\n"), 0o755)
	if got, err := clientSource(named, hab); err != nil || got != named {
		t.Errorf("-gff-bin: %q %v", got, err)
	}
	if _, err := clientSource(filepath.Dir(named), hab); err == nil {
		t.Error("-gff-bin naming a folder was accepted")
	}
	if _, err := clientSource(filepath.Join(dir, "missing"), hab); err == nil {
		t.Error("-gff-bin naming nothing was accepted")
	}
}
