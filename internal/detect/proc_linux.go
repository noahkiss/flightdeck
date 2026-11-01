package detect

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// listGroup reads /proc/<pid>/stat for each process and keeps those in group pgid. A process
// that exits during the walk is skipped.
func listGroup(pgid int) ([]Process, error) {
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", d.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ...; comm may hold spaces and parentheses, so cut at the
		// last ")".
		lp, rp := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
		if lp < 0 || rp < lp {
			continue
		}
		f := strings.Fields(string(stat[rp+1:]))
		if len(f) < 3 {
			continue
		}
		if g, err := strconv.Atoi(f[2]); err != nil || g != pgid {
			continue
		}
		p := Process{PID: pid, Name: string(stat[lp+1 : rp])}
		if cl, err := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline")); err == nil && len(cl) > 0 {
			p.Args = strings.Split(strings.TrimRight(string(cl), "\x00"), "\x00")
		}
		out = append(out, p)
	}
	return out, nil
}
