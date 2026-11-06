package hoststats

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// writeTree writes files under root, each path relative to it.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// This machine's figures from /proc and /sys fixtures: CPU busy time leaves out idle and
// iowait; the battery is the machine's, not a wireless mouse's (scope Device), and not a mains
// supply; with the battery off the bar, it is not read.
func TestReadProcSys(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		// user nice system idle iowait irq softirq steal: 30 busy of 100, then 60 more busy of 100.
		"proc/stat":                          "cpu  10 5 15 60 10 0 0 0 0 0\ncpu0 1 1 1 1 1 0 0 0\n",
		"proc/meminfo":                       "MemTotal:       16000 kB\nMemFree:         1000 kB\nMemAvailable:    4000 kB\n",
		"proc/uptime":                        "273600.50 1000.00\n",
		"sys/class/power_supply/AC/type":     "Mains\n",
		"sys/class/power_supply/AC/capacity": "100\n",
		"sys/class/power_supply/hid-mouse-battery/type":     "Battery\n",
		"sys/class/power_supply/hid-mouse-battery/scope":    "Device\n",
		"sys/class/power_supply/hid-mouse-battery/capacity": "40\n",
		"sys/class/power_supply/BAT0/type":                  "Battery\n",
		"sys/class/power_supply/BAT0/scope":                 "System\n",
		"sys/class/power_supply/BAT0/capacity":              "81\n",
	})
	var prev cpuSample
	var s api.HostStats
	readProcSys(&s, &prev, true, root)
	if s.CPUPct == nil || *s.CPUPct != 30 {
		t.Errorf("first CPU reading %v, want 30 (since boot)", s.CPUPct)
	}
	if s.MemTotal != 16000*1024 || s.MemUsed != 12000*1024 || s.UptimeS != 273600 {
		t.Errorf("mem %d/%d uptime %d", s.MemUsed, s.MemTotal, s.UptimeS)
	}
	if s.BatteryPct == nil || *s.BatteryPct != 81 {
		t.Errorf("battery %v, want 81 from BAT0", s.BatteryPct)
	}
	writeTree(t, root, map[string]string{"proc/stat": "cpu  40 5 45 90 20 0 0 0 0 0\n"})
	s = api.HostStats{}
	readProcSys(&s, &prev, false, root)
	if s.CPUPct == nil || *s.CPUPct != 60 {
		t.Errorf("second CPU reading %v, want 60", s.CPUPct)
	}
	if s.BatteryPct != nil {
		t.Errorf("battery read while off the bar: %d", *s.BatteryPct)
	}

	// A desktop with only a mouse's battery shows none.
	desk := t.TempDir()
	writeTree(t, desk, map[string]string{
		"sys/class/power_supply/hid-mouse-battery/type":     "Battery\n",
		"sys/class/power_supply/hid-mouse-battery/scope":    "Device\n",
		"sys/class/power_supply/hid-mouse-battery/capacity": "40\n",
	})
	s = api.HostStats{}
	readProcSys(&s, &cpuSample{}, true, desk)
	if s.BatteryPct != nil || s.CPUPct != nil {
		t.Errorf("desktop: battery %v, cpu %v; want neither", s.BatteryPct, s.CPUPct)
	}
}
