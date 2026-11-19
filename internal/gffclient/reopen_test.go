package gffclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// build makes a tab from steps of (view, anchor, place) over the views in v.
func build(t *testing.T, v map[string]*view, steps ...any) *tab {
	t.Helper()
	tb := &tab{split: SplitColumns}
	for i := 0; i < len(steps); i += 3 {
		var anchor *view
		if a := steps[i+1].(string); a != "" {
			anchor = v[a]
		}
		tb.put(v[steps[i].(string)], anchor, steps[i+2].(place))
	}
	return tb
}

// TestLayoutReopen: a view taken out of the tree goes back where it was (gff-client.md 5.8).
func TestLayoutReopen(t *testing.T) {
	v := views("a", "b", "c", "d", "e")
	for _, tc := range []struct {
		name  string
		tab   func() *tab
		close string
		then  func(tb *tab) // a change while the view is closed
		want  string
	}{
		{"split middle", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeRight, "c", "b", placeRight)
		}, "b", nil, "cols(a,b,c)"},
		{"split first", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeRight, "c", "b", placeRight)
		}, "a", nil, "cols(a,b,c)"},
		{"rows last", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeDown, "c", "b", placeDown)
		}, "c", nil, "rows(a,b,c)"},
		{"the only split folds", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeDown)
		}, "a", nil, "rows(a,b)"},
		{"stack first", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeStack, "c", "b", placeStack)
		}, "a", nil, "stack(*a,b,c)"},
		{"stack middle", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeStack, "c", "b", placeStack)
		}, "b", nil, "stack(a,*b,c)"},
		{"a stack of two folds", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeStack, "c", "b", placeRight)
		}, "a", nil, "cols(stack(*a,b),c)"},
		{"beside a split subtree", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeRight, "c", "b", placeDown)
		}, "a", nil, "cols(a,rows(b,c))"},
		{"the neighbour left: beside a view that was in it", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeRight, "c", "b", placeDown)
		}, "a", func(tb *tab) {
			// rows(b,c) loses c and folds away; b, the view next to a, stays.
			tb.take(v["c"])
		}, "cols(a,b)"},
		{"nothing left: the end", func() *tab {
			return build(t, v, "a", "", placeEnd, "b", "a", placeRight)
		}, "a", func(tb *tab) {
			tb.take(v["b"])
			tb.put(v["e"], nil, placeEnd)
		}, "e"},
	} {
		tb := tc.tab()
		at := tb.spotOf(v[tc.close])
		tb.take(v[tc.close])
		if tc.then != nil {
			tc.then(tb)
		}
		tb.putBack(v[tc.close], at)
		got := shape(tb)
		if tc.name == "nothing left: the end" {
			tc.want = "cols(e," + tc.close + ")"
		}
		if strings.ReplaceAll(got, "*", "") != strings.ReplaceAll(tc.want, "*", "") || strings.Contains(tc.want, "*") && got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestReopenView: pane mode's u brings back the view closed last, in its place; a closed tab's
// views come back one at a time; a session that exited meanwhile reopens exited; a closed
// session is skipped; with none left the bar says so; the list keeps ten (gff-client.md 5.8).
func TestReopenView(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	_, _, _ = a.AddSession("job")
	hst := newHost(t, 200, 50, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("the running row", func(s string) bool { return line(s, "   job") > 0 })
	hst.typ("\r")
	hst.waitScreen("job in a view", func(s string) bool { return strings.Contains(s, " job · one") })
	hst.typ("\x10")
	hst.waitScreen("pane mode's bar", func(s string) bool { return strings.Contains(s, "u reopen") })
	hst.typ("l")
	hst.waitScreen("sh beside job", func(s string) bool { return strings.Contains(s, " sh · one") })
	hst.typ("\x10l")
	hst.waitScreen("sh-2 beside sh", func(s string) bool { return strings.Contains(s, " sh-2 · one") })
	creates := len(a.Creates())
	_, shCol := pos(hst.screen(), " sh · one")
	_, sh2Col := pos(hst.screen(), " sh-2 · one")

	// Close sh, the middle view, with sh-2 focused first: focus it with Alt+h.
	hst.typ("\x1bh")
	hst.typ("\x10x")
	hst.waitScreen("sh closed", func(s string) bool { return !strings.Contains(s, " sh · one") && strings.Contains(s, " sh-2 · one") })
	hst.typ("\x10u")
	hst.waitScreen("sh back between job and sh-2", func(s string) bool {
		_, jc := pos(s, " job · one")
		_, sc := pos(s, " sh · one")
		_, s2c := pos(s, " sh-2 · one")
		return jc >= 0 && sc > jc && s2c > sc && sc == shCol && s2c == sh2Col
	})
	if n := len(a.Creates()); n != creates {
		t.Fatalf("reopen made %d session.create calls", n-creates)
	}
	// The reopened view has the focus: close view closes it.
	hst.typ("\x10x")
	hst.waitScreen("the focused view closed again", func(s string) bool { return !strings.Contains(s, " sh · one") })

	// Its session exits while it is closed: it reopens exited, with Resume and Shell.
	for _, ss := range a.Sessions() {
		if ss.Name == "sh" {
			a.ExitPane(ss.Pane)
		}
	}
	waitUntil(t, "sh exited", func() bool {
		for _, ss := range a.Sessions() {
			if ss.Name == "sh" {
				return ss.State == api.SessionStateExited
			}
		}
		return false
	})
	hst.typ("\x10u")
	hst.waitScreen("sh back, exited", func(s string) bool { return strings.Contains(s, " sh · one · exited 0 · Enter resume · Esc shell") })

	// A closed session is skipped: close sh-2's view, then its session; u skips it and says
	// nothing is left.
	var sh2 string
	for _, ss := range a.Sessions() {
		if ss.Name == "sh-2" {
			sh2 = ss.ID
		}
	}
	hst.typ("\x1bl") // sh-2
	hst.typ("\x10x")
	hst.waitScreen("sh-2 closed", func(s string) bool { return !strings.Contains(s, " sh-2 · one") })
	c, err := control.Dial(a.Sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), waitLong())
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "other", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "session.close", api.SessionRef{Session: sh2}, &api.Empty{}); err != nil {
		t.Fatal(err)
	}
	hst.waitScreen("sh-2 gone from the sidebar", func(s string) bool { return !strings.Contains(s, "sh-2") })
	hst.typ("\x10u")
	hst.waitScreen("nothing to reopen", func(s string) bool { return strings.Contains(s, "nothing to reopen") })

	// A closed tab's views come back one by one into the current tab, the last first.
	hst.typ("\x14n") // a new tab: the sidebar has the keys
	hst.waitScreen("a new tab", func(s string) bool { return strings.Contains(s, "2:2") })
	hst.typ("\x14" + "1")
	hst.waitScreen("back on tab 1", func(s string) bool { return strings.Contains(s, " job · one") })
	hst.typ("\x14x")
	hst.waitScreen("tab 1 closed", func(s string) bool { return !strings.Contains(s, " job · one") })
	hst.typ("\x10u")
	hst.waitScreen("the last view of the closed tab", func(s string) bool { return strings.Contains(s, " sh · one") && !strings.Contains(s, " job · one") })
	hst.typ("\x10u")
	hst.waitScreen("then the first", func(s string) bool { return strings.Contains(s, " sh · one") && strings.Contains(s, " job · one") })
}

