package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// The fresh-session swap of a group member (groups.md 9.3, 12.8), and the sibling actions'
// routing (9.4): the asker's hab finds the member's hab and hands the call to it.

// memberSwap replaces a member with a fresh session under its name, in its worktree.
type memberSwap struct {
	store *structure.Store
	// hold and move are the channels' (channels.Service.Hold, Move).
	hold      func(ctx context.Context, session, name string) (func(), error)
	move      func(from, to string) (int, error)
	rename    func(session, name string) error
	closePane func(ctx context.Context, pane string) error
	// create runs session.create on this hab, as its own confirmations do.
	create func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error)
	folder func(ctx context.Context, session string) string // nil or "": the record's
	logf   func(string, ...any)
}

// asideName is the first free <name>-<n> for the old session.
func (w *memberSwap) asideName(name string) string {
	for n := 1; ; n++ {
		cand := fmt.Sprintf("%s-%d", name, n)
		if _, taken := w.store.SessionByName(cand); !taken && channels.ValidName(cand) {
			return cand
		}
		if !channels.ValidName(cand) { // a name at the length limit: cut it
			name = name[:len(name)-1]
		}
	}
}

// swap holds deliveries to the old session, renames it <name>-<n> and takes it out of the
// group, starts the new member under the old name in the same folder, harness and model, with
// the old one's group and envelope, moves the held messages to it, and closes the old pane.
// The old conversation stays resumable. It returns the new session's id.
func (w *memberSwap) swap(ctx context.Context, ss api.Session, st api.AgentState, rec *api.Group, seed string) (string, error) {
	if ss.Group == nil {
		return "", fmt.Errorf("session %s is no member", ss.Name)
	}
	var model, folder string
	if rec != nil {
		for _, m := range rec.Members {
			if m.Name == ss.Name {
				model, folder = m.Model, m.Folder
			}
		}
	}
	if w.folder != nil {
		if f := w.folder(ctx, ss.ID); f != "" {
			folder = f
		}
	}
	if folder == "" {
		return "", fmt.Errorf("the member's folder is not known")
	}
	env, hasEnv := w.store.Envelope(ss.ID)
	var envp *api.GroupEnvelope
	conf := confirmationNone
	if hasEnv {
		envp = &env
		if env.Confirmation != "" {
			conf = env.Confirmation
		}
	}
	release, err := w.hold(ctx, ss.ID, ss.Name)
	if err != nil {
		return "", fmt.Errorf("holding its messages: %w", err)
	}
	defer release()
	name, group := ss.Name, *ss.Group
	aside := w.asideName(name)
	// The mark goes on disk first: a gff-hab that stops mid-swap finishes or undoes it at its
	// next start (repairSwaps).
	if err := w.store.SetSwap(ss.ID, &structure.SwapMark{Name: name, Aside: aside, Group: group, Envelope: envp}); err != nil {
		return "", err
	}
	if err := w.rename(ss.ID, aside); err != nil {
		w.store.SetSwap(ss.ID, nil)
		return "", fmt.Errorf("renaming the old session: %s", errText(err))
	}
	if err := w.store.SetGroup(ss.ID, nil, nil); err != nil {
		w.rename(ss.ID, name)
		w.store.SetSwap(ss.ID, nil)
		return "", err
	}
	undo := func() {
		w.store.SetGroup(ss.ID, &group, envp)
		if err := w.rename(ss.ID, name); err != nil {
			w.logf("supervise: putting back the name of %s: %v", aside, err)
		}
		w.store.SetSwap(ss.ID, nil)
	}
	key := sha256.Sum256([]byte(group.ID + "\x00" + name + "\x00fresh\x00" + ss.ID))
	p := api.SessionCreateParams{Name: name, ProjectDir: folder, Harness: st.Harness, Model: model, Seed: seed,
		IdempotencyKey: "group-" + hex.EncodeToString(key[:16]),
		Group:          &api.SessionGroup{ID: group.ID, Role: group.Role, Item: group.Item}, Envelope: envp,
		TrustCover: &api.TrustCover{Group: group.ID, Confirmation: conf, Harness: st.Harness, BranchPrefix: confirm.GroupBranchPrefix(group.ID)}}
	if rec != nil && validSeedFrom(rec.Coordinator.Address) {
		p.SeedFrom = rec.Coordinator.Address
	}
	made, err := w.create(withOwnCover(ctx), p)
	if err != nil {
		// A create that timed out may have made the session all the same: then the swap goes on
		// with it, so the group never has two sessions named for the member (review M6).
		id, ok := swapSuccessor(w.store, ss.ID, name, group.ID)
		if !ok {
			undo()
			return "", err
		}
		w.logf("supervise: %s: the create answered %v, but the fresh session %s runs; the swap goes on", name, err, id)
		made = api.SessionCreated{Session: id}
	}
	if err := w.store.SetReplaced(made.Session, ss.ID); err != nil {
		w.logf("supervise: %s: replaced: %v", name, err)
	}
	if n, err := w.move(ss.ID, made.Session); err != nil {
		w.logf("supervise: %s: moving its held messages: %v", name, err)
	} else if n > 0 {
		w.logf("supervise: %s: %d held messages moved to the fresh session", name, n)
	}
	if ss.Pane != "" {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := w.closePane(cctx, ss.Pane); err != nil {
			w.logf("supervise: closing the old pane of %s (%s): %v", name, aside, err)
		}
		cancel()
	}
	if err := w.store.SetSwap(ss.ID, nil); err != nil {
		w.logf("supervise: %s: clearing the swap mark: %v", name, err)
	}
	return made.Session, nil
}

