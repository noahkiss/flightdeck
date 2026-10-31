//go:build !ghostty

package link

import "github.com/noahkiss/flightdeck/internal/vt"

// testParser is the parser the link process uses in tests: vt.Fake without the ghostty tag.
func testParser() vt.Parser { return vt.Fake{} }
