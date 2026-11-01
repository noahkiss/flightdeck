package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
	"github.com/noahkiss/flightdeck/internal/harness"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/opencode"
)

// opencodeDir is the folder in the state directory that records each opencode pane's server.
const opencodeDir = "opencode"

// opencodePasswordLabel separates opencode server passwords from any other use of the hook
// secret.
const opencodePasswordLabel = "gff-hab opencode password v1\x00"

// opencodeChatEvents are the server events after which a session's messages changed: the
// chat view fetches them again.
var opencodeChatEvents = []string{
	"session.inbox.delivered", "session.step.ended", "session.text.ended", "session.tool.called",
	"session.tool.success", "session.tool.failed", "session.compaction.ended", "session.compaction.failed",
	"session.shell.started", "session.shell.ended", "session.skill.activated", "session.agent.selected",
	"session.model.selected",
}

// opencodeRetry bounds the wait between two attempts to reach a pane's server.
const (
	opencodeRetryMin = 250 * time.Millisecond
	opencodeRetryMax = 10 * time.Second
)

// opencodeServers follows the private opencode server of each opencode pane gff-hab started
// (harness-adapters.md, opencode, built choices). For each it keeps one event stream open and
// applies its state events to the agent store with source server; it serves the client that
// session.send, approval.answer and the chat view use. A pane's record, <state>/opencode/<pane
// uuid>.json, holds the server's URL and the session last bound, so a restarted gff-hab finds
// the server again; the password is derived from the hook secret, never stored.
type opencodeServers struct {
	dir    string
	secret []byte
	agents *harness.Store
	logf   func(string, ...any)
	// dirty, if set, hears that a pane's conversation changed (the chat view).
	dirty func(pane string)

	mu    sync.Mutex
	panes map[string]*ocPane
}

// ocPane is one pane's server and what the adapter learned from it.
type ocPane struct {
	url     string
	client  *opencode.Client
	cancel  context.CancelFunc
	done    chan struct{}
	version string

	mu       sync.Mutex
	session  string            // the session last bound, as recorded
	children map[string]bool   // session id: has a parent
	running  map[string]bool   // sessions whose agent loop runs
	requests map[string]string // permission request id: its session
	docOK    bool              // the OpenAPI document was checked
}

// ocRecord is a pane's record file.
type ocRecord struct {
	URL     string `json:"url"`
	Session string `json:"session,omitempty"`
}

func newOpencodeServers(state string, secret []byte, agents *harness.Store, logf func(string, ...any)) *opencodeServers {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &opencodeServers{dir: filepath.Join(state, opencodeDir), secret: append([]byte(nil), secret...), agents: agents,
		logf: logf, panes: map[string]*ocPane{}}
}

// password is the opencode server password of the pane with this uuid.
func (o *opencodeServers) password(u hp.UUID) string {
	m := hmac.New(sha256.New, o.secret)
	m.Write([]byte(opencodePasswordLabel))
	m.Write(u[:])
	return hex.EncodeToString(m.Sum(nil))
}

// freePort asks the system for a free loopback port. Another program may take it before the
// pane's server binds it; the wrapper then fails visibly in the pane.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (o *opencodeServers) recordPath(pane string) string {
	return filepath.Join(o.dir, pane+".json")
}

func (o *opencodeServers) writeRecord(pane string, r ocRecord) error {
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	tmp, err := os.CreateTemp(o.dir, ".rec-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), o.recordPath(pane)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// register records a new pane's server and starts following it.
func (o *opencodeServers) register(pane string, u hp.UUID, url string) error {
	if err := o.writeRecord(pane, ocRecord{URL: url}); err != nil {
		return fmt.Errorf("record the opencode server of pane %s: %w", pane, err)
	}
	return o.follow(pane, u, ocRecord{URL: url})
}

// load follows the server of every recorded pane that live says the link still follows, and
// removes the record of every other one.
func (o *opencodeServers) load(live func(pane string) (hp.UUID, bool)) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		pane, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(pane, ".") {
			continue
		}
		u, ok := live(pane)
		if !ok {
			os.Remove(filepath.Join(o.dir, e.Name()))
			continue
		}
		b, err := os.ReadFile(filepath.Join(o.dir, e.Name()))
		var r ocRecord
		if err == nil {
			err = json.Unmarshal(b, &r)
		}
		if err != nil {
			o.logf("opencode: pane %s: unreadable record: %v", pane, err)
			continue
		}
		if err := o.follow(pane, u, r); err != nil {
			o.logf("opencode: pane %s: %v", pane, err)
		}
	}
}

