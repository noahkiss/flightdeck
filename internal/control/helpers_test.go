package control

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// socketPath returns a socket path short enough for macOS (104 bytes) in a private directory.
func socketPath(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gffc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(d, "s", "hab.sock")
}

// hab is a server with a real structure store on a fake holder, serving one socket.
type hab struct {
	t      *testing.T
	srv    *Server
	store  *structure.Store
	holder *structure.FakeHolder
	path   string
	dir    string
	served chan struct{}
}

func testConfig(src Source) Config {
	return Config{
		Build:    "test",
		Host:     api.HostInfo{ID: "host-a"},
		DeviceID: "SHA256:test",
		Source:   src,
	}
}

// startHab opens a store in dir (made when empty) and serves path.
func startHab(t *testing.T, dir, path string, tweak func(*Config)) *hab {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	h := &hab{t: t, holder: structure.NewFakeHolder(), path: path, dir: dir, served: make(chan struct{})}
	var pub atomic.Pointer[Server]
	st, err := structure.Open(structure.Config{
		Dir:     dir,
		Holders: h.holder,
		OnEvent: func(e structure.Event) {
			if s := pub.Load(); s != nil {
				s.Publish(e)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.store = st
	cfg := testConfig(st)
	if tweak != nil {
		tweak(&cfg)
	}
	h.srv = New(cfg)
	pub.Store(h.srv)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(h.served)
		if err := h.srv.Serve(l); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *hab) stop() {
	if h.srv == nil {
		return
	}
	h.srv.Close()
	<-h.served
	h.store.Close()
	h.srv = nil
}

func dial(t *testing.T, path string) *Client {
	t.Helper()
	c, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

var testClient = api.ClientInfo{Name: "control-test", Version: "0", Kind: api.ClientKindScript}

func hello(t *testing.T, c *Client, token string) api.Welcome {
	t.Helper()
	w, err := c.Hello(ctx(t), testClient, "", token)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// mirror is a client's copy of the structure, kept by the seq and revision rules (spec 3.5).
type mirror struct {
	seq     uint64 // events with this seq or lower are dropped
	objects map[string]map[string]json.RawMessage
	revs    map[string]uint64
	dropped int
}

func newMirror() *mirror {
	return &mirror{objects: map[string]map[string]json.RawMessage{}, revs: map[string]uint64{}}
}

func (m *mirror) snapshot(t *testing.T, r api.StructureResult) {
	t.Helper()
	m.seq = r.Seq.Seq
	m.objects = map[string]map[string]json.RawMessage{"project": {}, "session": {}, "pane": {}}
	m.revs = map[string]uint64{}
	put := func(kind, id string, rev uint64, v any) {
		b, _ := json.Marshal(v)
		m.objects[kind][id] = b
		m.revs[kind+"/"+id] = rev
	}
	for _, p := range r.Projects {
		put("project", p.ID, p.Revision, p)
	}
	for _, s := range r.Sessions {
		put("session", s.ID, s.Revision, s)
	}
	for _, p := range r.Panes {
		put("pane", p.ID, p.Revision, p)
	}
}

func (m *mirror) apply(e api.Event) {
	if e.Seq <= m.seq {
		m.dropped++
		return
	}
	m.seq = e.Seq
	kind, verb := splitType(string(e.Type))
	key := kind + "/" + e.ID
	if e.Revision != nil && *e.Revision <= m.revs[key] {
		return
	}
	if e.Revision != nil {
		m.revs[key] = *e.Revision
	}
	switch verb {
	case "updated":
		m.objects[kind][e.ID] = e.Data
	case "deleted":
		delete(m.objects[kind], e.ID)
	}
}

func splitType(s string) (string, string) {
	for i := range s {
		if s[i] == '.' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

// sameAs compares the mirror with a structure, object by object.
func (m *mirror) sameAs(t *testing.T, st api.Structure) {
	t.Helper()
	other := newMirror()
	other.snapshot(t, api.StructureResult{Structure: st})
	for kind, objs := range other.objects {
		if len(objs) != len(m.objects[kind]) {
			t.Errorf("%s: mirror has %d, hab has %d", kind, len(m.objects[kind]), len(objs))
		}
		for id, want := range objs {
			if got := m.objects[kind][id]; !jsonEqual(got, want) {
				t.Errorf("%s %s: mirror %s, hab %s", kind, id, got, want)
			}
		}
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ab, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return string(ab) == string(bb)
}

// fakeSource is a fixed structure for tests that publish events by hand.
type fakeSource struct {
	mu sync.Mutex
	st api.Structure
}

func (f *fakeSource) Snapshot(host api.HostInfo) api.Structure {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.st
	st.Host = host
	return api.NonNil(st)
}

// pipeHab serves one in-memory connection and returns the client end.
func pipeServe(t *testing.T, srv *Server) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	go srv.ServeConn(a)
	t.Cleanup(func() { b.Close() })
	return b
}
