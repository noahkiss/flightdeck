package hosts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/board"
	"github.com/noahkiss/flightdeck/internal/fakehab"
	"github.com/noahkiss/flightdeck/internal/habconn"
	"github.com/noahkiss/flightdeck/internal/hosts"
)

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
	d, err := os.MkdirTemp("", "gffh")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(d, 0o700)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func startHab(t *testing.T, dir, id string) *fakehab.Hab {
	t.Helper()
	h, err := fakehab.New(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	return h
}

func open(t *testing.T, specs ...hosts.Spec) *hosts.Set {
	t.Helper()
	s, err := hosts.Open(hosts.Config{
		Source: hosts.Static(specs),
		Client: api.ClientInfo{Name: "test", Kind: api.ClientKindTerminal},
		Conn: func(c *habconn.Config) {
			c.MinBackoff, c.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// waitHosts reads the Set's updates until cond holds on its hosts. Every update is read, so
// the wait follows the Set's own order of events, never a timer.
func waitHosts(t *testing.T, s *hosts.Set, what string, cond func([]hosts.Host) bool) {
	t.Helper()
	timer := time.NewTimer(waitLong())
	defer timer.Stop()
	for !cond(s.Hosts()) {
		select {
		case _, ok := <-s.Updates():
			if !ok {
				t.Fatalf("waiting for %s: updates closed", what)
			}
		case <-timer.C:
			t.Fatalf("waiting for %s: timed out; hosts %+v", what, s.Hosts())
		}
	}
}

func status(hs []hosts.Host, label string) hosts.Status {
	for _, h := range hs {
		if h.Label == label {
			return h.Status
		}
	}
	return 0
}

func sessionsOf(hs []hosts.Host, label string) int {
	for _, h := range hs {
		if h.Label == label {
			return len(h.Structure.Sessions)
		}
	}
	return -1
}

// TestHostOfflineAndBack: two habs; one is down when the client starts, which is normal: the
// other shows at once. The down one comes up and shows; then it goes down, its rows stay with
// the host offline, and when it comes back with a new epoch its rows are read again, with no
// restart of the client.
func TestHostOfflineAndBack(t *testing.T) {
	d := shortDir(t)
	a := startHab(t, d, "a")
	b := startHab(t, d, "b")
	b.Stop()
	if _, _, err := a.AddSession("alpha"); err != nil {
		t.Fatal(err)
	}
	s := open(t,
		hosts.Spec{Label: "one", Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: a.Sock}}},
		hosts.Spec{Label: "two", Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: b.Sock}}},
	)
	waitHosts(t, s, "one online with its session, two trying", func(hs []hosts.Host) bool {
		two := hs[1]
		return status(hs, "one") == hosts.Online && sessionsOf(hs, "one") == 1 &&
			(two.Status == hosts.Connecting || two.Status == hosts.Offline) && two.Err != nil
	})

	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	waitHosts(t, s, "two online", func(hs []hosts.Host) bool { return status(hs, "two") == hosts.Online })
	_, pane, err := b.AddSession("beta")
	if err != nil {
		t.Fatal(err)
	}
	waitHosts(t, s, "two's session", func(hs []hosts.Host) bool { return sessionsOf(hs, "two") == 1 })
	epoch := b.Epoch()

	b.Stop()
	waitHosts(t, s, "two offline", func(hs []hosts.Host) bool { return status(hs, "two") == hosts.Offline })
	rows := board.Rows(s.Hosts(), nil)
	if len(rows) != 2 {
		t.Fatalf("rows while two is offline: %+v", rows)
	}
	for _, r := range rows {
		if (r.Host == "two") != r.Offline() {
			t.Errorf("row %s: offline %v", r.Key, r.Offline())
		}
	}

	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if b.Epoch() == epoch {
		t.Fatal("the restart kept its epoch")
	}
	b.SetAgent(pane, api.AgentStatusWorking, "")
	waitHosts(t, s, "two back with the agent state", func(hs []hosts.Host) bool {
		if status(hs, "two") != hosts.Online {
			return false
		}
		for _, r := range board.Rows(hs, nil) {
			if r.Host == "two" && r.Rank == board.RankWorking {
				return true
			}
		}
		return false
	})
	if h, _ := s.Host("two"); h.Welcome.Epoch != b.Epoch() {
		t.Errorf("welcome epoch %q, want %q", h.Welcome.Epoch, b.Epoch())
	}
}

// TestBoardOrdersBlockedFirst: across two habs, a blocked session ranks above a done one, and
// that above a working one, whatever host each is on. A row moves only when its rank changes.
func TestBoardOrdersBlockedFirst(t *testing.T) {
	d := shortDir(t)
	a := startHab(t, d, "a")
	b := startHab(t, d, "b")
	_, work, _ := a.AddSession("working")
	_, done, _ := a.AddSession("finished")
	_, blocked, _ := b.AddSession("waiting")
	s := open(t,
		hosts.Spec{Label: "one", Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: a.Sock}}},
		hosts.Spec{Label: "two", Endpoints: []hosts.Endpoint{{Kind: hosts.EndpointSocket, Value: b.Sock}}},
	)
	a.SetAgent(work, api.AgentStatusWorking, "")
	a.SetAgent(done, api.AgentStatusDone, "")
	b.SetAgent(blocked, api.AgentStatusBlocked, api.BlockedKindPermission)
	var o board.Order
	names := func() []string {
		rows := board.Rows(s.Hosts(), nil)
		o.Sort(rows)
		var out []string
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return out
	}
	want := "[waiting finished working]"
	waitHosts(t, s, "the three states", func([]hosts.Host) bool { return fmt.Sprint(names()) == want })

	// A second working session's later state change does not move the first.
	_, work2, _ := b.AddSession("working2")
	b.SetAgent(work2, api.AgentStatusWorking, "")
	// The fake files the pane a moment after it appears, so wait for the filed row.
	waitHosts(t, s, "the second working row", func(hs []hosts.Host) bool {
		names() // the order sees every step the rows take
		return slices.ContainsFunc(board.Rows(hs, nil), func(r board.Row) bool {
			return r.Name == "working2" && r.Rank == board.RankWorking
		})
	})
	got := names()
	if got[0] != "waiting" || got[1] != "finished" {
		t.Fatalf("order %v", got)
	}
	a.SetAgent(work, api.AgentStatusWorking, "") // same rank, new time: stays put
	waitHosts(t, s, "the repeat", func(hs []hosts.Host) bool {
		for _, h := range hs {
			for _, ag := range h.Structure.Agents {
				if ag.Pane == work && ag.Revision >= 2 {
					return true
				}
			}
		}
		return false
	})
	if again := names(); fmt.Sprint(again) != fmt.Sprint(got) {
		t.Fatalf("a same-rank change moved rows: %v, then %v", got, again)
	}
	if n := board.NeedsYou(board.Rows(s.Hosts(), nil)); n != 2 {
		t.Errorf("needs you: %d, want 2 (blocked and done)", n)
	}
}

