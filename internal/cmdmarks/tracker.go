package cmdmarks

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxCommands is the most commands a Tracker keeps; past it the oldest is dropped.
const MaxCommands = 1000

// State says how far a command got.
type State string

const (
	Running State = "running" // output started, no end yet
	Done    State = "done"    // a D mark ended it
	Unknown State = "unknown" // a prompt mark or a gap closed it without a D
)

// Command is one shell command: the offsets of its marks, how it ended and when.
type Command struct {
	// ID is the offset of the command's C mark: unique in the pane, and the same when the list
	// is rebuilt from the ring.
	ID uint64 `json:"id"`
	// Prompt is the offset of the A mark before it; Input the offset just after the B mark.
	Prompt *uint64 `json:"prompt,omitempty"`
	Input  *uint64 `json:"input,omitempty"`
	// Output is the offset just after the C mark: the first byte of output.
	Output uint64 `json:"output"`
	// End is where the output ends: the first byte of the D mark, of the A mark that closed
	// it, or of a gap. Nil while the command runs.
	End   *uint64 `json:"end,omitempty"`
	State State   `json:"state"`
	// Exit is the D mark's exit status, when it had one.
	Exit *int `json:"exit,omitempty"`
	// Text is the command line from the C mark's cmdline_url, when the shell sent it.
	Text string `json:"text,omitempty"`
	// Started and Ended are when the C mark and the closing mark arrived as live output. Zero
	// for marks found in replayed bytes: the time is unknown.
	Started time.Time `json:"started,omitzero"`
	Ended   time.Time `json:"ended,omitzero"`
}

type phase string

const (
	phIdle    phase = ""
	phPrompt  phase = "prompt"
	phTyping  phase = "typing"
	phRunning phase = "running"
)

// Saved is a Tracker's state without its scanner, for a checkpoint. A checkpoint is taken where
// the stream is in the ground state, so no mark is half read there.
type Saved struct {
	Phase    phase     `json:"phase,omitempty"`
	Prompt   *uint64   `json:"prompt,omitempty"`
	Input    *uint64   `json:"input,omitempty"`
	Commands []Command `json:"commands,omitempty"`
}

// Tracker keeps one pane's command list. It is not safe for concurrent use.
type Tracker struct {
	sc      Scanner
	phase   phase
	prompt  *uint64 // the A of the prompt now showing
	input   *uint64 // just after the B of the line being typed
	cmds    []Command
	changed map[uint64]bool // ids changed since the last Changes
	now     time.Time       // the time of the bytes being fed; zero for replayed bytes
}

// New returns a tracker for a stream whose next byte is at offset off.
func New(off uint64) *Tracker {
	return &Tracker{sc: Scanner{off: off}, changed: map[uint64]bool{}}
}

// Restore returns a tracker in the state s, for a stream whose next byte is at offset off.
func Restore(off uint64, s Saved) *Tracker {
	t := New(off)
	t.phase, t.prompt, t.input = s.Phase, s.Prompt, s.Input
	switch t.phase {
	case phIdle, phPrompt, phTyping, phRunning:
	default:
		t.phase = phIdle
	}
	t.cmds = append([]Command(nil), s.Commands...)
	if len(t.cmds) > MaxCommands {
		t.cmds = t.cmds[len(t.cmds)-MaxCommands:]
	}
	if t.phase == phRunning && (len(t.cmds) == 0 || t.cmds[len(t.cmds)-1].State != Running) {
		t.phase = phIdle
	}
	return t
}

// Save returns the tracker's state for a checkpoint.
func (t *Tracker) Save() Saved {
	return Saved{Phase: t.phase, Prompt: t.prompt, Input: t.input, Commands: append([]Command(nil), t.cmds...)}
}

// Offset returns the offset after the last byte fed.
func (t *Tracker) Offset() uint64 { return t.sc.Offset() }

// Feed applies the bytes data at offset off. now is when they arrived as live output, or zero
// for replayed bytes. An offset after the last byte fed is a gap: a running command closes as
// unknown where the gap starts, and the scanner starts afresh. An offset before it restarts the
// scanner there.
func (t *Tracker) Feed(off uint64, data []byte, now time.Time) {
	t.now = now
	if at := t.sc.Offset(); off != at {
		if off > at {
			t.closeRunning(at)
			t.phase, t.prompt, t.input = phIdle, nil, nil
		}
		t.sc.Reset(off)
	}
	t.sc.Feed(data, t.apply)
}

