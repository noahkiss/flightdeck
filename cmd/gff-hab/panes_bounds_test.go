package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// The bounds and refusals of reading panes (hab-logic-api.md 3.11): the near hab's rule on a
// forward, another hab's text treated as untrusted, the wait bounds and lease, the cost of a
// long open line, and no existence oracle. The Capcom route is in panecapcom_test.go.

// paneErrKind returns the error kind of err, "" for none.
func paneErrKind(err error) api.ErrorKind {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Data.Kind
	}
	if err != nil {
		return "other: " + api.ErrorKind(err.Error())
	}
	return ""
}

// projectSender files a session in the project at repo that saves its GFF_HAB, and returns it.
func projectSender(t *testing.T, e *habEnv, name, repo string) string {
	t.Helper()
	createSession(t, e.sock, api.SessionCreateParams{Name: name, Argv: []string{"/bin/sh", "-c", fakeSender},
		ProjectDir: repo, Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work}})
	addr := filepath.Join(e.work, "addr")
	waitUntil(t, name+"'s GFF_HAB", func() bool { b, err := os.ReadFile(addr); return err == nil && len(b) > 0 })
	b, _ := os.ReadFile(addr)
	return string(b)
}

// TestPaneReadProject: under project, an agent reads its own project's panes, by name and by
// pane id, and is refused another project's; under off and project an unknown session is
// denied as well, so a refused agent cannot tell which names exist (F5).
func TestPaneReadProject(t *testing.T) {
	t.Parallel()
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	d := shortDir(t)
	r1, r2 := gitRepo(t, d, "one"), gitRepo(t, d, "two")
	addr := projectSender(t, e, "alice", r1)
	mate := createSession(t, e.sock, api.SessionCreateParams{Name: "mate", Argv: []string{"/bin/sh", "-c", "printf 'mate text\\n'; exec sleep 1000"},
		ProjectDir: r1, Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}})
	stranger := createSession(t, e.sock, api.SessionCreateParams{Name: "stranger", Argv: []string{"/bin/sh", "-c", "exec sleep 1000"},
		ProjectDir: r2, Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}})
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	read := func(p api.PaneReadParams) (api.PaneText, error) {
		var r api.PaneText
		err := hook.Call(ctx, addr, "pane.read", p, &r)
		return r, err
	}
	// all: an unknown session is not_found, as for the owner.
	if _, err := read(api.PaneReadParams{Session: "ghost"}); paneErrKind(err) != api.ErrorKindNotFound {
		t.Fatalf("all, ghost: %v", err)
	}
	setReadPanes(t, e.sock, readPanesProject)
	waitUntil(t, "mate's text", func() bool {
		r, err := read(api.PaneReadParams{Session: "mate"})
		return err == nil && strings.Contains(strings.Join(r.Screen, "\n"), "mate text")
	})
	if r, err := read(api.PaneReadParams{Pane: mate.Pane}); err != nil || r.Session != "mate" {
		t.Fatalf("project, mate by pane id: %v %+v", err, r)
	}
	for _, p := range []api.PaneReadParams{{Session: "stranger"}, {Pane: stranger.Pane}, {Session: "ghost"},
		{Pane: "00000000-0000-0000-0000-0000000000ff"}} {
		if _, err := read(p); paneErrKind(err) != api.ErrorKindDenied {
			t.Fatalf("project, %+v: %v, want denied", p, err)
		}
	}
	setReadPanes(t, e.sock, readPanesOff)
	for _, p := range []api.PaneReadParams{{Session: "mate"}, {Session: "ghost"}} {
		if _, err := read(p); paneErrKind(err) != api.ErrorKindDenied {
			t.Fatalf("off, %+v: %v, want denied", p, err)
		}
	}
}

