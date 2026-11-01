package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/opencode"
)

// ocFake is an in-process opencode 2.2.5 server for the follower's tests: the state it reports
// is set by the test, and events go to every open stream.
type ocFake struct {
	srv      *httptest.Server
	password string

	mu          sync.Mutex
	pid         int
	active      []string
	perms       []string // raw permission requests
	last        string   // the newest message of every session, raw, or ""
	sessionErr  bool     // session.get fails
	sessionHang bool     // session.get answers only when the caller gives up
	docFail     bool
	onInfo      func(f *ocFake) // runs inside server.info, once
	subs        map[chan string]bool
	streams     int
	reqs        []string
}

func newOcFake(t *testing.T, password string) *ocFake {
	t.Helper()
	f := &ocFake{password: password, pid: os.Getpid(), subs: map[chan string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() { f.drop(); f.srv.Close() })
	return f
}

func (f *ocFake) serve(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != opencode.User || p != f.password {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/api/info":
		f.mu.Lock()
		pid, hook := f.pid, f.onInfo
		f.onInfo = nil
		f.mu.Unlock()
		if hook != nil {
			hook(f)
		}
		fmt.Fprintf(w, `{"version":"2.2.5","pid":%d}`, pid)
	case path == "/openapi.json":
		f.mu.Lock()
		fail := f.docFail
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		paths := map[string]map[string]map[string]string{}
		for _, op := range opencode.Operations {
			if paths[op.Path] == nil {
				paths[op.Path] = map[string]map[string]string{}
			}
			paths[op.Path][strings.ToLower(op.Method)] = map[string]string{"operationId": op.ID}
		}
		json.NewEncoder(w).Encode(map[string]any{"paths": paths})
	case path == "/api/session/active":
		f.mu.Lock()
		active := map[string]any{}
		for _, id := range f.active {
			active[id] = map[string]string{"type": "running"}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"data": active})
	case path == "/api/permission/request":
		f.mu.Lock()
		io.WriteString(w, `{"data":[`+strings.Join(f.perms, ",")+`]}`)
		f.mu.Unlock()
	case strings.HasSuffix(path, "/message"):
		f.mu.Lock()
		last := f.last
		f.mu.Unlock()
		if last == "" {
			io.WriteString(w, `{"data":[],"cursor":{}}`)
			return
		}
		io.WriteString(w, `{"data":[`+last+`],"cursor":{}}`)
	case strings.HasPrefix(path, "/api/session/"):
		f.mu.Lock()
		fail, hang := f.sessionErr, f.sessionHang
		f.mu.Unlock()
		switch {
		case hang:
			<-r.Context().Done()
		case fail:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			fmt.Fprintf(w, `{"data":{"id":%q}}`, strings.TrimPrefix(path, "/api/session/"))
		}
	case path == "/api/event":
		c := make(chan string, 64)
		f.mu.Lock()
		f.subs[c] = true
		f.streams++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"server.connected\",\"data\":{}}\n\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case ev, ok := <-c:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", ev)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				f.mu.Lock()
				delete(f.subs, c)
				f.mu.Unlock()
				return
			}
		}
	default:
		http.NotFound(w, r)
	}
}

// emit sends an event, stamped now, to every open stream.
func (f *ocFake) emit(typ, data string) {
	ev := fmt.Sprintf(`{"type":%q,"created":%d,"data":%s}`, typ, time.Now().UnixMilli(), data)
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.subs {
		c <- ev
	}
}

// drop ends every open stream: the follower reconnects.
func (f *ocFake) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.subs {
		close(c)
		delete(f.subs, c)
	}
}

func (f *ocFake) streamCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams
}

func (f *ocFake) requests(what string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if r == what {
			n++
		}
	}
	return n
}

