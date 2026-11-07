package main

// End-to-end tests of the group trust cover (groups.md 4.2, 12.6, 16.2 B2) with the real
// gff-hab and holder: a fake Claude Code (or Codex) draws its folder trust dialog in the folder
// it starts in, and the member's own hab answers Claude Code's when the folder is a worktree it
// made for the group. Codex's trust reaches the whole repository, so its prompt always shows the
// card.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// fakeTrustCodexPrelude plays Codex 0.167 in a folder it has not trusted: "1. Trust and
// continue" first and selected, "2. Back to Agent Command Center" below; Up and Down move the
// cursor; each key it reads goes (hex) to DIR/keys.<pid>. Enter on the first row clears the
// screen and goes on as the fake teammate.
const fakeTrustCodexPrelude = `#!/bin/sh
D=@DIR@
stty raw -echo
cur=0
draw() {
  printf '\033[2J\033[H\r\n  Folder access\r\n\r\n  %s\r\n\r\n  Trust this folder? Codex can read, edit, and run files here, subject to your permission\r\n  settings. Your trust decision will be saved.\r\n\r\n' "$(pwd -P)"
  if [ "$cur" = 0 ]; then
    printf '\342\200\272 1. Trust and continue\r\n  2. Back to Agent Command Center\r\n'
  else
    printf '  1. Trust and continue\r\n\342\200\272 2. Back to Agent Command Center\r\n'
  fi
  printf '\r\n  enter continue \302\267 esc back\r\n'
}
byte() { dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'; }
draw
while :; do
  k=$(byte)
  case $k in
    1b)
      stty min 0 time 2
      a=$(byte); b=$(byte)
      stty min 1 time 0
      printf '%s%s%s ' "$k" "$a" "$b" >> "$D/keys.$$"
      case $b in
        42) cur=1 ;;
        41) cur=0 ;;
      esac
      draw
      ;;
    0d)
      printf '%s ' "$k" >> "$D/keys.$$"
      [ "$cur" = 0 ] && break
      draw
      ;;
    *) printf '%s ' "$k" >> "$D/keys.$$" ;;
  esac
done
printf '\033[2J\033[H'
pwd -P > "$D/trusted.$$"
`

// fakeTrustClaudePrelude plays Claude Code 2.1.317's workspace trust dialog: "No, exit" first
// and selected, "Yes, I trust this folder" below. Enter on Yes clears the screen and goes on
// as the fake teammate; Esc, or Enter on No, exits.
const fakeTrustClaudePrelude = `#!/bin/sh
D=@DIR@
stty raw -echo
cur=0
draw() {
  printf '\033[2J\033[H Accessing workspace:\r\n\r\n %s\r\n\r\n Quick safety check: Is this a project you created or one you trust?\r\n\r\n Claude Code'"'"'ll be able to read, edit, and execute files here.\r\n\r\n' "$(pwd -P)"
  if [ "$cur" = 0 ]; then
    printf ' \342\235\257 No, exit\r\n   Yes, I trust this folder\r\n'
  else
    printf '   No, exit\r\n \342\235\257 Yes, I trust this folder\r\n'
  fi
  printf '\r\n Enter to confirm \302\267 Esc to cancel\r\n'
}
byte() { dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'; }
draw
while :; do
  k=$(byte)
  case $k in
    1b)
      stty min 0 time 2
      a=$(byte); b=$(byte)
      stty min 1 time 0
      printf '%s%s%s ' "$k" "$a" "$b" >> "$D/keys.$$"
      [ -n "$a" ] || exit 0
      case $b in
        42) cur=1 ;;
        41) cur=0 ;;
      esac
      draw
      ;;
    0d)
      printf '%s ' "$k" >> "$D/keys.$$"
      [ "$cur" = 1 ] || exit 0
      break
      ;;
    *) printf '%s ' "$k" >> "$D/keys.$$" ;;
  esac
done
printf '\033[2J\033[H'
pwd -P > "$D/trusted.$$"
`

