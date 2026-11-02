package main

// End-to-end tests of resume and fork runs (rounds.md 5, 6, 7.5, 10.1, 11; stage 5) with the
// real gff-hab and holder: a fake harness plays the user's own session, the target. It logs
// every byte its pane gets, and takes orders from a file: report working, blocked, done, or
// exit.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/rounds"
)

// fakeTarget stands in for Claude Code or Codex in a session a round targets, named after the
// harness it plays. --fake <tag> names its files in @DIR@: in-<tag> logs every byte its pane
// gets, argv-<tag>-<pid> what it was started with. Its session id is the one --resume or
// `resume <id>` names; a fork (--fork-session, or `fork <id>`) and a new start take their own.
// At start it writes its transcript (transcript-<app>.jsonl, @SID@ filled in) and reports idle
// by SessionStart. It then follows orders written to ctl-<tag>: working (UserPromptSubmit),
// blocked (PermissionRequest), done (Stop), hold (the next prompt's turn waits for done),
// exit. A round's prompt (the envelope's end line, then Enter) gets UserPromptSubmit, a
// transcript with the reply, then Stop, unless held.
const fakeTarget = `#!/bin/sh
D=@DIR@
app=$(basename "$0")
tag=; sid=019a0000-0000-7000-8000-0000000000a1; fork=
args="$*"
while [ $# -gt 0 ]; do
  case "$1" in
    --fake) tag=$2; shift ;;
    --resume) sid=$2; shift ;;
    resume) sid=$2; shift ;;
    fork) sid=$2; fork=1; shift ;;
    --fork-session) fork=1 ;;
  esac
  shift
done
[ -n "$fork" ] && sid=019a0000-0000-7000-8000-0000000000b2
echo "$args" > "$D/argv-$tag-$$"
p() { printf '{"session_id":"%s","cwd":"/work/app","hook_event_name":"%s"%s}' "$sid" "$1" "$2"; }
hook() { p "$1" "$2" | "$GFF_HAB_BIN" hook "$app" "$1"; }
if [ "$app" = codex ]; then
  out="$CODEX_HOME/sessions/2026/01/01/rollout-2026-01-01T10-00-00-$sid.jsonl"
else
  out="$CLAUDE_CONFIG_DIR/projects/-work-app/$sid.jsonl"
fi
rec() { printf '{"parentUuid":"00000000-0000-4000-8000-00000000000%s","isSidechain":false,"type":"%s","uuid":"00000000-0000-4000-8000-00000000000%s","timestamp":"2026-01-01T10:00:0%s.000Z","userType":"external","entrypoint":"cli","cwd":"/work/app","sessionId":"%s","version":"2.1.310","message":%s}\n' "$1" "$2" "$3" "$3" "$sid" "$4"; }
write() {
  [ -e "$out" ] && [ -z "$fork" ] && [ "$1" = start ] && return
  mkdir -p "$(dirname "$out")"
  sed "s/@SID@/$sid/g" "$D/transcript-$app.jsonl" > "$out.tmp"
  # Claude Code: the round's prompt as a user message, and the reply after it once done.
  m=$(grep -o 'begin round message [^]]*' "$in" 2>/dev/null | tail -n 1)
  if [ "$app" = claude ] && [ -n "$m" ] && [ "$1" != start ]; then
    rec 3 user 4 '{"role":"user","content":"['"$m"']"}' >> "$out.tmp"
    [ "$1" = reply ] && rec 4 assistant 5 '{"id":"msg_3","role":"assistant","model":"model-x","type":"message","content":[{"type":"text","text":"Round reply."}]}' >> "$out.tmp"
  fi
  mv "$out.tmp" "$out"
}
stty raw -echo
in="$D/in-$tag"
: >> "$in"
exec 3<&0
cat <&3 >> "$in" &
seen=$(grep -c 'end round message' "$in")
write start
printf '\033]7501;state=working:app=%s\033\\' "$app"
hook SessionStart ',"source":"startup"'
hold=
while :; do
  if [ -e "$D/ctl-$tag" ]; then
    c=$(cat "$D/ctl-$tag"); rm -f "$D/ctl-$tag"
    case $c in
      working) hook UserPromptSubmit ',"prompt":"x"' ;;
      blocked) hook PermissionRequest ',"tool_name":"Bash","tool_input":{"command":"make test"}' ;;
      done) write reply; hook Stop '' ;;
      hold) hold=1 ;;
      exit) : > "$D/ack-$tag"; kill $! 2>/dev/null; exit 0 ;;
    esac
    : > "$D/ack-$tag"
  fi
  n=$(grep -c 'end round message' "$in")
  if [ "$n" -gt "$seen" ] && [ "$(tail -c 1 "$in" | od -An -c | tr -d ' ')" = '\r' ]; then
    seen=$n
    write prompt
    hook UserPromptSubmit ',"prompt":"x"'
    if [ -z "$hold" ]; then write reply; hook Stop ''; fi
  fi
  sleep 0.05
done
`

