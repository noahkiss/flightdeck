package holder

import (
	"bufio"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// Handover and crash recovery (spec 2.6, 6.5, 6.8; protocol section 9).

// Environment of the crash client: a test binary re-executed as a logic process that the test
// kills with SIGKILL.
const (
	crashClientSock = "FDH_CRASH_CLIENT_SOCK"
	crashClientPane = "FDH_CRASH_CLIENT_PANE"
)

func TestMain(m *testing.M) {
	if os.Getenv(crashClientSock) != "" {
		os.Exit(crashClient())
	}
	os.Exit(m.Run())
}

// crashClient is the body of the re-executed test binary. It says hello, opens a replay of one
// pane from its oldest kept byte, prints "ready <previous>" once replay data arrives, then keeps
// reading until it is killed.
func crashClient() int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "crash client: "+format+"\n", args...)
		return 2
	}
	hd, err := strconv.ParseUint(os.Getenv(crashClientPane), 10, 32)
	if err != nil {
		return fail("pane: %v", err)
	}
	nc, err := net.Dial("unix", os.Getenv(crashClientSock))
	if err != nil {
		return fail("dial: %v", err)
	}
	pc := hp.NewConn(nc, hp.V1())
	w, err := pc.Hello("crash-client")
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

// numbered is a shell script that prints 1, 2, 3, ... as eight-digit lines, with a short sleep
// every `every` lines. Through the PTY each line is 10 bytes ("%08d\r\n"), so the byte at any
// offset is known: numberedAt.
func numbered(every int) string {
	return fmt.Sprintf(`i=0; while :; do i=$((i+1)); printf '%%08d\n' $i; [ $((i%%%d)) -ne 0 ] || sleep 0.002; done`, every)
}

// numberedUpTo prints lines 1 to n like numbered, then sleeps, so the pane stays open.
func numberedUpTo(every, n int) string {
	return fmt.Sprintf(`i=0; while [ $i -lt %d ]; do i=$((i+1)); printf '%%08d\n' $i; [ $((i%%%d)) -ne 0 ] || sleep 0.002; done; exec sleep 60`, n, every)
}

func numberedAt(off uint64) byte {
	return fmt.Sprintf("%08d\r\n", off/10+1)[off%10]
}

// checkNumbered checks data against the numbered stream at offset off, byte for byte.
func checkNumbered(t *testing.T, off uint64, data []byte) {
	t.Helper()
	for i, b := range data {
		if want := numberedAt(off + uint64(i)); b != want {
			t.Fatalf("byte at offset %d is %q, want %q", off+uint64(i), b, want)
		}
	}
}

// follow applies one pane's replay (token tk) and live output frames to *applied. Every frame
// must start exactly at *applied: a later start is a lost byte, an earlier one a doubled byte.
// It returns the replay end when f is one, and the exit when f is one.
func follow(t *testing.T, hd hp.Handle, tk uint32, from uint64, applied *uint64, f hp.Frame, partial int) (*hp.ReplayEnd, *hp.Exited) {
	t.Helper()
	var off uint64
	var data []byte
	switch f := f.(type) {
	case *hp.ReplayData:
		if f.Handle != hd || f.Token != tk {
			return nil, nil
		}
		off, data = f.Offset, f.Data
	case *hp.Output:
		if f.Handle != hd {
			return nil, nil
		}
		off, data = f.Offset, f.Data
	case *hp.ReplayEnd:
		if f.Handle != hd || f.Token != tk {
			return nil, nil
		}
		if f.Start != from || f.Gap || f.End != *applied {
			t.Fatalf("replay end %+v; replayed from %d, applied up to %d", f, from, *applied)
		}
		return f, nil
	case *hp.Exited:
		if f.Handle == hd {
			return nil, f
		}
		return nil, nil
	default:
		return nil, nil
	}
	if off != *applied {
		t.Fatalf("pane %d: %s starts at %d, applied up to %d", hd, describe(f), off, *applied)
	}
	if partial > 0 && partial < len(data) {
		data = data[:partial] // the client applied only part of this frame before it left
	}
	checkNumbered(t, off, data)
	*applied += uint64(len(data))
	return nil, nil
}

