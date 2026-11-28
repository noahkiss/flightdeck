package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/diaglog"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// TestHookLog is diagnostics test 13: a failed hook run writes one hook.log line with the
// harness, the event, the failure kind and the duration, never the payload; it exits 0 within
// its budget. A run outside any pane, or on a host with no logs folder, writes nothing (4.4).
func TestHookLog(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	state := filepath.Join(d, "state")
	if err := os.MkdirAll(logsDir(state), 0o700); err != nil {
		t.Fatal(err)
	}
	const token = "0123456789abcdef0123"
	const secret = "the payload words q7w8e9"

	// A socket that accepts and never answers: the run times out.
	hung, err := net.Listen("unix", filepath.Join(d, "hung.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer hung.Close()
	go func() {
		for {
			c, err := hung.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	// A hab that answers with an error status.
	bad, err := net.Listen("unix", filepath.Join(d, "bad.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "refused", http.StatusInternalServerError)
	})}
	go srv.Serve(bad)
	defer srv.Close()

	runHook := func(env ...string) time.Duration {
		t.Helper()
		cmd := exec.Command(bin, "hook", "claude", "Notification")
		cmd.Env = append(os.Environ(), append([]string{"GFF_HAB_STATE=" + state, "GFF_HOOK_DEBUG="}, env...)...)
		cmd.Stdin = strings.NewReader(`{"message":"` + secret + `","hook_event_name":"Notification"}`)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		took := time.Since(start)
		if err != nil || len(out) != 0 {
			t.Fatalf("hook %v: %v\n%s", env, err, out)
		}
		return took
	}
	lines := func() []diaglog.Line {
		t.Helper()
		b, _ := os.ReadFile(filepath.Join(logsDir(state), hookLogFile))
		if strings.Contains(string(b), secret) {
			t.Fatalf("hook.log holds the payload:\n%s", b)
		}
		var out []diaglog.Line
		for l := range strings.Lines(string(b)) {
			dl, ok := diaglog.Parse(l)
			if !ok {
				t.Fatalf("hook.log line %q", l)
			}
			out = append(out, dl)
		}
		return out
	}

	budget := hook.Budget + 50*time.Millisecond
	for i, c := range []struct {
		sock, kind string
	}{
		{filepath.Join(d, "none.sock"), hook.FailNoHab},
		{filepath.Join(d, "hung.sock"), hook.FailTimeout},
		{filepath.Join(d, "bad.sock"), hook.FailStatus},
	} {
		took := runHook("GFF_HAB=" + hook.Addr(c.sock, token))
		if took > budget+scaleTimeout(time.Second) {
			t.Errorf("%s: the run took %v, over its budget", c.kind, took)
		}
		ls := lines()
		if len(ls) != i+1 {
			t.Fatalf("%s: %d lines in hook.log, want %d", c.kind, len(ls), i+1)
		}
		l := ls[i]
		h, _ := l.Get("harness")
		ev, _ := l.Get("event")
		kind, _ := l.Get("failure")
		ms, hasMs := l.Get("took_ms")
		if l.Proc != "hook" || l.Lvl != "warn" || l.Msg != "hook failed" || h != "claude" || ev != "Notification" || kind != c.kind || !hasMs || ms == "" {
			t.Errorf("%s: line %+v", c.kind, l)
		}
	}

	// Outside any pane (no GFF_HAB): nothing.
	runHook("GFF_HAB=")
	if n := len(lines()); n != 3 {
		t.Errorf("a run outside any pane wrote a line: %d", n)
	}

	// The event from the payload when no argument names it; a payload event that no hook config
	// posts is its own text, so the line has no event at all (review part 2 L12).
	none := "GFF_HAB=" + hook.Addr(filepath.Join(d, "none.sock"), token)
	for i, c := range []struct{ payload, event string }{
		{`{"hook_event_name":"Notification","message":"` + secret + `"}`, "Notification"},
		{`{"hook_event_name":"` + secret + `"}`, ""},
	} {
		cmd := exec.Command(bin, "hook", "claude")
		cmd.Env = append(os.Environ(), "GFF_HAB_STATE="+state, "GFF_HOOK_DEBUG=", none)
		cmd.Stdin = strings.NewReader(c.payload)
		if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("hook from the payload: %v\n%s", err, out)
		}
		ls := lines()
		if len(ls) != 4+i {
			t.Fatalf("payload %d: %d lines", i, len(ls))
		}
		l := ls[len(ls)-1]
		ev, ok := l.Get("event")
		_, text := l.Get("event_text")
		if kind, _ := l.Get("failure"); kind != hook.FailNoHab || ev != c.event || ok != (c.event != "") || text {
			t.Errorf("payload %d: line %+v", i, l)
		}
	}

	// The hard cap: a harness that never closes stdin. The cap's timer writes the timeout line,
	// with the argument's event, and the run exits 0.
	cmd := exec.Command(bin, "hook", "claude", "Stop")
	cmd.Env = append(os.Environ(), "GFF_HAB_STATE="+state, "GFF_HOOK_DEBUG=", none, noRaceExitSleep)
	w, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("the capped run: %v", err)
	}
	if took := time.Since(start); took > budget+scaleTimeout(time.Second) {
		t.Errorf("the capped run took %v", took)
	}
	ls := lines()
	if len(ls) != 6 {
		t.Fatalf("the capped run: %d lines", len(ls))
	}
	capEv, _ := ls[5].Get("event")
	if capKind, _ := ls[5].Get("failure"); capEv != "Stop" || capKind != hook.FailTimeout || ls[5].Msg != "hook failed" {
		t.Errorf("the capped run's line %+v", ls[5])
	}

	// With no logs folder: nothing.
	other := filepath.Join(d, "other")
	cmd = exec.Command(bin, "hook", "claude", "Notification")
	cmd.Env = append(os.Environ(), "GFF_HAB_STATE="+other, "GFF_HAB="+hook.Addr(filepath.Join(d, "none.sock"), token))
	cmd.Stdin = strings.NewReader(`{}`)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Errorf("a host with no logs folder: %v", err)
	}
}
