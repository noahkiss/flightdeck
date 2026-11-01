package peers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/habconn"
)

func TestPath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := func() (string, error) { return "/home/u", nil }
	cases := []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"linux", map[string]string{"GFF_HAB_PEERS": "/x/p.toml"}, "/x/p.toml"},
		{"linux", map[string]string{"XDG_CONFIG_HOME": "/cfg"}, "/cfg/gff/peers.toml"},
		{"linux", map[string]string{"XDG_CONFIG_HOME": "rel"}, "/home/u/.config/gff/peers.toml"},
		{"linux", nil, "/home/u/.config/gff/peers.toml"},
		{"darwin", map[string]string{"XDG_CONFIG_HOME": "/cfg"}, "/home/u/Library/Application Support/gff/peers.toml"},
	}
	for _, c := range cases {
		got, err := Path(c.goos, env(c.env), home)
		if err != nil || got != c.want {
			t.Errorf("%s %v: %q %v, want %q", c.goos, c.env, got, err, c.want)
		}
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestLoadProblemsByLine: bad entries are reported with their line; good ones load.
func TestLoadProblemsByLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "peers.toml")
	write(t, p, `# my peers
[[peers]]
label = "desk"
endpoints = ["desk-box", "cmd:ssh -J jump desk-box gff-hab connect"]

[[peers]]
label = "desk"
endpoints = ["other"]

[[peers]]
label = "empty"
endpoints = []

[[peers]]
endpoints = ["x"]
`)
	specs, problems, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Label != "desk" || len(specs[0].Endpoints) != 2 {
		t.Fatalf("specs %+v", specs)
	}
	want := []string{p + `:6: peer "desk": label given twice`, p + `:10: peer "empty": no endpoints`, p + `:14: peer: no label`}
	if strings.Join(problems, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems\n%s\nwant\n%s", strings.Join(problems, "\n"), strings.Join(want, "\n"))
	}
	if specs, problems, err := Load(filepath.Join(t.TempDir(), "none.toml")); specs != nil || problems != nil || err != nil {
		t.Fatalf("missing file: %v %v %v", specs, problems, err)
	}
}

// TestSetPin: the pin replaces a device_id line or goes after the label line; every other line,
// comments included, stays as it was.
func TestSetPin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "peers.toml")
	orig := `# shared peer list
[[peers]]
  label = "laptop" # the small one
  endpoints = ["laptop-box"]

[[peers]]
label = "desk"
# pinned by hand
device_id = "SHA256:old"
endpoints = ["desk-box"]

[other]
label = "laptop"
`
	write(t, p, orig)
	os.Chmod(p, 0o640)
	if err := SetPin(p, "laptop", "SHA256:new1"); err != nil {
		t.Fatal(err)
	}
	if err := SetPin(p, "desk", "SHA256:new2"); err != nil {
		t.Fatal(err)
	}
	want := `# shared peer list
[[peers]]
  label = "laptop" # the small one
  device_id = "SHA256:new1"
  endpoints = ["laptop-box"]

[[peers]]
label = "desk"
# pinned by hand
device_id = "SHA256:new2"
endpoints = ["desk-box"]

[other]
label = "laptop"
`
	if got := read(t, p); got != want {
		t.Fatalf("file\n%s\nwant\n%s", got, want)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v, want 0640 kept", fi.Mode().Perm())
	}
	if err := SetPin(p, "nobody", "SHA256:x"); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("unknown label: %v", err)
	}
	write(t, p, "[[peers]]\nlabel = \"last\"\nendpoints = [\"x\"]\n[[peers]]\nendpoints = [\"y\"]\nlabel = \"end\"")
	if err := SetPin(p, "end", "SHA256:e"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); !strings.HasSuffix(got, "label = \"end\"\ndevice_id = \"SHA256:e\"\n") {
		t.Fatalf("pin after a last line with no newline:\n%s", got)
	}

	// A save that lands while the pin is written wins: the pin's file is not renamed over it.
	saved := "[[peers]]\nlabel = \"saved\"\n"
	write(t, p, saved)
	if err := replaceFile(p, []byte("pinned"), []byte("what the pin was made from")); !errors.Is(err, errChanged) {
		t.Fatalf("replace over a changed file: %v", err)
	}
	if got := read(t, p); got != saved {
		t.Fatalf("the save went: %q", got)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".peers-*")); len(m) != 0 {
		t.Fatalf("temporary files left: %v", m)
	}
}

