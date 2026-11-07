package main

// Capcom and group rounds across two habs (rounds.md 5.2, 14.1): two gff-hab processes over the
// ssh stand-in, b standing in a Capcom service and a group record from files (roundfake.go).
// A capcom round on a is refused until a's copy of b's catalog shows the service; a run hands
// one message, with the round fields and the id the run's id gives, and gets accepted, then the
// outcome; with b stopped the run ends queued for Capcom, and once b is back the message is
// taken once; a refusal skips the run and keeps its batch. A group round with no coordinator
// skips and keeps its batch; once its group ended, the round completes.

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// startWith is start with more environment for gff-hab.
func (p *peerRoundEnv) startWith(t *testing.T, standIn, hostID string, env ...string) {
	t.Helper()
	cfg := filepath.Join(p.work, "hab.toml")
	writeFile(t, cfg, "[rounds]\nwake_settle = \"0s\"\n\n[catalog]\nproject_roots = ["+tomlString(p.work)+"]\n")
	p.path = "PATH=" + standIn + string(os.PathListSeparator) + os.Getenv("PATH")
	p.env = append([]string{"GFF_HAB_CONFIG=" + cfg, "GFF_TEST_ROUND_TICK=200ms", p.path, "GFF_HAB_PEERS=" + p.peers,
		"XDG_RUNTIME_DIR=" + p.rt, "GFF_TEST_ROUND_MIN_BATCH=500ms"}, env...)
	p.srv = startServeWith(t, p.sock, p.state, p.env, "-holders", p.hdir, "-host-id", hostID)
}

// taken reads the messages the stand-in Capcom service took.
func taken(t *testing.T, path string) []api.ChannelMessage {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []api.ChannelMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m api.ChannelMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("got file: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestRoundCapcomPeers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	a := newPeerRoundEnv(t, "bee", "hab-b")
	b := newPeerRoundEnv(t, "ay", "hab-a")
	answer := filepath.Join(b.work, "capcom-answer")
	groups := filepath.Join(b.work, "groups.json")
	writeFile(t, answer, "accepted\noutcome told\n")
	writeFile(t, groups, `[{"id":"g1","state":"running"}]`)
	standIn := sshStandIn(t, map[string]string{"hab-a": a.sock, "hab-b": b.sock})
	a.startWith(t, standIn, "host-a")
	b.startWith(t, standIn, "host-b", "GFF_TEST_CAPCOM_SERVICE="+answer, "GFF_TEST_GROUPS="+groups)
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
		return ok && hasWord(e.Catalog, capCapcomService) && e.Catalog.Capcom != nil && e.Catalog.Capcom.Home == e.Catalog.DeviceID
	})
	a.must(t, "", "add", "cc", "-mode", "capcom", "-manual", "check the builds")

	// A run hands one message and is accepted; the outcome follows.
	a.must(t, "", "run", "cc")
	r := waitRun(t, a.habEnv, ca, "cc", api.RoundRunStateSucceeded)
	if r.Reason != reasonAccepted || r.Destination != "Capcom on bee" || r.Message != roundMessageID(r.ID) {
		t.Fatalf("run %+v", r)
	}
	got := taken(t, answer+".got")
	if len(got) != 1 || got[0].ID != r.Message || got[0].Text != "check the builds" || got[0].Round == nil ||
		got[0].Round.Run != r.ID || got[0].Round.Name != "cc" || got[0].Round.Trigger != "manual" {
		t.Fatalf("taken %+v", got)
	}
	waitUntil(t, "the outcome", func() bool {
		runs := roundRuns(t, ca, "cc")
		return len(runs) == 1 && runs[0].Outcome != nil && runs[0].Outcome.Word == "told"
	})

	// b stopped: the run ends queued for Capcom; b back: the message is taken once.
	b.stopHab(false)
	waitPeer(t, a.sock, "bee", "a sees b offline", func(p api.Peer) bool { return p.State != api.PeerStateOnline })
	a.must(t, "", "run", "cc")
	waitUntil(t, "the second run queued for Capcom", func() bool {
		runs := roundRuns(t, ca, "cc")
		return len(runs) == 2 && runs[0].State == api.RoundRunStateSucceeded && runs[0].Reason == reasonQueuedFor+"bee"
	})
	b.srv = startServeWith(t, b.sock, b.state, b.env, "-holders", b.hdir, "-host-id", "host-b")
	waitUntil(t, "b takes the queued message", func() bool { return len(taken(t, answer+".got")) == 2 })
	second := roundRuns(t, ca, "cc")[0]
	if g := taken(t, answer+".got"); g[1].ID != second.Message {
		t.Fatalf("taken %+v, want %s", g[1], second.Message)
	}
	waitUntil(t, "the queued run's outcome", func() bool {
		runs := roundRuns(t, ca, "cc")
		return runs[0].Outcome != nil && runs[0].Outcome.Word == "told"
	})

	// A refusal skips the run and keeps its batch. a's own link to b must be up first: b may
	// take its mail over the link it dialed while a's is still connecting, and a message queued
	// for an offline home host ends the run "queued for Capcom".
	waitPeer(t, a.sock, "bee", "a sees b online again", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	writeFile(t, answer, "refused Capcom moved to elsewhere\n")
	a.must(t, "", "add", "cc-ev", "-mode", "capcom", "-on", "notice.raised", "-debounce", "1h", "look at these")
	if out, err := exec.Command(bin, "notify", "-socket", a.sock, "-level", "active", "disk full").CombinedOutput(); err != nil {
		t.Fatalf("notify: %v %s", err, out)
	}
	waitUntil(t, "one pending", func() bool { return getRound(t, ca, "cc-ev").Pending == 1 })
	a.must(t, "", "run", "cc-ev")
	r = waitRun(t, a.habEnv, ca, "cc-ev", api.RoundRunStateSkipped)
	if !strings.Contains(r.Reason, "Capcom moved to elsewhere") || r.Events != 1 {
		t.Fatalf("refused run %+v", r)
	}
	waitUntil(t, "the batch back", func() bool { return getRound(t, ca, "cc-ev").Pending == 1 })
	g := taken(t, answer+".got")
	if last := g[len(g)-1]; last.Round == nil || len(last.Round.Events) != 1 || last.Round.Events[0].Title != "disk full" || last.Round.Level != "active" {
		t.Fatalf("event message %+v", last.Round)
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
