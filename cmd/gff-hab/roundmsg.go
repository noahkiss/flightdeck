package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/groups"
	"github.com/noahkiss/flightdeck/internal/rounds"
)

// The capcom and group modes (rounds.md 5.2): a run starts no session. It hands one channel
// message, with the prompt and the event batch, to the Capcom service or to a group's
// coordinator, and ends at the message's answer. The far sides (the Capcom service of
// capcom-service.md 9.5 and the group record of groups.md 5.1) are read from the catalog: the
// capcom-service capability and the capcom section's service id, and the groups section.

// Catalog words of the interface (rounds.md 5.2, 13).
const (
	capRoundLock     = "round-lock"
	capCapcomService = "capcom-service"
)

// Reasons of message runs (rounds.md 5.2, the run-state tables).
const (
	reasonAccepted       = "accepted by Capcom"
	reasonDuplicate      = "already accepted by Capcom"
	reasonNoCapcom       = "no Capcom service"
	reasonQueuedFor      = "queued for Capcom on "
	reasonCoordinatorOff = "coordinator not running"
	reasonTooOld         = "the coordinator's hab is too old"
	reasonGroupEnded     = "Its group ended."
)

// messageOverhead is room kept under the 64 KiB channel ceiling for the message's other fields.
const messageOverhead = 4 << 10

// roundChannels is what message runs need of channels.
type roundChannels interface {
	SendRound(ctx context.Context, p channels.RoundSend) (api.ChannelSent, error)
	Record(id string) (api.ChannelRecord, bool)
}

// roundMessageID is a run's channel message id: a hash of the run id, so a run's message is
// never sent twice under two ids (rounds.md 5.2).
func roundMessageID(runID string) string {
	h := sha256.Sum256([]byte("round-message:" + runID))
	return hex.EncodeToString(h[:16])
}

// msgWatch is one live message run.
type msgWatch struct {
	run    rounds.Run
	round  rounds.Round
	capcom bool   // the destination is the Capcom service
	label  string // the destination's peer label, "" for this hab
	timer  *time.Timer
}

func hasWord(c api.Catalog, w string) bool {
	return slices.ContainsFunc(c.Capabilities, func(x api.CatalogCapability) bool { return x.Name == w })
}

// capcomHome is the Capcom home host as the merged catalog shows it: the home of the highest
// assignment, and its entry. ok is false when no copy names one; service says whether the home
// lists capcom-service.
func capcomHome(l api.CatalogList) (e api.CatalogEntry, service, ok bool) {
	var best *api.CatalogCapcom
	for _, h := range l.Hosts {
		c := h.Catalog.Capcom
		if c == nil || c.Home == "" {
			continue
		}
		if best == nil || c.Assignment > best.Assignment || (c.Assignment == best.Assignment && c.Home < best.Home) {
			best = c
		}
	}
	if best == nil {
		return e, false, false
	}
	for _, h := range l.Hosts {
		if h.Catalog.DeviceID == best.Home {
			return h, hasWord(h.Catalog, capCapcomService), true
		}
	}
	return e, false, false
}

// serviceActor is the Capcom service's actor id on the hab with this device id, from its
// catalog's capcom section (capcom-service.md 9.5).
func serviceActor(l api.CatalogList, device string) string {
	for _, h := range l.Hosts {
		if h.Catalog.DeviceID == device && h.Catalog.Capcom != nil {
			return h.Catalog.Capcom.Service
		}
	}
	return ""
}

// groupOf finds a group in the catalog's groups sections (groups.md 5.1, 12.5): the entry with
// the highest generation, and for a tombstone its new home (5.7).
func groupOf(l api.CatalogList, id string) (api.CatalogGroup, bool) {
	w, ok := groups.Find(l, id)
	if !ok {
		return api.CatalogGroup{}, false
	}
	g := w.Entry
	g.Home = w.Device
	return g, true
}

func entryOf(l api.CatalogList, device string) (api.CatalogEntry, bool) {
	for _, h := range l.Hosts {
		if h.Catalog.DeviceID == device {
			return h, true
		}
	}
	return api.CatalogEntry{}, false
}

func (s *roundsService) catalogList() api.CatalogList {
	if s.cfg.catalog == nil {
		return api.CatalogList{}
	}
	return s.cfg.catalog()
}

// destinationActors are the session ids a capcom or group round's destination writes as: the
// no-loop rule counts them as the run session (rounds.md 4.3).
func (s *roundsService) destinationActors(r rounds.Round) []string {
	l := s.catalogList()
	switch r.Mode {
	case rounds.ModeCapcom:
		if home, _, ok := capcomHome(l); ok {
			if a := serviceActor(l, home.Catalog.DeviceID); a != "" {
				return []string{a}
			}
		}
	case rounds.ModeGroup:
		g, ok := groupOf(l, r.Group)
		if !ok || g.Coordinator == nil {
			return nil
		}
		if g.Coordinator.Capcom {
			if a := serviceActor(l, g.Coordinator.Device); a != "" {
				return []string{a}
			}
			return nil
		}
		if g.Coordinator.Session != "" {
			return []string{g.Coordinator.Session}
		}
	}
	return nil
}

