package main

import (
	"bytes"
	"encoding/json"
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

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
)

// syncBuf is a buffer a process writes while the test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// heavyRun is one `gff-hab heavy` process.
type heavyRun struct {
	cmd    *exec.Cmd
	stdout *syncBuf
	stderr *syncBuf
	done   chan struct{}
	code   int
}

// startHeavyRun runs `gff-hab heavy -socket sock -- sh -c <script>` with name and work in its
// environment. The script should end once the file go-<name> exists in work.
func startHeavyRun(t *testing.T, sock, work, name, script string) *heavyRun {
	t.Helper()
	cmd := exec.Command(bin, "heavy", "-socket", sock, "--", "/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "W="+work, "N="+name, "GFF_HAB=", noRaceExitSleep)
	r := &heavyRun{cmd: cmd, stdout: &syncBuf{}, stderr: &syncBuf{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = r.stdout, r.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		cmd.Wait()
		r.code = cmd.ProcessState.ExitCode()
		close(r.done)
	}()
	t.Cleanup(func() {
		os.WriteFile(filepath.Join(work, "go-"+name), nil, 0o600)
		select {
		case <-r.done:
		case <-time.After(scaleTimeout(10 * time.Second)):
			cmd.Process.Kill()
			<-r.done
		}
	})
	return r
}

func (r *heavyRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-r.done:
		return r.code
	case <-time.After(scaleTimeout(15 * time.Second)):
		t.Fatalf("gff-hab heavy did not end; stderr %q", r.stderr.String())
	}
	return 0
}

// heavyScript logs its start and end to $W/log, waits for $W/go-$N, prints to stdout and exits
// with the status given.
func heavyScript(status int) string {
	return `echo "start-$N" >> "$W/log"; while [ ! -e "$W/go-$N" ]; do sleep 0.02; done; echo "end-$N" >> "$W/log"; echo "out-$N"; exit ` + strconv.Itoa(status)
}

func heavyStatus(t *testing.T, sock string) api.HeavyStatus {
	t.Helper()
	var s api.HeavyStatus
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "heavy.status")), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// heavyHab starts a gff-hab whose config file holds the [heavy] lines given, with the free space
// of every volume read from free (in GiB), and returns its socket, state and work folders.
func heavyHab(t *testing.T, heavyLines string, env []string, args ...string) (sock, state, work, free string, s *served) {
	t.Helper()
	d := shortDir(t)
	state, work = filepath.Join(d, "state"), filepath.Join(d, "w")
	for _, dir := range []string{state, work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	free = filepath.Join(d, "free")
	os.WriteFile(free, []byte("400"), 0o600)
	cfg := filepath.Join(d, "hab.toml")
	os.WriteFile(cfg, []byte("[heavy]\n"+heavyLines+"\n"), 0o600)
	sock = filepath.Join(d, "run", "hab.sock")
	env = append([]string{"GFF_TEST_HEAVY_FREE=" + free, "GFF_TEST_HEAVY_DISK_EVERY=100ms", "GFF_HAB_WORKTREES=" + filepath.Join(d, "wt")}, env...)
	s = startServeWith(t, sock, state, env, append([]string{"-config", cfg}, args...)...)
	return sock, state, work, free, s
}

func readLog(work string) string {
	b, _ := os.ReadFile(filepath.Join(work, "log"))
	return strings.TrimSpace(string(b))
}

// TestHeavyArrivalOrder: three runs on one host take the lock in arrival order; each waiter
// prints one line saying how many are ahead; the command's output and exit code pass through;
// heavy.status lists the holder and the waiters in order.
func TestHeavyArrivalOrder(t *testing.T) {
	sock, _, work, _, _ := heavyHab(t, `restart_hold = "0s"`, nil)
	if w := welcome(t, sock); !slices.Contains(w.Capabilities, "heavy") {
		t.Fatalf("capabilities %v lack heavy", w.Capabilities)
	}
	a := startHeavyRun(t, sock, work, "a", heavyScript(7))
	waitUntil(t, "a holds", func() bool { return readLog(work) == "start-a" })
	b := startHeavyRun(t, sock, work, "b", heavyScript(0))
	waitUntil(t, "b waits", func() bool { return len(heavyStatus(t, sock).Waiters) == 1 })
	c := startHeavyRun(t, sock, work, "c", heavyScript(0))
	waitUntil(t, "c waits", func() bool { return len(heavyStatus(t, sock).Waiters) == 2 })
	st := heavyStatus(t, sock)
	if len(st.Holders) != 1 || !strings.Contains(st.Holders[0].Command, "start-$N") || st.Slots != 1 {
		t.Fatalf("status %+v", st)
	}
	waitUntil(t, "the waiting lines", func() bool {
		return strings.Contains(b.stderr.String(), "waiting for the heavy-run lock: 1 ahead") &&
			strings.Contains(c.stderr.String(), "waiting for the heavy-run lock: 2 ahead")
	})
	for _, n := range []string{"a", "b", "c"} {
		os.WriteFile(filepath.Join(work, "go-"+n), nil, 0o600)
	}
	if code := a.wait(t); code != 7 {
		t.Fatalf("a exited %d, want the command's 7", code)
	}
	b.wait(t)
	c.wait(t)
	if got := a.stdout.String(); got != "out-a\n" {
		t.Fatalf("a's stdout %q", got)
	}
	if got, want := readLog(work), "start-a\nend-a\nstart-b\nend-b\nstart-c\nend-c"; got != want {
		t.Fatalf("log\n%s\nwant\n%s", got, want)
	}
	for _, r := range []*heavyRun{b, c} {
		if n := strings.Count(r.stderr.String(), "\n"); n != 1 {
			t.Fatalf("a waiter printed %d lines: %q", n, r.stderr.String())
		}
	}
	if a.stderr.String() != "" {
		t.Fatalf("the holder printed %q", a.stderr.String())
	}
	if st := heavyStatus(t, sock); len(st.Holders)+len(st.Waiters) != 0 {
		t.Fatalf("after every run: %+v", st)
	}
}

// TestHeavyKilledCLI: a holder's CLI killed with SIGKILL drops its connection, and the lock goes
// to the next run. The command it ran is not the hab's concern.
func TestHeavyKilledCLI(t *testing.T) {
	sock, _, work, _, _ := heavyHab(t, `restart_hold = "0s"`, nil)
	a := startHeavyRun(t, sock, work, "a", heavyScript(0))
	waitUntil(t, "a holds", func() bool { return readLog(work) == "start-a" })
	startHeavyRun(t, sock, work, "b", heavyScript(0))
	waitUntil(t, "b waits", func() bool { return len(heavyStatus(t, sock).Waiters) == 1 })
	a.cmd.Process.Signal(syscall.SIGKILL)
	waitUntil(t, "b runs", func() bool { return readLog(work) == "start-a\nstart-b" })
	if st := heavyStatus(t, sock); len(st.Holders) != 1 || len(st.Waiters) != 0 {
		t.Fatalf("after the kill: %+v", st)
	}
}

// TestHeavyRestartKeepsPlace: gff-hab restarts while one run holds and two wait. Each CLI comes
// back with its ticket: the holder holds again and its command runs on, the waiters keep their
// order ahead of a run that asked after the restart, and nothing is granted during the
// restart hold. When the holder ends, the first waiter runs.
func TestHeavyRestartKeepsPlace(t *testing.T) {
	sock, state, work, _, s := heavyHab(t, `restart_hold = "5s"`, nil)
	a := startHeavyRun(t, sock, work, "a", heavyScript(0))
	waitUntil(t, "a holds", func() bool { return readLog(work) == "start-a" })
	startHeavyRun(t, sock, work, "b", heavyScript(0))
	waitUntil(t, "b waits", func() bool { return len(heavyStatus(t, sock).Waiters) == 1 })
	startHeavyRun(t, sock, work, "c", heavyScript(0))
	waitUntil(t, "c waits", func() bool { return len(heavyStatus(t, sock).Waiters) == 2 })
	before := heavyStatus(t, sock)

	s.stop(t)
	cfg := s.cmd.Args[slices.Index(s.cmd.Args, "-config")+1]
	env := s.cmd.Env
	ns := exec.Command(bin, "serve", "-socket", sock, "-state", state, "-host-id", "test-host", "-config", cfg)
	ns.Env, ns.Stderr = env, os.Stderr
	if err := ns.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ns.Process.Signal(syscall.SIGTERM)
		ns.Wait()
	})
	waitUntil(t, "the new gff-hab", func() bool {
		c, err := control.Dial(sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	startHeavyRun(t, sock, work, "d", heavyScript(0))
	var st api.HeavyStatus
	waitUntil(t, "every run back", func() bool {
		st = heavyStatus(t, sock)
		return len(st.Holders) == 1 && len(st.Waiters) == 3
	})
	if st.Holders[0].Ticket != before.Holders[0].Ticket || st.Holders[0].Granted != before.Holders[0].Granted {
		t.Fatalf("the holder came back as %+v, was %+v", st.Holders[0], before.Holders[0])
	}
	if st.Waiters[0].Ticket != before.Waiters[0].Ticket || st.Waiters[1].Ticket != before.Waiters[1].Ticket {
		t.Fatalf("waiters %+v, were %+v", st.Waiters, before.Waiters)
	}
	if st.HoldUntil == "" {
		t.Fatal("every run came back after the restart hold ended; the test proves nothing about the hold")
	}
	select {
	case <-a.done:
		t.Fatal("the holder's CLI ended with the restart")
	default:
	}
	os.WriteFile(filepath.Join(work, "go-a"), nil, 0o600)
	waitUntil(t, "b runs after a", func() bool { return readLog(work) == "start-a\nend-a\nstart-b" })
}

// TestHeavyPaneClose: `gff-hab heavy` in a pane finds the hab through the pane's GFF_HAB, and
// heavy.status names its session; closing the pane releases the lock to the next run.
func TestHeavyPaneClose(t *testing.T) {
	d := shortDir(t)
	hdir := filepath.Join(d, "h")
	os.MkdirAll(hdir, 0o700)
	startHolder(t, hdir, "one")
	sock, _, work, _, _ := heavyHab(t, `restart_hold = "0s"`, nil, "-holders", hdir)
	script := `"$HAB" heavy -- /bin/sh -c 'touch "$W/held"; exec sleep 60'`
	created := createSession(t, sock, api.SessionCreateParams{Name: "builder", Argv: []string{"/bin/sh", "-c", script},
		Env: []string{"PATH=/usr/bin:/bin", "HAB=" + bin, "W=" + work, noRaceExitSleep}, Cwd: work})
	waitUntil(t, "the pane's run holds", func() bool {
		_, err := os.Stat(filepath.Join(work, "held"))
		return err == nil
	})
	st := heavyStatus(t, sock)
	if len(st.Holders) != 1 || st.Holders[0].Session != "builder" || st.Holders[0].Pane != created.Pane {
		t.Fatalf("status %+v, pane %s", st, created.Pane)
	}
	startHeavyRun(t, sock, work, "b", heavyScript(0))
	waitUntil(t, "b waits", func() bool { return len(heavyStatus(t, sock).Waiters) == 1 })
	run(t, "api", "call", "-socket", sock, "pane.close", `{"pane":"`+created.Pane+`"}`)
	waitUntil(t, "b runs", func() bool { return readLog(work) == "start-b" })
}

// TestHeavyLowDisk: free space below heavy.disk_min_free makes a run wait with the reason and
// sends the coordinator exactly one message; the catalog shows the low state and both volumes;
// free space back above the threshold and the margin grants the run.
func TestHeavyLowDisk(t *testing.T) {
	d := shortDir(t)
	hdir := filepath.Join(d, "h")
	os.MkdirAll(hdir, 0o700)
	startHolder(t, hdir, "one")
	sock, _, work, free, _ := heavyHab(t, `restart_hold = "0s"`, []string{"GFF_TEST_HEAVY_COORDINATOR=lead"}, "-holders", hdir)
	createSession(t, sock, api.SessionCreateParams{Name: "lead", Argv: []string{"/bin/sh", "-c", "exec sleep 600"},
		Env: []string{"PATH=/usr/bin:/bin"}, Cwd: work})
	os.WriteFile(free, []byte("5"), 0o600)
	waitUntil(t, "disk low", func() bool { return heavyStatus(t, sock).DiskLow })
	r := startHeavyRun(t, sock, work, "a", heavyScript(0))
	waitUntil(t, "the waiting line", func() bool { return strings.Contains(r.stderr.String(), "disk low") })
	if !strings.Contains(r.stderr.String(), "0 ahead") {
		t.Fatalf("waiting line %q", r.stderr.String())
	}

	// The catalog publishes after its debounce.
	var own *api.CatalogHeavy
	waitUntil(t, "the catalog's heavy section", func() bool {
		var cat api.CatalogList
		if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "catalog.list")), &cat); err != nil {
			t.Fatal(err)
		}
		for _, e := range cat.Hosts {
			if e.Own {
				own = e.Catalog.Heavy
			}
		}
		return own != nil && own.Waiting == 1
	})
	if !own.DiskLow || len(own.Volumes) != 2 || own.Volumes[0].FreeBytes != 5<<30 {
		t.Fatalf("catalog heavy section %+v", own)
	}

	var got api.ChannelWaitResult
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "channel.wait", `{"session":"lead","timeout_ms":10000}`)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Message == nil || !strings.Contains(got.Message.Text, "Disk low on test-host") {
		t.Fatalf("the coordinator got %+v", got)
	}
	time.Sleep(500 * time.Millisecond) // several disk readings, all low
	if err := json.Unmarshal([]byte(run(t, "api", "call", "-socket", sock, "channel.wait", `{"session":"lead","timeout_ms":1000}`)), &got); err != nil {
		t.Fatal(err)
	}
	if !got.TimedOut {
		t.Fatalf("a second message: %+v", got)
	}
	os.WriteFile(free, []byte("11"), 0o600) // above the threshold, inside the margin
	time.Sleep(500 * time.Millisecond)
	if readLog(work) != "" {
		t.Fatal("granted inside the margin")
	}
	os.WriteFile(free, []byte("12"), 0o600)
	waitUntil(t, "the run", func() bool { return readLog(work) == "start-a" })
}