// trustFakes writes the trust-asking fake Claude Code and Codex over the spawn fakes.
func trustFakes(t *testing.T) (fakes, work string) {
	t.Helper()
	fakes, work = spawnFakes(t)
	body := strings.SplitN(fakeTeammate, "\n", 3)[2] // the teammate after its #!/bin/sh and D= lines
	// As Claude Code, the fake writes a Claude Code transcript where Claude Code keeps it.
	codexOut := `out="$CODEX_HOME/sessions/2026/01/01/rollout-2026-01-01T10-00-00-$sid.jsonl"`
	if !strings.Contains(body, codexOut) || !strings.Contains(body, `"$D/transcript" >`) {
		t.Fatal("the fake teammate changed: update trustFakes")
	}
	body = strings.Replace(body, codexOut, `if [ "$app" = codex ]; then `+codexOut+`; tr="$D/transcript"; else out="$CLAUDE_CONFIG_DIR/projects/-work-app/$sid.jsonl"; tr="$D/transcript-claude"; fi`, 1)
	body = strings.Replace(body, `"$D/transcript" >`, `"$tr" >`, 1)
	writeFile(t, filepath.Join(work, "transcript-claude"), claudeTranscript(spawnReview))
	for name, prelude := range map[string]string{"codex": fakeTrustCodexPrelude, "claude": fakeTrustClaudePrelude} {
		writeFile(t, filepath.Join(fakes, name), strings.ReplaceAll(prelude, "@DIR@", work)+body)
		if err := os.Chmod(filepath.Join(fakes, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return fakes, work
}

// trustShows counts, per pane, the agent.state events of a hab that showed blocked:trust.
type trustShows struct {
	mu sync.Mutex
	n  map[string]int
}

func watchTrustShows(t *testing.T, e *habEnv) *trustShows {
	t.Helper()
	ts := &trustShows{n: map[string]int{}}
	c := e.control()
	call(t, c, "events.subscribe", api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}}, nil)
	go func() {
		for n := range c.Notifications() {
			ev, err := n.Event()
			if n.Method != "event" || err != nil || ev.Type != api.EventAgentState {
				continue
			}
			var st api.AgentState
			if json.Unmarshal(ev.Data, &st) == nil && st.State == api.AgentStatusBlocked && st.Kind == api.BlockedKindTrust {
				ts.mu.Lock()
				ts.n[st.Pane]++
				ts.mu.Unlock()
			}
		}
	}()
	return ts
}

func (s *trustShows) of(pane string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n[canonicalPane(pane)] + s.n[pane]
}

// groupWorktreeOn makes a worktree of repo on branch through e's worktree.create, with no setup.
func groupWorktreeOn(t *testing.T, e *habEnv, repo, branch string) string {
	t.Helper()
	var c api.WorktreeCreated
	call(t, e.control(), "worktree.create", api.WorktreeCreateParams{WorktreeProject: api.WorktreeProject{ProjectDir: repo},
		Branch: branch, Path: filepath.Join(shortDir(t), "wt"), Setup: "skip"}, &c)
	if !c.Worktree.Managed {
		t.Fatalf("worktree %+v", c.Worktree)
	}
	return c.Worktree.Path
}

// keysIn returns every key the fake trust dialogs read.
func keysIn(work string) string { return pastedAll(work, "keys") }

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
}

// TestGroupTrustOnPeer (task acceptance, groups.md 16.1 rows 2 and 3): a Claude Code
// coordinator spawns a fake Claude Code lead of a group on the other hab, in a worktree that hab
// made on the group's branch. The owner confirms once, on the coordinator's hab. The other hab
// answers the trust dialog itself (Down, then Enter): no blocked:trust report, so no card, board
// row or push; the seed goes in after; the member's review reaches the coordinator by name. The
// task named a Codex member; Codex trusts the whole repository, so it is not covered (groups.md
// 4.2), and TestGroupTrustCases checks its card.
func TestGroupTrustOnPeer(t *testing.T) {
	needGit(t)
	fakes, work := trustFakes(t)
	m := newMeshPairEnv(t, fakes, "GFF_HAB_BIN="+bin,
		"CODEX_HOME="+filepath.Join(work, "codex"), "CLAUDE_CONFIG_DIR="+filepath.Join(work, "claude"), noRaceExitSleep)
	repo := gitRepo(t, m.a.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	wt := groupWorktreeOn(t, m.b, repo, "group/g3fa9c2/cache")
	shows := watchTrustShows(t, m.b)
	addr := startSpawner(t, m.a, work, repo)

	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(90*time.Second))
	defer cancel()
	tl, _ := findIn(actionTools, "spawn")
	type out struct {
		res  any
		text string
		err  error
	}
	done := make(chan out, 1)
	go func() {
		res, text, err := tl.run(ctx, hookCaller{addr}, toolArgs{"harness": "claude", "seed": spawnSeed, "host": "bee", "folder": wt,
			"group": "g3fa9c2", "role": "lead", "item": "cache"})
		done <- out{res, text, err}
	}()
	// confirm.group_plan is on by default: one confirmation, on the coordinator's hab.
	ac := m.a.control()
	var open api.Confirmation
	waitUntil(t, "the group start's confirmation", func() bool {
		var l api.ConfirmList
		call(t, ac, "confirm.list", api.ConfirmListParams{}, &l)
		for _, c := range l.Confirmations {
			if c.State == "open" {
				open = c
				return true
			}
		}
		return false
	})
	if s := open.Start; s.Group != "g3fa9c2" || s.Role != "lead" || s.Item != "cache" || s.Host != "bee" {
		t.Fatalf("confirmation %+v", s)
	}
	var answered api.ConfirmationResult
	call(t, ac, "confirm.answer", api.ConfirmAnswerParams{ID: open.ID, Choice: "confirm"}, &answered)
	o := <-done
	if o.err != nil {
		t.Fatal(o.err)
	}
	cf := o.res.(spawnResult).Confirmation
	name := "claude-" + cf.ID[:6]
	if cf.State != "done" || o.res.(spawnResult).Address != "bee/"+name {
		t.Fatalf("spawn: %+v\n%s", cf, o.text)
	}
	waitUntil(t, "the trust dialog answered", func() bool { return pastedAll(work, "trusted") != "" })
	if k := keysIn(work); k != "1b5b42 0d " {
		t.Fatalf("keys %q: want Down, then Enter", k)
	}
	sid := teammateRun(t, work, "")
	waitUntil(t, "the seed in the member", func() bool {
		b, _ := os.ReadFile(filepath.Join(work, "seed-"+sid))
		return strings.Contains(string(b), spawnSeed)
	})
	waitUntil(t, "the review back at the coordinator", func() bool {
		b, _ := os.ReadFile(filepath.Join(work, "alice-in"))
		return strings.Contains(string(b), "[Flightdeck: agent message from 'bee/"+name+"'") && strings.Contains(string(b), spawnReview)
	})
	if n := shows.of(cf.Result.Pane); n != 0 {
		t.Fatalf("b showed the trust prompt %d times", n)
	}
}