// lockEnv is harnessRoundEnv with the fake target as claude and codex.
func lockEnv(t *testing.T) (*habEnv, *control.Client, string, string) {
	t.Helper()
	fake := shortDir(t)
	work := shortDir(t)
	script := strings.ReplaceAll(fakeTarget, "@DIR@", work)
	for _, name := range []string{"claude", "codex"} {
		writeFile(t, filepath.Join(fake, name), script)
		if err := os.Chmod(filepath.Join(fake, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(work, "transcript-claude.jsonl"), claudeTranscript("Round reply."))
	writeFile(t, filepath.Join(work, "transcript-codex.jsonl"), codexTranscript("Round reply."))
	e, repo := newRoundEnvWith(t, "PATH="+fake+":/usr/bin:/bin",
		"CLAUDE_CONFIG_DIR="+filepath.Join(work, "claude"), "CODEX_HOME="+filepath.Join(work, "codex"),
		"GFF_HAB_BIN="+bin, noRaceExitSleep, "GFF_TEST_ROUND_READY=5s", "GFF_TEST_ROUND_QUIET=0s")
	return e, e.control(), repo, work
}

// startTarget starts the user's own session: the fake as app, tagged tag, idle once it reports.
func startTarget(t *testing.T, e *habEnv, c *control.Client, repo, work, app, tag string) api.SessionCreated {
	t.Helper()
	made := createSession(t, e.sock, api.SessionCreateParams{Name: tag, Harness: app, Argv: []string{app, "--fake", tag}, ProjectDir: repo})
	waitUntil(t, tag+" idle with its session id", func() bool {
		st := agentStates(t, e.sock)[made.Pane]
		return st.State == api.AgentStatusIdle && st.Session != ""
	})
	return made
}

// order gives the target an order and waits until it took it.
func order(t *testing.T, work, tag, what string) {
	t.Helper()
	ack := filepath.Join(work, "ack-"+tag)
	_ = os.Remove(ack)
	writeFile(t, filepath.Join(work, "ctl-"+tag), what)
	waitUntil(t, tag+" took "+what, func() bool { _, err := os.Stat(ack); return err == nil })
}

// inputOf is every byte the target's pane got.
func inputOf(work, tag string) string {
	b, _ := os.ReadFile(filepath.Join(work, "in-"+tag))
	return string(b)
}

// addLockRound creates a manual resume or fork round on target.
func addLockRound(t *testing.T, c *control.Client, name string, mode api.RoundMode, target string, edit func(*api.RoundSpec)) {
	t.Helper()
	spec := api.RoundSpec{Name: name, Mode: mode, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		Target: target, Prompt: roundPrompt, HarnessArgs: []string{"--fake", name}}
	if mode == api.RoundModeResume {
		spec.WaitMS = 30_000
	}
	if edit != nil {
		edit(&spec)
	}
	call(t, c, "round.create", api.RoundCreateParams{Round: spec}, nil)
}

// waitRunWhy waits until the round's newest run is in state, with a reason holding why.
func waitRunWhy(t *testing.T, c *control.Client, name string, state api.RoundRunState, why string) api.RoundRun {
	t.Helper()
	var last []api.RoundRun
	waitUntil(t, name+" "+string(state)+" "+why, func() bool {
		last = roundRuns(t, c, name)
		return len(last) > 0 && last[0].State == state && strings.Contains(last[0].Reason, why)
	})
	return last[0]
}

// TestRoundLockWorking: a target working when its run starts gets the prompt only after its
// turn ends done; the run waits meanwhile, saying why. The prompt is one paste in the round's
// envelope, with the run's values in it; the run ends with the reply, and the target's pane
// stays: resume mode never closes it.
func TestRoundLockWorking(t *testing.T) {
	e, c, repo, work := lockEnv(t)
	tgt := startTarget(t, e, c, repo, work, "claude", "target")
	order(t, work, "target", "working")
	addLockRound(t, c, "hourly", api.RoundModeResume, tgt.Session, nil)
	runRound(t, c, "hourly")
	waitRunWhy(t, c, "hourly", api.RoundRunStateWaiting, lockWorking)
	time.Sleep(2 * time.Second) // the turn goes on; the run keeps waiting
	if got := inputOf(work, "target"); got != "" {
		t.Fatalf("the round wrote into a working session: %q", got)
	}
	if r := roundRuns(t, c, "hourly")[0]; r.State != api.RoundRunStateWaiting {
		t.Fatalf("run while the target works: %+v", r)
	}
	order(t, work, "target", "done")
	run := waitRun(t, e, c, "hourly", api.RoundRunStateSucceeded)
	if run.Result != "Round reply." || run.Session != tgt.Session {
		t.Errorf("run: %+v", run)
	}
	in := inputOf(work, "target")
	r := getRound(t, c, "hourly")
	for _, want := range []string{"\x1b[200~[Flightdeck: the prompt of round 'hourly', run 1, run by hand",
		"GFF_ROUND=" + r.ID, "GFF_ROUND_RUN=" + run.ID, "[begin round message " + run.ID + "]\n" + roundPrompt + "\n[end round message " + run.ID + "]\x1b[201~\r"} {
		if !strings.Contains(in, want) {
			t.Errorf("the prompt as delivered lacks %q:\n%q", want, in)
		}
	}
	if strings.Count(in, "\x1b[200~") != 1 {
		t.Errorf("one paste, got %q", in)
	}
	time.Sleep(300 * time.Millisecond)
	if !paneOpen(t, c, tgt.Session) {
		t.Error("a resume run closed its target")
	}
}

// TestRoundLockInUse: a target a client typed into, one in focus on a client, and a blocked one
// make the run wait; a target busy past wait ends the run skipped with the reason, and never
// gets a byte from the round. Pausing the round ends a waiting run skipped, "paused".
func TestRoundLockInUse(t *testing.T) {
	e, c, repo, work := lockEnv(t)
	short := func(s *api.RoundSpec) { s.WaitMS = 1500 }

	typed := startTarget(t, e, c, repo, work, "claude", "typed")
	call(t, c, "pane.input", api.InputParams{Pane: typed.Pane, Text: "a draft"}, nil)
	addLockRound(t, c, "r-typed", api.RoundModeResume, typed.Session, short)
	runRound(t, c, "r-typed")
	waitRunWhy(t, c, "r-typed", api.RoundRunStateWaiting, lockTyped)
	waitRunWhy(t, c, "r-typed", api.RoundRunStateSkipped, lockTyped)

	focused := startTarget(t, e, c, repo, work, "claude", "focused")
	call(t, c, "session.focus", api.SessionFocusParams{Session: focused.Session, Active: true}, nil)
	addLockRound(t, c, "r-focus", api.RoundModeResume, focused.Session, short)
	runRound(t, c, "r-focus")
	waitRunWhy(t, c, "r-focus", api.RoundRunStateSkipped, lockFocused)
	// Out of focus, the next run goes in.
	call(t, c, "session.focus", api.SessionFocusParams{Session: focused.Session, Active: false}, nil)
	runRound(t, c, "r-focus")
	waitRun(t, e, c, "r-focus", api.RoundRunStateSucceeded)

	blocked := startTarget(t, e, c, repo, work, "claude", "blocked")
	order(t, work, "blocked", "blocked")
	waitUntil(t, "the target blocked", func() bool { return agentStates(t, e.sock)[blocked.Pane].State == api.AgentStatusBlocked })
	addLockRound(t, c, "r-blocked", api.RoundModeResume, blocked.Session, short)
	runRound(t, c, "r-blocked")
	skipped := waitRunWhy(t, c, "r-blocked", api.RoundRunStateSkipped, lockBlocked)
	if skipped.Reason != lockBlocked {
		t.Errorf("reason %q", skipped.Reason)
	}

	working := startTarget(t, e, c, repo, work, "claude", "busy")
	order(t, work, "busy", "working")
	addLockRound(t, c, "r-paused", api.RoundModeResume, working.Session, nil)
	runRound(t, c, "r-paused")
	waitRunWhy(t, c, "r-paused", api.RoundRunStateWaiting, lockWorking)
	call(t, c, "round.pause", api.RoundRef{Name: "r-paused"}, nil)
	waitRunWhy(t, c, "r-paused", api.RoundRunStateSkipped, rounds.ReasonPaused)

	time.Sleep(300 * time.Millisecond)
	for _, tag := range []string{"blocked", "busy"} {
		if got := inputOf(work, tag); got != "" {
			t.Errorf("%s got bytes from a round: %q", tag, got)
		}
	}
	if got := inputOf(work, "typed"); got != "a draft" {
		t.Errorf("typed got %q, only the client's own input", got)
	}
}

// TestRoundLockTwoRounds: two rounds on one target take turns, one send at a time: each
// prompt goes in whole after the other's turn ended.
func TestRoundLockTwoRounds(t *testing.T) {
	e, c, repo, work := lockEnv(t)
	tgt := startTarget(t, e, c, repo, work, "claude", "shared")
	order(t, work, "shared", "working")
	addLockRound(t, c, "one", api.RoundModeResume, tgt.Session, nil)
	addLockRound(t, c, "two", api.RoundModeResume, tgt.Session, nil)
	runRound(t, c, "one")
	runRound(t, c, "two")
	waitRunWhy(t, c, "one", api.RoundRunStateWaiting, lockWorking)
	waitRunWhy(t, c, "two", api.RoundRunStateWaiting, lockWorking)
	order(t, work, "shared", "done")
	a := waitRun(t, e, c, "one", api.RoundRunStateSucceeded)
	b := waitRun(t, e, c, "two", api.RoundRunStateSucceeded)
	in := inputOf(work, "shared")
	if strings.Count(in, "\x1b[200~") != 2 {
		t.Fatalf("two pastes, got %q", in)
	}
	first, second := a, b
	if strings.Index(in, b.ID) < strings.Index(in, a.ID) {
		first, second = b, a
	}
	end := strings.Index(in, "[end round message "+first.ID+"]\x1b[201~\r")
	begin := strings.Index(in, "\x1b[200~[Flightdeck: the prompt of round")
	next := strings.LastIndex(in, "\x1b[200~[Flightdeck: the prompt of round")
	if end < 0 || begin != 0 || next < end || !strings.Contains(in[next:], second.ID) {
		t.Errorf("the prompts interleave:\n%q", in)
	}
}

// TestRoundLockTakeOver: typing into the target after the prompt went in ends the run taken
// over, and nothing else changes: the session runs on.
func TestRoundLockTakeOver(t *testing.T) {
	e, c, repo, work := lockEnv(t)
	tgt := startTarget(t, e, c, repo, work, "claude", "mine")
	order(t, work, "mine", "hold")
	addLockRound(t, c, "watch", api.RoundModeResume, tgt.Session, nil)
	runRound(t, c, "watch")
	waitUntil(t, "the prompt in", func() bool { return strings.Contains(inputOf(work, "mine"), "[end round message") })
	waitUntil(t, "the turn started", func() bool { return agentStates(t, e.sock)[tgt.Pane].State == api.AgentStatusWorking })
	call(t, c, "pane.input", api.InputParams{Pane: tgt.Pane, Text: "y"}, nil)
	waitRun(t, e, c, "watch", api.RoundRunStateTakenOver)
	time.Sleep(300 * time.Millisecond)
	if !paneOpen(t, c, tgt.Session) {
		t.Error("the target must run on after a take over")
	}
}

// TestRoundLockExited: an exited target is started again in a new pane of the same session,
// with its own session id, then gets the prompt.
func TestRoundLockExited(t *testing.T) {
	e, c, repo, work := lockEnv(t)
	tgt := startTarget(t, e, c, repo, work, "claude", "gone")
	sid := agentStates(t, e.sock)[tgt.Pane].Session
	addLockRound(t, c, "back", api.RoundModeResume, tgt.Session, nil)
	order(t, work, "gone", "exit")
	waitUntil(t, "the target exited", func() bool {
		ss, ok := runSessionOf(structureOf(t, c), tgt.Session)
		return ok && ss.State == api.SessionStateExited
	})
	runRound(t, c, "back")
	run := waitRun(t, e, c, "back", api.RoundRunStateSucceeded)
	ss, _ := runSessionOf(structureOf(t, c), tgt.Session)
	if run.Session != tgt.Session || ss.Pane == tgt.Pane || ss.State != api.SessionStateRunning {
		t.Errorf("run %+v, target %+v", run, ss)
	}
	argv := argvOf(t, work, "back")
	if !strings.Contains(argv, "--fake back --resume "+sid) {
		t.Errorf("resumed with %q, want --resume %s", argv, sid)
	}
	if !strings.Contains(inputOf(work, "back"), "[end round message "+run.ID+"]") {
		t.Errorf("the resumed target got %q", inputOf(work, "back"))
	}
}

// argvOf is the argv of the one start of the fake tagged tag.
func argvOf(t *testing.T, work, tag string) string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(work, "argv-"+tag+"-*"))
	if len(m) != 1 {
		t.Fatalf("starts of %s: %v", tag, m)
	}
	b, _ := os.ReadFile(m[0])
	return strings.TrimSpace(string(b))
}