// TestHandoverReplayExact: a client saves an offset in the middle of a frame and a line, hands
// over and leaves. The child keeps printing with no client. The next client is told handover,
// replays from the saved offset, and gets exactly the bytes after it: the first byte at the
// saved offset, none missing, none repeated, then live output from the replay's end.
func TestHandoverReplayExact(t *testing.T) {
	h, path := testHolder(t, Config{}, nil)
	c1 := dial(t, path, true)
	s := c1.sh(1, numbered(20))
	for len(c1.output(s.Handle)) < 700 {
		c1.next()
	}
	checkNumbered(t, 0, c1.output(s.Handle))
	// The saved offset lies strictly inside a frame, and not at a line start: the client
	// applied part of a frame.
	var saved uint64
	for _, f := range c1.all {
		o, ok := f.(*hp.Output)
		if !ok || o.Handle != s.Handle || o.Offset+uint64(len(o.Data)) <= 500 {
			continue
		}
		at := max(o.Offset, 500) + 1
		if at%10 == 0 {
			at++
		}
		if at > o.Offset && at < o.Offset+uint64(len(o.Data)) {
			saved = at
			break
		}
	}
	if saved == 0 {
		t.Fatal("no frame past offset 500 is long enough to save inside")
	}
	c1.send(&hp.Handover{Token: c1.tok()})
	c1.nc.Close()
	eventually(t, h, "client gone", func() bool { return h.conn == nil })
	if h.previous != hp.PreviousHandover {
		t.Fatalf("previous %d", h.previous)
	}
	p := paneOf(t, h, s.Handle)
	eventually(t, h, "output while no client", func() bool { return p.ring.count > saved+2000 })

	c2 := dial(t, path, true)
	if c2.w.Previous != hp.PreviousHandover || len(c2.w.Panes) != 1 {
		t.Fatalf("welcome %+v", c2.w)
	}
	pi := c2.w.Panes[0]
	if pi.Handle != s.Handle || pi.UUID != uuidN(1) || pi.Pid != s.Pid || pi.Exited || pi.Count <= saved+2000 || pi.Oldest != 0 {
		t.Fatalf("pane record %+v", pi)
	}
	tk := c2.tok()
	c2.send(&hp.Replay{Handle: s.Handle, Token: tk, From: saved})
	applied := saved
	var end *hp.ReplayEnd
	for end == nil || applied < end.End+500 {
		e, x := follow(t, s.Handle, tk, saved, &applied, c2.next(), 0)
		if x != nil {
			t.Fatalf("pane exited: %+v", x)
		}
		if e != nil {
			if e.End < pi.Count {
				t.Fatalf("replay end %+v before the welcome count %d", e, pi.Count)
			}
			end = e
		}
	}
	for _, f := range c2.all {
		if d, ok := f.(*hp.ReplayData); ok && d.Token == tk {
			if d.Offset != saved {
				t.Fatalf("first replay frame at %d, saved %d", d.Offset, saved)
			}
			break
		}
	}
}

// TestCrashKilledClient: a logic process in its own OS process is killed with SIGKILL while it
// follows a pane that is printing. The holder records crash; the child lives on. Once the child
// has printed three rings' worth and gone quiet, the next client is told crash, finds the pane
// in its welcome with the ring full, and replays the full ring: every kept byte, from the
// oldest, with no gap. (The child goes quiet so the ring holds still: a replay of a ring that
// wraps under it reports a gap by design.)
func TestCrashKilledClient(t *testing.T) {
	const ringSize = 64 << 10
	h, path := testHolder(t, Config{RingSize: ringSize}, nil)
	c1 := dial(t, path, true)
	const lines = 3 * ringSize / 10
	s := c1.sh(1, numberedUpTo(100, lines))
	c1.waitOutput(s.Handle, "00000002\r\n")
	c1.send(&hp.Handover{Token: c1.tok()})
	c1.nc.Close()
	eventually(t, h, "first client gone", func() bool { return h.conn == nil })

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), crashClientSock+"="+path, crashClientPane+"="+strconv.Itoa(int(s.Handle)))
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(out).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if want := fmt.Sprintf("ready %d\n", hp.PreviousHandover); l != want {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("crash client said %q, want %q", l, want)
		}
	case <-time.After(waitLong):
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("crash client never got replay data")
	}
	eventually(t, h, "crash client connected", func() bool { return h.conn != nil })
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash client ended with %v", err)
	}
	eventually(t, h, "killed client dropped", func() bool { return h.conn == nil })
	if h.previous != hp.PreviousCrash {
		t.Fatalf("previous %d", h.previous)
	}
	p := paneOf(t, h, s.Handle)
	eventually(t, h, "all lines printed", func() bool { return p.ring.count == 10*lines })

	c2 := dial(t, path, true)
	if c2.w.Previous != hp.PreviousCrash || len(c2.w.Panes) != 1 {
		t.Fatalf("welcome %+v", c2.w)
	}
	pi := c2.w.Panes[0]
	if pi.Handle != s.Handle || pi.Pid != s.Pid || pi.Exited || pi.Count-pi.Oldest != ringSize {
		t.Fatalf("pane record %+v, ring %d", pi, ringSize)
	}
	if err := syscall.Kill(int(s.Pid), 0); err != nil {
		t.Fatalf("child: %v", err)
	}
	tk := c2.tok()
	c2.send(&hp.Replay{Handle: s.Handle, Token: tk, From: pi.Oldest})
	applied := pi.Oldest
	var end *hp.ReplayEnd
	for end == nil {
		e, x := follow(t, s.Handle, tk, pi.Oldest, &applied, c2.next(), 0)
		if x != nil {
			t.Fatalf("pane exited: %+v", x)
		}
		if e != nil {
			end = e
		}
	}
	if end.End != pi.Count || applied-pi.Oldest != ringSize {
		t.Fatalf("replay end %+v, welcome count %d, %d bytes replayed", end, pi.Count, applied-pi.Oldest)
	}
	if err := syscall.Kill(int(s.Pid), 0); err != nil {
		t.Fatalf("child: %v", err)
	}
	h.mu.Lock()
	exited := p.exited
	h.mu.Unlock()
	if exited {
		t.Fatal("the child died")
	}
}

