package holder

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// readSize is the most a pump reads at once. It stays under the protocol's cap on the bytes in
// one output frame, so each read becomes at most one frame.
const readSize = 32 << 10

// pane is one program in one PTY. Every field below cmd is guarded by Holder.mu.
type pane struct {
	uuid   hp.UUID
	handle hp.Handle
	pid    int
	cmd    *exec.Cmd
	master *os.File

	cols, rows uint16
	ring       *ring

	// Input: bytes accepted but not yet written to the PTY. inCount counts every accepted byte
	// since spawn.
	inq      []byte
	inQueued int // accepted bytes not yet written, including a write in progress
	inCount  uint64
	inFull   *conn // the connection that saw a queue-full refusal and waits for input ready
	inWake   chan struct{}

	paused       bool // the pump waits on backpressure
	masterClosed bool
	readerEnded  bool
	reaped       bool // the waiter collected the child; never signal the pid after this
	exited       bool // the exit is reported (welcome or exited frame)
	status       hp.ExitStatus
	released     bool

	readerDone chan struct{} // closed when the pump ends
	final      chan struct{} // closed when the exit is reported
}

func (p *pane) infoLocked() hp.PaneInfo {
	pi := hp.PaneInfo{
		UUID: p.uuid, Handle: p.handle, Pid: uint32(p.pid), Cols: p.cols, Rows: p.rows,
		Count: p.ring.count, Oldest: p.ring.oldest(), InputCount: p.inCount, Exited: p.exited,
	}
	if p.exited {
		pi.Exit = p.status
	}
	return pi
}

// closeMasterLocked closes the PTY master. The pump's Read returns at once (the master is
// registered with the poller) and the input writer stops.
func (p *pane) closeMasterLocked() {
	if p.masterClosed {
		return
	}
	p.masterClosed = true
	p.master.Close()
	p.wakeInput()
}

// hangupLocked sends SIGHUP to the pane's session and closes the master. The child leads its
// own session and process group, so -pid reaches the group; the pid is signalled only while
// the waiter has not collected it.
func (p *pane) hangupLocked() {
	if !p.reaped {
		_ = syscall.Kill(-p.pid, syscall.SIGHUP)
	}
	p.closeMasterLocked()
}

// killLocked sends SIGKILL to the pane's child and its process group. After the waiter has
// collected the child, its pid lives on only as the group id of processes left in the group (a
// background job). The kernel does not reuse a pid while a group still carries it, so the group
// is probed first and killed only while it exists.
func (p *pane) killLocked() {
	if !p.reaped {
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		return
	}
	if syscall.Kill(-p.pid, 0) == nil {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	}
}

func (p *pane) wakeInput() {
	select {
	case p.inWake <- struct{}{}:
	default:
	}
}

// spawn handles a spawn request.
func (h *Holder) spawn(c *conn, f *hp.Spawn) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()

	if h.spawnAnswered(c, f) {
		return
	}
	fail := func(code hp.ErrorCode, msg string) {
		h.locked(func() {
			c.sendLocked(&hp.SpawnFailed{Token: f.Token, UUID: f.UUID, Code: code, Message: msg})
		})
	}
	if len(f.Argv) == 0 || f.Argv[0] == "" {
		fail(hp.CodeBadRequest, "empty argv")
		return
	}
	if f.Cols == 0 || f.Rows == 0 {
		fail(hp.CodeBadRequest, "size must be at least 1x1")
		return
	}
	p, err := h.startPane(f)
	if err != nil {
		fail(hp.CodeStart, err.Error())
		return
	}

	h.addPane(c, f, p)
	h.goSafe("pump", func() { h.pump(p) }, func() { h.locked(func() { h.pumpDiedLocked(p) }) })
	h.goSafe("input", func() { h.inputWriter(p) }, nil)
	h.goSafe("waiter", func() { h.waiter(p) }, func() { h.waiterDied(p) })
}

// pumpDiedLocked ends a pane whose pump panicked. Nothing reads its PTY any more, so a program
// that writes would block for good. The holder hangs the pane up instead, and the pane's exit
// is reported as usual once the waiter reaps the child. The pump's deferred close of readerDone
// has run, so the waiter does not wait out the exit window.
func (h *Holder) pumpDiedLocked(p *pane) {
	p.readerEnded = true
	p.paused = false
	p.hangupLocked()
}

