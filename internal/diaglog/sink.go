package diaglog

import (
	"bytes"
	"container/list"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/internal/trace"
)

// Level is a line's level (diagnostics.md 4.5).
type Level int8

const (
	LevelDebug Level = -4
	LevelInfo  Level = 0
	LevelWarn  Level = 4
	LevelError Level = 8
)

func (l Level) String() string {
	switch {
	case l <= LevelDebug:
		return "debug"
	case l <= LevelInfo:
		return "info"
	case l <= LevelWarn:
		return "warn"
	}
	return "error"
}

// ParseLevel reads debug, info, warn or error. ok is false for any other word.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, true
	case "info":
		return LevelInfo, true
	case "warn", "warning":
		return LevelWarn, true
	case "error":
		return LevelError, true
	}
	return LevelInfo, false
}

// MaxLine is the longest line written, newline included. A longer line has its longest values
// cut and marked with Cut (4.2).
const MaxLine = 8 << 10

// Cut ends a value that was cut to keep its line within MaxLine.
const Cut = "…(cut)"

// RepeatEvery is how often a line with the same component, message and identity is written
// (4.5). A line's identity is the values of its safe, name and path keys, not its text, error or
// numbers: so a line per peer or per pane is kept, and a flood of one line is cut. When the
// window ends, a line with the same identity carries the count left out under suppressed.
// Legacy lines are not limited: they all share one message.
const RepeatEvery = 10 * time.Second

// afterFunc starts the timer that writes a window's count. A test replaces it.
var afterFunc = func(d time.Duration, f func()) { time.AfterFunc(d, f) }

// maxRepeat bounds the limiter's table. When it is full, the oldest window ends early.
const maxRepeat = 1024

// MsgLegacy is the message of a line the legacy shim wrote: an old Logf call's text, under
// KeyText (4.2).
const MsgLegacy = "legacy"

// CompLegacy is the component of a legacy line. The old text names its subsystem, if any, in
// its own words.
const CompLegacy = "legacy"

// timeLayout is a line's t: RFC 3339 in UTC with milliseconds.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// stderrLayout is the time stamp of a stderr copy: the standard log package's, so a line on a
// terminal reads as before.
const stderrLayout = "2006/01/02 15:04:05.000000"

// Config sets up the process's sink.
type Config struct {
	// Proc is the process kind: hab, holder, gff, hook, capcom, speech.
	Proc string
	// Epoch is gff-hab's epoch; empty for other processes.
	Epoch string
	// File gets each line as JSON, one Write per line. Nil: no file.
	File io.Writer
	// Stderr gets a text copy of a warn or error line, and of every line while CopyAll says so.
	Stderr io.Writer
	// CopyAll says every line goes to Stderr too: the process runs by hand, or its file is not
	// open yet. Nil: never.
	CopyAll func() bool
	// Prefix starts each Stderr line, as the standard log package's prefix: "gff-hab: ".
	Prefix string
	// Level is the floor: typed lines below it are dropped. A debug line is written also while
	// the debug switch (SetDebug) is on. Legacy lines at info or above are written whatever the
	// floor (Legacy).
	Level Level
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

type sink struct {
	cfg   Config
	level atomic.Int32
	debug atomic.Pointer[func() bool]

	mu     sync.Mutex
	repeat map[string]*list.Element // of *repeated, oldest window first in order
	order  list.List
}

// repeated is one open window of the limiter: the first line written, and the count left out.
type repeated struct {
	key   string
	first time.Time
	left  int64
	lvl   Level
	comp  string
	msg   string
	ids   []Attr
}

var current atomic.Pointer[sink]

// Setup makes cfg the process's sink. Until it runs, lines go nowhere.
func Setup(cfg Config) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &sink{cfg: cfg, repeat: map[string]*list.Element{}}
	s.level.Store(int32(cfg.Level))
	if old := current.Load(); old != nil {
		s.debug.Store(old.debug.Load())
	}
	current.Store(s)
}

// Reset drops the sink: lines go nowhere. For tests.
func Reset() { current.Store(nil) }

// SetLevel changes the floor, as the hab's config gives it ([diagnostics] log_level).
func SetLevel(l Level) {
	if s := current.Load(); s != nil {
		s.level.Store(int32(l))
	}
}

// SetDebug sets the debug switch: while on returns true, debug lines are written whatever the
// floor (4.5).
func SetDebug(on func() bool) {
	if s := current.Load(); s != nil {
		s.debug.Store(&on)
	}
}

// Enabled says whether a line at l would be written.
func Enabled(l Level) bool {
	s := current.Load()
	return s != nil && s.enabled(l)
}

func (s *sink) enabled(l Level) bool {
	if l >= Level(s.level.Load()) {
		return true
	}
	if l == LevelDebug {
		if f := s.debug.Load(); f != nil && (*f)() {
			return true
		}
	}
	return false
}

