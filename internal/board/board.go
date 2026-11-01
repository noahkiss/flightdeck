// Package board turns every host's structure into the rows gff's sidebar and status board show:
// one row per session and per unfiled pane, on every host, ranked by who needs the owner
// (docs/specs/gff-client.md 4.1, 7.1; app.md 3.1).
//
// Rows are merged by host: a row is named by (host, session) or (host, pane), and nothing is
// compared across hosts but the time each row entered its rank. An Order keeps rows still while
// the owner reads: a row moves only when its rank changes.
package board

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/merge"
)

// Rank is a row's group, top to bottom (app.md 3.1).
type Rank int

const (
	RankBlocked Rank = iota + 1 // blocked: permission, question, auth
	RankError                   // the turn stopped
	RankDone                    // done, not seen
	RankWorking                 // running
	RankIdle                    // idle, or done and seen
	RankOther                   // no agent, exited, unfiled
)

// NeedsYou reports whether the rank is one the owner should act on: blocked, error, or done
// and not seen (gff-client.md 2.2).
func (r Rank) NeedsYou() bool { return r >= RankBlocked && r <= RankDone }

// Section is the plain label of a rank's board section (app.md 3.1).
func (r Rank) Section() string {
	switch {
	case r.NeedsYou():
		return "Needs you"
	case r == RankWorking:
		return "Working"
	case r == RankIdle:
		return "Done"
	}
	return "Other"
}

// Key names a row across hosts.
type Key struct {
	Host    string // hosts.Spec.Label
	Session string // empty for an unfiled pane
	Pane    string // empty for a session with no live pane
}

func (k Key) String() string { return k.Host + "/" + k.Session + "/" + k.Pane }

// Row is one session, or one unfiled pane, on one host.
type Row struct {
	Key
	Name    string      // the session's name; for an unfiled pane, "unfiled <pane>"
	Title   string      // the agent session's title, when its harness gives one
	Project api.Project // zero when unfiled or the project is unknown
	State   api.SessionState
	Exit    *api.ExitStatus
	Agent   *api.AgentState // nil when the pane has no agent state
	Rank    Rank
	// At is when the agent state began, by the hab's clock (AgentState.at); zero without one.
	At time.Time
	// Host status: rows of a host that is not online stay, dimmed, with no actions.
	HostStatus hosts.Status
	HostSince  time.Time
}

// Offline reports whether the row's host has no live connection.
func (r Row) Offline() bool { return r.HostStatus != hosts.Online }

// StateWord is the row's state in the spec's words: the agent state with its kind, else
// "exited" or "no agent".
func (r Row) StateWord() string {
	if r.State == api.SessionStateExited {
		return "exited"
	}
	if r.Agent == nil {
		if r.Session == "" {
			return "unfiled"
		}
		return "no agent"
	}
	if r.Agent.State == api.AgentStatusBlocked && r.Agent.Kind != "" {
		return "blocked:" + string(r.Agent.Kind)
	}
	return string(r.Agent.State)
}

// Context is the row's one line of context (gff-client.md 7.1): the pending command for a
// permission, else the request's title, its description, the agent's last line, or the tool.
// Empty when there is no request.
func (r Row) Context() string {
	if r.Agent == nil || r.Agent.Request == nil {
		return ""
	}
	q := r.Agent.Request
	switch {
	case q.Command != "":
		return q.Command
	case q.Title != "":
		return q.Title
	case q.Description != "":
		return q.Description
	case q.ContextLine != "":
		return q.ContextLine
	}
	return q.Tool
}

// RankOf ranks a session or pane from its session state and agent state. seen says the owner
// has looked at the pane since its agent became done.
func RankOf(state api.SessionState, a *api.AgentState, seen bool) Rank {
	if state == api.SessionStateExited || a == nil {
		return RankOther
	}
	switch a.State {
	case api.AgentStatusBlocked:
		return RankBlocked
	case api.AgentStatusError:
		return RankError
	case api.AgentStatusDone:
		if seen {
			return RankIdle
		}
		return RankDone
	case api.AgentStatusWorking:
		return RankWorking
	case api.AgentStatusIdle:
		return RankIdle
	}
	return RankOther // none, exited, and words this client does not know
}

// Seen reports whether the owner has seen a pane's done state. gff marks a pane seen when it
// focuses it; until session.focus is built the mark stays in gff.
type Seen func(host, pane string, a *api.AgentState) bool

