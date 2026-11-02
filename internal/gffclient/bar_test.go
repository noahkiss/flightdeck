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
// keymap (gff-client.md 8.1): the sidebar's keys while it has focus, the leaders in normal mode,
// session mode's keys after Ctrl+O, pane mode's keys through a focus move until a command
// ends it, tab mode's through a tab move until Enter, and only the unlock key while locked.
func TestBarFollowsMode(t *testing.T) {
	d := shortDir(t)
	a := fakeHab(t, d, "a")
	a.AddSession("alpha")
	hst := newHost(t, 240, 30, false)
	startWorkspace(t, hst, a.Sock)

	leaders := "Ctrl+G lock  Ctrl+P pane  Ctrl+T tab  Ctrl+O session  Alt+h/l/j/k move focus  Alt+i/o move tab"
	steps := []struct {
		keys      string
		want, not []string
	}{
		{"", []string{"↑/k/↓/j move", "Enter open", "n new", "/ filter", "Esc/q back"}, []string{"Ctrl+O session"}},
		{"\x1b", []string{leaders}, []string{"Enter open", "zoom"}},
		{"\x0f", []string{"session ·", "w sidebar", "g board", "r redraw", "d detach", "Ctrl+P pane"}, []string{"Ctrl+O session"}},
		// Ctrl+P switches from session mode to pane mode.
		{"\x10", []string{"pane ·", "p/Tab focus", "f zoom", "s split", "x close view", "Esc/Enter back", "Ctrl+T tab", "Ctrl+O session"},
			[]string{"w sidebar", "session ·", "Ctrl+P pane"}},
		{"p", []string{"pane ·", "p/Tab focus"}, nil}, // a focus move keeps pane mode
		{"f", []string{leaders}, []string{"pane ·"}},  // a command ends it
		{"\x14", []string{"tab ·", "n new tab", "h/←/k/↑ previous tab", "l/→/j/↓ next tab", "1-9 go to tab"}, []string{"f zoom"}},
		{"\x1b[C", []string{"tab ·"}, nil}, // a tab move keeps tab mode
		{"\r", []string{leaders}, []string{"tab ·"}},
		{"\x07", []string{"locked · Ctrl+G unlock"}, []string{"session", "zoom"}},
		{"\x07", []string{leaders}, []string{"locked"}},
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

[keys.session]
g = "none"
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
	hs := s.Keymap.hints(ModeSession)
	var all []string
	for _, h := range hs {
		all = append(all, h.full())
	}
	got := strings.Join(all, "  ")
	if strings.Contains(got, "board") {
		t.Errorf("unbound g still hinted: %s", got)
	}
	if !strings.Contains(got, "d/q detach") {
		t.Errorf("q not joined to detach: %s", got)
	}
	if s.Keymap.lookup(ModeNormal, "ctrl-a") != actZoom {
		t.Error("ctrl-a not bound in normal mode")
	}
	for _, want := range []string{
		p + `:8: [keys.session] k: unknown action "launch"`,
		p + `:9: [keys.session] x: action "open" does not work in session mode`,
		p + `:10: [keys.session] ctrl-m:`,
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

	// clear-defaults starts from no keys.
	s, _, err = LoadSettings(writeConfig(t, "[keys]\nclear-defaults = true\n[keys.normal]\n\"ctrl-a\" = \"session-mode\"\n[keys.session]\nd = \"detach\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if h := s.Keymap.hints(ModeSession); len(h) != 1 || h[0].full() != "d detach" {
		t.Errorf("defaults off: prefix hints %v", h)
	}
	k := newKeyReader(s.Keymap)
	if evs := k.feed([]byte("\x1c\x01d")); len(evs) != 3 || string(evs[0].data) != "\x1c" || evs[2].act != actDetach {
		t.Errorf("ctrl-a prefix: %q", evs)
	}

	// SetPrefix moves the leader; pressed twice, the new one sends itself and the old one does
	// nothing in session mode.
	km := DefaultKeymap()
	if p := km.SetPrefix("ctrl-a"); len(p) != 0 {
		t.Errorf("SetPrefix problems %q", p)
	}
	for _, m := range []Mode{ModeNormal, ModePane, ModeTab} {
		if km.lookup(m, "ctrl-a") != actSession || km.lookup(m, "ctrl-o") != "" {
			t.Errorf("SetPrefix did not move the prefix in %s mode", m)
		}
	}
	if evs := newKeyReader(km).feed([]byte("\x01\x01\x01\x0fx")); !reflect.DeepEqual(evs, []keyEvent{
		{act: actSession, key: "ctrl-a"}, {act: actSend, key: "ctrl-a"}, {act: actSession, key: "ctrl-a"}, {data: []byte("x")}}) {
		t.Errorf("ctrl-a twice, then ctrl-a and ctrl-o: %q", evs)
	}
	if _, _, err := LoadSettings(filepath.Join(t.TempDir(), "none.toml")); err != nil {
		t.Errorf("missing file: %v", err)
	}
}

// reachable lists the modes a keymap reaches from normal through its mode keys, and reports a
// mode other than normal that has no key back.
func reachable(t *testing.T, km *Keymap) map[Mode]bool {
	t.Helper()
	to := map[string]Mode{actSession: ModeSession, actPaneMode: ModePane, actTabMode: ModeTab, actLock: ModeLocked,
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

// The one-prefix sample (15.5, B) loads with no problems, reaches every mode, takes no Ctrl key
// from programs but its prefix, and replaces the default keymap: using it is a config change
// only. The default keymap reaches every mode too.
func TestPrefixKeymapSample(t *testing.T) {
	km := prefixKeymap(t)
	for name, km := range map[string]*Keymap{"sample": km, "default": DefaultKeymap()} {
		if got := reachable(t, km); len(got) != len(Modes) {
			t.Errorf("%s keymap reaches %v, want every mode", name, got)
		}
	}
	if bs := km.Bindings(ModeNormal); len(bs) != 1 || bs[0] != (Binding{`ctrl-\`, actSession}) {
		t.Errorf("sample normal bindings %v, want only Ctrl+\\: the default keymap leaked in", bs)
	}
	for _, m := range []Mode{ModePane, ModeTab} {
		for _, b := range km.Bindings(m) {
			if strings.HasPrefix(b.Key, "ctrl-") {
				t.Errorf("sample %s mode binds %s = %s: a default leader leaked in", m, b.Key, b.Action)
			}
		}
	}
	// What its header says: the prefix takes one key and returns; pressed twice it sends
	// itself; tab mode stays through a tab move and a digit ends it; locked passes the prefix.
	evs := newKeyReader(km).feed([]byte("\x1cw\x1c\x1c\x1ct][3x\x1cl\x1cz\x07"))
	pre := keyEvent{act: actSession, key: `ctrl-\`}
	want := []keyEvent{pre, {act: actSidebar, key: "w"}, pre, {act: actSend, key: `ctrl-\`},
		pre, {act: actTabMode, key: "t"}, {act: actNextTab, key: "]"}, {act: actPrevTab, key: "["}, {act: actTabN + "3", key: "3"},
		{data: []byte("x")}, pre, {act: actLock, key: "l"}, {data: []byte("\x1cz")}, {act: actUnlock, key: "ctrl-g"}}
	if !reflect.DeepEqual(evs, want) {
		t.Errorf("sample keys: %q, want %q", evs, want)
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
