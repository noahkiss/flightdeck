package holder

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// TestShellInputResizeExit spawns a shell, writes input, resizes (the child sees the new size)
// and collects the exit status after the last output byte.
func TestShellInputResizeExit(t *testing.T) {
	_, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	if c.w.Previous != hp.PreviousNone || len(c.w.Panes) != 0 || c.w.RingSize != DefaultRingSize {
		t.Fatalf("welcome %+v", c.w)
	}
	s := c.sh(1, `stty size; read line; echo "got:$line"; stty size; exit 3`)
	c.waitOutput(s.Handle, "24 80")

	c.send(&hp.Resize{Handle: s.Handle, Token: c.tok(), Cols: 100, Rows: 30})
	c.send(&hp.Input{Handle: s.Handle, Token: c.tok(), Offset: 0, Data: []byte("hello\n")})
	e := c.exited(s.Handle)
	out := c.output(s.Handle)
	if !bytes.Contains(out, []byte("got:hello")) || !bytes.Contains(out, []byte("30 100")) {
		t.Fatalf("output %q", out)
	}
	if e.Status.Signaled || e.Status.Code != 3 {
		t.Fatalf("exit %+v", e.Status)
	}
	if e.Final != uint64(len(out)) {
		t.Fatalf("final %d, received %d bytes", e.Final, len(out))
	}
	// Nothing for the pane after its exit.
	c.sync()
	if got := c.output(s.Handle); len(got) != len(out) {
		t.Fatalf("output after exited: %q", got[len(out):])
	}
}

// TestOutputBeforeExitKept checks that output written just before exit is not lost (spec 6.3,
// the macOS concern).
func TestOutputBeforeExitKept(t *testing.T) {
	_, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	const n = 20000
	for i := 0; i < 5; i++ {
		s := c.sh(i+1, fmt.Sprintf(`head -c %d /dev/zero | tr '\0' x; exit 0`, n))
		e := c.exited(s.Handle)
		out := c.output(s.Handle)
		if e.Final != n || len(out) != n || strings.Trim(string(out), "x") != "" {
			t.Fatalf("run %d: final %d, got %d bytes", i, e.Final, len(out))
		}
	}
}

func TestExitBySignal(t *testing.T) {
	_, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	s := c.spawn(1, 80, 24, "sleep", "30")
	c.send(&hp.Signal{Handle: s.Handle, Token: c.tok(), Target: hp.TargetChild, Signal: hp.SigTERM})
	e := c.exited(s.Handle)
	if !e.Status.Signaled || e.Status.Signal != hp.SigTERM || e.Status.Code != 0 {
		t.Fatalf("exit %+v", e.Status)
	}
}

