package main

import (
	"encoding/json"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestBellScanner: a BEL rings outside control strings only, from any read split. A BEL that
// ends an OSC (a title) does not ring; one inside a DCS or APC string does not either.
func TestBellScanner(t *testing.T) {
	in := "a\x07b\x1b]0;title\x07c\x07\x1bPq\x07x\x1b\\\x07\x1b_G\x07\x1b\\d\x1b]2;t\x1b\\\x07"
	const want = 4
	for split := 1; split <= len(in); split++ {
		var b bellScanner
		n := 0
		for i := 0; i < len(in); i += split {
			n += b.count([]byte(in[i:min(i+split, len(in))]))
		}
		if n != want {
			t.Fatalf("split %d: %d bells, want %d", split, n, want)
		}
	}
}

// TestActivityEvents: a pane that prints and rings sends pane.output with its offset and
// pane.bell with the count, as ephemeral events (no revision), and each at most once a second:
// a burst of output is one event. The hab follows a new pane from a goroutine and from now on,
// so the program prints dots until a key comes: the test types only once a pane.output shows
// the hab follows it, else the bells could go by before it does.
func TestActivityEvents(t *testing.T) {
	e := newHabEnv(t, 0)
	cc := e.control()
	pane := e.spawn(cc, "stty -icanon min 0 time 1; while [ -z \"$(dd bs=1 count=1 2>/dev/null)\" ]; do printf .; done; "+
		"printf 'out\\a\\a'; for i in 1 2 3 4 5 6 7 8 9; do echo $i; done; exec sleep 600")
	ec := e.control()
	call(t, ec, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, nil)
	type seen struct {
		outputs []api.PaneOutput
		bells   int
	}
	got := make(chan seen, 1)
	got <- seen{}
	go func() {
		for n := range ec.Notifications() {
			ev, err := n.Event()
			if n.Method != "event" || err != nil || ev.ID != pane {
				continue
			}
			if ev.Revision != nil {
				t.Errorf("%s carries a revision", ev.Type)
			}
			s := <-got
			switch ev.Type {
			case api.EventPaneOutput:
				var o api.PaneOutput
				json.Unmarshal(ev.Data, &o)
				s.outputs = append(s.outputs, o)
			case api.EventPaneBell:
				var b api.PaneBell
				json.Unmarshal(ev.Data, &b)
				s.bells += b.Count
			}
			got <- s
		}
	}()
	var before int
	waitUntil(t, "the hab to follow the pane's output", func() bool {
		s := <-got
		got <- s
		before = len(s.outputs)
		return before > 0
	})
	call(t, cc, "pane.input", api.InputParams{Pane: pane, Text: "g"}, nil)
	var last seen
	waitUntil(t, "two bells and output past the burst", func() bool {
		s := <-got
		got <- s
		last = s
		if s.bells != 2 || len(s.outputs) == 0 {
			return false
		}
		var st api.StructureResult
		call(t, cc, "structure.get", api.StructureGetParams{}, &st)
		for _, p := range st.Panes {
			if p.ID == pane {
				return p.Offset > 0 && s.outputs[len(s.outputs)-1].Offset == p.Offset
			}
		}
		return false
	})
	if n := len(last.outputs) - before; n > 3 {
		t.Errorf("%d pane.output events for one burst, want at most a few (one a second)", n)
	}
}
