package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// Self-compact (harness-adapters.md, Self-compact): an agent asks, from its own pane, for its
// conversation to be compacted and continued. gff-hab sends /compact once the agent's turn
// ends, waits for the compaction to start and end, then sends the follow-up.

// The states of a compaction, in order.
const (
	compactQueued     = "queued"
	compactSent       = "sent"
	compactCompacting = "compacting"
	compactDone       = "done"
	compactFailed     = "failed"
)

// maxFollowUp bounds a follow-up, in characters, as a seed is bounded (capcom.md 3.3).
const maxFollowUp = 8000

// compactLimits are how long each step may take.
type compactLimits struct {
	turn   time.Duration // queued: the caller's turn must end
	start  time.Duration // sent: compaction must start
	end    time.Duration // compacting: compaction must end
	prompt time.Duration // after the end: the agent must be at its prompt for the follow-up
	poll   time.Duration // how often the transcript is read for compact_boundary
}

var defaultCompactLimits = compactLimits{turn: 60 * time.Minute, start: 2 * time.Minute, end: 15 * time.Minute,
	prompt: 2 * time.Minute, poll: time.Second}

// compactAgents is what the service needs of *harness.Store.
type compactAgents interface {
	Get(pane string) (api.AgentState, bool)
	Changed(pane string) <-chan struct{}
}

// compactService runs self-compacts, one per pane at most.
type compactService struct {
	agents compactAgents
	// session names the gff-hab session in a pane.
	session func(pane string) (string, bool)
	// send delivers text to a session through session.send's path.
	send    func(ctx context.Context, session, text string) error
	publish func(structure.Event)
	logf    func(string, ...any)
	limits  compactLimits
	now     func() time.Time

	mu    sync.Mutex
	jobs  map[string]*compactJob // pending, by pane
	paths map[string]string      // the transcript path each Claude pane's hooks named
}

// compactJob is one pending compaction.
type compactJob struct {
	c        api.Compaction
	followUp string
	armed    bool          // /compact went in: start and end signals count from now (under mu)
	started  chan struct{} // closed on the first start signal
	ended    chan struct{} // closed on the first end signal
	sOnce    sync.Once
	eOnce    sync.Once
}

func newCompactService(agents compactAgents, session func(string) (string, bool), send func(context.Context, string, string) error,
	publish func(structure.Event), logf func(string, ...any)) *compactService {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if publish == nil {
		publish = func(structure.Event) {}
	}
	return &compactService{agents: agents, session: session, send: send, publish: publish, logf: logf,
		limits: defaultCompactLimits, now: time.Now, jobs: map[string]*compactJob{}, paths: map[string]string{}}
}

// Request starts a compaction of pane. It checks the pane and answers at once with state
// queued; the rest runs on its own.
func (s *compactService) Request(pane string, p api.CompactParams) (api.Compaction, error) {
	if n := utf8.RuneCountInString(p.FollowUp); n > maxFollowUp {
		return api.Compaction{}, control.Errorf(api.ErrorKindInvalid, "the follow-up is %d characters; at most %d", n, maxFollowUp)
	}
	st, ok := s.agents.Get(pane)
	if !ok || st.State == "" || st.State == api.AgentStatusNone || st.State == api.AgentStatusExited {
		return api.Compaction{}, control.Errorf(api.ErrorKindUnavailable, "no supported harness runs in this pane; self-compact needs Claude Code")
	}
	if st.Harness != detect.KindClaude {
		return api.Compaction{}, control.Errorf(api.ErrorKindUnavailable, "self-compact is not built for %s yet; it supports Claude Code", st.Harness)
	}
	if st.Request != nil {
		return api.Compaction{}, control.Errorf(api.ErrorKindConflict, "a permission request is open in this pane; answer it first")
	}
	session, _ := s.session(pane)
	if session == "" {
		return api.Compaction{}, control.Errorf(api.ErrorKindInvalid, "this pane is not a session; self-compact runs in an agent's session")
	}
	s.mu.Lock()
	if j := s.jobs[pane]; j != nil {
		s.mu.Unlock()
		return api.Compaction{}, control.Errorf(api.ErrorKindConflict, "compaction %s is already pending for this pane", j.c.ID)
	}
	j := &compactJob{c: api.Compaction{ID: compactID(), Pane: pane, Session: session, Harness: st.Harness, State: compactQueued,
		FollowUp: p.FollowUp != "", At: s.stamp()}, followUp: p.FollowUp, started: make(chan struct{}), ended: make(chan struct{})}
	s.jobs[pane] = j
	c := j.c
	s.mu.Unlock()
	s.logf("pane %s: compaction %s queued", pane, c.ID)
	s.emit(c)
	go s.run(j)
	return c, nil
}

