//go:build testseam

package main

import (
	"os"
	"time"
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
}
