package gffclient

import (
	"context"
	"encoding/binary"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/noahkiss/flightdeck/api"
)

// readLocal fills CPU, memory, battery and uptime from sysctl, with no cgo. macOS gives no CPU
// time counters through sysctl, so CPU is the one-minute load over the core count. Memory in
// use is all but the free pages, so it counts the file cache.
func readLocal(s *api.HostStats, _ *cpuSample) {
	if b, err := unix.SysctlRaw("vm.loadavg"); err == nil && len(b) >= 24 {
		// struct loadavg { fixpt_t ldavg[3]; long fscale; }: three uint32, padding, an int64.
		load := binary.LittleEndian.Uint32(b[0:4])
		scale := binary.LittleEndian.Uint64(b[16:24])
		if scale > 0 {
			s.CPUPct = min(int(uint64(load)*100/scale)/max(runtime.NumCPU(), 1), 100)
		}
	}
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
	s.BatteryPct = darwinBattery()
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

// diskUse is the used and total bytes of the file system that holds path.
func diskUse(path string) (used, total uint64) {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	return (st.Blocks - st.Bfree) * bs, st.Blocks * bs
}
