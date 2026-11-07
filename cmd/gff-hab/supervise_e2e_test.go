package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/groups"
)

// fakeSupervisedBody follows the trust prelude: a Claude Code stand-in that runs the real hook
// and status-line commands. It posts its context percentage (its own file, else the test's) to
// the status line every 200 ms, answers each message with one turn (UserPromptSubmit, then
// Stop; a message is read up to the paste's end mark, since bash's read sets its own terminal
// mode), writes a group_note when asked for progress or a hand-off, and compacts on /compact
// (PreCompact, a compact_boundary, SessionStart compact), after which its percentage is 5.
const fakeSupervisedBody = `printf '%s' "$GFF_HAB" > "$D/addr.$$"
pwd -P > "$D/cwd.$$"
T="$D/t.$$.jsonl"; : > "$T"
base="\"session_id\":\"sess-$$\",\"cwd\":\"$(pwd -P)\",\"transcript_path\":\"$T\""
hook() { printf '{%s,"hook_event_name":"%s"%s}' "$base" "$1" "$2" | @BIN@ hook claude "$1" > /dev/null 2>&1; }
pct() { cat "$D/pct.$$" 2>/dev/null || cat "$D/pct" 2>/dev/null || echo 10; }
hook SessionStart ',"source":"startup"'
( while :; do
    printf '{%s,"hook_event_name":"Status","context_window":{"context_window_size":200000,"used_percentage":%s}}' "$base" "$(pct)" | @BIN@ statusline claude > /dev/null 2>&1
    sleep 0.2
  done ) &
buf=
while IFS= read -r -d '~' chunk; do
  buf="$buf$chunk~"
  case "$chunk" in *$'\033[201') ;; *) continue ;; esac
  msg=$buf; buf=
  printf '%s\n' "$msg" >> "$D/in.$$"
  case "$msg" in
    *"/compact"*)
      hook PreCompact ',"trigger":"manual","custom_instructions":""'
      printf '%s\n' '{"type":"system","subtype":"compact_boundary","uuid":"b1"}' >> "$T"
      echo 5 > "$D/pct.$$"
      hook SessionStart ',"source":"compact"'
      ;;
    *)
      hook UserPromptSubmit ',"prompt":"work"'
      case "$msg" in
        *"your progress"*|*"hand-off"*)
          GFF_HAB="$GFF_HAB" @BIN@ act group_note -group "$(cat "$D/gid")" -item a -text "note from $$: cart rounding half done" >> "$D/note.$$" 2>&1 ;;
      esac
      hook Stop ''
      ;;
  esac
done
`

