// Package habits keeps the recording habits of docs/specs/logbook.md 11: when a session gets the
// reminder and the closeout, and which habits a project turned off. gff-hab feeds it the hook
// events of each session and asks it at a prompt and at a stop; the brief itself comes from
// internal/lbsvc. Pure Go; it keeps its state in memory only.
package habits

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noahkiss/flightdeck/internal/settings"
)

// The habits, by the word their settings and the config use (11.1).
const (
	Brief    = "brief"
	Reminder = "reminder"
	Closeout = "closeout"
)

// Names lists the habits.
var Names = []string{Brief, Reminder, Closeout}

// ReminderText is what the reminder adds to a prompt (11.1).
const ReminderText = "Nothing recorded yet this session. Record findings and task changes with `gff lb`."

// CloseoutText is the reason the closeout gives at a stop (11.1).
const CloseoutText = "Before you stop: this session changed files and recorded nothing in Logbook. " +
	"Record what you learned with `gff lb` (a finding, a task's status, a note), or say there is nothing to record."

// The reminder's thresholds (11.1): files changed for ReminderAfter or ReminderTurns prompts with
// no Logbook write, and at most one reminder per ReminderEvery.
const (
	ReminderAfter = 30 * time.Minute
	ReminderTurns = 20
	ReminderEvery = time.Hour
)

// maxSessions bounds the sessions the tracker remembers; idle is when one may go.
const (
	maxSessions = 4096
	idle        = 24 * time.Hour
)

// Tracker follows each session's file changes, prompts and Logbook writes. A key is a
// Flightdeck session id, or a pane id for a pane outside a session.
type Tracker struct {
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*state
}

type state struct {
	dirty    time.Time // the first change since the last Logbook write; zero: none
	turns    int       // prompts since dirty
	changed  bool      // any change this session
	wrote    bool      // any Logbook write this session
	reminded time.Time
	closed   bool // the closeout came: never again for this session
	seen     time.Time
}

// New returns a tracker; now may be nil.
func New(now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{now: now, sessions: map[string]*state{}}
}

func (t *Tracker) get(key string) *state {
	now := t.now()
	s, ok := t.sessions[key]
	if !ok {
		if len(t.sessions) >= maxSessions {
			for k, o := range t.sessions {
				if now.Sub(o.seen) > idle {
					delete(t.sessions, k)
				}
			}
			if len(t.sessions) >= maxSessions {
				clear(t.sessions)
			}
		}
		s = &state{}
		t.sessions[key] = s
	}
	s.seen = now
	return s
}

// Changed records a file change or a commit by the session.
func (t *Tracker) Changed(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(key)
	s.changed = true
	if s.dirty.IsZero() {
		s.dirty, s.turns = t.now(), 0
	}
}

// Wrote records a Logbook write by the session: the reminder's count starts again, and the
// closeout no longer asks.
func (t *Tracker) Wrote(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(key)
	s.wrote = true
	s.dirty, s.turns = time.Time{}, 0
}

// Prompt counts one prompt and reports whether the reminder goes with it: files changed for
// ReminderAfter or ReminderTurns prompts since the last Logbook write, and no reminder in the
// last ReminderEvery.
func (t *Tracker) Prompt(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(key)
	if s.dirty.IsZero() {
		return false
	}
	s.turns++
	now := t.now()
	if now.Sub(s.dirty) < ReminderAfter && s.turns < ReminderTurns {
		return false
	}
	if !s.reminded.IsZero() && now.Sub(s.reminded) < ReminderEvery {
		return false
	}
	s.reminded = now
	return true
}

// Stop reports whether the closeout goes with this stop: the session changed files or made a
// commit and wrote nothing to Logbook, and no closeout came before. It comes once per session.
func (t *Tracker) Stop(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.get(key)
	if s.closed || !s.changed || s.wrote {
		return false
	}
	s.closed = true
	return true
}

