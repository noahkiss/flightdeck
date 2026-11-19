package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/capcomengine"
	"github.com/noahkiss/flightdeck/internal/health"
	"github.com/noahkiss/flightdeck/internal/holderreg"
	"github.com/noahkiss/flightdeck/internal/logfile"
)

// The files and caps of docs/specs/diagnostics.md 4.3, 6 and 11.
const (
	habLogFile    = "hab.log"
	habStderrFile = "hab.stderr" // the service manager's capture of gff-hab's stdout and stderr
	healthFile    = "health.jsonl"
	hookLogFile   = "hook.log"
	crashDir      = "crash"
	runDir        = "run"
	habMarkerFile = "hab.json"
	crashNotice   = "crash-notice.json"

	habLogMax, habLogKeep       = 10 << 20, 3
	stderrMax, stderrKeep       = 1 << 20, 1
	holderLogMax, holderLogKeep = 5 << 20, 2
	hookLogMax, hookLogKeep     = 1 << 20, 1
	engineLogKeep               = 1
	crashKeep                   = 20
	logsBudget                  = 100 << 20

	// habLogFlags: UTC date and time with microseconds, which gff-hab logs parses (4.1).
	habLogFlags = log.LstdFlags | log.Lmicroseconds | log.LUTC
	habPrefix   = "gff-hab: "

	sweepEvery    = time.Hour
	hookDownEvery = 10 * time.Minute
	noticeEvery   = time.Hour
	markerEvery   = time.Minute
	// goneHolderAge: a holder's file whose instance left the registry this long ago goes in the
	// sweep. launchd's own sweep removes the current file sooner (internal/service).
	goneHolderAge = 7 * 24 * time.Hour
)

func logsDir(state string) string { return filepath.Join(state, "logs") }

// holderLogPrefix starts a holder's file in the logs folder: gff-hab-holder.<instance>.log, the
// name the service manager writes (internal/service).
const holderLogPrefix = "gff-hab-holder."

// newLogWriter makes gff-hab's log writer (4.1, 4.3). Run by hand, each line goes to stderr too;
// run by a service manager, a line goes to stderr only before the file opens.
func newLogWriter(service bool, onFlush func()) *logfile.Writer {
	cfg := logfile.Config{Max: habLogMax, Keep: habLogKeep, OnFlush: onFlush,
		Dropped: func(n int64) []byte {
			var b bytes.Buffer
			log.New(&b, habPrefix, habLogFlags).Printf("log: %d lines dropped", n)
			return b.Bytes()
		}}
	if service {
		cfg.Early = os.Stderr
	} else {
		cfg.Tee = os.Stderr
	}
	return logfile.New(cfg)
}

// habDiag is gff-hab's side of diagnostics stage 1: its run marker, crash output, health
// records, the crash notice, the hook endpoint's state and the hourly sweep of the logs folder.
type habDiag struct {
	state     string
	holderDir string
	logf      func(string, ...any)
	w         *logfile.Writer
	now       func() time.Time
	hookEvery time.Duration

	mu        sync.Mutex
	marker    health.Marker
	lastWrite time.Time
	// markerFailing is set while marker writes fail, so the failure is logged once.
	markerFailing bool
	notice    bool // the previous run crashed and a notice is due
	stopped   bool

	stop chan struct{}
	wg   sync.WaitGroup
}

// diagRef lets the log writer, which starts before the state lock, reach the diag once it runs.
type diagRef struct{ atomic.Pointer[habDiag] }

func (r *diagRef) touch() {
	if d := r.Load(); d != nil {
		d.touch()
	}
}

func (r *diagRef) holderLost(instance, build string, panes int) {
	if d := r.Load(); d != nil {
		d.holderLost(instance, build, panes)
	}
}

// startDiag runs once gff-hab holds the state lock, so one gff-hab owns these files. It opens
// hab.log, reads the previous run's marker (6.3), writes this run's, points crash output at
// crash/hab-<epoch>.txt (6.2) and starts the sweep.
func startDiag(state, holderDir, epoch string, w *logfile.Writer, logf func(string, ...any)) *habDiag {
	d := &habDiag{state: state, holderDir: holderDir, logf: logf, w: w, now: time.Now, hookEvery: hookDownEvery,
		stop: make(chan struct{})}
	if err := w.Open(filepath.Join(logsDir(state), habLogFile)); err != nil {
		logf("log file: %v; lines go to stderr only", err)
	}
	d.previous()
	crash := filepath.Join(state, crashDir, "hab-"+epoch+".txt")
	if err := os.MkdirAll(filepath.Dir(crash), 0o700); err == nil {
		if f, err := os.OpenFile(crash, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err == nil {
			if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
				logf("crash output: %v", err)
			}
			f.Close() // SetCrashOutput keeps its own copy
		} else {
			logf("crash output: %v", err)
		}
	}
	now := d.now().UTC()
	d.marker = health.Marker{Proc: "hab", Run: epoch, Pid: os.Getpid(), Build: build, Version: version,
		Started: now, LastSeen: now, CrashFile: filepath.Base(crash), Debug: os.Getenv("GFF_HAB_DEBUG") != ""}
	d.writeMarker()
	d.sweep()
	d.wg.Add(1)
	go d.sweepLoop()
	return d
}

