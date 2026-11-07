package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/groups"
)

// Moving a group's home hab (groups.md 5.7). The current home runs the hand-over, one writer at
// every step: freeze (moving), drain, adopt (the new hab keeps a pending copy), commit (the old
// home's tombstone is the commit point), then group.adopt_commit. The new hab also commits by
// itself from a catalog copy that shows the tombstone, and drops a pending copy the catalog
// shows stale. A move asked while the home is offline waits on the target hab.

// moveStartWait bounds how long a move waits for a member start in flight before its adopt: the
// start's outcome lands in the record that moves.
const moveStartWait = 3 * time.Minute

// queuedTries bounds the tries of a queued move the home answers unavailable: about a minute of
// watch ticks.
const queuedTries = 12

// bgLocked runs f on its own goroutine, counted by Stop; none after Stop. The caller holds s.mu.
func (s *groupsService) bgLocked(f func()) {
	if s.closed {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// bg is bgLocked for a caller that does not hold s.mu.
func (s *groupsService) bg(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bgLocked(f)
}

// labelOf is a device's label in this hab's catalog: its own host id for this hab.
func (s *groupsService) labelOf(cl api.CatalogList, device string) string {
	if device == s.cfg.self {
		return s.cfg.host
	}
	for _, h := range cl.Hosts {
		if h.Catalog.DeviceID == device && !h.Own {
			return h.Label
		}
	}
	return ""
}

// linkTo is the link this hab dialed to a device, online.
func (s *groupsService) linkTo(cl api.CatalogList, device string) (confirmLink, string, bool) {
	label := s.labelOf(cl, device)
	if label == "" || device == s.cfg.self {
		return confirmLink{}, label, false
	}
	l, ok := s.link(label)
	if !ok || !l.Online || l.Caller == nil {
		return confirmLink{}, label, false
	}
	return l, label, true
}

func movedErr(group, to, label string) error {
	e := control.Errorf(api.ErrorKindMoved, "group %s moved home to %s", group, nameOr(label, to))
	e.Data.Home = to
	return e
}

func nameOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func errKind(err error) api.ErrorKind {
	var e *control.Error
	if errors.As(err, &e) {
		return e.Data.Kind
	}
	return ""
}

// Move is group.move (groups.md 5.7): the hand-over on the home hab; on another hab the call
// goes on to the home, or, while the home is offline, to the target hab, which queues it.
func (s *groupsService) Move(ctx context.Context, p api.GroupMoveParams) (api.GroupMoveResult, error) {
	if p.Cancel {
		return s.cancelQueued(p.Group)
	}
	cl := s.cfg.catalog()
	to := p.Device
	if to == "" {
		dev, ok := s.hostDevice(cl, p.Host)
		if !ok || p.Host == "" {
			return api.GroupMoveResult{}, control.Errorf(api.ErrorKindNotFound, "no host labelled %q in this hab's catalog", p.Host)
		}
		to = dev
	}
	s.mu.Lock()
	_, home := s.groups[p.Group]
	_, pending := s.pending[p.Group]
	s.mu.Unlock()
	if home {
		return s.handOver(p.Group, to)
	}
	if pending {
		return api.GroupMoveResult{}, control.Errorf(api.ErrorKindUnavailable, "group %s is moving to this hab; try again in a moment", p.Group)
	}
	_, fromPeer := control.CallerPeer(ctx)
	w, ok := groups.Find(cl, p.Group)
	if t := s.tomb(p.Group); t != nil {
		w, ok = groups.Where{Device: t.Moved.To, Generation: t.Moved.Generation}, true
	}
	if !ok || w.Device == s.cfg.self {
		return api.GroupMoveResult{}, control.Errorf(api.ErrorKindNotFound, "no group %s on this hab or in the catalog", p.Group)
	}
	fwd := api.GroupMoveParams{Group: p.Group, Device: to}
	var out api.GroupMoveResult
	if l, _, up := s.linkTo(cl, w.Device); up {
		if fromPeer {
			// Passed on once only: a hab that is not the home, reached over a link, does not
			// pass the call on again.
			return api.GroupMoveResult{}, control.Errorf(api.ErrorKindNotFound, "this hab is not group %s's home", p.Group)
		}
		err := l.Caller.Call(ctx, "group.move", fwd, &out)
		if err == nil || errKind(err) != "" {
			return out, err // the home answered: its refusal is final
		}
	}
	// The home is offline: the target hab keeps the request (groups.md 5.7, the sleeping laptop).
	homeLabel := nameOr(s.labelOf(cl, w.Device), w.Device)
	if to == s.cfg.self {
		return s.queue(p.Group, w, homeLabel)
	}
	if fromPeer {
		return api.GroupMoveResult{}, control.Errorf(api.ErrorKindUnavailable, "group %s's home %s is offline", p.Group, homeLabel)
	}
	l, label, up := s.linkTo(cl, to)
	if !up {
		return api.GroupMoveResult{}, control.Errorf(api.ErrorKindUnavailable, "group %s's home %s and the target %s are both offline", p.Group, homeLabel, nameOr(label, to))
	}
	err := l.Caller.Call(ctx, "group.move", fwd, &out)
	return out, err
}

// queue keeps a move on this hab, the target, until the home is back. A later request for the
// group replaces it.
func (s *groupsService) queue(id string, w groups.Where, homeLabel string) (api.GroupMoveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := groups.Queued{Group: id, Title: w.Entry.Title, Home: w.Device, Label: homeLabel, At: stampT(s.cfg.now())}
	s.queued = slices.DeleteFunc(s.queued, func(o groups.Queued) bool { return o.Group == id })
	s.queued = append(s.queued, q)
	if err := s.st.SaveQueued(s.queued); err != nil {
		return api.GroupMoveResult{}, control.Errorf(api.ErrorKindUnavailable, "the queued move was not saved: %v", err)
	}
	return api.GroupMoveResult{State: "queued", Home: s.cfg.self,
		Text: fmt.Sprintf("%s is offline; the group moves when it is back", homeLabel)}, nil
}

func (s *groupsService) cancelQueued(id string) (api.GroupMoveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.queued)
	s.queued = slices.DeleteFunc(s.queued, func(o groups.Queued) bool { return o.Group == id })
	if len(s.queued) == n {
		return api.GroupMoveResult{}, control.Errorf(api.ErrorKindNotFound, "no move of group %s is queued on this hab", id)
	}
	if err := s.st.SaveQueued(s.queued); err != nil {
		return api.GroupMoveResult{}, control.Errorf(api.ErrorKindUnavailable, "the change was not saved: %v", err)
	}
	return api.GroupMoveResult{State: "cancelled", Text: "the queued move is cancelled"}, nil
}

func (s *groupsService) tomb(id string) *api.Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.tombs[id]; t != nil {
		c := copyGroup(*t)
		return &c
	}
	return nil
}

