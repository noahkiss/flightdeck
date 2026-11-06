// Package procscope ends same-user processes that retained a released pane's hook token.
package procscope

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"github.com/noahkiss/flightdeck/internal/hook"
	"golang.org/x/sys/unix"
)

var ErrBusy = errors.New("a process scan is still running")

// A scan can block inside a kernel environment read. One flight bounds those reads
// across every sweep, even after its caller's context ends.
var flight = make(chan struct{}, 1)

// Sweep ends processes carrying token. The caller need not wait for a stuck OS read.
// A process that clears its environment cannot be found by this policy.
func Sweep(ctx context.Context, token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	select {
	case flight <- struct{}{}:
	default:
		return 0, ErrBusy
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-flight }()
		n, err := sweep(ctx, token, listProcesses, inspectProcess, unix.Kill)
		done <- result{n, err}
	}()
	select {
	case r := <-done:
		return r.n, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type process struct {
	pid  int
	born string
}
type inspector func(int, string) (process, bool)

func sweep(ctx context.Context, token string, list func() ([]int, error), inspect inspector, kill func(int, unix.Signal) error) (int, error) {
	pids, err := list()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		p, ok := inspect(pid, token)
		if !ok {
			continue
		}
		// A PID or its environment can change during discovery. Re-read both before
		// sending a signal; never signal a process that no longer carries the token.
		q, ok := inspect(pid, token)
		if !ok || p != q || ctx.Err() != nil {
			continue
		}
		if err := kill(pid, unix.SIGKILL); err == nil {
			n++
		}
	}
	return n, nil
}

func matchesEnv(env []byte, token string) bool {
	for _, entry := range bytes.Split(env, []byte{0}) {
		if !bytes.HasPrefix(entry, []byte(hook.Env+"=")) {
			continue
		}
		_, got, err := hook.ParseAddr(string(entry[len(hook.Env)+1:]))
		if err == nil && got == token {
			return true
		}
	}
	return false
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 2<<20+1))
	if len(b) > 2<<20 {
		return nil, errors.New("process environment too large")
	}
	return b, err
}
