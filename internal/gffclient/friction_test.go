package gffclient

import (
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// TestFrictionKey: in a session view, Ctrl+O F asks for one line naming the session; Tab marks
// the note a blocker; Enter sends diag.friction to the session's hab with its session and pane,
// and the bottom bar shows the note's trace (diagnostics.md 9.1). Esc sends nothing.
func TestFrictionKey(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	session, pane, _ := a.AddSession("job")
	hst := newHost(t, 160, 40, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("the running row", func(s string) bool { return line(s, "   job") > 0 })
	hst.typ("\r")
	hst.waitScreen("job in a view", func(s string) bool { return strings.Contains(s, " job · one") })

	hst.typ(cmdKey + "F")
	hst.waitScreen("the friction prompt", func(s string) bool {
		return strings.Contains(s, "[preference] friction note on job")
	})
	hst.typ("gave up\x1b")
	hst.waitScreen("the prompt gone", func(s string) bool { return !strings.Contains(s, "friction note on job") })

	hst.typ(cmdKey + "F")
	hst.waitScreen("the friction prompt again", func(s string) bool { return strings.Contains(s, "friction note on job") })
	hst.typ("\t")
	hst.waitScreen("blocker", func(s string) bool { return strings.Contains(s, "[blocker] friction note on job") })
	hst.typ("went back to tmux\r")
	hst.waitScreen("the trace on the bar", func(s string) bool {
		return strings.Contains(s, "friction note hFAKETRACE01 stored on one")
	})
	calls := a.Friction()
	if len(calls) != 1 {
		t.Fatalf("diag.friction calls: %+v", calls)
	}
	want := api.FrictionParams{Text: "went back to tmux", Session: session, Pane: pane, Blocker: true}
	if c := calls[0]; c.Params != want || c.Client != api.ClientKindTerminal {
		t.Fatalf("diag.friction got %+v from %s, want %+v", c.Params, c.Client, want)
	}
}
