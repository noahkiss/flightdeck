package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/panetext"
)

// Driving panes (hab-logic-api.md 3.12): pane.list lists the panes a caller may read,
// pane.type types into a pane under the send gate, pane.new starts a pane. agent.read_panes
// filters the list; agent.drive_panes decides what an agent may type into and start.

// paneDriveCapability is the capability of pane.list, pane.type and pane.new.
const paneDriveCapability = "pane-drive"

// The bounds of 3.12.
const (
	paneTypeMax      = 64 << 10         // the most text one pane.type writes
	paneTypeTime     = 10 * time.Second // the longest a pane.type waits for its writes
	paneTypeMargin   = 5 * time.Second  // and a forwarded one, the far hab's wait plus this
	paneListRows     = 1000             // the most rows from one host
	paneListPeerTime = 5 * time.Second  // the longest a peer is asked for its rows
	paneDrivePer     = 4                // pane.type or pane.list calls at once per caller
	paneDriveAll     = 32               // and on the hab
)

// paneTypeQuiet is how long a client's input to a pane holds a type back: a person comes first.
// A test build may shorten it.
var paneTypeQuiet = 2 * time.Second

// paneGate is one of the two settings that bind the agent doors.
type paneGate int

const (
	gateRead  paneGate = iota // agent.read_panes (3.11): pane.read, pane.wait, pane.list
	gateDrive                 // agent.drive_panes (3.12): pane.type, pane.new
)

func (g paneGate) setting() string {
	if g == gateDrive {
		return "agent.drive_panes"
	}
	return "agent.read_panes"
}

// capability is what a peer needs for a forward under the gate.
func (g paneGate) capability() string {
	if g == gateDrive {
		return paneDriveCapability
	}
	return paneReadCapability
}

func (g paneGate) may(rule string, c paneCaller, callerProject string, t paneTarget) bool {
	if g == gateDrive {
		return mayDrive(rule, c, callerProject, t)
	}
	return mayRead(rule, c, callerProject, t)
}

// allowed says what the rule lets this caller do, for a refusal's hint. It names no target.
func (g paneGate) allowed(rule string, c paneCaller) string {
	if g == gateDrive {
		switch {
		case c.door == doorPeer:
			return "type into or start no pane from another hab"
		case rule == readPanesProject:
			return "type into and start its own project's panes only, never its own pane"
		case rule == drivePanesAgents:
			return "type only into the panes of agents in its own project, never its own pane, and start no pane"
		case rule == readPanesAll:
			return "type into any pane but its own"
		}
		return "type into or start no pane"
	}
	switch {
	case c.door == doorPeer:
		return "read no pane for an agent on another hab"
	case rule == readPanesProject:
		return "read its own pane and its own project's panes only"
	}
	return "read its own pane only"
}

func (s *paneService) ruleOf(g paneGate) string {
	if g == gateDrive {
		if s.drive == nil {
			return readPanesOff
		}
		return s.drive()
	}
	return s.rule()
}

// drivePanesAgents is the agent.drive_panes value between off and project (Balanced): an agent
// types only into other agents' panes in its own project, and starts no pane.
const drivePanesAgents = "agents"

// mayDrive is agent.drive_panes for pane.type (hab-logic-api.md 3.12). The owner drives every
// pane. The Capcom route drives none. The pane route never types into its own pane: a key there
// could change its own mode. Beyond that it drives every pane under all; the panes of its own
// session's project under project; only those of them that run an agent under agents; none
// under off. A peer link drives every pane under all, else none.
func mayDrive(rule string, c paneCaller, callerProject string, t paneTarget) bool {
	switch {
	case c.door == doorOwner:
		return true
	case c.isCapcom():
		return false
	case c.door == doorRoute && samePane(c.pane, t.pane):
		return false
	case rule == readPanesAll:
		return true
	case c.door == doorPeer:
		return false
	}
	inProject := callerProject != "" && t.filed && t.session.Project == callerProject
	switch rule {
	case readPanesProject:
		return inProject
	case drivePanesAgents:
		return inProject && t.agent
	}
	return false
}

