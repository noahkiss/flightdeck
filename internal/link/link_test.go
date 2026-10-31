package link

// End-to-end tests of the holder link (docs/testing.md core tests 1 and 10): a real holder
// binary, and the link as a separate process (this test binary in helper mode), restarted by
// plan and by SIGKILL.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/vt"
)

// waitLong bounds every wait. GFF_TEST_TIMEOUT_SCALE multiplies it on slow machines; see
// docs/testing.md.
var waitLong = scaleTimeout(15 * time.Second)

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

var testEnv = []string{"PATH=/usr/bin:/bin", "TERM=xterm-256color", "LC_ALL=C", "HOME=/"}

// holderBin is the holder binary TestMain builds.
var holderBin string

func TestMain(m *testing.M) {
	if os.Getenv("GFF_LINK_HELPER") == "1" {
		os.Exit(helperMain())
	}
	code := func() int {
		dir, err := os.MkdirTemp("", "fdlink")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		if _, err := exec.LookPath("go"); err == nil {
			holderBin = filepath.Join(dir, "holder")
			cmd := exec.Command("go", "build", "-o", holderBin, "-ldflags", "-X main.build=test",
				"github.com/noahkiss/flightdeck/cmd/gff-hab-holder")
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "build holder: %v\n%s", err, out)
				return 1
			}
		}
		return m.Run()
	}()
	os.Exit(code)
}

// shortDir returns a fresh directory with a short path (macOS caps socket paths at 104 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fdl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

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

// startHolder runs the holder binary in its own session, as an init system would.
func startHolder(t *testing.T, dir, id string) *exec.Cmd {
	t.Helper()
	if holderBin == "" {
		t.Skip("no go toolchain to build the holder binary")
	}
	cmd := exec.Command(holderBin, "-dir", dir, "-instance", id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logs := &lockedBuffer{}
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(waitLong):
			cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("holder log:\n%s", logs.String())
		}
	})
	waitFor(t, "holder registry file", func() bool {
		_, err := os.Stat(holderreg.Path(dir, id))
		return err == nil
	})
	return cmd
}

// hab is one link process.
type hab struct {
	t       *testing.T
	cmd     *exec.Cmd
	in      io.WriteCloser
	replies chan helperEvent
	chunks  chan helperEvent // chunk and subend events; closed at the process's end
	exited  chan struct{}
	logs    *lockedBuffer
	ready   helperEvent
}

type habEnv struct {
	holders, state string
	idle, every    string // durations; "-1s" turns that checkpoint trigger off
}

func startHab(t *testing.T, e habEnv) *hab {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "GFF_LINK_HELPER=1", "GFF_LINK_HOLDERS="+e.holders,
		"GFF_LINK_STATE="+e.state, "GFF_LINK_IDLE="+e.idle, "GFF_LINK_EVERY="+e.every)
	h := &hab{t: t, cmd: cmd, replies: make(chan helperEvent, 16), chunks: make(chan helperEvent, 1<<16),
		exited: make(chan struct{}), logs: &lockedBuffer{}}
	cmd.Stderr = h.logs
	if h.in, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(nil, 64<<20)
		for sc.Scan() {
			var ev helperEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				continue // a line cut short by SIGKILL
			}
			if ev.Ev == "chunk" || ev.Ev == "subend" {
				h.chunks <- ev
			} else {
				h.replies <- ev
			}
		}
		close(h.chunks)
		cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-h.exited
		if strings.Contains(h.logs.String(), "DATA RACE") {
			t.Error("the link process reported a data race")
		}
		if t.Failed() {
			t.Logf("link log (pid %d):\n%s", cmd.Process.Pid, h.logs.String())
		}
	})
	h.ready = h.reply("ready")
	return h
}

func (h *hab) reply(want string) helperEvent {
	h.t.Helper()
	select {
	case ev := <-h.replies:
		if ev.Ev != want {
			h.t.Fatalf("link sent %q, want %q", ev.Ev, want)
		}
		if ev.Err != "" {
			h.t.Fatalf("%s: %s", want, ev.Err)
		}
		return ev
	case <-h.exited:
		h.t.Fatalf("link exited while waiting for %s", want)
	case <-time.After(waitLong):
		h.t.Fatalf("timed out waiting for %s", want)
	}
	return helperEvent{}
}

