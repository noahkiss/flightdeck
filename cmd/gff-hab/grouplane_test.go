package main

// The merge lane in the group service (groups.md 7.3 to 7.5, 16.2 B4) over real git: a bare
// remote in a temporary folder, the home hab's checkout, and a second clone that moves main.
// The records are written by hand; the lane, the messages and the end are the service's.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/groups"
	"github.com/noahkiss/flightdeck/internal/structure"
)

type laneFix struct {
	t                 *testing.T
	dir               string
	bare, src, other  string
	store             *structure.Store
	svc               *groupsService
	keys              groups.Keys
	mu                sync.Mutex
	notes             map[string][]string // session -> Flightdeck messages
	closed            []string
	removeErr         map[string]error
	stateDir, laneDir string
}

func lgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newLaneFix makes the repositories, main with flightdeck.json's lane keys, and the service.
func newLaneFix(t *testing.T, laneJSON string, keys groups.Keys) *laneFix {
	t.Helper()
	needGit(t)
	// No global git config: a core.hooksPath there would hide the remote's hooks.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	d := t.TempDir()
	f := &laneFix{t: t, dir: d, bare: filepath.Join(d, "shop.git"), src: filepath.Join(d, "shop"), other: filepath.Join(d, "other"),
		notes: map[string][]string{}, removeErr: map[string]error{}, keys: keys, stateDir: filepath.Join(d, "state"), laneDir: filepath.Join(d, "lane")}
	lgit(t, d, "init", "-q", "--bare", "-b", "main", f.bare)
	lgit(t, d, "init", "-q", "-b", "main", f.src)
	lgit(t, f.src, "remote", "add", "origin", f.bare)
	if laneJSON != "" {
		writeFile(t, filepath.Join(f.src, "flightdeck.json"), laneJSON)
	}
	writeFile(t, filepath.Join(f.src, "README"), "shop\n")
	lgit(t, f.src, "add", "-A")
	lgit(t, f.src, "commit", "-q", "-m", "first")
	lgit(t, f.src, "push", "-q", "origin", "main")
	lgit(t, d, "clone", "-q", f.bare, f.other)
	st, err := structure.Open(structure.Config{Dir: filepath.Join(d, "structure"), Holders: structure.NewFakeHolder(
		structure.HolderPane{ID: "p-a"}, structure.HolderPane{ID: "p-b"})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	f.store = st
	f.start()
	return f
}

func (f *laneFix) catalog() api.CatalogList {
	return api.CatalogList{
		Hosts: []api.CatalogEntry{{Own: true, Catalog: api.Catalog{DeviceID: "dev-me"}}},
		Projects: []api.CatalogProjectGroup{{Key: "remote:" + f.bare, Name: "shop", Remote: f.bare,
			Locations: []api.CatalogLocation{{DeviceID: "dev-me", Folders: []api.CatalogFolder{{Path: f.src}}}}}},
	}
}

// start makes the service, as a gff-hab start does.
func (f *laneFix) start() {
	f.t.Helper()
	s, err := newGroups(groupsConfig{host: "home", self: "dev-me", dir: filepath.Join(f.stateDir, "groups"), keys: f.keys,
		store: f.store, catalog: f.catalog, laneDir: f.laneDir, laneRetry: 50 * time.Millisecond,
		note: func(session, text string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.notes[session] = append(f.notes[session], text)
			return nil
		},
		closePane: func(ctx context.Context, pane string) error {
			f.mu.Lock()
			f.closed = append(f.closed, pane)
			f.mu.Unlock()
			if ss, ok := f.store.SessionByPane(pane); ok {
				return f.store.CloseSession(ss.ID)
			}
			return nil
		},
		removeWorktree: func(ctx context.Context, p api.WorktreeRemoveParams) (api.WorktreeRemoved, error) {
			cmd := exec.CommandContext(ctx, "git", "-C", f.src, "worktree", "remove", "--", p.Path)
			if out, err := cmd.CombinedOutput(); err != nil {
				return api.WorktreeRemoved{}, fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			return api.WorktreeRemoved{}, nil
		},
		logf: f.t.Logf,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.svc = s
	f.t.Cleanup(s.Stop)
}

func (f *laneFix) notesOf(session string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.notes[session], "\n---\n")
}

// branch makes an item's branch from base in the home checkout with one commit per file
// (name=text), and pushes it, as the lead's submit does.
func (f *laneFix) branch(name, base string, files ...string) string {
	f.t.Helper()
	lgit(f.t, f.src, "fetch", "-q", "origin")
	lgit(f.t, f.src, "checkout", "-q", "-B", name, base)
	for _, file := range files {
		n, text, _ := strings.Cut(file, "=")
		if text == "" {
			text = n + "\n"
		}
		writeFile(f.t, filepath.Join(f.src, n), text)
		lgit(f.t, f.src, "add", "-A")
		lgit(f.t, f.src, "commit", "-q", "-m", "change "+n)
	}
	head := lgit(f.t, f.src, "rev-parse", "HEAD")
	lgit(f.t, f.src, "push", "-q", "--force", "origin", name)
	lgit(f.t, f.src, "checkout", "-q", "--detach", "origin/main")
	return head
}

// move pushes a commit to main from the other clone.
func (f *laneFix) move(file, text string) {
	lgit(f.t, f.other, "pull", "-q", "--rebase", "origin", "main")
	writeFile(f.t, filepath.Join(f.other, file), text)
	lgit(f.t, f.other, "add", "-A")
	lgit(f.t, f.other, "commit", "-q", "-m", "move "+file)
	lgit(f.t, f.other, "push", "-q", "origin", "HEAD:main")
}

func (f *laneFix) subjects() string {
	return strings.ReplaceAll(lgit(f.t, f.bare, "log", "--format=%s", "main"), "\n", ",")
}

// item is one item of a hand-made group: ready on head with review off unless verdict is set.
type laneItem struct {
	id, after, head string
	state           api.GroupItemState
	verdict         *api.GroupVerdict
	land            string
	folder          string // the lead's worktree
	session         string // the lead's session id; "" for none
}

// group adds a running group with the items, their leads on this hab, and the lane queue in
// the order given, then kicks it.
func (f *laneFix) group(id string, items ...laneItem) {
	f.t.Helper()
	g := makeGroup(id, items...)
	f.svc.mu.Lock()
	f.svc.groups[id] = &g
	f.svc.order = append(f.svc.order, id)
	f.svc.saveLocked(&g)
	f.svc.mu.Unlock()
	f.svc.kick(id)
}

// makeGroup is the record group adds.
func makeGroup(id string, items ...laneItem) api.Group {
	plan := api.GroupPlan{Title: "Lane " + id, Project: "shop"}
	for _, it := range items {
		plan.Items = append(plan.Items, api.GroupPlanItem{ID: it.id, Title: it.id, After: it.after, Review: "off"})
		plan.Members = append(plan.Members, api.GroupPlanMember{Role: "lead", Item: it.id, Harness: "claude", Name: "lead-" + it.id})
	}
	g := groups.New(id, plan, "shop", "dev-me", api.GroupCoordinator{Session: "s-coord", Name: "alice", Address: "home/alice"}, time.Now())
	g.State = groups.StateRunning
	for i, it := range items {
		gi := &g.Items[i]
		gi.State, gi.Submitted, gi.Land = it.state, it.head, it.head
		if gi.State == "" {
			gi.State = groups.ItemReady
		}
		if it.land != "" {
			gi.Land = it.land
		}
		if it.verdict != nil {
			gi.Review, gi.Reviewer, gi.Verdict = "on", "rev-"+it.id, it.verdict
		}
		if gi.State == groups.ItemReady {
			g.Lane.Queue = append(g.Lane.Queue, it.id)
		}
		m := &g.Members[i]
		m.State, m.Device, m.Folder, m.Session = groups.MemberRunning, "dev-me", it.folder, it.session
		if m.Session == "" {
			m.Session = "s-gone-" + it.id
		}
	}
	return g
}

func (f *laneFix) get(id string) api.Group {
	f.t.Helper()
	g, err := f.svc.Get(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return g
}

func (f *laneFix) waitItem(id, item string, st api.GroupItemState) api.Group {
	f.t.Helper()
	var g api.Group
	waitUntil(f.t, fmt.Sprintf("item %s %s", item, st), func() bool {
		g = f.get(id)
		return g.Items[groups.ItemIndex(g, item)].State == st
	})
	return g
}

func inbox(g api.Group) string {
	var b []string
	for _, r := range g.Inbox {
		b = append(b, r.Text)
	}
	return strings.Join(b, "\n")
}

// TestLaneInOrder: two items land on main in order, with no CI the item and the lane say so,
// the leads hear of the land and keep running, and the group ends with the plan done and the
// build folders listed.
func TestLaneInOrder(t *testing.T) {
	f := newLaneFix(t, `{"lane":{"checks":"test -f README","build_folders":["out","dist"]}}`, groups.DefaultKeys)
	a := f.branch("group/g1/a", "origin/main", "a.txt")
	b := f.branch("group/g1/b", "origin/main", "b.txt")
	f.group("1111111111111111", laneItem{id: "a", head: a}, laneItem{id: "b", head: b})
	g := f.waitItem("1111111111111111", "b", groups.ItemLanded)
	if got := f.subjects(); got != "change b.txt,change a.txt,first" {
		t.Fatalf("main: %s", got)
	}
	a0, b0 := g.Items[0], g.Items[1]
	if a0.State != groups.ItemLanded || !a0.NoCI || a0.Landed == "" || b0.Landed != lgit(t, f.bare, "rev-parse", "main") || !g.Lane.NoCI {
		t.Fatalf("items %+v %+v lane %+v", a0, b0, g.Lane)
	}
	if !strings.Contains(inbox(g), "item a landed on main at "+a0.Landed[:7]+" (no CI)") {
		t.Fatalf("coordinator: %s", inbox(g))
	}
	if n := f.notesOf("s-gone-a"); !strings.Contains(n, "landed on main") || !strings.Contains(n, "keeps running") {
		t.Fatalf("lead a: %s", n)
	}
	waitUntil(t, "the end", func() bool { return f.get("1111111111111111").State == groups.StateEnded })
	g = f.get("1111111111111111")
	left := strings.Join(g.Leftovers, "\n")
	if !strings.Contains(left, "build folder dist") || !strings.Contains(left, "build folder out") || strings.Contains(left, "branch ") {
		t.Fatalf("leftovers: %s", left)
	}
	if !strings.Contains(inbox(g), "The group ended: every item is landed or dropped") {
		t.Fatalf("coordinator: %s", inbox(g))
	}
}

// TestLaneBackToLead: a conflict goes back to the lead with the first [groups]
// fix_output_lines lines and the coordinator gets a line; the lane goes on with the next item.
// A failed CI blocks its land; a fix_needed verdict never lands, a pass with fixes lands the
// review head.
func TestLaneBackToLead(t *testing.T) {
	ci := filepath.Join(t.TempDir(), "ci.sh")
	writeFile(t, ci, "#!/bin/sh\necho ci-ran \"$1\"\ni=0; while [ $i -lt 30 ]; do echo ci-line$i; i=$((i+1)); done\n[ \"$1\" != group/2222222222222222/c ]\n")
	os.Chmod(ci, 0o700)
	keys := groups.DefaultKeys
	keys.FixOutputLines = 4
	f := newLaneFix(t, fmt.Sprintf(`{"lane":{"ci":%q}}`, ci+` "$1"`), keys)
	f.move("x.txt", "main's x\n")
	x := f.branch("group/g2/x", "origin/main~1", "x.txt=the item's x")
	ok := f.branch("group/g2/ok", "origin/main", "ok.txt")
	c := f.branch("group/g2/c", "origin/main", "c.txt")
	fix := f.branch("group/g2/fixneeded", "origin/main", "nope.txt")
	sub := f.branch("group/g2/withfix", "origin/main", "w.txt")
	rev := f.branch("group/g2/withfix-review", sub, "w-fix.txt")
	f.group("2222222222222222",
		laneItem{id: "x", head: x}, laneItem{id: "ok", head: ok}, laneItem{id: "c", head: c},
		laneItem{id: "fixneeded", head: fix, verdict: &api.GroupVerdict{Verdict: "fix_needed", Head: fix}},
		laneItem{id: "withfix", head: sub, land: rev, verdict: &api.GroupVerdict{Verdict: "pass", Head: sub, FixedHead: rev}})
	g := f.waitItem("2222222222222222", "withfix", groups.ItemLanded)
	if it := g.Items[0]; it.State != groups.ItemFix || !strings.Contains(it.Reason, "rebase") {
		t.Fatalf("conflict: %+v", it)
	}
	n := f.notesOf("s-gone-x")
	if !strings.Contains(n, "the rebase step") || !strings.Contains(n, "Rebase on main") || !strings.Contains(n, "x.txt") {
		t.Fatalf("lead x: %s", n)
	}
	if out := n[strings.Index(n, "The output:\n")+len("The output:\n"):]; len(strings.Split(out, "\n")) > 4 {
		t.Fatalf("more than fix_output_lines lines: %q", out)
	}
	if g.Items[1].State != groups.ItemLanded {
		t.Fatalf("the lane did not go on: %+v", g.Items[1])
	}
	// A failed CI: back with the CI's first lines, nothing landed.
	if it := g.Items[2]; it.State != groups.ItemFix || !strings.Contains(f.notesOf("s-gone-c"), "ci-ran group/2222222222222222/c\nci-line0\nci-line1\nci-line2") {
		t.Fatalf("failed CI: %+v %s", it, f.notesOf("s-gone-c"))
	}
	// fix_needed never lands: it stays ready in the queue, untouched.
	if it := g.Items[3]; it.State != groups.ItemReady || it.Landed != "" {
		t.Fatalf("fix_needed: %+v", it)
	}
	subj := f.subjects()
	for _, want := range []string{"change w-fix.txt", "change w.txt", "change ok.txt"} {
		if !strings.Contains(subj, want) {
			t.Fatalf("main %s lacks %s", subj, want)
		}
	}
	for _, not := range []string{"change c.txt", "change nope.txt", "the item's x"} {
		if strings.Contains(subj, not) {
			t.Fatalf("main %s has %s", subj, not)
		}
	}
	in := inbox(g)
	if !strings.Contains(in, "item x went back to its lead from the lane's rebase step") || !strings.Contains(in, "item c went back to its lead from the lane's ci step") {
		t.Fatalf("coordinator: %s", in)
	}
	if g.State != groups.StateRunning {
		t.Fatalf("the group %s with items in fix", g.State)
	}
}

// TestLaneStacked: an item that stacks on another lands after it, whatever the queue order,
// with the earlier commits out of its diff.
func TestLaneStacked(t *testing.T) {
	f := newLaneFix(t, "", groups.DefaultKeys)
	a := f.branch("group/g3/a", "origin/main", "a.txt")
	b := f.branch("group/g3/b", a, "b.txt")
	f.move("m.txt", "m\n")
	f.group("3333333333333333", laneItem{id: "b", after: "a", head: b}, laneItem{id: "a", head: a})
	f.waitItem("3333333333333333", "b", groups.ItemLanded)
	if got := f.subjects(); got != "change b.txt,change a.txt,move m.txt,first" {
		t.Fatalf("main: %s", got)
	}
}

// TestLaneMovingAndStopped: with [groups] lane_tries 1 one move of the target sends the item
// back with "main keeps moving"; a protected-branch refusal stops the lane for the repository
// until group.retry with lane.
func TestLaneMovingAndStopped(t *testing.T) {
	d := t.TempDir()
	ci := filepath.Join(d, "ci.sh")
	writeFile(t, ci, `#!/bin/sh
cd "$OTHER" && git pull -q --rebase origin main && echo moved > "moved-$$" && git add -A && git commit -q -m moved && git push -q origin HEAD:main
`)
	os.Chmod(ci, 0o700)
	keys := groups.DefaultKeys
	keys.LaneTries = 1
	f := newLaneFix(t, fmt.Sprintf(`{"lane":{"ci":%q}}`, ci), keys)
	t.Setenv("OTHER", f.other)
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	a := f.branch("group/g4/a", "origin/main", "a.txt")
	f.group("4444444444444444", laneItem{id: "a", head: a})
	g := f.waitItem("4444444444444444", "a", groups.ItemFix)
	if !strings.Contains(inbox(g), "main keeps moving (1 tries)") || !strings.Contains(f.notesOf("s-gone-a"), "keeps moving") {
		t.Fatalf("moving: %s / %s", inbox(g), f.notesOf("s-gone-a"))
	}

	// A protected main: the lane stops, and the item waits in ready.
	f2 := newLaneFix(t, "", groups.DefaultKeys)
	writeFile(t, filepath.Join(f2.bare, "hooks", "pre-receive"), "#!/bin/sh\nwhile read o n r; do [ \"$r\" = refs/heads/main ] && { echo main is protected; exit 1; }; done\nexit 0\n")
	os.Chmod(filepath.Join(f2.bare, "hooks", "pre-receive"), 0o700)
	p := f2.branch("group/g5/p", "origin/main", "p.txt")
	q := f2.branch("group/g5/q", "origin/main", "q.txt")
	f2.group("5555555555555555", laneItem{id: "p", head: p}, laneItem{id: "q", head: q})
	waitUntil(t, "the lane to stop", func() bool { return f2.get("5555555555555555").Lane.Stopped != "" })
	g = f2.get("5555555555555555")
	if g.Items[0].State != groups.ItemReady || g.Items[1].State != groups.ItemReady || !strings.Contains(g.Lane.Stopped, "rule") ||
		!strings.Contains(inbox(g), "The lane stopped") {
		t.Fatalf("stopped: %+v %s", g.Lane, inbox(g))
	}
	os.Remove(filepath.Join(f2.bare, "hooks", "pre-receive"))
	if _, err := f2.svc.Retry(context.Background(), api.GroupRetryParams{Group: "5555555555555555", Lane: true}); err != nil {
		t.Fatal(err)
	}
	f2.waitItem("5555555555555555", "q", groups.ItemLanded)
}

// TestLaneRestart: a hab that stopped after the land push, before it marked the item, finds
// the saved rebased head on the target at its next start and marks the item landed, pushing
// nothing.
func TestLaneRestart(t *testing.T) {
	f := newLaneFix(t, "", groups.DefaultKeys)
	f.svc.Stop()
	a := f.branch("group/6666666666666666/a", "origin/main", "a.txt")
	f.move("m.txt", "m\n")
	// The lane rebased a on the moved main, saved the head, pushed it to main, and the hab
	// stopped before it marked the item.
	lgit(t, f.other, "fetch", "-q", "origin")
	lgit(t, f.other, "cherry-pick", a)
	rebased := lgit(t, f.other, "rev-parse", "HEAD")
	lgit(t, f.other, "push", "-q", "origin", "HEAD:main")
	before := lgit(t, f.bare, "rev-parse", "main")
	g := makeGroup("6666666666666666", laneItem{id: "a", head: a})
	g.Items[0].State, g.Lane.Landing = groups.ItemLanding, "a"
	g.Items[0].Attempts = []api.GroupLaneAttempt{{At: "2026-11-07T00:00:00Z", Rebased: rebased, Outcome: "pushed", Step: "push"}}
	st, err := groups.NewStore(filepath.Join(f.stateDir, "groups"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(g); err != nil {
		t.Fatal(err)
	}
	f.start()
	g = f.waitItem("6666666666666666", "a", groups.ItemLanded)
	if g.Items[0].Landed != rebased || lgit(t, f.bare, "rev-parse", "main") != before {
		t.Fatalf("after the restart %+v, main %s", g.Items[0], lgit(t, f.bare, "rev-parse", "main"))
	}
}

// TestLaneWorktrees: after a land the lead's worktree stays while its session runs and goes
// once it ended; the lead holds no slot and closing it stalls nothing. The end closes idle
// members, removes nothing with uncommitted changes, and lists the leftovers.
func TestLaneWorktrees(t *testing.T) {
	f := newLaneFix(t, "", groups.DefaultKeys)
	a := f.branch("group/g7/a", "origin/main", "a.txt")
	wtA := filepath.Join(f.dir, "wt-a")
	lgit(t, f.src, "worktree", "add", "-q", wtA, "group/g7/a")
	wtB := filepath.Join(f.dir, "wt-b")
	b := f.branch("group/g7/b", "origin/main", "b.txt")
	lgit(t, f.src, "worktree", "add", "-q", wtB, "group/g7/b")
	writeFile(t, filepath.Join(wtB, "dirty.txt"), "not committed\n")
	sa, err := f.store.CreateSession(structure.SessionSpec{Name: "lead-a", Pane: "p-a", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := f.store.CreateSession(structure.SessionSpec{Name: "lead-b", Pane: "p-b", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	f.group("7777777777777777", laneItem{id: "a", head: a, folder: wtA, session: sa.ID},
		laneItem{id: "b", head: b, state: groups.ItemFix, folder: wtB, session: sb.ID})
	f.waitItem("7777777777777777", "a", groups.ItemLanded)
	f.svc.refresh("7777777777777777")
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(wtA); err != nil {
		t.Fatal("the worktree of a running lead went")
	}
	// The user closes the landed item's lead: nothing stalls, and its worktree goes.
	if err := f.store.CloseSession(sa.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "a's worktree to go", func() bool {
		f.svc.refresh("7777777777777777")
		_, err := os.Stat(wtA)
		return os.IsNotExist(err)
	})
	if g := f.get("7777777777777777"); g.Items[0].State != groups.ItemLanded || !g.Members[0].WorktreeRemoved {
		t.Fatalf("after the close %+v %+v", g.Items[0], g.Members[0])
	}
	// The end: b's lead is idle and closed; its worktree has uncommitted changes and stays.
	g, err := f.svc.Close(context.Background(), "7777777777777777")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wtB, "dirty.txt")); err != nil {
		t.Fatal("the end removed a worktree with uncommitted changes")
	}
	left := strings.Join(g.Leftovers, "\n")
	if g.State != groups.StateEnded || !strings.Contains(left, wtB) || !strings.Contains(left, "uncommitted") || !strings.Contains(left, "branch group/7777777777777777/b, not landed") {
		t.Fatalf("end %s: %s", g.State, left)
	}
	f.mu.Lock()
	closed := strings.Join(f.closed, ",")
	f.mu.Unlock()
	if closed != "p-b" {
		t.Fatalf("closed panes %q", closed)
	}
}
