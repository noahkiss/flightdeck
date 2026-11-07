package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/aliases"
	"github.com/noahkiss/flightdeck/internal/confirm"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/groups"
	"github.com/noahkiss/flightdeck/internal/lbsvc"
	"github.com/noahkiss/flightdeck/internal/settings"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Agent groups on their home hab (groups.md 3, 5): the record and its file, one confirmation
// per plan, the Logbook plan and tasks, the leads' starts with the envelope and the trust
// cover, reports and the coordinator's inbox, and the user's pause. The home hab is the hab of
// the coordinator's session; a member's hab keeps its envelope on the session record and
// decides a later start alone (groupmember.go).

// groupsConfig is what the group service needs.
type groupsConfig struct {
	host string // this hab's host id
	self string // this hab's device id
	dir  string // <state>/groups
	keys groups.Keys

	store   *structure.Store
	catalog func() api.CatalogList
	links   func() []confirmLink
	aliases func() aliases.Table // nil: none
	// confirm asks the plan's confirmation (confirm.Service.Request).
	confirm func(ctx context.Context, p api.ConfirmRequestParams) (api.Confirmation, error)
	// create runs session.create on this hab; worktree runs worktree.create on this hab.
	create        func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error)
	worktree      func(ctx context.Context, p api.WorktreeCreateParams) (api.WorktreeCreated, error)
	listWorktrees func(ctx context.Context, p api.WorktreeListParams) (api.WorktreeList, error)
	// closePane closes a member's pane on this hab.
	closePane func(ctx context.Context, pane string) error
	logbook   *lbsvc.Service // nil: group.create refuses, "groups need Logbook"
	// note sends a message from Flightdeck to a session on this hab, by id.
	note    func(session, text string) error
	setting func(id string) json.RawMessage
	// ready reports whether a pane's agent takes a message now. Nil: once it runs.
	ready func(pane string) bool
	publish func(structure.Event)
	poke    func() // the catalog's groups section changed
	now     func() time.Time
	logf    func(string, ...any)
}

// groupsService keeps the groups this hab is the home of.
type groupsService struct {
	cfg groupsConfig
	st  *groups.Store

	mu      sync.Mutex
	groups  map[string]*api.Group
	order   []string        // ids, oldest first
	running map[string]bool // a scheduler pass runs for the group
	again   map[string]bool // another pass was asked for meanwhile
	panes   map[string]string
	closed  bool
	wg      sync.WaitGroup
	stop    chan struct{}
}

var _ control.Groups = (*groupsService)(nil)
var _ confirm.GroupRunner = (*groupsService)(nil)

// newGroups loads the records. A group still confirming lost its confirmation with the restart:
// it is dropped, and its coordinator told (groups.md 3.2). Ended and dropped groups past
// [groups] keep_ended go.
func newGroups(cfg groupsConfig) (*groupsService, error) {
	st, err := groups.NewStore(cfg.dir)
	if err != nil {
		return nil, err
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.logf == nil {
		cfg.logf = func(string, ...any) {}
	}
	s := &groupsService{cfg: cfg, st: st, groups: map[string]*api.Group{}, running: map[string]bool{}, again: map[string]bool{},
		panes: map[string]string{}, stop: make(chan struct{})}
	loaded, problems := st.Load(cfg.now())
	for _, p := range problems {
		cfg.logf("groups: %s", p)
	}
	now := cfg.now()
	for i := range loaded {
		g := loaded[i]
		if !groups.Live(g) && endedBefore(g, now.Add(-cfg.keys.KeepEnded)) {
			if err := st.Remove(g.ID); err != nil {
				cfg.logf("groups: %s: %v", g.ID, err)
			}
			continue
		}
		if g.State == groups.StateConfirming {
			s.dropLocked(&g, "the hab restarted before you answered; propose the plan again")
		}
		for j := range g.Members {
			if g.Members[j].State == groups.MemberStarting {
				// A start cut short by the restart: tried again under the same key, so it starts once.
				g.Members[j].State = groups.MemberPlanned
			}
		}
		s.groups[g.ID] = &g
		s.order = append(s.order, g.ID)
	}
	for _, id := range s.order {
		s.kick(id)
	}
	s.wg.Add(1)
	go s.watch()
	return s, nil
}

func endedBefore(g api.Group, cut time.Time) bool {
	at := g.Ended
	if at == "" {
		at = g.Updated
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	return err == nil && t.Before(cut)
}

// Stop stops the scheduler and waits for its passes.
func (s *groupsService) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.stop)
	s.mu.Unlock()
	s.wg.Wait()
}

