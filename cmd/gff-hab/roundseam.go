package main

import "time"

// roundTickSeam, when set, is the rounds tick instead of rounds.TickEvery. Only a build with the
// testseam tag sets it (roundseam_testseam.go), from $GFF_TEST_ROUND_TICK, so end-to-end tests
// see a slot come due in seconds.
var roundTickSeam time.Duration

func roundTick() time.Duration { return roundTickSeam }

// roundReadySeam, when set, is the readiness limit of a harness run instead of roundReady.
// Only a build with the testseam tag sets it, from $GFF_TEST_ROUND_READY.
var roundReadySeam time.Duration

// roundQuietSeam is how long after its start gff-hab counts every pane as in use (rounds.md
// 6.1): roundInputWindow, unless a build with the testseam tag sets it from
// $GFF_TEST_ROUND_QUIET.
var roundQuietSeam = roundInputWindow

// superviseTickSeam, when set, is context supervision's reading interval instead of
// superviseTick. Only a build with the testseam tag sets it, from $GFF_TEST_SUPERVISE_TICK.
var superviseTickSeam time.Duration

// roundGroupsSeam, when set, makes this hab a fake far side of the group mode (rounds.md 5.2)
// for end-to-end tests: a JSON file of the catalog's groups section (roundfake.go). Only a build
// with the testseam tag sets it, from $GFF_TEST_GROUPS.
var roundGroupsSeam string
