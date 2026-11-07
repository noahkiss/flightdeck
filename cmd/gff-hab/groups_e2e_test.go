package main

// End-to-end tests of agent groups (groups.md 3, 5, 16.2 B3) with the real gff-hab and holder,
// two habs over the ssh stand-in, real git, and a fake Claude Code member that draws its folder
// trust dialog, then reports idle and keeps every byte pasted into it.

import (
	"context"
	"encoding/json"
	"os"
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
	if got := pastedAll(work, "trusted"); strings.Count(got, "\n") != 2 {
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

func (s *trustShows) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.n {
		n += v
	}
	return n
}
