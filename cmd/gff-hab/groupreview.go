package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/groups"
)

// Review on the home hab (groups.md 6): the lead's submit, the reviewer's start and its new
// heads, the verdict with its findings and fixes, the coordinator's one line, and Flightdeck's
// notes into members on other habs. The lead's and the reviewer's habs read the commits
// (groupmember.go); the home hab takes them from its own route, or from the member's hab over
// the link.

// Submit is lane.submit on a control connection: a peer hab's forward of its lead's submit.
// Anything else is refused: a submit comes from the lead's pane, whose hab reads the head.
func (s *groupsService) Submit(ctx context.Context, p api.LaneSubmitParams) (api.LaneSubmitted, error) {
	peer, ok := s.linkedPeer(ctx)
	if !ok {
		return api.LaneSubmitted{}, control.Errorf(api.ErrorKindDenied, "lane.submit comes from the item's lead, through its pane, or from a peer hab in this hab's peer list")
	}
	return s.submit(ctx, p, peer.DeviceID)
}

// submit takes a submit on the home hab, or forwards one from this hab's pane to the home.
// device is the hab the lead runs on, as the link says: this hab for its own route.
func (s *groupsService) submit(ctx context.Context, p api.LaneSubmitParams, device string) (api.LaneSubmitted, error) {
	s.mu.Lock()
	g := s.groups[p.Group]
	if g == nil {
		s.mu.Unlock()
		var out api.LaneSubmitted
		err := s.forward(ctx, p.Group, "lane.submit", p, &out)
		return out, err
	}
	defer s.mu.Unlock()
	if !groups.Live(*g) || g.State == groups.StateConfirming {
		return api.LaneSubmitted{}, control.Errorf(api.ErrorKindConflict, "group %s is %s", g.ID, g.State)
	}
	ii := groups.ItemIndex(*g, p.Item)
	if ii < 0 {
		return api.LaneSubmitted{}, control.Errorf(api.ErrorKindNotFound, "group %s has no item %q", g.ID, p.Item)
	}
	if err := s.isMember(g, g.Items[ii].Lead, p.From, device, "lead"); err != nil {
		return api.LaneSubmitted{}, err
	}
	res, err := groups.Submit(g, p.Item, p.Head, s.cfg.now())
	if err != nil {
		return api.LaneSubmitted{}, err
	}
	it := g.Items[ii]
	out := api.LaneSubmitted{State: res.State, Head: p.Head, Reviewer: res.Reviewer, Restart: res.Restart}
	if res.Same {
		return out, nil
	}
	switch {
	case res.State == groups.ItemReady:
		s.tellLocked(g, fmt.Sprintf("item %s: submitted with review off; ready for the lane at %s", it.ID, groups.Short7(p.Head)), "Flightdeck")
	default:
		if ri := groups.MemberIndex(*g, it.Reviewer); ri >= 0 {
			switch rm := g.Members[ri]; rm.State {
			case groups.MemberRunning:
				if rm.Agent != api.AgentStatusExited {
					s.memberNoteLocked(g, rm, newHeadText(*g, it))
					break
				}
				s.tellLocked(g, fmt.Sprintf("item %s is submitted, but its reviewer %s exited; group_retry starts it again.", it.ID, rm.Name), "Flightdeck")
			case groups.MemberFailed, groups.MemberExited:
				s.tellLocked(g, fmt.Sprintf("item %s is submitted, but its reviewer %s is %s; group_retry starts it again.", it.ID, rm.Name, rm.State), "Flightdeck")
			}
		}
	}
	s.saveLocked(g)
	go s.kick(g.ID) // the reviewer starts; an item stacked on this one may start (groups.md 7.2)
	return out, nil
}

// isMember checks that the call comes from the item's member name, and over the link from the
// hab that member runs on. The home hab's own route passes its own device.
func (s *groupsService) isMember(g *api.Group, want, from, device, role string) error {
	if want == "" || from != want {
		return control.Errorf(api.ErrorKindDenied, "only the item's %s %s calls this", role, want)
	}
	mi := groups.MemberIndex(*g, want)
	if mi < 0 || g.Members[mi].Session == "" {
		return control.Errorf(api.ErrorKindConflict, "the %s %s has not started", role, want)
	}
	if d := g.Members[mi].Device; d != "" && d != device {
		return control.Errorf(api.ErrorKindDenied, "the %s %s runs on another hab", role, want)
	}
	return nil
}

