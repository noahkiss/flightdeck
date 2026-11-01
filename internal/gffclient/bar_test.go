package gffclient

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// bottom is the screen's last row.
func bottom(screen string) string {
	rows := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	return rows[len(rows)-1]
}

// TestBarFollowsMode: the shortcut bar shows the keys of the mode gff is in, from the loaded
// keymap (gff-client.md 8.1): the sidebar's keys while it has focus, the prefix in normal mode,
// the prefix mode's commands after the prefix, pane mode's keys until Esc, and only the unlock
// key while locked.
func TestBarFollowsMode(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	a.AddSession("alpha")
	hst := newHost(t, 160, 30, false)
	startWorkspace(t, hst, a.Sock)

	steps := []struct {
		keys      string
		want, not []string
	}{
		{"", []string{"↑/k/↓/j move", "Enter open", "n new", "/ filter", "Esc/q back"}, []string{"Ctrl+\\ commands"}},
		{"\x1b", []string{"Ctrl+\\ commands"}, []string{"Enter open", "zoom"}},
		{prefix, []string{"prefix ·", "w sidebar", "g board", "p pane", "z zoom"}, []string{"Ctrl+\\ commands"}},
		{"p", []string{"pane ·", "o/Tab focus", "z zoom", "s split", "Esc/Enter back"}, []string{"w sidebar", "prefix ·"}},
		{"z", []string{"pane ·", "z zoom"}, nil}, // pane mode stays after a command
		{"\x1b", []string{"Ctrl+\\ commands"}, []string{"pane ·"}},
		{prefix + "t", []string{"tab ·", "c new tab", "] next tab", "1-9 go to tab"}, []string{"z zoom"}},
		{"\r", []string{"Ctrl+\\ commands"}, []string{"tab ·"}},
		{prefix + "l", []string{"locked · Ctrl+G unlock"}, []string{"commands", "zoom"}},
		{"\x07", []string{"Ctrl+\\ commands"}, []string{"locked"}},
	}
	for _, s := range steps {
		hst.typ(s.keys)
		hst.waitScreen("bar after "+strings.Join(s.want, ", "), func(sc string) bool {
			b := bottom(sc)
			for _, w := range s.want {
				if !strings.Contains(b, w) {
					return false
				}
			}
			for _, n := range s.not {
				if strings.Contains(b, n) {
					return false
				}
			}
			return true
		})
	}
}

