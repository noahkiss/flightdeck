package modhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/noahkiss/flightdeck/api/modules"
)

// Options configure a Module.
type Options struct {
	Backends
	Limits Limits
	// Settings are the person's values for the module's settings. Keys the manifest does not
	// declare are ignored; a declared key with no value takes the manifest's default.
	Settings map[string]any
}

// Call is the context of one call: the ctx argument a module's function receives (modules.md
// 7.1).
type Call struct {
	Host  string // the host's name
	OS    string // its operating system, as Go names it
	Place string // the place being drawn: board, host, session or sheet
	// Vocabulary is the newest block vocabulary the client draws. Zero means this hab's
	// newest. Render checks the tree against min(Vocabulary, modules.Version).
	Vocabulary int
	Now        time.Time // zero means time.Now()
}

// phase is what the module's code is running: the bridge allows less in load and render.
type phase int

const (
	phaseLoad phase = iota
	phaseRender
	phaseRefresh
	phaseAct
)

func (p phase) String() string {
	return [...]string{"load", "render", "refresh", "act"}[p]
}

// Module is one loaded module in its own goja runtime. Its methods run one at a time, in the
// order they are called (modules.md 12: actions run one at a time per module).
type Module struct {
	b      *Bundle
	opts   Options
	limits Limits
	powers map[string]bool

	mu sync.Mutex
	r  *goja.Runtime
	// err is set when the module stops (a MemoryError, an InternalError, a stuck call); every
	// later call returns ErrStopped. Only run's own goroutine writes it, under mu.
	err error

	// Captured before any module code runs, so a module that replaces the globals cannot
	// change how the bridge parses JSON or builds errors.
	jsonParse goja.Callable
	errorCtor goja.Value
	freeze    goja.Callable

	// The running call. The interrupt guard keeps a late timer from interrupting the next call.
	cur     *callState
	imu     sync.Mutex
	iactive bool

	files    map[string]*goja.Object // module objects of required files, by path
	fns      map[string]goja.Callable
	exported map[string]bool // the names in fns, kept when a stop drops the runtime

	// State: committed is what the Store holds; state is what the running call sees. The first
	// fd.state.set of a call copies committed; run saves once at the call's end, or drops the
	// writes when the call failed (modules.md 7.3).
	committed map[string]json.RawMessage
	csize     int
	state     map[string]json.RawMessage
	size      int
	dirty     bool

	growth atomic.Int64 // heap change over the last call, in bytes
}

type callState struct {
	ctx   context.Context
	phase phase
	limit time.Duration
	logs  int // fd.log lines in this call
}

// regexMatchTimeout bounds one regexp2 match (goja uses regexp2 for patterns with
// backreferences or lookaround). A match past it throws a RangeError the module may catch.
// regexp2 checks its own clock, so this holds where the interrupt cannot reach.
const regexMatchTimeout = 250 * time.Millisecond

var regexOnce sync.Once

// stuckGrace is how long run waits for the runtime after the call's context ends. A runtime
// still busy then is in a built-in that never checks the interrupt; run abandons it.
const stuckGrace = 250 * time.Millisecond

