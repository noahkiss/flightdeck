package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/channels"
)

// Teammate reports (harness-adapters.md, Spawn): a session started with report_to sends its
// spawner the final agent message of each turn that the spawner began, with the seed or a
// later message, as a channel message from the new session. This hab watches its own sessions
// only: the report goes from the hab that runs the teammate, so the spawner may be on a peer.

const (
	teammatesFile = "teammates.json"
	// maxTeammates bounds the file; the oldest entries go first.
	maxTeammates = 1000
	// reportRead bounds the read of a turn's final message: the transcript may trail the turn.
	reportRead = 15 * time.Second
)

// teammate is one session that reports, as saved.
type teammate struct {
	Session string         `json:"session"` // the session id on this hab
	Pane    string         `json:"pane"`    // its pane now
	To      api.ChannelEnd `json:"to"`      // the spawner: device id, host id and session name
	Added   time.Time      `json:"added"`

	// In memory: a turn the spawner began.
	armed   bool   // a message from the spawner was written, or is being written, to the agent
	rev     uint64 // the agent's revision before that write
	started bool   // the agent worked after the write
}

type teammatesFileV1 struct {
	Version int         `json:"version"`
	Mates   []*teammate `json:"mates"`
}

// teammates keeps the sessions that report and watches their turns.
type teammates struct {
	path string
	logf func(string, ...any)
	// agent returns a pane's agent state now.
	agent func(pane string) (api.AgentState, bool)
	// name returns a session's name on this hab by id.
	name func(session string) (string, bool)
	// read reads a session's conversation.
	read func(ctx context.Context, session, harness, native string) ([]api.ChatMessage, error)
	// send sends a channel message from a session on this hab, by name, to an end.
	send func(ctx context.Context, from string, to api.ChannelEnd, text string) error
	// readFor replaces reportRead; tests shorten it.
	readFor time.Duration

	mu     sync.Mutex
	byID   map[string]*teammate
	byPane map[string]string // pane -> session id
	closed bool
	wg     sync.WaitGroup
	ctx    context.Context // ends at Close: a report on its way stops
	stop   context.CancelFunc
}

func openTeammates(dir string, logf func(string, ...any)) *teammates {
	t := &teammates{path: filepath.Join(dir, teammatesFile), logf: logf, byID: map[string]*teammate{}, byPane: map[string]string{}}
	t.ctx, t.stop = context.WithCancel(context.Background())
	b, err := os.ReadFile(t.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		logf("teammates: %v", err)
	default:
		var f teammatesFileV1
		if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 {
			logf("teammates: %s does not read; set aside", t.path)
			_ = os.Rename(t.path, t.path+".bad")
			break
		}
		for _, m := range f.Mates {
			if m != nil && m.Session != "" {
				t.byID[m.Session] = m
				t.byPane[m.Pane] = m.Session
			}
		}
	}
	return t
}

// Close stops the reports on their way and waits for them. A turn that ends while gff-hab is
// down is not reported.
func (t *teammates) Close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.stop()
	t.wg.Wait()
}

// add files a new session that reports to its spawner.
func (t *teammates) add(session, pane string, to api.ChannelEnd) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.byID[session]; ok {
		delete(t.byPane, old.Pane)
	}
	t.byID[session] = &teammate{Session: session, Pane: pane, To: to, Added: time.Now().UTC()}
	t.byPane[pane] = session
	t.pruneLocked()
	t.saveLocked()
}

// repane follows a resumed session to its new pane.
func (t *teammates) repane(session, pane string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.byID[session]
	if !ok {
		return
	}
	delete(t.byPane, m.Pane)
	m.Pane, m.armed = pane, false
	t.byPane[pane] = session
	t.saveLocked()
}

// forget drops a removed session.
func (t *teammates) forget(session string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.byID[session]
	if !ok {
		return
	}
	delete(t.byID, session)
	delete(t.byPane, m.Pane)
	t.saveLocked()
}

// pruneLocked drops the oldest entries past the cap. A removed session goes by forget; one
// removed while gff-hab was down stays until the cap, and never reports, having no pane.
func (t *teammates) pruneLocked() {
	for len(t.byID) > maxTeammates {
		var oldest *teammate
		for _, m := range t.byID {
			if oldest == nil || m.Added.Before(oldest.Added) {
				oldest = m
			}
		}
		delete(t.byID, oldest.Session)
		delete(t.byPane, oldest.Pane)
	}
}

