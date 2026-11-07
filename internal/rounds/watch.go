package rounds

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Watchers (rounds.md 4.4): a watch trigger names a long-lived program that gff-hab keeps
// running in a pane. Each JSON line it prints is an event, which joins the round's pending batch
// as a hab event does (4.5). This file holds the trigger's definition, the event line rules, the
// dedup keys and the cursor, the watcher limits of 4.7, and the restart backoff. gff-hab runs the
// program and its pane (cmd/gff-hab/roundwatch.go).

// TriggerWatch is a watch trigger.
const TriggerWatch TriggerKind = "watch"

// Batches reports whether a trigger kind makes event runs from a pending batch: an event or a
// watch trigger (rounds.md 4.5).
func (k TriggerKind) Batches() bool { return k == TriggerEvent || k == TriggerWatch }

// Watch is a watch trigger's watcher (rounds.md 4.4).
type Watch struct {
	// Argv is the program and its arguments. No shell.
	Argv []string `json:"argv"`
	// Cwd is an absolute folder; empty: <state>/rounds/watchers/<round id>/.
	Cwd string `json:"cwd,omitempty"`
	// Kinds are the event kinds that count; empty: every kind.
	Kinds []string `json:"kinds,omitempty"`
}

// MaxWatchLine is the longest event line, a fixed ceiling (rounds.md 4.7): the wrapper's read
// buffer, and a round message's bound.
const MaxWatchLine = 64 << 10

// Bounds of a watch trigger's own fields.
const (
	maxWatchArgs  = 256
	maxWatchArg   = 4 << 10
	maxWatchKinds = 32
	maxKindLen    = 64
	maxKeyLen     = 512
)

func checkWatch(w *Watch) error {
	if w == nil || len(w.Argv) == 0 {
		return errors.New("watch: argv is required: the watcher program and its arguments")
	}
	if w.Argv[0] == "" {
		return errors.New("watch: the program name is empty")
	}
	if len(w.Argv) > maxWatchArgs {
		return fmt.Errorf("watch: %d arguments; the limit is %d", len(w.Argv), maxWatchArgs)
	}
	for _, a := range w.Argv {
		if len(a) > maxWatchArg || !utf8.ValidString(a) || strings.ContainsRune(a, 0) {
			return fmt.Errorf("watch: an argument is over %d bytes, not UTF-8, or holds a NUL", maxWatchArg)
		}
	}
	if w.Cwd != "" && !filepath.IsAbs(w.Cwd) {
		return fmt.Errorf("watch: cwd %q is not an absolute path", w.Cwd)
	}
	if len(w.Kinds) > maxWatchKinds {
		return fmt.Errorf("watch: %d kinds; the limit is %d", len(w.Kinds), maxWatchKinds)
	}
	for _, k := range w.Kinds {
		if !ValidKind(k) {
			return fmt.Errorf("watch: kind %q: use up to %d letters, digits, ., - and _", k, maxKindLen)
		}
	}
	return nil
}