// handOver runs the hand-over of 5.7 on this hab, the current home.
func (s *groupsService) handOver(id, to string) (api.GroupMoveResult, error) {
	cl := s.cfg.catalog()
	refuse := func(kind api.ErrorKind, f string, a ...any) (api.GroupMoveResult, error) {
		return api.GroupMoveResult{}, control.Errorf(kind, f, a...)
	}
	movable := func() (*api.Group, error) {
		g := s.groups[id]
		if g == nil {
			return nil, control.Errorf(api.ErrorKindNotFound, "no group %s on this hab", id)
		}
		if why := groups.Movable(*g); why != "" {
			return nil, control.Errorf(api.ErrorKindConflict, "%s", why)
		}
		return g, nil
	}
	s.mu.Lock()
	_, err := movable()
	s.mu.Unlock()
	if err != nil {
		return api.GroupMoveResult{}, err
	}
	if to == s.cfg.self {
		return refuse(api.ErrorKindInvalid, "this hab is the group's home already")
	}
	l, label, up := s.linkTo(cl, to)
	if !up {
		return refuse(api.ErrorKindUnavailable, "%s is offline or has no link from this hab", nameOr(label, to))
	}
	for _, c := range []string{groups.Capability, "logbook"} {
		if !slices.Contains(l.Caps, c) {
			return refuse(api.ErrorKindConflict, "%s lacks the %s capability", label, c)
		}
	}
	s.mu.Lock()
	g, err := movable()
	if err != nil {
		s.mu.Unlock()
		return api.GroupMoveResult{}, err
	}
	if pg, ok := project(cl, g.Plan.Project); !ok || folderOn(pg, to) == "" {
		s.mu.Unlock()
		return refuse(api.ErrorKindConflict, "%s does not have project %s checked out; set it up there to move the group", label, g.Plan.Project)
	}
	// 1. Freeze; 2. drain the lane (before its push: back to the front of the queue).
	gen := max(g.Generation, g.Tried) + 1
	g.Tried = gen
	g.Moving = &api.GroupMoveRef{To: to, Label: label, Generation: gen, Since: stampT(s.cfg.now())}
	g.MoveFailed = ""
	groups.Drain(g)
	s.saveLocked(g)
	s.mu.Unlock()

	// A member start in flight finishes first, so its outcome moves with the record.
	deadline := time.Now().Add(moveStartWait)
	for {
		s.mu.Lock()
		g = s.groups[id]
		busy := g != nil && (s.running[id] || slices.ContainsFunc(g.Members, func(m api.GroupMember) bool { return m.State == groups.MemberStarting }))
		s.mu.Unlock()
		if !busy || time.Now().After(deadline) {
			break
		}
		select {
		case <-s.stop:
			return refuse(api.ErrorKindUnavailable, "the hab is stopping")
		case <-time.After(100 * time.Millisecond):
		}
	}

	// 3. Adopt.
	s.mu.Lock()
	g = s.groups[id]
	if g == nil || g.Moving == nil || g.Moving.Generation != gen {
		s.mu.Unlock()
		return refuse(api.ErrorKindConflict, "group %s changed during the move", id)
	}
	rec := s.portable(cl, *g)
	s.mu.Unlock()
	actx, cancel := context.WithTimeout(s.base, s.cfg.keys.MoveAnswer)
	err = l.Caller.Call(actx, "group.adopt", api.GroupAdoptParams{Record: rec, Generation: gen}, &api.Empty{})
	cancel()

	s.mu.Lock()
	g = s.groups[id]
	if g == nil || g.Moving == nil || g.Moving.Generation != gen {
		s.mu.Unlock()
		return refuse(api.ErrorKindConflict, "group %s changed during the move", id)
	}
	if err != nil {
		// 5. No answer, or a refusal: the group stays, and the card says why. No tombstone,
		// so a late answer commits nothing.
		why := errText(err)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(actx.Err(), context.DeadlineExceeded) {
			why = fmt.Sprintf("no answer in %s", s.cfg.keys.MoveAnswer)
		}
		g.Moving = nil
		g.MoveFailed = fmt.Sprintf("the move to %s did not commit: %s", label, why)
		failed := g.MoveFailed
		s.saveLocked(g)
		s.mu.Unlock()
		s.kick(id)
		return refuse(api.ErrorKindUnavailable, "%s", failed)
	}
	// 4. Commit: the tombstone. From here the group lives on the new home.
	final := s.portable(cl, *g)
	g.Moving = nil
	g.Moved = &api.GroupMoveRef{To: to, Label: label, Generation: gen, Since: stampT(s.cfg.now())}
	if err := s.saveLocked(g); err != nil {
		// No tombstone on disk, no commit: the group stays here, and the new home's pending
		// copy goes once the catalog shows the attempt ended (Tried at its generation).
		g.Moved = nil
		g.MoveFailed = fmt.Sprintf("the move to %s did not commit: the tombstone was not saved: %v", label, err)
		failed := g.MoveFailed
		s.saveLocked(g)
		s.mu.Unlock()
		s.kick(id)
		return refuse(api.ErrorKindUnavailable, "%s", failed)
	}
	delete(s.groups, id)
	s.tombs[id] = g
	title := g.Title
	s.mu.Unlock()
	s.sendCommit(id, to, gen, &final)
	return api.GroupMoveResult{State: "moved", Home: to, Text: fmt.Sprintf("group %s moved home to %s", title, label)}, nil
}

