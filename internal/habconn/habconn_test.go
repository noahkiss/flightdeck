package habconn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// fakeSSHEnv makes the test binary act as ssh: it checks the options the transport must pass,
// then runs the remote command here, with its stdio, in place of a remote shell.
const fakeSSHEnv = "GFF_HABCONN_FAKE_SSH"

func TestMain(m *testing.M) {
	if os.Getenv(fakeSSHEnv) == "1" {
		os.Exit(fakeSSH(os.Args[1:]))
	}
	code := m.Run()
	if gffHabDir != "" {
		os.RemoveAll(gffHabDir)
	}
	os.Exit(code)
}

func fakeSSH(args []string) int {
	i := slices.Index(args, "--")
	if i < 0 || len(args) < i+3 || !slices.Contains(args[:i], "-T") || !slices.Contains(args[:i], "BatchMode=yes") {
		fmt.Fprintf(os.Stderr, "fake ssh: unexpected arguments %q\n", args)
		return 255
	}
	remote := args[i+2:]
	cmd := exec.Command(remote[0], remote[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "fake ssh: %v\n", err)
		return 255
	}
	return 0
}

var (
	gffHabOnce sync.Once
	gffHabDir  string
	gffHabBin  string
	gffHabErr  error
)

// gffHab builds the real gff-hab once, for its `connect` bridge.
func gffHab(t *testing.T) string {
	t.Helper()
	gffHabOnce.Do(func() {
		gffHabDir, gffHabErr = os.MkdirTemp("", "gffhcbin")
		if gffHabErr != nil {
			return
		}
		gffHabBin = filepath.Join(gffHabDir, "gff-hab")
		cmd := exec.Command("go", "build", "-o", gffHabBin, "github.com/noahkiss/flightdeck/cmd/gff-hab")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			gffHabErr = fmt.Errorf("build gff-hab: %v\n%s", err, out)
		}
	})
	if gffHabErr != nil {
		t.Fatal(gffHabErr)
	}
	return gffHabBin
}

// pipeTransport is a second transport: an in-process pipe straight into the hab, with no
// socket and no command.
type pipeTransport struct{ h *fakeHab }

func (p pipeTransport) Dial(context.Context, Kind) (io.ReadWriteCloser, error) {
	a, b := net.Pipe()
	go p.h.serveConn(a)
	return b, nil
}

// sshHost is the host the real-ssh tests reach: $GFF_TEST_SSH_HOST, else localhost.
func sshHost() string {
	if h := os.Getenv("GFF_TEST_SSH_HOST"); h != "" {
		return h
	}
	return "localhost"
}

var transports = []struct {
	name string
	make func(t *testing.T, h *fakeHab) Transport
}{
	{"unix", func(t *testing.T, h *fakeHab) Transport { return UnixTransport{Path: h.sock} }},
	{"pipe", func(t *testing.T, h *fakeHab) Transport { return pipeTransport{h} }},
	{"ssh-shim", func(t *testing.T, h *fakeHab) Transport {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		return SSHTransport{
			SSH:    self,
			Host:   "hab-test",
			Remote: []string{gffHab(t), "connect", "-socket", h.sock},
			Env:    append(os.Environ(), fakeSSHEnv+"=1"),
		}
	}},
	{"ssh", func(t *testing.T, h *fakeHab) Transport {
		host := sshHost()
		if err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", host, "true").Run(); err != nil {
			t.Skipf("key-based ssh to %s is not set up: %v", host, err)
		}
		return SSHTransport{Host: host, Remote: []string{gffHab(t), "connect", "-socket", h.sock}}
	}},
}

var testClient = api.ClientInfo{Name: "habconn-test", Version: "0", Kind: api.ClientKindScript}

