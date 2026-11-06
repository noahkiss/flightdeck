package link

import (
	"context"
	"errors"
	"time"

	"github.com/noahkiss/flightdeck/internal/cmdmarks"
	hp "github.com/noahkiss/flightdeck/internal/holderproto"
)

// Commands from OSC 133 prompt marks (docs/specs/hab-logic-api.md 2.5). Each pane runs one
// cmdmarks.Tracker over its whole stream, fed from feed, live and replayed bytes alike. The list
// is saved with the pane's checkpoint. A command's bytes are read back from the holder's ring by a
// bounded replay; the link keeps no copy of them.

// MaxCommandOutput is the most bytes one CommandOutput call returns.
const MaxCommandOutput = 1 << 20

// ErrNoCommand is returned for a command id the pane does not hold (never seen, or dropped).
var ErrNoCommand = errors.New("link: no such command")

// findMarks feeds the bytes the parser just took to the pane's command tracker, then drops the
// commands whose output has left the holder's ring. Live bytes give the commands their times.
// The caller holds p.mu.
func (p *pane) findMarks(off uint64, data []byte, live bool) {
	var now time.Time
	if live {
		now = time.Now()
	}
	p.cmds.Feed(off, data, now)
	if p.holder == nil {
		return
	}
	end := off + uint64(len(data))
	if ring := p.holder.Welcome().RingSize; ring > 0 && end > ring {
		p.cmds.Trim(end - ring)
	}
}

// commandNotice appends a PaneCommands notice when the pane's commands changed. It runs on the
// event loop, after feed.
func (p *pane) commandNotice(ns []Notice) []Notice {
	p.mu.Lock()
	changed := p.cmds.Changed()
	p.mu.Unlock()
	if !changed {
		return ns
	}
	return append(ns, Notice{Kind: PaneCommands, Pane: p.status()})
}

// saveCommands returns the tracker's state for a checkpoint. The caller holds p.mu.
func (p *pane) saveCommands() *cmdmarks.Saved {
	s := p.cmds.Save()
	return &s
}

// restoreCommands returns the tracker a checkpoint saved, or an empty one at its offset.
func restoreCommands(ck *checkpoint) *cmdmarks.Tracker {
	if ck.Commands == nil {
		return cmdmarks.New(ck.Offset)
	}
	return cmdmarks.Restore(ck.Offset, *ck.Commands)
}

// Commands returns up to limit of pane u's commands older than the command with id before (the
// newest, with before nil), oldest first, and whether older ones remain.
func (l *Link) Commands(u hp.UUID, before *uint64, limit int) ([]cmdmarks.Command, bool, error) {
	p := l.pane(u)
	if p == nil {
		return nil, false, ErrNoPane
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cs, more := p.cmds.List(before, max(limit, 0))
	return cs, more, nil
}

// Cwd returns the working directory pane u's shell last reported with OSC 7, or "".
func (l *Link) Cwd(u hp.UUID) string {
	p := l.pane(u)
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmds.Cwd()
}

// CommandChanges returns pane u's commands that started or ended since the last call, oldest
// first. A PaneCommands notice says there are some.
func (l *Link) CommandChanges(u hp.UUID) []cmdmarks.Command {
	p := l.pane(u)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmds.Changes()
}

// CommandBytes is a run of one command's output, read from the holder's ring.
type CommandBytes struct {
	// Data is the output in [Start, End).
	Data       []byte
	Start, End uint64
	// Gap is true when the ring no longer held bytes between the asked offset and Start.
	Gap bool
	// More is true when the command's output goes on past End: the limit cut it, or the
	// command still runs.
	More bool
	// Command is the command as it stood when the range was read.
	Command cmdmarks.Command
}

// place fills out from the holder's answer to a replay of [lo, to) of a command whose output
// ends at hi (for this call), or says the answer cannot be placed: the ring moved on while the
// holder sent it. When the ring has lost bytes, the answer starts later and says gap. When it
// has lost the command's whole range, the holder answers at its oldest byte, past hi; the run is
// then the empty one at hi, still with gap, so it never falls outside the command.
func place(out CommandBytes, lo, hi uint64, data []byte, end *hp.ReplayEnd) (CommandBytes, bool) {
	if uint64(len(data)) != end.End-end.Start {
		return CommandBytes{}, false
	}
	out.Data, out.Start, out.End = data, end.Start, end.End
	if end.Start > hi {
		out.Data, out.Start, out.End = nil, hi, hi
	}
	out.Gap = end.Gap || end.Start > lo
	out.More = out.End < hi || out.Command.End == nil
	return out, true
}

// CommandOutput reads up to limit bytes of command id's output on pane u, from offset from
// (the output's start, with from nil), with a bounded replay from the pane's holder. A running
// command's output ends, for this call, at the bytes the link holds now. limit is capped at
// MaxCommandOutput; zero or less means the cap.
func (l *Link) CommandOutput(ctx context.Context, u hp.UUID, id uint64, from *uint64, limit int) (CommandBytes, error) {
	p := l.pane(u)
	if p == nil {
		return CommandBytes{}, ErrNoPane
	}
	if limit <= 0 || limit > MaxCommandOutput {
		limit = MaxCommandOutput
	}
	p.mu.Lock()
	h, hd, lost := p.holder, p.handle, p.lost
	c, ok := p.cmds.Get(id)
	hi := p.off
	p.mu.Unlock()
	switch {
	case lost != nil:
		return CommandBytes{}, lost
	case !ok:
		return CommandBytes{}, ErrNoCommand
	case h == nil:
		return CommandBytes{}, ErrNoPane
	}
	if c.End != nil {
		hi = *c.End
	}
	lo := c.Output
	if from != nil {
		lo = min(max(*from, c.Output), hi)
	}
	to := min(hi, lo+uint64(limit))
	out := CommandBytes{Start: lo, End: to, Command: c, More: to < hi || c.End == nil}
	if to == lo {
		return out, nil
	}
	// The ring may move on while the holder sends the range; it then skips bytes in the middle,
	// which cannot be placed. Ask again: the next replay starts after them.
	for try := 0; ; try++ {
		data, end, err := h.ReplayRange(ctx, hd, lo, to)
		if err != nil {
			return CommandBytes{}, err
		}
		if placed, ok := place(out, lo, hi, data, end); ok {
			return placed, nil
		}
		if try == 2 {
			return CommandBytes{}, errors.New("link: the ring moved on while reading a command's output")
		}
	}
}
