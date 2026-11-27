package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// subreaperEnv runs TestSubreaperHelper as the process under test.
const subreaperEnv = "GFF_TEST_SUBREAPER"

// TestSubreaper: as a subreaper, serve adopts what a program it started leaves running, also
// with a new session and a cleared environment, and reaps it when it ends; os/exec's own
// waits keep working beside the reaper (hab-logic-api.md 4.1). The check runs in a copy of the
// test binary, so this process stays no subreaper.
func TestSubreaper(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSubreaperHelper$", "-test.v")
	cmd.Env = append(os.Environ(), subreaperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "subreaper ok") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestSubreaperHelper(t *testing.T) {
	if os.Getenv(subreaperEnv) == "" {
		t.Skip("runs from TestSubreaper")
	}
	startSubreaper(func(f string, a ...any) { t.Fatalf(f, a...) })
	// A program that leaves a child in a new session with no environment, then exits.
	cmd := exec.Command("/bin/sh", "-c", "env -i setsid sleep 30 </dev/null >/dev/null 2>&1 & echo $!")
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.Atoi(strings.TrimSpace(string(b))); err != nil {
		t.Fatalf("%q", b)
	}
	// setsid forks when it leads a group, so look for the sleep among this process's children.
	var sleeper int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sleeper == 0 {
		for _, c := range children() {
			if comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", c)); strings.TrimSpace(string(comm)) == "sleep" {
				sleeper = c
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sleeper == 0 {
		t.Fatalf("the detached sleep did not move to this process; children %v", children())
	}
	// os/exec's waits beside the reaper: none fails.
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := exec.Command("/bin/sh", "-c", "exit 0").Run(); err != nil {
				errs <- err
			}
		}()
	}
	syscall.Kill(sleeper, syscall.SIGKILL)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("os/exec beside the reaper: %v", err)
	}
	deadline = time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", sleeper)); os.IsNotExist(err) {
			fmt.Println("subreaper ok")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the adopted sleep %d was not reaped", sleeper)
}
