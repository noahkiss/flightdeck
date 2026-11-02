package main

// Rounds on other hosts (rounds.md 9, 11): two gff-hab processes, each with its own holder,
// linked as peers over the ssh stand-in. Each lists the other's rounds in catalog.list; a round
// is created on the peer with -host; a fresh round moves with its next slot kept; a resume
// round does not move; a stopped peer's rounds stay, stale with their age, and an edit to them
// fails "host offline".

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// peerRoundEnv is one hab of a peer pair: a holder, a gff-hab with rounds and a peer file, and
// a git checkout of the shared project, under its catalog's project root.
type peerRoundEnv struct {
	*habEnv
	repo, peers, rt, path string
}

const peerRemote = "https://example.com/team/app.git"

// newPeerRoundEnv makes the holder, the checkout and the files; start runs gff-hab.
func newPeerRoundEnv(t *testing.T, peerLabel, peerEndpoint string) *peerRoundEnv {
	t.Helper()
	e := newHolderEnv(t, 0)
	p := &peerRoundEnv{habEnv: e, repo: filepath.Join(e.work, "app"), peers: filepath.Join(e.work, "peers.toml"), rt: shortDir(t)}
	if out, err := exec.Command("git", "init", "-q", p.repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", p.repo, "remote", "add", "origin", peerRemote).CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v %s", err, out)
	}
	writePeers(t, p.peers, peerLabel, peerEndpoint)
	return p
}

func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// start runs gff-hab with the stand-in first on PATH.
func (p *peerRoundEnv) start(t *testing.T, standIn, hostID string) {
	t.Helper()
	cfg := filepath.Join(p.work, "hab.toml")
	writeFile(t, cfg, "[rounds]\nwake_settle = \"0s\"\n\n[catalog]\nproject_roots = ["+tomlString(p.work)+"]\n")
	p.path = "PATH=" + standIn + string(os.PathListSeparator) + os.Getenv("PATH")
	p.env = []string{"GFF_HAB_CONFIG=" + cfg, "GFF_TEST_ROUND_TICK=200ms", p.path, "GFF_HAB_PEERS=" + p.peers, "XDG_RUNTIME_DIR=" + p.rt}
	p.srv = startServeWith(t, p.sock, p.state, p.env, "-holders", p.hdir, "-host-id", hostID)
}