// Rows builds the rows of every host. A host that failed for good (the API revision does not
// fit, the device id changed) has no rows: its sessions are hidden, not stale (4.2). Rows come
// unordered; Order sorts them.
func Rows(hs []hosts.Host, seen Seen) []Row {
	var rows []Row
	shown := merge.Shown(hs) // one hab reached by two labels shows once (mesh.md 2)
	for i, h := range hs {
		if !shown[i] {
			continue
		}
		st := h.Structure
		projects := map[string]api.Project{}
		for _, p := range st.Projects {
			projects[p.ID] = p
		}
		agents := map[string]*api.AgentState{}
		for i := range st.Agents {
			agents[st.Agents[i].Pane] = &st.Agents[i]
		}
		filed := map[string]bool{}
		for _, s := range st.Sessions {
			r := Row{Key: Key{Host: h.Label, Session: s.ID, Pane: s.Pane}, Name: s.Name, Title: s.Title,
				Project: projects[s.Project], State: s.State, Exit: s.Exit, HostStatus: h.Status, HostSince: h.Since}
			if s.Pane != "" {
				filed[s.Pane] = true
				r.Agent = agents[s.Pane]
			}
			rows = append(rows, finish(r, seen))
		}
		for _, p := range st.Panes {
			if filed[p.ID] || p.Session != "" {
				continue
			}
			r := Row{Key: Key{Host: h.Label, Pane: p.ID}, Name: "unfiled " + short(p.ID), State: api.SessionStateRunning,
				Agent: agents[p.ID], HostStatus: h.Status, HostSince: h.Since}
			r = finish(r, seen)
			r.Rank = RankOther // an unfiled pane sits at the bottom of its host (4.2)
			rows = append(rows, r)
		}
	}
	return rows
}

func finish(r Row, seen Seen) Row {
	s := false
	if r.Agent != nil && r.Agent.State == api.AgentStatusDone && seen != nil {
		s = seen(r.Host, r.Pane, r.Agent)
	}
	r.Rank = RankOf(r.State, r.Agent, s)
	if r.Agent != nil {
		if t, err := time.Parse(time.RFC3339Nano, r.Agent.At); err == nil {
			r.At = t
		}
	}
	return r
}

// Order keeps the board's order steady. Rows sort by rank, then by when they entered that
// rank, newest first. A row's entry time changes only when its rank changes, so a row moves
// only then (app.md 3.1). The entry time is the agent state's own time when it has one, else
// the time Order first saw the row in that rank.
type Order struct {
	Now     func() time.Time
	entered map[Key]entry
}

type entry struct {
	rank Rank
	at   time.Time
}

// Sort orders rows in place and forgets rows that are gone.
func (o *Order) Sort(rows []Row) {
	if o.entered == nil {
		o.entered = map[Key]entry{}
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	t := now() // one time for every row first seen in this sort, so they tie and sort by name
	live := make(map[Key]bool, len(rows))
	for _, r := range rows {
		live[r.Key] = true
		e, ok := o.entered[r.Key]
		if !ok || e.rank != r.Rank {
			at := r.At
			if at.IsZero() {
				at = t
			}
			o.entered[r.Key] = entry{r.Rank, at}
		}
	}
	for k := range o.entered {
		if !live[k] {
			delete(o.entered, k)
		}
	}
	slices.SortStableFunc(rows, func(a, b Row) int {
		if c := cmp.Compare(a.Rank, b.Rank); c != 0 {
			return c
		}
		if c := o.entered[b.Key].at.Compare(o.entered[a.Key].at); c != 0 {
			return c // newest first
		}
		if c := cmp.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.Key.String(), b.Key.String())
	})
}

// GroupBy says how the sidebar and the board group their rows.
type GroupBy int

const (
	GroupNone    GroupBy = iota // one list, by rank
	GroupProject                // by project; a project on two hosts is one group
	GroupHost                   // by host
)

// Group is one titled run of rows. Rows keep the order they were given.
type Group struct {
	Title string
	Rows  []Row
}

// Groups splits ordered rows into groups. Groups appear in the order of their first row, so
// the group that holds the row needing the owner most comes first; unfiled rows and rows with
// no project come last under "no project".
func Groups(rows []Row, by GroupBy) []Group {
	if by == GroupNone {
		return []Group{{Rows: rows}}
	}
	var out []Group
	index := map[string]int{}
	var loose []Row
	for _, r := range rows {
		var key, title string
		switch by {
		case GroupHost:
			key, title = r.Host, r.Host
		case GroupProject:
			key, title = projectKey(r), r.Project.Name
		}
		if key == "" {
			loose = append(loose, r)
			continue
		}
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, Group{Title: title})
		}
		out[i].Rows = append(out[i].Rows, r)
	}
	if len(loose) > 0 {
		out = append(out, Group{Title: "no project", Rows: loose})
	}
	return out
}

// projectKey is a project's identity across hosts (merge.ProjectKey).
func projectKey(r Row) string { return merge.ProjectKey(r.Host, r.Project) }

// NeedsYou counts the rows that need the owner on hosts that are online.
func NeedsYou(rows []Row) int {
	n := 0
	for _, r := range rows {
		if r.Rank.NeedsYou() && !r.Offline() {
			n++
		}
	}
	return n
}

// Match reports whether a row matches a filter typed in the sidebar: session, project or host
// name, case folded (gff-client.md 4.1).
func Match(r Row, filter string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	for _, s := range []string{r.Name, r.Title, r.Project.Name, r.Host} {
		if strings.Contains(strings.ToLower(s), f) {
			return true
		}
	}
	return false
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
