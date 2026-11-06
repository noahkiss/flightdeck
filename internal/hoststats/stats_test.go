package hoststats

import (
	"github.com/noahkiss/flightdeck/api"
	"testing"
	"time"
)

func TestReaderCache(t *testing.T) {
	now := time.Unix(100, 0)
	calls := 0
	r := &Reader{DiskPath: "/state", now: func() time.Time { return now }, sample: func(s *api.HostStats, _ *cpuSample, battery bool) {
		calls++
		v := 42
		s.CPUPct = &v
	}, disk: func(path string) (uint64, uint64) {
		if path != "/state" {
			t.Errorf("disk path %q", path)
		}
		return 10, 20
	}}
	first, _ := r.Read()
	*first.CPUPct = 99
	again, _ := r.Read()
	if calls != 1 || *again.CPUPct != 42 || again.DiskUsed != 10 || again.DiskTotal != 20 {
		t.Fatalf("cached: calls=%d %+v", calls, again)
	}
	now = now.Add(2 * time.Second)
	last, _ := r.Read()
	if calls != 2 || last.At == first.At {
		t.Fatalf("refresh: calls=%d %+v", calls, last)
	}
}

func TestDiskFigures(t *testing.T) {
	used, total := diskFigures(1000, 400, 350, 4096)
	if used != 600*4096 || total != 950*4096 {
		t.Fatalf("disk %d/%d", used, total)
	}
	if used, total := diskFigures(1, 2, 0, 4096); used != 0 || total != 0 {
		t.Fatalf("invalid disk %d/%d", used, total)
	}
}
