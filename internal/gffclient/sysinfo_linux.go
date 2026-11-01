package gffclient

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/noahkiss/flightdeck/api"
)

// readLocal fills CPU, memory, battery and uptime from /proc and /sys.
func readLocal(s *api.HostStats, prev *cpuSample) {
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		line, _, _ := strings.Cut(string(b), "\n")
		f := strings.Fields(line)
		if len(f) > 4 && f[0] == "cpu" {
			var cur cpuSample
			for i, v := range f[1:min(len(f), 9)] { // user to steal; guest is inside user
				n, _ := strconv.ParseUint(v, 10, 64)
				cur.total += n
				if i != 3 && i != 4 { // idle and iowait
					cur.busy += n
				}
			}
			s.CPUPct = cpuPct(*prev, cur)
			*prev = cur
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail uint64
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			n, _ := strconv.ParseUint(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				total = n * 1024
			case "MemAvailable:":
				avail = n * 1024
			}
		}
		if total >= avail {
			s.MemTotal, s.MemUsed = total, total-avail
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			if up, err := strconv.ParseFloat(f[0], 64); err == nil {
				s.UptimeS = uint64(up)
			}
		}
	}
	supplies, _ := filepath.Glob("/sys/class/power_supply/*")
	for _, d := range supplies {
		if t, err := os.ReadFile(filepath.Join(d, "type")); err != nil || strings.TrimSpace(string(t)) != "Battery" {
			continue
		}
		if c, err := os.ReadFile(filepath.Join(d, "capacity")); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(c))); err == nil {
				s.BatteryPct = &n
				break
			}
		}
	}
}

// diskUse is the used and total bytes of the file system that holds path.
func diskUse(path string) (used, total uint64) {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	return (st.Blocks - st.Bfree) * bs, st.Blocks * bs
}