// paneDrive is what pane.type and pane.new need beyond reading.
type paneDrive struct {
	// agent returns a pane's agent state, for pane.list.
	agent func(pane string) (api.AgentState, bool)
	// typer writes to a pane under the send gate.
	typer paneTyper
	// typed reports whether a client's input reached the pane within d.
	typed func(pane string, d time.Duration) bool
	// create starts a session as session.create does.
	create func(ctx context.Context, p api.SessionCreateParams) (api.SessionCreated, error)
	// typeSlots and listSlots bound the calls that can block.
	typeSlots, listSlots waitSlots
}

// paneTyper writes to a pane under the send gate (harness-adapters.md, Input delivery) and
// returns the bytes written.
type paneTyper interface {
	typeInto(ctx context.Context, pane string, w typeWrite) (int, error)
}

// typeWrite is one pane.type for the typer.
type typeWrite struct {
	text      string
	enter     bool
	agentDoor bool                   // the call came through an agent's door
	recent    func(pane string) bool // a person typed into the pane just now
}

// agentText says whether text may come through an agent's door: no ESC, and no C0 control but
// tab and LF, so the text can press no key but Enter by LF (hab-logic-api.md 3.12).
func agentText(text string) bool {
	for _, r := range text {
		if (r < 0x20 && r != '\t' && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}
	return true
}

// callerName names the caller in a log line: a pane id, "peer link" or "owner".
func (c paneCaller) name() string {
	switch c.door {
	case doorPeer:
		return "peer link"
	case doorOwner:
		return "owner"
	}
	return c.pane
}

// --- pane.list ---

// List serves pane.list: the panes the caller may read under agent.read_panes, never a title.
func (s *paneService) List(ctx context.Context, c paneCaller, p api.PaneListParams) (api.PaneList, error) {
	rule := s.rule()
	if c.isCapcom() || (c.door == doorPeer && rule != readPanesAll) {
		return api.PaneList{}, s.deny("pane.list", c, rule, "")
	}
	release, ok := s.drv.listSlots.takeN(c.key(), paneDrivePer, paneDriveAll)
	if !ok {
		return api.PaneList{}, control.Errorf(api.ErrorKindUnavailable, "too many pane lists at once (at most %d per caller, %d on this hab); try again", paneDrivePer, paneDriveAll)
	}
	defer release()
	lim := limited(rule, c)
	out := api.PaneList{Panes: []api.PaneListEntry{}}
	var ask []channels.Link
	here := true
	if p.Host != "" && p.Host != s.host {
		l, self, err := s.peerByLabel(p.Host)
		switch {
		case lim:
			return api.PaneList{}, s.deny("pane.list", c, rule, "")
		case c.door == doorPeer && !self:
			return api.PaneList{}, control.Errorf(api.ErrorKindInvalid, "a call over a peer link reaches only this hab's own panes: a forward never forwards")
		case err != nil:
			return api.PaneList{}, err
		case !self:
			here, ask = false, []channels.Link{l}
		}
	} else if p.Host == "" && !lim && c.door != doorPeer {
		for _, l := range s.links() {
			if l.DeviceID != "" && l.DeviceID == s.self {
				continue
			}
			ask = append(ask, l)
		}
	}
	if here {
		out.Panes = append(out.Panes, s.localRows(rule, c)...)
	}
	rows, unreached := s.askPeers(ctx, ask)
	out.Panes = append(out.Panes, rows...)
	out.Unreached = unreached
	return out, nil
}

// peerByLabel finds a peer link by label; self is true when the label names this hab.
func (s *paneService) peerByLabel(label string) (channels.Link, bool, error) {
	for _, l := range s.links() {
		if l.Label != label {
			continue
		}
		if l.DeviceID != "" && l.DeviceID == s.self {
			return channels.Link{}, true, nil
		}
		return l, false, nil
	}
	return channels.Link{}, false, control.Errorf(api.ErrorKindNotFound, "no peer labelled %q in the peer list", label)
}

// localRows lists this hab's live panes the caller may read, in session-name order, unfiled
// panes last, at most paneListRows.
func (s *paneService) localRows(rule string, c paneCaller) []api.PaneListEntry {
	project := s.callerProject(c)
	var rows []api.PaneListEntry
	for _, st := range s.lk.Panes() {
		if st.Gone || st.Exited || st.Lost {
			continue
		}
		id := st.UUID.String()
		t := paneTarget{pane: id}
		t.session, t.filed = s.store.SessionByPane(id)
		if !mayRead(rule, c, project, t) {
			continue
		}
		r := api.PaneListEntry{Pane: id, Host: s.host, Own: c.door == doorRoute && samePane(c.pane, id)}
		if t.filed {
			r.Session, r.SessionID, r.Project, r.State = t.session.Name, t.session.ID, t.session.Project, t.session.State
			if pr, ok := s.store.Project(t.session.Project); ok && t.session.Project != "" {
				r.ProjectName = pr.Name
			}
		}
		if s.drv.agent != nil {
			if a, ok := s.drv.agent(id); ok && a.Revision > 0 && a.State != api.AgentStatusExited && a.State != api.AgentStatusNone {
				r.Agent, r.AgentState = a.Harness, a.State
			}
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.Session == "") != (b.Session == "") {
			return a.Session != ""
		}
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		return a.Pane < b.Pane
	})
	if len(rows) > paneListRows {
		rows = rows[:paneListRows]
	}
	return rows
}