// TestHeavyNoHab: with no gff-hab listening the command runs without the lock, says so once,
// and its exit code passes through.
func TestHeavyNoHab(t *testing.T) {
	d := shortDir(t)
	cmd := exec.Command(bin, "heavy", "-socket", filepath.Join(d, "none.sock"), "--", "/bin/sh", "-c", "echo hi; exit 3")
	cmd.Env = append(os.Environ(), "GFF_HAB=", noRaceExitSleep)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	cmd.Run()
	if code := cmd.ProcessState.ExitCode(); code != 3 || out.String() != "hi\n" || !strings.Contains(errs.String(), "running without the heavy-run lock") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
	cmd = exec.Command(bin, "heavy", "-socket", filepath.Join(d, "none.sock"))
	if err := cmd.Run(); cmd.ProcessState.ExitCode() != 64 {
		t.Fatalf("no command: %v", err)
	}
}

// TestHeavySignalWhileHabDown: a waiting CLI whose gff-hab is down still ends on SIGINT, at once,
// with 128 plus the signal, and never runs its command.
func TestHeavySignalWhileHabDown(t *testing.T) {
	sock, _, work, _, s := heavyHab(t, `restart_hold = "0s"`, nil)
	startHeavyRun(t, sock, work, "a", heavyScript(0))
	waitUntil(t, "a holds", func() bool { return readLog(work) == "start-a" })
	b := startHeavyRun(t, sock, work, "b", heavyScript(0))
	waitUntil(t, "b waits", func() bool { return len(heavyStatus(t, sock).Waiters) == 1 })
	s.stop(t)
	time.Sleep(300 * time.Millisecond) // b is dialing again
	b.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-b.done:
	case <-time.After(scaleTimeout(5 * time.Second)):
		t.Fatal("SIGINT did not end a CLI waiting for gff-hab")
	}
	if b.code != 130 || strings.Contains(readLog(work), "start-b") {
		t.Fatalf("exit %d, log %q", b.code, readLog(work))
	}
}
