package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/logfile"
)

// appendScript prints a 110-byte line every 20 ms to stdout.
const appendScript = `pad=$(printf '%0100d' 0); i=0; while :; do i=$((i+1)); echo "line $i $pad"; sleep 0.02; done`

// TestCopyTruncateUnderInit runs a job under the real init system (launchd on macOS, a systemd
// user unit on Linux) whose output goes to a file, the way the hab's jobs do, then
// copy-truncates that file as gff-hab's sweep does. The job's later lines must land at the
// start of the truncated file: the init system opened it for append (diagnostics.md 4.3,
// test 2). It loads a job, so it runs only with GFF_INIT_APPEND_TEST=1, which CI sets.
func TestCopyTruncateUnderInit(t *testing.T) {
	if os.Getenv("GFF_INIT_APPEND_TEST") != "1" {
		t.Skip("loads a job into the user's init system; set GFF_INIT_APPEND_TEST=1")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "job.log")
	label := fmt.Sprintf("gfftest-append-%d", os.Getpid())
	switch runtime.GOOS {
	case "darwin":
		file := filepath.Join(dir, label+".plist")
		var p plist
		p.begin()
		p.kv("Label", label)
		p.args("ProgramArguments", []string{"/bin/sh", "-c", appendScript})
		p.kb("RunAtLoad", true)
		p.kv("StandardOutPath", out)
		p.kv("StandardErrorPath", out)
		if err := os.WriteFile(file, p.end(), 0o600); err != nil {
			t.Fatal(err)
		}
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		run(t, "launchctl", "bootstrap", domain, file)
		t.Cleanup(func() { exec.Command("launchctl", "bootout", domain+"/"+label).Run() })
	case "linux":
		// As the units do (withOutput), with the log folder missing at the start: the job must
		// make it.
		out = filepath.Join(dir, "logs", "job.log")
		startUnit(t, label+".service", out)
	default:
		t.Skip("no init system test on " + runtime.GOOS)
	}
	size := func() int64 {
		st, err := os.Stat(out)
		if err != nil {
			return 0
		}
		return st.Size()
	}
	waitUntil(t, "the job's file to pass 4 KiB", func() bool { return size() > 4096 })
	did, err := logfile.CopyTruncate(out, 2048, 1)
	if err != nil || !did {
		t.Fatalf("CopyTruncate: %v %v", did, err)
	}
	old, err := os.ReadFile(logfile.Old(out, 1))
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "lines after the truncate", func() bool { return size() > 500 })
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.IndexByte(b, 0) >= 0 || len(b) >= len(old) {
		t.Fatalf("after the truncate the file is %d bytes (%d before) and starts %q: the init system did not open it for append",
			len(b), len(old), b[:min(len(b), 40)])
	}
	if !strings.HasPrefix(string(b), "line ") {
		t.Fatalf("the truncated file starts %q", b[:min(len(b), 40)])
	}
	t.Logf("%s: %d bytes before the truncate, %d after; no gap", runtime.GOOS, len(old), len(b))
	if runtime.GOOS == "linux" {
		// The user removes the log folder; the next start of the job makes it again.
		exec.Command("systemctl", "--user", "stop", label+".service").Run()
		if err := os.RemoveAll(filepath.Dir(out)); err != nil {
			t.Fatal(err)
		}
		startUnit(t, label+"-2.service", out)
		waitUntil(t, "lines after the log folder was removed", func() bool { return size() > 0 })
		t.Logf("linux: the job made its log folder again after it was removed")
	}
}

// startUnit writes a user unit file the way install renders ExecStart (execLine over
// withOutput, with '$' and '%' escaped), runs appendScript from it, and when the test ends stops
// it and removes the file. A transient unit from systemd-run would not do: it expands '$' in
// its arguments, which a unit file escapes.
func startUnit(t *testing.T, unit, out string) {
	t.Helper()
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		cfg = filepath.Join(home, ".config")
	}
	dir := filepath.Join(cfg, "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, unit)
	argv := withOutput(out, []string{"/bin/sh", "-c", appendScript})
	body := "[Service]\nType=simple\nExecStart=" + execLine(argv[0], argv[1:]...) + "\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		exec.Command("systemctl", "--user", "stop", unit).Run()
		os.Remove(file)
		exec.Command("systemctl", "--user", "daemon-reload").Run()
	})
	run(t, "systemctl", "--user", "daemon-reload")
	run(t, "systemctl", "--user", "start", unit)
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
