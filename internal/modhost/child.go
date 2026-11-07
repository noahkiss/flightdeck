package modhost

// The module host process's side of the pipe (modules.md 7.4). gff-hab runs it as
// "gff-hab module-host"; the modules run here, their backends stay in gff-hab.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"time"
)

// ChildMemoryLimit is the module host's soft Go memory limit (modules.md 8.2).
const ChildMemoryLimit = 256 << 20

// ChildMaxStack bounds one goroutine's stack in the module host. The JS stack is bounded by
// Limits.Stack; this bounds a built-in that recurses in Go (modules.md 8.4), so it crashes the
// child early, well below the resident-memory kill.
const ChildMaxStack = 128 << 20

// childMaxStack is ChildMaxStack; the tests' child lowers it so a crash is quick to cause.
var childMaxStack = ChildMaxStack

// ServeChild runs the module host on r and w, normally the process's stdin and stdout, until r
// ends. It sets the process's soft memory limit. A read error other than the end of r is
// returned.
func ServeChild(r io.Reader, w io.Writer) error {
	debug.SetMemoryLimit(ChildMemoryLimit)
	debug.SetMaxStack(childMaxStack)
	s := &child{c: newConn(w), mods: map[uint64]*Module{}, calls: map[uint64]context.CancelFunc{}}
	defer s.c.end()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		f, err := readFrame(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case f.Re != 0:
			s.c.answered(f)
		case f.Op == opPing:
			_ = s.c.reply(f.ID, wirePong{Mem: heldMemory()}, nil)
		case f.Op == opCancel:
			s.cancel(f.ID)
		default:
			ctx, cancel := context.WithCancel(context.Background())
			s.mu.Lock()
			s.calls[f.ID] = cancel
			s.mu.Unlock()
			go s.handle(ctx, f)
		}
	}
}

type child struct {
	c *conn

	mu    sync.Mutex
	mods  map[uint64]*Module
	calls map[uint64]context.CancelFunc // the parent's requests that run, by ID
}

