package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// runErr runs gff-hab and returns its output, stdout and stderr, and its error.
func runErr(args ...string) (string, error) {
	out, err := exec.Command(bin, args...).CombinedOutput()
	return string(out), err
}

// stackDumps lists the stack dumps in the state's crash folder.
func stackDumps(t *testing.T, state string) []string {
	t.Helper()
	var out []string
	for _, n := range dumpsOf(filepath.Join(state, crashDir), "hab-") {
		if strings.Contains(n, stacksInfix) {
			out = append(out, n)
		}
	}
	return out
}

// TestStallWatchdog is diagnostics test 7, first half: a test seam holds the structure store's
// lock past the threshold, and gff-hab logs one "loop stalled" line and writes one stack file,
// however long the stall lasts; it logs one "loop recovered" line when the store answers
// (diagnostics.md 4.7, 14 stage 2).
func TestStallWatchdog(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	sock, state := filepath.Join(dir, "hab.sock"), filepath.Join(dir, "state")
	startServeWith(t, sock, state, []string{"GFF_TEST_HAB_STALL=4s", "GFF_TEST_STALL_EVERY=100ms", "GFF_TEST_STALL_AFTER=1s"})
	waitUntil(t, "the recovered line", func() bool {
		return strings.Contains(habLog(t, state), "loop recovered: structure answered after ")
	})
	log := habLog(t, state)
	if n := strings.Count(log, "loop stalled: "); n != 1 {
		t.Fatalf("%d stall lines, want 1:\n%s", n, log)
	}
	if !regexp.MustCompile(`loop stalled: structure has not answered for \d+s; stacks in crash/hab-`).MatchString(log) {
		t.Fatalf("stall line:\n%s", log)
	}
	dumps := stackDumps(t, state)
	if len(dumps) != 1 || !strings.Contains(dumps[0], stacksInfix) {
		t.Fatalf("stack files: %v", dumps)
	}
	b, err := os.ReadFile(filepath.Join(state, crashDir, dumps[0]))
	if err != nil || !strings.Contains(string(b), "HoldForTest") {
		t.Fatalf("the dump lacks the stuck goroutine: %v\n%.2000s", err, b)
	}
	if st, err := os.Stat(filepath.Join(state, crashDir, dumps[0])); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("dump mode: %v %v", st.Mode(), err)
	}
	// The dump stays out of the log: the log names the file and holds no goroutine trace.
	if strings.Contains(log, "goroutine ") {
		t.Fatalf("a stack reached hab.log:\n%s", log)
	}
}

// TestStacksSignal is diagnostics test 7, second half: gff-hab stacks sends SIGUSR1, and
// gff-hab writes a dump while it keeps serving; the command prints the dump's path (4.7).
func TestStacksSignal(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	sock, state := filepath.Join(dir, "hab.sock"), filepath.Join(dir, "state")
	startServe(t, sock, state)
	if m := readMarker(t, state); !m.Stacks {
		t.Fatalf("the marker does not say gff-hab catches SIGUSR1: %+v", m)
	}
	out := strings.TrimSpace(run(t, "stacks", "-state", state))
	b, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(b), "goroutine ") || !strings.Contains(string(b), "stacksOnSignal") {
		t.Fatalf("gff-hab stacks printed %q: %v", out, err)
	}
	waitUntil(t, "the stacks line", func() bool {
		return strings.Contains(habLog(t, state), "stacks: written to crash/"+filepath.Base(out)+" on SIGUSR1")
	})
	if !answers(sock) {
		t.Fatal("gff-hab stopped answering after SIGUSR1")
	}
	// A second dump is a second file.
	if out2 := strings.TrimSpace(run(t, "stacks", "-state", state)); out2 == out {
		t.Fatalf("a second dump reused %s", out)
	}
}

