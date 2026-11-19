package main

import (
	"context"
	"path/filepath"
	"slices"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/friction"
	"github.com/noahkiss/flightdeck/internal/logfile"
)

// frictionCapability says diag.friction works (diagnostics.md 9).
const frictionCapability = "friction"

// frictionNotes serves diag.friction: it stores the user's note with the merged log tail of
// the last 10 minutes (9.2). The text goes into the note's file only; the line it logs names
// the trace alone.
type frictionNotes struct {
	state string
	w     *logfile.Writer // flushed first, so the tail holds the lines written up to the note
	logf  func(string, ...any)
	now   func() time.Time
}

func frictionDir(state string) string { return filepath.Join(state, friction.Dir) }

func (f *frictionNotes) Note(_ context.Context, client api.ClientKind, p api.FrictionParams) (api.FrictionNoted, error) {
	now := f.now()
	if f.w != nil {
		f.w.FlushTimeout(exitFlush)
	}
	n := friction.Note{T: now.UTC(), Trace: friction.NewTrace('h'), Client: string(client), Session: p.Session,
		Pane: p.Pane, Blocker: p.Blocker, Text: p.Text, Build: build, Version: version, Log: logTail(f.state, now)}
	if err := friction.Write(frictionDir(f.state), n, now); err != nil {
		return api.FrictionNoted{}, err
	}
	kind := "preference"
	if p.Blocker {
		kind = "blocker"
	}
	f.logf("friction note stored: trace %s, %s, client %s, %d log lines", n.Trace, kind, client, len(n.Log))
	return api.FrictionNoted{Trace: n.Trace}, nil
}

// logTail is the merged log (4.8) of the TailWindow before now, its last TailLines entries,
// copied as the files hold them.
func logTail(state string, now time.Time) []friction.Line {
	ring := make([]friction.Line, 0, friction.TailLines)
	start := 0
	logfile.Merge(logSources(state), logfile.Filter{Since: now.Add(-friction.TailWindow), Until: now.Add(time.Second)},
		func(e logfile.Entry) {
			if e.T.IsZero() {
				return // before the first time stamp in its file: its time is unknown
			}
			l := friction.Line{T: e.T.UTC(), Proc: e.Name, Text: e.Text}
			if len(ring) < friction.TailLines {
				ring = append(ring, l)
				return
			}
			ring[start] = l
			start = (start + 1) % friction.TailLines
		})
	return slices.Concat(ring[start:], ring[:start])
}
