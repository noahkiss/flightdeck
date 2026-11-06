//go:build testseam

package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/internal/heavy"
)

func init() {
	if f := os.Getenv("GFF_TEST_HEAVY_FREE"); f != "" {
		heavyFreeSeam = func(string) (uint64, uint64, bool) {
			b, err := os.ReadFile(f)
			if err != nil {
				return 0, 0, false
			}
			g, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
			if err != nil || g < 0 {
				return 0, 0, false
			}
			return uint64(g * float64(heavy.GiB)), 1000 * heavy.GiB, true
		}
	}
	if s := os.Getenv("GFF_TEST_HEAVY_COORDINATOR"); s != "" {
		heavyCoordSeam = func() []string { return []string{s} }
	}
	if d, err := time.ParseDuration(os.Getenv("GFF_TEST_HEAVY_DISK_EVERY")); err == nil && d > 0 {
		heavyDiskEverySeam = d
	}
}