// TestRoundForkClaude and TestRoundForkCodex: a fork run starts a new session from the
// target's conversation in the target's folder; the fork gets the prompt, the target's
// transcript stays byte-identical and its pane gets nothing.
func TestRoundForkClaude(t *testing.T) { testRoundFork(t, "claude") }
func TestRoundForkCodex(t *testing.T)  { testRoundFork(t, "codex") }

func testRoundFork(t *testing.T, app string) {
	e, c, repo, work := lockEnv(t)
	tgt := startTarget(t, e, c, repo, work, app, "plan")
	sid := agentStates(t, e.sock)[tgt.Pane].Session
	path := filepath.Join(work, "claude", "projects", "-work-app", sid+".jsonl")
	if app == "codex" {
		path = filepath.Join(work, "codex", "sessions", "2026", "01", "01", "rollout-2026-01-01T10-00-00-"+sid+".jsonl")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	addLockRound(t, c, "next", api.RoundModeFork, tgt.Session, nil)
	runRound(t, c, "next")
	run := waitRun(t, e, c, "next", api.RoundRunStateSucceeded)
	if run.Session == tgt.Session || run.Result != "Round reply." {
		t.Errorf("fork run: %+v", run)
	}
	ss, _ := runSessionOf(structureOf(t, c), run.Session)
	if ss.Name != "next" || ss.Round == nil || ss.Round.Run != run.ID {
		t.Errorf("fork session: %+v", ss)
	}
	want := "--fake next --resume " + sid + " --fork-session"
	if app == "codex" {
		want = "--no-daemon fork " + sid + " --fake next"
	}
	if got := argvOf(t, work, "next"); got != want {
		t.Errorf("fork argv %q, want %q", got, want)
	}
	if !strings.Contains(inputOf(work, "next"), "[end round message "+run.ID+"]") {
		t.Errorf("the fork got %q", inputOf(work, "next"))
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("the target's transcript changed")
	}
	if got := inputOf(work, "plan"); got != "" {
		t.Errorf("the target got %q", got)
	}
}

// TestRoundLockTargetGone: a round whose target's record is removed completes, and its run
// ends skipped.
func TestRoundLockTargetGone(t *testing.T) {
	e, c, repo, work := lockEnv(t)
	tgt := startTarget(t, e, c, repo, work, "claude", "old")
	addLockRound(t, c, "orphan", api.RoundModeResume, tgt.Session, nil)
	addLockRound(t, c, "orphan-fork", api.RoundModeFork, tgt.Session, nil)
	call(t, c, "session.close", api.SessionRef{Session: tgt.Session}, nil)
	for _, name := range []string{"orphan", "orphan-fork"} {
		runRound(t, c, name)
		waitRunWhy(t, c, name, api.RoundRunStateSkipped, rounds.ErrTargetGone.Error())
		waitUntil(t, name+" completed", func() bool {
			r := getRound(t, c, name)
			return r.Completed && r.CompletedReason == rounds.ReasonTargetGone
		})
	}
}

// TestRoundLockDeletedKeepsTarget (review finding 1): a resume round deleted while its run is in
// its turn; the turn ends done, the run succeeds, and the target's pane stays open: only a
// session the run made is ever closed.
func TestRoundLockDeletedKeepsTarget(t *testing.T) {
	e, c, repo, work := lockEnv(t)
	tgt := startTarget(t, e, c, repo, work, "claude", "mine")
	order(t, work, "mine", "hold")
	addLockRound(t, c, "watch", api.RoundModeResume, tgt.Session, nil)
	runRound(t, c, "watch")
	waitUntil(t, "the prompt in", func() bool { return strings.Contains(inputOf(work, "mine"), "[end round message") })
	waitUntil(t, "the turn started", func() bool { return agentStates(t, e.sock)[tgt.Pane].State == api.AgentStatusWorking })
	r := getRound(t, c, "watch")
	call(t, c, "round.delete", api.RoundRef{ID: r.ID}, nil)
	order(t, work, "mine", "done")
	time.Sleep(3 * time.Second)
	if !paneOpen(t, c, tgt.Session) {
		t.Fatal("the target's pane was closed after its deleted resume round's run succeeded")
	}
}

// TestRoundLockRestart: gff-hab restarts with one resume run in its turn and another waiting.
// The run in its turn is adopted by its envelope in the conversation and gets no second
// prompt. The waiting one waits out the quiet window after the start, saying why, though its
// target is idle: a client may have typed while gff-hab was down. Each target gets one paste.
func TestRoundLockRestart(t *testing.T) {
	e, c, repo, work := lockEnv(t)
	sent := startTarget(t, e, c, repo, work, "claude", "sent")
	order(t, work, "sent", "hold")
	addLockRound(t, c, "in-turn", api.RoundModeResume, sent.Session, nil)
	runRound(t, c, "in-turn")
	waitUntil(t, "the prompt in", func() bool { return strings.Contains(inputOf(work, "sent"), "[end round message") })
	waitUntil(t, "the turn started", func() bool { return agentStates(t, e.sock)[sent.Pane].State == api.AgentStatusWorking })

	waiting := startTarget(t, e, c, repo, work, "claude", "waits")
	order(t, work, "waits", "working")
	addLockRound(t, c, "waiting", api.RoundModeResume, waiting.Session, nil)
	runRound(t, c, "waiting")
	waitRunWhy(t, c, "waiting", api.RoundRunStateWaiting, lockWorking)

	e.env = append(e.env, "GFF_TEST_ROUND_QUIET=6s")
	e.restart(true)
	c = e.control()
	order(t, work, "waits", "done")
	waitRunWhy(t, c, "waiting", api.RoundRunStateWaiting, lockStarted)
	if got := inputOf(work, "waits"); got != "" {
		t.Fatalf("the round wrote in the quiet window: %q", got)
	}
	if r := roundRuns(t, c, "in-turn")[0]; r.State != api.RoundRunStateRunning {
		t.Fatalf("the run in its turn after the restart: %+v", r)
	}
	order(t, work, "sent", "done")
	waitRun(t, e, c, "in-turn", api.RoundRunStateSucceeded)
	waitRun(t, e, c, "waiting", api.RoundRunStateSucceeded)
	time.Sleep(time.Second)
	for _, tag := range []string{"sent", "waits"} {
		if n := strings.Count(inputOf(work, tag), "\x1b[200~"); n != 1 {
			t.Errorf("%s got %d pastes: %q", tag, n, inputOf(work, tag))
		}
	}
}

// TestRoundLockTwoExited (review finding 5): two resume rounds on one exited target. One run
// starts it again; the other's start is refused, so it waits for the lock and takes its turn.
// Both succeed, and the target was started once.
func TestRoundLockTwoExited(t *testing.T) {
	e, c, repo, work := lockEnv(t)
	tgt := startTarget(t, e, c, repo, work, "claude", "gone")
	addLockRound(t, c, "one", api.RoundModeResume, tgt.Session, nil)
	addLockRound(t, c, "two", api.RoundModeResume, tgt.Session, nil)
	order(t, work, "gone", "exit")
	waitUntil(t, "the target exited", func() bool {
		ss, ok := runSessionOf(structureOf(t, c), tgt.Session)
		return ok && ss.State == api.SessionStateExited
	})
	runRound(t, c, "one")
	runRound(t, c, "two")
	waitRun(t, e, c, "one", api.RoundRunStateSucceeded)
	waitRun(t, e, c, "two", api.RoundRunStateSucceeded)
	a, _ := filepath.Glob(filepath.Join(work, "argv-one-*"))
	b, _ := filepath.Glob(filepath.Join(work, "argv-two-*"))
	if len(a)+len(b) != 1 {
		t.Fatalf("the target started %d times: %v %v", len(a)+len(b), a, b)
	}
	tag := "one"
	if len(b) == 1 {
		tag = "two"
	}
	if n := strings.Count(inputOf(work, tag), "\x1b[200~"); n != 2 {
		t.Errorf("two pastes into the started target, got %d: %q", n, inputOf(work, tag))
	}
}