// previous records how the last run ended, from its marker and crash file (6.3).
func (d *habDiag) previous() {
	path := filepath.Join(d.state, runDir, habMarkerFile)
	prev, ok, err := health.ReadMarker(path)
	if err != nil {
		d.logf("previous run: marker: %v", err)
	}
	if !ok {
		return
	}
	var crash []byte
	if prev.CrashFile != "" {
		p := filepath.Join(d.state, crashDir, filepath.Base(prev.CrashFile))
		crash, _ = os.ReadFile(p)
		if len(crash) == 0 {
			os.Remove(p)
		}
	}
	r, ended := health.Previous(prev, crash, d.now())
	if !ended {
		return
	}
	if err := health.Append(filepath.Join(d.state, healthFile), r); err != nil {
		d.logf("health record: %v", err)
	}
	d.logf("previous run ended %s: %s (epoch %s, build %s, version %s, last seen %s); this run: build %s, version %s",
		r.Kind, r.Cause, r.Run, r.Build, r.Version, r.LastSeen.Format(time.RFC3339), build, version)
	if r.Kind == health.KindCrash && health.NoticeDue(filepath.Join(d.state, runDir, crashNotice), d.now(), noticeEvery) {
		d.mu.Lock()
		d.notice = true
		d.mu.Unlock()
	}
}

// raiseNotice raises the crash notice, when the previous run crashed and none was raised in the
// last hour (6.3). It names no file and no host.
func (d *habDiag) raiseNotice(raise func(api.NoticeRaiseParams) error) {
	d.mu.Lock()
	due := d.notice
	d.notice = false
	d.mu.Unlock()
	if !due {
		return
	}
	if err := raise(api.NoticeRaiseParams{Title: "gff-hab restarted after a crash",
		Body: "gff-hab status shows the cause.", Level: api.NoticeLevelActive}); err != nil {
		d.logf("crash notice: %v", err)
	}
}

// writeMarker writes the run marker. A write that fails counts as a write for touch's minute,
// so a full disk or a read-only folder costs one try a minute, and its line is logged once per
// change between failing and working: the line itself flushes the log, which calls touch.
func (d *habDiag) writeMarker() {
	d.lastWrite = d.now()
	err := health.WriteMarker(filepath.Join(d.state, runDir, habMarkerFile), d.marker)
	switch {
	case err != nil && !d.markerFailing:
		d.markerFailing = true
		d.logf("run marker: %v; retrying once a minute", err)
	case err == nil && d.markerFailing:
		d.markerFailing = false
		d.logf("run marker: written again")
	}
}

// touch moves last_seen when gff-hab flushed log lines, at most once a minute (6.1). An idle
// gff-hab writes nothing.
func (d *habDiag) touch() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.now().Sub(d.lastWrite) < markerEvery {
		return
	}
	d.marker.LastSeen = d.now().UTC()
	d.writeMarker()
}

func (d *habDiag) setHook(state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.marker.Hook == state {
		return
	}
	d.marker.Hook = state
	d.writeMarker()
}

// holderLost writes the record of a holder whose connection ended with an error (6.3).
func (d *habDiag) holderLost(instance, build string, panes int) {
	r := health.Record{T: d.now().UTC(), Proc: "holder", Instance: instance, Kind: health.KindUnclean,
		Cause: health.CauseHolderLost, Build: build, Panes: panes}
	if err := health.Append(filepath.Join(d.state, healthFile), r); err != nil {
		d.logf("health record: %v", err)
	}
}

// serveHooks runs the hook server; the caller marks the endpoint up first. When it stops with an
// error, the hook endpoint is down for the rest of the run: the marker says so for gff-hab
// status, and a line repeats every 10 minutes (4.6).
func (d *habDiag) serveHooks(serve func() error) {
	err := serve()
	if err == nil {
		return
	}
	d.setHook("down")
	d.logf("%v: %v", errHookServe, err)
	t := time.NewTicker(d.hookEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.logf("%v: %v; agent states do not update", errHookServe, err)
		case <-d.stop:
			return
		}
	}
}

// hookErrorLog is the hook server's ErrorLog (6.2): each line goes to gff-hab's log, and a
// handler panic the server recovered also writes a health record of kind recovered.
func (d *habDiag) hookErrorLog() *log.Logger { return log.New(hookErrors{d}, "", 0) }

type hookErrors struct{ d *habDiag }

func (h hookErrors) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	h.d.logf("hook server: %s", s)
	if _, rest, ok := strings.Cut(s, "http: panic serving "); ok {
		_, value, _ := strings.Cut(rest, ": ")
		value, _, _ = strings.Cut(value, "\n")
		r := health.Record{T: h.d.now().UTC(), Proc: "hab", Kind: health.KindRecovered,
			Cause: "panic: " + health.Cut(value, health.CauseMax), Run: h.d.marker.Run, Build: build, Version: version}
		if err := health.Append(filepath.Join(h.d.state, healthFile), r); err != nil {
			h.d.logf("health record: %v", err)
		}
	}
	return len(p), nil
}

