package holder

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// flakyListener returns injected accept errors before it accepts for real.
type flakyListener struct {
	*net.UnixListener
	mu    sync.Mutex
	err   error
	left  int
	calls int
}

func (f *flakyListener) inject(err error, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.left = err, n
}

func (f *flakyListener) injected() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *flakyListener) AcceptUnix() (*net.UnixConn, error) {
	f.mu.Lock()
	if f.left > 0 {
		f.left--
		f.calls++
		err := f.err
		f.mu.Unlock()
		return nil, &net.OpError{Op: "accept", Net: "unix", Err: os.NewSyscallError("accept4", err)}
	}
	f.mu.Unlock()
	return f.UnixListener.AcceptUnix()
}

// TestAcceptErrors: transient accept errors are retried; a lasting error does not end the
// holder while it owns a pane.
func TestAcceptErrors(t *testing.T) {
	var fl *flakyListener
	h, path := testHolderOn(t, Config{}, nil, func(l *net.UnixListener) acceptor {
		fl = &flakyListener{UnixListener: l}
		fl.inject(syscall.EMFILE, 5)
		return fl
	})
	c := dial(t, path, true) // accepted once the five EMFILE errors are behind
	if fl.injected() != 5 {
		t.Fatalf("%d errors injected", fl.injected())
	}
	s := c.sh(1, `echo ready; while :; do sleep 0.05; done`)
	c.waitOutput(s.Handle, "ready")

	// A lasting error (EINVAL is not transient) with a pane owned: Serve keeps trying. The
	// accept already waiting takes c2; the errors hit the accepts after it, and c3 gets
	// through only once they are behind.
	fl.inject(syscall.EINVAL, 4)
	dial(t, path, true)
	c3 := dial(t, path, true)
	if c3.w.Previous != hp.PreviousCrash || len(c3.w.Panes) != 1 {
		t.Fatalf("welcome %+v", c3.w)
	}
	if fl.injected() != 9 {
		t.Fatalf("%d errors injected", fl.injected())
	}
	select {
	case <-h.Done():
		t.Fatal("holder exited")
	default:
	}
}

// TestAcceptErrorNoPanes: with no pane owned, a lasting accept error ends Serve.
func TestAcceptErrorNoPanes(t *testing.T) {
	dir, err := os.MkdirTemp("", "fda")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := Listen(filepath.Join(dir, "h.sock"))
	if err != nil {
		t.Fatal(err)
	}
	fl := &flakyListener{UnixListener: l}
	fl.inject(syscall.EINVAL, 1)
	h := New(Config{})
	if err := h.serve(fl); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("serve: %v", err)
	}
	h.Shutdown()
}

// TestWriteHangBound: a write that stalls after a partial write is found hung within the hang
// timeout plus one write step, not twice the timeout.
func TestWriteHangBound(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "sp")
		defer f.Close()
		nc, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		return nc.(*net.UnixConn)
	}
	a, b := mk(fds[0]), mk(fds[1])
	defer a.Close()
	defer b.Close() // never read
	const hang = 400 * time.Millisecond
	c := &conn{h: &Holder{cfg: Config{HangTimeout: hang}}, nc: a}
	start := time.Now()
	err = c.write(make([]byte, 16<<20))
	el := time.Since(start)
	if !errors.Is(err, errHung) {
		t.Fatalf("write: %v", err)
	}
	if el < hang || el > hang+hang/5+200*time.Millisecond {
		t.Fatalf("hung after %v, hang timeout %v", el, hang)
	}
}