// askPeers asks each peer for its own rows at once, each for at most paneListPeerTime. A peer
// that is offline, lacks pane-drive or fails is listed as unreached. Its rows are untrusted.
func (s *paneService) askPeers(ctx context.Context, links []channels.Link) ([]api.PaneListEntry, []api.PaneUnreached) {
	type answer struct {
		rows []api.PaneListEntry
		miss *api.PaneUnreached
	}
	answers := make([]answer, len(links))
	var wg sync.WaitGroup
	for i, l := range links {
		switch {
		case !l.Online || l.Caller == nil:
			answers[i].miss = &api.PaneUnreached{Label: l.Label, Reason: "offline"}
			continue
		case !slices.Contains(l.Caps, paneDriveCapability):
			answers[i].miss = &api.PaneUnreached{Label: l.Label, Reason: "runs without pane-drive"}
			continue
		}
		wg.Add(1)
		go func(i int, l channels.Link) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, paneListPeerTime)
			defer cancel()
			var r api.PaneList
			if err := l.Caller.Call(ctx, "pane.list", api.PaneListParams{}, &r); err != nil {
				reason := "no answer in time"
				var ce *control.Error
				if errors.As(err, &ce) {
					reason = panetext.CleanControls(ce.Message)
				}
				answers[i].miss = &api.PaneUnreached{Label: l.Label, Reason: reason}
				return
			}
			answers[i].rows = cleanPeerRows(l.Label, r.Panes)
		}(i, l)
	}
	wg.Wait()
	var rows []api.PaneListEntry
	var miss []api.PaneUnreached
	for _, a := range answers {
		rows = append(rows, a.rows...)
		if a.miss != nil {
			miss = append(miss, *a.miss)
		}
	}
	return rows, miss
}

// cleanPeerRows treats a peer's rows as untrusted: control characters out of every string, at
// most paneListRows rows, the peer's label as host, and no row marked as the caller's own.
func cleanPeerRows(label string, in []api.PaneListEntry) []api.PaneListEntry {
	if len(in) > paneListRows {
		in = in[:paneListRows]
	}
	out := make([]api.PaneListEntry, 0, len(in))
	for _, r := range in {
		c := panetext.CleanControls
		out = append(out, api.PaneListEntry{Pane: c(r.Pane), Session: c(r.Session), SessionID: c(r.SessionID),
			Project: c(r.Project), ProjectName: c(r.ProjectName), Host: label, State: api.SessionState(c(string(r.State))),
			Agent: c(r.Agent), AgentState: api.AgentStatus(c(string(r.AgentState)))})
	}
	return out
}

// --- pane.type ---

