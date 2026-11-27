package peerproc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, the unit of a /proc stat start time. Linux fixes it at 100 on every
// architecture Flightdeck builds for.
const clockTicks = 100

// procfs reads processes from a /proc tree. root is "/proc" on Linux; tests point it at a
// fixture.
type procfs struct {
	root string
	now  func() time.Time
	// sinceBoot is the time since boot. Linux reads CLOCK_BOOTTIME, the clock the stat start
	// time counts on (an older kernel counts on CLOCK_MONOTONIC, which places every start
	// earlier by the time asleep: safe for the check). Nil reads <root>/uptime, which a
	// container's /proc may give for another clock.
	sinceBoot func() (time.Duration, error)
}

// Proc parses <root>/<pid>/stat. The command name may hold spaces and parentheses, so the
// fields are read after the last ')'. The start time is ticks since boot; the boot is now minus
// the time since boot.
func (f procfs) Proc(pid int) (Proc, error) {
	b, err := os.ReadFile(filepath.Join(f.root, strconv.Itoa(pid), "stat"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Proc{}, fmt.Errorf("no process %d", pid)
		}
		return Proc{}, err
	}
	end := bytes.LastIndexByte(b, ')')
	if end < 0 {
		return Proc{}, fmt.Errorf("process %d: unreadable stat", pid)
	}
	// After ')': state(3) ppid(4) pgrp(5) session(6) ... starttime(22); field n is at n-3.
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 {
		return Proc{}, fmt.Errorf("process %d: short stat", pid)
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return Proc{}, fmt.Errorf("process %d ended", pid)
	}
	ppid, err1 := strconv.Atoi(fields[1])
	sid, err2 := strconv.Atoi(fields[3])
	ticks, err3 := strconv.ParseUint(fields[19], 10, 64)
	if err := errors.Join(err1, err2, err3); err != nil {
		return Proc{}, fmt.Errorf("process %d: stat: %v", pid, err)
	}
	since := f.sinceBoot
	if since == nil {
		since = f.uptime
	}
	up, err := since()
	if err != nil {
		return Proc{}, err
	}
	boot := f.now().Add(-up)
	start := boot.Add(time.Duration(ticks) * time.Second / clockTicks)
	return Proc{PID: pid, PPID: ppid, SID: sid, Start: start}, nil
}

// uptime reads the first field of <root>/uptime.
func (f procfs) uptime() (time.Duration, error) {
	up, err := os.ReadFile(filepath.Join(f.root, "uptime"))
	if err != nil {
		return 0, err
	}
	uf := strings.Fields(string(up))
	if len(uf) == 0 {
		return 0, errors.New("unreadable uptime")
	}
	secs, err := strconv.ParseFloat(uf[0], 64)
	if err != nil {
		return 0, fmt.Errorf("uptime: %v", err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// Env reads <root>/<pid>/environ, at most 2 MiB.
func (f procfs) Env(pid int) []byte {
	fh, err := os.Open(filepath.Join(f.root, strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil
	}
	defer fh.Close()
	b, _ := io.ReadAll(io.LimitReader(fh, 2<<20))
	return b
}
