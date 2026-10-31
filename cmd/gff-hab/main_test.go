package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/structure"
)

var (
	bin        string
	buildFlags []string // race_test.go adds -race under the race detector
)

func TestMain(m *testing.M) {
	d, err := os.MkdirTemp("", "gffhab")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(d, "gff-hab")
	out, err := exec.Command("go", append(append([]string{"build"}, buildFlags...), "-o", bin, ".")...).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build gff-hab: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}

// shortDir returns a private directory with a short path (macOS socket paths max 104 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gffh")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(d, 0o700)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type served struct {
	cmd     *exec.Cmd
	sock    string
	done    chan error
	stopped bool // stop ran; only the test goroutine touches it
}

// startServe runs `gff-hab serve` and waits until its socket answers.
func startServe(t *testing.T, sock, state string) *served {
	t.Helper()
	cmd := exec.Command(bin, "serve", "-socket", sock, "-state", state, "-host-id", "test-host")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &served{cmd: cmd, sock: sock, done: make(chan error, 1)}
	go func() { s.done <- cmd.Wait() }()
	t.Cleanup(func() { s.stop(t) })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := control.Dial(sock); err == nil {
			c.Close()
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("gff-hab serve did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *served) stop(t *testing.T) {
	if s.stopped {
		return
	}
	s.stopped = true
	s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-s.done:
		if err != nil {
			t.Errorf("serve exited: %v", err)
		}
	case <-time.After(10 * time.Second):
		s.cmd.Process.Kill()
		t.Error("serve did not stop on SIGTERM")
	}
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		t.Fatalf("gff-hab %v: %v", args, err)
	}
	return string(out)
}

func welcome(t *testing.T, sock string) api.Welcome {
	t.Helper()
	var w api.Welcome
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "hab.hello")), &w); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestNoTCPListener: in single-machine mode gff-hab holds no TCP or UDP socket at all.
func TestNoTCPListener(t *testing.T) {
	d := shortDir(t)
	s := startServe(t, filepath.Join(d, "run", "hab.sock"), filepath.Join(d, "state"))
	pid := s.cmd.Process.Pid
	if lsof, err := exec.LookPath("lsof"); err == nil {
		out, err := exec.Command(lsof, "-nP", "-a", "-p", strconv.Itoa(pid), "-i").Output()
		// lsof exits 1 when it finds nothing to list.
		if strings.TrimSpace(string(out)) != "" {
			t.Fatalf("gff-hab has internet sockets:\n%s", out)
		}
		if err == nil {
			t.Fatal("lsof found internet sockets")
		}
		unix, err := exec.Command(lsof, "-nP", "-a", "-p", strconv.Itoa(pid), "-U").Output()
		if err != nil || !strings.Contains(string(unix), "hab.sock") {
			t.Fatalf("lsof does not see the Unix socket (%v):\n%s", err, unix)
		}
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("no lsof on this system")
	}
	if l := linuxTCPListeners(t, pid); len(l) > 0 {
		t.Fatalf("gff-hab listens on TCP: %v", l)
	}
}

// linuxTCPListeners returns the LISTEN entries in /proc/net/tcp{,6} whose inode is one of pid's
// file descriptors.
func linuxTCPListeners(t *testing.T, pid int) []string {
	t.Helper()
	inodes := map[string]bool{}
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		link, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
		if ino, ok := strings.CutPrefix(link, "socket:["); ok {
			inodes[strings.TrimSuffix(ino, "]")] = true
		}
	}
	var found []string
	for _, f := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, f))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) > 9 && fields[3] == "0A" && inodes[fields[9]] {
				found = append(found, f+" "+fields[1])
			}
		}
	}
	return found
}