// checkMessageRound refuses a capcom round while no catalog copy shows a Capcom service, and a
// group round whose group no copy shows (rounds.md 5.2): each mode answers unavailable until
// its other side exists.
func (s *roundsService) checkMessageRound(r rounds.Round) error {
	l := s.catalogList()
	switch r.Mode {
	case rounds.ModeCapcom:
		if _, service, ok := capcomHome(l); !ok || !service {
			return control.Errorf(api.ErrorKindUnavailable, "no Capcom service: no host in the catalog lists %s", capCapcomService)
		}
	case rounds.ModeGroup:
		if _, ok := groupOf(l, r.Group); !ok {
			return control.Errorf(api.ErrorKindUnavailable, "no group %s in the catalog", r.Group)
		}
	}
	return nil
}

// skipMsg ends a message run that sent nothing: skipped, its batch kept for the next run.
func (s *roundsService) skipMsg(run rounds.Run, reason string, keep bool) {
	if err := s.updateRun(run.ID, rounds.RunUpdate{State: rounds.RunSkipped, Reason: reason, KeepBatch: keep}); err != nil {
		s.cfg.logf("rounds: run %s: %v", run.ID, err)
	}
}

// startMessage starts a capcom or group run: it finds the destination, sends the message, and
// watches its state. It returns no session: the run has none.
func (s *roundsService) startMessage(r rounds.Round, run rounds.Run) (string, error) {
	if s.cfg.channels == nil {
		return "", fmt.Errorf("this hab runs without channels")
	}
	l := s.catalogList()
	var device, session string
	capcom := false
	switch r.Mode {
	case rounds.ModeCapcom:
		home, service, ok := capcomHome(l)
		if !ok || !service {
			s.skipMsg(run, reasonNoCapcom, true)
			return "", nil
		}
		device, session, capcom = home.Catalog.DeviceID, channels.CapcomName, true
	case rounds.ModeGroup:
		g, ok := groupOf(l, r.Group)
		switch {
		case !ok:
			s.skipMsg(run, "group "+r.Group+" is not in the catalog", true)
			return "", nil
		case g.State == "ended" || g.State == "dropped":
			if err := s.updateRun(run.ID, rounds.RunUpdate{State: rounds.RunSkipped, Reason: "group " + g.State,
				TargetGone: true, GoneReason: reasonGroupEnded}); err != nil {
				s.cfg.logf("rounds: run %s: %v", run.ID, err)
			}
			return "", nil
		case g.Coordinator == nil:
			s.skipMsg(run, reasonCoordinatorOff, true)
			return "", nil
		}
		c := g.Coordinator
		device, session, capcom = c.Device, c.Name, c.Capcom
		if capcom {
			session = channels.CapcomName
		} else if e, ok := entryOf(l, device); device != s.cfg.self && (!ok || !hasWord(e.Catalog, capRoundLock)) {
			s.skipMsg(run, reasonTooOld, true)
			return "", nil
		}
	}
	label := ""
	if device != s.cfg.self {
		if e, ok := entryOf(l, device); ok {
			label = e.Label
		}
		if label == "" {
			s.skipMsg(run, "the destination's hab is not in the peer list", true)
			return "", nil
		}
	}
	mark := s.roundMark(r, run, capcom)
	id := roundMessageID(run.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.cfg.channels.SendRound(ctx, channels.RoundSend{ID: id, Name: r.Name, Device: device, Session: session,
		Label: label, Text: r.Prompt, Round: mark}); err != nil {
		s.skipMsg(run, "channels refused it: "+roundErrText(err), true)
		return "", nil
	}
	dest := session
	if label != "" {
		dest = label + "/" + session
	}
	if capcom {
		dest = "Capcom on " + hostWord(label)
	}
	run.Message, run.Destination = id, dest
	if err := s.updateRun(run.ID, rounds.RunUpdate{State: rounds.RunRunning, Destination: dest, Message: id}); err != nil {
		s.cfg.logf("rounds: run %s: %v", run.ID, err)
		return "", nil
	}
	s.watchMessage(run, r, capcom, label)
	return "", nil
}

func hostWord(label string) string {
	if label == "" {
		return "this host"
	}
	return label
}

func roundErrText(err error) string {
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce.Message
	}
	return err.Error()
}

