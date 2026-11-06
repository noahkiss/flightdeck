package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// A new pane beside another starts in that pane's working folder (gff-client.md 5.4,
// session.create's cwd_of): the folder its program is in, else the one its shell last reported
// with OSC 7, else the one its session started in.

// paneCwd returns the working folder of pane, or "" when none is known. Each candidate must be
// an absolute path to a directory.
func (a *actions) paneCwd(ctx context.Context, pane string) string {
	var cands []func() string
	if a.sp != nil && a.sp.lk != nil {
		if u, ok := parseUUID(pane); ok {
			lk := a.sp.lk
			cands = append(cands, func() string {
				if st, ok := lk.Pane(u); ok && st.Pid > 0 && !st.Exited {
					return procCwd(ctx, int(st.Pid))
				}
				return ""
			}, func() string { return lk.Cwd(u) })
		}
	}
	if a.folderOf != nil {
		cands = append(cands, func() string {
			if ss, ok := a.store.SessionByPane(pane); ok {
				return a.folderOf(ctx, ss.ID)
			}
			return ""
		})
	}
	for _, f := range cands {
		if d := f(); d != "" && checkCwd(d) == nil {
			return d
		}
	}
	return ""
}

// procCwd returns a process's working directory: /proc on Linux, lsof on macOS. "" when it
// cannot be read.
func procCwd(ctx context.Context, pid int) string {
	switch runtime.GOOS {
	case "linux":
		d, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
		if err != nil {
			return ""
		}
		return d
	case "darwin":
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
		if err != nil {
			return ""
		}
		return lsofName(out)
	}
	return ""
}

// lsofName reads the name field (n) of lsof's -F output.
func lsofName(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if n, ok := strings.CutPrefix(sc.Text(), "n"); ok {
			return n
		}
	}
	return ""
}