// writeConfig writes a gff config file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// An unbound key leaves the bar; a new binding joins it; a bad line is a problem naming its
// file and line, and the rest still loads (gff-client.md 8.1, 8.3, 12).
func TestKeymapConfig(t *testing.T) {
	p := writeConfig(t, `[[hosts]]
label = "a"
endpoints = ["/tmp/a.sock"]

[keys.prefix]
z = "none"
q = "detach"
k = "launch"
x = "open"
"ctrl-m" = "zoom"

[keys.normal]
"ctrl-a" = "zoom"
a = "zoom"

[keys.nope]
a = "zoom"

[bar]
max_hints = 4
clock = ""
info = ["cpu", "temp"]
`)
	s, problems, err := LoadSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	hs := s.Keymap.hints(ModePrefix)
	var all []string
	for _, h := range hs {
		all = append(all, h.full())
	}
	got := strings.Join(all, "  ")
	if strings.Contains(got, "zoom") {
		t.Errorf("unbound z still hinted: %s", got)
	}
	if !strings.Contains(got, "d/q detach") {
		t.Errorf("q not joined to detach: %s", got)
	}
	if s.Keymap.lookup(ModeNormal, "ctrl-a") != actZoom {
		t.Error("ctrl-a not bound in normal mode")
	}
	for _, want := range []string{
		p + `:8: [keys.prefix] k: unknown action "launch"`,
		p + `:9: [keys.prefix] x: action "open" does not work in prefix mode`,
		p + `:10: [keys.prefix] ctrl-m:`,
		p + `:14: [keys.normal] a: normal mode takes only ctrl-<key>`,
		p + `: [keys.nope]: unknown mode`,
		`unknown info item "temp"`,
	} {
		if !slices.ContainsFunc(problems, func(s string) bool { return strings.Contains(s, want) }) {
			t.Errorf("no problem %q in %q", want, problems)
		}
	}
	if s.Bar.MaxHints != 4 || s.Bar.Clock != "" || !slices.Equal(s.Bar.Info, []string{"cpu"}) || !s.Bar.Usage {
		t.Errorf("bar = %+v", s.Bar)
	}
	// The bar caps the hints at max_hints.
	if bar := fitHints(hs, 200, s.Bar.MaxHints); strings.Count(bar, "  ") != 3 {
		t.Errorf("max_hints 4 shows %q", bar)
	}

	// defaults = false starts from no keys.
	s, _, err = LoadSettings(writeConfig(t, "[keys]\ndefaults = false\n[keys.normal]\n\"ctrl-a\" = \"prefix\"\n[keys.prefix]\nd = \"detach\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if h := s.Keymap.hints(ModePrefix); len(h) != 1 || h[0].full() != "d detach" {
		t.Errorf("defaults off: prefix hints %v", h)
	}
	k := newKeyReader(s.Keymap)
	if evs := k.feed([]byte("\x1c\x01d")); len(evs) != 3 || string(evs[0].data) != "\x1c" || evs[2].act != actDetach {
		t.Errorf("ctrl-a prefix: %q", evs)
	}

	// SetPrefix moves the leader; pressed twice, the new one sends itself and the old one does
	// nothing in prefix mode.
	km := DefaultKeymap()
	if p := km.SetPrefix("ctrl-a"); len(p) != 0 {
		t.Errorf("SetPrefix problems %q", p)
	}
	if km.lookup(ModeNormal, "ctrl-a") != actPrefix || km.lookup(ModeNormal, `ctrl-\`) != "" {
		t.Error("SetPrefix did not move the prefix")
	}
	if evs := newKeyReader(km).feed([]byte("\x01\x01\x01\x1cx")); !reflect.DeepEqual(evs, []keyEvent{
		{act: actPrefix, key: "ctrl-a"}, {act: actSend, key: "ctrl-a"}, {act: actPrefix, key: "ctrl-a"}, {data: []byte("x")}}) {
		t.Errorf("ctrl-a twice, then ctrl-a and ctrl-\\: %q", evs)
	}
	if _, _, err := LoadSettings(filepath.Join(t.TempDir(), "none.toml")); err != nil {
		t.Errorf("missing file: %v", err)
	}
}

// reachable lists the modes a keymap reaches from normal through its mode keys, and reports a
// mode other than normal that has no key back.
func reachable(t *testing.T, km *Keymap) map[Mode]bool {
	t.Helper()
	to := map[string]Mode{actPrefix: ModePrefix, actPaneMode: ModePane, actTabMode: ModeTab, actLock: ModeLocked,
		actSidebar: ModeSidebar, actBoard: ModeBoard}
	seen := map[Mode]bool{ModeNormal: true}
	queue := []Mode{ModeNormal}
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		out := false
		for _, b := range km.Bindings(m) {
			if b.Action == actBack || b.Action == actUnlock {
				out = true
			}
			if n, ok := to[b.Action]; ok && !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
		if m != ModeNormal && !out {
			t.Errorf("%s mode has no key back", m)
		}
	}
	return seen
}

// The sample keymap of modes with Ctrl leaders (15.5, A) loads with no problems, reaches every
// mode, keeps the keys 8.1 and 15.5 leave to programs, and replaces the default keymap: using it
// is a config change only. The default keymap reaches every mode too.
func TestSampleKeymapModes(t *testing.T) {
	s, problems, err := LoadSettings(filepath.Join("..", "..", "docs", "examples", "gff-keys-modes.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Errorf("problems: %q", problems)
	}
	for name, km := range map[string]*Keymap{"sample": s.Keymap, "default": DefaultKeymap()} {
		if got := reachable(t, km); len(got) != len(Modes) {
			t.Errorf("%s keymap reaches %v, want every mode", name, got)
		}
	}
	for _, k := range []string{"ctrl-o", "ctrl-s", "ctrl-p", "ctrl-n", "ctrl-b"} {
		if a := s.Keymap.lookup(ModeNormal, k); a != "" {
			t.Errorf("sample binds %s to %s in normal mode", k, a)
		}
	}
	if a := s.Keymap.lookup(ModeNormal, `ctrl-\`); a != actPaneMode {
		t.Errorf(`sample ctrl-\ = %q, want pane-mode: the default keymap leaked in`, a)
	}
	if h := s.Keymap.hints(ModeNormal); len(h) != 4 {
		t.Errorf("sample normal hints %v, want the four leaders", h)
	}
	// The leaders drive the reader: Ctrl+T enters tab mode, 3 goes to tab 3, Esc leaves; Ctrl+G
	// locks, and the pane leader then reaches the program.
	k := newKeyReader(s.Keymap)
	evs := k.feed([]byte("\x143\x1b\x07\x1cz\x07"))
	want := []keyEvent{{act: actTabMode, key: "ctrl-t"}, {act: actTabN + "3", key: "3"}, {act: actBack, key: "esc"},
		{act: actLock, key: "ctrl-g"}, {data: []byte("\x1cz")}, {act: actUnlock, key: "ctrl-g"}}
	if !reflect.DeepEqual(evs, want) {
		t.Errorf("sample keys: %q, want %q", evs, want)
	}
	// What its header says: each of Ctrl+\, Ctrl+T and Ctrl+] pressed twice sends itself; Ctrl+]
	// then Ctrl+G sends Ctrl+G; Ctrl+G twice locks and unlocks, sending nothing.
	evs = newKeyReader(s.Keymap).feed([]byte("\x1c\x1c\x1b\x14\x14\x1b\x1d\x1d\x1d\x07\x07\x07"))
	want = []keyEvent{{act: actPaneMode, key: `ctrl-\`}, {act: actSend, key: `ctrl-\`}, {act: actBack, key: "esc"},
		{act: actTabMode, key: "ctrl-t"}, {act: actSend, key: "ctrl-t"}, {act: actBack, key: "esc"},
		{act: actPrefix, key: "ctrl-]"}, {act: actSend, key: "ctrl-]"},
		{act: actPrefix, key: "ctrl-]"}, {act: actSend, key: "ctrl-g"},
		{act: actLock, key: "ctrl-g"}, {act: actUnlock, key: "ctrl-g"}}
	if !reflect.DeepEqual(evs, want) {
		t.Errorf("sample sends: %q, want %q", evs, want)
	}
}

// When the bar is short, hints drop from the lowest priority; with fewer than three left, the
// keys show alone.
func TestFitHints(t *testing.T) {
	hs := []hint{{"w", "sidebar"}, {"g", "board"}, {"p", "pane"}, {"z", "zoom"}}
	for _, tc := range []struct {
		w    int
		want string
	}{
		{80, " w sidebar  g board  p pane  z zoom"},
		{30, " w sidebar  g board  p pane"},
		{20, " w  g  p  z"},
		{6, " w  g"},
		{1, ""},
	} {
		if got := fitHints(hs, tc.w, 0); got != tc.want {
			t.Errorf("width %d: %q, want %q", tc.w, got, tc.want)
		}
	}
}

// usageFixture is a reading as a hab sends it: the 5h window 42% used with 1h 58m to go (3h 2m
// of 5h gone: pace 60%), the 7d window 61% used with 4d 23h to go (1d 1h of 7d gone: pace 14%).
func usageFixture(now time.Time) api.UsageAccount {
	read := now.Add(-time.Minute).Format(time.RFC3339)
	return api.UsageAccount{Key: "k1", Label: "Max 5x", Harness: "claude", Windows: []api.UsageWindow{
		{Name: "5h", UsedPct: 42, ResetsAt: now.Add(time.Hour + 58*time.Minute).Format(time.RFC3339), ReadAt: read},
		{Name: "7d", UsedPct: 61, ResetsAt: now.Add(4*24*time.Hour + 23*time.Hour).Format(time.RFC3339), ReadAt: read},
	}}
}

// A fixture usage reading renders the expected pace text (app.md 14.2, 14.5).
func TestUsagePaceText(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fx := usageFixture(now)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	read := at(-time.Minute)
	for _, tc := range []struct {
		name  string
		acct  api.UsageAccount
		want  string
		level usageLevel
		stale bool
	}{
		{"over pace", fx, "claude Max 5x · 5h 42% resets 1h 58m · 7d 61% resets 4d 23h · over pace on 7d", 0, false},
		{"on pace", api.UsageAccount{Harness: "codex", Windows: []api.UsageWindow{
			{Name: "5h", UsedPct: 10, ResetsAt: at(time.Hour), ReadAt: read}}},
			"codex · 5h 10% resets 1h 0m · on pace", 0, false},
		{"limit reached", api.UsageAccount{Harness: "claude", Windows: []api.UsageWindow{
			{Name: "5h", UsedPct: 104, ResetsAt: at(time.Hour + 58*time.Minute), ReadAt: read},
			{Name: "7d", UsedPct: 90, ResetsAt: at(time.Hour), ReadAt: read}}},
			"claude · 5h 100% resets 1h 58m · 7d 90% resets 1h 0m · 5h limit reached · resets in 1h 58m", 2, false},
		{"near a limit", api.UsageAccount{Harness: "claude", Windows: []api.UsageWindow{
			{Name: "5h", UsedPct: 85, ResetsAt: at(3 * time.Hour), ReadAt: read}}},
			"claude · 5h 85% resets 3h 0m · over pace on 5h", 1, false},
		{"model window has no pace", api.UsageAccount{Harness: "claude", Windows: []api.UsageWindow{
			{Name: "opus", UsedPct: 20, ResetsAt: at(time.Hour), ReadAt: read}}},
			"claude · opus 20% resets 1h 0m", 0, false},
		{"no reset time", api.UsageAccount{Harness: "codex", Windows: []api.UsageWindow{
			{Name: "5h", UsedPct: 20, ReadAt: read}}},
			"codex · 5h 20%", 0, false},
		{"reset with no reading since", api.UsageAccount{Harness: "claude", Windows: []api.UsageWindow{
			{Name: "5h", UsedPct: 70, ResetsAt: at(-10 * time.Minute), ReadAt: at(-20 * time.Minute)}}},
			"claude · 5h reset at 11:50; no reading since", 0, false},
		// A stale window has no pace: its pace would rise with now while its figure stays old.
		{"stale", api.UsageAccount{Harness: "claude", Windows: []api.UsageWindow{
			{Name: "7d", UsedPct: 5, ResetsAt: at(6 * 24 * time.Hour), ReadAt: at(-2 * time.Hour)}}},
			"claude · 7d 5% resets 6d 0h · as of 10:00", 0, true},
		{"stale for its window, inside the hour", api.UsageAccount{Harness: "codex", Windows: []api.UsageWindow{
			{Name: "30m", UsedPct: 10, ResetsAt: at(10 * time.Minute), ReadAt: at(-45 * time.Minute)}}},
			"codex · 30m 10% resets 10m · as of 11:15", 0, true},
		// 2h of 5h gone: pace 40%. Used at the pace is on pace, not over.
		{"used equals pace", api.UsageAccount{Harness: "codex", Windows: []api.UsageWindow{
			{Name: "5h", UsedPct: 40, ResetsAt: at(3 * time.Hour), ReadAt: read}}},
			"codex · 5h 40% resets 3h 0m · on pace", 0, false},
		{"a reading after the reset", api.UsageAccount{Harness: "claude", Windows: []api.UsageWindow{
			{Name: "5h", UsedPct: 3, ResetsAt: at(-10 * time.Minute), ReadAt: at(-5 * time.Minute)}}},
			"claude · 5h 3% resets under 1m", 0, false},
		{"no windows", api.UsageAccount{Harness: "claude", Label: "API"}, "claude API · no limits reported", 0, false},
	} {
		v := usageText(tc.acct, "", now)
		if v.text != tc.want || v.level != tc.level || v.stale != tc.stale {
			t.Errorf("%s: %q level %d stale %v, want %q level %d stale %v", tc.name, v.text, v.level, v.stale, tc.want, tc.level, tc.stale)
		}
	}
}

// Readings of one account from two hosts merge: the newest wins, whichever host sent it.
func TestUsageBookNewestWins(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	newer := usageFixture(now)
	older := usageFixture(now.Add(-time.Hour))
	older.Windows[0].UsedPct = 1
	var b usageBook
	b.put("one", newer)
	b.put("two", older)
	if es := b.entries(); len(es) != 1 || es[0].host != "one" || es[0].acct.Windows[0].UsedPct != 42 {
		t.Errorf("older reading replaced the newer: %+v", es)
	}
}

// The usage row says so when no host reports usage: absent, never zero (task acceptance). With
// a reading it shows the account's pace text, and the counts per harness on the right.
func TestUsageRow(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	_, busy, _ := a.AddSession("busy")
	a.SetAgent(busy, api.AgentStatusWorking, "")
	hst := newHost(t, 160, 30, false)
	startWorkspace(t, hst, a.Sock)
	hst.waitScreen("no usage, one working", func(s string) bool {
		return line(s, "no usage reported") == 1 && line(s, "claude 1 working") == 1
	})
}

// System info: the formatter over fixed figures; the battery hides on a machine with none.
func TestInfoText(t *testing.T) {
	bat, cpu := 80, 12
	s := api.HostStats{CPUPct: &cpu, MemUsed: 8 << 30, MemTotal: 16 << 30, DiskUsed: 61, DiskTotal: 100, BatteryPct: &bat,
		UptimeS: uint64((3*24*time.Hour + 4*time.Hour) / time.Second)}
	if got, want := infoText(s, infoItems), "cpu 12% mem 50% disk 61% bat 80% up 3d 4h"; got != want {
		t.Errorf("infoText = %q, want %q", got, want)
	}
	s.BatteryPct = nil
	if got, want := infoText(s, []string{"battery", "cpu"}), "cpu 12%"; got != want {
		t.Errorf("no battery: %q, want %q", got, want)
	}
	var l LocalStats
	if st, err := l.Read(); err != nil || st.At == "" {
		t.Errorf("local stats: %+v, %v", st, err)
	}
}

// canvasRow is row y of the canvas as text.
func canvasRow(c *client, y int) string {
	var b strings.Builder
	for x := 0; x < c.canvas.w; x++ {
		t := c.canvas.next[y*c.canvas.w+x].text
		if t == "" {
			t = " "
		}
		b.WriteString(t)
	}
	return b.String()
}

// The usage row shows a fixture reading's pace text under the tab bar, in the warning colour
// when a window is near its limit.
func TestUsageRowReading(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	c := testClient(t, 160, 30)
	set, err := hosts.Open(hosts.Config{Source: hosts.Static([]hosts.Spec{{Label: "one",
		Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: filepath.Join(t.TempDir(), "none.sock")}}}})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(set.Close)
	bar := DefaultBar()
	c.ws, c.cfg.Hosts, c.cfg.Bar, c.cfg.Keymap, c.cfg.Now = true, set, &bar, DefaultKeymap(), func() time.Time { return now }
	c.ui.init(c)
	c.ui.usage.put("one", usageFixture(now))
	hot := api.UsageAccount{Key: "k2", Harness: "codex", Windows: []api.UsageWindow{
		{Name: "5h", UsedPct: 96, ResetsAt: now.Add(time.Hour).Format(time.RFC3339), ReadAt: now.Format(time.RFC3339)}}}
	c.ui.usage.put("one", hot)
	c.ui.draw()
	row := []rune(canvasRow(c, 1))
	want := " claude Max 5x · 5h 42% resets 1h 58m · 7d 61% resets 4d 23h · over pace on 7d │ codex · 5h 96% resets 1h 0m · over pace on 5h"
	if !strings.HasPrefix(string(row), want) {
		t.Errorf("usage row %q, want it to start %q", string(row), want)
	}
	if st := c.canvas.next[1*c.canvas.w+1].style; st != usageStyles[0] {
		t.Errorf("claude's style %+v, want plain", st)
	}
	cx := slices.Index(row, '│') + 2
	if st := c.canvas.next[1*c.canvas.w+cx].style; st != usageStyles[2] {
		t.Errorf("codex at 96%% style %+v, want the limit colour", st)
	}
	if !strings.Contains(canvasRow(c, 0), "12:00") || c.ui.paneArea().y != 2 {
		t.Errorf("tab bar %q, pane area %+v", canvasRow(c, 0), c.ui.paneArea())
	}
}
