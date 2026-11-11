package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

// clock is a settable Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// fakePeers is a peer list and links the test sets.
type fakePeers struct {
	mu    sync.Mutex
	list  api.PeerList
	links []Link
}

func (f *fakePeers) List() api.PeerList {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list
}

func (f *fakePeers) Links() []Link {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.links)
}

func (f *fakePeers) set(list api.PeerList, links ...Link) {
	f.mu.Lock()
	f.list, f.links = list, links
	f.mu.Unlock()
}

// fakeCaller is a peer's catalog methods.
type fakeCaller struct {
	mu        sync.Mutex
	cat       api.Catalog
	puts      int
	gets      []uint64 // the since of each get
	failGet   bool
	putFailed bool
}

func (f *fakeCaller) Call(_ context.Context, method string, params, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "catalog.put":
		f.puts++
		if f.putFailed {
			return errors.New("put refused")
		}
		return nil
	case "catalog.get":
		p := params.(api.CatalogGetParams)
		f.gets = append(f.gets, p.Since)
		if f.failGet {
			return errors.New("get refused")
		}
		r := result.(*api.CatalogGetResult)
		r.Generation = f.cat.Generation
		if p.Since != f.cat.Generation {
			c := f.cat
			r.Catalog = &c
		}
		return nil
	}
	return errors.New("no method " + method)
}

func (f *fakeCaller) counts() (puts, gets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, len(f.gets)
}

// suspended is base moved on by wall on the wall clock and by mono on the monotonic clock, as
// after a sleep of the host: Linux and macOS stop the monotonic clock while asleep. It writes
// time.Time's monotonic field directly; ok is false when that layout is not the one expected.
func suspended(base time.Time, wall, mono time.Duration) (t time.Time, ok bool) {
	type layout struct {
		wall uint64
		ext  int64
		loc  *time.Location
	}
	if unsafe.Sizeof(base) != unsafe.Sizeof(layout{}) {
		return time.Time{}, false
	}
	t = base.Add(wall)
	(*layout)(unsafe.Pointer(&t)).ext = (*layout)(unsafe.Pointer(&base)).ext + int64(mono)
	return t, t.Sub(base) == mono && t.Round(0).Sub(base.Round(0)) == wall
}

const (
	idM = "SHA256:mmmm" // the host marked always on
	idA = "SHA256:aaaa"
	idB = "SHA256:bbbb"
)

// alertHab is a Service with no goroutines that watches idM, for driving checkAlwaysOn by hand.
func alertHab(t *testing.T, self string, clk *clock, fp *fakePeers) (*Service, func() []api.NoticeRaiseParams) {
	t.Helper()
	var mu sync.Mutex
	var notices []api.NoticeRaiseParams
	s := newService(Config{Self: self, Now: clk.now, Peers: fp, Pull: 15 * time.Second,
		Notify: func(p api.NoticeRaiseParams) { mu.Lock(); notices = append(notices, p); mu.Unlock() }})
	s.copies[idM] = &copyRec{Catalog: api.Catalog{DeviceID: idM, Host: "m", AlwaysOn: true}}
	s.own.Peers, s.own.Watches = []string{idM}, []string{idM}
	return s, func() []api.NoticeRaiseParams { mu.Lock(); defer mu.Unlock(); return slices.Clone(notices) }
}

func peerM(online bool, lastSeen time.Time) api.Peer {
	st := api.PeerStateOffline
	if online {
		st = api.PeerStateOnline
	}
	return api.Peer{Label: "m", DeviceID: idM, State: st, LastSeen: lastSeen.UTC().Format(time.RFC3339)}
}

