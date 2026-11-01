package detect

import (
	"strconv"
	"strings"
)

// psGroupPIDs returns the pids of group pgid from `ps -o pid=,pgid=` lines.
func psGroupPIDs(lines []string, pgid int) []string {
	var pids []string
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 2 && f[1] == strconv.Itoa(pgid) {
			pids = append(pids, f[0])
		}
	}
	return pids
}

// parsePS builds the processes of group pgid from the lines of `ps -o pid=,pgid=,ucomm=` and
// `ps -o pid=,pgid=,args=`. Each run asks by pid, so a row whose group is no longer pgid is a
// pid that exited and was reused between runs: it is dropped. A process whose name row is gone
// is dropped; one whose args row is gone has no Args. At most maxGroup processes are returned.
func parsePS(names, args []string, pgid int) []Process {
	byPID := map[int]int{}
	var out []Process
	for _, l := range names {
		if pid, rest, ok := psLine(l, pgid); ok && len(out) < maxGroup {
			byPID[pid] = len(out)
			out = append(out, Process{PID: pid, Name: rest})
		}
	}
	for _, l := range args {
		if pid, rest, ok := psLine(l, pgid); ok {
			if i, ok := byPID[pid]; ok {
				out[i].Args = strings.Fields(rest)
			}
		}
	}
	return out
}

// psLine splits a "pid pgid rest" ps line, and reports false unless the group is pgid.
func psLine(l string, pgid int) (int, string, bool) {
	f := strings.Fields(l)
	if len(f) < 2 {
		return 0, "", false
	}
	pid, err := strconv.Atoi(f[0])
	if err != nil {
		return 0, "", false
	}
	if g, err := strconv.Atoi(f[1]); err != nil || g != pgid {
		return 0, "", false
	}
	rest := strings.TrimSpace(l)
	for range 2 { // drop the pid and pgid fields, keep the rest as printed
		rest = strings.TrimLeft(rest, " \t")
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			rest = rest[i:]
		} else {
			rest = ""
		}
	}
	return pid, strings.TrimSpace(rest), true
}
