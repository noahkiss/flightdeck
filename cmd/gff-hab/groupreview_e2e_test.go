package main

// End-to-end test of review in agent groups (groups.md 6, 16.2 B7) with the real gff-hab and
// holder, real git, a fake Claude Code lead and a fake Codex reviewer.

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

// gitHead returns the full commit id rev names in dir.
func gitHead(t *testing.T, dir string, rev ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "rev-parse"}, rev...)...).Output()
	if err != nil {
		t.Fatalf("git rev-parse %v: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}

// commitIn writes a file in dir and commits it.
func commitIn(t *testing.T, dir, file, text string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, file), text)
	gitRun(t, dir, "add", file)
	gitRun(t, dir, "commit", "-q", "-m", "change "+file)
	return gitHead(t, dir, "HEAD")
}

// TestGroupReview (groups.md 6, 16.2 B7) on one hab that declares codex: a Claude Code lead gets
// a Codex reviewer at plan time. The lead's lane_submit starts the reviewer on the branch head
// in its own worktree on the review branch. A verdict with more than [groups] max_findings (2)
// findings is refused. fix_needed moves the item to fix, the lead gets every finding, and the
// coordinator one line without them. The lead's second submit reaches the same reviewer. A pass
// with fixes on the review branch lands its head, and the lead is told to fast-forward. A pass
// whose review branch head does not descend from the submitted head ignores the fixes, and the
// record says so. A submit from the reviewer, and a verdict from the lead or a control
// connection, are refused.
func TestGroupReview(t *testing.T) {
	needGit(t)
	fakes, work := groupFakes(t)
	codex := "#!/bin/sh\nD=" + work + "\nstty raw -echo\n" + strings.ReplaceAll(fakeMemberBody, "app=claude", "app=codex")
	writeFile(t, filepath.Join(fakes, "codex"), codex)
	if err := os.Chmod(filepath.Join(fakes, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := newHolderEnv(t, 0)
	cfg := filepath.Join(e.work, "hab.toml")
	writeFile(t, cfg, "[catalog]\ncapabilities = [\"codex\"]\n\n[groups]\nmax_findings = 2\n")
	e.env = []string{"PATH=" + fakes + ":/usr/bin:/bin", "GFF_HAB_BIN=" + bin, "GFF_HAB_WORKTREES=" + filepath.Join(shortDir(t), "wt"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(work, "claude"), "GFF_HAB_CONFIG=" + cfg, noRaceExitSleep}
	e.serve()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(150*time.Second))
	defer cancel()
	var cur api.SettingsResult
	call(t, e.control(), "settings.get", api.SettingsGetParams{IDs: []string{"confirm.group_plan"}}, &cur)
	call(t, e.control(), "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{
		{ID: "confirm.group_plan", Value: json.RawMessage("false"), Revision: cur.Settings[0].Revision}}}, &api.SettingsResult{})

	repo := gitRepo(t, e.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	first := gitHead(t, repo, "HEAD")
	createSession(t, e.sock, api.SessionCreateParams{Name: "alice", Argv: []string{"/bin/sh", "-c", fakeCoordinator, "sh", work}, ProjectDir: repo,
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}})
	waitUntil(t, "alice's GFF_HAB", func() bool { return readIn(work, "addr") != "" })
	alice := hookCaller{readIn(work, "addr")}
	waitCatalogProject(t, e, "shop", 1)

	plan := api.GroupPlan{Title: "Fix cart", Project: "shop", Items: []api.GroupPlanItem{{ID: "a", Title: "Round once", Brief: "Round the total once."}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "a", Harness: "claude", Name: "lead-a"}}}
	pj, _ := json.Marshal(plan)
	tl, _ := findIn(actionTools, "group_create")
	res, _, err := tl.run(ctx, alice, toolArgs{"plan": string(pj)})
	if err != nil {
		t.Fatal(err)
	}
	gid := res.(api.GroupCreated).Group.ID
	g := getGroup(t, e, gid)
	it := g.Items[0]
	rv := g.Members[1]
	if it.Reviewer == "" || it.SameHarness || rv.Role != "reviewer" || rv.Harness != "codex" || rv.State != "planned" {
		t.Fatalf("the reviewer at plan time: item %+v, member %+v", it, rv)
	}

	// The lead commits and submits; the reviewer starts on that head, on its own branch.
	lPid := memberIn(t, work, "-a")
	lead := hookCaller{readIn(work, "addr."+lPid)}
	leadDir := strings.TrimSpace(readIn(work, "cwd."+lPid))
	c1 := commitIn(t, leadDir, "cart.go", "package cart\n")
	st, _ := findIn(actionTools, "lane_submit")
	if _, text, err := st.run(ctx, lead, toolArgs{"group": gid, "item": "a"}); err != nil || !strings.Contains(text, c1[:7]) {
		t.Fatalf("submit: %v %s", err, text)
	}
	if g := getGroup(t, e, gid); g.Items[0].State != "review" || g.Items[0].Submitted != c1 {
		t.Fatalf("after the submit %+v", g.Items[0])
	}
	rPid := memberIn(t, work, "-a-review")
	reviewer := hookCaller{readIn(work, "addr."+rPid)}
	revDir := strings.TrimSpace(readIn(work, "cwd."+rPid))
	waitUntil(t, "the reviewer's seed", func() bool {
		in := readIn(work, "in."+rPid)
		return strings.Contains(in, "reviewer of item a") && strings.Contains(in, c1) && strings.Contains(in, "review_verdict")
	})
	if h := gitHead(t, revDir, "HEAD"); h != c1 {
		t.Fatalf("the review worktree is at %s, want %s", h, c1)
	}
	if b := gitHead(t, revDir, "--abbrev-ref", "HEAD"); b != "group/"+gid+"/a-review" {
		t.Fatalf("the review worktree is on %s", b)
	}

	vt, _ := findIn(actionTools, "review_verdict")
	three := `[{"severity":"high","title":"One","place":"cart.go:1","text":"First paragraph."},{"severity":"medium","title":"Two","text":"Second."},{"severity":"medium","title":"Three","text":"Third."}]`
	if _, _, err := vt.run(ctx, reviewer, toolArgs{"group": gid, "item": "a", "verdict": "fix_needed", "findings": three}); err == nil || !strings.Contains(err.Error(), "max_findings") {
		t.Fatalf("three findings with max_findings 2: %v", err)
	}
	// Only the lead submits; only the reviewer gives the verdict, and never over the control socket.
	if _, _, err := st.run(ctx, reviewer, toolArgs{"group": gid, "item": "a"}); err == nil {
		t.Fatal("the reviewer submitted")
	}
	if _, _, err := vt.run(ctx, lead, toolArgs{"group": gid, "item": "a", "verdict": "pass"}); err == nil {
		t.Fatal("the lead gave the verdict")
	}
	if err := e.control().Call(ctx, "review.verdict", api.ReviewVerdictParams{Group: gid, Item: "a", Verdict: "pass", From: rv.Name}, &api.ReviewVerdictTaken{}); err == nil {
		t.Fatal("a verdict over the control socket")
	}

	// fix_needed: the item goes to fix, the lead gets the findings, the coordinator one line.
	two := `[{"severity":"high","title":"Rounds twice","place":"cart.go:1","text":"The total is rounded in two places."},{"severity":"medium","title":"No test","text":"Nothing covers it."}]`
	if _, _, err := vt.run(ctx, reviewer, toolArgs{"group": gid, "item": "a", "verdict": "fix_needed", "findings": two}); err != nil {
		t.Fatal(err)
	}
	if g := getGroup(t, e, gid); g.Items[0].State != "fix" || g.Items[0].Verdict == nil || len(g.Items[0].Verdict.Findings) != 2 {
		t.Fatalf("after fix_needed %+v", g.Items[0])
	}
	waitUntil(t, "the findings at the lead", func() bool {
		in := readIn(work, "in."+lPid)
		return strings.Contains(in, "The total is rounded in two places.") && strings.Contains(in, "Nothing covers it.")
	})
	waitUntil(t, "the coordinator's line", func() bool { return strings.Contains(readIn(work, "alice-in"), "item a: fix needed, 2 findings") })
	if in := readIn(work, "alice-in"); strings.Contains(in, "rounded in two places") {
		t.Fatal("the coordinator got the review text")
	}

	// The lead fixes and submits again: the same reviewer hears of the new head.
	c2 := commitIn(t, leadDir, "cart_test.go", "package cart\n")
	if _, _, err := st.run(ctx, lead, toolArgs{"group": gid, "item": "a"}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the new head at the reviewer", func() bool {
		return strings.Contains(readIn(work, "in."+rPid), "submitted again") && strings.Contains(readIn(work, "in."+rPid), c2)
	})
	if g := getGroup(t, e, gid); len(g.Members) != 2 {
		t.Fatalf("a second reviewer started: %+v", g.Members)
	}

	// A pass while the review branch is still on the earlier head is refused: c2 would land
	// unreviewed.
	if _, _, err := vt.run(ctx, reviewer, toolArgs{"group": gid, "item": "a", "verdict": "pass"}); err == nil || !strings.Contains(err.Error(), "does not descend") {
		t.Fatalf("a pass on the earlier head: %v", err)
	}
	if it := getGroup(t, e, gid).Items[0]; it.State != "review" {
		t.Fatalf("after the stale pass %+v", it)
	}

	// A pass with fixes on the review branch lands its head; the lead is told to fast-forward.
	gitRun(t, revDir, "reset", "-q", "--hard", c2)
	f1 := commitIn(t, revDir, "fix.go", "package cart\n")
	if _, text, err := vt.run(ctx, reviewer, toolArgs{"group": gid, "item": "a", "verdict": "pass", "fixed": f1}); err != nil || !strings.Contains(text, "ready") {
		t.Fatalf("pass with fixes: %v %s", err, text)
	}
	g = getGroup(t, e, gid)
	if it := g.Items[0]; it.State != "ready" || it.Land != f1 || it.Verdict.FixedHead != f1 || len(g.Lane.Queue) != 1 {
		t.Fatalf("after a pass with fixes %+v, lane %+v", it, g.Lane)
	}
	waitUntil(t, "the fast-forward at the lead", func() bool { return strings.Contains(readIn(work, "in."+lPid), "git merge --ff-only "+f1) })
	waitUntil(t, "the pass line", func() bool {
		return strings.Contains(readIn(work, "alice-in"), "item a: pass, 0 findings, with the reviewer's fixes")
	})

	// A review branch head that does not descend from the submitted head: the fixes are
	// refused and nothing lands; a fixed.head other than the review branch's is refused too.
	gitRun(t, leadDir, "merge", "-q", "--ff-only", f1)
	c3 := commitIn(t, leadDir, "more.go", "package cart\n")
	if _, _, err := st.run(ctx, lead, toolArgs{"group": gid, "item": "a"}); err != nil {
		t.Fatal(err)
	}
	gitRun(t, revDir, "reset", "-q", "--hard", first)
	f2 := commitIn(t, revDir, "other.go", "package cart\n")
	if _, _, err := vt.run(ctx, reviewer, toolArgs{"group": gid, "item": "a", "verdict": "pass", "fixed": f2}); err == nil || !strings.Contains(err.Error(), "does not descend") {
		t.Fatalf("pass with fixes that do not descend: %v", err)
	}
	if it := getGroup(t, e, gid).Items[0]; it.State != "review" || it.Land != "" {
		t.Fatalf("fixes that do not descend %+v", it)
	}
	if err := reviewer.Call(ctx, "review.verdict", api.ReviewVerdictParams{Group: gid, Item: "a", Verdict: "pass",
		Fixed: &api.ReviewFixed{Commits: []string{c3}, Head: c3}}, &api.ReviewVerdictTaken{}); err == nil || !strings.Contains(err.Error(), "not the head") {
		t.Fatalf("a fixed.head off the review branch: %v", err)
	}
	// Reset to the submitted head, the reviewer passes it with no fixes: c3 lands.
	gitRun(t, revDir, "reset", "-q", "--hard", c3)
	if _, _, err := vt.run(ctx, reviewer, toolArgs{"group": gid, "item": "a", "verdict": "pass"}); err != nil {
		t.Fatal(err)
	}
	if it := getGroup(t, e, gid).Items[0]; it.State != "ready" || it.Land != c3 {
		t.Fatalf("a pass on the submitted head %+v", it)
	}
}

