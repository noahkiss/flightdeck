package modhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests hold the runtime's limits against a hostile module: built-ins that ignore the
// interrupt, memory held between calls, values that amplify in Go, Go panics, and the bridge's
// own bounds.

// TestRegexTimeout runs a catastrophically backtracking pattern (regexp2, since it has a
// backreference). regexp2's own timeout ends the match with a RangeError the module can catch;
// uncaught, it is a ScriptError with a bounded message, and the module keeps working.
func TestRegexTimeout(t *testing.T) {
	m := newModule(t, writeBundle(t, "regex", "", `
var re = /^(a+)+\1$/;
var input = "a".repeat(5000) + "b";
exports.render = function () { return { type: "text", text: String(fd.state.get("caught")) }; };
exports.act = function (name) {
  if (name === "caught") {
    try { re.test(input); } catch (e) { fd.state.set("caught", e.name); }
    return;
  }
  re.test(input);
};
`), Options{})
	ctx := context.Background()
	start := time.Now()
	if err := m.Act(ctx, "caught", nil, fixedCall); err != nil {
		t.Fatalf("caught: %v", err)
	}
	if tree, err := m.Render(ctx, fixedCall); err != nil || string(tree) != `{"text":"RangeError","type":"text"}` {
		t.Fatalf("Render = %s, %v; want the caught RangeError", tree, err)
	}
	var se *ScriptError
	err := m.Act(ctx, "uncaught", nil, fixedCall)
	if !errors.As(err, &se) || se.Name != "RangeError" {
		t.Fatalf("uncaught: %v, want a RangeError", err)
	}
	if n := len([]rune(se.Message)); n > maxErrorMessage {
		t.Errorf("message is %d runes, over %d", n, maxErrorMessage)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("two timed-out matches took %s", d)
	}
	// Render's own limit equals the match timeout: either error is fine, but it returns.
	m2 := newModule(t, writeBundle(t, "regex-render", "", `
exports.render = function () { /^(a+)+\1$/.test("a".repeat(5000) + "b"); return { type: "text", text: "x" }; };
`), Options{})
	start = time.Now()
	if _, err := m2.Render(ctx, fixedCall); err == nil {
		t.Error("render with a runaway pattern succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("render took %s", d)
	}
	if _, err := m2.Render(ctx, fixedCall); errors.Is(err, ErrStopped) {
		t.Error("a regex timeout stopped the module")
	}
}

// blockingExec stands for any native call that ignores the interrupt and the context.
type blockingExec struct{ release chan struct{} }

func (e *blockingExec) Run(ctx context.Context, req ExecRequest) (ExecResult, error) {
	<-e.release
	return ExecResult{}, nil
}

// TestStuckCall: a call whose runtime never comes back returns at its limit plus the grace,
// as a stuck TimeoutError, and the module is stopped. The caller's cancel does the same.
func TestStuckCall(t *testing.T) {
	js := `
exports.render = function () { return { type: "text", text: "x" }; };
exports.refresh = function () { fd.exec(["sleep"]); };
`
	for _, tc := range []struct {
		name   string
		limits Limits
		cancel time.Duration
	}{
		{"limit", Limits{Refresh: 100 * time.Millisecond}, 0},
		{"caller", Limits{}, 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &blockingExec{release: make(chan struct{})}
			t.Cleanup(func() { close(ex.release) })
			m := newModule(t, writeBundle(t, "stuck", `"exec"`, js), Options{Backends: Backends{Exec: ex}, Limits: tc.limits})
			ctx := context.Background()
			if tc.cancel > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				time.AfterFunc(tc.cancel, cancel)
				defer cancel()
			}
			start := time.Now()
			err := m.Refresh(ctx, fixedCall)
			var te *TimeoutError
			if !errors.As(err, &te) || !te.Stuck || te.Func != "refresh" {
				t.Fatalf("err = %v, want a stuck refresh TimeoutError", err)
			}
			if d := time.Since(start); d > 100*time.Millisecond+stuckGrace+2*time.Second {
				t.Errorf("returned after %s", d)
			}
			if _, err := m.Render(context.Background(), fixedCall); !errors.Is(err, ErrStopped) {
				t.Errorf("Render after a stuck call: %v, want ErrStopped", err)
			}
		})
	}
}

