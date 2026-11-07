package rounds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Watch triggers (rounds.md 4.4, 4.7) on a fake clock.

func (h *harness) watchRound(name string, edit func(*Round)) Entry {
	h.t.Helper()
	r := Round{Name: name, Mode: ModeFresh, Project: "proj", Command: []string{"true"},
		Trigger: Trigger{Kind: TriggerWatch, Watch: &Watch{Argv: []string{"mailwatch", "--from", "a@example.com"}}}}
	if edit != nil {
		edit(&r)
	}
	e, err := h.store.Create(r, "", h.clock.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

func (h *harness) lines(id string, ls ...string) []LineResult {
	h.t.Helper()
	var b [][]byte
	for _, l := range ls {
		b = append(b, []byte(l))
	}
	out, err := h.s.WatchLines(id, b)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func TestWatchDefinition(t *testing.T) {
	r := Round{Name: "w", Mode: ModeCapcom, Prompt: "look", Secrets: "mail",
		Trigger: Trigger{Kind: TriggerWatch, Watch: &Watch{Argv: []string{"mailwatch"}, Kinds: []string{"mail"}}}}
	if err := r.Normalize(testNow, "UTC", nil); err != nil {
		t.Fatal(err)
	}
	if r.Debounce != Duration(DefaultDebounce) || r.Cooldown != Duration(DefaultCooldown) {
		t.Errorf("batch timing %v %v", r.Debounce, r.Cooldown)
	}
	bad := []func(*Round){
		func(r *Round) { r.Trigger.Watch = nil },
		func(r *Round) { r.Trigger.Watch.Argv = nil },
		func(r *Round) { r.Trigger.Watch.Argv = []string{""} },
		func(r *Round) { r.Trigger.Watch.Cwd = "relative/dir" },
		func(r *Round) { r.Trigger.Watch.Kinds = []string{"a b"} },
		func(r *Round) { r.Secrets = "bad name" },
		func(r *Round) { r.Secrets = strings.Repeat("s", 65) },
		func(r *Round) { r.Trigger = Trigger{Kind: TriggerManual} }, // secrets for watch only
		func(r *Round) {
			r.Secrets = ""
			r.Trigger = Trigger{Kind: TriggerManual, Watch: &Watch{Argv: []string{"x"}}}
		},
	}
	for i, edit := range bad {
		r := Round{Name: "w", Mode: ModeCapcom, Prompt: "look", Secrets: "mail",
			Trigger: Trigger{Kind: TriggerWatch, Watch: &Watch{Argv: []string{"mailwatch"}}}}
		edit(&r)
		if err := r.Normalize(testNow, "UTC", nil); err == nil {
			t.Errorf("%d: accepted %+v", i, r)
		}
	}
	a := Trigger{Kind: TriggerWatch, Watch: &Watch{Argv: []string{"x"}}}
	b := Trigger{Kind: TriggerWatch, Watch: &Watch{Argv: []string{"x", "y"}}}
	if a.Equal(b) || !a.Equal(Trigger{Kind: TriggerWatch, Watch: &Watch{Argv: []string{"x"}}}) {
		t.Error("Equal ignores the watcher")
	}
	if w := TriggerWords("watch", "", "", "", ""); w != "a watcher" {
		t.Errorf("words %q", w)
	}
}

func TestParseWatchLine(t *testing.T) {
	now := utc("2026-03-02T10:00:00Z")
	lim := DefaultWatchLimits()
	l := ParseWatchLine([]byte(`{"kind":"mail","key":"1700000000:4521","title":"Invoice 1042 from billing@example.com","level":"active","at":"2026-03-02T09:59:00+01:00","data":{"from":"billing@example.com","subject":"Invoice 1042"}}`), now, lim)
	if l.Drop != "" || l.Event == nil || l.Key != "1700000000:4521" {
		t.Fatalf("%+v", l)
	}
	if e := l.Event; e.Kind != "mail" || e.Level != "active" || !e.At.Equal(utc("2026-03-02T08:59:00Z")) || !strings.Contains(string(e.Data), "billing@example.com") {
		t.Errorf("%+v", e)
	}
	if l := ParseWatchLine([]byte(`{"title":"t"}`), now, lim); l.Event == nil || !l.Event.At.Equal(now) {
		t.Errorf("default at: %+v", l)
	}
	if l := ParseWatchLine([]byte(`{"cursor":"1700000000:4521"}`), now, lim); l.Cursor == nil || *l.Cursor != "1700000000:4521" || l.Event != nil {
		t.Errorf("cursor: %+v", l)
	}
	small := lim
	small.TitleMax, small.DataMax, small.CursorMax = 20, 10, 64
	for _, c := range []struct {
		line string
		lim  WatchLimits
		want string
	}{
		{`not json`, lim, "not a JSON object"},
		{`[1,2]`, lim, "not a JSON object"},
		{`null`, lim, "not a JSON object"},
		{`{"kind":"mail"}`, lim, "no title"},
		{`{"title":"  "}`, lim, "no title"},
		{`{"title":5}`, lim, "title is not a string"},
		{`{"title":"a\nb"}`, lim, "more than one line"},
		{`{"title":"` + strings.Repeat("é", 201) + `"}`, lim, "title over 200"},
		{`{"title":"` + strings.Repeat("x", 21) + `"}`, small, "title over 20"},
		{`{"title":"t","data":{"k":"0123456789"}}`, small, "data over 10"},
		{`{"title":"t","data":"text"}`, lim, "data is not a JSON object"},
		{`{"title":"t","level":"loud"}`, lim, "level"},
		{`{"title":"t","at":"yesterday"}`, lim, "RFC 3339"},
		{`{"title":"t","kind":"a b"}`, lim, "kind"},
		{`{"cursor":"` + strings.Repeat("c", 65) + `"}`, small, "cursor over 64"},
		{`{"cursor":7}`, lim, "cursor is not a string"},
		{`{"title":"` + strings.Repeat("x", MaxWatchLine) + `"}`, lim, "line over 64 KiB"},
		{"{\"title\":\"\xff\"}", lim, "not UTF-8"},
	} {
		got := ParseWatchLine([]byte(c.line), now, c.lim)
		if got.Event != nil || got.Cursor != nil || !strings.Contains(got.Drop, c.want) {
			t.Errorf("%.40q: %+v, want a drop with %q", c.line, got, c.want)
		}
	}
}

func TestBackoff(t *testing.T) {
	lim := DefaultWatchLimits()
	t0 := utc("2026-03-02T10:00:00Z")
	var b Backoff
	var delays []time.Duration
	pause := false
	start := t0
	for i := 0; i < 5; i++ {
		d, p := b.Exit(start, start.Add(time.Second), false, lim)
		delays = append(delays, d)
		pause = p
		start = start.Add(time.Second + d)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}; !equalDurations(delays, want) {
		t.Errorf("delays %v, want %v", delays, want)
	}
	if !pause {
		t.Error("5 quick exits did not pause")
	}
	// gff-hab's own hang-ups count for nothing.
	b = Backoff{}
	for i := 0; i < 10; i++ {
		if d, p := b.Exit(t0, t0.Add(time.Second), true, lim); d != 0 || p {
			t.Fatalf("own hang-up: %v %v", d, p)
		}
	}
	if b != (Backoff{}) {
		t.Errorf("own hang-ups changed the backoff: %+v", b)
	}
	// A watcher that ran past the reset starts again at 1 s; a slow exit resets the count.
	b = Backoff{Next: 2 * time.Minute, Quick: 3}
	if d, p := b.Exit(t0, t0.Add(11*time.Minute), false, lim); d != time.Second || p || b.Quick != 0 {
		t.Errorf("after the reset: %v %v %+v", d, p, b)
	}
	// The cap, and a lower pause count.
	lim.BackoffMax, lim.PauseAfter = 10*time.Second, 2
	b = Backoff{}
	delays = nil
	for i := 0; i < 6; i++ {
		d, p := b.Exit(t0, t0.Add(2*time.Minute), false, lim) // slow: never a pause
		if p {
			t.Fatal("slow exits paused")
		}
		delays = append(delays, d)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}; !equalDurations(delays, want) {
		t.Errorf("capped delays %v, want %v", delays, want)
	}
	b = Backoff{}
	if _, p := b.Exit(t0, t0.Add(time.Second), false, lim); p {
		t.Error("one quick exit paused")
	}
	if _, p := b.Exit(t0, t0.Add(time.Second), false, lim); !p {
		t.Error("two quick exits with watch_pause_after 2 did not pause")
	}
	b.Reset()
	if b != (Backoff{}) {
		t.Error("Reset")
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWatchLinesBatchAndRun(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle = -1
	h.reopen()
	e := h.watchRound("mail", func(r *Round) { r.Trigger.Watch.Kinds = []string{"mail"} })
	h.s.Tick()
	res := h.lines(e.Round.ID,
		`{"kind":"mail","key":"k1","title":"one"}`,
		`{"kind":"mail","key":"k1","title":"one again"}`,
		`{"kind":"news","title":"not counted"}`,
		`{"title":"no kind"}`,
		`garbage`,
		`{"cursor":"c1"}`,
		`{"kind":"mail","key":"k2","title":"two"}`,
	)
	want := []LineResult{{Taken: true, Title: "one"}, {Title: "one again", Drop: DropKey, Quiet: true},
		{Title: "not counted", Drop: DropKind, Quiet: true}, {Title: "no kind", Drop: DropKind, Quiet: true},
		{Drop: "not a JSON object"}, {Cursor: true}, {Taken: true, Title: "two"}}
	if len(res) != len(want) {
		t.Fatalf("%+v", res)
	}
	for i := range want {
		if res[i] != want[i] {
			t.Errorf("line %d: %+v, want %+v", i, res[i], want[i])
		}
	}
	if c := h.s.WatchCursor(e.Round.ID); c != "c1" {
		t.Errorf("cursor %q", c)
	}
	h.tickAfter(30 * time.Second)
	runs := h.runs(e.Round.ID)
	if len(runs) != 1 || runs[0].Trigger != TriggerWatch || runs[0].Events != 2 {
		t.Fatalf("runs %+v", runs)
	}
	if got := titles(h.batchOf(runs[0])); strings.Join(got, ",") != "one,two" {
		t.Errorf("batch %v", got)
	}
	// The run's write saved the cursor and the keys.
	st := h.entry(e.Round.ID).State
	if st.Watch == nil || st.Watch.Cursor != "c1" || len(st.Watch.Keys) != 2 || st.LastEventStart == nil {
		t.Errorf("saved state %+v", st)
	}
	// A key past the window counts again.
	h.end(runs[0], RunSucceeded, "", false)
	h.clock.move(25*time.Hour, 25*time.Hour)
	if r := h.lines(e.Round.ID, `{"kind":"mail","key":"k1","title":"one later"}`); !r[0].Taken {
		t.Errorf("a key past the window: %+v", r)
	}
	// Run now takes the pending batch.
	run, err := h.s.RunNow(e.Round.ID)
	if err != nil || run.Events != 1 {
		t.Errorf("run now: %+v %v", run, err)
	}
}

func TestWatchStateSaveAndRestart(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle = -1
	h.limits = Limits{StateSaveEvery: 10 * time.Second}
	h.reopen()
	e := h.watchRound("mail", nil)
	h.s.Tick()
	h.lines(e.Round.ID, `{"cursor":"c1"}`) // the first save is at once
	if st := h.entry(e.Round.ID).State; st.Watch == nil || st.Watch.Cursor != "c1" {
		t.Fatalf("first save %+v", st.Watch)
	}
	h.clock.move(time.Second, time.Second)
	h.lines(e.Round.ID, `{"cursor":"c2"}`, `{"key":"k","title":"t"}`)
	if st := h.entry(e.Round.ID).State; st.Watch.Cursor != "c1" {
		t.Errorf("saved again within state_save_every: %+v", st.Watch)
	}
	if c := h.s.WatchCursor(e.Round.ID); c != "c2" {
		t.Errorf("memory cursor %q", c)
	}
	h.s.FlushPending()
	if st := h.entry(e.Round.ID).State; st.Watch.Cursor != "c2" || len(st.Watch.Keys) != 1 || st.Pending.Len() != 1 {
		t.Errorf("flushed %+v %+v", st.Watch, st.Pending)
	}
	// A restart keeps the cursor, the keys and the batch; a repeated key is still dropped.
	h.reopen()
	if c := h.s.WatchCursor(e.Round.ID); c != "c2" {
		t.Errorf("cursor after a restart %q", c)
	}
	if r := h.lines(e.Round.ID, `{"key":"k","title":"t again"}`); r[0].Drop != DropKey {
		t.Errorf("repeat after a restart: %+v", r)
	}
	// Lower limits apply at the next start: the cursor over the bound goes, the keys are cut.
	h.s.FlushPending()
	h.watch = &WatchLimits{CursorMax: 1, KeysMax: 0, KeyWindow: time.Hour, TitleMax: 200, DataMax: 100}
	h.reopen()
	if c := h.s.WatchCursor(e.Round.ID); c != "" {
		t.Errorf("a cursor over a lowered bound kept: %q", c)
	}
	// The state file holds no line's text it was not asked to keep: only the cursor and keys.
	b, err := os.ReadFile(filepath.Join(h.dir, "state.json"))
	if err == nil && strings.Contains(string(b), "t again") {
		t.Error("a dropped line reached the state file")
	}
}

func TestWatchLinesRefusals(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	if _, err := h.s.WatchLines("round-none", nil); err != ErrNotFound {
		t.Errorf("gone: %v", err)
	}
	c := h.round("cron", "0 9 * * *", nil)
	if _, err := h.s.WatchLines(c.Round.ID, nil); err != ErrNotWatch {
		t.Errorf("cron: %v", err)
	}
	e := h.watchRound("w", nil)
	if err := h.s.Pause(e.Round.ID); err != nil {
		t.Fatal(err)
	}
	if r := h.lines(e.Round.ID, `{"title":"t"}`, `{"cursor":"c"}`); r[0].Drop != DropPaused || !r[1].Cursor {
		t.Errorf("paused: %+v", r)
	}
	// An edit away from watch drops the state.
	ent := h.entry(e.Round.ID)
	r := ent.Round
	r.Trigger, r.Debounce, r.Cooldown = Trigger{Kind: TriggerManual}, 0, 0
	if _, err := h.s.Update(r); err != nil {
		t.Fatal(err)
	}
	if c := h.s.WatchCursor(e.Round.ID); c != "" {
		t.Errorf("cursor kept past an edit away from watch: %q", c)
	}
}

func TestPauseWatcher(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	e := h.watchRound("w", func(r *Round) { r.Notify = NotifyNever })
	if err := h.s.PauseWatcher(e.Round.ID, "exit status 3", "sess-1", 5); err != nil {
		t.Fatal(err)
	}
	ent := h.entry(e.Round.ID)
	if !ent.Round.Paused || ent.State.PausedReason != ReasonWatcherPause {
		t.Errorf("%+v", ent)
	}
	ns := h.sink.noticesOf(NoticePaused)
	if len(ns) != 1 || ns[0].Exit != "exit status 3" || ns[0].Session != "sess-1" || ns[0].Exits != 5 {
		t.Fatalf("notices %+v", ns)
	}
	// Paused already: no second notice.
	h.s.PauseWatcher(e.Round.ID, "exit status 3", "sess-1", 5)
	if n := len(h.sink.noticesOf(NoticePaused)); n != 1 {
		t.Errorf("%d notices", n)
	}
	if err := h.s.Unpause(e.Round.ID); err != nil || h.entry(e.Round.ID).State.PausedReason != "" {
		t.Errorf("unpause: %v", err)
	}
}

func TestSeedCursor(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	e := h.watchRound("w", nil)
	if err := h.s.SeedCursor(e.Round.ID, "carried"); err != nil {
		t.Fatal(err)
	}
	if st := h.entry(e.Round.ID).State; st.Watch == nil || st.Watch.Cursor != "carried" {
		t.Errorf("%+v", st.Watch)
	}
	if err := h.s.SeedCursor(e.Round.ID, strings.Repeat("c", 2000)); err == nil {
		t.Error("an oversize cursor seeded")
	}
}
