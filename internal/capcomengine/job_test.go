package capcomengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ProcessJob: the process runs in its own folder with the given environment and log; a later
// gff-hab adopts it by its pid file and sees it end.
func TestProcessJob(t *testing.T) {
	dir := t.TempDir()
	j := ProcessJob{PidFile: filepath.Join(dir, PidFile)}
	log := filepath.Join(dir, LogFile)
	p, err := j.Start(context.Background(), JobSpec{Argv: []string{"/bin/sh", "-c", `echo "dir $(pwd) var $FD_TEST"; echo last line >&2; exec sleep 60`},
		Dir: dir, Env: []string{"FD_TEST=yes", "PATH=/usr/bin:/bin"}, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(time.Second)
	waitFor(t, "the log", func() bool { b, _ := os.ReadFile(log); return strings.HasSuffix(string(b), "last line\n") })
	b, _ := os.ReadFile(log)
	real, _ := filepath.EvalSymlinks(dir)
	if want := "dir " + real + " var yes\n"; string(b[:len(want)]) != want {
		t.Fatalf("log %q", b)
	}
	a, ok := j.Adopt(context.Background())
	if !ok || a.Pid() != p.Pid() {
		t.Fatalf("adopt: %v %v", ok, a)
	}
	if err := p.Stop(time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the adopted process did not end")
	}
	if _, ok := j.Adopt(context.Background()); ok {
		t.Fatal("adopted a process that ended")
	}
}

// A process the job started reports how it ended, for a crash's health record (diagnostics.md
// 6.3): its exit code, or the signal that ended it. An adopted one reports nothing known.
func TestProcessExitStatus(t *testing.T) {
	dir := t.TempDir()
	j := ProcessJob{PidFile: filepath.Join(dir, PidFile)}
	for _, c := range []struct{ script, want string }{{"exit 3", "exit 3"}, {"kill -9 $$", "signal 9"}} {
		p, err := j.Start(context.Background(), JobSpec{Argv: []string{"/bin/sh", "-c", c.script}, Dir: dir, Log: filepath.Join(dir, LogFile)})
		if err != nil {
			t.Fatal(err)
		}
		<-p.Done()
		if got := exitStatus(p); got != c.want {
			t.Errorf("%s: %q, want %q", c.script, got, c.want)
		}
	}
	if got := exitStatus(&unitProc{}); got != "unknown" {
		t.Errorf("unit: %q", got)
	}
}

// LastError names the engine's last error line of this run by its message and error type, and
// reads nothing else of it: not its other values, not a text line, not an older run's line.
func TestLastError(t *testing.T) {
	log := filepath.Join(t.TempDir(), "capcom.log")
	start := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	os.WriteFile(log, []byte(`{"t":"2026-11-27T09:00:00.000Z","lvl":"error","msg":"engine did not start","proc":"capcom","comp":"engine","err_type":"Error"}
{"t":"2026-11-27T10:00:05.000Z","lvl":"error","msg":"uncaught exception","proc":"capcom","comp":"crash","err_type":"TypeError","frames":"frame-names"}
{"t":"2026-11-27T10:00:06.000Z","lvl":"warn","msg":"turn failed","proc":"capcom","comp":"codex","class":"other"}
a text line with a prompt fragment
`), 0o600)
	if got := LastError(log, start); got != "uncaught exception (TypeError)" {
		t.Errorf("LastError = %q", got)
	}
	if got := LastError(log, start.Add(time.Hour)); got != "" {
		t.Errorf("an older run's line: %q", got)
	}
	if got := LastError("", start); got != "" {
		t.Errorf("no log: %q", got)
	}
}
