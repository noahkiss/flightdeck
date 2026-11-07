package rounds

import (
	"time"
)

// Event batches in the scheduler (rounds.md 4.5): events join a round's pending batch; the
// batch makes one run after the debounce, at the cooldown's end, and never while the round's
// run is live.

// AddEvents adds events that matched a round's filter to its pending batch. A round that is
// gone answers ErrNotFound; a paused or completed round, or one whose trigger is no event
// trigger, takes none.
func (s *Scheduler) AddEvents(roundID string, evs []Event) error {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	e, ok := s.store.Get(roundID)
	var err error
	switch {
	case !ok:
		err = ErrNotFound
	case len(evs) == 0, e.Round.Paused, e.State.Completed, !e.Round.Trigger.Kind.Batches():
	default:
		p := s.pend[roundID]
		if p == nil {
			p = &Pending{}
			s.pend[roundID] = p
		}
		if p.Opened == nil {
			t := now
			p.Opened = &t
		}
		p.Events = append(p.Events, evs...)
		p.Cap(s.cfg.Limits.BatchMaxEvents, s.cfg.Limits.BatchMaxBytes)
		s.pendDirty[roundID] = true
		s.flushPendingLocked(now, false)
	}
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
	return err
}

// Pending returns how many events wait in a round's pending batch, dropped ones included, as
// of the scheduler's last change. It never takes the scheduler's lock, so the sink may call it.
func (s *Scheduler) Pending(roundID string) int {
	s.pendNMu.Lock()
	defer s.pendNMu.Unlock()
	return s.pendN[roundID]
}

// FlushPending saves every pending batch not saved yet, whatever the time since the last save:
// at Close, and when the save timer fires.
func (s *Scheduler) FlushPending() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.flushPendingLocked(s.cfg.Clock.Now(), true)
	s.unlockAndFlush()
}

// Close stops the scheduler's timers and saves the pending batches. Run's context stops the
// tick; nothing starts after Close.
func (s *Scheduler) Close() {
	s.mu.Lock()
	s.flushPendingLocked(s.cfg.Clock.Now(), true)
	s.closed = true
	if s.pendTimer != nil {
		s.pendTimer.Stop()
	}
	if s.dueTimer != nil {
		s.dueTimer.Stop()
	}
	s.unlockAndFlush()
}

// flushPendingLocked saves the batches not saved yet, when force is set or StateSaveEvery has
// passed since the last save; else it arms a timer for the rest of the time.
func (s *Scheduler) flushPendingLocked(now time.Time, force bool) {
	if len(s.pendDirty) == 0 && len(s.wstDirty) == 0 {
		return
	}
	every := s.cfg.Limits.StateSaveEvery
	if since := now.Sub(s.pendSaved); !force && since >= 0 && since < every {
		if s.pendTimer == nil && !s.closed {
			s.pendTimer = time.AfterFunc(every-since, func() {
				s.mu.Lock()
				s.pendTimer = nil
				s.mu.Unlock()
				s.FlushPending()
			})
		}
		return
	}
	m := map[string]*Pending{}
	for id := range s.pendDirty {
		m[id] = s.pend[id]
	}
	w := map[string]*WatchState{}
	for id := range s.wstDirty {
		w[id] = s.wst[id]
	}
	entries, err := s.store.SetPending(m, w)
	if err != nil {
		s.logf("rounds: save pending events: %v", err)
		return
	}
	s.pendSaved = now
	s.pendDirty = map[string]bool{}
	s.wstDirty = map[string]bool{}
	for i := range entries {
		e := entries[i]
		s.events = append(s.events, event{round: &e})
	}
}

// evaluateEvents makes an event round's run when its pending batch is due: the debounce after
// the batch opened, and the cooldown after the last event run started, with no run live.
func (s *Scheduler) evaluateEvents(e Entry, now time.Time, live *Run) {
	p := s.pend[e.Round.ID]
	if p == nil || p.Len() == 0 || live != nil {
		return
	}
	due := now
	if p.Opened != nil {
		due = p.Opened.Add(time.Duration(e.Round.Debounce))
	}
	if t := e.State.LastEventStart; t != nil {
		if cd := t.Add(time.Duration(e.Round.Cooldown)); cd.After(due) {
			due = cd
		}
	}
	if now.Before(due) {
		s.armDueLocked(due, now)
		return
	}
	run := Run{
		ID:      NewID("run"),
		Number:  e.State.LastRun + 1,
		RoundID: e.Round.ID,
		Trigger: e.Round.Trigger.Kind,
		State:   RunQueued,
		Created: now,
	}
	if _, err := s.takeBatchLocked(e.Round.ID, &run); err != nil {
		s.logf("rounds: round %s: save the run's events: %v", e.Round.Name, err)
		return
	}
	ws, wsDirty := s.wst[e.Round.ID], s.wstDirty[e.Round.ID]
	ne, err := s.store.QueueRun(run, func(st *RoundState) {
		st.LastRun = run.Number
		st.Pending = nil
		if wsDirty {
			// The cursor and the keys go in the write that makes the run (rounds.md 4.4).
			st.Watch = nil
			if !ws.empty() {
				st.Watch = cloneWatchState(ws)
			}
		}
	})
	if err != nil {
		s.store.RemoveEvents(e.Round.ID, run.ID)
		s.logf("rounds: round %s: queue run: %v", e.Round.Name, err)
		return
	}
	delete(s.pend, e.Round.ID)
	delete(s.pendDirty, e.Round.ID)
	delete(s.wstDirty, e.Round.ID)
	s.live[run.ID] = &run
	c := run
	s.events = append(s.events, event{round: &ne}, event{run: &c})
}