// TestSleepGap (review M1): a host that slept is not counted as watching. The marked host was
// last seen as this one went to sleep; on wake it is not back yet. Without the sleep seen, the
// two hours asleep count as two hours offline and the alert fires at once.
func TestSleepGap(t *testing.T) {
	for _, c := range []struct {
		name string
		wake func(at time.Time) (time.Time, bool)
	}{
		// The monotonic clock stood still while the wall clock moved two hours.
		{"suspend", func(at time.Time) (time.Time, bool) { return suspended(at, 2*time.Hour, 15*time.Second) }},
		// Both clocks jumped: a long stall, or a test Now with no monotonic reading.
		{"jump", func(at time.Time) (time.Time, bool) { return at.Round(0).Add(2 * time.Hour), true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t0 := time.Now() // with a monotonic reading, as in production
			clk := &clock{t: t0}
			fp := &fakePeers{}
			s, notices := alertHab(t, idA, clk, fp)
			tick := func(at time.Time, online bool, seen time.Time) {
				clk.set(at)
				fp.set(api.PeerList{Peers: []api.Peer{peerM(online, seen)}})
				s.checkAlwaysOn(fp.List())
			}
			up := t0.Add(time.Hour)
			tick(up.Add(-15*time.Second), true, up.Add(-15*time.Second))
			tick(up, true, up)
			wake, ok := c.wake(up)
			if !ok {
				t.Skip("time.Time's layout is not the one this test edits")
			}
			tick(wake, false, up)
			if n := notices(); len(n) != 0 {
				t.Fatalf("alert on wake: %+v", n)
			}
			// The watch starts again at wake: not before the threshold, once after it. Rounds
			// come every poll period.
			for d := 15 * time.Second; d <= 40*time.Minute; d += 15 * time.Second {
				tick(wake.Add(d), false, up)
				if n := notices(); d < 30*time.Minute && len(n) != 0 {
					t.Fatalf("alert %v after wake, before the threshold: %+v", d, n)
				}
			}
			if n := notices(); len(n) != 1 || n[0].Title != "m is offline" {
				t.Fatalf("notices %+v", n)
			}
		})
	}
}

// TestBrokenConfigAtStart (review M2): a config file that does not parse at start keeps the
// default alert threshold; later, it keeps the last good section.
func TestBrokenConfigAtStart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hab.toml")
	write(t, p, "[catalog\n")
	s := newService(Config{Self: idA, ConfigPath: p})
	s.loadConfig()
	if s.declared.AlertAfter != DefaultAlertAfter || len(s.Problems()) != 1 {
		t.Fatalf("broken at start: %+v %q", s.declared, s.Problems())
	}
	write(t, p, "[catalog]\nalways_on = true\nalways_on_alert = \"1m\"\n")
	s.loadConfig()
	if s.declared.AlertAfter != time.Minute || !s.declared.AlwaysOn || len(s.Problems()) != 0 {
		t.Fatalf("good: %+v %q", s.declared, s.Problems())
	}
	write(t, p, "[catalog\n")
	s.loadConfig()
	if s.declared.AlertAfter != time.Minute || !s.declared.AlwaysOn || len(s.Problems()) != 1 {
		t.Fatalf("broken after a good read: %+v %q", s.declared, s.Problems())
	}
}

// TestWatches (review S3): the catalog publishes the marked peers this hab watches, and a hab
// defers the alert only to a lower hab that watches the marked one, not to one that only
// lists it.
func TestWatches(t *testing.T) {
	fp := &fakePeers{}
	fp.set(api.PeerList{}, Link{Label: "m", DeviceID: idM}, Link{Label: "x", DeviceID: "SHA256:xxxx"}, Link{Label: "y"})
	s := newService(Config{Self: idA, Peers: fp})
	s.copies[idM] = &copyRec{Catalog: api.Catalog{DeviceID: idM, AlwaysOn: true}}
	s.copies["SHA256:xxxx"] = &copyRec{Catalog: api.Catalog{DeviceID: "SHA256:xxxx"}}
	s.copies["SHA256:zzzz"] = &copyRec{Catalog: api.Catalog{DeviceID: "SHA256:zzzz", AlwaysOn: true}} // not a peer of this hab
	s.build(true)
	if !slices.Equal(s.own.Watches, []string{idM}) || len(s.own.Peers) != 2 {
		t.Fatalf("own catalog: peers %v, watches %v", s.own.Peers, s.own.Watches)
	}

	for _, c := range []struct {
		name        string
		lower       api.Catalog // a's copy, online
		ownWatches  []string
		wantAlerted bool
	}{
		{"lower hab lists m but holds no marked copy", api.Catalog{DeviceID: idA, Peers: []string{idM}}, []string{idM}, true},
		{"lower hab watches m", api.Catalog{DeviceID: idA, Peers: []string{idM}, Watches: []string{idM}}, []string{idM}, false},
		{"this hab does not watch m", api.Catalog{DeviceID: idA}, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t0 := time.Now().Round(0)
			clk := &clock{t: t0}
			fp := &fakePeers{}
			b, notices := alertHab(t, idB, clk, fp)
			b.own.Watches = c.ownWatches
			b.copies[idA] = &copyRec{Catalog: c.lower}
			clk.set(t0.Add(31 * time.Minute))
			fp.set(api.PeerList{Peers: []api.Peer{peerM(false, t0.Add(-time.Hour)), {Label: "a", DeviceID: idA, State: api.PeerStateOnline}}})
			b.checkAlwaysOn(fp.List())
			if got := len(notices()) == 1; got != c.wantAlerted {
				t.Fatalf("alerted %v, want %v", got, c.wantAlerted)
			}
		})
	}
}

