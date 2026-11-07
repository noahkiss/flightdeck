package main

import (
	"os"
	"strconv"
)

// procStart reads a pid's start as Claude Code records it in its registry on Linux: the start
// tick, field 22 of /proc/<pid>/stat.
func procStart(pid int) (string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	return statStart(b)
}
