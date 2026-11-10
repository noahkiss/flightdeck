package main

// Capcom as a group's coordinator (groups.md 12.1, 16.2 B8) with the real gff-hab and holder,
// the stub engine, real git, and the fake Claude Code member of groups_e2e_test.go.

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

// threadItem returns the first thread item that matches, or false.
func threadItem(items []api.CapcomItem, f func(api.CapcomItem) bool) (api.CapcomItem, bool) {
	for _, it := range items {
		if f(it) {
			return it, true
		}
	}
	return api.CapcomItem{}, false
}

// TestCapcomGroup: Capcom's group_create on the engine route raises exactly one confirmation and
// starts nothing before it; the thread gets one group card that names the group; the group's
// coordinator address is capcom. An event turn's group_create is refused with nothing filed.
// After the yes, the lead's seed names capcom as its coordinator, the lead's report reaches the
// thread as an agent item and starts a turn, and a user's close sends Capcom the result.
func TestCapcomGroup(t *testing.T) {
	needGit(t)
	fakes, work := groupFakes(t)
	// Events reach Capcom at once and at any hour: an event turn comes from a real notice.
	e := newServiceEnv(t, "\n[settings]\n\"capcom.quiet_hours\" = \"00:00-00:00\"\n\"capcom.events.batch_max\" = 1\n")
	e.env = append(e.env, "PATH="+fakes+":/usr/bin:/bin", "GFF_HAB_BIN="+bin, "GFF_HAB_WORKTREES="+filepath.Join(shortDir(t), "wt"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(work, "claude"), noRaceExitSleep)
	e.serve()
	c := e.control()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("stub log:\n%s", e.stubLog())
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(150*time.Second))
	defer cancel()
	run(t, "capcom", "assign", "-socket", e.sock, "self")
	var st api.CapcomServiceStatus
	waitUntil(t, "the engine connected", func() bool {
		st, _ = callJSON[api.CapcomServiceStatus](t, c, "capcom.status", api.CapcomServiceParams{})
		return st.Connected
	})
	repo := gitRepo(t, e.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	fileProject(t, e.habEnv, repo)
	waitCatalogProject(t, e.habEnv, "shop", 1)

	plan := api.GroupPlan{Title: "Fix cart", Project: "shop", Items: []api.GroupPlanItem{{ID: "a", Title: "Round once"}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "a", Harness: "claude", Name: "lead-a"}}}
	pj, _ := json.Marshal(plan)
	// reply waits for Capcom's reply that starts with prefix and holds part.
	reply := func(prefix, part string) string {
		t.Helper()
		var text string
		waitUntil(t, "the "+prefix+" reply", func() bool {
			it, ok := threadItem(threadText(t, c), func(it api.CapcomItem) bool {
				return it.Author.Kind == "capcom" && strings.HasPrefix(it.Text, prefix) && strings.Contains(it.Text, part)
			})
			text = it.Text
			return ok
		})
		return text
	}

	// An event turn may not start a group (capcom-service.md 9.4): a notice starts it, and the
	// stub calls group_create with the plan from its folder.
	if err := os.WriteFile(filepath.Join(e.stubDir, "plan.json"), pj, 0o600); err != nil {
		t.Fatal(err)
	}
	notifyAt(t, e.sock, "active", "try group")
	if r := reply("group: ", "error"); !strings.Contains(r, "ask the user first") || !strings.Contains(r, `"denied"`) {
		t.Fatalf("an event turn's group_create: %s", r)
	}
	var l api.GroupList
	call(t, c, "group.list", api.GroupListParams{}, &l)
	if len(l.Groups) != 0 || len(openConfirmations(t, e.habEnv)) != 0 {
		t.Fatalf("an event turn filed %+v", l.Groups)
	}

	// A user turn's group_create: one confirmation, nothing started, one card.
	run(t, "capcom", "say", "-socket", e.sock, "group "+string(pj))
	r := reply("group: 200 ", `"result"`)
	var gc struct {
		Result api.GroupCreated `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(r, "group: 200 ")), &gc); err != nil || gc.Result.Group.ID == "" {
		t.Fatalf("group_create from Capcom: %v %s", err, r)
	}
	gid := gc.Result.Group.ID
	open := openConfirmations(t, e.habEnv)
	if len(open) != 1 || open[0].Kind != "group" || open[0].Group == nil || open[0].Group.Group != gid {
		t.Fatalf("confirmations %+v", open)
	}
	g := getGroup(t, e.habEnv, gid)
	if g.State != "confirming" || g.Coordinator.Address != "capcom" || g.Coordinator.Session != st.Session {
		t.Fatalf("record %+v", g)
	}
	waitUntil(t, "the catalog's Capcom mark on the group", func() bool {
		var l api.CatalogList
		call(t, c, "catalog.list", api.CatalogListParams{}, &l)
		cg, ok := groupOf(l, gid)
		return ok && cg.Coordinator != nil && cg.Coordinator.Capcom
	})
	cards := 0
	for _, it := range threadText(t, c) {
		if it.Kind == "card" && it.Card != nil && it.Card.Type == "group" {
			cards++
			if it.Card.Ref != gid || !strings.Contains(it.Text, "Fix cart") || len(it.Card.Choices) != 0 {
				t.Fatalf("group card %+v", it)
			}
		}
	}
	if cards != 1 {
		t.Fatalf("%d group cards", cards)
	}
	time.Sleep(time.Second)
	if ents, _ := filepath.Glob(filepath.Join(work, "cwd.*")); len(ents) != 0 {
		t.Fatal("a member started before the confirmation")
	}

	call(t, c, "confirm.answer", api.ConfirmAnswerParams{ID: open[0].ID, Choice: "confirm"}, &api.ConfirmationResult{})
	aPid := memberIn(t, work, "-a")
	waitUntil(t, "the lead's seed", func() bool { return strings.Contains(readIn(work, "in."+aPid), "The coordinator is capcom.") })

	// The lead's report reaches the thread at once and starts a turn.
	rt, _ := findIn(actionTools, "group_report")
	if _, text, err := rt.run(ctx, hookCaller{readIn(work, "addr."+aPid)}, toolArgs{"group": gid, "text": "rounded once"}); err != nil || !strings.Contains(text, "Sent") {
		t.Fatalf("report to Capcom: %v %s", err, text)
	}
	waitUntil(t, "the report's turn", func() bool {
		_, ok := threadItem(threadText(t, c), func(it api.CapcomItem) bool {
			return it.Author.Kind == "capcom" && strings.HasPrefix(it.Text, "echo: ") && strings.Contains(it.Text, "lead-a: rounded once")
		})
		return ok
	})
	if g := getGroup(t, e.habEnv, gid); len(g.Inbox) != 0 {
		t.Fatalf("a report to Capcom waited: %+v", g.Inbox)
	}

	// An event turn may not close it either; the user's close sends Capcom the result.
	if err := os.WriteFile(filepath.Join(e.stubDir, "group"), []byte(gid), 0o600); err != nil {
		t.Fatal(err)
	}
	notifyAt(t, e.sock, "active", "try groupclose")
	if r := reply("groupclose: ", "error"); !strings.Contains(r, "ask the user first") {
		t.Fatalf("an event turn's group_close: %s", r)
	}
	call(t, c, "group.close", api.GroupRef{Group: gid}, &api.GroupResult{})
	waitUntil(t, "the result in the thread", func() bool {
		_, ok := threadItem(threadText(t, c), func(it api.CapcomItem) bool {
			return it.Kind == "agent" && strings.Contains(it.Text, "the user ended the group") && strings.Contains(it.Text, "0 of 1 item landed")
		})
		return ok
	})
}