func connect(t *testing.T, tr Transport, tweak func(*Config)) *Conn {
	t.Helper()
	cfg := Config{Transport: tr, Client: testClient, MinBackoff: 10 * time.Millisecond, MaxBackoff: 200 * time.Millisecond}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := Connect(testCtx(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	u := <-c.Updates()
	if u.Kind != UpdateOnline || u.NewEpoch {
		t.Fatalf("first update: %+v", u)
	}
	return c
}

// waitUpdate reads updates until ok accepts one.
func waitUpdate(t *testing.T, c *Conn, what string, ok func(Update) bool) Update {
	t.Helper()
	timer := time.NewTimer(waitLong())
	defer timer.Stop()
	for {
		select {
		case u, open := <-c.Updates():
			if !open {
				t.Fatalf("waiting for %s: updates closed (%v)", what, c.Err())
			}
			if ok(u) {
				return u
			}
		case <-timer.C:
			t.Fatalf("waiting for %s: timed out", what)
		}
	}
}

// norm puts a structure in a form to compare: lists sorted by id, as JSON.
func norm(st api.Structure) string {
	slices.SortFunc(st.Projects, func(a, b api.Project) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(st.Sessions, func(a, b api.Session) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(st.Panes, func(a, b api.Pane) int { return strings.Compare(a.ID, b.ID) })
	b, _ := json.Marshal(api.NonNil(st))
	return string(b)
}

// waitStructure reads updates until the client's copy equals the hab's structure.
func waitStructure(t *testing.T, c *Conn, h *fakeHab) {
	t.Helper()
	if norm(c.Structure()) == norm(h.snapshot()) {
		return
	}
	waitUpdate(t, c, "the structure to match the hab", func(Update) bool {
		return norm(c.Structure()) == norm(h.snapshot())
	})
}

// produce writes n bytes of numbered lines to the pane in pieces, then returns.
func produce(src *paneSource, n int) {
	var line int
	var buf []byte
	for written := 0; written < n; {
		buf = buf[:0]
		for len(buf) < 4<<10 {
			buf = fmt.Appendf(buf, "line %07d of the test output\r\n", line)
			line++
		}
		src.write(buf)
		written += len(buf)
	}
}

// readBytes reads frames until the stream holds want bytes from offset 0, appending to got.
// Every bytes frame must start where the last one ended, and no snapshot may come. Each data
// connection the stream opens on the way must say hello with the offset the client held.
func readBytes(t *testing.T, h *fakeHab, p *PaneStream, got []byte, want int, each func(got []byte)) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*waitLong())
	defer cancel()
	for len(got) < want {
		opens, seen := p.Opens(), len(h.src.subscribes())
		held, ok := p.Offset()
		if !ok || held != uint64(len(got)) {
			t.Fatalf("offset %d %v before a read, want %d", held, ok, len(got))
		}
		f, err := p.Next(ctx)
		if err != nil {
			t.Fatalf("next at %d of %d: %v", len(got), want, err)
		}
		if p.Opens() > opens {
			checkResume(t, h, seen, held)
		}
		switch f.Kind {
		case api.FrameBytes:
			if f.Offset != uint64(len(got)) {
				t.Fatalf("bytes frame at %d, want %d: a gap or a repeat", f.Offset, len(got))
			}
			got = append(got, f.Data...)
			if each != nil {
				each(got)
			}
		case api.FrameSnapshot:
			t.Fatalf("snapshot at %d: the resume should have been served from bytes", f.Offset)
		}
	}
	noTrim(t, p)
	return got
}

// checkResume: every data hello the hab got since the first seen ones carried offset held.
func checkResume(t *testing.T, h *fakeHab, seen int, held uint64) {
	t.Helper()
	froms := h.src.subscribes()[seen:]
	if len(froms) == 0 {
		t.Fatal("a data connection opened, but the hab saw no data hello")
	}
	for _, from := range froms {
		if from != held {
			t.Fatalf("data hello with offset %d, want the client's offset %d", from, held)
		}
	}
}

// noTrim: the hab never sent a byte the client already had, so the repeat guard dropped none.
func noTrim(t *testing.T, p *PaneStream) {
	t.Helper()
	p.mu.Lock()
	n := p.trimmed
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("the hab repeated %d bytes the client already had", n)
	}
}

func TestSuite(t *testing.T) {
	for _, tc := range transports {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("ResumeAfterDrop", func(t *testing.T) {
				h := startFakeHab(t)
				testResumeAfterDrop(t, h, tc.make(t, h))
			})
			t.Run("NewEpoch", func(t *testing.T) {
				h := startFakeHab(t)
				testNewEpoch(t, h, tc.make(t, h))
			})
			t.Run("SameEpoch", func(t *testing.T) {
				h := startFakeHab(t)
				testSameEpoch(t, h, tc.make(t, h))
			})
			t.Run("SnapshotWhenTrimmed", func(t *testing.T) {
				h := startFakeHab(t)
				testSnapshotWhenTrimmed(t, h, tc.make(t, h))
			})
		})
	}
}

