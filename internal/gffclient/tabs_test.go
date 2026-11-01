package gffclient

import (
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestTabsKeepPanes: with the real holder and gff-hab, the sidebar opens a session in tab 1
// and another in a new tab 2. The first program prints while its tab is hidden; showing tab 1
// again shows that output: the hidden view's stream closed and resumed from its offset
// (gff-client.md 11.3). Opening a session already on screen focuses it, adding no view.
func TestTabsKeepPanes(t *testing.T) {
	h := startHab(t)
	other := h.other()
	first := spawn(t, other, "echo hello-one; read x; echo after-$x; exec sleep 600", 80, 24)
	spawn(t, other, "echo hello-two; exec sleep 600", 80, 24)
	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, h.sock)
	// Both rows are in one rank and first seen together, so they sort by name: sh, then sh-2.
	hst.waitScreen("both sessions", func(s string) bool {
		a, b := line(s, " sh  "), line(s, "sh-2")
		return a > 0 && b > a
	})

	hst.typ("\r")
	hst.waitScreen("sh in tab 1", func(s string) bool { return strings.Contains(s, "hello-one") })
	hst.typ(prefix + "c")
	hst.waitScreen("the empty tab 2", func(s string) bool {
		return strings.Contains(s, "2:2") && strings.Contains(s, "empty tab") && !strings.Contains(s, "hello-one")
	})
	hst.typ("j\r")
	hst.waitScreen("sh-2 in tab 2", func(s string) bool {
		return strings.Contains(s, "hello-two") && !strings.Contains(s, "hello-one")
	})

	var empty api.Empty
	call(t, other, "pane.input", api.InputParams{Pane: first, Data: []byte("zz\n")}, &empty)
	// The hidden tab gets the activity mark from the hab's pane.output event, with no stream.
	hst.waitScreen("tab 1 marked as printed", func(s string) bool { return strings.Contains(s, "1:1~") })
	hst.typ(prefix + "1")
	hst.waitScreen("tab 1 with what sh printed while hidden", func(s string) bool {
		return strings.Contains(s, "hello-one") && strings.Contains(s, "after-zz") && !strings.Contains(s, "hello-two")
	})

	// Open sh from the sidebar while it is on screen: focus, not a second view.
	hst.typ(prefix + "w")
	hst.typ("k\r")
	hst.waitScreen("one view of sh", func(s string) bool {
		return strings.Count(s, "hello-one") == 1 && strings.Contains(s, "1:1")
	})
}