// TestSignals sends HUP, INT, TERM and WINCH to the child and INT to the foreground group, checks
// the refusals, then ends the child with KILL.
func TestSignals(t *testing.T) {
	_, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	s := c.sh(1, `for s in HUP INT TERM WINCH; do trap "echo got-$s" $s; done; echo ready; while :; do sleep 0.02; done`)
	c.waitOutput(s.Handle, "ready")

	for _, sig := range []struct {
		s    hp.Sig
		name string
	}{{hp.SigHUP, "HUP"}, {hp.SigINT, "INT"}, {hp.SigTERM, "TERM"}, {hp.SigWINCH, "WINCH"}} {
		tk := c.tok()
		c.send(&hp.Signal{Handle: s.Handle, Token: tk, Target: hp.TargetChild, Signal: sig.s})
		c.waitOutput(s.Handle, "got-"+sig.name)
		c.noErrorFor(tk)
	}

	// The non-interactive shell runs everything in its own group, which leads the PTY.
	tk := c.tok()
	c.send(&hp.ForegroundQuery{Handle: s.Handle, Token: tk})
	fg := c.until(func(f hp.Frame) bool { return f.Header().Token == tk }).(*hp.Foreground)
	if fg.Pgid != int32(s.Pid) {
		t.Fatalf("foreground group %d, child %d", fg.Pgid, s.Pid)
	}
	before := strings.Count(string(c.output(s.Handle)), "got-INT")
	tk = c.tok()
	c.send(&hp.Signal{Handle: s.Handle, Token: tk, Target: hp.TargetForeground, Signal: hp.SigINT})
	for strings.Count(string(c.output(s.Handle)), "got-INT") == before {
		c.next()
	}
	c.noErrorFor(tk)

	// Only the sendable signals pass, and only known targets.
	tk = c.tok()
	c.send(&hp.Signal{Handle: s.Handle, Token: tk, Target: hp.TargetChild, Signal: hp.SigPIPE})
	if e := c.errorFor(tk); e.Code != hp.CodeBadRequest || e.Ref != hp.KindSignal {
		t.Fatalf("PIPE: %+v", e)
	}
	tk = c.tok()
	c.send(&hp.Signal{Handle: s.Handle, Token: tk, Target: 9, Signal: hp.SigINT})
	if e := c.errorFor(tk); e.Code != hp.CodeBadRequest {
		t.Fatalf("bad target: %+v", e)
	}

	c.send(&hp.Signal{Handle: s.Handle, Token: c.tok(), Target: hp.TargetChild, Signal: hp.SigKILL})
	e := c.exited(s.Handle)
	if !e.Status.Signaled || e.Status.Signal != hp.SigKILL {
		t.Fatalf("exit %+v", e.Status)
	}
	// A reaped child is never signalled.
	tk = c.tok()
	c.send(&hp.Signal{Handle: s.Handle, Token: tk, Target: hp.TargetChild, Signal: hp.SigTERM})
	if e := c.errorFor(tk); e.Code != hp.CodeExited {
		t.Fatalf("signal after reap: %+v", e)
	}
}

func TestInputOffsets(t *testing.T) {
	_, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	s := c.sh(1, `stty -echo; echo ready; while read l; do echo "<$l>"; done`)
	c.waitOutput(s.Handle, "ready")

	tk := c.tok()
	c.send(&hp.Input{Handle: s.Handle, Token: tk, Offset: 5, Data: []byte("x\n")})
	if e := c.errorFor(tk); e.Code != hp.CodeInputOffset || e.Offset != 0 || e.Ref != hp.KindInput {
		t.Fatalf("wrong offset: %+v", e)
	}
	tk = c.tok()
	c.send(&hp.Input{Handle: s.Handle, Token: tk, Offset: 0, Data: []byte("ab\n")})
	c.send(&hp.Input{Handle: s.Handle, Token: c.tok(), Offset: 3, Data: []byte("cd\n")})
	c.waitOutput(s.Handle, "<cd>")
	c.noErrorFor(tk)
	// A resend of accepted input is refused, not written twice.
	tk = c.tok()
	c.send(&hp.Input{Handle: s.Handle, Token: tk, Offset: 3, Data: []byte("cd\n")})
	if e := c.errorFor(tk); e.Code != hp.CodeInputOffset || e.Offset != 6 {
		t.Fatalf("stale offset: %+v", e)
	}
	// Unknown handle.
	tk = c.tok()
	c.send(&hp.Input{Handle: 99, Token: tk, Offset: 0, Data: []byte("x")})
	if e := c.errorFor(tk); e.Code != hp.CodeNoPane {
		t.Fatalf("no pane: %+v", e)
	}

	// A new connection learns the input count from its welcome.
	c.nc.Close()
	c2 := dial(t, path, true)
	if len(c2.w.Panes) != 1 || c2.w.Panes[0].InputCount != 6 {
		t.Fatalf("welcome panes %+v", c2.w.Panes)
	}
}

