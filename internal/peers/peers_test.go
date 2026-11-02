package peers

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
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
	// endpoint is awaited with a's own link up, not read once. The live row can show this while
	// a's pump is still writing the pin, before it has published anything for that link, so the
	// wait is on the last peer.updated event too, not on the row alone.
	var pa api.Peer
	wait(t, "b online again after the pin, over a's own link to b.sock, and published", func() bool {
		h, _ := a.m.set.Host("b")
		pa, _ = peer(a.m.List(), "b")
		ev, _ := a.last("b")
		return h.Online() && pa.State == api.PeerStateOnline && pa.Inbound && pa.Endpoint == filepath.Join(d, "b.sock") &&
			samePeer(ev, pa)
	})
	if pa.Host != "b" || pa.LastSeen == "" || pa.DeviceID != b.id {
		t.Fatalf("a's row for b: %+v", pa)
	}

	b.stop()
	wait(t, "a sees b offline, and published it", func() bool {
		p, _ := peer(a.m.List(), "b")
		ev, _ := a.last("b")
		return p.State == api.PeerStateOffline && p.LastSeen != "" && ev.State == api.PeerStateOffline
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

// last is the last peer.updated event the hab published for label.
func (h *hab) last(label string) (api.Peer, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.evs) - 1; i >= 0; i-- {
		if h.evs[i].Label == label {
			return h.evs[i], true
		}
	}
	return api.Peer{}, false
}

// TestPublishInOrder: peer.updated events go out in the order their rows were made, so the
// last one is the peer's state. A link from the peer comes up and its online row is held in
// Publish while the link ends; the offline row must not go out before the online one.
func TestPublishInOrder(t *testing.T) {
	d := shortDir(t)
	const id = "SHA256:bbbbbbbbbbbb"
	file := filepath.Join(d, "peers.toml")
	write(t, file, `[[peers]]
label = "b"
device_id = "`+id+`"
endpoints = ["`+filepath.Join(d, "missing.sock")+`"]
`)
	var mu sync.Mutex
	var evs []api.Peer
	hold, held := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m, err := Open(Config{Path: file, Self: "SHA256:aaaaaaaaaaaa", Host: "a", Build: "test",
		Publish: func(p api.Peer) {
			if p.Inbound {
				once.Do(func() { close(held); <-hold })
			}
			mu.Lock()
			evs = append(evs, p)
			mu.Unlock()
		},
		Conn: func(c *habconn.Config) { c.MinBackoff, c.MaxBackoff = 10*time.Millisecond, 100*time.Millisecond }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(evs) }
	wait(t, "b offline published", func() bool { return count() > 0 })

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.Linked(1, api.PeerHello{DeviceID: id, Host: "b"}) }()
	<-held // the online row is made and held in Publish
	n := count()
	go func() { defer wg.Done(); m.Unlinked(1) }()
	// Give an offline event in the wrong order time to go out while the online one is held.
	// With the order kept, Unlinked waits and no event comes.
	deadline := time.Now().Add(300 * time.Millisecond)
	for count() == n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(hold)
	wg.Wait()

	row, _ := peer(m.List(), "b")
	mu.Lock()
	defer mu.Unlock()
	if row.State != api.PeerStateOffline || row.Inbound {
		t.Fatalf("row %+v", row)
	}
	if last := evs[len(evs)-1]; last.State != api.PeerStateOffline || last.Inbound {
		t.Fatalf("last peer.updated %+v is not the row %+v; events %+v", last, row, evs)
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

// oldHab listens on sock and answers every hello as a gff-hab from before peer links does: it
// refuses the client kind hab with kind invalid.
func oldHab(t *testing.T, sock string) net.Listener {
	t.Helper()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					ID json.RawMessage `json:"id"`
				}
				json.Unmarshal(line, &req)
				hint := `client.kind "hab" is not one of [terminal app]`
				resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{
					"code": api.ErrorCode, "message": hint, "data": map[string]any{"kind": "invalid", "hint": hint}}})
				c.Write(append(resp, '\n'))
			}()
		}
	}()
	return l
}

// TestRefusedHelloRetried: a peer whose older gff-hab refuses the peer link's hello shows update
// needed, not offline, and its link keeps trying: once the peer runs a build with peer links it
// comes online with no restart of this hab (review M3).
func TestRefusedHelloRetried(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "b.sock")
	l := oldHab(t, sock)
	a := startHab(t, d, "a", "SHA256:aaaaaaaaaaaa", `[[peers]]
label = "b"
endpoints = ["`+sock+`"]
`)
	wait(t, "b update needed", func() bool {
		p, _ := peer(a.m.List(), "b")
		return p.State == api.PeerStateUpdateNeeded && strings.Contains(p.Reason, "refused the peer link")
	})
	l.Close() // the update: the old build stops, the new one starts on the same socket
	b := startHab(t, d, "b", "SHA256:bbbbbbbbbbbb", "")
	wait(t, "b online after its update", func() bool {
		p, _ := peer(a.m.List(), "b")
		return p.State == api.PeerStateOnline && p.DeviceID == b.id
	})
}

