package hoststats

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// Reader reads this machine's figures. Each call measures CPU since the last call; the
// first measures since boot.
type Reader struct {
	Battery  bool
	DiskPath string // Empty uses the user's home directory.
	mu       sync.Mutex
	prev     cpuSample
	got      time.Time
	cached   api.HostStats
	now      func() time.Time
	sample   func(*api.HostStats, *cpuSample, bool)
	disk     func(string) (uint64, uint64)
}

type cpuSample struct{ busy, total uint64 }

// Read returns one cached reading across callers for two seconds.
func (l *Reader) Read() (api.HostStats, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	if !l.got.IsZero() && now.Sub(l.got) < 2*time.Second {
		return clone(l.cached), nil
	}
	s := api.HostStats{At: now.UTC().Format(time.RFC3339)}
	sample := l.sample
	if sample == nil {
		sample = readLocal
	}
	sample(&s, &l.prev, l.Battery)
	disk := l.disk
	if disk == nil {
		disk = diskUse
	}
	path := l.DiskPath
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	if path != "" {
		s.DiskUsed, s.DiskTotal = disk(path)
	}
	l.cached, l.got = s, now
	return clone(s), nil
}

// Stats implements the control connection's stats provider.
func (l *Reader) Stats(ctx context.Context) (api.HostStats, error) {
	if err := ctx.Err(); err != nil {
		return api.HostStats{}, err
	}
	return l.Read()
}

func clone(s api.HostStats) api.HostStats {
	if s.CPUPct != nil {
		v := *s.CPUPct
		s.CPUPct = &v
	}
	if s.BatteryPct != nil {
		v := *s.BatteryPct
		s.BatteryPct = &v
	}
	return s
}

// diskFigures is a file system's used and total bytes as df counts them: used is every block
// not free, and total is used plus what an ordinary user may still write, so the blocks kept
// for root count as neither.
func diskFigures(blocks, bfree, bavail, bsize uint64) (used, total uint64) {
	if bfree > blocks {
		return 0, 0
	}
	used = (blocks - bfree) * bsize
	return used, used + bavail*bsize
}

// cpuPct is the busy share between two samples, in whole percent.
func cpuPct(prev, cur cpuSample) int {
	if cur.total <= prev.total || cur.busy < prev.busy {
		return 0
	}
	return int(min((cur.busy-prev.busy)*100/(cur.total-prev.total), 100))
}