func stampT(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// intSetting reads a whole-number hab-side setting.
func (s *groupsService) intSetting(id string, def int) int {
	if s.cfg.setting == nil {
		return def
	}
	if n, err := strconv.Atoi(string(s.cfg.setting(id))); err == nil {
		return n
	}
	return def
}

func (s *groupsService) boolSetting(id string, def bool) bool {
	if s.cfg.setting == nil {
		return def
	}
	switch string(s.cfg.setting(id)) {
	case "true":
		return true
	case "false":
		return false
	}
	return def
}

// confirmTTL is group.confirm_max_age.
func (s *groupsService) confirmTTL() time.Duration {
	return time.Duration(s.intSetting(settings.GroupConfirmMaxAge, 60)) * time.Minute
}

// saveLocked bumps the revision, writes the record and publishes it. The caller holds s.mu.
func (s *groupsService) saveLocked(g *api.Group) error {
	g.Revision++
	g.Updated = stampT(s.cfg.now())
	err := s.st.Save(*g)
	if err != nil {
		s.cfg.logf("groups: %s: save: %v", g.ID, err)
	}
	if s.cfg.publish != nil {
		s.cfg.publish(structure.Event{Type: control.EventGroupUpdated, ID: g.ID, Revision: g.Revision, Data: copyGroup(*g)})
	}
	if s.cfg.poke != nil {
		s.cfg.poke()
	}
	return err
}

func copyGroup(g api.Group) api.Group {
	b, _ := json.Marshal(g)
	var c api.Group
	json.Unmarshal(b, &c)
	return api.NonNil(c)
}

// --- the plan check ---

// hostDevice returns the device id of a plan host: this hab, or a peer label in the catalog.
func (s *groupsService) hostDevice(cl api.CatalogList, host string) (string, bool) {
	if host == "" || host == s.cfg.host {
		return s.cfg.self, true
	}
	for _, h := range cl.Hosts {
		if h.Label == host {
			return h.Catalog.DeviceID, true
		}
	}
	return "", false
}

// project finds the plan's project in the catalog by name, remote or key.
func project(cl api.CatalogList, name string) (api.CatalogProjectGroup, bool) {
	for _, p := range cl.Projects {
		if p.Name == name || (p.Remote != "" && p.Remote == name) || p.Key == name {
			return p, true
		}
	}
	return api.CatalogProjectGroup{}, false
}

// folderOn returns a folder of the project on a host: its first one.
func folderOn(p api.CatalogProjectGroup, device string) string {
	for _, l := range p.Locations {
		if l.DeviceID == device && len(l.Folders) > 0 {
			return l.Folders[0].Path
		}
	}
	return ""
}

// CheckPlan checks a plan before any card and after the user's edits (groups.md 3.1).
func (s *groupsService) CheckPlan(cg api.ConfirmGroup) (api.ConfirmGroup, bool, error) {
	if s.cfg.logbook == nil {
		return cg, false, control.Errorf(api.ErrorKindUnavailable, "groups need Logbook, which this hab runs without")
	}
	plan := cg.Plan
	full := plan // an amend is checked as the whole plan with the change in it
	s.mu.Lock()
	g := s.groups[cg.Group]
	if cg.Amend {
		if g == nil {
			s.mu.Unlock()
			return cg, false, control.Errorf(api.ErrorKindNotFound, "no group %s on this hab", cg.Group)
		}
		full = g.Plan
		full.Items = append(slices.Clone(full.Items), plan.Items...)
		full.Members = append(slices.Clone(full.Members), plan.Members...)
		plan.Title, plan.Brief, plan.Project, plan.Lane = full.Title, full.Brief, full.Project, full.Lane
	}
	s.mu.Unlock()
	for i := range plan.Members {
		plan.Members[i] = s.resolveMember(plan.Members[i])
	}
	for i := range full.Members {
		full.Members[i] = s.resolveMember(full.Members[i])
	}
	plan = groups.Normalize(plan, cg.Group)
	full = groups.Normalize(full, cg.Group)
	if err := groups.Check(full, s.intSetting(settings.GroupMaxWorkers, 2)); err != nil {
		return cg, false, err
	}
	cl := s.cfg.catalog()
	proj, ok := project(cl, plan.Project)
	if !ok {
		return cg, false, control.Errorf(api.ErrorKindNotFound, "no project %q in the catalog; the projects tool lists them", plan.Project)
	}
	elsewhere := false
	var hosts, harnessList []string
	for _, m := range plan.Members {
		if !slices.Contains(harnesses, m.Harness) {
			return cg, false, control.Errorf(api.ErrorKindInvalid, "member %s: harness %q is not one of %s", m.Name, m.Harness, strings.Join(harnesses, ", "))
		}
		dev, ok := s.hostDevice(cl, m.Host)
		if !ok {
			return cg, false, control.Errorf(api.ErrorKindNotFound, "member %s: no peer labelled %q; the hosts tool lists them", m.Name, m.Host)
		}
		label := m.Host
		if dev == s.cfg.self {
			label = s.cfg.host
		} else {
			elsewhere = true
			l, ok := s.link(m.Host)
			switch {
			case !ok || !l.Online || l.Caller == nil:
				return cg, false, control.Errorf(api.ErrorKindUnavailable, "member %s: %s is offline; plan it elsewhere, or once it is back", m.Name, m.Host)
			case !slices.Contains(l.Caps, groups.Capability):
				return cg, false, control.Errorf(api.ErrorKindUnavailable, "member %s: %s runs a gff-hab without groups; update it first", m.Name, m.Host)
			}
		}
		if folderOn(proj, dev) == "" {
			return cg, false, control.Errorf(api.ErrorKindInvalid, "member %s: set up the project on %s first", m.Name, label)
		}
		if where := s.nameInUse(cl, m.Name); where != "" {
			return cg, false, control.Errorf(api.ErrorKindConflict, "member %s: the name is in use on %s; name it another way", m.Name, where)
		}
		if !slices.Contains(hosts, label) {
			hosts = append(hosts, label)
		}
		if !slices.Contains(harnessList, m.Harness) {
			harnessList = append(harnessList, m.Harness)
		}
	}
	cg.Plan = plan
	cg.Trust = groups.TrustRule(cg.Group, hosts, harnessList)
	cg.Warnings = groups.Warnings(full)
	return cg, elsewhere, nil
}

// resolveMember turns a member's alias into its harness and model.
func (s *groupsService) resolveMember(m api.GroupPlanMember) api.GroupPlanMember {
	if s.cfg.aliases == nil {
		return m
	}
	if a, _, ok := s.cfg.aliases().Lookup(m.Harness); ok {
		m.Harness = a.Harness
		if m.Model == "" {
			m.Model = a.Model
		}
	}
	return m
}

// nameInUse returns where a session name is in use: this hab, or a host in the catalog.
func (s *groupsService) nameInUse(cl api.CatalogList, name string) string {
	if _, ok := s.cfg.store.SessionByName(name); ok {
		return s.cfg.host
	}
	for _, h := range cl.Hosts {
		if h.Own {
			continue
		}
		for _, a := range h.Catalog.Agents {
			if a.Session == name {
				return h.Label
			}
		}
	}
	return ""
}

func (s *groupsService) link(label string) (confirmLink, bool) {
	for _, l := range s.cfg.links() {
		if l.Label == label {
			return l, true
		}
	}
	return confirmLink{}, false
}

// --- create, confirm, drop ---

// Create checks the plan, files its Logbook plan and tasks, keeps the record in confirming and
// asks the confirmation (groups.md 3.1, 3.2).
func (s *groupsService) Create(ctx context.Context, p api.GroupCreateParams) (api.GroupCreated, error) {
	coord, ok := s.cfg.store.SessionByName(p.Coordinator)
	if p.Coordinator == "" || !ok {
		return api.GroupCreated{}, control.Errorf(api.ErrorKindInvalid, "a group needs its coordinator: a session on this hab (call group_create from your Flightdeck pane)")
	}
	id := groups.NewID()
	cg, _, err := s.CheckPlan(api.ConfirmGroup{Group: id, Plan: p.Plan})
	if err != nil {
		return api.GroupCreated{}, err
	}
	plan := cg.Plan
	cl := s.cfg.catalog()
	proj, _ := project(cl, plan.Project)
	lbProject, err := s.lbProject(ctx, proj)
	if err != nil {
		return api.GroupCreated{}, err
	}
	g := groups.New(id, plan, proj.Name, s.cfg.self,
		api.GroupCoordinator{Session: coord.ID, Name: coord.Name, Address: s.cfg.host + "/" + coord.Name}, s.cfg.now())
	planRec, tasks, err := s.filePlan(ctx, g, plan, lbProject)
	if err != nil {
		return api.GroupCreated{}, err
	}
	g.PlanRecord = planRec
	for i := range g.Items {
		g.Items[i].Task = tasks[i]
	}
	s.mu.Lock()
	s.groups[id] = &g
	s.order = append(s.order, id)
	s.saveLocked(&g)
	s.mu.Unlock()

	c, err := s.cfg.confirm(ctx, api.ConfirmRequestParams{Kind: "group", From: coord.Name, Group: &cg, WaitMS: p.WaitMS})
	if err != nil {
		s.mu.Lock()
		s.dropLocked(s.groups[id], "")
		s.mu.Unlock()
		return api.GroupCreated{}, err
	}
	s.mu.Lock()
	gp := s.groups[id]
	if gp.Confirmation.ID == "" {
		gp.Confirmation = api.GroupConfirmationRef{ID: c.ID, Outcome: c.State}
		s.saveLocked(gp)
	}
	out := copyGroup(*gp)
	s.mu.Unlock()
	return api.GroupCreated{Group: out, Confirmation: &c}, nil
}

// lbProject is the Logbook project of a catalog project, filed by its identity when new.
func (s *groupsService) lbProject(ctx context.Context, p api.CatalogProjectGroup) (string, error) {
	ident := lbsvc.Identity(p.Remote, p.RootCommit)
	if ident == "" {
		return "", nil
	}
	if id, ok := s.cfg.logbook.Lookup(ctx, ident); ok {
		return id, nil
	}
	return s.cfg.logbook.Sight(ctx, lbsvc.Sighting{Identity: ident, Name: p.Name, Host: s.cfg.host})
}

// habActor is the hab writing for a group (groups.md 13 Q5): its writes land under
// logbook.approval agents too. session, when set, is the member the write is for.
func (s *groupsService) habActor(session string) api.LogbookActor {
	return api.LogbookActor{Kind: "hab", Host: s.cfg.host, Device: s.cfg.self, Session: session}
}

// filePlan files one Logbook plan record with the group id as a tag and one task per item,
// linked as stages (groups.md 3.1), in one batch.
func (s *groupsService) filePlan(ctx context.Context, g api.Group, plan api.GroupPlan, project string) (string, []string, error) {
	changes := []api.LogbookChange{{Op: api.LogbookChangeOpCreate, Type: "plan", Project: project, Title: plan.Title,
		Body: plan.Brief, Tags: []string{"group-" + g.ID}}}
	writ, err := s.cfg.logbook.Write(ctx, s.habActor(""), api.LogbookWriteParams{Changes: changes, IdempotencyKey: "group-plan-" + g.ID})
	if err != nil || len(writ.IDs) == 0 {
		return "", nil, control.Errorf(api.ErrorKindUnavailable, "groups need Logbook: filing the plan failed: %v", err)
	}
	planID := writ.IDs[0]
	tasks, err := s.fileTasks(ctx, g.ID, planID, project, plan.Items, len(g.Items)-len(plan.Items))
	return planID, tasks, err
}

// fileTasks files one task per item, each a stage of the plan.
func (s *groupsService) fileTasks(ctx context.Context, group, planID, project string, items []api.GroupPlanItem, first int) ([]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	var changes []api.LogbookChange
	for i, it := range items {
		body := it.Brief
		changes = append(changes, api.LogbookChange{Op: api.LogbookChangeOpCreate, Type: "task", Project: project, Title: it.Title,
			Body: body, Tags: []string{"group-" + group}})
		_ = i
	}
	writ, err := s.cfg.logbook.Write(ctx, s.habActor(""), api.LogbookWriteParams{Changes: changes,
		IdempotencyKey: fmt.Sprintf("group-tasks-%s-%d", group, first)})
	if err != nil || len(writ.IDs) != len(items) {
		return nil, control.Errorf(api.ErrorKindUnavailable, "groups need Logbook: filing the tasks failed: %v", err)
	}
	var links []api.LogbookChange
	for i, id := range writ.IDs {
		links = append(links, api.LogbookChange{Op: api.LogbookChangeOpAdd, ID: planID, Link: &api.LogbookLink{Kind: "stage", To: id, Pos: first + i + 1}})
	}
	if _, err := s.cfg.logbook.Write(ctx, s.habActor(""), api.LogbookWriteParams{Changes: links,
		IdempotencyKey: fmt.Sprintf("group-stages-%s-%d", group, first)}); err != nil {
		s.cfg.logf("groups: %s: linking the tasks as stages: %v", group, err)
	}
	return writ.IDs, nil
}

// setStatus marks Logbook records with a status, as the hab.
func (s *groupsService) setStatus(ids []string, status string, session string) {
	if s.cfg.logbook == nil {
		return
	}
	v, _ := json.Marshal(status)
	var changes []api.LogbookChange
	for _, id := range ids {
		if id != "" {
			changes = append(changes, api.LogbookChange{Op: api.LogbookChangeOpSet, ID: id, Field: "status", Value: v})
		}
	}
	if len(changes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.cfg.logbook.Write(ctx, s.habActor(session), api.LogbookWriteParams{Changes: changes}); err != nil {
		s.cfg.logf("groups: marking %v %s: %v", ids, status, err)
	}
}

// dropLocked drops a group whose confirmation was cancelled, expired or lost: its plan record
// and tasks are marked dropped; with why, the coordinator gets one message from Flightdeck.
func (s *groupsService) dropLocked(g *api.Group, why string) {
	if g == nil || !groups.Live(*g) {
		return
	}
	g.State = groups.StateDropped
	g.Ended = stampT(s.cfg.now())
	ids := []string{g.PlanRecord}
	for _, it := range g.Items {
		ids = append(ids, it.Task)
	}
	go s.setStatus(ids, "dropped", "")
	if why != "" {
		s.tellLocked(g, fmt.Sprintf("Group %s (%s) was dropped: %s.", g.ID, g.Title, why), "Flightdeck")
	}
	s.saveLocked(g)
}

// GroupClosed drops a plan whose confirmation was cancelled or expired (groups.md 3.2). The
// confirmation's own note tells the coordinator. A cancelled amend changes nothing.
func (s *groupsService) GroupClosed(c api.Confirmation) {
	if c.Group == nil || c.Group.Amend {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.groups[c.Group.Group]; g != nil && g.State == groups.StateConfirming {
		g.Confirmation = api.GroupConfirmationRef{ID: c.ID, Outcome: c.State}
		s.dropLocked(g, "")
	}
}

// RunGroup starts a confirmed plan, or adds a confirmed amend (groups.md 3.2, 3.3). The edited
// plan replaces the record's items and members; a dropped item's task is marked dropped.
func (s *groupsService) RunGroup(ctx context.Context, id string, cg api.ConfirmGroup, confirmed bool) (api.ConfirmResult, error) {
	cid := id
	if !confirmed {
		cid = confirmationNone
	}
	s.mu.Lock()
	g := s.groups[cg.Group]
	if g == nil || (!cg.Amend && g.State != groups.StateConfirming) || (cg.Amend && !groups.Live(*g)) {
		s.mu.Unlock()
		return api.ConfirmResult{}, control.Errorf(api.ErrorKindConflict, "group %s is not waiting for this confirmation", cg.Group)
	}
	var newItems []api.GroupPlanItem
	if cg.Amend {
		for _, it := range cg.Plan.Items {
			if groups.ItemIndex(*g, it.ID) < 0 {
				newItems = append(newItems, it)
			}
		}
		g.Plan.Items = append(g.Plan.Items, newItems...)
		g.Plan.Members = append(g.Plan.Members, cg.Plan.Members...)
		groups.AddPlan(g, api.GroupPlan{Items: newItems, Members: cg.Plan.Members})
		g.Envelope = s.envelope(*g, cid)
	} else {
		var dropped []string
		keep := map[string]string{}
		for _, it := range g.Items {
			keep[it.ID] = it.Task
		}
		g.Plan = cg.Plan
		g.Items, g.Members = nil, nil
		groups.AddPlan(g, cg.Plan)
		for i := range g.Items {
			g.Items[i].Task = keep[g.Items[i].ID]
			delete(keep, g.Items[i].ID)
		}
		for _, t := range keep {
			dropped = append(dropped, t)
		}
		if len(dropped) > 0 {
			go s.setStatus(dropped, "dropped", "")
		}
		g.State = groups.StateRunning
		g.Confirmation = api.GroupConfirmationRef{ID: cid, Outcome: "done"}
		g.Envelope = s.envelope(*g, cid)
	}
	planRec, lbProj, first := g.PlanRecord, "", len(g.Items)-len(newItems)
	s.saveLocked(g)
	s.mu.Unlock()
	if len(newItems) > 0 {
		cl := s.cfg.catalog()
		if proj, ok := project(cl, g.Plan.Project); ok {
			lbProj, _ = s.lbProject(ctx, proj)
		}
		tasks, err := s.fileTasks(ctx, cg.Group, planRec, lbProj, newItems, first)
		if err == nil {
			s.mu.Lock()
			for i, t := range tasks {
				g.Items[first+i].Task = t
			}
			s.saveLocked(g)
			s.mu.Unlock()
		}
	}
	s.kick(cg.Group)
	return api.ConfirmResult{Host: s.cfg.host}, nil
}

// envelope is what the confirmed plan allows (groups.md 3.3).
func (s *groupsService) envelope(g api.Group, confirmation string) *api.GroupEnvelope {
	cl := s.cfg.catalog()
	e := &api.GroupEnvelope{Group: g.ID, Home: s.cfg.self, Confirmation: confirmation, MaxWorkers: s.intSetting(settings.GroupMaxWorkers, 2)}
	if p, ok := project(cl, g.Plan.Project); ok {
		e.Project = lbsvc.Identity(p.Remote, p.RootCommit)
	}
	for _, m := range g.Plan.Members {
		if dev, ok := s.hostDevice(cl, m.Host); ok && !slices.Contains(e.Hosts, dev) {
			e.Hosts = append(e.Hosts, dev)
		}
		if !slices.Contains(e.Harnesses, m.Harness) {
			e.Harnesses = append(e.Harnesses, m.Harness)
		}
	}
	return e
}

// Amend asks one confirmation for a change: new items and members (groups.md 3.3).
func (s *groupsService) Amend(ctx context.Context, p api.GroupAmendParams) (api.GroupCreated, error) {
	s.mu.Lock()
	g := s.groups[p.Group]
	if g == nil || !groups.Live(*g) || g.State == groups.StateConfirming {
		s.mu.Unlock()
		return api.GroupCreated{}, control.Errorf(api.ErrorKindNotFound, "no running group %s on this hab", p.Group)
	}
	coord := g.Coordinator.Name
	s.mu.Unlock()
	if len(p.Change.Items) == 0 && len(p.Change.Members) == 0 {
		return api.GroupCreated{}, control.Errorf(api.ErrorKindInvalid, "the change adds no item and no member")
	}
	cg := api.ConfirmGroup{Group: p.Group, Amend: true, Plan: api.GroupPlan{Items: p.Change.Items, Members: p.Change.Members}}
	c, err := s.cfg.confirm(ctx, api.ConfirmRequestParams{Kind: "group", From: coord, Group: &cg, WaitMS: p.WaitMS})
	if err != nil {
		return api.GroupCreated{}, err
	}
	gg, _ := s.Get(p.Group)
	return api.GroupCreated{Group: gg, Confirmation: &c}, nil
}

// --- the scheduler: lead slots and starts (groups.md 4.3) ---

// kick runs a scheduler pass for the group, or asks the running one for another.
func (s *groupsService) kick(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.running[id] {
		s.again[id] = true
		return
	}
	s.running[id] = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			s.pass(id)
			s.mu.Lock()
			if !s.again[id] || s.closed {
				delete(s.running, id)
				delete(s.again, id)
				s.mu.Unlock()
				return
			}
			s.again[id] = false
			s.mu.Unlock()
		}
	}()
}

// pass starts the leads whose items have a slot now, one at a time.
func (s *groupsService) pass(id string) {
	for {
		s.mu.Lock()
		g := s.groups[id]
		if g == nil || s.closed {
			s.mu.Unlock()
			return
		}
		next := groups.NextLeads(*g, s.intSetting(settings.GroupMaxLeads, 3))
		var item string
		for _, it := range next {
			if i := groups.MemberIndex(*g, g.Items[groups.ItemIndex(*g, it)].Lead); i >= 0 && g.Members[i].State == groups.MemberPlanned {
				item = it
				break
			}
		}
		if item == "" {
			s.mu.Unlock()
			return
		}
		ii := groups.ItemIndex(*g, item)
		mi := groups.MemberIndex(*g, g.Items[ii].Lead)
		g.Members[mi].State = groups.MemberStarting
		g.Items[ii].Reason = ""
		s.saveLocked(g)
		snap := copyGroup(*g)
		s.mu.Unlock()

		res, err := s.startMember(snap, snap.Members[mi], snap.Items[ii])

		s.mu.Lock()
		g = s.groups[id]
		if g == nil {
			s.mu.Unlock()
			return
		}
		m := &g.Members[mi]
		it := &g.Items[ii]
		switch {
		case err != nil && errors.Is(err, errSetupWaits):
			m.State = groups.MemberPlanned
			it.Reason = err.Error()
			g.Waits = appendOnce(g.Waits, fmt.Sprintf("%s: %v", it.ID, err))
			s.saveLocked(g)
			s.mu.Unlock()
			return // the yes comes from the user; a retry starts it
		case err != nil:
			m.State, m.Reason = groups.MemberFailed, errText(err)
			it.State, it.Reason = groups.ItemStalled, "its lead failed to start: "+errText(err)
			s.tellLocked(g, fmt.Sprintf("Lead %s of item %s failed to start: %s. The item is stalled; group_retry starts it again.", m.Name, it.ID, errText(err)), "Flightdeck")
		default:
			m.State, m.Session, m.Device, m.Folder, m.Host = groups.MemberRunning, res.Session, res.device, res.folder, res.host
			s.panes[g.ID+"/"+m.Name] = res.Pane
			it.State = groups.ItemWorking
			go s.claim(it.Task, res.Session)
		}
		s.saveLocked(g)
		s.mu.Unlock()
	}
}

func appendOnce(list []string, line string) []string {
	if slices.Contains(list, line) {
		return list
	}
	return append(list, line)
}

// claim writes an item's claim as the hab: assignee the lead's session, status doing (groups.md
// 4.3 step 3, 13 Q5).
func (s *groupsService) claim(task, session string) {
	if task == "" || s.cfg.logbook == nil {
		return
	}
	who, _ := json.Marshal(session)
	doing, _ := json.Marshal("doing")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.cfg.logbook.Write(ctx, s.habActor(session), api.LogbookWriteParams{Changes: []api.LogbookChange{
		{Op: api.LogbookChangeOpSet, ID: task, Field: "assignee", Value: who},
		{Op: api.LogbookChangeOpSet, ID: task, Field: "status", Value: doing},
	}, IdempotencyKey: "group-claim-" + task + "-" + session}); err != nil {
		s.cfg.logf("groups: claim of %s: %v", task, err)
	}
}

var errSetupWaits = errors.New("its worktree's setup waits for your yes")

type started struct {
	api.SessionCreated
	host, device, folder string
}

// startMember makes the member's worktree on its host and starts it there with the envelope,
// its group fields and the trust cover (groups.md 4.2, 4.3).
func (s *groupsService) startMember(g api.Group, m api.GroupMember, it api.GroupItem) (started, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cl := s.cfg.catalog()
	proj, ok := project(cl, g.Plan.Project)
	if !ok {
		return started{}, fmt.Errorf("project %s is no longer in the catalog", g.Plan.Project)
	}
	dev, ok := s.hostDevice(cl, m.Host)
	if !ok {
		return started{}, fmt.Errorf("no peer labelled %q", m.Host)
	}
	src := folderOn(proj, dev)
	if src == "" {
		return started{}, fmt.Errorf("the project is not set up on %s", m.Host)
	}
	self := dev == s.cfg.self
	var caller api.Caller
	host := s.cfg.host
	if !self {
		l, ok := s.link(m.Host)
		if !ok || !l.Online || l.Caller == nil {
			return started{}, fmt.Errorf("%s is offline", m.Host)
		}
		caller, host = l.Caller, m.Host
	}
	wp := api.WorktreeCreateParams{WorktreeProject: api.WorktreeProject{ProjectDir: src}, Branch: it.Branch, Base: g.Plan.Lane.Target}
	var wt api.WorktreeCreated
	var err error
	if self {
		wt, err = s.cfg.worktree(ctx, wp)
	} else {
		err = caller.Call(ctx, "worktree.create", wp, &wt)
	}
	if err != nil && strings.Contains(err.Error(), "exists") {
		// A start tried again after a restart: the branch's worktree is there already.
		wt, err = s.findWorktree(ctx, caller, src, it.Branch)
	}
	if err != nil {
		return started{}, fmt.Errorf("making the worktree on %s: %s", host, errText(err))
	}
	if wt.Setup.State == "pending" {
		return started{}, fmt.Errorf("%w on %s: %s", errSetupWaits, host, wt.Setup.Reason)
	}
	env := g.Envelope
	if env == nil {
		env = &api.GroupEnvelope{Group: g.ID, Home: s.cfg.self, Confirmation: g.Confirmation.ID}
	}
	key := sha256.Sum256([]byte(g.ID + "\x00" + m.Name + "\x00" + strconv.Itoa(m.Attempts)))
	p := api.SessionCreateParams{Name: m.Name, ProjectDir: wt.Worktree.Path, Harness: m.Harness, Model: m.Model,
		Seed: leadSeed(g, m, it, wt.Worktree.Path), IdempotencyKey: "group-" + hex.EncodeToString(key[:16]),
		Group:      &api.SessionGroup{ID: g.ID, Role: m.Role, Item: m.Item},
		Envelope:   env,
		TrustCover: &api.TrustCover{Group: g.ID, Confirmation: env.Confirmation, Harness: m.Harness, BranchPrefix: confirm.GroupBranchPrefix(g.ID)}}
	if sf := s.cfg.host + "/" + g.Coordinator.Name; validSeedFrom(sf) {
		p.SeedFrom = sf // the seed names the coordinator, so the lead knows its address
	}
	var made api.SessionCreated
	if self {
		made, err = s.cfg.create(withOwnCover(ctx), p)
	} else {
		err = caller.Call(ctx, "session.create", p, &made)
	}
	if err != nil {
		return started{}, fmt.Errorf("%s refused the start: %s", host, errText(err))
	}
	return started{SessionCreated: made, host: host, device: dev, folder: wt.Worktree.Path}, nil
}

// findWorktree finds the worktree of a branch, on this hab or over the link.
func (s *groupsService) findWorktree(ctx context.Context, caller api.Caller, src, branch string) (api.WorktreeCreated, error) {
	var list api.WorktreeList
	p := api.WorktreeListParams{WorktreeProject: api.WorktreeProject{ProjectDir: src}}
	var err error
	if caller == nil {
		err = errors.New("no worktree list on this hab")
		if s.cfg.listWorktrees != nil {
			list, err = s.cfg.listWorktrees(ctx, p)
		}
	} else {
		err = caller.Call(ctx, "worktree.list", p, &list)
	}
	if err != nil {
		return api.WorktreeCreated{}, err
	}
	for _, w := range list.Worktrees {
		if w.Branch == branch {
			return api.WorktreeCreated{Worktree: w, Setup: api.WorktreeSetup{State: "none"}}, nil
		}
	}
	return api.WorktreeCreated{}, fmt.Errorf("no worktree on branch %s", branch)
}

// leadSeed is a lead's first message: the group's brief, its item and how to report.
func leadSeed(g api.Group, m api.GroupMember, it api.GroupItem, folder string) string {
	var brief string
	for _, pi := range g.Plan.Items {
		if pi.ID == it.ID {
			brief = pi.Brief
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are the %s of item %s (%s) in agent group %s: %s.\n", m.Role, it.ID, it.Title, g.ID, g.Title)
	fmt.Fprintf(&b, "Work in %s on branch %s. The coordinator is %s.\n", folder, it.Branch, g.Coordinator.Address)
	fmt.Fprintf(&b, "Report with group_report (group %s); write progress and hand-off notes with group_note (item %s). Your item's Logbook task is %s.\n\n", g.ID, it.ID, it.Task)
	if g.Plan.Brief != "" {
		b.WriteString("The task:\n" + g.Plan.Brief + "\n\n")
	}
	if brief != "" {
		b.WriteString("Your item:\n" + brief + "\n")
	}
	out := b.String()
	if utf8.RuneCountInString(out) > confirm.MaxSeed {
		r := []rune(out)
		out = string(r[:confirm.MaxSeed])
	}
	return out
}

// --- reads ---

// Get returns a group's record with its members' states from the catalog copies.
func (s *groupsService) Get(id string) (api.Group, error) {
	s.refresh(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[id]
	if g == nil {
		return api.Group{}, control.Errorf(api.ErrorKindNotFound, "no group %s on this hab", id)
	}
	return copyGroup(*g), nil
}

// List returns the short forms, newest first.
func (s *groupsService) List(state api.GroupState) []api.CatalogGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.CatalogGroup{}
	for i := len(s.order) - 1; i >= 0; i-- {
		g := s.groups[s.order[i]]
		if g != nil && (state == "" || g.State == state) {
			out = append(out, groups.Short(*g))
		}
	}
	return out
}

// Snapshot returns the records for the host snapshot, oldest first.
func (s *groupsService) Snapshot() []api.Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []api.Group
	for _, id := range s.order {
		if g := s.groups[id]; g != nil {
			out = append(out, copyGroup(*g))
		}
	}
	return out
}

// Section is the catalog's groups section: the short form of each group (groups.md 5.1).
func (s *groupsService) Section() []api.CatalogGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []api.CatalogGroup
	for _, id := range s.order {
		if g := s.groups[id]; g != nil {
			out = append(out, groups.Short(*g))
		}
	}
	return out
}

// watch refreshes every live group's members from the catalog copies, as often as they come
// (mesh.md 5: every 15 s), and prunes ended records past keep_ended.
func (s *groupsService) watch() {
	defer s.wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		ids := slices.Clone(s.order)
		s.mu.Unlock()
		for _, id := range ids {
			s.refresh(id)
		}
	}
}