func (s *compactService) stamp() string { return s.now().UTC().Format(time.RFC3339Nano) }

func compactID() string {
	var b [8]byte
	rand.Read(b[:])
	return "cmp-" + hex.EncodeToString(b[:])
}

func (s *compactService) emit(c api.Compaction) {
	s.publish(structure.Event{Type: api.EventAgentCompact, ID: c.ID, Data: c})
}

// step moves j to state and sends the event.
func (s *compactService) step(j *compactJob, state, reason string) {
	s.mu.Lock()
	j.c.State, j.c.Reason, j.c.At = state, reason, s.stamp()
	c := j.c
	if state == compactDone || state == compactFailed {
		if s.jobs[c.Pane] == j {
			delete(s.jobs, c.Pane)
		}
	}
	s.mu.Unlock()
	if reason != "" {
		s.logf("pane %s: compaction %s %s: %s", c.Pane, c.ID, state, reason)
	} else {
		s.logf("pane %s: compaction %s %s", c.Pane, c.ID, state)
	}
	s.emit(c)
}

// Observe hears every Claude Code hook of a pane: the transcript path, and the start and end
// of a compaction.
func (s *compactService) Observe(pane string, env harness.Envelope) {
	if env.Harness != detect.KindClaude {
		return
	}
	var p struct {
		TranscriptPath string `json:"transcript_path"`
		Source         string `json:"source"`
		AgentID        string `json:"agent_id"`
	}
	if json.Unmarshal(env.Payload, &p) != nil || p.AgentID != "" {
		return
	}
	s.mu.Lock()
	if p.TranscriptPath != "" {
		s.paths[pane] = p.TranscriptPath
	}
	j := s.jobs[pane]
	armed := j != nil && j.armed
	s.mu.Unlock()
	if !armed {
		return // an auto-compaction mid-turn is not ours
	}
	switch {
	case env.Event == "PreCompact":
		j.start()
	case env.Event == "SessionStart" && p.Source == "compact":
		j.start()
		j.end()
	}
}

// Forget drops what the service knows of a pane that is gone.
func (s *compactService) Forget(pane string) {
	s.mu.Lock()
	delete(s.paths, pane)
	s.mu.Unlock()
}

func (j *compactJob) start() { j.sOnce.Do(func() { close(j.started) }) }
func (j *compactJob) end()   { j.eOnce.Do(func() { close(j.ended) }) }

// transcript is the pane's transcript path, or "".
func (s *compactService) transcript(pane string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paths[pane]
}

// errGone: the agent left the pane, or the pane exited.
var errGone = errors.New("the agent left the pane")