// TestWelcomePaneRecords: the welcome lists every live pane, in handle order, with its uuid,
// pid, current size, byte count, oldest kept offset, input count and exit state. Handles stay
// the same across reconnects, by handover and by crash; a new pane gets a new handle; a
// released pane leaves the list.
func TestWelcomePaneRecords(t *testing.T) {
	h, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	a := c.spawn(1, 100, 30, "/bin/sh", "-c", "echo ready; exec sleep 30")
	c.waitOutput(a.Handle, "ready\r\n")
	b := c.spawn(2, 80, 24, "/bin/sh", "-c", "echo bye; exit 3")
	eb := c.exited(b.Handle)
	c.send(&hp.Resize{Handle: a.Handle, Token: c.tok(), Cols: 120, Rows: 40})
	c.send(&hp.Input{Handle: a.Handle, Token: c.tok(), Offset: 0, Data: []byte("x")})
	c.waitOutput(a.Handle, "ready\r\nx") // the terminal echoes it
	c.sync()
	countA := uint64(len(c.output(a.Handle)))

	type want struct {
		s          *hp.Spawned
		uuid       hp.UUID
		cols, rows uint16
		count      uint64
		input      uint64
		exited     bool
	}
	wants := []want{
		{a, uuidN(1), 120, 40, countA, 1, false},
		{b, uuidN(2), 80, 24, eb.Final, 0, true},
	}
	check := func(w *hp.Welcome) {
		t.Helper()
		if len(w.Panes) != len(wants) {
			t.Fatalf("welcome lists %d panes, want %d: %+v", len(w.Panes), len(wants), w.Panes)
		}
		for i, x := range wants {
			pi := w.Panes[i]
			if pi.Handle != x.s.Handle || pi.UUID != x.uuid || pi.Pid != x.s.Pid || pi.Cols != x.cols || pi.Rows != x.rows ||
				pi.Count != x.count || pi.Oldest != 0 || pi.InputCount != x.input || pi.Exited != x.exited {
				t.Fatalf("pane record %d: %+v, want %+v", i, pi, x)
			}
			if x.exited && (pi.Exit.Code != 3 || pi.Exit.Signaled) {
				t.Fatalf("pane record %d exit %+v", i, pi.Exit)
			}
		}
	}

	leave := func(c *client, handover bool) {
		t.Helper()
		if handover {
			c.send(&hp.Handover{Token: c.tok()})
		}
		c.nc.Close()
		eventually(t, h, "client gone", func() bool { return h.conn == nil })
	}
	leave(c, true)
	c = dial(t, path, true)
	if c.w.Previous != hp.PreviousHandover {
		t.Fatalf("previous %d", c.w.Previous)
	}
	check(c.w)

	// A pane spawned now gets a handle no earlier pane had.
	n := c.spawn(3, 90, 20, "/bin/sh", "-c", "echo up; exec sleep 30")
	c.waitOutput(n.Handle, "up\r\n")
	if n.Handle == a.Handle || n.Handle == b.Handle {
		t.Fatalf("new pane reused handle %d", n.Handle)
	}
	wants = append(wants, want{n, uuidN(3), 90, 20, uint64(len(c.output(n.Handle))), 0, false})
	leave(c, false)
	c = dial(t, path, true)
	if c.w.Previous != hp.PreviousCrash {
		t.Fatalf("previous %d", c.w.Previous)
	}
	check(c.w)

	// Releasing the exited pane removes it from the next welcome.
	c.send(&hp.Release{Handle: b.Handle, Token: c.tok()})
	c.sync()
	wants = append(wants[:1], wants[2])
	leave(c, true)
	c = dial(t, path, true)
	check(c.w)
}

