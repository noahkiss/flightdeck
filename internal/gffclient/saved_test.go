package gffclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// runWS runs gff's workspace like startWorkspaceWith, and returns Run's result channel, so a
// test can detach and start gff again.
func runWS(t *testing.T, hst *host, more func(*Config), specs ...hosts.Spec) <-chan error {
	t.Helper()
	set, err := hosts.Open(hosts.Config{
		Source: hosts.Static(specs),
		Client: api.ClientInfo{Name: "gff", Version: "test", Kind: api.ClientKindTerminal},
		Conn:   func(c *habconn.Config) { c.MinBackoff, c.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	timer := &fakeTimer{c: make(chan time.Time)}
	cfg := Config{Hosts: set, Parser: vt.Fake{}, In: hst.inR, Out: hst, Size: hst.size,
		Resized: hst.resized, NewTimer: func() Timer { return timer }, Logf: t.Logf}
	if more != nil {
		more(&cfg)
	}
	out := make(chan error, 1)
	go func() {
		err := Run(ctx, cfg)
		set.Close()
		done <- err
		out <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(waitLong()):
			t.Error("Run did not return")
		}
	})
	return out
}

func detach(t *testing.T, hst *host, done <-chan error) {
	t.Helper()
	hst.typ(cmdKey + "d")
	select {
	case err := <-done:
		if !errors.Is(err, ErrDetached) {
			t.Fatalf("Run: %v, want detached", err)
		}
	case <-time.After(waitLong()):
		t.Fatal("gff did not detach")
	}
}

// TestWorkspaceSaved: tabs, their names, a split, the views and the focus come back after gff
// restarts. A saved view whose program exited shows exited and offers Resume; one on a hab that
// is down shows offline.
func TestWorkspaceSaved(t *testing.T) {
	d := shortDir(t)
	a, b := fakeHab(t, d, "a"), fakeHab(t, d, "b")
	_, jobPane, _ := a.AddSession("job")
	b.AddSession("other")
	state := filepath.Join(d, "ws", "default.json")
	withState := func(c *Config) { c.StatePath, c.Workspace = state, "default" }
	hst := newHost(t, 160, 30, false)
	done := runWS(t, hst, withState, specsOf(a.Sock, b.Sock)...)
	hst.waitScreen("both rows", func(s string) bool { return line(s, "job") > 0 && line(s, "other") > 0 })
	hst.typ("\r") // job: first by name
	hst.waitScreen("job in tab 1", func(s string) bool { return strings.Contains(s, " job · one") })
	hst.typ("\x10l")
	hst.waitScreen("a new pane beside job", func(s string) bool {
		jr, jc := pos(s, " job · one")
		sr, sc := pos(s, " sh · one")
		return jr >= 0 && sr == jr && sc > jc
	})
	hst.typ("\x1bh") // focus back on job
	hst.typ(tabKey + "r\x7fwork\r")
	hst.waitScreen("the name", func(s string) bool { return strings.Contains(s, " 1:work ") })
	hst.typ(tabKey + "n")
	hst.waitScreen("tab 2", func(s string) bool { return strings.Contains(s, " 2:2 ") })
	hst.waitScreen("other in the sidebar", func(s string) bool { return line(s, "other") > 0 })
	hst.typ("/other\r\r") // filter to other, then open it
	hst.waitScreen("other in tab 2", func(s string) bool { return strings.Contains(s, " other · two") })
	hst.typ(tabKey + "1")
	hst.waitScreen("tab 1 again", func(s string) bool { return strings.Contains(s, " job · one") })
	waitUntil(t, "the workspace file", func() bool {
		b, _ := os.ReadFile(state)
		return strings.Contains(string(b), `"work"`) && strings.Contains(string(b), jobPane) && strings.Contains(string(b), `"two"`)
	})
	detach(t, hst, done)

	// Start again: the same tabs, views and focus.
	hst2 := newHost(t, 160, 30, false)
	done = runWS(t, hst2, withState, specsOf(a.Sock, b.Sock)...)
	hst2.waitScreen("tab 1 back with both views", func(s string) bool {
		jr, jc := pos(s, " job · one")
		sr, sc := pos(s, " sh · one")
		return strings.Contains(s, " 1:work ") && strings.Contains(s, " 2:2 ") && jr >= 0 && sr == jr && sc > jc
	})
	hst2.typ("\x10x") // close the focused view: job, if the focus came back
	hst2.waitScreen("job's view closed, sh's left", func(s string) bool {
		return !strings.Contains(s, " job · one") && strings.Contains(s, " sh · one")
	})
	hst2.typ(tabKey + "2")
	hst2.waitScreen("tab 2 back", func(s string) bool { return strings.Contains(s, " other · two") })
	detach(t, hst2, done)

	// job's program exits and hab b goes down while gff is not running.
	a.ExitPane(jobPane)
	hst3 := newHost(t, 160, 30, false)
	done = runWS(t, hst3, withState, specsOf(a.Sock, b.Sock)...)
	hst3.waitScreen("tab 2 is current", func(s string) bool { return strings.Contains(s, " other · two") })
	b.Stop()
	detach(t, hst3, done)
	hst4 := newHost(t, 160, 30, false)
	done = runWS(t, hst4, withState, specsOf(a.Sock, b.Sock)...)
	hst4.waitScreen("the view on the down hab offline", func(s string) bool { return strings.Contains(s, " other · two · offline") })
	_ = done
}

// TestWorkspaceSavedExited: a saved view whose program exited while gff was not running shows
// exited, with Resume.
func TestWorkspaceSavedExited(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	_, jobPane, _ := a.AddSession("job")
	state := filepath.Join(d, "ws.json")
	withState := func(c *Config) { c.StatePath = state }
	hst := newHost(t, 120, 30, false)
	done := runWS(t, hst, withState, specsOf(a.Sock)...)
	hst.waitScreen("the row", func(s string) bool { return line(s, "job") > 0 })
	hst.typ("\r")
	hst.waitScreen("job in a view", func(s string) bool { return strings.Contains(s, " job · one") })
	waitUntil(t, "the workspace file", func() bool { b, _ := os.ReadFile(state); return strings.Contains(string(b), jobPane) })
	detach(t, hst, done)
	a.ExitPane(jobPane)
	hst2 := newHost(t, 120, 30, false)
	runWS(t, hst2, withState, specsOf(a.Sock)...)
	hst2.waitScreen("the view exited, with Resume", func(s string) bool { return strings.Contains(s, " job · one · exited 0 · Enter resume") })
}

// TestStartupTabs: a new workspace starts with the config's startup tabs, each with its program
// started in its folder; an existing workspace does not get them again.
func TestStartupTabs(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	state := filepath.Join(d, "ws.json")
	withTabs := func(c *Config) {
		c.StatePath = state
		c.StartupTabs = []StartupTab{{Name: "code", Dir: "/src/app"}, {Name: "notes"}, {Name: "build", Dir: "/src/app", Command: "make watch"}}
	}
	hst := newHost(t, 140, 30, false)
	done := runWS(t, hst, withTabs, specsOf(a.Sock)...)
	hst.waitScreen("the startup tabs", func(s string) bool {
		return strings.Contains(s, " 1:code ") && strings.Contains(s, " 2:notes ") && strings.Contains(s, " 3:build ") &&
			strings.Contains(s, " · one")
	})
	waitUntil(t, "both creates", func() bool { return len(a.Creates()) == 2 })
	var shell, watch int
	for _, cr := range a.Creates() {
		switch {
		case cr.Cwd == "/src/app" && len(cr.Argv) == 0:
			shell++
		case cr.Cwd == "/src/app" && strings.Join(cr.Argv, " ") == "/bin/sh -c make watch":
			watch++
		}
	}
	if shell != 1 || watch != 1 {
		t.Fatalf("creates %+v", a.Creates())
	}
	hst.typ(tabKey + "2")
	hst.waitScreen("notes is empty", func(s string) bool { return strings.Contains(s, "empty tab") })
	waitUntil(t, "the workspace file", func() bool { b, _ := os.ReadFile(state); return strings.Contains(string(b), `"build"`) })
	detach(t, hst, done)

	hst2 := newHost(t, 140, 30, false)
	runWS(t, hst2, withTabs, specsOf(a.Sock)...)
	hst2.waitScreen("the saved tabs", func(s string) bool { return strings.Contains(s, " 1:code ") && strings.Contains(s, " 3:build ") })
	time.Sleep(200 * time.Millisecond)
	if n := len(a.Creates()); n != 2 {
		t.Fatalf("%d creates: a saved workspace gets no startup tabs again", n)
	}
}

// TestWorkspaceFile: the path per system; a file that does not parse is set aside and the
// workspace starts new; views of a host no longer configured are left out, and a split left
// with one view gives way to it.
func TestWorkspaceFile(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := func() (string, error) { return "/home/u", nil }
	for _, c := range []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"darwin", nil, "/home/u/Library/Application Support/gff/workspaces/w.json"},
		{"linux", nil, "/home/u/.local/state/gff/workspaces/w.json"},
		{"linux", map[string]string{"XDG_STATE_HOME": "/s"}, "/s/gff/workspaces/w.json"},
		{"darwin", map[string]string{"GFF_WORKSPACE_DIR": "/x"}, "/x/w.json"},
	} {
		if got, err := workspacePath("w", c.goos, env(c.env), home); err != nil || got != c.want {
			t.Errorf("%s %v: %q %v, want %q", c.goos, c.env, got, err, c.want)
		}
	}
	if _, err := workspacePath("../w", "linux", env(nil), home); err == nil {
		t.Error("a bad name made a path")
	}

	d := t.TempDir()
	path := filepath.Join(d, "w.json")
	c := testClient(t, 80, 24)
	c.ws, c.cfg.StatePath = true, path
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if existed, writable, err := c.restoreWorkspace(); existed || !writable || err == nil {
		t.Fatalf("bad file: %v %v %v", existed, writable, err)
	}
	if _, err := os.Stat(path + ".bad"); err != nil {
		t.Fatalf("not set aside: %v", err)
	}
	saved := `{"version":1,"current":0,"tabs":[{"name":"t","split":"columns","focus":0,"layout":{"split":"rows","kids":[
		{"view":{"host":"gone","pane":"p1"}},{"view":{"host":"hab","pane":"p2","session":"s2","label":"two"}}]}}]}`
	if err := os.WriteFile(path, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ctx, c.wg = ctx, &sync.WaitGroup{}
	if existed, writable, err := c.restoreWorkspace(); !existed || !writable || err != nil {
		t.Fatalf("restore: %v %v %v", existed, writable, err)
	}
	tb := c.tabs[0]
	if tb.name != "t" || shape(tb) != "p2" || tb.views[0].spec.Session != "s2" || tb.views[0].spec.Label != "two" {
		t.Fatalf("restored %q %s %+v", tb.name, shape(tb), tb.views[0].spec)
	}
	cancel()
	c.wg.Wait()
}

// TestStartupTabsConfig: [[workspace.tabs]] with each key; a relative dir and an unknown key are
// problems on their tab.
func TestStartupTabsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := `[workspace]
name = "w"

[[workspace.tabs]]
name = "code"
host = "laptop"
dir = "/src"
command = "make watch"

[[workspace.tabs]]
name = "rel"
dir = "src"

[[workspace.tabs]]
name = "odd"
colour = "red"
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	s, problems, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []StartupTab{{Name: "code", Host: "laptop", Dir: "/src", Command: "make watch"}, {Name: "odd"}}
	if len(s.Workspace.Tabs) != 2 || s.Workspace.Tabs[0] != want[0] || s.Workspace.Tabs[1] != want[1] {
		t.Fatalf("tabs %+v", s.Workspace.Tabs)
	}
	if len(problems) != 2 || !strings.Contains(problems[0], "[[workspace.tabs]] 2: dir \"src\": want an absolute path") ||
		!strings.Contains(problems[1], "[[workspace.tabs]] 3: colour: unknown key") {
		t.Fatalf("problems %q", problems)
	}
}
