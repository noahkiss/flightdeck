package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// changer makes a run of structure changes: projects, panes, sessions, renames, moves, closes.
func changer(t *testing.T, h *hab, prefix string, n int) {
	t.Helper()
	var projects []string
	for i := range n {
		p, err := h.store.CreateProject(fmt.Sprintf("%s-p%d", prefix, i), fmt.Sprintf("example.com/o/%s-%d", prefix, i), "")
		if err != nil {
			t.Error(err)
			return
		}
		projects = append(projects, p.ID)
		pane := fmt.Sprintf("%s-pane-%d", prefix, i)
		h.holder.Add(structure.HolderPane{ID: pane, HolderID: "h1", HolderBuild: "b", Cols: 80, Rows: 24})
		s, err := h.store.CreateSession(structure.SessionSpec{Name: fmt.Sprintf("%s-s%d", prefix, i), Project: p.ID, Pane: pane, Index: -1})
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := h.store.RenameProject(p.ID, fmt.Sprintf("%s-renamed-%d", prefix, i)); err != nil {
			t.Error(err)
		}
		h.holder.Resize(pane, 100+i, 30)
		if i > 0 {
			if _, err := h.store.MoveSession(s.ID, projects[i-1], 0); err != nil {
				t.Error(err)
			}
		}
		if i%3 == 2 {
			if err := h.store.CloseSession(s.ID); err != nil {
				t.Error(err)
			}
			h.holder.Remove(pane)
		}
		if i%4 == 3 {
			if err := h.store.DeleteProject(projects[i-1]); err != nil {
				t.Error(err)
			}
		}
	}
}

// collect reads notifications until an event with seq last has arrived, checking that every
// event seq is exactly one higher than the one before.
func collect(t *testing.T, c *Client, events *[]api.Event, last uint64) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		if n := len(*events); n > 0 && (*events)[n-1].Seq >= last {
			return
		}
		if last == 0 {
			return
		}
		select {
		case n, ok := <-c.Notifications():
			if !ok {
				t.Fatalf("connection ended: %v", c.Err())
			}
			if n.Method != "event" {
				continue
			}
			e, err := n.Event()
			if err != nil {
				t.Fatal(err)
			}
			if k := len(*events); k > 0 && e.Seq != (*events)[k-1].Seq+1 {
				t.Fatalf("seq %d after %d: a gap or a repeat", e.Seq, (*events)[k-1].Seq)
			} else if k == 0 && e.Seq != 1 {
				t.Fatalf("first seq %d, want 1", e.Seq)
			}
			*events = append(*events, e)
		case <-timeout:
			t.Fatalf("waited for seq %d; have %d events", last, len(*events))
		}
	}
}

// TestEventsOnceInOrder is the stage acceptance test: a client subscribes while changes run,
// sees every event once and in order, and the seq on each response lets it drop the events the
// response already covers. Its mirror ends equal to the hab's structure.
func TestEventsOnceInOrder(t *testing.T) {
	h := startHab(t, "", socketPath(t), nil)
	c := dial(t, h.path)
	hello(t, c, "")
	cl := api.Client{Caller: c}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); changer(t, h, "a", 12) }()

	// Subscribe while the changes run.
	m := newMirror()
	var events []api.Event
	r1, err := cl.EventsSubscribe(ctx(t), api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}})
	if err != nil {
		t.Fatal(err)
	}
	m.snapshot(t, r1)
	wg.Wait()

	// Subscribe again while more changes run: the second response's seq is above some
	// events already received, and the client drops those.
	wg.Add(1)
	go func() { defer wg.Done(); changer(t, h, "b", 12) }()
	time.Sleep(5 * time.Millisecond)
	r2, err := cl.EventsSubscribe(ctx(t), api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}})
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	ping, err := cl.HabPing(ctx(t), api.PingParams{})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, c, &events, ping.Seq.Seq)
	if ping.Seq.Seq == 0 || uint64(len(events)) != ping.Seq.Seq {
		t.Fatalf("ping seq %d, %d events", ping.Seq.Seq, len(events))
	}
	if r2.Seq.Seq < r1.Seq.Seq || r2.Seq.Seq == 0 {
		t.Fatalf("second subscribe seq %d, first %d", r2.Seq.Seq, r1.Seq.Seq)
	}

	// Replay as a client would: events up to r2's seq apply to the first snapshot, then the
	// second snapshot replaces everything and older events drop.
	for _, e := range events {
		if e.Seq > r2.Seq.Seq {
			break
		}
		m.apply(e)
	}
	m.snapshot(t, r2)
	for _, e := range events {
		m.apply(e)
	}
	if m.dropped < int(r2.Seq.Seq) {
		t.Fatalf("dropped %d events, want at least %d", m.dropped, r2.Seq.Seq)
	}
	m.sameAs(t, h.store.Snapshot(api.HostInfo{ID: "host-a"}))

	// Every revision an object carries rises along the stream.
	revs := map[string]uint64{}
	for _, e := range events {
		if e.Revision == nil {
			continue
		}
		k := string(e.Type[:strings.IndexByte(string(e.Type), '.')]) + "/" + e.ID
		if *e.Revision <= revs[k] {
			t.Fatalf("event %d: %s revision %d after %d", e.Seq, k, *e.Revision, revs[k])
		}
		revs[k] = *e.Revision
	}
}