// swapSuccessor is the session that took a member's name in its group: the fresh session of a
// swap, when it was made.
func swapSuccessor(store *structure.Store, old, name, group string) (string, bool) {
	s2, ok := store.SessionByName(name)
	if !ok || s2.ID == old || s2.Group == nil || s2.Group.ID != group {
		return "", false
	}
	return s2.ID, true
}

// undoSwaps is the first half of the start repair (groups.md 9.5), run before the channels
// start: a swap a stopped gff-hab left with no fresh session is undone (the old session's
// group, envelope and name back, the mark gone). It returns the swaps to finish, by old
// session id: their fresh session runs, and their old session's deliveries stay held until
// finishSwaps moved them.
func undoSwaps(store *structure.Store, logf func(string, ...any)) map[string]string {
	finish := map[string]string{}
	for old, m := range store.Swaps() {
		ss, ok := store.Session(old)
		if !ok {
			continue
		}
		if id, ok := swapSuccessor(store, old, m.Name, m.Group.ID); ok {
			finish[old] = id
			continue
		}
		g := m.Group
		if err := store.SetGroup(old, &g, m.Envelope); err != nil {
			logf("supervise: undoing the swap of %s: %v", m.Name, err)
			continue
		}
		if ss.Name != m.Name {
			if _, err := store.RenameSession(old, m.Name); err != nil {
				logf("supervise: undoing the swap of %s: the name: %v", m.Name, err)
			}
		}
		store.SetSwap(old, nil)
		logf("supervise: %s: a swap cut short by a stop is undone", m.Name)
	}
	return finish
}

// finishSwaps is the second half of the start repair: the old session leaves the group, the
// fresh one records it, the held messages move to it, the old pane closes (tried until the
// holder answers, at most 2 minutes), then the hold and the mark go.
func finishSwaps(ctx context.Context, store *structure.Store, finish map[string]string, move func(from, to string) (int, error),
	unhold func(string), closePane func(ctx context.Context, pane string) error, logf func(string, ...any)) {
	for old, id := range finish {
		ss, ok := store.Session(old)
		if !ok {
			unhold(old)
			continue
		}
		if ss.Group != nil {
			store.SetGroup(old, nil, nil)
		}
		store.SetReplaced(id, old)
		if n, err := move(old, id); err != nil {
			logf("supervise: %s: moving its held messages: %v", ss.Name, err)
		} else if n > 0 {
			logf("supervise: %s: %d held messages moved to the fresh session", ss.Name, n)
		}
		go func(old string, ss api.Session) {
			done := false
			defer func() {
				if !done {
					unhold(old)
				}
			}()
			if ss.Pane != "" && ss.State == api.SessionStateRunning && closePane != nil {
				deadline := time.Now().Add(2 * time.Minute)
				for {
					cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					err := closePane(cctx, ss.Pane)
					cancel()
					var ce *control.Error
					if err == nil || (errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindNotFound) {
						break
					}
					if time.Now().After(deadline) || ctx.Err() != nil {
						logf("supervise: closing the old pane of %s: %v", ss.Name, err)
						return // the mark stays: the next start tries again
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(2 * time.Second):
					}
				}
			}
			done = true
			unhold(old)
			store.SetSwap(old, nil)
			logf("supervise: %s: a swap cut short by a stop is finished", ss.Name)
		}(old, ss)
	}
}