// TestReopenKeepsTen: the list of closed views keeps the newest ten.
func TestReopenKeepsTen(t *testing.T) {
	u := &workspaceUI{c: &client{}}
	tb := &tab{split: SplitColumns}
	for i := 0; i < 12; i++ {
		v := &view{spec: PaneSpec{ID: string(rune('a' + i))}, h: &hostState{label: "one"}, quit: func() {}}
		tb.put(v, nil, placeEnd)
		u.closeView(tb, v)
	}
	if len(u.closed) != maxClosed || u.closed[0].spec.ID != "c" || u.closed[9].spec.ID != "l" {
		t.Fatalf("closed list %d long, first %s", len(u.closed), u.closed[0].spec.ID)
	}
}

// TestShellAfterExit: an exited view offers a shell; Esc starts the hab's shell in the exited
// pane's folder in the same view; the exited session stays in the sidebar; [keys.exited]
// moves the key and its hints (gff-client.md 5.8).
func TestShellAfterExit(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	_, pane, _ := a.AddSession("job")
	a.AddSession("other")
	hst := newHost(t, 160, 40, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("the running row", func(s string) bool { return line(s, "   job") > 0 })
	hst.typ("\r")
	hst.waitScreen("job in a view", func(s string) bool { return strings.Contains(s, " job · one") })
	a.ExitPane(pane)
	hst.waitScreen("the exited frame and the bar", func(s string) bool {
		return strings.Contains(s, " job · one · exited 0 · Enter resume · Esc shell") &&
			strings.Contains(s, "Enter resume  Esc shell  Ctrl+G lock")
	})
	hst.typ("ls")
	time.Sleep(100 * time.Millisecond)
	if n := len(a.Creates()); n != 0 {
		t.Fatalf("%d creates after typing", n)
	}
	hst.typ("\x1b")
	waitUntil(t, "the shell's create", func() bool { return len(a.Creates()) == 1 })
	if cr := a.Creates()[0]; cr.CwdOf != pane || len(cr.Argv) != 0 || cr.Cols <= 80 || cr.Rows <= 24 {
		t.Fatalf("create %+v, want the hab's shell with cwd_of %s at the view's size", cr, pane)
	}
	hst.waitScreen("the shell in the view, job exited in the sidebar", func(s string) bool {
		return strings.Contains(s, " sh · one") && !strings.Contains(s, "Esc shell") && line(s, "job") > 0
	})
	if n := a.Resumes(); n != 0 {
		t.Fatalf("%d resumes", n)
	}
}

// TestShellKeyRebound: [keys.exited] moves the shell key; the frame and bar follow.
func TestShellKeyRebound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[keys.exited]\nesc = \"none\"\ns = \"shell\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, problems, err := LoadSettings(path)
	if err != nil || len(problems) > 0 {
		t.Fatal(problems, err)
	}
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	_, pane, _ := a.AddSession("job")
	hst := newHost(t, 160, 40, false)
	startWorkspaceWith(t, hst, func(c *Config) { c.Keymap = settings.Keymap; c.Bar = &settings.Bar }, specsOf(a.Sock)...)
	hst.waitScreen("the running row", func(s string) bool { return line(s, "   job") > 0 })
	hst.typ("\r")
	hst.waitScreen("job in a view", func(s string) bool { return strings.Contains(s, " job · one") })
	a.ExitPane(pane)
	hst.waitScreen("the moved hint", func(s string) bool { return strings.Contains(s, " job · one · exited 0 · Enter resume · s shell") })
	hst.typ("\x1b")
	time.Sleep(100 * time.Millisecond)
	if n := len(a.Creates()); n != 0 {
		t.Fatalf("Esc started %d shells after its unbind", n)
	}
	hst.typ("s")
	waitUntil(t, "the shell's create", func() bool { return len(a.Creates()) == 1 })
}

