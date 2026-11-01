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
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// fakeSSHEnv makes the test binary act as ssh: it checks the options the transport must pass,
// then runs the remote command here, with its stdio, in place of a remote shell.
const fakeSSHEnv = "GFF_HABCONN_FAKE_SSH"

// fakeSSHLogEnv names a file the fake ssh appends its arguments to, one line per run.
const fakeSSHLogEnv = "GFF_HABCONN_FAKE_SSH_LOG"

// fakeSSHNoKeyEnv makes the fake ssh fail as ssh does with no usable key: it closes its
// stdout, waits for its stdin to close, then prints the error and exits 255. The message
// comes only after the client gave up on the stream, as a slow ssh's does.
const fakeSSHNoKeyEnv = "GFF_HABCONN_FAKE_SSH_NOKEY"

const noKeyMessage = "hab-test: Permission denied (publickey)."

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
	if log := os.Getenv(fakeSSHLogEnv); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake ssh: %v\n", err)
			return 255
		}
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if os.Getenv(fakeSSHNoKeyEnv) == "1" {
		os.Stdout.Close()
		io.Copy(io.Discard, os.Stdin)
		fmt.Fprintln(os.Stderr, noKeyMessage)
		return 255
	}
	if slices.Contains(args, "-O") {
		return 0 // a control command such as -O exit: no master runs here
	}
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
		return shimSSH(t, h)
	}},
	{"ssh", func(t *testing.T, h *fakeHab) Transport {
		host := sshHost()
		if err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", host, "true").Run(); err != nil {
			t.Skipf("key-based ssh to %s is not set up: %v", host, err)
		}
		tr := SSHTransport{Host: host, Remote: []string{gffHab(t), "connect", "-socket", h.sock}, ControlDir: sockDir(t)}
		t.Cleanup(func() { tr.Reset(context.Background()) }) // runs before sockDir's cleanup
		return tr
	}},
}

// shimSSH is SSHTransport with the test binary standing in for ssh, sharing on, its control
// directory a fresh one.
func shimSSH(t *testing.T, h *fakeHab) SSHTransport {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return SSHTransport{
		SSH:        self,
		Host:       "hab-test",
		Remote:     []string{gffHab(t), "connect", "-socket", h.sock},
		ControlDir: sockDir(t),
		Env:        append(os.Environ(), fakeSSHEnv+"=1"),
	}
}

// sockDir returns a fresh control directory short enough for socket paths on macOS, where the
// temporary directory is too long.
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "gffc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
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
	// A dead link may be a shared connection that died: the reconnect resets it first.
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if want := []string{"dial", "reset", "dial"}; !slices.Equal(ft.log, want) {
		t.Fatalf("transport calls %q, want %q", ft.log, want)
	}
}

// freezeTransport wraps streams so that, once frozen, their reads deliver nothing until Close.
// It is a Resetter and logs each Dial and Reset.
type freezeTransport struct {
	inner   Transport
	mu      sync.Mutex
	streams []*freezeStream
	log     []string
	// onWrite, when set, sees every write before it is sent; it may freeze the transport.
	onWrite func(p []byte)
}

func (f *freezeTransport) Dial(ctx context.Context, k Kind) (io.ReadWriteCloser, error) {
	rw, err := f.inner.Dial(ctx, k)
	if err != nil {
		return nil, err
	}
	s := &freezeStream{ReadWriteCloser: rw, f: f, frozen: make(chan struct{}), closed: make(chan struct{})}
	f.mu.Lock()
	f.streams = append(f.streams, s)
	f.log = append(f.log, "dial")
	f.mu.Unlock()
	return s, nil
}

