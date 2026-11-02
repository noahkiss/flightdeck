package pairing

import (
	"strconv"
	"unicode/utf8"
)

// MaxPrinted bounds a client string printed to the owner's terminal, in bytes before quoting.
const MaxPrinted = 64

// Printable is the one way a string a client sent reaches the owner's terminal: cut to
// MaxPrinted bytes on a rune boundary, then quoted, so control, format and invalid bytes print
// as escapes and cannot move the cursor, draw or retitle the terminal. A cut string ends in
// "...", outside the quotes.
func Printable(s string) string {
	cut := false
	if len(s) > MaxPrinted {
		n := MaxPrinted
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s, cut = s[:n], true
	}
	q := strconv.Quote(s)
	if cut {
		q += "..."
	}
	return q
}
