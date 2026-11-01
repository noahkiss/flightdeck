package catalog

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/peers"
	"github.com/noahkiss/flightdeck/internal/structure"
)

type emptySource struct{}

func (emptySource) Snapshot(h api.HostInfo) api.Structure { return api.NonNil(api.Structure{Host: h}) }

// peerList is the catalog's view of a peers.Manager, as gff-hab adapts it.
type peerList struct{ m *peers.Manager }

func (p peerList) List() api.PeerList { return p.m.List() }

func (p peerList) Links() []Link {
	var out []Link
	for _, l := range p.m.Links() {
		cl := Link{Label: l.Label, DeviceID: l.DeviceID, Online: l.Online, Caps: l.Caps}
		if l.Conn != nil {
			cl.Caller = l.Conn
		}
		out = append(out, cl)
	}
	return out
}

// hab is one test hab: a control server with a peer list and a catalog over a real socket.
type hab struct {
	name, id, sock, peersFile, configFile, state string
	m                                            *peers.Manager
	cat                                          *Service
	srv                                          *control.Server

	mu      sync.Mutex
	notices []api.NoticeRaiseParams
	stopped bool
}

// habOpts are a test hab's folders (path -> folder read) and config file text.
type habOpts struct {
	folders map[string]Folder
	config  string
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gffc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func peersFile(entries ...[2]string) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString("[[peers]]\nlabel = \"" + e[0] + "\"\nendpoints = [\"" + e[1] + "\"]\n\n")
	}
	return b.String()
}

func startHab(t *testing.T, d, name, id, peerText string, o habOpts) *hab {
	t.Helper()
	h := &hab{name: name, id: id, sock: filepath.Join(d, name+".sock"), peersFile: filepath.Join(d, name+".toml"),
		configFile: filepath.Join(d, name+"-hab.toml"), state: filepath.Join(d, name+"-state")}
	if _, err := os.Stat(h.peersFile); err != nil {
		write(t, h.peersFile, peerText)
	}
	write(t, h.configFile, o.config)
	os.MkdirAll(h.state, 0o700)
	h.start(t, o)
	t.Cleanup(h.stop)
	return h
}

