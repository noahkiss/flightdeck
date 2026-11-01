//go:build race

package gffclient

import "os"

// Under the race detector, TestMain builds gff-hab and gff with -race too, so a race in a
// binary fails the test that ran it. GORACE drops the race runtime's 1 s sleep at exit.
func init() {
	buildFlags = append(buildFlags, "-race")
	os.Setenv("GORACE", "atexit_sleep_ms=0")
}