// TestStacksRefusesOldHab: a marker without the stacks flag names a gff-hab that a SIGUSR1
// would end, so gff-hab stacks sends none.
func TestStacksRefusesOldHab(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	sock, state := filepath.Join(dir, "hab.sock"), filepath.Join(dir, "state")
	s := startServe(t, sock, state)
	m := readMarker(t, state)
	m.Stacks = false
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(state, runDir, habMarkerFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runErr("stacks", "-state", state)
	if err == nil || !strings.Contains(out, "predates stack dumps") {
		t.Fatalf("gff-hab stacks against an old gff-hab: %v\n%s", err, out)
	}
	if !answers(s.sock) {
		t.Fatal("gff-hab stopped")
	}
}

// TestDebugAcrossCrash is the debug half of stage 2's acceptance: a window opened by gff-hab
// debug is still on after a crash and a restart, status shows it, and debug off closes it
// (diagnostics.md 4.5, 12).
func TestDebugAcrossCrash(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	sock, state := filepath.Join(dir, "hab.sock"), filepath.Join(dir, "state")
	s := startServe(t, sock, state)
	if out := run(t, "status", "-state", state, "-socket", sock); !strings.Contains(out, "debug    off") {
		t.Fatalf("status before debug:\n%s", out)
	}
	if out := run(t, "debug", "-socket", sock, "10m"); !strings.HasPrefix(out, "debug on until ") {
		t.Fatalf("gff-hab debug 10m: %q", out)
	}
	if out := run(t, "status", "-state", state, "-socket", sock); !strings.Contains(out, "debug    on until ") {
		t.Fatalf("status after debug 10m:\n%s", out)
	}
	s.stop(t)

	panicServe(t, sock, state) // a crash at start: the window must survive it
	s = startServe(t, sock, state)
	if rs := readHealth(t, state); len(rs) == 0 || rs[len(rs)-1].Kind != "crash" {
		t.Fatalf("no crash record: %+v", rs)
	}
	if out := run(t, "status", "-state", state, "-socket", sock); !strings.Contains(out, "debug    on until ") {
		t.Fatalf("status after a crash and a restart:\n%s", out)
	}
	waitUntil(t, "the start's debug line", func() bool {
		return strings.Contains(habLog(t, state), "a window opened before this start")
	})
	if out := run(t, "debug", "-socket", sock, "off"); out != "debug off\n" {
		t.Fatalf("gff-hab debug off: %q", out)
	}
	if out := run(t, "status", "-state", state, "-socket", sock); !strings.Contains(out, "debug    off") {
		t.Fatalf("status after debug off:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, runDir, debugFile)); !os.IsNotExist(err) {
		t.Fatalf("debug.json after debug off: %v", err)
	}
	if out, err := runErr("debug", "-socket", sock, "25h"); err == nil {
		t.Fatalf("gff-hab debug 25h worked: %s", out)
	}
}

// An expired window is ignored at start and its file removed (12); an open one turns debug on
// and ends by itself.
func TestDebugWindow(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, runDir, debugFile)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"until":"2020-01-01T00:00:00Z"}`), 0o600)
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, f) }
	var marks []bool
	changed := func(on bool, _ time.Time) { marks = append(marks, on) }
	d := openDebug(state, false, logf, time.Now, changed)
	if d.on.Load() {
		t.Fatal("an expired window turned debug on")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the expired window's file stays: %v", err)
	}
	d.printf("hidden")
	if len(lines) != 0 {
		t.Fatalf("lines with debug off: %q", lines)
	}
	d.Close()

	// A window still open at start.
	b, _ := json.Marshal(debugRecord{Until: time.Now().Add(300 * time.Millisecond)})
	os.WriteFile(path, b, 0o600)
	ended := make(chan struct{})
	d = openDebug(state, false, func(f string, a ...any) {
		if f == "debug off: the window ended" {
			close(ended)
		}
	}, time.Now, func(bool, time.Time) {})
	defer d.Close()
	if !d.on.Load() {
		t.Fatal("an open window did not turn debug on")
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the window did not end")
	}
	if d.on.Load() {
		t.Fatal("debug still on after the window ended")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the ended window's file stays: %v", err)
	}
	if len(marks) != 1 || marks[0] {
		t.Fatalf("marker updates: %v", marks)
	}
}
