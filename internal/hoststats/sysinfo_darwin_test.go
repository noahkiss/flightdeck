package hoststats

import (
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// On macOS, CPU, memory and uptime come from sysctl, and pmset runs only for a bar that shows
// the battery.
func TestReadLocalDarwin(t *testing.T) {
	battMu.Lock()
	battAt, battPct = battAt.AddDate(-1, 0, 0), nil // as if no reading in the last minute
	before := battAt
	battMu.Unlock()
	var s api.HostStats
	readLocal(&s, &cpuSample{}, false)
	if s.CPUPct != nil || s.MemTotal == 0 || s.MemUsed > s.MemTotal || s.UptimeS == 0 {
		t.Errorf("figures %+v", s)
	}
	battMu.Lock()
	ran := battAt != before
	battMu.Unlock()
	if ran || s.BatteryPct != nil {
		t.Error("pmset ran for a bar that does not show the battery")
	}
	readLocal(&s, &cpuSample{}, true)
	battMu.Lock()
	ran = battAt != before
	battMu.Unlock()
	if !ran {
		t.Error("pmset did not run for a bar that shows the battery")
	}
	if used, total := diskUse("/"); total == 0 || used > total {
		t.Errorf("disk %d/%d", used, total)
	}
}
