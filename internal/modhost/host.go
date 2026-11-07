package modhost

// gff-hab's side of the module host process (modules.md 4, 7.4, 8.2): Host starts the child,
// restarts it with backoff when it ends, kills it when it wedges or holds too much memory, and
// answers with a HostError while it is down. Remote is a module loaded in the child, with the
// same calls as Module.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noahkiss/flightdeck/api/modules"
)

// Runner is a loaded module: a Module in this process, or a Remote in the module host.
type Runner interface {
	Manifest() *modules.Manifest
	Has(name string) bool
	HeapGrowth() int64
	Refresh(ctx context.Context, c Call) error
	Render(ctx context.Context, c Call) ([]byte, error)
	Act(ctx context.Context, name string, args json.RawMessage, c Call) error
}

var (
	_ Runner = (*Module)(nil)
	_ Runner = (*Remote)(nil)
)

// HostOptions configure a Host. Zero values take the defaults of modules.md 8.2.
type HostOptions struct {
	// Command makes the module host's command, a process that runs ServeChild on its stdin and
	// stdout ("gff-hab module-host"). The Host sets Stdin, Stdout and Stderr. Required.
	Command func() *exec.Cmd
	// MaxMemory is the child's resident memory above which it is killed: 512 MiB.
	MaxMemory uint64
	// Log takes the Host's own lines (a start, an exit and why, a kill) and the first lines the
	// child writes on stderr. Nil drops them.
	Log func(line string)

	PingEvery   time.Duration // 1 s: how often the child is asked for its memory
	PingTimeout time.Duration // 10 s: a child that answers no ping for this long is wedged
	// WedgeGrace is how long past a call's limit and the runtime's own stuck grace the Host
	// waits for an answer before it kills the child: 2 s.
	WedgeGrace time.Duration
	BackoffMin time.Duration // 250 ms: the first restart's wait
	BackoffMax time.Duration // 30 s
	// StableAfter is how long a child must run for the next restart to wait BackoffMin again:
	// 60 s.
	StableAfter time.Duration
}

func (o HostOptions) withDefaults() HostOptions {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if o.MaxMemory == 0 {
		o.MaxMemory = 512 << 20
	}
	set(&o.PingEvery, time.Second)
	set(&o.PingTimeout, 10*time.Second)
	set(&o.WedgeGrace, 2*time.Second)
	set(&o.BackoffMin, 250*time.Millisecond)
	set(&o.BackoffMax, 30*time.Second)
	set(&o.StableAfter, time.Minute)
	return o
}

// ErrHostDown is what a HostError wraps: the module host is not running, or stopped during the
// call.
var ErrHostDown = errors.New("the module host is not running")

// ErrHostClosed is returned by every call after Host.Close, and by a Remote's calls after its
// Close.
var ErrHostClosed = errors.New("the module host is closed")

// HostError is a call the module host could not answer: it was down, or it ended while the
// call ran (modules.md 8.2).
type HostError struct {
	Func   string // the call: load, refresh, render or act
	Reason string // why the host is down: its exit, a crash's first line, or why it was killed
	// Culprit is a call that was running when the host crashed or was killed for memory, or
	// the call whose wedge got it killed. The module counts one failure (8.3). A call refused
	// while the host was down, or cut off by another module's wedge, is not a culprit.
	Culprit bool
	// RestartAt is when the Host starts the child again; zero when it is starting now.
	RestartAt time.Time
}

func (e *HostError) Error() string {
	s := "module host: " + e.Reason
	if !e.RestartAt.IsZero() {
		if d := time.Until(e.RestartAt).Round(100 * time.Millisecond); d > 0 {
			s += "; it starts again in " + d.String()
		}
	}
	return s
}

func (e *HostError) Unwrap() error { return ErrHostDown }

// Host supervises one module host process. Its methods are safe to call from many goroutines.
type Host struct {
	o HostOptions

	mu        sync.Mutex
	cur       *proc
	gen       uint64
	closed    bool
	reason    string    // why the last child ended
	restartAt time.Time // no start before this
	backoff   time.Duration
	timer     *time.Timer
	mods      map[uint64]*Remote
	nextMod   uint64
	procs     sync.WaitGroup
}

// NewHost returns a Host. It starts the child at the first New, not before.
func NewHost(o HostOptions) *Host {
	o = o.withDefaults()
	return &Host{o: o, mods: map[uint64]*Remote{}, backoff: o.BackoffMin}
}

func (h *Host) logf(format string, args ...any) {
	if h.o.Log != nil {
		h.o.Log(fmt.Sprintf(format, args...))
	}
}

