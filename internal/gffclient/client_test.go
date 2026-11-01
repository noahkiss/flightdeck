package gffclient

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// ownerEvents subscribes c to the host's events and returns the pane.owner events for pane.
func ownerEvents(t *testing.T, c *control.Client, pane string) <-chan api.PaneOwner {
	t.Helper()
	call(t, c, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, nil)
	out := make(chan api.PaneOwner, 64)
	go func() {
		defer close(out)
		for n := range c.Notifications() {
			if n.Method != "event" {
				continue
			}
			ev, err := n.Event()
			if err != nil || ev.Type != "pane.owner" || ev.ID != pane {
				continue
			}
			var o api.PaneOwner
			if json.Unmarshal(ev.Data, &o) == nil {
				out <- o
			}
		}
	}()
	return out
}

func nextOwner(t *testing.T, ch <-chan api.PaneOwner, what string) api.PaneOwner {
	t.Helper()
	select {
	case o, ok := <-ch:
		if !ok {
			t.Fatalf("waiting for %s: the control connection ended", what)
		}
		return o
	case <-time.After(waitLong()):
		t.Fatalf("timed out waiting for %s", what)
	}
	return api.PaneOwner{}
}

func sizeParams(t *testing.T, q request) api.DesiredSizeParams {
	t.Helper()
	var p api.DesiredSizeParams
	if err := json.Unmarshal(q.Params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// An observer view never asks for a size, never takes the pane and never sends input, through
// opening, a host resize, typing and the fit key. Another device owns the pane and its size
// stays.
func TestObserverNeverResizes(t *testing.T) {
	h := startHab(t)
	other := h.other()
	watched := spawn(t, other, "echo watched; exec sleep 600", 80, 24)
	mine := spawn(t, other, "echo mine; exec sleep 600", 80, 24)
	call(t, other, "pane.disown", api.PaneRef{Pane: mine}, nil)

	hst := newHost(t, 120, 40, false)
	// The observer comes first: sizes go out in view order on one queue, so once the second
	// view's size is out, any size for the observer would be out too.
	g := startGFF(t, h, hst, SplitColumns, PaneSpec{ID: watched, Observe: true}, PaneSpec{ID: mine})
	hst.waitScreen("both panes", func(s string) bool { return strings.Contains(s, "watched") && strings.Contains(s, "mine") })
	waitUntil(t, "the first size for the full view", func() bool { return len(g.rec.calls("pane.set_desired_size", mine)) == 1 })

	// Typing into the observer (focused first) and the fit key send nothing.
	hst.typ("typed\r\x1cf")
	hst.resize(100, 30)
	waitUntil(t, "the debounce armed", func() bool { return g.timer.count() == 1 })
	g.timer.fire(t)
	waitUntil(t, "the full view's new size", func() bool { return len(g.rec.calls("pane.set_desired_size", mine)) == 2 })
	if got := sizeParams(t, g.rec.calls("pane.set_desired_size", mine)[1]); got.Cols != 49 || got.Rows != 29 {
		t.Errorf("full view asked for %dx%d, want 49x29", got.Cols, got.Rows)
	}
	// Focus the full view and type, so the observer's input lane would have run by now.
	hst.typ("\x1co" + "x")
	waitUntil(t, "input to the full view", func() bool { return len(g.rec.calls("pane.input", mine)) > 0 })

	for _, m := range []string{"pane.set_desired_size", "pane.take", "pane.request", "pane.input", "pane.redraw"} {
		if n := len(g.rec.calls(m, watched)); n != 0 {
			t.Errorf("gff sent %d %s calls for the observed pane", n, m)
		}
	}
	tickets := g.rec.calls("data.ticket", watched)
	if len(tickets) == 0 {
		t.Fatal("no data.ticket for the observed pane")
	}
	for _, q := range tickets {
		var p api.TicketParams
		json.Unmarshal(q.Params, &p)
		if !p.Observe {
			t.Errorf("data.ticket for the observed pane without observe: %s", q.Params)
		}
	}
	if c, r, owner := paneSize(t, other, watched); c != 80 || r != 24 || owner == g.clientID() {
		t.Errorf("observed pane is %dx%d owned by %q, want 80x24 not owned by gff", c, r, owner)
	}
	if !strings.Contains(hst.screen(), "observer") {
		t.Errorf("observer frame note missing:\n%s", hst.screen())
	}
}

// Typing takes ownership: another device owns the pane, gff shows it composed with the
// owner's size, input moves the owner to gff after the hab's debounce, the pane takes gff's
// size, and gff passes its bytes through. gff never takes the pane on its own.
func TestTypingTakesOwnership(t *testing.T) {
	h := startHab(t)
	other := h.other()
	pane := spawn(t, other, `trap 'printf "size %s\n" "$(stty size)"' WINCH; echo ready; while :; do sleep 0.1; done`, 80, 24)
	owners := ownerEvents(t, other, pane)

	hst := newHost(t, 100, 30, false)
	g := startGFF(t, h, hst, SplitColumns, PaneSpec{ID: pane})
	hst.waitScreen("the pane, composed under the owner note", func(s string) bool {
		return strings.Contains(s, "ready") && strings.Contains(s, "owner ")
	})
	waitUntil(t, "gff's desired size", func() bool { return len(g.rec.calls("pane.set_desired_size", pane)) == 1 })
	if c, r, owner := paneSize(t, other, pane); c != 80 || r != 24 || owner == g.clientID() {
		t.Fatalf("before typing the pane is %dx%d owned by %q; want 80x24 owned by the other device", c, r, owner)
	}

	hst.typ("hello\r")
	o := nextOwner(t, owners, "the owner change")
	if o.Owner != g.clientID() || o.Reason != "typed" {
		t.Fatalf("owner event %+v, want gff (%s) by typing", o, g.clientID())
	}
	hst.waitScreen("the program at gff's size, passed through", func(s string) bool {
		return strings.Contains(s, "hello") && strings.Contains(s, "size 30 100")
	})
	out := hst.output()
	if !bytes.Contains(out, []byte(leaveCompose)) {
		t.Error("gff never left the composed view for pass-through")
	}
	if n := len(g.rec.calls("pane.take", pane)); n != 0 {
		t.Errorf("gff sent %d pane.take calls; only typing may move ownership", n)
	}
}

// A burst of host resizes repaints at once but asks the hab once, for the last size, when the
// debounce fires.
func TestHostResizeDebounced(t *testing.T) {
	h := startHab(t)
	other := h.other()
	pane := spawn(t, other, `trap 'printf "size %s\n" "$(stty size)"' WINCH; echo ready; while :; do sleep 0.1; done`, 80, 24)
	call(t, other, "pane.disown", api.PaneRef{Pane: pane}, nil)

	hst := newHost(t, 80, 24, false)
	g := startGFF(t, h, hst, SplitRows, PaneSpec{ID: pane})
	hst.waitScreen("the pane", func(s string) bool { return strings.Contains(s, "ready") })
	waitUntil(t, "the first size", func() bool { return len(g.rec.calls("pane.set_desired_size", pane)) == 1 })

	sizes := [][2]int{{90, 30}, {100, 32}, {110, 34}, {120, 36}, {130, 40}}
	for i, s := range sizes {
		hst.resize(s[0], s[1])
		waitUntil(t, "the debounce re-armed", func() bool { return g.timer.count() == i+1 })
	}
	if n := len(g.rec.calls("pane.set_desired_size", pane)); n != 1 {
		t.Fatalf("%d desired sizes before the debounce fired, want 1 (the first)", n)
	}
	g.timer.fire(t)
	waitUntil(t, "the debounced size", func() bool { return len(g.rec.calls("pane.set_desired_size", pane)) == 2 })
	if got := sizeParams(t, g.rec.calls("pane.set_desired_size", pane)[1]); got.Cols != 130 || got.Rows != 40 {
		t.Errorf("debounced size %dx%d, want the last, 130x40", got.Cols, got.Rows)
	}
	hst.waitScreen("the program at the last size", func(s string) bool { return strings.Contains(s, "size 40 130") })
	if s := hst.screen(); strings.Contains(s, "size 30 90") || strings.Contains(s, "size 36 120") {
		t.Errorf("the program saw an intermediate size:\n%s", s)
	}
	if n := len(g.rec.calls("pane.set_desired_size", pane)); n != 2 {
		t.Errorf("%d desired sizes in all, want 2", n)
	}
}

// The client never answers a terminal query, and no query reaches the host terminal, which
// would answer it into the program's input. The host here answers every query it sees. The
// queries are the ones gff-hab leaves to clients: pixel size reports, a clipboard read, and a
// size report with a control byte inside. Both views are checked: pass-through and composed.
func TestQueriesNeverAnswered(t *testing.T) {
	script := `stty raw -echo; printf 'start\033[14t\033[16t\033]52;c;?\007\033[1\0108tready\r\n'; ` +
		`l=$(dd bs=1 count=2 2>/dev/null | od -An -c | tr -d ' \n'); printf 'got[%s]\r\n' "$l"; exec sleep 600`
	for _, tc := range []struct {
		name  string
		split bool
	}{{"pass-through", false}, {"composed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHab(t)
			other := h.other()
			hst := newHost(t, 100, 30, true)
			// The pane starts at its view's size, so no resize moves its first line.
			cols, rows := 100, 30
			if tc.split {
				cols, rows = 50, 29
			}
			pane := spawn(t, other, script, cols, rows)
			call(t, other, "pane.disown", api.PaneRef{Pane: pane}, nil)
			specs := []PaneSpec{{ID: pane}}
			if tc.split {
				specs = append(specs, PaneSpec{ID: spawn(t, other, "exec sleep 600", 40, 10)})
			}
			startGFF(t, h, hst, SplitColumns, specs...)
			hst.waitScreen("ready", func(s string) bool { return strings.Contains(s, "ready") })
			hst.typ("XY")
			hst.waitScreen("what the program read", func(s string) bool { return strings.Contains(s, "got[") })
			if s := hst.screen(); !strings.Contains(s, "got[XY]") {
				t.Errorf("the program read something before the typed keys:\n%s", s)
			}
			if m := queryRE.FindAll(hst.output(), -1); len(m) > 0 {
				t.Errorf("queries reached the host terminal: %q", m)
			}
			if !tc.split && !bytes.Contains(hst.output(), []byte("start")) {
				t.Error("no output passed through")
			}
		})
	}
}

// Detaching ends Run with ErrDetached and leaves the composed screen.
func TestDetach(t *testing.T) {
	h := startHab(t)
	other := h.other()
	a := spawn(t, other, "echo left; exec sleep 600", 40, 10)
	b := spawn(t, other, "echo right; exec sleep 600", 40, 10)
	hst := newHost(t, 80, 24, false)
	g := startGFF(t, h, hst, SplitColumns, PaneSpec{ID: a}, PaneSpec{ID: b})
	hst.waitScreen("both panes", func(s string) bool { return strings.Contains(s, "left") && strings.Contains(s, "right") })
	hst.typ("\x1cd")
	select {
	case err := <-g.done:
		g.done <- err // for the cleanup
		if err != ErrDetached {
			t.Fatalf("Run = %v, want ErrDetached", err)
		}
	case <-time.After(waitLong()):
		t.Fatal("Run did not return after detach")
	}
	if out := hst.output(); !bytes.HasSuffix(out, []byte(leaveCompose)) {
		t.Errorf("the host was not left on its own screen: ends %q", out[max(len(out)-40, 0):])
	}
}
