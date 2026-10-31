package holder

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// hook is a test hook that can be armed after the holder starts. The holder field calls run,
// which is set before Serve, so arming needs no holder lock.
type hook struct{ fn atomic.Pointer[func()] }

func (k *hook) run() {
	if f := k.fn.Swap(nil); f != nil {
		(*f)()
	}
}

func (k *hook) arm(f func()) { k.fn.Store(&f) }

// echoPane spawns a raw, no-echo cat: every input byte comes back as output unchanged, so the
// test controls exactly what a pane produces. Its output stream is "ready\n" then the input.
func echoPane(c *client, n int) *hp.Spawned {
	c.t.Helper()
	s := c.sh(n, `stty raw -echo; echo ready; exec cat`)
	c.waitOutput(s.Handle, "ready\n")
	return s
}

// pattern returns input bytes whose value depends on their input offset.
func pattern(from, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte((from+i)%26)
	}
	return b
}

// streamAt is the echo pane's output byte at offset off.
func streamAt(off uint64) byte {
	if off < 6 {
		return "ready\n"[off]
	}
	return 'a' + byte((off-6)%26)
}

func checkStream(t *testing.T, off uint64, data []byte) {
	t.Helper()
	for i, b := range data {
		if b != streamAt(off+uint64(i)) {
			t.Fatalf("byte at offset %d is %q, want %q", off+uint64(i), b, streamAt(off+uint64(i)))
		}
	}
}

// feed sends n pattern bytes of input in frames of at most 64 KiB, starting at input offset
// *in, and waits until the pane's ring holds them.
func feed(t *testing.T, h *Holder, c *client, s *hp.Spawned, in *uint64, n int) {
	t.Helper()
	p := paneOf(t, h, s.Handle)
	for n > 0 {
		k := min(n, 64<<10)
		c.send(&hp.Input{Handle: s.Handle, Token: c.tok(), Offset: *in, Data: pattern(int(*in), k)})
		*in += uint64(k)
		n -= k
		want := 6 + *in
		eventually(t, h, "echo in ring", func() bool { return p.ring.count >= want })
	}
}

// TestReopenAtomic re-opens a replay on a subscribed pane while the program outruns the
// writer: after every replay frame the test pushes more than a frame of output through the
// pane, so the ring wraps under the replay. The replay still ends, at the pane's count when the
// request arrived. No live output arrives inside the replay; the wrap shows as an offset jump
// and gap; live output, queued behind the replay, resumes exactly at end.
func TestReopenAtomic(t *testing.T) {
	testReplayUnderWrap(t, false)
}

// TestBoundedReplayGap runs the same race with a bounded replay. Bytes the ring overwrote
// before the writer reached them make the replay end say gap.
func TestBoundedReplayGap(t *testing.T) {
	testReplayUnderWrap(t, true)
}