// waiterDied ends a pane whose waiter panicked. Without it the pane's exit is never reported,
// final never closes and a released pane stays owned, so a draining holder would never exit.
// The pane is reported exited with an unknown status, and its program is hung up. A child the
// waiter had not yet collected is reaped in the background, so it does not stay a zombie. It
// runs in the waiter's goroutine, the only one that closes final.
func (h *Holder) waiterDied(p *pane) {
	var reap bool
	h.locked(func() {
		reap = !p.reaped
		if !p.reaped {
			p.hangupLocked()
			p.reaped = true // the waiter's Wait may have collected the pid; never signal it again
		} else {
			p.closeMasterLocked()
		}
		if !p.exited {
			p.exited = true
			p.status = hp.ExitStatus{Code: -1}
			if c := h.conn; c != nil && c.ready && !c.closed && !p.released {
				c.sendLocked(&hp.Exited{Handle: p.handle, Status: p.status, Final: p.ring.count})
			}
		}
		if p.released {
			delete(h.owned, p)
			h.drainExitLocked()
		}
	})
	select {
	case <-p.final:
	default:
		close(p.final)
	}
	if reap {
		h.goSafe("reap", func() { _, _ = p.cmd.Process.Wait() }, nil)
	}
}

// spawnAnswered answers a spawn that needs no new program: the uuid already has a pane, or the
// holder is draining or closing. It returns false when the spawn must go ahead.
func (h *Holder) spawnAnswered(c *conn, f *hp.Spawn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.byUUID[f.UUID]; p != nil {
		c.sendLocked(&hp.Spawned{Handle: p.handle, Token: f.Token, UUID: f.UUID, Pid: uint32(p.pid), Existing: true})
		return true
	}
	if h.draining || h.closing {
		c.sendLocked(&hp.SpawnFailed{Token: f.Token, UUID: f.UUID, Code: hp.CodeDraining, Message: "holder is draining"})
		return true
	}
	return false
}

// addPane gives a started pane its handle, records it, and answers the spawn.
func (h *Holder) addPane(c *conn, f *hp.Spawn, p *pane) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	p.handle = h.next
	h.panes[p.handle] = p
	h.byUUID[p.uuid] = p
	h.owned[p] = struct{}{}
	if h.conn == c && !c.closed {
		c.sendLocked(&hp.Spawned{Handle: p.handle, Token: f.Token, UUID: f.UUID, Pid: uint32(p.pid)})
		// A pane spawned on this connection is subscribed from offset 0 at once. The pump
		// starts after this, so no byte can precede the spawned frame.
		c.subs[p] = true
	}
}

// startPane opens a PTY and starts the program in a new session with the PTY as its
// controlling terminal. The environment is exactly the one in the request.
func (h *Holder) startPane(f *hp.Spawn) (*pane, error) {
	env := append([]string{}, f.Env...) // never nil: a nil Env would inherit the holder's
	path, err := lookPath(f.Argv[0], env, f.Cwd)
	if err != nil {
		return nil, err
	}
	master, tty, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}
	if err := setSize(master, f.Cols, f.Rows); err != nil {
		master.Close()
		tty.Close()
		return nil, fmt.Errorf("set pty size: %w", err)
	}
	cmd := &exec.Cmd{
		Path:   path,
		Args:   append([]string{}, f.Argv...),
		Env:    env,
		Dir:    f.Cwd,
		Stdin:  tty,
		Stdout: tty,
		Stderr: tty,
		// Ctty is the child's descriptor 0, the PTY.
		SysProcAttr: &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0},
	}
	err = cmd.Start()
	tty.Close() // the child holds the slave now
	if err != nil {
		master.Close()
		return nil, err
	}
	return &pane{
		uuid:       f.UUID,
		pid:        cmd.Process.Pid,
		cmd:        cmd,
		master:     master,
		cols:       f.Cols,
		rows:       f.Rows,
		ring:       newRing(h.cfg.RingSize),
		inWake:     make(chan struct{}, 1),
		readerDone: make(chan struct{}),
		final:      make(chan struct{}),
	}, nil
}

// lookPath finds name the way execvp would, but with the PATH from the pane's own environment,
// not the holder's. A relative directory in PATH, or a relative name with a slash, is taken
// relative to the pane's cwd.
func lookPath(name string, env []string, cwd string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	pathEnv, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathEnv, found = v, true
		}
	}
	if !found {
		pathEnv = "/usr/bin:/bin"
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		cand := filepath.Join(dir, name)
		check := cand
		if !filepath.IsAbs(check) && cwd != "" {
			check = filepath.Join(cwd, check)
		}
		fi, err := os.Stat(check)
		if err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			if !filepath.IsAbs(cand) {
				// exec.Cmd resolves a relative Path against Dir; keep the ./ so it is not
				// taken as a name.
				cand = "./" + cand
			}
			return cand, nil
		}
	}
	return "", fmt.Errorf("%s: not found in PATH", name)
}

