package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
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

// opencodeRelistEvents are the server events after which messages may have left a session
// (opencode's undo and revert): the chat view reads the whole loaded window again.
var opencodeRelistEvents = []string{"session.revert.staged", "session.revert.committed", "session.revert.cleared"}

// opencodeRetry bounds the wait between two attempts to reach a pane's server.
const (
	opencodeRetryMin = 250 * time.Millisecond
	opencodeRetryMax = 10 * time.Second
)

// opencodeMaxRequests bounds the permission requests a pane remembers the session of.
const opencodeMaxRequests = 256

// opencodeServers follows the private opencode server of each opencode pane gff-hab started
// (harness-adapters.md, opencode, built choices). For each it keeps one event stream open and
// applies its state events to the agent store with source server; it serves the client that
// session.send, approval.answer and the chat view use.
//
// Two files per pane live in <state>/opencode. <pane uuid>.server.json is the pane's: its
// wrapper writes the URL its server listens on, the server's pid, and that the server ended
// (ocServer). <pane uuid>.json is gff-hab's: the session last bound, so a restarted gff-hab
// binds it again. The password is derived from the hook secret, never stored.
type opencodeServers struct {
	recMu  sync.Mutex // one record write at a time, so none undoes another's fields
	dir    string
	secret []byte
	agents *harness.Store
	logf   func(string, ...any)
	// dirty, if set, hears that a pane's conversation changed (the chat view); relist says
	// messages may have been removed.
	dirty func(pane string, relist bool)

	mu      sync.Mutex
	brief   func(ctx context.Context, pane string) string // the Logbook brief for a new session; nil: none
	panes   map[string]*ocPane
	pending map[string]pendingPane // recorded panes the link does not follow yet
}

// pendingPane is a recorded pane load found before the link reported it.
type pendingPane struct {
	uuid hp.UUID
	rec  ocRecord
}

// ocPane is one pane's server and what the adapter learned from it.
type ocPane struct {
	password   string
	serverPath string
	cancel     context.CancelFunc
	done       chan struct{}

	mu       sync.Mutex
	client   *opencode.Client // the client of the server the server file names; nil before
	srvPID   int              // that server's pid
	version  string
	session  string            // the session last bound, as recorded
	children map[string]bool   // session id: has a parent
	running  map[string]bool   // sessions whose agent loop runs
	requests map[string]string // permission request id: its session
	reqOrder []string          // request ids, oldest first
	docOK    bool              // the OpenAPI document was checked
	docErr   string            // the last document fetch error logged
	briefed  map[string]bool   // sessions the brief went to, or was tried for
}

// ocRecord is a pane's record file.
type ocRecord struct {
	Session string `json:"session,omitempty"`
	// Briefed lists the sessions the Logbook brief went to, or was tried for, so a follower
	// made again (a gff-hab restart, a replaced follower) never posts it twice.
	Briefed []string `json:"briefed,omitempty"`
}

// errServerGone: the server file says the pane's server ended, or names a process that is gone.
var errServerGone = errors.New("the pane's opencode server ended")

// errNotOurServer: the server answering at the URL is not the process the server file names.
var errNotOurServer = errors.New("another process answers at the pane's opencode server URL")

func newOpencodeServers(state string, secret []byte, agents *harness.Store, logf func(string, ...any)) *opencodeServers {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &opencodeServers{dir: filepath.Join(state, opencodeDir), secret: append([]byte(nil), secret...), agents: agents,
		logf: logf, panes: map[string]*ocPane{}, pending: map[string]pendingPane{}}
}

// password is the opencode server password of the pane with this uuid.
func (o *opencodeServers) password(u hp.UUID) string {
	m := hmac.New(sha256.New, o.secret)
	m.Write([]byte(opencodePasswordLabel))
	m.Write(u[:])
	return hex.EncodeToString(m.Sum(nil))
}

func (o *opencodeServers) recordPath(pane string) string {
	return filepath.Join(o.dir, pane+".json")
}

