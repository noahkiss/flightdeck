//go:build testseam

package main

import (
	"os"
	"time"

	"github.com/noahkiss/flightdeck/internal/structure"
)

func init() {
	if v := os.Getenv("GFF_TEST_HAB_PANIC"); v != "" {
		diagStartSeam = func() {
			go func() { panic("test seam: " + v) }()
		}
	}
	// GFF_TEST_HAB_STALL: once gff-hab answers, the store lock is held this long.
	if d, err := time.ParseDuration(os.Getenv("GFF_TEST_HAB_STALL")); err == nil && d > 0 {
		diagStoreSeam = func(s *structure.Store) {
			go func() {
				time.Sleep(time.Second) // past the test's first hello
				s.HoldForTest(d)
			}()
		}
	}
	if p := os.Getenv("GFF_TEST_BUNDLE_HOLD"); p != "" {
		bundleHoldSeam = func(work string) {
			os.WriteFile(p, []byte(work), 0o600)
			time.Sleep(30 * time.Second)
		}
	}
	every, _ := time.ParseDuration(os.Getenv("GFF_TEST_STALL_EVERY"))
	after, _ := time.ParseDuration(os.Getenv("GFF_TEST_STALL_AFTER"))
	stallTiming = func() (time.Duration, time.Duration) { return every, after }
}