// TestInputQueueFull fills a pane's input queue while the child does not read, then lets it
// read: the holder refuses whole frames with queue full and sends input ready once, with the
// offset to resend from.
func TestInputQueueFull(t *testing.T) {
	_, path := testHolder(t, Config{InputQueue: 4096}, nil)
	c := dial(t, path, true)
	s := c.sh(1, `stty raw -echo; trap 'exec cat >/dev/null' USR1; echo ready; while :; do sleep 0.02; done`)
	c.waitOutput(s.Handle, "ready")

	chunk := bytes.Repeat([]byte("q"), 1024)
	var off uint64
	var full *hp.Error
	for i := 0; i < 4096 && full == nil; i++ {
		tk := c.tok()
		c.send(&hp.Input{Handle: s.Handle, Token: tk, Offset: off, Data: chunk})
		c.send(&hp.Ping{Token: tk})
		f := c.until(func(f hp.Frame) bool { return f.Header().Token == tk })
		switch f := f.(type) {
		case *hp.Pong:
			off += uint64(len(chunk))
		case *hp.Error:
			full = f
		}
	}
	if full == nil || full.Code != hp.CodeQueueFull || full.Offset != off {
		t.Fatalf("queue full: %+v after %d bytes", full, off)
	}
	// The whole frame was refused: a resend at the same offset is still the next one.
	c.send(&hp.Signal{Handle: s.Handle, Token: c.tok(), Target: hp.TargetChild, Signal: hp.SigUSR1})
	r := c.until(func(f hp.Frame) bool { _, ok := f.(*hp.InputReady); return ok }).(*hp.InputReady)
	if r.Handle != s.Handle || r.Offset != off {
		t.Fatalf("input ready %+v, want offset %d", r, off)
	}
	tk := c.tok()
	c.send(&hp.Input{Handle: s.Handle, Token: tk, Offset: off, Data: chunk})
	c.noErrorFor(tk)
	n := 0
	for _, f := range c.all {
		if _, ok := f.(*hp.InputReady); ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d input ready frames, want 1", n)
	}

	// A frame larger than the whole queue can never fit; input ready follows at once so the
	// sender is not left waiting.
	tk = c.tok()
	c.send(&hp.Input{Handle: s.Handle, Token: tk, Offset: off + 1024, Data: make([]byte, 5000)})
	if e := c.errorFor(tk); e.Code != hp.CodeQueueFull {
		t.Fatalf("oversize: %+v", e)
	}
	c.until(func(f hp.Frame) bool { _, ok := f.(*hp.InputReady); return ok })
}

// TestRingOverflow fills a small ring past its size and checks the byte count, the oldest kept
// byte, and replays across the edge.
func TestRingOverflow(t *testing.T) {
	_, path := testHolder(t, Config{RingSize: 1024}, nil)
	c := dial(t, path, true)
	// 300 records of 10 bytes, no newlines, so the PTY adds nothing.
	s := c.sh(1, `i=0; while [ $i -lt 300 ]; do printf '%09d,' $i; i=$((i+1)); done`)
	var want strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&want, "%09d,", i)
	}
	e := c.exited(s.Handle)
	if e.Final != 3000 || string(c.output(s.Handle)) != want.String() {
		t.Fatalf("final %d, output %d bytes", e.Final, len(c.output(s.Handle)))
	}
	c.nc.Close()

	c2 := dial(t, path, true)
	pi := c2.w.Panes[0]
	if pi.Count != 3000 || pi.Oldest != 3000-1024 || !pi.Exited || pi.Exit.Code != 0 {
		t.Fatalf("pane %+v", pi)
	}
	tk := c2.tok()
	c2.send(&hp.Replay{Handle: s.Handle, Token: tk, From: 0})
	end := c2.until(func(f hp.Frame) bool { _, ok := f.(*hp.ReplayEnd); return ok }).(*hp.ReplayEnd)
	if end.Start != 1976 || end.End != 3000 || !end.Gap || end.Token != tk {
		t.Fatalf("replay end %+v", end)
	}
	got := replayBytes(t, c2.all, tk, end.Start)
	if string(got) != want.String()[1976:] {
		t.Fatalf("replay %q", got)
	}

	// A bounded replay inside the ring: no gap, exactly the range.
	tk = c2.tok()
	c2.send(&hp.Replay{Handle: s.Handle, Token: tk, From: 2000, HasTo: true, To: 2100})
	end = c2.until(func(f hp.Frame) bool { _, ok := f.(*hp.ReplayEnd); return ok }).(*hp.ReplayEnd)
	if end.Start != 2000 || end.End != 2100 || end.Gap {
		t.Fatalf("bounded end %+v", end)
	}
	if got := replayBytes(t, c2.all, tk, 2000); string(got) != want.String()[2000:2100] {
		t.Fatalf("bounded replay %q", got)
	}
	// From beyond the count.
	tk = c2.tok()
	c2.send(&hp.Replay{Handle: s.Handle, Token: tk, From: 5000})
	end = c2.until(func(f hp.Frame) bool { _, ok := f.(*hp.ReplayEnd); return ok }).(*hp.ReplayEnd)
	if end.Start != 3000 || end.End != 3000 || end.Gap {
		t.Fatalf("beyond end %+v", end)
	}
	// Unknown handle: an error, no replay end.
	tk = c2.tok()
	c2.send(&hp.Replay{Handle: 77, Token: tk})
	if e := c2.errorFor(tk); e.Code != hp.CodeNoPane || e.Ref != hp.KindReplay {
		t.Fatalf("unknown handle %+v", e)
	}
}