// TestExampleConfig: the example in docs/examples loads with no problems.
func TestExampleConfig(t *testing.T) {
	specs, problems, err := Load(filepath.Join("..", "..", "docs", "examples", "peers.toml"))
	if err != nil || len(problems) != 0 || len(specs) != 3 {
		t.Fatalf("example: %d specs, problems %v, %v", len(specs), problems, err)
	}
}

// hab is a control server with the Manager under test as its Peers.
type hab struct {
	sock string
	id   string
	srv  *control.Server
	m    *Manager
	file string
	mu   sync.Mutex
	evs  []api.Peer
}

type emptySource struct{ host string }

func (s emptySource) Snapshot(h api.HostInfo) api.Structure {
	return api.NonNil(api.Structure{Host: h})
}

func startHab(t *testing.T, dir, name, id, peers string, caps ...string) *hab {
	t.Helper()
	h := &hab{sock: filepath.Join(dir, name+".sock"), id: id, file: filepath.Join(dir, name+".toml")}
	write(t, h.file, peers)
	m, err := Open(Config{Path: h.file, Self: id, Host: name, Build: "test", Poll: 50 * time.Millisecond,
		Publish: func(p api.Peer) { h.mu.Lock(); h.evs = append(h.evs, p); h.mu.Unlock() },
		Conn: func(c *habconn.Config) {
			c.MinBackoff, c.MaxBackoff = 10*time.Millisecond, 100*time.Millisecond
			c.PingInterval, c.DeadAfter = 200*time.Millisecond, 2*time.Second
		}})
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	if caps == nil {
		caps = []string{Capability}
	}
	h.srv = control.New(control.Config{Build: "test", Host: api.HostInfo{ID: name}, DeviceID: id, Capabilities: caps,
		Source: emptySource{}, Peers: m})
	l, err := control.Listen(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	go h.srv.Serve(l)
	t.Cleanup(h.stop)
	return h
}

func (h *hab) stop() {
	h.srv.Close()
	h.m.Close()
}

func peer(l api.PeerList, label string) (api.Peer, bool) {
	for _, p := range l.Peers {
		if p.Label == label {
			return p, true
		}
	}
	return api.Peer{}, false
}

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

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gffp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// TestTwoHabsOneFile: two habs share one peer file. Each skips its own entry, links to the
// other, pins it in its file, and sees it online; after one stops the other sees it offline.
func TestTwoHabsOneFile(t *testing.T) {
	d := shortDir(t)
	file := `# one file for both
[[peers]]
label = "a"
endpoints = ["` + filepath.Join(d, "a.sock") + `"]

[[peers]]
label = "b"
endpoints = ["` + filepath.Join(d, "missing.sock") + `", "` + filepath.Join(d, "b.sock") + `"]
`
	a := startHab(t, d, "a", "SHA256:aaaaaaaaaaaa", file)
	b := startHab(t, d, "b", "SHA256:bbbbbbbbbbbb", file)
	wait(t, "a sees b online and b sees a online", func() bool {
		pa, ok1 := peer(a.m.List(), "b")
		pb, ok2 := peer(b.m.List(), "a")
		return ok1 && ok2 && pa.State == api.PeerStateOnline && pb.State == api.PeerStateOnline &&
			pa.DeviceID == b.id && pb.DeviceID == a.id
	})
	wait(t, "each skips its own entry", func() bool { return len(a.m.List().Peers) == 1 && len(b.m.List().Peers) == 1 })
	wait(t, "pins in the files", func() bool {
		return strings.Contains(read(t, a.file), `device_id = "`+b.id+`"`) && strings.Contains(read(t, b.file), `device_id = "`+a.id+`"`)
	})
	if !strings.HasPrefix(read(t, a.file), "# one file for both\n") {
		t.Fatal("the comment went")
	}
	// The first pin reconnects once (mesh.md 4.4). Until a's new link is up, b's inbound link
	// alone keeps the row online, with no endpoint in use or the missing one being tried. So the
	// endpoint is awaited with a's own link up, not read once.
	var pa api.Peer
	wait(t, "b online again after the pin, over a's own link to b.sock", func() bool {
		h, _ := a.m.set.Host("b")
		pa, _ = peer(a.m.List(), "b")
		return h.Online() && pa.State == api.PeerStateOnline && pa.Inbound && pa.Endpoint == filepath.Join(d, "b.sock")
	})
	if pa.Host != "b" || pa.LastSeen == "" || pa.DeviceID != b.id {
		t.Fatalf("a's row for b: %+v", pa)
	}

	b.stop()
	wait(t, "a sees b offline", func() bool {
		p, _ := peer(a.m.List(), "b")
		return p.State == api.PeerStateOffline && p.LastSeen != ""
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	saw := map[api.PeerState]bool{}
	for _, e := range a.evs {
		if e.Label == "b" {
			saw[e.State] = true
		}
	}
	if !saw[api.PeerStateOnline] || !saw[api.PeerStateOffline] {
		t.Fatalf("peer.updated events for b: %+v", a.evs)
	}
}

// TestRefusedThenAccepted: an endpoint that answers with a device id other than the pinned one
// is refused with both ids; Accept pins the new one and the peer comes online.
func TestRefusedThenAccepted(t *testing.T) {
	d := shortDir(t)
	b := startHab(t, d, "b", "SHA256:bbbbbbbbbbbb", "")
	a := startHab(t, d, "a", "SHA256:aaaaaaaaaaaa", `[[peers]]
label = "b"
device_id = "SHA256:the-old-one"
endpoints = ["`+b.sock+`"]
`)
	wait(t, "b refused", func() bool {
		p, _ := peer(a.m.List(), "b")
		return p.State == api.PeerStateRefused
	})
	p, _ := peer(a.m.List(), "b")
	if p.OfferedID != b.id || p.DeviceID != "SHA256:the-old-one" ||
		!strings.Contains(p.Reason, "different hab") || !strings.Contains(p.Reason, b.id) || !strings.Contains(p.Reason, "SHA256:the-old-one") {
		t.Fatalf("refused row %+v", p)
	}
	if _, err := a.m.Accept("nobody"); err == nil {
		t.Fatal("accept of an unknown label")
	}
	r, err := a.m.Accept("b")
	if err != nil {
		t.Fatal(err)
	}
	if r.Old != "SHA256:the-old-one" || r.New != b.id {
		t.Fatalf("accepted %+v", r)
	}
	wait(t, "b online after accept", func() bool {
		p, _ := peer(a.m.List(), "b")
		return p.State == api.PeerStateOnline && p.DeviceID == b.id
	})
	if _, err := a.m.Accept("b"); err == nil {
		t.Fatal("a second accept found something to accept")
	}
}

// TestUpdateNeededAndUnlisted: a peer without the mesh capability shows update needed; a hab
// that links in without being listed shows as unlisted; the file reloads on change.
func TestUpdateNeededAndUnlisted(t *testing.T) {
	d := shortDir(t)
	old := startHab(t, d, "old", "SHA256:oooooooooooo", "", "agent-state")
	a := startHab(t, d, "a", "SHA256:aaaaaaaaaaaa", "")
	if l := a.m.List(); len(l.Peers) != 0 {
		t.Fatalf("peers %+v", l)
	}
	write(t, a.file, `[[peers]]
label = "old"
endpoints = ["`+old.sock+`"]
[[peers]]
label = "a-self"
endpoints = ["`+a.sock+`"]
`)
	wait(t, "old shows update needed", func() bool {
		p, _ := peer(a.m.List(), "old")
		return p.State == api.PeerStateUpdateNeeded
	})
	wait(t, "the entry that reaches a itself is dropped", func() bool {
		_, ok := peer(a.m.List(), "a-self")
		return !ok
	})
	// old has a in no list; a's link to old makes a show up there as unlisted.
	wait(t, "a unlisted on old", func() bool {
		l := old.m.List()
		return len(l.Unlisted) == 1 && l.Unlisted[0].DeviceID == a.id && l.Unlisted[0].Host == "a"
	})
	// old's hello reaches old before a has pinned old in its file. Wait for the pin, so the edit
	// below tests the reload, not an edit that lands while a writes its pin.
	wait(t, "a pins old", func() bool { return strings.Contains(read(t, a.file), `device_id = "`+old.id+`"`) })
	write(t, a.file, "[[peers]]\nlabel = \n")
	wait(t, "a parse problem", func() bool {
		l := a.m.List()
		return len(l.Problems) == 1 && len(l.Peers) == 1
	})
}