// Logger writes the lines of one component. The zero Logger and a nil one write nothing.
type Logger struct {
	comp  string
	trace string
}

// For returns the logger of a component: link, control, peers, speech, and so on. comp must
// be a constant word.
func For(comp constant) *Logger { return &Logger{comp: string(comp)} }

// Ctx returns the logger with the trace of ctx, when it carries a valid one (5.2): each line it
// writes names the action.
func (l *Logger) Ctx(ctx context.Context) *Logger {
	if l == nil {
		return nil
	}
	t := trace.From(ctx)
	if !trace.Valid(t) {
		return l
	}
	return &Logger{comp: l.comp, trace: t}
}

// Debug writes a debug line. msg is a constant phrase; values go in attrs.
func (l *Logger) Debug(msg constant, attrs ...Attr) { l.log(LevelDebug, string(msg), attrs) }

// Info writes an info line.
func (l *Logger) Info(msg constant, attrs ...Attr) { l.log(LevelInfo, string(msg), attrs) }

// Warn writes a warn line; it goes to stderr too.
func (l *Logger) Warn(msg constant, attrs ...Attr) { l.log(LevelWarn, string(msg), attrs) }

// Error writes an error line; it goes to stderr too.
func (l *Logger) Error(msg constant, attrs ...Attr) { l.log(LevelError, string(msg), attrs) }

func (l *Logger) log(lvl Level, msg string, attrs []Attr) {
	if l == nil {
		return
	}
	s := current.Load()
	if s == nil || !s.enabled(lvl) {
		return
	}
	if !s.limit(lvl, l.comp, msg, attrs) {
		return
	}
	s.write(lvl, l.comp, msg, l.trace, attrs, 0)
}

// identity is the part of attrs that tells one entity's line from another's: each string value
// of class safe, name or path, but no trace. Text, errors and numbers vary from line to line of
// one flood, so they are left out.
func identity(attrs []Attr) []Attr {
	var ids []Attr
	for _, a := range attrs {
		if a.key == "" || a.isNum || a.class == ClassText || a.key == KeyTrace {
			continue
		}
		ids = append(ids, a)
	}
	return ids
}

// limit applies RepeatEvery to a line: false when the line is left out. A line opens a window
// for its component, message and identity; the lines that follow in the window are counted, and
// when it ends the count is written as a line of its own (sweep).
func (s *sink) limit(lvl Level, comp, msg string, attrs []Attr) bool {
	now := s.cfg.Now()
	ids := identity(attrs)
	var k strings.Builder
	k.WriteString(comp)
	k.WriteByte(0)
	k.WriteString(msg)
	for _, a := range ids {
		k.WriteByte(0)
		k.WriteString(a.key)
		k.WriteByte('=')
		k.WriteString(a.str)
	}
	key := k.String()
	s.mu.Lock()
	ended := s.endedLocked(now)
	e, open := s.repeat[key]
	if open {
		r := e.Value.(*repeated)
		r.left++
		if r.left == 1 {
			wait := RepeatEvery - now.Sub(r.first)
			afterFunc(max(wait, 0)+time.Millisecond, s.sweep)
		}
	} else {
		if s.order.Len() >= maxRepeat {
			ended = append(ended, s.dropLocked(s.order.Front()))
		}
		s.repeat[key] = s.order.PushBack(&repeated{key: key, first: now, lvl: lvl, comp: comp, msg: msg, ids: ids})
	}
	s.mu.Unlock()
	s.writeEnded(ended)
	return !open
}

// endedLocked removes the windows that ended by now, oldest first, and returns them. A clock
// that went back ends a window too.
func (s *sink) endedLocked(now time.Time) []*repeated {
	var ended []*repeated
	for e := s.order.Front(); e != nil; e = s.order.Front() {
		r := e.Value.(*repeated)
		if d := now.Sub(r.first); d >= 0 && d < RepeatEvery {
			break
		}
		ended = append(ended, s.dropLocked(e))
	}
	return ended
}

func (s *sink) dropLocked(e *list.Element) *repeated {
	r := s.order.Remove(e).(*repeated)
	delete(s.repeat, r.key)
	return r
}

// writeEnded writes, for each ended window that left lines out, one line with its identity and
// the count under suppressed.
func (s *sink) writeEnded(ended []*repeated) {
	for _, r := range ended {
		if r.left > 0 {
			s.write(r.lvl, r.comp, r.msg, "", r.ids, r.left)
		}
	}
}

// sweep ends the windows whose time is up. A timer calls it when a window has a count to write,
// so the count is written even when no other line follows.
func (s *sink) sweep() {
	s.mu.Lock()
	ended := s.endedLocked(s.cfg.Now())
	s.mu.Unlock()
	s.writeEnded(ended)
}