// New loads a bundle into a fresh runtime: it reads the module's state, runs main.js's top level
// under Limits.Load, and finds the exported functions. main.js is CommonJS: it sets
// module.exports (or properties of exports) to an object with render and, optionally, refresh
// and act; require("./file.js") loads another file of the bundle (modules.md 7.3).
func New(b *Bundle, opts Options) (*Module, error) {
	// regexp2 reads the package value when a pattern compiles, so it is set once before any
	// module code runs.
	regexOnce.Do(func() { regexp2.DefaultMatchTimeout = regexMatchTimeout })
	m := &Module{
		b:        b,
		opts:     opts,
		limits:   opts.Limits.withDefaults(),
		powers:   map[string]bool{},
		r:        goja.New(),
		files:    map[string]*goja.Object{},
		fns:      map[string]goja.Callable{},
		exported: map[string]bool{},
		state:    map[string]json.RawMessage{},
	}
	for _, p := range b.Manifest.Powers {
		m.powers[p] = true
	}
	m.r.SetMaxCallStackSize(m.limits.Stack)
	// goja's parser reads the file a "//# sourceMappingURL=" comment names, from the host's
	// disk. eval and new Function compile with these options; require passes the same.
	m.r.SetParserOptions(parser.WithDisableSourceMaps)
	// GoError is goja's own error type for wrapped Go errors. The bridge never wraps one, and a
	// module needs no constructor for it.
	_ = m.r.GlobalObject().Delete("GoError")
	jsonObj := m.r.Get("JSON").ToObject(m.r)
	m.jsonParse, _ = goja.AssertFunction(jsonObj.Get("parse"))
	m.errorCtor = m.r.Get("Error")
	m.freeze, _ = goja.AssertFunction(m.r.Get("Object").ToObject(m.r).Get("freeze"))

	if opts.State != nil {
		st, err := opts.State.Load(b.Manifest.ID)
		if err != nil {
			return nil, fmt.Errorf("module state: %w", err)
		}
		for k, v := range st {
			m.state[k] = v
			m.size += len(k) + len(v)
		}
	}
	m.committed, m.csize = m.state, m.size
	if err := m.installBridge(); err != nil {
		return nil, err
	}
	// Requiring main.js and reading its exports can run module code (a getter on exports), so
	// both run as a guarded JS call under the load limit.
	var bad *BundleError
	err := m.run(context.Background(), phaseLoad, m.limits.Load, func() (goja.Value, error) {
		var rerr error
		gerr := m.guard(func() {
			var exports goja.Value
			exports, rerr = m.require(MainFile)
			if rerr != nil {
				return
			}
			obj, ok := exports.(*goja.Object)
			if !ok {
				bad = &BundleError{Path: MainFile, Message: "module.exports is not an object"}
				return
			}
			for _, name := range []string{"refresh", "render", "act"} {
				v := obj.Get(name)
				if v == nil || goja.IsUndefined(v) {
					continue
				}
				fn, ok := goja.AssertFunction(v)
				if !ok {
					bad = &BundleError{Path: MainFile, Message: name + " is exported but is not a function"}
					return
				}
				m.fns[name] = fn
				m.exported[name] = true
			}
		})
		if gerr != nil {
			return nil, gerr
		}
		return nil, rerr
	})
	if err != nil {
		return nil, err
	}
	if bad != nil {
		return nil, bad
	}
	if m.fns["render"] == nil {
		return nil, &BundleError{Path: MainFile, Message: "exports no render function"}
	}
	return m, nil
}

// Manifest returns the module's manifest.
func (m *Module) Manifest() *modules.Manifest { return m.b.Manifest }

// Has reports whether the module exports the function: refresh, render or act.
func (m *Module) Has(name string) bool { return m.exported[name] }

// HeapGrowth is how much the Go heap grew over the module's last finished call, in bytes; it is
// negative when the heap shrank. The heap is the process's, and it counts garbage not yet
// collected, so this is a hint: the host compares modules' growth to find the one that holds
// memory (modules.md 8.4).
func (m *Module) HeapGrowth() int64 { return m.growth.Load() }

// Refresh runs the module's refresh under Limits.Refresh. A module without one succeeds at once.
func (m *Module) Refresh(ctx context.Context, c Call) error {
	if !m.Has("refresh") {
		return nil
	}
	return m.run(ctx, phaseRefresh, m.limits.Refresh, func() (goja.Value, error) {
		return m.settle(m.fns["refresh"](goja.Undefined(), m.ctxValue(c)))
	})
}