// roundMark is the message's round fields (rounds.md 5.2): the run, its trigger and slot, the
// batch cut to the block's bounds and to the channel ceiling, the level hint, last_ok, and for
// a group message the deadline: the run's start plus wait.
func (s *roundsService) roundMark(r rounds.Round, run rounds.Run, capcom bool) api.ChannelRound {
	m := api.ChannelRound{Round: r.ID, Name: r.Name, Run: run.ID, Trigger: string(run.Trigger)}
	if run.Slot != nil {
		m.Slot = run.Slot.UTC().Format(time.RFC3339)
	}
	if e, ok := s.store.Get(r.ID); ok && e.State.LastOK != nil {
		m.LastOK = e.State.LastOK.UTC().Format(time.RFC3339)
	}
	if !capcom {
		start := time.Now()
		if run.StartedAt != nil {
			start = *run.StartedAt
		}
		m.Deadline = start.Add(time.Duration(r.Wait)).UTC().Format(time.RFC3339Nano)
	}
	b := s.runBatch(run)
	m.Level = b.Level()
	evs := apiEvents(b.Events)
	_, kept := channels.EventBlock(run.ID, evs, b.Len(), s.cfg.settings.BlockMaxEvents, s.cfg.settings.BlockMaxBytes, s.cfg.settings.BlockDataMax)
	// The whole message fits the channel ceiling: the oldest events go first.
	room := channels.MaxText - len(r.Prompt) - messageOverhead
	for len(kept) > 0 {
		j, _ := json.Marshal(kept)
		if len(j) <= room {
			break
		}
		kept = kept[1:]
	}
	for i := range kept { // the data each event carries is cut as the block cuts it
		if len(kept[i].Data) > s.cfg.settings.BlockDataMax {
			kept[i].Data = nil
		}
	}
	m.Events, m.Omitted = kept, b.Len()-len(kept)
	return m
}

func apiEvents(evs []rounds.Event) []api.RoundEvent {
	out := make([]api.RoundEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, api.RoundEvent{At: e.At.UTC().Format(time.RFC3339), Kind: e.Kind, Title: e.Title, Level: e.Level, Data: api.RawJSON(e.Data)})
	}
	return out
}

// watchMessage follows a message run until its message's answer, with its time limit: a capcom
// run's timeout, a group run's wait.
func (s *roundsService) watchMessage(run rounds.Run, r rounds.Round, capcom bool, label string) {
	limit := time.Duration(r.Timeout)
	if !capcom || r.Mode == rounds.ModeGroup {
		limit = time.Duration(r.Wait)
	}
	start := time.Now()
	if run.StartedAt != nil {
		start = *run.StartedAt
	}
	w := &msgWatch{run: run, round: r, capcom: capcom, label: label}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if old := s.msgRuns[run.Message]; old != nil && old.timer != nil {
		old.timer.Stop()
	}
	s.msgRuns[run.Message] = w
	w.timer = time.AfterFunc(max(time.Until(start.Add(limit)), 0), func() { s.goBg(func() { s.messageLimit(run.Message) }) })
	s.mu.Unlock()
	if rec, ok := s.cfg.channels.Record(run.Message); ok {
		s.messageState(rec)
	}
}

// messageState maps a round message's state to its run (rounds.md 5.2, the run-state tables).
// channels calls it with its lock held, through channelRecord, so it runs off that goroutine.
func (s *roundsService) messageState(rec api.ChannelRecord) {
	s.mu.Lock()
	w := s.msgRuns[rec.ID]
	s.mu.Unlock()
	if w == nil {
		return
	}
	var u *rounds.RunUpdate
	end := func(st rounds.RunState, reason string, keep bool) {
		u = &rounds.RunUpdate{State: st, Reason: reason, KeepBatch: keep}
	}
	switch rec.State {
	case api.ChannelStateAccepted:
		end(rounds.RunSucceeded, reasonAccepted, false)
	case api.ChannelStateDuplicate:
		end(rounds.RunSucceeded, reasonDuplicate, false)
	case api.ChannelStateDeclined:
		end(rounds.RunSkipped, "declined by Capcom: "+rec.Reason, false)
	case api.ChannelStateDelivered:
		end(rounds.RunSucceeded, "", false)
	case api.ChannelStateNotDelivered:
		end(rounds.RunFailed, rec.Reason, false)
	case api.ChannelStateExpired:
		end(rounds.RunSkipped, rec.Reason, true)
	case api.ChannelStateRefused:
		reason := rec.Reason
		if !w.capcom && strings.HasPrefix(reason, "agent not running") {
			reason = reasonCoordinatorOff
		}
		end(rounds.RunSkipped, reason, true)
	case api.ChannelStateQueued:
		if w.capcom && w.label != "" && s.cfg.peerOnline != nil && !s.cfg.peerOnline(w.label) {
			// The outbox holds it while the home host sleeps; it is delivered later, once.
			end(rounds.RunSucceeded, reasonQueuedFor+w.label, false)
		}
	}
	if u == nil {
		return
	}
	s.endMessage(rec.ID, w, *u)
}

