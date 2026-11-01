package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/hook"
)

// waitUntil polls cond until it holds, within a bound $GFF_TEST_TIMEOUT_SCALE multiplies.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(scaleTimeout(15 * time.Second))
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startHolder runs the holder binary in its own session, as an init system would, and waits for
// its registry file.
func startHolder(t *testing.T, dir, id string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(holderBin, "-dir", dir, "-instance", id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(scaleTimeout(10 * time.Second)):
			cmd.Process.Kill()
			<-done
		}
	})
	waitUntil(t, "holder "+id+" registry file", func() bool {
		_, err := os.Stat(holderreg.Path(dir, id))
		return err == nil
	})
	return cmd
}

// agentStates reads the agent state of every pane through the control connection.
func agentStates(t *testing.T, sock string) map[string]api.AgentState {
	t.Helper()
	var res api.StructureResult
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "structure.get")), &res); err != nil {
		t.Fatal(err)
	}
	out := map[string]api.AgentState{}
	for _, a := range res.Agents {
		out[a.Pane] = a
	}
	return out
}

// paneScript is the program in each test pane. It saves its GFF_HAB, then for each step n waits
// for the file go-$NAME-n and runs the real hook command with payload-$NAME-n, as a harness's
// hook would, and marks done-$NAME-n. After end-$NAME it exits.
const paneScript = `
printf '%s' "$GFF_HAB" > "$DIR/addr-$NAME.tmp" && mv "$DIR/addr-$NAME.tmp" "$DIR/addr-$NAME"
n=1
while :; do
  while [ ! -e "$DIR/go-$NAME-$n" ] && [ ! -e "$DIR/end-$NAME" ]; do sleep 0.02; done
  [ -e "$DIR/end-$NAME" ] && exit 0
  "$HAB" hook claude < "$DIR/payload-$NAME-$n"
  touch "$DIR/done-$NAME-$n"
  n=$((n+1))
done
`

// createSession calls session.create through the CLI and returns the new session and pane ids.
func createSession(t *testing.T, sock string, p api.SessionCreateParams) api.SessionCreated {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var res api.SessionCreated
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "session.create", string(b))), &res); err != nil {
		t.Fatal(err)
	}
	if res.Session == "" || res.Pane == "" {
		t.Fatalf("session.create: %+v", res)
	}
	return res
}