// replayBytes joins the replay data frames of one token, checking they are contiguous from
// start.
func replayBytes(t *testing.T, frames []hp.Frame, tk uint32, start uint64) []byte {
	t.Helper()
	var b []byte
	next := start
	for _, f := range frames {
		d, ok := f.(*hp.ReplayData)
		if !ok || d.Token != tk {
			continue
		}
		if d.Offset != next {
			t.Fatalf("replay data at %d, expected %d", d.Offset, next)
		}
		b = append(b, d.Data...)
		next += uint64(len(d.Data))
	}
	return b
}

func TestSpawnRules(t *testing.T) {
	h, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	s := c.spawn(1, 80, 24, "sleep", "30")
	again := c.spawn(1, 80, 24, "sleep", "30")
	if !again.Existing || again.Handle != s.Handle || again.Pid != s.Pid {
		t.Fatalf("respawn %+v, first %+v", again, s)
	}

	tk := c.tok()
	c.send(&hp.Spawn{Token: tk, UUID: uuidN(2), Cols: 80, Rows: 24, Argv: []string{"no-such-program-here"}, Env: testEnv})
	if f, ok := c.until(func(f hp.Frame) bool { return f.Header().Token == tk }).(*hp.SpawnFailed); !ok || f.Code != hp.CodeStart {
		t.Fatalf("missing program: %+v", f)
	}
	tk = c.tok()
	c.send(&hp.Spawn{Token: tk, UUID: uuidN(3), Cols: 0, Rows: 24, Argv: []string{"sleep", "1"}, Env: testEnv})
	if f, ok := c.until(func(f hp.Frame) bool { return f.Header().Token == tk }).(*hp.SpawnFailed); !ok || f.Code != hp.CodeBadRequest {
		t.Fatalf("zero size: %+v", f)
	}

	// The environment is exactly the one sent; the cwd is honoured.
	dir, _ := os.MkdirTemp("", "fdc")
	defer os.RemoveAll(dir)
	tk = c.tok()
	c.send(&hp.Spawn{Token: tk, UUID: uuidN(4), Cols: 80, Rows: 24, Cwd: dir,
		Argv: []string{"/bin/sh", "-c", `echo "[$ONLY]"; env | wc -l; pwd -P`}, Env: []string{"ONLY=yes", "PATH=/usr/bin:/bin"}})
	e := c.until(func(f hp.Frame) bool { return f.Header().Token == tk }).(*hp.Spawned)
	c.exited(e.Handle)
	real, _ := filepath.EvalSymlinks(dir)
	out := string(c.output(e.Handle))
	if !strings.Contains(out, "[yes]") || !strings.Contains(out, real) {
		t.Fatalf("env/cwd output %q", out)
	}

	// Drain: new spawns are refused; the last release ends the holder with bye.
	c.send(&hp.Drain{Token: c.tok()})
	tk = c.tok()
	c.send(&hp.Spawn{Token: tk, UUID: uuidN(5), Cols: 80, Rows: 24, Argv: []string{"sleep", "1"}, Env: testEnv})
	if f, ok := c.until(func(f hp.Frame) bool { return f.Header().Token == tk }).(*hp.SpawnFailed); !ok || f.Code != hp.CodeDraining {
		t.Fatalf("spawn while draining: %+v", f)
	}
	c.send(&hp.Release{Handle: s.Handle, Token: c.tok()})
	c.send(&hp.Release{Handle: e.Handle, Token: c.tok()})
	c.until(func(f hp.Frame) bool { _, ok := f.(*hp.Bye); return ok })
	<-h.Done()
}