func (s *child) cancel(id uint64) {
	s.mu.Lock()
	cancel := s.calls[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *child) handle(ctx context.Context, f frame) {
	body, err := s.do(ctx, f)
	s.mu.Lock()
	cancel := s.calls[f.ID]
	delete(s.calls, f.ID)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if d, ok := body.(wireDone); ok && err != nil {
		// A failed call still tells its heap growth: the host's hint for a hog (modules.md 8.4).
		data, _ := marshalBody(wireDone{Growth: d.Growth})
		_ = s.c.send(frame{Re: f.ID, Body: data, Err: encodeError(err)})
		return
	}
	if rerr := s.c.reply(f.ID, body, err); errors.Is(rerr, errFrameTooBig) {
		_ = s.c.reply(f.ID, nil, rerr) // the caller waits: an answer it can take
	}
}

func (s *child) do(ctx context.Context, f frame) (any, error) {
	if f.Op == opLoad {
		var l wireLoad
		if err := json.Unmarshal(f.Body, &l); err != nil {
			return nil, err
		}
		return s.load(f.Mod, l)
	}
	s.mu.Lock()
	m := s.mods[f.Mod]
	if f.Op == opUnload {
		delete(s.mods, f.Mod)
	}
	s.mu.Unlock()
	if f.Op == opUnload {
		return nil, nil
	}
	if m == nil {
		return nil, fmt.Errorf("module host: no module %d", f.Mod)
	}
	var c wireCall
	if err := json.Unmarshal(f.Body, &c); err != nil {
		return nil, err
	}
	var (
		tree []byte
		err  error
	)
	switch f.Op {
	case opRefresh:
		err = m.Refresh(ctx, c.Call)
	case opRender:
		tree, err = m.Render(ctx, c.Call)
	case opAct:
		err = m.Act(ctx, c.Name, c.Args, c.Call)
	default:
		return nil, fmt.Errorf("module host: unknown request %q", f.Op)
	}
	return wireDone{Tree: tree, Growth: m.HeapGrowth()}, err
}

func (s *child) load(mod uint64, l wireLoad) (any, error) {
	if l.Manifest == nil {
		return nil, errors.New("module host: load without a manifest")
	}
	b := &Bundle{Dir: l.Dir, Manifest: l.Manifest, Sources: l.Sources}
	be := &childBackends{s: s, mod: mod}
	opts := Options{Limits: l.Limits, Settings: l.Settings}
	if l.Hab {
		opts.Hab = be
	}
	if l.Exec {
		opts.Exec = be
	}
	if l.Fetch {
		opts.Fetch = be
	}
	if l.State {
		opts.State = be
	}
	if l.Log {
		opts.Log = be.log
	}
	m, err := New(b, opts)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.mods[mod] = m
	s.mu.Unlock()
	var out wireLoaded
	for _, name := range []string{"refresh", "render", "act"} {
		if m.Has(name) {
			out.Exported = append(out.Exported, name)
		}
	}
	return out, nil
}

// childBackends are a module's backends in the module host: each call goes to gff-hab.
type childBackends struct {
	s   *child
	mod uint64
}

// backendPanic is a backend's runtime.Error panic, raised again in the child so goja handles it
// as it would in process.
type backendPanic struct{ msg string }

func (p backendPanic) Error() string { return p.msg }
func (backendPanic) RuntimeError()   {}

// call sends one backend request and waits for its answer. When ctx ends it tells the parent,
// then still waits: a backend that ignores its context holds the call in process too, and the
// runtime's stuck-call guard handles that (modules.md 8.4). The answer of a closed pipe is an
// error.
func (b *childBackends) call(ctx context.Context, op string, body any, out any) error {
	f := frame{Op: op, Mod: b.mod}
	if body != nil {
		data, err := marshalBody(body)
		if err != nil {
			return err
		}
		f.Body = data
	}
	id, ch, ok := b.s.c.start()
	if !ok {
		return errors.New("module host: gff-hab is gone")
	}
	f.ID = id
	if err := b.s.c.send(f); err != nil {
		b.s.c.forget(id)
		return fmt.Errorf("module host: %w", err)
	}
	var ans frame
	var got bool
	select {
	case ans, got = <-ch:
	case <-ctx.Done():
		_ = b.s.c.send(frame{ID: id, Op: opCancel})
		ans, got = <-ch
	}
	if !got {
		return errors.New("module host: gff-hab is gone")
	}
	if ans.Err != nil {
		if ans.Err.Kind == kindPanic {
			if ans.Err.Runtime {
				panic(backendPanic{msg: ans.Err.Message})
			}
			panic(ans.Err.Message)
		}
		return ans.Err.decode()
	}
	if out != nil && len(ans.Body) > 0 {
		return json.Unmarshal(ans.Body, out)
	}
	return nil
}

func deadlineOf(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}

func (b *childBackends) Get(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	err := b.call(ctx, opHabGet, wireHab{Method: method, Params: params, Deadline: deadlineOf(ctx)}, &out)
	return out, err
}

func (b *childBackends) Act(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	err := b.call(ctx, opHabAct, wireHab{Method: method, Params: params, Deadline: deadlineOf(ctx)}, &out)
	return out, err
}

func (b *childBackends) Run(ctx context.Context, req ExecRequest) (ExecResult, error) {
	var out ExecResult
	err := b.call(ctx, opExec, wireExec{Req: req, Deadline: deadlineOf(ctx)}, &out)
	return out, err
}

func (b *childBackends) Do(ctx context.Context, req FetchRequest) (FetchResponse, error) {
	var out FetchResponse
	err := b.call(ctx, opFetch, wireFetch{Req: req, Deadline: deadlineOf(ctx)}, &out)
	return out, err
}

// Load and Save run outside a call's context (New, and a call's end), so they have no
// deadline; gff-hab's store answers at once.
func (b *childBackends) Load(string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	err := b.call(context.Background(), opStateLoad, nil, &out)
	return out, err
}

func (b *childBackends) Save(_ string, state map[string]json.RawMessage) error {
	return b.call(context.Background(), opStateSave, state, nil)
}

func (b *childBackends) log(line string) {
	if data, err := marshalBody(line); err == nil {
		_ = b.s.c.send(frame{Op: opLog, Mod: b.mod, Body: data})
	}
}

// heldMemory is the memory the Go runtime holds from the system, less what it gave back: the
// child's resident size, near enough, since the module host runs no code outside Go.
func heldMemory() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 || s[1].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64() - s[1].Value.Uint64()
}
