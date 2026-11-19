package testtmp

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The process guard. A test binary starts helper processes from its TestMain directory or from
// a Dir: a holder, a gff-hab serve, a fake harness script. Each such process names one of those
// directories in its command line, so the guard finds them with ps. A holder runs in its own
// session, by design, and a gff-hab outlives its parent; when the binary dies before its
// cleanups (a -timeout panic, a signal), nothing else would stop them.

var (
	mu    sync.Mutex
	dirs  []string // the MainDir and Dir directories of this process, removed or not
	swept sync.Map // prefix → struct{}: Dir swept it once
)

func register(d string) {
	mu.Lock()
	dirs = append(dirs, d)
	mu.Unlock()
}

func registered() []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), dirs...)
}

// Dir returns a fresh private directory with a short path in the temporary folder, for socket
// paths: macOS caps one at 104 bytes, and t.TempDir is too long there. It removes the directory
// when the test ends. Its pid file names this process, so the next Dir or MainDir with the same
// prefix removes it, and stops the processes started from it, after this binary dies without
// its cleanups. The first Dir of a prefix in a process sweeps that prefix.
func Dir(t testing.TB, prefix string) string {
	t.Helper()
	if _, done := swept.LoadOrStore(prefix, struct{}{}); !done {
		Sweep(prefix)
	}
	d, err := owned(prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// owned makes a directory under prefix in the temporary folder, writes this process's pid into
// it and registers it with the guard.
func owned(prefix string) (string, error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, pidFile), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	register(dir)
	return dir, nil
}

// Proc is a running process: its pid and its command line.
type Proc struct {
	Pid  int
	Args string
}

// ProcsUnder lists this user's processes, other than this one, whose command line names a path
// in one of dirs. It reads ps; with no ps it lists none.
func ProcsUnder(dirs []string) []Proc {
	if len(dirs) == 0 {
		return nil
	}
	var ps []Proc
	for _, p := range listProcs() {
		for _, d := range dirs {
			if names(p.Args, d) {
				ps = append(ps, p)
				break
			}
		}
	}
	return ps
}

// listProcs lists this user's processes other than this one. A zombie lists without its
// command line ("<defunct>" on Linux, "(name)" on macOS), so it names no directory.
func listProcs() []Proc {
	out, err := exec.Command("ps", "-U", strconv.Itoa(os.Getuid()), "-ww", "-o", "pid=", "-o", "args=").Output()
	if err != nil {
		return nil
	}
	var ps []Proc
	for _, line := range strings.Split(string(out), "\n") {
		pidText, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid == os.Getpid() {
			continue
		}
		ps = append(ps, Proc{pid, strings.TrimSpace(args)})
	}
	return ps
}

// running returns the processes of ps that still run with the same command line.
func running(ps []Proc) []Proc {
	now := map[int]string{}
	for _, p := range listProcs() {
		now[p.Pid] = p.Args
	}
	var left []Proc
	for _, p := range ps {
		if now[p.Pid] == p.Args {
			left = append(left, p)
		}
	}
	return left
}

// names reports whether a command line names dir or a path inside it.
func names(args, dir string) bool {
	if strings.Contains(args, dir+"/") {
		return true
	}
	for _, f := range strings.Fields(args) {
		if f == dir {
			return true
		}
	}
	return false
}

// Stop sends SIGTERM to each process, waits up to grace for them to end, then sends SIGKILL to
// the ones left. A holder ends its panes' programs on SIGTERM. A process counts as ended once
// ps no longer lists it with the same command line, so an unreaped child does not hold it up.
func Stop(ps []Proc, grace time.Duration) {
	for _, p := range ps {
		syscall.Kill(p.Pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(grace)
	for {
		left := running(ps)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, p := range left {
				syscall.Kill(p.Pid, syscall.SIGKILL)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// leaked waits up to grace for the processes started from this binary's directories to end,
// and returns the ones still running.
func leaked(grace time.Duration) []Proc {
	deadline := time.Now().Add(grace)
	for {
		ps := ProcsUnder(registered())
		if len(ps) == 0 || time.Now().After(deadline) {
			return ps
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stopAll stops every process started from this binary's directories and removes them.
func stopAll() {
	Stop(ProcsUnder(registered()), 2*time.Second)
	for _, d := range registered() {
		os.RemoveAll(d)
	}
}

// Run runs the tests with the process guard, then ends the binary as Exit does. Until m.Run
// returns, SIGINT or SIGTERM stops the binary's processes, removes its directories and exits 1;
// and shortly before -test.timeout ends the binary with a panic, the guard stops the processes
// and removes the directories, since the panic skips every cleanup.
func Run(m *testing.M, dir string) {
	if !flag.Parsed() {
		flag.Parse()
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	quit := make(chan struct{})
	go func() {
		select {
		case s := <-sigs:
			fmt.Fprintf(os.Stderr, "testtmp: %v: stopping the test's processes\n", s)
			stopAll()
			os.Exit(1)
		case <-quit:
		}
	}()
	var reaper *time.Timer
	if to := testTimeout(); to > 0 {
		margin := min(max(to/20, 5*time.Second), 30*time.Second)
		reaper = time.AfterFunc(to-margin, func() {
			fmt.Fprintf(os.Stderr, "testtmp: the run nears -test.timeout %v: stopping the test's processes\n", to)
			stopAll()
		})
	}
	code := m.Run()
	if reaper != nil {
		reaper.Stop()
	}
	signal.Stop(sigs)
	close(quit)
	Exit(code, dir)
}

// testTimeout is the binary's -test.timeout, or 0.
func testTimeout() time.Duration {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return 0
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return 0
	}
	d, _ := g.Get().(time.Duration)
	return d
}