// refresh reads the members' agent states from the catalog copies, adds members a member's hab
// started inside the envelope (workers), and stalls an item whose lead's session on this hab
// was closed (groups.md 14). It saves only a change of the record's shape.
func (s *groupsService) refresh(id string) {
	cl := s.cfg.catalog()
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[id]
	if g == nil || !groups.Live(*g) {
		return
	}
	changed := false
	for _, h := range cl.Hosts {
		dev := h.Catalog.DeviceID
		label := h.Label
		if h.Own {
			label = s.cfg.host
		}
		for _, a := range h.Catalog.Agents {
			if a.Group == nil || a.Group.ID != g.ID {
				continue
			}
			i := slices.IndexFunc(g.Members, func(m api.GroupMember) bool { return m.Name == a.Session && (m.Device == "" || m.Device == dev) })
			if i < 0 {
				g.Members = append(g.Members, api.GroupMember{Session: a.ID, Name: a.Session, Host: label, Device: dev, Harness: a.Harness,
					Role: a.Group.Role, Item: a.Group.Item, State: groups.MemberRunning})
				i, changed = len(g.Members)-1, true
			}
			m := &g.Members[i]
			m.Agent, m.ContextPct, m.AgentAge = a.State, a.ContextPct, h.SeenAt
		}
	}
	for i := range g.Members {
		m := &g.Members[i]
		if m.Device != s.cfg.self || m.Session == "" || m.State != groups.MemberRunning {
			continue
		}
		ss, ok := s.cfg.store.Session(m.Session)
		switch {
		case !ok:
			m.State, changed = groups.MemberExited, true
			if m.Role == groups.RoleLead {
				if ii := groups.ItemIndex(*g, m.Item); ii >= 0 && g.Items[ii].State != groups.ItemLanded && g.Items[ii].State != groups.ItemDropped {
					g.Items[ii].State, g.Items[ii].Reason = groups.ItemStalled, "its lead's session was closed"
					s.tellLocked(g, fmt.Sprintf("Lead %s of item %s was closed. The item is stalled; group_retry starts a new lead.", m.Name, m.Item), "Flightdeck")
				}
			}
		case ss.State == "exited":
			m.Agent = "exited"
		}
	}
	if changed {
		s.saveLocked(g)
		go s.kick(g.ID)
	}
}