// takeBatchLocked writes a round's pending batch to the new run's event file and counts it on
// the run. It reports whether there was a batch to take; the caller clears it once the run is
// saved.
func (s *Scheduler) takeBatchLocked(roundID string, run *Run) (bool, error) {
	p := s.pend[roundID]
	if e, ok := s.store.Get(roundID); p == nil || p.Len() == 0 || !ok || !e.Round.Trigger.Kind.Batches() {
		return false, nil
	}
	if err := s.store.WriteEvents(roundID, run.ID, p.Batch); err != nil {
		return false, err
	}
	run.Events = p.Len()
	return true, nil
}

// returnBatchLocked puts an ended run's events back in front of its round's pending batch: its
// destination was not there, and the next run carries them (rounds.md 4.5, 5.2).
func (s *Scheduler) returnBatchLocked(run *Run, now time.Time) {
	e, ok := s.store.Get(run.RoundID)
	if !ok || !e.Round.Trigger.Kind.Batches() {
		return // a round that went, or whose trigger changed away from events, keeps no batch
	}
	b, err := s.store.ReadEvents(run.RoundID, run.ID)
	if err != nil {
		s.logf("rounds: run %s: read its events back: %v", run.ID, err)
		return
	}
	cur := s.pend[run.RoundID]
	merged := &Pending{Batch: Batch{Events: append(b.Events, nil...), Dropped: b.Dropped}}
	opened := now
	if len(b.Events) > 0 {
		opened = b.Events[0].At
	}
	if cur != nil {
		merged.Events = append(merged.Events, cur.Events...)
		merged.Dropped += cur.Dropped
		if cur.Opened != nil && cur.Opened.Before(opened) {
			opened = *cur.Opened
		}
	}
	merged.Opened = &opened
	merged.Cap(s.cfg.Limits.BatchMaxEvents, s.cfg.Limits.BatchMaxBytes)
	s.pend[run.RoundID] = merged
	s.pendDirty[run.RoundID] = true
	s.flushPendingLocked(now, true)
}

// armDueLocked makes sure a tick comes at due, the next time a batch becomes due.
func (s *Scheduler) armDueLocked(due, now time.Time) {
	if s.closed {
		return
	}
	if s.dueTimer != nil && !s.dueAt.After(due) && s.dueAt.After(now) {
		return
	}
	if s.dueTimer != nil {
		s.dueTimer.Stop()
	}
	s.dueAt = due
	s.dueTimer = time.AfterFunc(due.Sub(now), func() {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if !closed {
			s.Tick()
		}
	})
}

// SetOutcome records a capcom run's second answer once (rounds.md 5.2): only on a run that has
// ended and has none. It reports whether it recorded it.
func (s *Scheduler) SetOutcome(runID string, o Outcome) (Run, bool) {
	s.mu.Lock()
	r, ok := s.store.Run(runID)
	if !ok || !r.State.Final() || r.Outcome != nil || o.Word == "" {
		s.mu.Unlock()
		return r, false
	}
	r.Outcome = &o
	s.saveRun(&r)
	s.unlockAndFlush()
	return r, true
}

// PendingBatch returns a copy of a round's pending batch: what a move carries (rounds.md 9).
func (s *Scheduler) PendingBatch(roundID string) Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pend[roundID]
	if p == nil {
		return Batch{}
	}
	return Batch{Events: append([]Event(nil), p.Events...), Dropped: p.Dropped}
}

// SeedPending gives a new event round the pending batch a move carried (rounds.md 9): it joins
// the round's batch, which opens now, so its debounce starts at the create. A paused round takes
// it too: the move carries the pause mark and the batch both. A round that is gone answers
// ErrNotFound; one whose trigger is no event trigger takes none.
func (s *Scheduler) SeedPending(roundID string, b Batch) error {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	e, ok := s.store.Get(roundID)
	var err error
	switch {
	case !ok:
		err = ErrNotFound
	case b.Len() == 0, e.State.Completed, !e.Round.Trigger.Kind.Batches():
	default:
		p := s.pend[roundID]
		if p == nil {
			p = &Pending{}
			s.pend[roundID] = p
		}
		if p.Opened == nil {
			t := now
			p.Opened = &t
		}
		p.Events = append(append([]Event(nil), b.Events...), p.Events...)
		p.Dropped += b.Dropped
		p.Cap(s.cfg.Limits.BatchMaxEvents, s.cfg.Limits.BatchMaxBytes)
		s.pendDirty[roundID] = true
		// A move's batch is saved at once: the old runner deletes its copy next.
		s.flushPendingLocked(now, true)
	}
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
	return err
}