// TestGoSafe: a panic is logged and survived, with the lock free afterwards: when the lock was
// free, under heavy contention, and inside a real critical section.
func TestGoSafe(t *testing.T) {
	var logMu sync.Mutex
	var logs []string
	logged := func(want string) bool {
		logMu.Lock()
		defer logMu.Unlock()
		for _, l := range logs {
			if strings.Contains(l, want) {
				return true
			}
		}
		return false
	}
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	// lockFree waits until the holder lock can be taken, without blocking on it for good.
	lockFree := func(h *Holder, what string) {
		t.Helper()
		deadline := time.Now().Add(waitLong)
		for !h.mu.TryLock() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the holder lock stayed held", what)
			}
			time.Sleep(time.Millisecond)
		}
		h.mu.Unlock()
	}

	h := New(Config{Logf: logf})
	done := make(chan struct{})
	h.goSafe("free", func() { defer close(done); panic("boom") }, nil)
	<-done
	lockFree(h, "free")

	// Heavy contention (which can put the mutex in starvation mode) changes nothing.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.mu.Lock()
				time.Sleep(2 * time.Millisecond)
				h.mu.Unlock()
			}
		}()
	}
	done = make(chan struct{})
	h.goSafe("contended", func() { defer close(done); panic("boom") }, nil)
	<-done
	// A panic inside a locked section under the same contention.
	done = make(chan struct{})
	h.goSafe("contended section", func() { defer close(done); h.locked(func() { panic("boom") }) }, nil)
	<-done
	close(stop)
	wg.Wait()
	lockFree(h, "contended")
	// done closes in fn's own defer, before runSafe's recover writes the log line: wait for it.
	for _, name := range []string{"free", "contended", "contended section"} {
		deadline := time.Now().Add(waitLong)
		for !logged("panic in " + name + ": boom") {
			if time.Now().After(deadline) {
				t.Errorf("no log for the panic in %s", name)
				break
			}
			time.Sleep(time.Millisecond)
		}
	}

	// A panic inside a real critical section: the pump of pane 1 panics while it holds the lock
	// to record output. The lock is free afterwards and the holder keeps serving.
	boom := uuidN(1)
	h2, path := testHolder(t, Config{}, func(h *Holder) {
		testLogf := h.cfg.Logf
		h.cfg.Logf = func(format string, args ...any) {
			testLogf(format, args...)
			logf(format, args...)
		}
		h.onOutput = func(p *pane) {
			if p.uuid == boom {
				panic("boom in output")
			}
		}
	})
	c := dial(t, path, true)
	s1 := c.sh(1, "echo first; exec sleep 30")
	deadline := time.Now().Add(waitLong)
	for !logged("panic in pump: boom in output") {
		if time.Now().After(deadline) {
			t.Fatal("the pump never panicked")
		}
		time.Sleep(time.Millisecond)
	}
	lockFree(h2, "pump")
	// Pane 1 lost its pump, so the holder hung it up: its reader counts as ended, the sleep
	// ends on the hang-up, and its exit is reported.
	p1 := paneOf(t, h2, s1.Handle)
	if e := c.exited(s1.Handle); !e.Status.Signaled || e.Status.Signal != hp.SigHUP {
		t.Errorf("pane 1 after its pump panicked: %+v", e.Status)
	}
	h2.locked(func() {
		if !p1.readerEnded || !p1.masterClosed {
			t.Errorf("pane 1: readerEnded %v, masterClosed %v", p1.readerEnded, p1.masterClosed)
		}
	})
	c.sync()
	s := c.sh(2, "echo alive")
	c.waitOutput(s.Handle, "alive")
	c.exited(s.Handle)
}

// TestConnPanic: a panic in the connection's writer or reader drops the connection, so gff-hab
// sees a crash and replays. The pumps keep running, and the next hello says crash.
func TestConnPanic(t *testing.T) {
	for _, where := range []string{"writer", "reader"} {
		t.Run(where, func(t *testing.T) {
			var arm atomic.Bool
			boom := func() {
				if arm.CompareAndSwap(true, false) {
					panic("boom in " + where)
				}
			}
			h, path := testHolder(t, Config{}, func(h *Holder) {
				if where == "writer" {
					h.beforeWrite = func(*conn) { boom() }
				} else {
					h.beforeDispatch = func(*conn, hp.Frame) { boom() }
				}
			})
			c1 := dial(t, path, true)
			s := c1.sh(1, "while :; do echo tick; sleep 0.01; done")
			c1.waitOutput(s.Handle, "tick")
			arm.Store(true)
			c1.send(&hp.Ping{Token: c1.tok()})
			if err := c1.closed(); !isEOF(err) {
				t.Fatalf("connection after the panic: %v", err)
			}
			eventually(t, h, "connection dropped", func() bool { return h.conn == nil })

			p := paneOf(t, h, s.Handle)
			var before uint64
			h.locked(func() { before = p.ring.count })
			eventually(t, h, "pump still running", func() bool { return p.ring.count > before })

			c2 := dial(t, path, true)
			if c2.w.Previous != hp.PreviousCrash {
				t.Fatalf("welcome after the panic: previous %v", c2.w.Previous)
			}
			c2.send(&hp.Replay{Handle: s.Handle, Token: c2.tok(), From: c2.w.Panes[0].Count})
			c2.waitOutput(s.Handle, "tick")
		})
	}
}

