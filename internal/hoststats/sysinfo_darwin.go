package hoststats

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/noahkiss/flightdeck/api"
)

// readLocal fills CPU, memory, battery (when battery is set) and uptime from sysctl, with no
// cgo. CPU is absent until a CPU-time reader exists; load average is not CPU usage. Memory in use is all but the free pages, so it counts the file cache. The
// battery comes from pmset, run only when the bar shows the battery.
func readLocal(s *api.HostStats, _ *cpuSample, battery bool) {
	if total, err := unix.SysctlUint64("hw.memsize"); err == nil {
		s.MemTotal = total
		if free, err := unix.SysctlUint32("vm.page_free_count"); err == nil {
			if f := uint64(free) * uint64(unix.Getpagesize()); f <= total {
				s.MemUsed = total - f
			}
		}
	}
	if tv, err := unix.SysctlTimeval("kern.boottime"); err == nil && tv.Sec > 0 {
		s.UptimeS = uint64(max(time.Since(time.Unix(tv.Sec, int64(tv.Usec)*1000))/time.Second, 0))
	}
	if battery {
		s.BatteryPct = darwinBattery()
	}
}

var (
	battMu   sync.Mutex
	battAt   time.Time
	battPct  *int
	battExpr = regexp.MustCompile(`(\d+)%`)
)

// darwinBattery reads the battery charge from pmset at most once a minute. A Mac with no
// battery gives no percentage, so the item hides.
func darwinBattery() *int {
	battMu.Lock()
	defer battMu.Unlock()
	if !battAt.IsZero() && time.Since(battAt) < time.Minute {
		return battPct
	}
	battAt = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g", "batt").Output()
	battPct = nil
	if err == nil {
		if m := battExpr.FindSubmatch(out); m != nil {
			if n, err := strconv.Atoi(string(m[1])); err == nil {
				battPct = &n
			}
		}
	}
	return battPct
}

// diskUse is the used and total bytes of the file system that holds path, as df counts them.
func diskUse(path string) (used, total uint64) {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0, 0
	}
	return diskFigures(st.Blocks, st.Bfree, st.Bavail, uint64(st.Bsize))
}