// Render runs render under Limits.Render and returns the tree as JSON: text over a field's limit
// cut with an ellipsis (modules.md 6.1), then checked with modules.ValidateTree against the
// client's vocabulary. A bad tree is a *TreeError.
func (m *Module) Render(ctx context.Context, c Call) ([]byte, error) {
	v := c.Vocabulary
	if v == 0 || v > modules.Version {
		v = modules.Version
	}
	if int64(v) < m.b.Manifest.Blocks {
		return nil, &ClientTooOld{Vocabulary: v, Blocks: m.b.Manifest.Blocks}
	}
	c.Vocabulary = v
	var raw []byte
	err := m.run(ctx, phaseRender, m.limits.Render, func() (goja.Value, error) {
		res, err := m.settle(m.fns["render"](goja.Undefined(), m.ctxValue(c)))
		if err != nil {
			return nil, err
		}
		if res == nil || goja.IsUndefined(res) {
			return nil, &TreeError{Problems: modules.Problems{{Message: "render returned nothing"}}}
		}
		// The bound is above the tree limit so that long text can still be cut to fit. Reading
		// the result can run module code (getters, proxies, toJSON), so it runs as a JS call:
		// an exception or an interrupt comes back as an error, never as a panic.
		if gerr := m.guard(func() { raw, err = exportJSON(m.r, res, renderExportMax) }); gerr != nil {
			return nil, gerr
		}
		var ve *valueError
		if errors.As(err, &ve) {
			return nil, &TreeError{Problems: modules.Problems{{Message: ve.msg}}}
		}
		return res, err
	})
	if err != nil {
		return nil, err
	}
	tree, err := cutTree(raw)
	if err != nil {
		return nil, &TreeError{Problems: modules.Problems{{Message: err.Error()}}}
	}
	if err := modules.ValidateTree(tree, v); err != nil {
		var ps modules.Problems
		if errors.As(err, &ps) {
			return nil, &TreeError{Problems: ps}
		}
		return nil, &TreeError{Problems: modules.Problems{{Message: err.Error()}}}
	}
	return tree, nil
}

// renderExportMax bounds the JSON a render may return before text is cut.
const renderExportMax = 1 << 20

// Act runs act(name, args, ctx) under Limits.Act. args is a JSON object, or empty for {}.
func (m *Module) Act(ctx context.Context, name string, args json.RawMessage, c Call) error {
	if strings.HasPrefix(name, modules.ClientPrefix) {
		return ErrClientAction
	}
	if !m.Has("act") {
		return ErrNoAct
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(args, &probe); err != nil {
		return fmt.Errorf("action args are not a JSON object: %w", err)
	}
	return m.run(ctx, phaseAct, m.limits.Act, func() (goja.Value, error) {
		a, err := m.importJSON(args)
		if err != nil {
			return nil, err
		}
		return m.settle(m.fns["act"](goja.Undefined(), m.r.ToValue(name), a, m.ctxValue(c)))
	})
}

// run runs f with the module's runtime under a time limit and the memory watch, and turns what
// goja returns into the package's typed errors. f runs on its own goroutine: run returns at the
// limit plus stuckGrace even when the runtime never comes back, and a Go panic in f stops the
// module instead of the process.
func (m *Module) run(parent context.Context, ph phase, limit time.Duration, f func() (goja.Value, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return ErrStopped
	}
	before, err := m.heapAtStart()
	if err != nil {
		return err
	}
	base, cancelBase := context.WithCancelCause(parent)
	defer cancelBase(nil)
	ctx, cancel := context.WithTimeoutCause(base, limit, &TimeoutError{Func: ph.String(), Limit: limit})
	defer cancel()

	m.imu.Lock()
	m.iactive = true
	m.imu.Unlock()
	stop := context.AfterFunc(ctx, func() {
		m.imu.Lock()
		defer m.imu.Unlock()
		if m.iactive {
			m.r.Interrupt(context.Cause(ctx))
		}
	})
	watchDone := make(chan struct{})
	if m.limits.Memory > 0 {
		go func() {
			defer close(watchDone)
			watchMemory(ctx, m.limits.Memory, func() {
				cancelBase(&MemoryError{Func: ph.String(), Limit: m.limits.Memory})
			})
		}()
	} else {
		close(watchDone)
	}
	m.cur = &callState{ctx: ctx, phase: ph, limit: limit}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- &InternalError{Func: ph.String(), Value: fmt.Sprint(p), Stack: string(debug.Stack())}
			}
		}()
		_, err := f()
		// Classified while the limits still hold: reading a thrown object's name and message can
		// run module code (getters, toString).
		done <- m.classify(ph, err)
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		t := time.NewTimer(stuckGrace)
		select {
		case err = <-done:
		case <-t.C:
			// The runtime is in a built-in that ignores the interrupt. Leave it: its goroutine
			// still owns the runtime, the call state and the state maps, so nothing here
			// touches them again. The interrupt stays set, so it stops at its next instruction.
			t.Stop()
			stop()
			cancel()
			<-watchDone
			m.err = &TimeoutError{Func: ph.String(), Limit: limit, Stuck: true}
			return m.err
		}
		t.Stop()
	}

	stop()
	m.imu.Lock()
	m.iactive = false
	m.imu.Unlock()
	m.r.ClearInterrupt()
	cancel()
	<-watchDone
	m.cur = nil

	var me *MemoryError
	var ie *InternalError
	if err == nil {
		err = m.heapAtEnd(ph)
	}
	m.growth.Store(int64(heapBytes()) - int64(before))
	if errors.As(err, &me) || errors.As(err, &ie) {
		m.stop(err)
		return err
	}
	return m.endState(err)
}

