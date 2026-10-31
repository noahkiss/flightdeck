//go:build ghostty

package main

import (
	"github.com/noahkiss/flightdeck/internal/vt"
	"github.com/noahkiss/flightdeck/internal/vt/ghostty"
)

// parser is the per-pane terminal parser: libghostty-vt, chosen by the ghostty build tag.
func parser() (vt.Parser, string) { return ghostty.Parser{}, "libghostty" }
