package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

func TestLsofName(t *testing.T) {
	if got := lsofName([]byte("p42\nfcwd\nn/work/my dir\n")); got != "/work/my dir" {
		t.Fatalf("lsofName: %q", got)
	}
	if got := lsofName([]byte("p42\n")); got != "" {
		t.Fatalf("no name: %q", got)
	}
}

// TestCreateCwdOf: session.create with cwd_of starts in the folder the other pane's program is
// in, filed in that pane's project; cwd wins over it; an unknown pane falls back to the default.
func TestCreateCwdOf(t *testing.T) {
	e := newHolderEnv(t, 0)
	e.serve()
	c := e.control()
	sub := filepath.Join(e.work, "sub dir")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	// Keep the shell PID stable after publishing readiness. A marker before exec
	// lets macOS lsof race the exec transition and fall back to the original cwd.
	first := e.spawn(c, `cd "$DIR/sub dir" && printf ready > "$DIR/in" && read -r keep_open`)
	waitUntil(t, "the first pane in its folder", func() bool { _, err := os.Stat(filepath.Join(e.work, "in")); return err == nil })
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	start := func(p api.SessionCreateParams, out string) string {
		t.Helper()
		p.Argv = []string{"/bin/sh", "-c", `pwd -P > "$OUT"; exec sleep 600`}
		p.Env = []string{"PATH=/usr/bin:/bin", "OUT=" + out}
		var res api.SessionCreated
		if err := c.Call(ctx, "session.create", p, &res); err != nil {
			t.Fatal(err)
		}
		var b []byte
		waitUntil(t, "pwd in "+out, func() bool { b, _ = os.ReadFile(out); return len(b) > 0 })
		return strings.TrimSpace(string(b))
	}
	want, _ := filepath.EvalSymlinks(sub)
	if got := start(api.SessionCreateParams{CwdOf: first}, filepath.Join(e.work, "a")); got != want {
		t.Fatalf("cwd_of: %q, want %q", got, want)
	}
	if got := start(api.SessionCreateParams{CwdOf: first, Cwd: "/"}, filepath.Join(e.work, "b")); got != "/" {
		t.Fatalf("cwd over cwd_of: %q", got)
	}
	home, _ := os.UserHomeDir()
	home, _ = filepath.EvalSymlinks(home)
	if got := start(api.SessionCreateParams{CwdOf: "00000000-0000-0000-0000-000000000000"}, filepath.Join(e.work, "c")); got != home {
		t.Fatalf("unknown pane: %q, want the home folder %q", got, home)
	}
}