// TestGroupPeerCalls (groups.md 6.5): lane.submit, review.verdict and group.message are taken
// over a link only from a peer the peer list pins, never one claiming this hab's own device
// id; group.message only from the hab whose own catalog lists itself as the group's home.
func TestGroupPeerCalls(t *testing.T) {
	cl := api.CatalogList{Hosts: []api.CatalogEntry{
		{Label: "bee", Catalog: api.Catalog{DeviceID: "dev-bee", Groups: []api.CatalogGroup{{ID: "g1", Home: "dev-bee"}}}},
		{Label: "wasp", Catalog: api.Catalog{DeviceID: "dev-wasp", Groups: []api.CatalogGroup{{ID: "g1", Home: "dev-wasp"}, {ID: "g2", Home: "dev-bee"}}}},
	}}
	s := &groupsService{cfg: groupsConfig{self: "dev-me", catalog: func() api.CatalogList { return cl },
		listedPeer: func(d string) bool { return d == "dev-bee" || d == "dev-wasp" || d == "dev-me" }}, groups: map[string]*api.Group{}}
	peer := func(d string) context.Context {
		return control.WithCallerPeer(context.Background(), api.PeerHello{DeviceID: d, Host: d})
	}
	for _, ctx := range []context.Context{context.Background(), peer("dev-me"), peer("dev-stranger")} {
		if _, err := s.Submit(ctx, api.LaneSubmitParams{Group: "g9", Item: "a", Head: "c1"}); errKind(err) != api.ErrorKindDenied {
			t.Fatalf("submit: %v", err)
		}
		if _, err := s.Verdict(ctx, api.ReviewVerdictParams{Group: "g9", Item: "a", Verdict: "pass"}); errKind(err) != api.ErrorKindDenied {
			t.Fatalf("verdict: %v", err)
		}
		if err := s.Message(ctx, api.GroupMessageParams{Group: "g1", Member: "m", Text: "hi"}); errKind(err) != api.ErrorKindDenied {
			t.Fatalf("message: %v", err)
		}
	}
	// wasp lists g2 with bee as its home: only bee's own copy makes bee the home.
	if !homeOf(cl, "g1", "dev-bee") || homeOf(cl, "g2", "dev-bee") || !homeOf(cl, "g1", "dev-wasp") {
		t.Fatal("homeOf")
	}
	if err := s.Message(peer("dev-wasp"), api.GroupMessageParams{Group: "g2", Member: "m", Text: "hi"}); errKind(err) != api.ErrorKindDenied {
		t.Fatalf("a message from a hab that is not the home: %v", err)
	}
}
