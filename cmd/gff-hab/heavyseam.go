package main

import "time"

// The heavy-run lock's test seams. Only a build with the testseam tag sets them
// (heavyseam_testseam.go), so end-to-end tests can make the disk low and name a coordinator
// before groups exist.
var (
	// heavyFreeSeam reads a volume instead of hoststats.Volume: $GFF_TEST_HEAVY_FREE names a
	// file that holds the free space in GiB.
	heavyFreeSeam func(path string) (free, total uint64, ok bool)
	// heavyCoordSeam names the coordinators of a low-disk episode: $GFF_TEST_HEAVY_COORDINATOR.
	heavyCoordSeam func() []string
	// heavyDiskEverySeam is how often the disk watch reads: $GFF_TEST_HEAVY_DISK_EVERY.
	heavyDiskEverySeam time.Duration
)
