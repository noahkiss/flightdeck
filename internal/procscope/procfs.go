package procscope

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func procPIDs(root string) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err == nil && n > 1 {
			out = append(out, n)
		}
	}
	return out, nil
}

// inspectProc checks the real and effective uid and the process's start tick.
// Zombie and uninterruptible processes have no readable environment to inspect.
func inspectProc(root string, pid, uid int, token string) (process, bool) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	stat, err := readLimited(filepath.Join(dir, "stat"))
	if err != nil {
		return process{}, false
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return process{}, false
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 || fields[0] == "D" || fields[0] == "Z" {
		return process{}, false
	}
	born, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return process{}, false
	}
	status, err := readLimited(filepath.Join(dir, "status"))
	if err != nil {
		return process{}, false
	}
	owned := false
	for _, line := range strings.Split(string(status), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "Uid:" {
			owned = f[1] == strconv.Itoa(uid) && f[2] == strconv.Itoa(uid)
		}
	}
	if !owned {
		return process{}, false
	}
	env, err := readLimited(filepath.Join(dir, "environ"))
	if err != nil || !matchesEnv(env, token) {
		return process{}, false
	}
	// Verify start tick after reading the process memory too.
	again, err := readLimited(filepath.Join(dir, "stat"))
	if err != nil {
		return process{}, false
	}
	e := bytes.LastIndexByte(again, ')')
	if e < 0 {
		return process{}, false
	}
	f := strings.Fields(string(again[e+1:]))
	if len(f) < 20 || f[19] != strconv.FormatUint(born, 10) {
		return process{}, false
	}
	return process{pid: pid, born: strconv.FormatUint(born, 10)}, true
}