// waitAndSend waits until the agent in pane is at its prompt, with no request open, then tries
// send; a busy refusal waits for the next change. It gives up after limit. before, when not
// nil, runs before each try.
func (s *compactService) waitAndSend(j *compactJob, text string, limit time.Duration, what string, before func()) error {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	retry := time.NewTicker(time.Second)
	defer retry.Stop()
	for {
		changed := s.agents.Changed(j.c.Pane)
		st, ok := s.agents.Get(j.c.Pane)
		if !ok || st.State == api.AgentStatusExited || st.State == api.AgentStatusNone || st.Harness != j.c.Harness {
			return errGone
		}
		if (st.State == api.AgentStatusIdle || st.State == api.AgentStatusDone || st.State == api.AgentStatusError) && st.Request == nil {
			if before != nil {
				before()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := s.send(ctx, j.c.Session, text)
			cancel()
			if err == nil {
				return nil
			}
			var ce *control.Error
			if !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindConflict {
				return err
			}
			// Another send holds the pane, or the state moved: try again.
		}
		select {
		case <-changed:
		case <-retry.C:
		case <-deadline.C:
			return errors.New(what)
		}
	}
}

// run moves one compaction through its steps.
func (s *compactService) run(j *compactJob) {
	// queued: wait for the caller's turn to end, then send /compact.
	// Arm just before /compact goes in: a start or end signal from then on is ours, and the
	// transcript as it stands then is the offset a compact_boundary must come after.
	path, offset := "", int64(-1)
	arm := func() {
		s.mu.Lock()
		j.armed = true
		s.mu.Unlock()
		path, offset = s.transcript(j.c.Pane), -1
		if path != "" {
			offset = 0
			if fi, err := os.Stat(path); err == nil {
				offset = fi.Size()
			}
		}
	}
	if err := s.waitAndSend(j, "/compact", s.limits.turn, "the turn did not end within "+s.limits.turn.String(), arm); err != nil {
		s.step(j, compactFailed, "/compact was not sent: "+err.Error())
		return
	}
	s.step(j, compactSent, "")

	poll := time.NewTicker(s.limits.poll)
	defer poll.Stop()
	look := func() {
		if p := s.transcript(j.c.Pane); p != "" && p != path {
			path, offset = p, 0 // the first hook named it after the send
		}
		if path == "" || offset < 0 {
			return
		}
		if found, next := boundaryAfter(path, offset); found {
			j.start()
			j.end()
		} else {
			offset = next
		}
	}
	gone := func() bool {
		st, ok := s.agents.Get(j.c.Pane)
		return !ok || st.State == api.AgentStatusExited || st.Harness != j.c.Harness
	}

	startBy := time.NewTimer(s.limits.start)
	defer startBy.Stop()
wait:
	for {
		select {
		case <-j.started:
			break wait
		case <-poll.C:
			look()
			if gone() {
				s.step(j, compactFailed, "the agent left the pane before compaction started")
				return
			}
		case <-startBy.C:
			s.step(j, compactFailed, "compaction did not start within "+s.limits.start.String()+"; the follow-up was not sent")
			return
		}
	}
	s.step(j, compactCompacting, "")

	endBy := time.NewTimer(s.limits.end)
	defer endBy.Stop()
end:
	for {
		select {
		case <-j.ended:
			break end
		case <-poll.C:
			look()
			if gone() {
				s.step(j, compactFailed, "the agent left the pane during compaction; the follow-up was not sent")
				return
			}
		case <-endBy.C:
			s.step(j, compactFailed, "compaction did not end within "+s.limits.end.String()+"; the follow-up was not sent")
			return
		}
	}
	if j.followUp != "" {
		if err := s.waitAndSend(j, j.followUp, s.limits.prompt, "the agent was not at its prompt within "+s.limits.prompt.String(), nil); err != nil {
			s.step(j, compactFailed, "compaction ended, but the follow-up was not sent: "+err.Error())
			return
		}
	}
	s.step(j, compactDone, "")
}

// boundaryAfter reads the transcript from offset and reports whether a complete line after it
// is a compact_boundary record. It returns the offset of the first incomplete line, to read on
// from. It reads at most 8 MiB per call.
func boundaryAfter(path string, offset int64) (bool, int64) {
	f, err := os.Open(path)
	if err != nil {
		return false, offset
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, offset
	}
	if fi.Size() < offset {
		offset = 0 // replaced
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, offset
	}
	r := bufio.NewReaderSize(io.LimitReader(f, 8<<20), 64<<10)
	pos := offset
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return false, pos // an incomplete last line waits for its newline
		}
		pos += int64(len(line))
		if !bytes.Contains(line, []byte("compact_boundary")) {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Type == "system" && rec.Subtype == "compact_boundary" {
			return true, pos
		}
	}
}

// compactHook serves agent.compact on the pane route.
func compactHook(svc *compactService) hook.ChannelHandler {
	return func(ctx context.Context, pane, method string, params json.RawMessage) (any, func(error), error) {
		var p api.CompactParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, nil, control.Errorf(api.ErrorKindInvalid, "%s params: %v", method, err)
		}
		c, err := svc.Request(pane, p)
		return c, nil, err
	}
}