// TestGroupSupervision (groups.md 9, 16.2 B6) under logbook.approval = agents, with the real
// holder and gff-hab and a fake Claude Code member: at 39% nothing happens; at 41% the member
// is asked for its progress, writes a group_note, and gets exactly one compaction with the
// hab's follow-up, counted on its session and the group record. With
// group.fresh_after_compacts at 1 the next trigger asks for a hand-off note and starts a fresh
// session under the old name in the same worktree; the old one is renamed lead-a-1, leaves the
// group and its conversation stays. A message held for the old session (the group is paused)
// reaches the new one, after its seed, which points at the notes on the item's task.
func TestGroupSupervision(t *testing.T) {
	needGit(t)
	fakes, work := shortDir(t), shortDir(t)
	prelude := strings.Replace(strings.ReplaceAll(fakeTrustClaudePrelude, "@DIR@", work), "#!/bin/sh", "#!/bin/bash", 1)
	writeFile(t, filepath.Join(fakes, "claude"), prelude+strings.ReplaceAll(fakeSupervisedBody, "@BIN@", bin))
	if err := os.Chmod(filepath.Join(fakes, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := newHolderEnv(t, 0)
	e.env = []string{"PATH=" + fakes + ":/usr/bin:/bin", "GFF_HAB_BIN=" + bin, "GFF_HAB_WORKTREES=" + filepath.Join(shortDir(t), "wt"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(work, "claude"), "GFF_TEST_ROUND_QUIET=0s", "GFF_TEST_SUPERVISE_TICK=200ms", noRaceExitSleep}
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
	session := func(name string) (api.Session, bool) {
		var st api.StructureResult
		call(t, e.control(), "structure.get", api.StructureGetParams{}, &st)
		for _, s := range st.Sessions {
			if s.Name == name {
				return s, true
			}
		}
		return api.Session{}, false
	}
	repo := gitRepo(t, e.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	createSession(t, e.sock, api.SessionCreateParams{Name: "alice", Argv: []string{"/bin/sh", "-c", fakeCoordinator, "sh", work}, ProjectDir: repo,
		Cols: 80, Rows: 24, Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}})
	waitUntil(t, "alice's GFF_HAB", func() bool { return readIn(work, "addr") != "" })
	addr := readIn(work, "addr")
	waitCatalogProject(t, e, "shop", 1)
	set("confirm.group_plan", "false")
	set("logbook.approval", `"agents"`)
	writeFile(t, filepath.Join(work, "pct"), "39")

	plan := api.GroupPlan{Title: "Fix cart", Project: "shop", Items: []api.GroupPlanItem{{ID: "a", Title: "Round once"}},
		Members: []api.GroupPlanMember{{Role: "lead", Item: "a", Harness: "claude", Name: "lead-a"}}}
	pj, _ := json.Marshal(plan)
	tl, _ := findIn(actionTools, "group_create")
	res, _, err := tl.run(ctx, hookCaller{addr}, toolArgs{"plan": string(pj)})
	if err != nil {
		t.Fatal(err)
	}
	gid := res.(api.GroupCreated).Group.ID
	writeFile(t, filepath.Join(work, "gid"), gid)
	pid := memberIn(t, work, "-a")
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ents, _ := os.ReadDir(work)
		for _, en := range ents {
			t.Logf("%s: %q", en.Name(), readIn(work, en.Name()))
		}
	})
	waitUntil(t, "the lead's seed", func() bool { return strings.Contains(readIn(work, "in."+pid), "agent group "+gid) })
	waitUntil(t, "context_pct 39", func() bool {
		s, ok := session("lead-a")
		return ok && s.ContextPct != nil && *s.ContextPct == 39
	})
	time.Sleep(time.Second)
	if strings.Contains(readIn(work, "in."+pid), "your progress") {
		t.Fatal("a member under group.compact_at was asked")
	}

	// 41%: one compact.
	writeFile(t, filepath.Join(work, "pct"), "41")
	waitUntil(t, "the progress ask", func() bool {
		in := readIn(work, "in."+pid)
		return strings.Contains(in, "Your context is at 41%: send your progress with group_note (group "+gid+", item a)")
	})
	waitUntil(t, "the follow-up", func() bool {
		return strings.Contains(readIn(work, "in."+pid), "Continue item a (Round once); your notes are on task ")
	})
	waitUntil(t, "the compact counted", func() bool { s, _ := session("lead-a"); return s.Compacts == 1 })
	if n := strings.Count(readIn(work, "in."+pid), "/compact"); n != 1 {
		t.Fatalf("%d compactions", n)
	}
	if note := readIn(work, "note."+pid); !strings.Contains(note, "Noted") {
		t.Fatalf("the member's group_note: %q", note)
	}
	waitUntil(t, "the record's compacts", func() bool {
		for _, m := range getGroup(t, e, gid).Members {
			if m.Name == "lead-a" {
				return m.Compacts == 1
			}
		}
		return false
	})
	time.Sleep(time.Second)
	if n := strings.Count(readIn(work, "in."+pid), "/compact"); n != 1 {
		t.Fatalf("%d compactions after the reading dropped", n)
	}

	// The next trigger after group.fresh_after_compacts: a fresh session. The group is paused
	// first, so a message to lead-a waits for the old session.
	old, _ := session("lead-a")
	call(t, e.control(), "group.pause", api.GroupRef{Group: gid}, &api.GroupResult{})
	waitUntil(t, "the pause in the catalog", func() bool {
		var l api.CatalogList
		call(t, e.control(), "catalog.list", api.CatalogListParams{}, &l)
		g, ok := groupOf(l, gid)
		return ok && g.Pause != nil
	})
	if err := (hookCaller{addr}).Call(ctx, "channel.send", api.ChannelSendParams{To: "lead-a", Text: "status after the swap?"}, &api.ChannelSent{}); err != nil {
		t.Fatal(err)
	}
	set("group.fresh_after_compacts", "1")
	os.Remove(filepath.Join(work, "pct"))
	writeFile(t, filepath.Join(work, "pct."+pid), "41")
	waitUntil(t, "the hand-off ask", func() bool { return strings.Contains(readIn(work, "in."+pid), "Send a hand-off with group_note") })
	var fresh api.Session
	waitUntil(t, "the fresh session", func() bool {
		s, ok := session("lead-a")
		fresh = s
		return ok && s.ID != old.ID && s.Replaced != ""
	})
	if fresh.Replaced != old.ID || fresh.Group == nil || fresh.Group.ID != gid || fresh.Group.Item != "a" {
		t.Fatalf("fresh %+v", fresh)
	}
	aside, ok := session("lead-a-1")
	if !ok || aside.ID != old.ID || aside.Group != nil {
		t.Fatalf("the old session %+v %v", aside, ok)
	}
	waitUntil(t, "the old session exited", func() bool { s, _ := session("lead-a-1"); return s.State == api.SessionStateExited })
	if strings.Contains(readIn(work, "in."+pid), "status after the swap?") {
		t.Fatal("the held message went into the old session")
	}
	call(t, e.control(), "group.resume", api.GroupRef{Group: gid}, &api.GroupResult{})
	var npid string
	waitUntil(t, "the new member", func() bool {
		ents, _ := os.ReadDir(work)
		for _, en := range ents {
			if p, ok := strings.CutPrefix(en.Name(), "cwd."); ok && p != pid && strings.HasSuffix(strings.TrimSpace(readIn(work, en.Name())), "-a") {
				npid = p
				return true
			}
		}
		return false
	})
	if c1, c2 := readIn(work, "cwd."+pid), readIn(work, "cwd."+npid); c1 != c2 {
		t.Fatalf("worktree %q, fresh in %q", c1, c2)
	}
	waitUntil(t, "the seed and the held message in the fresh session", func() bool {
		in := readIn(work, "in."+npid)
		return strings.Contains(in, "You are a fresh session of member lead-a") && strings.Contains(in, "Read the notes on task ") &&
			strings.Contains(in, "status after the swap?")
	})
	in := readIn(work, "in."+npid)
	if strings.Index(in, "fresh session of member") > strings.Index(in, "status after the swap?") {
		t.Fatalf("the held message came before the seed:\n%s", in)
	}
	waitUntil(t, "the record follows the fresh session", func() bool {
		for _, m := range getGroup(t, e, gid).Members {
			if m.Name == "lead-a" {
				return m.Session == fresh.ID && m.Replaced == old.ID
			}
		}
		return false
	})
	// One lead for the item; the record also lists the item's reviewer (stage B7).
	leads := 0
	for _, m := range getGroup(t, e, gid).Members {
		if m.Role == groups.RoleLead {
			leads++
		}
	}
	if leads != 1 {
		t.Fatalf("members %+v", getGroup(t, e, gid).Members)
	}
}
