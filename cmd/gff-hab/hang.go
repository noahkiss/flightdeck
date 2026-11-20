package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/noahkiss/flightdeck/internal/health"
	"github.com/noahkiss/flightdeck/internal/stall"
)

// Hang diagnosis (diagnostics.md 4.7): a stack dump on SIGUSR1 and the stall watchdog. A dump
// holds every goroutine's stack, which can show argument values, so it lives in crash/ only:
// a log line names its file, never its content.

// stacksInfix is in the name of each stack dump in crash/: hab-<epoch>-stacks-<time>.txt.
const stacksInfix = "-stacks-"

// stacksTime is the dump's time in its name: UTC, to the millisecond, so two dumps in one second
// get two files.
const stacksTime = "20060102T150405.000Z"

// dumpEvery bounds the watchdog's dumps: one an hour across all loops. One dump shows every
// goroutine, so a stall of two loops at once needs one.
const dumpEvery = time.Hour

// writeStacks writes every goroutine's stack to a new file in crash/ and returns its name. It
// then keeps the newest stacksKeep dumps; crash traces have their own keep (4.3).
func (d *habDiag) writeStacks() (string, error) {
	dir := filepath.Join(d.state, crashDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := "hab-" + d.marker.Run + stacksInfix + d.now().UTC().Format(stacksTime) + ".txt"
	// Written under a hidden name and renamed, so gff-hab stacks never prints a half file.
	f, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return "", err
	}
	err = pprof.Lookup("goroutine").WriteTo(f, 2) // the traceback form a panic prints
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	d.pruneCrash()
	return name, nil
}

// stacksOnSignal writes a dump for each signal on sigs until the diag stops. The signal handler
// runs on its own goroutine, so it works while a loop is stuck (Q3).
func (d *habDiag) stacksOnSignal(sigs <-chan os.Signal) {
	defer d.wg.Done()
	for {
		select {
		case <-sigs:
			if name, err := d.writeStacks(); err != nil {
				d.logf("stacks: %v", err)
			} else {
				d.logf("stacks: written to %s/%s on SIGUSR1", crashDir, name)
			}
		case <-d.stop:
			return
		}
	}
}

// startStacks marks the run as one that catches SIGUSR1, so gff-hab stacks may send it, and
// writes a dump for each. sigs must already be subscribed to SIGUSR1.
func (d *habDiag) startStacks(sigs <-chan os.Signal) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.marker.Stacks = true
	d.writeMarker()
	d.wg.Add(1)
	d.mu.Unlock()
	go d.stacksOnSignal(sigs)
}

// stalled is the watchdog's report (4.7): one line with the loop and its wait, and a stack dump
// unless one was written for a stall in the last hour.
func (d *habDiag) stalled(name string, wait time.Duration) {
	d.mu.Lock()
	due := d.lastDump.IsZero() || d.now().Sub(d.lastDump) >= dumpEvery
	if due {
		d.lastDump = d.now()
	}
	d.mu.Unlock()
	where := "no stack dump: one was written for a stall in the last hour"
	if due {
		if name, err := d.writeStacks(); err != nil {
			where = fmt.Sprintf("stack dump: %v", err)
		} else {
			where = "stacks in " + crashDir + "/" + name
		}
	}
	d.logf("loop stalled: %s has not answered for %s; %s", name, wait.Round(time.Second), where)
}

func (d *habDiag) recovered(name string, wait time.Duration) {
	d.logf("loop recovered: %s answered after %s", name, wait.Round(time.Second))
}

// startWatchdog watches the loops of probes (4.7). every and after are zero for the defaults;
// a testseam build shortens them.
func (d *habDiag) startWatchdog(probes []stall.Probe) *stall.Watchdog {
	every, after := stallTiming()
	w := stall.New(stall.Config{Probes: probes, Every: every, After: after, Stalled: d.stalled, Recovered: d.recovered})
	w.Start()
	return w
}

// stacksCmd is gff-hab stacks (4.7): it sends SIGUSR1 to the gff-hab its run marker names and
// prints the path of the dump that gff-hab writes. It needs no answer on the socket, so it works
// while the control loop is stuck.
func stacksCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stacks", flag.ExitOnError)
	state := fs.String("state", "", "state directory (default: see gff-hab -h)")
	wait := fs.Duration("wait", 5*time.Second, "give up when no dump appears within this long")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errors.New("stacks takes no arguments")
	}
	dir, err := stateDir(*state)
	if err != nil {
		return err
	}
	m, ok, err := health.ReadMarker(filepath.Join(dir, runDir, habMarkerFile))
	if err != nil {
		return err
	}
	if !ok || !markerAlive(m, procStarted) {
		return errors.New("gff-hab is not running (gff-hab status)")
	}
	if !m.Stacks {
		return errors.New("this gff-hab predates stack dumps: SIGUSR1 would end it; restart it on this build first")
	}
	crash := filepath.Join(dir, crashDir)
	prefix := "hab-" + m.Run + stacksInfix
	before := map[string]bool{}
	for _, n := range dumpsOf(crash, prefix) {
		before[n] = true
	}
	if err := syscall.Kill(m.Pid, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("signal gff-hab (pid %d): %w", m.Pid, err)
	}
	deadline := time.Now().Add(*wait)
	for time.Now().Before(deadline) {
		for _, n := range dumpsOf(crash, prefix) {
			if !before[n] {
				_, err := fmt.Fprintln(out, filepath.Join(crash, n))
				return err
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("no stack dump in %s after %s; gff-hab logs -grep stacks may say why", crash, *wait)
}

// dumpsOf lists the stack dumps in dir whose names start with prefix.
func dumpsOf(dir, prefix string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".txt") {
			out = append(out, e.Name())
		}
	}
	return out
}
