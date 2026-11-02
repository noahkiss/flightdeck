package pairing

import (
	"strings"

	"rsc.io/qr"
)

// quiet is the margin around the code, in modules.
const quiet = 2

// QR renders text as a QR code (error correction level M) for a terminal: two modules per
// character cell with half blocks, black on bright white set by ANSI colours so the code reads
// the same on a dark or a light terminal.
func QR(text string) (string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	black := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return x >= 0 && y >= 0 && x < c.Size && y < c.Size && c.Black(x, y)
	}
	n := c.Size + 2*quiet
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		b.WriteString("\x1b[30;107m")
		for x := 0; x < n; x++ {
			top, bottom := black(x, y), black(x, y+1) && y+1 < n
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String(), nil
}