// testResumeAfterDrop: the hab drops the data connection three times while output flows; the
// client ends with exactly the pane's output, with no gap, no repeat and no snapshot.
func testResumeAfterDrop(t *testing.T, h *fakeHab, tr Transport) {
	c := connect(t, tr, nil)
	zero := uint64(0)
	p, err := c.OpenPane(testCtx(t), testPane, PaneOptions{Offset: &zero})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const total = 2 << 20 // below the hab's 4 MiB behind limit, so no snapshot replaces bytes
	done := make(chan struct{})
	go func() { defer close(done); produce(h.src, total) }()
	drops := 0
	got := readBytes(t, h, p, nil, total, func(got []byte) {
		if drops < 3 && len(got) >= (drops+1)*total/4 && h.dropData() > 0 {
			drops++
		}
	})
	<-done
	want, end := h.src.output()
	got = readBytes(t, h, p, got, int(end), nil)
	if drops != 3 {
		t.Fatalf("dropped %d data connections, want 3", drops)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("client bytes differ from the pane's output (%d and %d bytes)", len(got), len(want))
	}
	if n := p.Opens(); n != 4 {
		t.Fatalf("opened %d data connections, want 4", n)
	}
	if off, ok := p.Offset(); !ok || off != end {
		t.Fatalf("offset %d %v, want %d", off, ok, end)
	}
}

// testNewEpoch: the hab restarts with its state file lost. The client sees a new epoch,
// replaces its structure with the fresh snapshot although the new revisions are lower, applies
// later events, and its pane stream resumes from its offset across the restart.
func testNewEpoch(t *testing.T, h *fakeHab, tr Transport) {
	c := connect(t, tr, nil)
	h.createSession("one")
	waitStructure(t, c, h)
	before, _ := c.Welcome()

	zero := uint64(0)
	p, err := c.OpenPane(testCtx(t), testPane, PaneOptions{Offset: &zero})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	produce(h.src, 64<<10)
	_, end := h.src.output()
	got := readBytes(t, h, p, nil, int(end), nil)

	h.restart(true)
	u := waitUpdate(t, c, "online after the restart", func(u Update) bool { return u.Kind == UpdateOnline })
	if !u.NewEpoch || u.Welcome.Epoch == before.Epoch || u.Welcome.Epoch != h.epoch() {
		t.Fatalf("after restart: new epoch %v, epoch %s (was %s, hab %s)", u.NewEpoch, u.Welcome.Epoch, before.Epoch, h.epoch())
	}
	if norm(u.Structure) != norm(h.snapshot()) || norm(c.Structure()) != norm(h.snapshot()) {
		t.Fatalf("structure after a new epoch:\n got %s\nwant %s", norm(c.Structure()), norm(h.snapshot()))
	}
	if len(u.Structure.Sessions) != 0 {
		t.Fatalf("the lost state's session survived: %+v", u.Structure.Sessions)
	}
	h.createSession("two")
	waitStructure(t, c, h)

	produce(h.src, 64<<10)
	want, end := h.src.output()
	got = readBytes(t, h, p, got, int(end), nil)
	if !bytes.Equal(got, want) {
		t.Fatal("client bytes differ from the pane's output across the restart")
	}
}

// testSameEpoch: the hab drops the control connection. The client reconnects with its token,
// keeps its client id and epoch, and reads state again.
func testSameEpoch(t *testing.T, h *fakeHab, tr Transport) {
	c := connect(t, tr, nil)
	before, _ := c.Welcome()
	if h.dropControl() == 0 {
		t.Fatal("no control connection to drop")
	}
	waitUpdate(t, c, "offline", func(u Update) bool { return u.Kind == UpdateOffline })
	u := waitUpdate(t, c, "online", func(u Update) bool { return u.Kind == UpdateOnline })
	if u.NewEpoch || u.Welcome.ClientID != before.ClientID || u.Welcome.ReconnectToken == before.ReconnectToken {
		t.Fatalf("reconnect: new epoch %v, client %s (was %s)", u.NewEpoch, u.Welcome.ClientID, before.ClientID)
	}
	h.createSession("after")
	waitStructure(t, c, h)
	var r api.StructureResult
	if err := c.Call(testCtx(t), "structure.get", api.StructureGetParams{}, &r); err != nil {
		t.Fatal(err)
	}
}

