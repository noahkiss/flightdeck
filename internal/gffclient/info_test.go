package gffclient

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// barClient is a workspace client with no connection, the bars on, its clock at now and its
// hosts the given labels, each with an endpoint that never answers (so not online).
func barClient(t *testing.T, w, h int, now *time.Time, labels ...string) *client {
	t.Helper()
	c := testClient(t, w, h)
	var specs []hosts.Spec
	for _, l := range labels {
		specs = append(specs, hosts.Spec{Label: l,
			Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: filepath.Join(t.TempDir(), "none.sock")}}})
	}
	set, err := hosts.Open(hosts.Config{Source: hosts.Static(specs)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(set.Close)
	bar := DefaultBar()
	c.ws, c.cfg.Hosts, c.cfg.Bar, c.cfg.Keymap = true, set, &bar, DefaultKeymap()
	c.cfg.Now = func() time.Time { return *now }
	c.ctx, c.later = context.Background(), make(chan func(), 8)
	c.ui.init(c)
	return c
}

// Figures refresh at most every 2 s, one reading at a time, and never while the bars are
// hidden (gff-client.md 9.2).
func TestStatsRefreshRules(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := barClient(t, 160, 30, &now)
	calls := make(chan struct{}, 8)
	cpu := 7
	c.cfg.Stats = func() (api.HostStats, error) {
		calls <- struct{}{}
		return api.HostStats{CPUPct: &cpu}, nil
	}
	started := func() bool { return c.ui.sysBusy }

	c.ui.bars = false
	c.ui.tick()
	if started() || len(calls) != 0 {
		t.Fatal("a reading started while the bars are hidden")
	}
	c.ui.bars = true
	c.ui.tick()
	if !started() {
		t.Fatal("no reading with the bars shown")
	}
	<-calls
	(<-c.later)()
	if c.ui.sys == nil || infoText(c.ui.sys.stats, []string{"cpu"}) != "cpu 7%" {
		t.Fatalf("reading %+v", c.ui.sys)
	}
	now = now.Add(time.Second)
	c.ui.tick()
	if started() {
		t.Error("a second reading 1 s after the first")
	}
	now = now.Add(time.Second)
	c.ui.tick()
	if !started() {
		t.Error("no reading 2 s after the first")
	}
	c.ui.tick()
	<-calls
	if len(calls) != 0 {
		t.Error("two readings out at once")
	}
}

// The usage row needs 8 rows; below that the panes start under the tab bar.
func TestUsageRowHeight(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for h, want := range map[int]int{7: 1, 8: 2} {
		c := barClient(t, 80, h, &now)
		if got := c.ui.top(); got != want {
			t.Errorf("height %d: panes start at row %d, want %d", h, got, want)
		}
	}
}

// The tab bar: the warnings count, and the clock in the configured layout.
func TestTabBarWarningsAndClock(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 7, 0, time.UTC)
	c := barClient(t, 160, 30, &now, "one", "two")
	c.cfg.Bar.Clock = "15:04:05"
	c.ui.draw()
	row := canvasRow(c, 0)
	if !strings.Contains(row, "2 warnings") || !strings.Contains(row, "0/2 hosts") || !strings.Contains(row, "12:00:07") {
		t.Errorf("tab bar %q", row)
	}
	c = barClient(t, 160, 30, &now, "one")
	c.ui.draw()
	if row := canvasRow(c, 0); !strings.Contains(row, "1 warning ") {
		t.Errorf("tab bar %q", row)
	}
}

// A host's figures are labelled with it; while it is offline they dim and show their age. The
// info gets at most half the row.
func TestInfoOfflineAndWidth(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := barClient(t, 160, 30, &now, "one")
	cpu := 7
	c.ui.sys = &sysReading{stats: api.HostStats{CPUPct: &cpu}, host: "one", got: now.Add(-3 * time.Minute)}
	s, st := c.ui.infoText()
	if !strings.HasPrefix(s, "one cpu 7% ") || !st.Faint {
		t.Errorf("offline info %q, faint %v", s, st.Faint)
	}
	c.ui.sys = &sysReading{stats: api.HostStats{CPUPct: &cpu}}
	c.ui.draw()
	if b := canvasRow(c, 29); !strings.HasSuffix(strings.TrimRight(b, " "), "cpu 7%") {
		t.Errorf("wide bottom row %q", b)
	}
	c = barClient(t, 12, 30, &now)
	c.ui.sys = &sysReading{stats: api.HostStats{CPUPct: &cpu}}
	c.ui.draw()
	if b := canvasRow(c, 29); strings.Contains(b, "cpu") {
		t.Errorf("narrow bottom row %q: info over half the row", b)
	}
}