// --- reports, notes, pause, close, retry ---

// tellLocked sends the coordinator a report: a message from Flightdeck while it runs, else it
// waits in the inbox (groups.md 5.5). The caller holds s.mu and saves the record.
func (s *groupsService) tellLocked(g *api.Group, text, from string) string {
	ss, ok := s.cfg.store.Session(g.Coordinator.Session)
	if ok && ss.State == "running" && s.cfg.note != nil {
		msg := fmt.Sprintf("Group %s (%s), from %s: %s", g.ID, g.Title, from, text)
		if err := s.cfg.note(ss.ID, msg); err == nil {
			return "sent"
		} else {
			s.cfg.logf("groups: %s: report to %s: %v; it waits in the inbox", g.ID, ss.Name, err)
		}
	}
	groups.AddReport(g, api.GroupReport{At: stampT(s.cfg.now()), From: from, Text: text}, s.cfg.keys.InboxMax, s.cfg.keys.InboxDays, s.cfg.now())
	return "waiting"
}

// Report takes a report for the coordinator, on the home hab or forwarded to it.
func (s *groupsService) Report(ctx context.Context, p api.GroupReportParams) (api.GroupReported, error) {
	if strings.TrimSpace(p.Text) == "" || utf8.RuneCountInString(p.Text) > groups.MaxText {
		return api.GroupReported{}, control.Errorf(api.ErrorKindInvalid, "a report is 1 to %d characters", groups.MaxText)
	}
	s.mu.Lock()
	g := s.groups[p.Group]
	if g == nil {
		s.mu.Unlock()
		var out api.GroupReported
		err := s.forward(ctx, p.Group, "group.report", p, &out)
		return out, err
	}
	if !groups.Live(*g) {
		s.mu.Unlock()
		return api.GroupReported{}, control.Errorf(api.ErrorKindConflict, "group %s is %s", g.ID, g.State)
	}
	from := p.From
	if from == "" {
		from = "the user"
	}
	state := s.tellLocked(g, p.Text, from)
	s.saveLocked(g)
	s.mu.Unlock()
	return api.GroupReported{State: state}, nil
}