// TestPinMismatchTriesOtherEndpoints: an endpoint that reaches another hab does not end the
// peer while a later endpoint may present the pin (review S1). The peer is refused only when
// every endpoint presented the same other id; one other id and one dead endpoint is offline,
// and nothing can be accepted.
func TestPinMismatchTriesOtherEndpoints(t *testing.T) {
	d := shortDir(t)
	b := startHab(t, d, "b", "SHA256:bbbbbbbbbbbb", "")
	c := startHab(t, d, "c", "SHA256:cccccccccccc", "")
	c2 := filepath.Join(d, "c2.sock") // a second path to c
	if err := os.Symlink(c.sock, c2); err != nil {
		t.Fatal(err)
	}
	a := startHab(t, d, "a", "SHA256:aaaaaaaaaaaa", `[[peers]]
label = "b"
device_id = "`+b.id+`"
endpoints = ["`+c.sock+`", "`+b.sock+`"]

[[peers]]
label = "c-twice"
device_id = "SHA256:gone"
endpoints = ["`+c.sock+`", "`+c2+`"]

[[peers]]
label = "half"
device_id = "SHA256:gone"
endpoints = ["`+c.sock+`", "`+filepath.Join(d, "none.sock")+`"]
`)
	wait(t, "b online at its second endpoint", func() bool {
		p, _ := peer(a.m.List(), "b")
		return p.State == api.PeerStateOnline && p.DeviceID == b.id && p.Endpoint == b.sock
	})
	wait(t, "c-twice refused", func() bool {
		p, _ := peer(a.m.List(), "c-twice")
		return p.State == api.PeerStateRefused && p.OfferedID == c.id
	})
	wait(t, "half offline, naming the other hab", func() bool {
		p, _ := peer(a.m.List(), "half")
		return p.State == api.PeerStateOffline && strings.Contains(p.Reason, "trying the other endpoints")
	})
	if _, err := a.m.Accept("half"); err == nil {
		t.Fatal("accepted the other hab of a peer that is not refused")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.evs {
		if e.Label == "b" && e.State == api.PeerStateRefused {
			t.Fatalf("b was refused on its way online: %+v", e)
		}
	}
}

// TestPinNotWritten: when the file cannot take the first pin, the row shows no pinned id and
// the list says why (review S2).
func TestPinNotWritten(t *testing.T) {
	d := shortDir(t)
	b := startHab(t, d, "b", "SHA256:bbbbbbbbbbbb", "")
	// The label shares its line, so the pin is not edited in.
	a := startHab(t, d, "a", "SHA256:aaaaaaaaaaaa", `peers = [{label = "b", endpoints = ["`+b.sock+`"]}]
`)
	wait(t, "b online, unpinned, with a problem", func() bool {
		l := a.m.List()
		p, _ := peer(l, "b")
		return p.State == api.PeerStateOnline && p.DeviceID == "" && len(l.Problems) == 1 &&
			strings.Contains(l.Problems[0], "could not pin") && strings.Contains(l.Problems[0], b.id)
	})
}

// TestSelfEntryEndpointsChange: an entry hidden because its endpoint reached this hab shows a
// problem, and comes back once its endpoints change (review S3).
func TestSelfEntryEndpointsChange(t *testing.T) {
	d := shortDir(t)
	a := startHab(t, d, "a", "SHA256:aaaaaaaaaaaa", `[[peers]]
label = "x"
endpoints = ["`+filepath.Join(d, "a.sock")+`"]
`)
	wait(t, "x hidden with a problem", func() bool {
		l := a.m.List()
		_, ok := peer(l, "x")
		return !ok && len(l.Problems) == 1 && strings.Contains(l.Problems[0], "answers as this hab")
	})
	b := startHab(t, d, "b", "SHA256:bbbbbbbbbbbb", "")
	write(t, a.file, `[[peers]]
label = "x"
endpoints = ["`+b.sock+`"]
`)
	wait(t, "x online at its new endpoint", func() bool {
		l := a.m.List()
		p, _ := peer(l, "x")
		return p.State == api.PeerStateOnline && p.DeviceID == b.id && len(l.Problems) == 0
	})
}

// TestReloadWaitsForStableFile: a changed file is read only once its size and modification
// time held for one poll, so a file in the middle of a write is not read (review N3).
func TestReloadWaitsForStableFile(t *testing.T) {
	d := shortDir(t)
	file := filepath.Join(d, "a.toml")
	write(t, file, "")
	m, err := Open(Config{Path: file, Self: "SHA256:aaaaaaaaaaaa", Host: "a", Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	entry := func(label string) string {
		return "[[peers]]\nlabel = \"" + label + "\"\nendpoints = [\"" + filepath.Join(d, "none.sock") + "\"]\n"
	}
	write(t, file, entry("x"))
	m.check()
	if n := len(m.List().Peers); n != 0 {
		t.Fatalf("read at the first poll that saw the change: %d peers", n)
	}
	write(t, file, entry("x")+entry("y"))
	m.check()
	if n := len(m.List().Peers); n != 0 {
		t.Fatalf("read while the file still changed: %d peers", n)
	}
	m.check()
	if n := len(m.List().Peers); n != 2 {
		t.Fatalf("not read once the file held: %d peers", n)
	}
	m.check()
	if n := len(m.List().Peers); n != 2 {
		t.Fatalf("after a quiet poll: %d peers", n)
	}
}

// TestAcceptEmptyID: a refused peer that presents no device id cannot be accepted: pinning an
// empty id would turn the check off (review N4).
func TestAcceptEmptyID(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "anon.sock")
	srv := control.New(control.Config{Build: "test", Host: api.HostInfo{ID: "anon"}, Capabilities: []string{Capability}, Source: emptySource{}})
	l, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(srv.Close)
	a := startHab(t, d, "a", "SHA256:aaaaaaaaaaaa", `[[peers]]
label = "anon"
device_id = "SHA256:old"
endpoints = ["`+sock+`"]
`)
	wait(t, "anon refused", func() bool {
		p, _ := peer(a.m.List(), "anon")
		return p.State == api.PeerStateRefused
	})
	if _, err := a.m.Accept("anon"); err == nil {
		t.Fatal("accepted an empty device id")
	}
	if !strings.Contains(read(t, a.file), `device_id = "SHA256:old"`) {
		t.Fatalf("the pin changed:\n%s", read(t, a.file))
	}
}