// Verdict is review.verdict on a control connection: a peer hab's forward of its reviewer's
// verdict. Anything else is refused.
func (s *groupsService) Verdict(ctx context.Context, p api.ReviewVerdictParams) (api.ReviewVerdictTaken, error) {
	peer, ok := s.linkedPeer(ctx)
	if !ok {
		return api.ReviewVerdictTaken{}, control.Errorf(api.ErrorKindDenied, "review.verdict comes from the item's reviewer, through its pane, or from a peer hab in this hab's peer list")
	}
	return s.verdict(ctx, p, peer.DeviceID)
}

// verdict takes a verdict on the home hab, or forwards one from this hab's pane to the home.
func (s *groupsService) verdict(ctx context.Context, p api.ReviewVerdictParams, device string) (api.ReviewVerdictTaken, error) {
	if err := groups.CheckVerdict(p, s.cfg.keys.MaxFindings); err != nil {
		return api.ReviewVerdictTaken{}, err
	}
	s.mu.Lock()
	g := s.groups[p.Group]
	if g == nil {
		s.mu.Unlock()
		var out api.ReviewVerdictTaken
		err := s.forward(ctx, p.Group, "review.verdict", p, &out)
		return out, err
	}
	defer s.mu.Unlock()
	if !groups.Live(*g) || g.State == groups.StateConfirming {
		return api.ReviewVerdictTaken{}, control.Errorf(api.ErrorKindConflict, "group %s is %s", g.ID, g.State)
	}
	ii := groups.ItemIndex(*g, p.Item)
	if ii < 0 {
		return api.ReviewVerdictTaken{}, control.Errorf(api.ErrorKindNotFound, "group %s has no item %q", g.ID, p.Item)
	}
	if err := s.isMember(g, g.Items[ii].Reviewer, p.From, device, "reviewer"); err != nil {
		return api.ReviewVerdictTaken{}, err
	}
	out, err := groups.TakeVerdict(g, p, s.cfg.now())
	if err != nil {
		return api.ReviewVerdictTaken{}, err
	}
	it := g.Items[ii]
	v := *it.Verdict
	// The coordinator gets one line, never the review (groups.md 6.3).
	s.tellLocked(g, groups.VerdictLine(it.ID, v), "Flightdeck")
	if li := groups.MemberIndex(*g, it.Lead); li >= 0 && g.Members[li].State == groups.MemberRunning {
		lead := g.Members[li]
		switch {
		case v.Verdict == "fix_needed":
			s.memberNoteLocked(g, lead, groups.FindingsText(*g, it, v))
		case v.FixedHead != "":
			s.memberNoteLocked(g, lead, fmt.Sprintf("Group %s, item %s: the review passed with the reviewer's fixes on %s. Before any new change, fast-forward your branch to it: git merge --ff-only %s. The lane lands %s.",
				g.ID, it.ID, it.ReviewBranch, v.FixedHead, groups.Short7(v.FixedHead)))
		default:
			s.memberNoteLocked(g, lead, fmt.Sprintf("Group %s, item %s: the review passed; the item is ready for the lane at %s.", g.ID, it.ID, groups.Short7(it.Land)))
		}
	}
	s.saveLocked(g)
	go s.kick(g.ID)
	return out, nil
}