// forward sends a call about a group this hab is not the home of to its home, found in the
// catalog's groups sections, over the link to it.
func (s *groupsService) forward(ctx context.Context, group, method string, p, out any) error {
	if _, peer := control.CallerPeer(ctx); peer {
		return control.Errorf(api.ErrorKindNotFound, "no group %s on this hab", group) // never forwarded twice
	}
	cl := s.cfg.catalog()
	cg, ok := groupOf(cl, group)
	if !ok || cg.Home == "" || cg.Home == s.cfg.self {
		return control.Errorf(api.ErrorKindNotFound, "no group %s on this hab or in the catalog", group)
	}
	label := ""
	for _, h := range cl.Hosts {
		if h.Catalog.DeviceID == cg.Home {
			label = h.Label
		}
	}
	l, ok := s.link(label)
	if label == "" || !ok || !l.Online || l.Caller == nil {
		return control.Errorf(api.ErrorKindUnavailable, "group %s's home hab is offline; try again once it is back", group)
	}
	return l.Caller.Call(ctx, method, p, out)
}

// Note writes a note on an item's Logbook task, as the hab (groups.md 4.3, 13 Q5).
func (s *groupsService) Note(ctx context.Context, p api.GroupNoteParams) error {
	if strings.TrimSpace(p.Text) == "" || utf8.RuneCountInString(p.Text) > groups.MaxText {
		return control.Errorf(api.ErrorKindInvalid, "a note is 1 to %d characters", groups.MaxText)
	}
	s.mu.Lock()
	g := s.groups[p.Group]
	if g == nil {
		s.mu.Unlock()
		var out api.Empty
		return s.forward(ctx, p.Group, "group.note", p, &out)
	}
	i := groups.ItemIndex(*g, p.Item)
	if i < 0 || g.Items[i].Task == "" {
		s.mu.Unlock()
		return control.Errorf(api.ErrorKindNotFound, "group %s has no item %q with a task", g.ID, p.Item)
	}
	task := g.Items[i].Task
	session := ""
	if mi := groups.MemberIndex(*g, p.From); mi >= 0 {
		session = g.Members[mi].Session
	}
	s.mu.Unlock()
	text := p.Text
	if p.From != "" {
		text = p.From + ": " + text
	}
	_, err := s.cfg.logbook.Write(ctx, s.habActor(session), api.LogbookWriteParams{Changes: []api.LogbookChange{
		{Op: api.LogbookChangeOpNote, ID: task, Text: text}}})
	return err
}

