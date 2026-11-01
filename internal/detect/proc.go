package detect

import (
	"context"
	"errors"
	"sort"
)

// ErrNoGroup means the process group has no live process.
var ErrNoGroup = errors.New("no process in the group")

// GroupProcesses lists the live processes of process group pgid, the way DetectProcess wants
// them: the leader (the process whose pid is pgid) and the other members by pid. A group whose
// leader has exited has an empty Leader. It reads /proc on Linux and runs ps elsewhere, so it
// needs no cgo. It returns ctx's error when ctx ends first, and at most maxGroup processes.
// Where ps prints argv as one line, Args are split on white space, so an argument that holds a
// space reads as two.
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