// TestRestartGivesNewEpoch restarts the hab under a connected client. The client sees closing
// (shutdown), reconnects, sees a new epoch, its token is refused, and its fresh snapshot holds
// the stored structure.
func TestRestartGivesNewEpoch(t *testing.T) {
	dir := t.TempDir()
	path := socketPath(t)
	h := startHab(t, dir, path, nil)
	c := dial(t, path)
	w1 := hello(t, c, "")
	if _, err := (api.Client{Caller: c}).EventsSubscribe(ctx(t), api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}); err != nil {
		t.Fatal(err)
	}
	p, err := h.store.CreateProject("kept", "example.com/o/kept", "")
	if err != nil {
		t.Fatal(err)
	}
	h.stop()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client not closed by the shutdown")
	}
	if c.ClosingReason() != api.ClosingReasonShutdown {
		t.Fatalf("closing reason %q", c.ClosingReason())
	}

	startHab(t, dir, path, nil)
	c2 := dial(t, path)
	w2 := hello(t, c2, w1.ReconnectToken)
	if w2.Epoch == w1.Epoch {
		t.Fatal("same epoch after a restart")
	}
	if w2.ClientID == w1.ClientID {
		t.Fatal("a token from the old epoch kept its client id")
	}
	r, err := (api.Client{Caller: c2}).EventsSubscribe(ctx(t), api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Projects) != 1 || r.Projects[0].ID != p.ID || r.Projects[0].Revision != p.Revision {
		t.Fatalf("snapshot after restart: %+v", r.Projects)
	}
}

func TestReconnectTokenKeepsClientID(t *testing.T) {
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Now = clock; c.ReconnectGrace = time.Minute })
	c1 := dial(t, h.path)
	w1 := hello(t, c1, "")
	c1.Close()
	waitConns(t, h.srv, 0)

	c2 := dial(t, h.path)
	w2 := hello(t, c2, w1.ReconnectToken)
	if w2.ClientID != w1.ClientID || w2.Epoch != w1.Epoch {
		t.Fatalf("reconnect in grace: client %s -> %s", w1.ClientID, w2.ClientID)
	}
	if w2.ReconnectToken == w1.ReconnectToken {
		t.Fatal("token was not replaced")
	}
	c3 := dial(t, h.path)
	if w := hello(t, c3, w1.ReconnectToken); w.ClientID == w1.ClientID {
		t.Fatal("a used token worked twice")
	}

	c2.Close()
	waitConns(t, h.srv, 1)
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	c4 := dial(t, h.path)
	if w := hello(t, c4, w2.ReconnectToken); w.ClientID == w1.ClientID {
		t.Fatal("token worked after the grace period")
	}
}

func waitConns(t *testing.T, s *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		k := len(s.conns)
		s.mu.Unlock()
		if k == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("connections did not drop to %d", n)
}

