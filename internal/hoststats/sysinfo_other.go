//go:build !linux && !darwin

package hoststats

import "github.com/noahkiss/flightdeck/api"

// readLocal reads nothing on a system gff has no reader for; the bar shows what is left.
func readLocal(*api.HostStats, *cpuSample, bool) {}

func diskUse(string) (used, total uint64) { return 0, 0 }
