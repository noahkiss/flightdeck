package main

// End-to-end tests of agent groups (groups.md 3, 5, 16.2 B3) with the real gff-hab and holder,
// two habs over the ssh stand-in, real git, and a fake Claude Code member that draws its folder
// trust dialog, then reports idle and keeps every byte pasted into it.

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
)

// fakeMemberBody follows the trust prelude: it saves its GFF_HAB and folder by its pid, reports
// idle and keeps what is pasted into it.
const fakeMemberBody = `printf '%s' "$GFF_HAB" > "$D/addr.$$"
pwd -P > "$D/cwd.$$"
printf '\033]7501;state=idle:app=claude\033\\'
exec cat > "$D/in.$$"
`

// groupFakes writes the fake member as claude, with Claude Code's trust dialog first.
func groupFakes(t *testing.T) (fakes, work string) {
	t.Helper()
	fakes, work = shortDir(t), shortDir(t)
	writeFile(t, filepath.Join(fakes, "claude"), strings.ReplaceAll(fakeTrustClaudePrelude, "@DIR@", work)+fakeMemberBody)
	if err := os.Chmod(filepath.Join(fakes, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakes, work
}

// memberIn waits for the fake member whose folder ends with suffix and returns its pid.
func memberIn(t *testing.T, work, suffix string) string {
	t.Helper()
	var pid string
	waitUntil(t, "a member in "+suffix, func() bool {
		ents, _ := os.ReadDir(work)
		for _, e := range ents {
			if p, ok := strings.CutPrefix(e.Name(), "cwd."); ok {
				b, _ := os.ReadFile(filepath.Join(work, e.Name()))
				if strings.HasSuffix(strings.TrimSpace(string(b)), suffix) {
					pid = p
					return true
				}
			}
		}
		return false
	})
	return pid
}

func readIn(work, name string) string {
	b, _ := os.ReadFile(filepath.Join(work, name))
	return string(b)
}

// fileProject files repo on e with a plain session in it, so e's catalog lists the project.
func fileProject(t *testing.T, e *habEnv, repo string) {
	t.Helper()
	createSession(t, e.sock, api.SessionCreateParams{Argv: []string{"/bin/sh", "-c", "sleep 600"}, ProjectDir: repo, Cols: 80, Rows: 24})
}

// waitCatalogProject waits until e's catalog lists the project on n hosts.
func waitCatalogProject(t *testing.T, e *habEnv, name string, n int) {
	t.Helper()
	c := e.control()
	deadline := time.Now().Add(scaleTimeout(60 * time.Second))
	for {
		var l api.CatalogList
		call(t, c, "catalog.list", api.CatalogListParams{}, &l)
		for _, p := range l.Projects {
			if p.Name == name && len(p.Locations) >= n {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the catalog never listed %s on %d hosts: %+v", name, n, l.Projects)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func openConfirmations(t *testing.T, e *habEnv) []api.Confirmation {
	t.Helper()
	var l api.ConfirmList
	call(t, e.control(), "confirm.list", api.ConfirmListParams{}, &l)
	var out []api.Confirmation
	for _, c := range l.Confirmations {
		if c.State == "open" {
			out = append(out, c)
		}
	}
	return out
}

func getGroup(t *testing.T, e *habEnv, id string) api.Group {
	t.Helper()
	var r api.GroupResult
	call(t, e.control(), "group.get", api.GroupRef{Group: id}, &r)
	return r.Group
}

// TestGroupPlanOnPeer (groups.md 16.1 row 1, 16.2 B3): a coordinator's group_create with a lead
// on this hab, a lead on the other hab and a reviewer raises exactly one confirmation; nothing
// starts before it. After the yes, each hab makes its lead's worktree on the group's branch,
// answers the lead's trust dialog itself (no blocked:trust report), and the seed goes in. The
// home hab files the Logbook plan and one task per item, and claims each task for its lead. A
// report from the peer's lead reaches the coordinator through the home hab. A worker the peer's
// lead starts inside the envelope asks nothing, also with the home hab stopped, and is a member.
// The record survives a gff-hab restart.
func TestGroupPlanOnPeer(t *testing.T) {
	needGit(t)
	fakes, work := groupFakes(t)
	m := newMeshPairEnv(t, fakes, "GFF_HAB_BIN="+bin, "GFF_HAB_WORKTREES="+filepath.Join(shortDir(t), "wt"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(work, "claude"), noRaceExitSleep)
	repo := gitRepo(t, m.a.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	fileProject(t, m.b, repo)
	shows := watchTrustShows(t, m.b)
	addr := startSpawner(t, m.a, work, repo)
	waitCatalogProject(t, m.a, "shop", 2)

	plan := api.GroupPlan{Title: "Add export", Brief: "Export the board as CSV.", Project: "shop",
		Items: []api.GroupPlanItem{{ID: "api", Title: "Export API", Brief: "The endpoint."}, {ID: "ui", Title: "Export button"}},
		Members: []api.GroupPlanMember{
			{Role: "lead", Item: "api", Harness: "claude", Name: "lead-api"},
			{Role: "lead", Item: "ui", Harness: "claude", Host: "bee", Name: "lead-ui"},
			{Role: "reviewer", Item: "api", Harness: "claude", Host: "bee", Name: "rev-api"}}}
	pj, _ := json.Marshal(plan)
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(120*time.Second))
	defer cancel()
	tl, _ := findIn(actionTools, "group_create")
	type out struct {
		res  any
		text string
		err  error
	}
	done := make(chan out, 1)
	go func() {
		res, text, err := tl.run(ctx, hookCaller{addr}, toolArgs{"plan": string(pj)})
		done <- out{res, text, err}
	}()
	var open []api.Confirmation
	waitUntil(t, "the plan's confirmation", func() bool { open = openConfirmations(t, m.a); return len(open) > 0 })
	time.Sleep(500 * time.Millisecond)
	if open = openConfirmations(t, m.a); len(open) != 1 || open[0].Kind != "group" || open[0].Group == nil || len(open[0].Group.Plan.Members) != 3 {
		t.Fatalf("want one group confirmation for three members: %+v", open)
	}
	if !strings.Contains(open[0].Group.Trust, "group/"+open[0].Group.Group+"/") {
		t.Fatalf("trust rule %q", open[0].Group.Trust)
	}
	gid := open[0].Group.Group
	if g := getGroup(t, m.a, gid); g.State != "confirming" || g.Members[0].State != "planned" {
		t.Fatalf("before the answer: %+v", g)
	}
	if n := len(pastedAll(work, "cwd")); n != 0 {
		t.Fatal("a member started before the answer")
	}
	call(t, m.a.control(), "confirm.answer", api.ConfirmAnswerParams{ID: open[0].ID, Choice: "confirm"}, &api.ConfirmationResult{})
	o := <-done
	if o.err != nil {
		t.Fatal(o.err)
	}

	apiPid, uiPid := memberIn(t, work, "-api"), memberIn(t, work, "-ui")
	for _, pid := range []string{apiPid, uiPid} {
		waitUntil(t, "the seed in member "+pid, func() bool { return strings.Contains(readIn(work, "in."+pid), "agent group "+gid) })
	}
	if got := pastedAll(work, "trusted"); strings.Count(got, "/group-"+gid+"-") != 2 {
		t.Fatalf("trust answers %q: want both leads' dialogs answered by their habs", got)
	}
	g := getGroup(t, m.a, gid)
	if g.State != "running" || g.Confirmation.ID != open[0].ID || g.Items[0].State != "working" || g.Items[1].State != "working" {
		t.Fatalf("after the answer: %+v", g)
	}
	ui := g.Members[1]
	if ui.Host != "bee" || ui.Session == "" || ui.State != "running" || g.Members[2].State != "planned" {
		t.Fatalf("members %+v", g.Members)
	}
	if n := shows.total(); n != 0 {
		t.Fatalf("b showed the trust prompt %d times", n)
	}
	// Logbook: the plan, its tasks, and the hab's claim for each lead.
	var task api.LogbookRecordResult
	waitUntil(t, "the ui task claimed", func() bool {
		call(t, m.a.control(), "logbook.get", api.LogbookGetParams{ID: g.Items[1].Task}, &task)
		return task.Record.Status == "doing" && task.Record.Assignee == ui.Session
	})

	// A report from the peer's lead reaches the coordinator through the home hab.
	uiAddr := readIn(work, "addr."+uiPid)
	rt, _ := findIn(actionTools, "group_report")
	if _, text, err := rt.run(ctx, hookCaller{uiAddr}, toolArgs{"group": gid, "text": "the button is in"}); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(text, "Sent") {
		t.Fatalf("report: %s", text)
	}
	waitUntil(t, "the report at the coordinator", func() bool {
		return strings.Contains(readIn(work, "alice-in"), "from lead-ui: the button is in")
	})

	// With the home hab stopped, the peer's lead starts a worker inside the envelope: no
	// question, and the worker is a member of the group.
	m.a.stopHab(false)
	st, _ := findIn(actionTools, "spawn")
	res, text, err := st.run(ctx, hookCaller{uiAddr}, toolArgs{"harness": "claude", "seed": "help with the button", "group": gid,
		"folder": strings.TrimSpace(readIn(work, "cwd."+uiPid)), "name": "worker-ui", "mode": "detached"})
	if err != nil {
		t.Fatal(err)
	}
	if cf := res.(spawnResult).Confirmation; cf.State != "done" || !cf.Record {
		t.Fatalf("the worker asked: %+v\n%s", cf, text)
	}
	var ss api.StructureResult
	call(t, m.b.control(), "structure.get", api.StructureGetParams{}, &ss)
	found := false
	for _, s := range ss.Sessions {
		if s.Name == "worker-ui" && s.Group != nil && s.Group.ID == gid && s.Group.Role == "worker" && s.Group.Item == "ui" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the worker is no member: %+v", ss.Sessions)
	}
	if n := len(openConfirmations(t, m.b)); n != 0 {
		t.Fatalf("b opened %d confirmations", n)
	}

	// The record survives the restart, and the home hab learns the worker from b's copy.
	m.a.serve()
	waitUntil(t, "the worker in the record", func() bool {
		var r api.GroupResult
		if err := m.a.control().Call(ctx, "group.get", api.GroupRef{Group: gid}, &r); err != nil {
			return false
		}
		for _, mm := range r.Group.Members {
			if mm.Name == "worker-ui" && mm.Role == "worker" {
				return r.Group.State == "running"
			}
		}
		return false
	})
}

// fakeCoordinator is fakeSpawner that also saves its pid, so the test can end it. Its folder
// comes as $1, not from the environment: session.resume starts the same argv again, but not
// the environment of the first create.
const fakeCoordinator = `
DIR=$1
printf '%s' "$GFF_HAB" > "$DIR/addr.tmp" && mv "$DIR/addr.tmp" "$DIR/addr"
echo $$ > "$DIR/alice-pid"
stty raw -echo
printf '\033]7501;state=idle:app=claude\033\\'
exec cat > "$DIR/alice-in"
`

// TestGroupLifecycle (groups.md 16.2 B3) on one hab: with confirm.group_plan off a plan starts
// at once and the cover names no confirmation; a user pause holds a channel message into a
// member until the resume; with the setting on an amend asks once for its change; reports to an
// exited coordinator wait and arrive at its resume; a restart during confirming drops the group
// and tells the coordinator, and the running group survives it.
func TestGroupLifecycle(t *testing.T) {
	needGit(t)
	fakes, work := groupFakes(t)
	e := newHolderEnv(t, 0)
	e.env = []string{"PATH=" + fakes + ":/usr/bin:/bin", "GFF_HAB_BIN=" + bin, "GFF_HAB_WORKTREES=" + filepath.Join(shortDir(t), "wt"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(work, "claude"), noRaceExitSleep}
	e.serve()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(150*time.Second))
	defer cancel()
	set := func(id, v string) {
		t.Helper()
		var cur api.SettingsResult
		call(t, e.control(), "settings.get", api.SettingsGetParams{IDs: []string{id}}, &cur)
		if err := e.control().Call(ctx, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{
			{ID: id, Value: json.RawMessage(v), Revision: cur.Settings[0].Revision}}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	repo := gitRepo(t, e.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	alice := createSession(t, e.sock, api.SessionCreateParams{Name: "alice", Argv: []string{"/bin/sh", "-c", fakeCoordinator, "sh", work}, ProjectDir: repo,
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}})
	waitUntil(t, "alice's GFF_HAB", func() bool { return readIn(work, "addr") != "" })
	addr := readIn(work, "addr")
	waitCatalogProject(t, e, "shop", 1)

	// Off: the plan starts at once, with no question.
	set("confirm.group_plan", "false")
	plan := api.GroupPlan{Title: "Fix cart", Project: "shop", Items: []api.GroupPlanItem{{ID: "a", Title: "Round once"}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "a", Harness: "claude", Name: "lead-a"}}}
	pj, _ := json.Marshal(plan)
	tl, _ := findIn(actionTools, "group_create")
	res, text, err := tl.run(ctx, hookCaller{addr}, toolArgs{"plan": string(pj)})
	if err != nil {
		t.Fatal(err)
	}
	gc := res.(api.GroupCreated)
	if gc.Confirmation == nil || !gc.Confirmation.Record || !strings.Contains(text, "no confirmation asked") {
		t.Fatalf("setting off: %+v\n%s", gc.Confirmation, text)
	}
	gid := gc.Group.ID
	aPid := memberIn(t, work, "-a")
	waitUntil(t, "the lead's seed", func() bool { return strings.Contains(readIn(work, "in."+aPid), "agent group "+gid) })
	if g := getGroup(t, e, gid); g.Confirmation.ID != "none" || g.Envelope == nil || g.Envelope.Confirmation != "none" {
		t.Fatalf("record %+v", g)
	}

	// A user pause holds a message into the member until the resume.
	call(t, e.control(), "group.pause", api.GroupRef{Group: gid}, &api.GroupResult{})
	waitUntil(t, "the pause in the catalog", func() bool {
		var l api.CatalogList
		call(t, e.control(), "catalog.list", api.CatalogListParams{}, &l)
		g, ok := groupOf(l, gid)
		return ok && g.Pause != nil
	})
	if err := (hookCaller{addr}).Call(ctx, "channel.send", api.ChannelSendParams{To: "lead-a", Text: "status?"}, &api.ChannelSent{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if strings.Contains(readIn(work, "in."+aPid), "status?") {
		t.Fatal("a message went into a member of a paused group")
	}
	call(t, e.control(), "group.resume", api.GroupRef{Group: gid}, &api.GroupResult{})
	waitUntil(t, "the held message after the resume", func() bool { return strings.Contains(readIn(work, "in."+aPid), "status?") })

	// On: an amend asks once for its change.
	set("confirm.group_plan", "true")
	change, _ := json.Marshal(api.GroupPlan{Items: []api.GroupPlanItem{{ID: "b", Title: "Tests"}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "b", Harness: "claude", Name: "lead-b"}}})
	at, _ := findIn(actionTools, "group_amend")
	done := make(chan error, 1)
	go func() {
		_, _, err := at.run(ctx, hookCaller{addr}, toolArgs{"group": gid, "change": string(change)})
		done <- err
	}()
	var open []api.Confirmation
	waitUntil(t, "the amend's confirmation", func() bool { open = openConfirmations(t, e); return len(open) == 1 })
	if !open[0].Group.Amend || len(open[0].Group.Plan.Items) != 1 {
		t.Fatalf("amend card %+v", open[0].Group)
	}
	call(t, e.control(), "confirm.answer", api.ConfirmAnswerParams{ID: open[0].ID, Choice: "confirm"}, &api.ConfirmationResult{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	bPid := memberIn(t, work, "-b")
	waitUntil(t, "the new lead's seed", func() bool { return strings.Contains(readIn(work, "in."+bPid), "item b") })

	// The coordinator exits: a report waits, and arrives at its resume.
	if err := exec.Command("kill", strings.TrimSpace(readIn(work, "alice-pid"))).Run(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "alice exited", func() bool {
		var ss api.StructureResult
		call(t, e.control(), "structure.get", api.StructureGetParams{}, &ss)
		for _, s := range ss.Sessions {
			if s.ID == alice.Session {
				return s.State == "exited"
			}
		}
		return false
	})
	rt, _ := findIn(actionTools, "group_report")
	if _, text, err := rt.run(ctx, hookCaller{readIn(work, "addr."+aPid)}, toolArgs{"group": gid, "text": "rounded once"}); err != nil || !strings.Contains(text, "waits") {
		t.Fatalf("report to an exited coordinator: %v %s", err, text)
	}
	call(t, e.control(), "session.resume", api.SessionRef{Session: alice.Session}, &api.SessionCreated{})
	waitUntil(t, "the inbox at the resume", func() bool {
		in := readIn(work, "alice-in")
		return strings.Contains(in, "reports came while you were away") && strings.Contains(in, "lead-a: rounded once")
	})
	if g := getGroup(t, e, gid); len(g.Inbox) != 0 {
		t.Fatalf("inbox after delivery: %+v", g.Inbox)
	}

	// A restart while a plan waits for its answer drops the group and tells the coordinator.
	var waiting api.GroupCreated
	p := api.GroupPlan{Title: "Later", Project: "shop", Items: []api.GroupPlanItem{{ID: "c", Title: "C"}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "c", Harness: "claude", Name: "lead-c"}}}
	call(t, e.control(), "group.create", api.GroupCreateParams{Plan: p, Coordinator: "alice"}, &waiting)
	if waiting.Group.State != "confirming" {
		t.Fatalf("waiting %+v", waiting.Group)
	}
	e.restart(false)
	if g := getGroup(t, e, waiting.Group.ID); g.State != "dropped" {
		t.Fatalf("after the restart: %s", g.State)
	}
	waitUntil(t, "the drop message", func() bool {
		return strings.Contains(readIn(work, "alice-in"), "the hab restarted before you answered")
	})
	if g := getGroup(t, e, gid); g.State != "running" || len(g.Members) != 2 {
		t.Fatalf("the running group after the restart: %+v", g)
	}
}

func (s *trustShows) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.n {
		n += v
	}
	return n
}

// TestGroupCardEdits (groups.md 5.8): the title and the briefs the user edits on the plan's
// card reach the Logbook plan record and the item's task; an item not edited keeps its task.
func TestGroupCardEdits(t *testing.T) {
	needGit(t)
	fakes, work := groupFakes(t)
	e := newHolderEnv(t, 0)
	e.env = []string{"PATH=" + fakes + ":/usr/bin:/bin", "GFF_HAB_BIN=" + bin, "GFF_HAB_WORKTREES=" + filepath.Join(shortDir(t), "wt"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(work, "claude"), noRaceExitSleep}
	e.serve()
	repo := gitRepo(t, e.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	createSession(t, e.sock, api.SessionCreateParams{Name: "alice", Argv: []string{"/bin/sh", "-c", "sleep 600"}, ProjectDir: repo, Cols: 80, Rows: 24})
	waitCatalogProject(t, e, "shop", 1)

	plan := api.GroupPlan{Title: "Fix cart", Brief: "Round once.", Project: "shop",
		Items: []api.GroupPlanItem{{ID: "a", Title: "Round", Brief: "In the total."}, {ID: "b", Title: "Tests"}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "a", Harness: "claude", Name: "lead-a"},
			{Role: "lead", Item: "b", Harness: "claude", Name: "lead-b"}}}
	var gc api.GroupCreated
	call(t, e.control(), "group.create", api.GroupCreateParams{Plan: plan, Coordinator: "alice"}, &gc)
	if gc.Confirmation == nil || gc.Confirmation.State != "open" {
		t.Fatalf("want an open card: %+v", gc.Confirmation)
	}
	edited := gc.Confirmation.Group.Plan
	edited.Title, edited.Brief = "Fix the cart total", "Round once, at the end."
	edited.Items[0].Title, edited.Items[0].Brief = "Round the total", "Only the total."
	call(t, e.control(), "confirm.answer", api.ConfirmAnswerParams{ID: gc.Confirmation.ID, Choice: "confirm",
		Edits: &api.ConfirmEdits{Plan: &edited}}, &api.ConfirmationResult{})
	var g api.Group
	waitUntil(t, "the group running", func() bool { g = getGroup(t, e, gc.Group.ID); return g.State == "running" })
	if g.Title != "Fix the cart total" {
		t.Fatalf("record title %q", g.Title)
	}
	get := func(id string) api.LogbookRecord {
		var r api.LogbookRecordResult
		call(t, e.control(), "logbook.get", api.LogbookGetParams{ID: id}, &r)
		return r.Record
	}
	if r := get(g.PlanRecord); r.Title != "Fix the cart total" || r.Body != "Round once, at the end." {
		t.Fatalf("plan record %q %q", r.Title, r.Body)
	}
	if r := get(g.Items[0].Task); r.Title != "Round the total" || r.Body != "Only the total." {
		t.Fatalf("item a's task %q %q", r.Title, r.Body)
	}
	if r := get(g.Items[1].Task); r.Title != "Tests" {
		t.Fatalf("item b's task %q", r.Title)
	}
}
