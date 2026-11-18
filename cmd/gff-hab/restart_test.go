package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// restartRig is a restarter over a store with one Claude Code session in pane p1, a stub agent
// state, a stub signal that ends the pane on the signals named in exitOn, and a stub resume.
type restartRig struct {
	r       *restarter
	h       *structure.FakeHolder
	store   *structure.Store
	session string

	mu      sync.Mutex
	state   api.AgentState
	exitOn  []hp.Sig
	signals []hp.Sig
	resumed []string
	resumeE error
}

func newRestartRig(t *testing.T) *restartRig {
	t.Helper()
	g := &restartRig{h: structure.NewFakeHolder(structure.HolderPane{ID: "p1", HolderID: "h1", Cols: 80, Rows: 24}),
		state: api.AgentState{Pane: "p1", Harness: "claude", Session: "conv-1", State: api.AgentStatusIdle}, exitOn: []hp.Sig{hp.SigHUP}}
	st, err := structure.Open(structure.Config{Dir: t.TempDir(), Holders: g.h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	g.store = st
	ss, err := st.CreateSession(structure.SessionSpec{Name: "agent", Pane: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	g.session = ss.ID
	g.r = &restarter{store: st, logf: t.Logf, hupWait: 2 * time.Second, termWait: 2 * time.Second,
		agent: func(pane string) (api.AgentState, bool) {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.state, pane == g.state.Pane && g.state.Harness != ""
		},
		signal: func(ctx context.Context, pane string, sig hp.Sig) error {
			g.mu.Lock()
			g.signals = append(g.signals, sig)
			exit := slices.Contains(g.exitOn, sig)
			g.mu.Unlock()
			if exit {
				go g.h.Exit(pane, api.ExitStatus{})
			}
			return nil
		},
		resume: func(ctx context.Context, session string) (string, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			if ss, _ := st.Session(session); ss.State != api.SessionStateExited {
				return "", errors.New("resumed a running session")
			}
			g.resumed = append(g.resumed, session)
			if g.resumeE != nil {
				return "", g.resumeE
			}
			return "p2", nil
		}}
	return g
}

func (g *restartRig) sent() []hp.Sig {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.signals)
}

// TestRestartRefused: a restart while the agent works, waits on a request, or reports no
// harness or no id is refused, and so is one of a session that is not running, a watcher, an
// unknown id, one that cannot start again, and a second restart while one runs. A refused
// restart sends no signal and starts nothing.
func TestRestartRefused(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		st   api.AgentState
		want api.ErrorKind
	}{
		{"working", api.AgentState{Pane: "p1", Harness: "claude", Session: "conv-1", State: api.AgentStatusWorking}, api.ErrorKindConflict},
		{"blocked", api.AgentState{Pane: "p1", Harness: "codex", Session: "conv-1", State: api.AgentStatusBlocked}, api.ErrorKindConflict},
		{"no agent", api.AgentState{}, api.ErrorKindUnavailable},
		{"no adapter", api.AgentState{Pane: "p1", Harness: "aider", Session: "x", State: api.AgentStatusIdle}, api.ErrorKindUnavailable},
		{"no id", api.AgentState{Pane: "p1", Harness: "claude", State: api.AgentStatusDone}, api.ErrorKindUnavailable},
	} {
		g := newRestartRig(t)
		g.state = c.st
		if _, err := g.r.Restart(ctx, g.session); errKind(err) != c.want {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
		if len(g.sent()) != 0 || len(g.resumed) != 0 {
			t.Errorf("%s: signals %v, resumed %v", c.name, g.sent(), g.resumed)
		}
	}

	g := newRestartRig(t)
	if _, err := g.r.Restart(ctx, "nope"); errKind(err) != api.ErrorKindNotFound {
		t.Errorf("unknown session: %v", err)
	}
	g.r.ready = func(context.Context, string, api.AgentState) error {
		return control.Errorf(api.ErrorKindUnavailable, "its folder is not known")
	}
	if _, err := g.r.Restart(ctx, g.session); errKind(err) != api.ErrorKindUnavailable {
		t.Errorf("not ready: %v", err)
	}
	g.r.ready = nil
	g.r.direct = func(context.Context, string) (bool, error) { return false, nil }
	if _, err := g.r.Restart(ctx, g.session); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "shell") {
		t.Errorf("under a shell: %v", err)
	}
	g.r.direct = nil
	// The agent takes a prompt between the check and the hang-up.
	calls := 0
	agent := g.r.agent
	g.r.agent = func(pane string) (api.AgentState, bool) {
		st, ok := agent(pane)
		if calls++; calls > 1 {
			st.State = api.AgentStatusWorking
		}
		return st, ok
	}
	if _, err := g.r.Restart(ctx, g.session); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "became working") {
		t.Errorf("became busy: %v", err)
	}
	g.r.agent = agent
	g.r.op.Lock()
	if _, err := g.r.Restart(ctx, g.session); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "one session at a time") {
		t.Errorf("a second restart: %v", err)
	}
	g.r.op.Unlock()
	if len(g.sent()) != 0 {
		t.Fatalf("signals %v", g.sent())
	}

	w, err := g.store.CreateSession(structure.SessionSpec{Name: "watch", NoPane: true, Round: &api.SessionRound{Round: "r", Watcher: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Restart(ctx, w.ID); errKind(err) != api.ErrorKindConflict {
		t.Errorf("watcher: %v", err)
	}
	g.h.Exit("p1", api.ExitStatus{})
	if _, err := g.r.Restart(ctx, g.session); errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "session.resume") {
		t.Errorf("exited: %v", err)
	}
}

// TestRestartExitsThenResumes: an idle agent gets a hang-up, and session.resume's path runs
// only once its program ended. One that ignores the hang-up gets a TERM. One that outlives both
// keeps running, and nothing starts. A start that fails after the exit says the conversation is
// untouched and keeps its error kind.
func TestRestartExitsThenResumes(t *testing.T) {
	ctx := context.Background()
	g := newRestartRig(t)
	var heard []string
	g.r.restarted = func(session string, at time.Time) { heard = append(heard, session) }
	pane, err := g.r.Restart(ctx, g.session)
	if err != nil || pane != "p2" || !slices.Equal(heard, []string{g.session}) {
		t.Fatalf("restart: %q %v, heard %v", pane, err, heard)
	}
	if !slices.Equal(g.sent(), []hp.Sig{hp.SigHUP}) || !slices.Equal(g.resumed, []string{g.session}) {
		t.Fatalf("signals %v, resumed %v", g.sent(), g.resumed)
	}

	g = newRestartRig(t)
	g.state.State = api.AgentStatusDone
	g.exitOn = []hp.Sig{hp.SigTERM}
	g.r.hupWait = 100 * time.Millisecond
	if _, err := g.r.Restart(ctx, g.session); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.sent(), []hp.Sig{hp.SigHUP, hp.SigTERM}) || len(g.resumed) != 1 {
		t.Fatalf("escalation: signals %v, resumed %v", g.sent(), g.resumed)
	}

	g = newRestartRig(t)
	g.exitOn = nil
	g.r.hupWait, g.r.termWait = 100*time.Millisecond, 100*time.Millisecond
	if _, err := g.r.Restart(ctx, g.session); errKind(err) != api.ErrorKindUnavailable || !strings.Contains(err.Error(), "still runs") {
		t.Fatalf("no exit: %v", err)
	}
	if ss, _ := g.store.Session(g.session); ss.State != api.SessionStateRunning || len(g.resumed) != 0 {
		t.Fatalf("after no exit: %+v, resumed %v", ss, g.resumed)
	}

	g = newRestartRig(t)
	g.resumeE = control.Errorf(api.ErrorKindConflict, "the conversation runs outside the hab")
	_, err = g.r.Restart(ctx, g.session)
	if errKind(err) != api.ErrorKindConflict || !strings.Contains(err.Error(), "conv-1 is untouched") {
		t.Fatalf("failed start: %v", err)
	}
	if ss, _ := g.store.Session(g.session); ss.State != api.SessionStateExited {
		t.Fatalf("after a failed start: %+v", ss)
	}
}

// TestSessionRestart: a real holder and gff-hab, a stand-in claude on gff-hab's PATH and a
// hand-written transcript. session.restart of the idle agent hangs it up and starts
// `claude --resume <id>` in a new pane of the same session and folder; the transcript is
// byte-identical; the session runs again under the same id and name.
func TestSessionRestart(t *testing.T) {
	e := newHolderEnv(t, 0)
	fake := shortDir(t)
	writeFile(t, filepath.Join(fake, "claude"), fakeResumable)
	if err := os.Chmod(filepath.Join(fake, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfgDir := shortDir(t)
	log := filepath.Join(e.work, "argv.log")
	const native = "3c9d51e0-restart-test"
	path := filepath.Join(cfgDir, "projects", "w", native+".jsonl")
	body := "{\"type\":\"user\",\"sessionId\":\"" + native + "\"}\n"
	adoptWrite(t, path, body)
	e.env = []string{"PATH=" + fake + string(os.PathListSeparator) + os.Getenv("PATH"), "CLAUDE_CONFIG_DIR=" + cfgDir,
		"CODEX_HOME=" + shortDir(t), "RESUME_LOG=" + log, "RESUME_HAB=" + bin, "RESUME_ID=" + native}
	e.serve()
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(40*time.Second))
	defer cancel()
	var made api.SessionCreated
	if err := c.Call(ctx, "session.create", api.SessionCreateParams{Name: "agent", Harness: "claude", Cwd: e.work}, &made); err != nil {
		t.Fatal(err)
	}
	lines := func() int { b, _ := os.ReadFile(log); return strings.Count(string(b), "\n") }
	waitUntil(t, "claude started", func() bool { return lines() == 1 })
	waitUntil(t, "the agent idle with its id", func() bool {
		var st api.StructureResult
		if err := c.Call(ctx, "structure.get", api.StructureGetParams{}, &st); err != nil {
			t.Fatal(err)
		}
		for _, a := range st.Agents {
			if a.Pane == made.Pane {
				return a.Session == native && (a.State == api.AgentStatusIdle || a.State == api.AgentStatusDone)
			}
		}
		return false
	})
	var res api.SessionCreated
	if err := c.Call(ctx, "session.restart", api.SessionRef{Session: made.Session}, &res); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if res.Session != made.Session || res.Pane == "" || res.Pane == made.Pane {
		t.Fatalf("restart: %+v, first pane %s", res, made.Pane)
	}
	ss := waitSession(t, c, made.Session, "running again", func(s api.Session) bool { return s.State == api.SessionStateRunning && s.Pane == res.Pane })
	if ss.Name != "agent" {
		t.Fatalf("session after restart: %+v", ss)
	}
	waitUntil(t, "claude started again", func() bool { return lines() == 2 })
	if got := readLines(t, log)[1]; got != "claude --resume "+native {
		t.Fatalf("restart start %q", got)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != body {
		t.Fatalf("transcript changed: %q %v", b, err)
	}
}