// memberNoteLocked sends a member a message from Flightdeck: on this hab through the channels,
// on another hab over the link with group.message. A failure is logged: the record and the
// coordinator's line hold what the message said. The caller holds s.mu.
func (s *groupsService) memberNoteLocked(g *api.Group, m api.GroupMember, text string) {
	if m.Session == "" {
		return
	}
	if m.Device == "" || m.Device == s.cfg.self {
		if s.cfg.note == nil {
			return
		}
		if err := s.cfg.note(m.Session, text); err != nil {
			s.cfg.logf("groups: %s: message to %s: %v", g.ID, m.Name, err)
		}
		return
	}
	if s.closed {
		return
	}
	id, host := g.ID, m.Host
	p := api.GroupMessageParams{Group: g.ID, Member: m.Name, Text: text}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		l, ok := s.link(host)
		if !ok || !l.Online || l.Caller == nil {
			s.cfg.logf("groups: %s: message to %s: %s is offline", id, p.Member, host)
			return
		}
		if err := l.Caller.Call(ctx, "group.message", p, &api.Empty{}); err != nil {
			s.cfg.logf("groups: %s: message to %s on %s: %v", id, p.Member, host, err)
		}
	}()
}

// maxGroupMessage bounds a group.message: every finding at the largest [groups] max_findings,
// under the channels' 64 KiB text limit.
const maxGroupMessage = 60000

// linkedPeer is the peer hab a call came over, when it is not this hab and this hab's peer
// list pins its device id. A hello alone names any device; the peer list is what this hab
// trusts (groups.md 6.5).
func (s *groupsService) linkedPeer(ctx context.Context) (api.PeerHello, bool) {
	peer, ok := control.CallerPeer(ctx)
	if !ok || peer.DeviceID == "" || peer.DeviceID == s.cfg.self || s.cfg.listedPeer == nil || !s.cfg.listedPeer(peer.DeviceID) {
		return api.PeerHello{}, false
	}
	return peer, true
}

// homeOf reports whether device is group's home hab: its own catalog copy lists the group
// with itself as the home. Another host's copy that claims the group is not taken.
func homeOf(cl api.CatalogList, group, device string) bool {
	for _, h := range cl.Hosts {
		if h.Catalog.DeviceID != device {
			continue
		}
		for _, g := range h.Catalog.Groups {
			if g.ID == group && g.Home == device {
				return true
			}
		}
	}
	return false
}

// Message is group.message: the home hab's note into a member on this hab. Only over the link
// from the group's home hab, as the catalog names it, and only into a member of that group.
func (s *groupsService) Message(ctx context.Context, p api.GroupMessageParams) error {
	peer, ok := s.linkedPeer(ctx)
	if !ok {
		return control.Errorf(api.ErrorKindDenied, "group.message comes from a group's home hab, a peer in this hab's peer list, over the link")
	}
	if strings.TrimSpace(p.Text) == "" || len(p.Text) > maxGroupMessage {
		return control.Errorf(api.ErrorKindInvalid, "a message is 1 to %d bytes", maxGroupMessage)
	}
	if !homeOf(s.cfg.catalog(), p.Group, peer.DeviceID) {
		return control.Errorf(api.ErrorKindDenied, "%s is not group %s's home hab", peer.Host, p.Group)
	}
	ss, ok := s.cfg.store.SessionByName(p.Member)
	if !ok || ss.Group == nil || ss.Group.ID != p.Group {
		return control.Errorf(api.ErrorKindNotFound, "no member %s of group %s on this hab", p.Member, p.Group)
	}
	if s.cfg.note == nil {
		return control.Errorf(api.ErrorKindUnavailable, "this hab sends no messages")
	}
	return s.cfg.note(ss.ID, p.Text)
}

// stuckLocked tells the coordinator of each review whose reviewer exited or stayed idle
// [groups] reviewer_idle with no verdict (groups.md 6.3). The item stays in review. It reports
// whether the record changed.
func (s *groupsService) stuckLocked(g *api.Group) bool {
	before := make([]string, len(g.Items))
	for i, it := range g.Items {
		before[i] = it.IdleSince + "\x00" + it.Stuck
	}
	for _, st := range groups.StuckReviews(g, s.cfg.keys.ReviewerIdle, s.cfg.now()) {
		s.tellLocked(g, fmt.Sprintf("item %s: its reviewer %s %s. The item stays in review: nudge the reviewer, or group_retry starts it again once it exited.", st.Item, st.Reviewer, st.Why), "Flightdeck")
	}
	return !slices.EqualFunc(before, g.Items, func(b string, it api.GroupItem) bool { return b == it.IdleSince+"\x00"+it.Stuck })
}