// TestPaneReadForwardRule: the near hab's own rule gates a forward from the pane route (F1); a
// call over a peer link naming a label is refused, a forward never forwards; a peer's door
// learns nothing of a target it may not read (F5).
func TestPaneReadForwardRule(t *testing.T) {
	t.Parallel()
	p := newMeshPair(t)
	createSession(t, p.b.sock, api.SessionCreateParams{Name: "far", Argv: []string{"/bin/sh", "-c", "printf 'peer text\\n'; exec sleep 1000"},
		Cwd: "/", Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}})
	addr := startSender(t, p.a)
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(30*time.Second))
	defer cancel()
	var r api.PaneText
	waitUntil(t, "a route read of bee/far under all", func() bool {
		return hook.Call(ctx, addr, "pane.read", api.PaneReadParams{Session: "bee/far"}, &r) == nil
	})
	if r.Session != "bee/far" {
		t.Fatalf("session %q", r.Session)
	}
	setReadPanes(t, p.a.sock, readPanesOff) // near off, far all
	if err := hook.Call(ctx, addr, "pane.read", api.PaneReadParams{Session: "bee/far"}, &r); paneErrKind(err) != api.ErrorKindDenied {
		t.Fatalf("near off, route read of bee/far: %v, want denied", err)
	}
	var w api.PaneWaitResult
	if err := hook.Call(ctx, addr, "pane.wait", api.PaneWaitParams{Session: "bee/far", QuietMS: 10}, &w); paneErrKind(err) != api.ErrorKindDenied {
		t.Fatalf("near off, route wait on bee/far: %v, want denied", err)
	}
	if _, code := runCode(t, []string{"GFF_HAB=" + addr}, "screen", "bee/far"); code != 3 {
		t.Fatalf("near off, gff-hab screen from the pane: exit %d", code)
	}
	// The peer door, as b's link to a would call: a foreign label is refused.
	c, err := control.Dial(p.a.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "peer", Version: "0", Kind: api.ClientKindHab}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "pane.read", api.PaneReadParams{Session: "bee/far"}, &r); paneErrKind(err) != api.ErrorKindInvalid ||
		!strings.Contains(err.Error(), "never forwards") {
		t.Fatalf("peer door, foreign label: %v", err)
	}
	// Under a value other than all the peer door is denied alike for a session that exists and
	// for one that does not.
	for _, s := range []string{"alice", "ghost"} {
		if err := c.Call(ctx, "pane.read", api.PaneReadParams{Session: s}, &r); paneErrKind(err) != api.ErrorKindDenied {
			t.Fatalf("peer door, near off, %s: %v, want denied", s, err)
		}
	}
}

// TestPaneWaitBounds: a pane route holds at most 16 waits at once; a 17th answers unavailable,
// and a wait that ends gives its slot back (F3).
func TestPaneWaitBounds(t *testing.T) {
	t.Parallel()
	e := newHabEnv(t, 0)
	t.Cleanup(func() { writeFile(t, filepath.Join(e.work, "end"), "") })
	addr := startSender(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	refused := make(chan struct{}, paneWaitsPerCaller+1)
	for range paneWaitsPerCaller + 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var w api.PaneWaitResult
			err := hook.Call(ctx, addr, "pane.wait", api.PaneWaitParams{Session: "alice", Match: "never printed", TimeoutMS: 60000}, &w)
			if paneErrKind(err) == api.ErrorKindUnavailable && strings.Contains(err.Error(), "too many pane waits") {
				refused <- struct{}{}
			}
		}()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
	short, done := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer done()
	select {
	case <-refused:
	case <-short.Done():
		t.Fatal("17 waits from one pane, none refused")
	}
	var w api.PaneWaitResult
	cancel()
	wg.Wait()
	waitUntil(t, "a wait taken again", func() bool {
		return hook.Call(short, addr, "pane.wait", api.PaneWaitParams{Session: "alice", QuietMS: 1}, &w) == nil
	})
}