// New loads a bundle in the module host, as modhost.New does in process, and returns the
// same errors. A second module with an id the Host already holds is refused (modules.md 7.3).
func (h *Host) New(b *Bundle, opts Options) (*Remote, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrHostClosed
	}
	for _, r := range h.mods {
		if r.b.Manifest.ID == b.Manifest.ID {
			h.mu.Unlock()
			return nil, fmt.Errorf("a module with id %q is already loaded", b.Manifest.ID)
		}
	}
	h.nextMod++
	r := &Remote{h: h, id: h.nextMod, b: b, opts: opts, limits: opts.Limits.withDefaults()}
	h.mods[r.id] = r
	h.mu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.ready(context.Background()); err != nil {
		h.drop(r)
		return nil, err
	}
	return r, nil
}

func (h *Host) drop(r *Remote) {
	h.mu.Lock()
	delete(h.mods, r.id)
	h.mu.Unlock()
}

func (h *Host) remote(id uint64) *Remote {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mods[id]
}

// running returns the child, starting it when it is due. While a restart waits out its
// backoff, it returns a HostError at once.
func (h *Host) running(fn string) (*proc, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrHostClosed
	}
	if h.cur != nil {
		return h.cur, nil
	}
	if time.Now().Before(h.restartAt) {
		return nil, &HostError{Func: fn, Reason: h.reason, RestartAt: h.restartAt}
	}
	if err := h.startLocked(); err != nil {
		return nil, &HostError{Func: fn, Reason: h.reason, RestartAt: h.restartAt}
	}
	return h.cur, nil
}

// startLocked starts a child. On failure it sets the reason and the next start's time.
func (h *Host) startLocked() error {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.gen++
	p, err := startProc(h, h.gen)
	if err != nil {
		h.reason = "could not start: " + err.Error()
		h.logf("module host %s", h.reason)
		h.scheduleLocked()
		return err
	}
	h.cur = p
	h.logf("module host started (pid %d)", p.cmd.Process.Pid)
	return nil
}

// scheduleLocked sets the next start after the backoff, and starts it then when modules are
// loaded.
func (h *Host) scheduleLocked() {
	wait := h.backoff
	h.backoff = min(h.backoff*2, h.o.BackoffMax)
	h.restartAt = time.Now().Add(wait)
	if len(h.mods) == 0 || h.closed {
		return
	}
	h.timer = time.AfterFunc(wait, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed || h.cur != nil || len(h.mods) == 0 {
			return
		}
		_ = h.startLocked()
	})
}

// ended is called once a child has exited and every request on it has failed.
func (h *Host) ended(p *proc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur != p {
		return
	}
	h.cur = nil
	h.reason = p.reason
	if h.closed {
		return
	}
	if time.Since(p.started) >= h.o.StableAfter {
		h.backoff = h.o.BackoffMin
	}
	h.logf("module host stopped (pid %d): %s", p.cmd.Process.Pid, p.reason)
	h.scheduleLocked()
}

// Close stops the child: it closes its stdin, so a healthy child exits, and kills it when it
// has not after a second. Every later call returns ErrHostClosed.
func (h *Host) Close() error {
	h.mu.Lock()
	h.closed = true
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	p := h.cur
	h.mu.Unlock()
	if p != nil {
		_ = p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(time.Second):
			p.kill("closed", 0)
		}
	}
	h.procs.Wait()
	return nil
}

// Pid is the running child's process id, 0 when none runs. For tests and gff-hab's log.
func (h *Host) Pid() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil {
		return 0
	}
	return h.cur.cmd.Process.Pid
}

// proc is one run of the child.
type proc struct {
	h       *Host
	gen     uint64
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	c       *conn
	started time.Time
	ctx     context.Context // ends with the child: the backends' calls end with it
	stop    context.CancelFunc
	done    chan struct{} // closed once the child exited and its requests failed

	mu       sync.Mutex
	reason   string // why it ended; the first kill's reason wins
	culprit  uint64 // the Remote a kill blames; 0 blames every call that ran
	backend  map[uint64]context.CancelFunc
	lastPong time.Time
	mem      uint64
	stderr   *headBuffer
}

func startProc(h *Host, gen uint64) (*proc, error) {
	if h.o.Command == nil {
		return nil, errors.New("no command")
	}
	cmd := h.o.Command()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &proc{h: h, gen: gen, cmd: cmd, stdin: stdin, c: newConn(stdin), done: make(chan struct{}),
		backend: map[uint64]context.CancelFunc{}, stderr: &headBuffer{max: 8 << 10}}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.started = time.Now()
	p.lastPong = p.started
	p.ctx, p.stop = context.WithCancel(context.Background())
	h.procs.Add(1)
	go p.read(stdout)
	go p.watch()
	return p, nil
}

