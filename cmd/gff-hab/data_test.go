package main

// End-to-end tests of the data connections (hab-logic-api.md 5, docs/testing.md core test 4),
// with the real holder and gff-hab binaries and a test client that speaks the frames.

import (
	"bytes"
	"context"
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

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/datastream"
	"github.com/noahkiss/flightdeck/internal/holderclient"
	"github.com/noahkiss/flightdeck/internal/termq"
)

// habEnv is one holder and one gff-hab serving it.
type habEnv struct {
	t                 *testing.T
	hdir, state, sock string
	work              string
	env               []string // extra environment for each gff-hab started
	srv               *served
	holder            *exec.Cmd
	holderDone        chan struct{}
}

func newHabEnv(t *testing.T, ring int) *habEnv {
	t.Helper()
	d := shortDir(t)
	e := &habEnv{t: t, hdir: filepath.Join(d, "h"), state: filepath.Join(d, "state"),
		sock: filepath.Join(d, "run", "hab.sock"), work: filepath.Join(d, "w")}
	for _, dir := range []string{e.hdir, e.work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"-dir", e.hdir, "-instance", "one"}
	if ring > 0 {
		args = append(args, "-ring", strconv.Itoa(ring))
	}
	cmd := exec.Command(holderBin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	e.holder, e.holderDone = cmd, make(chan struct{})
	go func() { cmd.Wait(); close(e.holderDone) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-e.holderDone:
		case <-time.After(scaleTimeout(10 * time.Second)):
			cmd.Process.Kill()
			<-e.holderDone
		}
	})
	waitUntil(t, "the holder's registry file", func() bool {
		m, _ := filepath.Glob(filepath.Join(e.hdir, "*.json"))
		return len(m) > 0
	})
	e.serve()
	return e
}

func (e *habEnv) serve() {
	e.srv = startServeWith(e.t, e.sock, e.state, e.env, "-holders", e.hdir)
}

// stopHab stops gff-hab by plan (SIGTERM) or by SIGKILL.
func (e *habEnv) stopHab(kill bool) {
	e.t.Helper()
	if kill {
		e.srv.cmd.Process.Kill()
		<-e.srv.done
		e.srv.stopped = true
	} else {
		e.srv.stop(e.t)
	}
}

// restart stops gff-hab by plan (SIGTERM) or by SIGKILL and starts a new one.
func (e *habEnv) restart(kill bool) {
	e.t.Helper()
	e.stopHab(kill)
	e.serve()
}

// control dials the control socket and says hello.
func (e *habEnv) control() *control.Client {
	e.t.Helper()
	c, err := control.Dial(e.sock)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	if _, err := c.Hello(ctx, api.ClientInfo{Name: "test", Version: "0", Kind: api.ClientKindApp}, "", ""); err != nil {
		e.t.Fatal(err)
	}
	return c
}

// spawn starts script in a new pane and returns the pane id.
func (e *habEnv) spawn(c *control.Client, script string) string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var res api.SessionCreated
	err := c.Call(ctx, "session.create", api.SessionCreateParams{
		Argv: []string{"/bin/sh", "-c", script}, Cwd: "/", Cols: 80, Rows: 24,
		Env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "DIR=" + e.work}}, &res)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Pane
}

// stdioConn is the client side of `gff-hab connect`, the stand-in for ssh.
type stdioConn struct {
	io.Reader
	io.WriteCloser
}

// open asks for a ticket and opens a data connection, over the socket or, with viaConnect,
// through `gff-hab connect` (spec 4.2, option A: one bridge per data connection).
func (e *habEnv) open(c *control.Client, pane string, off *uint64, observe, viaConnect bool) (*datastream.Client, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var tk api.Ticket
	if err := c.Call(ctx, "data.ticket", api.TicketParams{Pane: pane, Observe: observe}, &tk); err != nil {
		e.t.Fatal(err)
	}
	var rw io.ReadWriteCloser
	if viaConnect {
		cmd := exec.Command(bin, "connect", "-socket", e.sock)
		cmd.Stderr = os.Stderr
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			e.t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		e.t.Cleanup(func() {
			in.Close()
			select {
			case <-done:
			case <-time.After(scaleTimeout(10 * time.Second)):
				cmd.Process.Kill()
				<-done
			}
		})
		rw = stdioConn{Reader: out, WriteCloser: in}
	} else {
		nc, err := net.Dial("unix", e.sock)
		if err != nil {
			e.t.Fatal(err)
		}
		e.t.Cleanup(func() { nc.Close() })
		rw = nc
	}
	dc, _, err := datastream.Open(rw, api.DataHello{Ticket: tk.Ticket, Offset: off})
	return dc, err
}