// Type serves pane.type: the text, then Enter when asked, under agent.drive_panes for an agent
// and under the send gate for every caller. A gate that holds refuses with conflict; nothing is
// queued (hab-logic-api.md 3.12).
func (s *paneService) Type(ctx context.Context, c paneCaller, p api.PaneTypeParams) (api.PaneTyped, error) {
	switch {
	case len(p.Text) > paneTypeMax:
		return api.PaneTyped{}, control.Errorf(api.ErrorKindInvalid, "text is %d bytes: at most %d", len(p.Text), paneTypeMax)
	case p.Text == "" && !p.Enter:
		return api.PaneTyped{}, control.Errorf(api.ErrorKindInvalid, "pane.type needs text, or enter")
	case c.door != doorOwner && !agentText(p.Text):
		return api.PaneTyped{}, control.Errorf(api.ErrorKindInvalid, "an agent's text may hold no control characters but tab and newline: no Esc, no CR; use enter for Enter")
	}
	t, peer, bare, err := s.prepareBy(gateDrive, "pane.type", c, p.Pane, p.Session)
	if err != nil {
		return api.PaneTyped{}, err
	}
	release, ok := s.drv.typeSlots.takeN(c.key(), paneDrivePer, paneDriveAll)
	if !ok {
		return api.PaneTyped{}, control.Errorf(api.ErrorKindUnavailable, "too many pane types at once (at most %d per caller, %d on this hab); try again", paneDrivePer, paneDriveAll)
	}
	defer release()
	if peer != nil {
		fwd := p
		fwd.Session = bare
		var r api.PaneTyped
		fctx, cancel := context.WithTimeout(ctx, paneTypeTime+paneTypeMargin)
		defer cancel()
		if err := peer.Caller.Call(fctx, "pane.type", fwd, &r); err != nil {
			return api.PaneTyped{}, peerErr(peer.Label, err)
		}
		r.Pane, r.Session = panetext.CleanControls(r.Pane), peer.Label+"/"+bare
		return r, nil
	}
	if s.drv.typer == nil {
		return api.PaneTyped{}, control.Errorf(api.ErrorKindUnavailable, "pane.type needs gff-hab serve with its holder link")
	}
	recent := func(pane string) bool { return s.drv.typed != nil && s.drv.typed(pane, paneTypeQuiet) }
	ctx, cancel := context.WithTimeout(ctx, paneTypeTime)
	defer cancel()
	n, err := s.drv.typer.typeInto(ctx, t.pane, typeWrite{text: p.Text, enter: p.Enter, agentDoor: c.door != doorOwner, recent: recent})
	if err != nil {
		return api.PaneTyped{}, err
	}
	s.logf("panes: pane.type: caller %s typed %d bytes into pane %s (%s %s)", c.name(), n, t.pane, gateDrive.setting(), s.ruleOf(gateDrive))
	r := api.PaneTyped{Pane: t.pane, Bytes: n}
	if t.filed {
		r.Session = t.session.Name
	}
	return r, nil
}

// typeInto writes to the pane under the send gate (harness-adapters.md, Input delivery;
// hab-logic-api.md 3.12): one send or type at a time per pane, no open permission request and no
// blocked agent, from an agent's door no agent that reports through no hook, and no client input
// within the quiet time. Each check runs under the pane's claim, before any byte goes in. Text
// into an agent's pane goes as one bracketed paste, so it presses no key. With Enter, or a
// newline in the text, into an agent's pane, the claim holds until the turn starts, as a send's
// does. A type is not the owner's key: it never marks an interrupt. It returns the bytes written.
func (a *agentService) typeInto(ctx context.Context, pane string, w typeWrite) (int, error) {
	if !a.claim(pane) {
		return 0, control.Errorf(api.ErrorKindConflict, "a message or another type to pane %s is in flight; try again in a few seconds", pane)
	}
	held := false
	defer func() {
		if !held {
			a.unclaim(pane)
		}
	}()
	if err := a.agents.CheckType(pane, w.agentDoor); err != nil {
		return 0, agentErr(err)
	}
	if w.recent != nil && w.recent(pane) {
		return 0, control.Errorf(api.ErrorKindConflict, "a client typed into pane %s in the last %v: a person comes first; try again when typing pauses", pane, paneTypeQuiet)
	}
	st, ok := a.agents.Get(pane)
	agent := ok && st.Revision > 0 && st.State != api.AgentStatusExited && st.State != api.AgentStatusNone
	var writes [][]byte
	switch {
	case w.text == "":
	case agent:
		writes = append(writes, harness.PasteWrite(w.text))
	default:
		writes = append(writes, []byte(w.text))
	}
	if w.enter {
		writes = append(writes, []byte{'\r'})
	}
	n := 0
	for _, b := range writes {
		n += len(b)
	}
	if taken, err := a.writeAll(ctx, pane, writes...); err != nil {
		if taken == 0 {
			return 0, err
		}
		return 0, control.Partial(control.Errorf(api.ErrorKindUnavailable, "pane %s took the text but not Enter (%v); look at the pane before typing again", pane, err))
	}
	if agent && (w.enter || strings.ContainsAny(w.text, "\r\n")) {
		held = true
		go a.holdForTurn(pane)
	}
	return n, nil
}