// cli runs gff-hab round on this hab's socket, from dir (the current folder when empty), with
// the stand-in first on PATH, so -host reaches the peer.
func (p *peerRoundEnv) cli(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command(bin, append([]string{"round"}, args...)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), "GFF_HAB_SOCKET="+p.sock, "GFF_HAB=", "XDG_RUNTIME_DIR="+p.rt, p.path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (p *peerRoundEnv) must(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := p.cli(dir, "", args...)
	if err != nil {
		t.Fatalf("gff-hab round %v: %v\n%s", args, err, out)
	}
	return out
}

func catalogOf(t *testing.T, c *control.Client) api.CatalogList {
	t.Helper()
	var l api.CatalogList
	call(t, c, "catalog.list", api.CatalogListParams{}, &l)
	return l
}

// hostEntry is the entry of the host with that host id.
func hostEntry(l api.CatalogList, host string) (api.CatalogEntry, bool) {
	for _, h := range l.Hosts {
		if h.Catalog.Host == host {
			return h, true
		}
	}
	return api.CatalogEntry{}, false
}

func roundNames(e api.CatalogEntry) []string {
	var out []string
	for _, r := range e.Catalog.Rounds {
		out = append(out, r.Name)
	}
	return out
}

func hasRound(c *control.Client, name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var r api.RoundResult
	return c.Call(ctx, "round.get", api.RoundRef{Name: name}, &r) == nil
}

func TestRoundPeers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	a := newPeerRoundEnv(t, "bee", "hab-b")
	b := newPeerRoundEnv(t, "ay", "hab-a")
	standIn := sshStandIn(t, map[string]string{"hab-a": a.sock, "hab-b": b.sock})
	a.start(t, standIn, "host-a")
	b.start(t, standIn, "host-b")
	waitPeer(t, a.sock, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	waitPeer(t, b.sock, "ay", "b sees a online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	ca, cb := a.control(), b.control()

	// A round on a; one made on b from a by this folder's remote, while b's structure has no
	// project yet: only b's catalog scan found the checkout; one by folder.
	a.must(t, "", "add", "nightly", "-every", "1h", "-project", a.repo, "-command", "--", "true")
	waitUntil(t, "a's and b's catalogs have their projects", func() bool {
		ea, _ := hostEntry(catalogOf(t, ca), "host-a")
		eb, _ := hostEntry(catalogOf(t, cb), "host-b")
		return len(ea.Catalog.Projects) > 0 && len(eb.Catalog.Projects) > 0
	})
	a.must(t, a.repo, "add", "weekly", "-host", "bee", "-cron", "0 9 * * 1", "-tz", "UTC", "-command", "--", "true")
	a.must(t, "", "add", "daily", "-host", "bee", "-every", "1d", "-tz", "UTC", "-project", b.repo, "-command", "--", "true")
	if !hasRound(cb, "daily") || !hasRound(cb, "weekly") || hasRound(ca, "daily") {
		t.Fatal("the -host rounds are not on b alone")
	}
	if r := getRound(t, cb, "weekly"); r.ProjectRemote != "example.com/team/app" && r.ProjectRemote != peerRemote {
		t.Errorf("weekly's project on b: %+v", r)
	}

	// Each lists the other's rounds.
	waitUntil(t, "a lists b's rounds", func() bool {
		e, ok := hostEntry(catalogOf(t, ca), "host-b")
		return ok && !e.Stale && strings.Join(roundNames(e), ",") == "daily,weekly"
	})
	waitUntil(t, "b lists a's round", func() bool {
		e, ok := hostEntry(catalogOf(t, cb), "host-a")
		return ok && strings.Join(roundNames(e), ",") == "nightly"
	})
	e, _ := hostEntry(catalogOf(t, ca), "host-b")
	if r := e.Catalog.Rounds[0]; r.Trigger != "every 1d in UTC" || r.Mode != api.RoundModeFresh || r.NextSlot == "" || r.Project == "" {
		t.Errorf("b's daily in a's catalog: %+v", r)
	}
	ls := a.must(t, "", "ls", "-all")
	for _, w := range []string{"nightly", "bee", "daily", "weekly", "(here)"} {
		if !strings.Contains(ls, w) {
			t.Errorf("ls -all lacks %q:\n%s", w, ls)
		}
	}
	if out := a.must(t, "", "ls", "-host", "bee"); !strings.Contains(out, "weekly") || strings.Contains(out, "nightly") {
		t.Errorf("ls -host bee:\n%s", out)
	}

	// The -host connection: an online peer row pinned to b's id reaches b; a pin b does not
	// present is refused; a row with no pin and an offline row are refused before any dial,
	// though b's socket would answer.
	idB := welcome(t, b.sock).DeviceID
	row := func(state api.PeerState, id string) caller {
		return &moveFake{handlers: map[string]func(context.Context, any) (any, error){
			"peers.list": answer(api.PeerList{Peers: []api.Peer{{Label: "x", State: state, Endpoint: b.sock, DeviceID: id, LastSeen: "2026-11-02T08:00:00Z"}}}),
		}}
	}
	ctx := context.Background()
	pc, pdone, err := peerCaller(ctx, row(api.PeerStateOnline, idB), "x")
	if err != nil {
		t.Fatalf("a pinned online peer: %v", err)
	}
	var got api.RoundResult
	if err := pc.Call(ctx, "round.get", api.RoundRef{Name: "daily"}, &got); err != nil {
		t.Errorf("round.get through the peer connection: %v", err)
	}
	pdone()
	for _, c := range []struct {
		state api.PeerState
		id    string
		want  string
	}{
		{api.PeerStateOnline, strings.Repeat("0f", 32), "not the pinned"},
		{api.PeerStateOnline, "", "host not pinned"},
		{api.PeerStateOffline, idB, "host offline: x is offline, last seen 2026-11-02T08:00:00Z"},
	} {
		if _, _, err := peerCaller(ctx, row(c.state, c.id), "x"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("peer row %s %q: %v, want %q", c.state, c.id, err, c.want)
		}
	}

	// Move nightly to b: only b has it after, with the same next slot.
	before := getRound(t, ca, "nightly")
	if out, err := a.cli("", "n\n", "move", "nightly", "-host", "bee"); err == nil || !hasRound(ca, "nightly") || hasRound(cb, "nightly") {
		t.Fatalf("a move answered no: %v %s", err, out)
	}
	a.must(t, "", "move", "nightly", "-host", "bee", "-yes")
	if hasRound(ca, "nightly") {
		t.Fatal("nightly is still on a")
	}
	after := getRound(t, cb, "nightly")
	if after.NextSlot != before.NextSlot || after.Paused || after.Trigger.Cron != before.Trigger.Cron {
		t.Fatalf("moved: before %+v, after %+v", before, after)
	}

	// A resume round does not move.
	sess := e2eSession(t, a, ca)
	a.must(t, "", "add", "check", "-mode", "resume", "-target", sess, "-manual", "look again")
	if out, err := a.cli("", "", "move", "check", "-host", "bee", "-yes"); err == nil || !strings.Contains(out, "cannot move") || !hasRound(ca, "check") {
		t.Fatalf("resume move: %v %s", err, out)
	}

	// b stops: its rounds stay in a's view, stale with the copy's age; an edit fails.
	waitUntil(t, "a lists the moved round on b", func() bool {
		e, _ := hostEntry(catalogOf(t, ca), "host-b")
		return len(e.Catalog.Rounds) == 3
	})
	b.stopHab(false)
	waitUntil(t, "b stale in a's view", func() bool {
		e, ok := hostEntry(catalogOf(t, ca), "host-b")
		return ok && e.Stale && len(e.Catalog.Rounds) == 3
	})
	e, _ = hostEntry(catalogOf(t, ca), "host-b")
	if at, err := time.Parse(time.RFC3339, e.SeenAt); err != nil || time.Since(at) > time.Minute {
		t.Errorf("seen_at %q: %v", e.SeenAt, err)
	}
	if out := a.must(t, "", "ls", "-all"); !strings.Contains(out, "host asleep since "+e.SeenAt) {
		t.Errorf("ls -all with b asleep:\n%s", out)
	}
	if out, err := a.cli("", "", "pause", "daily", "-host", "bee"); err == nil || !strings.Contains(out, "host offline") {
		t.Errorf("an edit to an offline runner: %v %s", err, out)
	}
	if out, err := a.cli("", "", "add", "late", "-host", "bee", "-manual", "-project", b.repo, "-command", "--", "true"); err == nil || !strings.Contains(out, "host offline") {
		t.Errorf("an add on an offline runner: %v %s", err, out)
	}
}

// e2eSession starts a plain session on p and returns its id.
func e2eSession(t *testing.T, p *peerRoundEnv, c *control.Client) string {
	t.Helper()
	var res api.SessionCreated
	call(t, c, "session.create", api.SessionCreateParams{Argv: []string{"/bin/sh", "-c", "sleep 600"}, Cwd: p.repo, Cols: 80, Rows: 24}, &res)
	return res.Session
}
