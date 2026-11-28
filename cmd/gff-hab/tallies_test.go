package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/health"
	"github.com/noahkiss/flightdeck/internal/tallies"
)

// TestTalliesEndToEnd is diagnostics test 8 and the stage 3 acceptance: after a run of
// gff-hab, gff-hab tallies --unused 1 lists the methods the run did not call, and --days 1 shows
// calls and errors per method and client kind. A name outside the surface is never stored.
func TestTalliesEndToEnd(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	sock, state := filepath.Join(d, "run", "hab.sock"), filepath.Join(d, "state")
	s := startServe(t, sock, state) // its start check calls hab.hello as an app

	c, err := control.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "test", Version: "0", Kind: api.ClientKindCli}, "", ""); err != nil {
		t.Fatal(err)
	}
	var st api.StructureResult
	if err := c.Call(ctx, "structure.get", api.StructureGetParams{}, &st); err != nil {
		t.Fatal(err)
	}
	var set api.SettingsResult
	if err := c.Call(ctx, "settings.get", api.SettingsGetParams{IDs: []string{"heavy.slots"}}, &set); err != nil || len(set.Settings) != 1 {
		t.Fatalf("settings.get: %v %+v", err, set)
	}
	if err := c.Call(ctx, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{{ID: "heavy.slots", Revision: set.Settings[0].Revision,
		Value: json.RawMessage(`2`)}}}, &set); err != nil {
		t.Fatal(err)
	}
	const secret = "a-private-method-name"
	if err := c.Call(ctx, secret, struct{}{}, nil); err == nil {
		t.Fatal("an unknown method worked")
	}
	if err := c.Call(ctx, "session.close", api.SessionRef{Session: "no-such-session"}, nil); err == nil {
		t.Fatal("closing a session that does not exist worked")
	}
	// A hook envelope for a token gff-hab does not know: refused, counted by its event.
	hc := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(filepath.Dir(sock), "hook.sock"))
	}}}
	resp, err := hc.Post("http://hab/hooks/unknown-token", "application/json",
		strings.NewReader(`{"harness":"claude","event":"Stop","t":1,"n":1,"payload":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	c.Close()
	s.stop(t) // a clean exit flushes

	out := run(t, "tallies", "-state", state, "-unused", "1")
	unused := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			unused[f[0]+" "+f[1]] = true
		}
	}
	for _, used := range []string{"method hab.hello", "method structure.get", "method settings.set", "method session.close",
		"hook claude/Stop", "setting heavy.slots"} {
		if unused[used] {
			t.Errorf("%s was used but is listed:\n%s", used, out)
		}
	}
	for _, never := range []string{"method session.send", "method rpc.discover", "event pane.updated", "feature send",
		"setting group.max_leads", "hook codex/Stop"} {
		if !unused[never] {
			t.Errorf("%s was not used but is not listed:\n%s", never, out)
		}
	}
	if !strings.Contains(out, "never since "+time.Now().Format("2006-01-02")) {
		t.Errorf("no never-since line:\n%s", out)
	}

	var w tallies.Window
	if err := json.Unmarshal([]byte(run(t, "tallies", "-state", state, "-days", "1", "-json")), &w); err != nil {
		t.Fatal(err)
	}
	if w.Files != 1 || w.Sum.Calls["structure.get"]["cli"].N != 1 || w.Sum.Calls["hab.hello"]["app"].N < 1 {
		t.Errorf("calls %+v", w.Sum.Calls)
	}
	if e := w.Sum.Calls["session.close"]["cli"]; e == nil || e.Errors["not_found"] != 1 {
		t.Errorf("session.close: %+v", e)
	}
	if e := w.Sum.Calls[tallies.Other]["cli"]; e == nil || e.N != 1 || len(e.Errors) != 1 {
		t.Errorf("the unknown method: %+v", e)
	}
	if h := w.Sum.Hooks["claude/Stop"]; h == nil || h.Refused != 1 {
		t.Errorf("hooks %+v", w.Sum.Hooks)
	}
	if x := w.Sum.Settings["heavy.slots"]; x == nil || x.Set != 1 {
		t.Errorf("settings %+v", w.Sum.Settings)
	}
	if h := w.Sum.Health["hab"]; h == nil || h.Starts != 1 {
		t.Errorf("health %+v", w.Sum.Health)
	}
	es, _ := os.ReadDir(filepath.Join(state, tallies.DirName))
	for _, e := range es {
		b, _ := os.ReadFile(filepath.Join(state, tallies.DirName, e.Name()))
		if strings.Contains(string(b), secret) || strings.Contains(string(b), "no-such-session") || strings.Contains(string(b), "unknown-token") {
			t.Errorf("%s holds a name outside the surface:\n%s", e.Name(), b)
		}
	}
	text := run(t, "tallies", "-state", state, "-days", "1")
	if !strings.Contains(text, "client kinds seen: app, cli") || !strings.Contains(text, "structure.get") {
		t.Errorf("tallies --days 1:\n%s", text)
	}
}

// TestTalliesOff: [diagnostics] tallies = false in the hab's config turns them off.
func TestTalliesOff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "hab.toml")
	for body, want := range map[string]bool{"": true, "[diagnostics]\ntallies = false\n": false,
		"[diagnostics]\ntallies = true\n": true, "[other]\nx = 1\n": true} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := talliesOn(p, t.Logf); got != want {
			t.Errorf("%q: on %v, want %v", body, got, want)
		}
	}
	if !talliesOn(filepath.Join(dir, "missing.toml"), t.Logf) {
		t.Error("no config file: off")
	}
	os.WriteFile(p, []byte("[diagnostics]\ntallies = false\n"), 0o600)
	if openTallies(dir, p, t.Logf) != nil {
		t.Error("openTallies with tallies off")
	}
}

// TestChannelTally: a message queued counts channel.send once; a later change of the same
// record does not, and an expiry counts channel.expired (7.2).
func TestChannelTally(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tl := tallies.New(tallies.Config{Dir: dir, Surface: tallies.Current()})
	channelTally(tl, api.ChannelRecord{State: api.ChannelStateQueued, At: "a", Created: "a"})
	channelTally(tl, api.ChannelRecord{State: api.ChannelStateQueued, At: "b", Created: "a"})
	channelTally(tl, api.ChannelRecord{State: api.ChannelStateDelivered, At: "c", Created: "a"})
	channelTally(tl, api.ChannelRecord{State: api.ChannelStateExpired, At: "d", Created: "a"})
	channelTally(nil, api.ChannelRecord{State: api.ChannelStateQueued})
	if err := tl.Flush(); err != nil {
		t.Fatal(err)
	}
	w, err := tallies.ReadWindow(dir, time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if w.Sum.Features["channel.send"] != 1 || w.Sum.Features["channel.expired"] != 1 {
		t.Fatalf("features %v", w.Sum.Features)
	}
}

// TestEngineCrashTally: a Capcom engine crash counts under the process kind capcom, as a lost
// holder counts under holder (7.1, diagnostics review part 3 L7).
func TestEngineCrashTally(t *testing.T) {
	t.Parallel()
	state, dir := t.TempDir(), t.TempDir()
	tl := tallies.New(tallies.Config{Dir: dir, Surface: tallies.Current()})
	d := &habDiag{state: state, logf: t.Logf, now: time.Now, tally: tl}
	d.engineCrash(health.Record{T: time.Now().UTC(), Proc: "capcom", Kind: health.KindCrash, Cause: "exit 3"})
	d.holderLost("0011223344556677", "b1", 2)
	if err := tl.Flush(); err != nil {
		t.Fatal(err)
	}
	w, err := tallies.ReadWindow(dir, time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if h := w.Sum.Health["capcom"]; h == nil || h.Crashes != 1 {
		t.Errorf("capcom health %+v (all %v)", h, w.Sum.Health)
	}
	if h := w.Sum.Health["holder"]; h == nil || h.Unclean != 1 {
		t.Errorf("holder health %+v", h)
	}
	if h := w.Sum.Health["other"]; h != nil {
		t.Errorf("a crash counted under other: %+v", h)
	}
}
