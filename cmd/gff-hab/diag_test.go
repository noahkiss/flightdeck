package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/harness"
	"github.com/noahkiss/flightdeck/internal/health"
	"github.com/noahkiss/flightdeck/internal/hook"
	"github.com/noahkiss/flightdeck/internal/logfile"
)

func readHealth(t *testing.T, state string) []health.Record {
	t.Helper()
	rs, err := health.Read(filepath.Join(state, healthFile))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func readMarker(t *testing.T, state string) health.Marker {
	t.Helper()
	m, ok, err := health.ReadMarker(filepath.Join(state, runDir, habMarkerFile))
	if err != nil || !ok {
		t.Fatalf("run marker: %v %v", ok, err)
	}
	return m
}

func habLog(t *testing.T, state string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(logsDir(state), habLogFile))
	return string(b)
}

// crashNotices counts the crash notices in the state's notice log.
func crashNotices(t *testing.T, state string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, noticesFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	var f noticeFile
	if err != nil || json.Unmarshal(b, &f) != nil {
		t.Fatalf("notices: %v", err)
	}
	n := 0
	for _, x := range f.Notices {
		if x.Title == "gff-hab restarted after a crash" {
			n++
		}
	}
	return n
}

// panicServe runs gff-hab serve with the panic seam and waits for it to crash.
func panicServe(t *testing.T, sock, state string) {
	t.Helper()
	cmd := exec.Command(bin, "serve", "-socket", sock, "-state", state, "-host-id", "test-host")
	cmd.Env = append(os.Environ(), "GFF_HAB_CONFIG="+filepath.Join(state, "no-hab.toml"), "GFF_TEST_HAB_PANIC=boom")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || !strings.Contains(string(out), "panic: test seam: boom") {
		t.Fatalf("serve with the panic seam: %v\n%s", err, out)
	}
}