// TestRestartNewEpochCLI: `gff-hab events` sees closing (shutdown) when the hab stops; the next
// hab has a new epoch and the stored structure.
func TestRestartNewEpochCLI(t *testing.T) {
	d := shortDir(t)
	sock, state := filepath.Join(d, "run", "hab.sock"), filepath.Join(d, "state")
	s1 := startServe(t, sock, state)
	w1 := welcome(t, sock)
	if w1.Host.ID != "test-host" || !strings.HasPrefix(w1.DeviceID, "SHA256:") || w1.Build == "" {
		t.Fatalf("welcome %+v", w1)
	}

	ev := exec.Command(bin, "events", "-socket", sock)
	out, err := ev.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(out)
	var methods []string
	for len(methods) < 2 && lines.Scan() {
		var m struct{ Method string }
		json.Unmarshal(lines.Bytes(), &m)
		methods = append(methods, m.Method)
	}
	s1.stop(t)
	for lines.Scan() {
		var m struct {
			Method string
			Params api.Closing
		}
		json.Unmarshal(lines.Bytes(), &m)
		methods = append(methods, m.Method)
		if m.Method == "closing" && m.Params.Reason != api.ClosingReasonShutdown {
			t.Fatalf("closing reason %q", m.Params.Reason)
		}
	}
	if err := ev.Wait(); err != nil {
		t.Fatalf("events exited: %v", err)
	}
	if strings.Join(methods, ",") != "welcome,snapshot,closing" {
		t.Fatalf("events printed %v", methods)
	}

	startServe(t, sock, state)
	w2 := welcome(t, sock)
	if w2.Epoch == w1.Epoch {
		t.Fatal("same epoch after a restart")
	}
	if w2.DeviceID != w1.DeviceID {
		t.Fatal("the device id changed across a restart")
	}
	var st api.StructureResult
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "structure.get")), &st); err != nil {
		t.Fatal(err)
	}
	if st.Host.ID != "test-host" || st.Projects == nil {
		t.Fatalf("structure %+v", st)
	}
}

func TestAPICallErrors(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "run", "hab.sock")
	startServe(t, sock, filepath.Join(d, "state"))
	cmd := exec.Command(bin, "api", "call", "-socket", sock, "devices.list")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("exit %v", err)
	}
	if !strings.Contains(stderr.String(), "unavailable") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

// pipeConn is the client side of a child process's stdin and stdout: the stand-in for ssh.
type pipeConn struct {
	io.Reader
	io.WriteCloser
}

