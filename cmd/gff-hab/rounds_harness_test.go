package main

// End-to-end tests of harness runs (rounds.md 5, 7.2 to 7.6; stage 4) with the real gff-hab
// and holder: a fake harness on PATH reports over OSC 7501 and runs the real hook command, as
// Claude Code and Codex do, and writes a hand-written transcript.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/rounds"
)

// fakeRoundHarness stands in for a harness in a round's pane; it is named after the harness it
// plays (claude or codex). Its arguments pick what it does: --fake <mode> and --len <bytes of
// the prompt as delivered>. It saves what it was started with, then by mode before the prompt:
//
//	trust, trustwait: draws Claude Code's workspace trust dialog and reads one key: "y" goes
//	  on, any other exits 1 (trustwait's key must never come)
//	mute: reports nothing, ever
//	restart-start: waits for its gate file before it reports anything
//
// It then reports working over OSC 7501, idle by the SessionStart hook, reads the prompt, writes
// the user record of its transcript, reports working by UserPromptSubmit, then by mode:
//
//	success, keep: writes its transcript and runs Stop
//	error: writes its transcript and runs StopFailure with error server_error
//	permission: runs PermissionRequest, reads one key, runs PostToolUse, then as success
//	timeout: reads one key (the interrupt), then reports idle over OSC 7501
//	blocked: runs PermissionRequest and reads one key, which must never come
//	takeover: waits for its gate file, runs Notification idle_prompt, then reads the client's message, "x" as delivered
//	exit: exits 3
//	trail: runs Stop, and writes its transcript 1.5 s later
//	restart-turn: waits for its gate file, then as success
//	restart-end: waits for its gate file, writes its transcript, reports idle over OSC 7501
//
// It then waits until its pane is closed.
const fakeRoundHarness = `#!/bin/sh
D=@DIR@
app=$(basename "$0")
mode=; len=0; sid=019a0000-0000-7000-8000-0000000000f1
args="$*"
while [ $# -gt 0 ]; do
  case "$1" in
    --fake) mode=$2; shift ;;
    --len) len=$2; shift ;;
    --session-id) sid=$2; shift ;;
  esac
  shift
done
echo "$args" > "$D/argv-$mode"
echo "$sid" > "$D/sid-$mode"
pwd -P > "$D/cwd-$mode"
p() { printf '{"session_id":"%s","cwd":"/work/app","hook_event_name":"%s"%s}' "$sid" "$1" "$2"; }
hook() { p "$1" "$2" | "$GFF_HAB_BIN" hook "$app" "$1"; }
forever() { while :; do sleep 0.05; done; }
gate() { while [ ! -e "$D/gate-$mode" ]; do sleep 0.05; done; }
key() { head -c 1 > "$D/key-$mode.tmp" && mv "$D/key-$mode.tmp" "$D/key-$mode"; }
if [ "$app" = codex ]; then
  out="$CODEX_HOME/sessions/2026/01/01/rollout-2026-01-01T10-00-00-$sid.jsonl"; head=2
else
  out="$CLAUDE_CONFIG_DIR/projects/-work-app/$sid.jsonl"; head=1
fi
transcript() {
  mkdir -p "$(dirname "$out")"
  sed -n "1,${1}p" "$D/transcript-$app-$mode.jsonl" | sed "s/@SID@/$sid/g" > "$out.tmp" && mv "$out.tmp" "$out"
}
stty raw -echo
case $mode in
  trust|trustwait)
    printf '\033[2J\033[H Accessing workspace:\r\n\r\n /work/app\r\n\r\n Quick safety check: Is this a project you created or one you trust?\r\n\r\n \342\235\257 1. Yes, I trust this folder\r\n   2. No, exit\r\n\r\n Enter to confirm \302\267 Esc to cancel\r\n'
    key
    [ "$(cat "$D/key-$mode")" = y ] || exit 1
    printf '\033[2J\033[H'
    ;;
  mute) forever ;;
  restart-start) gate ;;
esac
printf '\033]7501;state=working:app=%s\033\\' "$app"
hook SessionStart ',"source":"startup"'
head -c "$len" > "$D/prompt-$mode.tmp" && mv "$D/prompt-$mode.tmp" "$D/prompt-$mode"
transcript $head
hook UserPromptSubmit ',"prompt":"x"'
case $mode in
  permission)
    hook PermissionRequest ',"tool_name":"Bash","tool_input":{"command":"make test"}'
    key
    hook PostToolUse ',"tool_name":"Bash","tool_input":{"command":"make test"},"tool_use_id":"toolu_1","tool_response":{"stdout":"ok"}'
    ;;
  timeout)
    key
    printf '\033]7501;state=idle:app=%s\033\\' "$app"
    forever
    ;;
  blocked)
    hook PermissionRequest ',"tool_name":"Bash","tool_input":{"command":"make test"}'
    key
    forever
    ;;
  takeover)
    gate
    hook Notification ',"notification_type":"idle_prompt"'
    head -c 14 > "$D/msg-$mode.tmp" && mv "$D/msg-$mode.tmp" "$D/msg-$mode"
    forever
    ;;
  exit) exit 3 ;;
  trail)
    hook Stop ''
    sleep 1.5
    transcript '$'
    forever
    ;;
  restart-turn) gate ;;
  restart-end)
    gate
    transcript '$'
    printf '\033]7501;state=idle:app=%s\033\\' "$app"
    forever
    ;;
esac
transcript '$'
if [ "$mode" = error ]; then
  hook StopFailure ',"error":"server_error"'
else
  hook Stop ''
fi
forever
`

