package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/groups"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// A group member's hab (groups.md 2.2, 3.3, 5.6): it keeps each member's envelope on its
// session record, so a later start from that member inside the envelope asks nothing, also
// while the home hab sleeps; and it holds messages into members of a paused group.

// groupRunnerRef hands the confirmations and the catalog the group service once it runs: both
// are made before it.
type groupRunnerRef struct {
	p atomic.Pointer[groupsService]
}

var _ confirm.GroupRunner = (*groupRunnerRef)(nil)

func (r *groupRunnerRef) svc() (*groupsService, error) {
	if s := r.p.Load(); s != nil {
		return s, nil
	}
	return nil, control.Errorf(api.ErrorKindUnavailable, "this hab keeps no groups: it runs without its holder link")
}

func (r *groupRunnerRef) CheckPlan(g api.ConfirmGroup) (api.ConfirmGroup, bool, error) {
	s, err := r.svc()
	if err != nil {
		return g, false, err
	}
	return s.CheckPlan(g)
}

func (r *groupRunnerRef) RunGroup(ctx context.Context, id string, g api.ConfirmGroup, confirmed bool) (api.ConfirmResult, error) {
	s, err := r.svc()
	if err != nil {
		return api.ConfirmResult{}, err
	}
	return s.RunGroup(ctx, id, g, confirmed)
}

func (r *groupRunnerRef) GroupClosed(c api.Confirmation) {
	if s := r.p.Load(); s != nil {
		s.GroupClosed(c)
	}
}

func (r *groupRunnerRef) section() []api.CatalogGroup {
	if s := r.p.Load(); s != nil {
		return s.Section()
	}
	return nil
}

// groupPaused reports whether the catalog (this hab's own section and the copies) lists the
// group as paused, by the user or for usage.
func groupPaused(cl api.CatalogList, group string) bool {
	g, ok := groupOf(cl, group)
	return ok && g.Pause != nil
}

// memberEnvelope is confirm.Config.Envelope: a start with a group from a session that is not a
// member of that group reports member false and follows the normal start rules (groups.md
// 3.3). A start from a member is inside its envelope when its host and harness are the plan's,
// its role is worker or reviewer, a worker works the member's own item within the envelope's
// workers per lead, and the group is not paused (groups.md 3.3, 5.6).
func memberEnvelope(store *structure.Store, host, self string, catalog func() api.CatalogList) func(from string, s api.ConfirmStart) (bool, error) {
	return func(from string, s api.ConfirmStart) (bool, error) {
		ss, ok := store.SessionByName(from)
		if !ok || ss.Group == nil || ss.Group.ID != s.Group {
			return false, nil
		}
		env, ok := store.Envelope(ss.ID)
		if !ok {
			return true, control.Errorf(api.ErrorKindInvalid, "this session's group %s left it no envelope; ask the coordinator to add the member with group_amend", s.Group)
		}
		role, item := memberDefaults(ss, s)
		cl := catalog()
		dev := self
		if s.Host != "" && s.Host != host {
			dev = ""
			for _, h := range cl.Hosts {
				if h.Label == s.Host {
					dev = h.Catalog.DeviceID
				}
			}
		}
		switch {
		case role != groups.RoleWorker && role != groups.RoleReviewer:
			return true, control.Errorf(api.ErrorKindInvalid, "a member starts workers and reviewers; the home hab starts leads")
		case dev == "" || !slices.Contains(env.Hosts, dev):
			return true, control.Errorf(api.ErrorKindInvalid, "%s is not a host of group %s's plan; the coordinator adds it with group_amend", s.Host, s.Group)
		case !slices.Contains(env.Harnesses, s.Harness):
			return true, control.Errorf(api.ErrorKindInvalid, "%s is not a harness of group %s's plan; the coordinator adds it with group_amend", s.Harness, s.Group)
		case groupPaused(cl, s.Group):
			return true, control.Errorf(api.ErrorKindUnavailable, "group %s is paused; no new member starts until it is resumed", s.Group)
		}
		if role == groups.RoleWorker {
			if item != ss.Group.Item {
				return true, control.Errorf(api.ErrorKindInvalid, "a worker works its lead's item %s", ss.Group.Item)
			}
			n := 0
			for _, o := range store.Snapshot(api.HostInfo{}).Sessions {
				if o.Group != nil && o.Group.ID == s.Group && o.Group.Role == groups.RoleWorker && o.Group.Item == item && o.State == "running" {
					n++
				}
			}
			if n >= env.MaxWorkers {
				return true, control.Errorf(api.ErrorKindInvalid, "item %s has %d workers running; the plan allows %d per lead", item, n, env.MaxWorkers)
			}
		}
		return true, nil
	}
}

// memberDefaults are a member's start's role and item: worker, and the member's own item.
func memberDefaults(ss api.Session, s api.ConfirmStart) (api.GroupRole, string) {
	role, item := api.GroupRole(s.Role), s.Item
	if role == "" {
		role = groups.RoleWorker
	}
	if item == "" && ss.Group != nil {
		item = ss.Group.Item
	}
	return role, item
}

