//go:build race

package main

import "os"

// Under the race detector, TestMain builds gff-hab with -race too, so a race in the binary
// fails the test that ran it.
// GORACE drops the race runtime's default 1 s sleep at exit, which each child process would pay.
func init() {
	raceBuild = true
	buildFlags = append(buildFlags, "-race")
	os.Setenv("GORACE", "atexit_sleep_ms=0")
}
