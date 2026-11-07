package rounds

import (
	"errors"
	"slices"
	"strconv"
)

// Watch rounds in the scheduler (rounds.md 4.4): the event lines a watcher printed join the
// round's pending batch through the round's kinds and dedup keys, and its cursor is kept.

// ErrNotWatch refuses watcher lines for a round whose trigger is not a watch trigger.
var ErrNotWatch = errors.New("the round has no watch trigger")

// Reasons of a watcher line the round drops.
const (
	DropKind   = "kind not counted"
	DropKey    = "key seen within the key window"
	DropPaused = "the round is paused"
	DropDone   = "the round has completed"
)

// LineResult is what became of one watcher line: an event the batch took, a cursor kept, or a
// drop with its reason. A quiet drop is shown nowhere and counted nowhere.
type LineResult struct {
	Taken  bool
	Cursor bool
	Title  string
	Drop   string
	Quiet  bool
}

// WatchLines takes a page of lines a watch round's watcher printed, in order, read at now
// (their default time). Events that pass the line rules, the round's kinds and its dedup keys
// join the pending batch; a cursor line replaces the round's cursor. The cursor and the keys
// are saved at most once per state_save_every and in the write that makes a run. A round that
// is gone answers ErrNotFound, one with another trigger ErrNotWatch.
func (s *Scheduler) WatchLines(roundID string, lines [][]byte) ([]LineResult, error) {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	e, ok := s.store.Get(roundID)
	var err error
	var out []LineResult
	switch {
	case !ok:
		err = ErrNotFound
	case e.Round.Trigger.Kind != TriggerWatch || e.Round.Trigger.Watch == nil:
		err = ErrNotWatch
	default:
		out = make([]LineResult, 0, len(lines))
		ws := s.wst[roundID]
		if ws == nil {
			ws = &WatchState{}
		}
		var evs []Event
		for _, b := range lines {
			l := ParseWatchLine(b, now, s.watchLim)
			switch {
			case l.Drop != "":
				out = append(out, LineResult{Drop: l.Drop})
			case l.Cursor != nil:
				if ws.Cursor != *l.Cursor {
					ws.Cursor = *l.Cursor
					s.wstDirty[roundID] = true
				}
				out = append(out, LineResult{Cursor: true})
			case e.Round.Paused:
				out = append(out, LineResult{Title: l.Event.Title, Drop: DropPaused})
			case e.State.Completed:
				out = append(out, LineResult{Title: l.Event.Title, Drop: DropDone})
			case len(e.Round.Trigger.Watch.Kinds) > 0 && !slices.Contains(e.Round.Trigger.Watch.Kinds, l.Event.Kind):
				out = append(out, LineResult{Title: l.Event.Title, Drop: DropKind, Quiet: true})
			case l.Key != "" && ws.seen(l.Key, now, s.watchLim.KeyWindow, s.watchLim.KeysMax):
				out = append(out, LineResult{Title: l.Event.Title, Drop: DropKey, Quiet: true})
			default:
				if l.Key != "" {
					s.wstDirty[roundID] = true
				}
				evs = append(evs, *l.Event)
				out = append(out, LineResult{Taken: true, Title: l.Event.Title})
			}
		}
		if !ws.empty() || s.wst[roundID] != nil {
			s.wst[roundID] = ws
		}
		if len(evs) > 0 {
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
		}
		s.flushPendingLocked(now, false)
	}
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
	return out, err
}

// WatchCursor returns a watch round's latest cursor, saved or not: the next start's
// GFF_WATCH_CURSOR.
func (s *Scheduler) WatchCursor(roundID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ws := s.wst[roundID]; ws != nil {
		return ws.Cursor
	}
	return ""
}

// SeedCursor gives a new watch round the cursor a move carried (rounds.md 9), saved at once: the
// old runner deletes its copy next. The dedup keys stay behind.
func (s *Scheduler) SeedCursor(roundID, cursor string) error {
	if cursor == "" {
		return nil
	}
	s.mu.Lock()
	e, ok := s.store.Get(roundID)
	var err error
	switch {
	case !ok:
		err = ErrNotFound
	case e.Round.Trigger.Kind != TriggerWatch:
		err = ErrNotWatch
	case len(cursor) > s.watchLim.CursorMax:
		err = &FieldError{Field: "cursor", Reason: "over the cursor bound"}
	default:
		ws := s.wst[roundID]
		if ws == nil {
			ws = &WatchState{}
			s.wst[roundID] = ws
		}
		ws.Cursor = cursor
		s.wstDirty[roundID] = true
		s.flushPendingLocked(s.cfg.Clock.Now(), true)
	}
	s.unlockAndFlush()
	return err
}

// ReasonWatcherPause is the paused reason of a round its watcher's exits paused (rounds.md 4.4).
const ReasonWatcherPause = "its watcher exited too often soon after its start"

// PauseWatcher pauses a watch round whose watcher exited PauseAfter times in a row, each soon
// after its start (rounds.md 4.4), and raises the paused notice naming the last exit, with the
// watcher session to open. It is a self-pause: notify = never keeps it. A round already paused
// raises nothing.
func (s *Scheduler) PauseWatcher(roundID, lastExit, session string, exits int) error {
	s.mu.Lock()
	now, prev, wake := s.observeLocked()
	e, ok := s.store.Get(roundID)
	var err error
	switch {
	case !ok:
		err = ErrNotFound
	case e.Round.Paused:
	default:
		if err = s.pauseLocked(roundID, ReasonWatcherPause, now); err == nil {
			ne, _ := s.store.Get(roundID)
			s.notice(Notice{Kind: NoticePaused, Round: ne, Exit: lastExit, Session: session, Exits: exits,
				Text: "round " + e.Round.Name + " paused: its watcher exited " + strconv.Itoa(exits) + " times in a row soon after its start; the last: " + lastExit})
		}
	}
	s.evaluateLocked(now, prev, wake)
	s.startLocked(now)
	s.unlockAndFlush()
	return err
}