// claudeTranscript is a hand-written Claude Code transcript of one turn: the prompt, a first
// agent message, then the final one.
func claudeTranscript(final string) string {
	rec := func(typ, uuid, parent, msg string) string {
		return `{"parentUuid":` + parent + `,"isSidechain":false,"type":"` + typ + `","uuid":"` + uuid +
			`","timestamp":"2026-01-01T10:00:0` + uuid[len(uuid)-1:] + `.000Z","userType":"external","entrypoint":"cli","cwd":"/work/app","sessionId":"@SID@","version":"2.1.310","message":` + msg + "}\n"
	}
	text := func(s string) string { b, _ := jsonString(s); return b }
	return rec("user", "00000000-0000-4000-8000-000000000001", "null", `{"role":"user","content":"Triage the open issues."}`) +
		rec("assistant", "00000000-0000-4000-8000-000000000002", `"00000000-0000-4000-8000-000000000001"`,
			`{"id":"msg_1","role":"assistant","model":"model-x","type":"message","content":[{"type":"text","text":"Looking."}]}`) +
		rec("assistant", "00000000-0000-4000-8000-000000000003", `"00000000-0000-4000-8000-000000000002"`,
			`{"id":"msg_2","role":"assistant","model":"model-x","type":"message","content":[{"type":"text","text":`+text(final)+`}]}`)
}

// codexTranscript is a hand-written Codex rollout of one turn.
func codexTranscript(final string) string {
	b, _ := jsonString(final)
	return `{"timestamp":"2026-01-01T10:00:00.000Z","type":"session_meta","payload":{"session_id":"@SID@","id":"@SID@","timestamp":"2026-01-01T10:00:00.000Z","cwd":"/work/app","originator":"codex-tui","cli_version":"0.166.0","source":"cli","model_provider":"openai"}}
{"timestamp":"2026-01-01T10:00:01.000Z","type":"event_msg","payload":{"type":"item_completed","thread_id":"@SID@","turn_id":"t1","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"Triage the open issues.","text_elements":[]}]}}}
{"timestamp":"2026-01-01T10:00:02.000Z","type":"event_msg","payload":{"type":"item_completed","thread_id":"@SID@","turn_id":"t1","item":{"type":"AgentMessage","id":"a1","content":[{"type":"Text","text":` + b + `}],"phase":"final"}}}
`
}

func jsonString(s string) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

