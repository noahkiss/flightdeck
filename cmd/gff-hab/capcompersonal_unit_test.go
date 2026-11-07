package main

import (
	"strings"
	"testing"
)

// TestScrubCoords: a tool line keeps no position (capcom-service.md 8.1), whatever the
// precision or the separator, and keeps plain numbers.
func TestScrubCoords(t *testing.T) {
	for _, in := range []string{"weather 52.52, 13.40", "lat 52.52 lon 13.40", "52.5,13.4", "near 52.52001,13.40495", "WebSearch: weather 52.520,13.405"} {
		out := scrubCoords(in)
		if strings.ContainsAny(strings.ReplaceAll(out, "(a position)", ""), "0123456789") {
			t.Errorf("%q kept a number: %q", in, out)
		}
	}
	for _, keep := range []string{"places_search: vegetarian", "a 2.5 hour walk", "gff 1.2"} {
		if out := scrubCoords(keep); out != keep {
			t.Errorf("%q changed to %q", keep, out)
		}
	}
}
