//go:build ghostty

package gffclient

import (
	"bytes"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/vt"
	"github.com/noahkiss/flightdeck/internal/vt/ghostty"
)

// A program that pushed kitty flags gets each key in its own encoding: with flags 9 (report
// every key as an escape code), the host's text "a" reaches it as CSI 97u, and Ctrl+C in the
// host's kitty form stays CSI 99;5u.
func TestKittyPaneEncoding(t *testing.T) {
	paneParser = ghostty.Parser{}
	t.Cleanup(func() { paneParser = vt.Fake{} })
	h := startHab(t)
	other := h.other()
	// The push comes as live bytes, after gff opened the pane: the test hab parses with vt.Fake.
	a := spawn(t, other, `echo ready; read -r x; stty raw -echo; printf '\033[>9upushed\r\n'; head -c 12 | od -An -tx1 | tr -d ' \n'; echo; exec sleep 600`, 40, 10)
	b := spawn(t, other, "echo right; exec sleep 600", 40, 10)
	hst := newHost(t, 100, 30, false)
	hst.kitty = true
	startGFF(t, h, hst, SplitColumns, PaneSpec{ID: a}, PaneSpec{ID: b})
	hst.waitScreen("both panes", func(s string) bool { return strings.Contains(s, "ready") && strings.Contains(s, "right") })
	waitUntil(t, "the push", func() bool { return bytes.Contains(hst.output(), []byte(kittyPush)) })
	call(t, other, "pane.input", api.InputParams{Pane: a, Data: []byte("\n")}, nil)
	hst.waitScreen("the program's push", func(s string) bool { return strings.Contains(s, "pushed") })
	hst.typ("a\x1b[99;5u")
	want := "1b5b3937751b5b39393b3575"
	hst.waitScreen("the program's bytes", func(s string) bool { return strings.Contains(s, want) })
}