// testSnapshotWhenTrimmed: the hab no longer has the bytes after the client's offset, so the
// resume starts with a fresh snapshot, and bytes follow from the snapshot's offset.
func testSnapshotWhenTrimmed(t *testing.T, h *fakeHab, tr Transport) {
	c := connect(t, tr, nil)
	zero := uint64(0)
	p, err := c.OpenPane(testCtx(t), testPane, PaneOptions{Offset: &zero})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	produce(h.src, 8<<10)
	_, end := h.src.output()
	readBytes(t, h, p, nil, int(end), nil)
	// Trim with no live subscriber: drop the stream, wait until the hab has none, then trim.
	p.kick()
	produce(h.src, 8<<10)
	_, end = h.src.output()
	h.src.trim(end)
	produce(h.src, 100)
	_, snapAt := h.src.output()

	ctx := testCtx(t)
	held, _ := p.Offset()
	seen := len(h.src.subscribes())
	f, err := p.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkResume(t, h, seen, held)
	if f.Kind != api.FrameSnapshot || f.Offset != snapAt {
		t.Fatalf("first frame after the trim: kind %d at %d, want a snapshot at %d", f.Kind, f.Offset, snapAt)
	}
	produce(h.src, 100)
	out, final := h.src.output()
	want := out[snapAt-end:]
	var got []byte
	for uint64(len(got)) < final-snapAt {
		f, err := p.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Kind == api.FrameBytes {
			if f.Offset != snapAt+uint64(len(got)) {
				t.Fatalf("bytes at %d, want %d", f.Offset, snapAt+uint64(len(got)))
			}
			got = append(got, f.Data...)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("bytes after the snapshot differ")
	}
	noTrim(t, p)
}

// TestDeadLink: a link that stops delivering anything is found dead after DeadAfter, and the
// client reconnects.
func TestDeadLink(t *testing.T) {
	h := startFakeHab(t)
	ft := &freezeTransport{inner: pipeTransport{h}}
	c := connect(t, ft, func(cfg *Config) {
		cfg.DeadAfter = 300 * time.Millisecond
		cfg.PingInterval = 100 * time.Millisecond
	})
	ft.freeze()
	u := waitUpdate(t, c, "offline", func(u Update) bool { return u.Kind == UpdateOffline })
	if !errors.Is(u.Err, errDead) {
		t.Fatalf("offline because %v, want a dead link", u.Err)
	}
	waitUpdate(t, c, "online", func(u Update) bool { return u.Kind == UpdateOnline })
}

// freezeTransport wraps streams so that, once frozen, their reads deliver nothing until Close.
type freezeTransport struct {
	inner   Transport
	mu      sync.Mutex
	streams []*freezeStream
}

func (f *freezeTransport) Dial(ctx context.Context, k Kind) (io.ReadWriteCloser, error) {
	rw, err := f.inner.Dial(ctx, k)
	if err != nil {
		return nil, err
	}
	s := &freezeStream{ReadWriteCloser: rw, frozen: make(chan struct{}), closed: make(chan struct{})}
	f.mu.Lock()
	f.streams = append(f.streams, s)
	f.mu.Unlock()
	return s, nil
}

func (f *freezeTransport) freeze() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.streams {
		s.once.Do(func() { close(s.frozen) })
	}
	f.streams = nil
}