// harnessRoundEnv is newRoundEnv with fake claude and codex programs on a PATH of their folder
// and the system's (so no real harness is found), their config folders in the test's own
// directories, and a short readiness limit; env adds to gff-hab's environment, over those.
func harnessRoundEnv(t *testing.T, env ...string) (*habEnv, string, string) {
	t.Helper()
	fake := shortDir(t)
	work := shortDir(t)
	script := strings.ReplaceAll(fakeRoundHarness, "@DIR@", work)
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(fake, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e, repo := newRoundEnvWith(t, append([]string{
		"PATH=" + fake + ":/usr/bin:/bin",
		"CLAUDE_CONFIG_DIR=" + filepath.Join(work, "claude"),
		"CODEX_HOME=" + filepath.Join(work, "codex"),
		"GFF_HAB_BIN=" + bin, noRaceExitSleep, "GFF_TEST_ROUND_READY=5s"}, env...)...)
	return e, repo, work
}

// roundPrompt is the prompt of every harness round in these tests.
const roundPrompt = "Triage the open issues."

// deliveredLen is the length of a prompt as session.send delivers it: a bracketed paste, then
// Enter.
var deliveredLen = len("\x1b[200~" + roundPrompt + "\x1b[201~\r")

// addHarnessRound creates a manual harness round that runs the fake in mode.
func addHarnessRound(t *testing.T, c *control.Client, repo, work, name, app, mode string, edit func(*api.RoundSpec)) {
	t.Helper()
	final := "Done: " + mode + "."
	if mode == "keep" {
		final = strings.Repeat("long final message ", 400) // past the 4 KiB the run keeps
	}
	tr := claudeTranscript(final)
	if app == "codex" {
		tr = codexTranscript(final)
	}
	writeFile(t, filepath.Join(work, "transcript-"+app+"-"+mode+".jsonl"), tr)
	spec := api.RoundSpec{Name: name, Mode: api.RoundModeFresh, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		ProjectDir: repo, Harness: app, HarnessArgs: []string{"--fake", mode, "--len", strconv.Itoa(deliveredLen)}, Prompt: roundPrompt}
	if edit != nil {
		edit(&spec)
	}
	call(t, c, "round.create", api.RoundCreateParams{Round: spec}, nil)
}

func runRound(t *testing.T, c *control.Client, name string) {
	t.Helper()
	call(t, c, "round.run", api.RoundRef{Name: name}, nil)
}

// runPane returns the pane of a run's session.
func runPane(t *testing.T, c *control.Client, run api.RoundRun) string {
	t.Helper()
	ss, ok := runSessionOf(structureOf(t, c), run.Session)
	if !ok || ss.Pane == "" {
		t.Fatalf("run %d: no session pane: %+v", run.Number, ss)
	}
	return ss.Pane
}

func paneOpen(t *testing.T, c *control.Client, session string) bool {
	st := structureOf(t, c)
	ss, ok := runSessionOf(st, session)
	return ok && ss.Pane != "" && paneIn(st, ss.Pane) && ss.State == api.SessionStateRunning
}

// TestRoundHarnessEnds: a fresh Claude Code run gets its prompt once idle, as one paste and
// Enter, with the model and harness arguments and a session id gff-hab chose; done ends it
// succeeded with the final message and closes its pane, and the transcript stays; error ends
// it failed with the harness's failure kind and keeps the pane; on_finish keep leaves a
// succeeded run's pane open, and the run keeps only the first 4 KiB of its final message.
func TestRoundHarnessEnds(t *testing.T) {
	e, repo, work := harnessRoundEnv(t)
	c := e.control()
	addHarnessRound(t, c, repo, work, "ok", "claude", "success", func(s *api.RoundSpec) { s.Model = "model-x" })
	addHarnessRound(t, c, repo, work, "bad", "claude", "error", nil)
	addHarnessRound(t, c, repo, work, "kept", "claude", "keep", func(s *api.RoundSpec) { s.OnFinish = api.RoundOnFinishKeep })

	runRound(t, c, "ok")
	ok := waitRun(t, e, c, "ok", api.RoundRunStateSucceeded)
	if ok.Result != "Done: success." {
		t.Errorf("result %q", ok.Result)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "prompt-success")); string(b) != "\x1b[200~"+roundPrompt+"\x1b[201~\r" {
		t.Errorf("prompt as delivered: %q", b)
	}
	// on_finish close: the pane closes and the session record goes (rounds.md 7.6); the run
	// log keeps the result and the transcript stays.
	waitUntil(t, "the succeeded run's session removed", func() bool {
		_, found := runSessionOf(structureOf(t, c), ok.Session)
		return !found
	})
	wantCwd, _ := filepath.EvalSymlinks(repo)
	if b, _ := os.ReadFile(filepath.Join(work, "cwd-success")); strings.TrimSpace(string(b)) != wantCwd {
		t.Errorf("the run started in %q, not the project's folder %q", b, wantCwd)
	}
	sid, _ := os.ReadFile(filepath.Join(work, "sid-success"))
	id := strings.TrimSpace(string(sid))
	if len(id) != 36 {
		t.Fatalf("session id gff-hab chose: %q", sid)
	}
	argv, _ := os.ReadFile(filepath.Join(work, "argv-success"))
	if want := "--fake success --len " + strconv.Itoa(deliveredLen) + " --session-id " + id + " --model model-x"; strings.TrimSpace(string(argv)) != want {
		t.Errorf("argv %q, want %q", argv, want)
	}
	if _, err := os.Stat(filepath.Join(work, "claude", "projects", "-work-app", id+".jsonl")); err != nil {
		t.Errorf("the transcript must stay for a resume: %v", err)
	}
	if got := roundRuns(t, c, "ok")[0]; got.ID != ok.ID || got.Session != ok.Session || got.Result != ok.Result {
		t.Errorf("the run log after the session went: %+v", got)
	}

	runRound(t, c, "bad")
	bad := waitRun(t, e, c, "bad", api.RoundRunStateFailed)
	if bad.Reason != reasonTurnError+": server_error" || bad.Result != "Done: error." {
		t.Errorf("failed run: reason %q, result %q", bad.Reason, bad.Result)
	}
	time.Sleep(300 * time.Millisecond)
	if !paneOpen(t, c, bad.Session) {
		t.Error("a failed run's pane must stay")
	}
	if st := agentStates(t, e.sock)[runPane(t, c, bad)]; st.State != api.AgentStatusError || st.Error != "server_error" {
		t.Errorf("agent state after StopFailure: %+v", st)
	}

	runRound(t, c, "kept")
	kept := waitRun(t, e, c, "kept", api.RoundRunStateSucceeded)
	if len(kept.Result) != rounds.MaxResultBytes || !strings.HasPrefix(kept.Result, "long final message") {
		t.Errorf("kept result: %d bytes", len(kept.Result))
	}
	time.Sleep(300 * time.Millisecond)
	if !paneOpen(t, c, kept.Session) {
		t.Error("on_finish keep closed the pane")
	}
	for _, s := range []string{bad.Session, kept.Session} {
		call(t, c, "pane.close", api.PaneRef{Pane: func() string { ss, _ := runSessionOf(structureOf(t, c), s); return ss.Pane }()}, nil)
	}
}