// heapAtStart samples the heap before a call. A heap over the limit after a collection is not
// this module's doing, since none of its code has run: the call is refused with
// ErrHeapPressure and the module keeps running.
func (m *Module) heapAtStart() (uint64, error) {
	h := heapBytes()
	if m.limits.Memory == 0 || h <= m.limits.Memory {
		return h, nil
	}
	runtime.GC()
	if h = heapBytes(); h > m.limits.Memory {
		return h, fmt.Errorf("%w: %d MiB, the limit is %d MiB", ErrHeapPressure, h>>20, m.limits.Memory>>20)
	}
	return h, nil
}

// heapAtEnd checks the heap after a call that succeeded: the watch samples every few
// milliseconds, so a short call can allocate past the limit between samples. A heap still over
// after a collection was under at the start, so this call kept the memory.
func (m *Module) heapAtEnd(ph phase) error {
	if m.limits.Memory == 0 || heapBytes() <= m.limits.Memory {
		return nil
	}
	runtime.GC()
	if heapBytes() > m.limits.Memory {
		return &MemoryError{Func: ph.String(), Limit: m.limits.Memory}
	}
	return nil
}

// stop marks the module stopped and lets go of its runtime, so the memory a hog holds is
// freed even while the host still holds the Module. Only called once the runtime's goroutine
// has returned.
func (m *Module) stop(err error) {
	m.err = err
	m.r = nil
	m.fns = nil
	m.files = nil
	m.jsonParse, m.errorCtor, m.freeze = nil, nil, nil
	m.state, m.committed = nil, nil
}

// endState saves the call's state writes once, when the call succeeded; a call that failed
// keeps none of them.
func (m *Module) endState(err error) error {
	if !m.dirty {
		return err
	}
	m.dirty = false
	if err == nil && m.opts.State != nil {
		if serr := m.opts.State.Save(m.b.Manifest.ID, m.state); serr != nil {
			err = fmt.Errorf("module state: %w", serr)
		}
	}
	if err != nil {
		m.state, m.size = m.committed, m.csize
		return err
	}
	m.committed, m.csize = m.state, m.size
	return nil
}

