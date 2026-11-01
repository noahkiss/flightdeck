package detect

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// ErrNoGroup means the process group has no live process.
var ErrNoGroup = errors.New("no process in the group")

// GroupProcesses lists the live processes of process group pgid, the way DetectProcess wants
// them: the leader (the process whose pid is pgid) and the other members by pid. A group whose
// leader has exited has an empty Leader. It reads /proc on Linux, asks the kernel with sysctl
// on macOS, and runs ps elsewhere, so it needs no cgo. It returns ctx's error as soon as ctx
// ends, even when a read under it hangs, and at most maxGroup processes. Where ps prints argv
// as one line, Args are split on white space, so an argument that holds a space reads as two.
func GroupProcesses(ctx context.Context, pgid int) (Foreground, error) {
	if pgid <= 0 {
		return Foreground{}, ErrNoGroup
	}
	ps, err := listGroup(ctx, pgid)
	if err != nil {
		return Foreground{}, err
	}
	if len(ps) == 0 {
		return Foreground{}, ErrNoGroup
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].PID < ps[j].PID })
	var fg Foreground
	for _, p := range ps {
		if p.PID == pgid {
			fg.Leader = p
		} else {
			fg.Members = append(fg.Members, p)
		}
	}
	return fg, nil
}

// maxAbandoned bounds how many group listings may run on with no caller waiting.
const maxAbandoned = 8

// ErrListingStuck means an earlier listing of the same group has not returned, though every
// caller that waited for it has given up: a read under it is stuck in the kernel.
var ErrListingStuck = errors.New("an earlier listing has not returned")

// ErrTooManyStuck means maxAbandoned listings are still stuck, so no new one starts.
var ErrTooManyStuck = errors.New("too many earlier listings have not returned")

// flightKey names one listing: the source it reads (a procfs root, sysctl, ps) and the group.
type flightKey struct {
	src  string
	pgid int
}

// flightGroup runs at most one listing per key at a time. A caller that finds one running
// waits for it with the others; when every waiter has given up, the listing's context ends and
// it is abandoned. A listing that then still does not return is stuck in the kernel (a read
// that takes a hung process's memory lock), and it holds a goroutine and an OS thread. A later
// call for that key answers ErrListingStuck at once instead of starting another, and no new
// listing starts while max are abandoned. So a client that polls a stuck pane costs nothing.
type flightGroup struct {
	max       int
	mu        sync.Mutex
	m         map[flightKey]*flight
	abandoned int
}

type flight struct {
	done      chan struct{}
	ps        []Process
	err       error
	waiters   int
	abandoned bool
	cancel    context.CancelFunc
}

var groupFlights = &flightGroup{max: maxAbandoned}

// bounded runs list on its own goroutine, one per key at a time, and returns when list does or
// ctx ends, whichever is first. A read that hangs in the kernel (a /proc file, a sysctl, a ps
// that a kill cannot reach) then holds only that goroutine, never the caller; list should stop
// on its context too.
func bounded(ctx context.Context, key flightKey, list func(context.Context) ([]Process, error)) ([]Process, error) {
	return groupFlights.run(ctx, key, list)
}

func (g *flightGroup) run(ctx context.Context, key flightKey, list func(context.Context) ([]Process, error)) ([]Process, error) {
	g.mu.Lock()
	f := g.m[key]
	switch {
	case f != nil && f.abandoned:
		g.mu.Unlock()
		return nil, ErrListingStuck
	case f == nil && g.abandoned >= g.max:
		g.mu.Unlock()
		return nil, ErrTooManyStuck
	case f == nil:
		if g.m == nil {
			g.m = map[flightKey]*flight{}
		}
		lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &flight{done: make(chan struct{}), cancel: cancel}
		g.m[key] = f
		go func() {
			ps, err := list(lctx)
			cancel()
			g.mu.Lock()
			f.ps, f.err = ps, err
			delete(g.m, key)
			if f.abandoned {
				g.abandoned--
			}
			g.mu.Unlock()
			close(f.done)
		}()
	}
	f.waiters++
	g.mu.Unlock()
	select {
	case <-f.done:
		return append([]Process(nil), f.ps...), f.err
	case <-ctx.Done():
		g.mu.Lock()
		defer g.mu.Unlock()
		if f.waiters--; f.waiters == 0 && g.m[key] == f {
			f.abandoned = true
			g.abandoned++
			f.cancel()
		}
		return nil, ctx.Err()
	}
}
