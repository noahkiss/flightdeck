package habconn

import (
	"encoding/json"
	"slices"

	"github.com/noahkiss/flightdeck/api"
)

// state is the client's copy of one hab's structure, kept by the seq and revision rules
// (docs/specs/hab-logic-api.md 3.5).
type state struct {
	host     api.HostInfo
	projects map[string]api.Project
	sessions map[string]api.Session
	panes    map[string]api.Pane
	agents   map[string]api.AgentState
	// revs holds the last revision seen per object, deleted ones included, so a late
	// update after a delete is still ignored within the epoch.
	revs map[string]uint64
}

func newState() *state {
	return &state{
		projects: map[string]api.Project{},
		sessions: map[string]api.Session{},
		panes:    map[string]api.Pane{},
		agents:   map[string]api.AgentState{},
		revs:     map[string]uint64{},
	}
}

// reset replaces every object with the snapshot's, whatever revisions it held.
func (s *state) reset(st api.Structure) {
	*s = *newState()
	s.host = st.Host
	for _, p := range st.Projects {
		s.projects[p.ID] = p
		s.revs["project/"+p.ID] = p.Revision
	}
	for _, x := range st.Sessions {
		s.sessions[x.ID] = x
		s.revs["session/"+x.ID] = x.Revision
	}
	for _, p := range st.Panes {
		s.panes[p.ID] = p
		s.revs["pane/"+p.ID] = p.Revision
	}
	for _, a := range st.Agents {
		s.agents[a.Pane] = a
		s.revs["agent/"+a.Pane] = a.Revision
	}
}

// apply merges one event and reports whether it changed the copy. An event whose revision is
// not higher than the one held is ignored. Events this copy does not keep, the ephemeral ones
// included, change nothing; the caller still hands them on.
func (s *state) apply(e api.Event) bool {
	key, ok := eventKey(e.Type)
	if !ok {
		return false
	}
	var rev uint64
	if e.Revision != nil {
		rev = *e.Revision
		if held, ok := s.revs[key+"/"+e.ID]; ok && rev <= held {
			return false
		}
	}
	switch e.Type {
	case api.EventProjectUpdated:
		var v api.Project
		if json.Unmarshal(e.Data, &v) != nil {
			return false
		}
		s.projects[e.ID] = v
	case api.EventSessionUpdated:
		var v api.Session
		if json.Unmarshal(e.Data, &v) != nil {
			return false
		}
		s.sessions[e.ID] = v
	case api.EventPaneUpdated:
		var v api.Pane
		if json.Unmarshal(e.Data, &v) != nil {
			return false
		}
		s.panes[e.ID] = v
	case api.EventPaneOwner:
		var v api.PaneOwner
		if json.Unmarshal(e.Data, &v) != nil {
			return false
		}
		p, ok := s.panes[e.ID]
		if !ok {
			return false
		}
		p.Owner = v.Owner
		if e.Revision != nil {
			p.Revision = rev
		}
		s.panes[e.ID] = p
	case api.EventAgentState:
		var v api.AgentState
		if json.Unmarshal(e.Data, &v) != nil {
			return false
		}
		if e.Revision == nil {
			rev = v.Revision
			if held, ok := s.revs[key+"/"+e.ID]; ok && rev <= held {
				return false
			}
		}
		s.agents[e.ID] = v
	case api.EventProjectDeleted:
		delete(s.projects, e.ID)
	case api.EventSessionDeleted:
		delete(s.sessions, e.ID)
	case api.EventPaneDeleted:
		delete(s.panes, e.ID)
		delete(s.agents, e.ID)
	}
	if e.Revision != nil || e.Type == api.EventAgentState {
		s.revs[key+"/"+e.ID] = rev
	}
	return true
}

// eventKey names the kind of object an event changes, for the events this copy keeps.
func eventKey(t api.EventType) (string, bool) {
	switch t {
	case api.EventProjectUpdated, api.EventProjectDeleted:
		return "project", true
	case api.EventSessionUpdated, api.EventSessionDeleted:
		return "session", true
	case api.EventPaneUpdated, api.EventPaneDeleted, api.EventPaneOwner:
		return "pane", true
	case api.EventAgentState:
		return "agent", true
	}
	return "", false
}

// structure returns a copy in the hab's own shape, every list sorted by id. Order within a
// project lives in Project.Sessions, not in list order.
func (s *state) structure() api.Structure {
	st := api.Structure{
		Host:     s.host,
		Projects: make([]api.Project, 0, len(s.projects)),
		Sessions: make([]api.Session, 0, len(s.sessions)),
		Panes:    make([]api.Pane, 0, len(s.panes)),
	}
	for _, id := range sortedKeys(s.projects) {
		st.Projects = append(st.Projects, s.projects[id])
	}
	for _, id := range sortedKeys(s.sessions) {
		st.Sessions = append(st.Sessions, s.sessions[id])
	}
	for _, id := range sortedKeys(s.panes) {
		st.Panes = append(st.Panes, s.panes[id])
	}
	for _, id := range sortedKeys(s.agents) {
		st.Agents = append(st.Agents, s.agents[id])
	}
	return st
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