func (f *ocFake) set(fn func(f *ocFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// ocLogs collects log lines.
type ocLogs struct {
	mu    sync.Mutex
	lines []string
}

func (l *ocLogs) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *ocLogs) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// ocTest is one followed pane against an ocFake.
type ocTest struct {
	t      *testing.T
	o      *opencodeServers
	agents *harness.Store
	f      *ocFake
	pane   string
	uuid   hp.UUID
	logs   *ocLogs

	mu    sync.Mutex
	dirty int
}

// newOcTest makes the follower and the fake, and writes the pane's server file. follow starts
// following.
func newOcTest(t *testing.T) *ocTest {
	t.Helper()
	x := &ocTest{t: t, logs: &ocLogs{}, uuid: newUUID()}
	x.pane = x.uuid.String()
	x.agents = harness.NewStore(harness.Config{})
	x.o = newOpencodeServers(t.TempDir(), []byte("hook secret"), x.agents, x.logs.logf)
	x.o.dirty = func(string, bool) { x.mu.Lock(); x.dirty++; x.mu.Unlock() }
	x.f = newOcFake(t, x.o.password(x.uuid))
	if err := os.MkdirAll(x.o.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	x.writeServer(ocServer{URL: x.f.srv.URL, PID: os.Getpid()})
	t.Cleanup(x.o.Close)
	return x
}

func (x *ocTest) writeServer(s ocServer) {
	x.t.Helper()
	if err := writeServerFile(x.o.serverPath(x.pane), s); err != nil {
		x.t.Fatal(err)
	}
}

func (x *ocTest) follow() {
	x.t.Helper()
	if err := x.o.register(x.pane, x.uuid); err != nil {
		x.t.Fatal(err)
	}
}

func (x *ocTest) state() api.AgentState {
	st, _ := x.agents.Get(x.pane)
	return st
}

func (x *ocTest) waitState(what string, ok func(api.AgentState) bool) {
	x.t.Helper()
	waitUntil(x.t, what, func() bool { return ok(x.state()) })
}

// emitSeen emits an event that only marks the chat dirty, and waits until the follower read
// it: every event before it on the stream, and the resync before them, were applied.
func (x *ocTest) emitSeen() {
	x.t.Helper()
	x.mu.Lock()
	n := x.dirty
	x.mu.Unlock()
	x.f.emit("session.step.ended", `{"sessionID":"ses_1"}`)
	waitUntil(x.t, "the marker event", func() bool { x.mu.Lock(); defer x.mu.Unlock(); return x.dirty > n })
}

// reconnect drops the stream and waits until the follower subscribed again and resynced.
func (x *ocTest) reconnect() {
	x.t.Helper()
	n := x.f.streamCount()
	x.f.drop()
	waitUntil(x.t, "a new stream", func() bool { return x.f.streamCount() > n })
	x.emitSeen()
}

// working binds ses_1 and starts a turn on it.
func (x *ocTest) working() {
	x.t.Helper()
	waitUntil(x.t, "the stream", func() bool { return x.f.streamCount() > 0 })
	x.emitSeen()
	x.f.emit("session.created", `{"sessionID":"ses_1","version":"2.2.5"}`)
	x.f.emit("session.execution.started", `{"sessionID":"ses_1"}`)
	x.waitState("working", func(s api.AgentState) bool { return s.Session == "ses_1" && s.State == api.AgentStatusWorking })
}

const perm1 = `{"id":"per_1","sessionID":"ses_1","action":"shell","resources":["make test"]}`

// TestOpencodeReconnectKeepsRequest (review M2): a stream that reconnects while a running
// session waits on a permission request keeps the pane blocked on that request; the answer
// still resolves it. A request answered while the stream was down ends on reconnect.
func TestOpencodeReconnectKeepsRequest(t *testing.T) {
	x := newOcTest(t)
	x.follow()
	x.working()
	x.f.set(func(f *ocFake) { f.active, f.perms = []string{"ses_1"}, []string{perm1} })
	x.f.emit("permission.asked", perm1)
	x.waitState("blocked", func(s api.AgentState) bool { return s.State == api.AgentStatusBlocked })

	x.reconnect()
	if st := x.state(); st.State != api.AgentStatusBlocked || st.Request == nil || st.Request.ID != "per_1" {
		t.Fatalf("after a reconnect with the request open: %+v", st)
	}
	x.reconnect() // a second reconnect must not drop it either
	if st := x.state(); st.State != api.AgentStatusBlocked || st.Request == nil || st.Request.ID != "per_1" {
		t.Fatalf("after a second reconnect: %+v", st)
	}
	x.f.emit("permission.replied", `{"sessionID":"ses_1","requestID":"per_1","reply":"once"}`)
	x.waitState("working after the reply", func(s api.AgentState) bool { return s.State == api.AgentStatusWorking && s.Request == nil })

	// Asked again, then answered in the TUI while the stream was down.
	x.f.emit("permission.asked", strings.Replace(perm1, "per_1", "per_2", 1))
	x.waitState("blocked on per_2", func(s api.AgentState) bool { return s.Request != nil && s.Request.ID == "per_2" })
	x.f.set(func(f *ocFake) { f.perms = nil })
	x.reconnect()
	if st := x.state(); st.State != api.AgentStatusWorking || st.Request != nil {
		t.Fatalf("after a reconnect with the request answered: %+v", st)
	}
}

// TestOpencodeResyncStamp (review S1): a turn that ends between the subscription and resync's
// reads shows done: resync's own updates are older than every event the stream delivers.
func TestOpencodeResyncStamp(t *testing.T) {
	x := newOcTest(t)
	x.follow()
	x.working()
	x.f.set(func(f *ocFake) {
		f.onInfo = func(f *ocFake) {
			// The turn ends while resync reads the server: the event waits on the new stream.
			f.mu.Lock()
			f.active = nil
			f.mu.Unlock()
			f.emit("session.execution.succeeded", `{"sessionID":"ses_1"}`)
			time.Sleep(5 * time.Millisecond)
		}
	})
	x.reconnect()
	if st := x.state(); st.State != api.AgentStatusDone {
		t.Fatalf("a turn that ended during resync: %+v", st)
	}
}

// TestOpencodeTurnEndWhileDown: a turn that ended while the stream was down shows how it ended,
// from the session's newest message: done, error, or idle.
func TestOpencodeTurnEndWhileDown(t *testing.T) {
	for _, c := range []struct {
		last string
		want api.AgentStatus
	}{
		{`{"id":"m9","type":"idle","outcome":"succeeded"}`, api.AgentStatusDone},
		{`{"id":"m9","type":"idle","outcome":"failed"}`, api.AgentStatusError},
		{`{"id":"m9","type":"idle","outcome":"interrupted"}`, api.AgentStatusIdle},
		{`{"id":"m9","type":"assistant"}`, api.AgentStatusIdle},
	} {
		x := newOcTest(t)
		x.follow()
		x.working()
		x.f.set(func(f *ocFake) { f.active, f.last = nil, c.last })
		x.reconnect()
		if st := x.state(); st.State != c.want {
			t.Errorf("last message %s: state %s, want %s", c.last, st.State, c.want)
		}
	}
}

// TestOpencodeFollowerSquatter (review M1, gff-hab's side): once the pane's server is gone, the
// follower never sends the password to whoever takes its port, whether the wrapper reported
// the end or was killed first; and it stops when another process answers at the URL.
func TestOpencodeFollowerSquatter(t *testing.T) {
	deadPID := func() int {
		c := exec.Command("true")
		if err := c.Run(); err != nil {
			t.Skip("no true:", err)
		}
		return c.Process.Pid
	}
	for _, how := range []string{"reported", "killed"} {
		t.Run(how, func(t *testing.T) {
			x := newOcTest(t)
			x.follow()
			x.working()
			addr := x.f.srv.Listener.Addr().String()
			if how == "reported" {
				x.writeServer(ocServer{URL: x.f.srv.URL, PID: os.Getpid(), Ended: true})
			} else {
				x.writeServer(ocServer{URL: x.f.srv.URL, PID: deadPID()})
			}
			x.f.drop()
			x.f.srv.Close()
			l, err := net.Listen("tcp", addr)
			if err != nil {
				t.Skipf("the port was not free again: %v", err)
			}
			var mu sync.Mutex
			var got []string
			go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, pw, _ := r.BasicAuth()
				mu.Lock()
				got = append(got, r.URL.Path+" password="+pw)
				mu.Unlock()
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer l.Close()
			time.Sleep(scaleTimeout(1500 * time.Millisecond))
			mu.Lock()
			defer mu.Unlock()
			if len(got) > 0 {
				t.Fatalf("the squatter got %q", got)
			}
			if _, ok := x.o.Client(x.pane); ok {
				t.Fatal("a client of a server that ended")
			}
		})
	}
	t.Run("another process", func(t *testing.T) {
		x := newOcTest(t)
		x.f.set(func(f *ocFake) { f.pid = 1 })
		x.follow()
		waitUntil(t, "the end of following", func() bool { return x.logs.count("another process answers") > 0 })
		time.Sleep(scaleTimeout(700 * time.Millisecond))
		if n := x.f.requests("GET /api/event"); n != 1 || x.f.requests("GET /api/permission/request") != 0 {
			t.Fatalf("the follower went on with another process: %d streams, %d request lists", n, x.f.requests("GET /api/permission/request"))
		}
	})
}

// TestOpencodeLoad (review S2): a restarted gff-hab keeps the record of a pane the link does
// not know yet and follows it once the link reports the pane; it removes the records of
// servers that ended, and of a pane whose wrapper never reported long after.
func TestOpencodeLoad(t *testing.T) {
	x := newOcTest(t)
	if err := x.o.writeRecord(x.pane, ocRecord{Session: "ses_1"}); err != nil {
		t.Fatal(err)
	}
	mk := func(srv *ocServer, age time.Duration) string {
		u := newUUID().String()
		if err := x.o.writeRecord(u, ocRecord{Session: "ses_9"}); err != nil {
			t.Fatal(err)
		}
		if srv != nil {
			if err := writeServerFile(x.o.serverPath(u), *srv); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Now().Add(-age)
		os.Chtimes(x.o.recordPath(u), old, old)
		return u
	}
	ended := mk(&ocServer{URL: "http://127.0.0.1:1", PID: os.Getpid(), Ended: true}, 0)
	silent := mk(nil, 2*time.Minute)
	starting := mk(nil, 0)
	x.o.load(func(string) (hp.UUID, bool) { return hp.UUID{}, false })
	for _, c := range []struct {
		pane string
		keep bool
	}{{x.pane, true}, {ended, false}, {silent, false}, {starting, true}} {
		_, err := os.Stat(x.o.recordPath(c.pane))
		if kept := err == nil; kept != c.keep {
			t.Errorf("record of %s kept %v, want %v", c.pane, kept, c.keep)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := x.f.streamCount(); n != 0 {
		t.Fatalf("followed a pane the link does not know: %d streams", n)
	}
	x.o.added(x.pane)
	x.waitState("the recorded session bound", func(s api.AgentState) bool { return s.Session == "ses_1" && s.Source == api.StateSourceServer })
}

// TestOpencodeDocFetchError (review S4): an OpenAPI document that cannot be read is logged as an
// error once, not on every reconnect, and not silently.
func TestOpencodeDocFetchError(t *testing.T) {
	x := newOcTest(t)
	x.f.set(func(f *ocFake) { f.docFail = true })
	x.follow()
	waitUntil(t, "the stream", func() bool { return x.f.streamCount() > 0 })
	x.emitSeen()
	x.reconnect()
	x.reconnect()
	if n := x.logs.count("the OpenAPI document could not be read"); n != 1 {
		t.Fatalf("the document fetch error was logged %d times, want once: %q", n, x.logs.lines)
	}
}

// TestOpencodeChildLookup (review S7, N6): a session lookup that hangs is cut off, so the stream
// goes on; a session the server cannot describe never moves the pane off its bound session.
func TestOpencodeChildLookup(t *testing.T) {
	old := opencodeTimeout
	opencodeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { opencodeTimeout = old })
	x := newOcTest(t)
	x.follow()
	x.working()
	x.f.set(func(f *ocFake) { f.sessionHang = true })
	x.f.emit("session.execution.started", `{"sessionID":"ses_x"}`)
	x.emitSeen()
	x.f.set(func(f *ocFake) { f.sessionHang, f.sessionErr = false, true })
	x.f.emit("session.execution.started", `{"sessionID":"ses_y"}`)
	x.emitSeen()
	if st := x.state(); st.Session != "ses_1" {
		t.Fatalf("a session the server could not describe took the pane: %+v", st)
	}
}

// TestOpencodeRequestSessions (review S8): an answered request is forgotten, and past the bound
// the oldest goes first, so an open child request keeps its session.
func TestOpencodeRequestSessions(t *testing.T) {
	x := newOcTest(t)
	p := &ocPane{children: map[string]bool{"ses_c": true, "ses_1": false}, running: map[string]bool{}, requests: map[string]string{},
		cancel: func() {}, done: make(chan struct{})}
	close(p.done) // no stream runs for it
	x.o.mu.Lock()
	x.o.panes[x.pane] = p
	x.o.mu.Unlock()
	ctx := context.Background()
	ask := func(id, session string) {
		x.o.event(ctx, x.pane, p, opencode.Event{Type: "permission.asked", Data: json.RawMessage(fmt.Sprintf(`{"id":%q,"sessionID":%q,"action":"read"}`, id, session))})
	}
	reply := func(id, session string) {
		x.o.event(ctx, x.pane, p, opencode.Event{Type: "permission.replied", Data: json.RawMessage(fmt.Sprintf(`{"requestID":%q,"sessionID":%q}`, id, session))})
	}
	ask("per_c", "ses_c")
	for i := range 2 * opencodeMaxRequests {
		id := fmt.Sprintf("per_%d", i)
		ask(id, "ses_1")
		reply(id, "ses_1")
	}
	if s := x.o.requestSession(x.pane, "per_c"); s != "ses_c" {
		t.Fatalf("the open child request's session: %q", s)
	}
	for i := range opencodeMaxRequests + 1 {
		ask(fmt.Sprintf("open_%d", i), "ses_1")
	}
	p.mu.Lock()
	n, order := len(p.requests), len(p.reqOrder)
	p.mu.Unlock()
	if n != opencodeMaxRequests || order != n || x.o.requestSession(x.pane, "per_c") != "" || x.o.requestSession(x.pane, fmt.Sprintf("open_%d", opencodeMaxRequests)) != "ses_1" {
		t.Fatalf("past the bound: %d requests, %d in order", n, order)
	}
}

// TestOpencodeAnswerErrors (review S3): only a 404 tagged as a missing session or request is a
// conflict; a 404 from a route the server lacks is unavailable and says the API drifted.
func TestOpencodeAnswerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		switch {
		case strings.Contains(r.URL.Path, "ses_gone"):
			io.WriteString(w, `{"_tag":"SessionNotFoundError","message":"Session not found"}`)
		case strings.Contains(r.URL.Path, "per_gone"):
			io.WriteString(w, `{"_tag":"PermissionNotFoundError","message":"Permission request not found"}`)
		}
	}))
	defer srv.Close()
	c, err := opencode.New(srv.URL, "pw")
	if err != nil {
		t.Fatal(err)
	}
	a := &agentService{opencode: newOpencodeServers(t.TempDir(), []byte("k"), harness.NewStore(harness.Config{}), nil)}
	ctx := context.Background()
	kind := func(err error) (api.ErrorKind, string) {
		var ce *control.Error
		if !errors.As(err, &ce) {
			t.Fatalf("not a control error: %v", err)
		}
		return ce.Data.Kind, ce.Message
	}
	check := func(what string, err error, want api.ErrorKind, drift bool) {
		t.Helper()
		k, msg := kind(err)
		if k != want || strings.Contains(msg, "API drift") != drift {
			t.Errorf("%s: %s %q", what, k, msg)
		}
	}
	check("send to a moved route", a.promptOpencode(ctx, c, api.AgentState{Session: "ses_1"}, "hi"), api.ErrorKindUnavailable, true)
	check("send to a gone session", a.promptOpencode(ctx, c, api.AgentState{Session: "ses_gone"}, "hi"), api.ErrorKindConflict, false)
	st := api.AgentState{Session: "ses_1"}
	check("answer on a moved route", a.replyOpencode(ctx, c, api.ApprovalAnswerParams{Pane: "p", Request: "per_1"}, st, true), api.ErrorKindUnavailable, true)
	check("answer to a gone request", a.replyOpencode(ctx, c, api.ApprovalAnswerParams{Pane: "p", Request: "per_gone"}, st, true), api.ErrorKindConflict, false)
}

