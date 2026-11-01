package gffclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/fakehab"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// The workspace tests run gff over in-process fake habs (internal/fakehab: the real control
// server and structure store, agent states the test sets) and read the fake host terminal's
// screen. Every wait is on the screen or the hab reaching a state, never on time alone.

const prefix = "\x1c"

func fakeHab(t *testing.T, dir, id string) *fakehab.Hab {
	t.Helper()
	h, err := fakehab.New(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	return h
}

// startWorkspace runs gff's workspace over the habs' sockets, labelled one, two, ... in order.
func startWorkspace(t *testing.T, hst *host, socks ...string) {
	t.Helper()
	var specs []hosts.Spec
	for i, sock := range socks {
		specs = append(specs, hosts.Spec{Label: []string{"one", "two", "three"}[i],
			Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: sock}}})
	}
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
	go func() {
		done <- Run(ctx, Config{Hosts: set, Parser: vt.Fake{}, In: hst.inR, Out: hst, Size: hst.size,
			Resized: hst.resized, NewTimer: func() Timer { return timer }, Logf: t.Logf})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && err != context.Canceled {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(waitLong()):
			t.Error("Run did not return")
		}
		set.Close()
	})
}

// line returns the screen row that holds s, or -1.
func line(screen, s string) int {
	for i, l := range strings.Split(screen, "\n") {
		if strings.Contains(l, s) {
			return i
		}
	}
	return -1
}

// TestSidebarRecovers: two habs; one goes offline and comes back with a new epoch. Its rows
// stay while it is down, marked offline, and its sessions come back, new ones included, with
// no restart of gff.
func TestSidebarRecovers(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	b := fakeHab(t, d, "b")
	a.AddSession("alpha")
	b.AddSession("beta")
	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, a.Sock, b.Sock)
	hst.waitScreen("both hosts' sessions", func(s string) bool {
		return line(s, "alpha") > 0 && line(s, "beta") > 0 && strings.Contains(s, "2/2 hosts")
	})

	b.Stop()
	hst.waitScreen("two offline, its row kept", func(s string) bool {
		return strings.Contains(s, "○ two offline") && line(s, "beta") > 0 && strings.Contains(s, "1/2 hosts")
	})

	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	b.AddSession("gamma")
	hst.waitScreen("two back with its new session", func(s string) bool {
		return !strings.Contains(s, "○ two") && line(s, "beta") > 0 && line(s, "gamma") > 0 && strings.Contains(s, "2/2 hosts")
	})
}

// TestBoardBlockedAboveWorking: on the status board a blocked session, on one hab, sits above
// a working session on the other, under the plain section labels.
func TestBoardBlockedAboveWorking(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	b := fakeHab(t, d, "b")
	_, busy, _ := a.AddSession("busy")
	_, stuck, _ := b.AddSession("stuck")
	a.SetAgent(busy, api.AgentStatusWorking, "")
	hst := newHost(t, 140, 30, false)
	startWorkspace(t, hst, a.Sock, b.Sock)
	hst.waitScreen("the working session", func(s string) bool { return line(s, "busy") > 0 })
	hst.typ(prefix + "g")
	hst.waitScreen("the board", func(s string) bool { return strings.Contains(s, "status board") })
	b.SetAgent(stuck, api.AgentStatusBlocked, api.BlockedKindPermission)
	hst.waitScreen("blocked above working", func(s string) bool {
		needs, working := line(s, "Needs you"), line(s, " Working")
		st, bu := line(s, "stuck"), line(s, "busy")
		return needs > 0 && working > needs && st > needs && st < working && bu > working &&
			strings.Contains(s, "blocked:permission")
	})
	if s := hst.screen(); !strings.Contains(s, "1 need you") {
		t.Errorf("tab bar has no needs-you count:\n%s", s)
	}
}

// TestSidebarStartRenameClose: from the sidebar, start a session on a host, rename it, end its
// program after a yes, then remove its record.
func TestSidebarStartRenameClose(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	hst := newHost(t, 120, 30, false)
	startWorkspace(t, hst, a.Sock)
	// The sidebar says "no sessions" while the host is still connecting; n starts nothing then.
	hst.waitScreen("the empty sidebar, the host online", func(s string) bool {
		return strings.Contains(s, "no sessions") && strings.Contains(s, "1/1 hosts")
	})

	hst.typ("n")
	hst.waitScreen("the directory prompt", func(s string) bool { return strings.Contains(s, "[one] new session") })
	hst.typ("\r")
	hst.waitScreen("the command prompt", func(s string) bool { return strings.Contains(s, "command on one") })
	hst.typ("true\r")
	waitUntil(t, "the session on the hab", func() bool { return len(a.Sessions()) == 1 })
	hst.waitScreen("the new session in the sidebar and a tab", func(s string) bool {
		return line(s, "▸~ sh") > 0 || line(s, "▸  sh") > 0
	})

	hst.typ(prefix + "w")
	hst.typ("r")
	hst.waitScreen("the rename prompt", func(s string) bool { return strings.Contains(s, "rename sh on one: sh") })
	hst.typ("\x7f\x7frenamed\r")
	waitUntil(t, "the rename on the hab", func() bool {
		ss := a.Sessions()
		return len(ss) == 1 && ss[0].Name == "renamed"
	})
	hst.waitScreen("the new name", func(s string) bool { return line(s, "renamed") > 0 })

	hst.typ("x")
	hst.waitScreen("the end question", func(s string) bool { return strings.Contains(s, "end renamed on one?") })
	hst.typ("y")
	waitUntil(t, "the program ended", func() bool {
		ss := a.Sessions()
		return len(ss) == 1 && ss[0].State == api.SessionStateExited
	})
	// A sidebar row looks the same running or exited, and x asks to end until gff has the
	// exited state. The board names the state: wait for it there, then go back to the sidebar.
	hst.typ(prefix + "g")
	hst.waitScreen("the exited row on the board", func(s string) bool {
		r := line(s, "renamed")
		return r > 0 && strings.Contains(strings.Split(s, "\n")[r], "exited")
	})
	hst.typ(prefix + "w")
	hst.waitScreen("the sidebar again", func(s string) bool { return !strings.Contains(s, "status board") })
	hst.typ("x")
	hst.waitScreen("the remove question", func(s string) bool { return strings.Contains(s, "remove renamed from one?") })
	hst.typ("y")
	waitUntil(t, "the record removed", func() bool { return len(a.Sessions()) == 0 })
	hst.waitScreen("the row gone", func(s string) bool { return line(s, "renamed") < 0 && strings.Contains(s, "no sessions") })
}