// kill ends the child by its pid, which this Host started. The first reason is kept.
func (p *proc) kill(reason string, culprit uint64) {
	p.mu.Lock()
	if p.reason == "" {
		p.reason = reason
		p.culprit = culprit
		p.h.logf("module host (pid %d) killed: %s", p.cmd.Process.Pid, reason)
	}
	p.mu.Unlock()
	_ = p.cmd.Process.Kill()
}

func (p *proc) read(stdout io.Reader) {
	defer p.h.procs.Done()
	br := bufio.NewReaderSize(stdout, 64<<10)
	for {
		f, err := readFrame(br)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				p.kill(err.Error(), 0)
			}
			break
		}
		switch {
		case f.Re != 0:
			p.c.answered(f)
		case f.Op == opLog:
			var line string
			if r := p.h.remote(f.Mod); r != nil && r.opts.Log != nil && json.Unmarshal(f.Body, &line) == nil {
				r.opts.Log(line)
			}
		case f.Op == opCancel:
			p.mu.Lock()
			cancel := p.backend[f.ID]
			p.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		default:
			ctx, cancel := context.WithCancel(p.ctx)
			p.mu.Lock()
			p.backend[f.ID] = cancel
			p.mu.Unlock()
			go p.serve(ctx, f)
		}
	}
	werr := p.cmd.Wait()
	p.stop()
	p.mu.Lock()
	if p.reason == "" {
		p.reason = exitReason(werr, p.stderr.firstLine())
	}
	p.mu.Unlock()
	if lines := p.stderr.lines(10); lines != "" {
		p.h.logf("module host (pid %d) stderr:\n%s", p.cmd.Process.Pid, lines)
	}
	p.c.end()
	close(p.done)
	p.h.ended(p)
}

func exitReason(err error, line string) string {
	s := "exited"
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		s = "exited: " + ee.ProcessState.String()
	default:
		s = "exited: " + err.Error()
	}
	if line != "" {
		s += ": " + line
	}
	return s
}

// watch pings the child and checks its memory until it ends (modules.md 8.2).
func (p *proc) watch() {
	t := time.NewTicker(p.h.o.PingEvery)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
		}
		p.mu.Lock()
		quiet := time.Since(p.lastPong)
		mem := p.mem
		p.mu.Unlock()
		if quiet > p.h.o.PingTimeout {
			p.kill(fmt.Sprintf("answered no ping for %s", quiet.Round(time.Second)), 0)
			return
		}
		if rss := residentMemory(p.cmd.Process.Pid); rss > 0 {
			mem = rss
		}
		if mem > p.h.o.MaxMemory {
			p.kill(fmt.Sprintf("held %d MiB, over the limit of %d MiB", mem>>20, p.h.o.MaxMemory>>20), 0)
			return
		}
		go p.ping()
	}
}

func (p *proc) ping() {
	id, ch, ok := p.c.start()
	if !ok {
		return
	}
	if p.c.send(frame{ID: id, Op: opPing}) != nil {
		p.c.forget(id)
		return
	}
	select {
	case f, ok := <-ch:
		var pong wirePong
		if ok && f.Err == nil && json.Unmarshal(f.Body, &pong) == nil {
			p.mu.Lock()
			p.lastPong, p.mem = time.Now(), pong.Mem
			p.mu.Unlock()
		}
	case <-time.After(p.h.o.PingTimeout):
		p.c.forget(id)
	}
}

// residentMemory reads a process's resident size where the system shows it without cgo
// (Linux); elsewhere 0, and the child's own count is used.
func residentMemory(pid int) uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(data))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}

// serve runs one backend call of the child for the module f.Mod names.
func (p *proc) serve(ctx context.Context, f frame) {
	defer func() {
		p.mu.Lock()
		cancel := p.backend[f.ID]
		delete(p.backend, f.ID)
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}()
	defer func() {
		if v := recover(); v != nil {
			_, rt := v.(runtime.Error)
			_ = p.c.send(frame{Re: f.ID, Err: &wireError{Kind: kindPanic, Message: fmt.Sprint(v), Runtime: rt}})
		}
	}()
	r := p.h.remote(f.Mod)
	if r == nil {
		_ = p.c.reply(f.ID, nil, errors.New("the module is unloaded"))
		return
	}
	body, err := r.backend(ctx, f)
	_ = p.c.reply(f.ID, body, err)
}

