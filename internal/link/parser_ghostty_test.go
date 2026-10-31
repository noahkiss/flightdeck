//go:build ghostty

package link

import (
	"github.com/noahkiss/flightdeck/internal/vt"
	"github.com/noahkiss/flightdeck/internal/vt/ghostty"
)

// testParser is the parser the link process uses in tests: libghostty with the ghostty tag.
func testParser() vt.Parser { return ghostty.Parser{} }