func (h *hab) start(t *testing.T, o habOpts) {
	t.Helper()
	var cat atomic.Pointer[Service]
	m, err := peers.Open(peers.Config{Path: h.peersFile, Self: h.id, Host: h.name, Build: "test", Poll: 50 * time.Millisecond,
		Publish: func(api.Peer) {
			if c := cat.Load(); c != nil {
				c.Poke()
				c.Kick()
			}
		},
		Conn: func(c *habconn.Config) {
			c.MinBackoff, c.MaxBackoff = 10*time.Millisecond, 100*time.Millisecond
			c.PingInterval, c.DeadAfter = 200*time.Millisecond, 2*time.Second
		}})
	if err != nil {
		t.Fatal(err)
	}
	var dirs []structure.ProjectDir
	for p := range o.folders {
		dirs = append(dirs, structure.ProjectDir{ID: "p-" + filepath.Base(p), Dirs: []string{p}})
	}
	slices.SortFunc(dirs, func(a, b structure.ProjectDir) int { return strings.Compare(a.Dirs[0], b.Dirs[0]) })
	cs, err := New(Config{
		Self: h.id, Host: h.name, State: h.state, ConfigPath: h.configFile,
		ProjectDirs: func() []structure.ProjectDir { return dirs },
		Peers:       peerList{m},
		Notify: func(p api.NoticeRaiseParams) {
			h.mu.Lock()
			h.notices = append(h.notices, p)
			h.mu.Unlock()
		},
		Detect: func(context.Context) Facts {
			return Facts{System: api.CatalogSystem{OS: "linux", Arch: "amd64", Cores: 4}, Capabilities: []string{"linux"}}
		},
		ReadFolder: func(_ context.Context, d string) (Folder, bool) {
			f, ok := o.folders[d]
			f.State.Path = d
			return f, ok
		},
		Pull: 30 * time.Millisecond, Debounce: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	cat.Store(cs)
	srv := control.New(control.Config{Build: "test", Host: api.HostInfo{ID: h.name}, DeviceID: h.id,
		Capabilities: []string{peers.Capability, Capability}, Source: emptySource{}, Peers: m, Catalog: cs})
	l, err := control.Listen(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	h.mu.Lock()
	h.m, h.cat, h.srv, h.stopped = m, cs, srv, false
	h.mu.Unlock()
}

func (h *hab) stop() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	h.mu.Unlock()
	h.srv.Close()
	h.cat.Close()
	h.m.Close()
}

func (h *hab) noticeList() []api.NoticeRaiseParams {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.notices)
}

func (h *hab) noticeCount() int { return len(h.noticeList()) }

func wait(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func entry(l api.CatalogList, id string) (api.CatalogEntry, bool) {
	for _, e := range l.Hosts {
		if e.Catalog.DeviceID == id {
			return e, true
		}
	}
	return api.CatalogEntry{}, false
}

func group(l api.CatalogList, key string) (api.CatalogProjectGroup, bool) {
	for _, g := range l.Projects {
		if g.Key == key {
			return g, true
		}
	}
	return api.CatalogProjectGroup{}, false
}

// TestSharedProject (acceptance 1): two habs that share a project show it once in the merged
// view, with one location on each host; a project with no remote and no root commit match
// stays its own.
func TestSharedProject(t *testing.T) {
	d := shortDir(t)
	app := Folder{Remote: "example.com/o/app", State: api.CatalogFolder{Branch: "main", Head: "abc"}}
	a := startHab(t, d, "a", "SHA256:aaaa", peersFile([2]string{"b", filepath.Join(d, "b.sock")}), habOpts{
		folders: map[string]Folder{"/src/app": app, "/src/notes": {RootCommit: "r00t"}},
	})
	b := startHab(t, d, "b", "SHA256:bbbb", peersFile([2]string{"a", filepath.Join(d, "a.sock")}), habOpts{
		folders: map[string]Folder{"/work/app-clone": app},
	})
	for _, h := range []*hab{a, b} {
		wait(t, h.name+" holds both catalogs with the app on two hosts", func() bool {
			l := h.cat.List()
			g, ok := group(l, "remote:example.com/o/app")
			return len(l.Hosts) == 2 && ok && len(g.Locations) == 2
		})
	}
	l := a.cat.List()
	if !l.Hosts[0].Self || l.Hosts[0].Catalog.DeviceID != a.id || l.Hosts[1].Label != "b" || l.Hosts[1].Stale {
		t.Fatalf("hosts %+v", l.Hosts)
	}
	g, _ := group(l, "remote:example.com/o/app")
	if g.Name != "app" || g.Locations[0].DeviceID != a.id || g.Locations[1].DeviceID != b.id || g.Locations[1].Label != "b" ||
		g.Locations[0].Folders[0].Path != "/src/app" || g.Locations[1].Folders[0].Path != "/work/app-clone" {
		t.Fatalf("app %+v", g)
	}
	if n, ok := group(l, "root:r00t"); !ok || len(n.Locations) != 1 {
		t.Fatalf("notes %+v", n)
	}
	if len(l.Projects) != 2 {
		t.Fatalf("projects %+v", l.Projects)
	}
	// The copy of b is b's own catalog, and lists a as its peer.
	eb, _ := entry(l, b.id)
	if eb.Catalog.Host != "b" || !slices.Equal(eb.Catalog.Peers, []string{a.id}) || eb.Catalog.Generation == 0 {
		t.Fatalf("copy of b %+v", eb.Catalog)
	}
}

// TestOfflineStale (acceptance 2): a peer that goes offline stays in the merged view, stale,
// with its projects; the dialer also handed its own catalog to a peer that does not list it.
func TestOfflineStale(t *testing.T) {
	d := shortDir(t)
	app := Folder{Remote: "example.com/o/app"}
	b := startHab(t, d, "b", "SHA256:bbbb", "", habOpts{folders: map[string]Folder{"/w/app": app}})
	a := startHab(t, d, "a", "SHA256:aaaa", peersFile([2]string{"b", b.sock}), habOpts{folders: map[string]Folder{"/s/app": app}})
	wait(t, "a holds b's catalog", func() bool {
		e, ok := entry(a.cat.List(), b.id)
		return ok && !e.Stale
	})
	wait(t, "b holds a's catalog, handed over on a's link", func() bool {
		e, ok := entry(b.cat.List(), a.id)
		return ok && !e.Stale && e.Label == ""
	})
	b.stop()
	wait(t, "b stale in a's view", func() bool {
		e, ok := entry(a.cat.List(), b.id)
		return ok && e.Stale
	})
	l := a.cat.List()
	g, ok := group(l, "remote:example.com/o/app")
	if !ok || len(g.Locations) != 2 || g.Locations[0].Stale || !g.Locations[1].Stale {
		t.Fatalf("app %+v", g)
	}
	// Still there a few rounds later: offline is not removed.
	time.Sleep(200 * time.Millisecond)
	if e, ok := entry(a.cat.List(), b.id); !ok || !e.Stale {
		t.Fatalf("b after more rounds: %+v %v", e, ok)
	}
	// Removing b from the peer list drops the copy.
	write(t, a.peersFile, "")
	wait(t, "the copy of b dropped", func() bool {
		_, ok := entry(a.cat.List(), b.id)
		return !ok
	})
}

// TestAlwaysOnAlert (acceptance 3): three habs list each other; c is marked always on. When c
// stops, exactly one notification goes out, from a, the lowest device id of the online habs
// that list c. Back online clears it, and the next outage alerts once more.
func TestAlwaysOnAlert(t *testing.T) {
	d := shortDir(t)
	sock := func(n string) string { return filepath.Join(d, n+".sock") }
	cfg := "[catalog]\nalways_on_alert = \"300ms\"\n"
	a := startHab(t, d, "a", "SHA256:aaaa", peersFile([2]string{"b", sock("b")}, [2]string{"c", sock("c")}), habOpts{config: cfg})
	b := startHab(t, d, "b", "SHA256:bbbb", peersFile([2]string{"a", sock("a")}, [2]string{"c", sock("c")}), habOpts{config: cfg})
	cOpts := habOpts{config: "[catalog]\nalways_on = true\n"}
	c := startHab(t, d, "c", "SHA256:cccc", peersFile([2]string{"a", sock("a")}, [2]string{"b", sock("b")}), cOpts)
	for _, h := range []*hab{a, b} {
		wait(t, h.name+" holds the other two catalogs, c always on, each listing c", func() bool {
			l := h.cat.List()
			ec, ok := entry(l, c.id)
			if len(l.Hosts) != 3 || !ok || !ec.Catalog.AlwaysOn || ec.Stale {
				return false
			}
			for _, e := range l.Hosts {
				if e.Catalog.DeviceID != c.id && !slices.Contains(e.Catalog.Peers, c.id) {
					return false
				}
			}
			return true
		})
	}
	c.stop()
	wait(t, "one notification", func() bool { return a.noticeCount()+b.noticeCount() >= 1 })
	time.Sleep(500 * time.Millisecond) // more rounds, past the threshold again
	if a.noticeCount() != 1 || b.noticeCount() != 0 {
		t.Fatalf("notices: a %+v, b %+v", a.noticeList(), b.noticeList())
	}
	if n := a.noticeList()[0]; n.Title != "c is offline" || n.Level != api.NoticeLevelActive || !strings.Contains(n.Body, "always on") {
		t.Fatalf("notice %+v", n)
	}

	// Back online clears it; the next outage alerts once more.
	c.start(t, cOpts)
	wait(t, "c online again in a's view", func() bool {
		e, ok := entry(a.cat.List(), c.id)
		return ok && !e.Stale
	})
	time.Sleep(100 * time.Millisecond) // a round sees c online
	c.stop()
	wait(t, "a second notification", func() bool { return a.noticeCount() == 2 })
	time.Sleep(500 * time.Millisecond)
	if a.noticeCount() != 2 || b.noticeCount() != 0 {
		t.Fatalf("notices after the second outage: a %d, b %d", a.noticeCount(), b.noticeCount())
	}
}

// TestGetSince: catalog.get returns the whole catalog unless since is the current generation;
// a change raises the generation, and the generation survives a restart.
func TestGetSince(t *testing.T) {
	state := t.TempDir()
	var mu sync.Mutex
	agents := []api.AgentState{}
	var events []api.CatalogUpdated
	cfg := Config{Self: "SHA256:self", Host: "h", State: state, Debounce: time.Millisecond,
		Detect: func(context.Context) Facts { return Facts{} },
		Structure: func() api.Structure {
			mu.Lock()
			defer mu.Unlock()
			return api.Structure{
				Projects: []api.Project{{ID: "p1", Name: "app"}},
				Sessions: []api.Session{{ID: "s1", Name: "fix-bug", Project: "p1", Pane: "pane-1"}},
				Agents:   slices.Clone(agents),
			}
		},
		Publish: func(u api.CatalogUpdated) { mu.Lock(); events = append(events, u); mu.Unlock() },
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, "the first background build", func() bool { r := s.Get(0); return r.Catalog != nil })
	g := s.Get(0).Generation
	if r := s.Get(g); r.Catalog != nil || r.Generation != g {
		t.Fatalf("get at the current generation: %+v", r)
	}
	if r := s.Get(g + 5); r.Catalog == nil {
		t.Fatal("a since past the generation (a puller with an older life of this hab) gets everything")
	}
	mu.Lock()
	agents = []api.AgentState{{Pane: "pane-1", Harness: "claude", State: api.AgentStatusWorking, At: "2026-11-01T10:00:00Z"},
		{Pane: "pane-2", Harness: "codex", State: api.AgentStatusExited, At: "2026-11-01T10:00:00Z"}}
	mu.Unlock()
	s.Poke()
	wait(t, "a new generation", func() bool { return s.Get(0).Generation > g })
	r := s.Get(g)
	if r.Catalog == nil || len(r.Catalog.Agents) != 1 {
		t.Fatalf("agents %+v", r.Catalog)
	}
	if a := r.Catalog.Agents[0]; a.Session != "fix-bug" || a.Project != "app" || a.Harness != "claude" || a.State != api.AgentStatusWorking || a.Since == "" {
		t.Fatalf("agent %+v", a)
	}
	mu.Lock()
	if len(events) == 0 || events[len(events)-1].DeviceID != "SHA256:self" {
		t.Fatalf("events %+v", events)
	}
	mu.Unlock()
	last := s.Get(0).Generation
	s.Close()
	s2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if g2 := s2.Get(0).Generation; g2 <= last {
		t.Fatalf("generation after a restart %d, was %d", g2, last)
	}
}

// TestPutRules: only a peer link hands over a catalog, and only its own.
func TestPutRules(t *testing.T) {
	d := shortDir(t)
	h := startHab(t, d, "a", "SHA256:aaaa", "", habOpts{})
	dial := func(p *api.PeerHello) *control.Client {
		c, err := control.Dial(h.sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.HelloAs(ctx, api.HelloParams{Client: api.ClientInfo{Name: "t", Version: "0", Kind: api.ClientKindHab}, Peer: p}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	put := func(c *control.Client, id string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := api.Client{Caller: c}.CatalogPut(ctx, api.CatalogPutParams{Catalog: api.Catalog{DeviceID: id, Host: "x", Generation: 3}})
		return err
	}
	isInvalid := func(err error) bool {
		var ce *control.Error
		return errors.As(err, &ce) && ce.Data.Kind == api.ErrorKindInvalid
	}
	if err := put(dial(nil), "SHA256:x"); !isInvalid(err) {
		t.Fatalf("put with no peer field: %v", err)
	}
	link := dial(&api.PeerHello{DeviceID: "SHA256:x", Host: "x"})
	if err := put(link, "SHA256:y"); !isInvalid(err) {
		t.Fatalf("put of another hab's catalog: %v", err)
	}
	if err := put(link, "SHA256:x"); err != nil {
		t.Fatal(err)
	}
	e, ok := entry(h.cat.List(), "SHA256:x")
	if !ok || e.Label != "" || e.Stale || e.Catalog.Generation != 3 {
		t.Fatalf("the unlisted linked hab's copy: %+v %v", e, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := api.Client{Caller: link}.CatalogGet(ctx, api.CatalogGetParams{})
	if err != nil || r.Catalog == nil || r.Catalog.DeviceID != h.id {
		t.Fatalf("catalog.get: %+v %v", r, err)
	}
	// One hop: catalog.get serves only the hab's own catalog, never a copy.
	if r.Catalog.DeviceID == "SHA256:x" {
		t.Fatal("served a copy")
	}
}

func TestWords(t *testing.T) {
	got := words([]string{"linux", "docker"}, []string{"signing", "docker"})
	want := []api.CatalogCapability{{Name: "docker", Detected: true, Declared: true}, {Name: "linux", Detected: true}, {Name: "signing", Declared: true}}
	if !slices.Equal(got, want) {
		t.Fatalf("%+v", got)
	}
	if w := detectedWords("darwin", []api.CatalogTool{{Name: "xcode"}, {Name: "podman"}}, func(string) bool { return false }); !slices.Equal(w, []string{"macos", "xcode", "docker"}) {
		t.Fatalf("%v", w)
	}
}

func TestLoadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hab.toml")
	write(t, p, `[catalog]
capabilities = ["signing", " "]
project_roots = ["~/code", "rel/dir", "/abs"]
always_on = true
always_on_alert = "soon"

[[catalog.services]]
name = "grafana"
url = "http://localhost:3000"
health = "http://localhost:3000/api/health"

[[catalog.services]]
name = "broken"

[[catalog.actions]]
name = "update"
command = "make update"
`)
	d, problems, err := LoadConfig(p, func() (string, error) { return "/home/u", nil })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Capabilities, []string{"signing"}) || !slices.Equal(d.ProjectRoots, []string{"/home/u/code", "/abs"}) ||
		!d.AlwaysOn || d.AlertAfter != DefaultAlertAfter || len(d.Services) != 1 || d.Services[0].Health == "" ||
		len(d.Actions) != 1 {
		t.Fatalf("%+v", d)
	}
	if len(problems) != 3 {
		t.Fatalf("problems %q", problems)
	}
	if d, problems, err := LoadConfig(filepath.Join(t.TempDir(), "none.toml"), nil); err != nil || problems != nil || d.AlertAfter != DefaultAlertAfter {
		t.Fatalf("missing file: %+v %v %v", d, problems, err)
	}
	write(t, p, "[catalog\n")
	if _, _, err := LoadConfig(p, nil); err == nil {
		t.Fatal("a file that does not parse is an error")
	}
}

func TestExampleConfig(t *testing.T) {
	d, problems, err := LoadConfig(filepath.Join("..", "..", "docs", "examples", "hab.toml"), func() (string, error) { return "/home/u", nil })
	if err != nil || len(problems) != 0 || len(d.Capabilities) != 2 || len(d.Services) != 1 || len(d.Actions) != 1 ||
		!slices.Equal(d.ProjectRoots, []string{"/home/u/code"}) || d.AlertAfter != 30*time.Minute {
		t.Fatalf("example: %+v, problems %v, %v", d, problems, err)
	}
}

func TestConfigPath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := func() (string, error) { return "/home/u", nil }
	for _, c := range []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"linux", map[string]string{"GFF_HAB_CONFIG": "/x/h.toml"}, "/x/h.toml"},
		{"linux", map[string]string{"XDG_CONFIG_HOME": "/cfg"}, "/cfg/gff/hab.toml"},
		{"linux", nil, "/home/u/.config/gff/hab.toml"},
		{"darwin", nil, "/home/u/Library/Application Support/gff/hab.toml"},
	} {
		if got, err := ConfigPath(c.goos, env(c.env), home); err != nil || got != c.want {
			t.Errorf("%s %v: %q %v, want %q", c.goos, c.env, got, err, c.want)
		}
	}
}

// TestScanWithGit: the scan finds repositories to depth 3 and no deeper, follows no symbolic
// link, and reads identity, branch, dirty state, last commit and summary with real git.
func TestScanWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
			"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	repo := func(rel string) string {
		p := filepath.Join(root, rel)
		os.MkdirAll(p, 0o755)
		git(p, "init", "-q", "-b", "main")
		write(t, filepath.Join(p, "README.md"), "# The app\n\nMore.\n")
		git(p, "add", ".")
		git(p, "commit", "-q", "-m", "first")
		return p
	}
	app := repo("a/app")
	git(app, "remote", "add", "origin", "git@Example.com:o/app.git")
	write(t, filepath.Join(app, "new.txt"), "x")
	local := repo("b/c/local")
	repo("d/e/f/too-deep")
	os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link"))
	os.MkdirAll(filepath.Join(root, ".hidden"), 0o755)

	found := FindRepos([]string{root})
	if !slices.Equal(found, []string{app, local}) {
		t.Fatalf("found %v", found)
	}
	f, ok := ReadFolder(context.Background(), app)
	if !ok || f.Remote != "example.com/o/app" || f.RootCommit != "" || f.Summary != "The app" || f.State.Branch != "main" ||
		len(f.State.Head) < 40 || f.State.Dirty == nil || !*f.State.Dirty || f.State.LastCommit == "" {
		t.Fatalf("app %+v", f)
	}
	f, ok = ReadFolder(context.Background(), local)
	if !ok || f.Remote != "" || len(f.RootCommit) < 40 || f.State.Dirty == nil || *f.State.Dirty {
		t.Fatalf("local %+v", f)
	}
	if _, ok := ReadFolder(context.Background(), root); ok {
		t.Fatal("a folder that is not a repository")
	}
}
