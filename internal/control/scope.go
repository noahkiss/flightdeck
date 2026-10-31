package control

import (
	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// filter decides which events a subscription sees (spec 3.5). Scope host sees everything. Scope
// projects sees those projects, the sessions filed in them and the panes those sessions link.
// Scope sessions sees those sessions and their panes. The filter tracks which sessions and panes
// are in scope as events pass, so an object that leaves the scope sends one last event, and then
// nothing more.
type filter struct {
	kind     api.ScopeKind
	projects map[string]bool // named projects (scope projects)
	sessions map[string]bool // sessions in scope: named (scope sessions) or tracked (projects)
	named    bool            // sessions is the fixed list from the scope
	panes    map[string]bool // panes in scope, tracked
}

func newFilter(sc api.Scope) *filter {
	f := &filter{kind: sc.Kind, projects: map[string]bool{}, sessions: map[string]bool{}, panes: map[string]bool{}}
	switch sc.Kind {
	case api.ScopeKindProjects:
		for _, id := range sc.IDs {
			f.projects[id] = true
		}
	case api.ScopeKindSessions:
		f.named = true
		for _, id := range sc.IDs {
			f.sessions[id] = true
		}
	}
	return f
}

// seed fills the tracked sets from a snapshot and returns the part of it in scope.
func (f *filter) seed(st api.Structure) api.Structure {
	if f.kind == api.ScopeKindHost {
		return st
	}
	out := api.Structure{Host: st.Host, Projects: []api.Project{}, Sessions: []api.Session{}, Panes: []api.Pane{}}
	for _, p := range st.Projects {
		if f.projects[p.ID] {
			out.Projects = append(out.Projects, p)
		}
	}
	for _, s := range st.Sessions {
		if f.sessionIn(s) {
			f.sessions[s.ID] = true
			if s.Pane != "" {
				f.panes[s.Pane] = true
			}
			out.Sessions = append(out.Sessions, s)
		}
	}
	for _, p := range st.Panes {
		if f.panes[p.ID] || (p.Session != "" && f.sessions[p.Session]) {
			f.panes[p.ID] = true
			out.Panes = append(out.Panes, p)
		}
	}
	return out
}

func (f *filter) sessionIn(s api.Session) bool {
	if f.named {
		return f.sessions[s.ID]
	}
	return s.Project != "" && f.projects[s.Project]
}

// pass reports whether the subscription sees e, and updates the tracked sets.
func (f *filter) pass(e structure.Event) bool {
	if f.kind == api.ScopeKindHost {
		return true
	}
	switch e.Type {
	case structure.ProjectUpdated, structure.ProjectDeleted:
		return f.projects[e.ID]
	case structure.SessionUpdated:
		s, ok := e.Data.(api.Session)
		if !ok {
			return false
		}
		if f.sessionIn(s) {
			f.sessions[s.ID] = true
			if s.Pane != "" {
				f.panes[s.Pane] = true
			}
			return true
		}
		if f.sessions[s.ID] { // it left the scope: one last event
			delete(f.sessions, s.ID)
			return true
		}
		return false
	case structure.SessionDeleted:
		if f.sessions[e.ID] {
			if !f.named {
				delete(f.sessions, e.ID)
			}
			return true
		}
		return false
	case structure.PaneUpdated:
		p, ok := e.Data.(api.Pane)
		if !ok {
			return false
		}
		if p.Session != "" && f.sessions[p.Session] {
			f.panes[p.ID] = true
			return true
		}
		if f.panes[p.ID] {
			delete(f.panes, p.ID)
			return true
		}
		return false
	case structure.PaneDeleted:
		if f.panes[e.ID] {
			delete(f.panes, e.ID)
			return true
		}
		return false
	}
	// Event types this filter does not know yet (pane.owner, pane.activity) are about a pane.
	return f.panes[e.ID]
}