// --- sibling actions on the group service ---

// MemberAction is member.compact and member.handoff (groups.md 9.4). A member on this hab gets
// the action here; one on another hab gets the call over the link, and a pane's asker hears the
// end from this hab, which follows the action there. From a peer link only a member on this
// hab is acted on: a forwarded call never forwards again.
func (s *groupsService) MemberAction(ctx context.Context, kind api.MemberActionKind, p api.MemberActionParams) (api.MemberAction, error) {
	sup := s.cfg.supervisor.get()
	if kind != actCompact && kind != actHandoff {
		return api.MemberAction{}, control.Errorf(api.ErrorKindInvalid, "no sibling action %q", kind)
	}
	if p.Group == "" || p.Member == "" {
		return api.MemberAction{}, control.Errorf(api.ErrorKindInvalid, "member.%s needs group and member", kind)
	}
	peer, fromPeer := control.CallerPeer(ctx)
	asker := p.From
	if asker != "" && fromPeer {
		asker = peer.Host + "/" + asker
	}
	if ss, ok := s.cfg.store.SessionByName(p.Member); ok && ss.Group != nil && ss.Group.ID == p.Group && ss.State == api.SessionStateRunning {
		if sup == nil {
			return api.MemberAction{}, control.Errorf(api.ErrorKindUnavailable, "this hab runs no supervision")
		}
		tell := ""
		if !fromPeer {
			tell = p.From // a local pane's asker; a forwarding hab tells its own
		}
		return sup.Request(kind, ss, asker, tell)
	}
	if fromPeer {
		return api.MemberAction{}, control.Errorf(api.ErrorKindNotFound, "no member %s of group %s runs on this hab", p.Member, p.Group)
	}
	cl := s.cfg.catalog()
	for _, h := range cl.Hosts {
		if h.Own {
			continue
		}
		for _, a := range h.Catalog.Agents {
			if a.Group == nil || a.Group.ID != p.Group || a.Session != p.Member {
				continue
			}
			l, ok := s.link(h.Label)
			if !ok || !l.Online || l.Caller == nil {
				return api.MemberAction{}, control.Errorf(api.ErrorKindUnavailable, "member %s's host %s is offline; try again once it is back", p.Member, h.Label)
			}
			var out api.MemberActionResult
			if err := l.Caller.Call(ctx, "member."+string(kind), p, &out); err != nil {
				return api.MemberAction{}, err
			}
			if p.From != "" {
				s.follow(h.Label, out.Action, p.From)
			}
			return out.Action, nil
		}
	}
	return api.MemberAction{}, control.Errorf(api.ErrorKindNotFound, "no member %s of group %s on this hab or in the catalog", p.Member, p.Group)
}

// followPoll is how often this hab reads an action it forwarded.
var followPoll = 2 * time.Second

// follow reads an action on another hab until it ends, then tells the local asker. A link that
// stays down past the action's longest wait gives up.
func (s *groupsService) follow(label string, a api.MemberAction, asker string) {
	go func() {
		limit := time.Now().Add(s.cfg.keys.SiblingWait + 2*s.cfg.keys.HandoffNoteWait + 10*time.Minute)
		for time.Now().Before(limit) {
			select {
			case <-s.stop:
				return
			case <-time.After(followPoll):
			}
			l, ok := s.link(label)
			if !ok || !l.Online || l.Caller == nil {
				continue
			}
			var out api.MemberActionResult
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err := l.Caller.Call(ctx, "member.action", api.MemberActionRef{ID: a.ID}, &out)
			cancel()
			if err != nil {
				continue
			}
			switch out.Action.State {
			case actDone, actSkipped, actFailed:
				if s.cfg.noteName != nil {
					if err := s.cfg.noteName(asker, actionText(out.Action)); err != nil {
						s.cfg.logf("groups: telling %s: %v", asker, err)
					}
				}
				return
			}
		}
	}()
}

// MemberActionGet is member.action: an action on this hab.
func (s *groupsService) MemberActionGet(_ context.Context, id string) (api.MemberAction, error) {
	if sup := s.cfg.supervisor.get(); sup != nil {
		if a, ok := sup.Get(id); ok {
			return a, nil
		}
	}
	return api.MemberAction{}, control.Errorf(api.ErrorKindNotFound, "no action %s on this hab", id)
}
