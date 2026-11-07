package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// statStart reads the start tick, field 22, of a /proc/<pid>/stat line. The command name in
// field 2 may hold spaces and parentheses, so the fields count from its last ')'.
func statStart(b []byte) (string, bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return "", false
	}
	return f[19], true
}

// readProcfs lists the processes under a procfs root with their arguments from
// <pid>/cmdline, NUL-separated words joined by spaces. A process with no arguments (a kernel
// thread, a zombie) or one that went away is skipped. An empty list is an error: a procfs that
// shows no process cannot prove a conversation free.
func readProcfs(ctx context.Context, root string) ([]outsideProc, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var procs []outsideProc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.TrimSpace(string(bytes.ReplaceAll(bytes.TrimRight(b, "\x00"), []byte{0}, []byte{' '})))
		if args == "" {
			continue
		}
		procs = append(procs, outsideProc{pid: pid, args: args})
	}
	if len(procs) == 0 {
		return nil, errors.New(root + " lists no process")
	}
	return procs, nil
}