// TestRoundHarnessPermission: a permission blocks the run, which shows blocked; the answer
// through approval.answer takes nothing over, the run shows running again and ends
// succeeded.
func TestRoundHarnessPermission(t *testing.T) {
	e, repo, work := harnessRoundEnv(t)
	c := e.control()
	addHarnessRound(t, c, repo, work, "asks", "claude", "permission", nil)
	runRound(t, c, "asks")
	run := waitRun(t, e, c, "asks", api.RoundRunStateBlocked)
	pane := runPane(t, c, run)
	st := waitState(t, e.sock, pane, "blocked", func(s api.AgentState) bool { return s.State == api.AgentStatusBlocked && s.Request != nil })
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(20*time.Second))
	defer cancel()
	if err := c.Call(ctx, "approval.answer", api.ApprovalAnswerParams{Pane: pane, Request: st.Request.ID, Choice: api.ApprovalChoiceAllow}, nil); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !fileIs(filepath.Join(work, "key-permission"), "1")() {
		t.Error("the dialog did not get the allow key")
	}
	done := waitRun(t, e, c, "asks", api.RoundRunStateSucceeded)
	if done.Result != "Done: permission." {
		t.Errorf("result %q", done.Result)
	}
}

// TestRoundHarnessTimeout: a working turn past the timeout gets the interrupt key and ends
// timed_out with its pane kept; a run blocked at its timeout ends timed_out with its request
// left open and no key sent.
func TestRoundHarnessTimeout(t *testing.T) {
	e, repo, work := harnessRoundEnv(t)
	c := e.control()
	short := func(s *api.RoundSpec) { s.TimeoutMS = scaleTimeout(3 * time.Second).Milliseconds() }
	addHarnessRound(t, c, repo, work, "slow", "claude", "timeout", short)
	// The stuck run must spawn, start and ask before its timeout, so it gets longer.
	addHarnessRound(t, c, repo, work, "stuck", "claude", "blocked", func(s *api.RoundSpec) {
		s.TimeoutMS = scaleTimeout(8 * time.Second).Milliseconds()
	})

	runRound(t, c, "slow")
	slow := waitRun(t, e, c, "slow", api.RoundRunStateTimedOut)
	waitUntil(t, "the interrupt key", fileIs(filepath.Join(work, "key-timeout"), "\x1b"))
	if !paneOpen(t, c, slow.Session) {
		t.Error("a timed-out run's pane must stay")
	}

	runRound(t, c, "stuck")
	waitRun(t, e, c, "stuck", api.RoundRunStateBlocked)
	stuck := waitRun(t, e, c, "stuck", api.RoundRunStateTimedOut)
	if !strings.Contains(stuck.Reason, "permission request, which stays open") {
		t.Errorf("reason %q", stuck.Reason)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(work, "key-blocked")); err == nil {
		b, _ := os.ReadFile(filepath.Join(work, "key-blocked"))
		t.Errorf("a key reached the blocked run's dialog: %q", b)
	}
	if st := agentStates(t, e.sock)[runPane(t, c, stuck)]; st.State != api.AgentStatusBlocked || st.Request == nil {
		t.Errorf("the request must stay open: %+v", st)
	}
}

