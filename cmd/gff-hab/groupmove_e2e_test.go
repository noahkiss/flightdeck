package main

// End-to-end tests of moving a group's home hab (groups.md 5.7, 16.2 B9) with the real gff-hab
// and holder, habs over the ssh stand-in, real git and the fake Claude Code member of
// groups_e2e_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/groups"
)

// meshTrio is three habs, each with its own holder, all linked over the ssh stand-in. Each
// names the others ay, bee and sea.
type meshTrio struct {
	a, b, c *habEnv
}

func newMeshTrio(t *testing.T, path string, env ...string) *meshTrio {
	t.Helper()
	hs := []*habEnv{newHolderEnv(t, 0), newHolderEnv(t, 0), newHolderEnv(t, 0)}
	labels := []string{"ay", "bee", "sea"}
	ends := []string{"hab-a", "hab-b", "hab-c"}
	standIn := sshStandIn(t, map[string]string{"hab-a": hs[0].sock, "hab-b": hs[1].sock, "hab-c": hs[2].sock})
	d := shortDir(t)
	for i, e := range hs {
		var b strings.Builder
		b.WriteString("# test peers\n")
		for j := range hs {
			if j != i {
				fmt.Fprintf(&b, "[[peers]]\nlabel = %q\nendpoints = [%q]\n", labels[j], ends[j])
			}
		}
		file := filepath.Join(d, labels[i]+".toml")
		writeFile(t, file, b.String())
		e.env = append([]string{
			"PATH=" + path + string(os.PathListSeparator) + standIn + string(os.PathListSeparator) + os.Getenv("PATH"),
			"GFF_HAB_PEERS=" + file,
			"XDG_RUNTIME_DIR=" + shortDir(t),
		}, env...)
		e.serve()
	}
	for i, e := range hs {
		for j := range hs {
			if j != i {
				waitPeer(t, e.sock, labels[j], labels[i]+" sees "+labels[j], func(p api.Peer) bool { return p.State == api.PeerStateOnline })
			}
		}
	}
	t.Cleanup(func() {
		for _, e := range hs {
			writeFile(t, filepath.Join(e.work, "end"), "")
		}
	})
	return &meshTrio{a: hs[0], b: hs[1], c: hs[2]}
}

// deviceOf is a hab's own device id, from its catalog.
func deviceOf(t *testing.T, e *habEnv) string {
	t.Helper()
	var l api.CatalogList
	call(t, e.control(), "catalog.list", api.CatalogListParams{}, &l)
	for _, h := range l.Hosts {
		if h.Own {
			return h.Catalog.DeviceID
		}
	}
	t.Fatal("no own catalog")
	return ""
}

// groupErr is the error kind of group.get on e, or "" when e holds the group.
func groupErr(t *testing.T, e *habEnv, id string) api.ErrorKind {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var r api.GroupResult
	return errKind(e.control().Call(ctx, "group.get", api.GroupRef{Group: id}, &r))
}

