package flightrules

import (
	"path/filepath"

	"github.com/noahkiss/flightdeck/internal/instructions"
)

// Rendered returns a Ledger.Configured function for one harness: a file at the target the
// instruction renderer last wrote, whose header digest matches the render record, names the
// configured sources the render took text from (spec 11.1). Any other file names none. The
// record is read on each call, so a new render counts at once.
func Rendered(stateDir string, h instructions.Harness) func(path, text string) []string {
	return func(path, text string) []string {
		dig := instructions.HeaderDigest(text)
		if dig == "" {
			return nil
		}
		r, ok, err := instructions.ReadRendered(stateDir, h)
		if err != nil || !ok || r.Digest != dig || !samePath(path, r.Target) {
			return nil
		}
		return append([]string(nil), r.Sources...)
	}
}

// samePath compares two paths as written, then through their links.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}