func (m *Module) classify(ph phase, err error) error {
	if err == nil {
		return nil
	}
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		if cause, ok := ie.Value().(error); ok {
			return cause // run stops the module on a MemoryError
		}
		return &ScriptError{Func: ph.String(), Message: "interrupted"}
	}
	var ex *goja.Exception
	if errors.As(err, &ex) {
		se := m.scriptError(ex.Value())
		se.Func = ph.String()
		_ = m.guard(func() {
			se.Stack = cutRunes(ex.String(), maxErrorStack)
			if se.Message == "" {
				se.Message = cutRunes(ex.Error(), maxErrorMessage)
			}
		})
		return se
	}
	var se *goja.CompilerSyntaxError
	if errors.As(err, &se) {
		return &ScriptError{Func: ph.String(), Name: "SyntaxError", Message: se.Error()}
	}
	var so *goja.StackOverflowError
	if errors.As(err, &so) {
		return &ScriptError{Func: ph.String(), Name: "RangeError", Message: "call stack too deep"}
	}
	var mine *ScriptError
	if errors.As(err, &mine) && mine.Func == "" {
		mine.Func = ph.String()
	}
	return err
}

// Bounds on what a ScriptError carries: a module's message can be as long as it likes, and
// regexp2's timeout message holds the whole input.
const (
	maxErrorMessage = 2000 // runes
	maxErrorStack   = 8000 // runes
)

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// scriptError reads a thrown value's name and message. Both may run module code, so they are
// read as a guarded JS call; a getter that throws or loops leaves the field empty.
func (m *Module) scriptError(v goja.Value) *ScriptError {
	se := &ScriptError{}
	_ = m.guard(func() {
		if o, ok := v.(*goja.Object); ok {
			if n := o.Get("name"); n != nil && !goja.IsUndefined(n) {
				se.Name = cutRunes(n.String(), 100)
			}
			if msg := o.Get("message"); msg != nil && !goja.IsUndefined(msg) {
				se.Message = cutRunes(msg.String(), maxErrorMessage)
			}
		} else if v != nil {
			se.Message = cutRunes(v.String(), maxErrorMessage)
		}
	})
	return se
}

// settle waits out a returned promise: bridge calls are synchronous and there are no timers, so
// an async function has settled by the time the call returns, and goja has run its jobs. A
// promise still pending awaits something that never comes.
func (m *Module) settle(v goja.Value, err error) (goja.Value, error) {
	if err != nil {
		return nil, err
	}
	o, ok := v.(*goja.Object)
	if !ok {
		return v, nil
	}
	if o.ExportType() != promiseType {
		return v, nil
	}
	// Export of a promise returns it without reading its properties; of another object, it
	// would run every getter outside a JS call.
	p, ok := o.Export().(*goja.Promise)
	if !ok {
		return v, nil
	}
	switch p.State() {
	case goja.PromiseStateFulfilled:
		return p.Result(), nil
	case goja.PromiseStateRejected:
		return nil, m.scriptError(p.Result())
	}
	return nil, &ScriptError{Name: BridgeError, Message: "the returned promise never settled"}
}

// ctxValue builds the frozen ctx argument.
func (m *Module) ctxValue(c Call) goja.Value {
	now := c.Now
	if now.IsZero() {
		now = time.Now()
	}
	o := m.r.NewObject()
	_ = o.Set("host", c.Host)
	_ = o.Set("os", c.OS)
	_ = o.Set("place", c.Place)
	_ = o.Set("vocabulary", c.Vocabulary)
	_ = o.Set("now", now.UTC().Format(time.RFC3339))
	_, _ = m.freeze(goja.Undefined(), o)
	return o
}