// serverPath is the server file the pane's wrapper writes.
func (o *opencodeServers) serverPath(pane string) string {
	return filepath.Join(o.dir, pane+".server.json")
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

// saveRecord writes the record of a followed pane from its follower: the session last bound
// and the sessions briefed. It writes nothing for a follower that was replaced.
func (o *opencodeServers) saveRecord(pane string, p *ocPane) error {
	o.recMu.Lock()
	defer o.recMu.Unlock()
	o.mu.Lock()
	live := o.panes[pane] == p
	o.mu.Unlock()
	if !live {
		return nil
	}
	p.mu.Lock()
	r := ocRecord{Session: p.session}
	for sid := range p.briefed {
		r.Briefed = append(r.Briefed, sid)
	}
	p.mu.Unlock()
	sort.Strings(r.Briefed)
	return o.writeRecord(pane, r)
}

// register records a new pane and starts following its server, once the wrapper reports it.
func (o *opencodeServers) register(pane string, u hp.UUID) error {
	if err := o.writeRecord(pane, ocRecord{}); err != nil {
		return fmt.Errorf("record the opencode server of pane %s: %w", pane, err)
	}
	o.follow(pane, u, ocRecord{})
	return nil
}

// load reads the records a previous gff-hab left. It removes the records of panes whose server
// ended, follows the server of every pane live says the link follows, and keeps the others
// pending: a holder the link has not reached yet may still list them, and added follows them
// when it does. A record with no server file is removed once it is older than the time a
// wrapper takes to report.
func (o *opencodeServers) load(live func(pane string) (hp.UUID, bool)) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		pane, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(pane, ".") || strings.HasSuffix(pane, ".server") {
			continue
		}
		u, ok := parseUUID(pane)
		if !ok {
			continue
		}
		if o.serverEnded(pane, e) {
			os.Remove(o.recordPath(pane))
			os.Remove(o.serverPath(pane))
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
		o.mu.Lock()
		o.pending[pane] = pendingPane{uuid: u, rec: r}
		o.mu.Unlock()
		if _, ok := live(pane); ok {
			o.added(pane)
		}
	}
}

// serverEnded reports whether a recorded pane's server is gone for good: its server file says
// it ended or names a process that is gone, or there is no server file long after the record.
func (o *opencodeServers) serverEnded(pane string, rec fs.DirEntry) bool {
	s, err := readServerFile(o.serverPath(pane))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		info, err := rec.Info()
		return err == nil && time.Since(info.ModTime()) > 2*opencodeReady
	case err != nil:
		return false
	}
	return s.Ended || !processAlive(s.PID)
}

// added follows a recorded pane once the link follows it.
func (o *opencodeServers) added(pane string) {
	o.mu.Lock()
	pp, ok := o.pending[pane]
	delete(o.pending, pane)
	o.mu.Unlock()
	if ok {
		o.follow(pane, pp.uuid, pp.rec)
	}
}

func (o *opencodeServers) follow(pane string, u hp.UUID, r ocRecord) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &ocPane{password: o.password(u), serverPath: o.serverPath(pane), cancel: cancel, done: make(chan struct{}),
		session: r.Session, children: map[string]bool{}, running: map[string]bool{}, requests: map[string]string{},
		briefed: map[string]bool{}}
	for _, sid := range r.Briefed {
		p.briefed[sid] = true
	}
	o.mu.Lock()
	if old := o.panes[pane]; old != nil {
		old.cancel()
		old.mu.Lock()
		for sid := range old.briefed {
			p.briefed[sid] = true
		}
		old.mu.Unlock()
	}
	o.panes[pane] = p
	o.mu.Unlock()
	go o.watch(ctx, pane, p)
}

