package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/control"
	"github.com/noahkiss/flightdeck/internal/health"
)

// Debug lines (diagnostics.md 4.5): diag.debug opens a window until a time, and
// <state>/run/debug.json keeps it, so a restart or a crash loop keeps the lines on until the
// window ends. GFF_HAB_DEBUG turns them on with no time limit, for development.

// debugCapability says diag.debug works.
const debugCapability = "debug"

const debugFile = "debug.json"

type debugRecord struct {
	Until time.Time `json:"until"`
}

// debugSwitch holds whether gff-hab writes its debug lines. printf costs one atomic load when
// debug is off.
type debugSwitch struct {
	path    string
	env     bool
	logf    func(string, ...any)
	now     func() time.Time
	changed func(on bool, until time.Time) // the marker, for gff-hab status; called with mu held

	on atomic.Bool

	mu     sync.Mutex
	until  time.Time
	timer  *time.Timer
	closed bool
}

// openDebug reads the window a previous run left. A window still open turns debug on until it
// ends; one already past is ignored and removed (12).
func openDebug(state string, env bool, logf func(string, ...any), now func() time.Time, changed func(bool, time.Time)) *debugSwitch {
	d := &debugSwitch{path: filepath.Join(state, runDir, debugFile), env: env, logf: logf, now: now, changed: changed}
	d.on.Store(env)
	var r debugRecord
	b, err := os.ReadFile(d.path)
	d.mu.Lock()
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		logf("debug: %v", err)
	case json.Unmarshal(b, &r) != nil || !r.Until.After(now()):
		os.Remove(d.path)
	default:
		d.open(r.Until)
		logf("debug on until %s, a window opened before this start", r.Until.UTC().Format(time.RFC3339))
	}
	d.changed(d.on.Load(), d.until)
	d.mu.Unlock()
	return d
}

// open turns debug on until t and arms its end. Called with d.mu held.
func (d *debugSwitch) open(t time.Time) {
	d.until = t
	d.on.Store(true)
	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(t.Sub(d.now()), func() { d.end(t) })
}

// end closes the window that ends at t, unless a later Set replaced it.
func (d *debugSwitch) end(t time.Time) {
	d.mu.Lock()
	if d.closed || !d.until.Equal(t) {
		d.mu.Unlock()
		return
	}
	d.until, d.timer = time.Time{}, nil
	d.on.Store(d.env)
	os.Remove(d.path)
	d.changed(d.env, time.Time{})
	d.mu.Unlock()
	d.logf("debug off: the window ended")
}

// printf writes a debug line when debug is on.
func (d *debugSwitch) printf(format string, args ...any) {
	if d.on.Load() {
		d.logf(format, args...)
	}
}

// Set opens a window until until, or closes it when until is zero (diag.debug). The control
// server checked until: zero or within control.DebugMax.
func (d *debugSwitch) Set(until time.Time) (api.DebugState, error) {
	d.mu.Lock()
	if until.IsZero() {
		if d.timer != nil {
			d.timer.Stop()
		}
		d.until, d.timer = time.Time{}, nil
		d.on.Store(d.env)
		err := os.Remove(d.path)
		d.changed(d.env, time.Time{})
		d.mu.Unlock()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return api.DebugState{}, err
		}
		d.logf("debug off: diag.debug")
		return api.DebugState{Env: d.env}, nil
	}
	b, _ := json.Marshal(debugRecord{Until: until.UTC()})
	if err := health.WriteFile(d.path, append(b, '\n')); err != nil {
		d.mu.Unlock()
		return api.DebugState{}, fmt.Errorf("debug: %w", err)
	}
	d.open(until)
	d.changed(true, until)
	d.mu.Unlock()
	d.logf("debug on until %s: diag.debug", until.UTC().Format(time.RFC3339))
	return api.DebugState{Until: until.UTC().Format(time.RFC3339), Env: d.env}, nil
}

// Close stops the window's timer. The file stays, so the next start honours the window.
func (d *debugSwitch) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.timer != nil {
		d.timer.Stop()
	}
}

var debugClient = api.ClientInfo{Name: "gff-hab debug", Version: build, Kind: api.ClientKindCli}

const debugUsage = `usage: gff-hab debug [-socket path | -via command] <duration>|off

Turns gff-hab's debug lines on for a time (at most 24h), or off. The window survives a restart
and a crash loop, and ends by itself. gff-hab status shows it.
`

// debugCmd is gff-hab debug (4.5).
func debugCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("debug", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), debugUsage); fs.PrintDefaults() }
	sock := socketFlag(fs)
	via := fs.String("via", "", "run this command and speak over its stdio, e.g. \"ssh <host> gff-hab connect\"")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("debug takes one argument: a duration such as 30m, or off")
	}
	var p api.DebugParams
	if a := fs.Arg(0); a != "off" {
		d, err := time.ParseDuration(a)
		if err != nil || d <= 0 {
			return fmt.Errorf("debug %q: want a duration such as 30m, or off", a)
		}
		if d > control.DebugMax {
			return fmt.Errorf("debug %s: at most %s", a, control.DebugMax)
		}
		p.Until = time.Now().Add(d).UTC().Format(time.RFC3339)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return fmt.Errorf("%v: is gff-hab running? (gff-hab status)", err)
	}
	defer c.Close()
	if _, err := c.Hello(ctx, debugClient, "", ""); err != nil {
		return err
	}
	res, err := api.Client{Caller: c}.DiagDebug(ctx, p)
	if err != nil {
		return err
	}
	switch {
	case res.Until != "":
		t, _ := time.Parse(time.RFC3339, res.Until)
		fmt.Fprintf(out, "debug on until %s\n", t.Local().Format("2006-01-02 15:04:05"))
	case res.Env:
		fmt.Fprintln(out, "debug window closed; GFF_HAB_DEBUG keeps debug on")
	default:
		fmt.Fprintln(out, "debug off")
	}
	return nil
}