// TestHealthRecords is diagnostics test 3: a gff-hab killed by SIGKILL leaves an unclean
// record, a panic a crash record with its cause and crash file, both read at the next start;
// a crash raises one notice an hour; a clean stop leaves no record and no crash file. It also
// checks the log file, the start line's version and the next-start line (diagnostics.md 4.1, 6).
func TestHealthRecords(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	sock, state := filepath.Join(dir, "hab.sock"), filepath.Join(dir, "state")

	s := startServe(t, sock, state)
	m := readMarker(t, state)
	if m.Clean || m.Pid != s.cmd.Process.Pid || m.Hook != "up" || m.Run == "" {
		t.Fatalf("marker of a running gff-hab: %+v", m)
	}
	waitUntil(t, "the start line in hab.log", func() bool {
		return strings.Contains(habLog(t, state), "epoch "+m.Run+" version dev build ")
	})
	if st, err := os.Stat(filepath.Join(logsDir(state), habLogFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("hab.log: %v", err)
	}
	s.cmd.Process.Kill()
	<-s.done
	s.stopped = true

	s = startServe(t, sock, state)
	rs := readHealth(t, state)
	if len(rs) != 1 || rs[0].Kind != health.KindUnclean || rs[0].Cause != health.CauseKilled || rs[0].Run != m.Run {
		t.Fatalf("after SIGKILL: %+v", rs)
	}
	waitUntil(t, "the next-start line", func() bool {
		return strings.Contains(habLog(t, state), "previous run ended unclean: signal or kill (epoch "+m.Run)
	})
	s.stop(t)
	if m := readMarker(t, state); !m.Clean {
		t.Fatalf("marker after a clean stop: %+v", m)
	}
	if fs, _ := os.ReadDir(filepath.Join(state, crashDir)); len(fs) != 0 {
		t.Fatalf("crash files after a clean stop: %v", fs)
	}

	panicServe(t, sock, state)
	crashed := readMarker(t, state)
	s = startServe(t, sock, state)
	rs = readHealth(t, state)
	if len(rs) != 2 || rs[1].Kind != health.KindCrash || rs[1].Cause != "panic: test seam: boom" ||
		rs[1].CrashFile != "hab-"+crashed.Run+".txt" {
		t.Fatalf("after a panic: %+v", rs)
	}
	b, err := os.ReadFile(filepath.Join(state, crashDir, rs[1].CrashFile))
	if err != nil || !strings.Contains(string(b), "goroutine") {
		t.Fatalf("crash file: %v %q", err, b)
	}
	if n := crashNotices(t, state); n != 1 {
		t.Fatalf("%d crash notices, want 1", n)
	}
	s.stop(t)

	// A second crash within the hour: a record, and no second notice.
	panicServe(t, sock, state)
	s = startServe(t, sock, state)
	if rs := readHealth(t, state); len(rs) != 3 || rs[2].Kind != health.KindCrash {
		t.Fatalf("after a second panic: %+v", rs)
	}
	if n := crashNotices(t, state); n != 1 {
		t.Fatalf("%d crash notices after two crashes in an hour, want 1", n)
	}

	// gff-hab status and logs read the same files.
	out := run(t, "status", "-state", state, "-socket", sock)
	for _, want := range []string{"gff-hab  running, pid ", "version dev", "2 crashes, 1 unclean ends", "hooks    up", "peers    0 of 0 online"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	out = run(t, "logs", "-state", state, "-grep", "previous run ended", "-proc", "hab")
	if n := strings.Count(out, "\n"); n != 3 || !strings.Contains(out, " hab ") {
		t.Errorf("logs -grep: %d lines:\n%s", n, out)
	}
}

// testDiag is a habDiag on a temporary state folder, with its lines in memory.
func testDiag(t *testing.T) (*habDiag, func() string) {
	t.Helper()
	state := t.TempDir()
	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	w := logfile.New(logfile.Config{})
	d := startDiag(state, "", control.NewEpoch(), w, logf)
	t.Cleanup(d.cleanExit)
	return d, func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(lines, "\n") }
}

const testHookToken = "0123456789abcdef"

type oneToken struct{}

func (oneToken) Pane(token string) (string, bool) { return "pane-1", token == testHookToken }

// TestHookPanicRecovered is diagnostics test 4: a hook handler that panics is recovered by the
// HTTP server, logged through its ErrorLog with a health record of kind recovered, and the
// server goes on serving (diagnostics.md 6.2).
func TestHookPanicRecovered(t *testing.T) {
	t.Parallel()
	d, lines := testDiag(t)
	var calls sync.Mutex
	n := 0
	srv := hook.NewServer(oneToken{}, func(string, harness.Envelope) string {
		calls.Lock()
		n++
		first := n == 1
		calls.Unlock()
		if first {
			panic("sink failed on pane-1")
		}
		return ""
	}, nil)
	srv.SetErrorLog(d.hookErrorLog())
	dir := shortDir(t)
	sock := filepath.Join(dir, "hook.sock")
	l, err := control.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go d.serveHooks(func() error { return srv.Serve(l) })
	defer srv.Close()
	env := harness.Envelope{Harness: "claude", Event: "Stop", Payload: json.RawMessage(`{}`)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hook.Post(ctx, hook.Addr(sock, testHookToken), env); err == nil {
		t.Fatal("the post whose handler panicked succeeded")
	}
	if err := hook.Post(ctx, hook.Addr(sock, testHookToken), env); err != nil {
		t.Fatalf("the hook server stopped serving after a recovered panic: %v", err)
	}
	waitUntil(t, "the recovered record", func() bool { return len(readHealth(t, d.state)) == 1 })
	r := readHealth(t, d.state)[0]
	if r.Kind != health.KindRecovered || r.Cause != "panic: sink failed on pane-1" || r.Run != d.marker.Run {
		t.Fatalf("record %+v", r)
	}
	if !strings.Contains(lines(), "hook server: http: panic serving") {
		t.Fatalf("no ErrorLog line:\n%s", lines())
	}
}

// TestHookServerDown: a hook server that stops with an error marks the endpoint down for
// gff-hab status and logs one line, then one more each period while it stays down (4.6).
func TestHookServerDown(t *testing.T) {
	t.Parallel()
	d, lines := testDiag(t)
	d.hookEvery = 20 * time.Millisecond
	done := make(chan struct{})
	go func() { d.serveHooks(func() error { return errors.New("accept: too many files") }); close(done) }()
	waitUntil(t, "two lines", func() bool { return strings.Count(lines(), errHookServe.Error()) >= 2 })
	if !strings.Contains(lines(), "hook endpoint stopped: accept: too many files\n") {
		t.Fatalf("lines:\n%s", lines())
	}
	if m := readMarker(t, d.state); m.Hook != "down" {
		t.Fatalf("marker %+v", m)
	}
	d.cleanExit()
	<-done
}

// TestSweep: the sweep copy-truncates hab.stderr and a holder's file at their caps, removes a
// gone holder's old files and a holder file idle past goneHolderAge, keeps the newest crash
// files, and leaves hab.log, which its writer rotates (diagnostics.md 4.3, 11).
func TestSweep(t *testing.T) {
	t.Parallel()
	d, _ := testDiag(t)
	logs := logsDir(d.state)
	big := func(name string, n int) string {
		p := filepath.Join(logs, name)
		os.WriteFile(p, []byte(strings.Repeat("x", n)), 0o600)
		return p
	}
	stderr := big(habStderrFile, stderrMax+1)
	holder := big("gff-hab-holder.aaaa0001.log", holderLogMax+1)
	orphan := big("gff-hab-holder.aaaa0002.log.1", 10)
	idle := big("gff-hab-holder.aaaa0003.log", 10)
	old := time.Now().Add(-goneHolderAge - time.Hour)
	os.Chtimes(idle, old, old)
	crash := filepath.Join(d.state, crashDir)
	for i := range crashKeep + 3 {
		p := filepath.Join(crash, fmt.Sprintf("hab-%02d.txt", i))
		os.WriteFile(p, []byte("panic: x"), 0o600)
		at := time.Now().Add(time.Duration(i-100) * time.Minute)
		os.Chtimes(p, at, at)
	}
	d.sweep()
	for p, size := range map[string]int64{stderr: 0, logfile.Old(stderr, 1): stderrMax + 1, holder: 0,
		logfile.Old(holder, 1): holderLogMax + 1} {
		if st, err := os.Stat(p); err != nil || st.Size() != size {
			t.Errorf("%s: %v", filepath.Base(p), err)
		}
	}
	for _, p := range []string{orphan, idle} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stayed", filepath.Base(p))
		}
	}
	fs, _ := os.ReadDir(crash)
	if len(fs) != crashKeep {
		t.Errorf("%d crash files, want %d", len(fs), crashKeep)
	}
	if _, err := os.Stat(filepath.Join(crash, "hab-00.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the oldest crash file stayed")
	}
}