// request sends one call to the child and waits for its answer. When ctx ends it tells the
// child; when no answer comes by limit plus the runtime's stuck grace plus WedgeGrace (counted
// again from a cancel), it kills the child. A child that ends returns a HostError.
func (p *proc) request(ctx context.Context, fn, op string, mod uint64, body any, limit time.Duration) (frame, error) {
	id, ch, ok := p.c.start()
	if !ok {
		return frame{}, p.failed(fn, mod)
	}
	f := frame{ID: id, Op: op, Mod: mod}
	if body != nil {
		f.Body = marshalBody(body)
	}
	if err := p.c.send(f); err != nil {
		p.c.forget(id)
		if p.c.closedNow() {
			return frame{}, p.failed(fn, mod)
		}
		return frame{}, err
	}
	wedge := limit + stuckGrace + p.h.o.WedgeGrace
	timer := time.NewTimer(wedge)
	defer timer.Stop()
	cancelled := ctx.Done()
	for {
		select {
		case ans, ok := <-ch:
			if !ok {
				return frame{}, p.failed(fn, mod)
			}
			return ans, nil
		case <-cancelled:
			cancelled = nil
			_ = p.c.send(frame{ID: id, Op: opCancel})
			timer.Reset(stuckGrace + p.h.o.WedgeGrace)
		case <-timer.C:
			p.kill(fmt.Sprintf("%s did not answer within %s", fn, wedge), mod)
		}
	}
}

// failed waits for the child's end and returns the HostError for a call on it.
func (p *proc) failed(fn string, mod uint64) error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return &HostError{Func: fn, Reason: p.reason, Culprit: p.culprit == 0 || p.culprit == mod}
}

func (c *conn) closedNow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// headBuffer keeps the first bytes the child writes on stderr: a crash's message comes first,
// its goroutine dump after.
type headBuffer struct {
	mu  sync.Mutex
	max int
	b   bytes.Buffer
}

func (h *headBuffer) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room := h.max - h.b.Len(); room > 0 {
		h.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (h *headBuffer) firstLine() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range strings.Split(h.b.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return cutRunes(l, 300)
		}
	}
	return ""
}

func (h *headBuffer) lines(n int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ls := strings.SplitN(strings.TrimRight(h.b.String(), "\n"), "\n", n+1)
	if len(ls) > n {
		ls = ls[:n]
	}
	return strings.TrimSpace(strings.Join(ls, "\n"))
}

// Remote is a module loaded in the module host. Its calls are Module's, and return Module's
// errors, plus HostError while the host is down or when it ends during a call. After the host
// restarts, the next call loads the module into the new child again. A call that stops the
// module in the child (MemoryError, InternalError, a stuck TimeoutError) makes every later call
// return ErrStopped, as in process; a stuck call also restarts the child, since its abandoned
// runtime may never stop.
type Remote struct {
	h      *Host
	id     uint64
	b      *Bundle
	opts   Options
	limits Limits

	mu       sync.Mutex
	gen      uint64 // the child it is loaded in; 0 for none
	exported map[string]bool
	err      error
	closed   bool
	growth   atomic.Int64
}

// Manifest returns the module's manifest.
func (r *Remote) Manifest() *modules.Manifest { return r.b.Manifest }

// Has reports whether the module exports the function: refresh, render or act.
func (r *Remote) Has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exported[name]
}

// HeapGrowth is Module.HeapGrowth of the module's last call in the child.
func (r *Remote) HeapGrowth() int64 { return r.growth.Load() }

// ready makes sure the module is loaded in the running child, and returns it. r.mu is held.
func (r *Remote) ready(ctx context.Context) (*proc, error) {
	if r.closed {
		return nil, ErrHostClosed
	}
	if r.err != nil {
		return nil, ErrStopped
	}
	p, err := r.h.running("load")
	if err != nil {
		return nil, err
	}
	if r.gen == p.gen {
		return p, nil
	}
	o := r.opts
	l := wireLoad{Dir: r.b.Dir, Manifest: r.b.Manifest, Sources: r.b.Sources, Settings: o.Settings, Limits: o.Limits,
		Hab: o.Hab != nil, Exec: o.Exec != nil, Fetch: o.Fetch != nil, State: o.State != nil, Log: o.Log != nil}
	ans, err := p.request(ctx, "load", opLoad, r.id, l, r.limits.Load)
	if err != nil {
		return nil, err
	}
	if ans.Err != nil {
		return nil, ans.Err.decode()
	}
	var out wireLoaded
	if err := json.Unmarshal(ans.Body, &out); err != nil {
		return nil, err
	}
	r.exported = map[string]bool{}
	for _, n := range out.Exported {
		r.exported[n] = true
	}
	r.gen = p.gen
	return p, nil
}