func testReplayUnderWrap(t *testing.T, bounded bool) {
	const ringSize = 256 << 10 // four replay frames
	var calls atomic.Int32
	var feedMid func()
	h, path := testHolder(t, Config{RingSize: ringSize}, func(h *Holder) {
		h.afterReplayFrame = func() {
			if calls.Add(1) <= 16 && feedMid != nil {
				feedMid()
			}
		}
	})
	c := dial(t, path, true)
	s := echoPane(c, 1)
	var in uint64
	feed(t, h, c, s, &in, 3*ringSize)
	countAtRequest := 6 + in
	var inMu sync.Mutex // the hook feeds from the holder's writer goroutine
	feedMid = func() {
		inMu.Lock()
		defer inMu.Unlock()
		feed(t, h, c, s, &in, 128<<10)
	}

	tk := c.tok()
	req := &hp.Replay{Handle: s.Handle, Token: tk, From: 0}
	if bounded {
		req.HasTo, req.To = true, 1<<40
	}
	c.send(req)
	end := c.until(func(f hp.Frame) bool { e, ok := f.(*hp.ReplayEnd); return ok && e.Token == tk }).(*hp.ReplayEnd)
	if end.End != countAtRequest || end.Start != countAtRequest-ringSize || !end.Gap {
		t.Fatalf("replay end %+v, count at request %d", end, countAtRequest)
	}
	if n := calls.Load(); n > 4 {
		t.Fatalf("replay took %d frames; its end was not fixed", n)
	}

	// Wait until every echoed byte has come out live. c.output checks the live frames are
	// contiguous, before the re-open and after it.
	inMu.Lock()
	total := 6 + in
	inMu.Unlock()
	for uint64(len(c.output(s.Handle))) < total {
		c.next()
	}

	inReplay, jumps, sawEnd := false, 0, false
	var next uint64
	var firstAfter *hp.Output
	for _, f := range c.all {
		switch f := f.(type) {
		case *hp.Output:
			if f.Handle != s.Handle {
				continue
			}
			if inReplay && !bounded {
				t.Fatalf("live output %s inside the replay", describe(f))
			}
			if sawEnd && firstAfter == nil {
				firstAfter = f
			}
		case *hp.ReplayData:
			if f.Token != tk {
				continue
			}
			if !inReplay {
				inReplay = true
				if f.Offset != end.Start {
					t.Fatalf("first replay frame at %d, start %d", f.Offset, end.Start)
				}
			} else if f.Offset > next {
				jumps++
			} else if f.Offset < next {
				t.Fatalf("replay went back from %d to %d", next, f.Offset)
			}
			checkStream(t, f.Offset, f.Data)
			next = f.Offset + uint64(len(f.Data))
		case *hp.ReplayEnd:
			if f.Token == tk {
				inReplay, sawEnd = false, true
			}
		}
	}
	if jumps == 0 {
		t.Fatal("the ring wrapped under the replay, but no offset jump showed")
	}
	if next > end.End {
		t.Fatalf("replay sent up to %d, past end %d", next, end.End)
	}
	if !bounded {
		if firstAfter == nil || firstAfter.Offset != end.End {
			t.Fatalf("live output resumed at %+v, replay end %d", firstAfter, end.End)
		}
		checkStream(t, firstAfter.Offset, firstAfter.Data)
	}
}

// TestExitedInHandshake lands a child's exit on each side of the welcome. Exactly one of the
// welcome's pane record or an exited frame reports it.
func TestExitedInHandshake(t *testing.T) {
	for _, side := range []string{"before welcome", "after welcome"} {
		t.Run(side, func(t *testing.T) {
			var before, after hook
			h, path := testHolder(t, Config{}, func(h *Holder) {
				h.beforeWelcome = before.run
				h.afterWelcome = after.run
			})
			c1 := dial(t, path, true)
			s := c1.spawn(1, 80, 24, "sleep", "30")
			p := paneOf(t, h, s.Handle)
			c1.nc.Close()
			eventually(t, h, "first client gone", func() bool { return h.conn == nil })

			kill := func() {
				_ = syscall.Kill(int(s.Pid), syscall.SIGTERM)
				<-p.final
			}
			if side == "before welcome" {
				before.arm(kill)
			} else {
				after.arm(kill)
			}
			c2 := dial(t, path, true)
			c2.sync()
			frames := 0
			for _, f := range c2.all {
				if e, ok := f.(*hp.Exited); ok && e.Handle == s.Handle {
					frames++
					if e.Status.Signal != hp.SigTERM {
						t.Fatalf("exited %+v", e)
					}
				}
			}
			pi := c2.w.Panes[0]
			inWelcome := 0
			if pi.Exited {
				inWelcome = 1
				if pi.Exit.Signal != hp.SigTERM {
					t.Fatalf("welcome exit %+v", pi.Exit)
				}
			}
			if inWelcome+frames != 1 {
				t.Fatalf("exit reported %d times in the welcome and %d as frames", inWelcome, frames)
			}
			if (side == "before welcome") != (inWelcome == 1) {
				t.Fatalf("%s: welcome exited = %v", side, pi.Exited)
			}
		})
	}
}