// reader pumps one data connection's frames into a channel, returning credit for each frame
// while credit is on.
type reader struct {
	c      *datastream.Client
	ch     chan datastream.Msg
	mu     sync.Mutex
	credit bool
	err    error
}

func startReader(c *datastream.Client, credit bool) *reader {
	r := &reader{c: c, ch: make(chan datastream.Msg, 1<<16), credit: credit}
	go func() {
		defer close(r.ch)
		for {
			m, err := c.Next()
			if err != nil {
				r.mu.Lock()
				r.err = err
				r.mu.Unlock()
				return
			}
			r.mu.Lock()
			give := r.credit
			r.mu.Unlock()
			if give && m.Cost > 0 {
				c.Credit(m.Cost)
			}
			r.ch <- m
		}
	}()
	return r
}

// log is what a client applied: its frames in order.
type streamLog struct {
	frames []datastream.Msg
	end    uint64 // offset after the last frame: what the client stores to resume
	text   []byte // every bytes frame's data, joined
}

func (l *streamLog) add(m datastream.Msg) {
	l.frames = append(l.frames, m)
	switch m.Kind {
	case api.FrameBytes:
		l.text = append(l.text, m.Data...)
		l.end = m.End()
	case api.FrameSnapshot:
		l.end = m.End()
	}
}