// TestOpencodeStartArgv (review N3): opencode's argv may not name the server in either form,
// nor be empty; the wrapper gets the server file to report in.
func TestOpencodeStartArgv(t *testing.T) {
	o := newOpencodeServers(t.TempDir(), []byte("k"), harness.NewStore(harness.Config{}), nil)
	a := &actions{opencode: o, exe: "/bin/gff-hab"}
	for _, argv := range [][]string{{"opencode", "--server", "http://127.0.0.1:1"}, {"opencode", "--server=http://127.0.0.1:1"}, {""}} {
		if _, _, err := a.opencodeStart(api.SessionCreateParams{Argv: argv}); err == nil {
			t.Errorf("argv %q accepted", argv)
		}
	}
	u, p, err := a.opencodeStart(api.SessionCreateParams{Argv: []string{"opencode", "--continue"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/gff-hab", "opencode-pane", "-report", o.serverPath(u.String()), "--", "opencode", "--continue"}
	if strings.Join(p.Argv, " ") != strings.Join(want, " ") || !strings.HasPrefix(p.Env[len(p.Env)-1], opencodePasswordEnv+"=") {
		t.Fatalf("argv %q env %q", p.Argv, p.Env)
	}
}

var (
	fakeOpencodeOnce sync.Once
	fakeOpencodePath string
	fakeOpencodeErr  error
)

// buildFakeOpencode builds testdata/fakeopencode once per test binary.
func buildFakeOpencode(t *testing.T) string {
	t.Helper()
	fakeOpencodeOnce.Do(func() {
		d, err := os.MkdirTemp("", "fakeopencode")
		if err != nil {
			fakeOpencodeErr = err
			return
		}
		fakeOpencodePath = filepath.Join(d, "opencode")
		if out, err := exec.Command("go", "build", "-o", fakeOpencodePath, "./testdata/fakeopencode").CombinedOutput(); err != nil {
			fakeOpencodeErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if fakeOpencodeErr != nil {
		t.Fatalf("build the fake opencode: %v", fakeOpencodeErr)
	}
	return fakeOpencodePath
}

// TestOpencodePaneSquatter (review M1, the wrapper's side): the server picks its own port and
// the wrapper takes the URL from it; when the process answering at that URL is not the
// server, the wrapper starts no TUI and reports no server.
func TestOpencodePaneSquatter(t *testing.T) {
	fake := buildFakeOpencode(t)
	d := t.TempDir()
	squatter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":"2.2.5","pid":1}`)
	}))
	defer squatter.Close()
	if err := os.WriteFile(filepath.Join(d, "url"), []byte(squatter.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DIR", d)
	t.Setenv(opencodePasswordEnv, "pw")
	report := filepath.Join(d, "srv.json")
	done := make(chan int, 1)
	go func() { done <- opencodePaneCmd([]string{"-report", report, "--", fake}) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
	case <-time.After(scaleTimeout(10 * time.Second)):
		os.WriteFile(filepath.Join(d, "end"), nil, 0o600) // a TUI that started: end it
		<-done
		t.Fatal("the wrapper ran on against another process")
	}
	if _, err := os.Stat(filepath.Join(d, "tui.args")); err == nil {
		t.Fatal("the TUI started against another process")
	}
	if _, err := os.Stat(report); err == nil {
		t.Fatal("the wrapper reported another process as the pane's server")
	}
	waitUntil(t, "the server's end", func() bool { b, _ := os.ReadFile(filepath.Join(d, "server.exit")); return len(b) > 0 })
}

// TestOpencodePaneReport: the wrapper reports the URL its server printed and the server's pid,
// starts the TUI against that URL, marks the report ended when the server dies under the TUI,
// and removes it when the TUI exits.
func TestOpencodePaneReport(t *testing.T) {
	fake := buildFakeOpencode(t)
	d := t.TempDir()
	t.Setenv("DIR", d)
	t.Setenv(opencodePasswordEnv, "pw")
	report := filepath.Join(d, "srv.json")
	done := make(chan int, 1)
	go func() { done <- opencodePaneCmd([]string{"-report", report, "--", fake, "--continue"}) }()
	var s ocServer
	waitUntil(t, "the report", func() bool { var err error; s, err = readServerFile(report); return err == nil })
	if s.PID <= 0 || s.Ended || !strings.HasPrefix(s.URL, "http://127.0.0.1:") {
		t.Fatalf("report %+v", s)
	}
	waitUntil(t, "the TUI", func() bool { b, _ := os.ReadFile(filepath.Join(d, "tui.args")); return len(b) > 0 })
	if b, _ := os.ReadFile(filepath.Join(d, "tui.args")); !strings.HasPrefix(string(b), "--server "+s.URL+" --continue |") {
		t.Fatalf("TUI argv %q", b)
	}
	os.WriteFile(filepath.Join(d, "die"), nil, 0o600)
	waitUntil(t, "the report of the end", func() bool { s, _ = readServerFile(report); return s.Ended })
	os.WriteFile(filepath.Join(d, "end"), nil, 0o600)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(scaleTimeout(10 * time.Second)):
		t.Fatal("the wrapper did not exit with the TUI")
	}
	if _, err := os.Stat(report); !os.IsNotExist(err) {
		t.Fatalf("the report outlived the wrapper: %v", err)
	}
}
