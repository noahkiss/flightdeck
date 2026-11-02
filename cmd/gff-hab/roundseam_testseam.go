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
}