// TestExitedExactlyOnceStress lets many children exit around a reconnect, with no hooks.
func TestExitedExactlyOnceStress(t *testing.T) {
	h, path := testHolder(t, Config{ExitWindow: 20 * time.Millisecond}, nil)
	for round := 0; round < 5; round++ {
		c1 := dial(t, path, true)
		var handles []hp.Handle
		for i := 0; i < 20; i++ {
			s := c1.sh(round*100+i, "sleep 0.0"+strconv.Itoa(i%10))
			handles = append(handles, s.Handle)
		}
		c1.nc.Close()
		c2 := dial(t, path, true)
		ps := make([]*pane, len(handles))
		for i, hd := range handles {
			ps[i] = paneOf(t, h, hd)
		}
		for _, p := range ps {
			<-p.final
		}
		c2.sync()
		for _, hd := range handles {
			n := 0
			for _, pi := range c2.w.Panes {
				if pi.Handle == hd && pi.Exited {
					n++
				}
			}
			for _, f := range c2.all {
				if e, ok := f.(*hp.Exited); ok && e.Handle == hd {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("round %d pane %d: exit reported %d times", round, hd, n)
			}
		}
		for _, hd := range handles {
			c2.send(&hp.Release{Handle: hd, Token: c2.tok()})
		}
		c2.sync()
		c2.nc.Close()
	}
}

// TestReplayWriterPaced replays a 2 MiB ring to a reader that is slow but keeps reading. The
// replay is produced one frame at a time as the writer drains (the queue never holds more than
// a couple of frames), and the reader is never cut off, although each pause is longer than the
// socket buffer takes to fill.
func TestReplayWriterPaced(t *testing.T) {
	const ringSize = 2 << 20
	h, path := testHolder(t, Config{RingSize: ringSize, HangTimeout: 150 * time.Millisecond}, nil)
	c1 := dial(t, path, true)
	s := echoPane(c1, 1)
	var in uint64
	feed(t, h, c1, s, &in, ringSize)
	c1.nc.Close()

	c2 := dial(t, path, false)
	tk := c2.tok()
	c2.send(&hp.Replay{Handle: s.Handle, Token: tk, From: 6})
	var got uint64 = 6
	var sinceNap int
	var end *hp.ReplayEnd
	for end == nil {
		f, err := c2.readFrame(waitLong)
		if err != nil {
			t.Fatalf("after %d bytes: %v", got-6, err)
		}
		switch f := f.(type) {
		case *hp.ReplayData:
			if f.Offset != got {
				t.Fatalf("replay data at %d, expected %d", f.Offset, got)
			}
			checkStream(t, f.Offset, f.Data)
			got += uint64(len(f.Data))
			sinceNap += len(f.Data)
		case *hp.ReplayEnd:
			end = f
		}
		if sinceNap >= 32<<10 {
			sinceNap = 0
			time.Sleep(10 * time.Millisecond) // slow, but well inside the hang timeout
		}
	}
	if end.Start != 6 || end.End != 6+ringSize || end.Gap || got != end.End {
		t.Fatalf("replay end %+v, got up to %d", end, got)
	}
	h.mu.Lock()
	conn := h.conn
	maxQ := 0
	if conn != nil {
		maxQ = conn.maxQueued
	}
	h.mu.Unlock()
	if conn == nil {
		t.Fatal("the slow reader was dropped")
	}
	if limit := 2 * (hp.MaxOutputData + 64); maxQ > limit {
		t.Fatalf("writer queue peaked at %d bytes; a paced replay holds at most %d", maxQ, limit)
	}
	tk = c2.tok()
	c2.send(&hp.Ping{Token: tk})
	for {
		f, err := c2.readFrame(waitLong)
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := f.(*hp.Pong); ok && p.Token == tk {
			break
		}
	}
}

// TestHangRule connects a client that never reads while its pane floods output. Backpressure
// pauses the pumps; the writer then makes no progress for the hang timeout (shortened here
// from 5 s), so the holder drops the connection, records a crash, and the pump resumes into
// the ring alone.
func TestHangRule(t *testing.T) {
	var cfg Config
	cfg.fill()
	if cfg.HangTimeout != 5*time.Second {
		t.Fatalf("default hang timeout %v", cfg.HangTimeout)
	}
	const hang = 200 * time.Millisecond
	h, path := testHolder(t, Config{QueueLimit: 64 << 10, HangTimeout: hang}, nil)
	c := dial(t, path, false)
	start := time.Now()
	c.send(&hp.Spawn{Token: 1, UUID: uuidN(1), Cols: 80, Rows: 24, Argv: []string{"yes", "flood"}, Env: testEnv})

	var p *pane
	eventually(t, h, "pane", func() bool {
		for _, q := range h.panes {
			p = q
		}
		return p != nil
	})
	var pausedAt uint64
	eventually(t, h, "backpressure", func() bool {
		pausedAt = p.ring.count
		return h.conn != nil && p.paused
	})
	eventually(t, h, "hung connection dropped", func() bool { return h.conn == nil })
	if el := time.Since(start); el < hang {
		t.Fatalf("dropped after %v, before the hang timeout", el)
	}
	if h.previous != hp.PreviousCrash {
		t.Fatalf("previous %d", h.previous)
	}
	eventually(t, h, "pump resumed", func() bool { return p.ring.count > pausedAt+(1<<20) })

	// The dropped client drains what reached its socket, then sees the end.
	for {
		if _, err := c.readFrame(waitLong); err != nil {
			if !isEOF(err) {
				t.Fatalf("dropped client: %v", err)
			}
			break
		}
	}
	c2 := dial(t, path, true)
	if c2.w.Previous != hp.PreviousCrash || len(c2.w.Panes) != 1 || c2.w.Panes[0].Exited {
		t.Fatalf("welcome %+v", c2.w)
	}
}

// TestRestartSurvival kills the client mid-stream. The child keeps running and its output keeps
// landing in the ring; a new client gets the pane in its welcome, replays from its saved offset
// and sees no gap.
func TestRestartSurvival(t *testing.T) {
	h, path := testHolder(t, Config{}, nil)
	c1 := dial(t, path, true)
	s := c1.sh(1, `i=0; while :; do i=$((i+1)); echo $i; sleep 0.002; done`)
	c1.waitOutput(s.Handle, "\n50\r\n")
	before := c1.output(s.Handle)
	saved := uint64(len(before))
	c1.nc.Close() // no handover: a crash

	p := paneOf(t, h, s.Handle)
	eventually(t, h, "client gone", func() bool { return h.conn == nil })
	eventually(t, h, "output while no client", func() bool { return p.ring.count > saved+300 })

	c2 := dial(t, path, true)
	if c2.w.Previous != hp.PreviousCrash || len(c2.w.Panes) != 1 {
		t.Fatalf("welcome %+v", c2.w)
	}
	pi := c2.w.Panes[0]
	if pi.Handle != s.Handle || pi.Pid != s.Pid || pi.Exited || pi.Count <= saved+300 {
		t.Fatalf("pane %+v, saved %d", pi, saved)
	}
	tk := c2.tok()
	c2.send(&hp.Replay{Handle: s.Handle, Token: tk, From: saved})
	end := c2.until(func(f hp.Frame) bool { e, ok := f.(*hp.ReplayEnd); return ok && e.Token == tk }).(*hp.ReplayEnd)
	if end.Start != saved || end.Gap || end.End < pi.Count {
		t.Fatalf("replay end %+v, saved %d", end, saved)
	}
	stream := append(append([]byte{}, before...), replayBytes(t, c2.all, tk, saved)...)
	// Then live output, from end on.
	for {
		f := c2.next()
		o, ok := f.(*hp.Output)
		if !ok || o.Handle != s.Handle {
			continue
		}
		if o.Offset != uint64(len(stream)) {
			t.Fatalf("live output at %d, have %d", o.Offset, len(stream))
		}
		stream = append(stream, o.Data...)
		if bytes.Count(stream, []byte("\n")) > 400 {
			break
		}
	}
	lines := strings.Split(string(stream), "\r\n")
	lines = lines[:len(lines)-1]
	for i, l := range lines {
		if l != strconv.Itoa(i+1) {
			t.Fatalf("line %d is %q", i+1, l)
		}
	}
}
