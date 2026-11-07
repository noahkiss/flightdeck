package groups

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Keys are the [groups] keys of the hab's config file that the record reads (groups.md 15.3).
type Keys struct {
	KeepEnded time.Duration // keep_ended: how long an ended or dropped record stays
	InboxMax  int           // inbox_max: reports kept for a coordinator that is not running
	InboxDays int           // inbox_days: how long a report waits
	// Context supervision (groups.md 9.3, 9.4, stage B6).
	HandoffNoteWait time.Duration // handoff_note_wait: how long a fresh session waits for the hand-off note
	SiblingWait     time.Duration // sibling_wait: how long a sibling action waits for a member in use
}

// Defaults of the [groups] keys.
var DefaultKeys = Keys{KeepEnded: 30 * 24 * time.Hour, InboxMax: 200, InboxDays: 7,
	HandoffNoteWait: 10 * time.Minute, SiblingWait: 10 * time.Minute}

// LoadKeys reads [groups] keep_ended (whole days, 1 to 365), inbox_max (10 to 5000) and
// inbox_days (1 to 90), and the durations handoff_note_wait ("1m" to "2h") and sibling_wait ("1m"
// to "24h"). A value outside its range, or of the wrong type, is logged once and the
// default runs; a file that does not parse runs every default. A missing file is no problem.
// Other [groups] keys belong to other stages and are left alone.
func LoadKeys(path string, logf func(string, ...any)) Keys {
	k := DefaultKeys
	if path == "" {
		return k
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return k
	}
	if err != nil {
		logf("groups: %v; the [groups] defaults run", err)
		return k
	}
	var fc struct {
		Groups map[string]any `toml:"groups"`
	}
	if _, err := toml.Decode(string(b), &fc); err != nil {
		logf("groups: %s does not parse (%v); the [groups] defaults run", filepath.Base(path), err)
		return k
	}
	whole := func(key string, lo, hi int64, def int) int {
		v, ok := fc.Groups[key]
		if !ok {
			return def
		}
		n, isInt := v.(int64)
		if !isInt || n < lo || n > hi {
			logf("groups: %s: [groups] %s %v is not a whole number from %d to %d; %d runs", filepath.Base(path), key, fmt.Sprint(v), lo, hi, def)
			return def
		}
		return int(n)
	}
	k.KeepEnded = time.Duration(whole("keep_ended", 1, 365, 30)) * 24 * time.Hour
	k.InboxMax = whole("inbox_max", 10, 5000, DefaultKeys.InboxMax)
	k.InboxDays = whole("inbox_days", 1, 90, DefaultKeys.InboxDays)
	span := func(key string, lo, hi, def time.Duration) time.Duration {
		v, ok := fc.Groups[key]
		if !ok {
			return def
		}
		str, isStr := v.(string)
		d, err := time.ParseDuration(str)
		if !isStr || err != nil || d < lo || d > hi {
			logf("groups: %s: [groups] %s %v is not a duration from %s to %s; %s runs", filepath.Base(path), key, fmt.Sprint(v), lo, hi, def)
			return def
		}
		return d
	}
	k.HandoffNoteWait = span("handoff_note_wait", time.Minute, 2*time.Hour, DefaultKeys.HandoffNoteWait)
	k.SiblingWait = span("sibling_wait", time.Minute, 24*time.Hour, DefaultKeys.SiblingWait)
	return k
}