func (r *Remote) call(ctx context.Context, fn, op string, limit time.Duration, body wireCall) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.ready(ctx)
	if err != nil {
		return nil, r.cause(ctx, err)
	}
	ans, err := p.request(ctx, fn, op, r.id, body, limit)
	if err != nil {
		return nil, err
	}
	var done wireDone
	if len(ans.Body) > 0 {
		if err := json.Unmarshal(ans.Body, &done); err != nil {
			return nil, err
		}
		r.growth.Store(done.Growth)
	}
	if ans.Err != nil {
		err := ans.Err.decode()
		var te *TimeoutError
		var me *MemoryError
		var ie *InternalError
		switch {
		case errors.As(err, &te) && te.Stuck:
			r.err = err
			// The child's abandoned runtime may run on without end: start a fresh child.
			p.kill(fmt.Sprintf("a stuck %s in module %s", fn, r.b.Manifest.ID), r.id)
		case errors.As(err, &me), errors.As(err, &ie):
			r.err = err
		}
		return nil, r.cause(ctx, err)
	}
	return done.Tree, nil
}

// cause turns the child's cancel back into the caller's own error.
func (r *Remote) cause(ctx context.Context, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return context.Cause(ctx)
	}
	return err
}

// Refresh runs the module's refresh in the child. A module without one succeeds at once.
func (r *Remote) Refresh(ctx context.Context, c Call) error {
	if !r.Has("refresh") {
		return nil
	}
	_, err := r.call(ctx, "refresh", opRefresh, r.limits.Refresh, wireCall{Call: c})
	return err
}

// Render runs render in the child and returns the checked tree, as Module.Render does.
func (r *Remote) Render(ctx context.Context, c Call) ([]byte, error) {
	return r.call(ctx, "render", opRender, r.limits.Render, wireCall{Call: c})
}

// Act runs act(name, args, ctx) in the child.
func (r *Remote) Act(ctx context.Context, name string, args json.RawMessage, c Call) error {
	if strings.HasPrefix(name, modules.ClientPrefix) {
		return ErrClientAction
	}
	_, err := r.call(ctx, "act", opAct, r.limits.Act, wireCall{Call: c, Name: name, Args: args})
	return err
}

// Close unloads the module; its id is free for another New.
func (r *Remote) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	r.h.drop(r)
	r.h.mu.Lock()
	p := r.h.cur
	r.h.mu.Unlock()
	if p != nil && p.gen == r.gen {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = p.request(ctx, "unload", opUnload, r.id, nil, 0)
	}
}

// backend serves one of the child's backend calls with the module's own backends.
func (r *Remote) backend(ctx context.Context, f frame) (any, error) {
	withDeadline := func(d time.Time) (context.Context, context.CancelFunc) {
		if d.IsZero() {
			return ctx, func() {}
		}
		return context.WithDeadline(ctx, d)
	}
	o := r.opts
	switch f.Op {
	case opHabGet, opHabAct:
		var w wireHab
		if err := json.Unmarshal(f.Body, &w); err != nil {
			return nil, err
		}
		if o.Hab == nil {
			return nil, errors.New("no hab backend")
		}
		cctx, cancel := withDeadline(w.Deadline)
		defer cancel()
		if f.Op == opHabGet {
			return o.Hab.Get(cctx, w.Method, w.Params)
		}
		return o.Hab.Act(cctx, w.Method, w.Params)
	case opExec:
		var w wireExec
		if err := json.Unmarshal(f.Body, &w); err != nil {
			return nil, err
		}
		if o.Exec == nil {
			return nil, errors.New("no exec backend")
		}
		cctx, cancel := withDeadline(w.Deadline)
		defer cancel()
		return o.Exec.Run(cctx, w.Req)
	case opFetch:
		var w wireFetch
		if err := json.Unmarshal(f.Body, &w); err != nil {
			return nil, err
		}
		if o.Fetch == nil {
			return nil, errors.New("no fetch backend")
		}
		cctx, cancel := withDeadline(w.Deadline)
		defer cancel()
		return o.Fetch.Do(cctx, w.Req)
	case opStateLoad:
		if o.State == nil {
			return map[string]json.RawMessage{}, nil
		}
		return o.State.Load(r.b.Manifest.ID)
	case opStateSave:
		var st map[string]json.RawMessage
		if err := json.Unmarshal(f.Body, &st); err != nil {
			return nil, err
		}
		if o.State == nil {
			return nil, nil
		}
		return nil, o.State.Save(r.b.Manifest.ID, st)
	}
	return nil, fmt.Errorf("unknown backend call %q", f.Op)
}
