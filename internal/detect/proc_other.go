//go:build !linux && !darwin

package detect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	// psTimeout bounds each ps run.
	psTimeout = 5 * time.Second
	// psWaitDelay bounds the wait for a killed ps and its pipes.
	psWaitDelay = 500 * time.Millisecond
)

// listGroup asks ps for every process's group, then for the name and arguments of those in
// group pgid. Each later run prints the group again and parsePS drops rows of another group.
// ucomm is the executable name; args is argv as one line. The runs go on their own goroutine,
// so a ps that a kill cannot reach holds only that goroutine.
func listGroup(ctx context.Context, pgid int) ([]Process, error) {
	return bounded(ctx, func() ([]Process, error) {
		groups, err := ps(ctx, "-A", "-o", "pid=,pgid=")
		if err != nil {
			return nil, err
		}
		pids := psGroupPIDs(groups, pgid)
		if len(pids) == 0 {
			return nil, nil
		}
		list := strings.Join(pids, ",")
		names, err := ps(ctx, "-o", "pid=,pgid=,ucomm=", "-p", list)
		if err != nil {
			return nil, err
		}
		args, err := ps(ctx, "-o", "pid=,pgid=,args=", "-p", list)
		if err != nil {
			return nil, err
		}
		return parsePS(names, args, pgid), nil
	})
}

// ps runs ps with args and returns its non-empty output lines. ps exits 1 with no output when
// -p names no live process; that is no lines, not an error.
func ps(ctx context.Context, args ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, psTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", args...)
	cmd.WaitDelay = psWaitDelay
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ps: %w", ctx.Err())
		}
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
