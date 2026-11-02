package holderclient

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/holder"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/testtmp"
)

// waitLong bounds every wait. Conditions are waited for, never slept on.
// GFF_TEST_TIMEOUT_SCALE multiplies it on slow machines; see docs/testing.md.
var waitLong = scaleTimeout(15 * time.Second)

// scaleTimeout multiplies d by $GFF_TEST_TIMEOUT_SCALE, a positive number such as 3 or 2.5.
// Unset, it returns d. A value that does not parse panics, so a typo is not silently ignored.
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

var testEnv = []string{"PATH=/usr/bin:/bin", "TERM=dumb", "LC_ALL=C", "HOME=/"}

// binDir holds the holder binaries TestMain builds, one per build id.
var binDir string

// TestMain builds cmd/gff-hab-holder twice, as build A and build B, so the tests can run two
// real holder builds side by side.
func TestMain(m *testing.M) {
	// The drain helper process runs no holder of its own, and the test kills it: it makes no
	// directory, since it would never get to remove one.
	if _, err := exec.LookPath("go"); err != nil || os.Getenv(helperEnv) != "" {
		os.Exit(m.Run())
	}
	dir, err := testtmp.MainDir("fdbin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, b := range []string{"A", "B"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "holder-"+b),
			"-ldflags", "-X main.build="+b, "github.com/noahkiss/flightdeck/cmd/gff-hab-holder")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build holder %s: %v\n%s", b, err, out)
			testtmp.Exit(1, dir)
		}
	}
	binDir = dir
	testtmp.Exit(m.Run(), dir)
}

// regDir returns a fresh registry directory with a short path (macOS caps socket paths at 104
// bytes).
func regDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fdc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// proc is a holder process started the way an init system would: its own process, reaped by
// the starter, never a child the client knows about.
type proc struct {
	instance string
	cmd      *exec.Cmd
	stderr   *syncBuffer
	done     chan struct{}
	err      error
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// procs is a fake init system for holder binaries.
type procs struct {
	t   *testing.T
	dir string
	mu  sync.Mutex
	all []*proc
}

func newProcs(t *testing.T, dir string) *procs {
	t.Helper()
	if binDir == "" {
		t.Skip("no go toolchain to build the holder binary")
	}
	ps := &procs{t: t, dir: dir}
	t.Cleanup(func() {
		ps.mu.Lock()
		all := ps.all
		ps.mu.Unlock()
		for _, p := range all {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-p.done:
			case <-time.After(waitLong):
				_ = p.cmd.Process.Kill()
				<-p.done
			}
			if t.Failed() {
				t.Logf("holder %s log:\n%s", p.instance, p.stderr.String())
			}
		}
	})
	return ps
}

// start runs holder build b as instance id.
func (ps *procs) start(b, id string) *proc {
	ps.t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "holder-"+b), "-dir", ps.dir, "-instance", id, "-ring", "65536")
	p := &proc{instance: id, cmd: cmd, stderr: &syncBuffer{}, done: make(chan struct{})}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		ps.t.Fatal(err)
	}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	ps.mu.Lock()
	ps.all = append(ps.all, p)
	ps.mu.Unlock()
	return p
}

// starter returns a Config.Start that starts build b under a new instance id and records it.
func (ps *procs) starter(b string, started *[]*proc) func(context.Context) error {
	return func(context.Context) error {
		*started = append(*started, ps.start(b, "h"+b+holder.NewInstanceID()[:6]))
		return nil
	}
}

func (p *proc) exited(t *testing.T) error {
	t.Helper()
	select {
	case <-p.done:
		return p.err
	case <-time.After(waitLong):
		t.Fatalf("holder %s did not exit", p.instance)
		return nil
	}
}

// inproc starts an in-process holder that speaks protocol reg, as instance id of build b.
func inproc(t *testing.T, dir, b, id string, reg *hp.Registry) *holder.Holder {
	t.Helper()
	l, err := holder.Listen(holderreg.SocketPath(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	h := holder.New(holder.Config{Build: b, Instance: id, Protocol: reg, RegistryDir: dir,
		ShutdownGrace: 200 * time.Millisecond, RingSize: 65536})
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := h.Serve(l); err != nil {
			t.Errorf("serve %s: %v", id, err)
		}
	}()
	t.Cleanup(func() {
		h.Shutdown()
		<-served
	})
	waitFor(t, "registry file of "+id, func() bool { return exists(holderreg.Path(dir, id)) })
	return h
}

// events collects a client's events so tests can wait on them.
type events struct {
	mu  sync.Mutex
	all []Event
}

func collect(t *testing.T, c *Client) *events {
	ev := &events{}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case e := <-c.Events():
				ev.mu.Lock()
				ev.all = append(ev.all, e)
				ev.mu.Unlock()
			case <-stop:
				return
			}
		}
	}()
	return ev
}

// wait returns the first event that matches, waiting for one to arrive.
func (ev *events) wait(t *testing.T, what string, match func(Event) bool) Event {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		ev.mu.Lock()
		for _, e := range ev.all {
			if match(e) {
				ev.mu.Unlock()
				return e
			}
		}
		ev.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// output returns the live output of pane hd on holder h seen so far.
func (ev *events) output(h *Holder, hd hp.Handle) string {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	var b strings.Builder
	for _, e := range ev.all {
		if o, ok := e.Frame.(*hp.Output); ok && e.Holder == h && o.Handle == hd {
			b.Write(o.Data)
		}
	}
	return b.String()
}

// waitOutput waits until pane p's live output contains s.
func (ev *events) waitOutput(t *testing.T, p *Pane, s string) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for !strings.Contains(ev.output(p.Holder, p.Handle), s) {
		if time.Now().After(deadline) {
			t.Fatalf("pane %d on %s: no %q in %q", p.Handle, p.Holder.Instance(), s, ev.output(p.Holder, p.Handle))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (ev *events) waitExited(t *testing.T, p *Pane) *hp.Exited {
	t.Helper()
	return ev.wait(t, "exited", func(e Event) bool {
		x, ok := e.Frame.(*hp.Exited)
		return ok && e.Holder == p.Holder && x.Handle == p.Handle
	}).Frame.(*hp.Exited)
}

func (ev *events) waitEnd(t *testing.T, h *Holder) Event {
	t.Helper()
	return ev.wait(t, "connection end", func(e Event) bool { return e.Holder == h && e.Frame == nil })
}

func uuidN(n int) hp.UUID {
	var u hp.UUID
	u[0], u[15] = 0xfc, byte(n)
	return u
}

// echoPane is a shell that prints ready-n, answers each line with got-n:<line>, and exits on
// the line quit.
func echoSpawn(n int) hp.Spawn {
	script := fmt.Sprintf(`echo ready-%d; while read l; do [ "$l" = quit ] && exit 0; echo "got-%d:$l"; done`, n, n)
	return hp.Spawn{UUID: uuidN(n), Cols: 80, Rows: 24, Cwd: "/", Argv: []string{"/bin/sh", "-c", script}, Env: testEnv}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), waitLong)
	t.Cleanup(cancel)
	return c
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func waitGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for exists(path) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still exists", path)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitFor polls cond until it holds.
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