// TestSameDeviceOnce: two labels that reach one hab show its rows once (mesh.md 2).
func TestSameDeviceOnce(t *testing.T) {
	d := shortDir(t)
	a := startHab(t, d, "a")
	a.AddSession("alpha")
	link := filepath.Join(d, "alias.sock")
	if err := os.Symlink(a.Sock, link); err != nil {
		t.Fatal(err)
	}
	s := open(t,
		hosts.Spec{Label: "one", Transport: habconn.UnixTransport{Path: a.Sock}},
		hosts.Spec{Label: "again", Transport: habconn.UnixTransport{Path: link}},
	)
	waitHosts(t, s, "both online", func(hs []hosts.Host) bool {
		return status(hs, "one") == hosts.Online && status(hs, "again") == hosts.Online
	})
	rows := board.Rows(s.Hosts(), nil)
	if len(rows) != 1 || rows[0].Host != "one" {
		t.Fatalf("rows %+v", rows)
	}
}

// TestReload: a host added to the source starts; one removed closes and reports Removed.
func TestReload(t *testing.T) {
	d := shortDir(t)
	a := startHab(t, d, "a")
	b := startHab(t, d, "b")
	src := &listSource{specs: []hosts.Spec{{Label: "one", Transport: habconn.UnixTransport{Path: a.Sock}}}}
	s, err := hosts.Open(hosts.Config{Source: src, Client: api.ClientInfo{Name: "test", Kind: api.ClientKindTerminal}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	waitHosts(t, s, "one online", func(hs []hosts.Host) bool { return status(hs, "one") == hosts.Online })
	src.specs = []hosts.Spec{{Label: "two", Transport: habconn.UnixTransport{Path: b.Sock}}}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	removed := false
	timer := time.NewTimer(waitLong())
	defer timer.Stop()
	for !removed || status(s.Hosts(), "two") != hosts.Online {
		select {
		case u := <-s.Updates():
			if u.Removed && u.Host == "one" {
				removed = true
			}
		case <-timer.C:
			t.Fatalf("reload: removed %v, hosts %+v", removed, s.Hosts())
		}
	}
	if len(s.Hosts()) != 1 {
		t.Fatalf("hosts after reload: %+v", s.Hosts())
	}
}

type listSource struct{ specs []hosts.Spec }

func (l *listSource) Hosts() ([]hosts.Spec, error) { return l.specs, nil }

func TestConfig(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "config.toml")
	os.WriteFile(p, []byte(`
[[hosts]]
label = "box"
endpoints = ["box.example", "/run/hab.sock", "cmd:ssh box gff-hab connect"]
device_id = "SHA256:abc"

[[hosts]]
label = "box"
endpoints = ["other"]

[[hosts]]
endpoints = ["x"]

[[hosts]]
label = "bad"
endpoints = ["-oProxyCommand=x"]
`), 0o600)
	specs, problems, err := hosts.LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Label != "box" || specs[0].DeviceID != "SHA256:abc" || len(specs[0].Endpoints) != 3 ||
		specs[0].Endpoints[0].Kind != hosts.EndpointSSH || specs[0].Endpoints[1].Kind != hosts.EndpointSocket ||
		specs[0].Endpoints[2].Kind != hosts.EndpointCommand || specs[0].Endpoints[2].Value != "ssh box gff-hab connect" {
		t.Fatalf("specs %+v", specs)
	}
	if len(problems) != 3 {
		t.Fatalf("problems %q", problems)
	}
	if s, _, err := hosts.LoadConfig(filepath.Join(d, "none.toml")); err != nil || s != nil {
		t.Fatalf("missing file: %v %v", s, err)
	}
	env := map[string]string{"XDG_CONFIG_HOME": "/x"}
	home := func() (string, error) { return "/h", nil }
	for _, c := range []struct{ goos, want string }{{"linux", "/x/gff/config.toml"}, {"darwin", "/h/Library/Application Support/gff/config.toml"}} {
		if got, _ := hosts.ConfigPath(c.goos, func(k string) string { return env[k] }, home); got != c.want {
			t.Errorf("%s: %s, want %s", c.goos, got, c.want)
		}
	}
}
