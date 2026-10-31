package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// TestSpawnHookTokens is the spawn path end to end, with the real holder and gff-hab binaries:
// gff-hab spawns two panes with GFF_HAB in their environment; a hook run inside a pane changes
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

	const uuidA, uuidB = "fd0000000000400080000000000000a1", "fd0000000000400080000000000000b2"
	idA, idB := "fd000000-0000-4000-8000-0000000000a1", "fd000000-0000-4000-8000-0000000000b2"
	paneEnv := func(name string) []string {
		return []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + work, "HAB=" + bin, "NAME=" + name, noRaceExitSleep}
	}
	type testSpawn struct { // as spawn_gfftest.go reads it
		UUID string   `json:"uuid"`
		Argv []string `json:"argv"`
		Env  []string `json:"env"`
	}
	spawns, _ := json.Marshal([]testSpawn{
		{UUID: uuidA, Argv: []string{"/bin/sh", "-c", paneScript}, Env: paneEnv("a")},
		{UUID: uuidB, Argv: []string{"/bin/sh", "-c", paneScript}, Env: paneEnv("b")},
	})
	srv := startServeWith(t, sock, state, []string{"GFF_HAB_TEST_SPAWN=" + string(spawns)}, "-holders", hdir)
	if w := welcome(t, sock); !slices.Contains(w.Capabilities, "agent-state") {
		t.Errorf("welcome capabilities %v lack agent-state", w.Capabilities)
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