// forget stops following a pane that is gone or whose program exited, and removes its files.
func (o *opencodeServers) forget(pane string) {
	o.mu.Lock()
	p := o.panes[pane]
	delete(o.panes, pane)
	delete(o.pending, pane)
	o.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	os.Remove(o.recordPath(pane))
	os.Remove(o.serverPath(pane))
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

// Client returns the client of a pane's server, if gff-hab started the pane's opencode and the
// pane's server file still names a live server. It reads the server file on every call, so a
// send, an answer or a chat read never goes to a port the server gave up.
func (o *opencodeServers) Client(pane string) (*opencode.Client, bool) {
	if o == nil {
		return nil, false
	}
	p := o.pane(pane)
	if p == nil {
		return nil, false
	}
	c, err := p.server()
	return c, err == nil && c != nil
}

// processAlive reports whether a process with this pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// server reads the pane's server file and returns the client of the server it names. Nil with
// no error: the wrapper has not reported yet. errServerGone: the server ended, or its process
// is gone, so its port may belong to anyone now.
func (p *ocPane) server() (*opencode.Client, error) {
	s, err := readServerFile(p.serverPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the pane's server file: %w", err)
	}
	if s.Ended || !processAlive(s.PID) {
		return nil, errServerGone
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil && p.client.Base() == strings.TrimSuffix(s.URL, "/") && p.srvPID == s.PID {
		return p.client, nil
	}
	c, err := opencode.New(s.URL, p.password)
	if err != nil {
		return nil, err
	}
	p.client, p.srvPID = c, s.PID
	return c, nil
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
// Before each attempt it reads the server file; it stops for good when the server ended or
// another process answers at its URL.
func (o *opencodeServers) watch(ctx context.Context, pane string, p *ocPane) {
	defer close(p.done)
	types := append(append(append([]string(nil), harness.OpencodeStateEvents...), opencodeChatEvents...), opencodeRelistEvents...)
	wait := opencodeRetryMin
	var lastErr string
	for {
		c, err := p.server()
		if errors.Is(err, errServerGone) {
			o.logf("opencode: pane %s: %v; no longer followed", pane, err)
			return
		}
		if c == nil {
			if err != nil && err.Error() != lastErr {
				o.logf("opencode: pane %s: %v; trying again", pane, err)
				lastErr = err.Error()
			}
			// The wrapper has not reported its server yet: look again soon.
			select {
			case <-ctx.Done():
				return
			case <-time.After(opencodeRetryMin):
			}
			continue
		}
		start := time.Now()
		actx, acancel := context.WithCancelCause(ctx)
		err = c.Events(actx, types, func() {
			if err := o.resync(actx, pane, p, c, start.UnixMilli()); err != nil {
				acancel(err)
			}
		}, func(e opencode.Event) { o.event(actx, pane, p, e) })
		acancel(nil)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errNotOurServer) {
			o.logf("opencode: pane %s: error: %v; no longer followed", pane, err)
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

// resync reads the server's state when the stream (re)connects: who the server is, the
// version, the document's operations (once), the open permission requests and the sessions that
// run. Every update it applies is stamped at, a time taken before the subscription was sent, so
// every event the stream then delivers is newer and wins. It returns errNotOurServer when the
// process answering is not the one the server file names.
func (o *opencodeServers) resync(ctx context.Context, pane string, p *ocPane, c *opencode.Client, at int64) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := c.Info(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	pid := p.srvPID
	p.mu.Unlock()
	if info.PID != 0 && info.PID != pid {
		return fmt.Errorf("%w: it is process %d, the server file names %d", errNotOurServer, info.PID, pid)
	}
	p.mu.Lock()
	changed := p.version != info.Version
	p.version = info.Version
	docOK := p.docOK
	p.mu.Unlock()
	if changed && detect.CheckVersion(detect.KindOpencode, info.Version) != nil {
		o.logf("opencode: pane %s: server version %s is outside the tested %s; the adapter goes on", pane, info.Version, opencode.TestedVersion)
	}
	if !docOK {
		o.checkDoc(ctx, pane, p, c)
	}
	// Requests first: a running session with one open stays blocked.
	perms, err := c.Permissions(ctx)
	if err != nil {
		o.logf("opencode: pane %s: %v", pane, err)
		return nil
	}
	active, err := c.Active(ctx)
	if err != nil {
		o.logf("opencode: pane %s: %v", pane, err)
		return nil
	}
	var pending []json.RawMessage
	open := map[string]bool{}
	for _, raw := range perms {
		var d opencode.Permission
		if json.Unmarshal(raw, &d) != nil || d.ID == "" {
			continue
		}
		pending = append(pending, raw)
		open[d.ID] = true
	}
	p.mu.Lock()
	clear(p.running)
	for _, id := range active {
		p.running[id] = true
	}
	recorded := p.session
	version := p.version
	p.mu.Unlock()
	st, _ := o.agents.Get(pane)
	bound, blockedOn := "", ""
	if st.Harness == detect.KindOpencode {
		bound = st.Session
		if st.State == api.AgentStatusBlocked && st.Request != nil {
			blockedOn = st.Request.ID
		}
	}
	// A running top-level session is the pane's; else the one recorded before a restart.
	target := ""
	for _, id := range active {
		if !o.child(ctx, pane, p, id) {
			target = id
			break
		}
	}
	state := api.AgentStatusWorking
	if target == "" {
		target, state = firstNonEmpty(bound, recorded), api.AgentStatusIdle
	}
	switch {
	case target == "":
	case target != bound:
		o.apply(pane, p, at, harness.Update{Session: target, Bind: true, State: state, Version: version})
	case state == api.AgentStatusWorking:
		// Working would end the open request: leave it to the requests below.
		if len(pending) == 0 {
			o.apply(pane, p, at, harness.Update{Session: target, State: state, Version: version})
		}
	case st.State == api.AgentStatusWorking || st.State == api.AgentStatusBlocked:
		// The stream was down while the turn ended: its last message says how.
		o.apply(pane, p, at, harness.Update{Session: target, State: o.turnEnd(ctx, c, target), Version: version})
	}
	for _, raw := range pending {
		var d opencode.Permission
		json.Unmarshal(raw, &d)
		if d.ID == blockedOn && open[blockedOn] {
			continue // still open, and shown
		}
		o.event(ctx, pane, p, opencode.Event{Type: "permission.asked", Created: at, Data: raw})
	}
	return nil
}

// checkDoc checks the server's OpenAPI document once, and logs a fetch error once per kind.
func (o *opencodeServers) checkDoc(ctx context.Context, pane string, p *ocPane, c *opencode.Client) {
	doc, err := c.Doc(ctx)
	if err != nil {
		p.mu.Lock()
		repeat := p.docErr == err.Error()
		p.docErr = err.Error()
		p.mu.Unlock()
		if !repeat {
			o.logf("opencode: pane %s: error: the OpenAPI document could not be read, so the API was not checked: %v", pane, err)
		}
		return
	}
	if err := opencode.CheckDoc(doc); err != nil {
		o.logf("opencode: pane %s: error: %v; sends, answers or the chat view may fail", pane, err)
	}
	p.mu.Lock()
	p.docOK = true
	p.mu.Unlock()
}

// turnEnd reads how the session's last turn ended from its newest message: done, error, or
// idle when it was interrupted or the message says nothing.
func (o *opencodeServers) turnEnd(ctx context.Context, c *opencode.Client, session string) api.AgentStatus {
	page, err := c.Messages(ctx, session, "", 1)
	if err != nil || len(page.Data) == 0 {
		return api.AgentStatusIdle
	}
	var m struct {
		Type    string `json:"type"`
		Outcome string `json:"outcome"`
	}
	json.Unmarshal(page.Data[0], &m)
	if m.Type != "idle" {
		return api.AgentStatusIdle
	}
	switch m.Outcome {
	case "succeeded":
		return api.AgentStatusDone
	case "failed":
		return api.AgentStatusError
	}
	return api.AgentStatusIdle
}

// child reports whether a session has a parent session, asking the server on first sight. When
// the server cannot say, a session other than the one the pane is bound to is taken as a child,
// so a failed lookup never moves the pane off its session.
func (o *opencodeServers) child(ctx context.Context, pane string, p *ocPane, id string) bool {
	p.mu.Lock()
	c, ok := p.children[id]
	client := p.client
	p.mu.Unlock()
	if ok {
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, opencodeTimeout)
	defer cancel()
	s, err := client.Session(ctx, id)
	if err != nil {
		st, ok := o.agents.Get(pane)
		return ok && st.Harness == detect.KindOpencode && st.Session != "" && st.Session != id
	}
	p.mu.Lock()
	p.children[id] = s.ParentID != ""
	p.mu.Unlock()
	return s.ParentID != ""
}

// event applies one server event.
func (o *opencodeServers) event(ctx context.Context, pane string, p *ocPane, e opencode.Event) {
	if o.dirty != nil {
		o.dirty(pane, isRelistEvent(e.Type))
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
		if d.ParentID == "" && sid != "" {
			o.postBrief(ctx, pane, p, sid)
		}
	}
	s := harness.OpencodeSession{}
	if sid != "" {
		s.Child = o.child(ctx, pane, p, sid)
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
			p.remember(d.ID, d.SessionID)
		}
	case "permission.replied":
		var d struct {
			RequestID string `json:"requestID"`
		}
		if json.Unmarshal(e.Data, &d) == nil {
			p.forgetRequest(d.RequestID)
		}
	}
	s.Running = p.running[sid]
	p.mu.Unlock()
	s.Bound = o.agents.OpencodeBound(pane, sid)
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

// SetBrief sets what gives the Logbook brief for a pane's new session.
func (o *opencodeServers) SetBrief(fn func(ctx context.Context, pane string) string) {
	o.mu.Lock()
	o.brief = fn
	o.mu.Unlock()
}

// postBrief adds the Logbook brief to a new top-level session of a pane's server as a synthetic
// message that starts no turn (logbook.md 11.4). Once per session: a failed post is logged and
// not tried again. It runs beside the event stream, so a slow brief or server holds no event.
func (o *opencodeServers) postBrief(ctx context.Context, pane string, p *ocPane, sid string) {
	o.mu.Lock()
	brief := o.brief
	o.mu.Unlock()
	if brief == nil {
		return
	}
	p.mu.Lock()
	if p.briefed[sid] {
		p.mu.Unlock()
		return
	}
	p.briefed[sid] = true
	c := p.client
	p.mu.Unlock()
	if c == nil {
		return
	}
	// Recorded before the post: a gff-hab that stops mid-post never posts it again.
	if err := o.saveRecord(pane, p); err != nil {
		o.logf("opencode: pane %s: record the brief for session %s: %v", pane, sid, err)
	}
	go func() {
		ctx, cancel := context.WithTimeout(ctx, opencodeTimeout+briefBuild)
		defer cancel()
		text := brief(ctx, pane)
		if text == "" {
			return
		}
		pctx, pcancel := context.WithTimeout(ctx, opencodeTimeout)
		defer pcancel()
		if err := c.Synthetic(pctx, sid, text, "Logbook brief"); err != nil {
			o.logf("opencode: pane %s: the Logbook brief for session %s: %v", pane, sid, err)
		}
	}()
}

// briefBuild bounds building the brief for an opencode session.
const briefBuild = 5 * time.Second

// remember records the session of a permission request, dropping the oldest past the bound.
// It runs under p.mu.
func (p *ocPane) remember(id, session string) {
	if _, ok := p.requests[id]; !ok {
		p.reqOrder = append(p.reqOrder, id)
	}
	p.requests[id] = session
	for len(p.reqOrder) > opencodeMaxRequests {
		delete(p.requests, p.reqOrder[0])
		p.reqOrder = p.reqOrder[1:]
	}
}

// forgetRequest drops an answered request. It runs under p.mu.
func (p *ocPane) forgetRequest(id string) {
	if _, ok := p.requests[id]; !ok {
		return
	}
	delete(p.requests, id)
	for i, r := range p.reqOrder {
		if r == id {
			p.reqOrder = append(p.reqOrder[:i:i], p.reqOrder[i+1:]...)
			break
		}
	}
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
	p.mu.Unlock()
	if changed {
		o.mu.Lock()
		live := o.panes[pane] == p
		o.mu.Unlock()
		if live {
			if err := o.saveRecord(pane, p); err != nil {
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

func isRelistEvent(t string) bool {
	for _, s := range opencodeRelistEvents {
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