func (s *groupsService) change(id string, f func(g *api.Group) error) (api.Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[id]
	if g == nil {
		return api.Group{}, control.Errorf(api.ErrorKindNotFound, "no group %s on this hab", id)
	}
	if err := f(g); err != nil {
		return api.Group{}, err
	}
	s.saveLocked(g)
	return copyGroup(*g), nil
}

// Pause is the user's pause (groups.md 5.6).
func (s *groupsService) Pause(id string) (api.Group, error) {
	return s.change(id, func(g *api.Group) error {
		if g.State != groups.StateRunning && g.State != groups.StatePaused {
			return control.Errorf(api.ErrorKindConflict, "group %s is %s", g.ID, g.State)
		}
		groups.SetPause(g, "user", true, "", s.cfg.now())
		return nil
	})
}

// Resume lifts the user's pause; a usage pause stays.
func (s *groupsService) Resume(id string) (api.Group, error) {
	g, err := s.change(id, func(g *api.Group) error {
		if g.State != groups.StatePaused {
			return control.Errorf(api.ErrorKindConflict, "group %s is %s, not paused", g.ID, g.State)
		}
		groups.SetPause(g, "user", false, "", s.cfg.now())
		return nil
	})
	if err == nil {
		s.kick(id)
	}
	return g, err
}