func (f *freezeTransport) Reset(context.Context) error {
	f.mu.Lock()
	f.log = append(f.log, "reset")
	f.mu.Unlock()
	return nil
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
	f         *freezeTransport
	once      sync.Once
	frozen    chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

// Read delivers nothing once frozen, also when the freeze came during a read: what arrives
// after it is lost, as on a dead link.
func (s *freezeStream) Read(p []byte) (int, error) {
	n, err := 0, error(nil)
	select {
	case <-s.frozen:
	default:
		n, err = s.ReadWriteCloser.Read(p)
	}
	select {
	case <-s.frozen:
		<-s.closed
		return 0, net.ErrClosed
	default:
	}
	return n, err
}

func (s *freezeStream) Write(p []byte) (int, error) {
	if s.f.onWrite != nil {
		s.f.onWrite(p)
	}
	return s.ReadWriteCloser.Write(p)
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

// TestSSHErrorShown: when ssh fails, the error carries what ssh printed, even when ssh prints
// it only as it exits, after the stream already ended.
func TestSSHErrorShown(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tr := SSHTransport{SSH: self, Host: "hab-test", NoSharing: true, Env: append(os.Environ(), fakeSSHEnv+"=1", fakeSSHNoKeyEnv+"=1")}
	_, err = Connect(testCtx(t), Config{Transport: tr, Client: testClient})
	if err == nil || !strings.Contains(err.Error(), noKeyMessage) {
		t.Fatalf("connect error %v, want ssh's message %q", err, noKeyMessage)
	}
}

// TestResetBeforeRetry: with sharing on, the attempt after a failed open first tells the
// shared connection's master to exit, so a master that died in sleep cannot hold the
// reconnect. Retry starts that attempt at once.
func TestResetBeforeRetry(t *testing.T) {
	h := startFakeHab(t)
	tr := shimSSH(t, h)
	log := filepath.Join(sockDir(t), "ssh.log")
	tr.Env = append(tr.Env, fakeSSHLogEnv+"="+log)
	failed := make(chan struct{}, 1)
	c := connect(t, tr, func(cfg *Config) {
		cfg.MinBackoff, cfg.MaxBackoff = time.Hour, time.Hour // only Retry ends the wait
		cfg.Logf = func(format string, _ ...any) {
			if strings.HasPrefix(format, "habconn: reconnect:") {
				select {
				case failed <- struct{}{}:
				default:
				}
			}
		}
	})
	h.stop()
	waitUpdate(t, c, "offline", func(u Update) bool { return u.Kind == UpdateOffline })
	select {
	case <-failed:
	case <-time.After(waitLong()):
		t.Fatal("no failed reconnect")
	}
	h.start()
	c.Retry()
	waitUpdate(t, c, "online", func(u Update) bool { return u.Kind == UpdateOnline })

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	exit := "-O exit -- hab-test"
	var kinds []string
	for _, l := range lines {
		switch {
		case strings.HasSuffix(l, exit):
			if !strings.Contains(l, "ControlPath="+filepath.Join(tr.ControlDir, "%C")) {
				t.Fatalf("reset without the control path: %s", l)
			}
			kinds = append(kinds, "exit")
		case strings.Contains(l, " connect "):
			kinds = append(kinds, "connect")
		default:
			t.Fatalf("unexpected ssh run: %s", l)
		}
	}
	// The first connect came online, the second failed with the hab down, the third came
	// online after Retry; only that one follows a failure, so only it is reset first.
	if want := []string{"connect", "connect", "exit", "connect"}; !slices.Equal(kinds, want) {
		t.Fatalf("ssh runs %q, want %q", kinds, want)
	}
}

// TestRetryWhileOnline: a Retry sent while online is forgotten when a reconnect completes, so
// it cannot cut the first backoff of a later outage short.
func TestRetryWhileOnline(t *testing.T) {
	h := startFakeHab(t)
	c := connect(t, pipeTransport{h}, nil)
	c.Retry()
	h.dropControl()
	waitUpdate(t, c, "online", func(u Update) bool { return u.Kind == UpdateOnline })
	if n := len(c.retry); n != 0 {
		t.Fatalf("%d stale Retry left after the reconnect", n)
	}
}

// TestDropDuringCall: the link dies while a call waits for its answer. The call fails with
// the closed connection instead of hanging, is never sent again, and a call after the
// reconnect works.
func TestDropDuringCall(t *testing.T) {
	h := startFakeHab(t)
	ft := &freezeTransport{inner: pipeTransport{h}}
	var sent atomic.Int32
	ft.onWrite = func(p []byte) {
		if bytes.Contains(p, []byte(`"method":"structure.get"`)) && sent.Add(1) == 1 {
			ft.freeze() // the answer to the first call never arrives
		}
	}
	c := connect(t, ft, func(cfg *Config) {
		cfg.DeadAfter = 300 * time.Millisecond
		cfg.PingInterval = 100 * time.Millisecond
	})
	var r api.StructureResult
	if err := c.Call(testCtx(t), "structure.get", api.StructureGetParams{}, &r); !errors.Is(err, control.ErrClosed) {
		t.Fatalf("call across a dead link: %v", err)
	}
	waitUpdate(t, c, "online", func(u Update) bool { return u.Kind == UpdateOnline })
	if err := c.Call(testCtx(t), "structure.get", api.StructureGetParams{}, &r); err != nil {
		t.Fatalf("call after the reconnect: %v", err)
	}
	if n := sent.Load(); n != 2 {
		t.Fatalf("structure.get sent %d times, want 2: a call was replayed", n)
	}
}

// TestPinAcrossReconnect: the first welcome's device id stays pinned across reconnects; a hab
// that comes back with another id stops the Conn.
func TestPinAcrossReconnect(t *testing.T) {
	h := startFakeHab(t)
	c := connect(t, pipeTransport{h}, nil)
	h.restart(false) // same id: the reconnect is fine
	waitUpdate(t, c, "online", func(u Update) bool { return u.Kind == UpdateOnline })
	h.mu.Lock()
	h.deviceID = "SHA256:other"
	h.mu.Unlock()
	h.restart(false)
	u := waitUpdate(t, c, "failed", func(u Update) bool { return u.Kind == UpdateFailed })
	var dc *DeviceChangedError
	if !errors.As(u.Err, &dc) || dc.Pinned != "SHA256:test" || dc.Got != "SHA256:other" {
		t.Fatalf("failed with %v, want a device change from SHA256:test to SHA256:other", u.Err)
	}
	if _, open := <-c.Updates(); open {
		t.Fatal("updates still open after the failure")
	}
	if !errors.As(c.Err(), &dc) {
		t.Fatalf("Err %v", c.Err())
	}
}
