package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good", "**\n!docs/**\ndocs/examples/**\n")
	empty := write("empty", "\n  \n")
	bad := write("bad", "**\n!\n")
	in := "cmd/a.go\ndocs/a.md\ndocs/examples/b.toml\n\n"
	for _, c := range []struct {
		name string
		args []string
		code int
		out  string
	}{
		{"match", []string{"-patterns", good}, 0, "cmd/a.go\ndocs/examples/b.toml\n"},
		{"no patterns flag", nil, 64, ""},
		{"extra argument", []string{"-patterns", good, "x"}, 64, ""},
		{"unknown flag", []string{"-x"}, 64, ""},
		{"missing file", []string{"-patterns", filepath.Join(dir, "none")}, 1, ""},
		{"empty file", []string{"-patterns", empty}, 1, ""},
		{"bad pattern", []string{"-patterns", bad}, 1, ""},
	} {
		var out, errb bytes.Buffer
		code := run(c.args, strings.NewReader(in), &out, &errb)
		if code != c.code || out.String() != c.out {
			t.Errorf("%s: code %d out %q, want %d %q (stderr %q)", c.name, code, out.String(), c.code, c.out, errb.String())
		}
	}
}