// TestBacklogClosesSlowClient: a client that stops reading is closed with event_backlog once
// its queue passes the limit. Publish never blocks. What it read is gapless, and closing is the
// last message.
func TestBacklogClosesSlowClient(t *testing.T) {
	src := &fakeSource{}
	srv := New(func() Config {
		c := testConfig(src)
		c.BacklogLimit = 64 << 10
		c.CloseGrace = 10 * time.Second
		return c
	}())
	t.Cleanup(srv.Close)
	path := socketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)

	nc, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	fmt.Fprintln(nc, `{"jsonrpc":"2.0","id":1,"method":"hab.hello","params":{"revision":1,"min_revision":1,"client":{"name":"t","version":"0","kind":"script"}}}`)
	fmt.Fprintln(nc, `{"jsonrpc":"2.0","id":2,"method":"events.subscribe","params":{"scope":{"kind":"host"}}}`)
	br := bufio.NewReader(nc)
	for range 2 {
		if _, err := br.ReadBytes('\n'); err != nil {
			t.Fatal(err)
		}
	}

	// Stop reading; publish well past the limit plus any socket buffers.
	name := strings.Repeat("x", 1000)
	start := time.Now()
	for i := range 4000 {
		srv.Publish(structure.Event{Type: structure.ProjectUpdated, ID: "p", Revision: uint64(i + 1),
			Data: api.Project{ID: "p", Revision: uint64(i + 1), Name: name, Sessions: []string{}}})
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("publishing took %v: it blocked on the slow client", d)
	}

	var last uint64
	var closing *api.Closing
	nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break
		}
		if closing != nil {
			t.Fatalf("message after closing: %s", line)
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		switch m.Method {
		case "event":
			var e api.Event
			json.Unmarshal(m.Params, &e)
			if e.Seq != last+1 {
				t.Fatalf("seq %d after %d", e.Seq, last)
			}
			last = e.Seq
		case "closing":
			closing = &api.Closing{}
			json.Unmarshal(m.Params, closing)
		}
	}
	if closing == nil || closing.Reason != api.ClosingReasonEventBacklog {
		t.Fatalf("no closing event_backlog; read %d events", last)
	}
	if last >= 4000 {
		t.Fatalf("read all %d events; the limit never applied", last)
	}
}

func TestNoNullsAndWelcomeFields(t *testing.T) {
	srv := New(testConfig(&fakeSource{}))
	t.Cleanup(srv.Close)
	nc := pipeServe(t, srv)
	br := bufio.NewReader(nc)
	send := func(s string) map[string]any {
		t.Helper()
		go fmt.Fprintln(nc, s)
		nc.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := br.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(line), "null") {
			t.Fatalf("null in %s", line)
		}
		var m map[string]any
		json.Unmarshal(line, &m)
		return m
	}
	w := send(`{"jsonrpc":"2.0","id":"a","method":"hab.hello","params":{"revision":5,"min_revision":1,"client":{"name":"t","version":"0","kind":"app"},"later_field":true}}`)
	res := w["result"].(map[string]any)
	if res["revision"].(float64) != 1 || res["seq"].(float64) != 0 || res["device_id"] != "SHA256:test" || w["id"] != "a" {
		t.Fatalf("welcome %v", w)
	}
	if caps, ok := res["capabilities"].([]any); !ok || len(caps) != 1 || caps[0] != IdempotencyCapability { // always offered (idem.go)
		t.Fatalf("capabilities %v", res["capabilities"])
	}
	st := send(`{"jsonrpc":"2.0","id":2,"method":"structure.get"}`)
	r := st["result"].(map[string]any)
	if _, ok := r["projects"].([]any); !ok || r["seq"] == nil {
		t.Fatalf("structure.get %v", st)
	}
	d := send(`{"jsonrpc":"2.0","id":3,"method":"rpc.discover"}`)
	if d["result"].(map[string]any)["openrpc"] == nil || d["result"].(map[string]any)["seq"] != nil {
		t.Fatalf("rpc.discover %v", d)
	}
	for _, method := range []string{"devices.list", "devices.pair", "devices.revoke", "pane.input", "data.ticket", "peers.list", "peers.accept", "catalog.get", "catalog.put", "catalog.list"} {
		e := send(`{"jsonrpc":"2.0","id":4,"method":"` + method + `","params":{}}`)["error"].(map[string]any)
		data := e["data"].(map[string]any)
		if data["kind"] != "unavailable" || data["seq"] == nil || e["code"].(float64) != api.ErrorCode {
			t.Fatalf("%s: %v", method, e)
		}
	}
	for _, method := range []string{"worktree.list", "worktree.create"} {
		e := send(`{"jsonrpc":"2.0","id":4,"method":"` + method + `","params":{"project_dir":"/r"}}`)["error"].(map[string]any)
		if data := e["data"].(map[string]any); data["kind"] != "unavailable" || data["seq"] == nil {
			t.Fatalf("%s: %v", method, e)
		}
	}
	e := send(`{"jsonrpc":"2.0","id":4,"method":"worktree.remove","params":{"project_dir":"/r"}}`)["error"].(map[string]any)
	if e["data"].(map[string]any)["kind"] != "invalid" {
		t.Fatalf("worktree.remove with no path: %v", e)
	}
	e = send(`{"jsonrpc":"2.0","id":5,"method":"worktree.nope","params":{}}`)["error"].(map[string]any)
	if e["code"].(float64) != codeMethodNotFound {
		t.Fatalf("unknown worktree method: %v", e)
	}
	e = send(`{"jsonrpc":"2.0","id":5,"method":"no.such"}`)["error"].(map[string]any)
	if e["code"].(float64) != codeMethodNotFound {
		t.Fatalf("unknown method: %v", e)
	}
	e = send(`{"jsonrpc":"2.0","id":6,"method":"events.subscribe","params":{"scope":{"kind":"galaxy"}}}`)["error"].(map[string]any)
	if e["data"].(map[string]any)["kind"] != "invalid" {
		t.Fatalf("closed word set: %v", e)
	}
	e = send(`{"jsonrpc":"2.0","id":7,"method":"hab.hello","params":{"revision":1,"min_revision":1,"client":{"name":"t","version":"0","kind":"app"}}}`)["error"].(map[string]any)
	if e["data"].(map[string]any)["kind"] != "invalid" {
		t.Fatalf("second hello: %v", e)
	}
}