// TestSpawnHookTokens is the spawn path end to end, with the real holder and gff-hab binaries:
// session.create over the control connection spawns two panes with GFF_HAB in their
// environment and files them as sessions; a hook run inside a pane changes
// that pane's agent state; forged and spliced tokens and the token of a pane gff-hab does not
// follow are refused; after a gff-hab restart the same pane's token still lands; and once the
// panes end and their holder drains, gff-hab releases them, the holder exits, and their tokens
// are refused.
func TestSpawnHookTokens(t *testing.T) {
	d := shortDir(t)
	hdir, state, work := filepath.Join(d, "h"), filepath.Join(d, "state"), filepath.Join(d, "w")
	for _, dir := range []string{hdir, work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(d, "run", "hab.sock")
	one := startHolder(t, hdir, "one")

	paneEnv := func(name string) []string {
		return []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + work, "HAB=" + bin, "NAME=" + name, noRaceExitSleep}
	}
	srv := startServeWith(t, sock, state, nil, "-holders", hdir)
	if w := welcome(t, sock); !slices.Contains(w.Capabilities, "agent-state") {
		t.Errorf("welcome capabilities %v lack agent-state", w.Capabilities)
	}
	created := map[string]api.SessionCreated{}
	for _, name := range []string{"a", "b"} {
		created[name] = createSession(t, sock, api.SessionCreateParams{
			Name: "pane-" + name, Argv: []string{"/bin/sh", "-c", paneScript}, Env: paneEnv(name), Cwd: "/"})
	}
	idA, idB := created["a"].Pane, created["b"].Pane
	uuidA := strings.ReplaceAll(idA, "-", "")
	var st api.StructureResult
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "structure.get")), &st); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		i := slices.IndexFunc(st.Sessions, func(s api.Session) bool { return s.ID == created[name].Session })
		if i < 0 || st.Sessions[i].Name != "pane-"+name || st.Sessions[i].Pane != created[name].Pane || st.Sessions[i].State != api.SessionStateRunning {
			t.Errorf("session %s in the structure: %+v", name, st.Sessions)
		}
	}

	addr := func(name string) string {
		t.Helper()
		var b []byte
		waitUntil(t, "pane "+name+"'s GFF_HAB", func() bool {
			var err error
			b, err = os.ReadFile(filepath.Join(work, "addr-"+name))
			return err == nil
		})
		return string(b)
	}
	addrA, addrB := addr("a"), addr("b")
	hookSock, tokA, err := hook.ParseAddr(addrA)
	if err != nil || hookSock != filepath.Join(d, "run", "hook.sock") {
		t.Fatalf("pane a's GFF_HAB %q: socket %q, %v", addrA, hookSock, err)
	}
	_, tokB, _ := hook.ParseAddr(addrB)
	if tokA == tokB {
		t.Fatal("two panes share a token")
	}

	// step runs the hook command inside pane name with payload and waits until it finished.
	step := func(name string, n int, payload string) {
		t.Helper()
		suffix := name + "-" + string(rune('0'+n))
		if err := os.WriteFile(filepath.Join(work, "payload-"+suffix), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(work, "go-"+suffix), nil, 0o600)
		waitUntil(t, "pane "+name+"'s hook "+suffix, func() bool {
			_, err := os.Stat(filepath.Join(work, "done-"+suffix))
			return err == nil
		})
	}
	stateOf := func(id string) api.AgentStatus { return agentStates(t, sock)[id].State }

	step("a", 1, `{"session_id":"sa","hook_event_name":"SessionStart","source":"startup"}`)
	waitUntil(t, "pane a idle", func() bool { return stateOf(idA) == api.AgentStatusIdle })
	if _, ok := agentStates(t, sock)[idB]; ok {
		t.Error("pane a's hook changed pane b")
	}
	step("b", 1, `{"session_id":"sb","hook_event_name":"UserPromptSubmit","prompt_id":"p1"}`)
	waitUntil(t, "pane b working", func() bool { return stateOf(idB) == api.AgentStatusWorking })
	if got := stateOf(idA); got != api.AgentStatusIdle {
		t.Errorf("pane b's hook changed pane a to %s", got)
	}

	// Refused tokens: each answers 404 and changes nothing.
	secret, err := os.ReadFile(filepath.Join(state, hook.KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	var unknown [16]byte
	copy(unknown[:], must(hex.DecodeString("fd0000000000400080000000000000c3")))
	refused := func(name, token string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		env := harness.Envelope{Harness: "claude", Event: "Stop", N: 1, Payload: json.RawMessage(`{"session_id":"sa","hook_event_name":"Stop"}`)}
		if err := hook.Post(ctx, hook.Addr(hookSock, token), env); err == nil || !strings.Contains(err.Error(), "404") {
			t.Errorf("%s token: %v, want 404", name, err)
		}
	}
	refused("forged", strings.Repeat("ab", len(tokA)/2))
	refused("spliced (b's uuid, a's hash)", tokB[:32]+tokA[32:])
	refused("another hab's", hook.NewTokens(make([]byte, 32), nil).Token([16]byte(must(hex.DecodeString(uuidA)))))
	refused("unknown pane", hook.NewTokens(secret, nil).Token(unknown))
	if got := stateOf(idA); got != api.AgentStatusIdle {
		t.Errorf("a refused post changed pane a to %s", got)
	}

	// A gff-hab restart: the same pane's token still lands.
	srv.stop(t)
	srv = startServeWith(t, sock, state, nil, "-holders", hdir)
	step("a", 2, `{"session_id":"sa","hook_event_name":"UserPromptSubmit","prompt_id":"p2"}`)
	waitUntil(t, "pane a working after the restart", func() bool { return stateOf(idA) == api.AgentStatusWorking })

	// The panes end; a newer holder makes gff-hab drain the old one, release both ended panes, and
	// the old holder exits. Their tokens are refused after that.
	for _, n := range []string{"a", "b"} {
		os.WriteFile(filepath.Join(work, "end-"+n), nil, 0o600)
	}
	startHolder(t, hdir, "two")
	waitUntil(t, "the drained holder to exit", func() bool {
		return errors.Is(syscall.Kill(one.Process.Pid, 0), syscall.ESRCH)
	})
	if _, err := os.Stat(holderreg.Path(hdir, "one")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the drained holder's registry file: %v", err)
	}
	waitUntil(t, "the released panes' agent state dropped", func() bool { return len(agentStates(t, sock)) == 0 })
	refused("released pane a's", tokA)
	refused("released pane b's", tokB)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestNewHookKeyWarning: a hook key made while live panes exist is logged loudly with the count;
// a key made with no panes, or an existing key, logs nothing.
func TestNewHookKeyWarning(t *testing.T) {
	if m := newHookKeyWarning(true, 3); !strings.Contains(m, "WARNING") || !strings.Contains(m, "3 live pane(s)") {
		t.Errorf("new key with panes: %q", m)
	}
	if m := newHookKeyWarning(true, 0); m != "" {
		t.Errorf("new key, no panes: %q", m)
	}
	if m := newHookKeyWarning(false, 3); m != "" {
		t.Errorf("existing key: %q", m)
	}
}

// callErr calls method through the CLI, which must fail, and returns the error kind it printed.
func callErr(t *testing.T, sock, method string, params any) string {
	t.Helper()
	b, _ := json.Marshal(params)
	out, err := exec.Command(bin, "api", "call", "-socket", sock, method, string(b)).CombinedOutput()
	if err == nil {
		t.Fatalf("%s %s succeeded: %s", method, b, out)
	}
	for _, kind := range api.Enums["ErrorKind"] {
		if strings.Contains(string(out), "("+kind+")") {
			return kind
		}
	}
	return string(out)
}

// TestSessionCreateAndClose: session.create files a session in the project of a git directory
// and refuses a taken name; pane.close hangs up a running program and releases an ended pane on
// the current holder. Both panes leave the structure and their sessions are exited.
func TestSessionCreateAndClose(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	d := shortDir(t)
	hdir, state, work, repo := filepath.Join(d, "h"), filepath.Join(d, "state"), filepath.Join(d, "w"), filepath.Join(d, "repo")
	for _, dir := range []string{hdir, work, repo} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://example.com/team/thing.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	sock := filepath.Join(d, "run", "hab.sock")
	startHolder(t, hdir, "one")
	startServeWith(t, sock, state, nil, "-holders", hdir)

	pidFile := filepath.Join(work, "pid")
	running := createSession(t, sock, api.SessionCreateParams{
		Name:       "loop",
		ProjectDir: repo,
		Argv:       []string{"/bin/sh", "-c", `echo $$ > "$PIDF.tmp" && mv "$PIDF.tmp" "$PIDF"; while :; do sleep 0.05; done`},
		Env:        []string{"PIDF=" + pidFile, noRaceExitSleep},
	})
	ended := createSession(t, sock, api.SessionCreateParams{Argv: []string{"/bin/sh", "-c", "exit 0"}, Env: []string{noRaceExitSleep}})
	// A create retried with its idempotency key, here over a second connection, starts nothing.
	keyed := api.SessionCreateParams{Argv: []string{"/bin/sh", "-c", "exit 0"}, Env: []string{noRaceExitSleep}, IdempotencyKey: "retry-1"}
	if first, again := createSession(t, sock, keyed), createSession(t, sock, keyed); first.Session != again.Session || first.Pane != again.Pane {
		t.Errorf("a retried create: %+v, then %+v", first, again)
	}
	if kind := callErr(t, sock, "session.create", api.SessionCreateParams{Name: "loop", Argv: []string{"/bin/true"}}); kind != "conflict" {
		t.Errorf("a taken name: %s, want conflict", kind)
	}
	if kind := callErr(t, sock, "session.create", api.SessionCreateParams{Harness: "claude"}); kind != "unavailable" {
		t.Errorf("a harness: %s, want unavailable", kind)
	}
	if kind := callErr(t, sock, "pane.close", api.PaneRef{Pane: "fd000000-0000-4000-8000-0000000000ff"}); kind != "not_found" {
		t.Errorf("an unknown pane: %s, want not_found", kind)
	}

	structureNow := func() api.StructureResult {
		var st api.StructureResult
		if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "structure.get")), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	session := func(st api.StructureResult, id string) api.Session {
		i := slices.IndexFunc(st.Sessions, func(s api.Session) bool { return s.ID == id })
		if i < 0 {
			t.Fatalf("no session %s in %+v", id, st.Sessions)
		}
		return st.Sessions[i]
	}
	hasPane := func(st api.StructureResult, id string) bool {
		return slices.ContainsFunc(st.Panes, func(p api.Pane) bool { return p.ID == id })
	}
	st := structureNow()
	if len(st.Projects) != 1 || st.Projects[0].Name != "repo" || st.Projects[0].Remote != "example.com/team/thing" ||
		!slices.Equal(st.Projects[0].Sessions, []string{running.Session}) {
		t.Errorf("projects: %+v", st.Projects)
	}
	// The new project's repository state follows: an empty repository has a branch and no head.
	waitUntil(t, "the project's branch", func() bool {
		ps := structureNow().Projects
		return len(ps) == 1 && ps[0].Branch != "" && ps[0].Head == "" && ps[0].Ahead == nil
	})
	if len(st.Sessions) != 3 {
		t.Errorf("%d sessions, want 3: %+v", len(st.Sessions), st.Sessions)
	}
	if s := session(st, ended.Session); s.Name != "sh" || s.Project != "" {
		t.Errorf("the default-named session: %+v", s)
	}

	var pid int
	waitUntil(t, "the pid file", func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		_, err = fmt.Sscan(string(b), &pid)
		return err == nil
	})
	waitUntil(t, "the short program exited", func() bool { return session(structureNow(), ended.Session).State == api.SessionStateExited })
	for _, c := range []api.SessionCreated{running, ended} {
		run(t, "api", "call", "-socket", sock, "pane.close", `{"pane":"`+c.Pane+`"}`)
	}
	waitUntil(t, "the hung-up program ended", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	st = structureNow()
	for _, c := range []api.SessionCreated{running, ended} {
		if hasPane(st, c.Pane) {
			t.Errorf("pane %s still listed after pane.close", c.Pane)
		}
		if s := session(st, c.Session); s.State != api.SessionStateExited {
			t.Errorf("session %s after pane.close: %+v", c.Session, s)
		}
	}

	// session.rename and session.close work on the records; a taken name is a conflict.
	if kind := callErr(t, sock, "session.rename", api.SessionRenameParams{Session: ended.Session, Name: "loop"}); kind != "conflict" {
		t.Errorf("rename to a taken name: %s, want conflict", kind)
	}
	if kind := callErr(t, sock, "session.rename", api.SessionRenameParams{Session: "nope", Name: "x"}); kind != "not_found" {
		t.Errorf("rename an unknown session: %s, want not_found", kind)
	}
	run(t, "api", "call", "-socket", sock, "session.rename", `{"session":"`+ended.Session+`","name":"renamed"}`)
	if s := session(structureNow(), ended.Session); s.Name != "renamed" {
		t.Errorf("after rename: %+v", s)
	}
	run(t, "api", "call", "-socket", sock, "session.close", `{"session":"`+ended.Session+`"}`)
	if slices.ContainsFunc(structureNow().Sessions, func(s api.Session) bool { return s.ID == ended.Session }) {
		t.Error("session still listed after session.close")
	}
	if kind := callErr(t, sock, "session.close", api.SessionRef{Session: ended.Session}); kind != "not_found" {
		t.Errorf("close a closed session: %s, want not_found", kind)
	}
}

// TestHarnessStart: session.create with harness codex starts `codex --no-daemon` (owner
// decision: Codex runs without the shared daemon in the holder), keeps the caller's arguments,
// adds the flag only once, and sets AGENT_KIND. Other harnesses are refused.
func TestHarnessStart(t *testing.T) {
	for _, c := range []struct{ argv, want []string }{
		{nil, []string{"codex", "--no-daemon"}},
		{[]string{"/opt/bin/codex", "resume", "abc"}, []string{"/opt/bin/codex", "--no-daemon", "resume", "abc"}},
		{[]string{"codex", "resume", "--no-daemon"}, []string{"codex", "resume", "--no-daemon"}},
	} {
		in := api.SessionCreateParams{Harness: "codex", Argv: c.argv, Env: []string{"A=1"}}
		p, err := harnessStart(in)
		if err != nil || !slices.Equal(p.Argv, c.want) || !slices.Equal(p.Env, []string{"A=1", "AGENT_KIND=codex"}) {
			t.Errorf("%q: %q %q %v", c.argv, p.Argv, p.Env, err)
		}
		if len(c.argv) > 0 && !slices.Equal(in.Argv, c.argv) {
			t.Errorf("the caller's argv changed: %q", in.Argv)
		}
	}
	if _, err := harnessStart(api.SessionCreateParams{Harness: "claude"}); errKind(err) != api.ErrorKindUnavailable {
		t.Errorf("claude: %v", err)
	}
}
