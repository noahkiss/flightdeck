package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/service"
)

// gff-hab uninstall stops a plain-process engine the pid file names, and leaves alone a process
// whose command line is not the engine's (a pid reused since the file was written).
func TestUninstallStopsEngine(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, capcomFolders), 0o700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(state, capcomFolders, capcomengine.PidFile)
	// start runs bin and reaps it at once, so an ended process is no zombie that still answers.
	start := func(bin string) (*exec.Cmd, chan struct{}) {
		cmd := exec.Command(bin, "60")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		t.Cleanup(func() { cmd.Process.Kill(); <-done })
		os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
		return cmd, done
	}
	ended := func(done chan struct{}) bool {
		select {
		case <-done:
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}

	// Another program at the pid: left running.
	other, otherDone := start("/bin/sleep")
	var out bytes.Buffer
	stopEngine(context.Background(), service.Launchd, "test", state, &out)
	if out.Len() != 0 || ended(otherDone) {
		t.Fatalf("an unrelated process was stopped: %q", out.String())
	}
	other.Process.Kill()
	<-otherDone
	// The first stop's watcher removes the pid file once that process ended; let it, so it never
	// removes the next one.
	waitUntil(t, "the pid file removed", func() bool { _, err := os.Stat(pidFile); return os.IsNotExist(err) })

	// The engine: stopped.
	dir := filepath.Join(t.TempDir(), "capcom-engine")
	os.MkdirAll(dir, 0o700)
	bin := filepath.Join(dir, "engine.sh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, engDone := start(bin)
	out.Reset()
	stopEngine(context.Background(), service.Launchd, "test", state, &out)
	if !strings.Contains(out.String(), "stopped the Capcom engine") || !ended(engDone) {
		t.Fatalf("the engine was not stopped: %q", out.String())
	}
}
