// Package fakehab is a hab for tests of clients that show many hosts: the real control server
// and structure store over a fake holder, with agent states the test sets. It has no data
// streams. Stop and Start take it down and bring it back with a new epoch and the same state
// directory, as a gff-hab restart does.
package fakehab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Hab is one fake hab on a Unix socket.
type Hab struct {
	Sock     string
	HostID   string
	DeviceID string
	// Capabilities are offered in the welcome beside agent-state. Set it, then Stop and Start.
	Capabilities []string

	holder *structure.FakeHolder
	dir    string

	mu       sync.Mutex
	store    *structure.Store
	srv      *control.Server
	served   chan struct{}
	agents   map[string]api.AgentState
	next     int
	hold     chan struct{} // while set, session.create waits for it to close
	closes   int           // pane.close calls
	resumes  int           // session.resume calls answered
	creates  []api.SessionCreateParams
	noResume error // set: session.resume answers it

	rounds roundBook            // rounds.go
	groups map[string]api.Group // groups.go
	grev   uint64
	cat    *api.CatalogList // what catalog.list answers; nil: unavailable
}

// New makes a hab with its socket and state under dir (keep it short: socket paths are limited
// to about 100 bytes) and starts it.
func New(dir, hostID string) (*Hab, error) {
	h := &Hab{
		Sock:     filepath.Join(dir, hostID+".sock"),
		HostID:   hostID,
		DeviceID: "SHA256:" + hostID,
		holder:   structure.NewFakeHolder(),
		dir:      filepath.Join(dir, hostID+".state"),
		agents:   map[string]api.AgentState{},
		rounds:   newRoundBook(),
		groups:   map[string]api.Group{},
	}
	return h, h.Start()
}

// Start brings the hab up: a new control server, so a new epoch, over the same state.
func (h *Hab) Start() error {
	var pub atomic.Pointer[control.Server]
	st, err := structure.Open(structure.Config{
		Dir:     h.dir,
		Holders: h.holder,
		OnEvent: func(e structure.Event) {
			if s := pub.Load(); s != nil {
				s.Publish(e)
			}
		},
	})
	if err != nil {
		return err
	}
	srv := control.New(control.Config{
		Build:        "test",
		Host:         api.HostInfo{ID: h.HostID},
		DeviceID:     h.DeviceID,
		Capabilities: append([]string{"agent-state"}, h.Capabilities...),
		Source:       source{h, st},
		Actions:      actions{h},
		Rounds:       roundsAPI{h},
		Catalog:      catalogAPI{h},
	})
	pub.Store(srv)
	os.Remove(h.Sock)
	l, err := control.Listen(h.Sock)
	if err != nil {
		st.Close()
		return err
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		srv.Serve(l)
	}()
	h.mu.Lock()
	h.store, h.srv, h.served = st, srv, served
	h.mu.Unlock()
	return nil
}

// Stop takes the hab down: its connections close and its socket goes away.
func (h *Hab) Stop() {
	h.mu.Lock()
	srv, st, served := h.srv, h.store, h.served
	h.srv, h.store = nil, nil
	h.mu.Unlock()
	if srv == nil {
		return
	}
	srv.Close()
	<-served
	st.Close()
	os.Remove(h.Sock)
}

// Epoch is the running server's epoch, empty while down.
func (h *Hab) Epoch() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.srv == nil {
		return ""
	}
	return h.srv.Epoch()
}

// AddSession starts a fake pane and files it as a session named name. It returns the session
// and pane ids.
func (h *Hab) AddSession(name string) (session, pane string, err error) {
	return h.create(name, "")
}

// AddProject makes a project identified by a git remote and returns its id.
func (h *Hab) AddProject(name, remote string) (string, error) {
	h.mu.Lock()
	st := h.store
	h.mu.Unlock()
	if st == nil {
		return "", errors.New("fakehab: down")
	}
	p, err := st.CreateProject(name, remote, "")
	return p.ID, err
}

// AddSessionIn is AddSession with the session filed in a project.
func (h *Hab) AddSessionIn(project, name string) (session, pane string, err error) {
	return h.create(name, project)
}

func (h *Hab) create(name, project string) (string, string, error) {
	return h.createRun(name, project, nil)
}

func (h *Hab) createRun(name, project string, round *api.SessionRound) (string, string, error) {
	h.mu.Lock()
	st := h.store
	h.next++
	pane := fmt.Sprintf("%s-p%d-%s", h.HostID, h.next, randHex())
	h.mu.Unlock()
	if st == nil {
		return "", "", errors.New("fakehab: down")
	}
	h.holder.Add(structure.HolderPane{ID: pane, HolderID: "h1", HolderBuild: "test", Cols: 80, Rows: 24})
	if name == "" {
		name = "sh"
		for i := 2; ; i++ {
			if _, taken := st.SessionByName(name); !taken {
				break
			}
			name = fmt.Sprintf("sh-%d", i)
		}
	}
	var s api.Session
	err := waitFor(func() error {
		var err error
		s, err = st.CreateSession(structure.SessionSpec{Name: name, Project: project, Pane: pane, Index: -1, Round: round})
		return err
	})
	if err != nil {
		return "", "", err
	}
	return s.ID, pane, nil
}