// TestWelcomeTime: the welcome carries the hab's clock, RFC 3339 in UTC with milliseconds,
// taken while the hello is answered.
func TestWelcomeTime(t *testing.T) {
	h := startHab(t, "", socketPath(t), nil)
	before := time.Now().Truncate(time.Millisecond)
	w := hello(t, dial(t, h.path), "")
	after := time.Now()
	got, err := time.Parse(time.RFC3339Nano, w.Time)
	if err != nil || got.Before(before) || got.After(after) {
		t.Fatalf("welcome time %q (%v), want between %v and %v", w.Time, err, before, after)
	}
	fixed := time.Date(2026, 3, 1, 9, 30, 0, 7_000_000, time.FixedZone("x", 3600))
	h2 := startHab(t, "", socketPath(t), func(c *Config) { c.Now = func() time.Time { return fixed } })
	if w := hello(t, dial(t, h2.path), ""); w.Time != "2026-03-01T08:30:00.007Z" {
		t.Fatalf("welcome time %q, want 2026-03-01T08:30:00.007Z", w.Time)
	}
}

// TestWelcomeVersion: the welcome carries the configured release version, and none when unset.
func TestWelcomeVersion(t *testing.T) {
	h := startHab(t, "", socketPath(t), func(c *Config) { c.Version = "1.2.3" })
	if w := hello(t, dial(t, h.path), ""); w.Version != "1.2.3" {
		t.Fatalf("welcome version %q, want 1.2.3", w.Version)
	}
	h2 := startHab(t, "", socketPath(t), nil)
	if w := hello(t, dial(t, h2.path), ""); w.Version != "" {
		t.Fatalf("welcome version %q, want none", w.Version)
	}
}

func TestHelloRules(t *testing.T) {
	srv := New(testConfig(&fakeSource{}))
	t.Cleanup(srv.Close)
	cases := []struct {
		name, line, kind string
	}{
		{"not first", `{"jsonrpc":"2.0","id":1,"method":"structure.get"}`, "invalid"},
		{"too new", `{"jsonrpc":"2.0","id":1,"method":"hab.hello","params":{"revision":4,"min_revision":2,"client":{"name":"t","version":"0","kind":"cli"}}}`, "revision_unsupported"},
		{"bad kind", `{"jsonrpc":"2.0","id":1,"method":"hab.hello","params":{"revision":1,"min_revision":1,"client":{"name":"t","version":"0","kind":"robot"}}}`, "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := pipeServe(t, srv)
			go fmt.Fprintln(nc, tc.line)
			br := bufio.NewReader(nc)
			nc.SetReadDeadline(time.Now().Add(5 * time.Second))
			line, err := br.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var m message
			json.Unmarshal(line, &m)
			if m.Error == nil || string(m.Error.Data.Kind) != tc.kind {
				t.Fatalf("got %s", line)
			}
			if tc.kind == "revision_unsupported" {
				d := m.Error.Data
				if d.Server == nil || d.Client == nil || d.Server.Max != api.Revision || d.Client.Min != 2 {
					t.Fatalf("ranges missing: %s", line)
				}
			}
			if _, err := br.ReadBytes('\n'); err == nil {
				t.Fatal("connection stayed open")
			}
		})
	}
}

