package hoststats

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/noahkiss/flightdeck/api"
)

// readLocal fills CPU, memory, battery (when battery is set) and uptime from /proc and /sys.
func readLocal(s *api.HostStats, prev *cpuSample, battery bool) {
	readProcSys(s, prev, battery, "/")
}

// readProcSys is readLocal over the file systems under root, so a test can give fixtures.
func readProcSys(s *api.HostStats, prev *cpuSample, battery bool, root string) {
	if b, err := os.ReadFile(filepath.Join(root, "proc/stat")); err == nil {
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
			pct := cpuPct(*prev, cur)
			s.CPUPct = &pct
			*prev = cur
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "proc/meminfo")); err == nil {
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
	if b, err := os.ReadFile(filepath.Join(root, "proc/uptime")); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			if up, err := strconv.ParseFloat(f[0], 64); err == nil {
				s.UptimeS = uint64(up)
			}
		}
	}
	if !battery {
		return
	}
	// The machine's battery: type Battery, and not a device's own (a wireless mouse or
	// keyboard has scope Device).
	supplies, _ := filepath.Glob(filepath.Join(root, "sys/class/power_supply/*"))
	for _, d := range supplies {
		if t, err := os.ReadFile(filepath.Join(d, "type")); err != nil || strings.TrimSpace(string(t)) != "Battery" {
			continue
		}
		if sc, err := os.ReadFile(filepath.Join(d, "scope")); err == nil && strings.TrimSpace(string(sc)) == "Device" {
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

// diskUse is the used and total bytes of the file system that holds path, as df counts them.
func diskUse(path string) (used, total uint64) {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0, 0
	}
	return diskFigures(st.Blocks, st.Bfree, st.Bavail, uint64(st.Bsize))
}
