// Package health keeps the run markers and health records of docs/specs/diagnostics.md 6: a
// process writes a marker when it starts and marks it clean when it exits cleanly; the next
// start reads the previous marker and records a crash or an unclean end.
package health

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kinds of health record (6.3).
const (
	KindCrash     = "crash"     // the process ended with a panic or fatal error
	KindUnclean   = "unclean"   // it ended with no clean exit and no crash output: a signal, a kill, a reboot
	KindRecovered = "recovered" // a panic was recovered and the process went on
)

// CauseKilled is the cause of an unclean end.
const CauseKilled = "signal or kill"

// CauseHolderLost is the cause of a record gff-hab writes for a holder whose connection ended
// with an error.
const CauseHolderLost = "holder lost"

// Keep is how many records health.jsonl keeps (6.3, 11).
const Keep = 500

// CauseMax is the most bytes of a panic value a cause keeps (6.3, 12).
const CauseMax = 120

// Marker is a process's run marker (6.1).
type Marker struct {
	Proc     string    `json:"proc"`
	Instance string    `json:"instance,omitempty"`
	Run      string    `json:"run"` // gff-hab: its epoch
	Pid      int       `json:"pid"`
	Build    string    `json:"build"`
	Version  string    `json:"version"`
	Started  time.Time `json:"started"`
	LastSeen time.Time `json:"last_seen"`
	Clean    bool      `json:"clean"`
	// CrashFile is the name of the file under crash/ that gets the run's crash output.
	CrashFile string `json:"crash_file,omitempty"`
	// Hook is gff-hab's hook endpoint: "up", or "down" once it stopped (4.6).
	Hook string `json:"hook,omitempty"`
	// Debug is set while gff-hab writes its debug lines (4.5).
	Debug bool `json:"debug,omitempty"`
}

// Record is one line of health.jsonl (6.3).
type Record struct {
	T         time.Time `json:"t"`
	Proc      string    `json:"proc"`
	Instance  string    `json:"instance,omitempty"`
	Kind      string    `json:"kind"`
	Cause     string    `json:"cause"`
	Run       string    `json:"run,omitempty"`
	Build     string    `json:"build,omitempty"`
	Version   string    `json:"version,omitempty"`
	Started   time.Time `json:"started,omitzero"`
	LastSeen  time.Time `json:"last_seen,omitzero"`
	CrashFile string    `json:"crash_file,omitempty"`
	Panes     int       `json:"panes,omitempty"` // a lost holder: panes lost
}

// ReadMarker reads a marker. ok is false when there is none.
func ReadMarker(path string) (m Marker, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, err
	}
	return m, true, nil
}

// WriteMarker writes a marker in one rename, with mode 0600.
func WriteMarker(path string, m Marker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CrashCause finds the cause in a crash file's text: the first line that starts with "panic:"
// or "fatal error:", with the value cut to CauseMax bytes. ok is false when there is none.
func CrashCause(b []byte) (cause string, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		for _, p := range []string{"panic: ", "fatal error: "} {
			if v, found := strings.CutPrefix(line, p); found {
				return p + Cut(v, CauseMax), true
			}
		}
	}
	return "", false
}

// Cut cuts s to at most n bytes, at a rune boundary.
func Cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}

// Previous classifies how the run of marker prev ended (6.3). crash is its crash file's
// content, nil when it has none. ok is false when it ended clean, so there is nothing to record.
func Previous(prev Marker, crash []byte, now time.Time) (r Record, ok bool) {
	if prev.Clean {
		return r, false
	}
	r = Record{T: now.UTC(), Proc: prev.Proc, Instance: prev.Instance, Kind: KindUnclean, Cause: CauseKilled,
		Run: prev.Run, Build: prev.Build, Version: prev.Version, Started: prev.Started, LastSeen: prev.LastSeen}
	if cause, found := CrashCause(crash); found {
		r.Kind, r.Cause, r.CrashFile = KindCrash, cause, prev.CrashFile
	}
	return r, true
}

// Append adds r to the records file at path and keeps the newest Keep.
func Append(path string, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(string(old), "\n"), "\n")
	if len(old) == 0 {
		lines = nil
	}
	lines = append(lines, string(b))
	if len(lines) > Keep {
		lines = lines[len(lines)-Keep:]
	}
	return writeAtomic(path, []byte(strings.Join(lines, "\n")+"\n"))
}

// Read returns the records in path, oldest first. A line that does not parse is skipped.
func Read(path string) ([]Record, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for line := range strings.SplitSeq(string(b), "\n") {
		var r Record
		if line != "" && json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// NoticeDue reports whether a crash notice may be raised now: none was in the last every. It
// then records now in the file at path, so a crash loop raises at most one per every (6.3).
func NoticeDue(path string, now time.Time, every time.Duration) bool {
	var last struct {
		T time.Time `json:"t"`
	}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &last) == nil {
		if d := now.Sub(last.T); d >= 0 && d < every {
			return false
		}
	}
	last.T = now.UTC()
	b, _ := json.Marshal(last)
	writeAtomic(path, b)
	return true
}