func (t *teammates) saveLocked() {
	f := teammatesFileV1{Version: 1, Mates: []*teammate{}}
	for _, m := range t.byID {
		f.Mates = append(f.Mates, m)
	}
	b, err := json.Marshal(f)
	if err == nil {
		err = writeFileAtomic(t.path, b)
	}
	if err != nil {
		t.logf("teammates: save: %v", err)
	}
}

// delivering is channels' Delivering hook: a seed, or a message from the spawner, arms the
// session's next turn. The agent's revision now marks where that turn starts.
func (t *teammates) delivering(session string, from api.ChannelEnd, seed bool) func(bool) {
	t.mu.Lock()
	m, ok := t.byID[session]
	if !ok || !(seed || (from.DeviceID == m.To.DeviceID && from.Session == m.To.Session)) {
		t.mu.Unlock()
		return nil
	}
	pane := m.Pane
	t.mu.Unlock()
	// The harness store calls agentState with its own lock held, so its state is read here
	// with t.mu released. The write comes after this returns, so no state of the turn is lost.
	var rev uint64
	if t.agent != nil {
		if st, ok := t.agent(pane); ok {
			rev = st.Revision
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.byID[session]; !ok || cur != m || m.Pane != pane {
		return nil
	}
	m.armed, m.rev, m.started = true, rev, false
	return func(delivered bool) {
		if delivered {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if cur, ok := t.byID[session]; ok && cur == m && m.rev == rev {
			m.armed = false
		}
	}
}

// agentState takes one agent.state of any pane. The harness store calls it with its lock held,
// so it only takes t.mu and hands the report off its goroutine.
func (t *teammates) agentState(st api.AgentState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.byID[t.byPane[st.Pane]]
	if m == nil || !m.armed || st.Revision <= m.rev || t.closed {
		return
	}
	switch st.State {
	case api.AgentStatusWorking, api.AgentStatusBlocked:
		m.started = true
	case api.AgentStatusDone, api.AgentStatusError:
		if !m.started {
			return
		}
		m.armed = false
		t.reportLocked(m, st)
	case api.AgentStatusExited:
		m.armed = false
		t.reportLocked(m, st)
	}
}

func (t *teammates) reportLocked(m *teammate, st api.AgentState) {
	session, to := m.Session, m.To
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.report(session, to, st)
	}()
}

// report reads the turn's final message and sends it to the spawner.
func (t *teammates) report(session string, to api.ChannelEnd, st api.AgentState) {
	name, ok := t.name(session)
	if !ok {
		return
	}
	wait := reportRead
	if t.readFor > 0 {
		wait = t.readFor
	}
	ctx, cancel := context.WithTimeout(t.ctx, wait)
	defer cancel()
	var final string
	var readErr error
	if st.State != api.AgentStatusExited {
		final, readErr = readResult(ctx, func(ctx context.Context) ([]api.ChatMessage, error) {
			return t.read(ctx, session, st.Harness, st.Session)
		})
	}
	text := reportText(st, final, readErr)
	if t.ctx.Err() != nil {
		return
	}
	sctx, scancel := context.WithTimeout(t.ctx, 10*time.Second)
	defer scancel()
	if err := t.send(sctx, name, to, text); err != nil {
		t.logf("teammates: %s: report to %s/%s: %v", name, to.Host, to.Session, err)
	}
}

// reportText is a teammate's report: a first line from Flightdeck, then the final message, cut
// to fit one channel message.
func reportText(st api.AgentState, final string, readErr error) string {
	var head string
	switch {
	case st.State == api.AgentStatusExited:
		return "(Flightdeck: my session exited before the turn your message began ended. There is no report.)"
	case readErr != nil:
		head = fmt.Sprintf("(Flightdeck: my turn ended, but my final message could not be read: %v. Read my transcript with transcript_read.)", readErr)
		if st.State == api.AgentStatusError {
			head = fmt.Sprintf("(Flightdeck: my turn ended with an error%s, and my final message could not be read: %v.)", errorKind(st), readErr)
		}
		return head
	case st.State == api.AgentStatusError:
		head = fmt.Sprintf("(Flightdeck: my turn ended with an error%s. My last message follows.)", errorKind(st))
	default:
		head = "(Flightdeck: my turn ended. My final message follows.)"
	}
	room := channels.MaxText - len(head) - len("\n\n") - len(" [cut]")
	if len(final) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(final[cut]) {
			cut--
		}
		final = final[:cut] + " [cut]"
	}
	return head + "\n\n" + final
}

func errorKind(st api.AgentState) string {
	if st.Error == "" {
		return ""
	}
	return " (" + st.Error + ")"
}