// TestRoundHarnessTakeOver: a client's session.send to a live run session ends the run
// taken_over; the session stays the user's.
func TestRoundHarnessTakeOver(t *testing.T) {
	e, repo, work := harnessRoundEnv(t)
	c := e.control()
	addHarnessRound(t, c, repo, work, "mine", "claude", "takeover", nil)
	runRound(t, c, "mine")
	waitUntil(t, "the prompt", func() bool { _, err := os.Stat(filepath.Join(work, "prompt-takeover")); return err == nil })
	run := roundRuns(t, c, "mine")[0]
	pane := runPane(t, c, run)
	// The prompt is in, so the agent's idle before it has passed: working is the turn's, and
	// an idle above its revision is mid-turn.
	turn := waitState(t, e.sock, pane, "working after the prompt", func(s api.AgentState) bool { return s.State == api.AgentStatusWorking })
	writeFile(t, filepath.Join(work, "gate-takeover"), "")
	waitState(t, e.sock, pane, "idle mid-turn", func(s api.AgentState) bool {
		return s.State == api.AgentStatusIdle && s.Revision > turn.Revision
	})
	time.Sleep(300 * time.Millisecond)
	if r := roundRuns(t, c, "mine")[0]; r.State != api.RoundRunStateRunning {
		t.Fatalf("idle mid-turn ended the run: %+v", r)
	}
	call(t, c, "session.send", api.SessionSendParams{Session: run.Session, Text: "x"}, nil)
	if got := waitRun(t, e, c, "mine", api.RoundRunStateTakenOver); got.Reason != reasonSent {
		t.Errorf("reason %q", got.Reason)
	}
	waitUntil(t, "the client's message", func() bool { _, err := os.Stat(filepath.Join(work, "msg-takeover")); return err == nil })
	if !paneOpen(t, c, run.Session) {
		t.Error("a taken-over session must stay")
	}
}

// TestRoundHarnessCodex: a Codex run starts with --no-daemon and its arguments, and ends
// succeeded with its final message read from the rollout through the chat reader.
func TestRoundHarnessCodex(t *testing.T) {
	e, repo, work := harnessRoundEnv(t)
	c := e.control()
	addHarnessRound(t, c, repo, work, "cx", "codex", "success", nil)
	runRound(t, c, "cx")
	got := waitRun(t, e, c, "cx", api.RoundRunStateSucceeded)
	if got.Result != "Done: success." {
		t.Errorf("result %q", got.Result)
	}
	argv, _ := os.ReadFile(filepath.Join(work, "argv-success"))
	if want := "--no-daemon --fake success --len " + strconv.Itoa(deliveredLen); strings.TrimSpace(string(argv)) != want {
		t.Errorf("argv %q, want %q", argv, want)
	}
}

