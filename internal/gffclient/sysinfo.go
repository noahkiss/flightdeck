package gffclient

// System info: CPU, memory, disk, battery and uptime on the right of the shortcut bar
// (gff-client.md 9). The focused pane's host gives them through hab.stats when it has the
// host-stats capability; otherwise gff reads this machine's own, unlabelled.

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/hoststats"
)

// statsEvery is the shortest time between two readings (9.2).
const statsEvery = 2 * time.Second

// sysReading is the figures the bar shows and where they came from.
type sysReading struct {
	stats api.HostStats
	host  string // the host label, or "" for this machine's own figures
	got   time.Time
}

// infoText writes the figures for the bar, the items in the order given: "cpu 12% mem 51%
// disk 61% bat 80% up 3d 4h". An item the host does not give hides: the battery on a machine
// with none, CPU where the system gives no figure.
func infoText(s api.HostStats, items []string) string {
	var parts []string
	for _, it := range items {
		switch it {
		case "cpu":
			if s.CPUPct != nil {
				parts = append(parts, "cpu "+strconv.Itoa(*s.CPUPct)+"%")
			}
		case "mem":
			if s.MemTotal > 0 {
				parts = append(parts, "mem "+strconv.Itoa(int(s.MemUsed*100/s.MemTotal))+"%")
			}
		case "disk":
			if s.DiskTotal > 0 {
				parts = append(parts, "disk "+strconv.Itoa(int(s.DiskUsed*100/s.DiskTotal))+"%")
			}
		case "battery":
			if s.BatteryPct != nil {
				parts = append(parts, "bat "+strconv.Itoa(*s.BatteryPct)+"%")
			}
		case "uptime":
			if s.UptimeS > 0 {
				parts = append(parts, "up "+upText(time.Duration(s.UptimeS)*time.Second))
			}
		}
	}
	return strings.Join(parts, " ")
}

// upText is an uptime: "3d 4h", "5h 12m", "12m".
func upText(d time.Duration) string {
	if d < time.Minute {
		return "0m"
	}
	return durText(d)
}

// hasCap reports whether a welcome lists a capability.
func hasCap(w api.Welcome, c string) bool { return slices.Contains(w.Capabilities, c) }

// LocalStats reads this machine through the shared host resource reader.
type LocalStats = hoststats.Reader
