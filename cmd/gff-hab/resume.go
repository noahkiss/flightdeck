package main

// session.resume for every session, not only Capcom's (gff-client.md 6.2). The hab remembers
// how each session started and the harness and native session id last seen in it, in
// <state>/resume.json, so a session whose pane is gone (a holder that died, a reboot) can start
// again in its folder: the harness with its resume id, or the command it was started with.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcom"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
)

// resumeFile holds the resume book in the state directory.
const resumeFile = "resume.json"

// resumeEntry is what the hab knows to start a session again.
type resumeEntry struct {
	// Started is how session.create started it: the harness, model and argv it was asked for.
	Harness string   `json:"harness,omitempty"`
	Model   string   `json:"model,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	// Seen is the harness last seen in the session's pane and its native session id, from the
	// adapters (agent.state). It wins over Started: a shell where the owner ran claude resumes
	// claude.
	Seen   string `json:"seen,omitempty"`
	Native string `json:"native,omitempty"`
}

type resumeFileV1 struct {
	Version  int                    `json:"version"`
	Sessions map[string]resumeEntry `json:"sessions"`
}

// resumeBook is the resume book. Agent states come in on the harness store's publish path, so
// noting one never blocks: a worker maps the pane to its session and writes the file.
type resumeBook struct {
	path string
	logf func(string, ...any)
	// sessionOf maps a pane to its session id.
	sessionOf func(pane string) (string, bool)

	applyMu sync.Mutex // preserve the order of pending states across worker and get
	saveMu  sync.Mutex // serialize snapshots and writes
	mu      sync.Mutex
	m       map[string]resumeEntry
	pend    map[string]api.AgentState // pane -> the latest state not yet applied
	gone    []string                  // sessions to forget
	kick    chan struct{}
	done    chan struct{}
	quit    chan struct{}
	saves   int // for tests
}

// openResumeBook reads the book from dir. A file that does not read is set aside and the book
// starts empty: losing it loses only the resume of sessions already exited.
func openResumeBook(dir string, sessionOf func(string) (string, bool), logf func(string, ...any)) *resumeBook {
	b := &resumeBook{path: filepath.Join(dir, resumeFile), logf: logf, sessionOf: sessionOf,
		m: map[string]resumeEntry{}, pend: map[string]api.AgentState{},
		kick: make(chan struct{}, 1), done: make(chan struct{}), quit: make(chan struct{})}
	data, err := os.ReadFile(b.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		logf("resume: %v", err)
	default:
		var f resumeFileV1
		if err := json.Unmarshal(data, &f); err != nil || f.Version != 1 {
			logf("resume: %s does not read; set aside", b.path)
			_ = os.Rename(b.path, b.path+".bad")
		} else if f.Sessions != nil {
			b.m = f.Sessions
		}
	}
	go b.run()
	return b
}

// Close writes what is pending and stops the worker.
func (b *resumeBook) Close() {
	close(b.quit)
	<-b.done
}

func (b *resumeBook) poke() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

func (b *resumeBook) run() {
	defer close(b.done)
	for {
		select {
		case <-b.kick:
			b.apply()
		case <-b.quit:
			b.apply()
			return
		}
	}
}

// apply maps pending states to sessions and writes the file when anything changed.
func (b *resumeBook) apply() {
	b.applyMu.Lock()
	defer b.applyMu.Unlock()
	b.mu.Lock()
	pend, gone := b.pend, b.gone
	b.pend, b.gone = map[string]api.AgentState{}, nil
	b.mu.Unlock()
	type note struct {
		session string
		st      api.AgentState
	}
	var notes []note
	for pane, st := range pend {
		if s, ok := b.sessionOf(pane); ok {
			notes = append(notes, note{s, st})
		}
	}
	b.mu.Lock()
	changed := false
	for _, n := range notes {
		e := b.m[n.session]
		seen, native := n.st.Harness, n.st.Session
		if native == "" && seen == e.Seen {
			native = e.Native // the id comes with a later state; keep the one known
		}
		if seen != e.Seen || native != e.Native {
			e.Seen, e.Native = seen, native
			b.m[n.session] = e
			changed = true
		}
	}
	for _, s := range gone {
		if _, ok := b.m[s]; ok {
			delete(b.m, s)
			changed = true
		}
	}
	b.mu.Unlock()
	if changed {
		b.save()
	}
}

func (b *resumeBook) save() {
	b.saveMu.Lock()
	defer b.saveMu.Unlock()
	b.mu.Lock()
	data, err := json.Marshal(resumeFileV1{Version: 1, Sessions: b.m})
	b.saves++
	b.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(b.path, data)
	}
	if err != nil {
		b.logf("resume: %v", err)
	}
}

// writeFileAtomic writes data to path through a temporary file and a rename, mode 0600.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// started notes how session.create started a session. It writes at once: the session is new.
func (b *resumeBook) started(session string, p api.SessionCreateParams) {
	b.mu.Lock()
	e := b.m[session]
	e.Harness, e.Model, e.Argv = p.Harness, p.Model, slices.Clone(p.Argv)
	b.m[session] = e
	b.mu.Unlock()
	b.save()
}

// agent notes an agent state. It never blocks: the worker maps the pane and writes. Only a
// harness with an adapter is kept; another detection id says nothing about resuming.
func (b *resumeBook) agent(st api.AgentState) {
	if !slices.Contains(harness.Harnesses, st.Harness) {
		return
	}
	b.mu.Lock()
	b.pend[st.Pane] = st
	b.mu.Unlock()
	b.poke()
}

// forget drops a session whose record is gone.
func (b *resumeBook) forget(session string) {
	b.mu.Lock()
	b.gone = append(b.gone, session)
	b.mu.Unlock()
	b.poke()
}

// get returns what the book knows of a session, with pending states applied.
func (b *resumeBook) get(session string) (resumeEntry, bool) {
	b.apply()
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[session]
	return e, ok
}

// resumeStart is how a session starts again: the harness with its resume id, else the start it
// was created with. ok is false when the book knows neither.
func resumeStart(e resumeEntry, claudeTranscript func(string) bool) (api.SessionCreateParams, bool, error) {
	if e.Seen != "" && e.Native != "" {
		model := ""
		if e.Seen == e.Harness {
			model = e.Model
		}
		var st capcom.Start
		var err error
		if e.Seen == detect.KindClaude && claudeTranscript != nil && !claudeTranscript(e.Native) {
			// Claude Code writes no transcript before the first message, and --resume of
			// such an id fails: start again under the same id.
			st, err = capcom.StartFor(capcom.Claude, e.Native)
		} else {
			st, err = capcom.ResumeFor(e.Seen, e.Native)
		}
		if err != nil {
			return api.SessionCreateParams{}, false, err
		}
		return api.SessionCreateParams{Harness: e.Seen, Model: model, Argv: st.Argv}, true, nil
	}
	if e.Harness != "" || len(e.Argv) > 0 {
		return api.SessionCreateParams{Harness: e.Harness, Model: e.Model, Argv: slices.Clone(e.Argv)}, true, nil
	}
	return api.SessionCreateParams{}, false, nil
}

// generalResumer serves session.resume for sessions that are not Capcom's.
type generalResumer struct {
	acts *actions
	book *resumeBook
	// folder is the session's working folder (the content service's), "" when not known.
	folder func(ctx context.Context, session string) string
	// complete reports whether every holder the state names has been reached.
	complete         func() bool
	claudeTranscript func(id string) bool

	op sync.Mutex // one resume at a time
}

// Resume starts an exited session again in a new pane of the same session. It refuses a
// running session, and an exited one whose pane may still run in a holder not reached yet.
func (g *generalResumer) Resume(ctx context.Context, session string) (string, error) {
	g.op.Lock()
	defer g.op.Unlock()
	ss, ok := g.acts.store.Session(session)
	if !ok {
		return "", control.Errorf(api.ErrorKindNotFound, "session %s does not exist", session)
	}
	if ss.State != api.SessionStateExited {
		return "", control.Errorf(api.ErrorKindConflict, "session %s is running", session)
	}
	if ss.Pane != "" && ss.Exit == nil && g.complete != nil && !g.complete() {
		return "", control.Errorf(api.ErrorKindUnavailable, "session %s: its holder is not connected; its pane may still run", session)
	}
	e, _ := g.book.get(session)
	p, ok, err := resumeStart(e, g.claudeTranscript)
	if err != nil {
		return "", control.Errorf(api.ErrorKindUnavailable, "%v", err)
	}
	if !ok {
		return "", control.Errorf(api.ErrorKindUnavailable, "session %s: how it started is not known", session)
	}
	cwd := ""
	if g.folder != nil {
		cwd = g.folder(ctx, session)
	}
	if cwd == "" {
		return "", control.Errorf(api.ErrorKindUnavailable, "session %s: its folder is not known", session)
	}
	return resumeExited(ctx, g.acts, session, p, cwd)
}
