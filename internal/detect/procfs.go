package detect

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxGroup bounds how many processes one group listing returns.
const maxGroup = 256

// procGroup lists group pgid from a procfs mounted at root (/proc on Linux). The walk runs on
// its own goroutine and procGroup returns when ctx ends, so a read that hangs cannot hold the
// caller. It reads cmdline only for a process that is not in uninterruptible sleep (state D):
// cmdline takes the process's memory lock, and a process stuck in D (a hung network or FUSE
// mount) can hold it for good. A process that exits during the walk is skipped. At most
// maxGroup processes are returned.
func procGroup(ctx context.Context, root string, pgid int) ([]Process, error) {
	type result struct {
		ps  []Process
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ps, err := walkProc(ctx, root, pgid)
		ch <- result{ps, err}
	}()
	select {
	case r := <-ch:
		return r.ps, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func walkProc(ctx context.Context, root string, pgid int) ([]Process, error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(d.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(root, d.Name(), "stat"))
		if err != nil {
			continue
		}
		name, state, g, ok := parseStat(stat)
		if !ok || g != pgid {
			continue
		}
		p := Process{PID: pid, Name: name}
		if state != "D" {
			if cl, err := os.ReadFile(filepath.Join(root, d.Name(), "cmdline")); err == nil && len(cl) > 0 {
				p.Args = strings.Split(strings.TrimRight(string(cl), "\x00"), "\x00")
			}
		}
		if out = append(out, p); len(out) == maxGroup {
			break
		}
	}
	return out, nil
}

// parseStat reads the name, state and process group from a /proc/<pid>/stat line:
// "pid (comm) state ppid pgrp ...". comm may hold spaces and parentheses, so it is cut at the
// last ")".
func parseStat(stat []byte) (name, state string, pgrp int, ok bool) {
	lp, rp := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if lp < 0 || rp < lp {
		return "", "", 0, false
	}
	f := strings.Fields(string(stat[rp+1:]))
	if len(f) < 3 {
		return "", "", 0, false
	}
	g, err := strconv.Atoi(f[2])
	if err != nil {
		return "", "", 0, false
	}
	return string(stat[lp+1 : rp]), f[0], g, true
}
