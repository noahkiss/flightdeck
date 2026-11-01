// Package merge is the multi-host merge of the shared client library: one view of every hab a
// client connects to, and one ordered stream of their events (docs/specs/mesh.md 4.5).
//
// Package hosts keeps one connection per hab, each with its own seq, epoch and resync. This
// package sits on top of it and adds what a client shows across hosts:
//
//   - Build turns the hosts' models into one Model: every host with its status, projects
//     matched by identity (git remote, else root commit) across hosts with one location per
//     host, and every session and unfiled pane tagged with its host.
//   - A host that is not online keeps its last structure, marked stale. A client shows stale
//     rows dimmed and sends them no input until the host's next snapshot arrives
//     (landscape.md idea 4.9).
//   - A Merger reads a hosts.Set's updates and hands them on as one stream of Events, numbered
//     by a client-side seq and keyed by host label plus object id. A host that comes online, or
//     comes back with a new epoch, sends one Resync event for that host alone; the other hosts'
//     objects are untouched (hab-logic-api.md 7.9).
//
// Nothing here needs cgo, so the app's Go library and gff share it.
package merge

import (
	"cmp"
	"slices"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

// Host is one host in the merged view.
type Host struct {
	Label    string // the client's name for the host (hosts.Spec.Label)
	DeviceID string // the host's identity, from its last welcome; empty before one
	HostID   string // the name the hab gives itself; display only
	Epoch    string // the epoch of the last welcome
	Status   hosts.Status
	Since    time.Time // when Status began, on this machine's clock
	Err      error     // why the host is not online; nil while online
	// Shown is false for a second label that reaches a hab another label already shows (the
	// same device id). Its objects are left out of the model.
	Shown bool
}

// Stale reports whether the host's objects are a cached copy: the host is not online, but a
// structure from an earlier connection is still held. A client shows them dimmed.
func (h Host) Stale() bool { return h.Status != hosts.Online && h.Epoch != "" }

// Input reports whether a client may send the host input: only while it is online with a fresh
// snapshot applied. A stale host takes none until its next snapshot (idea 4.9).
func (h Host) Input() bool { return h.Status == hosts.Online }

// Ref names one object on one host. Object ids are unique on their hab only.
type Ref struct {
	Host string // Host.Label
	ID   string
}

// Location is one host's copy of a project.
type Location struct {
	Host    string // Host.Label
	Project api.Project
	Stale   bool
}

// Project is one project across hosts, matched by identity.
type Project struct {
	// Key is the identity: "remote:<remote>", else "root:<root commit>", else
	// "host:<label>/<id>" for a project with neither, which matches nothing on another host.
	Key        string
	Name       string // the name on the first location
	Remote     string
	RootCommit string
	// Locations has one entry per host that holds the project, in host order.
	Locations []Location
}

// Session is one session on one host.
type Session struct {
	Ref
	Session api.Session
	Pane    *api.Pane       // nil when the session has no live pane
	Agent   *api.AgentState // nil when the pane has no agent state
	Project string          // Project.Key; empty when unfiled
	Stale   bool
}

// Pane is one unfiled pane on one host: a live pane no session links.
type Pane struct {
	Ref
	Pane  api.Pane
	Agent *api.AgentState
	Stale bool
}

// Model is the merged view of every host.
type Model struct {
	// Seq is the seq of the last Event the Merger applied before it built this model; 0 from
	// Build.
	Seq      uint64
	Hosts    []Host    // in the Set's order
	Projects []Project // by name, then key
	Sessions []Session // by host order, then the hab's order (project order, then id)
	Unfiled  []Pane    // by host order, then pane id
}

// Host returns one host by label.
func (m Model) Host(label string) (Host, bool) {
	for _, h := range m.Hosts {
		if h.Label == label {
			return h, true
		}
	}
	return Host{}, false
}

// Session returns one session by host and id.
func (m Model) Session(r Ref) (Session, bool) {
	for _, s := range m.Sessions {
		if s.Ref == r {
			return s, true
		}
	}
	return Session{}, false
}

// Input reports whether a client may send input to an object on host label.
func (m Model) Input(label string) bool {
	h, ok := m.Host(label)
	return ok && h.Input()
}

// ProjectKey is a project's identity across hosts: its remote, else its root commit
// (hab-logic-api.md 2.2). A project with neither is its own on its host.
func ProjectKey(host string, p api.Project) string {
	switch {
	case p.ID == "":
		return ""
	case p.Remote != "":
		return "remote:" + p.Remote
	case p.RootCommit != "":
		return "root:" + p.RootCommit
	}
	return "host:" + host + "/" + p.ID
}

// Shown reports, per host, whether its objects are shown. The device id is the host's identity
// (mesh.md 2): when two labels reach one hab, the first online label shows it, else the first
// label. A host that failed for good (the API revision does not fit, the device id changed)
// shows nothing: its objects are hidden, not stale.
func Shown(hs []hosts.Host) []bool {
	devices := map[string]int{}
	for i, h := range hs {
		d := h.DeviceID()
		if d == "" || h.Status == hosts.Failed {
			continue
		}
		if j, ok := devices[d]; !ok || hs[j].Status != hosts.Online && h.Status == hosts.Online {
			devices[d] = i
		}
	}
	out := make([]bool, len(hs))
	for i, h := range hs {
		if h.Status == hosts.Failed {
			continue
		}
		d := h.DeviceID()
		out[i] = d == "" || devices[d] == i
	}
	return out
}

// Build merges the hosts' models into one view.
func Build(hs []hosts.Host) Model {
	m := Model{Hosts: make([]Host, 0, len(hs))}
	shown := Shown(hs)
	projects := map[string]int{}
	for i, h := range hs {
		mh := Host{Label: h.Label, DeviceID: h.DeviceID(), HostID: h.Welcome.Host.ID, Epoch: h.Welcome.Epoch,
			Status: h.Status, Since: h.Since, Err: h.Err, Shown: shown[i]}
		m.Hosts = append(m.Hosts, mh)
		if !mh.Shown {
			continue
		}
		stale := mh.Stale()
		st := h.Structure
		keys := map[string]string{} // project id to key
		for _, p := range st.Projects {
			k := ProjectKey(h.Label, p)
			keys[p.ID] = k
			j, ok := projects[k]
			if !ok {
				j = len(m.Projects)
				projects[k] = j
				m.Projects = append(m.Projects, Project{Key: k, Name: p.Name, Remote: p.Remote, RootCommit: p.RootCommit})
			}
			m.Projects[j].Locations = append(m.Projects[j].Locations, Location{Host: h.Label, Project: p, Stale: stale})
		}
		panes := map[string]*api.Pane{}
		for k := range st.Panes {
			panes[st.Panes[k].ID] = &st.Panes[k]
		}
		agents := map[string]*api.AgentState{}
		for k := range st.Agents {
			agents[st.Agents[k].Pane] = &st.Agents[k]
		}
		filed := map[string]bool{}
		for _, s := range sessionOrder(st) {
			x := Session{Ref: Ref{h.Label, s.ID}, Session: s, Project: keys[s.Project], Stale: stale}
			if s.Pane != "" {
				filed[s.Pane] = true
				x.Pane = panes[s.Pane]
				x.Agent = agents[s.Pane]
			}
			m.Sessions = append(m.Sessions, x)
		}
		for _, p := range st.Panes {
			if filed[p.ID] || p.Session != "" {
				continue
			}
			m.Unfiled = append(m.Unfiled, Pane{Ref: Ref{h.Label, p.ID}, Pane: p, Agent: agents[p.ID], Stale: stale})
		}
	}
	slices.SortStableFunc(m.Projects, func(a, b Project) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Key, b.Key))
	})
	return m
}

// sessionOrder lists a hab's sessions in its own order: each project's sessions in the
// project's order, projects by id, then sessions in no project, by id.
func sessionOrder(st api.Structure) []api.Session {
	byID := make(map[string]api.Session, len(st.Sessions))
	for _, s := range st.Sessions {
		byID[s.ID] = s
	}
	out := make([]api.Session, 0, len(st.Sessions))
	done := map[string]bool{}
	for _, p := range st.Projects { // the copy is sorted by id
		for _, id := range p.Sessions {
			if s, ok := byID[id]; ok && !done[id] {
				out = append(out, s)
				done[id] = true
			}
		}
	}
	for _, s := range st.Sessions {
		if !done[s.ID] {
			out = append(out, s)
		}
	}
	return out
}