// TestRoundHarnessRefusals: a harness with no adapter, or one this host does not have, is
// refused at creation; a harness that goes away later fails the run "harness not found".
func TestRoundHarnessRefusals(t *testing.T) {
	e, repo, work := harnessRoundEnv(t)
	c := e.control()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	spec := api.RoundSpec{Name: "x", Mode: api.RoundModeFresh, Trigger: api.RoundTrigger{Kind: api.RoundTriggerKindManual},
		ProjectDir: repo, Harness: "pi", Prompt: roundPrompt}
	if err := c.Call(ctx, "round.create", api.RoundCreateParams{Round: spec}, nil); errKind(err) != api.ErrorKindInvalid {
		t.Errorf("a harness with no adapter: %v", err)
	}
	spec.Harness = "opencode"
	if err := c.Call(ctx, "round.create", api.RoundCreateParams{Round: spec}, nil); errKind(err) != api.ErrorKindUnavailable {
		t.Errorf("a harness this host does not have: %v", err)
	}
	addHarnessRound(t, c, repo, work, "gone", "codex", "success", nil)
	if err := os.Remove(filepath.Join(filepath.Dir(lookFake(t, e)), "codex")); err != nil {
		t.Fatal(err)
	}
	runRound(t, c, "gone")
	if got := waitRun(t, e, c, "gone", api.RoundRunStateFailed); !strings.Contains(got.Reason, "harness not found") {
		t.Errorf("reason %q", got.Reason)
	}
}

// lookFake returns the path of the fake claude in e's PATH.
func lookFake(t *testing.T, e *habEnv) string {
	t.Helper()
	for _, kv := range e.env {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			return filepath.Join(strings.Split(p, string(os.PathListSeparator))[0], "claude")
		}
	}
	t.Fatal("no PATH in the hab's environment")
	return ""
}

// TestRoundHarnessTypedAfterDone: a client typing into the pane right after the turn's done,
// while the final message is still being read (the transcript trails the Stop), leaves the run
// succeeded, with the message read once the transcript has it.
func TestRoundHarnessTypedAfterDone(t *testing.T) {
	e, repo, work := harnessRoundEnv(t)
	c := e.control()
	addHarnessRound(t, c, repo, work, "trail", "claude", "trail", nil)
	runRound(t, c, "trail")
	waitUntil(t, "the prompt", func() bool { _, err := os.Stat(filepath.Join(work, "prompt-trail")); return err == nil })
	run := roundRuns(t, c, "trail")[0]
	pane := runPane(t, c, run)
	waitState(t, e.sock, pane, "done", func(s api.AgentState) bool { return s.State == api.AgentStatusDone })
	call(t, c, "pane.input", api.InputParams{Pane: pane, Text: "z"}, nil)
	got := waitRun(t, e, c, "trail", api.RoundRunStateSucceeded)
	if got.Result != "Done: trail." {
		t.Errorf("result %q", got.Result)
	}
}

// TestRoundHarnessNeverReady: a harness that never reaches idle fails at the readiness limit.
func TestRoundHarnessNeverReady(t *testing.T) {
	e, repo, work := harnessRoundEnv(t, "GFF_TEST_ROUND_READY=1s")
	c := e.control()
	addHarnessRound(t, c, repo, work, "mute", "claude", "mute", nil)
	runRound(t, c, "mute")
	if got := waitRun(t, e, c, "mute", api.RoundRunStateFailed); got.Reason != "the harness did not reach idle within 1s" {
		t.Errorf("reason %q", got.Reason)
	}
}

// TestRoundHarnessExits: a harness that exits before its turn ends fails the run with its exit
// status.
func TestRoundHarnessExits(t *testing.T) {
	e, repo, work := harnessRoundEnv(t)
	c := e.control()
	addHarnessRound(t, c, repo, work, "quits", "claude", "exit", nil)
	runRound(t, c, "quits")
	if got := waitRun(t, e, c, "quits", api.RoundRunStateFailed); got.Reason != reasonHarnessExited+" (exit status 3)" {
		t.Errorf("reason %q", got.Reason)
	}
}