// portable is the record as it goes to the new home: each member names its hab's device, so
// the new home can label the hosts its own way. The caller holds s.mu.
func (s *groupsService) portable(cl api.CatalogList, g api.Group) api.Group {
	c := copyGroup(g)
	for i := range c.Members {
		if c.Members[i].Device == "" {
			if dev, ok := s.hostDevice(cl, c.Members[i].Host); ok {
				c.Members[i].Device = dev
			}
		}
	}
	if c.Coordinator.Device == "" {
		c.Coordinator.Device = s.cfg.self
	}
	c.Moved = nil // Moving stays: it carries this hab's label for the new home
	return c
}

// sendCommit sends group.adopt_commit and marks the tombstone acknowledged on its answer.
func (s *groupsService) sendCommit(id, to string, gen uint64, rec *api.Group) {
	s.mu.Lock()
	if s.committing[id] {
		s.mu.Unlock()
		return
	}
	s.committing[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.committing, id)
		s.mu.Unlock()
	}()
	if groupSkipCommitSeam {
		return
	}
	l, _, up := s.linkTo(s.cfg.catalog(), to)
	if !up {
		return
	}
	ctx, cancel := context.WithTimeout(s.base, s.cfg.keys.MoveAnswer)
	defer cancel()
	if err := l.Caller.Call(ctx, "group.adopt_commit", api.GroupAdoptCommitParams{Group: id, Generation: gen, Record: rec}, &api.Empty{}); err != nil {
		if errKind(err) != api.ErrorKindNotFound {
			s.cfg.logf("groups: %s: adopt_commit to %s: %v; sent again later", id, to, err)
			return
		}
		// The new home holds no copy of this attempt: nothing can commit it there. Said once;
		// the tombstone is marked so it is not sent again.
		s.cfg.logf("groups: %s: %s holds no copy of the move at generation %d: %v", id, to, gen, err)
		if s.cfg.notice != nil {
			s.cfg.notice("Group move lost", fmt.Sprintf("Group %s moved to %s, which holds no copy of it. Check the group on both hosts.", id, to))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.tombs[id]; t != nil && t.Moved != nil && t.Moved.Generation == gen && !t.Moved.Acked {
		t.Moved.Acked = true
		s.saveLocked(t)
	}
}

// Adopt is group.adopt on the new home (5.7 step 3): the record is kept pending.
func (s *groupsService) Adopt(ctx context.Context, p api.GroupAdoptParams) error {
	peer, ok := control.CallerPeer(ctx)
	rec := p.Record
	switch {
	case !ok:
		return control.Errorf(api.ErrorKindDenied, "group.adopt comes over a peer link only")
	case !groups.ValidID(rec.ID):
		return control.Errorf(api.ErrorKindInvalid, "group id %q is not 16 hex digits", rec.ID)
	case rec.Home != peer.DeviceID:
		return control.Errorf(api.ErrorKindDenied, "only group %s's home hab hands it over", rec.ID)
	case p.Generation <= rec.Generation:
		return control.Errorf(api.ErrorKindInvalid, "generation %d is not above the record's %d", p.Generation, rec.Generation)
	}
	cl := s.cfg.catalog()
	if pg, ok := project(cl, rec.Plan.Project); !ok || folderOn(pg, s.cfg.self) == "" {
		return control.Errorf(api.ErrorKindConflict, "%s does not have project %s checked out", s.cfg.host, rec.Plan.Project)
	}
	var catGen uint64
	for _, h := range cl.Hosts {
		if h.Catalog.DeviceID == peer.DeviceID {
			catGen = h.Catalog.Generation
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.groups[rec.ID]; g != nil {
		return control.Errorf(api.ErrorKindConflict, "group %s is live on this hab", rec.ID)
	}
	if pd := s.pending[rec.ID]; pd != nil {
		if pd.Generation == p.Generation {
			return nil // the same attempt again
		}
		if pd.Generation > p.Generation {
			return control.Errorf(api.ErrorKindConflict, "this hab holds group %s at generation %d", rec.ID, pd.Generation)
		}
	}
	if t := s.tombs[rec.ID]; t != nil && t.Moved != nil && t.Moved.Generation >= p.Generation {
		return control.Errorf(api.ErrorKindConflict, "this hab moved group %s on at generation %d", rec.ID, t.Moved.Generation)
	}
	c := copyGroup(rec)
	c.Home, c.Generation, c.Pending, c.Tried = s.cfg.self, p.Generation, true, max(rec.Tried, p.Generation)
	c.Moving, c.Moved, c.MoveFailed = nil, nil, ""
	c.From = &api.GroupFrom{Device: peer.DeviceID, Catalog: catGen}
	if rec.Moving != nil {
		c.From.Label = rec.Moving.Label
	}
	if c.Coordinator.Device == "" {
		c.Coordinator.Device = peer.DeviceID
	}
	s.relabel(cl, &c)
	if err := s.st.Save(c); err != nil {
		return control.Errorf(api.ErrorKindUnavailable, "the record was not saved: %v", err)
	}
	delete(s.tombs, c.ID)
	s.pending[c.ID] = &c
	if !slices.Contains(s.order, c.ID) {
		s.order = append(s.order, c.ID)
	}
	return nil
}

// relabel names each member's host as this hab labels it, by its device.
func (s *groupsService) relabel(cl api.CatalogList, g *api.Group) {
	byName := map[string]string{}
	for i := range g.Members {
		m := &g.Members[i]
		if m.Device == "" {
			continue
		}
		if l := s.labelOf(cl, m.Device); l != "" {
			m.Host = l
		} else {
			s.cfg.logf("groups: %s: member %s's hab %s is not in this hab's catalog", g.ID, m.Name, m.Device)
		}
		byName[m.Name] = m.Host
	}
	for i := range g.Plan.Members {
		if h, ok := byName[g.Plan.Members[i].Name]; ok {
			g.Plan.Members[i].Host = h
		}
	}
}

// AdoptCommit is group.adopt_commit on the new home (5.7 step 4).
func (s *groupsService) AdoptCommit(ctx context.Context, p api.GroupAdoptCommitParams) error {
	cl := s.cfg.catalog()
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.groups[p.Group]; g != nil && g.Generation == p.Generation {
		return nil // committed already, from the catalog or an earlier message
	}
	if t := s.tombs[p.Group]; t != nil && t.Moved != nil && t.Moved.Generation > p.Generation {
		return nil // committed here, then moved on
	}
	if g := s.groups[p.Group]; g != nil && g.Generation > p.Generation {
		return nil // committed here, then moved away and back
	}
	pd := s.pending[p.Group]
	if pd == nil || pd.Generation != p.Generation {
		return control.Errorf(api.ErrorKindNotFound, "no pending copy of group %s at generation %d on this hab", p.Group, p.Generation)
	}
	if peer, _ := control.CallerPeer(ctx); pd.From != nil && peer.DeviceID != pd.From.Device {
		return control.Errorf(api.ErrorKindDenied, "only group %s's old home commits its move", p.Group)
	}
	s.commitLocked(cl, pd, p.Record)
	return nil
}

// commitLocked makes a pending copy the live record: with the old home's final record when the
// commit message carried one. The caller holds s.mu.
func (s *groupsService) commitLocked(cl api.CatalogList, pd *api.Group, final *api.Group) {
	g := pd
	if final != nil && final.ID == pd.ID {
		c := copyGroup(*final)
		c.Home, c.Generation, c.Tried, c.Coordinator.Device = pd.Home, pd.Generation, pd.Tried, pd.Coordinator.Device
		c.Moving, c.Moved, c.MoveFailed = nil, nil, ""
		s.relabel(cl, &c)
		g = &c
	}
	label := s.cfg.host
	if pd.From != nil && pd.From.Label != "" {
		label = pd.From.Label // as the old home, and so most likely the coordinator, names this hab
	}
	g.Pending, g.From = false, nil
	for j := range g.Members {
		if g.Members[j].State == groups.MemberStarting {
			g.Members[j].State = groups.MemberPlanned // tried again under the same key
		}
	}
	delete(s.pending, g.ID)
	s.groups[g.ID] = g
	s.tellLocked(g, fmt.Sprintf("group %s moved home to %s", g.Title, label), "Flightdeck")
	s.saveLocked(g)
	id := g.ID
	go s.kick(id)
	s.bgLocked(func() { s.flushRemote(id) })
}

// Tell is group.tell on the coordinator's hab: a note from Flightdeck while it runs.
func (s *groupsService) Tell(ctx context.Context, p api.GroupTellParams) error {
	// Only the group's home tells its coordinator: the hab this hab's tombstone names, or the
	// home the catalog copies name.
	peer, _ := control.CallerPeer(ctx)
	home := false
	if t := s.tomb(p.Group); t != nil {
		home = t.Moved.To == peer.DeviceID && t.Coordinator.Session == p.Session
	}
	if w, ok := groups.Find(s.cfg.catalog(), p.Group); !home && ok && w.Device == peer.DeviceID {
		home = w.Entry.Coordinator != nil && w.Entry.Coordinator.Session == p.Session && w.Entry.Coordinator.Device == s.cfg.self
	}
	if !home || peer.DeviceID == "" {
		return control.Errorf(api.ErrorKindDenied, "only group %s's home tells its coordinator", p.Group)
	}
	ss, ok := s.cfg.store.Session(p.Session)
	if !ok || ss.State != "running" || ss.Pane == "" || (s.cfg.ready != nil && !s.cfg.ready(ss.Pane)) {
		return control.Errorf(api.ErrorKindConflict, "the coordinator is not running on this hab")
	}
	if s.cfg.note == nil {
		return control.Errorf(api.ErrorKindUnavailable, "this hab cannot send notes")
	}
	if err := s.cfg.note(ss.ID, p.Text); err != nil {
		return control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	return nil
}

// flushRemote hands the inbox of a group whose coordinator runs on another hab to that hab, as
// one message. It reports whether the reports went.
func (s *groupsService) flushRemote(id string) bool {
	s.mu.Lock()
	g := s.groups[id]
	if g == nil || groups.CoordinatorDevice(*g) == s.cfg.self || len(g.Inbox) == 0 || s.flushing[id] {
		s.mu.Unlock()
		return false
	}
	s.flushing[id] = true
	dev, sess, n := groups.CoordinatorDevice(*g), g.Coordinator.Session, len(g.Inbox)
	sent := slices.Clone(g.Inbox)
	text := groups.InboxText(*g)
	if n == 1 {
		r := g.Inbox[0]
		text = fmt.Sprintf("Group %s (%s), from %s: %s", g.ID, g.Title, nameOr(r.From, "Flightdeck"), r.Text)
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.flushing, id)
		s.mu.Unlock()
	}()
	l, _, up := s.linkTo(s.cfg.catalog(), dev)
	if !up {
		return false
	}
	ctx, cancel := context.WithTimeout(s.base, 30*time.Second)
	defer cancel()
	if err := l.Caller.Call(ctx, "group.tell", api.GroupTellParams{Group: id, Session: sess, Text: text}, &api.Empty{}); err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g = s.groups[id]; g != nil {
		for _, r := range sent {
			if i := slices.Index(g.Inbox, r); i >= 0 {
				g.Inbox = slices.Delete(g.Inbox, i, i+1)
			}
		}
		s.saveLocked(g)
	}
	return true
}

// moveTick runs with the watch: tombstones past tombstone_keep go and unacknowledged commits
// are sent again; pending copies commit or go by the catalog; a remote coordinator's inbox is
// handed over; queued moves run once their home is back, or expire.
func (s *groupsService) moveTick() {
	cl := s.cfg.catalog()
	now := s.cfg.now()
	type commit struct {
		id, to string
		gen    uint64
	}
	var commits []commit
	var flush []string
	s.mu.Lock()
	for id, t := range s.tombs {
		at, err := time.Parse(time.RFC3339Nano, t.Moved.Since)
		if err == nil && at.Before(now.Add(-s.cfg.keys.TombstoneKeep)) {
			if err := s.st.Remove(id); err != nil {
				s.cfg.logf("groups: %s: %v", id, err)
			}
			delete(s.tombs, id)
			s.order = slices.DeleteFunc(s.order, func(o string) bool { return o == id })
			continue
		}
		if !t.Moved.Acked {
			commits = append(commits, commit{id, t.Moved.To, t.Moved.Generation})
		}
	}
	for id, pd := range s.pending {
		switch s.pendingVerdict(cl, pd) {
		case "commit":
			s.commitLocked(cl, pd, nil)
		case "stale":
			s.cfg.logf("groups: %s: the catalog shows the move to this hab did not commit; the pending copy goes", id)
			if err := s.st.Remove(id); err != nil {
				s.cfg.logf("groups: %s: %v", id, err)
			}
			delete(s.pending, id)
			s.order = slices.DeleteFunc(s.order, func(o string) bool { return o == id })
		}
	}
	for id, g := range s.groups {
		if len(g.Inbox) > 0 && groups.CoordinatorDevice(*g) != s.cfg.self {
			flush = append(flush, id)
		}
	}
	var run []groups.Queued
	keep := s.queued[:0:0]
	changed := false
	for _, q := range s.queued {
		at, err := time.Parse(time.RFC3339Nano, q.At)
		if err == nil && at.Before(now.Add(-s.cfg.keys.QueuedMove)) {
			changed = true
			if s.cfg.notice != nil {
				s.cfg.notice("Group move dropped", fmt.Sprintf("The move of group %s to this hab waited %s for %s and was dropped.", nameOr(q.Title, q.Group), s.cfg.keys.QueuedMove, nameOr(q.Label, q.Home)))
			}
			continue
		}
		keep = append(keep, q)
		if _, _, up := s.linkTo(cl, q.Home); up && !s.queuing[q.Group] {
			run = append(run, q)
		}
	}
	if changed {
		s.queued = keep
		if err := s.st.SaveQueued(s.queued); err != nil {
			s.cfg.logf("groups: queued moves: %v", err)
		}
	}
	for _, q := range run {
		s.queuing[q.Group] = true
	}
	s.mu.Unlock()
	for _, c := range commits {
		c := c
		s.bg(func() { s.sendCommit(c.id, c.to, c.gen, nil) })
	}
	for _, id := range flush {
		id := id
		s.bg(func() { s.flushRemote(id) })
	}
	for _, q := range run {
		q := q
		s.bg(func() { s.runQueued(q) })
	}
}

// pendingVerdict reads the catalog copies for a pending copy (5.7 steps 6 and 7): "commit"
// when a tombstone moves the group to this hab at its generation, "stale" when the group is
// live elsewhere or a tombstone names another move, else "".
func (s *groupsService) pendingVerdict(cl api.CatalogList, pd *api.Group) string {
	verdict := ""
	for _, h := range cl.Hosts {
		if h.Own {
			continue
		}
		fromOld := pd.From != nil && h.Catalog.DeviceID == pd.From.Device
		for _, e := range h.Catalog.Groups {
			if e.ID != pd.ID {
				continue
			}
			switch {
			case e.Moved != nil && e.Moved.To == s.cfg.self && e.Moved.Generation == pd.Generation:
				return "commit"
			case e.Moved != nil && e.Moved.Generation >= pd.Generation:
				verdict = "stale" // another move, at this generation or a later one
			case e.Moved == nil && fromOld && h.Catalog.Generation > pd.From.Catalog && e.Tried >= pd.Generation && (e.Moving == nil || e.Moving.Generation != pd.Generation):
				verdict = "stale" // the old home kept the group: the attempt did not commit
			case e.Moved == nil && !fromOld && e.Generation >= pd.Generation:
				verdict = "stale" // live on another hab at this generation or a later one
			}
		}
	}
	return verdict
}

// runQueued sends a queued move to the home once it is back: once, whatever its answer.
func (s *groupsService) runQueued(q groups.Queued) {
	defer func() {
		s.mu.Lock()
		delete(s.queuing, q.Group)
		s.mu.Unlock()
	}()
	cl := s.cfg.catalog()
	l, _, up := s.linkTo(cl, q.Home)
	if !up {
		return
	}
	ctx, cancel := context.WithTimeout(s.base, s.cfg.keys.MoveAnswer+moveStartWait+time.Minute)
	defer cancel()
	var out api.GroupMoveResult
	err := l.Caller.Call(ctx, "group.move", api.GroupMoveParams{Group: q.Group, Device: s.cfg.self}, &out)
	if err != nil && errKind(err) == "" {
		s.cfg.logf("groups: %s: the queued move: %v; tried again", q.Group, err)
		return // the link failed: the request stays
	}
	if err != nil && errKind(err) == api.ErrorKindUnavailable {
		// The home may not reach this hab yet, just after its start: a few more tries.
		s.mu.Lock()
		s.queueTries[q.Group]++
		n := s.queueTries[q.Group]
		s.mu.Unlock()
		if n < queuedTries {
			s.cfg.logf("groups: %s: the queued move: %v; tried again", q.Group, err)
			return
		}
	}
	s.mu.Lock()
	delete(s.queueTries, q.Group)
	s.mu.Unlock()
	s.mu.Lock()
	s.queued = slices.DeleteFunc(s.queued, func(o groups.Queued) bool { return o.Group == q.Group && o.At == q.At })
	if serr := s.st.SaveQueued(s.queued); serr != nil {
		s.cfg.logf("groups: queued moves: %v", serr)
	}
	s.mu.Unlock()
	if err != nil {
		s.cfg.logf("groups: %s: the queued move was refused: %v", q.Group, err)
		if s.cfg.notice != nil {
			s.cfg.notice("Group move refused", fmt.Sprintf("The queued move of group %s to this hab was refused: %s", nameOr(q.Title, q.Group), errText(err)))
		}
		return
	}
	s.cfg.logf("groups: %s: the queued move ran: %s", q.Group, out.Text)
}