// pump reads the PTY for the pane's whole life, whether or not a logic process listens. Each
// read lands in the ring, and goes out as an output frame when the pane is subscribed.
func (h *Holder) pump(p *pane) {
	defer close(p.readerDone)
	buf := make([]byte, readSize)
	for {
		h.awaitFlow(p)
		n, err := p.master.Read(buf)
		if n > 0 {
			h.output(p, buf[:n])
		}
		if err != nil {
			// EIO on Linux once every slave descriptor is closed; EOF elsewhere; ErrClosed
			// after release or shutdown.
			if !errors.Is(err, os.ErrClosed) && !errors.Is(err, syscall.EIO) && !errors.Is(err, io.EOF) {
				h.cfg.Logf("holder: pane %d read: %v", p.handle, err)
			}
			break
		}
	}
	h.locked(func() { p.readerEnded = true })
}

// awaitFlow waits while backpressure pauses the pumps, unless the pane's master is closed.
func (h *Holder) awaitFlow(p *pane) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for h.backpressureLocked() && !p.masterClosed {
		p.paused = true
		h.bp.Wait()
	}
	p.paused = false
}

// output appends a read to the ring and, when the current connection follows the pane live,
// queues it as an output frame. Ring order and queue order are the same because both happen
// under one lock hold.
func (h *Holder) output(p *pane, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.onOutput != nil {
		h.onOutput(p)
	}
	off := p.ring.count
	p.ring.write(data)
	c := h.conn
	if c == nil || !c.ready || c.closed {
		return
	}
	if c.subs[p] {
		c.sendLocked(&hp.Output{Handle: p.handle, Offset: off, Data: append([]byte{}, data...)})
	}
}

// inputWriter writes accepted input to the PTY. A child that stops reading blocks only this
// pane's writer.
func (h *Holder) inputWriter(p *pane) {
	for {
		data, closed := h.takeInput(p)
		if closed {
			return
		}
		if len(data) == 0 {
			<-p.inWake
			continue
		}

		// A failed write (the child is gone, the master closed) drops the bytes: they were
		// accepted, and the input count already includes them.
		_, _ = p.master.Write(data)

		h.locked(func() {
			p.inQueued -= len(data)
			if p.inQueued == 0 {
				h.inputReadyLocked(p)
			}
		})
	}
}

// takeInput takes the pane's queued input. It returns closed true once the master is closed,
// and no data when nothing is queued.
func (h *Holder) takeInput(p *pane) (data []byte, closed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.masterClosed {
		return nil, true
	}
	data = p.inq
	p.inq = nil
	return data, false
}

// inputReadyLocked sends input ready once the queue is empty after a queue-full refusal, to the
// connection that was refused, if it is still the current one.
func (h *Holder) inputReadyLocked(p *pane) {
	c := p.inFull
	if c == nil {
		return
	}
	p.inFull = nil
	if h.conn == c && !c.closed {
		c.sendLocked(&hp.InputReady{Handle: p.handle, Offset: p.inCount})
	}
}

// waiter reaps the child, then reports the exit after the pane's last output byte (spec 2.7).
func (h *Holder) waiter(p *pane) {
	st, err := p.cmd.Process.Wait()
	status := hp.ExitStatus{Code: -1}
	if err == nil {
		if ws, ok := st.Sys().(syscall.WaitStatus); ok {
			status = hp.ExitFromHost(ws)
		}
	} else {
		h.cfg.Logf("holder: pane %d wait: %v", p.handle, err)
	}
	h.locked(func() {
		p.reaped = true
		p.status = status
	})
	if h.afterWait != nil {
		h.afterWait(p)
	}

	h.awaitOutputEnd(p)

	h.locked(func() {
		p.exited = true
		if c := h.conn; c != nil && c.ready && !c.closed && !p.released {
			c.sendLocked(&hp.Exited{Handle: p.handle, Status: p.status, Final: p.ring.count})
		}
		if p.released {
			delete(h.owned, p)
			h.drainExitLocked()
		}
	})
	close(p.final)
}

// awaitOutputEnd returns when the pump has ended, or when ExitWindow of unpaused time has
// passed. Time the pump spends paused by backpressure does not use up the window, because
// output could not flow then.
func (h *Holder) awaitOutputEnd(p *pane) {
	step := min(h.cfg.ExitWindow, 10*time.Millisecond)
	left := h.cfg.ExitWindow
	t := time.NewTicker(step)
	defer t.Stop()
	for left > 0 {
		select {
		case <-p.readerDone:
			return
		case <-t.C:
		}
		var paused bool
		h.locked(func() { paused = p.paused })
		if !paused {
			left -= step
		}
	}
}

// --- Requests on a pane -----------------------------------------------------------------------

// paneLocked looks up a handle, or answers the request with "no pane".
func (h *Holder) paneLocked(c *conn, hd hp.Header) *pane {
	p := h.panes[hd.Pane]
	if p == nil {
		c.errorLocked(hd, hp.CodeNoPane, 0, fmt.Sprintf("no pane %d", hd.Pane))
	}
	return p
}