// TestConnectCarriesFullSession runs a whole session through `gff-hab connect` over a pipe
// pair, as `ssh <host> gff-hab connect` would carry it: hello, subscribe, live events, a
// request with the seq rule, and a clean end when the client closes its side.
func TestConnectCarriesFullSession(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "run", "hab.sock")
	holder := structure.NewFakeHolder()
	var pub atomic.Pointer[control.Server]
	store, err := structure.Open(structure.Config{Dir: filepath.Join(d, "state"), Holders: holder,
		OnEvent: func(e structure.Event) {
			if s := pub.Load(); s != nil {
				s.Publish(e)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := control.New(control.Config{Build: "test", Host: api.HostInfo{ID: "test-host"}, DeviceID: "SHA256:x", Source: store})
	pub.Store(srv)
	l, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Close()

	cmd := exec.Command(bin, "connect", "-socket", sock)
	cmd.Stderr = os.Stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := control.NewClient(pipeConn{Reader: out, WriteCloser: in})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cl := api.Client{Caller: c}

	w, err := c.Hello(ctx, api.ClientInfo{Name: "t", Version: "0", Kind: api.ClientKindApp}, "phone", "")
	if err != nil {
		t.Fatal(err)
	}
	if w.Epoch != srv.Epoch() || w.DeviceID != "SHA256:x" {
		t.Fatalf("welcome %+v", w)
	}
	snap, err := cl.EventsSubscribe(ctx, api.SubscribeParams{Scope: api.Scope{Kind: api.ScopeKindHost}})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Seq.Seq != 0 || len(snap.Projects) != 0 {
		t.Fatalf("snapshot %+v", snap)
	}
	p, err := store.CreateProject("app", "example.com/o/app", "")
	if err != nil {
		t.Fatal(err)
	}
	holder.Add(structure.HolderPane{ID: "pane-1", HolderID: "h", HolderBuild: "b", Cols: 80, Rows: 24})
	if _, err := store.CreateSession(structure.SessionSpec{Name: "s", Project: p.ID, Pane: "pane-1", Index: -1}); err != nil {
		t.Fatal(err)
	}
	got, err := cl.StructureGet(ctx, api.StructureGetParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Projects) != 1 || len(got.Sessions) != 1 || len(got.Panes) != 1 || got.Seq.Seq == 0 {
		t.Fatalf("structure.get %+v", got)
	}
	var seqs []uint64
	for uint64(len(seqs)) < got.Seq.Seq {
		select {
		case n := <-c.Notifications():
			if n.Method != "event" {
				continue
			}
			e, err := n.Event()
			if err != nil {
				t.Fatal(err)
			}
			seqs = append(seqs, e.Seq)
			if e.Seq != uint64(len(seqs)) {
				t.Fatalf("seqs %v", seqs)
			}
		case <-ctx.Done():
			t.Fatalf("events through connect: %v", seqs)
		}
	}
	if err := c.Call(ctx, "devices.list", nil, nil); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("devices.list: %v", err)
	}

	// The client closes its side; connect ends cleanly.
	in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("connect exited: %v", err)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("connect did not end after the client closed")
	}
}

// TestVia runs `api call -via` through `gff-hab connect`, the shape of a call over ssh. It runs
// several times: under -race the binary is a race build, and a race in it fails the call.
func TestVia(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "run", "hab.sock")
	startServe(t, sock, filepath.Join(d, "state"))
	for range 5 {
		var w api.Welcome
		out := run(t, "api", "call", "-via", bin+" connect -socket "+sock, "hab.hello")
		if err := json.Unmarshal([]byte(out), &w); err != nil || w.Host.ID != "test-host" {
			t.Fatalf("welcome via connect: %v %s", err, out)
		}
	}
	// A bridge that fails at once: the client sees EOF and closes while the caller's own Close
	// runs. The race build reports any race on stderr, and the exit code alone hides it.
	for range 5 {
		cmd := exec.Command(bin, "api", "call", "-via", bin+" connect -socket "+filepath.Join(d, "none.sock"), "hab.hello")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("a call through a failed bridge succeeded")
		}
		if strings.Contains(stderr.String(), "DATA RACE") {
			t.Fatalf("race in gff-hab:\n%s", stderr.String())
		}
	}
}

// TestViaSSH runs the call through the system sshd to this machine. It skips unless key-based
// ssh to localhost already works; it changes no ssh configuration.
func TestViaSSH(t *testing.T) {
	if err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "localhost", "true").Run(); err != nil {
		t.Skipf("ssh to localhost is not set up: %v", err)
	}
	d := shortDir(t)
	sock := filepath.Join(d, "run", "hab.sock")
	startServe(t, sock, filepath.Join(d, "state"))
	var w api.Welcome
	out := run(t, "api", "call", "-via", "ssh -o BatchMode=yes localhost "+bin+" connect -socket "+sock, "hab.hello")
	if err := json.Unmarshal([]byte(out), &w); err != nil || w.Host.ID != "test-host" {
		t.Fatalf("welcome via ssh: %v %s", err, out)
	}
}

// TestEventsFailsWithoutClosing: a hab that dies without closing makes `gff-hab events` fail.
func TestEventsFailsWithoutClosing(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "run", "hab.sock")
	s := startServe(t, sock, filepath.Join(d, "state"))
	ev := exec.Command(bin, "events", "-socket", sock)
	out, err := ev.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	ev.Stderr = &stderr
	if err := ev.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(out)
	for i := 0; i < 2 && lines.Scan(); i++ {
	}
	s.stopped = true
	s.cmd.Process.Kill()
	<-s.done
	for lines.Scan() {
	}
	err = ev.Wait()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("events exit %v, stderr %q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no closing message") {
		t.Fatalf("stderr %q", stderr.String())
	}
}