// TestReconnectSoak runs 100 reconnect cycles while a child prints a numbered stream. Each cycle
// replays from the previous client's last applied offset, applies a random number of frames
// (sometimes none, sometimes only part of the last one), then leaves one of four ways: handover
// and close, handover and replaced by the next hello, close with no handover, or replaced with no
// handover. Every applied byte is checked against the stream at its offset, so a lost or doubled
// byte fails at once. At the end the child is stopped, and the bytes applied across all cycles
// equal the pane's final count.
func TestReconnectSoak(t *testing.T) {
	const cycles = 100
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)
	start := time.Now()

	h, path := testHolder(t, Config{}, nil)
	// A setup client spawns the pane and leaves at once; its live output is not applied.
	c0 := dial(t, path, true)
	s := c0.sh(1, numbered(50))
	hd := s.Handle
	c0.send(&hp.Handover{Token: c0.tok()})
	c0.nc.Close()
	eventually(t, h, "setup client gone", func() bool { return h.conn == nil })
	c := dial(t, path, true)
	var applied uint64
	prev := hp.PreviousHandover
	modes := map[string]int{}

	for i := 0; i < cycles; i++ {
		if c.w.Previous != prev {
			t.Fatalf("cycle %d: previous %d, want %d", i, c.w.Previous, prev)
		}
		if len(c.w.Panes) != 1 {
			t.Fatalf("cycle %d: welcome %+v", i, c.w)
		} else if pi := c.w.Panes[0]; pi.Handle != hd || pi.UUID != uuidN(1) || pi.Pid != s.Pid || pi.Exited || pi.Count < applied || pi.Oldest > applied {
			t.Fatalf("cycle %d: pane record %+v, applied %d", i, pi, applied)
		}
		tk := c.tok()
		from := applied
		c.send(&hp.Replay{Handle: hd, Token: tk, From: from})
		frames := rng.Intn(6)
		partial := 0
		if rng.Intn(3) == 0 {
			partial = 1 + rng.Intn(9)
		}
		for got := 0; got < frames; {
			f := c.next()
			cut := 0
			if got == frames-1 {
				cut = partial
			}
			before := applied
			_, x := follow(t, hd, tk, from, &applied, f, cut)
			if x != nil {
				t.Fatalf("cycle %d: pane exited: %+v", i, x)
			}
			if applied != before {
				got++
			}
		}

		handover := rng.Intn(2) == 0
		replace := rng.Intn(2) == 0
		if handover {
			c.send(&hp.Handover{Token: c.tok()})
			prev = hp.PreviousHandover
		} else {
			prev = hp.PreviousCrash
		}
		if !replace {
			c.nc.Close()
			eventually(t, h, "client gone", func() bool { return h.conn == nil })
		}
		modes[fmt.Sprintf("handover=%v replace=%v", handover, replace)]++
		c = dial(t, path, true)
	}
	if c.w.Previous != prev {
		t.Fatalf("last welcome: previous %d, want %d", c.w.Previous, prev)
	}

	// The last client replays from the last applied offset, stops the child, and applies
	// everything up to the exit.
	tk := c.tok()
	from := applied
	c.send(&hp.Replay{Handle: hd, Token: tk, From: from})
	var end *hp.ReplayEnd
	for end == nil {
		end, _ = follow(t, hd, tk, from, &applied, c.next(), 0)
	}
	c.send(&hp.Signal{Handle: hd, Token: c.tok(), Target: hp.TargetChild, Signal: hp.SigTERM})
	var exit *hp.Exited
	for exit == nil {
		_, exit = follow(t, hd, tk, from, &applied, c.next(), 0)
	}
	// Any output after the exit frame still counts.
	ping := c.tok()
	c.send(&hp.Ping{Token: ping})
	for {
		f := c.next()
		if p, ok := f.(*hp.Pong); ok && p.Token == ping {
			break
		}
		follow(t, hd, tk, from, &applied, f, 0)
	}
	p := paneOf(t, h, hd)
	h.mu.Lock()
	count := p.ring.count
	h.mu.Unlock()
	if applied != exit.Final || applied != count {
		t.Fatalf("applied %d bytes; exit final %d; pane count %d", applied, exit.Final, count)
	}
	if applied < 10*cycles {
		t.Fatalf("only %d bytes over %d cycles", applied, cycles)
	}
	t.Logf("%d cycles, %d bytes (%d lines), %v; %v", cycles, applied, applied/10, time.Since(start).Round(time.Millisecond), modes)
}