func TestScopeProjects(t *testing.T) {
	src := &fakeSource{st: api.Structure{
		Projects: []api.Project{{ID: "p1", Revision: 1, Sessions: []string{"s1"}}, {ID: "p2", Revision: 1, Sessions: []string{"s2"}}},
		Sessions: []api.Session{{ID: "s1", Revision: 1, Project: "p1", Pane: "x1"}, {ID: "s2", Revision: 1, Project: "p2", Pane: "x2"}},
		Panes:    []api.Pane{{ID: "x1", Revision: 1, Session: "s1"}, {ID: "x2", Revision: 1, Session: "s2"}, {ID: "x3", Revision: 1}},
	}}
	srv := New(testConfig(src))
	t.Cleanup(srv.Close)
	c := NewClient(pipeServe(t, srv))
	hello(t, c, "")
	r, err := (api.Client{Caller: c}).EventsSubscribe(ctx(t), api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindProjects, IDs: []string{"p1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Projects) != 1 || len(r.Sessions) != 1 || len(r.Panes) != 1 || r.Panes[0].ID != "x1" {
		t.Fatalf("scoped snapshot %+v", r.Structure)
	}
	pub := func(typ api.EventType, id string, data any) {
		srv.Publish(structure.Event{Type: typ, ID: id, Revision: 2, Data: data})
	}
	pub(structure.ProjectUpdated, "p2", api.Project{ID: "p2"})                // out
	pub(structure.PaneUpdated, "x2", api.Pane{ID: "x2", Session: "s2"})       // out
	pub(structure.SessionUpdated, "s2", api.Session{ID: "s2", Project: "p1"}) // moves in
	pub(structure.PaneUpdated, "x2", api.Pane{ID: "x2", Session: "s2"})       // now in
	pub(structure.SessionUpdated, "s1", api.Session{ID: "s1", Project: "p2"}) // leaves: one last
	pub(structure.SessionUpdated, "s1", api.Session{ID: "s1", Project: "p2"}) // out
	pub(structure.ProjectUpdated, "p1", api.Project{ID: "p1"})                // in
	pub(structure.PaneDeleted, "x3", api.Deleted{})                           // out
	ping, err := (api.Client{Caller: c}).HabPing(ctx(t), api.PingParams{})
	if err != nil {
		t.Fatal(err)
	}
	var events []api.Event
	collect(t, c, &events, ping.Seq.Seq)
	var got []string
	for _, e := range events {
		got = append(got, string(e.Type)+" "+e.ID)
	}
	want := []string{"session.updated s2", "pane.updated x2", "session.updated s1", "project.updated p1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", got, want)
	}
}

func TestHeartbeatAndIdleTimeout(t *testing.T) {
	srv := New(func() Config {
		c := testConfig(&fakeSource{})
		c.Heartbeat = 30 * time.Millisecond
		c.IdleTimeout = 300 * time.Millisecond
		return c
	}())
	t.Cleanup(srv.Close)
	c := NewClient(pipeServe(t, srv))
	hello(t, c, "")
	select {
	case n := <-c.Notifications():
		if n.Method != "heartbeat" {
			t.Fatalf("got %s", n.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat")
	}
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a silent client was not closed")
	}

	// A client that pings stays open.
	c2 := NewClient(pipeServe(t, srv))
	hello(t, c2, "")
	c2.KeepAlive(60 * time.Millisecond)
	select {
	case <-c2.Done():
		t.Fatalf("a pinging client was closed: %v", c2.Err())
	case <-time.After(time.Second):
	}
}

func TestListenPermissions(t *testing.T) {
	path := socketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode().Perm())
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("a live socket was taken over")
	}
	l.Close()

	// A stale socket is replaced; a regular file is never deleted.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	l, err = Listen(path)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	l.Close()
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("Listen took over a regular file")
	}
	if b, _ := os.ReadFile(path); string(b) != "keep" {
		t.Fatal("Listen deleted a regular file")
	}

	open := filepath.Join(filepath.Dir(filepath.Dir(path)), "open")
	os.Mkdir(open, 0o755)
	os.Chmod(open, 0o755)
	if _, err := Listen(filepath.Join(open, "hab.sock")); err == nil {
		t.Fatal("a socket in a directory others can enter was accepted")
	}
}

