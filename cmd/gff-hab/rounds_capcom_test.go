package main

// Capcom and group rounds across two habs (rounds.md 5.2, 14.1): two gff-hab processes over the
// ssh stand-in. b runs the Capcom service with the stub engine (capcom-service.md 9.5), and
// stands in a group record from a file (roundfake.go). A capcom round on a is refused until a's
// copy of b's catalog shows the service; a run hands one message, with the round fields and the
// id the run's id gives, b's thread takes it as one round item, and the outcome comes back; with
// b stopped the run ends queued for Capcom, and once b is back the message is taken once and its
// outcome reaches a; with round messages off b declines it. A group round with no coordinator
// skips and keeps its batch; once its group ended, the round completes.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
)

// startWith is start with more environment for gff-hab, and more of the hab's config file.
func (p *peerRoundEnv) startWith(t *testing.T, standIn, hostID string, env ...string) {
	p.startWithConfig(t, standIn, hostID, "", env...)
}

func (p *peerRoundEnv) startWithConfig(t *testing.T, standIn, hostID, extra string, env ...string) {
	t.Helper()
	cfg := filepath.Join(p.work, "hab.toml")
	writeFile(t, cfg, "[rounds]\nwake_settle = \"0s\"\n\n[catalog]\nproject_roots = ["+tomlString(p.work)+"]\n"+extra)
	p.path = "PATH=" + standIn + string(os.PathListSeparator) + os.Getenv("PATH")
	p.env = append([]string{"GFF_HAB_CONFIG=" + cfg, "GFF_TEST_ROUND_TICK=200ms", p.path, "GFF_HAB_PEERS=" + p.peers,
		"XDG_RUNTIME_DIR=" + p.rt, "GFF_TEST_ROUND_MIN_BATCH=500ms"}, env...)
	p.srv = startServeWith(t, p.sock, p.state, p.env, "-holders", p.hdir, "-host-id", hostID)
}

// startCapcom starts gff-hab running the Capcom service with the stub engine: no quiet hours,
// and each event in a batch of its own, so a round message's turn runs at once.
func (p *peerRoundEnv) startCapcom(t *testing.T, standIn, hostID string, env ...string) (stubDir string) {
	t.Helper()
	d := shortDir(t)
	stub := buildCapcomStub(t, d)
	stubDir = filepath.Join(d, "stub")
	os.MkdirAll(stubDir, 0o700)
	t.Cleanup(func() {
		b, err := os.ReadFile(filepath.Join(p.state, "capcom", capcomengine.PidFile))
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
			syscall.Kill(pid, syscall.SIGKILL) // the stub engine this test started
		}
	})
	extra := "\n[capcom]\nengine = \"claude\"\nrestart_delay = \"1s\"\n\n[settings]\n\"capcom.quiet_hours\" = \"00:00-00:00\"\n\"capcom.events.batch_max\" = 1\n"
	env = append(env, "GFF_HAB_CAPCOM_ENGINE="+stub, "STUB_DIR="+stubDir, "CLAUDE_CODE_OAUTH_TOKEN="+plantedToken)
	p.startWithConfig(t, standIn, hostID, extra, env...)
	return stubDir
}

// roundItems are the round items of a hab's Capcom thread, oldest first.
func roundItems(t *testing.T, p *peerRoundEnv) []api.CapcomItem {
	t.Helper()
	items := threadText(t, p.control())
	var out []api.CapcomItem
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Kind == "round" {
			out = append(out, items[i])
		}
	}
	return out
}

