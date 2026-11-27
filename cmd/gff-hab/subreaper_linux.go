package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// startSubreaper makes gff-hab serve a child subreaper (hab-logic-api.md 4.1): a process that
// a program serve started leaves running, and whose parent exits, moves to serve instead of
// init, so the ancestor walk still finds serve. Serve then reaps what it adopts. It reaps only
// zombies no Go process handle holds: os/exec waits on its own children through a pidfd, and
// reaping one of those would fail its Wait. Where Go waits without a pidfd (an old kernel), serve
// cannot tell the two apart and stays no subreaper.
func startSubreaper(logf func(format string, args ...any)) {
	if !goWaitsByPidfd() {
		logf("gff-hab serve is no subreaper: Go waits on its children without a pidfd here, so serve could not tell what it adopts from what it started")
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		logf("gff-hab serve is no subreaper: %v", err)
		return
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, unix.SIGCHLD)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sig:
			case <-t.C:
			}
			reapAdopted()
		}
	}()
}

// goWaitsByPidfd starts a short program and reports whether this process then holds a pidfd
// for it.
func goWaitsByPidfd() bool {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		return false
	}
	held := pidfdPids()[cmd.Process.Pid]
	_ = cmd.Wait()
	return held
}

// reapAdopted reaps each zombie child of this process that no pidfd here holds.
func reapAdopted() {
	var zombies []int
	for _, pid := range children() {
		if childState(pid) == 'Z' {
			zombies = append(zombies, pid)
		}
	}
	if len(zombies) == 0 {
		return
	}
	held := pidfdPids()
	for _, pid := range zombies {
		if held[pid] {
			continue
		}
		var ws unix.WaitStatus
		_, _ = unix.Wait4(pid, &ws, unix.WNOHANG, nil)
	}
}

// children lists this process's children: /proc/self/task/*/children, or a scan of /proc for
// a kernel without it.
func children() []int {
	tasks, _ := filepath.Glob("/proc/self/task/*/children")
	if len(tasks) > 0 {
		var out []int
		ok := false
		for _, f := range tasks {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			ok = true
			for _, w := range strings.Fields(string(b)) {
				if pid, err := strconv.Atoi(w); err == nil {
					out = append(out, pid)
				}
			}
		}
		if ok {
			return out
		}
	}
	self := os.Getpid()
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, ok := procPPID(pid); ok && ppid == self {
			out = append(out, pid)
		}
	}
	return out
}

// statFields returns the fields of /proc/<pid>/stat after the command name.
func statFields(pid int) []string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil
	}
	end := bytes.LastIndexByte(b, ')')
	if end < 0 {
		return nil
	}
	return strings.Fields(string(b[end+1:]))
}

func childState(pid int) byte {
	f := statFields(pid)
	if len(f) == 0 || f[0] == "" {
		return 0
	}
	return f[0][0]
}

func procPPID(pid int) (int, bool) {
	f := statFields(pid)
	if len(f) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	return ppid, err == nil
}

// pidfdPids returns the pids this process holds a pidfd for, from the "Pid:" line of each
// /proc/self/fdinfo entry.
func pidfdPids() map[int]bool {
	out := map[int]bool{}
	ents, err := os.ReadDir("/proc/self/fdinfo")
	if err != nil {
		return out
	}
	for _, e := range ents {
		f, err := os.Open(filepath.Join("/proc/self/fdinfo", e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "Pid:"); ok {
				if pid, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && pid > 0 {
					out[pid] = true
				}
				break
			}
		}
		f.Close()
	}
	return out
}