// waitFor retries f while the store has not yet seen a new pane.
func waitFor(f func() error) error {
	var err error
	for range 200 {
		if err = f(); err == nil || !errors.Is(err, structure.ErrInvalid) && !errors.Is(err, structure.ErrNotFound) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// SetAgent sets a pane's agent state and sends agent.state. kind is for blocked.
func (h *Hab) SetAgent(pane string, state api.AgentStatus, kind api.BlockedKind) {
	h.mu.Lock()
	a := h.agents[pane]
	a.Pane, a.Harness, a.State, a.Kind, a.Source = pane, "claude", state, kind, api.StateSource("hook")
	a.Revision++
	a.At = time.Now().UTC().Format(time.RFC3339Nano)
	h.agents[pane] = a
	srv := h.srv
	h.mu.Unlock()
	if srv != nil {
		srv.Publish(structure.Event{Type: "agent.state", ID: pane, Revision: a.Revision, Data: a})
	}
}

// HoldCreates makes session.create wait until release is called, so a test can act while a
// start is in flight.
func (h *Hab) HoldCreates() (release func()) {
	ch := make(chan struct{})
	h.mu.Lock()
	h.hold = ch
	h.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			h.hold = nil
			h.mu.Unlock()
			close(ch)
		})
	}
}

// ExitPane ends a pane's program with exit code 0, as when it quits on its own. The pane stays
// listed and its session becomes exited.
func (h *Hab) ExitPane(pane string) {
	code := 0
	h.holder.Exit(pane, api.ExitStatus{Code: &code})
}

// PaneCloses counts the pane.close calls the hab has answered.
func (h *Hab) PaneCloses() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closes
}

// Sessions returns the hab's session records, for test checks.
func (h *Hab) Sessions() []api.Session {
	h.mu.Lock()
	st := h.store
	h.mu.Unlock()
	if st == nil {
		return nil
	}
	return st.Snapshot(api.HostInfo{ID: h.HostID}).Sessions
}

type source struct {
	h  *Hab
	st *structure.Store
}

func (s source) Snapshot(host api.HostInfo) api.Structure {
	snap := s.st.Snapshot(host)
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	for _, p := range snap.Panes {
		if a, ok := s.h.agents[p.ID]; ok {
			snap.Agents = append(snap.Agents, a)
		}
	}
	slices.SortFunc(snap.Agents, func(a, b api.AgentState) int {
		switch {
		case a.Pane < b.Pane:
			return -1
		case a.Pane > b.Pane:
			return 1
		}
		return 0
	})
	snap.Rounds = s.h.rounds.list()
	snap.Groups = s.h.groupList()
	return snap
}

type actions struct{ h *Hab }

func (a actions) CreateSession(ctx context.Context, p api.SessionCreateParams) (string, string, error) {
	a.h.mu.Lock()
	a.h.creates = append(a.h.creates, p)
	hold := a.h.hold
	a.h.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	return a.h.create(p.Name, p.Project)
}

func (a actions) ClosePane(_ context.Context, pane string) error {
	a.h.mu.Lock()
	a.h.closes++
	a.h.mu.Unlock()
	a.h.holder.Remove(pane) // released: the pane is gone and its session exited
	return nil
}

// ResumeSession starts an exited session again in a new fake pane, as gff-hab's session.resume
// does: the old pane is released first.
func (a actions) ResumeSession(_ context.Context, session string) (string, error) {
	a.h.mu.Lock()
	st, fail := a.h.store, a.h.noResume
	a.h.resumes++
	a.h.next++
	pane := fmt.Sprintf("%s-p%d-%s", a.h.HostID, a.h.next, randHex())
	a.h.mu.Unlock()
	if fail != nil {
		return "", fail
	}
	ss, ok := st.Session(session)
	if !ok {
		return "", control.Errorf(api.ErrorKindNotFound, "session %s does not exist", session)
	}
	if ss.State != api.SessionStateExited {
		return "", control.Errorf(api.ErrorKindConflict, "session %s is running", session)
	}
	if ss.Pane != "" {
		a.h.holder.Remove(ss.Pane)
	}
	if _, err := st.ResumeSession(session, pane); err != nil {
		return "", kindOf(err)
	}
	a.h.holder.Add(structure.HolderPane{ID: pane, HolderID: "h1", HolderBuild: "test", Cols: 80, Rows: 24})
	return pane, nil
}

// Creates returns the session.create calls the hab got, in order.
func (h *Hab) Creates() []api.SessionCreateParams {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]api.SessionCreateParams(nil), h.creates...)
}

// Resumes counts the session.resume calls the hab has answered.
func (h *Hab) Resumes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resumes
}

// FailResumes makes session.resume answer err; nil answers again.
func (h *Hab) FailResumes(err error) {
	h.mu.Lock()
	h.noResume = err
	h.mu.Unlock()
}

// Session returns one session record.
func (h *Hab) Session(id string) (api.Session, bool) {
	h.mu.Lock()
	st := h.store
	h.mu.Unlock()
	if st == nil {
		return api.Session{}, false
	}
	return st.Session(id)
}

func (a actions) RenameSession(session, name string) error {
	a.h.mu.Lock()
	st := a.h.store
	a.h.mu.Unlock()
	_, err := st.RenameSession(session, name)
	return kindOf(err)
}

func (a actions) CloseSession(session string) error {
	a.h.mu.Lock()
	st := a.h.store
	a.h.mu.Unlock()
	return kindOf(st.CloseSession(session))
}

func kindOf(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, structure.ErrConflict):
		return control.Errorf(api.ErrorKindConflict, "%v", err)
	case errors.Is(err, structure.ErrNotFound):
		return control.Errorf(api.ErrorKindNotFound, "%v", err)
	}
	return control.Errorf(api.ErrorKindInvalid, "%v", err)
}

func randHex() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
