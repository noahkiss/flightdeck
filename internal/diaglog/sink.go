package diaglog

import (
	"bytes"
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

// RepeatEvery is how often a line with the same message and component is written; the next one
// written carries the count of those left out (4.5). Legacy lines are not limited: they all
// share one message.
const RepeatEvery = 10 * time.Second

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
	// Level is the floor: lines below it are dropped. A debug line is written also while the
	// debug switch (SetDebug) is on.
	Level Level
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

type sink struct {
	cfg   Config
	level atomic.Int32
	debug atomic.Pointer[func() bool]

	mu     sync.Mutex
	repeat map[string]*repeated
}

type repeated struct {
	last time.Time
	left int64
}

var current atomic.Pointer[sink]

// Setup makes cfg the process's sink. Until it runs, lines go nowhere.
func Setup(cfg Config) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &sink{cfg: cfg, repeat: map[string]*repeated{}}
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
func For(comp string) *Logger { return &Logger{comp: comp} }

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
func (l *Logger) Debug(msg string, attrs ...Attr) { l.log(LevelDebug, msg, attrs) }

// Info writes an info line.
func (l *Logger) Info(msg string, attrs ...Attr) { l.log(LevelInfo, msg, attrs) }

// Warn writes a warn line; it goes to stderr too.
func (l *Logger) Warn(msg string, attrs ...Attr) { l.log(LevelWarn, msg, attrs) }

// Error writes an error line; it goes to stderr too.
func (l *Logger) Error(msg string, attrs ...Attr) { l.log(LevelError, msg, attrs) }

func (l *Logger) log(lvl Level, msg string, attrs []Attr) {
	if l == nil {
		return
	}
	s := current.Load()
	if s == nil || !s.enabled(lvl) {
		return
	}
	left, ok := s.limit(l.comp, msg)
	if !ok {
		return
	}
	s.write(lvl, l.comp, msg, l.trace, attrs, left)
}

// limit applies RepeatEvery to a line of comp and msg: ok is false when the line is left out,
// and left counts the lines left out since the last one written.
func (s *sink) limit(comp, msg string) (left int64, ok bool) {
	now := s.cfg.Now()
	key := comp + "\x00" + msg
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repeat[key]
	if r == nil {
		s.repeat[key] = &repeated{last: now}
		return 0, true
	}
	if d := now.Sub(r.last); d >= 0 && d < RepeatEvery {
		r.left++
		return 0, false
	}
	left, r.left, r.last = r.left, 0, now
	return left, true
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
// is one line at lvl with msg "legacy" and its text under KeyText (4.2).
func Legacy(lvl Level) io.Writer { return legacy{lvl} }

type legacy struct{ lvl Level }

func (w legacy) Write(p []byte) (int, error) {
	s := current.Load()
	if s == nil || !s.enabled(w.lvl) {
		return len(p), nil
	}
	text := strings.TrimSuffix(string(p), "\n")
	s.write(w.lvl, CompLegacy, MsgLegacy, "", []Attr{Text(text)}, 0)
	return len(p), nil
}

// Format is one line as the sink would write it to its file, without writing it: for a line a
// writer makes itself, such as the count of lines it dropped. Nil before Setup.
func Format(lvl Level, comp, msg string, attrs ...Attr) []byte {
	s := current.Load()
	if s == nil {
		return nil
	}
	return s.encode(s.cfg.Now(), lvl, comp, msg, "", attrs, 0)
}
