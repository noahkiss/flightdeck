package modhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary is its own module host: run with GFF_MODHOST_TEST_CHILD=1 it serves the pipe
// on stdin and stdout. With GFF_MODHOST_VIA_PROCESS=1, newModule and newRunner load every module
// through a Host, so the in-process tests run against the process boundary
// (TestSuiteThroughProcess).
func TestMain(m *testing.M) {
	if os.Getenv("GFF_MODHOST_TEST_CHILD") == "1" {
		childMaxStack = 16 << 20 // TestHostCrashRestarts's recursion overflows it quickly
		if err := ServeChild(os.Stdin, os.Stdout); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var viaProcess = os.Getenv("GFF_MODHOST_VIA_PROCESS") == "1"

// childCommand runs this test binary as the module host.
func childCommand() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "GFF_MODHOST_TEST_CHILD=1", "GFF_MODHOST_VIA_PROCESS=")
	return cmd
}

func testHost(t *testing.T, o HostOptions) *Host {
	t.Helper()
	if o.Command == nil {
		o.Command = childCommand
	}
	if o.Log == nil {
		o.Log = func(l string) { t.Log(l) }
	}
	h := NewHost(o)
	t.Cleanup(func() { h.Close() })
	return h
}

// newRunner loads a module in process, or in its own module host under GFF_MODHOST_VIA_PROCESS.
func newRunner(t *testing.T, b *Bundle, opts Options) (Runner, error) {
	t.Helper()
	if !viaProcess {
		return New(b, opts)
	}
	r, err := testHost(t, HostOptions{}).New(b, opts)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func skipViaProcess(t *testing.T, why string) {
	if viaProcess {
		t.Skip("in process only: " + why)
	}
}

// TestSuiteThroughProcess runs the package's module tests again with every module in a module
// host process (modules.md 7.4): the boundary keeps each result and typed error.
func TestSuiteThroughProcess(t *testing.T) {
	if viaProcess {
		t.Skip("already through the process")
	}
	if testing.Short() {
		t.Skip("-short")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^Test(HelloGolden|LoadRefuses|Timeouts|CallerCancel|ForbiddenCalls|BadTrees|MemoryLimit|StackDepth|PowersReachBackends|BridgeArguments|MultiFileAndAsync|StateLimitAndStore|NoHostAPI|HostileValues|RegexTimeout|StuckCall|MemoryHogBlamed|HeapPressure|NoValueAmplification|BackendPanic|HabReadList|StateSavedOnce|LogCap|ExecTimeoutUnits|RequireBody|NoSourceMapRead|LoadOversizedFile)$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GFF_MODHOST_VIA_PROCESS=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the tests through the process failed: %v\n%s", err, lastLines(string(out), 60))
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("no PASS:\n%s", lastLines(string(out), 20))
	}
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

const crashJS = `
exports.render = function () { return { type: "text", text: "up" }; };
exports.act = function (name) {
  if (name === "crash") {
    // Native recursion: join of an array nested deep overflows the goroutine stack, a fatal
    // error no recover catches (modules.md 8.4).
    var a = []; for (var i = 0; i < 100000; i++) { a = [a]; }
    a.join(",");
  }
};
`

// TestHostCrashRestarts: a module that crashes the module host fails its call with a culprit
// HostError, gff-hab's side lives on, a call during the backoff gets a HostError at once, and
// after the restart the module answers again.
func TestHostCrashRestarts(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{BackoffMin: 300 * time.Millisecond})
	r, err := h.New(writeBundle(t, "crasher", "", crashJS), Options{Limits: Limits{Act: 20 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.New(writeBundle(t, "bystander", "", `exports.render = function () { return { type: "text", text: "b" }; };`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	pid := h.Pid()
	ctx := context.Background()
	err = r.Act(ctx, "crash", nil, fixedCall)
	var he *HostError
	if !errors.As(err, &he) || !he.Culprit || !errors.Is(err, ErrHostDown) {
		t.Fatalf("crash: err = %v, want a culprit HostError", err)
	}
	if !strings.Contains(he.Reason, "exited") {
		t.Errorf("reason = %q", he.Reason)
	}
	// During the backoff: at once, not a culprit, and it says when it starts again.
	start := time.Now()
	_, err = other.Render(ctx, fixedCall)
	if !errors.As(err, &he) || he.Culprit || he.RestartAt.IsZero() {
		t.Fatalf("during the backoff: err = %v, want a HostError with a restart time", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("a call during the backoff waited %s", time.Since(start))
	}
	waitFor(t, 5*time.Second, func() bool { return h.Pid() != 0 })
	if h.Pid() == pid {
		t.Fatal("same pid after the crash")
	}
	for _, m := range []*Remote{r, other} {
		if _, err := m.Render(ctx, fixedCall); err != nil {
			t.Errorf("%s after the restart: %v", m.Manifest().ID, err)
		}
	}
}

func waitFor(t *testing.T, d time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !f() {
		if time.Now().After(end) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHostWedgeKilled: a child that stops answering (stopped with SIGSTOP, so neither the
// runtime's interrupt nor its stuck grace can help) is killed past the call's limit plus the
// grace; the call is the culprit, another module's call in flight is not, and both answer after
// the restart.
func TestHostWedgeKilled(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{WedgeGrace: 300 * time.Millisecond, BackoffMin: 50 * time.Millisecond, PingTimeout: time.Minute})
	slow := `exports.render = function () { return { type: "text", text: "s" }; };
exports.refresh = function () { fd.exec(["x"]); };`
	release := make(chan struct{})
	defer close(release)
	r, err := h.New(writeBundle(t, "wedge", `"exec"`, slow), Options{Backends: Backends{Exec: &blockingExec{release: release}}, Limits: Limits{Refresh: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	w, err := h.New(writeBundle(t, "wedger", "", `exports.render = function () { return { type: "text", text: "w" }; };`), Options{Limits: Limits{Render: 200 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	refreshErr := make(chan error, 1)
	go func() { refreshErr <- r.Refresh(ctx, fixedCall) }()
	time.Sleep(100 * time.Millisecond)
	pid := h.Pid()
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = w.Render(ctx, fixedCall)
	var he *HostError
	if !errors.As(err, &he) || !he.Culprit || !strings.Contains(he.Reason, "did not answer") {
		t.Fatalf("wedged render: err = %v, want a culprit HostError", err)
	}
	if d := time.Since(start); d < 700*time.Millisecond || d > 3*time.Second {
		t.Errorf("killed after %s, want the limit plus both graces", d)
	}
	select {
	case err := <-refreshErr:
		if !errors.As(err, &he) || he.Culprit {
			t.Errorf("bystander refresh: err = %v, want a HostError that is not a culprit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bystander refresh never returned")
	}
	waitFor(t, 5*time.Second, func() bool { p := h.Pid(); return p != 0 && p != pid })
	if _, err := w.Render(ctx, fixedCall); err != nil {
		t.Errorf("after the restart: %v", err)
	}
}

// TestHostPingTimeout: a child that answers no ping is killed even with no call running.
func TestHostPingTimeout(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	var mu sync.Mutex
	var logs []string
	h := testHost(t, HostOptions{PingEvery: 50 * time.Millisecond, PingTimeout: 300 * time.Millisecond, BackoffMin: time.Minute,
		Log: func(l string) { mu.Lock(); logs = append(logs, l); mu.Unlock() }})
	if _, err := h.New(writeBundle(t, "idle", "", `exports.render = function () { return { type: "text", text: "i" }; };`), Options{}); err != nil {
		t.Fatal(err)
	}
	pid := h.Pid()
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return h.Pid() == 0 })
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(logs, "\n"), "answered no ping") {
		t.Errorf("logs = %q", logs)
	}
}

// TestHostMemoryKill: a child over MaxMemory is killed, and the call that ran is the culprit.
func TestHostMemoryKill(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{MaxMemory: 64 << 20, PingEvery: 20 * time.Millisecond, BackoffMin: 50 * time.Millisecond})
	r, err := h.New(load(t, "memory"), Options{Limits: Limits{Refresh: 30 * time.Second, Memory: 0, Load: time.Second, Render: time.Second, Act: time.Second, State: 1 << 10}})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Refresh(context.Background(), fixedCall)
	var he *HostError
	if !errors.As(err, &he) || !he.Culprit || !strings.Contains(he.Reason, "over the limit") {
		t.Fatalf("err = %v, want a culprit HostError for memory", err)
	}
}

// TestHostStuckRestarts: a stuck call stops its module, as in process, and starts a fresh
// child, since the abandoned runtime may never stop.
func TestHostStuckRestarts(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{BackoffMin: 50 * time.Millisecond})
	release := make(chan struct{})
	defer close(release)
	r, err := h.New(writeBundle(t, "stuck", `"exec"`, `exports.render = function () { return { type: "text", text: "x" }; };
exports.refresh = function () { fd.exec(["sleep"]); };`), Options{Backends: Backends{Exec: &blockingExec{release: release}}, Limits: Limits{Refresh: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	pid := h.Pid()
	var te *TimeoutError
	if err := r.Refresh(context.Background(), fixedCall); !errors.As(err, &te) || !te.Stuck {
		t.Fatalf("err = %v, want a stuck TimeoutError", err)
	}
	if _, err := r.Render(context.Background(), fixedCall); !errors.Is(err, ErrStopped) {
		t.Errorf("after: %v, want ErrStopped", err)
	}
	waitFor(t, 5*time.Second, func() bool { p := h.Pid(); return p != 0 && p != pid })
	// The id is free once the stopped module is closed, and loads into the new child.
	r.Close()
	r2, err := h.New(writeBundle(t, "stuck", "", `exports.render = function () { return { type: "text", text: "y" }; };`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Render(context.Background(), fixedCall); err != nil {
		t.Error(err)
	}
}

// TestHostRefusesDuplicateID, and New after Close.
func TestHostRefusesDuplicateID(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{})
	b := writeBundle(t, "twice", "", `exports.render = function () { return { type: "text", text: "x" }; };`)
	if _, err := h.New(b, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.New(b, Options{}); err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Errorf("second New: %v", err)
	}
	h.Close()
	if _, err := h.New(writeBundle(t, "after", "", `exports.render = function () {};`), Options{}); !errors.Is(err, ErrHostClosed) {
		t.Errorf("New after Close: %v", err)
	}
}

// TestHostStartFails: a command that cannot start gives a HostError with the reason, and the
// next try waits out the backoff.
func TestHostStartFails(t *testing.T) {
	skipViaProcess(t, "drives its own host")
	h := testHost(t, HostOptions{Command: func() *exec.Cmd { return exec.Command("/nonexistent/module-host") }, BackoffMin: time.Minute})
	_, err := h.New(writeBundle(t, "nohost", "", `exports.render = function () {};`), Options{})
	var he *HostError
	if !errors.As(err, &he) || !strings.Contains(he.Reason, "could not start") {
		t.Fatalf("err = %v", err)
	}
	_, err = h.New(writeBundle(t, "nohost2", "", `exports.render = function () {};`), Options{})
	if !errors.As(err, &he) || he.RestartAt.IsZero() {
		t.Fatalf("second: %v, want a HostError with a restart time", err)
	}
}

// TestWireErrors: every typed error comes back as itself.
func TestWireErrors(t *testing.T) {
	for _, e := range []error{
		&TimeoutError{Func: "render", Limit: time.Second, Stuck: true},
		&InternalError{Func: "act", Value: "boom", Stack: "s"},
		&MemoryError{Func: "refresh", Limit: 1 << 20},
		&ScriptError{Func: "render", Name: "TypeError", Message: "m", Stack: "s"},
		&ClientTooOld{Vocabulary: 1, Blocks: 2},
		&BundleError{Path: "main.js", Message: "missing"},
		&NewerBlocks{Blocks: 9},
		ErrStopped, ErrNoAct, ErrClientAction, context.Canceled,
	} {
		data, _ := json.Marshal(encodeError(e))
		var w wireError
		if err := json.Unmarshal(data, &w); err != nil {
			t.Fatal(err)
		}
		got := w.decode()
		if got.Error() != e.Error() {
			t.Errorf("%T: %q, want %q", e, got, e)
		}
	}
	hp := encodeError(errors.Join(ErrHeapPressure)).decode()
	if !errors.Is(hp, ErrHeapPressure) {
		t.Errorf("heap pressure: %v", hp)
	}
}
