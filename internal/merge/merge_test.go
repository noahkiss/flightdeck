package merge_test

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/fakehab"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/merge"
)

const remote = "example.com/o/app"

func waitLong() time.Duration {
	d := 15 * time.Second
	if s := os.Getenv("GFF_TEST_TIMEOUT_SCALE"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f <= 0 {
			panic(fmt.Sprintf("GFF_TEST_TIMEOUT_SCALE=%q: want a positive number", s))
		}
		d = time.Duration(float64(d) * f)
	}
	return d
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gffm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// startHab starts a hab with the shared project and one session in it.
func startHab(t *testing.T, dir, id string) *fakehab.Hab {
	t.Helper()
	h, err := fakehab.New(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	p, err := h.AddProject("app", remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.AddSessionIn(p, "work-"+id); err != nil {
		t.Fatal(err)
	}
	return h
}

func spec(label string, h *fakehab.Hab) hosts.Spec {
	return hosts.Spec{Label: label, Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: h.Sock}}}
}

// source is a host list a test edits.
type source struct {
	mu    sync.Mutex
	specs []hosts.Spec
}

func (s *source) Hosts() ([]hosts.Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.specs), nil
}

func (s *source) add(sp hosts.Spec) {
	s.mu.Lock()
	s.specs = append(s.specs, sp)
	s.mu.Unlock()
}