// TestGroupTrustCases (groups.md 16.2 B2): with confirm.group_plan off, a covered folder is
// answered all the same with no confirmation; a group start into a folder outside the cover (the
// main checkout) raises the card; a spawn outside a group raises the card in a group worktree
// too; a Codex member's prompt raises the card in a group worktree.
func TestGroupTrustCases(t *testing.T) {
	needGit(t)
	fakes, work := trustFakes(t)
	e := newHolderEnv(t, 0)
	cfg := filepath.Join(work, "hab.toml")
	writeFile(t, cfg, "[groups]\ntrust_hold = \"2s\"\n")
	e.env = []string{"PATH=" + fakes + ":/usr/bin:/bin", "GFF_HAB_CONFIG=" + cfg, "GFF_HAB_BIN=" + bin,
		"CODEX_HOME=" + filepath.Join(work, "codex"), "CLAUDE_CONFIG_DIR=" + filepath.Join(work, "claude"), noRaceExitSleep}
	e.serve()
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(90*time.Second))
	defer cancel()
	if err := c.Call(ctx, "settings.set", api.SettingsSetParams{Values: []api.SettingWrite{
		{ID: "confirm.group_plan", Value: json.RawMessage(`false`), Revision: 1}}}, nil); err != nil {
		t.Fatal(err)
	}
	repo := gitRepo(t, e.work, "shop")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	covered := groupWorktreeOn(t, e, repo, "group/g1/a")
	plain := groupWorktreeOn(t, e, repo, "group/g1/c")
	shows := watchTrustShows(t, e)
	addr := startSpawner(t, e, work, repo)
	tl, _ := findIn(actionTools, "spawn")
	spawn := func(args toolArgs) api.Confirmation {
		t.Helper()
		if _, ok := args["harness"]; !ok {
			args["harness"] = "claude"
		}
		args["seed"], args["mode"] = spawnSeed, "detached"
		res, text, err := tl.run(ctx, hookCaller{addr}, args)
		if err != nil {
			t.Fatal(err)
		}
		cf := res.(spawnResult).Confirmation
		if cf.State != "done" || !cf.Record {
			t.Fatalf("spawn %v: %+v\n%s", args, cf, text)
		}
		return cf
	}

	// Covered, with no confirmation: answered by the hab, never shown.
	a := spawn(toolArgs{"folder": covered, "group": "g1", "role": "lead", "item": "a", "name": "lead-a"})
	waitUntil(t, "the covered dialog answered", func() bool { return strings.Contains(pastedAll(work, "trusted"), filepath.Base(covered)) })
	sid := teammateRun(t, work, "")
	waitUntil(t, "the seed in the covered member", func() bool {
		b, _ := os.ReadFile(filepath.Join(work, "seed-"+sid))
		return strings.Contains(string(b), spawnSeed)
	})
	if n := shows.of(a.Result.Pane); n != 0 {
		t.Fatalf("the covered prompt showed %d times", n)
	}

	// A group start into the main checkout: outside the cover, the card shows at once.
	start := time.Now()
	b := spawn(toolArgs{"folder": repo, "group": "g1", "role": "lead", "item": "b", "name": "lead-b"})
	waitUntil(t, "the card for the main checkout", func() bool { return shows.of(b.Result.Pane) > 0 })
	if d := time.Since(start); d > scaleTimeout(10*time.Second) {
		t.Errorf("the card took %s", d)
	}

	// A spawn outside a group, in a group worktree: the card shows.
	cc := spawn(toolArgs{"folder": plain, "name": "loner"})
	waitUntil(t, "the card for a spawn outside a group", func() bool { return shows.of(cc.Result.Pane) > 0 })

	// A Codex member in a group worktree: Codex's trust reaches the whole repository, so the
	// card shows.
	cx := spawn(toolArgs{"harness": "codex", "folder": groupWorktreeOn(t, e, repo, "group/g1/d"), "group": "g1", "role": "reviewer", "item": "d", "name": "rev-d"})
	waitUntil(t, "the card for a Codex member", func() bool { return shows.of(cx.Result.Pane) > 0 })
	time.Sleep(3 * time.Second) // past the hold: no key reached any other dialog
	if k := keysIn(work); k != "1b5b42 0d " {
		t.Fatalf("keys %q: only the covered dialog's Down and Enter", k)
	}
}