// require loads a bundle file as a CommonJS module, once, and returns its exports.
func (m *Module) require(p string) (goja.Value, error) {
	if mod, ok := m.files[p]; ok {
		return mod.Get("exports"), nil
	}
	src, ok := m.b.Sources[p]
	if !ok {
		return nil, fmt.Errorf("no file %s", p)
	}
	prog, err := compileBody(p, src)
	if err != nil {
		return nil, err
	}
	wrapper, err := m.r.RunProgram(prog)
	if err != nil {
		return nil, err
	}
	fn, _ := goja.AssertFunction(wrapper)
	mod := m.r.NewObject()
	exports := m.r.NewObject()
	_ = mod.Set("exports", exports)
	m.files[p] = mod // set before running, so a cycle gets the partial exports
	dir := path.Dir(p)
	req := m.r.ToValue(func(call goja.FunctionCall) goja.Value {
		spec := call.Argument(0)
		s, ok := jsString(spec)
		if !ok {
			panic(m.newError(BridgeError, "require takes a relative path such as \"./util.js\""))
		}
		target, rerr := m.resolve(dir, s)
		if rerr != "" {
			panic(m.newError(BridgeError, rerr))
		}
		v, err := m.require(target)
		if err != nil {
			var ex *goja.Exception
			if errors.As(err, &ex) {
				panic(ex)
			}
			var se *goja.CompilerSyntaxError
			if errors.As(err, &se) {
				panic(m.newError("SyntaxError", se.Error()))
			}
			panic(m.newError(BridgeError, err.Error()))
		}
		return v
	})
	if _, err := fn(goja.Undefined(), exports, req, mod); err != nil {
		delete(m.files, p)
		return nil, err
	}
	return mod.Get("exports"), nil
}

// compileBody compiles a file's source as the body of function (exports, require, module). It
// parses the wrapped text and checks that the result is that one function: a source that
// closes the function early, to run code outside it, is a syntax error.
func compileBody(name, src string) (*goja.Program, error) {
	prg, err := goja.Parse(name, "(function (exports, require, module) {"+src+"\n})", parser.WithDisableSourceMaps)
	if err != nil {
		return nil, err
	}
	ok := false
	if len(prg.Body) == 1 {
		if st, isExpr := prg.Body[0].(*ast.ExpressionStatement); isExpr {
			_, ok = st.Expression.(*ast.FunctionLiteral)
		}
	}
	if !ok {
		return nil, &goja.CompilerSyntaxError{CompilerError: goja.CompilerError{
			Message: name + ": the file closes the module function early",
		}}
	}
	return goja.CompileAST(prg, false)
}

// resolve turns a require path into a bundle path. Only relative paths inside the bundle
// resolve; ".js" is added when the path has no extension.
func (m *Module) resolve(dir, spec string) (string, string) {
	if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
		return "", fmt.Sprintf("require(%q): only a relative path to a file of the module, such as \"./util.js\"", spec)
	}
	p := path.Clean(path.Join(dir, spec))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Sprintf("require(%q): outside the module", spec)
	}
	if path.Ext(p) == "" {
		p += ".js"
	}
	if _, ok := m.b.Sources[p]; !ok {
		return "", fmt.Sprintf("require(%q): the module has no file %s", spec, p)
	}
	return p, ""
}

// newError builds a JS Error with the given name, with the constructor captured at New.
func (m *Module) newError(name, msg string) *goja.Object {
	o, err := m.r.New(m.errorCtor, m.r.ToValue(msg))
	if err != nil {
		o = m.r.NewObject()
		_ = o.Set("message", msg)
	}
	_ = o.Set("name", name)
	return o
}

// guard runs f as a native JS function, so goja turns an exception or an interrupt that module
// code raises inside f into an error.
func (m *Module) guard(f func()) error {
	fn, _ := goja.AssertFunction(m.r.ToValue(func(goja.FunctionCall) goja.Value {
		f()
		return goja.Undefined()
	}))
	_, err := fn(goja.Undefined())
	return err
}

var promiseType = reflect.TypeFor[*goja.Promise]()

// importJSON parses JSON into a JS value with the JSON.parse captured at New.
func (m *Module) importJSON(raw json.RawMessage) (goja.Value, error) {
	if len(raw) == 0 {
		return goja.Undefined(), nil
	}
	return m.jsonParse(goja.Undefined(), m.r.ToValue(string(raw)))
}
