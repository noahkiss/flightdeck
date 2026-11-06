package lbextra

import (
	"context"
	"time"

	"github.com/noahkiss/flightdeck/internal/logbook"
)

// Runner runs the backup and the git export on one hab. It reads the config file at each step,
// so a change holds from the next minute. Vector search is gff-hab's (Vectors), made at start.
type Runner struct {
	Store  *logbook.Store
	State  string
	Config string // the hab's config file
	// AlwaysOn reports the hab's always-on mark (mesh.md 8): only a marked hab backs up.
	AlwaysOn func() bool
	Now      func() time.Time
	Logf     func(string, ...any)
	Git      string // the git program; empty is "git"

	lastGit    time.Time
	gitCursor  int64
	gitChecked bool
	lastErr    string
}

// Tick is how often Run steps. A hab that slept steps at once on waking: the ticker fires.
const Tick = time.Minute

// Run steps every Tick until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		r.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step makes a backup when one is due on an always-on hab, and an export commit when the
// export is set, the replica changed and the last commit is an hour old.
func (r *Runner) Step(ctx context.Context) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	logf := r.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cfg, err := LoadConfig(r.Config, r.State)
	if err != nil {
		r.once(logf, "logbook extras: "+err.Error())
		return
	}
	if r.AlwaysOn != nil && r.AlwaysOn() {
		b := Backup{Dir: cfg.Backup.Path, Keep: cfg.Backup.Keep}
		if due, err := b.Due(now()); err != nil {
			r.once(logf, "logbook backup: "+err.Error())
		} else if due {
			if p, err := b.Run(ctx, r.Store, now()); err != nil {
				r.once(logf, "logbook backup: "+err.Error())
			} else {
				logf("logbook backup: %s", p)
			}
		}
	}
	if cfg.Export.Git == "" {
		return
	}
	g := GitExport{Dir: cfg.Export.Git, Git: r.Git}
	if !r.gitChecked {
		r.lastGit = g.LastCommit(ctx)
		r.gitChecked = true
	}
	cur, err := r.Store.Cursor(ctx)
	if err != nil {
		r.once(logf, "logbook export: "+err.Error())
		return
	}
	if r.gitCursor != 0 && cur == r.gitCursor || now().Sub(r.lastGit) < GitEvery {
		return
	}
	res, err := g.Export(ctx, r.Store, now())
	if err != nil {
		r.once(logf, "logbook export: "+err.Error())
		return
	}
	r.gitCursor = cur
	if res.Committed {
		r.lastGit = now()
	}
	r.lastErr = ""
}

// once logs an error the first time it shows, not every minute.
func (r *Runner) once(logf func(string, ...any), msg string) {
	if msg != r.lastErr {
		logf("%s", msg)
		r.lastErr = msg
	}
}
