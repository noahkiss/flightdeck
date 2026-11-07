package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/noahkiss/flightdeck/internal/testtmp"
)

var (
	bin       string
	holderBin string
	// race_test.go adds -race under the race detector. testseam compiles in the test seams,
	// which stay off unless a test sets their variable ($GFF_TEST_SPAWN_DELAY).
	buildFlags = []string{"-tags", "testseam"}
	// raceBuild: bin is a race build (race_test.go).
	raceBuild bool
)

// testDir is TestMain's directory: the built binaries and the fakes built once
// (buildFakeOpencode). It goes when the test binary ends.
var testDir string

func TestMain(m *testing.M) {
	d, err := testtmp.MainDir("gffhab")
	if err != nil {
		panic(err)
	}
	testDir = d
	bin = filepath.Join(d, "gff-hab")
	holderBin = filepath.Join(d, "gff-hab-holder")
	os.Setenv("GFF_HOLDER_DIR", filepath.Join(d, "holders"))     // never the user's real holders
	os.Setenv("GFF_HAB_PEERS", filepath.Join(d, "peers.toml"))   // never the user's real peer list
	os.Setenv("GFF_HAB_DETECT", "off")                           // the catalog runs no program from PATH
	os.Setenv("GFF_HAB_SPEECH_HELPER", filepath.Join(d, "none")) // never a speech helper found on PATH
	out, err := exec.Command("go", append(append([]string{"build"}, buildFlags...), "-o", bin, ".")...).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build gff-hab: %v\n%s", err, out)
		testtmp.Exit(1, d)
	}
	hb := exec.Command("go", "build", "-o", holderBin, "github.com/noahkiss/flightdeck/cmd/gff-hab-holder")
	hb.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := hb.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build gff-hab-holder: %v\n%s", err, out)
		testtmp.Exit(1, d)
	}
	testtmp.Exit(m.Run(), d)
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

// startServe runs `gff-hab serve` and waits until its socket answers a hello.
func startServe(t *testing.T, sock, state string) *served {
	t.Helper()
	return startServeWith(t, sock, state, nil)
}

// startServeWith is startServe with extra environment and arguments.
func startServeWith(t *testing.T, sock, state string, env []string, args ...string) *served {
	t.Helper()
	return startServeOut(t, sock, state, env, nil, args...)
}

// startServeOut is startServeWith with gff-hab's log written to stderr (nil: the test's).
func startServeOut(t *testing.T, sock, state string, env []string, stderr io.Writer, args ...string) *served {
	t.Helper()
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd := exec.Command(bin, append([]string{"serve", "-socket", sock, "-state", state, "-host-id", "test-host"}, args...)...)
	// The user's own hab config (project roots, the always-on mark) stays out of the test.
	cmd.Env = append(append(os.Environ(), "GFF_HAB_CONFIG="+filepath.Join(state, "no-hab.toml")), env...)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &served{cmd: cmd, sock: sock, done: make(chan error, 1)}
	go func() { s.done <- cmd.Wait() }()
	t.Cleanup(func() { s.stop(t) })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if answers(sock) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("gff-hab serve did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// answers says whether a gff-hab answers a hello on sock. A dial alone proves nothing after a
// SIGKILL: a child the killed gff-hab forked but had not yet exec'd still holds its listener, so
// a dial lands in that listener's backlog, and the connection is reset (Linux) or closed
// (macOS) once the child execs.
func answers(sock string) bool {
	c, err := control.Dial(sock)
	if err != nil {
		return false
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = c.Hello(ctx, api.ClientInfo{Name: "test", Version: "0", Kind: api.ClientKindApp}, "", "")
	return err == nil
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
	if l := linuxInetSockets(t, pid); len(l) > 0 {
		t.Fatalf("gff-hab has internet sockets: %v", l)
	}
}

// linuxInetSockets returns every entry in /proc/<pid>/net/{tcp,udp}{,6}, in any state, whose
// inode is one of pid's file descriptors. It fails the test when a table cannot be read; only
// an IPv6 table may be missing, on a system with IPv6 off.
func linuxInetSockets(t *testing.T, pid int) []string {
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
	for _, f := range []string{"tcp", "udp", "tcp6", "udp6"} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, f))
		if err != nil {
			if strings.HasSuffix(f, "6") && errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("cannot check for internet sockets: %v", err)
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) > 9 && inodes[fields[9]] {
				found = append(found, f+" "+fields[1]+" state "+fields[3])
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

// TestServeWaitsForPrivateSocketDir: a socket folder other users can open makes gff-hab log one
// line that names the folder and the chmod that fixes it, then wait without exiting (a
// supervisor would restart it every second). Once the folder is private it starts; SIGTERM
// during the wait ends it cleanly.
func TestServeWaitsForPrivateSocketDir(t *testing.T) {
	for _, fix := range []bool{true, false} {
		t.Run(fmt.Sprintf("fixed=%v", fix), func(t *testing.T) {
			d := shortDir(t)
			run := filepath.Join(d, "run")
			if err := os.Mkdir(run, 0o755); err != nil {
				t.Fatal(err)
			}
			os.Chmod(run, 0o755)
			sock := filepath.Join(run, "hab.sock")
			cmd := exec.Command(bin, "serve", "-socket", sock, "-state", filepath.Join(d, "state"), "-host-id", "test-host")
			cmd.Env = append(os.Environ(), "GFF_HAB_CONFIG="+filepath.Join(d, "no-hab.toml"))
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			lines := make(chan string, 100)
			go func() {
				sc := bufio.NewScanner(stderr)
				for sc.Scan() {
					lines <- sc.Text()
				}
				close(lines)
			}()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			defer func() {
				cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					cmd.Process.Kill()
				}
			}()
			select {
			case l := <-lines:
				if !strings.Contains(l, run) || !strings.Contains(l, "chmod 700") || !strings.Contains(l, "open to other users") {
					t.Fatalf("first line: %q", l)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("no line about the socket folder")
			}
			// Two polls later it still runs and has said nothing more.
			select {
			case l := <-lines:
				t.Fatalf("a second line while waiting: %q", l)
			case err := <-done:
				t.Fatalf("gff-hab exited while waiting: %v", err)
			case <-time.After(2*socketDirPoll + 500*time.Millisecond):
			}
			if !fix {
				cmd.Process.Signal(syscall.SIGTERM)
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("SIGTERM while waiting: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("gff-hab did not stop on SIGTERM while waiting")
				}
				return
			}
			os.Chmod(run, 0o700)
			deadline := time.Now().Add(10 * time.Second)
			for {
				if c, err := control.Dial(sock); err == nil {
					c.Close()
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("gff-hab did not start once the folder was private")
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}
