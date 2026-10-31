// Package structure keeps gff-hab's shared structure: projects, sessions, their names and order,
// and the link from each session to a holder pane. It persists that structure in one JSON file
// under a state directory and rebuilds it after a restart by matching the live holder panes to
// the stored session records. See docs/specs/hab-logic-api.md, section 2.2.
//
// The package keeps no layout, splits, tabs, focus or theme; each client owns its presentation.
// It mints no seq: it hands each change to a callback, and the control connection numbers it.
package structure

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

const (
	stateRunning = "running"
	stateExited  = "exited"
)

// Event types this package emits.
const (
	ProjectUpdated api.EventType = "project.updated"
	ProjectDeleted api.EventType = "project.deleted"
	SessionUpdated api.EventType = "session.updated"
	SessionDeleted api.EventType = "session.deleted"
	PaneUpdated    api.EventType = "pane.updated"
	PaneDeleted    api.EventType = "pane.deleted"
)

// Errors returned by Store methods. Each maps to an api.ErrorKind of the same name.
var (
	ErrNotFound = errors.New("not_found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
)

// Event is one structure change, before it gets a seq. Data is api.Project, api.Session,
// api.Pane or api.Deleted, already passed through api.NonNil. Revision is the object's revision
// after the change; on a *.deleted event it is the final revision.
type Event struct {
	Type     api.EventType
	ID       string
	Revision uint64
	Data     any
}

// API turns the event into the wire form with the given seq.
func (e Event) API(seq uint64) (api.Event, error) {
	data, err := json.Marshal(e.Data)
	if err != nil {
		return api.Event{}, err
	}
	rev := e.Revision
	return api.Event{Seq: seq, Type: e.Type, ID: e.ID, Revision: &rev, Data: data}, nil
}

// Health reports problems with the state file. The zero value means all is well.
type Health struct {
	StatePath string // the state file
	Problem   string // why the stored state could not be loaded; the store started empty
	KeptAs    string // where the unreadable file was kept, when Problem is set and it existed
	SaveError string // the last save failure; cleared by the next good save
}

// Config is what Open needs. Dir and Holders are required.
type Config struct {
	Dir     string      // state directory; created if missing
	Holders Holders     // the live holders on this host
	OnEvent func(Event) // called for every change, under the store lock, in order
	Now     func() time.Time
}

type paneState struct {
	rec      paneRecord
	holderID string
	offset   uint64
	exit     *api.ExitStatus
}

// Store holds the structure. It is safe for use from many goroutines. OnEvent runs under the
// store lock and must not call back into the store.
type Store struct {
	mu       sync.Mutex
	path     string
	holders  Holders
	onEvent  func(Event)
	now      func() time.Time
	cancel   func()
	health   Health
	projects []*projectRecord // creation order
	sessions []*sessionRecord // creation order
	panes    map[string]*paneState
}

// Open loads the state from cfg.Dir and matches it to the live holder panes. An unreadable or
// corrupt state file never stops it: the store starts empty, the live panes appear unfiled, the
// bad file is kept aside and Health reports the problem. Open fails only when the state
// directory cannot be made or the holders cannot list their panes; it never touches a pane.
func Open(cfg Config) (*Store, error) {
	if cfg.Dir == "" || cfg.Holders == nil {
		return nil, fmt.Errorf("%w: structure needs a state directory and holders", ErrInvalid)
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	s := &Store{
		path:    filepath.Join(cfg.Dir, StateFile),
		holders: cfg.Holders,
		onEvent: cfg.OnEvent,
		now:     cfg.Now,
		panes:   map[string]*paneState{},
	}
	if s.onEvent == nil {
		s.onEvent = func(Event) {}
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.health.StatePath = s.path

	st, problem := loadState(s.path)
	if problem != nil {
		s.health.Problem = problem.Error()
		if _, err := os.Stat(s.path); err == nil {
			aside, err := keepAside(s.path, s.now())
			if err != nil {
				s.health.Problem += "; could not keep the bad file aside: " + err.Error()
			}
			s.health.KeptAs = aside
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Subscribe before listing so no notice falls between the list and the subscription. A
	// notice that arrives now waits for the lock and then applies to the reconciled state.
	s.cancel = cfg.Holders.Subscribe(s.handle)
	live, err := cfg.Holders.Panes()
	if err != nil {
		s.cancel()
		return nil, fmt.Errorf("list holder panes: %w", err)
	}
	s.reconcile(&st, live)
	return s, nil
}

// Close stops listening to the holders. It touches no pane.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// Health returns the state file's health.
func (s *Store) Health() Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}

// reconcile builds the in-memory structure from the stored state and the live panes:
//   - a live pane keeps its stored revision; a size or build change while gff-hab was down
//     raises it;
//   - a live pane no session links is unfiled;
//   - a stored pane that is gone emits pane.deleted with a final revision;
//   - a running session whose pane is gone, or whose child exited, is marked exited.
func (s *Store) reconcile(st *stateFile, live []HolderPane) {
	var events []Event
	for i := range st.Projects {
		p := st.Projects[i]
		s.projects = append(s.projects, &p)
	}
	for i := range st.Sessions {
		ss := st.Sessions[i]
		s.sessions = append(s.sessions, &ss)
	}
	stored := map[string]paneRecord{}
	for _, p := range st.Panes {
		stored[p.ID] = p
	}
	var changed []string
	for _, hp := range live {
		ps := &paneState{holderID: hp.HolderID, offset: hp.Offset, exit: copyExit(hp.Exit)}
		if rec, ok := stored[hp.ID]; ok {
			ps.rec = rec
			if rec.Cols != hp.Cols || rec.Rows != hp.Rows || rec.HolderBuild != hp.HolderBuild {
				ps.rec.Cols, ps.rec.Rows, ps.rec.HolderBuild = hp.Cols, hp.Rows, hp.HolderBuild
				ps.rec.Revision++
				changed = append(changed, hp.ID)
			}
		} else {
			ps.rec = paneRecord{ID: hp.ID, Revision: 1, HolderBuild: hp.HolderBuild, Cols: hp.Cols, Rows: hp.Rows}
			changed = append(changed, hp.ID)
		}
		s.panes[hp.ID] = ps
	}
	ids := make([]string, 0, len(stored))
	for id := range stored {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, ok := s.panes[id]; !ok {
			events = append(events, Event{PaneDeleted, id, stored[id].Revision + 1, api.Deleted{}})
		}
	}
	for _, ss := range s.sessions {
		if ss.State != stateRunning || ss.Pane == "" {
			continue
		}
		ps, ok := s.panes[ss.Pane]
		switch {
		case !ok:
			ss.State = stateExited
		case ps.exit != nil:
			ss.State = stateExited
			ss.Exit = copyExit(ps.exit)
		default:
			continue
		}
		ss.Revision++
		events = append(events, s.sessionEvent(ss))
	}
	for _, id := range changed {
		events = append(events, s.paneEvent(id))
	}
	s.commit(events)
}

// --- Projects ---

// CreateProject adds a project. remote identifies it; rootCommit is the fallback when there is
// no remote. A second project with the same identity is a conflict.
func (s *Store) CreateProject(name, remote, rootCommit string) (api.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		return api.Project{}, fmt.Errorf("%w: project name is empty", ErrInvalid)
	}
	if remote == "" && rootCommit == "" {
		return api.Project{}, fmt.Errorf("%w: a project needs a git remote or a root commit", ErrInvalid)
	}
	if p := s.findProject(remote, rootCommit); p != nil {
		return api.Project{}, fmt.Errorf("%w: project %s has the same identity", ErrConflict, p.ID)
	}
	p := &projectRecord{ID: newID("p"), Revision: 1, Name: name, Remote: remote, RootCommit: rootCommit}
	s.projects = append(s.projects, p)
	s.commit([]Event{s.projectEvent(p)})
	return projectAPI(p), nil
}

// ProjectByIdentity finds the project for a git remote, or for a root commit when remote is
// empty.
func (s *Store) ProjectByIdentity(remote, rootCommit string) (api.Project, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.findProject(remote, rootCommit)
	if p == nil {
		return api.Project{}, false
	}
	return projectAPI(p), true
}

func (s *Store) findProject(remote, rootCommit string) *projectRecord {
	for _, p := range s.projects {
		if remote != "" && p.Remote == remote {
			return p
		}
		if remote == "" && p.Remote == "" && p.RootCommit == rootCommit {
			return p
		}
	}
	return nil
}

// RenameProject changes a project's name. Project names need not be unique.
func (s *Store) RenameProject(id, name string) (api.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.project(id)
	if p == nil {
		return api.Project{}, fmt.Errorf("%w: project %s", ErrNotFound, id)
	}
	if name == "" {
		return api.Project{}, fmt.Errorf("%w: project name is empty", ErrInvalid)
	}
	if p.Name == name {
		return projectAPI(p), nil
	}
	p.Name = name
	p.Revision++
	s.commit([]Event{s.projectEvent(p)})
	return projectAPI(p), nil
}

// DeleteProject removes a project. Its sessions stay, with no project.
func (s *Store) DeleteProject(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.projects, func(p *projectRecord) bool { return p.ID == id })
	if i < 0 {
		return fmt.Errorf("%w: project %s", ErrNotFound, id)
	}
	p := s.projects[i]
	var events []Event
	for _, sid := range p.Sessions {
		ss := s.session(sid)
		ss.Project = ""
		ss.Revision++
		events = append(events, s.sessionEvent(ss))
	}
	s.projects = slices.Delete(s.projects, i, i+1)
	events = append(events, Event{ProjectDeleted, p.ID, p.Revision + 1, api.Deleted{}})
	s.commit(events)
	return nil
}

// ReorderSessions sets the order of a project's sessions. ids must hold exactly the project's
// sessions.
func (s *Store) ReorderSessions(projectID string, ids []string) (api.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.project(projectID)
	if p == nil {
		return api.Project{}, fmt.Errorf("%w: project %s", ErrNotFound, projectID)
	}
	a, b := slices.Clone(p.Sessions), slices.Clone(ids)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) || len(slices.Compact(b)) != len(ids) {
		return api.Project{}, fmt.Errorf("%w: the order must name each session of the project once", ErrInvalid)
	}
	if slices.Equal(p.Sessions, ids) {
		return projectAPI(p), nil
	}
	p.Sessions = slices.Clone(ids)
	p.Revision++
	s.commit([]Event{s.projectEvent(p)})
	return projectAPI(p), nil
}

// --- Sessions ---

// SessionSpec describes a new session.
type SessionSpec struct {
	Name    string // unique on the host
	Project string // project id; empty for none
	Pane    string // a live pane no other session links
	Index   int    // position in the project's order; -1 or past the end appends
}

// CreateSession adds a session record for a live pane. Filing an unfiled pane is the same call.
func (s *Store) CreateSession(spec SessionSpec) (api.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if spec.Name == "" {
		return api.Session{}, fmt.Errorf("%w: session name is empty", ErrInvalid)
	}
	if s.sessionByName(spec.Name) != nil {
		return api.Session{}, fmt.Errorf("%w: a session named %q exists", ErrConflict, spec.Name)
	}
	var p *projectRecord
	if spec.Project != "" {
		if p = s.project(spec.Project); p == nil {
			return api.Session{}, fmt.Errorf("%w: project %s", ErrNotFound, spec.Project)
		}
	}
	ps := s.panes[spec.Pane]
	if ps == nil {
		return api.Session{}, fmt.Errorf("%w: pane %q", ErrNotFound, spec.Pane)
	}
	if other := s.sessionByPane(spec.Pane); other != nil {
		return api.Session{}, fmt.Errorf("%w: pane %s belongs to session %s", ErrConflict, spec.Pane, other.ID)
	}
	ss := &sessionRecord{ID: newID("s"), Revision: 1, Name: spec.Name, Project: spec.Project, Pane: spec.Pane, State: stateRunning}
	if ps.exit != nil {
		ss.State = stateExited
		ss.Exit = copyExit(ps.exit)
	}
	s.sessions = append(s.sessions, ss)
	events := []Event{s.sessionEvent(ss)}
	if p != nil {
		p.Sessions = insertAt(p.Sessions, spec.Index, ss.ID)
		p.Revision++
		events = append(events, s.projectEvent(p))
	}
	ps.rec.Revision++
	events = append(events, s.paneEvent(spec.Pane))
	s.commit(events)
	return sessionAPI(ss), nil
}

// RenameSession changes a session's name. A name another session holds is a conflict.
func (s *Store) RenameSession(id, name string) (api.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.session(id)
	if ss == nil {
		return api.Session{}, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if name == "" {
		return api.Session{}, fmt.Errorf("%w: session name is empty", ErrInvalid)
	}
	if ss.Name == name {
		return sessionAPI(ss), nil
	}
	if other := s.sessionByName(name); other != nil {
		return api.Session{}, fmt.Errorf("%w: a session named %q exists", ErrConflict, name)
	}
	ss.Name = name
	ss.Revision++
	s.commit([]Event{s.sessionEvent(ss)})
	return sessionAPI(ss), nil
}

// MoveSession puts a session at index in a project's order. projectID may be the session's own
// project (a move within it) or empty (the session leaves its project). index -1 or past the end
// appends.
func (s *Store) MoveSession(id, projectID string, index int) (api.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.session(id)
	if ss == nil {
		return api.Session{}, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	var to *projectRecord
	if projectID != "" {
		if to = s.project(projectID); to == nil {
			return api.Session{}, fmt.Errorf("%w: project %s", ErrNotFound, projectID)
		}
	}
	var events []Event
	if from := s.project(ss.Project); from != nil {
		before := slices.Clone(from.Sessions)
		from.Sessions = slices.DeleteFunc(from.Sessions, func(x string) bool { return x == id })
		if from == to {
			from.Sessions = insertAt(from.Sessions, index, id)
		}
		if !slices.Equal(before, from.Sessions) {
			from.Revision++
			events = append(events, s.projectEvent(from))
		}
		if from == to {
			s.commit(events)
			return sessionAPI(ss), nil
		}
	}
	if to != nil {
		to.Sessions = insertAt(to.Sessions, index, id)
		to.Revision++
		events = append(events, s.projectEvent(to))
	}
	if ss.Project != projectID {
		ss.Project = projectID
		ss.Revision++
		events = append([]Event{s.sessionEvent(ss)}, events...)
	}
	s.commit(events)
	return sessionAPI(ss), nil
}

// CloseSession removes a session record. It does not stop the program: the caller asks the
// holder for that. A pane that is still live becomes unfiled.
func (s *Store) CloseSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.sessions, func(x *sessionRecord) bool { return x.ID == id })
	if i < 0 {
		return fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	ss := s.sessions[i]
	s.sessions = slices.Delete(s.sessions, i, i+1)
	events := []Event{{SessionDeleted, ss.ID, ss.Revision + 1, api.Deleted{}}}
	if p := s.project(ss.Project); p != nil {
		p.Sessions = slices.DeleteFunc(p.Sessions, func(x string) bool { return x == id })
		p.Revision++
		events = append(events, s.projectEvent(p))
	}
	if ps := s.panes[ss.Pane]; ps != nil {
		ps.rec.Revision++
		events = append(events, s.paneEvent(ss.Pane))
	}
	s.commit(events)
	return nil
}

// --- Holder notices ---

func (s *Store) handle(n Notice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch n.Kind {
	case PaneAdded, PaneResized:
		s.paneSeen(n.Pane)
	case PaneExited:
		if ps := s.panes[n.Pane.ID]; ps != nil {
			ps.exit = copyExit(n.Exit)
			ps.offset = max(ps.offset, n.Pane.Offset)
		}
		ss := s.sessionByPane(n.Pane.ID)
		if ss != nil && ss.State == stateRunning {
			ss.State = stateExited
			ss.Exit = copyExit(n.Exit)
			ss.Revision++
			s.commit([]Event{s.sessionEvent(ss)})
		}
	case PaneGone:
		ps := s.panes[n.Pane.ID]
		if ps == nil {
			return
		}
		delete(s.panes, n.Pane.ID)
		var events []Event
		if ss := s.sessionByPane(n.Pane.ID); ss != nil && ss.State == stateRunning {
			ss.State = stateExited
			ss.Exit = copyExit(ps.exit)
			ss.Revision++
			events = append(events, s.sessionEvent(ss))
		}
		events = append(events, Event{PaneDeleted, n.Pane.ID, ps.rec.Revision + 1, api.Deleted{}})
		s.commit(events)
	}
}

func (s *Store) paneSeen(hp HolderPane) {
	ps := s.panes[hp.ID]
	if ps == nil {
		s.panes[hp.ID] = &paneState{
			rec:      paneRecord{ID: hp.ID, Revision: 1, HolderBuild: hp.HolderBuild, Cols: hp.Cols, Rows: hp.Rows},
			holderID: hp.HolderID, offset: hp.Offset, exit: copyExit(hp.Exit),
		}
		s.commit([]Event{s.paneEvent(hp.ID)})
		return
	}
	ps.holderID = hp.HolderID
	ps.offset = max(ps.offset, hp.Offset)
	if ps.rec.Cols == hp.Cols && ps.rec.Rows == hp.Rows && ps.rec.HolderBuild == hp.HolderBuild {
		return
	}
	ps.rec.Cols, ps.rec.Rows, ps.rec.HolderBuild = hp.Cols, hp.Rows, hp.HolderBuild
	ps.rec.Revision++
	s.commit([]Event{s.paneEvent(hp.ID)})
}

// --- Snapshot ---

// Snapshot returns the whole structure. Pane offsets come fresh from the holders when they
// answer, and from the last notice otherwise. Every required array is [], never null.
func (s *Store) Snapshot(host api.HostInfo) api.Structure {
	offsets := map[string]uint64{}
	if live, err := s.holders.Panes(); err == nil {
		for _, hp := range live {
			offsets[hp.ID] = hp.Offset
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.Structure{Host: host}
	for _, p := range s.projects {
		out.Projects = append(out.Projects, projectAPI(p))
	}
	for _, ss := range s.sessions {
		out.Sessions = append(out.Sessions, sessionAPI(ss))
	}
	for _, id := range s.paneIDs() {
		pa := s.paneAPI(id)
		if off, ok := offsets[id]; ok && off > pa.Offset {
			pa.Offset = off
		}
		out.Panes = append(out.Panes, pa)
	}
	return api.NonNil(out)
}

// Unfiled returns the ids of live panes no session links, sorted.
func (s *Store) Unfiled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, id := range s.paneIDs() {
		if s.sessionByPane(id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// --- Internals (callers hold s.mu) ---

// commit saves the state, then emits the events in order. A failed save keeps the change in
// memory and is reported in Health; the next good save writes it.
func (s *Store) commit(events []Event) {
	if err := writeState(s.path, s.state()); err != nil {
		s.health.SaveError = err.Error()
	} else {
		s.health.SaveError = ""
	}
	for _, e := range events {
		s.onEvent(e)
	}
}

func (s *Store) state() *stateFile {
	st := &stateFile{Schema: SchemaVersion, Projects: []projectRecord{}, Sessions: []sessionRecord{}, Panes: []paneRecord{}}
	for _, p := range s.projects {
		cp := *p
		cp.Sessions = append([]string{}, p.Sessions...)
		st.Projects = append(st.Projects, cp)
	}
	for _, ss := range s.sessions {
		st.Sessions = append(st.Sessions, *ss)
	}
	for _, id := range s.paneIDs() {
		st.Panes = append(st.Panes, s.panes[id].rec)
	}
	return st
}

func (s *Store) paneIDs() []string {
	ids := make([]string, 0, len(s.panes))
	for id := range s.panes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *Store) project(id string) *projectRecord {
	if id == "" {
		return nil
	}
	for _, p := range s.projects {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (s *Store) session(id string) *sessionRecord {
	for _, ss := range s.sessions {
		if ss.ID == id {
			return ss
		}
	}
	return nil
}

func (s *Store) sessionByName(name string) *sessionRecord {
	for _, ss := range s.sessions {
		if ss.Name == name {
			return ss
		}
	}
	return nil
}

func (s *Store) sessionByPane(pane string) *sessionRecord {
	if pane == "" {
		return nil
	}
	for _, ss := range s.sessions {
		if ss.Pane == pane {
			return ss
		}
	}
	return nil
}

func (s *Store) projectEvent(p *projectRecord) Event {
	return Event{ProjectUpdated, p.ID, p.Revision, projectAPI(p)}
}

func (s *Store) sessionEvent(ss *sessionRecord) Event {
	return Event{SessionUpdated, ss.ID, ss.Revision, sessionAPI(ss)}
}

func (s *Store) paneEvent(id string) Event {
	pa := s.paneAPI(id)
	return Event{PaneUpdated, id, pa.Revision, pa}
}

func (s *Store) paneAPI(id string) api.Pane {
	ps := s.panes[id]
	pa := api.Pane{
		ID: id, Revision: ps.rec.Revision, HolderBuild: ps.rec.HolderBuild,
		Cols: ps.rec.Cols, Rows: ps.rec.Rows, Offset: ps.offset,
	}
	if ss := s.sessionByPane(id); ss != nil {
		pa.Session = ss.ID
	}
	return api.NonNil(pa)
}

func projectAPI(p *projectRecord) api.Project {
	return api.NonNil(api.Project{
		ID: p.ID, Revision: p.Revision, Name: p.Name, Remote: p.Remote, RootCommit: p.RootCommit,
		Sessions: slices.Clone(p.Sessions),
	})
}

func sessionAPI(ss *sessionRecord) api.Session {
	return api.NonNil(api.Session{
		ID: ss.ID, Revision: ss.Revision, Name: ss.Name, Project: ss.Project, Pane: ss.Pane,
		State: api.SessionState(ss.State), Exit: copyExit(ss.Exit),
	})
}

func copyExit(e *api.ExitStatus) *api.ExitStatus {
	if e == nil {
		return nil
	}
	cp := *e
	if e.Code != nil {
		c := *e.Code
		cp.Code = &c
	}
	return &cp
}

func insertAt(list []string, index int, id string) []string {
	if index < 0 || index > len(list) {
		index = len(list)
	}
	return slices.Insert(list, index, id)
}

// newID returns a random id with a kind prefix. Random ids cannot repeat even when the state
// file is lost, so an id is never reused.
func newID(kind string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("structure: crypto/rand failed: " + err.Error())
	}
	return kind + "-" + hex.EncodeToString(b[:])
}