func TestDataHelloIsRejected(t *testing.T) {
	srv := New(testConfig(&fakeSource{}))
	t.Cleanup(srv.Close)
	nc := pipeServe(t, srv)
	go nc.Write([]byte{byte(api.FrameHello), 0, 0, 0, 2, '{', '}'})
	nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	hdr := make([]byte, 5)
	if _, err := ioReadFull(nc, hdr); err != nil {
		t.Fatal(err)
	}
	if hdr[0] != byte(api.FrameReject) {
		t.Fatalf("frame kind %d", hdr[0])
	}
}

type fakeData struct {
	served chan byte
	client string
}

func (d *fakeData) Ticket(client, pane string, observe bool) (string, time.Duration, error) {
	if pane != "p1" {
		return "", 0, Errorf(api.ErrorKindNotFound, "no pane %s", pane)
	}
	d.client = client
	return "tok", 30 * time.Second, nil
}

func (d *fakeData) ServeConn(nc net.Conn, br *bufio.Reader) {
	b, _ := br.ReadByte()
	d.served <- b
	nc.Close()
}

type fakeInput struct{ got chan string }

func (f *fakeInput) QueueInput(_ context.Context, client, pane string, data []byte) func() error {
	f.got <- client + "/" + pane + "/" + string(data)
	return func() error { return nil }
}