// until reads frames into l until cond holds. It fails the test when the stream ends first,
// unless ended is allowed, and returns whether the stream ended.
func (r *reader) until(t *testing.T, l *streamLog, what string, endOK bool, cond func() bool) bool {
	t.Helper()
	deadline := time.After(scaleTimeout(30 * time.Second))
	for !cond() {
		select {
		case m, ok := <-r.ch:
			if !ok {
				if endOK {
					return true
				}
				r.mu.Lock()
				err := r.err
				r.mu.Unlock()
				t.Fatalf("waiting for %s: the stream ended: %v", what, err)
			}
			l.add(m)
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	return false
}

// ringBytes stops gff-hab by plan and reads pane's whole ring straight from the holder.
func (e *habEnv) ringBytes(pane string) []byte {
	e.t.Helper()
	e.srv.stop(e.t)
	u, ok := parseUUID(pane)
	if !ok {
		e.t.Fatalf("pane id %q", pane)
	}
	hc := holderclient.New(holderclient.Config{Dir: e.hdir, Build: build})
	defer hc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	if err := hc.Scan(ctx); err != nil {
		e.t.Fatal(err)
	}
	p := hc.Pane(u)
	if p == nil {
		e.t.Fatalf("the holder lists no pane %s", pane)
	}
	data, end, err := p.Holder.Replay(ctx, p.Handle, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	if end.Gap || end.Start != 0 {
		e.t.Fatalf("the ring lost the pane's first bytes: %+v", end)
	}
	return data
}

// expectedText is the holder's bytes with every terminal query left out, as (offset, byte)
// pairs from offset from to to.
func expectedText(ring []byte, from, to uint64) (text []byte, offs []uint64) {
	s := termq.NewScanner(0)
	s.Feed(ring, func(seg termq.Segment) {
		if seg.Query != nil {
			return
		}
		for i, b := range seg.Data {
			o := seg.Off + uint64(i)
			if o >= from && o < to {
				text = append(text, b)
				offs = append(offs, o)
			}
		}
	})
	return text, offs
}

// checkAgainstRing compares the bytes frames of logs, applied in order, with the holder's
// ring: the same bytes at the same offsets, no gap, no repeat.
func checkAgainstRing(t *testing.T, ring []byte, from uint64, logs ...*streamLog) {
	t.Helper()
	var got []byte
	var offs []uint64
	for _, l := range logs {
		for _, m := range l.frames {
			if m.Kind == api.FrameSnapshot {
				t.Fatalf("a snapshot at %d in a stream that should need none", m.Offset)
			}
			if m.Kind != api.FrameBytes {
				continue
			}
			for i, b := range m.Data {
				got = append(got, b)
				offs = append(offs, m.Offset+uint64(i))
			}
		}
	}
	if len(offs) == 0 {
		t.Fatal("no bytes")
	}
	want, wantOffs := expectedText(ring, from, offs[len(offs)-1]+1)
	if !bytes.Equal(got, want) {
		i := 0
		for i < min(len(got), len(want)) && got[i] == want[i] {
			i++
		}
		t.Fatalf("client bytes differ from the ring at byte %d (got %d bytes, want %d): got %q, want %q",
			i, len(got), len(want), clip(got[i:]), clip(want[i:]))
	}
	for i := range offs {
		if offs[i] != wantOffs[i] {
			t.Fatalf("byte %d has offset %d, the ring has it at %d", i, offs[i], wantOffs[i])
		}
	}
}

func clip(b []byte) []byte { return b[:min(len(b), 60)] }

// printer prints numbered lines with a cursor position query in each, in bursts, then DONE.
// A background cat reads the answers, so the input queue never fills.
const printer = `stty raw -echo
cat > /dev/null &
i=0
while [ $i -lt ${LINES_N:-3000} ]; do
  printf 'line %05d \033[6n abcdefghijklmnopqrstuvwxyz\r\n' $i
  i=$((i+1))
  [ $((i % 100)) -eq 0 ] && sleep 0.05
done
printf 'DONE\r\n'
sleep 60`

// TestDataResumeAfterDrop is acceptance test 1 and the stdio transport: a client drops its data
// connection mid-stream and resumes from the offset it holds, the second time through
// `gff-hab connect`. Its bytes equal the holder's ring with the queries left out: no gap, no
// repeat, no snapshot.
func TestDataResumeAfterDrop(t *testing.T) {
	e := newHabEnv(t, 0)
	cc := e.control()
	pane := e.spawn(cc, printer)

	zero := uint64(0)
	dc, err := e.open(cc, pane, &zero, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var first streamLog
	r := startReader(dc, true)
	r.until(t, &first, "40 kB of output", false, func() bool { return first.end > 40000 })
	dc.Close()

	held := first.end
	dc2, err := e.open(cc, pane, &held, false, true)
	if err != nil {
		t.Fatal(err)
	}
	var second streamLog
	r2 := startReader(dc2, true)
	r2.until(t, &second, "DONE", false, func() bool { return bytes.Contains(second.text, []byte("DONE")) })
	if m := second.frames[0]; m.Kind != api.FrameBytes || m.Offset < held {
		t.Fatalf("resume from %d began with %+v", held, m.Kind)
	}
	if bytes.Contains(first.text, []byte("\x1b[6n")) || bytes.Contains(second.text, []byte("\x1b[6n")) {
		t.Error("a client saw a terminal query")
	}
	checkAgainstRing(t, e.ringBytes(pane), 0, &first, &second)
}

// TestDataResumeAcrossRestart is acceptance test 2 (core test 4): a client resumes from its
// stored offset after a planned gff-hab restart and after a SIGKILL, with no snapshot, no gap
// and no repeat.
func TestDataResumeAcrossRestart(t *testing.T) {
	e := newHabEnv(t, 0)
	cc := e.control()
	pane := e.spawn(cc, strings.Replace(printer, "${LINES_N:-3000}", "6000", 1))

	zero := uint64(0)
	dc, err := e.open(cc, pane, &zero, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var logs []*streamLog
	l := &streamLog{}
	logs = append(logs, l)
	r := startReader(dc, true)
	r.until(t, l, "20 kB of output", false, func() bool { return l.end > 20000 })
	held := l.end
	for _, kill := range []bool{false, true} {
		e.restart(kill)
		// The old stream ends with the old gff-hab; what it delivered meanwhile counts.
		r.until(t, l, "the end of the stream", true, func() bool { return false })
		held = l.end
		cc = e.control()
		dc, err = e.open(cc, pane, &held, false, false)
		if err != nil {
			t.Fatalf("resume after restart (kill %v): %v", kill, err)
		}
		l = &streamLog{}
		logs = append(logs, l)
		r = startReader(dc, true)
		want := held + 20000
		r.until(t, l, "more output", false, func() bool { return l.end > want || bytes.Contains(l.text, []byte("DONE")) })
		if m := l.frames[0]; m.Kind != api.FrameBytes || m.Offset < held {
			t.Fatalf("resume from %d after restart (kill %v) began with kind %d at %d", held, kill, m.Kind, m.Offset)
		}
	}
	r.until(t, l, "DONE", false, func() bool { return bytes.Contains(l.text, []byte("DONE")) })
	checkAgainstRing(t, e.ringBytes(pane), 0, logs...)
}

// TestDataSnapshotWhenBytesGone is acceptance test 3: a resume from an offset neither gff-hab's
// buffer nor the holder's ring still holds gets a snapshot at the pane's offset. The restart
// empties gff-hab's buffer (the new one starts at its checkpoint); a 64 KiB ring cannot hold
// the 400 kB the program printed.
func TestDataSnapshotWhenBytesGone(t *testing.T) {
	e := newHabEnv(t, 64<<10)
	cc := e.control()
	pane := e.spawn(cc, `stty raw -echo
while [ ! -e "$DIR/go" ]; do sleep 0.02; done
i=0
while [ $i -lt 8000 ]; do printf 'filler line %05d abcdefghijklmnopqrstuvwxyz\r\n' $i; i=$((i+1)); done
printf 'DONE\r\n'
sleep 60`)
	zero := uint64(0)
	dc, err := e.open(cc, pane, &zero, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.work, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var l streamLog
	r := startReader(dc, true)
	r.until(t, &l, "the first output", false, func() bool { return l.end > 100 })
	held := l.end
	dc.Close()
	waitUntil(t, "the program's last line", func() bool { return paneOffset(t, cc, pane) >= 8000*46+6 })
	final := paneOffset(t, cc, pane)

	e.restart(false)
	cc = e.control()
	dc, err = e.open(cc, pane, &held, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var after streamLog
	r = startReader(dc, true)
	r.until(t, &after, "a frame", false, func() bool { return len(after.frames) > 0 })
	if m := after.frames[0]; m.Kind != api.FrameSnapshot || m.Offset != final || m.Encoding != "snapshot-vt" {
		t.Fatalf("resume from %d past the ring: kind %d at %d, want a snapshot at %d", held, m.Kind, m.Offset, final)
	}
	if !bytes.Contains(after.frames[0].Data, []byte("DONE")) {
		t.Error("the snapshot lacks the screen's last line")
	}
}

// paneOffset reads a pane's holder offset from structure.get.
func paneOffset(t *testing.T, c *control.Client, pane string) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(10*time.Second))
	defer cancel()
	var res api.StructureResult
	if err := c.Call(ctx, "structure.get", nil, &res); err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Panes {
		if p.ID == pane {
			return p.Offset
		}
	}
	t.Fatalf("no pane %s", pane)
	return 0
}

// TestDataFakeFrames is acceptance test 4: a program prints bytes that look like frames and a
// control message. The client gets them as output, inside bytes frames, and nothing else.
func TestDataFakeFrames(t *testing.T) {
	e := newHabEnv(t, 0)
	cc := e.control()
	fake := "\x06\x00\x00\x00\x13{\"cols\":1,\"rows\":1}\x07\x00\x00\x00\x02{}" +
		`{"jsonrpc":"2.0","method":"closing","params":{"reason":"shutdown"}}`
	pane := e.spawn(cc, `stty raw -echo
printf 'begin'
printf '\006\000\000\000\023{"cols":1,"rows":1}\007\000\000\000\002{}'
printf '%s' '{"jsonrpc":"2.0","method":"closing","params":{"reason":"shutdown"}}'
printf 'end'
sleep 60`)
	zero := uint64(0)
	dc, err := e.open(cc, pane, &zero, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var l streamLog
	r := startReader(dc, true)
	r.until(t, &l, "the end marker", false, func() bool { return bytes.Contains(l.text, []byte("end")) })
	if !bytes.Contains(l.text, []byte("begin"+fake+"end")) {
		t.Errorf("output %q lacks the fake frames as text", l.text)
	}
	for _, m := range l.frames {
		if m.Kind != api.FrameBytes {
			t.Errorf("a frame of kind %d from program output", m.Kind)
		}
	}
}

// TestDataTwoClientsOneAnswer is acceptance test 5: two clients on one pane, one of them an
// observer, see the same bytes, and the program's cursor position query gets exactly one answer:
// gff-hab's, from its parser. Neither client sees the query.
func TestDataTwoClientsOneAnswer(t *testing.T) {
	e := newHabEnv(t, 0)
	cc := e.control()
	pane := e.spawn(cc, `stty raw -echo min 0 time 0
printf 'ready\r\n'
while [ ! -e "$DIR/go" ]; do sleep 0.02; done
printf 'ab\033[6n'
sleep 1
a=$(dd bs=256 count=1 2>/dev/null | od -An -c | tr -d ' \n')
printf '\r\ngot:%s:end\r\n' "$a"
sleep 60`)
	zero := uint64(0)
	var logs [2]streamLog
	var rs [2]*reader
	for i := range rs {
		dc, err := e.open(cc, pane, &zero, i == 1, false)
		if err != nil {
			t.Fatal(err)
		}
		rs[i] = startReader(dc, true)
		rs[i].until(t, &logs[i], "ready", false, func() bool { return bytes.Contains(logs[i].text, []byte("ready")) })
	}
	if err := os.WriteFile(filepath.Join(e.work, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range rs {
		rs[i].until(t, &logs[i], "the program's report", false, func() bool { return bytes.Contains(logs[i].text, []byte(":end")) })
	}
	text := string(logs[0].text)
	if got := text[strings.Index(text, "got:")+4 : strings.Index(text, ":end")]; got != "033[2;3R" {
		t.Errorf("the program read %q, want exactly one answer 033[2;3R", got)
	}
	a, b := logs[0].frames, logs[1].frames
	if !bytes.Equal(logs[0].text, logs[1].text) {
		t.Errorf("the clients saw different bytes: %q and %q", logs[0].text, logs[1].text)
	}
	for _, f := range append(append([]datastream.Msg{}, a...), b...) {
		if bytes.Contains(f.Data, []byte("\x1b[6n")) {
			t.Error("a client saw the query")
		}
	}
	// The query is a skip between frames: "ab" ends where the query starts.
	for _, l := range logs {
		for i, f := range l.frames {
			if bytes.HasSuffix(f.Data, []byte("ab")) && i+1 < len(l.frames) {
				if next := l.frames[i+1]; next.Offset != f.End()+4 {
					t.Errorf("after %q at %d the next frame is at %d, want %d", f.Data, f.Offset, next.Offset, f.End()+4)
				}
			}
		}
	}
}

// TestDataSlowClient is acceptance test 6: a client that returns no credit stalls only itself.
// Another client on the same pane gets every byte, and the control connection answers. Once
// the stalled client is more than 4 MiB behind it gets a fresh snapshot instead of the
// backlog, after at most its 256 KiB of credit.
func TestDataSlowClient(t *testing.T) {
	e := newHabEnv(t, 0)
	cc := e.control()
	pane := e.spawn(cc, `stty raw -echo
while [ ! -e "$DIR/go" ]; do sleep 0.02; done
i=0
while [ $i -lt 120 ]; do head -c 50000 /dev/zero | tr '\0' 'x'; printf '\r\n'; i=$((i+1)); done
printf 'DONE\r\n'
sleep 60`)
	slow, err := e.open(cc, pane, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	fastC, err := e.open(cc, pane, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var fast streamLog
	fr := startReader(fastC, true)
	fr.until(t, &fast, "the first snapshot", false, func() bool { return len(fast.frames) > 0 })
	if err := os.WriteFile(filepath.Join(e.work, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fr.until(t, &fast, "DONE on the fast client", false, func() bool { return bytes.Contains(fast.text, []byte("DONE")) })
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout(5*time.Second))
	defer cancel()
	if err := cc.Call(ctx, "hab.ping", nil, nil); err != nil {
		t.Fatalf("the control connection while a stream stalls: %v", err)
	}
	if n := bytes.Count(fast.text, []byte("x")); n != 120*50000 {
		t.Errorf("the fast client got %d of %d bytes", n, 120*50000)
	}

	var sl streamLog
	sr := startReader(slow, true)
	sr.until(t, &sl, "DONE on the slow client", false, func() bool {
		return bytes.Contains(sl.text, []byte("DONE")) || (len(sl.frames) > 1 && bytes.Contains(sl.frames[len(sl.frames)-1].Data, []byte("DONE")))
	})
	snaps, before := 0, 0
	for _, m := range sl.frames {
		if m.Kind == api.FrameSnapshot {
			snaps++
			continue
		}
		if snaps == 1 {
			before += len(m.Data)
		}
	}
	if snaps < 2 {
		t.Fatalf("the stalled client got %d snapshots, want a fresh one after falling behind", snaps)
	}
	if before > datastream.DefaultInitialCredit {
		t.Errorf("the stalled client got %d bytes before its fresh snapshot, past its credit", before)
	}
}