func (s *sink) write(lvl Level, comp, msg, tr string, attrs []Attr, left int64) {
	t := s.cfg.Now()
	if s.cfg.File != nil {
		s.cfg.File.Write(s.encode(t, lvl, comp, msg, tr, attrs, left))
	}
	if s.cfg.Stderr != nil && (lvl >= LevelWarn || (s.cfg.CopyAll != nil && s.cfg.CopyAll())) {
		var b strings.Builder
		b.WriteString(s.cfg.Prefix)
		b.WriteString(t.UTC().Format(stderrLayout))
		b.WriteByte(' ')
		l := Line{Lvl: lvl.String(), Comp: comp, Msg: msg, Trace: tr}
		for _, a := range attrs {
			if a.key != "" {
				l.Attrs = append(l.Attrs, KV{Key: a.key, Val: a.value(), Num: a.isNum})
			}
		}
		if left > 0 {
			l.Attrs = append(l.Attrs, KV{Key: KeySuppressed, Val: itoa(left), Num: true})
		}
		b.WriteString(l.Text())
		b.WriteByte('\n')
		s.cfg.Stderr.Write([]byte(b.String()))
	}
}

func (a Attr) value() string {
	if a.isNum {
		return itoa(a.num)
	}
	return a.str
}

func itoa(n int64) string {
	var b [24]byte
	i := len(b)
	neg := n < 0
	u := uint64(n)
	if neg {
		u = uint64(-n)
	}
	for {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
		if u == 0 {
			break
		}
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

var handlerOptions = &slog.HandlerOptions{
	Level: slog.LevelDebug,
	ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) > 0 {
			return a
		}
		switch a.Key {
		case slog.TimeKey:
			return slog.String(KeyT, a.Value.Time().UTC().Format(timeLayout))
		case slog.LevelKey:
			return slog.String(KeyLvl, Level(a.Value.Any().(slog.Level)).String())
		}
		return a
	},
}

// encode is one JSON line, newline included, at most MaxLine bytes: while it is longer, every
// string value longer than a cap is cut at the cap, and the cap halves.
func (s *sink) encode(t time.Time, lvl Level, comp, msg, tr string, attrs []Attr, left int64) []byte {
	limit := MaxLine
	for range 10 {
		b := s.marshal(t, lvl, comp, msg, tr, attrs, left, limit)
		if len(b) <= MaxLine {
			return b
		}
		limit /= 2
	}
	return s.marshal(t, lvl, comp, msg, tr, []Attr{Text(Cut)}, left, MaxLine)
}

func (s *sink) marshal(t time.Time, lvl Level, comp, msg, tr string, attrs []Attr, left int64, limit int) []byte {
	var buf bytes.Buffer
	r := slog.NewRecord(t, slog.Level(lvl), msg, 0)
	r.AddAttrs(slog.String(KeyProc, s.cfg.Proc))
	if s.cfg.Epoch != "" {
		r.AddAttrs(slog.String(KeyEpoch, s.cfg.Epoch))
	}
	r.AddAttrs(slog.String(KeyComp, comp))
	if tr != "" {
		r.AddAttrs(slog.String(KeyTrace, tr))
	}
	for _, a := range attrs {
		switch {
		case a.key == "":
		case a.isNum:
			r.AddAttrs(slog.Int64(a.key, a.num))
		default:
			r.AddAttrs(slog.String(a.key, cut(a.str, limit)))
		}
	}
	if left > 0 {
		r.AddAttrs(slog.Int64(KeySuppressed, left))
	}
	slog.NewJSONHandler(&buf, handlerOptions).Handle(context.Background(), r)
	return buf.Bytes()
}

// cut shortens s to at most limit bytes on a rune boundary, marked with Cut.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	n := max(limit-len(Cut), 0)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + Cut
}

// Legacy returns the writer for an old log.Logger, made with no prefix and no flags: each Write
// is one line at lvl with msg "legacy" and its text under KeyText (4.2). A legacy line at info or
// above is written whatever the floor: the old lines are not typed at their level yet (4.5). A
// legacy debug line keeps the debug rule.
func Legacy(lvl Level) io.Writer { return legacy{lvl} }

type legacy struct{ lvl Level }

func (w legacy) Write(p []byte) (int, error) {
	s := current.Load()
	if s == nil || (w.lvl < LevelInfo && !s.enabled(w.lvl)) {
		return len(p), nil
	}
	text := strings.TrimSuffix(string(p), "\n")
	s.write(w.lvl, CompLegacy, MsgLegacy, "", []Attr{Text(text)}, 0)
	return len(p), nil
}

// Format is one line as the sink would write it to its file, without writing it: for a line a
// writer makes itself, such as the count of lines it dropped. Nil before Setup.
func Format(lvl Level, comp, msg constant, attrs ...Attr) []byte {
	s := current.Load()
	if s == nil {
		return nil
	}
	return s.encode(s.cfg.Now(), lvl, string(comp), string(msg), "", attrs, 0)
}
