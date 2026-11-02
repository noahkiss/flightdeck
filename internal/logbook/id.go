package logbook

import (
	"crypto/rand"
	"regexp"
	"strconv"
)

// idAlphabet is lower-case base 32 without the look-alike letters i, l, o and u.
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// NewID mints a record id: the type, a dash and 8 characters of idAlphabet, 40 random bits
// (logbook.md 2.3). No coordination is needed.
func NewID(typ string) string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("logbook: no randomness: " + err.Error())
	}
	v := uint64(b[0])<<32 | uint64(b[1])<<24 | uint64(b[2])<<16 | uint64(b[3])<<8 | uint64(b[4])
	if p, ok := idPrefix[typ]; ok {
		typ = p
	}
	out := make([]byte, 0, len(typ)+9)
	out = append(out, typ...)
	out = append(out, '-')
	for i := 7; i >= 0; i-- {
		out = append(out, idAlphabet[(v>>(5*uint(i)))&31])
	}
	return string(out)
}

// importedID is the shape an import may carry verbatim (2.3). Minted ids match it too.
var importedID = regexp.MustCompile(`^[a-z]+-[a-z0-9]{3,16}$`)

// ValidID reports whether s may be a new record's id: a local create's or one a peer's create
// claims. It has exactly one dash.
func ValidID(s string) bool { return importedID.MatchString(s) }

// collisionID is the id a record gets when an earlier create holds its id: base-2, base-3 and so
// on, by create order (2.3). It has two dashes, so no create can claim it (ValidID), and the
// folded ids of all records stay unique.
func collisionID(base string, index int) string {
	if index == 0 {
		return base
	}
	return base + "-" + strconv.Itoa(index+1)
}
