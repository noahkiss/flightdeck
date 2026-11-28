package main

import (
	"time"

	"github.com/noahkiss/flightdeck/internal/structure"
)

// diagStartSeam runs once gff-hab has logged its start line. Only a build with the testseam tag
// sets it (diagseam_testseam.go): $GFF_TEST_HAB_PANIC makes gff-hab panic there, for the crash
// record test.
var diagStartSeam = func() {}

// diagStoreSeam runs once the stall watchdog runs. A testseam build with $GFF_TEST_HAB_STALL
// holds the store lock that long, for the stall watchdog test (4.7).
var diagStoreSeam = func(*structure.Store) {}

// bundleHoldSeam runs once gff-hab bundle made its hidden work folder, before it writes a file. A
// testseam build with $GFF_TEST_BUNDLE_HOLD writes the folder's path to that file and waits, so
// a test can send a signal mid-run.
var bundleHoldSeam = func(work string) {}

// stallTiming is the watchdog's check interval and threshold; zero for the defaults of 4.7. A
// testseam build reads $GFF_TEST_STALL_EVERY and $GFF_TEST_STALL_AFTER.
var stallTiming = func() (every, after time.Duration) { return 0, 0 }
