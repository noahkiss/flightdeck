//go:build ghostty

package gffclient

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/vt"
	"github.com/noahkiss/flightdeck/internal/vt/ghostty"
)

// An OSC 8 link a pane prints survives composing: with two panes side by side, gff writes the
// link's open sequence, with its own id and the pane's URI, right before the linked cells and the
// close right after them, and no other cell is linked (gff-client.md 3.4).
func TestLinkSurvivesComposing(t *testing.T) {
	paneParser = ghostty.Parser{}
	t.Cleanup(func() { paneParser = vt.Fake{} })
	h := startHab(t)
	other := h.other()
	// The pane prints its link once gff shows it: the test hab parses with vt.Fake, whose
	// snapshot keeps no link, so the link comes to gff's own parser as live bytes.
	left := spawn(t, other, `echo ready; read -r x; printf 'a\033]8;id=p1;https://example.com/a\033\\link\033]8;;\033\\ tail\n'; exec sleep 600`, 40, 10)
	right := spawn(t, other, `printf 'plain right\n'; exec sleep 600`, 40, 10)
	hst := newHost(t, 100, 30, false)
	startGFF(t, h, hst, SplitColumns, PaneSpec{ID: left}, PaneSpec{ID: right})
	hst.waitScreen("both panes", func(s string) bool {
		return strings.Contains(s, "ready") && strings.Contains(s, "plain right")
	})
	call(t, other, "pane.input", api.InputParams{Pane: left, Data: []byte("\n")}, nil)
	hst.waitScreen("the link", func(s string) bool { return strings.Contains(s, "alink tail") })
	out := hst.output()
	run := regexp.MustCompile(`a\x1b\]8;id=gff-[0-9a-z]+;https://example\.com/a\x1b\\link\x1b\]8;;\x1b\\ tail`)
	if !run.Match(out) {
		t.Fatalf("no linked run in the host output: %q", out)
	}
	if opens, runs := bytes.Count(out, []byte("\x1b]8;id=")), len(run.FindAll(out, -1)); opens != runs {
		t.Errorf("%d links opened, %d of them around the linked run: %q", opens, runs, out)
	}
}