// apply moves the state machine by one mark (spec 2.5, the rules table).
func (t *Tracker) apply(m Mark) {
	switch m.Kind {
	case 'A':
		t.closeRunning(m.Start)
		at := m.Start
		t.phase, t.prompt, t.input = phPrompt, &at, nil
	case 'B':
		if t.phase == phRunning {
			return
		}
		if t.phase == phIdle {
			t.prompt = nil
		}
		at := m.End
		t.phase, t.input = phTyping, &at
	case 'C':
		if t.phase == phRunning {
			return
		}
		c := Command{ID: m.Start, Prompt: t.prompt, Input: t.input, Output: m.End, State: Running,
			Text: cmdline(m.Params), Started: t.now}
		t.phase, t.prompt, t.input = phRunning, nil, nil
		t.add(c)
	case 'D':
		if t.phase != phRunning || len(t.cmds) == 0 {
			return
		}
		c := &t.cmds[len(t.cmds)-1]
		end := m.Start
		c.End, c.State, c.Ended = &end, Done, t.now
		if len(m.Params) > 0 {
			if n, err := strconv.Atoi(m.Params[0]); err == nil {
				c.Exit = &n
			}
		}
		t.changed[c.ID] = true
		t.phase = phIdle
	}
}

// closeRunning closes the running command, if any, as unknown at offset at.
func (t *Tracker) closeRunning(at uint64) {
	if t.phase != phRunning || len(t.cmds) == 0 {
		return
	}
	c := &t.cmds[len(t.cmds)-1]
	if c.State != Running {
		return
	}
	c.End, c.State, c.Ended = &at, Unknown, t.now
	t.changed[c.ID] = true
	t.phase = phIdle
}

func (t *Tracker) add(c Command) {
	if len(t.cmds) >= MaxCommands {
		delete(t.changed, t.cmds[0].ID)
		t.cmds = append(t.cmds[:0], t.cmds[1:]...)
	}
	t.cmds = append(t.cmds, c)
	t.changed[c.ID] = true
}

// cmdline decodes the cmdline_url param of a C mark.
func cmdline(params []string) string {
	for _, p := range params {
		if v, ok := strings.CutPrefix(p, "cmdline_url="); ok {
			if s, err := url.PathUnescape(v); err == nil {
				return s
			}
			return ""
		}
	}
	return ""
}

// Trim drops the ended commands whose output ends at or before oldest: the holder's ring no
// longer holds any of their output. A running command is kept.
func (t *Tracker) Trim(oldest uint64) {
	n := 0
	for n < len(t.cmds) && t.cmds[n].End != nil && *t.cmds[n].End <= oldest {
		delete(t.changed, t.cmds[n].ID)
		n++
	}
	if n > 0 {
		t.cmds = append(t.cmds[:0], t.cmds[n:]...)
	}
}

// Changed reports whether any command changed since the last Changes.
func (t *Tracker) Changed() bool { return len(t.changed) > 0 }

// Changes returns the commands that started or ended since the last call, oldest first, and
// forgets them. A command dropped meanwhile is not returned.
func (t *Tracker) Changes() []Command {
	if len(t.changed) == 0 {
		return nil
	}
	var out []Command
	for _, c := range t.cmds {
		if t.changed[c.ID] {
			out = append(out, c)
		}
	}
	clear(t.changed)
	return out
}

// Get returns the command with id.
func (t *Tracker) Get(id uint64) (Command, bool) {
	for i := len(t.cmds) - 1; i >= 0; i-- {
		if t.cmds[i].ID == id {
			return t.cmds[i], true
		}
	}
	return Command{}, false
}

// List returns up to limit commands older than before (all, with before nil), oldest first, and
// whether older ones remain.
func (t *Tracker) List(before *uint64, limit int) ([]Command, bool) {
	end := len(t.cmds)
	if before != nil {
		end = 0
		for end < len(t.cmds) && t.cmds[end].ID < *before {
			end++
		}
	}
	start := max(end-limit, 0)
	return append([]Command{}, t.cmds[start:end]...), start > 0
}

// Len returns the number of commands kept.
func (t *Tracker) Len() int { return len(t.cmds) }