// The focused pane's host gives the figures when it offers host-stats, labelled with it; else
// this machine's, unlabelled.
func TestStatsHost(t *testing.T) {
	d := shortDir(t)
	for _, offer := range []bool{false, true} {
		h := fakeHab(t, d, "a")
		if offer {
			h.Capabilities = []string{"host-stats"}
			h.Stop()
			if err := h.Start(); err != nil {
				t.Fatal(err)
			}
		}
		conn, err := habconn.Connect(context.Background(), habconn.Config{Transport: habconn.UnixTransport{Path: h.Sock},
			Client: api.ClientInfo{Name: "gff", Version: "test", Kind: api.ClientKindTerminal}})
		if err != nil {
			t.Fatal(err)
		}
		set := hosts.Wrap("hab", conn, nil)
		c := testClient(t, 160, 30, "p")
		bar := DefaultBar()
		c.ws, c.cfg.Hosts, c.cfg.Bar, c.cfg.Keymap = true, set, &bar, DefaultKeymap()
		c.ui.init(c)
		want := ""
		if offer {
			want = "hab"
		}
		if got := c.ui.statsHost(); got != want {
			t.Errorf("host-stats offered %v: stats host %q, want %q", offer, got, want)
		}
		set.Close()
		conn.Close()
		h.Stop()
	}
}

// Disk figures render as the percentage an ordinary user sees in df.
func TestDiskInfoText(t *testing.T) {
	used, total := uint64(600*4096), uint64(950*4096)
	if got := infoText(api.HostStats{DiskUsed: used, DiskTotal: total}, []string{"disk"}); got != "disk 63%" {
		t.Errorf("disk %q, want df's 63%%", got)
	}
}

// Two accounts that would read alike, one harness with no label from two habs, are each named
// by their hab (app.md 14.5); an account alone is not.
func TestUsageNamesHab(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := barClient(t, 200, 30, &now, "one", "two")
	w := []api.UsageWindow{{Name: "5h", UsedPct: 10, ResetsAt: now.Add(time.Hour).Format(time.RFC3339), ReadAt: now.Format(time.RFC3339)}}
	c.ui.usage.put("one", api.UsageAccount{Key: "one/claude", Harness: "claude", Windows: w})
	c.ui.usage.put("two", api.UsageAccount{Key: "two/claude", Harness: "claude", Windows: w})
	c.ui.usage.put("two", api.UsageAccount{Key: "two/codex", Harness: "codex", Windows: w})
	c.ui.draw()
	row := canvasRow(c, 1)
	for _, want := range []string{" claude on one · 5h 10%", "│ claude on two · 5h 10%", "│ codex · 5h 10%"} {
		if !strings.Contains(row, want) {
			t.Errorf("usage row %q lacks %q", row, want)
		}
	}
}

// Outside the workspace no bar shows the mode, so the focused frame row does: a pane alone
// leaves pass-through while gff is in pane or locked mode, and returns to it after.
func TestModeShownOutsideWorkspace(t *testing.T) {
	c := testClient(t, 80, 24, "p")
	v := c.tab().views[0]
	c.frame(snapshotFrame(v, "hi"))
	c.render()
	if c.pass != v {
		t.Fatal("the pane does not pass through")
	}
	for _, step := range []struct{ keys, note string }{
		{"\x10", "pane · Esc/Enter back"},
		{"\x1b", ""},
		{"\x07", "locked · Ctrl+G unlock"},
		{"\x07", ""},
	} {
		if err := c.input([]byte(step.keys)); err != nil {
			t.Fatal(err)
		}
		c.render()
		if step.note == "" {
			if c.pass != v {
				t.Errorf("after %q: the pane does not pass through again", step.keys)
			}
			continue
		}
		if c.pass != nil || !strings.Contains(canvasRow(c, 0), " · "+step.note+" ") {
			t.Errorf("after %q: frame row %q, pass-through %v; want %q", step.keys, canvasRow(c, 0), c.pass != nil, step.note)
		}
	}
}

// A paste while the sidebar or a prompt has the keys runs no sidebar command: its text goes
// into a prompt, and elsewhere nowhere.
func TestPasteInSidebar(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := barClient(t, 160, 30, &now, "one")
	c.ui.mode = modeSidebar
	c.ui.keys([]byte("\x1b[200~xn/q\x1b[201~"))
	if c.ui.mode != modeSidebar || c.ui.prompt != nil || c.ui.filterOn {
		t.Fatalf("a paste ran sidebar keys: mode %v prompt %v filter %v", c.ui.mode, c.ui.prompt, c.ui.filterOn)
	}
	c.ui.ask(&prompt{label: "name", done: func(string, string) {}})
	c.ui.keys([]byte("a\x1b[200~b\x1b[A\r"))
	c.ui.keys([]byte("c\x1b[201~d"))
	if got := string(c.ui.prompt.text); got != "ab[Acd" {
		t.Errorf("prompt text %q, want %q", got, "ab[Acd")
	}
}

// An unknown CPU figure hides the item, as an absent battery does.
func TestInfoHidesUnknown(t *testing.T) {
	if got := infoText(api.HostStats{MemUsed: 1, MemTotal: 2}, []string{"cpu", "mem", "battery"}); got != "mem 50%" {
		t.Errorf("infoText %q, want mem only", got)
	}
}