// messageLimit ends a message run whose answer did not come in time: a capcom run handed and
// unanswered is timed out; a group message not delivered in its wait is skipped, and the far
// side drops it at its deadline. Both keep their batch.
func (s *roundsService) messageLimit(id string) {
	s.mu.Lock()
	w := s.msgRuns[id]
	s.mu.Unlock()
	if w == nil {
		return
	}
	state := api.ChannelStateQueued
	if rec, ok := s.cfg.channels.Record(id); ok {
		state = rec.State
	}
	if w.capcom {
		s.endMessage(id, w, rounds.RunUpdate{State: rounds.RunTimedOut, Reason: fmt.Sprintf("no answer from Capcom in %s: %s", time.Duration(w.round.Timeout), state), KeepBatch: true})
		return
	}
	s.endMessage(id, w, rounds.RunUpdate{State: rounds.RunSkipped, Reason: fmt.Sprintf("not delivered in %s: %s", time.Duration(w.round.Wait), state), KeepBatch: true})
}

func (s *roundsService) endMessage(id string, w *msgWatch, u rounds.RunUpdate) {
	s.mu.Lock()
	if s.msgRuns[id] != w {
		s.mu.Unlock()
		return
	}
	delete(s.msgRuns, id)
	if w.timer != nil {
		w.timer.Stop()
	}
	s.mu.Unlock()
	if err := s.updateRun(w.run.ID, u); err != nil {
		s.cfg.logf("rounds: run %s: %v", w.run.ID, err)
	}
}

// channelRecord hears every change of a channel message this hab sent. channels holds its lock
// while it calls, so a round message's state is mapped on another goroutine.
func (s *roundsService) channelRecord(rec api.ChannelRecord) {
	if !strings.HasPrefix(rec.From, "round:") {
		return
	}
	s.goBg(func() { s.messageState(rec) })
}

// adoptMessage picks up a message run that was live when the last gff-hab stopped: its message
// is in channels' outbox or answered; the watch starts again from the record.
func (s *roundsService) adoptMessage(run rounds.Run) {
	e, ok := s.store.Get(run.RoundID)
	if !ok || s.cfg.channels == nil {
		s.updateRun(run.ID, rounds.RunUpdate{State: rounds.RunFailed, Reason: reasonHostRestarted})
		return
	}
	if _, ok := s.cfg.channels.Record(run.Message); !ok {
		s.updateRun(run.ID, rounds.RunUpdate{State: rounds.RunSkipped, Reason: "its message is gone", KeepBatch: true})
		return
	}
	capcom := strings.HasPrefix(run.Destination, "Capcom on ")
	label := ""
	if capcom {
		if l := strings.TrimPrefix(run.Destination, "Capcom on "); l != "this host" {
			label = l
		}
	}
	s.watchMessage(run, e.Round, capcom, label)
}

// outcome records Capcom's second answer on the run whose message it answers, once, within
// outcome_wait of the run's end (rounds.md 5.2). channels calls it with its lock held.
func (s *roundsService) outcome(id, word, at string) {
	s.goBg(func() {
		run, ok := s.store.RunByMessage(id)
		if !ok || !awaitsOutcome(run) {
			return
		}
		if run.EndedAt != nil && time.Since(*run.EndedAt) > s.cfg.settings.OutcomeWait {
			s.cfg.logf("rounds: run %s: an outcome after outcome_wait; dropped", run.ID)
			return
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			t = time.Now()
		}
		s.sched.SetOutcome(run.ID, rounds.Outcome{Word: word, At: t.UTC()})
	})
}

// awaitsOutcome reports whether a run's message was accepted by Capcom, so an outcome may come.
func awaitsOutcome(r rounds.Run) bool {
	return r.Message != "" && r.State == rounds.RunSucceeded &&
		(r.Reason == reasonAccepted || strings.HasPrefix(r.Reason, reasonQueuedFor))
}

// runOutcome is a run's outcome for the API: the recorded one, or unknown once outcome_wait
// passed without one (rounds.md 5.2).
func (s *roundsService) runOutcome(r rounds.Run, now time.Time) *api.RoundRunOutcome {
	if r.Outcome != nil {
		return &api.RoundRunOutcome{Word: r.Outcome.Word, At: r.Outcome.At.UTC().Format(time.RFC3339)}
	}
	if awaitsOutcome(r) && r.EndedAt != nil && now.Sub(*r.EndedAt) > s.cfg.settings.OutcomeWait {
		return &api.RoundRunOutcome{Word: "unknown"}
	}
	return nil
}