// groupMoveCLI runs gff-hab group move on e and returns its output and error.
func groupMoveCLI(e *habEnv, id string, args ...string) (string, error) {
	cmd := exec.Command(bin, append([]string{"group", "move", id, "-socket", e.sock}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// homeIn waits until e's catalog finds the group's home at want.
func homeIn(t *testing.T, e *habEnv, id, want string) {
	t.Helper()
	waitUntil(t, "the catalog pointing at the new home", func() bool {
		var l api.CatalogList
		call(t, e.control(), "catalog.list", api.CatalogListParams{}, &l)
		w, ok := groups.Find(l, id)
		return ok && w.Device == want
	})
}

// startGroup proposes plan from alice on e, confirms it and returns the group id.
func startGroup(t *testing.T, e *habEnv, addr string, plan api.GroupPlan) string {
	t.Helper()
	pj, _ := json.Marshal(plan)
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(120*time.Second))
	defer cancel()
	tl, _ := findIn(actionTools, "group_create")
	done := make(chan error, 1)
	go func() {
		_, _, err := tl.run(ctx, hookCaller{addr}, toolArgs{"plan": string(pj)})
		done <- err
	}()
	var open []api.Confirmation
	waitUntil(t, "the plan's confirmation", func() bool { open = openConfirmations(t, e); return len(open) > 0 })
	call(t, e.control(), "confirm.answer", api.ConfirmAnswerParams{ID: open[0].ID, Choice: "confirm"}, &api.ConfirmationResult{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return open[0].Group.Group
}

// TestGroupMove (groups.md 16.2 B9): with the coordinator on a and its lead on a third hab c,
// a move from a pane is denied and a move to b, which lacks the project, is refused. Once b has
// it, gff-hab group move moves the record and the catalog entry to b; the coordinator gets one
// line; a's tombstone answers moved; a report sent to a is forwarded to b and reaches the
// coordinator; after c's catalog points at b, the lead's next report reaches b with a stopped.
// An ended group does not move.
func TestGroupMove(t *testing.T) {
	needGit(t)
	fakes, work := groupFakes(t)
	m := newMeshTrio(t, fakes, "GFF_HAB_BIN="+bin, "GFF_HAB_WORKTREES="+filepath.Join(shortDir(t), "wt"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(work, "claude"), noRaceExitSleep)
	repo := gitRepo(t, m.a.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	fileProject(t, m.c, repo)
	addr := startSpawner(t, m.a, work, repo)
	waitCatalogProject(t, m.a, "shop", 2)
	gid := startGroup(t, m.a, addr, api.GroupPlan{Title: "Add export", Brief: "Export the board.", Project: "shop",
		Items:   []api.GroupPlanItem{{ID: "api", Title: "Export API"}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "api", Harness: "claude", Host: "sea", Name: "lead-api"}}})
	pid := memberIn(t, work, "-api")
	waitUntil(t, "the seed in the lead", func() bool { return strings.Contains(readIn(work, "in."+pid), "agent group "+gid) })
	leadAddr := readIn(work, "addr."+pid)
	devA, devB, devC := deviceOf(t, m.a), deviceOf(t, m.b), deviceOf(t, m.c)

	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(120*time.Second))
	defer cancel()
	// An agent cannot move a group.
	if err := (hookCaller{addr}).Call(ctx, "group.move", api.GroupMoveParams{Group: gid, Host: "bee"}, &api.GroupMoveResult{}); errKind(err) != api.ErrorKindDenied {
		t.Fatalf("a move from a pane: %v", err)
	}
	// b lacks the project.
	if out, err := groupMoveCLI(m.a, gid, "-host", "bee"); err == nil || !strings.Contains(out, "does not have project shop") {
		t.Fatalf("a move to a hab without the project: %v %s", err, out)
	}
	fileProject(t, m.b, repo)
	waitCatalogProject(t, m.a, "shop", 3)

	out, err := groupMoveCLI(m.a, gid, "-host", "bee")
	if err != nil || !strings.Contains(out, "group Add export moved home to bee") {
		t.Fatalf("move: %v %s", err, out)
	}
	if k := groupErr(t, m.a, gid); k != api.ErrorKindMoved {
		t.Fatalf("group.get on the old home: %q", k)
	}
	g := getGroup(t, m.b, gid)
	// The item's lead, and since B7 its reviewer, still planned.
	var leads []api.GroupMember
	for _, mb := range g.Members {
		if mb.Role == groups.RoleLead {
			leads = append(leads, mb)
		}
	}
	if g.Home != devB || g.Generation != 2 || g.State != "running" || g.Pending || g.Coordinator.Device != devA ||
		len(leads) != 1 || leads[0].Host != "sea" || leads[0].Device != devC {
		t.Fatalf("the new home's record: %+v", g)
	}
	waitUntil(t, "the coordinator's line", func() bool {
		return strings.Contains(readIn(work, "alice-in"), "group Add export moved home to bee")
	})
	// A's catalog publishes the tombstone, b's the group; a's lists it no more as its own.
	homeIn(t, m.a, gid, devB)
	var l api.GroupList
	call(t, m.a.control(), "group.list", api.GroupListParams{}, &l)
	if len(l.Groups) != 0 {
		t.Fatalf("the old home lists %+v", l.Groups)
	}

	// A report sent to the old home is forwarded to the new one, which tells the coordinator.
	var rep api.GroupReported
	call(t, m.a.control(), "group.report", api.GroupReportParams{Group: gid, Text: "sent to the old home"}, &rep)
	waitUntil(t, "the forwarded report at the coordinator", func() bool {
		return strings.Contains(readIn(work, "alice-in"), "sent to the old home")
	})
	if n := strings.Count(readIn(work, "alice-in"), "moved home to bee"); n != 1 {
		t.Fatalf("the coordinator got the move line %d times", n)
	}

	// After one catalog pull c finds the new home; with a stopped, the lead's report reaches b,
	// and waits there for the coordinator.
	homeIn(t, m.c, gid, devB)
	m.a.stopHab(false)
	rt, _ := findIn(actionTools, "group_report")
	if _, text, err := rt.run(ctx, hookCaller{leadAddr}, toolArgs{"group": gid, "text": "straight to the new home"}); err != nil {
		t.Fatalf("report: %v", err)
	} else if !strings.Contains(text, "wait") {
		t.Fatalf("report with the coordinator's hab stopped: %s", text)
	}
	g = getGroup(t, m.b, gid)
	if len(g.Inbox) == 0 || !strings.Contains(g.Inbox[len(g.Inbox)-1].Text, "straight to the new home") {
		t.Fatalf("b's inbox %+v", g.Inbox)
	}
	// The waiting reports reach the coordinator once its hab is back.
	m.a.serve()
	waitUntil(t, "the waiting report at the coordinator", func() bool {
		return strings.Contains(readIn(work, "alice-in"), "straight to the new home")
	})

	// An ended group does not move.
	call(t, m.b.control(), "group.close", api.GroupRef{Group: gid}, &api.GroupResult{})
	if out, err := groupMoveCLI(m.b, gid, "-host", "sea"); err == nil || !strings.Contains(out, "is ended") {
		t.Fatalf("a move of an ended group: %v %s", err, out)
	}
}

// moveConfig writes a hab config with [groups] move_answer at 10 s.
func moveConfig(t *testing.T) string {
	cfg := filepath.Join(shortDir(t), "hab.toml")
	writeFile(t, cfg, "[groups]\nmove_answer = \"10s\"\n")
	return cfg
}

// TestGroupMoveCrashes (groups.md 16.2 B9, 14): the new home stopped before its answer: the old
// home keeps the group after [groups] move_answer (10 s), says why, and the late pending copy
// goes. The old home killed after its tombstone, before any adopt_commit: the new home answers
// unavailable, then commits from the tombstone in its catalog copy and runs the group; the old
// home sends the commit again at its start.
func TestGroupMoveCrashes(t *testing.T) {
	needGit(t)
	fakes, work := groupFakes(t)
	m := newMeshPairEnv(t, fakes, "GFF_HAB_BIN="+bin, "GFF_HAB_WORKTREES="+filepath.Join(shortDir(t), "wt"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(work, "claude"), "GFF_HAB_CONFIG="+moveConfig(t), noRaceExitSleep)
	repo := gitRepo(t, m.a.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	fileProject(t, m.b, repo)
	addr := startSpawner(t, m.a, work, repo)
	waitCatalogProject(t, m.a, "shop", 2)
	gid := startGroup(t, m.a, addr, api.GroupPlan{Title: "Add export", Brief: "Export the board.", Project: "shop",
		Items:   []api.GroupPlanItem{{ID: "api", Title: "Export API"}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "api", Harness: "claude", Name: "lead-api"}}})
	memberIn(t, work, "-api")
	devB := deviceOf(t, m.b)

	// The new home does not answer: the old home keeps the group after 10 s.
	m.b.srv.cmd.Process.Signal(syscall.SIGSTOP)
	start := time.Now()
	out, err := groupMoveCLI(m.a, gid, "-host", "bee")
	m.b.srv.cmd.Process.Signal(syscall.SIGCONT)
	took := time.Since(start)
	if err == nil || !strings.Contains(out, "no answer in 10s") || took < 10*time.Second || took > scaleTimeout(40*time.Second) {
		t.Fatalf("a move with no answer: %v after %s: %s", err, took, out)
	}
	g := getGroup(t, m.a, gid)
	if g.Moving != nil || g.State != "running" || !strings.Contains(g.MoveFailed, "no answer") || g.Generation != 1 || g.Tried != 2 {
		t.Fatalf("after the failed move: %+v", g)
	}
	// The adopt b took late leaves a pending copy, which goes once b's copy of a's catalog
	// shows the group kept.
	waitUntil(t, "b drops its pending copy", func() bool { return groupErr(t, m.b, gid) == api.ErrorKindNotFound })

	// The old home writes its tombstone and dies before any adopt_commit.
	m.a.stopHab(false)
	m.a.env = append(m.a.env, "GFF_TEST_GROUP_SKIP_COMMIT=1")
	m.a.serve()
	waitPeer(t, m.a.sock, "bee", "a sees b again", func(p api.Peer) bool { return p.State == api.PeerStateOnline })
	if out, err := groupMoveCLI(m.a, gid, "-host", "bee"); err != nil || !strings.Contains(out, "moved home to bee") {
		t.Fatalf("move: %v %s", err, out)
	}
	if k := groupErr(t, m.b, gid); k != api.ErrorKindUnavailable {
		t.Fatalf("the pending copy answered %q", k)
	}
	// b holds a's tombstone in its catalog copy; a is killed and stays down.
	waitUntil(t, "b's copy of the tombstone", func() bool {
		var l api.CatalogList
		call(t, m.b.control(), "catalog.list", api.CatalogListParams{}, &l)
		for _, h := range l.Hosts {
			for _, e := range h.Catalog.Groups {
				if !h.Own && e.ID == gid && e.Moved != nil {
					return true
				}
			}
		}
		return false
	})
	m.a.stopHab(true)
	waitUntil(t, "b commits from the catalog", func() bool { return groupErr(t, m.b, gid) == "" })
	g = getGroup(t, m.b, gid)
	if g.Home != devB || g.Generation != 3 || g.Pending || g.State != "running" {
		t.Fatalf("committed from the catalog: %+v", g)
	}
	// The coordinator's hab is down: the move line waits in b's inbox.
	if len(g.Inbox) == 0 || !strings.Contains(g.Inbox[0].Text, "moved home to") {
		t.Fatalf("b's inbox %+v", g.Inbox)
	}
	// The old home, back without the seam, sends the commit again and its tombstone is acked;
	// the waiting line reaches the coordinator.
	m.a.env = m.a.env[:len(m.a.env)-1]
	m.a.serve()
	waitUntil(t, "the move line at the coordinator", func() bool {
		return strings.Contains(readIn(work, "alice-in"), "group Add export moved home to bee")
	})
	waitUntil(t, "the tombstone acknowledged", func() bool {
		b, _ := os.ReadFile(filepath.Join(m.a.state, "groups", gid+".json"))
		var t api.Group
		return json.Unmarshal(b, &t) == nil && t.Moved != nil && t.Moved.Acked
	})
}

// TestGroupMoveQueued (groups.md 16.2 B9): a move asked while the home is stopped is queued on
// the target, a second request replaces the first, and it runs once when the home is back.
func TestGroupMoveQueued(t *testing.T) {
	needGit(t)
	fakes, work := groupFakes(t)
	m := newMeshPairEnv(t, fakes, "GFF_HAB_BIN="+bin, "GFF_HAB_WORKTREES="+filepath.Join(shortDir(t), "wt"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(work, "claude"), noRaceExitSleep)
	repo := gitRepo(t, m.a.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	fileProject(t, m.b, repo)
	addr := startSpawner(t, m.a, work, repo)
	waitCatalogProject(t, m.a, "shop", 2)
	gid := startGroup(t, m.a, addr, api.GroupPlan{Title: "Add export", Brief: "Export the board.", Project: "shop",
		Items:   []api.GroupPlanItem{{ID: "api", Title: "Export API"}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "api", Harness: "claude", Name: "lead-api"}}})
	memberIn(t, work, "-api")
	devB := deviceOf(t, m.b)
	homeIn(t, m.b, gid, deviceOf(t, m.a))

	m.a.stopHab(false)
	waitPeer(t, m.b.sock, "ay", "b sees a offline", func(p api.Peer) bool { return p.State != api.PeerStateOnline })
	queued := filepath.Join(m.b.state, "groups", "queued-moves.json")
	read := func() []groups.Queued {
		b, _ := os.ReadFile(queued)
		var q []groups.Queued
		json.Unmarshal(b, &q)
		return q
	}
	for i := 0; i < 2; i++ {
		out, err := groupMoveCLI(m.b, gid, "-host", "test-host") // b's own host id
		if err != nil || !strings.Contains(out, "ay is offline; the group moves when it is back") {
			t.Fatalf("queue %d: %v %s", i, err, out)
		}
	}
	if q := read(); len(q) != 1 || q[0].Group != gid {
		t.Fatalf("queued %+v", q)
	}
	m.a.serve()
	waitUntil(t, "the queued move ran", func() bool { return groupErr(t, m.b, gid) == "" })
	if g := getGroup(t, m.b, gid); g.Home != devB || g.Generation != 2 {
		t.Fatalf("after the queued move: %+v", g)
	}
	waitUntil(t, "the queue empty", func() bool { return len(read()) == 0 })
	if k := groupErr(t, m.a, gid); k != api.ErrorKindMoved {
		t.Fatalf("the old home answers %q", k)
	}
}
