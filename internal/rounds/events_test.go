package rounds

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// Event triggers and batches (rounds.md 4.3, 4.5, 4.7) on a fake clock.

func (f *fakeSink) Woke(d time.Duration) { f.mu.Lock(); f.woke = append(f.woke, d); f.mu.Unlock() }

func (f *fakeSink) wokeList() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.woke...)
}

// eventRound creates an event round on h's store.
func (h *harness) eventRound(name, ev string, edit func(*Round)) Entry {
	h.t.Helper()
	r := Round{Name: name, Mode: ModeFresh, Project: "proj", Command: []string{"true"},
		Trigger: Trigger{Kind: TriggerEvent, Event: ev}}
	if edit != nil {
		edit(&r)
	}
	e, err := h.store.Create(r, "", h.clock.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

func (h *harness) add(id string, titles ...string) {
	h.t.Helper()
	var evs []Event
	for _, t := range titles {
		evs = append(evs, Event{At: h.clock.Now(), Kind: EventNoticeRaised, Title: t, Level: "active",
			Data: json.RawMessage(`{"n":"` + t + `"}`)})
	}
	if err := h.s.AddEvents(id, evs); err != nil {
		h.t.Fatal(err)
	}
}

// tickAfter moves awake time on by d and ticks.
func (h *harness) tickAfter(d time.Duration) {
	h.clock.move(d, d)
	h.s.Tick()
}

func (h *harness) batchOf(run Run) Batch {
	h.t.Helper()
	b, err := h.store.ReadEvents(run.RoundID, run.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

func titles(b Batch) []string {
	var out []string
	for _, e := range b.Events {
		out = append(out, e.Title)
	}
	return out
}

func TestEventRoundDefaults(t *testing.T) {
	for _, mode := range []Mode{ModeFresh, ModeCapcom, ModeGroup} {
		r := Round{Name: "ev", Mode: mode, Prompt: "look", Trigger: Trigger{Kind: TriggerEvent, Event: EventHabWoke}}
		switch mode {
		case ModeFresh:
			r.Harness, r.Project = "claude", "p"
		case ModeGroup:
			r.Group = "g1"
		}
		if err := r.Normalize(testNow, "UTC", nil); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if r.Debounce != Duration(30*time.Second) || r.Cooldown != Duration(5*time.Minute) || r.Trigger.Zone != "" {
			t.Errorf("%s: %+v", mode, r)
		}
		if mode == ModeGroup && r.Wait != Duration(DefaultWait) {
			t.Errorf("group wait %v", r.Wait)
		}
		if mode.Messages() && r.OnFinish != "" {
			t.Errorf("%s on_finish %q", mode, r.OnFinish)
		}
	}
	// A message round takes no session fields.
	for _, edit := range []func(*Round){
		func(r *Round) { r.Harness = "claude" }, func(r *Round) { r.Project = "p" }, func(r *Round) { r.Target = "s" },
		func(r *Round) { r.Command = []string{"x"} }, func(r *Round) { r.OnFinish = OnFinishKeep },
		func(r *Round) { r.Prompt = "" }, func(r *Round) { r.Mode = ModeGroup },
	} {
		r := Round{Name: "c", Mode: ModeCapcom, Prompt: "look", Trigger: Trigger{Kind: TriggerManual}}
		edit(&r)
		if err := r.Normalize(testNow, "UTC", nil); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestEventDebounceCooldownCoalesce(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle = -1
	h.reopen()
	e := h.eventRound("ev", EventNoticeRaised, nil)
	h.s.Tick()
	h.add(e.Round.ID, "a")
	h.tickAfter(10 * time.Second)
	h.add(e.Round.ID, "b")
	h.tickAfter(19 * time.Second)
	if n := len(h.runs(e.Round.ID)); n != 0 {
		t.Fatalf("a run before the debounce: %d", n)
	}
	h.tickAfter(time.Second) // 30 s after the first event
	runs := h.runs(e.Round.ID)
	if len(runs) != 1 || runs[0].Events != 2 || runs[0].Trigger != TriggerEvent || runs[0].State != RunRunning {
		t.Fatalf("runs %+v", runs)
	}
	if got := titles(h.batchOf(runs[0])); fmt.Sprint(got) != "[a b]" {
		t.Errorf("batch %v", got)
	}
	// While the run is live, a burst joins the pending batch.
	h.tickAfter(time.Minute)
	h.add(e.Round.ID, "c", "d")
	h.tickAfter(time.Minute)
	h.add(e.Round.ID, "e")
	if h.s.Pending(e.Round.ID) != 3 {
		t.Errorf("pending %d", h.s.Pending(e.Round.ID))
	}
	h.endLatest(e.Round.ID, RunSucceeded) // 2 min after the first run started: in the cooldown
	if n := len(h.runs(e.Round.ID)); n != 1 {
		t.Fatalf("a run inside the cooldown: %d", n)
	}
	h.tickAfter(2*time.Minute + 59*time.Second)
	if n := len(h.runs(e.Round.ID)); n != 1 {
		t.Fatalf("a run before the cooldown's end: %d", n)
	}
	h.tickAfter(time.Second) // 5 min after the first run started
	runs = h.runs(e.Round.ID)
	if len(runs) != 2 || runs[0].Events != 3 || fmt.Sprint(titles(h.batchOf(runs[0]))) != "[c d e]" {
		t.Fatalf("second run %+v", runs)
	}
	// A run that outlives the cooldown: the coalesced batch runs at its end.
	h.add(e.Round.ID, "f")
	h.tickAfter(10 * time.Minute)
	if n := len(h.runs(e.Round.ID)); n != 2 {
		t.Fatalf("a second run while one is live: %d", n)
	}
	h.endLatest(e.Round.ID, RunSucceeded)
	runs = h.runs(e.Round.ID)
	if len(runs) != 3 || runs[0].Events != 1 {
		t.Fatalf("coalesced run %+v", runs)
	}
}

func TestEventRunNowTakesBatch(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle = -1
	h.reopen()
	e := h.eventRound("ev", EventNoticeRaised, nil)
	h.add(e.Round.ID, "a", "b")
	run, err := h.s.RunNow(e.Round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Trigger != TriggerManual || run.Events != 2 || h.s.Pending(e.Round.ID) != 0 {
		t.Fatalf("run now %+v pending %d", run, h.s.Pending(e.Round.ID))
	}
	h.endLatest(e.Round.ID, RunSucceeded)
	// No cooldown from a run by hand: the next batch runs after its debounce.
	h.add(e.Round.ID, "c")
	h.tickAfter(30 * time.Second)
	if runs := h.runs(e.Round.ID); len(runs) != 2 || runs[0].Trigger != TriggerEvent {
		t.Fatalf("runs %+v", runs)
	}
	// Run now with nothing pending runs with no batch.
	h.endLatest(e.Round.ID, RunSucceeded)
	run, err = h.s.RunNow(e.Round.ID)
	if err != nil || run.Events != 0 {
		t.Fatalf("empty run now %+v %v", run, err)
	}
}

func TestEventBatchSurvivesRestart(t *testing.T) {
	for _, killed := range []bool{false, true} {
		h := newHarness(t, "2026-03-02T10:00:00Z")
		h.settle = -1
		h.limits = Limits{StateSaveEvery: 10 * time.Second}
		h.reopen()
		e := h.eventRound("ev", EventNoticeRaised, nil)
		h.add(e.Round.ID, "a")
		h.clock.move(11*time.Second, 11*time.Second)
		h.add(e.Round.ID, "b") // saved: past state_save_every
		if killed {
			h.s.stopTimers()
		} else {
			h.s.Close()
		}
		h.reopen()
		if n := h.s.Pending(e.Round.ID); n != 2 {
			t.Fatalf("killed %v: pending %d after a restart", killed, n)
		}
		h.tickAfter(30 * time.Second)
		runs := h.runs(e.Round.ID)
		if len(runs) != 1 || runs[0].Events != 2 {
			t.Fatalf("killed %v: runs %+v", killed, runs)
		}
	}
}

// Events added inside state_save_every reach the disk at Close, or when the save timer fires.
func TestEventBatchSavedAtClose(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.limits = Limits{StateSaveEvery: time.Minute}
	h.reopen()
	e := h.eventRound("ev", EventNoticeRaised, nil)
	h.add(e.Round.ID, "a")
	h.add(e.Round.ID, "b")
	if p := h.entry(e.Round.ID).State.Pending; p == nil || p.Len() != 1 {
		t.Fatalf("saved before state_save_every: %+v", p)
	}
	h.s.Close()
	if p := h.entry(e.Round.ID).State.Pending; p == nil || p.Len() != 2 {
		t.Fatalf("not saved at Close: %+v", p)
	}
}

func TestEventBatchLimits(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle = -1
	h.limits = Limits{BatchMaxEvents: 5, EventFilesKeep: 1}
	h.reopen()
	e := h.eventRound("ev", EventNoticeRaised, func(r *Round) { r.Cooldown = Duration(time.Minute) })
	h.add(e.Round.ID, "1", "2", "3", "4", "5", "6", "7", "8")
	h.tickAfter(30 * time.Second)
	runs := h.runs(e.Round.ID)
	b := h.batchOf(runs[0])
	if runs[0].Events != 8 || b.Dropped != 3 || fmt.Sprint(titles(b)) != "[4 5 6 7 8]" {
		t.Fatalf("run %+v batch %+v", runs[0], b)
	}
	first := h.endLatest(e.Round.ID, RunSucceeded)
	h.add(e.Round.ID, "9")
	h.tickAfter(time.Minute)
	second := h.endLatest(e.Round.ID, RunSucceeded)
	if _, err := os.Stat(h.store.EventsPath(e.Round.ID, first.ID)); !os.IsNotExist(err) {
		t.Errorf("the older file stayed with event_files_keep 1: %v", err)
	}
	if _, err := os.Stat(h.store.EventsPath(e.Round.ID, second.ID)); err != nil {
		t.Errorf("the newest file went: %v", err)
	}
	// A byte bound drops the oldest too.
	var b2 Batch
	for i := range 10 {
		b2.Events = append(b2.Events, Event{Kind: "k", Title: fmt.Sprint(i), Data: json.RawMessage(`"` + string(make([]byte, 0)) + `"`)})
	}
	b2.Cap(100, 3*Event{Kind: "k", Title: "0", Data: json.RawMessage(`""`)}.size())
	if len(b2.Events) != 3 || b2.Dropped != 7 || b2.Events[0].Title != "7" {
		t.Errorf("byte cap %+v", b2)
	}
}

func TestEventSkippedRunKeepsBatch(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle = -1
	h.reopen()
	e := h.eventRound("ev", EventNoticeRaised, func(r *Round) { r.Mode, r.Command, r.Project, r.Prompt = ModeCapcom, nil, "", "p" })
	h.add(e.Round.ID, "a")
	h.tickAfter(30 * time.Second)
	runs := h.runs(e.Round.ID)
	if err := h.s.UpdateRun(runs[0].ID, RunUpdate{State: RunSkipped, Reason: "no Capcom service", KeepBatch: true}); err != nil {
		t.Fatal(err)
	}
	h.add(e.Round.ID, "b")
	if n := h.s.Pending(e.Round.ID); n != 2 {
		t.Fatalf("pending %d after a skip that keeps its batch", n)
	}
	h.tickAfter(5 * time.Minute)
	runs = h.runs(e.Round.ID)
	if len(runs) != 2 || fmt.Sprint(titles(h.batchOf(runs[0]))) != "[a b]" {
		t.Fatalf("runs %+v", runs)
	}
	// A failed run drops its batch.
	if err := h.s.UpdateRun(runs[0].ID, RunUpdate{State: RunFailed, Reason: "not delivered"}); err != nil {
		t.Fatal(err)
	}
	if n := h.s.Pending(e.Round.ID); n != 0 {
		t.Fatalf("pending %d after a failed run", n)
	}
}

func TestEventPausedRoundTakesNone(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	e := h.eventRound("ev", EventNoticeRaised, nil)
	if err := h.s.Pause(e.Round.ID); err != nil {
		t.Fatal(err)
	}
	h.add(e.Round.ID, "a")
	if n := h.s.Pending(e.Round.ID); n != 0 {
		t.Fatalf("a paused round took %d events", n)
	}
	c := h.round("cron", "0 * * * *", nil)
	h.add(c.Round.ID, "a")
	if n := h.s.Pending(c.Round.ID); n != 0 {
		t.Fatalf("a cron round took %d events", n)
	}
}

func TestHabWoke(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle = 30 * time.Second
	h.reopen()
	h.s.Tick() // the first reading: a start, not a wake
	h.awake("2026-03-02T10:02:00Z")
	if w := h.sink.wokeList(); len(w) != 0 {
		t.Fatalf("hab.woke at start: %v", w)
	}
	h.sleep("2026-03-02T12:00:00Z")
	if w := h.sink.wokeList(); len(w) != 0 {
		t.Fatalf("hab.woke before the settle: %v", w)
	}
	h.awake("2026-03-02T12:01:00Z")
	w := h.sink.wokeList()
	if len(w) != 1 || w[0] < time.Hour {
		t.Fatalf("hab.woke %v", w)
	}
	h.awake("2026-03-02T12:05:00Z")
	if w := h.sink.wokeList(); len(w) != 1 {
		t.Fatalf("hab.woke twice for one wake: %v", w)
	}
}

func TestOutcomeOnce(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle = -1
	h.reopen()
	e := h.round("cap", "0 * * * *", func(r *Round) { r.Mode, r.Command, r.Project, r.Prompt = ModeCapcom, nil, "", "p" })
	run, err := h.s.RunNow(e.Round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.s.SetOutcome(run.ID, Outcome{Word: OutcomeTold, At: h.clock.Now()}); ok {
		t.Fatal("an outcome on a live run")
	}
	if err := h.s.UpdateRun(run.ID, RunUpdate{State: RunSucceeded, Reason: "accepted by Capcom", Message: "m1"}); err != nil {
		t.Fatal(err)
	}
	if r, ok := h.s.SetOutcome(run.ID, Outcome{Word: OutcomeTold, At: h.clock.Now()}); !ok || r.Outcome.Word != OutcomeTold || r.State != RunSucceeded {
		t.Fatalf("outcome %+v %v", r, ok)
	}
	if _, ok := h.s.SetOutcome(run.ID, Outcome{Word: OutcomeQuiet}); ok {
		t.Fatal("a second outcome was taken")
	}
	if r, ok := h.store.RunByMessage("m1"); !ok || r.Outcome == nil || r.Outcome.Word != OutcomeTold {
		t.Fatalf("by message %+v", r)
	}
}

// TestEventPauseKeepsQueuedBatch: a queued event run that a pause ends gives its events back
// to the pending batch, so a pause, or a move that pauses first, loses none.
func TestEventPauseKeepsQueuedBatch(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle, h.max = -1, 1
	h.reopen()
	a := h.eventRound("a", EventNoticeRaised, nil)
	b := h.eventRound("b", EventNoticeRaised, nil)
	h.add(a.Round.ID, "x1", "x2")
	h.add(b.Round.ID, "x1", "x2")
	h.tickAfter(30 * time.Second)
	// One runs; the other waits queued behind it under max_running 1.
	if r := h.runs(b.Round.ID); len(r) != 1 || r[0].State != RunQueued {
		a, b = b, a
	}
	if r := h.runs(b.Round.ID); len(r) != 1 || r[0].State != RunQueued {
		t.Fatalf("runs %+v %+v, want one queued", h.runs(a.Round.ID), r)
	}
	if err := h.s.Pause(b.Round.ID); err != nil {
		t.Fatal(err)
	}
	if n := h.s.Pending(b.Round.ID); n != 2 {
		t.Fatalf("pending %d after the pause, want 2", n)
	}
	if got := titles(h.s.PendingBatch(b.Round.ID)); fmt.Sprint(got) != "[x1 x2]" {
		t.Fatalf("batch %v", got)
	}
}

// TestEventTriggerChangedDropsBatch: an event round edited to a cron trigger keeps no batch,
// in memory or on disk, and its run by hand takes none.
func TestEventTriggerChangedDropsBatch(t *testing.T) {
	h := newHarness(t, "2026-03-02T10:00:00Z")
	h.settle = -1
	h.reopen()
	e := h.eventRound("ev", EventNoticeRaised, nil)
	h.add(e.Round.ID, "a")
	r := e.Round
	r.Trigger = Trigger{Kind: TriggerCron, Cron: "0 * * * *", Zone: "UTC"}
	r.Debounce, r.Cooldown = 0, 0
	if _, err := h.s.Update(r); err != nil {
		t.Fatal(err)
	}
	h.s.FlushPending()
	if n := h.s.Pending(e.Round.ID); n != 0 {
		t.Fatalf("pending %d on a cron round", n)
	}
	if got, ok := h.store.Get(e.Round.ID); !ok || got.State.Pending != nil {
		t.Fatalf("stored pending %+v", got.State.Pending)
	}
	run, err := h.s.RunNow(e.Round.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Events != 0 {
		t.Fatalf("a run by hand took %d old events", run.Events)
	}
}