func (o *opencodeServers) follow(pane string, u hp.UUID, r ocRecord) error {
	c, err := opencode.New(r.URL, o.password(u))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &ocPane{url: r.URL, client: c, cancel: cancel, done: make(chan struct{}), session: r.Session,
		children: map[string]bool{}, running: map[string]bool{}, requests: map[string]string{}}
	o.mu.Lock()
	if old := o.panes[pane]; old != nil {
		old.cancel()
	}
	o.panes[pane] = p
	o.mu.Unlock()
	go o.watch(ctx, pane, p)
	return nil
}

// forget stops following a pane that is gone and removes its record.
func (o *opencodeServers) forget(pane string) {
	o.mu.Lock()
	p := o.panes[pane]
	delete(o.panes, pane)
	o.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	os.Remove(o.recordPath(pane))
}

// Close stops every stream and waits for them.
func (o *opencodeServers) Close() {
	o.mu.Lock()
	ps := make([]*ocPane, 0, len(o.panes))
	for _, p := range o.panes {
		ps = append(ps, p)
	}
	o.mu.Unlock()
	for _, p := range ps {
		p.cancel()
		<-p.done
	}
}

func (o *opencodeServers) pane(pane string) *ocPane {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.panes[pane]
}

// Client returns the client of a pane's server, if gff-hab started the pane's opencode.
func (o *opencodeServers) Client(pane string) (*opencode.Client, bool) {
	if o == nil {
		return nil, false
	}
	p := o.pane(pane)
	if p == nil {
		return nil, false
	}
	return p.client, true
}

// requestSession returns the session a permission request of the pane belongs to: a child
// session's request is answered under the child.
func (o *opencodeServers) requestSession(pane, request string) string {
	p := o.pane(pane)
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[request]
}

// version returns the version the pane's server reported, if it was reached.
func (o *opencodeServers) version(pane string) string {
	p := o.pane(pane)
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.version
}

// watch keeps the pane's event stream open until ctx ends, reconnecting with a growing wait.
func (o *opencodeServers) watch(ctx context.Context, pane string, p *ocPane) {
	defer close(p.done)
	types := append(append([]string(nil), harness.OpencodeStateEvents...), opencodeChatEvents...)
	wait := opencodeRetryMin
	var lastErr string
	for {
		start := time.Now()
		err := p.client.Events(ctx, types, func() { o.resync(ctx, pane, p) }, func(e opencode.Event) { o.event(ctx, pane, p, e) })
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			wait = opencodeRetryMin // it was up a while: this is a new failure
		}
		if msg := err.Error(); msg != lastErr {
			o.logf("opencode: pane %s: %v; trying again", pane, err)
			lastErr = msg
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, opencodeRetryMax)
	}
}

// resync reads the server's state when the stream (re)connects: the version, the document's
// operations (once), the sessions that run, and the open permission requests. Events from now
// on arrive on the stream, so nothing between the read and the stream is lost.
func (o *opencodeServers) resync(ctx context.Context, pane string, p *ocPane) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if info, err := p.client.Info(ctx); err == nil {
		p.mu.Lock()
		changed := p.version != info.Version
		p.version = info.Version
		p.mu.Unlock()
		if changed && detect.CheckVersion(detect.KindOpencode, info.Version) != nil {
			o.logf("opencode: pane %s: server version %s is outside the tested %s; the adapter goes on", pane, info.Version, opencode.TestedVersion)
		}
	}
	p.mu.Lock()
	docOK := p.docOK
	p.mu.Unlock()
	if !docOK {
		if doc, err := p.client.Doc(ctx); err == nil {
			if err := opencode.CheckDoc(doc); err != nil {
				o.logf("opencode: pane %s: error: %v; sends, answers or the chat view may fail", pane, err)
			}
			p.mu.Lock()
			p.docOK = true
			p.mu.Unlock()
		}
	}
	now := time.Now().UnixMilli()
	active, err := p.client.Active(ctx)
	if err != nil {
		o.logf("opencode: pane %s: %v", pane, err)
		return
	}
	p.mu.Lock()
	clear(p.running)
	for _, id := range active {
		p.running[id] = true
	}
	recorded := p.session
	p.mu.Unlock()
	bound := ""
	if st, ok := o.agents.Get(pane); ok && st.Harness == detect.KindOpencode {
		bound = st.Session
	}
	// A running top-level session is the pane's; else the one recorded before a restart.
	target := ""
	for _, id := range active {
		if !o.child(ctx, p, id) {
			target = id
			break
		}
	}
	state := api.AgentStatusWorking
	if target == "" {
		target, state = firstNonEmpty(bound, recorded), api.AgentStatusIdle
	}
	if target != "" && (target != bound || state == api.AgentStatusWorking) {
		o.apply(pane, p, now, harness.Update{Session: target, Bind: target != bound, State: state})
	} else if target != "" && bound == target {
		// The stream was down: a turn may have ended meanwhile.
		if st, _ := o.agents.Get(pane); st.State == api.AgentStatusWorking {
			o.apply(pane, p, now, harness.Update{Session: target, State: api.AgentStatusIdle})
		}
	}
	perms, err := p.client.Permissions(ctx)
	if err != nil {
		o.logf("opencode: pane %s: %v", pane, err)
		return
	}
	for _, raw := range perms {
		var d opencode.Permission
		if json.Unmarshal(raw, &d) != nil || d.ID == "" {
			continue
		}
		o.event(ctx, pane, p, opencode.Event{Type: "permission.asked", Created: now, Data: raw})
	}
}