// TestDataAndInput: data.ticket and pane.input reach their services with the client id, and a
// connection that starts with a data hello goes to the data service.
func TestDataAndInput(t *testing.T) {
	cfg := testConfig(&fakeSource{})
	d := &fakeData{served: make(chan byte, 1)}
	in := &fakeInput{got: make(chan string, 1)}
	cfg.Data, cfg.Input = d, in
	srv := New(cfg)
	t.Cleanup(srv.Close)
	c := NewClient(pipeServe(t, srv))
	w := hello(t, c, "")
	var tk api.Ticket
	if err := c.Call(ctx(t), "data.ticket", api.TicketParams{Pane: "p1"}, &tk); err != nil {
		t.Fatal(err)
	}
	if tk.Ticket != "tok" || tk.ExpiresInMS != 30000 || d.client != w.ClientID {
		t.Errorf("ticket %+v for client %q", tk, d.client)
	}
	var ce *Error
	if err := c.Call(ctx(t), "data.ticket", api.TicketParams{Pane: "nope"}, &tk); !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindNotFound {
		t.Errorf("unknown pane: %v", err)
	}
	if err := c.Call(ctx(t), "pane.input", api.InputParams{Pane: "p1", Text: "hi"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-in.got; got != w.ClientID+"/p1/hi" {
		t.Errorf("input %q", got)
	}
	nc := pipeServe(t, srv)
	go nc.Write([]byte{byte(api.FrameHello), 0, 0, 0, 0})
	select {
	case b := <-d.served:
		if b != byte(api.FrameHello) {
			t.Errorf("data service read %d first", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the data hello never reached the data service")
	}
}

func ioReadFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		k, err := c.Read(b[n:])
		n += k
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func TestCallAfterCloseFails(t *testing.T) {
	srv := New(testConfig(&fakeSource{}))
	c := NewClient(pipeServe(t, srv))
	hello(t, c, "")
	srv.Close()
	<-c.Done()
	err := c.Call(ctx(t), "hab.ping", nil, nil)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

// stuckInput records each write as the program would see it. A write to pane "stuck" is taken
// and returns only 200 ms after its context ends, as a write already in flight does; a write
// queued behind it waits, and is never written when its context ends first.
type stuckInput struct {
	mu   sync.Mutex
	got  []string
	busy bool // a write to "stuck" is in flight
}

func (f *stuckInput) QueueInput(ctx context.Context, _, pane string, data []byte) func() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pane != "stuck" {
		f.got = append(f.got, pane+"/"+string(data))
		return func() error { return nil }
	}
	if f.busy {
		return func() error { <-ctx.Done(); return ctx.Err() }
	}
	f.busy = true
	f.got = append(f.got, pane+"/"+string(data))
	return func() error {
		<-ctx.Done()
		time.Sleep(200 * time.Millisecond)
		return ctx.Err()
	}
}

func (f *stuckInput) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// TestInputOffReadLoop: a pane that takes its input slowly holds up neither hab.ping nor input
// to another pane, and a call that timed out while it waited behind it is never written.
func TestInputOffReadLoop(t *testing.T) {
	cfg := testConfig(&fakeSource{})
	in := &stuckInput{}
	cfg.Input, cfg.ActionTimeout = in, 300*time.Millisecond
	srv := New(cfg)
	t.Cleanup(srv.Close)
	c := NewClient(pipeServe(t, srv))
	hello(t, c, "")

	errs := make(chan error, 2)
	go func() { errs <- c.Call(ctx(t), "pane.input", api.InputParams{Pane: "stuck", Text: "a"}, nil) }()
	for len(in.calls()) == 0 {
		time.Sleep(time.Millisecond)
	}
	go func() { errs <- c.Call(ctx(t), "pane.input", api.InputParams{Pane: "stuck", Text: "b"}, nil) }()

	start := time.Now()
	if err := c.Call(ctx(t), "hab.ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx(t), "pane.input", api.InputParams{Pane: "other", Text: "c"}, nil); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Errorf("ping and input to another pane took %v behind a stuck pane", took)
	}
	for range 2 {
		var ce *Error
		if err := <-errs; !errors.As(err, &ce) || ce.Data.Kind != api.ErrorKindUnavailable {
			t.Errorf("input to the stuck pane: %v", err)
		}
	}
	if got := in.calls(); !slices.Equal(got, []string{"stuck/a", "other/c"}) {
		t.Errorf("Input calls %v; the timed-out b must never be written", got)
	}
}

// writeSignal wraps a client's connection and reports each write, so a test can send calls in
// a known order without waiting for their replies.
type writeSignal struct {
	net.Conn
	wrote chan struct{}
}

func (w *writeSignal) Write(b []byte) (int, error) {
	n, err := w.Conn.Write(b)
	w.wrote <- struct{}{}
	return n, err
}

// heldInput queues each write at once and holds its result until release is closed.
type heldInput struct {
	mu      sync.Mutex
	got     []string
	release chan struct{}
}

func (f *heldInput) QueueInput(ctx context.Context, _, pane string, data []byte) func() error {
	f.mu.Lock()
	f.got = append(f.got, pane+"/"+string(data))
	f.mu.Unlock()
	return func() error { <-f.release; return nil }
}

func (f *heldInput) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// TestInputLanePipelined: a lane queues each pane.input call as soon as it is read, without
// waiting for the result of the call before it, and replies in order once results come.
func TestInputLanePipelined(t *testing.T) {
	cfg := testConfig(&fakeSource{})
	in := &heldInput{release: make(chan struct{})}
	cfg.Input = in
	srv := New(cfg)
	t.Cleanup(srv.Close)
	ws := &writeSignal{Conn: pipeServe(t, srv), wrote: make(chan struct{}, 16)}
	c := NewClient(ws)
	hello(t, c, "")
	<-ws.wrote

	type result struct {
		key string
		err error
	}
	results := make(chan result, 3)
	keys := []string{"l", "s", "\r"}
	for _, k := range keys {
		go func() { results <- result{k, c.Call(ctx(t), "pane.input", api.InputParams{Pane: "p", Text: k}, nil)} }()
		<-ws.wrote // in order: the next call goes out after this one
	}
	want := []string{"p/l", "p/s", "p/\r"}
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(in.calls(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("queued %q while no result came; want %q", in.calls(), want)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case r := <-results:
		t.Fatalf("a reply before any result: %+v", r)
	default:
	}
	close(in.release)
	for range keys {
		if r := <-results; r.err != nil {
			t.Errorf("%q: %v", r.key, r.err)
		}
	}
}

// TestCallExpiredContext: a call whose context is already over fails with the context's error
// and writes nothing. A write with a deadline in the past would fail as an i/o timeout and end
// the connection, so the next call would fail too.
func TestCallExpiredContext(t *testing.T) {
	srv := New(testConfig(&fakeSource{}))
	t.Cleanup(srv.Close)
	c := NewClient(pipeServe(t, srv))
	hello(t, c, "")

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := c.Call(expired, "hab.ping", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call with an expired context: %v; want %v", err, context.DeadlineExceeded)
	}
	if err := c.Call(ctx(t), "hab.ping", nil, nil); err != nil {
		t.Fatalf("the next call: %v", err)
	}
}