// listHook is a peer list that runs a function each time a round reads the list.
type listHook struct {
	*fakePeers
	hook func()
}

func (l listHook) List() api.PeerList {
	l.hook()
	return l.fakePeers.List()
}

// TestPruneKeepsMidRoundPut: a link that comes up during a round, after the round read the
// links, hands over its catalog; that round's prune keeps the copy, and a later round that
// sees neither the link nor a peer drops it.
func TestPruneKeepsMidRoundPut(t *testing.T) {
	fp := &fakePeers{}
	put := true
	var s *Service
	s = newService(Config{Self: idA, Peers: listHook{fp, func() {
		if put {
			put = false
			if err := s.Put(api.PeerHello{DeviceID: idB}, api.Catalog{DeviceID: idB, Generation: 3}); err != nil {
				t.Error(err)
			}
		}
	}}})
	s.round(context.Background())
	if _, ok := s.copies[idB]; !ok {
		t.Fatal("the round dropped a copy handed over after it read the links")
	}
	s.round(context.Background())
	if _, ok := s.copies[idB]; ok {
		t.Fatal("a copy of a hab neither listed nor linked stayed")
	}
}

// TestCheckPulled: a pulled catalog must be the linked hab's own.
func TestCheckPulled(t *testing.T) {
	s := newService(Config{Self: idA})
	for _, c := range []struct {
		link, cat string
		ok        bool
	}{
		{idB, idB, true},
		{idB, "SHA256:other", false},
		{idB, idA, false}, // this hab's own catalog
		{idA, idA, false}, // a link that reached this hab itself
		{idB, "", false},
		{"", idB, false}, // a link with no device id
	} {
		if err := s.checkPulled(Link{Label: "b", DeviceID: c.link}, api.Catalog{DeviceID: c.cat}); (err == nil) != c.ok {
			t.Errorf("link %q catalog %q: %v", c.link, c.cat, err)
		}
	}
}

// TestRePut: a link gets the catalog again after the peer's catalog changed (it may have lost
// this hab's copy in a restart) and after the link was down, and not otherwise.
func TestRePut(t *testing.T) {
	fc := &fakeCaller{cat: api.Catalog{DeviceID: idB, Host: "b", Generation: 1}}
	fp := &fakePeers{}
	link := Link{Label: "b", DeviceID: idB, Online: true, Caps: []string{Capability}, Caller: fc}
	list := api.PeerList{Peers: []api.Peer{{Label: "b", DeviceID: idB, State: api.PeerStateOnline}}}
	fp.set(list, link)
	s := newService(Config{Self: idA, Peers: fp})
	s.gen, s.own = 5, api.Catalog{DeviceID: idA, Generation: 5}
	ctx := context.Background()
	want := func(step string, puts, gets int) {
		t.Helper()
		if p, g := fc.counts(); p != puts || g != gets {
			t.Fatalf("%s: %d puts and %d gets, want %d and %d", step, p, g, puts, gets)
		}
	}
	s.round(ctx)
	want("first round", 1, 1)
	s.round(ctx)
	want("after the peer's catalog was new", 2, 2)
	s.round(ctx)
	want("nothing changed", 2, 3)
	link.Online = false
	fp.set(list, link)
	s.round(ctx)
	want("link down", 2, 3)
	link.Online = true
	fp.set(list, link)
	s.round(ctx)
	want("link back", 3, 4)
	fc.mu.Lock()
	fc.cat.Generation = 2
	fc.mu.Unlock()
	s.round(ctx)
	want("the peer changed", 3, 5)
	s.round(ctx)
	want("put after the peer changed", 4, 6)
	if e, _ := entry(s.List(), idB); e.Catalog.Generation != 2 {
		t.Fatalf("copy %+v", e.Catalog)
	}
}