// TestRoundHarnessTrust: Claude Code's workspace trust dialog shows the run blocked with a
// reason that names it, and the readiness limit waits. The user's answer in the pane is not a
// take over: the run goes on and succeeds. Unanswered, the run ends timed_out with that reason,
// and no key ever reaches the dialog from the round.
func TestRoundHarnessTrust(t *testing.T) {
	e, repo, work := harnessRoundEnv(t, "GFF_TEST_ROUND_READY=1s")
	c := e.control()
	addHarnessRound(t, c, repo, work, "asks", "claude", "trust", nil)
	addHarnessRound(t, c, repo, work, "waits", "claude", "trustwait", func(s *api.RoundSpec) {
		s.TimeoutMS = scaleTimeout(4 * time.Second).Milliseconds()
	})

	runRound(t, c, "asks")
	run := waitRun(t, e, c, "asks", api.RoundRunStateBlocked)
	if run.Reason != reasonTrust {
		t.Errorf("blocked reason %q", run.Reason)
	}
	time.Sleep(1500 * time.Millisecond) // past the readiness limit
	if r := roundRuns(t, c, "asks")[0]; r.State != api.RoundRunStateBlocked {
		t.Fatalf("the readiness limit ran while the dialog showed: %+v", r)
	}
	call(t, c, "pane.input", api.InputParams{Pane: runPane(t, c, run), Text: "y"}, nil)
	done := waitRun(t, e, c, "asks", api.RoundRunStateSucceeded)
	if done.Result != "Done: trust." {
		t.Errorf("result %q", done.Result)
	}

	runRound(t, c, "waits")
	waitRun(t, e, c, "waits", api.RoundRunStateBlocked)
	late := waitRun(t, e, c, "waits", api.RoundRunStateTimedOut)
	if !strings.Contains(late.Reason, reasonTrust) {
		t.Errorf("timed out reason %q", late.Reason)
	}
	if _, err := os.Stat(filepath.Join(work, "key-trustwait")); err == nil {
		t.Error("a key reached the trust dialog")
	}
	if !paneOpen(t, c, late.Session) {
		t.Error("the timed-out run's pane must stay, its dialog open")
	}
}

// TestRoundHarnessAdopt: harness runs adopted after a gff-hab restart. One whose prompt never
// went in gets it once the agent is idle; one in its turn ends at the turn's end; one whose
// turn ended while gff-hab was down (idle, the transcript holding the answer) ends succeeded
// at once. Claude Code's session id comes back from the program's arguments.
func TestRoundHarnessAdopt(t *testing.T) {
	e, repo, work := harnessRoundEnv(t)
	c := e.control()
	for _, m := range []string{"restart-turn", "restart-end", "restart-start"} {
		addHarnessRound(t, c, repo, work, m, "claude", m, nil)
	}
	for _, m := range []string{"restart-turn", "restart-end"} {
		runRound(t, c, m)
		waitUntil(t, m+": the prompt", func() bool { _, err := os.Stat(filepath.Join(work, "prompt-"+m)); return err == nil })
		pane := runPane(t, c, roundRuns(t, c, m)[0])
		waitState(t, e.sock, pane, m+": working", func(s api.AgentState) bool { return s.State == api.AgentStatusWorking })
	}
	runRound(t, c, "restart-start")
	waitUntil(t, "restart-start: its session", func() bool {
		r := roundRuns(t, c, "restart-start")
		return len(r) > 0 && r[0].Session != ""
	})

	e.stopHab(false)
	writeFile(t, filepath.Join(work, "gate-restart-end"), "")
	sid, _ := os.ReadFile(filepath.Join(work, "sid-restart-end"))
	tr := filepath.Join(work, "claude", "projects", "-work-app", strings.TrimSpace(string(sid))+".jsonl")
	waitUntil(t, "the turn ended while gff-hab was down", func() bool {
		b, _ := os.ReadFile(tr)
		return strings.Count(string(b), "\n") == 3
	})
	time.Sleep(300 * time.Millisecond) // the OSC idle after the transcript
	e.serve()
	c = e.control()

	if got := waitRun(t, e, c, "restart-end", api.RoundRunStateSucceeded); got.Result != "Done: restart-end." {
		t.Errorf("restart-end: result %q", got.Result)
	}
	writeFile(t, filepath.Join(work, "gate-restart-start"), "")
	if got := waitRun(t, e, c, "restart-start", api.RoundRunStateSucceeded); got.Result != "Done: restart-start." {
		t.Errorf("restart-start: result %q", got.Result)
	}
	writeFile(t, filepath.Join(work, "gate-restart-turn"), "")
	if got := waitRun(t, e, c, "restart-turn", api.RoundRunStateSucceeded); got.Result != "Done: restart-turn." {
		t.Errorf("restart-turn: result %q", got.Result)
	}
	for _, m := range []string{"restart-turn", "restart-end", "restart-start"} {
		if runs := roundRuns(t, c, m); len(runs) != 1 {
			t.Errorf("%s: runs %+v", m, runs)
		}
	}
}