func open(t *testing.T, src hosts.Source) *merge.Merger {
	t.Helper()
	s, err := hosts.Open(hosts.Config{
		Source: src,
		Client: api.ClientInfo{Name: "test", Kind: api.ClientKindTerminal},
		Conn: func(c *habconn.Config) {
			c.MinBackoff, c.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return merge.New(s)
}

// log reads the merged stream and checks its seq has no gaps.
type log struct {
	t      *testing.T
	m      *merge.Merger
	events []merge.Event
}

// wait reads events until cond holds on the model. Every event is read, so the wait follows
// the stream's own order, never a timer.
func (l *log) wait(what string, cond func(merge.Model) bool) {
	l.t.Helper()
	timer := time.NewTimer(waitLong())
	defer timer.Stop()
	for !cond(l.m.Model()) {
		select {
		case e, ok := <-l.m.Events():
			if !ok {
				l.t.Fatalf("waiting for %s: events closed", what)
			}
			if want := uint64(len(l.events) + 1); e.Seq != want {
				l.t.Fatalf("event seq %d, want %d: %+v", e.Seq, want, e)
			}
			l.events = append(l.events, e)
		case <-timer.C:
			l.t.Fatalf("waiting for %s: timed out; model %+v", what, l.m.Model())
		}
	}
}

// waitEvent reads events until one read after mark matches. The model may run ahead of the
// stream, so a test that checks events waits for the event itself.
func (l *log) waitEvent(what string, mark int, match func(merge.Event) bool) {
	l.t.Helper()
	l.wait(what, func(merge.Model) bool { return slices.ContainsFunc(l.since(mark), match) })
}

// waitResync reads events until each host named has sent a resync since mark.
func (l *log) waitResync(mark int, labels ...string) {
	l.t.Helper()
	l.wait(fmt.Sprintf("resyncs of %v", labels), func(merge.Model) bool {
		n := resyncs(l.since(mark))
		for _, h := range labels {
			if n[h] == 0 {
				return false
			}
		}
		return true
	})
}

// settle reads until every event the named hosts sent so far has arrived: it adds a session on
// each and waits for that session's event. The stream keeps each host's order, so a resync or
// any other event the host sent earlier is read first. A test then checks l.since(mark) knowing
// nothing more from those hosts is on its way.
func (l *log) settle(habs map[string]*fakehab.Hab) {
	l.t.Helper()
	for label, h := range habs {
		mark := len(l.events)
		id, _, err := h.AddSession(fmt.Sprintf("settle-%d", mark))
		if err != nil {
			l.t.Fatal(err)
		}
		key := merge.Key{Host: label, Object: "session", ID: id}
		l.waitEvent("the settle session on "+label, mark, func(e merge.Event) bool {
			return e.Kind == merge.KindObject && e.Key == key
		})
	}
}

// since returns the events read after mark.
func (l *log) since(mark int) []merge.Event { return l.events[mark:] }

func resyncs(evs []merge.Event) map[string]int {
	n := map[string]int{}
	for _, e := range evs {
		if e.Kind == merge.KindResync {
			n[e.Key.Host]++
		}
	}
	return n
}

func status(m merge.Model, label string) hosts.Status {
	h, _ := m.Host(label)
	return h.Status
}

func locations(m merge.Model, key string) []string {
	for _, p := range m.Projects {
		if p.Key == key {
			var out []string
			for _, l := range p.Locations {
				out = append(out, l.Host)
			}
			return out
		}
	}
	return nil
}

func sessionsOf(m merge.Model, label string) []merge.Session {
	var out []merge.Session
	for _, s := range m.Sessions {
		if s.Host == label {
			out = append(out, s)
		}
	}
	return out
}

// TestThreeHabsOneStopped: three habs, one stopped when the client starts. The merged view
// shows the two that run, with the shared project at two locations. Starting the third adds
// it, with one resync for that host only. A fourth host added to the list joins the same way:
// a Reload starts only it, and no other host resyncs.
func TestThreeHabsOneStopped(t *testing.T) {
	d := shortDir(t)
	a := startHab(t, d, "a")
	b := startHab(t, d, "b")
	c := startHab(t, d, "c")
	c.Stop()
	src := &source{specs: []hosts.Spec{spec("one", a), spec("two", b), spec("three", c)}}
	m := open(t, src)
	l := &log{t: t, m: m}
	key := "remote:" + remote

	l.wait("one and two merged, three trying", func(m merge.Model) bool {
		h, _ := m.Host("three")
		return status(m, "one") == hosts.Online && status(m, "two") == hosts.Online &&
			slices.Equal(locations(m, key), []string{"one", "two"}) && h.Err != nil
	})
	l.waitResync(0, "one", "two")
	mod := m.Model()
	if len(mod.Projects) != 1 || len(mod.Sessions) != 2 {
		t.Fatalf("model: %d projects, %d sessions; want 1 and 2: %+v", len(mod.Projects), len(mod.Sessions), mod)
	}
	for _, s := range mod.Sessions {
		if s.Project != key || s.Stale || !mod.Input(s.Host) || s.Session.Name != "work-"+map[string]string{"one": "a", "two": "b"}[s.Host] {
			t.Fatalf("session %+v", s)
		}
	}
	if h, _ := mod.Host("three"); h.Stale() || h.Input() {
		t.Fatalf("three never connected: stale %v, input %v; want neither", h.Stale(), h.Input())
	}
	before := map[string][]merge.Session{"one": sessionsOf(mod, "one"), "two": sessionsOf(mod, "two")}

	mark := len(l.events)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	l.wait("three added", func(m merge.Model) bool {
		return status(m, "three") == hosts.Online && slices.Equal(locations(m, key), []string{"one", "two", "three"}) &&
			len(sessionsOf(m, "three")) == 1
	})
	l.waitResync(mark, "three")
	for label, want := range before {
		if got := sessionsOf(m.Model(), label); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed when three came up:\n got %+v\nwant %+v", label, got, want)
		}
	}
	l.settle(map[string]*fakehab.Hab{"one": a, "two": b, "three": c})
	if got := resyncs(l.since(mark)); !reflect.DeepEqual(got, map[string]int{"three": 1}) {
		t.Fatalf("resyncs after starting three: %v; want three once", got)
	}

	e := startHab(t, d, "e")
	mark = len(l.events)
	src.add(spec("four", e))
	if err := m.Set().Reload(); err != nil {
		t.Fatal(err)
	}
	l.wait("four added", func(m merge.Model) bool {
		return status(m, "four") == hosts.Online && len(locations(m, key)) == 4
	})
	l.waitResync(mark, "four")
	l.settle(map[string]*fakehab.Hab{"one": a, "two": b, "three": c, "four": e})
	if got := resyncs(l.since(mark)); !reflect.DeepEqual(got, map[string]int{"four": 1}) {
		t.Fatalf("resyncs after adding four: %v; want four once", got)
	}
}

// TestRestartResyncsOneHost: three habs online. One stops: its objects stay, stale, and take no
// input, while the others keep working. It starts again with a new epoch: one resync with
// NewEpoch for that host, and the other hosts' objects are the same values as before.
func TestRestartResyncsOneHost(t *testing.T) {
	d := shortDir(t)
	a := startHab(t, d, "a")
	b := startHab(t, d, "b")
	c := startHab(t, d, "c")
	m := open(t, hosts.Static{spec("one", a), spec("two", b), spec("three", c)})
	l := &log{t: t, m: m}
	l.wait("all online", func(m merge.Model) bool {
		return status(m, "one") == hosts.Online && status(m, "two") == hosts.Online && status(m, "three") == hosts.Online
	})
	l.waitResync(0, "one", "two", "three")
	mod := m.Model()
	others := map[string][]merge.Session{"one": sessionsOf(mod, "one"), "three": sessionsOf(mod, "three")}
	twoBefore := sessionsOf(mod, "two")
	epoch := b.Epoch()

	mark := len(l.events)
	b.Stop()
	l.wait("two stale", func(m merge.Model) bool { h, _ := m.Host("two"); return h.Stale() })
	mod = m.Model()
	if mod.Input("two") || !mod.Input("one") || !mod.Input("three") {
		t.Fatalf("input: one %v two %v three %v; want only two off", mod.Input("one"), mod.Input("two"), mod.Input("three"))
	}
	stale := sessionsOf(mod, "two")
	if len(stale) != 1 || !stale[0].Stale || stale[0].Session.ID != twoBefore[0].Session.ID {
		t.Fatalf("two's cached sessions: %+v", stale)
	}
	for _, p := range mod.Projects {
		for _, loc := range p.Locations {
			if loc.Stale != (loc.Host == "two") {
				t.Fatalf("location %s stale %v", loc.Host, loc.Stale)
			}
		}
	}
	if err := m.Set().Hosts()[1].Conn.Call(t.Context(), "hab.ping", nil, nil); !errors.Is(err, habconn.ErrOffline) {
		t.Fatalf("a call to the stale host: %v; want ErrOffline", err)
	}

	// The others keep going while two is down: an event on one arrives keyed by its host.
	_, pane, err := a.AddSession("more")
	if err != nil {
		t.Fatal(err)
	}
	a.SetAgent(pane, api.AgentStatusWorking, "")
	agentKey := merge.Key{Host: "one", Object: "agent", ID: pane}
	l.waitEvent("one's agent", mark, func(e merge.Event) bool { return e.Kind == merge.KindObject && e.Key == agentKey })
	if len(sessionsOf(m.Model(), "one")) != 2 {
		t.Fatalf("one's sessions: %+v", sessionsOf(m.Model(), "one"))
	}
	for _, e := range l.since(mark) {
		if e.Kind == merge.KindObject && e.Key.Host != "one" {
			t.Fatalf("an object event from %s while only one changed: %+v", e.Key.Host, e)
		}
	}
	others["one"] = sessionsOf(m.Model(), "one")

	mark = len(l.events)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if b.Epoch() == epoch {
		t.Fatal("restart kept the epoch")
	}
	l.wait("two fresh", func(m merge.Model) bool { return m.Input("two") })
	l.waitResync(mark, "two")
	mod = m.Model()
	if h, _ := mod.Host("two"); h.Epoch != b.Epoch() || h.Stale() {
		t.Fatalf("two after restart: %+v", h)
	}
	if got := sessionsOf(mod, "two"); len(got) != 1 || got[0].Stale || got[0].Session.ID != twoBefore[0].Session.ID {
		t.Fatalf("two's sessions after restart: %+v", got)
	}
	for label, want := range others {
		if got := sessionsOf(mod, label); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed when two restarted:\n got %+v\nwant %+v", label, got, want)
		}
	}
	l.settle(map[string]*fakehab.Hab{"one": a, "two": b, "three": c})
	evs := l.since(mark)
	if got := resyncs(evs); !reflect.DeepEqual(got, map[string]int{"two": 1}) {
		t.Fatalf("resyncs after the restart: %v; want two once", got)
	}
	for _, e := range evs {
		if e.Kind == merge.KindResync && !e.NewEpoch {
			t.Fatalf("two's resync has no NewEpoch: %+v", e)
		}
	}
}

// TestBuild: the merge rules on hand-made host models.
func TestBuild(t *testing.T) {
	st := func(host string, projects []api.Project, sessions ...api.Session) api.Structure {
		return api.Structure{Host: api.HostInfo{ID: host}, Projects: projects, Sessions: sessions,
			Panes: []api.Pane{{ID: host + "-p1", Session: host + "-s1"}, {ID: host + "-loose"}}}
	}
	app := func(id string) api.Project {
		return api.Project{ID: id, Name: "app", Remote: remote, Sessions: []string{"x"}}
	}
	welcome := func(dev, epoch string) api.Welcome {
		return api.Welcome{DeviceID: dev, Epoch: epoch}
	}
	hs := []hosts.Host{
		{Label: "one", Status: hosts.Online, Welcome: welcome("d1", "e1"),
			Structure: st("h1", []api.Project{app("p1"), {ID: "p2", Name: "loose", RootCommit: "abc"}},
				api.Session{ID: "h1-s1", Project: "p1", Pane: "h1-p1"})},
		{Label: "two", Status: hosts.Offline, Welcome: welcome("d2", "e2"),
			Structure: st("h2", []api.Project{app("q1"), {ID: "q2", Name: "solo"}}, api.Session{ID: "h2-s1", Project: "q2", Pane: "h2-p1"})},
		// A second label for d1, offline: hidden.
		{Label: "alias", Status: hosts.Offline, Welcome: welcome("d1", "e1"), Structure: st("h1", []api.Project{app("p1")})},
		// Failed: hidden, not stale.
		{Label: "bad", Status: hosts.Failed, Welcome: welcome("d3", "e3"), Structure: st("h3", []api.Project{app("r1")})},
		{Label: "new", Status: hosts.Connecting},
	}
	m := merge.Build(hs)
	shown := map[string]bool{}
	for _, h := range m.Hosts {
		shown[h.Label] = h.Shown
	}
	if want := map[string]bool{"one": true, "two": true, "alias": false, "bad": false, "new": true}; !reflect.DeepEqual(shown, want) {
		t.Fatalf("shown %v, want %v", shown, want)
	}
	var keys []string
	for _, p := range m.Projects {
		keys = append(keys, p.Key)
	}
	if want := []string{"remote:" + remote, "root:abc", "host:two/q2"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("projects %v, want %v", keys, want)
	}
	if got := m.Projects[0].Locations; len(got) != 2 || got[0].Host != "one" || got[0].Stale || got[1].Host != "two" || !got[1].Stale {
		t.Fatalf("app locations %+v", got)
	}
	if len(m.Sessions) != 2 || m.Sessions[0].Ref != (merge.Ref{Host: "one", ID: "h1-s1"}) || m.Sessions[0].Pane == nil ||
		m.Sessions[1].Project != "host:two/q2" || !m.Sessions[1].Stale {
		t.Fatalf("sessions %+v", m.Sessions)
	}
	if len(m.Unfiled) != 2 || m.Unfiled[0].Ref != (merge.Ref{Host: "one", ID: "h1-loose"}) || !m.Unfiled[1].Stale {
		t.Fatalf("unfiled %+v", m.Unfiled)
	}
	two, _ := m.Host("two")
	nw, _ := m.Host("new")
	if !two.Stale() || two.Input() || nw.Stale() || nw.Input() || !m.Input("one") {
		t.Fatalf("stale and input: two %v/%v, new %v/%v, one input %v", two.Stale(), two.Input(), nw.Stale(), nw.Input(), m.Input("one"))
	}
}

// TestReplaceKeepsCache: a host that is down keeps its rows, stale, when a Reload changes its
// endpoints. The new connection starts with the old one's cache, takes no input, and replaces
// the cache with its own snapshot when the hab comes back (merge review S2).
func TestReplaceKeepsCache(t *testing.T) {
	d := shortDir(t)
	a := startHab(t, d, "a")
	b := startHab(t, d, "b")
	src := &source{specs: []hosts.Spec{spec("one", a), spec("two", b)}}
	m := open(t, src)
	l := &log{t: t, m: m}
	l.wait("both online", func(m merge.Model) bool { return m.Input("one") && m.Input("two") })
	want := sessionsOf(m.Model(), "two")
	epoch := b.Epoch()

	b.Stop()
	l.wait("two stale", func(m merge.Model) bool { h, _ := m.Host("two"); return h.Stale() })
	edited := spec("two", b)
	edited.Endpoints = append(edited.Endpoints, hosts.Endpoint{Kind: hosts.EndpointSocket, Value: d + "/other.sock"})
	src.mu.Lock()
	src.specs[1] = edited
	src.mu.Unlock()
	mark := len(l.events)
	if err := m.Set().Reload(); err != nil {
		t.Fatal(err)
	}
	l.waitEvent("two replaced", mark, func(e merge.Event) bool {
		return e.Kind == merge.KindHost && e.Key.Host == "two" && errors.Is(e.Err, hosts.ErrSpecChanged)
	})
	mod := m.Model()
	h, _ := mod.Host("two")
	if h.Status == hosts.Online || !h.Stale() || h.Input() || h.Epoch != epoch {
		t.Fatalf("two after the edit: %+v, stale %v, input %v; want stale under epoch %s, no input", h, h.Stale(), h.Input(), epoch)
	}
	got := sessionsOf(mod, "two")
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("two's sessions after the edit: %+v; want the cached %+v", got, want)
	}
	for i := range got {
		if !got[i].Stale || got[i].Session.ID != want[i].Session.ID {
			t.Fatalf("two's session after the edit: %+v; want %s, stale", got[i], want[i].Session.ID)
		}
	}

	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	l.wait("two fresh", func(m merge.Model) bool { return m.Input("two") })
	if h, _ := m.Model().Host("two"); h.Epoch != b.Epoch() || h.Stale() || h.Epoch == epoch {
		t.Fatalf("two after it came back: %+v", h)
	}
}