type freezeStream struct {
	io.ReadWriteCloser
	once      sync.Once
	frozen    chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

func (s *freezeStream) Read(p []byte) (int, error) {
	select {
	case <-s.frozen:
		<-s.closed
		return 0, net.ErrClosed
	default:
	}
	return s.ReadWriteCloser.Read(p)
}

func (s *freezeStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return s.ReadWriteCloser.Close()
}

// TestPaneGone: a pane the hab does not have ends the stream with ErrPaneGone.
func TestPaneGone(t *testing.T) {
	h := startFakeHab(t)
	c := connect(t, pipeTransport{h}, nil)
	if _, err := c.OpenPane(testCtx(t), "nope", PaneOptions{}); !errors.Is(err, ErrPaneGone) {
		t.Fatalf("open a missing pane: %v", err)
	}
}

// TestOfflineRefusesCalls: while the hab is down, calls fail at once instead of waiting.
func TestOfflineRefusesCalls(t *testing.T) {
	h := startFakeHab(t)
	c := connect(t, pipeTransport{h}, nil)
	h.stop()
	waitUpdate(t, c, "offline", func(u Update) bool { return u.Kind == UpdateOffline })
	if err := c.Call(testCtx(t), "hab.ping", api.PingParams{}, nil); !errors.Is(err, ErrOffline) {
		t.Fatalf("call while offline: %v", err)
	}
}

// TestDeviceChanged: a welcome with another device id than the pinned one stops the Conn.
func TestDeviceChanged(t *testing.T) {
	h := startFakeHab(t)
	_, err := Connect(testCtx(t), Config{Transport: pipeTransport{h}, Client: testClient, DeviceID: "SHA256:other"})
	var dc *DeviceChangedError
	if !errors.As(err, &dc) || dc.Got != "SHA256:test" {
		t.Fatalf("connect with another pinned id: %v", err)
	}
}

// TestCloseWakesNext: with the hab down, Next waits for the control connection to come back;
// Close from another goroutine ends that wait with ErrClosed.
func TestCloseWakesNext(t *testing.T) {
	h := startFakeHab(t)
	ended := make(chan struct{}, 1)
	c := connect(t, pipeTransport{h}, func(cfg *Config) {
		cfg.Logf = func(format string, _ ...any) {
			if strings.Contains(format, "data connection ended") {
				select {
				case ended <- struct{}{}:
				default:
				}
			}
		}
	})
	p, err := c.OpenPane(testCtx(t), testPane, PaneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h.stop()
	errc := make(chan error, 1)
	go func() {
		for {
			if _, err := p.Next(testCtx(t)); err != nil {
				errc <- err
				return
			}
		}
	}()
	// The log comes after the data connection ended and before Next waits to reopen it.
	select {
	case <-ended:
	case <-time.After(waitLong()):
		t.Fatal("the data connection did not end with the hab")
	}
	p.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("next after close: %v", err)
		}
	case <-time.After(waitLong()):
		t.Fatal("Close did not wake Next")
	}
}

// TestFrames: the channel delivers the pane's bytes in order across a dropped data connection,
// and closes with ErrClosed after Close.
func TestFrames(t *testing.T) {
	h := startFakeHab(t)
	c := connect(t, pipeTransport{h}, nil)
	zero := uint64(0)
	p, err := c.OpenPane(testCtx(t), testPane, PaneOptions{Offset: &zero})
	if err != nil {
		t.Fatal(err)
	}
	produce(h.src, 256<<10)
	want, end := h.src.output()
	var got []byte
	dropped := false
	timer := time.NewTimer(waitLong())
	defer timer.Stop()
	for uint64(len(got)) < end {
		select {
		case f, ok := <-p.Frames():
			if !ok {
				t.Fatalf("frames closed at %d: %v", len(got), p.Err())
			}
			if f.Kind != api.FrameBytes {
				t.Fatalf("frame kind %d at %d", f.Kind, f.Offset)
			}
			if f.Offset != uint64(len(got)) {
				t.Fatalf("bytes at %d, want %d", f.Offset, len(got))
			}
			got = append(got, f.Data...)
			if !dropped && len(got) >= len(want)/2 && h.dropData() > 0 {
				dropped = true
			}
		case <-timer.C:
			t.Fatalf("timed out at %d of %d", len(got), end)
		}
	}
	if !bytes.Equal(got, want) || !dropped {
		t.Fatalf("bytes equal %v, dropped %v", bytes.Equal(got, want), dropped)
	}
	p.Close()
	for range p.Frames() {
	}
	if err := p.Err(); !errors.Is(err, ErrClosed) {
		t.Fatalf("frames ended with %v, want ErrClosed", err)
	}
}
