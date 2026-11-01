package main

// End-to-end tests of pane size ownership and redraw (hab-logic-api.md 6, docs/testing.md core
// test 9), with the real holder and gff-hab binaries. The debounce here is the real 300 ms:
// each test waits for the event or frame it expects, never for time to pass. The same rules
// with a fake clock are in internal/panesize.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// ownerEvents subscribes c to the host's events and returns the pane.owner events for pane, in
// order.
func ownerEvents(t *testing.T, c *control.Client, pane string) <-chan api.PaneOwner {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	if err := c.Call(ctx, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, nil); err != nil {
		t.Fatal(err)
	}
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

// nextOwner waits for the next owner event.
func nextOwner(t *testing.T, ch <-chan api.PaneOwner, what string) api.PaneOwner {
	t.Helper()
	select {
	case o, ok := <-ch:
		if !ok {
			t.Fatalf("waiting for %s: the control connection ended", what)
		}
		return o
	case <-time.After(scaleTimeout(15 * time.Second)):
		t.Fatalf("timed out waiting for %s", what)
	}
	return api.PaneOwner{}
}

func call(t *testing.T, c *control.Client, method string, params, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	if err := c.Call(ctx, method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func hello(t *testing.T, e *habEnv) (*control.Client, string) {
	t.Helper()
	c, err := control.Dial(e.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	w, err := c.Hello(ctx, api.ClientInfo{Name: "test", Version: "0", Kind: api.ClientKindApp}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return c, w.ClientID
}

// sizeScript appends the PTY size the program sees to $DIR/sizes at the start and after every
// SIGWINCH.
const sizeScript = `stty -echo
trap 'stty size >> "$DIR/sizes"' WINCH
stty size >> "$DIR/sizes"
while :; do sleep 0.02; done`

// programSizes reads what sizeScript recorded, as "rows cols" lines.
func programSizes(e *habEnv) []string {
	b, _ := os.ReadFile(filepath.Join(e.work, "sizes"))
	return strings.Fields(strings.ReplaceAll(string(b), " ", "x"))
}

// TestTypingTakesOwnership is the stage's first acceptance test: B types and becomes the owner
// after the debounce, with its desired size; A's next keystroke takes the pane back; each
// change emits exactly one pane.owner event with its reason, and every data stream sees the
// new size with the owner.
func TestTypingTakesOwnership(t *testing.T) {
	e := newHabEnv(t, 0)
	a, aid := hello(t, e)
	b, bid := hello(t, e)
	pane := e.spawn(a, sizeScript)
	evs := ownerEvents(t, a, pane)
	waitUntil(t, "the program's first size", func() bool { return len(programSizes(e)) > 0 })
	dc, err := e.open(b, pane, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	r := startReader(dc, true)
	var l streamLog
	lastResize := func() *api.ResizeFrame {
		for i := len(l.frames) - 1; i >= 0; i-- {
			if l.frames[i].Resize != nil {
				return l.frames[i].Resize
			}
		}
		return nil
	}

	var res api.DesiredSizeResult
	call(t, b, "pane.set_desired_size", api.DesiredSizeParams{Pane: pane, Cols: 100, Rows: 30}, &res)
	if res.Applied {
		t.Fatal("B's size applied while A owns the pane")
	}
	for _, k := range []string{"l", "s"} {
		call(t, b, "pane.input", api.InputParams{Pane: pane, Text: k}, nil)
	}
	if o := nextOwner(t, evs, "B to own the pane"); o != (api.PaneOwner{Owner: bid, Reason: api.OwnerReasonTyped, Cols: 100, Rows: 30}) {
		t.Fatalf("owner event %+v, want B typed 100x30", o)
	}
	r.until(t, &l, "the resize to B's size", false, func() bool {
		f := lastResize()
		return f != nil && f.Cols == 100 && f.Rows == 30
	})
	if f := lastResize(); f.Owner != bid || f.Reason != api.OwnerReasonTyped {
		t.Fatalf("resize frame %+v, want owner B, reason typed", f)
	}
	waitUntil(t, "the program to see 100x30", func() bool {
		s := programSizes(e)
		return len(s) > 0 && s[len(s)-1] == "30x100"
	})

	call(t, a, "pane.input", api.InputParams{Pane: pane, Text: "x"}, nil)
	if o := nextOwner(t, evs, "A to take the pane back"); o != (api.PaneOwner{Owner: aid, Reason: api.OwnerReasonTyped, Cols: 80, Rows: 24}) {
		t.Fatalf("owner event %+v, want A typed 80x24", o)
	}
	r.until(t, &l, "the resize back to A's size", false, func() bool {
		f := lastResize()
		return f != nil && f.Cols == 80 && f.Rows == 24 && f.Owner == aid
	})

	// A disowns; its event comes next, so no other event came between.
	var or api.OwnerResult
	call(t, a, "pane.disown", api.PaneRef{Pane: pane}, &or)
	if o := nextOwner(t, evs, "the disown"); o != (api.PaneOwner{Reason: api.OwnerReasonDisown, Cols: 80, Rows: 24}) {
		t.Fatalf("owner event %+v, want no owner, disown", o)
	}
	var st api.StructureResult
	call(t, a, "structure.get", nil, &st)
	for _, p := range st.Panes {
		if p.ID == pane && (p.Owner != "" || p.Cols != 80 || p.Rows != 24) {
			t.Fatalf("pane in the structure: %+v", p)
		}
	}
}

// TestObserverNeverResizes is the stage's second acceptance test: a client that follows a pane
// through an observer stream sets its view's size, and the PTY keeps its size. A client with a
// full view then sets a size, which the PTY takes: that is the first size change any stream sees.
func TestObserverNeverResizes(t *testing.T) {
	e := newHabEnv(t, 0)
	a, _ := hello(t, e)
	obs, _ := hello(t, e)
	full, _ := hello(t, e)
	pane := e.spawn(a, sizeScript)
	waitUntil(t, "the program's first size", func() bool { return len(programSizes(e)) > 0 })
	call(t, a, "pane.disown", api.PaneRef{Pane: pane}, nil) // no owner: any size would apply

	dc, err := e.open(obs, pane, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	r := startReader(dc, true)
	var l streamLog
	var res api.DesiredSizeResult
	call(t, obs, "pane.set_desired_size", api.DesiredSizeParams{Pane: pane, Cols: 40, Rows: 10}, &res)
	if res.Applied {
		t.Fatal("the observer's size was applied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	if err := obs.Call(ctx, "pane.take", api.PaneRef{Pane: pane}, nil); err == nil || !strings.Contains(err.Error(), string(api.ErrorKindInvalid)) {
		t.Fatalf("observer pane.take: %v, want invalid", err)
	}

	call(t, full, "pane.set_desired_size", api.DesiredSizeParams{Pane: pane, Cols: 90, Rows: 30}, &res)
	if !res.Applied {
		t.Fatal("a full view's size on a pane with no owner was not applied")
	}
	r.until(t, &l, "a resize frame", false, func() bool {
		for _, f := range l.frames {
			if f.Resize != nil && (f.Resize.Cols != 80 || f.Resize.Rows != 24) {
				return true
			}
		}
		return false
	})
	for _, f := range l.frames {
		if f.Resize != nil && (f.Resize.Cols != 80 || f.Resize.Rows != 24) {
			if f.Resize.Cols != 90 || f.Resize.Rows != 30 || f.Resize.Owner != "" {
				t.Fatalf("first size change %+v, want 90x30 with no owner (the observer's 40x10 must never apply)", f.Resize)
			}
			break
		}
	}
	waitUntil(t, "the program to see 90x30", func() bool {
		s := programSizes(e)
		return len(s) > 0 && s[len(s)-1] == "30x90"
	})
	if s := programSizes(e); strings.Contains(strings.Join(s, " "), "10x40") {
		t.Fatalf("the program saw the observer's size: %q", s)
	}
}

// TestRedrawWritesNoInput is the stage's third acceptance test: pane.redraw, snapshot and nudge,
// writes no byte to the program's input. The snapshot goes to the asking client's stream only;
// the nudge resizes the PTY one column narrower and back.
func TestRedrawWritesNoInput(t *testing.T) {
	e := newHabEnv(t, 0)
	a, _ := hello(t, e)
	b, _ := hello(t, e)
	pane := e.spawn(a, `stty raw -echo
printf 'ready\r\n'
exec cat > "$DIR/in"`)
	var logs [2]streamLog
	var rs [2]*reader
	for i, c := range []*control.Client{a, b} {
		dc, err := e.open(c, pane, nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		rs[i] = startReader(dc, true)
		rs[i].until(t, &logs[i], "the first snapshot", false, func() bool { return len(logs[i].frames) > 0 })
	}
	snapshots := func(l *streamLog) int {
		n := 0
		for _, f := range l.frames {
			if f.Kind == api.FrameSnapshot {
				n++
			}
		}
		return n
	}
	resizes := func(l *streamLog) []string {
		var out []string
		for _, f := range l.frames {
			if f.Resize != nil {
				out = append(out, fmt.Sprintf("%dx%d", f.Resize.Cols, f.Resize.Rows))
			}
		}
		return out
	}

	call(t, a, "pane.redraw", api.RedrawParams{Pane: pane}, nil)
	rs[0].until(t, &logs[0], "the redraw snapshot", false, func() bool { return snapshots(&logs[0]) == 2 })
	call(t, a, "pane.redraw", api.RedrawParams{Pane: pane, Mode: api.RedrawModeNudge}, nil)
	rs[0].until(t, &logs[0], "the nudge snapshot", false, func() bool { return snapshots(&logs[0]) == 3 })
	// B's stream sees both steps of the nudge. A's may lose the narrow step with the frames its
	// redraw snapshot replaces, so it waits only for the final size.
	rs[1].until(t, &logs[1], "the nudge on the other stream", false, func() bool {
		s := strings.Join(resizes(&logs[1]), " ")
		return strings.HasSuffix(s, "79x24 80x24")
	})
	rs[0].until(t, &logs[0], "the final size on the redrawn stream", false, func() bool {
		s := resizes(&logs[0])
		return len(s) > 0 && s[len(s)-1] == "80x24"
	})
	if n := snapshots(&logs[1]); n != 1 {
		t.Fatalf("the other client's stream got %d snapshots, want only its first", n)
	}

	// The program has read nothing; the next byte it reads is the one typed now.
	call(t, a, "pane.input", api.InputParams{Pane: pane, Text: "Z"}, nil)
	waitUntil(t, "the typed byte", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.work, "in"))
		return len(b) > 0
	})
	if got, _ := os.ReadFile(filepath.Join(e.work, "in")); string(got) != "Z" {
		t.Fatalf("the program read %q, want only the typed Z", got)
	}
}
