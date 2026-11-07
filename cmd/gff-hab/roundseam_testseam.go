//go:build testseam

package main

import (
	"os"
	"time"

	"github.com/noahkiss/flightdeck/internal/rounds"
)

func init() {
	if d, err := time.ParseDuration(os.Getenv("GFF_TEST_ROUND_TICK")); err == nil && d > 0 {
		roundTickSeam = d
	}
	if d, err := time.ParseDuration(os.Getenv("GFF_TEST_ROUND_READY")); err == nil && d > 0 {
		roundReadySeam = d
	}
	if d, err := time.ParseDuration(os.Getenv("GFF_TEST_ROUND_QUIET")); err == nil && d >= 0 {
		roundQuietSeam = d
	}
	// The lower bounds of debounce and cooldown, so a test's batch runs in a second.
	if d, err := time.ParseDuration(os.Getenv("GFF_TEST_ROUND_MIN_BATCH")); err == nil && d > 0 {
		rounds.MinDebounce, rounds.MinCooldown = d, d
	}
	if d, err := time.ParseDuration(os.Getenv("GFF_TEST_SUPERVISE_TICK")); err == nil && d > 0 {
		superviseTickSeam = d
	}
	roundCapcomSeam = os.Getenv("GFF_TEST_CAPCOM_SERVICE")
	roundGroupsSeam = os.Getenv("GFF_TEST_GROUPS")
}