// TestBackoff: a link whose catalog calls fail waits one poll period, then twice that, and
// so on; one that answers again is worked every round.
func TestBackoff(t *testing.T) {
	fc := &fakeCaller{cat: api.Catalog{DeviceID: idB, Generation: 1}, failGet: true}
	fp := &fakePeers{}
	fp.set(api.PeerList{Peers: []api.Peer{{Label: "b", DeviceID: idB, State: api.PeerStateOnline}}},
		Link{Label: "b", DeviceID: idB, Online: true, Caps: []string{Capability}, Caller: fc})
	t0 := time.Now().Round(0)
	clk := &clock{t: t0}
	var logs []string
	var mu sync.Mutex
	s := newService(Config{Self: idA, Peers: fp, Now: clk.now, Pull: 15 * time.Second,
		Logf: func(f string, a ...any) { mu.Lock(); logs = append(logs, f); mu.Unlock() }})
	ctx := context.Background()
	gets := func() int { _, g := fc.counts(); return g }
	for _, step := range []struct {
		after time.Duration
		gets  int
	}{
		{0, 1}, {time.Second, 1}, {15 * time.Second, 2}, {30 * time.Second, 2}, {45 * time.Second, 3},
		{60 * time.Second, 3}, {104 * time.Second, 3}, {106 * time.Second, 4},
	} {
		clk.set(t0.Add(step.after))
		s.round(ctx)
		if g := gets(); g != step.gets {
			t.Fatalf("at %v: %d gets, want %d", step.after, g, step.gets)
		}
	}
	fc.mu.Lock()
	fc.failGet = false
	fc.mu.Unlock()
	clk.set(t0.Add(10 * time.Minute))
	s.round(ctx)
	s.round(ctx)
	if g := gets(); g != 6 {
		t.Fatalf("after recovery: %d gets", g)
	}
	// A failed put is backed off the same way, and logs once per longer wait.
	fc.mu.Lock()
	fc.putFailed = true
	fc.mu.Unlock()
	s.mu.Lock()
	s.gen++
	s.mu.Unlock()
	mu.Lock()
	logs = nil
	mu.Unlock()
	for i := range 20 {
		clk.set(t0.Add(time.Hour + time.Duration(i)*5*time.Minute))
		s.round(ctx)
	}
	mu.Lock()
	n := len(logs)
	mu.Unlock()
	// Waits 15s, 30s, 1m, 2m, 4m, then 5m from there on: six different waits.
	if p, _ := fc.counts(); p < 15 || n != 6 {
		t.Fatalf("%d puts, %d log lines", p, n)
	}
}