// TestWaiterPanic: a waiter that panics still reports the pane exited, with an unknown status,
// and closes final. Once released, the pane is no longer owned, so a draining holder exits.
func TestWaiterPanic(t *testing.T) {
	h, path := testHolder(t, Config{}, func(h *Holder) {
		h.afterWait = func(p *pane) {
			if p.uuid == uuidN(1) {
				panic("boom in waiter")
			}
		}
	})
	c := dial(t, path, true)
	s := c.sh(1, "exit 3")
	e := c.exited(s.Handle)
	if e.Status != (hp.ExitStatus{Code: -1}) {
		t.Errorf("exit after the waiter panicked: %+v", e.Status)
	}
	p := paneOf(t, h, s.Handle)
	select {
	case <-p.final:
	case <-time.After(waitLong):
		t.Fatal("final never closed")
	}
	c.send(&hp.Release{Handle: s.Handle, Token: c.tok()})
	c.sync()
	h.locked(func() {
		if len(h.owned) != 0 {
			t.Errorf("owned after release: %d panes", len(h.owned))
		}
	})
	c.send(&hp.Drain{Token: c.tok()})
	select {
	case <-h.Done():
	case <-time.After(waitLong):
		t.Fatal("the drained holder did not exit")
	}
}

// TestHandoverPrevious: a connection that sends handover and closes leaves previous = handover.
func TestHandoverPrevious(t *testing.T) {
	h, path := testHolder(t, Config{}, nil)
	c1 := dial(t, path, true)
	if c1.w.Previous != hp.PreviousNone {
		t.Fatalf("first welcome %+v", c1.w)
	}
	c1.send(&hp.Handover{Token: c1.tok()})
	c1.nc.Close()
	eventually(t, h, "connection gone", func() bool { return h.conn == nil })
	c2 := dial(t, path, true)
	if c2.w.Previous != hp.PreviousHandover {
		t.Fatalf("welcome %+v", c2.w)
	}
}

// TestNewerHello: a newer hello replaces the open connection. It records handover when the old
// connection sent handover, and crash when it did not.
func TestNewerHello(t *testing.T) {
	_, path := testHolder(t, Config{}, nil)
	c1 := dial(t, path, true)
	c1.send(&hp.Handover{Token: c1.tok()})
	c1.sync() // the handover is handled
	c2 := dial(t, path, true)
	if c2.w.Previous != hp.PreviousHandover {
		t.Fatalf("after handover: welcome %+v", c2.w)
	}
	if err := c1.closed(); !isEOF(err) {
		t.Fatalf("replaced connection: %v", err)
	}
	c3 := dial(t, path, true) // c2 sent no handover
	if c3.w.Previous != hp.PreviousCrash {
		t.Fatalf("without handover: welcome %+v", c3.w)
	}
	if err := c2.closed(); !isEOF(err) {
		t.Fatalf("replaced connection: %v", err)
	}
}

