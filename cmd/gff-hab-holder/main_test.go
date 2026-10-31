package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
)

// TestMain runs the real main when the test binary is started as the holder, and a logic
// client when it is started as one.
func TestMain(m *testing.M) {
	if os.Getenv("GFF_HOLDER_TEST_MAIN") == "1" {
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...) // drop the -test.run guard
		main()
		os.Exit(0)
	}
	if os.Getenv(clientSockEnv) != "" {
		os.Exit(logicClient())
	}
	os.Exit(m.Run())
}

// TestBinary runs the holder as its own process: connect, spawn, read output and the exit, then
// SIGTERM ends it cleanly and removes its socket.
func TestBinary(t *testing.T) {
	var stderr bytes.Buffer
	runBinary(t, &stderr)
}

// TestClosedStderr runs the holder with stderr a pipe whose reader is gone. Every log write
// then fails with EPIPE; the holder must live on and serve.
func TestClosedStderr(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	runBinary(t, w)
}

func runBinary(t *testing.T, stderr io.Writer) {
	dir, err := os.MkdirTemp("", "fdb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "h.sock")
	cmd := exec.Command(os.Args[0], "-test.run=^$", "-socket", sock, "-ring", "65536")
	cmd.Env = append(os.Environ(), "GFF_HOLDER_TEST_MAIN=1")
	cmd.Stderr = stderr
	logs := func() string {
		if b, ok := stderr.(*bytes.Buffer); ok {
			return b.String()
		}
		return ""
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var nc net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for {
		if nc, err = net.Dial("unix", sock); err == nil {
			break
		}
		select {
		case werr := <-exited:
			t.Fatalf("holder exited before it listened: %v\n%s", werr, logs())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder did not listen: %v\n%s", err, logs())
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(10 * time.Second))
	c := hp.NewConn(nc, hp.V1())
	w, err := c.Hello("test")
	if err != nil {
		t.Fatal(err)
	}
	if w.RingSize != 65536 || w.Build != "dev" || w.Pid != uint32(cmd.Process.Pid) {
		t.Fatalf("welcome %+v", w)
	}
	if err := c.WriteFrame(&hp.Spawn{Token: 1, Cols: 80, Rows: 24, Argv: []string{"/bin/sh", "-c", "echo hi; exit 4"},
		Env: []string{"PATH=/usr/bin:/bin"}}); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for {
		f, err := c.ReadFrame()
		if err != nil {
			t.Fatalf("%v; output %q", err, out)
		}
		if o, ok := f.(*hp.Output); ok {
			out = append(out, o.Data...)
		}
		if e, ok := f.(*hp.Exited); ok {
			if e.Status.Code != 4 || string(out) != "hi\r\n" {
				t.Fatalf("exit %+v, output %q", e.Status, out)
			}
			break
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := c.ReadFrame()
		if err != nil {
			t.Fatalf("no bye: %v", err)
		}
		if _, ok := f.(*hp.Bye); ok {
			break
		}
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("holder exit: %v\n%s", err, logs())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not exit")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestDefaultDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	state := func() (string, error) { return "/appsupport", nil }
	for _, tc := range []struct {
		goos string
		env  map[string]string
		want string
		err  error
	}{
		{"linux", map[string]string{"GFF_HOLDER_DIR": "/x", "XDG_RUNTIME_DIR": "/run/u"}, "/x", nil},
		{"linux", map[string]string{"XDG_RUNTIME_DIR": "/run/u"}, "/run/u/gff/holders", nil},
		{"linux", map[string]string{}, "", errNoRuntimeDir},
		{"darwin", map[string]string{"XDG_RUNTIME_DIR": "/run/u"}, "/appsupport/gff/holders", nil},
		{"darwin", map[string]string{"GFF_HOLDER_DIR": "/x"}, "/x", nil},
	} {
		got, err := defaultDir(tc.goos, env(tc.env), state)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s %v: got %q, %v; want %q, %v", tc.goos, tc.env, got, err, tc.want, tc.err)
		}
	}
}

// --- Signal and crash tests: the holder binary in its own session (docs/testing.md) ---

// waitLong bounds every wait below. GFF_TEST_TIMEOUT_SCALE multiplies it on slow machines.
var waitLong = scaleTimeout(15 * time.Second)

// scaleTimeout multiplies d by $GFF_TEST_TIMEOUT_SCALE, a positive number. Unset, it returns d.
func scaleTimeout(d time.Duration) time.Duration {
	v := os.Getenv("GFF_TEST_TIMEOUT_SCALE")
	if v == "" {
		return d
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		panic(fmt.Sprintf("GFF_TEST_TIMEOUT_SCALE=%q: want a positive number", v))
	}
	return time.Duration(float64(d) * f)
}

var paneEnv = []string{"PATH=/usr/bin:/bin", "TERM=dumb", "LC_ALL=C", "HOME=/"}

// lockedBuffer collects a process's stderr; the test reads it while the process writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// holderProc is the holder binary running in its own session, as an init system starts it.
type holderProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	logs   *lockedBuffer
	done   chan struct{}
	err    error // valid once done is closed
	socket string
}

func startHolder(t *testing.T, socket string, args ...string) *holderProc {
	t.Helper()
	h := &holderProc{t: t, logs: &lockedBuffer{}, done: make(chan struct{}), socket: socket}
	h.cmd = exec.Command(os.Args[0], append([]string{"-test.run=^$"}, args...)...)
	h.cmd.Env = append(os.Environ(), "GFF_HOLDER_TEST_MAIN=1")
	h.cmd.Stderr = h.logs
	h.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { h.err = h.cmd.Wait(); close(h.done) }()
	t.Cleanup(func() {
		h.cmd.Process.Kill()
		<-h.done
		if t.Failed() {
			t.Logf("holder %d log:\n%s", h.cmd.Process.Pid, h.logs.String())
		}
	})
	return h
}

func (h *holderProc) pid() int { return h.cmd.Process.Pid }

func (h *holderProc) exited() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// connect dials the holder once it listens and says hello.
func (h *holderProc) connect() *conn {
	h.t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		nc, err := net.Dial("unix", h.socket)
		if err == nil {
			return newConn(h.t, nc)
		}
		if h.exited() {
			h.t.Fatalf("holder exited before it listened: %v", h.err)
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("holder did not listen: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// conn is a logic connection in the test process, with a background reader.
type conn struct {
	t      *testing.T
	nc     net.Conn
	pc     *hp.Conn
	w      *hp.Welcome
	frames chan hp.Frame
	rerr   chan error
	token  uint32
	out    map[hp.Handle][]byte // live output per pane, checked contiguous
	base   map[hp.Handle]uint64 // offset of out[pane][0]
}

func newConn(t *testing.T, nc net.Conn) *conn {
	t.Helper()
	c := &conn{t: t, nc: nc, pc: hp.NewConn(nc, hp.V1()), frames: make(chan hp.Frame, 1<<14),
		rerr: make(chan error, 1), out: map[hp.Handle][]byte{}, base: map[hp.Handle]uint64{}}
	t.Cleanup(func() { nc.Close() })
	_ = nc.SetDeadline(time.Now().Add(waitLong))
	w, err := c.pc.Hello("main-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.SetDeadline(time.Time{})
	c.w = w
	go func() {
		for {
			f, err := c.pc.ReadFrame()
			if err != nil {
				c.rerr <- err
				return
			}
			c.frames <- f
		}
	}()
	return c
}

func (c *conn) send(f hp.Frame) {
	c.t.Helper()
	if err := c.pc.WriteFrame(f); err != nil {
		c.t.Fatalf("send %s: %v", f.Header().Kind, err)
	}
}

func (c *conn) tok() uint32 { c.token++; return c.token }

// next returns the next frame, recording live output.
func (c *conn) next() hp.Frame {
	c.t.Helper()
	select {
	case f := <-c.frames:
		if o, ok := f.(*hp.Output); ok {
			base, seen := c.base[o.Handle]
			if !seen {
				base = o.Offset // a connection may join a pane mid-stream
				c.base[o.Handle] = base
			}
			if end := base + uint64(len(c.out[o.Handle])); o.Offset != end {
				c.t.Fatalf("pane %d output at %d, expected %d", o.Handle, o.Offset, end)
			}
			c.out[o.Handle] = append(c.out[o.Handle], o.Data...)
		}
		return f
	case err := <-c.rerr:
		c.rerr <- err
		select {
		case f := <-c.frames:
			return f
		default:
		}
		c.t.Fatalf("connection ended: %v", err)
	case <-time.After(waitLong):
		c.t.Fatalf("no frame within %v", waitLong)
	}
	return nil
}

func (c *conn) until(match func(hp.Frame) bool) hp.Frame {
	c.t.Helper()
	for {
		if f := c.next(); match(f) {
			return f
		}
	}
}

// sh spawns /bin/sh -c script in a new pane, with extra environment entries.
func (c *conn) sh(script string, env ...string) *hp.Spawned {
	c.t.Helper()
	tk := c.tok()
	c.send(&hp.Spawn{Token: tk, UUID: hp.UUID{0xfd, byte(tk)}, Cols: 80, Rows: 24, Cwd: "/",
		Argv: []string{"/bin/sh", "-c", script}, Env: append(append([]string{}, paneEnv...), env...)})
	f := c.until(func(f hp.Frame) bool { return f.Header().Token == tk })
	s, ok := f.(*hp.Spawned)
	if !ok {
		c.t.Fatalf("spawn: got %#v", f)
	}
	return s
}

// waitOutput reads frames until the pane's live output contains s.
func (c *conn) waitOutput(hd hp.Handle, s string) {
	c.t.Helper()
	for !strings.Contains(string(c.out[hd]), s) {
		if e, ok := c.next().(*hp.Exited); ok && e.Handle == hd {
			c.t.Fatalf("pane %d exited before %q; output %q", hd, s, c.out[hd])
		}
	}
}

func (c *conn) ping() {
	c.t.Helper()
	tk := c.tok()
	c.send(&hp.Ping{Token: tk})
	c.until(func(f hp.Frame) bool { p, ok := f.(*hp.Pong); return ok && p.Token == tk })
}

// waitFor polls cond until it holds. It waits on a condition, never a fixed time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fdm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// numberedUpTo prints the lines 00000001 to n, then sleeps so the pane stays open. Through the
// PTY each line is 10 bytes, so the byte at any offset is known: numberedAt.
func numberedUpTo(n int) string {
	return fmt.Sprintf(`i=0; while [ $i -lt %d ]; do i=$((i+1)); printf '%%08d\n' $i; done; exec sleep 600`, n)
}

func numberedAt(off uint64) byte { return fmt.Sprintf("%08d\r\n", off/10+1)[off%10] }

// Environment of the logic client: the test binary re-executed as a separate process.
const (
	clientSockEnv = "GFF_HOLDER_TEST_CLIENT_SOCK"
	clientPaneEnv = "GFF_HOLDER_TEST_CLIENT_PANE"
)

// logicClient is the body of the re-executed logic client. It says hello, replays one pane from
// its oldest kept byte, prints "ready <previous>" when replay data arrives, then reads until it
// is killed.
func logicClient() int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "logic client: "+format+"\n", args...)
		return 2
	}
	hd, err := strconv.ParseUint(os.Getenv(clientPaneEnv), 10, 32)
	if err != nil {
		return fail("pane: %v", err)
	}
	nc, err := net.Dial("unix", os.Getenv(clientSockEnv))
	if err != nil {
		return fail("dial: %v", err)
	}
	pc := hp.NewConn(nc, hp.V1())
	w, err := pc.Hello("killed-client")
	if err != nil {
		return fail("hello: %v", err)
	}
	var from uint64
	for _, pi := range w.Panes {
		if pi.Handle == hp.Handle(hd) {
			from = pi.Oldest
		}
	}
	if err := pc.WriteFrame(&hp.Replay{Handle: hp.Handle(hd), Token: 1, From: from}); err != nil {
		return fail("replay: %v", err)
	}
	ready := false
	for {
		f, err := pc.ReadFrame()
		if err != nil {
			return fail("read: %v", err)
		}
		if _, ok := f.(*hp.ReplayData); ok && !ready {
			ready = true
			fmt.Printf("ready %d\n", w.Previous)
		}
	}
}

// TestKilledLogicClient: the holder runs in its own session. A logic client in another process
// follows a pane that is printing and is killed with SIGKILL. The child lives on with the same
// PID, and the next client replays every byte it printed with no gap and no repeat.
func TestKilledLogicClient(t *testing.T) {
	const lines = 20000 // 200,000 bytes; the ring holds them all
	dir := shortDir(t)
	h := startHolder(t, filepath.Join(dir, "h.sock"), "-socket", filepath.Join(dir, "h.sock"), "-ring", strconv.Itoa(1<<20))
	c1 := h.connect()
	s := c1.sh(numberedUpTo(lines))
	c1.waitOutput(s.Handle, "00000002\r\n")
	c1.send(&hp.Handover{Token: c1.tok()})
	c1.nc.Close()

	cl := exec.Command(os.Args[0], "-test.run=^$")
	cl.Env = append(os.Environ(), clientSockEnv+"="+h.socket, clientPaneEnv+"="+strconv.Itoa(int(s.Handle)))
	cl.Stderr = os.Stderr
	out, err := cl.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	defer cl.Process.Kill()
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(out).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		// The test's own client handed over, so the logic client is told handover.
		if want := fmt.Sprintf("ready %d\n", hp.PreviousHandover); l != want {
			t.Fatalf("logic client said %q, want %q", l, want)
		}
	case <-time.After(waitLong):
		t.Fatal("logic client never got replay data")
	}
	if err := cl.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err = cl.Wait()
	if ws, ok := cl.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("logic client ended with %v", err)
	}

	c2 := h.connect()
	if c2.w.Previous != hp.PreviousCrash || len(c2.w.Panes) != 1 {
		t.Fatalf("welcome %+v", c2.w)
	}
	pi := c2.w.Panes[0]
	if pi.Handle != s.Handle || pi.Pid != s.Pid || pi.Exited || pi.Oldest != 0 {
		t.Fatalf("pane record %+v, spawned %+v", pi, s)
	}
	tk := c2.tok()
	c2.send(&hp.Replay{Handle: s.Handle, Token: tk, From: 0})
	var applied uint64
	apply := func(f hp.Frame, off uint64, data []byte) {
		if off != applied {
			t.Fatalf("%s starts at %d, applied up to %d", f.Header().Kind, off, applied)
		}
		for i, b := range data {
			if want := numberedAt(off + uint64(i)); b != want {
				t.Fatalf("byte at offset %d is %q, want %q", off+uint64(i), b, want)
			}
		}
		applied += uint64(len(data))
	}
	replayDone := false
	for !replayDone || applied < 10*lines {
		switch f := c2.next().(type) {
		case *hp.ReplayData:
			if f.Handle == s.Handle && f.Token == tk {
				apply(f, f.Offset, f.Data)
			}
		case *hp.ReplayEnd:
			if f.Handle == s.Handle && f.Token == tk {
				if f.Start != 0 || f.Gap || f.End != applied {
					t.Fatalf("replay end %+v, applied %d", f, applied)
				}
				replayDone = true
			}
		case *hp.Output:
			if f.Handle == s.Handle {
				if !replayDone {
					t.Fatalf("live output at %d before the replay ended", f.Offset)
				}
				apply(f, f.Offset, f.Data)
			}
		case *hp.Exited:
			t.Fatalf("pane exited: %+v", f)
		}
	}
	if applied != 10*lines {
		t.Fatalf("applied %d bytes, want %d", applied, 10*lines)
	}
	if err := syscall.Kill(int(s.Pid), 0); err != nil {
		t.Fatalf("child %d: %v", s.Pid, err)
	}
	c3 := h.connect()
	if len(c3.w.Panes) != 1 || c3.w.Panes[0].Pid != s.Pid || c3.w.Panes[0].Exited {
		t.Fatalf("after the replay: %+v", c3.w.Panes)
	}
}

// hupTrap makes a pane's shell write "hup" to $HUPFILE when SIGHUP arrives.
const hupTrap = `trap 'echo hup >"$HUPFILE"; exit 0' HUP; `

// hupWaiter traps SIGHUP, prints ready, and waits. The wait builtin returns at once for a trapped
// signal, so the trap runs as soon as the signal arrives.
const hupWaiter = hupTrap + `echo ready; while :; do sleep 600 & wait $!; done`

// hupEcho traps SIGHUP, prints ready, then echoes each line it reads, so a test can tell it
// still runs.
const hupEcho = hupTrap + `echo ready; while read l; do echo "got $l"; done`

// TestHolderKilled: the holder dies by SIGKILL. Its children get SIGHUP, and its socket and
// registry file stay behind. Prune removes them by the registry's rules (the holder's pid is
// not running), and a new holder starts on the same path.
func TestHolderKilled(t *testing.T) {
	dir := shortDir(t)
	const instance = "killed"
	sock := holderreg.SocketPath(dir, instance)
	h1 := startHolder(t, sock, "-dir", dir, "-instance", instance)
	c := h1.connect()
	hupFile := filepath.Join(dir, "hup")
	s := c.sh(hupWaiter, "HUPFILE="+hupFile)
	c.waitOutput(s.Handle, "ready\r\n")
	entries, errs := holderreg.List(dir)
	if len(errs) != 0 || len(entries) != 1 || entries[0].Pid != h1.pid() || entries[0].Socket != sock {
		t.Fatalf("registry %+v %v", entries, errs)
	}
	e := entries[0]

	if err := h1.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-h1.done
	waitFor(t, "the child's SIGHUP record", func() bool {
		b, _ := os.ReadFile(hupFile)
		return string(b) == "hup\n"
	})

	// The dead holder left its socket and registry file.
	for _, p := range []string{sock, holderreg.Path(dir, instance)} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if stale, why := holderreg.Stale(e, true); !stale {
		t.Fatal("the dead holder's entry is not stale")
	} else {
		t.Logf("stale: %s", why)
	}
	removed, err := holderreg.Prune(dir, time.Now())
	if err != nil || len(removed) != 1 || removed[0].Pid != e.Pid {
		t.Fatalf("prune removed %+v: %v", removed, err)
	}
	for _, p := range []string{sock, holderreg.Path(dir, instance)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s after prune: %v", p, err)
		}
	}

	h2 := startHolder(t, sock, "-dir", dir, "-instance", instance)
	c2 := h2.connect()
	if c2.w.Pid != uint32(h2.pid()) || len(c2.w.Panes) != 0 {
		t.Fatalf("new holder welcome %+v", c2.w)
	}
	s2 := c2.sh("echo up; exec sleep 600")
	c2.waitOutput(s2.Handle, "up\r\n")
	waitFor(t, "the new holder's registry file", func() bool {
		entries, _ := holderreg.List(dir)
		return len(entries) == 1 && entries[0].Pid == h2.pid()
	})
}

// TestHolderIgnoresSIGHUP: a SIGHUP sent to the holder is caught and logged. The holder keeps
// serving, and its children neither get the signal nor die.
func TestHolderIgnoresSIGHUP(t *testing.T) {
	dir := shortDir(t)
	h := startHolder(t, filepath.Join(dir, "h.sock"), "-socket", filepath.Join(dir, "h.sock"))
	c := h.connect()
	hupFile := filepath.Join(dir, "hup")
	s := c.sh(hupEcho, "HUPFILE="+hupFile)
	c.waitOutput(s.Handle, "ready\r\n")

	for i := 1; i <= 3; i++ {
		if err := syscall.Kill(h.pid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		waitFor(t, fmt.Sprintf("SIGHUP %d in the log", i), func() bool {
			return strings.Count(h.logs.String(), "SIGHUP ignored") == i
		})
	}
	if h.exited() {
		t.Fatalf("holder exited: %v", h.err)
	}
	c.ping()
	c.send(&hp.Input{Handle: s.Handle, Token: c.tok(), Offset: 0, Data: []byte("x\n")})
	c.waitOutput(s.Handle, "got x\r\n")
	if err := syscall.Kill(int(s.Pid), 0); err != nil {
		t.Fatalf("child %d: %v", s.Pid, err)
	}
	if _, err := os.Stat(hupFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the child got SIGHUP: %v", err)
	}
	// A pane spawned after the signals works too.
	s2 := c.sh("echo later")
	c.waitOutput(s2.Handle, "later\r\n")
}