// UsagePause sets or lifts the usage pause: the usage governor's answer (groups.md 10). The
// coordinator hears once per pause and once at its resume.
func (s *groupsService) UsagePause(id string, on bool, until string) (api.Group, error) {
	g, err := s.change(id, func(g *api.Group) error {
		was := g.Pause.Usage != ""
		groups.SetPause(g, "usage", on, until, s.cfg.now())
		switch {
		case on && !was:
			s.tellLocked(g, "Paused for usage until "+until+". Working members finish their turn; nothing new starts.", "Flightdeck")
		case !on && was:
			s.tellLocked(g, "Usage has room again; the usage pause is lifted.", "Flightdeck")
		}
		return nil
	})
	if err == nil && !on {
		s.kick(id)
	}
	return g, err
}

// Close ends the group: no new starts; with group.close_members on, its idle members on this
// hab are closed (groups.md 5.2, 7.5). Members on other habs keep running until B4's end.
func (s *groupsService) Close(ctx context.Context, id string) (api.Group, error) {
	closeIdle := s.boolSetting(settings.GroupCloseMembers, true)
	var panes []string
	g, err := s.change(id, func(g *api.Group) error {
		if !groups.Live(*g) || g.State == groups.StateConfirming {
			return control.Errorf(api.ErrorKindConflict, "group %s is %s", g.ID, g.State)
		}
		g.State = groups.StateEnded
		g.Ended = stampT(s.cfg.now())
		for _, m := range g.Members {
			if m.Device == s.cfg.self && m.State == groups.MemberRunning && (m.Agent == "idle" || m.Agent == "") && closeIdle {
				if p := s.panes[g.ID+"/"+m.Name]; p != "" {
					panes = append(panes, p)
				} else if ss, ok := s.cfg.store.Session(m.Session); ok && ss.Pane != "" {
					panes = append(panes, ss.Pane)
				}
			}
			if m.State == groups.MemberRunning {
				g.Leftovers = appendOnce(g.Leftovers, fmt.Sprintf("member %s on %s, folder %s", m.Name, m.Host, m.Folder))
			}
		}
		return nil
	})
	if err != nil {
		return g, err
	}
	for _, p := range panes {
		if s.cfg.closePane != nil {
			if err := s.cfg.closePane(ctx, p); err != nil {
				s.cfg.logf("groups: %s: closing pane %s: %v", id, p, err)
			}
		}
	}
	return s.Get(id)
}