// editTools change files whatever their input: Claude Code's and Codex's edit tools.
var editTools = []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "apply_patch"}

// shellTools run a command: a git commit through one counts as a change.
var shellTools = []string{"Bash", "shell", "exec_command", "local_shell"}

var gitCommit = regexp.MustCompile(`(^|[^A-Za-z0-9_-])git(\s+-\S+(\s+\S+)?)*\s+commit\b`)

// Changes reports whether a PostToolUse payload is a file change or a commit: an edit tool, or
// a shell tool whose command runs git commit.
func Changes(payload json.RawMessage) bool {
	var p struct {
		Tool  string          `json:"tool_name"`
		Input json.RawMessage `json:"tool_input"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return false
	}
	if slices.Contains(editTools, p.Tool) {
		return true
	}
	if !slices.Contains(shellTools, p.Tool) {
		return false
	}
	var in struct {
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal(p.Input, &in) != nil {
		return false
	}
	var cmd string
	if json.Unmarshal(in.Command, &cmd) != nil {
		var argv []string
		if json.Unmarshal(in.Command, &argv) != nil {
			return false
		}
		for _, a := range argv {
			cmd += " " + a
		}
	}
	return gitCommit.MatchString(cmd)
}

// Setting names each habit's hab-side setting.
var Setting = map[string]string{Brief: settings.HabitBrief, Reminder: settings.HabitReminder, Closeout: settings.HabitCloseout}

// On reports whether habit is on for a project: its hab-side setting (value, as the settings
// service or settings.Read gives it; nil: every setting on), then the project's switch in off by
// any of keys, a Logbook project id or a project identity. gff-hab and the hook's file fallback
// both decide through it.
func On(habit string, value func(id string) json.RawMessage, off Off, keys ...string) bool {
	if value != nil {
		var v bool
		if json.Unmarshal(value(Setting[habit]), &v) == nil && !v {
			return false
		}
	}
	return !off.Has(habit, keys...)
}

// Off is the per-project switch (11.1): for each project, by a project id or an identity, the
// habits turned off. It comes from the [logbook.habits_off] table of the hab's config file.
type Off map[string][]string

// Has reports whether habit is off for any of keys.
func (o Off) Has(habit string, keys ...string) bool {
	for _, k := range keys {
		if k != "" && slices.Contains(o[k], habit) {
			return true
		}
	}
	return false
}

// LoadOff reads [logbook.habits_off] from the hab's config file. A missing file is no table.
// An unknown habit is refused, naming it.
func LoadOff(path string) (Off, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Off{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Logbook struct {
			HabitsOff map[string][]string `toml:"habits_off"`
		} `toml:"logbook"`
	}
	if _, err := toml.Decode(string(b), &f); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	out := Off{}
	for k, hs := range f.Logbook.HabitsOff {
		for _, h := range hs {
			if !slices.Contains(Names, h) {
				return nil, fmt.Errorf("%s: [logbook.habits_off] %q: no habit %q; habits are %v", path, k, h, Names)
			}
		}
		out[k] = hs
	}
	return out, nil
}

// OffFile reads the table again when the file changed, by size and modification time, so a
// hook call costs a stat. A file that does not parse keeps the last good table, and Err says why.
type OffFile struct {
	Path string

	mu    sync.Mutex
	stamp [2]int64
	read  bool
	off   Off
	err   error
}

// Get returns the current table.
func (f *OffFile) Get() Off {
	if f == nil || f.Path == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var stamp [2]int64
	if fi, err := os.Stat(f.Path); err == nil {
		stamp = [2]int64{fi.Size(), fi.ModTime().UnixNano()}
	}
	if f.read && stamp == f.stamp {
		return f.off
	}
	f.read, f.stamp = true, stamp
	off, err := LoadOff(f.Path)
	f.err = err
	if err == nil {
		f.off = off
	}
	return f.off
}

// Err is why the last read of the file failed, or nil.
func (f *OffFile) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}
