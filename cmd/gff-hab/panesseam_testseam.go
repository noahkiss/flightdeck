//go:build testseam

package main

import (
	"os"
	"time"
)

// The lease of a forwarded pane.wait (hab-logic-api.md 3.11): $GFF_TEST_PANE_LEASE shortens it,
// so a test sees a far wait end soon after its near caller went away.
func init() {
	if d, err := time.ParseDuration(os.Getenv("GFF_TEST_PANE_LEASE")); err == nil && d > 0 {
		paneLease = d
	}
}