// TestDrainExit: a draining holder refuses spawns, keeps serving its pane, and exits with bye
// when the last pane is released.
func TestDrainExit(t *testing.T) {
	h, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	s := c.sh(1, `echo ready; while :; do sleep 0.05; done`)
	c.waitOutput(s.Handle, "ready")
	c.send(&hp.Drain{Token: c.tok()})
	tk := c.tok()
	c.send(&hp.Spawn{Token: tk, UUID: uuidN(2), Cols: 80, Rows: 24, Argv: []string{"true"}, Env: testEnv})
	f := c.until(func(f hp.Frame) bool { return f.Header().Token == tk })
	if sf, ok := f.(*hp.SpawnFailed); !ok || sf.Code != hp.CodeDraining {
		t.Fatalf("spawn while draining: %#v", f)
	}
	select {
	case <-h.Done():
		t.Fatal("exited with a pane left")
	default:
	}
	c.send(&hp.Release{Handle: s.Handle, Token: c.tok()})
	c.until(func(f hp.Frame) bool { _, ok := f.(*hp.Bye); return ok })
	if err := c.closed(); !isEOF(err) {
		t.Fatalf("after bye: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(waitLong):
		t.Fatal("holder did not exit")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket still there: %v", err)
	}
}

// groupGone reports whether no process is left in group pgid.
func groupGone(pgid int) bool {
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// TestBackgroundJobHoldsPTY: the leader exits while a background job (which ignores the
// hang-up) still holds the PTY. The exit is reported after the exit window, output keeps
// flowing, and shutdown kills the job's group even though the leader is already reaped.
func TestBackgroundJobHoldsPTY(t *testing.T) {
	h, path := testHolder(t, Config{ShutdownGrace: 200 * time.Millisecond}, nil)
	c := dial(t, path, true)
	s := c.sh(1, `trap '' HUP; (while :; do echo tick; sleep 0.02; done) & echo ready; exit 3`)
	e := c.exited(s.Handle)
	if e.Status.Code != 3 {
		t.Fatalf("exit %+v", e.Status)
	}
	p := paneOf(t, h, s.Handle)
	if runtime.GOOS == "darwin" {
		// macOS revokes the terminal when the session leader exits: the master reads EOF at once
		// and the background job's writes fail with EIO. The holder must end the reader without
		// hanging and still kill the surviving group at shutdown (hab-holder.md 6.3).
		eventually(t, h, "reader ended after the leader exited", func() bool { return p.readerEnded })
		if groupGone(int(s.Pid)) {
			t.Fatal("the background job is gone before shutdown")
		}
	} else {
		// Linux keeps the slave usable: the job's output keeps landing in the ring.
		c.until(func(f hp.Frame) bool {
			o, ok := f.(*hp.Output)
			return ok && o.Handle == s.Handle && o.Offset >= e.Final
		})
		h.mu.Lock()
		ended := p.readerEnded
		h.mu.Unlock()
		if ended || groupGone(int(s.Pid)) {
			t.Fatal("the background job is gone before shutdown")
		}
	}
	h.Shutdown()
	eventually(t, h, "background job killed", func() bool { return groupGone(int(s.Pid)) })
}

// TestShutdownReleasedPane: shutdown also ends a released pane whose child ignored the hang-up.
func TestShutdownReleasedPane(t *testing.T) {
	h, path := testHolder(t, Config{ShutdownGrace: 200 * time.Millisecond}, nil)
	c := dial(t, path, true)
	s := c.sh(1, `trap '' HUP; echo ready; while :; do sleep 0.02; done`)
	c.waitOutput(s.Handle, "ready")
	p := paneOf(t, h, s.Handle)
	c.send(&hp.Release{Handle: s.Handle, Token: c.tok()})
	c.sync()
	time.Sleep(100 * time.Millisecond) // the child survives the release's hang-up
	h.mu.Lock()
	_, owned := h.owned[p]
	reaped := p.reaped
	h.mu.Unlock()
	if !owned || reaped {
		t.Fatalf("released pane: owned %v reaped %v", owned, reaped)
	}
	h.Shutdown()
	select {
	case <-p.final:
	case <-time.After(waitLong):
		t.Fatal("the released pane's child was not ended")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !p.status.Signaled || p.status.Signal != hp.SigKILL {
		t.Fatalf("exit %+v", p.status)
	}
	if len(h.owned) != 0 {
		t.Fatalf("%d panes still owned", len(h.owned))
	}
}

// TestListenChecks: Listen refuses a symlinked socket directory, a directory another user
// could rename, and accepts a sticky shared parent.
func TestListenChecks(t *testing.T) {
	dir, err := os.MkdirTemp("", "fdc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(link, "h.sock")); err == nil {
		t.Fatal("listen in a symlinked directory succeeded")
	}

	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(open, "sub", "h.sock")); err == nil {
		t.Fatal("listen under a parent others can write succeeded")
	}
	if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	l, err := Listen(filepath.Join(open, "sub2", "h.sock"))
	if err != nil {
		t.Fatalf("listen under a sticky parent: %v", err)
	}
	l.Close()
}

// TestHelloOrder: of two hellos that arrive while a connection is open, the later one wins, even
// when the earlier one is still waiting for the old reader as the later one installs. The earlier
// one ends without a welcome; it never takes over from the later one.
func TestHelloOrder(t *testing.T) {
	waiting, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	_, path := testHolder(t, Config{}, func(h *Holder) {
		h.afterReplaceWait = func(*conn) {
			once.Do(func() {
				close(waiting)
				<-release
			})
		}
	})
	c1 := dial(t, path, true)
	// The earlier hello: it replaces c1, then stops in the hook after c1's reader ended.
	type result struct {
		w   *hp.Welcome
		err error
	}
	early := make(chan result, 1)
	go func() {
		nc, err := net.Dial("unix", path)
		if err != nil {
			early <- result{err: err}
			return
		}
		t.Cleanup(func() { nc.Close() })
		_ = nc.SetDeadline(time.Now().Add(waitLong))
		w, err := hp.NewConn(nc, hp.V1()).Hello("early")
		early <- result{w, err}
	}()
	select {
	case <-waiting:
	case <-time.After(waitLong):
		t.Fatal("the earlier hello never waited for the old reader")
	}
	if err := c1.closed(); !isEOF(err) {
		t.Fatalf("replaced connection: %v", err)
	}
	c3 := dial(t, path, true) // the later hello: no connection is current, so it installs
	close(release)
	r := <-early
	if r.err == nil {
		t.Fatalf("the earlier hello got a welcome after the later one: %+v", r.w)
	}
	c3.sync() // the later connection is still current
}
