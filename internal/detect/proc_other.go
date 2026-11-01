//go:build !linux

package detect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// psTimeout bounds each ps run.
const psTimeout = 5 * time.Second

// listGroup asks ps for every process's group, then for the name and arguments of those in
// group pgid. ucomm is the executable name (macOS cuts it at 16 bytes); args is argv as one
// line.
func listGroup(pgid int) ([]Process, error) {
	groups, err := ps("-A", "-o", "pid=,pgid=")
	if err != nil {
		return nil, err
	}
	var pids []string
	for _, l := range groups {
		f := strings.Fields(l)
		if len(f) == 2 && f[1] == strconv.Itoa(pgid) {
			pids = append(pids, f[0])
		}
	}
	if len(pids) == 0 {
		return nil, nil
	}
	list := strings.Join(pids, ",")
	names, err := ps("-o", "pid=,ucomm=", "-p", list)
	if err != nil {
		return nil, err
	}
	args, err := ps("-o", "pid=,args=", "-p", list)
	if err != nil {
		return nil, err
	}
	byPID := map[int]int{}
	var out []Process
	for _, l := range names {
		if pid, rest, ok := pidLine(l); ok {
			byPID[pid] = len(out)
			out = append(out, Process{PID: pid, Name: rest})
		}
	}
	for _, l := range args {
		if pid, rest, ok := pidLine(l); ok {
			if i, ok := byPID[pid]; ok {
				out[i].Args = strings.Fields(rest)
			}
		}
	}
	return out, nil
}

// pidLine splits a ps line into its leading pid and the rest, trimmed.
func pidLine(l string) (int, string, bool) {
	num, rest, _ := strings.Cut(strings.TrimSpace(l), " ")
	pid, err := strconv.Atoi(num)
	if err != nil {
		return 0, "", false
	}
	return pid, strings.TrimSpace(rest), true
}

// ps runs ps with args and returns its non-empty output lines. ps exits 1 with no output when
// -p names no live process; that is no lines, not an error.
func ps(args ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 && out.Len() == 0 && errb.Len() == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("ps: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	var lines []string
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if l := sc.Text(); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines, sc.Err()
}
