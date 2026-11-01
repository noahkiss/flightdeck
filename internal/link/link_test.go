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
	"regexp"
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

// startHolder runs the holder binary in its own session, as an init system would. ring is the
// per-pane ring size; 0 keeps the default.
func startHolder(t *testing.T, dir, id string, ring int) *exec.Cmd {
	t.Helper()
	if holderBin == "" {
		t.Skip("no go toolchain to build the holder binary")
	}
	args := []string{"-dir", dir, "-instance", id}
	if ring > 0 {
		args = append(args, "-ring", strconv.Itoa(ring))
	}
	cmd := exec.Command(holderBin, args...)
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

// dumpRing is the holder ring in the dump tests: smaller than tuiScript's output, larger than its
// phase two.
const dumpRing = 2048

// tuiScript is a full-screen program: alternate screen, colours, absolute and relative cursor
// moves, a terminal query, and an erase. It prints a few KiB of padding, so the ring loses its
// first bytes, draws phase one, waits for the file $GO, draws phase two, then sleeps. Echo is
// off: the link answers the query, and an echoed answer would reach the screen whenever the line
// discipline gets to it, so the output would not stop at "ready-1" and the checkpoint taken there
// would not cover it.
const tuiScript = `stty -echo
printf '\033[?1049h'
i=0
while [ $i -lt 60 ]; do
  printf 'padding %02d ......................................................\r\n' $i
  i=$((i+1))
done
printf '\033[2J\033[H'
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
	// The ring is smaller than the program's output, so a rebuild from the ring alone cannot
	// match: only the checkpoint can.
	startHolder(t, hdir, "dump", dumpRing)
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
	if st := h2.pane(u); !st.Restored {
		t.Error("the screen was not restored from the checkpoint")
	}
	if int(before.Offset) <= dumpRing {
		t.Errorf("the program printed %d bytes, no more than the ring holds", before.Offset)
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

// loopRing is the holder ring in TestRestartLoop: big enough to reach back past a checkpoint,
// small enough that the program's output overflows it within a few rounds.
const loopRing = 8 << 10

// loopPrinter prints numbered lines full of escapes: colours, wide characters, relative and
// absolute cursor moves, erases and trips to the alternate screen. Each step is one printf, so
// one write. It waits while the file $PAUSE exists, so a slow restart does not overflow the ring.
const loopPrinter = `
i=0
while :; do
  while [ -e "$PAUSE" ]; do sleep 0.01; done
  i=$((i+1))
  printf '\033[3%dmline %d\033[0m \033[1;4m\344\270\226\347\225\214\033[0m \033[7m%d\033[0m\r\n' $((i%8)) $i $((i*7))
  if [ $((i%5)) -eq 0 ]; then
    printf '\033[?1049h\033[2J\033[%d;%dH\033[45malt %d\033[0m\033[?1049l' $((i%20+1)) $((i%60+1)) $i
  fi
  if [ $((i%9)) -eq 0 ]; then
    printf '\033[A\033[60G\033[K\033[42mtick %d\033[0m\r\033[B' $i
  fi
  sleep 0.01
done
`

var loopLine = regexp.MustCompile(`line (\d+)\x1b`)

// TestRestartLoop restarts the link many times, alternating plan and SIGKILL, while a program
// prints escape-heavy output and a subscriber follows it from the first byte (core test 1).
// Every round:
//   - the child pid never changes;
//   - the new link restored the pane's checkpoint, and the screen is not suspect;
//   - its screen equals, byte for byte, a reference parser in the test fed the same stream to the
//     same offset;
//   - the subscriber's offsets are contiguous, with no gap and no repeat;
//   - a second subscriber resumes from behind the checkpoint and gets those bytes from the ring.
//
// At the end the numbered lines are all there, in order. GFF_RESTART_LOOP sets the count
// (default 20).
func TestRestartLoop(t *testing.T) {
	n := 20
	if v := os.Getenv("GFF_RESTART_LOOP"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			t.Fatalf("GFF_RESTART_LOOP=%q: want a positive count", v)
		}
	}
	hdir, state := shortDir(t), shortDir(t)
	startHolder(t, hdir, "loop", loopRing)
	// Timed checkpoints are rarer than restarts, so a crash often leaves the subscriber ahead of
	// the checkpoint and the new link replays from the ring into the restored screen.
	env := habEnv{holders: hdir, state: state, idle: "50ms", every: "100ms"}
	u := uuidN(2)
	pause := filepath.Join(shortDir(t), "pause")

	ref, err := testParser().New(vt.Config{Cols: 80, Rows: 24, ScrollbackBytes: testScrollback})
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()
	var (
		pid      uint32
		next     uint64 // the offset after the last byte the subscriber holds
		fed      uint64 // the offset after the last byte the reference parser holds
		stream   bytes.Buffer
		chunks   int
		replayed int // rounds whose screen needed bytes from the ring past the checkpoint
	)
	feedRef := func(to uint64) {
		ref.Write(stream.Bytes()[fed:to])
		fed = to
	}
	for round := 0; round < n; round++ {
		var ck *checkpoint
		if round > 0 {
			if ck, err = readCheckpoint(state, u); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		h := startHab(t, env)
		if round == 0 {
			pid = h.call(helperCmd{Cmd: "spawn", Pane: u, Cols: 80, Rows: 24,
				Argv: []string{"/bin/sh", "-c", loopPrinter}, Env: append(testEnv, "PAUSE="+pause)}).Pane.Pid
		}
		st := h.pane(u)
		if st.Pid != pid || st.Exited || st.Lost {
			t.Fatalf("round %d: pane pid %d (want %d), exited %v, lost %v", round, st.Pid, pid, st.Exited, st.Lost)
		}
		if round > 0 && (!st.Restored || st.Suspect) {
			t.Fatalf("round %d: restored %v, suspect %v; want a clean restore from the checkpoint at %d",
				round, st.Restored, st.Suspect, ck.Offset)
		}
		screen := h.screen(u)
		if round > 0 && screen.Offset > ck.Offset {
			replayed++
		}
		readyAt := st.Offset
		os.Remove(pause)
		h.call(helperCmd{Cmd: "sub", Pane: u, From: next})
		got := 0
		var early bytes.Buffer // the second subscription's bytes
		var resume uint64      // where the second subscription starts
		second := false
		check := func(ev helperEvent) {
			if ev.Ev != "chunk" {
				return
			}
			if ev.Sub == 1 {
				if ev.Chunk.Kind != ChunkBytes || ev.Chunk.Offset != resume+uint64(early.Len()) {
					t.Fatalf("round %d: resume from the ring: chunk kind %d at %d, want bytes at %d",
						round, ev.Chunk.Kind, ev.Chunk.Offset, resume+uint64(early.Len()))
				}
				early.Write(ev.Chunk.Data)
				return
			}
			c := ev.Chunk
			if c.Kind != ChunkBytes {
				t.Fatalf("round %d: unexpected chunk kind %d at %d", round, c.Kind, c.Offset)
			}
			if c.Offset != next {
				t.Fatalf("round %d: chunk at %d, want %d (gap or repeat)", round, c.Offset, next)
			}
			if c.Live && c.Offset < readyAt && round > 0 {
				t.Fatalf("round %d: replayed bytes at %d marked live", round, c.Offset)
			}
			stream.Write(c.Data)
			next += uint64(len(c.Data))
			got += len(c.Data)
			chunks++
		}
		if round > 0 && ck.Offset > 0 {
			// The second subscriber resumes from behind the checkpoint the link restored, where
			// its buffer starts, so the link must fetch those bytes from the holder ring.
			resume, second = ck.Offset-min(ck.Offset, 512), true
			h.call(helperCmd{Cmd: "sub", Pane: u, From: resume, Sub: 1})
		}
		deadline := time.After(waitLong)
		for got < 1000 || next < screen.Offset || (second && resume+uint64(early.Len()) < next) {
			select {
			case ev := <-h.chunks:
				check(ev)
			case <-deadline:
				t.Fatalf("round %d: only %d bytes arrived", round, got)
			}
		}
		// The screen the new link rebuilt equals the reference parser's at the same offset.
		feedRef(screen.Offset)
		wantText, _ := ref.PlainText()
		wantVT, _ := ref.Snapshot(vt.EncodingVT, vt.SnapshotOptions{Scrollback: DefaultSnapshotScrollback})
		if screen.Text != wantText {
			t.Fatalf("round %d: screen at %d differs from the reference\n--- link ---\n%s\n--- reference ---\n%s",
				round, screen.Offset, screen.Text, wantText)
		}
		if !bytes.Equal(screen.VT, wantVT) {
			t.Fatalf("round %d: snapshot-vt at %d differs from the reference\n--- link ---\n%q\n--- reference ---\n%q",
				round, screen.Offset, screen.VT, wantVT)
		}
		if second && !bytes.Equal(early.Bytes()[:next-resume], stream.Bytes()[resume:next]) {
			t.Fatalf("round %d: bytes resumed from the ring differ from the bytes first received", round)
		}
		// The printer pauses while the link is down; bytes printed after the last checkpoint
		// still come back from the ring.
		if err := os.WriteFile(pause, nil, 0o600); err != nil {
			t.Fatal(err)
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
	if n > 2 && replayed == 0 {
		t.Error("no round replayed bytes past its checkpoint")
	}
	if stream.Len() <= loopRing {
		t.Errorf("the program printed %d bytes, no more than the ring holds", stream.Len())
	}

	// The numbered lines are all there, in order.
	ms := loopLine.FindAllStringSubmatch(stream.String(), -1)
	if len(ms) < n {
		t.Fatalf("only %d lines", len(ms))
	}
	for i, m := range ms {
		if m[1] != strconv.Itoa(i+1) {
			t.Fatalf("line %d is numbered %s", i+1, m[1])
		}
	}
	t.Logf("%d restarts (%d replayed past the checkpoint), %d bytes, %d lines, %d chunks", n, replayed, stream.Len(), len(ms), chunks)
}

// TestHolderKilled is core test 10 from the link's side: SIGKILL the holder, and the link marks
// its panes lost within 5 s, ends their subscriptions, and still answers and stops.
func TestHolderKilled(t *testing.T) {
	hdir, state := shortDir(t), shortDir(t)
	holder := startHolder(t, hdir, "doomed", 0)
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