// ValidKind reports whether k is an event kind word.
func ValidKind(k string) bool {
	if k == "" || len(k) > maxKindLen {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func equalWatch(a, b *Watch) bool {
	if a == nil || b == nil {
		return a == b
	}
	return slices.Equal(a.Argv, b.Argv) && a.Cwd == b.Cwd && slices.Equal(a.Kinds, b.Kinds)
}

// WatchLimits are the watcher limits of rounds.md 4.7. DefaultWatchLimits gives every default;
// a caller changes the fields its config sets.
type WatchLimits struct {
	MaxWatchers  int
	BackoffMax   time.Duration
	BackoffReset time.Duration
	PauseAfter   int
	QuickExit    time.Duration
	BufferLines  int
	BufferBytes  int
	Rate         int // events a minute
	Burst        int
	TitleMax     int // characters
	KeyWindow    time.Duration
	KeysMax      int
	DataMax      int // bytes of data, encoded
	CursorMax    int // bytes
	// SecretsTimeout bounds a secret set's command.
	SecretsTimeout time.Duration
}

// BackoffFirst is the backoff's first step, a fixed value (rounds.md 4.7).
const BackoffFirst = time.Second

// DefaultWatchLimits returns the defaults of rounds.md 4.7.
func DefaultWatchLimits() WatchLimits {
	return WatchLimits{
		MaxWatchers: 16, BackoffMax: 5 * time.Minute, BackoffReset: 10 * time.Minute,
		PauseAfter: 5, QuickExit: time.Minute, BufferLines: 1000, BufferBytes: 1 << 20,
		Rate: 60, Burst: 60, TitleMax: 200, KeyWindow: 24 * time.Hour, KeysMax: 1000,
		DataMax: 16 << 10, CursorMax: 1 << 10, SecretsTimeout: 30 * time.Second,
	}
}

// WatchLine is one event line read: an event, a cursor, or a drop with its reason. A quiet drop
// (a kind the round does not count, a repeated key) is not shown or counted.
type WatchLine struct {
	Event  *Event
	Key    string
	Cursor *string
	Drop   string
	Quiet  bool
}

// ParseWatchLine reads one line a watcher printed (rounds.md 4.4): a JSON object with a title,
// and kind, key, level, at and data, or a cursor line. now is the default of at. It checks the
// line's own bounds; kinds and keys are the round's (Scheduler.WatchLines).
func ParseWatchLine(b []byte, now time.Time, lim WatchLimits) WatchLine {
	drop := func(format string, a ...any) WatchLine { return WatchLine{Drop: fmt.Sprintf(format, a...)} }
	if len(b) > MaxWatchLine {
		return drop("line over %d KiB", MaxWatchLine>>10)
	}
	if !utf8.Valid(b) {
		return drop("not UTF-8")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return drop("not a JSON object")
	}
	str := func(field string) (string, bool, error) {
		raw, ok := m[field]
		if !ok || string(raw) == "null" {
			return "", false, nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", true, fmt.Errorf("%s is not a string", field)
		}
		return s, true, nil
	}
	if c, ok, err := str("cursor"); ok || err != nil {
		switch {
		case err != nil:
			return drop("%v", err)
		case len(c) > lim.CursorMax:
			return drop("cursor over %d bytes", lim.CursorMax)
		case strings.ContainsRune(c, 0):
			// The cursor goes into the environment as GFF_WATCH_CURSOR, which cannot hold a NUL.
			return drop("cursor holds a NUL")
		}
		return WatchLine{Cursor: &c}
	}
	title, _, err := str("title")
	if err != nil {
		return drop("%v", err)
	}
	switch {
	case strings.TrimSpace(title) == "":
		return drop("no title")
	case strings.ContainsAny(title, "\r\n"):
		return drop("title is more than one line")
	case utf8.RuneCountInString(title) > lim.TitleMax:
		return drop("title over %d characters", lim.TitleMax)
	}
	ev := Event{At: now.UTC(), Title: title}
	kind, _, err := str("kind")
	if err != nil {
		return drop("%v", err)
	}
	if kind != "" && !ValidKind(kind) {
		return drop("kind %q: use up to %d letters, digits, ., - and _", kind, maxKindLen)
	}
	ev.Kind = kind
	key, _, err := str("key")
	if err != nil {
		return drop("%v", err)
	}
	if len(key) > maxKeyLen {
		return drop("key over %d bytes", maxKeyLen)
	}
	level, _, err := str("level")
	if err != nil {
		return drop("%v", err)
	}
	if level != "" && LevelRank(level) < 0 {
		return drop("level %q: want %s", level, strings.Join(NoticeLevels, ", "))
	}
	ev.Level = level
	at, _, err := str("at")
	if err != nil {
		return drop("%v", err)
	}
	if at != "" {
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return drop("at %q is not an RFC 3339 time", at)
		}
		ev.At = t.UTC()
	}
	if raw, ok := m["data"]; ok && string(raw) != "null" {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return drop("data is not a JSON object")
		}
		compact, err := json.Marshal(obj)
		if err != nil {
			return drop("data: %v", err)
		}
		if len(compact) > lim.DataMax {
			return drop("data over %d bytes", lim.DataMax)
		}
		ev.Data = compact
	}
	return WatchLine{Event: &ev, Key: key}
}