// Retry starts a failed member again, or a new start for an item whose lead was closed: inside
// the envelope, so it asks nothing (groups.md 3.3, 4.3).
func (s *groupsService) Retry(ctx context.Context, p api.GroupRetryParams) (api.Group, error) {
	g, err := s.change(p.Group, func(g *api.Group) error {
		if g.State != groups.StateRunning && g.State != groups.StatePaused {
			return control.Errorf(api.ErrorKindConflict, "group %s is %s", g.ID, g.State)
		}
		i := groups.MemberIndex(*g, p.Member)
		if i < 0 {
			return control.Errorf(api.ErrorKindNotFound, "group %s has no member %s", g.ID, p.Member)
		}
		m := &g.Members[i]
		ii := groups.ItemIndex(*g, m.Item)
		waits := m.State == groups.MemberPlanned && ii >= 0 && g.Items[ii].Reason != ""
		if m.State != groups.MemberFailed && m.State != groups.MemberExited && !waits {
			return control.Errorf(api.ErrorKindConflict, "member %s is %s; only a failed or closed member, or one whose setup waited, starts again", m.Name, m.State)
		}
		if !waits {
			m.Attempts++ // a new idempotency key: this is a new start
		}
		m.State, m.Session, m.Reason = groups.MemberPlanned, "", ""
		g.Waits = slices.DeleteFunc(g.Waits, func(w string) bool { return strings.HasPrefix(w, m.Item+":") })
		if ii >= 0 && (g.Items[ii].State == groups.ItemStalled || waits) {
			g.Items[ii].State, g.Items[ii].Reason = groups.ItemQueued, ""
		}
		return nil
	})
	if err == nil {
		s.kick(p.Group)
	}
	return g, err
}

// Resumed delivers the inbox to a coordinator that was resumed, as one message, oldest first
// (groups.md 5.5).
func (s *groupsService) Resumed(session string) {
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if closed {
		return
	}
	go func() {
		defer s.wg.Done()
		// The resumed session's record names its new pane a moment after the call: the
		// message goes once it runs, so channels do not refuse it as exited.
		for i := 0; i < 600; i++ {
			if ss, ok := s.cfg.store.Session(session); ok && ss.State == "running" && ss.Pane != "" && (s.cfg.ready == nil || s.cfg.ready(ss.Pane)) {
				s.deliverInbox(session)
				return
			}
			select {
			case <-s.stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		s.cfg.logf("groups: session %s never ran after its resume; its inbox waits", session)
	}()
}

func (s *groupsService) deliverInbox(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		g := s.groups[id]
		if g == nil || g.Coordinator.Session != session || len(g.Inbox) == 0 || s.cfg.note == nil {
			continue
		}
		if err := s.cfg.note(session, groups.InboxText(*g)); err != nil {
			s.cfg.logf("groups: %s: the inbox for %s: %v", g.ID, g.Coordinator.Name, err)
			continue
		}
		g.Inbox = []api.GroupReport{}
		s.saveLocked(g)
	}
}