var _ paneTyper = (*agentService)(nil)

// --- pane.new ---

// New serves pane.new: a shell or a program in a new session on this hab, through
// session.create, under agent.drive_panes for an agent (hab-logic-api.md 3.12).
func (s *paneService) New(ctx context.Context, c paneCaller, p api.PaneNewParams) (api.PaneStarted, error) {
	rule := s.ruleOf(gateDrive)
	project := s.callerProject(c)
	switch {
	case c.isCapcom():
		return api.PaneStarted{}, s.denyBy(gateDrive, "pane.new", c, rule, "")
	case c.door == doorOwner, rule == readPanesAll:
	case c.door == doorPeer, rule != readPanesProject, project == "", p.Cwd != "":
		// off and agents start nothing: an agent that wants another agent uses spawn, which
		// asks the owner where the settings say so.
		return api.PaneStarted{}, s.denyBy(gateDrive, "pane.new", c, rule, "")
	default:
		p.InProject = true // project: in its own project only
	}
	if p.Cwd != "" && !filepath.IsAbs(p.Cwd) {
		return api.PaneStarted{}, control.Errorf(api.ErrorKindInvalid, "cwd must be an absolute folder")
	}
	if len(p.Argv) > 0 && p.Argv[0] == "" {
		return api.PaneStarted{}, control.Errorf(api.ErrorKindInvalid, "argv starts with an empty program")
	}
	cp := api.SessionCreateParams{Argv: p.Argv, Cwd: p.Cwd, Name: p.Name}
	if c.door == doorRoute && p.Cwd == "" {
		cp.CwdOf = c.pane
	}
	if p.InProject {
		if project == "" {
			return api.PaneStarted{}, control.Errorf(api.ErrorKindInvalid, "in_project needs a call from a pane whose session is filed in a project")
		}
		cp.Project = project
	}
	if s.drv.create == nil {
		return api.PaneStarted{}, control.Errorf(api.ErrorKindUnavailable, "pane.new needs gff-hab serve with its holder link")
	}
	made, err := s.drv.create(ctx, cp)
	if err != nil {
		var ce *control.Error
		if c.door == doorRoute && rule != readPanesAll && p.Name != "" && errors.As(err, &ce) && ce.Data.Kind != api.ErrorKindInvalid {
			// A limited caller learns nothing of names it may not list: no project, no peer.
			s.logf("panes: pane.new: caller %s: the name was refused (%s)", c.name(), ce.Data.Kind)
			return api.PaneStarted{}, control.Errorf(api.ErrorKindConflict, "pane.new could not use that name; leave it out, and the hab picks one")
		}
		return api.PaneStarted{}, err
	}
	s.logf("panes: pane.new: caller %s started pane %s (%s %s)", c.name(), made.Pane, gateDrive.setting(), rule)
	r := api.PaneStarted{Session: made.Session, Pane: made.Pane}
	if ss, ok := s.store.Session(made.Session); ok {
		r.Name = ss.Name
	}
	return r, nil
}

// --- Client input times ---

// inputTimes keeps when a client's pane.input last reached each pane: the send gate's "a person
// comes first" check for pane.type. In memory only.
type inputTimes struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (t *inputTimes) note(pane string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.at == nil {
		t.at = map[string]time.Time{}
	}
	t.at[canonicalPane(pane)] = time.Now()
}

func (t *inputTimes) within(pane string, d time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.at[canonicalPane(pane)]
	return ok && time.Since(at) < d
}

// typedInput notes each client's pane.input, as it is queued and once it is written.
type typedInput struct {
	control.Input
	t *inputTimes
}

func (i typedInput) QueueInput(ctx context.Context, client, pane string, data []byte) func() error {
	i.t.note(pane)
	wait := i.Input.QueueInput(ctx, client, pane, data)
	return func() error {
		err := wait()
		i.t.note(pane)
		return err
	}
}