// child reports whether a session has a parent session, asking the server on first sight.
func (o *opencodeServers) child(ctx context.Context, p *ocPane, id string) bool {
	p.mu.Lock()
	c, ok := p.children[id]
	p.mu.Unlock()
	if ok {
		return c
	}
	s, err := p.client.Session(ctx, id)
	if err != nil {
		return false // unknown: taken as top-level, as the TUI's own sessions are
	}
	p.mu.Lock()
	p.children[id] = s.ParentID != ""
	p.mu.Unlock()
	return s.ParentID != ""
}

// event applies one server event.
func (o *opencodeServers) event(ctx context.Context, pane string, p *ocPane, e opencode.Event) {
	if o.dirty != nil {
		o.dirty(pane)
	}
	if !isStateEvent(e.Type) {
		return
	}
	sid := e.SessionID()
	if e.Type == "session.created" {
		var d struct {
			ParentID string `json:"parentID"`
		}
		json.Unmarshal(e.Data, &d)
		p.mu.Lock()
		p.children[sid] = d.ParentID != ""
		p.mu.Unlock()
	}
	s := harness.OpencodeSession{}
	if sid != "" {
		s.Child = o.child(ctx, p, sid)
	}
	p.mu.Lock()
	switch e.Type {
	case "session.execution.started":
		p.running[sid] = true
	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted":
		delete(p.running, sid)
	case "permission.asked":
		var d opencode.Permission
		if json.Unmarshal(e.Data, &d) == nil && d.ID != "" {
			if len(p.requests) >= 256 {
				clear(p.requests)
			}
			p.requests[d.ID] = d.SessionID
		}
	}
	s.Running = p.running[sid]
	p.mu.Unlock()
	u, err := harness.NormaliseOpencodeServer(harness.ServerEvent{Type: e.Type, Data: e.Data, Dir: e.Location.Directory}, s)
	if err != nil {
		if !errors.Is(err, harness.ErrUnknownEvent) {
			o.logf("opencode: pane %s: event %s dropped: %v", pane, e.Type, err)
		}
		return
	}
	t := e.Created
	if t == 0 {
		t = time.Now().UnixMilli()
	}
	o.apply(pane, p, t, u)
}

// apply applies an update with source server and records the bound session when it changed.
func (o *opencodeServers) apply(pane string, p *ocPane, t int64, u harness.Update) {
	o.agents.Apply(pane, detect.KindOpencode, api.StateSourceServer, t, u)
	st, ok := o.agents.Get(pane)
	if !ok || st.Harness != detect.KindOpencode || st.Session == "" {
		return
	}
	p.mu.Lock()
	changed := st.Session != p.session
	p.session = st.Session
	url := p.url
	p.mu.Unlock()
	if changed {
		o.mu.Lock()
		live := o.panes[pane] == p
		o.mu.Unlock()
		if live {
			if err := o.writeRecord(pane, ocRecord{URL: url, Session: st.Session}); err != nil {
				o.logf("opencode: pane %s: %v", pane, err)
			}
		}
	}
}

func isStateEvent(t string) bool {
	for _, s := range harness.OpencodeStateEvents {
		if s == t {
			return true
		}
	}
	return false
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