func (h *hab) call(c helperCmd) helperEvent {
	h.t.Helper()
	b, _ := json.Marshal(c)
	if _, err := h.in.Write(append(b, '\n')); err != nil {
		h.t.Fatal(err)
	}
	return h.reply(c.Cmd)
}

func (h *hab) screen(u hp.UUID) Screen { return *h.call(helperCmd{Cmd: "screen", Pane: u}).Screen }

func (h *hab) pane(u hp.UUID) PaneStatus {
	h.t.Helper()
	for _, p := range h.call(helperCmd{Cmd: "panes"}).Panes {
		if p.UUID == u {
			return p
		}
	}
	h.t.Fatalf("no pane %s", u)
	return PaneStatus{}
}

// stop is the planned restart: checkpoints, handover, exit.
func (h *hab) stop() {
	h.t.Helper()
	h.call(helperCmd{Cmd: "stop"})
	h.wait()
}

// kill is the crash.
func (h *hab) kill() {
	h.t.Helper()
	h.cmd.Process.Kill()
	h.wait()
}

func (h *hab) wait() {
	h.t.Helper()
	select {
	case <-h.exited:
	case <-time.After(waitLong):
		h.t.Fatal("link process did not exit")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func uuidN(n int) hp.UUID {
	var u hp.UUID
	u[0], u[15] = 0xfd, byte(n)
	return u
}

// tuiScript is a full-screen program: alternate screen, colours, absolute and relative cursor
// moves, a terminal query, and an erase. It draws phase one, waits for the file $GO, draws phase
// two, then sleeps.
const tuiScript = `
printf '\033[?1049h\033[2J\033[H'
printf '\033[1;31mTITLE\033[0m\033[3;5Hphase one\033[5;1H'
i=1
while [ $i -le 5 ]; do
  printf '\033[%d;10H\033[3%dmrow %d\033[0m' $((i+5)) $i $i
  i=$((i+1))
done
printf '\033[6n'
printf '\033[20;1Hready-1'
while [ ! -e "$GO" ]; do sleep 0.02; done
printf '\033[2A\033[5Cphase two\033[K'
printf '\033[1;1H\033[2K\033[1;32mTITLE 2\033[0m'
printf '\033[7;12H\033[1K\033[4mX\033[0m'
printf '\033[22;1Hdone-2'
exec sleep 1000
`

// testDumpAcrossRestart is core test 1's screen part: a full-screen program, a checkpoint taken
// halfway, then a restart by plan or by SIGKILL. The new link's screen must equal the old one's.
func testDumpAcrossRestart(t *testing.T, crash bool) {
	hdir, state := shortDir(t), shortDir(t)
	startHolder(t, hdir, "dump")
	env := habEnv{holders: hdir, state: state, idle: "-1s", every: "-1s"} // checkpoints on request only
	h1 := startHab(t, env)
	u := uuidN(1)
	gofile := filepath.Join(shortDir(t), "go")
	h1.call(helperCmd{Cmd: "spawn", Pane: u, Cols: 80, Rows: 24,
		Argv: []string{"/bin/sh", "-c", tuiScript}, Env: append(testEnv, "GO="+gofile)})
	waitFor(t, "phase one", func() bool { return strings.Contains(h1.screen(u).Text, "ready-1") })
	h1.call(helperCmd{Cmd: "checkpoint"})
	mid := h1.screen(u).Offset
	if err := os.WriteFile(gofile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "phase two", func() bool { return strings.Contains(h1.screen(u).Text, "done-2") })
	before := h1.screen(u)
	if before.Offset <= mid {
		t.Fatalf("phase two added no bytes: %d <= %d", before.Offset, mid)
	}
	pid := h1.pane(u).Pid

	if crash {
		h1.kill()
	} else {
		h1.stop()
	}
	// A crash leaves the checkpoint taken halfway, so the new link replays phase two from the
	// ring into a restored screen. A planned stop writes a checkpoint of everything.
	ck, err := readCheckpoint(state, u)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[bool]uint64{true: mid, false: before.Offset}[crash]; ck.Offset != want {
		t.Errorf("checkpoint offset %d, want %d", ck.Offset, want)
	}
	wantEnc := vt.EncodingVT
	if slices.Contains(testParser().Encodings(), vt.EncodingGhostty) {
		wantEnc = vt.EncodingGhostty
	}
	if ck.Encoding != wantEnc {
		t.Errorf("checkpoint encoding %s, want %s", ck.Encoding, wantEnc)
	}
	t.Logf("checkpoint: %s, %d bytes, offset %d", ck.Encoding, len(ck.Snapshot), ck.Offset)
	h2 := startHab(t, env)
	wantPrev := hp.PreviousHandover
	if crash {
		wantPrev = hp.PreviousCrash
	}
	if got := h2.ready.Previous["dump"]; got != wantPrev {
		t.Errorf("previous = %d, want %d", got, wantPrev)
	}
	after := h2.screen(u)
	if after.Offset != before.Offset {
		t.Errorf("offset after = %d, before = %d", after.Offset, before.Offset)
	}
	if after.Suspect {
		t.Error("screen marked suspect, but the ring reached the checkpoint")
	}
	if after.Text != before.Text {
		t.Errorf("screen differs\n--- before ---\n%s\n--- after ---\n%s", before.Text, after.Text)
	}
	if !bytes.Equal(after.VT, before.VT) {
		t.Errorf("snapshot-vt differs\n--- before ---\n%q\n--- after ---\n%q", before.VT, after.VT)
	}
	if st := h2.pane(u); st.Pid != pid || st.Exited {
		t.Errorf("pane after restart: pid %d (was %d), exited %v", st.Pid, pid, st.Exited)
	}
	h2.stop()
}

func TestDumpAcrossPlannedRestart(t *testing.T) { testDumpAcrossRestart(t, false) }

func TestDumpAcrossCrash(t *testing.T) { testDumpAcrossRestart(t, true) }

// TestRestartLoop restarts the link many times, alternating plan and SIGKILL, while a program
// prints numbered lines and a subscriber follows it (core test 1). The child pid never changes;
// the subscriber's offsets are contiguous across every restart, with no gap and no repeat; and
// the text it received is every line in order. GFF_RESTART_LOOP sets the count (default 20).
func TestRestartLoop(t *testing.T) {
	n := 20
	if v := os.Getenv("GFF_RESTART_LOOP"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			t.Fatalf("GFF_RESTART_LOOP=%q: want a positive count", v)
		}
	}
	hdir, state := shortDir(t), shortDir(t)
	startHolder(t, hdir, "loop")
	// Timed checkpoints are rarer than restarts, so a crash often leaves the subscriber ahead of
	// the checkpoint and its resume must come from the holder ring.
	env := habEnv{holders: hdir, state: state, idle: "50ms", every: "500ms"}
	u := uuidN(2)
	const printer = `i=0; while :; do i=$((i+1)); echo "line $i"; sleep 0.002; done`

	var (
		pid    uint32
		next   uint64 // the offset after the last byte the subscriber holds
		base   uint64 // the offset of the first byte it holds
		stream bytes.Buffer
		chunks int
	)
	for round := 0; round < n; round++ {
		h := startHab(t, env)
		if round == 0 {
			pid = h.call(helperCmd{Cmd: "spawn", Pane: u, Cols: 80, Rows: 24,
				Argv: []string{"/bin/sh", "-c", printer}, Env: testEnv}).Pane.Pid
		}
		st := h.pane(u)
		if st.Pid != pid || st.Exited || st.Lost {
			t.Fatalf("round %d: pane pid %d (want %d), exited %v, lost %v", round, st.Pid, pid, st.Exited, st.Lost)
		}
		readyAt := st.Offset
		from := next
		if round == 0 {
			from = FromNow // attach mid-stream
		}
		h.call(helperCmd{Cmd: "sub", Pane: u, From: from})
		got := 0
		var early bytes.Buffer // the second subscription's bytes
		check := func(ev helperEvent) {
			if ev.Ev != "chunk" {
				return
			}
			if ev.Sub == 1 {
				if ev.Chunk.Kind != ChunkBytes || ev.Chunk.Offset != base+uint64(early.Len()) {
					t.Fatalf("round %d: resume from the ring: chunk kind %d at %d, want bytes at %d",
						round, ev.Chunk.Kind, ev.Chunk.Offset, base+uint64(early.Len()))
				}
				early.Write(ev.Chunk.Data)
				return
			}
			c := ev.Chunk
			switch {
			case c.Kind == ChunkSnapshot && round == 0 && chunks == 0:
				next, base = c.Offset, c.Offset
			case c.Kind == ChunkBytes:
				if c.Offset != next {
					t.Fatalf("round %d: chunk at %d, want %d (gap or repeat)", round, c.Offset, next)
				}
				if c.Live && c.Offset < readyAt && round > 0 {
					t.Fatalf("round %d: replayed bytes at %d marked live", round, c.Offset)
				}
				stream.Write(c.Data)
				next += uint64(len(c.Data))
				got += len(c.Data)
			default:
				t.Fatalf("round %d: unexpected chunk kind %d at %d", round, c.Kind, c.Offset)
			}
			chunks++
		}
		if round > 0 {
			// A second subscriber resumes from the first byte the test holds. That is behind
			// every checkpoint, so the link must fetch it from the holder ring.
			h.call(helperCmd{Cmd: "sub", Pane: u, From: base, Sub: 1})
		}
		deadline := time.After(waitLong)
		for got < 400 || (round > 0 && early.Len() < stream.Len()) {
			select {
			case ev := <-h.chunks:
				check(ev)
			case <-deadline:
				t.Fatalf("round %d: only %d bytes arrived", round, got)
			}
		}
		if round > 0 && !bytes.Equal(early.Bytes()[:stream.Len()], stream.Bytes()) {
			t.Fatalf("round %d: bytes resumed from the ring differ from the bytes first received", round)
		}
		if round%2 == 0 {
			h.stop()
		} else {
			h.kill()
		}
		for ev := range h.chunks { // what reached the pipe before the end
			check(ev)
		}
	}

	// The text is consecutive numbered lines. The first may be cut by the mid-stream attach.
	lines := strings.Split(stream.String(), "\r\n")
	lines = lines[1 : len(lines)-1]
	if len(lines) < n {
		t.Fatalf("only %d whole lines", len(lines))
	}
	first, err := strconv.Atoi(strings.TrimPrefix(lines[0], "line "))
	if err != nil {
		t.Fatalf("line %q", lines[0])
	}
	for i, l := range lines {
		if want := fmt.Sprintf("line %d", first+i); l != want {
			t.Fatalf("line %d is %q, want %q", i, l, want)
		}
	}
	t.Logf("%d restarts, %d bytes, %d lines, %d chunks", n, stream.Len(), len(lines), chunks)
}

// TestHolderKilled is core test 10 from the link's side: SIGKILL the holder, and the link marks
// its panes lost within 5 s, ends their subscriptions, and still answers and stops.
func TestHolderKilled(t *testing.T) {
	hdir, state := shortDir(t), shortDir(t)
	holder := startHolder(t, hdir, "doomed")
	h := startHab(t, habEnv{holders: hdir, state: state, idle: "50ms", every: "1s"})
	u := uuidN(3)
	h.call(helperCmd{Cmd: "spawn", Pane: u, Cols: 80, Rows: 24,
		Argv: []string{"/bin/sh", "-c", "echo up; exec sleep 1000"}, Env: testEnv})
	waitFor(t, "output", func() bool { return strings.Contains(h.screen(u).Text, "up") })
	h.call(helperCmd{Cmd: "sub", Pane: u, From: FromNow})

	killed := time.Now()
	holder.Process.Signal(syscall.SIGKILL)
	for !h.pane(u).Lost {
		if time.Since(killed) > 5*time.Second {
			t.Fatal("pane not marked lost within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("pane lost %v after SIGKILL", time.Since(killed).Round(time.Millisecond))
	deadline := time.After(5 * time.Second)
	for ended := false; !ended; {
		select {
		case ev := <-h.chunks:
			if ev.Ev == "subend" {
				if !strings.Contains(ev.Err, "lost") {
					t.Errorf("subscription ended with %q", ev.Err)
				}
				ended = true
			}
		case <-deadline:
			t.Fatal("subscription did not end")
		}
	}
	h.call(helperCmd{Cmd: "panes"}) // still answers
	h.stop()                        // and stops
}