// groupHook answers the group methods from a pane (groups.md 15.2): the pane's session is the
// coordinator of a new group, and the sender of a report or a note. Only the coordinator amends,
// closes or retries.
func groupHook(store *structure.Store, svc *groupsService) hook.ChannelHandler {
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		ss, ok := store.SessionByPane(pane)
		if !ok {
			return nil, nil, control.Errorf(api.ErrorKindInvalid, "this pane is not a session; a group's calls come from a session")
		}
		bad := func(err error) error { return control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err) }
		// record reads the group: on this hab, else from its home (a moved group, groups.md 5.7).
		record := func(id string) (api.Group, bool, error) {
			g, err := svc.Get(id)
			if err == nil {
				return g, true, nil
			}
			var r api.GroupResult
			if ferr := svc.forward(ctx, id, "group.get", api.GroupRef{Group: id}, &r); ferr != nil {
				return api.Group{}, false, err
			}
			return r.Group, false, nil
		}
		isCoordinator := func(g api.Group) bool {
			return g.Coordinator.Session == ss.ID && groups.CoordinatorDevice(g) == svc.cfg.self
		}
		// coordinator checks the caller is the group's coordinator, and reports whether this
		// hab is the group's home.
		coordinator := func(id string) (bool, error) {
			g, local, err := record(id)
			if err != nil {
				return false, err
			}
			if !isCoordinator(g) {
				return false, control.Errorf(api.ErrorKindDenied, "only group %s's coordinator %s calls %s", id, g.Coordinator.Name, method)
			}
			return local, nil
		}
		// inGroup: the caller is a member of the group (its session record says so), or its
		// coordinator on the home hab (groups.md 15.2: reports and notes come from members).
		inGroup := func(id string) error {
			if ss.Group != nil && ss.Group.ID == id {
				return nil
			}
			if g, _, err := record(id); err == nil && isCoordinator(g) {
				return nil
			}
			return control.Errorf(api.ErrorKindDenied, "session %s is not a member of group %s", ss.Name, id)
		}
		switch method {
		case "group.create":
			var p api.GroupCreateParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			p.Coordinator = ss.Name
			r, err := svc.Create(ctx, p)
			return r, nil, err
		case "group.amend":
			var p api.GroupAmendParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			local, err := coordinator(p.Group)
			if err != nil {
				return nil, nil, err
			}
			if !local {
				var r api.GroupCreated
				err := svc.forward(ctx, p.Group, method, p, &r)
				return r, nil, err
			}
			r, err := svc.Amend(ctx, p)
			return r, nil, err
		case "group.get":
			var p api.GroupRef
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			g, err := svc.Get(p.Group)
			if err != nil {
				// A member on another hab, or the coordinator of a moved group, reads the record
				// from the home.
				r, _, rerr := record(p.Group)
				if rerr == nil && ((ss.Group != nil && ss.Group.ID == p.Group) || isCoordinator(r)) {
					return api.GroupResult{Group: r}, nil, nil
				}
			}
			return api.GroupResult{Group: g}, nil, err
		case "group.list":
			var p api.GroupListParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			return api.GroupList{Groups: svc.List(p.State)}, nil, nil
		case "group.report":
			var p api.GroupReportParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			if err := inGroup(p.Group); err != nil {
				return nil, nil, err
			}
			p.From = ss.Name
			r, err := svc.Report(ctx, p)
			return r, nil, err
		case "group.note":
			var p api.GroupNoteParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			if err := inGroup(p.Group); err != nil {
				return nil, nil, err
			}
			if ss.Group != nil && ss.Group.ID == p.Group && ss.Group.Item != "" && p.Item != ss.Group.Item {
				return nil, nil, control.Errorf(api.ErrorKindDenied, "a member notes its own item %s", ss.Group.Item)
			}
			p.From = ss.Name
			err := svc.Note(ctx, p)
			if sup := svc.cfg.supervisor.get(); err == nil && sup != nil {
				sup.Noted(ss.ID) // a hand-off that waits for this note goes on (groups.md 9.3)
			}
			return api.Empty{}, nil, err
		case "member.compact", "member.handoff":
			// A sibling action (groups.md 9.4): a member of the group or its coordinator asks;
			// an agent cannot act on a member outside its own group.
			var p api.MemberActionParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			if err := inGroup(p.Group); err != nil {
				return nil, nil, err
			}
			p.From = ss.Name
			a, err := svc.MemberAction(ctx, api.MemberActionKind(strings.TrimPrefix(method, "member.")), p)
			return api.MemberActionResult{Action: a}, nil, err
		case "member.action":
			var p api.MemberActionRef
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			a, err := svc.MemberActionGet(ctx, p.ID)
			return api.MemberActionResult{Action: a}, nil, err
		case "group.close":
			var p api.GroupRef
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			local, err := coordinator(p.Group)
			if err != nil {
				return nil, nil, err
			}
			if !local {
				var r api.GroupResult
				err := svc.forward(ctx, p.Group, method, p, &r)
				return r, nil, err
			}
			g, err := svc.Close(ctx, p.Group)
			return api.GroupResult{Group: g}, nil, err
		case "group.move":
			return nil, nil, control.Errorf(api.ErrorKindDenied, "an agent cannot move a group's home; the user moves it (groups.md 5.7)")
		default: // group.retry
			var p api.GroupRetryParams
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, nil, bad(err)
			}
			local, err := coordinator(p.Group)
			if err != nil {
				return nil, nil, err
			}
			if !local {
				var r api.GroupResult
				err := svc.forward(ctx, p.Group, method, p, &r)
				return r, nil, err
			}
			g, err := svc.Retry(ctx, p)
			return api.GroupResult{Group: g}, nil, err
		}
	}
}
