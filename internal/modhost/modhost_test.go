package modhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api/modules"
)

var update = flag.Bool("update", false, "rewrite the golden trees in testdata/golden")

func bundleDir(name string) string { return filepath.Join("testdata", "bundles", name) }

func load(t *testing.T, name string) *Bundle {
	t.Helper()
	b, err := Load(bundleDir(name))
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	return b
}

func newModule(t *testing.T, b *Bundle, opts Options) *Module {
	t.Helper()
	m, err := New(b, opts)
	if err != nil {
		t.Fatalf("New(%s): %v", b.Manifest.ID, err)
	}
	return m
}

// writeBundle makes a module folder in a temporary directory.
func writeBundle(t *testing.T, id, powers, js string) *Bundle {
	t.Helper()
	dir := t.TempDir()
	manifest := "id = \"" + id + "\"\nname = \"Test\"\nversion = \"0.1.0\"\nblocks = 1\nbridge = 1\nplaces = [\"board\"]\n"
	if powers != "" {
		manifest += "powers = [" + powers + "]\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return b
}

type fakeHab struct {
	mu    sync.Mutex
	calls []string
}

func (h *fakeHab) Get(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	h.mu.Lock()
	h.calls = append(h.calls, "get "+method+" "+string(params))
	h.mu.Unlock()
	switch method {
	case "structure.get":
		return json.RawMessage(`{"sessions":[{"name":"build"},{"name":"docs"}]}`), nil
	}
	return nil, errors.New("unknown method " + method)
}

func (h *fakeHab) Act(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	h.mu.Lock()
	h.calls = append(h.calls, "act "+method+" "+string(params))
	h.mu.Unlock()
	return json.RawMessage(`{"id":"s9"}`), nil
}

type fakeExec struct {
	got ExecRequest
	out ExecResult
}

func (e *fakeExec) Run(ctx context.Context, req ExecRequest) (ExecResult, error) {
	e.got = req
	if _, ok := ctx.Deadline(); !ok {
		return ExecResult{}, errors.New("no deadline on the context")
	}
	return e.out, nil
}

type fakeFetch struct {
	got FetchRequest
}

func (f *fakeFetch) Do(ctx context.Context, req FetchRequest) (FetchResponse, error) {
	f.got = req
	return FetchResponse{Status: 201, Headers: map[string]string{"Content-Type": "text/plain"}, Body: []byte("pong pong pong")}, nil
}

var fixedCall = Call{Host: "box-a", OS: "linux", Place: "board", Vocabulary: 1, Now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}

// TestHelloGolden runs the sample module through refresh, an action and render, and compares
// the tree with its stored golden tree.
func TestHelloGolden(t *testing.T) {
	var logs []string
	hab := &fakeHab{}
	store := &MemStore{}
	m := newModule(t, load(t, "hello"), Options{
		Backends: Backends{Hab: hab, State: store, Log: func(l string) { logs = append(logs, l) }},
	})
	ctx := context.Background()
	if err := m.Refresh(ctx, fixedCall); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	for range 2 {
		if err := m.Act(ctx, "counter.tap", json.RawMessage(`{"by":2}`), fixedCall); err != nil {
			t.Fatalf("Act: %v", err)
		}
	}
	tree, err := m.Render(ctx, fixedCall)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	golden := filepath.Join("testdata", "golden", "hello.json")
	if *update {
		if err := os.WriteFile(golden, append(tree, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tree, bytes.TrimRight(want, "\n")) {
		t.Errorf("tree differs from %s (go test -update rewrites it)\ngot:  %s\nwant: %s", golden, tree, want)
	}
	if err := modules.ValidateTree(tree, 1); err != nil {
		t.Errorf("golden tree is not valid: %v", err)
	}
	if len(hab.calls) != 1 || hab.calls[0] != `get structure.get {"host":"box-a"}` {
		t.Errorf("hab calls = %q", hab.calls)
	}
	if len(logs) != 1 || logs[0] != "refreshed 2 sessions" {
		t.Errorf("logs = %q", logs)
	}
	st, _ := store.Load("hello")
	if string(st["count"]) != "4" {
		t.Errorf("stored count = %s, want 4", st["count"])
	}

	// The person's settings replace the defaults.
	m2 := newModule(t, load(t, "hello"), Options{
		Backends: Backends{State: store},
		Settings: map[string]any{"greeting": "Hi", "show_count": false, "unknown": 1},
	})
	tree, err = m2.Render(ctx, fixedCall)
	if err != nil {
		t.Fatalf("Render with settings: %v", err)
	}
	if !strings.Contains(string(tree), `"Hi from box-a (linux)"`) || strings.Contains(string(tree), "tapped") {
		t.Errorf("settings not applied: %s", tree)
	}
}

func TestLoadRefusesNewer(t *testing.T) {
	_, err := Load(bundleDir("newer-bridge"))
	var nb *modules.NewerBridge
	if !errors.As(err, &nb) || nb.Bridge != 2 {
		t.Errorf("newer bridge: err = %v, want *modules.NewerBridge for 2", err)
	}
	_, err = Load(bundleDir("newer-blocks"))
	var nbl *NewerBlocks
	if !errors.As(err, &nbl) || nbl.Blocks != 2 {
		t.Errorf("newer blocks: err = %v, want *NewerBlocks for 2", err)
	}
}

func TestLoadRefusesBadBundles(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "manifest.toml: missing") {
		t.Errorf("no manifest: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte("id = \"x\"\nname = \"X\"\nversion = \"0.1.0\"\nblocks = 1\nbridge = 1\nplaces = [\"board\"]\n"), 0o600)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "main.js: missing") {
		t.Errorf("no main.js: %v", err)
	}
	// A link that leads out of the folder is refused.
	outside := filepath.Join(t.TempDir(), "secret.js")
	os.WriteFile(outside, []byte("exports.render = function () {};"), 0o600)
	if err := os.Symlink(outside, filepath.Join(dir, "main.js")); err != nil {
		t.Skip("no symlinks:", err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a main.js that links out of the folder was loaded")
	}
	os.Remove(filepath.Join(dir, "main.js"))
	os.WriteFile(filepath.Join(dir, "main.js"), []byte("exports.refresh = function () {};"), 0o600)
	b, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(b, Options{}); err == nil || !strings.Contains(err.Error(), "exports no render") {
		t.Errorf("no render: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "main.js"), []byte("exports.render = function ( {"), 0o600)
	b, _ = Load(dir)
	var se *ScriptError
	if _, err := New(b, Options{}); !errors.As(err, &se) || se.Func != "load" {
		t.Errorf("syntax error: %v, want a load ScriptError", err)
	}
	os.WriteFile(filepath.Join(dir, "main.js"), []byte("for (;;) {}"), 0o600)
	b, _ = Load(dir)
	var te *TimeoutError
	if _, err := New(b, Options{Limits: Limits{Load: 50 * time.Millisecond}}); !errors.As(err, &te) || te.Func != "load" {
		t.Errorf("endless top level: %v, want a load TimeoutError", err)
	}
}

// TestTimeouts interrupts an endless loop in each function, and the module keeps working after.
func TestTimeouts(t *testing.T) {
	m := newModule(t, load(t, "loop"), Options{Limits: Limits{Refresh: 200 * time.Millisecond, Act: 200 * time.Millisecond}})
	ctx := context.Background()
	check := func(name string, err error, start time.Time, limit time.Duration) {
		t.Helper()
		var te *TimeoutError
		if !errors.As(err, &te) || te.Func != name || te.Limit != limit {
			t.Fatalf("%s: err = %v, want a TimeoutError for %s at %s", name, err, name, limit)
		}
		if d := time.Since(start); d > limit+2*time.Second {
			t.Errorf("%s took %s to stop", name, d)
		}
	}
	start := time.Now()
	_, err := m.Render(ctx, fixedCall)
	check("render", err, start, 250*time.Millisecond)
	start = time.Now()
	check("refresh", m.Refresh(ctx, fixedCall), start, 200*time.Millisecond)
	start = time.Now()
	check("act", m.Act(ctx, "spin", nil, fixedCall), start, 200*time.Millisecond)

	if err := m.Act(ctx, "calm.down", nil, fixedCall); err != nil {
		t.Fatalf("Act after timeouts: %v", err)
	}
	tree, err := m.Render(ctx, fixedCall)
	if err != nil || string(tree) != `{"text":"calm","type":"text"}` {
		t.Fatalf("Render after timeouts = %s, %v", tree, err)
	}
}

// TestCallerCancel ends a call when the caller's context ends.
func TestCallerCancel(t *testing.T) {
	m := newModule(t, load(t, "loop"), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := m.Refresh(ctx, fixedCall); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestForbiddenCalls(t *testing.T) {
	ctx := context.Background()
	store := &MemStore{}
	m := newModule(t, load(t, "forbidden"), Options{Backends: Backends{Hab: &fakeHab{}, State: store}})
	if err := m.Refresh(ctx, fixedCall); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	tree, err := m.Render(ctx, fixedCall)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tree), "PowerError: fd.hab.act needs the act power") {
		t.Errorf("caught error not kept: %s", tree)
	}
	// Not caught: the call fails with the error's name.
	err = m.Act(ctx, "run.program", nil, fixedCall)
	var se *ScriptError
	if !errors.As(err, &se) || se.Name != PowerError || se.Func != "act" || !strings.Contains(se.Message, "fd.exec needs the exec power") {
		t.Errorf("exec without the power: %v", err)
	}
	// Declared, but this host has no fetch backend.
	err = m.Act(ctx, "fetch.in.act", nil, fixedCall)
	if !errors.As(err, &se) || se.Name != BridgeError || !strings.Contains(se.Message, "makes no requests") {
		t.Errorf("fetch with no backend: %v", err)
	}
	// Render may not fetch, even with the power.
	store.Save("forbidden", map[string]json.RawMessage{"try_fetch": json.RawMessage("true")})
	m2 := newModule(t, load(t, "forbidden"), Options{Backends: Backends{State: store, Fetch: &fakeFetch{}}})
	_, err = m2.Render(ctx, fixedCall)
	if !errors.As(err, &se) || se.Name != BridgeError || !strings.Contains(se.Message, "not allowed in render") {
		t.Errorf("fetch in render: %v", err)
	}
}

func TestBadTrees(t *testing.T) {
	m := newModule(t, load(t, "bad-tree"), Options{})
	for place, want := range map[string]string{
		"board":   "unknown block type",
		"host":    "",
		"session": "render returned nothing",
		"sheet":   "nested deeper",
	} {
		c := fixedCall
		c.Place = place
		_, err := m.Render(context.Background(), c)
		var te *TreeError
		if !errors.As(err, &te) || len(te.Problems) == 0 {
			t.Errorf("%s: err = %v, want a TreeError", place, err)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want it to say %q", place, err, want)
		}
	}
}

func TestMemoryLimit(t *testing.T) {
	limit := heapBytes() + 32<<20
	m := newModule(t, load(t, "memory"), Options{Limits: Limits{Refresh: time.Minute, Memory: limit}})
	err := m.Refresh(context.Background(), fixedCall)
	var me *MemoryError
	if !errors.As(err, &me) || me.Func != "refresh" || me.Limit != limit {
		t.Fatalf("err = %v, want a MemoryError", err)
	}
	if _, err := m.Render(context.Background(), fixedCall); !errors.Is(err, ErrStopped) {
		t.Errorf("Render after the memory limit: %v, want ErrStopped", err)
	}
}

func TestStackDepth(t *testing.T) {
	m := newModule(t, writeBundle(t, "deep", "", `
function down(n) { return down(n + 1) + 1; }
exports.render = function () { return down(0); };
`), Options{})
	_, err := m.Render(context.Background(), fixedCall)
	var se *ScriptError
	if !errors.As(err, &se) || se.Name != "RangeError" {
		t.Errorf("err = %v, want a RangeError", err)
	}
}

func TestPowersReachBackends(t *testing.T) {
	hab := &fakeHab{}
	ex := &fakeExec{out: ExecResult{Code: 3, Stdout: []byte("M  a.go\n?? héllo.txt\n"), Stderr: []byte("warn")}}
	fe := &fakeFetch{}
	limits := DefaultLimits()
	limits.Output = 13 // cuts stdout inside the é, and the fetch body
	m := newModule(t, load(t, "powers"), Options{Backends: Backends{Hab: hab, Exec: ex, Fetch: fe}, Limits: limits})
	if err := m.Refresh(context.Background(), fixedCall); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if strings.Join(ex.got.Argv, " ") != "git status --short" || ex.got.Dir != "/work" || ex.got.Timeout > 2*time.Second || ex.got.Timeout <= 0 {
		t.Errorf("exec request = %+v", ex.got)
	}
	if fe.got.URL != "https://example.invalid/status" || fe.got.Method != "POST" || fe.got.Headers["X-Probe"] != "1" || fe.got.Body != "ping" {
		t.Errorf("fetch request = %+v", fe.got)
	}
	if len(hab.calls) != 1 || hab.calls[0] != `act session.create {"project":"p1"}` {
		t.Errorf("hab calls = %q", hab.calls)
	}
	tree, err := m.Render(context.Background(), fixedCall)
	if err != nil {
		t.Fatal(err)
	}
	var text struct{ Text string }
	json.Unmarshal(tree, &text)
	want := `{"body":"pong pong pon","body_cut":true,"code":3,"session":"s9","status":201,"stdout":"M  a.go\n?? h","stdout_cut":true,"type":"text/plain"}`
	if text.Text != want {
		t.Errorf("seen =\n%s\nwant\n%s", text.Text, want)
	}
}

func TestBridgeArguments(t *testing.T) {
	m := newModule(t, writeBundle(t, "args", `"exec", "fetch"`, `
exports.render = function () { return { type: "text", text: "x" }; };
exports.act = function (name, args) {
  switch (name) {
  case "exec.empty": fd.exec([]); break;
  case "exec.shell": fd.exec("ls -l"); break;
  case "exec.option": fd.exec(["ls"], { shell: true }); break;
  case "fetch.file": fd.fetch("file:///etc/passwd"); break;
  case "fetch.header": fd.fetch("https://example.invalid/", { headers: { a: "b\r\nc: d" } }); break;
  case "hab.method": fd.hab.get(""); break;
  case "state.key": fd.state.set("", 1); break;
  case "params.function": fd.hab.get("x", { f: Symbol("s") }); break;
  }
};
`), Options{Backends: Backends{Hab: &fakeHab{}, Exec: &fakeExec{}, Fetch: &fakeFetch{}}})
	for _, name := range []string{"exec.empty", "exec.shell", "exec.option", "fetch.file", "fetch.header", "hab.method", "state.key", "params.function"} {
		err := m.Act(context.Background(), name, nil, fixedCall)
		var se *ScriptError
		if !errors.As(err, &se) || se.Name != BridgeError {
			t.Errorf("%s: err = %v, want a BridgeError", name, err)
		}
	}
	if err := m.Act(context.Background(), "client.copy", nil, fixedCall); !errors.Is(err, ErrClientAction) {
		t.Errorf("client action: %v", err)
	}
	if err := m.Act(context.Background(), "x", json.RawMessage(`[1]`), fixedCall); err == nil {
		t.Error("args that are not an object were accepted")
	}
}

func TestMultiFileAndAsync(t *testing.T) {
	m := newModule(t, load(t, "multi-file"), Options{})
	ctx := context.Background()
	tree, err := m.Render(ctx, fixedCall)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var metric struct{ Label, Value, Unit string }
	json.Unmarshal(tree, &metric)
	if metric.Value != "same" || metric.Unit != "ms" {
		t.Errorf("metric = %+v: require did not resolve or cache", metric)
	}
	if n := len([]rune(metric.Label)); n != modules.ShortMax || !strings.HasSuffix(metric.Label, "…") {
		t.Errorf("label not cut to %d with an ellipsis: %d %q", modules.ShortMax, n, metric.Label)
	}
	var se *ScriptError
	if err := m.Act(ctx, "fail.later", nil, fixedCall); !errors.As(err, &se) || se.Name != "TypeError" || se.Message != "failed after await" {
		t.Errorf("rejected promise: %v", err)
	}
	for name, want := range map[string]string{"require.outside": "outside the module", "require.bare": "only a relative path"} {
		if err := m.Act(ctx, name, nil, fixedCall); !errors.As(err, &se) || se.Name != BridgeError || !strings.Contains(se.Message, want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	m2 := newModule(t, writeBundle(t, "pending", "", `exports.render = function () { return new Promise(function () {}); };`), Options{})
	if _, err := m2.Render(ctx, fixedCall); !errors.As(err, &se) || !strings.Contains(se.Message, "never settled") {
		t.Errorf("pending promise: %v", err)
	}
}

func TestStateLimitAndStore(t *testing.T) {
	dir := t.TempDir()
	store := DirStore{Dir: dir}
	limits := DefaultLimits()
	limits.State = 64
	js := `
exports.render = function () { return { type: "text", text: String(fd.state.get("n")) }; };
exports.act = function (name, args) {
  if (name === "set.small") fd.state.set("n", args.n);
  if (name === "set.big") fd.state.set("big", new Array(100).join("x"));
  if (name === "unset") fd.state.set("n", undefined);
};
`
	b := writeBundle(t, "stateful", "", js)
	m := newModule(t, b, Options{Backends: Backends{State: store}, Limits: limits})
	ctx := context.Background()
	if err := m.Act(ctx, "set.small", json.RawMessage(`{"n":41}`), fixedCall); err != nil {
		t.Fatal(err)
	}
	var se *ScriptError
	if err := m.Act(ctx, "set.big", nil, fixedCall); !errors.As(err, &se) || se.Name != BridgeError || !strings.Contains(se.Message, "64 bytes") {
		t.Errorf("state over the limit: %v", err)
	}
	// A new runtime, as after a restart, reads the state back; the refused write left nothing.
	m2 := newModule(t, b, Options{Backends: Backends{State: store}, Limits: limits})
	tree, err := m2.Render(ctx, fixedCall)
	if err != nil || string(tree) != `{"text":"41","type":"text"}` {
		t.Errorf("state after reload = %s, %v", tree, err)
	}
	if err := m2.Act(ctx, "unset", nil, fixedCall); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "stateful.json"))
	if string(data) != "{}" {
		t.Errorf("state file = %s, want {}", data)
	}
	if err := store.Save("../escape", nil); err == nil {
		t.Error("DirStore saved under a bad id")
	}
}

// TestNoHostAPI checks from inside JS that no filesystem, network, process or timer API exists,
// that the global object holds only ECMAScript built-ins and fd, and that fd holds only the
// bridge. A new global from a goja update fails here until it is judged and listed.
func TestNoHostAPI(t *testing.T) {
	js := `
var probes = {};
var names = ["require", "module", "exports", "process", "Buffer", "global", "fs", "os", "net",
  "child_process", "fetch", "XMLHttpRequest", "WebSocket", "setTimeout", "setInterval",
  "setImmediate", "queueMicrotask", "console", "Deno", "Bun", "importScripts", "navigator",
  "location", "performance", "crypto", "SharedArrayBuffer", "Atomics", "WebAssembly"];
names.forEach(function (n) {
  probes[n] = eval("typeof " + n);
});
probes.viaFunction = Function("return typeof require + ' ' + typeof process")();
try { eval("import('fs')"); probes.dynamicImport = "allowed"; } catch (e) { probes.dynamicImport = e.name; }
probes.globals = Object.getOwnPropertyNames(globalThis).sort();
probes.fd = Object.getOwnPropertyNames(fd).sort();
probes.hab = Object.getOwnPropertyNames(fd.hab).sort();
probes.state = Object.getOwnPropertyNames(fd.state).sort();
probes.frozen = Object.isFrozen(fd) && Object.isFrozen(fd.hab) && Object.isFrozen(fd.state) && Object.isFrozen(fd.settings);
// A module that replaces built-ins does not change how the bridge works.
JSON = null; Error = null;
exports.render = function () { return { type: "log", lines: [] }; };
exports.act = function () {
  var r = fd.hab.get("structure.get");
  try { fd.exec(["sh"]); } catch (e) { probes.afterReplace = e.name + " " + r.sessions.length; }
  fd.state.set("probes", probes);
};
`
	store := &MemStore{}
	m := newModule(t, writeBundle(t, "probe", "", js), Options{Backends: Backends{Hab: &fakeHab{}, State: store}})
	if err := m.Act(context.Background(), "probe", nil, fixedCall); err != nil {
		t.Fatalf("Act: %v", err)
	}
	st, _ := store.Load("probe")
	var p map[string]any
	if err := json.Unmarshal(st["probes"], &p); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"require", "module", "exports", "process", "Buffer", "global", "fs", "os", "net",
		"child_process", "fetch", "XMLHttpRequest", "WebSocket", "setTimeout", "setInterval",
		"setImmediate", "queueMicrotask", "console", "Deno", "Bun", "importScripts", "navigator",
		"location", "performance", "crypto", "SharedArrayBuffer", "Atomics", "WebAssembly"} {
		// require, module and exports exist only as main.js's own parameters, which eval sees.
		want := "undefined"
		switch n {
		case "require":
			want = "function"
		case "module", "exports":
			want = "object"
		}
		if p[n] != want {
			t.Errorf("typeof %s = %v, want %s", n, p[n], want)
		}
	}
	if p["viaFunction"] != "undefined undefined" {
		t.Errorf("Function sees %v", p["viaFunction"])
	}
	if p["dynamicImport"] == "allowed" {
		t.Error("dynamic import() succeeded")
	}
	allowed := map[string]bool{}
	for _, n := range strings.Fields(`fd globalThis Object Function Array String Number Boolean Symbol BigInt
		RegExp Date Math JSON Reflect Proxy Promise Map Set WeakMap WeakSet WeakRef FinalizationRegistry
		Error TypeError RangeError SyntaxError ReferenceError EvalError URIError AggregateError
		ArrayBuffer DataView Int8Array Uint8Array Uint8ClampedArray Int16Array Uint16Array Int32Array
		Uint32Array Float32Array Float64Array BigInt64Array BigUint64Array Iterator
		NaN Infinity undefined eval isNaN isFinite parseInt parseFloat
		encodeURI encodeURIComponent decodeURI decodeURIComponent escape unescape`) {
		allowed[n] = true
	}
	for _, g := range p["globals"].([]any) {
		if !allowed[g.(string)] {
			t.Errorf("unexpected global %q: judge whether it reaches the host, then list it", g)
		}
	}
	if got := join(p["fd"]); got != "exec fetch hab log settings state" {
		t.Errorf("fd holds %s", got)
	}
	if got := join(p["hab"]); got != "act get" {
		t.Errorf("fd.hab holds %s", got)
	}
	if got := join(p["state"]); got != "get set" {
		t.Errorf("fd.state holds %s", got)
	}
	if p["frozen"] != true {
		t.Error("the bridge objects are not frozen")
	}
	if p["afterReplace"] != "PowerError 2" {
		t.Errorf("after replacing JSON and Error: %v", p["afterReplace"])
	}
}

func join(v any) string {
	var s []string
	for _, x := range v.([]any) {
		s = append(s, x.(string))
	}
	return strings.Join(s, " ")
}

func TestCutText(t *testing.T) {
	if got := cutText("abcdef", 4); got != "abc…" {
		t.Errorf("cutText = %q", got)
	}
	if got := cutText("abc", 4); got != "abc" {
		t.Errorf("cutText = %q", got)
	}
	if s, c := cut([]byte("aé"), 2); s != "a" || !c {
		t.Errorf("cut = %q %v", s, c)
	}
}

// TestHostileValues feeds the runtime values whose reading runs module code that throws or loops:
// none may crash the host or outlive the call's limit.
func TestHostileValues(t *testing.T) {
	ctx := context.Background()
	m := newModule(t, writeBundle(t, "hostile", `"exec", "fetch"`, `
exports.render = function (ctx) {
  switch (ctx.place) {
  case "board": return { type: "text", get text() { throw new Error("getter threw"); } };
  case "host": return { type: "text", get text() { for (;;) {} } };
  case "session": return new Proxy({}, { ownKeys: function () { throw new Error("trap threw"); } });
  case "sheet": throw { get name() { throw 1; }, get message() { for (;;) {} } };
  }
};
exports.act = function (name) {
  if (name === "argv.hole") fd.exec(["ls", , "x"]);
  if (name === "header.getter") fd.fetch("https://example.invalid/", { headers: { get a() { throw new Error("no"); } } });
  if (name === "symbol.name") { var e = new Error("m"); e.name = Symbol("s"); throw e; }
};
`), Options{Backends: Backends{Exec: &fakeExec{}, Fetch: &fakeFetch{}}})
	render := func(place string) error {
		c := fixedCall
		c.Place = place
		_, err := m.Render(ctx, c)
		return err
	}
	var se *ScriptError
	if err := render("board"); !errors.As(err, &se) || se.Message != "getter threw" {
		t.Errorf("throwing getter: %v", err)
	}
	var te *TimeoutError
	if err := render("host"); !errors.As(err, &te) {
		t.Errorf("looping getter: %v", err)
	}
	// A proxy is refused before its traps run.
	var tre *TreeError
	if err := render("session"); !errors.As(err, &tre) || !strings.Contains(err.Error(), "a Proxy cannot cross") {
		t.Errorf("throwing proxy: %v", err)
	}
	start := time.Now()
	if err := render("sheet"); err == nil {
		t.Error("a hostile thrown object passed")
	} else if time.Since(start) > 3*time.Second {
		t.Errorf("a hostile thrown object held the call for %s", time.Since(start))
	}
	if err := m.Act(ctx, "argv.hole", nil, fixedCall); !errors.As(err, &se) || se.Name != BridgeError {
		t.Errorf("argv with a hole: %v", err)
	}
	if err := m.Act(ctx, "header.getter", nil, fixedCall); !errors.As(err, &se) || se.Message != "no" {
		t.Errorf("header getter: %v", err)
	}
	if err := m.Act(ctx, "symbol.name", nil, fixedCall); err == nil {
		t.Error("an error named by a symbol passed")
	}
	c := fixedCall
	c.Place = "unknown"
	if _, err := m.Render(ctx, c); !errors.As(err, &tre) || !strings.Contains(err.Error(), "returned nothing") {
		t.Errorf("render after the hostile calls: %v, want the module still running", err)
	}
	// A getter on module.exports that throws fails the load, without a panic.
	_, err := New(writeBundle(t, "hostile-exports", "", `
Object.defineProperty(module, "exports", { get: function () { throw new Error("no exports"); } });
`), Options{})
	if !errors.As(err, &se) || se.Func != "load" || se.Message != "no exports" {
		t.Errorf("throwing exports getter: %v", err)
	}
}