// TestMemoryHogBlamed is the review's probe: a module that keeps a large buffer across short
// calls (each shorter than the watch's poll) is stopped at the end of its own call, its memory
// is let go, and an innocent module that runs next is not blamed.
func TestMemoryHogBlamed(t *testing.T) {
	runtime.GC()
	limit := heapBytes() + 64<<20
	limits := Limits{Memory: limit}
	hog := newModule(t, writeBundle(t, "hog", "", `
var keep;
exports.render = function () { keep = new ArrayBuffer(128 * 1024 * 1024); return { type: "text", text: "hog" }; };
`), Options{Limits: limits})
	innocent := newModule(t, writeBundle(t, "innocent", "", `
exports.render = function () { var n = 0; for (var i = 0; i < 1000; i++) n += i; return { type: "text", text: String(n) }; };
`), Options{Limits: limits})
	ctx := context.Background()
	var me *MemoryError
	if _, err := hog.Render(ctx, fixedCall); !errors.As(err, &me) {
		t.Fatalf("hog: %v, want a MemoryError", err)
	}
	if g := hog.HeapGrowth(); g < 64<<20 {
		t.Errorf("hog's heap growth = %d, want at least 64 MiB", g)
	}
	if _, err := hog.Render(ctx, fixedCall); !errors.Is(err, ErrStopped) {
		t.Errorf("hog again: %v, want ErrStopped", err)
	}
	for i := range 3 {
		if _, err := innocent.Render(ctx, fixedCall); err != nil {
			t.Fatalf("innocent render %d: %v", i, err)
		}
	}
	if g := innocent.HeapGrowth(); g > 16<<20 {
		t.Errorf("innocent's heap growth = %d", g)
	}
}

// TestHeapPressure: a heap already over the limit before a call is no module's fault: the call
// is refused with ErrHeapPressure and the module keeps running.
func TestHeapPressure(t *testing.T) {
	runtime.GC()
	limits := Limits{Memory: heapBytes() + 32<<20}
	m := newModule(t, writeBundle(t, "bystander", "", `
exports.render = function () { return { type: "text", text: "ok" }; };
`), Options{Limits: limits})
	held := make([]byte, 64<<20)
	for i := range held {
		held[i] = 1
	}
	if _, err := m.Render(context.Background(), fixedCall); !errors.Is(err, ErrHeapPressure) {
		t.Fatalf("err = %v, want ErrHeapPressure", err)
	}
	runtime.KeepAlive(held)
	held = nil
	runtime.GC()
	if _, err := m.Render(context.Background(), fixedCall); err != nil {
		t.Fatalf("after the pressure went: %v", err)
	}
}

