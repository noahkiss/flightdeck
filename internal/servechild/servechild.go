// Package servechild starts the programs gff-hab serve runs that may run code a repository
// holds: git (its filters, hooks and fsmonitor), lane checks, worktree scripts
// (docs/specs/hab-logic-api.md 4.1). An agent with file writes can put such code in a
// repository, so each of these programs, and whatever it leaves running, must count as a pane's
// to the pane caller check, also after it detaches from gff-hab's process tree.
//
// Run marks each program twice:
//   - Its environment holds GFF_HAB set to hook.ChildAddr: it names gff-hab's hook socket, which
//     the check reads as a pane's, and holds no token, so it reaches no hook. A program that
//     detaches but keeps its environment stays a pane's, and the CLIs' environment checks
//     refuse in it.
//   - It starts in a session of its own, and the session id stays a root (Sessions) while the
//     program runs and after, until no process is left in that session (Sweep). A program that
//     detaches and clears its environment but keeps the session stays a pane's.
//
// Until Enable runs (in every process but gff-hab serve), Run is cmd.Run.
package servechild

import (
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/noahkiss/flightdeck/internal/hook"
)

type registry struct {
	// starting is held for reading across each Start and its record, and for writing by
	// Sessions, so Sessions never misses a program that has started.
	starting sync.RWMutex
	mu       sync.Mutex
	mark     string       // the GFF_HAB entry; "" until Enable
	live     map[int]bool // the session ids of programs that run; each is the program's pid
	ended    map[int]bool // session ids whose program ended
}

var std = registry{live: map[int]bool{}, ended: map[int]bool{}}

// Enable turns the marks on for this process, with the hook socket gff-hab serve listens on.
func Enable(hookSocket string) {
	std.mu.Lock()
	defer std.mu.Unlock()
	std.mark = hook.Env + "=" + hook.ChildAddr(hookSocket)
}

// Run runs cmd as cmd.Run does, marked as a program gff-hab serve started. cmd.Env nil means
// the process environment, as in exec.Cmd.
func Run(cmd *exec.Cmd) error {
	std.mu.Lock()
	mark := std.mark
	std.mu.Unlock()
	if mark == "" {
		return cmd.Run()
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, hook.Env+"=") {
			out = append(out, kv)
		}
	}
	cmd.Env = append(out, mark)
	ownSession(cmd)

	std.starting.RLock()
	if err := cmd.Start(); err != nil {
		std.starting.RUnlock()
		return err
	}
	pid := cmd.Process.Pid
	std.mu.Lock()
	std.live[pid] = true
	delete(std.ended, pid)
	std.mu.Unlock()
	std.starting.RUnlock()

	err := cmd.Wait()
	std.mu.Lock()
	delete(std.live, pid)
	std.ended[pid] = true
	std.mu.Unlock()
	return err
}

// Sessions returns the session ids that make a caller a pane's: one per program Run started
// that still runs, or whose session Sweep has not yet found empty.
func Sessions() []int {
	std.starting.Lock()
	defer std.starting.Unlock()
	std.mu.Lock()
	defer std.mu.Unlock()
	out := make([]int, 0, len(std.live)+len(std.ended))
	for sid := range std.live {
		out = append(out, sid)
	}
	for sid := range std.ended {
		out = append(out, sid)
	}
	return out
}

// Ended returns how many ended programs' sessions are kept, for Sweep.
func Ended() int {
	std.mu.Lock()
	defer std.mu.Unlock()
	return len(std.ended)
}

// Sweep drops each ended program's session that has no process left. members holds every
// session id that has a live process. running reports whether a process with this pid runs:
// the program at that pid ended, so a process there now took the pid, and a session it leads is
// not the program's.
func Sweep(members map[int]bool, running func(pid int) bool) {
	std.mu.Lock()
	defer std.mu.Unlock()
	for sid := range std.ended {
		if !members[sid] || running(sid) {
			delete(std.ended, sid)
		}
	}
}

// reset clears the registry, for tests.
func reset() {
	std.mu.Lock()
	defer std.mu.Unlock()
	std.mark = ""
	std.live = map[int]bool{}
	std.ended = map[int]bool{}
}