// TestPutDoesNotStick (review S4): a catalog.put that claims a peer's current generation is
// replaced at the next pull over this hab's own link.
func TestPutDoesNotStick(t *testing.T) {
	d := shortDir(t)
	b := startHab(t, d, "b", idB, "", habOpts{})
	a := startHab(t, d, "a", idA, peersFile([2]string{"b", b.sock}), habOpts{})
	wait(t, "a holds b's catalog", func() bool {
		e, ok := entry(a.cat.List(), b.id)
		return ok && !e.Stale && e.Catalog.Host == "b"
	})
	c, err := control.Dial(a.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.HelloAs(ctx, api.HelloParams{Client: api.ClientInfo{Name: "t", Version: "0", Kind: api.ClientKindHab},
		Peer: &api.PeerHello{DeviceID: b.id, Host: "b"}}); err != nil {
		t.Fatal(err)
	}
	forged := api.Catalog{DeviceID: b.id, Host: "forged", Generation: b.cat.Get(0).Generation}
	if _, err := (api.Client{Caller: c}).CatalogPut(ctx, api.CatalogPutParams{Catalog: forged}); err != nil {
		t.Fatal(err)
	}
	wait(t, "a's own pull replaces the put", func() bool {
		e, _ := entry(a.cat.List(), b.id)
		return e.Catalog.Host == "b"
	})
}

// TestGenerationAboveEarlierLife (review S5): a hab that lost or broke catalog.json starts
// above every generation of its earlier life, so no puller keeps its old copy.
func TestGenerationAboveEarlierLife(t *testing.T) {
	for _, loss := range []string{"corrupt", "deleted"} {
		t.Run(loss, func(t *testing.T) {
			state := t.TempDir()
			clk := &clock{t: time.Now()}
			var mu sync.Mutex
			agents := []api.AgentState{}
			cfg := Config{Self: idA, State: state, Now: clk.now, Debounce: time.Millisecond,
				Detect: func(context.Context) Facts { return Facts{} },
				Structure: func() api.Structure {
					mu.Lock()
					defer mu.Unlock()
					return api.Structure{Agents: slices.Clone(agents)}
				}}
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			// A few changes in a row raise the generation by one each.
			for i := range 3 {
				g := s.Get(0).Generation
				mu.Lock()
				agents = append(agents, api.AgentState{Pane: "p" + string(rune('0'+i)), Harness: "claude", State: api.AgentStatusWorking})
				mu.Unlock()
				s.Poke()
				wait(t, "a new generation", func() bool { return s.Get(0).Generation > g })
			}
			g1 := s.Get(0).Generation
			s.Close()
			p := filepath.Join(state, stateFile)
			if loss == "corrupt" {
				write(t, p, "{")
			} else {
				os.Remove(p)
			}
			clk.set(clk.now().Add(time.Second)) // the restart takes a moment
			s2, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			if g2 := s2.Get(0).Generation; g2 <= g1 {
				t.Fatalf("generation %d after the loss, %d before", g2, g1)
			}
		})
	}
}

// TestWriteSynced: the file is replaced whole, with no temporary file left.
func TestWriteSynced(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "f.json")
	for _, s := range []string{"one", "two"} {
		if err := writeSynced(p, []byte(s)); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != s {
			t.Fatalf("%q", b)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(d, "*")); len(m) != 1 {
		t.Fatalf("files %v", m)
	}
	if err := writeSynced(filepath.Join(d, "none", "f.json"), nil); err == nil {
		t.Fatal("a missing folder")
	}
}

// TestRestartServesSavedCatalog (review S6): after a restart the catalog has the facts and
// projects of the last run before detection and the scan finish, so peers never see them go.
func TestRestartServesSavedCatalog(t *testing.T) {
	state := t.TempDir()
	app := Folder{Remote: "example.com/o/app", Summary: "The app", State: api.CatalogFolder{Branch: "main"}}
	dirs := func() []structure.ProjectDir { return []structure.ProjectDir{{ID: "p1", Dirs: []string{"/src/app"}}} }
	facts := Facts{System: api.CatalogSystem{OS: "linux", Arch: "amd64", Cores: 4},
		Toolchains: []api.CatalogTool{{Name: "go", Version: "1.25"}}, Capabilities: []string{"linux"}}
	cfg := Config{Self: idA, State: state, ProjectDirs: dirs,
		Detect: func(context.Context) Facts { return facts },
		ReadFolder: func(_ context.Context, d string) (Folder, bool) {
			f := app
			f.State.Path = d
			return f, true
		}}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, "the full build", func() bool { c := s.Get(0).Catalog; return len(c.Projects) == 1 && len(c.Toolchains) == 1 })
	before := *s.Get(0).Catalog
	s.Close()

	// The next run's detection and reads hang until Close.
	block := func(ctx context.Context) { <-ctx.Done() }
	cfg.Detect = func(ctx context.Context) Facts { block(ctx); return Facts{} }
	cfg.ReadFolder = func(ctx context.Context, _ string) (Folder, bool) { block(ctx); return Folder{}, false }
	var mu sync.Mutex
	var events []api.CatalogUpdated
	cfg.Publish = func(u api.CatalogUpdated) { mu.Lock(); events = append(events, u); mu.Unlock() }
	s2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got := s2.Get(0)
	if got.Generation <= before.Generation || got.Catalog == nil {
		t.Fatalf("generation %d, was %d", got.Generation, before.Generation)
	}
	c := *got.Catalog
	if !slices.Equal(c.Toolchains, before.Toolchains) || c.System != before.System ||
		!slices.Equal(c.Capabilities, before.Capabilities) || len(c.Projects) != 1 ||
		c.Projects[0].Remote != app.Remote || c.Projects[0].Summary != "The app" || c.Projects[0].Folders[0].Path != "/src/app" {
		t.Fatalf("catalog after the restart %+v", c)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
}

// savedCopies reads the device ids of the copies in a catalog.json.
func savedCopies(t *testing.T, state string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	var sv saved
	if err := json.Unmarshal(b, &sv); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for id := range sv.Copies {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestDetectFor(t *testing.T) {
	if _, err := DetectFor("maybe"); err == nil || !strings.Contains(err.Error(), "GFF_HAB_DETECT") {
		t.Fatalf("%v", err)
	}
	f, err := DetectFor("off")
	if err != nil {
		t.Fatal(err)
	}
	if got := f(context.Background()); got.System.OS == "" || len(got.Toolchains) != 0 || len(got.Harnesses) != 0 {
		t.Fatalf("%+v", got)
	}
}