// TestUnknownKind checks the holder answers a frame kind it does not know with code 7.
func TestUnknownKind(t *testing.T) {
	_, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	raw := []byte{0x7f, 0x99, 0, 0, 0, 3, 0, 0, 0, 42, 0, 0, 0, 2, 0xaa, 0xbb}
	if _, err := c.nc.Write(raw); err != nil {
		t.Fatal(err)
	}
	e := c.errorFor(42)
	if e.Code != hp.CodeUnknownKind || e.Ref != 0x7f99 || e.Handle != 3 {
		t.Fatalf("unknown kind: %+v", e)
	}
	c.sync()
}

// TestReleaseHangsUp releases a pane whose child ignores SIGHUP: the pane is gone at once, and
// the child is still reaped later.
func TestReleaseHangsUp(t *testing.T) {
	h, path := testHolder(t, Config{}, nil)
	c := dial(t, path, true)
	s := c.sh(1, `trap '' HUP; echo ready; sleep 0.3`)
	c.waitOutput(s.Handle, "ready")
	p := paneOf(t, h, s.Handle)
	c.send(&hp.Release{Handle: s.Handle, Token: c.tok()})
	tk := c.tok()
	c.send(&hp.Resize{Handle: s.Handle, Token: tk, Cols: 10, Rows: 10})
	if e := c.errorFor(tk); e.Code != hp.CodeNoPane {
		t.Fatalf("after release: %+v", e)
	}
	eventually(t, h, "child reaped", func() bool { return p.reaped })
	<-p.final
	c.sync()
	for _, f := range c.all {
		if _, ok := f.(*hp.Exited); ok {
			t.Fatal("exited sent for a released pane")
		}
	}
}

// TestShutdown ends every pane, killing a child that ignores the hang-up, then says bye.
func TestShutdown(t *testing.T) {
	h, path := testHolder(t, Config{ShutdownGrace: 200e6}, nil)
	c := dial(t, path, true)
	s := c.sh(1, `trap '' HUP; echo ready; while :; do sleep 0.02; done`)
	c.waitOutput(s.Handle, "ready")
	c.send(&hp.Shutdown{Token: c.tok()})
	e := c.exited(s.Handle)
	if !e.Status.Signaled || e.Status.Signal != hp.SigKILL {
		t.Fatalf("exit %+v", e.Status)
	}
	c.until(func(f hp.Frame) bool { _, ok := f.(*hp.Bye); return ok })
	if err := c.closed(); !isEOF(err) {
		t.Fatalf("after bye: %v", err)
	}
	<-h.Done()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket still there: %v", err)
	}
}

func TestListenModes(t *testing.T) {
	dir, _ := os.MkdirTemp("", "fdl")
	defer os.RemoveAll(dir)
	l, err := Listen(filepath.Join(dir, "a", "h.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fi, _ := os.Stat(filepath.Join(dir, "a", "h.sock"))
	di, _ := os.Stat(filepath.Join(dir, "a"))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("socket %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
	}
	if _, err := Listen(filepath.Join(dir, "a", "h.sock")); err == nil {
		t.Fatal("second listen on a live socket succeeded")
	}
	open := filepath.Join(dir, "open")
	os.Mkdir(open, 0o755)
	os.Chmod(open, 0o755)
	if _, err := Listen(filepath.Join(open, "h.sock")); err == nil {
		t.Fatal("listen in a directory open to others succeeded")
	}
}
