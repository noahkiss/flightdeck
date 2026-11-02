package main

import "time"

// roundTickSeam, when set, is the rounds tick instead of rounds.TickEvery. Only a build with the
// testseam tag sets it (roundseam_testseam.go), from $GFF_TEST_ROUND_TICK, so end-to-end tests
// see a slot come due in seconds.
var roundTickSeam time.Duration

func roundTick() time.Duration { return roundTickSeam }