// TestHandoverBeforeWriteError: gff-hab sends handover and closes while the holder is still
// writing output to it. The write fails before the reader reaches the handover frame. The
// holder still records handover: the frame was sent, and the reader reads everything the peer
// sent before it closed.
func TestHandoverBeforeWriteError(t *testing.T) {
	var gate hook
	h, path := testHolder(t, Config{}, func(h *Holder) {
		h.beforeDispatch = func(c *conn, f hp.Frame) {
			if _, ok := f.(*hp.Handover); ok {
				gate.run()
			}
		}
	})
	c := dial(t, path, false)
	c.send(&hp.Spawn{Token: c.tok(), UUID: uuidN(1), Cols: 80, Rows: 24, Argv: []string{"yes", "flood"}, Env: testEnv})
	var hc *conn
	eventually(t, h, "connection", func() bool { hc = h.conn; return hc != nil && len(h.panes) == 1 })
	gate.arm(func() {
		// Hold the handover back until the writer has hit the closed socket.
		deadline := time.Now().Add(waitLong)
		for time.Now().Before(deadline) {
			h.mu.Lock()
			failed := hc.closed || hc.writeFailed
			h.mu.Unlock()
			if failed {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Error("the writer never failed")
	})
	c.send(&hp.Handover{Token: c.tok()})
	c.nc.Close()
	eventually(t, h, "connection dropped", func() bool { return h.conn == nil })
	if h.previous != hp.PreviousHandover {
		t.Fatalf("previous %d, want handover", h.previous)
	}
	c2 := dial(t, path, true)
	if c2.w.Previous != hp.PreviousHandover || len(c2.w.Panes) != 1 {
		t.Fatalf("welcome %+v", c2.w)
	}
}

// TestReplaceAfterHandover: gff-hab sends handover and its successor says hello at once, with
// no round trip in between. The handover may not be handled yet when the newer hello arrives:
// the holder lets the old connection's reader finish what was sent before it decides, so the
// successor is told handover. First with the old reader held inside the handover's handling,
// then many times with no hook, with the old connection left open or closed.
func TestReplaceAfterHandover(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		read := make(chan struct{}, 1)
		_, path := testHolder(t, Config{}, func(h *Holder) {
			h.beforeDispatch = func(c *conn, f hp.Frame) {
				if _, ok := f.(*hp.Handover); ok {
					read <- struct{}{}
					time.Sleep(50 * time.Millisecond)
				}
			}
		})
		c1 := dial(t, path, true)
		c1.send(&hp.Handover{Token: c1.tok()})
		<-read
		c2 := dial(t, path, true)
		if c2.w.Previous != hp.PreviousHandover {
			t.Fatalf("previous %d, want handover", c2.w.Previous)
		}
	})
	t.Run("stress", func(t *testing.T) {
		_, path := testHolder(t, Config{}, nil)
		c := dial(t, path, true)
		s := c.sh(1, numbered(20))
		for i := 0; i < 40; i++ {
			handover := i%4 < 2
			if handover {
				c.send(&hp.Handover{Token: c.tok()})
			}
			if i%2 == 1 {
				c.nc.Close()
			}
			c = dial(t, path, true)
			want := hp.PreviousCrash
			if handover {
				want = hp.PreviousHandover
			}
			if c.w.Previous != want || len(c.w.Panes) != 1 || c.w.Panes[0].Handle != s.Handle {
				t.Fatalf("round %d: previous %d, want %d; welcome %+v", i, c.w.Previous, want, c.w)
			}
		}
	})
}