// cleanExit marks the run clean, removes its crash file when empty, and closes the log (6.1,
// 6.2).
func (d *habDiag) cleanExit() {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.stopped = true
	d.marker.Clean = true
	d.marker.LastSeen = d.now().UTC()
	d.writeMarker()
	crash := filepath.Join(d.state, crashDir, d.marker.CrashFile)
	d.mu.Unlock()
	close(d.stop)
	d.wg.Wait()
	debug.SetCrashOutput(nil, debug.CrashOptions{})
	if st, err := os.Stat(crash); err == nil && st.Size() == 0 {
		os.Remove(crash)
	}
	d.w.Close()
}

func (d *habDiag) sweepLoop() {
	defer d.wg.Done()
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.sweep()
		case <-d.stop:
			return
		}
	}
}

// sweep caps the files other processes write (4.3): hab.stderr, the holders' files and
// engine.log by copy-truncate, hook.log by rename; it keeps the newest crash files and the
// logs folder under its budget (11), and removes the files of holders long gone.
func (d *habDiag) sweep() {
	logs := logsDir(d.state)
	capFile := func(path string, max int64, keep int) {
		if _, err := logfile.CopyTruncate(path, max, keep); err != nil {
			d.logf("logs: %v", err)
		}
	}
	capFile(filepath.Join(logs, habStderrFile), stderrMax, stderrKeep)
	capFile(filepath.Join(d.state, "capcom", "engine", capcomengine.LogFile), capcomengine.LogMax, engineLogKeep)
	if _, err := logfile.RenameCap(filepath.Join(logs, hookLogFile), hookLogMax, hookLogKeep); err != nil {
		d.logf("logs: %v", err)
	}
	// The gone-holder rule needs the full holder list: with none, or an error, it is skipped, so
	// the file of a live holder is never removed.
	live := map[string]bool{}
	listed := false
	if d.holderDir != "" {
		es, errs := holderreg.List(d.holderDir)
		listed = len(errs) == 0
		for _, e := range es {
			live[e.Instance] = true
		}
	}
	entries, _ := os.ReadDir(logs)
	for _, e := range entries {
		name := e.Name()
		inst, ok := holderInstance(name)
		if !ok {
			continue
		}
		path := filepath.Join(logs, name)
		cur := filepath.Join(logs, holderLogPrefix+inst+".log")
		if name == filepath.Base(cur) {
			info, err := e.Info()
			if err == nil && listed && !live[inst] && d.now().Sub(info.ModTime()) > goneHolderAge {
				logfile.Shift(cur, 0)
				continue
			}
			capFile(path, holderLogMax, holderLogKeep)
			continue
		}
		if _, err := os.Stat(cur); errors.Is(err, os.ErrNotExist) {
			os.Remove(path) // an old file whose holder's file is gone
		}
	}
	d.pruneCrash()
	d.budget()
}

// holderInstance returns the instance of a holder's file or one of its old files.
func holderInstance(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, holderLogPrefix)
	if !ok {
		return "", false
	}
	inst, _, ok := strings.Cut(rest, ".log")
	return inst, ok && holderreg.ValidInstance(inst)
}

// pruneCrash keeps the newest crashKeep files in crash/, and this run's.
func (d *habDiag) pruneCrash() {
	dir := filepath.Join(d.state, crashDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type f struct {
		name string
		t    time.Time
	}
	var files []f
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.Name() != d.marker.CrashFile {
			files = append(files, f{e.Name(), info.ModTime()})
		}
	}
	slices.SortFunc(files, func(a, b f) int { return b.t.Compare(a.t) })
	for i := crashKeep - 1; i < len(files); i++ {
		os.Remove(filepath.Join(dir, files[i].name))
	}
}

// budget removes the oldest old log files while the logs folder is over logsBudget (11).
func (d *habDiag) budget() {
	logs := logsDir(d.state)
	type f struct {
		path string
		size int64
		t    time.Time
		old  bool
	}
	var files []f
	var total int64
	filepath.WalkDir(logs, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		ext := filepath.Ext(p)
		old := len(ext) > 1 && strings.Trim(ext[1:], "0123456789") == ""
		files = append(files, f{p, info.Size(), info.ModTime(), old})
		total += info.Size()
		return nil
	})
	if total <= logsBudget {
		return
	}
	slices.SortFunc(files, func(a, b f) int { return a.t.Compare(b.t) })
	for _, x := range files {
		if total <= logsBudget {
			return
		}
		if x.old && os.Remove(x.path) == nil {
			total -= x.size
		}
	}
}

func diagNoticeRaiser(p *pushService) func(api.NoticeRaiseParams) error {
	return func(n api.NoticeRaiseParams) error {
		_, err := p.Raise("", n)
		return err
	}
}

// fmtSize formats a byte count for gff-hab status.
func fmtSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