// TestExitedKeymapConfig: [keys.exited] takes plain keys and only resume, shell and none;
// resume and shell work nowhere else.
func TestExitedKeymapConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[keys.exited]\nq = \"resume\"\nz = \"zoom\"\n[keys.pane]\ne = \"shell\"\n[keys.normal]\n\"ctrl-e\" = \"resume\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, problems, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	// In the order of the modes: normal, pane, exited.
	want := []string{`:7: [keys.normal] ctrl-e: action "resume" does not work in normal mode`,
		`:5: [keys.pane] e: action "shell" does not work in pane mode`, `:3: [keys.exited] z: action "zoom" does not work in exited mode`}
	if len(problems) != len(want) {
		t.Fatalf("problems %q", problems)
	}
	for i, w := range want {
		if !strings.HasSuffix(problems[i], w) {
			t.Errorf("problem %d: %q, want the end %q", i, problems[i], w)
		}
	}
	if got := barHints(s.Keymap, ModeExited); got != "Enter resume  Esc shell  q resume" && got != "Enter/q resume  Esc shell" {
		t.Errorf("exited hints %q", got)
	}
}

// TestShellAfterExitInFolder: end to end with the real holder and gff-hab. A command started in
// a folder exits 0 in a view; Esc gives a shell in the same view, in that folder.
func TestShellAfterExitInFolder(t *testing.T) {
	d := shortDir(t)
	hdir, state, sock, work := filepath.Join(d, "h"), filepath.Join(d, "s"), filepath.Join(d, "run", "hab.sock"), filepath.Join(d, "w")
	dir := filepath.Join(work, "proj-y")
	for _, p := range []string{hdir, dir} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	startHolderProc(t, hdir, "one")
	startServeProc(t, sock, state, hdir, append(os.Environ(), "SHELL=/bin/sh", "ENV=", "PS1=$ "))
	h := &hab{t: t, sock: sock}
	other := h.other()
	var res api.SessionCreated
	call(t, other, "session.create", api.SessionCreateParams{Name: "build",
		Argv: []string{"/bin/sh", "-c", "echo build-ready; read -r x; echo build-done; exit 0"}, Cwd: dir, Cols: 80, Rows: 24}, &res)
	hst := newHost(t, 160, 40, false)
	startWorkspace(t, hst, sock)
	hst.waitScreen("the session", func(s string) bool { return line(s, " build") > 0 })
	hst.typ("\r")
	hst.waitScreen("it in a view", func(s string) bool { return strings.Contains(s, "build-ready") })
	var empty api.Empty
	call(t, other, "pane.input", api.InputParams{Pane: res.Pane, Data: []byte("\n")}, &empty)
	hst.waitScreen("the exited frame", func(s string) bool {
		return strings.Contains(s, " build · one · exited 0 · Enter resume · Esc shell")
	})
	hst.typ("\x1b")
	hst.waitScreen("the shell's prompt in the view", func(s string) bool {
		if !strings.Contains(s, " sh · one") {
			return false
		}
		for _, l := range strings.Split(s, "\n") {
			if strings.HasSuffix(strings.TrimRight(l, " "), "$") {
				return true
			}
		}
		return false
	})
	hst.typ("pwd -P\r")
	want, _ := filepath.EvalSymlinks(dir)
	hst.waitScreen("pwd in the command's folder", func(s string) bool { return strings.Contains(s, want) })
}