// SeenKey is a dedup key a watch round saw, and when.
type SeenKey struct {
	Key string    `json:"key"`
	At  time.Time `json:"at"`
}

// WatchState is a watch round's saved watcher state: the latest cursor, passed as
// GFF_WATCH_CURSOR at each start, and the dedup keys of the key window (rounds.md 4.4). The
// scheduler saves it at most once per state_save_every, and in the write that makes a run.
type WatchState struct {
	Cursor string    `json:"cursor,omitempty"`
	Keys   []SeenKey `json:"keys,omitempty"`
}

// seen reports whether key was seen within window before now, after it drops the keys past
// the window; when not, it records key at now and keeps at most max keys, the newest.
func (w *WatchState) seen(key string, now time.Time, window time.Duration, max int) bool {
	keep := w.Keys[:0]
	for _, k := range w.Keys {
		if now.Sub(k.At) < window {
			keep = append(keep, k)
		}
	}
	w.Keys = keep
	for _, k := range w.Keys {
		if k.Key == key {
			return true
		}
	}
	if max <= 0 {
		w.Keys = nil
		return false
	}
	w.Keys = append(w.Keys, SeenKey{Key: key, At: now})
	if over := len(w.Keys) - max; over > 0 {
		w.Keys = append([]SeenKey(nil), w.Keys[over:]...)
	}
	return false
}

// trim applies lowered limits to a stored state (rounds.md 4.7): a cursor over the bound goes,
// and the keys keep the newest max.
func (w *WatchState) trim(lim WatchLimits) {
	if len(w.Cursor) > lim.CursorMax {
		w.Cursor = ""
	}
	if over := len(w.Keys) - max(lim.KeysMax, 0); over > 0 {
		w.Keys = append([]SeenKey(nil), w.Keys[over:]...)
	}
}

func (w *WatchState) empty() bool { return w == nil || (w.Cursor == "" && len(w.Keys) == 0) }

func cloneWatchState(w *WatchState) *WatchState {
	if w == nil {
		return nil
	}
	c := WatchState{Cursor: w.Cursor, Keys: append([]SeenKey(nil), w.Keys...)}
	return &c
}

// Backoff is a watcher's restart backoff and its count of quick exits (rounds.md 4.4). The zero
// value is a watcher that never exited.
type Backoff struct {
	// Next is the delay of the next restart; zero means BackoffFirst.
	Next time.Duration
	// Quick counts the exits in a row that came within QuickExit of their start.
	Quick int
}

// Exit takes one exit of a watcher that started at start and exited at end. own says gff-hab
// hung it up (a wake, an edit, a pause): it counts for nothing and restarts at once. It returns
// the delay before the next start, and pause when the exits in a row reached PauseAfter.
func (b *Backoff) Exit(start, end time.Time, own bool, lim WatchLimits) (delay time.Duration, pause bool) {
	if own {
		return 0, false
	}
	ran := end.Sub(start)
	if ran >= lim.BackoffReset {
		b.Next = 0
	}
	if ran < lim.QuickExit {
		b.Quick++
	} else {
		b.Quick = 0
	}
	delay = b.Next
	if delay <= 0 {
		delay = BackoffFirst
	}
	delay = min(delay, max(lim.BackoffMax, BackoffFirst))
	b.Next = min(2*delay, max(lim.BackoffMax, BackoffFirst))
	return delay, lim.PauseAfter > 0 && b.Quick >= lim.PauseAfter
}

// Reset clears the backoff and the exit count: an unpause.
func (b *Backoff) Reset() { *b = Backoff{} }