func TestRoundCapcomPeers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	a := newPeerRoundEnv(t, "bee", "hab-b")
	b := newPeerRoundEnv(t, "ay", "hab-a")
	groups := filepath.Join(b.work, "groups.json")
	writeFile(t, groups, `[{"id":"g1","state":"running"}]`)
	standIn := sshStandIn(t, map[string]string{"hab-a": a.sock, "hab-b": b.sock})
	a.startWith(t, standIn, "host-a")
	stubB := b.startCapcom(t, standIn, "host-b", "GFF_TEST_GROUPS="+groups)
	waitPeer(t, a.sock, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	waitPeer(t, b.sock, "ay", "b sees a online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	ca := a.control()

	// No Capcom home yet: a capcom round is refused.
	if out, err := a.cli("", "", "add", "cc", "-mode", "capcom", "-manual", "check the builds"); err == nil || !strings.Contains(out, "no Capcom service") {
		t.Fatalf("capcom round with no service: %v %s", err, out)
	}
	run(t, "capcom", "assign", "-socket", b.sock, "self")
	waitUntil(t, "a's copy of b shows the Capcom service", func() bool {
		e, ok := hostEntry(catalogOf(t, ca), "host-b")
		return ok && hasWord(e.Catalog, capCapcomService) && e.Catalog.Capcom != nil && e.Catalog.Capcom.Home == e.Catalog.DeviceID &&
			e.Catalog.Capcom.Service != ""
	})
	a.must(t, "", "add", "cc", "-mode", "capcom", "-manual", "check the builds")

	// A run hands one message and is accepted; b's thread holds one round item; the outcome
	// follows the event turn that took it.
	a.must(t, "", "run", "cc")
	r := waitRun(t, a.habEnv, ca, "cc", api.RoundRunStateSucceeded)
	if r.Reason != reasonAccepted || r.Destination != "Capcom on bee" || r.Message != roundMessageID(r.ID) {
		t.Fatalf("run %+v", r)
	}
	got := roundItems(t, b)
	if len(got) != 1 || got[0].Text != "check the builds" || got[0].Author.Address != "host-a/round:cc" {
		t.Fatalf("round items %+v", got)
	}
	waitUntil(t, "the outcome", func() bool {
		runs := roundRuns(t, ca, "cc")
		return len(runs) == 1 && runs[0].Outcome != nil && runs[0].Outcome.Word == "told"
	})

	// b stopped: the run ends queued for Capcom; b back: the message is taken once, and its
	// outcome reaches a.
	b.stopHab(false)
	waitPeer(t, a.sock, "bee", "a sees b offline", func(p api.Peer) bool { return p.State != api.PeerStateOnline })
	a.must(t, "", "run", "cc")
	waitUntil(t, "the second run queued for Capcom", func() bool {
		runs := roundRuns(t, ca, "cc")
		return len(runs) == 2 && runs[0].State == api.RoundRunStateSucceeded && runs[0].Reason == reasonQueuedFor+"bee"
	})
	b.srv = startServeWith(t, b.sock, b.state, b.env, "-holders", b.hdir, "-host-id", "host-b")
	waitUntil(t, "b takes the queued message", func() bool { return len(roundItems(t, b)) == 2 })
	waitUntil(t, "the queued run's outcome", func() bool {
		runs := roundRuns(t, ca, "cc")
		return runs[0].Outcome != nil && runs[0].Outcome.Word == "told"
	})
	if n := len(roundItems(t, b)); n != 2 {
		t.Fatalf("round items after the restart: %d", n)
	}

	// An event round's batch goes with its message: b's round item lists its events.
	waitPeer(t, a.sock, "bee", "a sees b online again", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	a.must(t, "", "add", "cc-ev", "-mode", "capcom", "-on", "notice.raised", "-debounce", "1h", "look at these")
	if out, err := exec.Command(bin, "notify", "-socket", a.sock, "-level", "active", "disk full").CombinedOutput(); err != nil {
		t.Fatalf("notify: %v %s", err, out)
	}
	waitUntil(t, "one pending", func() bool { return getRound(t, ca, "cc-ev").Pending == 1 })
	a.must(t, "", "run", "cc-ev")
	r = waitRun(t, a.habEnv, ca, "cc-ev", api.RoundRunStateSucceeded)
	if r.Events != 1 {
		t.Fatalf("event run %+v", r)
	}
	last := roundItems(t, b)
	if it := last[len(last)-1]; it.Text != "look at these" {
		t.Fatalf("event round item %+v", it)
	}

	// The runner sleeps through the turn: the outcome waits in b's outbox and reaches a at its
	// wake, once.
	a.must(t, "", "add", "cs", "-mode", "capcom", "-manual", "slow")
	a.must(t, "", "run", "cs")
	if r := waitRun(t, a.habEnv, ca, "cs", api.RoundRunStateSucceeded); r.Reason != reasonAccepted {
		t.Fatalf("slow run %+v", r)
	}
	a.stopHab(false)
	writeFile(t, filepath.Join(stubB, "release"), "")
	waitUntil(t, "b's turn on the slow message ended", func() bool { _, ok := replyAfter(threadText(t, b.control()), "slow"); return ok })
	a.srv = startServeWith(t, a.sock, a.state, a.env, "-holders", a.hdir, "-host-id", "host-a")
	ca = a.control()
	waitUntil(t, "the outcome at a's wake", func() bool {
		runs := roundRuns(t, ca, "cs")
		return len(runs) == 1 && runs[0].Outcome != nil && runs[0].Outcome.Word == "told"
	})

	// Round messages off on b: declined off.
	waitPeer(t, a.sock, "bee", "a sees b online after its restart", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	if err := setSetting(b.control(), "capcom.events.round_messages", "false"); err != nil {
		t.Fatal(err)
	}
	a.must(t, "", "run", "cc")
	if r := waitRun(t, a.habEnv, ca, "cc", api.RoundRunStateSkipped); r.Reason != "declined by Capcom: off" {
		t.Fatalf("declined run %+v", r)
	}

	// A group with no coordinator: skipped, its batch kept. The group ends: the round completes.
	waitUntil(t, "a's copy of b shows the group", func() bool {
		e, ok := hostEntry(catalogOf(t, ca), "host-b")
		return ok && len(e.Catalog.Groups) == 1
	})
	a.must(t, "", "add", "gr", "-mode", "group", "-group", "g1", "-on", "notice.raised", "-debounce", "1h", "sum up")
	if out, err := exec.Command(bin, "notify", "-socket", a.sock, "late build").CombinedOutput(); err != nil {
		t.Fatalf("notify: %v %s", err, out)
	}
	waitUntil(t, "one pending in gr", func() bool { return getRound(t, ca, "gr").Pending == 1 })
	a.must(t, "", "run", "gr")
	if r := waitRun(t, a.habEnv, ca, "gr", api.RoundRunStateSkipped); r.Reason != reasonCoordinatorOff {
		t.Fatalf("group run %+v", r)
	}
	waitUntil(t, "gr's batch back", func() bool { return getRound(t, ca, "gr").Pending == 1 })
	writeFile(t, groups, `[{"id":"g1","state":"ended"}]`)
	e2eSession(t, b, b.control()) // a structure change rebuilds b's catalog
	waitUntil(t, "a's copy of b shows the group ended", func() bool {
		e, _ := hostEntry(catalogOf(t, ca), "host-b")
		return len(e.Catalog.Groups) == 1 && e.Catalog.Groups[0].State == "ended"
	})
	a.must(t, "", "run", "gr")
	waitUntil(t, "gr completed", func() bool { return getRound(t, ca, "gr").Completed })
	if runs := roundRuns(t, ca, "gr"); len(runs) != 2 || runs[0].Reason != "group ended" {
		t.Fatalf("group runs %+v", runs)
	}
}

// TestRoundGroupPeers: a group round on a whose coordinator is a fake Claude Code session on b:
// the message is delivered there in the round envelope, once; while the coordinator works, the
// round lock holds the message until its deadline, and the run skips with its batch kept.
func TestRoundGroupPeers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	a := newPeerRoundEnv(t, "bee", "hab-b")
	b := newPeerRoundEnv(t, "ay", "hab-a")
	fake, work := shortDir(t), shortDir(t)
	writeFile(t, filepath.Join(fake, "claude"), strings.ReplaceAll(fakeTarget, "@DIR@", work))
	if err := os.Chmod(filepath.Join(fake, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "transcript-claude.jsonl"), claudeTranscript("Round reply."))
	groups := filepath.Join(b.work, "groups.json")
	writeFile(t, groups, `[]`)
	standIn := sshStandIn(t, map[string]string{"hab-a": a.sock, "hab-b": b.sock})
	a.startWith(t, standIn, "host-a")
	b.startWith(t, standIn, "host-b", "GFF_TEST_GROUPS="+groups,
		"PATH="+fake+string(os.PathListSeparator)+standIn+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(work, "claude"), "GFF_HAB_BIN="+bin, noRaceExitSleep, "GFF_TEST_ROUND_QUIET=0s")
	waitPeer(t, a.sock, "bee", "a sees b online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	waitPeer(t, b.sock, "ay", "b sees a online", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	ca, cb := a.control(), b.control()

	lead := startTarget(t, b.habEnv, cb, b.repo, work, "claude", "lead")
	idB := welcome(t, b.sock).DeviceID
	writeFile(t, groups, `[{"id":"g1","state":"running","coordinator":{"device":"`+idB+`","session":"`+lead.Session+`","name":"lead"}}]`)
	e2eSession(t, b, cb) // a structure change rebuilds b's catalog
	waitUntil(t, "a's copy of b shows the group's coordinator", func() bool {
		e, ok := hostEntry(catalogOf(t, ca), "host-b")
		return ok && len(e.Catalog.Groups) == 1 && e.Catalog.Groups[0].Coordinator != nil
	})

	a.must(t, "", "add", "gr", "-mode", "group", "-group", "g1", "-manual", "-wait", "5s", "sum up the day")
	a.must(t, "", "run", "gr")
	r := waitRun(t, a.habEnv, ca, "gr", api.RoundRunStateSucceeded)
	if r.Destination != "bee/lead" {
		t.Errorf("run %+v", r)
	}
	waitUntil(t, "the coordinator got the message", func() bool { return strings.Count(inputOf(work, "lead"), "[end round message") == 1 })
	in := inputOf(work, "lead")
	if !strings.Contains(in, "a message from round 'gr' on host host-a") || !strings.Contains(in, "sum up the day") {
		t.Fatalf("coordinator input:\n%s", in)
	}

	// The coordinator works: the lock holds the message past its deadline; the run skips.
	order(t, work, "lead", "hold")
	order(t, work, "lead", "working")
	waitUntil(t, "lead working", func() bool { return agentStates(t, b.sock)[lead.Pane].State == api.AgentStatusWorking })
	a.must(t, "", "run", "gr")
	r = waitRun(t, a.habEnv, ca, "gr", api.RoundRunStateSkipped)
	// The runner's wait and the far side's deadline end together: either row of 5.2 may win.
	if !strings.HasPrefix(r.Reason, "in use: ") && r.Reason != "not delivered in 5s: handed" {
		t.Errorf("held run %+v", r)
	}
	if n := strings.Count(inputOf(work, "lead"), "[end round message"); n != 1 {
		t.Errorf("the held message reached the coordinator: %d messages", n)
	}
}

// TestRoundCapcomOneWay: a capcom run whose home host can dial this hab, though this hab cannot
// dial it, is accepted: the home host pulls its mail over its own link, so it is awake, and the
// run does not end "queued for Capcom" (rounds.md 5.2, the outbox for a sleeping home host). The
// outcome travels back the same way.
func TestRoundCapcomOneWay(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	a := newPeerRoundEnv(t, "bee", "hab-b")
	b := newPeerRoundEnv(t, "ay", "hab-a")
	// Only b dials: the stand-in knows no hab-b.
	standIn := sshStandIn(t, map[string]string{"hab-a": a.sock})
	b.startCapcom(t, standIn, "host-b")
	// a lists b at an endpoint it cannot reach, pinned, so b's link in is known as bee.
	writeFile(t, a.peers, fmt.Sprintf("[[peers]]\nlabel = \"bee\"\nendpoints = [\"hab-b\"]\ndevice_id = %q\n", welcome(t, b.sock).DeviceID))
	a.startWith(t, standIn, "host-a")
	waitPeer(t, a.sock, "bee", "b linked in to a", func(p api.Peer) bool { return p.State == api.PeerStateOnline && p.Inbound })
	ca := a.control()
	run(t, "capcom", "assign", "-socket", b.sock, "self")
	waitUntil(t, "a's copy of b shows the Capcom service", func() bool {
		e, ok := hostEntry(catalogOf(t, ca), "host-b")
		return ok && hasWord(e.Catalog, capCapcomService) && e.Catalog.Capcom != nil && e.Catalog.Capcom.Home == e.Catalog.DeviceID
	})
	a.must(t, "", "add", "cc", "-mode", "capcom", "-manual", "check the builds")
	a.must(t, "", "run", "cc")
	r := waitRun(t, a.habEnv, ca, "cc", api.RoundRunStateSucceeded)
	if r.Reason != reasonAccepted {
		t.Fatalf("run %+v, want %q", r, reasonAccepted)
	}
	if got := roundItems(t, b); len(got) != 1 || got[0].Text != "check the builds" {
		t.Fatalf("round items %+v", got)
	}
	waitUntil(t, "the outcome", func() bool {
		runs := roundRuns(t, ca, "cc")
		return len(runs) == 1 && runs[0].Outcome != nil && runs[0].Outcome.Word == "told"
	})
}
