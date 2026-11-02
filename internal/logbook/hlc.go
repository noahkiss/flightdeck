// Package logbook is Flightdeck's record store (docs/specs/logbook.md): an append-only op log in
// one SQLite file per hab, the records folded from it, and an FTS5 index over their text. The
// fold is a pure function of the ops, so a replica folds the same records whatever order its ops
// arrived in, and Rebuild gives the same tables as folding op by op. Pure Go: the SQLite driver is
// modernc.org/sqlite.
package logbook

import (
	"fmt"
	"strconv"
	"strings"
)

// HLC is a hybrid logical clock stamp (logbook.md 5.1): wall time in ms, a counter, and the
// origin that made it as the tie-break.
type HLC struct {
	Wall    int64  `json:"wall"`
	Counter uint32 `json:"counter"`
	Origin  string `json:"origin"`
}

// String writes the stamp as wall.counter@origin.
func (h HLC) String() string {
	return strconv.FormatInt(h.Wall, 10) + "." + strconv.FormatUint(uint64(h.Counter), 10) + "@" + h.Origin
}

// Compare orders stamps by wall, then counter, then origin as bytes.
func (h HLC) Compare(o HLC) int {
	switch {
	case h.Wall != o.Wall:
		return cmpInt(h.Wall, o.Wall)
	case h.Counter != o.Counter:
		return cmpInt(int64(h.Counter), int64(o.Counter))
	default:
		return strings.Compare(h.Origin, o.Origin)
	}
}

// tick returns the stamp for a local write: wall = max(now, last wall); the counter rises when
// the wall did not move, else it resets to 0.
func tick(last HLC, nowMS int64, origin string) HLC {
	if nowMS > last.Wall {
		return HLC{Wall: nowMS, Origin: origin}
	}
	return HLC{Wall: last.Wall, Counter: last.Counter + 1, Origin: origin}
}

// observe moves the last stamp up to a received one, so the next local write orders after it.
func observe(last HLC, seen HLC) HLC {
	if seen.Wall > last.Wall || (seen.Wall == last.Wall && seen.Counter > last.Counter) {
		return HLC{Wall: seen.Wall, Counter: seen.Counter, Origin: last.Origin}
	}
	return last
}

// OpID names one op everywhere: its origin and its seq there. Its text form is seq@origin.
type OpID struct {
	Origin string
	Seq    uint64
}

func (id OpID) String() string { return strconv.FormatUint(id.Seq, 10) + "@" + id.Origin }

// IsZero reports whether the id is unset.
func (id OpID) IsZero() bool { return id.Origin == "" && id.Seq == 0 }

// MarshalText writes seq@origin.
func (id OpID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText reads seq@origin.
func (id *OpID) UnmarshalText(b []byte) error {
	p, err := ParseOpID(string(b))
	if err != nil {
		return err
	}
	*id = p
	return nil
}

// ParseOpID reads an op id from its text form, seq@origin.
func ParseOpID(s string) (OpID, error) {
	i := strings.IndexByte(s, '@')
	if i <= 0 || i == len(s)-1 {
		return OpID{}, fmt.Errorf("op id %q: want seq@origin", s)
	}
	n, err := strconv.ParseUint(s[:i], 10, 64)
	if err != nil || n == 0 {
		return OpID{}, fmt.Errorf("op id %q: bad seq", s)
	}
	return OpID{Origin: s[i+1:], Seq: n}, nil
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