func (h *Holder) input(c *conn, f *hp.Input) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.paneLocked(c, f.Header())
	if p == nil {
		return
	}
	if f.Offset != p.inCount {
		c.errorLocked(f.Header(), hp.CodeInputOffset, p.inCount,
			fmt.Sprintf("input offset %d, expected %d", f.Offset, p.inCount))
		return
	}
	if p.inQueued+len(f.Data) > h.cfg.InputQueue {
		c.errorLocked(f.Header(), hp.CodeQueueFull, p.inCount, "input queue full")
		p.inFull = c
		if p.inQueued == 0 {
			// Nothing is queued, so no write will finish to trigger input ready: the frame is
			// larger than the whole queue. Say so at once rather than never.
			h.inputReadyLocked(p)
		}
		return
	}
	if len(f.Data) == 0 {
		return
	}
	p.inq = append(p.inq, f.Data...)
	p.inQueued += len(f.Data)
	p.inCount += uint64(len(f.Data))
	p.wakeInput()
}

func (h *Holder) resize(c *conn, f *hp.Resize) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.paneLocked(c, f.Header())
	if p == nil {
		return
	}
	if f.Cols == 0 || f.Rows == 0 {
		c.errorLocked(f.Header(), hp.CodeBadRequest, 0, "size must be at least 1x1")
		return
	}
	if p.masterClosed {
		c.errorLocked(f.Header(), hp.CodeExited, 0, "pane terminal is closed")
		return
	}
	if err := setSize(p.master, f.Cols, f.Rows); err != nil {
		c.errorLocked(f.Header(), hp.CodeUnknown, 0, "resize: "+err.Error())
		return
	}
	p.cols, p.rows = f.Cols, f.Rows
}

func (h *Holder) signal(c *conn, f *hp.Signal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.paneLocked(c, f.Header())
	if p == nil {
		return
	}
	hs, ok := f.Signal.Host()
	if !f.Signal.Sendable() || !ok {
		c.errorLocked(f.Header(), hp.CodeBadRequest, 0, fmt.Sprintf("signal %d is not allowed", f.Signal))
		return
	}
	var err error
	switch f.Target {
	case hp.TargetChild:
		if p.reaped {
			c.errorLocked(f.Header(), hp.CodeExited, 0, "child already reaped")
			return
		}
		// Between Wait collecting the child and p.reaped being set, the pid is free. On Linux,
		// Process.Signal uses a pidfd and cannot reach another process. Elsewhere (macOS has no
		// pidfd) Go marks the process done only after Wait returns, so a signal in that short
		// window could reach a reused pid; the kernel makes reuse that fast unlikely.
		err = p.cmd.Process.Signal(hs)
	case hp.TargetForeground:
		if p.masterClosed {
			c.errorLocked(f.Header(), hp.CodeExited, 0, "pane terminal is closed")
			return
		}
		// The group comes from this pane's own PTY, so it is the pane's group right now.
		var pgid int
		pgid, err = foregroundGroup(p.master)
		if err == nil && pgid == 0 {
			c.errorLocked(f.Header(), hp.CodeBadRequest, 0, "no foreground group")
			return
		}
		if err == nil {
			err = syscall.Kill(-pgid, hs)
		}
	default:
		c.errorLocked(f.Header(), hp.CodeBadRequest, 0, fmt.Sprintf("unknown signal target %d", f.Target))
		return
	}
	if err != nil {
		code := hp.CodeUnknown
		if errors.Is(err, os.ErrProcessDone) {
			code = hp.CodeExited
		}
		c.errorLocked(f.Header(), code, 0, "signal: "+err.Error())
	}
}

func (h *Holder) foreground(c *conn, f *hp.ForegroundQuery) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.paneLocked(c, f.Header())
	if p == nil {
		return
	}
	pgid := 0
	if !p.masterClosed {
		var err error
		if pgid, err = foregroundGroup(p.master); err != nil {
			c.errorLocked(f.Header(), hp.CodeUnknown, 0, "foreground: "+err.Error())
			return
		}
	}
	c.sendLocked(&hp.Foreground{Handle: p.handle, Token: f.Token, Pgid: int32(pgid)})
}

// release drops a pane (spec 2.7). If its reader has not ended, other processes still hold the
// PTY: the holder hangs up the session and closes the master. The waiter keeps running, so a
// child that is still alive is reaped later.
func (h *Holder) release(c *conn, f *hp.Release) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.paneLocked(c, f.Header())
	if p == nil {
		return
	}
	p.released = true
	delete(h.panes, p.handle)
	delete(h.byUUID, p.uuid)
	if !p.readerEnded {
		p.hangupLocked()
	} else {
		p.closeMasterLocked()
	}
	if h.conn != nil {
		delete(h.conn.subs, p)
	}
	if p.exited {
		delete(h.owned, p)
	}
	h.drainExitLocked()
}