// totalAlloc runs f and returns the bytes the process allocated meanwhile.
func totalAlloc(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// TestNoValueAmplification: String objects, typed arrays and the like are refused before the
// host lists their keys, which allocated about 140 times their size in Go.
func TestNoValueAmplification(t *testing.T) {
	// Other tests allocate concurrently. Run these per-call measurements in a fresh
	// test process so those allocations cannot enter the process-wide counter.
	if os.Getenv("GFF_TEST_AMPLIFICATION_CHILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNoValueAmplification$", "-test.count=1")
		cmd.Env = append(os.Environ(), "GFF_TEST_AMPLIFICATION_CHILD=1", "GORACE=atexit_sleep_ms=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated allocation test: %v\n%s", err, out)
		}
		return
	}
	var logs []string
	var mu sync.Mutex
	m := newModule(t, writeBundle(t, "amplify", `"exec", "fetch"`, `
var big = new String("a".repeat(3e6));
var arr = new Uint8Array(3e6);
var prox = new Proxy(new Uint8Array(3e6), {});
exports.render = function (ctx) {
  return { type: "text", text: "x", extra: ctx.place === "board" ? big : arr };
};
exports.act = function (name) {
  switch (name) {
  case "log": fd.log(big, arr); break;
  case "options": fd.exec(["true"], big); break;
  case "headers": fd.fetch("https://example.invalid/", { headers: arr }); break;
  case "params": fd.hab.get("structure.get", big); break;
  case "state": fd.state.set("k", arr); break;
  case "proxy": fd.state.set("k", { p: prox }); break;
  }
};
`), Options{
		Backends: Backends{Hab: &fakeHab{}, Exec: &fakeExec{}, Fetch: &fakeFetch{}, Log: func(l string) {
			mu.Lock()
			logs = append(logs, l)
			mu.Unlock()
		}},
		// This test measures allocations, not speed. Building three large JS values can
		// exceed the default one-second load budget while other suites use the CPU.
		Limits: Limits{Memory: 0, Load: 10 * time.Second, Render: 5 * time.Second, Act: 5 * time.Second},
	})
	const bound = 64 << 20 // the probe allocated 422 MiB before the fix
	ctx := context.Background()
	for _, place := range []string{"board", "sheet"} {
		c := fixedCall
		c.Place = place
		var err error
		n := totalAlloc(func() { _, err = m.Render(ctx, c) })
		var te *TreeError
		if !errors.As(err, &te) {
			t.Errorf("render %s: %v, want a TreeError", place, err)
		}
		if n > bound {
			t.Errorf("render %s allocated %d MiB", place, n>>20)
		}
	}
	for _, name := range []string{"log", "options", "headers", "params", "state", "proxy"} {
		var err error
		n := totalAlloc(func() { err = m.Act(ctx, name, nil, fixedCall) })
		if n > bound {
			t.Errorf("%s allocated %d MiB", name, n>>20)
		}
		var se *ScriptError
		if name == "log" {
			if err != nil {
				t.Errorf("log: %v", err)
			}
			continue
		}
		if !errors.As(err, &se) || se.Name != BridgeError {
			t.Errorf("%s: %v, want a BridgeError", name, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || logs[0] != "[object String] [object Object]" {
		t.Errorf("logs = %q", logs)
	}
}

type panicHab struct{}

func (panicHab) Get(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	var p *ExecResult
	return json.RawMessage(p.Stdout), nil // nil dereference: a runtime.Error goja re-panics
}

func (panicHab) Act(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	panic("act backend failed")
}

// TestBackendPanic: a Go panic in a backend becomes an InternalError and stops the module, and
// the process lives on.
func TestBackendPanic(t *testing.T) {
	for _, name := range []string{"get", "act"} {
		m := newModule(t, writeBundle(t, "panics", `"act"`, `
exports.render = function () { return { type: "text", text: "x" }; };
exports.act = function (name) { if (name === "get") fd.hab.get("structure.get"); else fd.hab.act("session.close", {}); };
`), Options{Backends: Backends{Hab: panicHab{}}})
		err := m.Act(context.Background(), name, nil, fixedCall)
		var ie *InternalError
		var se *ScriptError
		switch {
		case errors.As(err, &ie):
			if ie.Func != "act" || ie.Stack == "" {
				t.Errorf("%s: %+v", name, ie)
			}
		case name == "act" && errors.As(err, &se):
			// goja turns a panic with a value that is not a runtime.Error into a JS
			// exception; the module keeps running.
			continue
		default:
			t.Fatalf("%s: err = %v, want an InternalError", name, err)
		}
		if _, err := m.Render(context.Background(), fixedCall); !errors.Is(err, ErrStopped) {
			t.Errorf("%s: Render after a panic: %v, want ErrStopped", name, err)
		}
	}
}

// TestHabReadList: fd.hab.get calls only listed reads, whatever the backend would answer.
func TestHabReadList(t *testing.T) {
	hab := &fakeHab{}
	m := newModule(t, writeBundle(t, "reads", "", `
exports.render = function () { return { type: "text", text: "x" }; };
exports.act = function (name) { fd.hab.get(name); };
`), Options{Backends: Backends{Hab: hab}})
	for _, method := range []string{"session.create", "session.send", "pane.input", "file.read", "transcript.read"} {
		err := m.Act(context.Background(), method, nil, fixedCall)
		var se *ScriptError
		if !errors.As(err, &se) || se.Name != BridgeError || !strings.Contains(se.Message, "not a read") {
			t.Errorf("%s: %v, want a BridgeError", method, err)
		}
	}
	if err := m.Act(context.Background(), "structure.get", nil, fixedCall); err != nil {
		t.Errorf("structure.get: %v", err)
	}
	if len(hab.calls) != 1 {
		t.Errorf("backend calls = %q, want only structure.get", hab.calls)
	}
}

type countingStore struct {
	MemStore
	mu    sync.Mutex
	saves int
}

func (s *countingStore) Save(module string, state map[string]json.RawMessage) error {
	s.mu.Lock()
	s.saves++
	s.mu.Unlock()
	return s.MemStore.Save(module, state)
}

// TestStateSavedOnce: a call saves its state once at its end; a call that throws keeps none of
// its writes; a call that reads its own writes sees them.
func TestStateSavedOnce(t *testing.T) {
	store := &countingStore{}
	m := newModule(t, writeBundle(t, "saves", "", `
exports.render = function () { return { type: "text", text: String(fd.state.get("n")) }; };
exports.refresh = function () {
  for (var i = 1; i <= 1000; i++) fd.state.set("n", i);
  if (fd.state.get("n") !== 1000) throw new Error("own write not seen");
};
exports.act = function (name) { fd.state.set("n", -1); if (name === "throw") throw new Error("no"); };
`), Options{Backends: Backends{State: store}})
	ctx := context.Background()
	if err := m.Refresh(ctx, fixedCall); err != nil {
		t.Fatal(err)
	}
	if store.saves != 1 {
		t.Errorf("saves = %d, want 1", store.saves)
	}
	var se *ScriptError
	if err := m.Act(ctx, "throw", nil, fixedCall); !errors.As(err, &se) {
		t.Fatalf("throw: %v", err)
	}
	if store.saves != 1 {
		t.Errorf("saves after a throw = %d, want 1", store.saves)
	}
	if tree, err := m.Render(ctx, fixedCall); err != nil || string(tree) != `{"text":"1000","type":"text"}` {
		t.Errorf("Render = %s, %v; the throwing call's write was kept", tree, err)
	}
	st, _ := store.Load("saves")
	if string(st["n"]) != "1000" {
		t.Errorf("stored n = %s", st["n"])
	}
}

// TestLogCap: fd.log keeps maxLogLines lines per call, then one line that says the rest went.
func TestLogCap(t *testing.T) {
	var lines []string
	m := newModule(t, writeBundle(t, "chatty", "", `
exports.render = function () { return { type: "text", text: "x" }; };
exports.refresh = function () { for (var i = 0; i < 100000; i++) fd.log("line", i); };
`), Options{Backends: Backends{Log: func(l string) { lines = append(lines, l) }}})
	for range 2 {
		lines = nil
		if err := m.Refresh(context.Background(), fixedCall); err != nil {
			t.Fatal(err)
		}
		if len(lines) != maxLogLines+1 || !strings.Contains(lines[maxLogLines], "dropped") {
			t.Fatalf("%d lines, last %q", len(lines), lines[len(lines)-1])
		}
	}
}

// TestExecTimeoutUnits: a timeout past the call's own limit is refused as likely milliseconds;
// Infinity never overflows a Duration.
func TestExecTimeoutUnits(t *testing.T) {
	ex := &fakeExec{}
	m := newModule(t, writeBundle(t, "units", `"exec"`, `
exports.render = function () { return { type: "text", text: "x" }; };
exports.act = function (name, args) { fd.exec(["true"], { timeout: args.t === "inf" ? Infinity : args.t }); };
`), Options{Backends: Backends{Exec: ex}})
	for _, arg := range []string{`{"t":5000}`, `{"t":"inf"}`, `{"t":1e300}`} {
		err := m.Act(context.Background(), "run", json.RawMessage(arg), fixedCall)
		var se *ScriptError
		if !errors.As(err, &se) || !strings.Contains(se.Message, "timeout is in seconds; at most 10") {
			t.Errorf("%s: %v", arg, err)
		}
	}
	if err := m.Act(context.Background(), "run", json.RawMessage(`{"t":1.5}`), fixedCall); err != nil {
		t.Fatal(err)
	}
	if ex.got.Timeout != 1500*time.Millisecond {
		t.Errorf("timeout = %s, want 1.5s", ex.got.Timeout)
	}
}

// TestRequireBody: a file that closes the module function early, to run code at the top level,
// is a syntax error.
func TestRequireBody(t *testing.T) {
	b := writeBundle(t, "breakout", "", `}); globalThis.escaped = 1; (function () {`)
	var se *ScriptError
	if _, err := New(b, Options{}); !errors.As(err, &se) || se.Name != "SyntaxError" {
		t.Errorf("err = %v, want a SyntaxError", err)
	}
	b = writeBundle(t, "breakout2", "", `}, function () {`)
	if _, err := New(b, Options{}); !errors.As(err, &se) || se.Name != "SyntaxError" {
		t.Errorf("sequence: err = %v, want a SyntaxError", err)
	}
}

// TestNoSourceMapRead: goja's parser would read the file a sourceMappingURL comment names from
// the host's disk (a FIFO wedges it, /dev/zero never ends). Neither a bundle file nor eval reads
// it: the source map's file name never shows in a stack.
func TestNoSourceMapRead(t *testing.T) {
	smap := filepath.Join(t.TempDir(), "leak.map")
	if err := os.WriteFile(smap, []byte(`{"version":3,"sources":["host-file-was-read.js"],"names":[],"mappings":"AAAA;AACA;AACA"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModule(t, writeBundle(t, "maps", "", `
exports.render = function () { return { type: "text", text: "x" }; };
exports.act = function (name) {
  if (name === "eval") eval("throw new Error('e')\n//# sourceMappingURL="+`+"`"+smap+"`"+`);
  throw new Error("main");
};
//# sourceMappingURL=`+smap+`
`), Options{})
	for _, name := range []string{"main", "eval"} {
		err := m.Act(context.Background(), name, nil, fixedCall)
		var se *ScriptError
		if !errors.As(err, &se) {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(se.Stack, "host-file-was-read") {
			t.Errorf("%s: the stack names the source map's file: %s", name, se.Stack)
		}
	}
}

// TestLoadOversizedFile: a .js file over the bundle limit is refused from its size, before it
// is read.
func TestLoadOversizedFile(t *testing.T) {
	b := writeBundle(t, "big", "", `exports.render = function () {};`)
	f, err := os.Create(filepath.Join(b.Dir, "huge.js"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4 << 30); err != nil {
		t.Skip("no sparse file:", err)
	}
	f.Close()
	var n uint64
	n = totalAlloc(func() { _, err = Load(b.Dir) })
	if err == nil || !strings.Contains(err.Error(), "bytes in all") {
		t.Errorf("err = %v", err)
	}
	if n > 16<<20 {
		t.Errorf("Load allocated %d MiB", n>>20)
	}
}
