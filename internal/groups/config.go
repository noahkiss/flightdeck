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
	// MaxFindings and BriefFindings bound a verdict and the card (groups.md 6.3, 5.4);
	// ReviewerIdle is how long a reviewer idles with no verdict before the coordinator hears.
	MaxFindings   int           // max_findings
	BriefFindings int           // brief_findings
	ReviewerIdle  time.Duration // reviewer_idle
	// LaneTries is how often the lane starts again on a target that moved before an item goes
	// back (groups.md 7.3 step 6); FixOutputLines is how many lines of a failed step's output
	// the lead gets (7.4).
	LaneTries      int // lane_tries
	FixOutputLines int // fix_output_lines

	// The movable home (groups.md 5.7).
	MoveDrain     time.Duration // move_drain: how long a land past its push may finish before a move
	MoveAnswer    time.Duration // move_answer: how long the old home waits for group.adopt's answer
	MoveRetry     time.Duration // move_retry: a member hab's pause before it calls a pending copy again
	TombstoneKeep time.Duration // tombstone_keep: how long the old home keeps its tombstone
	QueuedMove    time.Duration // queued_move_days: how long a queued move waits on its target hab
}

// Defaults of the [groups] keys.
var DefaultKeys = Keys{KeepEnded: 30 * 24 * time.Hour, InboxMax: 200, InboxDays: 7,
	HandoffNoteWait: 10 * time.Minute, SiblingWait: 10 * time.Minute,
	MaxFindings: 5, BriefFindings: 3, ReviewerIdle: 30 * time.Minute, LaneTries: 3, FixOutputLines: 40,
	MoveDrain: 10 * time.Minute, MoveAnswer: time.Minute, MoveRetry: 2 * time.Second,
	TombstoneKeep: 30 * 24 * time.Hour, QueuedMove: 7 * 24 * time.Hour}

// LoadKeys reads [groups] keep_ended (whole days, 1 to 365), inbox_max (10 to 5000),
// inbox_days (1 to 90), max_findings (1 to 20), brief_findings (1 to max_findings), lane_tries
// (1 to 20), fix_output_lines (0 to 500), tombstone_keep (whole days, 7 to 365) and
// queued_move_days (1 to 90), and the durations handoff_note_wait ("1m" to "2h"), sibling_wait
// ("1m" to "24h"), reviewer_idle ("5m" to "24h"), move_drain ("1m" to "2h"), move_answer ("10s"
// to "10m") and move_retry ("1s" to "60s"). A value outside its range, or of the wrong type, is logged once and the
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
	k.MaxFindings = whole("max_findings", 1, 20, DefaultKeys.MaxFindings)
	k.BriefFindings = whole("brief_findings", 1, int64(k.MaxFindings), min(DefaultKeys.BriefFindings, k.MaxFindings))
	k.LaneTries = whole("lane_tries", 1, 20, DefaultKeys.LaneTries)
	k.FixOutputLines = whole("fix_output_lines", 0, 500, DefaultKeys.FixOutputLines)
	if v, ok := fc.Groups["reviewer_idle"]; ok {
		str, _ := v.(string)
		d, err := time.ParseDuration(str)
		if err != nil || d < 5*time.Minute || d > 24*time.Hour {
			logf("groups: %s: [groups] reviewer_idle %v is not a duration from 5m to 24h; %s runs", filepath.Base(path), fmt.Sprint(v), DefaultKeys.ReviewerIdle)
		} else {
			k.ReviewerIdle = d
		}
	}
	dur := func(key string, lo, hi, def time.Duration) time.Duration {
		v, ok := fc.Groups[key]
		if !ok {
			return def
		}
		str, isStr := v.(string)
		d, err := time.ParseDuration(str)
		if !isStr || err != nil || d < lo || d > hi {
			logf("groups: %s: [groups] %s %v is not a duration from %q to %q; %s runs", filepath.Base(path), key, fmt.Sprint(v), lo.String(), hi.String(), def)
			return def
		}
		return d
	}
	k.MoveDrain = dur("move_drain", time.Minute, 2*time.Hour, DefaultKeys.MoveDrain)
	k.MoveAnswer = dur("move_answer", 10*time.Second, 10*time.Minute, DefaultKeys.MoveAnswer)
	k.MoveRetry = dur("move_retry", time.Second, time.Minute, DefaultKeys.MoveRetry)
	k.TombstoneKeep = time.Duration(whole("tombstone_keep", 7, 365, 30)) * 24 * time.Hour
	k.QueuedMove = time.Duration(whole("queued_move_days", 1, 90, 7)) * 24 * time.Hour
	return k
}
