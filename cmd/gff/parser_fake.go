//go:build !ghostty

package main

import "github.com/noahkiss/flightdeck/internal/vt"

// parser is the parser for each pane. Without the ghostty build tag it is vt.Fake, which keeps
// text and the cursor only: split panes lose colours, styles and modes. Release builds use
// -tags ghostty for libghostty.
func parser() (vt.Parser, string) {
	return vt.Fake{}, "vt.Fake (text only; build with -tags ghostty for libghostty)"
}
