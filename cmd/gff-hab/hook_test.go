package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/hook"
)

// noRaceExitSleep stops a -race build from sleeping one second at exit (its default), which
// would hide the hook's own timing.
const noRaceExitSleep = "GORACE=atexit_sleep_ms=0"

// runHook runs `gff-hab hook` as a harness would: payload on stdin, the given environment.
func runHook(t *testing.T, env []string, payload string, args ...string) (stdout string, d time.Duration, code int) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"hook"}, args...)...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	start := time.Now()
	err := cmd.Run()
	d = time.Since(start)
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), d, code
}

// TestHookNeverBlocks: with gff-hab stopped, with an unknown pane token, and outside any pane,
// `gff-hab hook` exits 0 within 200 ms and prints nothing.
func TestHookNeverBlocks(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "hab.sock")
	payload := `{"session_id":"s1","hook_event_name":"Stop"}`
	token := strings.Repeat("ab", 16)
	check := func(name string, env []string) {
		t.Helper()
		out, d, code := runHook(t, env, payload, "claude", "Stop")
		if code != 0 || out != "" || d > 200*time.Millisecond {
			t.Errorf("%s: exit %d after %v, stdout %q", name, code, d, out)
		}
	}
	base := []string{"PATH=" + os.Getenv("PATH"), noRaceExitSleep}
	// The first exec of a fresh binary can be slow (macOS checks it once); do not time that.
	runHook(t, base, payload, "claude", "Stop")

	check("gff-hab stopped", append(base, hook.Env+"="+hook.Addr(filepath.Join(dir, "hook.sock"), token)))
	check("outside any pane", base)
	check("malformed address", append(base, hook.Env+"=nonsense"))

	startServe(t, sock, filepath.Join(dir, "state"))
	check("unknown token", append(base, hook.Env+"="+hook.Addr(filepath.Join(dir, "hook.sock"), token)))
	if _, err := os.Stat(filepath.Join(dir, "hook.sock")); err != nil {
		t.Fatalf("serve made no hook socket beside the control socket: %v", err)
	}
}

// TestHookStdinNeverCloses: a harness that never closes stdin cannot hold the hook past its
// budget.
func TestHookStdinNeverCloses(t *testing.T) {
	cmd := exec.Command(bin, "hook", "claude", "Stop")
	cmd.Env = []string{noRaceExitSleep, hook.Env + "=" + hook.Addr("/nonexistent/hook.sock", strings.Repeat("ab", 16))}
	w, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("exit: %v", err)
	}
	if d := time.Since(start); d > hook.Budget+500*time.Millisecond {
		t.Fatalf("hook ran %v with an open stdin", d)
	}
}

func TestHookPrint(t *testing.T) {
	out := run(t, "hook", "-print", "claude")
	var cfg map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("hook -print claude is not JSON: %v\n%s", err, out)
	}
	for _, ev := range hook.ClaudeEvents {
		if _, ok := cfg["hooks"][ev]; !ok {
			t.Errorf("no %s hook", ev)
		}
	}
	if err := exec.Command(bin, "hook", "-print", "nonesuch").Run(); err == nil {
		t.Error("-print of an unknown harness exited 0")
	}
}