// TestPaneWaitLease: a forwarded wait ends on the far hab soon after its near caller goes away
// (F3). The far hab holds 16 waits per peer link; with the near callers gone, the far slots come
// free within a lease, long before the waits' own time.
func TestPaneWaitLease(t *testing.T) {
	t.Parallel()
	p := newMeshPairEnv(t, "", "GFF_TEST_PANE_LEASE=2s")
	createSession(t, p.b.sock, api.SessionCreateParams{Name: "far", Argv: []string{"/bin/sh", "-c", "exec sleep 1000"},
		Cwd: "/", Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}})
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(60*time.Second))
	defer cancel()
	var w api.PaneWaitResult
	owner := p.a.control()
	waitUntil(t, "a forwarded wait", func() bool {
		return owner.Call(ctx, "pane.wait", api.PaneWaitParams{Session: "bee/far", QuietMS: 1}, &w) == nil
	})
	// 17 forwarded waits on one connection that then goes away: the far hab holds 16 and refuses
	// one, which shows all 16 run there.
	gone, err := control.Dial(p.a.sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gone.Hello(ctx, api.ClientInfo{Name: "gone", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		t.Fatal(err)
	}
	refused := make(chan struct{}, paneWaitsPerCaller+1)
	for range paneWaitsPerCaller + 1 {
		go func() {
			err := gone.Call(ctx, "pane.wait", api.PaneWaitParams{Session: "bee/far", Match: "never printed", TimeoutMS: 300000}, &api.PaneWaitResult{})
			if paneErrKind(err) == api.ErrorKindUnavailable && strings.Contains(err.Error(), "too many pane waits") {
				refused <- struct{}{}
			}
		}()
	}
	select {
	case <-refused:
	case <-ctx.Done():
		t.Fatal("the far hab never refused a 17th forwarded wait")
	}
	gone.Close()
	waitUntil(t, "the far slots free within a lease", func() bool {
		return owner.Call(ctx, "pane.wait", api.PaneWaitParams{Session: "bee/far", QuietMS: 1}, &w) == nil
	})
}

// TestWaitMatcherLongLine: a long line fed in small chunks costs linear work for a literal, and
// a pattern runs on the open line at most once per interval (F4).
func TestWaitMatcherLongLine(t *testing.T) {
	t.Parallel()
	const n = 60000
	now := time.Unix(0, 0)
	lit := &waitMatcher{text: []byte("never printed"), every: time.Hour}
	pat := &waitMatcher{re: regexp.MustCompile(`never$`), every: time.Hour}
	for i := 0; i < n; i++ {
		b := []byte{'x'}
		if lit.feed(b, now) || pat.feed(b, now) {
			t.Fatal("matched")
		}
	}
	if lit.work > 20*n {
		t.Errorf("literal work %d bytes for a %d-byte line: not linear", lit.work, n)
	}
	if pat.work > 2*n || !pat.pending {
		t.Errorf("pattern work %d bytes, pending %v: the open line is tried every feed", pat.work, pat.pending)
	}
	// The interval passes: the pattern runs once more, and an ended line is always checked.
	if pat.checkOpen(now.Add(time.Hour)) {
		t.Fatal("matched")
	}
	if !pat.feed([]byte("never\n"), now) || pat.hit != strings.Repeat("x", n)+"never" {
		t.Fatalf("ended line not matched: %d", len(pat.hit))
	}
	// A literal split across chunks still matches on the open line.
	m := &waitMatcher{text: []byte("$ "), every: time.Hour}
	if m.feed([]byte("user@host $"), now) || !m.feed([]byte(" "), now) || m.hit != "user@host $ " {
		t.Fatalf("prompt across chunks: %q", m.hit)
	}
}

// TestWaitSlots: 16 per caller, 64 on the hab, the owner counted only on the hab's (F3).
func TestWaitSlots(t *testing.T) {
	t.Parallel()
	var w waitSlots
	var rel []func()
	for range paneWaitsPerCaller {
		r, ok := w.take("route:a")
		if !ok {
			t.Fatal("refused under the bound")
		}
		rel = append(rel, r)
	}
	if _, ok := w.take("route:a"); ok {
		t.Fatal("a 17th for one caller")
	}
	rel[0]()
	rel[0]() // a second release is a no-op
	if r, ok := w.take("route:a"); !ok {
		t.Fatal("not taken after a release")
	} else {
		rel[0] = r
	}
	for i := len(rel); i < paneWaitsAll; i++ {
		r, ok := w.take("")
		if !ok {
			t.Fatalf("owner wait %d refused", i)
		}
		rel = append(rel, r)
	}
	if _, ok := w.take("peer:x"); ok {
		t.Fatal("past the hab's bound")
	}
}

// TestCleanPeerText: another hab's answer loses its control characters and keeps the size cap
// (F2).
func TestCleanPeerText(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("y", paneTextCap/2+1)
	r := api.PaneText{Pane: "p\x1b", Screen: []string{"a\x1b]52;c;eA==\x07b", big, big, "c\u009b"}, Scrollback: []string{"s\x07"}}
	cleanPeerText(&r)
	if r.Screen[0] != "a]52;c;eA==b" || r.Screen[3] != "c" || r.Pane != "p" {
		t.Fatalf("screen %q pane %q", r.Screen[0], r.Pane)
	}
	if r.Screen[2] != "" || len(r.Scrollback) != 1 || r.Scrollback[0] != "s" || !r.Truncated {
		t.Fatalf("cap: row 2 %d bytes, %d scrollback rows, truncated %v", len(r.Screen[2]), len(r.Scrollback), r.Truncated)
	}
	wr := api.PaneWaitResult{Line: "x\x1b[2Jy‮"}
	cleanPeerWait(&wr)
	if wr.Line != "x[2Jy‮" {
		t.Fatalf("line %q", wr.Line)
	}
	err := peerErr("bee", control.Errorf(api.ErrorKindDenied, "no\x1b]0;t\x07 way"))
	if paneErrKind(err) != api.ErrorKindDenied || strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("peer error %q", err)
	}
	// The CLI's own pass, and its bidi strip for printed errors.
	cr := api.PaneText{Screen: []string{"q\x1bw"}}
	cleanText(&cr)
	if cr.Screen[0] != "qw" {
		t.Fatalf("cli row %q", cr.Screen[0])
	}
	if e := cliErr(control.Errorf(api.ErrorKindDenied, "a‮b\x07c")); strings.ContainsAny(e.Error(), "‮\x07") || paneErrKind(e) != api.ErrorKindDenied {
		t.Fatalf("cli error %q", e)
	}
}

// TestClampFrom: an agent's door replays at most 1 MiB; the owner's any offset (F3).
func TestClampFrom(t *testing.T) {
	t.Parallel()
	start := uint64(5 << 20)
	for _, c := range []struct {
		door       readDoor
		from, want uint64
	}{
		{doorRoute, 0, start - paneReplayMax},
		{doorPeer, 0, start - paneReplayMax},
		{doorRoute, start - 10, start - 10},
		{doorOwner, 0, 0},
	} {
		if got := clampFrom(paneCaller{door: c.door}, start, c.from); got != c.want {
			t.Errorf("door %d from %d: %d, want %d", c.door, c.from, got, c.want)
		}
	}
}
