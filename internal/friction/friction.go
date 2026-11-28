// Package friction stores friction notes: a user's own words about a moment where Flightdeck fell
// short, with the session and a tail of the hab's merged logs (docs/specs/diagnostics.md 9).
//
// Notes live in <state>/friction/<time>-<trace>.json, one file each, mode 0600. gff-hab writes
// them for diag.friction; gff-hab friction list and show read them. The text is kept here and
// nowhere else: no log line, health record or tally holds it (10.1).
package friction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/flightdeck/internal/trace"
)

// The limits of diagnostics.md 9.2 and 11.
const (
	Dir        = "friction"
	MaxText    = 2000      // characters of text
	MaxID      = 128       // bytes of a session or pane id
	Keep       = 200       // the newest notes kept
	MaxBytes   = 256 << 10 // one note's file
	TailWindow = 10 * time.Minute
	TailLines  = 500
	TailAge    = 90 * 24 * time.Hour // a note older than this loses its log tail; the text stays
)

// Note is one friction note as stored.
type Note struct {
	T       time.Time `json:"t"`
	Trace   string    `json:"trace"`
	Client  string    `json:"client"`
	Session string    `json:"session,omitempty"`
	Pane    string    `json:"pane,omitempty"`
	// Blocker: the work could not be done in Flightdeck. False: a preference.
	Blocker bool   `json:"blocker,omitempty"`
	Text    string `json:"text"`
	Build   string `json:"build,omitempty"`
	Version string `json:"version,omitempty"`
	Log     []Line `json:"log,omitempty"`
	// LogCut counts the oldest tail lines dropped to keep the file under MaxBytes.
	LogCut int `json:"log_cut,omitempty"`
	// Dropped: the tail went when the note passed TailAge.
	Dropped bool `json:"log_dropped,omitempty"`
}

// Line is one entry of the merged log tail, as gff-hab logs reads it: its time and the process it
// came from. A structured line (diagnostics.md 4.2) is in Line, as the JSON object the file holds,
// so a bundle redacts it by key, and its continuation lines, if any, are in Text. A text line is
// in Text, with its continuation lines. A note written before Line existed holds a structured
// line's JSON in Text.
type Line struct {
	T    time.Time       `json:"t,omitzero"`
	Proc string          `json:"proc"`
	Line json.RawMessage `json:"line,omitempty"`
	Text string          `json:"text,omitempty"`
}

// CheckText returns the text a note stores, or why it is refused: empty, or longer than MaxText
// characters. Surrounding white space is cut.
func CheckText(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("the note has no text")
	case !utf8.ValidString(s):
		return "", errors.New("the note's text is not UTF-8")
	case utf8.RuneCountInString(s) > MaxText:
		return "", fmt.Errorf("the note's text is longer than %d characters", MaxText)
	}
	return s, nil
}

// fileName is <time>-<trace>.json, the time in UTC, so names sort by time.
func fileName(n Note) string {
	return n.T.UTC().Format("20060102T150405.000000Z") + "-" + n.Trace + ".json"
}

// Write stores n under dir, cutting the oldest tail lines until the file fits MaxBytes, then
// prunes the folder (Prune).
func Write(dir string, n Note, now time.Time) error {
	if !trace.Valid(n.Trace) {
		return fmt.Errorf("friction: bad trace %q", n.Trace)
	}
	b, err := encode(&n)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, fileName(n))
	tmp, err := os.CreateTemp(dir, ".note-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	Prune(dir, now)
	return nil
}

// encode marshals n, dropping its oldest tail lines while the result is over MaxBytes.
func encode(n *Note) ([]byte, error) {
	for {
		b, err := marshal(n)
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxBytes || len(n.Log) == 0 {
			return append(b, '\n'), nil
		}
		cut := max(len(n.Log)/8, 1)
		n.Log = n.Log[cut:]
		n.LogCut += cut
	}
}

// marshal writes the note indented, except its log tail: one compact object per line, so the
// indent adds no space inside a structured line and more of the tail fits in MaxBytes.
func marshal(n *Note) ([]byte, error) {
	head := *n
	head.Log = nil
	b, err := json.MarshalIndent(head, "", "  ")
	if err != nil || len(n.Log) == 0 {
		return b, err
	}
	var out bytes.Buffer
	out.Write(bytes.TrimSuffix(b, []byte("\n}")))
	out.WriteString(",\n  \"log\": [")
	for i := range n.Log {
		lb, err := json.Marshal(&n.Log[i])
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString("\n    ")
		out.Write(lb)
	}
	out.WriteString("\n  ]\n}")
	return out.Bytes(), nil
}

// names lists the note files in dir, oldest first.
func names(dir string) ([]string, error) {
	es, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		if n := e.Name(); e.Type().IsRegular() && strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Prune keeps the newest Keep notes and drops the log tail of each note older than TailAge
// (diagnostics.md 9.2). gff-hab runs it after each note and in its hourly sweep.
func Prune(dir string, now time.Time) {
	ns, err := names(dir)
	if err != nil {
		return
	}
	if len(ns) > Keep {
		for _, n := range ns[:len(ns)-Keep] {
			os.Remove(filepath.Join(dir, n))
		}
		ns = ns[len(ns)-Keep:]
	}
	for _, name := range ns {
		path := filepath.Join(dir, name)
		n, err := read(path)
		if err != nil || len(n.Log) == 0 || now.Sub(n.T) <= TailAge {
			continue
		}
		n.Log, n.LogCut, n.Dropped = nil, 0, true
		b, err := encode(&n)
		if err != nil {
			continue
		}
		tmp := path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			if os.Rename(tmp, path) != nil {
				os.Remove(tmp)
			}
		}
	}
}

func read(path string) (Note, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Note{}, err
	}
	var n Note
	if err := json.Unmarshal(b, &n); err != nil {
		return Note{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return n, nil
}

// List returns the notes in dir made at or after since, oldest first. A file that does not
// parse is skipped and counted.
func List(dir string, since time.Time) (notes []Note, bad int, err error) {
	ns, err := names(dir)
	if err != nil {
		return nil, 0, err
	}
	for _, name := range ns {
		n, err := read(filepath.Join(dir, name))
		if err != nil {
			bad++
			continue
		}
		if n.T.Before(since) {
			continue
		}
		notes = append(notes, n)
	}
	return notes, bad, nil
}

// ErrNotFound is Find's answer for a trace no note has.
var ErrNotFound = errors.New("no friction note has that trace")

// Find returns the note with trace t. Case does not matter.
func Find(dir, t string) (Note, error) {
	t = trace.Normalize(t)
	ns, err := names(dir)
	if err != nil {
		return Note{}, err
	}
	for _, name := range ns {
		if strings.HasSuffix(name, "-"+t+".json") {
			return read(filepath.Join(dir, name))
		}
	}
	return Note{}, ErrNotFound
}
